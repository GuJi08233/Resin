//go:build with_gvisor

package outbound

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	wg "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// The legacy upstream WireGuard endpoint drives asynchronous device Up/Down
// events. Its immediate Close can deadlock in wireguard-go beta7. Reuse the
// synchronously started userspace backend while retaining legacy option/DNS
// handling. System-interface outbounds continue through the upstream registry.
type wireGuardCompatOutbound struct {
	sbOutbound.Adapter
	dns      adapter.DNSRouter
	endpoint warp.UserspaceEndpoint
}

func (b *SingboxBuilder) buildWireGuardCompat(cfg option.Outbound, logger log.ContextLogger) (adapter.Outbound, bool, error) {
	if cfg.Type != C.TypeWireGuard {
		return nil, false, nil
	}
	options, ok := cfg.Options.(*option.LegacyWireGuardOutboundOptions)
	if !ok || options.SystemInterface {
		return nil, false, nil
	}
	endpointOptions, err := b.legacyWireGuardEndpointOptions(*options, logger)
	if err != nil {
		return nil, true, err
	}
	endpoint, err := warp.NewUserspaceEndpoint(endpointOptions)
	if err != nil {
		return nil, true, err
	}
	return &wireGuardCompatOutbound{
		Adapter: sbOutbound.NewAdapterWithDialerOptions(C.TypeWireGuard, cfg.Tag,
			[]string{N.NetworkTCP, N.NetworkUDP}, options.DialerOptions),
		dns: b.dnsRouter, endpoint: endpoint,
	}, true, nil
}

func (b *SingboxBuilder) legacyWireGuardEndpointOptions(options option.LegacyWireGuardOutboundOptions, logger log.ContextLogger) (wg.EndpointOptions, error) {
	if options.Detour != "" && options.GSO {
		return wg.EndpointOptions{}, fmt.Errorf("gso is conflict with detour")
	}
	remoteIsDomain := options.ServerIsDomain()
	for _, peer := range options.Peers {
		remoteIsDomain = remoteIsDomain || peer.ServerIsDomain()
	}
	outer, err := dialer.NewWithOptions(dialer.Options{
		Context: b.ctx, Options: options.DialerOptions,
		RemoteIsDomain: remoteIsDomain, ResolverOnDetour: true,
	})
	if err != nil {
		return wg.EndpointOptions{}, err
	}
	peers := make([]wg.PeerOptions, 0, max(1, len(options.Peers)))
	for _, peer := range options.Peers {
		peers = append(peers, wg.PeerOptions{
			Endpoint: peer.ServerOptions.Build(), PublicKey: peer.PublicKey,
			PreSharedKey: peer.PreSharedKey, AllowedIPs: peer.AllowedIPs, Reserved: peer.Reserved,
		})
	}
	if len(peers) == 0 {
		peers = append(peers, wg.PeerOptions{
			Endpoint: options.ServerOptions.Build(), PublicKey: options.PeerPublicKey,
			PreSharedKey: options.PreSharedKey, Reserved: options.Reserved,
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(netip.IPv4Unspecified(), 0), netip.PrefixFrom(netip.IPv6Unspecified(), 0)},
		})
	}
	var queryOptions adapter.DNSQueryOptions
	if resolver, ok := outer.(dialer.ResolveDialer); ok {
		queryOptions = resolver.QueryOptions()
	}
	for index := range peers {
		if !peers[index].Endpoint.IsFqdn() {
			continue
		}
		addresses, err := b.dnsRouter.Lookup(b.ctx, peers[index].Endpoint.Fqdn, queryOptions)
		if err != nil {
			return wg.EndpointOptions{}, fmt.Errorf("resolve WireGuard peer %d: %w", index, err)
		}
		if len(addresses) == 0 {
			return wg.EndpointOptions{}, fmt.Errorf("resolve WireGuard peer %d: no addresses", index)
		}
		peers[index].Endpoint = M.Socksaddr{Addr: addresses[0], Port: peers[index].Endpoint.Port}
	}
	return wg.EndpointOptions{
		Context: b.ctx, Logger: logger, Dialer: outer, Name: options.InterfaceName,
		MTU: options.MTU, Address: options.LocalAddress, PrivateKey: options.PrivateKey,
		Peers: peers, Workers: options.Workers,
	}, nil
}

func (o *wireGuardCompatOutbound) Close() error { return o.endpoint.Close() }

func (o *wireGuardCompatOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if !destination.IsFqdn() {
		if !destination.Addr.IsValid() {
			return nil, fmt.Errorf("invalid destination: %s", destination)
		}
		return o.endpoint.DialContext(ctx, network, destination)
	}
	addresses, err := o.dns.Lookup(ctx, destination.Fqdn, adapter.DNSQueryOptions{})
	if err != nil {
		return nil, err
	}
	return N.DialSerial(ctx, o.endpoint, network, destination, addresses)
}

func (o *wireGuardCompatOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if !destination.IsFqdn() {
		return o.endpoint.ListenPacket(ctx, destination)
	}
	addresses, err := o.dns.Lookup(ctx, destination.Fqdn, adapter.DNSQueryOptions{})
	if err != nil {
		return nil, err
	}
	packet, _, err := N.ListenSerial(ctx, o.endpoint, destination, addresses)
	return packet, err
}
