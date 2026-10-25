package outbound

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/enfein/mieru/v3/apis/constant"
	mierumodel "github.com/enfein/mieru/v3/apis/model"
)

// The convenience client has a non-cancellable handshake and waits for its
// handshake mutex before Close. Use its public wire model over the official
// mux, closing only this stream when a handshake is cancelled.
func mieruHandshake(ctx context.Context, conn net.Conn, destination *mierumodel.NetAddrSpec, early []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	command := constant.Socks5ConnectCmd
	if destination.Net == "udp" {
		command = constant.Socks5UDPAssociateCmd
	}
	request := &mierumodel.Request{Command: command, DstAddr: destination.AddrSpec}
	var buffer bytes.Buffer
	if err := request.WriteToSocks5(&buffer); err != nil {
		return err
	}
	buffer.Write(early)
	if _, err := io.Copy(conn, &buffer); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("write Mieru handshake: %w", err)
	}
	var response mierumodel.Response
	if err := response.ReadFromSocks5(conn); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read Mieru handshake: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if response.Reply != constant.Socks5ReplySuccess {
		return fmt.Errorf("Mieru server rejected connection with SOCKS5 code %d", response.Reply)
	}
	return nil
}

type mieruEarlyConn struct {
	*mieruTrackedConn
	lifetime                    context.Context
	target                      *mierumodel.NetAddrSpec
	once                        sync.Once
	err                         error
	deadlineMu                  sync.Mutex
	readDeadline, writeDeadline time.Time
	handshakeActive             bool
	handshakeExpired            bool
	deadlineTimer               *time.Timer
	deadlineGeneration          uint64
}

func newMieruEarlyConn(conn *mieruTrackedConn, lifetime context.Context, target *mierumodel.NetAddrSpec) *mieruEarlyConn {
	return &mieruEarlyConn{mieruTrackedConn: conn, lifetime: lifetime, target: target}
}

func (c *mieruEarlyConn) Read(p []byte) (int, error) {
	// Server-first protocols must be able to receive their banner without a
	// prior Write. The first operation owns the handshake; a first Write can
	// still combine its payload with the request.
	c.once.Do(func() {
		c.err = c.handshake(nil)
		if c.err != nil {
			_ = c.mieruTrackedConn.Close()
		}
	})
	if c.err != nil {
		return 0, c.err
	}
	return c.mieruTrackedConn.Read(p)
}

func (c *mieruEarlyConn) Write(p []byte) (int, error) {
	first := false
	c.once.Do(func() {
		first = true
		c.err = c.handshake(p)
		if c.err != nil {
			_ = c.mieruTrackedConn.Close()
		}
	})
	if c.err != nil {
		return 0, c.err
	}
	if first {
		return len(p), nil
	}
	return c.mieruTrackedConn.Write(p)
}

func (c *mieruEarlyConn) handshake(early []byte) error {
	c.deadlineMu.Lock()
	c.handshakeActive, c.handshakeExpired = true, false
	c.armHandshakeDeadlineLocked()
	c.deadlineMu.Unlock()
	err := mieruHandshake(c.lifetime, c.mieruTrackedConn, c.target, early)
	c.deadlineMu.Lock()
	expired := c.handshakeExpired
	c.handshakeActive = false
	c.armHandshakeDeadlineLocked()
	c.deadlineMu.Unlock()
	if expired {
		return os.ErrDeadlineExceeded
	}
	return err
}

// A Read-first handshake can be writing its request when SetReadDeadline is
// called. Enforce the operation's deadline across both handshake directions,
// including later updates and clearing, rather than taking a stale snapshot.
func (c *mieruEarlyConn) armHandshakeDeadlineLocked() {
	c.deadlineGeneration++
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
		c.deadlineTimer = nil
	}
	deadline := c.readDeadline
	if !c.writeDeadline.IsZero() && (deadline.IsZero() || c.writeDeadline.Before(deadline)) {
		deadline = c.writeDeadline
	}
	if !c.handshakeActive || deadline.IsZero() {
		return
	}
	generation := c.deadlineGeneration
	c.deadlineTimer = time.AfterFunc(time.Until(deadline), func() {
		c.deadlineMu.Lock()
		if !c.handshakeActive || c.deadlineGeneration != generation {
			c.deadlineMu.Unlock()
			return
		}
		c.handshakeExpired = true
		c.deadlineMu.Unlock()
		_ = c.mieruTrackedConn.Close()
	})
}

func (c *mieruEarlyConn) SetDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline, c.writeDeadline = deadline, deadline
	c.armHandshakeDeadlineLocked()
	c.deadlineMu.Unlock()
	return c.mieruTrackedConn.SetDeadline(deadline)
}

func (c *mieruEarlyConn) SetReadDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = deadline
	c.armHandshakeDeadlineLocked()
	c.deadlineMu.Unlock()
	return c.mieruTrackedConn.SetReadDeadline(deadline)
}

func (c *mieruEarlyConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.writeDeadline = deadline
	c.armHandshakeDeadlineLocked()
	c.deadlineMu.Unlock()
	return c.mieruTrackedConn.SetWriteDeadline(deadline)
}
