package warppool

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func TestManagerProgressRemainsFreshWhileIdleAndBackingOff(t *testing.T) {
	for _, mode := range []string{"full", "no eligible parents", "backoff"} {
		t.Run(mode, func(t *testing.T) {
			f := newPoolFixture(t)
			f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 1 })
			calls := 0
			mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
				calls++
				candidate, _ := testCandidate("198.51.100.2", time.Millisecond)
				return candidate, nil
			})
			switch mode {
			case "full":
				mgr.reconcile(context.Background())
			case "no eligible parents":
				f.parent.CircuitOpenSince.Store(time.Now().UnixNano())
			case "backoff":
				mgr.parentFailures[parentFamily{f.parent.Hash, warp.IPv4}] = parentFailure{retryAt: time.Now().Add(time.Minute)}
			}
			sub := f.subs.Lookup(subscription.WarpSubscriptionID)
			sub.LastMaintenanceNs.Store(time.Now().Add(-time.Hour).UnixNano())
			beforeCalls := calls
			mgr.reconcile(context.Background())
			if calls != beforeCalls || sub.MaintenanceStatus(true, time.Now()) != "maintaining" ||
				sub.LastCheckedNs.Load() != sub.LastMaintenanceNs.Load() {
				t.Fatal("idle maintenance appeared stalled or started an unnecessary factory call")
			}
		})
	}
}

func TestManagerBlockedAttemptHasNoIndependentHeartbeat(t *testing.T) {
	f := newPoolFixture(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		close(entered)
		<-release
		return Candidate{}, context.DeadlineExceeded
	})
	go func() { defer close(done); mgr.reconcile(context.Background()) }()
	<-entered
	sub := f.subs.Lookup(subscription.WarpSubscriptionID)
	last := sub.LastMaintenanceNs.Load()
	if last == 0 || sub.MaintenanceStatus(true, time.Unix(0, last).Add(subscription.MaintenanceStallThreshold+time.Second)) != "stalled" {
		close(release)
		<-done
		t.Fatal("a blocked factory looked healthy without worker progress")
	}
	close(release)
	<-done
	if sub.MaintenanceStatus(true, time.Now()) != "maintaining" || sub.GetLastError() == "" {
		t.Fatal("completed failure did not restore progress and expose its error")
	}
}

func TestManagerRemovalUpdatesMembershipTimestamp(t *testing.T) {
	for _, mode := range []string{"disable", "parent removal"} {
		t.Run(mode, func(t *testing.T) {
			f := newPoolFixture(t)
			f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 1 })
			mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
				candidate, _ := testCandidate("198.51.100.2", time.Millisecond)
				return candidate, nil
			})
			mgr.reconcile(context.Background())
			sub := f.subs.Lookup(subscription.WarpSubscriptionID)
			sub.LastUpdatedNs.Store(1)
			if mode == "disable" {
				f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpEnabled = false })
			} else {
				f.pool.RemoveNodeFromSub(f.parent.Hash, f.sub.ID)
			}
			mgr.reconcile(context.Background())
			if sub.ManagedNodes().Size() != 0 || sub.LastUpdatedNs.Load() <= 1 {
				t.Fatal("removing all exits did not update the membership timestamp")
			}
		})
	}
}

func TestManagerFillsCapacityDespiteUnproductiveLanes(t *testing.T) {
	for _, mode := range []string{"failed family", "duplicate family", "failed parents"} {
		t.Run(mode, func(t *testing.T) {
			f := newPoolFixture(t)
			f.configure(func(cfg *config.RuntimeConfig) {
				cfg.WarpStack, cfg.WarpMaxNodes, cfg.WarpExitsPerFamily = "dual", 9, 1
			})
			working := *f.parent.Outbound.Load()
			parentCount := 1
			if mode == "failed parents" {
				parents := schedulingParents(t, f, 4)
				working = *parents[len(parents)-1].Outbound.Load()
				parentCount = len(parents)
			}
			calls := 0
			mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
				calls++
				if parent != working || (mode == "failed family" && family == warp.IPv6) {
					return Candidate{}, context.DeadlineExceeded
				}
				ip := fmt.Sprintf("198.51.100.%d", calls)
				if family == warp.IPv6 {
					ip = fmt.Sprintf("2001:db8::%x", calls)
					if mode == "duplicate family" {
						ip = "2001:db8::1"
					}
				}
				candidate, _ := testCandidate(ip, time.Duration(calls)*time.Millisecond)
				return candidate, nil
			})
			for index := 0; index < 150 && len(mgr.active) < 9; index++ {
				mgr.reconcile(context.Background())
			}
			if len(mgr.active) != 9 || f.pool.Size() != 9+parentCount {
				t.Fatalf("stranded capacity: active=%d pool=%d calls=%d", len(mgr.active), f.pool.Size(), calls)
			}
			for key := range mgr.active {
				if key.index >= 9 {
					t.Fatalf("slot allocation exceeded bounded candidate space: %+v", key)
				}
			}
			if mode == "failed family" && mgr.parentFailures[parentFamily{f.parent.Hash, warp.IPv6}].retryAt.IsZero() {
				t.Fatal("repeated IPv6 failures did not pause only the failing family")
			}
		})
	}
}

func TestManagerUnsuccessfulRetriesKeepCandidateSpaceBounded(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 4 })
	var created []*memoryOutbound
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		// This address already belongs to the original parent and can never
		// produce an additional exit, even after repeated reconnection attempts.
		candidate, outbound := testCandidate(f.parent.GetEgressIP().String(), time.Millisecond)
		created = append(created, outbound)
		return candidate, nil
	})
	for index := 0; index < 40; index++ {
		expireSchedulingSlotCooldowns(mgr)
		mgr.reconcile(context.Background())
		if mgr.lastAttempt.index >= 4 || len(mgr.nextTry) > 4 {
			t.Fatal("failed refills expanded candidate slots beyond the subscription cap")
		}
	}
	if len(mgr.active) != 0 || f.pool.Size() != 1 {
		t.Fatal("duplicate-only attempts changed the original node pool")
	}
	for _, outbound := range created {
		if outbound.closed.Load() != 1 {
			t.Fatal("retry candidate leaked or was closed more than once")
		}
	}
}
