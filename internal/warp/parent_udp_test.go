package warp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	M "github.com/sagernet/sing/common/metadata"
)

// probePacket answers DNS queries when reply is set and stays silent otherwise.
type probePacket struct {
	reply     bool
	mu        sync.Mutex
	writes    int
	responses chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newProbePacket(reply bool) *probePacket {
	return &probePacket{reply: reply, responses: make(chan []byte, 8), closed: make(chan struct{})}
}

func (p *probePacket) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case response := <-p.responses:
		return copy(b, response), udpProbeTarget.UDPAddr(), nil
	case <-p.closed:
		return 0, nil, net.ErrClosed
	}
}

func (p *probePacket) WriteTo(b []byte, _ net.Addr) (int, error) {
	p.mu.Lock()
	p.writes++
	p.mu.Unlock()
	if p.reply {
		response := append([]byte(nil), b...)
		response[2] |= 0x80
		p.responses <- response
	}
	return len(b), nil
}

func (p *probePacket) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}
func (*probePacket) LocalAddr() net.Addr              { return &net.UDPAddr{IP: net.IPv4zero} }
func (*probePacket) SetDeadline(time.Time) error      { return nil }
func (*probePacket) SetReadDeadline(time.Time) error  { return nil }
func (*probePacket) SetWriteDeadline(time.Time) error { return nil }

func probeParent(packet func(context.Context, M.Socksaddr) (net.PacketConn, error)) *transportParent {
	return &transportParent{Adapter: outbound.NewAdapter("socks", "parent", []string{"tcp", "udp"}, nil), packet: packet,
		dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			return nil, errors.New("unexpected TCP dial")
		}}
}

func shortenUDPProbe(t *testing.T) {
	timeout, interval := udpProbeTimeout, udpProbeInterval
	udpProbeTimeout, udpProbeInterval = 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { udpProbeTimeout, udpProbeInterval = timeout, interval })
}

func TestProbeParentUDPClassifiesParentBehavior(t *testing.T) {
	shortenUDPProbe(t)
	answering := newProbePacket(true)
	err := probeParentUDP(context.Background(), probeParent(func(context.Context, M.Socksaddr) (net.PacketConn, error) { return answering, nil }))
	if err != nil {
		t.Fatalf("answering parent failed the UDP probe: %v", err)
	}
	if !strings.Contains(string(dnsProbeQuery([2]byte{1, 2})), "cloudflare") {
		t.Fatal("probe query lost its question")
	}

	rejected := probeParent(func(context.Context, M.Socksaddr) (net.PacketConn, error) {
		return nil, errors.New("socks5: request rejected, code=7")
	})
	parentErr, ok := AsParentUDPError(probeParentUDP(context.Background(), rejected))
	if !ok || !parentErr.Rejected || !strings.Contains(parentErr.Error(), "code=7") {
		t.Fatalf("explicit UDP refusal was not classified as rejected: %v", parentErr)
	}

	silent := newProbePacket(false)
	start := time.Now()
	parentErr, ok = AsParentUDPError(probeParentUDP(context.Background(), probeParent(func(context.Context, M.Socksaddr) (net.PacketConn, error) { return silent, nil })))
	if !ok || parentErr.Rejected || time.Since(start) > 2*time.Second {
		t.Fatalf("silent parent was not reported as unavailable within the probe budget: %v", parentErr)
	}
	silent.mu.Lock()
	writes := silent.writes
	silent.mu.Unlock()
	if writes < 2 {
		t.Fatalf("silent parent probe did not resend its query: writes=%d", writes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeParentUDP(ctx, rejected); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation was misreported as a parent failure: %v", err)
	}
}

func TestFactoryStopsEndpointsWhenParentRefusesOuterUDP(t *testing.T) {
	reg := registration{addresses: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")},
		endpoints: []netip.AddrPort{netip.MustParseAddrPort("162.159.192.1:2408"), netip.MustParseAddrPort("162.159.192.1:500"), netip.MustParseAddrPort("162.159.193.1:2408")}}
	opens := 0
	factory := &Factory{
		openTunnel: func(context.Context, adapter.Outbound, Family, registration, []netip.Prefix, netip.AddrPort) (*Exit, error) {
			opens++
			return &Exit{endpoint: &diagnosticTestEndpoint{state: tunnelDiagnostics{available: true}}, cancel: func() {}, outer: newOuterDiagnostics()}, nil
		},
		verifyExit: func(probeCtx context.Context, exit *Exit) error {
			refusal := errors.New("socks5: request rejected, code=7")
			exit.outer.record("listen_packet", false, refusal)
			exit.outer.record("listen_packet", false, refusal)
			<-probeCtx.Done()
			return exit.diagnosticError("tcp_connect", probeCtx.Err())
		},
	}
	start := time.Now()
	_, err := factory.createRegistered(context.Background(), nil, IPv4, reg)
	if time.Since(start) > 2*time.Second {
		t.Fatal("refused outer UDP still waited for the endpoint probe timeout")
	}
	parentErr, ok := AsParentUDPError(err)
	if !ok || parentErr.Rejected || opens != 1 || !strings.Contains(err.Error(), "attempts=1") || !strings.Contains(err.Error(), "code=7") {
		t.Fatalf("refused outer UDP did not stop remaining endpoints: opens=%d err=%v", opens, err)
	}
}

func TestOuterDiagnosticsToleratesOneListenFailureAndIgnoresAfterTraffic(t *testing.T) {
	state := newOuterDiagnostics()
	exit := &Exit{outer: state}
	state.record("listen_packet", false, errors.New("transient"))
	if exit.outerFailure() {
		t.Fatal("one transient listen failure aborted the endpoint")
	}
	state.record("read", true, nil)
	state.record("listen_packet", false, errors.New("after traffic"))
	if exit.outerFailure() {
		t.Fatal("listen failure after received packets was treated as a UDP refusal")
	}
	if (&Exit{}).outerFailure() || (&Exit{}).outerFailed() != nil {
		t.Fatal("an exit without outer diagnostics reported a refusal")
	}
}

func TestFactorySpareRegistrationsAreBoundedAndExpire(t *testing.T) {
	factory := &Factory{}
	now := time.Now()
	for index := 0; index < spareLimit+2; index++ {
		factory.keepSpare(registration{id: string(rune('a' + index)), expires: now.Add(time.Minute)})
	}
	factory.keepSpare(registration{id: "exhausted", tries: registrationMaxTries, expires: now.Add(time.Minute)})
	if len(factory.spares) != spareLimit || factory.spares[0].id != "c" {
		t.Fatalf("spare pool exceeded its bound or kept the oldest devices: %d", len(factory.spares))
	}
	if reg, ok := factory.takeSpare(now); !ok || reg.id != string(rune('a'+spareLimit+1)) {
		t.Fatalf("spare pool did not hand out the newest device: %+v", reg)
	}
	if _, ok := factory.takeSpare(now.Add(2 * time.Minute)); ok || len(factory.spares) != 0 {
		t.Fatal("expired spare devices were reused")
	}
}
