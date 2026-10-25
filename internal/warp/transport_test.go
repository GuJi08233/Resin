package warp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter/outbound"
	M "github.com/sagernet/sing/common/metadata"
)

type transportParent struct {
	outbound.Adapter
	dial   func(context.Context, string, M.Socksaddr) (net.Conn, error)
	packet func(context.Context, M.Socksaddr) (net.PacketConn, error)
}

func (p *transportParent) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return p.dial(ctx, network, destination)
}
func (p *transportParent) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return p.packet(ctx, destination)
}

func TestRegistrationUsesParentForEveryRequest(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	parent := &transportParent{Adapter: outbound.NewAdapter("test", "parent", []string{"tcp", "udp"}, nil)}
	parent.dial = func(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
		if network != "tcp" || destination.Fqdn != "api.cloudflareclient.com" || destination.Port != 80 {
			t.Errorf("unexpected parent dial %s %s", network, destination)
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			req, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return
			}
			if req.Body != nil {
				io.Copy(io.Discard, req.Body)
				req.Body.Close()
			}
			mu.Lock()
			methods = append(methods, req.Method)
			mu.Unlock()
			response := `{"result":{"id":"test-device","token":"test-token"}}`
			if req.Method == http.MethodGet {
				if req.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing registration authorization")
				}
				response = fmt.Sprintf(`{"result":{"warp_enabled":true,"config":{"client_id":"AQID","interface":{"addresses":{"v4":"172.16.0.2","v6":"2606:4700:110::2"}},"peers":[{"public_key":%q,"endpoint":{"v4":"162.159.192.1:2408","v6":"[2606:4700:d0::1]:2408"}}]}}}`, base64.StdEncoding.EncodeToString(make([]byte, 32)))
			}
			fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(response), response)
		}()
		return client, nil
	}
	r := &registrar{base: "http://api.cloudflareclient.com"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reg, err := r.register(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if reg.id != "test-device" || len(reg.addresses) != 2 || len(reg.reserved) != 3 {
		t.Fatalf("incomplete registration: %#v", reg)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "POST,PATCH,GET" {
		t.Fatalf("unexpected calls: %v", methods)
	}
}

type recordingPacket struct {
	destination net.Addr
	closed      bool
}

func (p *recordingPacket) ReadFrom(b []byte) (int, net.Addr, error) { return 0, nil, io.EOF }
func (p *recordingPacket) WriteTo(b []byte, destination net.Addr) (int, error) {
	p.destination = destination
	return len(b), nil
}
func (p *recordingPacket) Close() error                   { p.closed = true; return nil }
func (*recordingPacket) LocalAddr() net.Addr              { return &net.UDPAddr{IP: net.IPv4zero, Port: 1234} }
func (*recordingPacket) SetDeadline(time.Time) error      { return nil }
func (*recordingPacket) SetReadDeadline(time.Time) error  { return nil }
func (*recordingPacket) SetWriteDeadline(time.Time) error { return nil }

func TestWireGuardOuterUDPOnlyUsesParentPacketConnection(t *testing.T) {
	packet := &recordingPacket{}
	called := false
	parent := &transportParent{}
	parent.dial = func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		t.Fatal("outer UDP must not use TCP/direct dial")
		return nil, nil
	}
	destination := M.ParseSocksaddr("162.159.192.1:2408")
	parent.packet = func(ctx context.Context, got M.Socksaddr) (net.PacketConn, error) {
		called = true
		if got != destination {
			t.Errorf("wrong destination %s", got)
		}
		return packet, nil
	}
	dialer := parentPacketDialer{parent: parent}
	conn, err := dialer.DialContext(context.Background(), "udp", destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("wireguard")); err != nil {
		t.Fatal(err)
	}
	if !called || packet.destination.String() != destination.String() {
		t.Fatalf("packet bypassed parent: %v %v", called, packet.destination)
	}
	conn.Close()
	if !packet.closed {
		t.Fatal("parent packet connection not closed")
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", destination); err == nil {
		t.Fatal("accepted non-UDP outer transport")
	}
}

func TestTraceRequiresWarpAndCorrectPublicFamily(t *testing.T) {
	for _, test := range []struct {
		name, body string
		family     Family
		want       string
	}{
		{"ipv4", "ip=104.28.1.2\nwarp=on\n", IPv4, "104.28.1.2"},
		{"ipv6", "ip=2606:4700:110::2\nwarp=plus\n", IPv6, "2606:4700:110::2"},
		{"off", "ip=104.28.1.2\nwarp=off\n", IPv4, ""},
		{"wrong-family", "ip=104.28.1.2\nwarp=on\n", IPv6, ""},
		{"private", "ip=192.168.1.1\nwarp=on\n", IPv4, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			addr, err := parseTrace(strings.NewReader(test.body), test.family)
			if test.want == "" {
				if err == nil {
					t.Fatalf("accepted unverified exit %s", addr)
				}
				return
			}
			if err != nil || addr != netip.MustParseAddr(test.want) {
				t.Fatalf("got %s %v", addr, err)
			}
		})
	}
}

func TestRegistrationThrottleHonorsCancellation(t *testing.T) {
	r := &registrar{next: time.Now().Add(time.Hour)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.wait(ctx); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestExitRejectsWrongFamilyWithoutDialing(t *testing.T) {
	e := &Exit{family: IPv6}
	if _, err := e.resolve(context.Background(), M.ParseSocksaddr("1.1.1.1:443")); err == nil {
		t.Fatal("IPv6 exit accepted IPv4 literal")
	}
}

type traceRoundTripper func(*http.Request) (*http.Response, error)

func (f traceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type traceResponseBody struct {
	io.Reader
	onRead func()
}

func (b *traceResponseBody) Read(p []byte) (int, error) {
	if b.onRead != nil {
		b.onRead()
		b.onRead = nil
	}
	return b.Reader.Read(p)
}

func TestExitLatencyUsesSuccessfulTLSHandshake(t *testing.T) {
	clock := time.Unix(1000, 0)
	now := func() time.Time { return clock }
	client := &http.Client{Transport: traceRoundTripper(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		if trace == nil || trace.TLSHandshakeStart == nil || trace.TLSHandshakeDone == nil {
			t.Fatal("verification did not attach TLS timing hooks")
		}
		clock = clock.Add(3 * time.Millisecond) // connection setup
		trace.TLSHandshakeStart()
		clock = clock.Add(5 * time.Millisecond)
		trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		body := &traceResponseBody{Reader: strings.NewReader("ip=104.28.1.2\nwarp=on\n"), onRead: func() { clock = clock.Add(100 * time.Millisecond) }}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: make(http.Header)}, nil
	})}
	exit := &Exit{family: IPv4}
	if err := exit.verifyWithClock(context.Background(), client, "https://1.1.1.1/cdn-cgi/trace", now); err != nil {
		t.Fatal(err)
	}
	if exit.Latency != 5*time.Millisecond {
		t.Fatalf("latency %s should contain only the 5ms TLS handshake, excluding setup and body", exit.Latency)
	}
}

func TestExitLatencyFallsBackWhenTLSWasNotMeasured(t *testing.T) {
	for _, failedHandshake := range []bool{false, true} {
		t.Run(fmt.Sprint(failedHandshake), func(t *testing.T) {
			clock := time.Unix(1000, 0)
			now := func() time.Time { return clock }
			client := &http.Client{Transport: traceRoundTripper(func(req *http.Request) (*http.Response, error) {
				clock = clock.Add(3 * time.Millisecond)
				if failedHandshake {
					trace := httptrace.ContextClientTrace(req.Context())
					trace.TLSHandshakeStart()
					clock = clock.Add(time.Millisecond)
					trace.TLSHandshakeDone(tls.ConnectionState{}, io.ErrUnexpectedEOF)
				}
				clock = clock.Add(7 * time.Millisecond)
				body := &traceResponseBody{Reader: strings.NewReader("ip=104.28.1.2\nwarp=on\n"), onRead: func() { clock = clock.Add(100 * time.Millisecond) }}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: make(http.Header)}, nil
			})}
			exit := &Exit{family: IPv4}
			if err := exit.verifyWithClock(context.Background(), client, "https://1.1.1.1/cdn-cgi/trace", now); err != nil {
				t.Fatal(err)
			}
			want := 110 * time.Millisecond
			if failedHandshake {
				want += time.Millisecond
			}
			if exit.Latency != want {
				t.Fatalf("latency %s did not fall back to request duration %s", exit.Latency, want)
			}
		})
	}
}

func TestExitLatencyRemainsPositiveWithinOneClockTick(t *testing.T) {
	for _, successfulHandshake := range []bool{false, true} {
		t.Run(fmt.Sprint(successfulHandshake), func(t *testing.T) {
			clock := time.Unix(1000, 0)
			client := &http.Client{Transport: traceRoundTripper(func(req *http.Request) (*http.Response, error) {
				if successfulHandshake {
					trace := httptrace.ContextClientTrace(req.Context())
					trace.TLSHandshakeStart()
					trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
					// A zero-duration successful handshake must remain a handshake
					// measurement, even if the later response takes much longer.
					clock = clock.Add(time.Second)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ip=104.28.1.2\nwarp=on\n")), Header: make(http.Header)}, nil
			})}
			exit := &Exit{family: IPv4}
			if err := exit.verifyWithClock(context.Background(), client, "https://1.1.1.1/cdn-cgi/trace", func() time.Time { return clock }); err != nil {
				t.Fatal(err)
			}
			if exit.Latency != time.Nanosecond {
				t.Fatalf("zero-tick successful probe has invalid latency %s", exit.Latency)
			}
		})
	}
}

func TestIPv6ExitsPreferIPv6OuterEndpointsWithIPv4Fallback(t *testing.T) {
	v4First := netip.MustParseAddrPort("162.159.192.1:2408")
	v6First := netip.MustParseAddrPort("[2606:4700:d0::1]:2408")
	v4Second := netip.MustParseAddrPort("162.159.193.1:2408")
	v6Second := netip.MustParseAddrPort("[2606:4700:d0::2]:2408")
	mappedV4 := netip.MustParseAddrPort("[::ffff:162.159.192.2]:2408")
	original := []netip.AddrPort{v4First, v6First, v4Second, v6Second, mappedV4}
	unchanged := slices.Clone(original)
	wantIPv6 := []netip.AddrPort{v6First, v6Second, v4First, v4Second, mappedV4}
	if got := orderedEndpoints(IPv6, original); !slices.Equal(got, wantIPv6) {
		t.Fatalf("IPv6 endpoint order = %v, want %v", got, wantIPv6)
	}
	if !slices.Equal(original, unchanged) {
		t.Fatal("IPv6 ordering mutated registration endpoint list")
	}
	if got := orderedEndpoints(IPv4, original); !slices.Equal(got, unchanged) {
		t.Fatalf("IPv4 endpoint order changed: %v", got)
	}
	if got := orderedEndpoints(IPv6, nil); len(got) != 0 {
		t.Fatalf("unexpected endpoints for empty input: %v", got)
	}
}
