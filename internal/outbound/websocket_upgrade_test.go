package outbound

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	singtls "github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/ws"
	"github.com/sagernet/ws/wsutil"
)

func TestWebsocketUpgradePreservesCoalescedFirstFrame(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	serverResult := make(chan error, 1)
	payload := "immediate-websocket-data"
	go func() {
		request, err := http.ReadRequest(bufio.NewReader(peer))
		if err != nil {
			serverResult <- err
			return
		}
		if request.Host != "front.example" || request.URL.EscapedPath() != "/encoded%2Fpath" || request.Header.Get("X-Transport") != "preserved" || request.Header.Get("Sec-WebSocket-Protocol") != "resin" || request.Header.Get("User-Agent") == "" {
			serverResult <- fmt.Errorf("upgrade changed headers or escaped URL: host=%s path=%s headers=%v", request.Host, request.URL.EscapedPath(), request.Header)
			return
		}
		accept := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		response := fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: resin\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
		// Exactly one net.Pipe write forces the HTTP parser to buffer the
		// complete first binary frame together with the 101 response.
		packet := append([]byte(response), 0x82, byte(len(payload)))
		packet = append(packet, payload...)
		_, err = peer.Write(packet)
		serverResult <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialWebsocketUpgrade(ctx, websocketRegressionDialer{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		return client, nil
	}}, M.ParseSocksaddr("127.0.0.1:1"), option.V2RayWebsocketOptions{
		Path: "/encoded%2Fpath", Headers: badoption.HTTPHeader{
			"Host": {"front.example"}, "X-Transport": {"preserved"}, "Sec-WebSocket-Protocol": {"resin"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWebsocketSocket(conn)
	cancel() // the established socket must outlive the upgrade context
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("first server frame was lost after HTTP upgrade: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("first server frame = %q, want %q", got, payload)
	}
	if err := awaitWebsocketError(t, serverResult); err != nil {
		t.Fatal(err)
	}
}

func TestWebsocketUpgradeTLSRoundTripAndCertificateValidation(t *testing.T) {
	for _, insecure := range []bool{true, false} {
		t.Run(fmt.Sprintf("insecure=%t", insecure), func(t *testing.T) {
			var upgraded atomic.Bool
			serverResult := make(chan error, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				upgraded.Store(true)
				if request.TLS == nil || request.TLS.NegotiatedProtocol != "http/1.1" || request.TLS.ServerName != "example.com" || request.Host != "front.example" || request.URL.Path != "/secure-tunnel" {
					serverResult <- fmt.Errorf("WSS handshake changed TLS or HTTP options: tls=%+v host=%s path=%s", request.TLS, request.Host, request.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				peer, _, _, err := ws.UpgradeHTTP(request, w)
				if err != nil {
					serverResult <- err
					return
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
				message, operation, err := wsutil.ReadClientData(peer)
				if err == nil {
					err = wsutil.WriteServerMessage(peer, operation, message)
				}
				serverResult <- err
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.StartTLS()
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			config, err := singtls.NewClient(ctx, "example.com", option.OutboundTLSOptions{Enabled: true, Insecure: insecure})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := dialWebsocketUpgrade(ctx, websocketRegressionDialer{}, M.ParseSocksaddr(server.Listener.Addr().String()),
				option.V2RayWebsocketOptions{Path: "/secure-tunnel", Headers: badoption.HTTPHeader{"Host": {"front.example"}}}, config)
			if !insecure {
				if err == nil || conn != nil || upgraded.Load() {
					if conn != nil {
						closeWebsocketSocket(conn)
					}
					t.Fatalf("untrusted TLS certificate was accepted: conn=%v err=%v upgraded=%t", conn, err, upgraded.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer closeWebsocketSocket(conn)
			if len(config.NextProtos()) != 0 {
				t.Fatal("default WebSocket ALPN mutated the caller's shared TLS config")
			}
			payload := []byte("encrypted-websocket-roundtrip")
			if _, err := conn.Write(payload); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, response); err != nil || string(response) != string(payload) {
				t.Fatalf("WSS round trip returned %q: %v", response, err)
			}
			if err := awaitWebsocketError(t, serverResult); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWebsocketUpgradeFailureClosesPhysicalSocket(t *testing.T) {
	for _, failure := range []string{"reject", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverResult := make(chan error, 1)
			go func() {
				if _, err := http.ReadRequest(bufio.NewReader(peer)); err != nil {
					serverResult <- err
					return
				}
				if failure == "reject" {
					if _, err := io.WriteString(peer, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"); err != nil {
						serverResult <- err
						return
					}
				} else {
					cancel()
				}
				_, err := peer.Read(make([]byte, 1))
				if errors.Is(err, io.EOF) {
					serverResult <- nil
				} else {
					serverResult <- fmt.Errorf("upgrade failure left physical socket open: %w", err)
				}
			}()
			conn, err := dialWebsocketUpgrade(ctx, websocketRegressionDialer{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				return client, nil
			}}, M.ParseSocksaddr("127.0.0.1:1"), option.V2RayWebsocketOptions{Path: "/"}, nil)
			if conn != nil || err == nil {
				t.Fatalf("failed upgrade returned conn=%v err=%v", conn, err)
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled upgrade returned %v", err)
			}
			if err := awaitWebsocketError(t, serverResult); err != nil {
				t.Fatal(err)
			}
		})
	}
}
