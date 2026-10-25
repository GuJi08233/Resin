//go:build with_gvisor

package outbound

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestWireGuardCompatImmediateClose(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	raw := json.RawMessage(`{
		"type":"wireguard", "tag":"legacy-wg", "server":"127.0.0.1", "server_port":2480,
		"local_address":["172.16.0.2/32","fd01::1/128"],
		"private_key":"eCtXsJZ27+4PbhDkHnB923tkUn2Gj59wZw5wFA75MnU=",
		"peer_public_key":"Cr8hWlKvtDt7nrvf+f0brNQQzabAqrjfBvas9pmowjo="
	}`)
	for iteration := 0; iteration < 20; iteration++ {
		done := make(chan error, 1)
		go func() {
			ob, err := b.Build(raw)
			if err == nil {
				if _, ok := ob.(*wireGuardCompatOutbound); !ok {
					err = fmt.Errorf("legacy usermode WireGuard bypassed the safe backend: %T", ob)
				}
				if closeErr := ob.(io.Closer).Close(); err == nil {
					err = closeErr
				}
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: %v", iteration, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: immediate legacy WireGuard Close blocked", iteration)
		}
	}
}

func TestWireGuardCompatPreservesLegacyPeerOptions(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	options := option.LegacyWireGuardOutboundOptions{
		InterfaceName: "legacy-name", MTU: 1380, Workers: 2,
		LocalAddress: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")}, PrivateKey: "private",
		Peers: []option.LegacyWireGuardPeer{
			{ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 2408},
				PublicKey: "first-public", PreSharedKey: "first-psk", Reserved: []uint8{1, 2, 3},
				AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
			{ServerOptions: option.ServerOptions{Server: "::1", ServerPort: 2409},
				PublicKey: "second-public", Reserved: []uint8{4, 5, 6},
				AllowedIPs: []netip.Prefix{netip.MustParsePrefix("::/0")}},
		},
	}
	result, err := b.legacyWireGuardEndpointOptions(options, b.logFactory.NewLogger("test"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Context != b.ctx || result.Dialer == nil || result.System || result.Name != options.InterfaceName ||
		result.MTU != options.MTU || result.Workers != options.Workers || result.PrivateKey != options.PrivateKey ||
		!reflect.DeepEqual(result.Address, []netip.Prefix(options.LocalAddress)) {
		t.Fatalf("legacy endpoint options not preserved: %+v", result)
	}
	if len(result.Peers) != len(options.Peers) {
		t.Fatal("legacy peer count changed")
	}
	for index, peer := range options.Peers {
		converted := result.Peers[index]
		if converted.Endpoint != peer.ServerOptions.Build() || converted.PublicKey != peer.PublicKey ||
			converted.PreSharedKey != peer.PreSharedKey || !reflect.DeepEqual(converted.Reserved, peer.Reserved) ||
			!reflect.DeepEqual(converted.AllowedIPs, []netip.Prefix(peer.AllowedIPs)) {
			t.Fatalf("peer %d changed: %+v", index, converted)
		}
	}

	options.Peers = nil
	options.Server, options.ServerPort = "127.0.0.1", 2408
	options.PeerPublicKey, options.PreSharedKey, options.Reserved = "fallback-public", "fallback-psk", []uint8{7, 8, 9}
	result, err = b.legacyWireGuardEndpointOptions(options, b.logFactory.NewLogger("test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Peers) != 1 || result.Peers[0].PublicKey != options.PeerPublicKey ||
		result.Peers[0].PreSharedKey != options.PreSharedKey || !reflect.DeepEqual(result.Peers[0].Reserved, options.Reserved) ||
		!reflect.DeepEqual(result.Peers[0].AllowedIPs, []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}) {
		t.Fatalf("legacy single-peer defaults changed: %+v", result.Peers)
	}
}

func TestWireGuardCompatResolvesPeersThroughConfiguredDNS(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var ipv6Request atomic.Bool
	server := &mDNS.Server{PacketConn: packet, Handler: mDNS.HandlerFunc(func(writer mDNS.ResponseWriter, request *mDNS.Msg) {
		requests.Add(1)
		response := new(mDNS.Msg)
		response.SetReply(request)
		for _, question := range request.Question {
			if question.Qtype == mDNS.TypeAAAA {
				ipv6Request.Store(true)
			}
			if question.Qtype == mDNS.TypeA {
				response.Answer = append(response.Answer, &mDNS.A{Hdr: mDNS.RR_Header{Name: question.Name, Rrtype: mDNS.TypeA, Class: mDNS.ClassINET, Ttl: 60}, A: net.IPv4(127, 0, 0, 1)})
			}
		}
		_ = writer.WriteMsg(response)
	})}
	go server.ActivateAndServe()
	defer server.Shutdown()
	b, err := NewSingboxBuilderWithConfig(SingboxBuilderConfig{DNSUpstreams: []string{"udp://" + packet.LocalAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	options := option.LegacyWireGuardOutboundOptions{
		DialerOptions: option.DialerOptions{DomainStrategy: option.DomainStrategy(C.DomainStrategyIPv4Only)},
		ServerOptions: option.ServerOptions{Server: "wireguard.local.test", ServerPort: 2408},
	}
	result, err := b.legacyWireGuardEndpointOptions(options, b.logFactory.NewLogger("test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Peers[0].Endpoint; got.Addr != netip.MustParseAddr("127.0.0.1") || got.Port != 2408 || got.IsFqdn() {
		t.Fatalf("configured DNS did not resolve the peer: %s", got)
	}
	if requests.Load() == 0 || ipv6Request.Load() {
		t.Fatal("peer lookup bypassed configured DNS or ignored the IPv4-only strategy")
	}
}

func TestWireGuardCompatLeavesSystemInterfaceOnRegistryPath(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	for _, cfg := range []option.Outbound{
		{Type: C.TypeHTTP},
		{Type: C.TypeWireGuard, Options: &option.LegacyWireGuardOutboundOptions{SystemInterface: true}},
	} {
		ob, handled, err := b.buildWireGuardCompat(cfg, b.logFactory.NewLogger("test"))
		if ob != nil || handled || err != nil {
			t.Fatalf("unhandled outbound entered userspace compatibility path: %v %t %v", ob, handled, err)
		}
	}
	_, handled, err := b.buildWireGuardCompat(option.Outbound{Type: C.TypeWireGuard, Options: &option.LegacyWireGuardOutboundOptions{
		DialerOptions: option.DialerOptions{Detour: "other"}, GSO: true,
	}}, b.logFactory.NewLogger("test"))
	if !handled || err == nil {
		t.Fatal("legacy GSO/detour conflict was lost")
	}
	var _ adapter.Outbound = (*wireGuardCompatOutbound)(nil)
}
