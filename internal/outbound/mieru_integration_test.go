package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mierucommon "github.com/enfein/mieru/v3/apis/common"
	mieruconstant "github.com/enfein/mieru/v3/apis/constant"
	mierumodel "github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	M "github.com/sagernet/sing/common/metadata"
	"google.golang.org/protobuf/proto"
)

// The official server handles real Mieru authentication, encryption and
// multiplexing. Its application side echoes data without dialing any target.
type mieruLoopbackServer struct {
	server       mieruserver.Server
	bound        chan net.Addr
	requests     chan *mierumodel.Request
	errors       chan error
	ctx          context.Context
	cancel       context.CancelFunc
	responseMode string
	mu           sync.Mutex
	conns        map[net.Conn]struct{}
	wg           sync.WaitGroup
}

func (s *mieruLoopbackServer) Listen(ctx context.Context, _, _ string) (net.Listener, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err == nil {
		s.bound <- listener.Addr()
	}
	return listener, err
}

func (s *mieruLoopbackServer) ListenPacket(ctx context.Context, _, _ string) (net.PacketConn, error) {
	packet, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp4", "127.0.0.1:0")
	if err == nil {
		s.bound <- packet.LocalAddr()
	}
	return packet, err
}

func newMieruLoopbackServer(t *testing.T, transport string, responseModes ...string) (*mieruLoopbackServer, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &mieruLoopbackServer{
		server: mieruserver.NewServer(), bound: make(chan net.Addr, 1),
		requests: make(chan *mierumodel.Request, 8), errors: make(chan error, 8),
		ctx: ctx, cancel: cancel, conns: make(map[net.Conn]struct{}),
	}
	if len(responseModes) > 0 {
		s.responseMode = responseModes[0]
	}
	protocol := pb.TransportProtocol_TCP
	if transport == "udp" {
		protocol = pb.TransportProtocol_UDP
	}
	if err := s.server.Store(&mieruserver.ServerConfig{
		Config: &pb.ServerConfig{
			ListenIPAddress: proto.String("127.0.0.1"),
			PortBindings:    []*pb.PortBinding{{Port: proto.Int32(8000), Protocol: protocol.Enum()}},
			Users:           []*pb.User{{Name: proto.String("resin-integration"), Password: proto.String("test-only-password")}},
		},
		StreamListenerFactory: s, PacketListenerFactory: s,
	}); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := s.server.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		s.mu.Lock()
		conns := make([]net.Conn, 0, len(s.conns))
		for conn := range s.conns {
			conns = append(conns, conn)
		}
		s.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		done := make(chan error, 1)
		go func() { err := s.server.Stop(); s.wg.Wait(); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop official loopback server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("official loopback server did not stop")
		}
	})
	s.wg.Add(1)
	go s.accept()
	select {
	case bound := <-s.bound:
		_, port, err := net.SplitHostPort(bound.String())
		if err != nil {
			t.Fatal(err)
		}
		var parsed int
		if _, err := fmt.Sscanf(port, "%d", &parsed); err != nil {
			t.Fatal(err)
		}
		return s, parsed
	case <-time.After(3 * time.Second):
		t.Fatal("official loopback server did not bind")
		return nil, 0
	}
}

func (s *mieruLoopbackServer) accept() {
	defer s.wg.Done()
	for {
		conn, request, err := s.server.Accept()
		if err != nil {
			if s.ctx.Err() == nil && s.server.IsRunning() {
				s.report(err)
			}
			return
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.echo(conn, request)
	}
}

func (s *mieruLoopbackServer) echo(conn net.Conn, request *mierumodel.Request) {
	defer s.wg.Done()
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	if s.responseMode != "" && strings.HasPrefix(request.DstAddr.FQDN, "stall-") {
		if s.responseMode == "partial" {
			if _, err := conn.Write([]byte{mieruconstant.Socks5Version}); err != nil {
				s.report(err)
				return
			}
		}
		select {
		case s.requests <- request:
		case <-s.ctx.Done():
			return
		}
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	response := mierumodel.Response{Reply: mieruconstant.Socks5ReplySuccess,
		BindAddr: mierumodel.AddrSpec{IP: net.IPv4(127, 0, 0, 1), Port: 8000}}
	if err := response.WriteToSocks5(conn); err != nil {
		s.report(err)
		return
	}
	select {
	case s.requests <- request:
	case <-s.ctx.Done():
		return
	}
	if request.Command == mieruconstant.Socks5ConnectCmd {
		_, _ = io.Copy(conn, conn)
		return
	}
	if request.Command != mieruconstant.Socks5UDPAssociateCmd {
		s.report(fmt.Errorf("unexpected Mieru SOCKS command %d", request.Command))
		return
	}
	packet := mierucommon.NewUDPAssociateWrapper(mierucommon.NewPacketOverStreamTunnel(conn))
	buf := make([]byte, 65535)
	for {
		n, addr, err := packet.ReadFrom(buf)
		if err != nil {
			return
		}
		if _, err := packet.WriteTo(buf[:n], addr); err != nil {
			return
		}
	}
}

func (s *mieruLoopbackServer) report(err error) {
	if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return
	}
	select {
	case s.errors <- err:
	default:
	}
}

func (s *mieruLoopbackServer) request(t *testing.T) *mierumodel.Request {
	t.Helper()
	select {
	case request := <-s.requests:
		return request
	case err := <-s.errors:
		t.Fatalf("official Mieru server: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("official Mieru server did not receive a request")
	}
	return nil
}

func TestMieruOfficialLoopbackTCPAndUDP(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		for _, handshake := range []string{"HANDSHAKE_STANDARD", "HANDSHAKE_NO_WAIT"} {
			t.Run(transport+"/"+handshake, func(t *testing.T) {
				server, port := newMieruLoopbackServer(t, transport)
				builder := newTestSingboxBuilder(t)
				t.Cleanup(func() { _ = builder.Close() })
				raw, err := json.Marshal(map[string]any{
					"type": "mieru", "tag": "loopback", "server": "127.0.0.1", "server_port": port,
					"username": "resin-integration", "password": "test-only-password",
					"transport": transport, "multiplexing": "MULTIPLEXING_HIGH", "handshake_mode": handshake,
				})
				if err != nil {
					t.Fatal(err)
				}
				outbound, err := builder.Build(raw)
				if err != nil {
					t.Fatal(err)
				}
				o := outbound.(*mieruOutbound)
				t.Cleanup(func() { _ = o.Close() })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				stream, err := o.DialContext(ctx, "tcp", M.ParseSocksaddr("echo-target.invalid:443"))
				if err != nil {
					t.Fatal(err)
				}
				_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
				for _, payload := range [][]byte{[]byte("Mieru encrypted loopback"), bytes.Repeat([]byte{0, 255, 1, 128}, 1024)} {
					if _, err := stream.Write(payload); err != nil {
						t.Fatal(err)
					}
					got := make([]byte, len(payload))
					if _, err := io.ReadFull(stream, got); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, payload) {
						t.Fatal("Mieru TCP payload changed")
					}
				}
				request := server.request(t)
				if request.Command != mieruconstant.Socks5ConnectCmd || request.DstAddr.FQDN != "echo-target.invalid" || request.DstAddr.Port != 443 {
					t.Fatalf("TCP destination was not preserved: %v", request)
				}
				packet, err := o.ListenPacket(ctx, M.ParseSocksaddr("127.0.0.2:5301"))
				if err != nil {
					t.Fatal(err)
				}
				_ = packet.SetDeadline(time.Now().Add(5 * time.Second))
				for index, addr := range []*net.UDPAddr{
					{IP: net.IPv4(127, 0, 0, 2), Port: 5301},
					{IP: net.ParseIP("::1"), Port: 5302},
					{IP: net.IPv4(127, 0, 0, 3), Port: 5303},
				} {
					payload := bytes.Repeat([]byte{byte(index), 255, 0, 128}, 1+index*512)
					if n, err := packet.WriteTo(payload, addr); err != nil || n != len(payload) {
						t.Fatalf("UDP write: n=%d err=%v", n, err)
					}
					buf := make([]byte, len(payload)+16)
					n, source, err := packet.ReadFrom(buf)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(buf[:n], payload) || source.String() != addr.String() {
						t.Fatalf("UDP boundary/payload/address changed: n=%d source=%v want=%v", n, source, addr)
					}
				}
				if request := server.request(t); request.Command != mieruconstant.Socks5UDPAssociateCmd {
					t.Fatalf("UDP command was not preserved: %v", request)
				}
				o.mu.Lock()
				active := len(o.conns)
				o.mu.Unlock()
				if active != 2 {
					t.Fatalf("active tracked sessions=%d, want 2", active)
				}
				closed := make(chan error, 1)
				go func() { closed <- o.Close() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Mieru outbound Close did not release active sessions")
				}
				o.mu.Lock()
				remaining := len(o.conns)
				o.mu.Unlock()
				if remaining != 0 {
					t.Fatalf("Mieru Close retained %d active sessions", remaining)
				}
				if _, err := stream.Read(make([]byte, 1)); err == nil {
					t.Fatal("closed TCP session remained usable")
				}
				if _, err := packet.WriteTo([]byte("closed"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}); err == nil {
					t.Fatal("closed UDP association remained usable")
				}
				if _, err := o.DialContext(ctx, "tcp", M.ParseSocksaddr("unused.invalid:443")); err == nil || !strings.Contains(err.Error(), "closed") {
					t.Fatalf("closed outbound accepted a new stream: %v", err)
				}
			})
		}
	}
}

func TestMieruOfficialLoopbackCanceledHandshakePreservesOtherStream(t *testing.T) {
	for _, responseMode := range []string{"stall", "partial"} {
		t.Run(responseMode, func(t *testing.T) {
			server, port := newMieruLoopbackServer(t, "tcp", responseMode)
			builder := newTestSingboxBuilder(t)
			t.Cleanup(func() { _ = builder.Close() })
			raw, err := json.Marshal(map[string]any{
				"type": "mieru", "server": "127.0.0.1", "server_port": port,
				"username": "resin-integration", "password": "test-only-password",
				"transport": "tcp", "multiplexing": "MULTIPLEXING_HIGH", "handshake_mode": "HANDSHAKE_STANDARD",
			})
			if err != nil {
				t.Fatal(err)
			}
			outbound, err := builder.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			o := outbound.(*mieruOutbound)
			t.Cleanup(func() { _ = o.Close() })
			working, err := o.DialContext(context.Background(), "tcp", M.ParseSocksaddr("echo-target.invalid:443"))
			if err != nil {
				t.Fatal(err)
			}
			echo := func(payload string) {
				t.Helper()
				_ = working.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := working.Write([]byte(payload)); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(payload))
				if _, err := io.ReadFull(working, got); err != nil {
					t.Fatal(err)
				}
				if string(got) != payload {
					t.Fatalf("other shared stream changed payload: %q", got)
				}
			}
			echo("before cancel")
			_ = server.request(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := o.DialContext(ctx, "tcp", M.ParseSocksaddr("stall-target.invalid:443"))
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			request := server.request(t)
			if request.DstAddr.FQDN != "stall-target.invalid" {
				t.Fatalf("unexpected stalled request %v", request)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("handshake cancellation returned %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("canceled stalled handshake did not return")
			}
			echo("after cancel")
			o.mu.Lock()
			active := len(o.conns)
			o.mu.Unlock()
			if active != 1 {
				t.Fatalf("cancellation retained a stalled session or killed another: active=%d", active)
			}
		})
	}
}
