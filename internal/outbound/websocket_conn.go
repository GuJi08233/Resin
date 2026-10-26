package outbound

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/v2raywebsocket"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// A lazy connection has a terminal close state even before it has a socket.
// Reads wait for creation, close, cancellation or a read deadline; Close also
// aborts any physical connection being upgraded concurrently with the first Write.
type earlyWebsocketConn struct {
	ctx              context.Context
	cancel           context.CancelFunc
	dialer           N.Dialer
	server           M.Socksaddr
	options          option.V2RayWebsocketOptions
	tlsConfig        tls.Config
	mu               sync.Mutex
	writeMu          sync.Mutex
	ready            chan struct{}
	changed          chan struct{}
	closed           bool
	complete         bool
	err              error
	conn             net.Conn
	raw              net.Conn
	readDeadline     time.Time
	writeDeadline    time.Time
	opening          bool
	writeTimer       *time.Timer
	closedErr        error
	stopCallerCancel func() bool
}

type websocketCloseMode uint8

const (
	closeWebsocketAlways websocketCloseMode = iota
	closeWebsocketWriteExpired
)

func newEarlyWebsocketConn(ctx context.Context, dialer N.Dialer, server M.Socksaddr, options option.V2RayWebsocketOptions, tlsConfig tls.Config) *earlyWebsocketConn {
	ctx, cancel := context.WithCancel(ctx)
	c := &earlyWebsocketConn{ctx: ctx, cancel: cancel, dialer: dialer, server: server, options: options, tlsConfig: tlsConfig, ready: make(chan struct{}), changed: make(chan struct{}), closedErr: net.ErrClosed}
	c.mu.Lock()
	c.stopCallerCancel = context.AfterFunc(ctx, func() { _ = c.Close() })
	c.mu.Unlock()
	return c
}

func (c *earlyWebsocketConn) finishLocked(err error) {
	if !c.complete {
		if c.writeTimer != nil {
			c.writeTimer.Stop()
		}
		c.opening = false
		c.complete, c.err = true, err
		close(c.ready)
	}
}

func (c *earlyWebsocketConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if c.closed {
			err := c.closedErr
			c.mu.Unlock()
			return 0, err
		}
		if c.complete {
			conn, err := c.conn, c.err
			c.mu.Unlock()
			if err != nil {
				return 0, err
			}
			return conn.Read(p)
		}
		deadline, changed := c.readDeadline, c.changed
		c.mu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case <-c.ready:
		case <-changed:
		case <-c.ctx.Done():
			// The registered cancellation callback or a completed first Write
			// closes ready. Joining that state change avoids closing a socket
			// that another goroutine has just successfully published.
			<-c.ready
		case <-timeout:
			c.mu.Lock()
			expired := !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline)
			c.mu.Unlock()
			if expired {
				return 0, os.ErrDeadlineExceeded
			}
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// trackWebsocketDialer exposes the physical socket to Close before TLS and HTTP
// upgrade finish. This makes cancellation interrupt handshakes, not just dialing.
type trackWebsocketDialer struct {
	N.Dialer
	owner *earlyWebsocketConn
}

func (d trackWebsocketDialer) DialContext(ctx context.Context, network string, address M.Socksaddr) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := d.Dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	d.owner.mu.Lock()
	if d.owner.closed {
		d.owner.mu.Unlock()
		conn.Close()
		return nil, net.ErrClosed
	}
	d.owner.raw = conn
	d.owner.mu.Unlock()
	return conn, nil
}

func (c *earlyWebsocketConn) open(first []byte) (net.Conn, int, error) {
	options := c.options
	options.MaxEarlyData = 0 // the first payload is encoded into this upgrade below
	n := min(len(first), int(c.options.MaxEarlyData))
	if n > 0 {
		encoded := base64.RawURLEncoding.EncodeToString(first[:n])
		if options.EarlyDataHeaderName == "" {
			options.Path += encoded
		} else {
			headers := options.Headers.Build().Clone()
			headers.Set(options.EarlyDataHeaderName, encoded)
			options.Headers = make(badoption.HTTPHeader, len(headers))
			for name, values := range headers {
				options.Headers[name] = values
			}
		}
	}
	conn, err := dialWebsocketUpgrade(c.ctx, trackWebsocketDialer{c.dialer, c}, c.server, options, c.tlsConfig)
	return conn, n, err
}

func (c *earlyWebsocketConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.closed {
		err := c.closedErr
		c.mu.Unlock()
		return 0, err
	}
	if c.complete {
		conn, err := c.conn, c.err
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return conn.Write(p)
	}
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		_ = c.Close()
		return 0, net.ErrClosed
	}
	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		c.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	c.opening = true
	c.armWriteDeadlineLocked()
	c.mu.Unlock()
	conn, n, err := c.open(p)
	c.mu.Lock()
	if c.closed {
		closeErr := c.closedErr
		c.mu.Unlock()
		if conn != nil {
			closeWebsocketSocket(conn)
		}
		return 0, closeErr
	}
	if err != nil {
		raw := c.raw
		c.finishLocked(err)
		c.mu.Unlock()
		if raw != nil {
			raw.Close()
		}
		return 0, err
	}
	// Cancellation is a dialing concern. Established sockets must survive a
	// caller cancelling its completed DialContext (including pooled sessions).
	if c.stopCallerCancel != nil && !c.stopCallerCancel() {
		c.mu.Unlock()
		closeWebsocketSocket(conn)
		_ = c.Close()
		return 0, net.ErrClosed
	}
	c.stopCallerCancel = nil
	c.conn = conn
	readDeadline, writeDeadline := c.readDeadline, c.writeDeadline
	setWebsocketDeadline(conn, readDeadline, writeDeadline)
	c.finishLocked(nil)
	c.mu.Unlock()
	if n < len(p) {
		written, err := conn.Write(p[n:])
		return n + written, err
	}
	return len(p), nil
}

func (c *earlyWebsocketConn) WriteBuffer(buffer *buf.Buffer) error {
	defer buffer.Release()
	_, err := c.Write(buffer.Bytes())
	return err
}

func closeWebsocketSocket(conn net.Conn) error {
	if ws, ok := conn.(*v2raywebsocket.WebsocketConn); ok {
		return ws.Conn.Close()
	}
	return conn.Close()
}

func setWebsocketDeadline(conn net.Conn, read, write time.Time) {
	if ws, ok := conn.(*v2raywebsocket.WebsocketConn); ok {
		conn = ws.Conn
	}
	_ = conn.SetReadDeadline(read)
	_ = conn.SetWriteDeadline(write)
}

func (c *earlyWebsocketConn) Close() error {
	return c.closeWithError(net.ErrClosed, closeWebsocketAlways)
}

func (c *earlyWebsocketConn) closeWithError(err error, mode websocketCloseMode) error {
	c.mu.Lock()
	if mode != closeWebsocketAlways && (!c.opening || c.complete) {
		c.mu.Unlock()
		return nil
	}
	if mode == closeWebsocketWriteExpired && (c.writeDeadline.IsZero() || time.Now().Before(c.writeDeadline)) {
		c.mu.Unlock()
		return nil
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.closedErr = err
	c.finishLocked(err)
	if c.stopCallerCancel != nil {
		c.stopCallerCancel()
		c.stopCallerCancel = nil
	}
	conn, raw := c.conn, c.raw
	c.mu.Unlock()
	c.cancel()
	if raw != nil {
		return raw.Close()
	}
	if conn != nil {
		return closeWebsocketSocket(conn)
	}
	return nil
}

func (c *earlyWebsocketConn) armWriteDeadlineLocked() {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
	}
	if c.opening && !c.complete && !c.writeDeadline.IsZero() {
		c.writeTimer = time.AfterFunc(time.Until(c.writeDeadline), func() {
			_ = c.closeWithError(os.ErrDeadlineExceeded, closeWebsocketWriteExpired)
		})
	}
}

func (c *earlyWebsocketConn) LocalAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw != nil {
		return c.raw.LocalAddr()
	}
	return M.Socksaddr{}
}
func (c *earlyWebsocketConn) RemoteAddr() net.Addr { return c.server }
func (c *earlyWebsocketConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}
func (c *earlyWebsocketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.readDeadline = t
	close(c.changed)
	c.changed = make(chan struct{})
	if c.conn != nil {
		setWebsocketDeadline(c.conn, t, c.writeDeadline)
	}
	return nil
}
func (c *earlyWebsocketConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.writeDeadline = t
	c.armWriteDeadlineLocked()
	if c.conn != nil {
		setWebsocketDeadline(c.conn, c.readDeadline, t)
	} else if c.raw != nil {
		return c.raw.SetDeadline(t)
	}
	return nil
}

var _ net.Conn = (*earlyWebsocketConn)(nil)
var _ N.ExtendedWriter = (*earlyWebsocketConn)(nil)
