package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/enfein/mieru/v3/apis/constant"
	mierumodel "github.com/enfein/mieru/v3/apis/model"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
)

func TestMieruProfileDefaultsAndEnums(t *testing.T) {
	options := mieruOutboundOptions{ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: 2012}, Username: "local-user", Password: "local-password"}
	profile, err := mieruProfile(options)
	if err != nil {
		t.Fatal(err)
	}
	if profile.GetMultiplexing().GetLevel() != pb.MultiplexingLevel_MULTIPLEXING_LOW ||
		profile.GetHandshakeMode() != pb.HandshakeMode_HANDSHAKE_STANDARD ||
		profile.GetServers()[0].GetPortBindings()[0].GetProtocol() != pb.TransportProtocol_TCP {
		t.Fatal("defaults do not match the official Mihomo configuration")
	}
	options.Transport, options.HandshakeMode, options.Multiplexing = "UDP", "HANDSHAKE_NO_WAIT", "MULTIPLEXING_HIGH"
	profile, err = mieruProfile(options)
	if err != nil || profile.GetHandshakeMode() != pb.HandshakeMode_HANDSHAKE_NO_WAIT ||
		profile.GetMultiplexing().GetLevel() != pb.MultiplexingLevel_MULTIPLEXING_HIGH ||
		profile.GetServers()[0].GetPortBindings()[0].GetProtocol() != pb.TransportProtocol_UDP {
		t.Fatalf("explicit Mieru settings lost: error=%v", err)
	}
}

func TestMieruBuildRejectsInvalidSettingsWithoutCredentialLeaks(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	base := map[string]any{"type": "mieru", "server": "127.0.0.1", "server_port": 2012, "username": "test-user", "password": "unit-private-password"}
	for _, change := range []struct {
		key   string
		value any
	}{
		{"transport", "invalid"}, {"handshake_mode", "unsupported"}, {"multiplexing", "unsupported"},
		{"username", ""}, {"password", ""}, {"server", ""}, {"server_port", 0},
	} {
		t.Run(change.key, func(t *testing.T) {
			values := make(map[string]any, len(base))
			for key, value := range base {
				values[key] = value
			}
			values[change.key] = change.value
			raw, _ := json.Marshal(values)
			_, err := b.Build(raw)
			if err == nil || strings.Contains(err.Error(), "unit-private-password") {
				t.Fatalf("invalid settings were accepted or leaked credentials: %v", err)
			}
		})
	}
	raw, _ := json.Marshal(base)
	ob, err := b.Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ob.Type() != "mieru" || strings.Join(ob.Network(), ",") != "tcp,udp" {
		t.Fatalf("Mieru must expose real TCP and UDP capability: %s %v", ob.Type(), ob.Network())
	}
	if err := ob.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.DialContext(context.Background(), "tcp", M.ParseSocksaddr("destination.test:443")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed outbound allowed a new stream: %v", err)
	}
}

func TestMieruMissingDetourDoesNotFallBackToDirect(t *testing.T) {
	b := newTestSingboxBuilder(t)
	defer b.Close()
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"type": "mieru", "server": "127.0.0.1", "server_port": 2012,
				"username": "test-only", "password": "test-only-password",
				"transport": transport, "detour": "not-registered",
			})
			outbound, err := b.Build(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer outbound.(io.Closer).Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = outbound.DialContext(ctx, "tcp", M.ParseSocksaddr("destination.test:443"))
			if err == nil || !strings.Contains(err.Error(), "outbound detour not found") {
				t.Fatalf("missing detour did not fail explicitly: %v", err)
			}
		})
	}
}

func TestMieruHandshakeCancelAfterPartialResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := &mierumodel.NetAddrSpec{Net: "tcp", AddrSpec: mierumodel.AddrSpec{FQDN: "target.test", Port: 443}}
	partial := make(chan error, 1)
	go func() {
		var request mierumodel.Request
		if err := request.ReadFromSocks5(server); err != nil {
			partial <- err
			return
		}
		_, err := server.Write([]byte{constant.Socks5Version})
		partial <- err
	}()
	result := make(chan error, 1)
	go func() { result <- mieruHandshake(ctx, client, destination, nil) }()
	select {
	case err := <-partial:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handshake did not reach the partial response")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not close the stalled handshake stream")
	}
}

func mieruPipeEarlyConn(t *testing.T) (*mieruEarlyConn, net.Conn, *mieruOutbound) {
	t.Helper()
	client, server := net.Pipe()
	lifetime, cancel := context.WithCancel(context.Background())
	owner := &mieruOutbound{ctx: lifetime, cancel: cancel, conns: make(map[*mieruTrackedConn]struct{})}
	tracked := &mieruTrackedConn{Conn: client, owner: owner}
	owner.conns[tracked] = struct{}{}
	early := newMieruEarlyConn(tracked, lifetime, &mierumodel.NetAddrSpec{Net: "tcp", AddrSpec: mierumodel.AddrSpec{FQDN: "banner.test", Port: 22}})
	t.Cleanup(func() { cancel(); _ = server.Close(); _ = tracked.Close() })
	return early, server, owner
}

func TestMieruNoWaitFirstReadSupportsServerBanner(t *testing.T) {
	early, server, _ := mieruPipeEarlyConn(t)
	serverResult := make(chan error, 1)
	go func() {
		var request mierumodel.Request
		if err := request.ReadFromSocks5(server); err != nil {
			serverResult <- err
			return
		}
		if request.Command != constant.Socks5ConnectCmd || request.DstAddr.FQDN != "banner.test" {
			serverResult <- errors.New("wrong banner request")
			return
		}
		if err := mierumodel.WriteSocks5Response(server, constant.Socks5ReplySuccess, mierumodel.AddrSpec{IP: net.IPv4zero, Port: 0}); err != nil {
			serverResult <- err
			return
		}
		_, err := server.Write([]byte("SSH-banner"))
		serverResult <- err
	}()
	result := make(chan error, 1)
	go func() {
		buffer := make([]byte, len("SSH-banner"))
		_, err := io.ReadFull(early, buffer)
		if err == nil && string(buffer) != "SSH-banner" {
			err = errors.New("server banner changed")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server-first connection waited for a client Write")
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestMieruNoWaitCloseInterruptsConcurrentHandshake(t *testing.T) {
	early, server, owner := mieruPipeEarlyConn(t)
	partial := make(chan error, 1)
	go func() {
		var request mierumodel.Request
		if err := request.ReadFromSocks5(server); err != nil {
			partial <- err
			return
		}
		payload := make([]byte, len("early-payload"))
		if _, err := io.ReadFull(server, payload); err != nil {
			partial <- err
			return
		}
		if string(payload) != "early-payload" {
			partial <- errors.New("early payload was not coalesced")
			return
		}
		_, err := server.Write([]byte{constant.Socks5Version})
		partial <- err
	}()
	writeResult := make(chan error, 1)
	go func() { _, err := early.Write([]byte("early-payload")); writeResult <- err }()
	select {
	case err := <-partial:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("early payload did not arrive before the server handshake reply")
	}
	readResult := make(chan error, 1)
	go func() { _, err := early.Read(make([]byte, 1)); readResult <- err }()
	closed := make(chan error, 1)
	go func() { closed <- early.Close() }()
	for _, result := range []<-chan error{closed, writeResult, readResult} {
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("Close waited for a stalled early handshake mutex")
		}
	}
	owner.mu.Lock()
	remaining := len(owner.conns)
	owner.mu.Unlock()
	if remaining != 0 {
		t.Fatal("closed early connection remained registered")
	}
}
