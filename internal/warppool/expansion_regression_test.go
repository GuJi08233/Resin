package warppool

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

// Timers are advanced explicitly: in a large real pool, time spent attempting
// other first slots lets earlier duplicate and transport cooldowns expire.
// Productive parents must get additional slots once initial coverage is done.
func TestExpansionWorkingParentNotStarvedByFirstSlotDuplicates(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", 12 })
	parents := schedulingParents(t, f, 5)
	working := parents[len(parents)-1]
	byOutbound := make(map[adapter.Outbound]node.Hash)
	for _, parent := range parents {
		byOutbound[*parent.Outbound.Load()] = parent.Hash
	}
	var attempted []slot
	var successful int
	var manager *Manager
	manager = f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		attempted = append(attempted, manager.lastAttempt)
		if byOutbound[parent] != working.Hash {
			// The same verified address duplicates an original node and cannot
			// grow the pool, even though its tunnel verification succeeded.
			candidate, _ := testCandidate(f.parent.GetEgressIP().String(), time.Millisecond)
			if family == warp.IPv6 {
				return Candidate{}, context.DeadlineExceeded
			}
			return candidate, nil
		}
		successful++
		ip := fmt.Sprintf("198.51.100.%d", successful)
		if family == warp.IPv6 {
			ip = fmt.Sprintf("2001:db8::%x", successful)
		}
		candidate, _ := testCandidate(ip, time.Millisecond)
		return candidate, nil
	})
	for range 100 {
		for key := range manager.nextTry {
			manager.nextTry[key] = time.Now().Add(-time.Second)
		}
		for key, failure := range manager.parentFailures {
			failure.retryAt = time.Now().Add(-time.Second)
			manager.parentFailures[key] = failure
		}
		manager.reconcile(context.Background())
	}
	expanded := 0
	for _, key := range attempted {
		if key.index > 0 {
			expanded++
		}
	}
	if len(manager.active) != 12 || expanded == 0 {
		t.Fatalf("first-slot retries starved productive expansion: attempts=%d additional-slot-attempts=%d active=%d/12 successful-parent-calls=%d", len(attempted), expanded, len(manager.active), successful)
	}
}

func TestExpansionGlobalRateLimitPreservesCachedParentPriority(t *testing.T) {
	f := newPoolFixture(t)
	parents := schedulingParents(t, f, 4)
	known := parents[len(parents)-1]
	var attempts []adapter.Outbound
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, _ warp.Family) (Candidate, error) {
		attempts = append(attempts, parent)
		return Candidate{}, &warp.RateLimitError{RetryAt: time.Now().Add(time.Minute)}
	})
	mgr.history.Record(known.Hash, warp.IPv4, time.Millisecond, time.Now())
	mgr.reconcile(context.Background())
	if len(attempts) != 1 || attempts[0] != *known.Outbound.Load() {
		t.Fatal("cached parent was not tried first")
	}
	mgr.rateRetryAt = time.Now().Add(-time.Second)
	expireSchedulingSlotCooldowns(mgr)
	key, parent, ok := mgr.pickCandidate(mgr.currentSettings(), time.Now())
	if !ok || parent != known || key.parent != known.Hash {
		t.Fatal("global throttling demoted a working cached parent as if its path had been attempted")
	}
}
