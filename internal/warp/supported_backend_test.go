//go:build with_gvisor

package warp

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter/outbound"
	wg "github.com/sagernet/sing-box/transport/wireguard"
	M "github.com/sagernet/sing/common/metadata"
)

type blockingParentPacket struct {
	closed chan struct{}
	once   sync.Once
}

type blockedWriteParentPacket struct {
	blockingParentPacket
	writeStarted chan struct{}
	writeOnce    sync.Once
}

func (p *blockedWriteParentPacket) WriteTo([]byte, net.Addr) (int, error) {
	p.writeOnce.Do(func() { close(p.writeStarted) })
	<-p.closed
	return 0, net.ErrClosed
}

func backendTestKeys(t *testing.T) (string, string) {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(private.Bytes()), base64.StdEncoding.EncodeToString(peer.PublicKey().Bytes())
}

func TestUserspaceEndpointCloseInterruptsBlockedParentWrite(t *testing.T) {
	packet := &blockedWriteParentPacket{
		blockingParentPacket: blockingParentPacket{closed: make(chan struct{})},
		writeStarted:         make(chan struct{}),
	}
	defer packet.Close()
	privateKey, peerKey := backendTestKeys(t)
	parent := &transportParent{packet: func(context.Context, M.Socksaddr) (net.PacketConn, error) { return packet, nil }}
	endpoint, err := NewUserspaceEndpoint(wg.EndpointOptions{
		Context: context.Background(), Dialer: parentPacketDialer{parent: parent},
		Address: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")}, Workers: 1,
		PrivateKey: privateKey,
		Peers: []wg.PeerOptions{{Endpoint: M.ParseSocksaddr("162.159.192.1:2408"),
			PublicKey:  peerKey,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		conn, err := endpoint.DialContext(ctx, "tcp", M.ParseSocksaddr("1.1.1.1:443"))
		if err == nil {
			conn.Close()
		}
	}()
	select {
	case <-packet.writeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("WireGuard did not attempt a parent write")
	}
	// A blocked sender holds WireGuard's network read lock. Closing the owned
	// bind must unblock it before the device waits for the network write lock.
	done := make(chan error, 8)
	for range cap(done) {
		go func() { done <- endpoint.Close() }()
	}
	for range cap(done) {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			packet.Close()
			t.Fatal("endpoint close waited for a blocked parent write")
		}
	}
	select {
	case <-dialDone:
	case <-time.After(3 * time.Second):
		t.Fatal("endpoint shutdown did not release its active dial")
	}
}

func TestTunnelSetupCancellationInterruptsParentWrite(t *testing.T) {
	packet := &blockedWriteParentPacket{
		blockingParentPacket: blockingParentPacket{closed: make(chan struct{})},
		writeStarted:         make(chan struct{}),
	}
	defer packet.Close()
	privateKey, peerKey := backendTestKeys(t)
	parent := &transportParent{packet: func(context.Context, M.Socksaddr) (net.PacketConn, error) { return packet, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		exit, err := newTunnel(ctx, parent, IPv4, registration{privateKey: privateKey, peerKey: peerKey},
			[]netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")}, netip.MustParseAddrPort("162.159.192.1:2408"))
		if exit != nil {
			exit.Close()
		}
		done <- err
	}()
	select {
	case <-packet.writeStarted:
	case <-time.After(3 * time.Second):
		packet.Close()
		t.Fatal("setup never reached the parent's blocking handshake write")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("setup cancellation returned %v", err)
		}
	case <-time.After(3 * time.Second):
		packet.Close()
		t.Fatal("attempt cancellation did not interrupt the parent's setup write")
	}
}

func TestTunnelSurvivesSuccessfulAttemptCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	packet := &blockingParentPacket{closed: make(chan struct{})}
	defer packet.Close()
	parentContext := make(chan context.Context, 1)
	parent := &transportParent{packet: func(ctx context.Context, _ M.Socksaddr) (net.PacketConn, error) {
		select {
		case parentContext <- ctx:
		default:
		}
		return packet, nil
	}}
	reg := registration{id: "test", privateKey: "eCtXsJZ27+4PbhDkHnB923tkUn2Gj59wZw5wFA75MnU=", peerKey: "Cr8hWlKvtDt7nrvf+f0brNQQzabAqrjfBvas9pmowjo="}
	exit, err := newTunnel(ctx, parent, IPv4, reg, []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")}, netip.MustParseAddrPort("162.159.192.1:2408"))
	if err != nil {
		t.Fatal(err)
	}
	defer exit.Close()
	var lifetime context.Context
	select {
	case lifetime = <-parentContext:
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel never opened its parent packet connection")
	}
	cancel()
	select {
	case <-lifetime.Done():
		t.Fatal("successful tunnel retained the short-lived creation context")
	case <-time.After(20 * time.Millisecond):
	}
	if err := exit.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lifetime.Done():
	default:
		t.Fatal("closing the tunnel did not cancel its owned lifetime")
	}
}

func (p *blockingParentPacket) ReadFrom([]byte) (int, net.Addr, error) {
	<-p.closed
	return 0, nil, net.ErrClosed
}
func (p *blockingParentPacket) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }
func (p *blockingParentPacket) Close() error                              { p.once.Do(func() { close(p.closed) }); return nil }
func (*blockingParentPacket) LocalAddr() net.Addr                         { return &net.UDPAddr{IP: net.IPv4zero, Port: 1234} }
func (*blockingParentPacket) SetDeadline(time.Time) error                 { return nil }
func (*blockingParentPacket) SetReadDeadline(time.Time) error             { return nil }
func (*blockingParentPacket) SetWriteDeadline(time.Time) error            { return nil }

// Exercise the actual gVisor/WireGuard lifecycle with an inert parent packet
// connection. No API registration, public sockets or network access is involved.
func TestUserspaceBackendOpensAndClosesParentPacket(t *testing.T) {
	for _, family := range []Family{IPv4, IPv6} {
		t.Run(string(family), func(t *testing.T) {
			packet := &blockingParentPacket{closed: make(chan struct{})}
			defer packet.Close()
			opened := make(chan struct{}, 1)
			parent := &transportParent{Adapter: outbound.NewAdapter("test", "parent", []string{"tcp", "udp"}, nil)}
			parent.dial = func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				t.Error("backend bypassed parent packet path")
				return nil, net.ErrClosed
			}
			parent.packet = func(context.Context, M.Socksaddr) (net.PacketConn, error) {
				select {
				case opened <- struct{}{}:
				default:
				}
				return packet, nil
			}
			address := netip.MustParsePrefix("172.16.0.2/32")
			if family == IPv6 {
				address = netip.MustParsePrefix("2606:4700:110::2/128")
			}
			reg := registration{id: "test", privateKey: "eCtXsJZ27+4PbhDkHnB923tkUn2Gj59wZw5wFA75MnU=", peerKey: "Cr8hWlKvtDt7nrvf+f0brNQQzabAqrjfBvas9pmowjo="}
			exit, err := newTunnel(context.Background(), parent, family, reg, []netip.Prefix{address}, netip.MustParseAddrPort("162.159.192.1:2408"))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-opened:
			case <-time.After(3 * time.Second):
				t.Fatal("WireGuard never opened parent packet connection")
			}
			closed := make(chan error, 1)
			go func() { closed <- exit.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				stack := make([]byte, 1<<20)
				stack = stack[:runtime.Stack(stack, true)]
				var selected []string
				for _, block := range strings.Split(string(stack), "\n\n") {
					if strings.Contains(block, "wireguard-go/device") || strings.Contains(block, "ClientBind") {
						selected = append(selected, block)
					}
				}
				t.Fatalf("WireGuard close did not unblock packet reader\n%s", strings.Join(selected, "\n\n"))
			}
			select {
			case <-packet.closed:
			default:
				t.Fatal("WireGuard did not close parent packet connection")
			}
		})
	}
}
