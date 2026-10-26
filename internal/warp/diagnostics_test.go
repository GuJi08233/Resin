package warp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

type diagnosticTestEndpoint struct {
	state  tunnelDiagnostics
	closed int
}

func (e *diagnosticTestEndpoint) diagnostics() tunnelDiagnostics { return e.state }
func (e *diagnosticTestEndpoint) Close() error                   { e.closed++; return nil }
func (*diagnosticTestEndpoint) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, errors.New("unexpected diagnostic test dial")
}
func (*diagnosticTestEndpoint) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("unexpected diagnostic test packet socket")
}

type diagnosticErrorReader struct{ err error }

func (r diagnosticErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestTunnelDiagnosticsOnlyRetainsAllowedNumbers(t *testing.T) {
	raw := "private_key=SECRET-PRIVATE\npreshared_key=SECRET-PSK\npublic_key=SECRET-PUBLIC\n" +
		"sessionid=SECRET-SESSION\ntoken=SECRET-TOKEN\nlast_handshake_time_sec=1700000000\n" +
		"last_handshake_time_nsec=999999999\nrx_bytes=123\ntx_bytes=456\n" +
		"public_key=SECRET-SECOND\nlast_handshake_time_sec=1700000010\nrx_bytes=7\ntx_bytes=8\n" +
		"rx_bytes=not-a-number\ntx_bytes=-1\nlast_handshake_time_sec=-2\n"
	state := parseTunnelDiagnostics(raw)
	if !state.available || state.handshakeUnix != 1700000010 || state.rxBytes != 130 || state.txBytes != 464 {
		t.Fatalf("allowed IPC counters parsed incorrectly: %+v", state)
	}
	exit := &Exit{endpoint: &diagnosticTestEndpoint{state: state}}
	message := exit.diagnosticError("tcp_connect", context.DeadlineExceeded).Error()
	for _, secret := range []string{"SECRET", "private_key", "preshared_key", "public_key", "sessionid", "token", "999999999"} {
		if strings.Contains(message, secret) {
			t.Fatalf("diagnostic exposed excluded IPC field %q: %s", secret, message)
		}
	}
	for _, selected := range []string{"wg_handshake=complete", "handshake_unix=1700000010", "rx_bytes=130", "tx_bytes=464"} {
		if !strings.Contains(message, selected) {
			t.Fatalf("diagnostic omitted %q: %s", selected, message)
		}
	}
}

func TestExitVerificationReportsFailurePhaseAndHandshake(t *testing.T) {
	for _, phase := range []string{"tcp_connect", "tls_handshake", "http_headers", "http_body"} {
		for _, handshake := range []int64{0, 1700000000} {
			t.Run(fmt.Sprintf("%s/%d", phase, handshake), func(t *testing.T) {
				failure := context.DeadlineExceeded
				endpoint := &diagnosticTestEndpoint{state: tunnelDiagnostics{available: true, handshakeUnix: handshake, rxBytes: 42, txBytes: 64}}
				exit := &Exit{family: IPv4, endpoint: endpoint}
				client := &http.Client{Transport: traceRoundTripper(func(request *http.Request) (*http.Response, error) {
					trace := httptrace.ContextClientTrace(request.Context())
					if phase == "tcp_connect" {
						return nil, failure
					}
					trace.TLSHandshakeStart()
					if phase == "tls_handshake" {
						return nil, failure
					}
					trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
					trace.GotConn(httptrace.GotConnInfo{})
					trace.WroteRequest(httptrace.WroteRequestInfo{})
					if phase == "http_headers" {
						return nil, failure
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(diagnosticErrorReader{failure})}, nil
				})}
				err := exit.verifyWithClient(context.Background(), client, "https://1.1.1.1/cdn-cgi/trace")
				if err == nil || !errors.Is(err, failure) {
					t.Fatalf("lost the original failure: %v", err)
				}
				status := "none"
				if handshake > 0 {
					status = "complete"
				}
				for _, want := range []string{"phase=" + phase, "wg_handshake=" + status, "rx_bytes=42", "tx_bytes=64"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("missing %q in %s", want, err)
					}
				}
			})
		}
	}
}

func TestFactoryPreservesEndpointFailureWhenOverallContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint := &diagnosticTestEndpoint{state: tunnelDiagnostics{available: true}}
	first := netip.MustParseAddrPort("162.159.192.1:2408")
	reg := registration{id: "SECRET-DEVICE", privateKey: "SECRET-PRIVATE", peerKey: "SECRET-PUBLIC",
		addresses: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")},
		endpoints: []netip.AddrPort{first, netip.MustParseAddrPort("162.159.193.1:2408")}}
	opens := 0
	factory := &Factory{
		openTunnel: func(context.Context, adapter.Outbound, Family, registration, []netip.Prefix, netip.AddrPort) (*Exit, error) {
			opens++
			return &Exit{endpoint: endpoint, cancel: func() {}}, nil
		},
		verifyExit: func(probeCtx context.Context, exit *Exit) error {
			deadline, ok := probeCtx.Deadline()
			if !ok || time.Until(deadline) > 8*time.Second {
				t.Fatal("endpoint probe lost its eight-second upper bound")
			}
			cancel()
			return exit.diagnosticError("tcp_connect", context.DeadlineExceeded)
		},
	}
	_, err := factory.createRegistered(ctx, nil, IPv4, reg)
	if err == nil || !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overall cancellation lost endpoint/timeout causes: %v", err)
	}
	for _, want := range []string{"attempts=1", "endpoint=" + first.String(), "phase=tcp_connect", "wg_handshake=none"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing endpoint context %q: %v", want, err)
		}
	}
	if opens != 1 || endpoint.closed != 1 || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("cancelled attempts/resources or secret isolation failed: opens=%d closes=%d error=%v", opens, endpoint.closed, err)
	}
}

func TestFactoryBoundsAggregatedEndpointFailures(t *testing.T) {
	reg := registration{addresses: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")}}
	for index := 1; index <= 20; index++ {
		reg.endpoints = append(reg.endpoints, netip.MustParseAddrPort(fmt.Sprintf("162.159.192.%d:2408", index)))
	}
	factory := &Factory{openTunnel: func(_ context.Context, _ adapter.Outbound, _ Family, _ registration, _ []netip.Prefix, _ netip.AddrPort) (*Exit, error) {
		return nil, errors.New("mock endpoint creation failure")
	}}
	_, err := factory.createRegistered(context.Background(), nil, IPv4, reg)
	if err == nil {
		t.Fatal("all failed endpoints unexpectedly succeeded")
	}
	message := err.Error()
	for _, want := range []string{"attempts=20", "endpoint=162.159.192.1:2408", "endpoint=162.159.192.20:2408", "phase=tunnel_create"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing bounded aggregate %q: %s", want, message)
		}
	}
	if strings.Count(message, "endpoint=") != 2 {
		t.Fatalf("error repeated every endpoint instead of bounded first/last details: %s", message)
	}
}

func TestFactoryCancelsTunnelSetup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	reg := registration{
		addresses: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")},
		endpoints: []netip.AddrPort{netip.MustParseAddrPort("162.159.192.1:2408")},
	}
	factory := &Factory{openTunnel: func(setupCtx context.Context, _ adapter.Outbound, _ Family, _ registration, _ []netip.Prefix, _ netip.AddrPort) (*Exit, error) {
		close(started)
		<-setupCtx.Done()
		return nil, setupCtx.Err()
	}}
	done := make(chan error, 1)
	go func() {
		_, err := factory.createRegistered(ctx, nil, IPv4, reg)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("factory never entered tunnel setup")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("setup lost attempt cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("attempt cancellation did not interrupt tunnel setup")
	}
}

type diagnosticFailurePacket struct{ recordingPacket }

func (*diagnosticFailurePacket) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, io.ErrUnexpectedEOF
}
func (*diagnosticFailurePacket) WriteTo([]byte, net.Addr) (int, error) {
	return 0, errors.New("parent UDP association refused")
}

func TestExitDiagnosticsIncludeParentPacketIOFailure(t *testing.T) {
	state := new(outerDiagnostics)
	parent := &transportParent{packet: func(context.Context, M.Socksaddr) (net.PacketConn, error) {
		return &diagnosticFailurePacket{}, nil
	}}
	wrapper := &diagnosticParentOutbound{Outbound: parent, state: state}
	packet, err := wrapper.ListenPacket(context.Background(), M.ParseSocksaddr("162.159.192.1:2408"))
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	_, _, _ = packet.ReadFrom(make([]byte, 1))
	exit := &Exit{outer: state}
	err = exit.diagnosticError("tcp_connect", context.DeadlineExceeded)
	if !strings.Contains(err.Error(), `outer_last_op=read outer_last_error="unexpected EOF"`) {
		t.Fatalf("parent read failure not preserved: %v", err)
	}
	_, _ = packet.WriteTo([]byte("test"), M.ParseSocksaddr("162.159.192.1:2408").UDPAddr())
	err = exit.diagnosticError("tcp_connect", context.DeadlineExceeded)
	if !strings.Contains(err.Error(), `outer_last_op=write outer_last_error="parent UDP association refused"`) {
		t.Fatalf("parent write failure not preserved: %v", err)
	}
}
