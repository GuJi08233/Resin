package outbound

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/ws"
	"github.com/sagernet/ws/wsutil"
)

type websocketRegressionDialer struct {
	dial func(context.Context, string, M.Socksaddr) (net.Conn, error)
}

func (d websocketRegressionDialer) DialContext(ctx context.Context, network string, address M.Socksaddr) (net.Conn, error) {
	if d.dial != nil {
		return d.dial(ctx, network, address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, address.String())
}

func (websocketRegressionDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("unexpected direct UDP socket")
}

func awaitWebsocketError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("WebSocket operation did not finish")
		return nil
	}
}

// This is the production failure: the receive loop starts before the lazy
// transport's first write, and shutdown must release it without ever dialing.
func TestEarlyWebsocketReadBeforeWriteClosesAndCancels(t *testing.T) {
	for _, action := range []string{"close", "cancel"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var dialed atomic.Int32
			dialer := websocketRegressionDialer{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				dialed.Add(1)
				return nil, errors.New("transport should remain lazy")
			}}
			conn := newEarlyWebsocketConn(ctx, dialer, M.ParseSocksaddr("127.0.0.1:1"), option.V2RayWebsocketOptions{MaxEarlyData: 2048}, nil)
			defer conn.Close()
			read := make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); read <- err }()
			if action == "close" {
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			if err := awaitWebsocketError(t, read); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("read after %s: %v", action, err)
			}
			if _, err := conn.Write([]byte("after-close")); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("write after %s: %v", action, err)
			}
			if dialed.Load() != 0 {
				t.Fatal("closing the lazy transport opened a socket")
			}
		})
	}
}

func TestEarlyWebsocketReadDeadlineBeforeWrite(t *testing.T) {
	conn := newEarlyWebsocketConn(context.Background(), websocketRegressionDialer{}, M.ParseSocksaddr("127.0.0.1:1"), option.V2RayWebsocketOptions{MaxEarlyData: 2048}, nil)
	defer conn.Close()
	read := make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); read <- err }()
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := awaitWebsocketError(t, read); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("pending read did not honor updated deadline: %v", err)
	}
}

func TestEarlyWebsocketCloseDuringHTTPUpgrade(t *testing.T) {
	for _, action := range []string{"close", "cancel", "deadline"} {
		t.Run(action, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := newEarlyWebsocketConn(ctx, websocketRegressionDialer{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				return client, nil
			}}, M.ParseSocksaddr("127.0.0.1:1"), option.V2RayWebsocketOptions{Path: "/tunnel", MaxEarlyData: 3}, nil)
			defer conn.Close()
			request := make(chan error, 1)
			peerClosed := make(chan error, 1)
			go func() {
				_, err := http.ReadRequest(bufio.NewReader(peer))
				request <- err
				_, err = peer.Read(make([]byte, 1))
				peerClosed <- err
			}()
			write := make(chan error, 1)
			go func() { _, err := conn.Write([]byte("payload")); write <- err }()
			if err := awaitWebsocketError(t, request); err != nil {
				t.Fatalf("upgrade request: %v", err)
			}
			switch action {
			case "close":
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			case "deadline":
				if err := conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			}
			if err := awaitWebsocketError(t, write); err == nil {
				t.Fatal("interrupted HTTP upgrade succeeded")
			}
			if err := awaitWebsocketError(t, peerClosed); err == nil {
				t.Fatal("interrupted upgrade left its physical socket open")
			}
		})
	}
}

func TestEarlyWebsocketPreservesEarlyDataAndPayload(t *testing.T) {
	for _, header := range []string{"", "X-Early-Data", "Sec-WebSocket-Protocol"} {
		t.Run("header="+header, func(t *testing.T) {
			payload := []byte("\xff\x00protocol-first-packet")
			const earlyLength = 5
			serverResult := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				encoded := r.Header.Get(header)
				if header == "" {
					encoded = strings.TrimPrefix(r.URL.Path, "/tunnel/")
				} else if r.URL.Path != "/tunnel/" {
					serverResult <- fmt.Errorf("early-data header changed path: %s", r.URL.Path)
					return
				}
				early, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil || !bytes.Equal(early, payload[:earlyLength]) {
					serverResult <- fmt.Errorf("early data: %q, %v", early, err)
					return
				}
				if r.Host != "front.test" || r.Header.Get("X-Configured") != "kept" {
					serverResult <- fmt.Errorf("request options changed: host=%s headers=%v", r.Host, r.Header)
					return
				}
				peer, rw, _, err := ws.UpgradeHTTP(r, w)
				if err != nil {
					serverResult <- err
					return
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
				readerWriter := struct {
					io.Reader
					io.Writer
				}{rw.Reader, peer}
				late, err := wsutil.ReadClientBinary(readerWriter)
				if err != nil || !bytes.Equal(late, payload[earlyLength:]) {
					serverResult <- fmt.Errorf("remaining payload: %q, %v", late, err)
					return
				}
				later, err := wsutil.ReadClientBinary(readerWriter)
				if err != nil || string(later) != "second-packet" {
					serverResult <- fmt.Errorf("subsequent payload: %q, %v", later, err)
					return
				}
				serverResult <- wsutil.WriteServerBinary(peer, append(early, append(late, later...)...))
			}))
			defer server.Close()
			conn := newEarlyWebsocketConn(context.Background(), websocketRegressionDialer{}, M.ParseSocksaddr(strings.TrimPrefix(server.URL, "http://")), option.V2RayWebsocketOptions{
				Path: "/tunnel/", MaxEarlyData: earlyLength, EarlyDataHeaderName: header,
				Headers: badoption.HTTPHeader{"Host": {"front.test"}, "X-Configured": {"kept"}},
			}, nil)
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if n, err := conn.Write(payload); err != nil || n != len(payload) {
				select {
				case serverErr := <-serverResult:
					t.Fatalf("first write=%d, %v; server: %v", n, err, serverErr)
				default:
				}
				t.Fatalf("first write=%d, %v", n, err)
			}
			if n, err := conn.Write([]byte("second-packet")); err != nil || n != len("second-packet") {
				t.Fatalf("second write=%d, %v", n, err)
			}
			want := append(append([]byte(nil), payload...), []byte("second-packet")...)
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("response=%q, %v; want %q", got, err, want)
			}
			if err := awaitWebsocketError(t, serverResult); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBuilderWebsocketPacketReadBeforeWriteCloses(t *testing.T) {
	builder := newTestSingboxBuilder(t)
	defer builder.Close()
	for _, protocol := range []string{"vmess", "vless", "trojan"} {
		t.Run(protocol, func(t *testing.T) {
			options := map[string]any{
				"type": protocol, "tag": protocol + "-early-ws", "server": "127.0.0.1", "server_port": 1,
				"transport": map[string]any{"type": "ws", "path": "/tunnel", "max_early_data": 2048},
			}
			if protocol == "trojan" {
				options["password"] = "local-test-password"
			} else {
				options["uuid"] = "00000000-0000-0000-0000-000000000001"
				options["packet_encoding"] = ""
			}
			if protocol == "vmess" {
				options["security"] = "auto"
				options["packet_encoding"] = ""
			}
			raw, err := json.Marshal(options)
			if err != nil {
				t.Fatal(err)
			}
			outbound, err := builder.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer closeOutbound(outbound)
			if _, ok := outbound.(*websocketCompatOutbound); !ok {
				t.Fatalf("early WebSocket node bypassed safe transport: %T", outbound)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			packet, err := outbound.ListenPacket(ctx, M.ParseSocksaddr("162.159.192.1:2408"))
			if err != nil {
				t.Fatalf("lazy UDP connection should not dial: %v", err)
			}
			defer packet.Close()
			read := make(chan error, 1)
			go func() { _, _, err := packet.ReadFrom(make([]byte, 1500)); read <- err }()
			if err := packet.Close(); err != nil {
				t.Fatal(err)
			}
			if err := awaitWebsocketError(t, read); err == nil {
				t.Fatal("packet read succeeded after close")
			}
		})
	}
}

// Like net.DialContext, cancellation bounds establishment, not the lifetime
// of an already established socket returned to a caller or multiplexed session.
func TestEarlyWebsocketEstablishedConnectionSurvivesDialCancellation(t *testing.T) {
	serverResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, rw, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			serverResult <- err
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
		readerWriter := struct {
			io.Reader
			io.Writer
		}{rw.Reader, peer}
		for _, want := range []string{"first-packet", "after-cancel"} {
			got, err := wsutil.ReadClientBinary(readerWriter)
			if err != nil || string(got) != want {
				serverResult <- fmt.Errorf("payload after establishment: got %q, %v; want %q", got, err, want)
				return
			}
			if err := wsutil.WriteServerBinary(peer, got); err != nil {
				serverResult <- err
				return
			}
		}
		serverResult <- nil
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Zero early bytes isolates the connection's lifetime from protocol framing.
	conn := newEarlyWebsocketConn(ctx, websocketRegressionDialer{}, M.ParseSocksaddr(strings.TrimPrefix(server.URL, "http://")), option.V2RayWebsocketOptions{Path: "/tunnel"}, nil)
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"first-packet", "after-cancel"} {
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatalf("write %q: %v", payload, err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != payload {
			t.Fatalf("read %q: got %q, %v", payload, got, err)
		}
		cancel()
	}
	if err := awaitWebsocketError(t, serverResult); err != nil {
		t.Fatal(err)
	}
}

func TestEarlyWebsocketUpgradeDeadlineCanBeExtendedOrCleared(t *testing.T) {
	for _, action := range []string{"extend", "clear"} {
		t.Run(action, func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			conn := newEarlyWebsocketConn(context.Background(), websocketRegressionDialer{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				return client, nil
			}}, M.ParseSocksaddr("127.0.0.1:1"), option.V2RayWebsocketOptions{Path: "/tunnel", MaxEarlyData: 3}, nil)
			defer conn.Close()
			request := make(chan error, 1)
			release := make(chan struct{})
			server := make(chan error, 1)
			go func() {
				r, err := http.ReadRequest(bufio.NewReader(peer))
				request <- err
				if err != nil {
					server <- err
					return
				}
				<-release
				accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				_, err = fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
				server <- err
			}()
			oldDeadline := time.Now().Add(100 * time.Millisecond)
			if err := conn.SetWriteDeadline(oldDeadline); err != nil {
				t.Fatal(err)
			}
			write := make(chan error, 1)
			go func() { _, err := conn.Write([]byte("abc")); write <- err }()
			if err := awaitWebsocketError(t, request); err != nil {
				close(release)
				t.Fatal(err)
			}
			var replacement time.Time
			if action == "extend" {
				replacement = time.Now().Add(3 * time.Second)
			}
			if err := conn.SetWriteDeadline(replacement); err != nil {
				close(release)
				t.Fatal(err)
			}
			// Wait beyond the replaced deadline before allowing the upgrade.
			timer := time.NewTimer(time.Until(oldDeadline.Add(20 * time.Millisecond)))
			<-timer.C
			close(release)
			if err := awaitWebsocketError(t, write); err != nil {
				t.Fatalf("%s deadline left the original timeout active: %v", action, err)
			}
			if err := awaitWebsocketError(t, server); err != nil {
				t.Fatal(err)
			}
		})
	}
}
