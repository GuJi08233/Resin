//go:build with_gvisor

package warp

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	wg "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service/pause"
	wgDevice "github.com/sagernet/wireguard-go/device"
	wgTun "github.com/sagernet/wireguard-go/tun"
)

func Supported() bool { return true }

// manualStack has no asynchronous interface events. This in-process
// stack with an explicit startup/shutdown lifecycle, so device.Up below is the
// only transition to Up. In the current WireGuard dependency, racing EventUp
// against Close inverts its state/ipc locks and can deadlock shutdown.
type manualStack struct {
	wg.Device
	events <-chan wgTun.Event
}

func (s *manualStack) Events() <-chan wgTun.Event { return s.events }

type userspaceEndpoint struct {
	wg.Device
	wireguard *wgDevice.Device
}

func (e *userspaceEndpoint) Close() error {
	e.wireguard.Close()
	return nil
}

func createEndpoint(ctx context.Context, parent adapter.Outbound, reg registration, addresses []netip.Prefix, endpoint netip.AddrPort) (tunnelEndpoint, error) {
	return NewUserspaceEndpoint(wg.EndpointOptions{
		Context: ctx, Logger: log.NewNOPFactory().NewLogger("warp"), MTU: 1280,
		Dialer: parentPacketDialer{parent: parent}, Address: addresses, PrivateKey: reg.privateKey,
		Peers: []wg.PeerOptions{{Endpoint: M.SocksaddrFromNetIP(endpoint), PublicKey: reg.peerKey,
			AllowedIPs:                  []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")},
			PersistentKeepaliveInterval: 25, Reserved: reg.reserved}}, Workers: 1,
	})
}

// UserspaceEndpoint is a started WireGuard network stack with synchronous
// lifecycle transitions. It owns its stack and bindings, not the supplied dialer.
type UserspaceEndpoint interface {
	N.Dialer
	Close() error
}

// NewUserspaceEndpoint starts a WireGuard device without asynchronous TUN
// events. Peers must have numeric endpoints; resolve hostnames before calling.
// The supplied dialer is mandatory and is never replaced with a direct dialer.
func NewUserspaceEndpoint(options wg.EndpointOptions) (UserspaceEndpoint, error) {
	if options.System {
		return nil, errors.New("userspace WireGuard cannot create a system interface")
	}
	if options.Dialer == nil {
		return nil, errors.New("userspace WireGuard requires an explicit dialer")
	}
	privateKey, err := keyHex(options.PrivateKey)
	if err != nil {
		return nil, errors.New("invalid WireGuard private key")
	}
	var configuration strings.Builder
	fmt.Fprintf(&configuration, "private_key=%s\n", privateKey)
	if options.ListenPort != 0 {
		fmt.Fprintf(&configuration, "listen_port=%d\n", options.ListenPort)
	}
	if len(options.Peers) == 0 {
		return nil, errors.New("userspace WireGuard requires at least one peer")
	}
	var allowed []netip.Prefix
	for index, peer := range options.Peers {
		if !peer.Endpoint.Addr.IsValid() || peer.Endpoint.Port == 0 {
			return nil, fmt.Errorf("WireGuard peer %d requires a numeric endpoint", index)
		}
		publicKey, err := keyHex(peer.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("invalid WireGuard public key for peer %d", index)
		}
		if len(peer.AllowedIPs) == 0 {
			return nil, fmt.Errorf("missing allowed ips for peer %d", index)
		}
		if len(peer.Reserved) != 0 && len(peer.Reserved) != 3 {
			return nil, fmt.Errorf("WireGuard peer %d requires three reserved bytes", index)
		}
		fmt.Fprintf(&configuration, "public_key=%s\nendpoint=%s\n", publicKey, peer.Endpoint)
		if peer.PreSharedKey != "" {
			preShared, err := keyHex(peer.PreSharedKey)
			if err != nil {
				return nil, fmt.Errorf("invalid WireGuard pre-shared key for peer %d", index)
			}
			fmt.Fprintf(&configuration, "preshared_key=%s\n", preShared)
		}
		for _, prefix := range peer.AllowedIPs {
			if !prefix.IsValid() {
				return nil, fmt.Errorf("invalid allowed IP prefix for WireGuard peer %d", index)
			}
			fmt.Fprintf(&configuration, "allowed_ip=%s\n", prefix)
			allowed = append(allowed, prefix)
		}
		if peer.PersistentKeepaliveInterval != 0 {
			fmt.Fprintf(&configuration, "persistent_keepalive_interval=%d\n", peer.PersistentKeepaliveInterval)
		}
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = pause.WithDefaultManager(ctx)
	logger := options.Logger
	if logger == nil {
		logger = log.NewNOPFactory().NewLogger("wireguard")
	}
	mtu := options.MTU
	if mtu == 0 {
		mtu = 1408
	}
	stack, err := wg.NewDevice(wg.DeviceOptions{
		Context: ctx, Logger: logger, System: false, MTU: mtu,
		Name: options.Name, Address: options.Address, AllowedAddress: allowed,
		Handler: options.Handler, UDPTimeout: options.UDPTimeout, CreateDialer: options.CreateDialer,
	})
	if err != nil {
		return nil, err
	}
	events := make(chan wgTun.Event)
	close(events)
	manual := &manualStack{Device: stack, events: events}
	var reserved [3]byte
	var connectAddr netip.AddrPort
	if len(options.Peers) == 1 {
		connectAddr = options.Peers[0].Endpoint.AddrPort()
		copy(reserved[:], options.Peers[0].Reserved)
	}
	bind := wg.NewClientBind(ctx, logger, options.Dialer, len(options.Peers) == 1, connectAddr, reserved)
	if len(options.Peers) > 1 {
		for _, peer := range options.Peers {
			var peerReserved [3]byte
			copy(peerReserved[:], peer.Reserved)
			bind.SetReservedForEndpoint(peer.Endpoint.AddrPort(), peerReserved)
		}
	}
	wireguard := wgDevice.NewDevice(ctx, manual, bind, &wgDevice.Logger{
		Verbosef: func(format string, args ...any) { logger.Debug(fmt.Sprintf(format, args...)) },
		Errorf:   func(format string, args ...any) { logger.Error(fmt.Sprintf(format, args...)) },
	}, options.Workers)
	stack.SetDevice(wireguard)
	if err := wireguard.IpcSet(configuration.String()); err != nil {
		wireguard.Close()
		return nil, fmt.Errorf("configure WireGuard device: %w", err)
	}
	// Complete startup synchronously before publishing the backend. No TUN
	// EventUp can race this call or a later shutdown.
	if err := wireguard.Up(); err != nil {
		wireguard.Close()
		return nil, fmt.Errorf("start WireGuard device: %w", err)
	}
	return &userspaceEndpoint{Device: stack, wireguard: wireguard}, nil
}

func keyHex(value string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid WireGuard key")
	}
	return hex.EncodeToString(decoded), nil
}
