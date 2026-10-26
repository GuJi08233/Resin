package warppool

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func TestParentHistoryOlderEvidenceCannotPruneOrReplaceNewSuccess(t *testing.T) {
	now := time.Now()
	history := newParentHistory(filepath.Join(t.TempDir(), "parents.json"), func(string, ...any) {})
	a, b := historyTestParent(1), historyTestParent(2)
	history.Record(a, warp.IPv6, time.Millisecond, now)
	history.Record(b, warp.IPv4, 2*time.Millisecond, now.Add(-2*time.Hour))
	history.Record(a, warp.IPv6, 3*time.Millisecond, now.Add(-time.Hour))
	history.Record(a, warp.IPv6, 4*time.Millisecond, now)
	if tier, latency := history.Preference(a, warp.IPv6, now); tier != 0 || latency != time.Millisecond {
		t.Fatal("old or equal evidence replaced newer verified latency")
	}
	history.Record(historyTestParent(3), warp.IPv4, time.Millisecond, now.Add(-parentHistoryTTL-time.Hour))
	if tier, _ := history.Preference(historyTestParent(3), warp.IPv4, now); tier != 2 {
		t.Fatal("expired probe evidence was accepted as a restart hint")
	}
}

func TestFullPoolRefreshesParentHistoryOnlyFromSuccessfulProbeEvidence(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 1 })
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		candidate, _ := testCandidate("198.51.100.80", time.Millisecond)
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	key := slot{f.parent.Hash, warp.IPv4, 0}
	entry := mgr.active[key].entry
	historyKey := parentHistoryKey{f.parent.Hash, warp.IPv4}
	old := time.Now().Add(-parentHistoryTTL - time.Hour)
	mgr.history.mu.Lock()
	mgr.history.entries[historyKey] = parentHistorySuccess{time.Millisecond, old.UnixNano()}
	mgr.history.mu.Unlock()
	entry.LastEgressUpdate.Store(old.UnixNano())
	mgr.reconcile(context.Background())
	if tier, _ := mgr.history.Preference(f.parent.Hash, warp.IPv4, time.Now()); tier != 2 {
		t.Fatal("routine reconciliation invented fresh validation for an old hint")
	}
	checked := time.Now()
	entry.LastEgressUpdate.Store(checked.UnixNano())
	mgr.reconcile(context.Background())
	if tier, _ := mgr.history.Preference(f.parent.Hash, warp.IPv4, time.Now()); tier != 0 {
		t.Fatal("a real successful probe did not refresh a full pool's restart hint")
	}
	stored := mgr.history.entries[historyKey]
	if stored.lastSuccess != checked.UnixNano() {
		t.Fatal("history used heartbeat time instead of the successful probe timestamp")
	}
}
