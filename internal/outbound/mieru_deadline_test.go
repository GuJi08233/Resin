package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	mierumodel "github.com/enfein/mieru/v3/apis/model"
)

// Model the upstream session's inability to wake blocked I/O on a deadline
// update, so these checks exercise our adapter rather than net.Pipe's timers.
type mieruIgnoredDeadlineConn struct {
	net.Conn
	reads, writes chan struct{}
}

func (c *mieruIgnoredDeadlineConn) Read(p []byte) (int, error) {
	select {
	case c.reads <- struct{}{}:
	default:
	}
	return c.Conn.Read(p)
}
func (c *mieruIgnoredDeadlineConn) Write(p []byte) (int, error) {
	select {
	case c.writes <- struct{}{}:
	default:
	}
	return c.Conn.Write(p)
}
func (*mieruIgnoredDeadlineConn) SetDeadline(time.Time) error      { return nil }
func (*mieruIgnoredDeadlineConn) SetReadDeadline(time.Time) error  { return nil }
func (*mieruIgnoredDeadlineConn) SetWriteDeadline(time.Time) error { return nil }

func mieruDeadlinePipe(t *testing.T) (*mieruTrackedConn, net.Conn, *mieruIgnoredDeadlineConn) {
	t.Helper()
	client, server := net.Pipe()
	ignored := &mieruIgnoredDeadlineConn{Conn: client, reads: make(chan struct{}, 4), writes: make(chan struct{}, 4)}
	lifetime, cancel := context.WithCancel(context.Background())
	owner := &mieruOutbound{ctx: lifetime, cancel: cancel, conns: make(map[*mieruTrackedConn]struct{})}
	tracked := &mieruTrackedConn{Conn: ignored, owner: owner}
	owner.conns[tracked] = struct{}{}
	t.Cleanup(func() { cancel(); server.Close(); tracked.Close() })
	return tracked, server, ignored
}

func TestMieruDeadlineUpdateWakesActiveIO(t *testing.T) {
	for _, direction := range []string{"read", "write"} {
		t.Run(direction, func(t *testing.T) {
			client, _, ignored := mieruDeadlinePipe(t)
			_ = client.SetDeadline(time.Now().Add(time.Hour))
			result := make(chan error, 1)
			started := ignored.reads
			if direction == "read" {
				go func() { _, err := client.Read(make([]byte, 1)); result <- err }()
			} else {
				started = ignored.writes
				go func() { _, err := client.Write([]byte("blocked")); result <- err }()
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("I/O never started")
			}
			if direction == "read" {
				_ = client.SetReadDeadline(time.Now().Add(-time.Second))
			} else {
				_ = client.SetWriteDeadline(time.Now().Add(-time.Second))
			}
			select {
			case err := <-result:
				var networkErr net.Error
				if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &networkErr) || !networkErr.Timeout() {
					t.Fatalf("deadline did not return a recognizable timeout: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("updating deadline left active I/O blocked")
			}
		})
	}
}

func TestMieruDeadlineClearedDuringActiveRead(t *testing.T) {
	client, server, ignored := mieruDeadlinePipe(t)
	result := make(chan error, 1)
	go func() { _, err := client.Read(make([]byte, 1)); result <- err }()
	<-ignored.reads
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_ = client.SetReadDeadline(time.Time{})
	select {
	case err := <-result:
		t.Fatalf("cleared deadline still interrupted the stream: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream was not usable after clearing deadline")
	}
}

func TestMieruDeadlinePersistsAcrossPartialReads(t *testing.T) {
	client, server, _ := mieruDeadlinePipe(t)
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	go func() { _, _ = server.Write([]byte("x")) }()
	result := make(chan error, 1)
	go func() {
		n, err := io.ReadFull(client, make([]byte, 2))
		if n != 1 {
			result <- errors.New("partial response was not received")
			return
		}
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline was cleared by first fragment: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("partial read lost its deadline")
	}
}

func TestMieruNoWaitReadDeadlineInterruptsHandshakeRequestWrite(t *testing.T) {
	client, _, ignored := mieruDeadlinePipe(t)
	early := newMieruEarlyConn(client, client.owner.ctx, &mierumodel.NetAddrSpec{
		Net: "tcp", AddrSpec: mierumodel.AddrSpec{FQDN: "banner.test", Port: 22},
	})
	result := make(chan error, 1)
	go func() { _, err := early.Read(make([]byte, 1)); result <- err }()
	select {
	case <-ignored.writes:
	case <-time.After(time.Second):
		t.Fatal("handshake request was not written")
	}
	_ = early.SetReadDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("read-first deadline error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read deadline did not interrupt its handshake Write")
	}
}
