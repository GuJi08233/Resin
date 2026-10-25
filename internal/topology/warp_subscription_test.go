package topology

import (
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
)

func TestWarpSubscriptionIsNotRefreshedOrPersistedByScheduler(t *testing.T) {
	pool, mgr, _, parent := derivedTestSetup(t)
	warp := mgr.Lookup(subscription.WarpSubscriptionID)
	child := derivedTestEntry(t, parent, "ipv4")
	if !pool.AddDerivedNode(child) {
		t.Fatal("failed to publish WARP child")
	}
	var fetched, persisted int
	scheduler := NewSubscriptionScheduler(SchedulerConfig{
		SubManager: mgr, Pool: pool,
		Fetcher: func(string) ([]byte, error) { fetched++; return []byte(`[]`), nil },
		OnSubUpdated: func(sub *subscription.Subscription) {
			if sub.ID == warp.ID {
				persisted++
			}
		},
	})
	defer scheduler.Stop()
	// Isolate the scheduler from ordinary subscriptions to verify all three
	// refresh entrypoints preserve runtime-owned managed membership.
	mgr.Range(func(id string, sub *subscription.Subscription) bool {
		if id != warp.ID {
			sub.SetEnabled(false)
		}
		return true
	})
	scheduler.UpdateSubscription(warp)
	scheduler.ForceRefreshAll()
	scheduler.tick()
	scheduler.SetSubscriptionEnabled(warp, false)
	scheduler.SetSubscriptionEnabled(warp, true)
	scheduler.RenameSubscription(warp, "replacement")
	if fetched != 0 || persisted != 0 || warp.Name() != subscription.WarpSubscriptionName {
		t.Fatalf("WARP subscription entered normal scheduler workflow: fetched=%d persisted=%d name=%q", fetched, persisted, warp.Name())
	}
	if _, exists := warp.ManagedNodes().LoadNode(child.Hash); !exists {
		t.Fatal("scheduler replaced runtime WARP membership")
	}
	if warp.LastCheckedNs.Load() != 0 || warp.LastUpdatedNs.Load() != 0 || warp.GetLastError() != "" {
		t.Fatal("scheduler changed WARP refresh state")
	}
}

func TestWarpSubscriptionIsNotEvictedByEphemeralCleaner(t *testing.T) {
	pool, mgr, _, parent := derivedTestSetup(t)
	warp := mgr.Lookup(subscription.WarpSubscriptionID)
	child := derivedTestEntry(t, parent, "ipv6")
	if !pool.AddDerivedNode(child) {
		t.Fatal("failed to publish WARP child")
	}
	warp.SetEphemeral(true) // even invalid legacy state must not enter cleanup
	warp.SetEphemeralNodeEvictDelayNs(0)
	child.CircuitOpenSince.Store(time.Now().Add(-time.Hour).UnixNano())
	cleaner := NewEphemeralCleaner(mgr, pool)
	var evicted int
	cleaner.SetOnNodeEvicted(func(string, node.Hash) { evicted++ })
	cleaner.sweep()
	cleaner.sweepOneSubscription(warp.ID, warp, time.Now().UnixNano(), nil)
	managed, exists := warp.ManagedNodes().LoadNode(child.Hash)
	if !exists || managed.Evicted || evicted != 0 {
		t.Fatal("ephemeral cleaner evicted a runtime-owned WARP child")
	}
	if _, exists := pool.GetEntry(child.Hash); !exists {
		t.Fatal("ephemeral cleaner removed WARP child from node pool")
	}
}
