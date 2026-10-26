package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/ws"
	"github.com/sagernet/ws/wsutil"
)

func TestWebsocketDialContextCancellationDoesNotCloseSharedOrEstablishedConnections(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%v", shared), func(t *testing.T) {
			serverResult := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				peer, rw, _, err := ws.UpgradeHTTP(r, w)
				if err != nil {
					serverResult <- err
					return
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
				payload, err := wsutil.ReadClientBinary(struct {
					io.Reader
					io.Writer
				}{rw.Reader, peer})
				if err != nil || string(payload) != "second" {
					serverResult <- fmt.Errorf("second packet=%q: %w", payload, err)
					return
				}
				serverResult <- wsutil.WriteServerBinary(peer, payload)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &websocketTransportOutbound{
				dialer: websocketRegressionDialer{}, server: M.ParseSocksaddr(strings.TrimPrefix(server.URL, "http://")),
				options: option.V2RayWebsocketOptions{Path: "/", MaxEarlyData: 3, EarlyDataHeaderName: "X-Early-Data"},
				shared:  shared,
			}
			conn, err := transport.DialContext(ctx, "tcp", M.ParseSocksaddr("destination.test:443"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if shared {
				// sing-mux cancels its dial context when returning a newly pooled
				// session, before its asynchronous first transport write.
				cancel()
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if n, err := conn.Write([]byte("one")); err != nil || n != 3 {
				t.Fatalf("first write after shared context cancellation=%d: %v", n, err)
			}
			cancel()
			// Give asynchronous cancellation callbacks a chance to run before
			// testing the established connection's independent lifetime.
			time.Sleep(20 * time.Millisecond)
			if n, err := conn.Write([]byte("second")); err != nil || n != 6 {
				t.Fatalf("established write after dial context cancellation=%d: %v", n, err)
			}
			response := make([]byte, 6)
			if _, err := io.ReadFull(conn, response); err != nil || string(response) != "second" {
				t.Fatalf("established read after dial context cancellation=%q: %v", response, err)
			}
			if err := awaitWebsocketError(t, serverResult); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
		})
	}
}
