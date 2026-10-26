package warppool

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

// Real large-pool sweeps outlive earlier slot and parent pauses. Advance those
// deadlines explicitly so tests exercise repeat rounds without wall-clock waits.
func expireExpansionPauses(manager *Manager) {
	expireSchedulingSlotCooldowns(manager)
	for key, failure := range manager.parentFailures {
		if !failure.retryAt.IsZero() {
			failure.retryAt = time.Now().Add(-time.Second)
			manager.parentFailures[key] = failure
		}
	}
}

func TestExpansionFairnessSlowIPv6OutgrowsFastDuplicateIPv4(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", 9 })
	parents := schedulingParents(t, f, 4)
	working := parents[len(parents)-1]
	workingOutbound := *working.Outbound.Load()
	workingCalls, totalCalls := 0, 0
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		totalCalls++
		if family == warp.IPv4 {
			candidate, _ := testCandidate(f.parent.GetEgressIP().String(), time.Millisecond)
			return candidate, nil
		}
		if parent != workingOutbound {
			return Candidate{}, context.DeadlineExceeded
		}
		workingCalls++
		candidate, _ := testCandidate(fmt.Sprintf("2001:db8:1::%x", workingCalls), 700*time.Millisecond)
		return candidate, nil
	})
	for _, parent := range parents {
		mgr.history.Record(parent.Hash, warp.IPv4, time.Millisecond, time.Now())
	}
	mgr.history.Record(working.Hash, warp.IPv6, 700*time.Millisecond, time.Now())
	for index := 0; index < 120 && len(mgr.active) < 9; index++ {
		expireExpansionPauses(mgr)
		mgr.reconcile(context.Background())
	}
	if len(mgr.active) != 9 || workingCalls != 9 || mgr.familyCounts(false)[warp.IPv6] != 9 {
		t.Fatalf("fast duplicate history or vacant first slots starved slow productive IPv6: active=%d productive=%d attempts=%d", len(mgr.active), workingCalls, totalCalls)
	}
	for key := range mgr.active {
		if key.parent != working.Hash {
			t.Fatal("a failed or duplicate path was published as productive capacity")
		}
	}
}

func TestExpansionFairnessInitialAttemptStillPrefersCachedSuccess(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack = "dual" })
	parents := schedulingParents(t, f, 3)
	known := parents[len(parents)-1]
	var firstParent adapter.Outbound
	var firstFamily warp.Family
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		firstParent, firstFamily = parent, family
		candidate, _ := testCandidate("2001:db8:2::1", 200*time.Millisecond)
		return candidate, nil
	})
	mgr.history.Record(known.Hash, warp.IPv6, 200*time.Millisecond, time.Now())
	mgr.reconcile(context.Background())
	if firstParent != *known.Outbound.Load() || firstFamily != warp.IPv6 || len(mgr.active) != 1 {
		t.Fatal("fair repeated allocation removed cached-parent preference on initial recovery")
	}
}

func TestExpansionFairnessFailedFirstSlotsCannotBlockSecondSlot(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 4 })
	parents := schedulingParents(t, f, 4)
	working := parents[len(parents)-1]
	byOutbound := make(map[adapter.Outbound]node.Hash)
	for _, parent := range parents {
		byOutbound[*parent.Outbound.Load()] = parent.Hash
	}
	var attempted []slot
	parentCalls := make(map[node.Hash]int)
	var mgr *Manager
	mgr = f.manager(t, func(_ context.Context, parent adapter.Outbound, _ warp.Family) (Candidate, error) {
		attempted = append(attempted, mgr.lastAttempt)
		hash := byOutbound[parent]
		parentCalls[hash]++
		if hash != working.Hash {
			return Candidate{}, context.DeadlineExceeded
		}
		candidate, _ := testCandidate(fmt.Sprintf("198.51.100.%d", parentCalls[hash]), 300*time.Millisecond)
		return candidate, nil
	})
	for range 2 * len(parents) {
		expireExpansionPauses(mgr)
		mgr.reconcile(context.Background())
	}
	seen := make(map[node.Hash]bool)
	for _, attempt := range attempted[:min(len(attempted), len(parents))] {
		if seen[attempt.parent] {
			t.Fatal("initial coverage retried a failed parent before visiting every parent")
		}
		seen[attempt.parent] = true
	}
	for _, parent := range parents {
		if parentCalls[parent.Hash] != 2 {
			t.Fatalf("post-coverage round was not fair: parent=%s calls=%d all=%v", parent.Hash.Hex(), parentCalls[parent.Hash], parentCalls)
		}
	}
	if _, exists := mgr.active[slot{working.Hash, warp.IPv4, 1}]; !exists {
		t.Fatal("recurring failed first slots blocked the productive parent's second slot")
	}
}

func TestExpansionFairnessFastDuplicateHistoryCannotMonopolizeIPv6(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "ipv6", 8 })
	// A parent that already exits through WARP can legitimately duplicate this
	// public IPv6 address; verification success alone is not additional capacity.
	f.parent.SetEgressIP(netip.MustParseAddr("2001:db8::feed"))
	parents := schedulingParents(t, f, 4)
	working := parents[len(parents)-1]
	workingOutbound := *working.Outbound.Load()
	productive := 0
	var attempted []slot
	var mgr *Manager
	mgr = f.manager(t, func(_ context.Context, parent adapter.Outbound, _ warp.Family) (Candidate, error) {
		attempted = append(attempted, mgr.lastAttempt)
		if parent != workingOutbound {
			candidate, _ := testCandidate("2001:db8::feed", time.Millisecond)
			return candidate, nil
		}
		productive++
		candidate, _ := testCandidate(fmt.Sprintf("2001:db8:3::%x", productive), 500*time.Millisecond)
		return candidate, nil
	})
	for _, parent := range parents {
		mgr.history.Record(parent.Hash, warp.IPv6, time.Millisecond, time.Now())
	}
	mgr.history.Record(working.Hash, warp.IPv6, 500*time.Millisecond, time.Now())
	for index := 0; index < 4*8 && len(mgr.active) < 8; index++ {
		expireExpansionPauses(mgr)
		mgr.reconcile(context.Background())
	}
	if len(mgr.active) != 8 || productive != 8 {
		t.Fatalf("low-latency duplicate evidence outranked fair expansion: active=%d productive=%d attempts=%d", len(mgr.active), productive, len(attempted))
	}
	if last := attempted[len(attempted)-1]; last.parent != working.Hash || last.index != 7 {
		t.Fatalf("productive expansion never reached the final bounded slot: %+v", last)
	}
}
