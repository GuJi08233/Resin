package warppool

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func TestReconnectSeparatesOldProbeAndRequestFeedback(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 1 })
	calls := 0
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		calls++
		candidate, _ := testCandidate(fmt.Sprintf("198.51.100.%d", calls), time.Millisecond)
		return candidate, nil
	})
	key := slot{f.parent.Hash, warp.IPv4, 0}
	mgr.reconcile(context.Background())
	old := mgr.active[key].entry
	old.CircuitOpenSince.Store(time.Now().UnixNano())
	mgr.reconcile(context.Background())
	current := mgr.active[key].entry
	if old.Hash == current.Hash {
		t.Fatal("a replacement tunnel reused its predecessor's node identity")
	}
	oldIP := netip.MustParseAddr("198.51.100.100")
	f.pool.RecordResult(old.Hash, false)
	f.pool.UpdateNodeEgressIP(old.Hash, &oldIP, nil)
	latency := time.Hour
	f.pool.RecordLatency(old.Hash, "cloudflare.com", &latency)
	if current.GetEgressIP().String() != "198.51.100.2" || !current.IsHealthy() ||
		mgr.latency(current) != time.Millisecond {
		t.Fatal("old feedback changed the replacement tunnel's address, health or latency")
	}
}
