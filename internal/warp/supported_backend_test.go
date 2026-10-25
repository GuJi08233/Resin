//go:build with_gvisor

package warp

import (
	"context"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter/outbound"
	M "github.com/sagernet/sing/common/metadata"
)

type blockingParentPacket struct {
	closed chan struct{}
	once   sync.Once
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
			exit, err := newTunnel(parent, family, reg, []netip.Prefix{address}, netip.MustParseAddrPort("162.159.192.1:2408"))
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
