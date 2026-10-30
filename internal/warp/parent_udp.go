package warp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// ParentUDPError reports that a parent cannot carry WARP's outer UDP at all.
// The cause is independent of the inner family and the Cloudflare endpoint,
// so callers should pause every WARP lane through that parent.
type ParentUDPError struct {
	// Rejected means the parent explicitly refused UDP, for example a SOCKS5
	// server answering UDP ASSOCIATE with "command not supported".
	Rejected bool
	Err      error
}

func (e *ParentUDPError) Error() string {
	if e.Rejected {
		return "parent rejected UDP: " + e.Err.Error()
	}
	return "parent UDP unavailable: " + e.Err.Error()
}

func (e *ParentUDPError) Unwrap() error { return e.Err }

// AsParentUDPError extracts a parent-level UDP failure from an attempt error.
func AsParentUDPError(err error) (*ParentUDPError, bool) {
	var target *ParentUDPError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// Variables only so tests can shorten the wait for a silent parent.
var (
	udpProbeTimeout  = 8 * time.Second
	udpProbeInterval = 2 * time.Second
)

var udpProbeTarget = M.ParseSocksaddr("1.1.1.1:53")

// probeParentUDP sends a tiny DNS query through the parent before a Cloudflare
// device is registered. Parents that cannot relay UDP are rejected without
// consuming the shared registration gap or a WireGuard attempt budget.
func probeParentUDP(ctx context.Context, parent adapter.Outbound) error {
	probeCtx, cancel := context.WithTimeout(ctx, udpProbeTimeout)
	defer cancel()
	packet, err := parent.ListenPacket(probeCtx, udpProbeTarget)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ParentUDPError{Rejected: probeCtx.Err() == nil, Err: fmt.Errorf("listen_packet: %w", err)}
	}
	defer packet.Close()
	stopRead := context.AfterFunc(probeCtx, func() { packet.Close() })
	defer stopRead()

	var id [2]byte
	_, _ = rand.Read(id[:])
	query := dnsProbeQuery(id)
	answered := make(chan struct{})
	go func() {
		buffer := make([]byte, 512)
		for {
			n, _, err := packet.ReadFrom(buffer)
			if err != nil {
				return
			}
			if n >= 12 && buffer[0] == id[0] && buffer[1] == id[1] && buffer[2]&0x80 != 0 {
				close(answered)
				return
			}
		}
	}()
	ticker := time.NewTicker(udpProbeInterval)
	defer ticker.Stop()
	for {
		if _, err := packet.WriteTo(query, udpProbeTarget.UDPAddr()); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &ParentUDPError{Err: fmt.Errorf("write: %w", err)}
		}
		select {
		case <-answered:
			return nil
		case <-probeCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &ParentUDPError{Err: fmt.Errorf("no DNS reply from %s within %s", udpProbeTarget, udpProbeTimeout)}
		case <-ticker.C:
		}
	}
}

// dnsProbeQuery builds a recursive A query for cloudflare.com.
func dnsProbeQuery(id [2]byte) []byte {
	query := []byte{id[0], id[1], 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range []string{"cloudflare", "com"} {
		query = append(query, byte(len(label)))
		query = append(query, label...)
	}
	return append(query, 0, 0, 1, 0, 1)
}

type progressKey struct{}

// WithProgress attaches a callback that Create invokes after each completed
// phase, so a long but healthy attempt is not reported as a stalled worker.
func WithProgress(ctx context.Context, report func()) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

func reportProgress(ctx context.Context) {
	if report, ok := ctx.Value(progressKey{}).(func()); ok && report != nil {
		report()
	}
}
