//go:build with_gvisor

package warp

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter/outbound"
	wg "github.com/sagernet/sing-box/transport/wireguard"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type dataPlaneEcho struct{}

func (dataPlaneEcho) PrepareConnection(string, M.Socksaddr, M.Socksaddr) error { return nil }
func (dataPlaneEcho) NewConnectionEx(_ context.Context, conn net.Conn, _, _ M.Socksaddr, _ N.CloseHandlerFunc) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.Copy(conn, conn)
}
func (dataPlaneEcho) NewPacketConnectionEx(_ context.Context, conn N.PacketConn, _, destination M.Socksaddr, _ N.CloseHandlerFunc) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	packet := buf.NewPacket()
	defer packet.Release()
	if _, err := conn.ReadPacket(packet); err == nil {
		_ = conn.WritePacket(buf.As(packet.Bytes()), destination)
	}
}

// Both WireGuard peers, their UDP outer sockets and their echo applications
// are local to this test. This verifies encrypted TCP/UDP payload round trips,
// not merely the creation/teardown of an idle tunnel. No WARP registration,
// Cloudflare endpoint, system TUN or external network is involved.
func TestUserspaceEndpointEncryptedDataPlane(t *testing.T) {
	for _, family := range []Family{IPv4, IPv6} {
		t.Run(string(family), func(t *testing.T) {
			left, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer left.Close()
			right, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer right.Close()
			leftKey, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			rightKey, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			leftAddr, rightAddr := netip.MustParsePrefix("10.66.0.1/32"), netip.MustParsePrefix("10.66.0.2/32")
			if family == IPv6 {
				leftAddr, rightAddr = netip.MustParsePrefix("fd66::1/128"), netip.MustParsePrefix("fd66::2/128")
			}
			parent := func(packet net.PacketConn) parentPacketDialer {
				p := &transportParent{Adapter: outbound.NewAdapter("test", "local-packet-only", []string{"udp"}, nil)}
				p.dial = func(context.Context, string, M.Socksaddr) (net.Conn, error) {
					t.Error("WireGuard bypassed the parent packet path")
					return nil, net.ErrClosed
				}
				p.packet = func(context.Context, M.Socksaddr) (net.PacketConn, error) { return packet, nil }
				return parentPacketDialer{parent: p}
			}
			server, err := NewUserspaceEndpoint(wg.EndpointOptions{
				Context: context.Background(), Dialer: parent(right), Address: []netip.Prefix{rightAddr},
				PrivateKey: base64.StdEncoding.EncodeToString(rightKey.Bytes()), Handler: dataPlaneEcho{}, UDPTimeout: time.Minute, MTU: 1280, Workers: 1,
				Peers: []wg.PeerOptions{{Endpoint: M.SocksaddrFromNet(left.LocalAddr()),
					PublicKey: base64.StdEncoding.EncodeToString(leftKey.PublicKey().Bytes()), AllowedIPs: []netip.Prefix{leftAddr}, Reserved: []byte{4, 5, 6}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client, err := NewUserspaceEndpoint(wg.EndpointOptions{
				Context: context.Background(), Dialer: parent(left), Address: []netip.Prefix{leftAddr},
				PrivateKey: base64.StdEncoding.EncodeToString(leftKey.Bytes()), MTU: 1280, Workers: 1,
				Peers: []wg.PeerOptions{{Endpoint: M.SocksaddrFromNet(right.LocalAddr()),
					PublicKey: base64.StdEncoding.EncodeToString(rightKey.PublicKey().Bytes()), AllowedIPs: []netip.Prefix{rightAddr}, Reserved: []byte{1, 2, 3}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for _, network := range []string{"tcp", "udp"} {
				t.Run(network, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					conn, err := client.DialContext(ctx, network, M.Socksaddr{Addr: rightAddr.Addr(), Port: 12345})
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					message := []byte("encrypted-" + string(family) + "-" + network)
					if _, err := conn.Write(message); err != nil {
						t.Fatal(err)
					}
					response := make([]byte, len(message))
					if _, err := io.ReadFull(conn, response); err != nil {
						t.Fatal(err)
					}
					if string(response) != string(message) {
						t.Fatalf("decrypted payload = %q, want %q", response, message)
					}
				})
			}
		})
	}
}
