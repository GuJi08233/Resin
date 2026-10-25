package outbound

import (
	"os"
	"sync"
	"time"
)

// Mieru sessions clear their internal deadline after each Read and don't wake
// an already blocked Read when its deadline changes. Preserve the net.Conn
// contract at this adapter boundary, including fragmented proxy handshakes.
// A timed-out stream is closed because its framing may already be partial.
type mieruDeadline struct {
	at         time.Time
	timer      *time.Timer
	generation uint64
	active     bool
	expired    bool
}

type mieruDeadlines struct {
	mu, readMu, writeMu sync.Mutex
	read, write         mieruDeadline
}

func (c *mieruTrackedConn) armDeadlineLocked(deadline *mieruDeadline) {
	deadline.generation++
	if deadline.timer != nil {
		deadline.timer.Stop()
		deadline.timer = nil
	}
	if !deadline.active || deadline.at.IsZero() {
		return
	}
	generation := deadline.generation
	deadline.timer = time.AfterFunc(time.Until(deadline.at), func() {
		c.deadlines.mu.Lock()
		if !deadline.active || deadline.generation != generation {
			c.deadlines.mu.Unlock()
			return
		}
		deadline.expired = true
		c.deadlines.mu.Unlock()
		_ = c.Close()
	})
}

func (c *mieruTrackedConn) beginIO(read bool) func() bool {
	c.deadlines.mu.Lock()
	deadline := &c.deadlines.write
	if read {
		deadline = &c.deadlines.read
	}
	deadline.active, deadline.expired = true, false
	c.armDeadlineLocked(deadline)
	c.deadlines.mu.Unlock()
	return func() bool {
		c.deadlines.mu.Lock()
		defer c.deadlines.mu.Unlock()
		deadline.active = false
		c.armDeadlineLocked(deadline)
		return deadline.expired
	}
}

func (c *mieruTrackedConn) Read(p []byte) (int, error) {
	c.deadlines.readMu.Lock()
	defer c.deadlines.readMu.Unlock()
	finish := c.beginIO(true)
	n, err := c.Conn.Read(p)
	if finish() {
		return n, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *mieruTrackedConn) Write(p []byte) (int, error) {
	c.deadlines.writeMu.Lock()
	defer c.deadlines.writeMu.Unlock()
	finish := c.beginIO(false)
	n, err := c.Conn.Write(p)
	if finish() {
		return n, os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *mieruTrackedConn) SetDeadline(deadline time.Time) error {
	c.deadlines.mu.Lock()
	c.deadlines.read.at, c.deadlines.write.at = deadline, deadline
	c.armDeadlineLocked(&c.deadlines.read)
	c.armDeadlineLocked(&c.deadlines.write)
	c.deadlines.mu.Unlock()
	return c.Conn.SetDeadline(deadline)
}

func (c *mieruTrackedConn) SetReadDeadline(deadline time.Time) error {
	c.deadlines.mu.Lock()
	c.deadlines.read.at = deadline
	c.armDeadlineLocked(&c.deadlines.read)
	c.deadlines.mu.Unlock()
	return c.Conn.SetReadDeadline(deadline)
}

func (c *mieruTrackedConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlines.mu.Lock()
	c.deadlines.write.at = deadline
	c.armDeadlineLocked(&c.deadlines.write)
	c.deadlines.mu.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *mieruTrackedConn) stopDeadlines() {
	c.deadlines.mu.Lock()
	defer c.deadlines.mu.Unlock()
	c.deadlines.read.active, c.deadlines.write.active = false, false
	c.armDeadlineLocked(&c.deadlines.read)
	c.armDeadlineLocked(&c.deadlines.write)
}
