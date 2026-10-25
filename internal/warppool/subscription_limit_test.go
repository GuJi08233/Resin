package warppool

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func assertManagedExitCount(t *testing.T, f *poolFixture, mgr *Manager, want int) {
	t.Helper()
	managed := f.subs.Lookup(subscription.WarpSubscriptionID).ManagedNodes()
	if len(mgr.active) != want || managed.Size() != want || f.pool.Size() != want+1 {
		t.Fatalf("active=%d managed=%d pool=%d, want %d WARP exits plus original", len(mgr.active), managed.Size(), f.pool.Size(), want)
	}
	for _, exit := range mgr.active {
		if !slices.Equal(exit.entry.SubscriptionIDs(), []string{subscription.WarpSubscriptionID}) {
			t.Fatalf("derived exit retained original subscription references: %+v", exit.entry.SubscriptionIDs())
		}
		if _, exists := managed.LoadNode(exit.entry.Hash); !exists {
			t.Fatalf("live exit missing from unified WARP view: %s", exit.entry.Hash)
		}
		if _, exists := f.sub.ManagedNodes().LoadNode(exit.entry.Hash); exists {
			t.Fatalf("derived exit incorrectly appeared in original subscription: %s", exit.entry.Hash)
		}
	}
}

func TestManagerAutomaticallyFillsAndResizesWarpSubscription(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) {
		cfg.WarpStack, cfg.WarpExitsPerFamily, cfg.WarpMaxNodes = "dual", 1, 300
	})
	original := f.parent.Outbound.Load()
	var created []*memoryOutbound
	var families []warp.Family
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		if original == nil || parent != *original {
			t.Fatal("expanded exit did not use the original parent")
		}
		number := len(created) + 1
		ip := fmt.Sprintf("198.51.%d.%d", number/256, number%256)
		if family == warp.IPv6 {
			ip = fmt.Sprintf("2001:db8::%x", number)
		}
		candidate, outbound := testCandidate(ip, time.Duration(number)*time.Millisecond)
		created = append(created, outbound)
		families = append(families, family)
		return candidate, nil
	})
	for index := 0; index < 310; index++ {
		mgr.reconcile(context.Background())
	}
	assertManagedExitCount(t, f, mgr, 300)
	if len(created) != 300 {
		t.Fatalf("reaching the subscription limit still created candidates: %d", len(created))
	}
	for _, family := range []warp.Family{warp.IPv4, warp.IPv6} {
		count := 0
		for _, createdFamily := range families {
			if createdFamily == family {
				count++
			}
		}
		if count != 150 {
			t.Fatalf("family %s produced %d exits, want 150 from the same parent", family, count)
		}
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 305 })
	for index := 0; index < 10; index++ {
		mgr.reconcile(context.Background())
	}
	assertManagedExitCount(t, f, mgr, 305)
	if len(created) != 305 {
		t.Fatalf("raising the limit did not automatically fill exactly five exits: %d", len(created))
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 200 })
	mgr.reconcile(context.Background())
	assertManagedExitCount(t, f, mgr, 200)
	if len(created) != 305 {
		t.Fatal("shrinking the subscription created new candidates")
	}
	for index, outbound := range created {
		want := int32(0)
		if index >= 200 {
			want = 1
		}
		if got := outbound.closed.Load(); got != want {
			t.Fatalf("candidate %d close count=%d, want %d: shrink must keep the fastest 200", index, got, want)
		}
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpEnabled = false })
	mgr.reconcile(context.Background())
	assertManagedExitCount(t, f, mgr, 0)
	for index, outbound := range created {
		if got := outbound.closed.Load(); got != 1 {
			t.Fatalf("candidate %d closed %d times after disable, want one", index, got)
		}
	}
	if f.parent.Outbound.Load() != original || (*original).(*memoryOutbound).closed.Load() != 0 ||
		f.sub.ManagedNodes().Size() != 1 {
		t.Fatal("WARP resizing or disable changed the original parent or original subscription")
	}
}

func TestManagerHonorsDisabledWarpSubscriptionWithEnabledRuntimeConfig(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 1 })
	warpSub := f.subs.Lookup(subscription.WarpSubscriptionID)
	warpSub.SetEnabled(false)
	calls := 0
	var candidateOutbound *memoryOutbound
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		calls++
		candidate, outbound := testCandidate("198.51.100.1", time.Millisecond)
		candidateOutbound = outbound
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	if calls != 0 || !f.config.Load().WarpEnabled {
		t.Fatal("disabled managed subscription did not prevent new registration with enabled runtime config")
	}
	assertManagedExitCount(t, f, mgr, 0)
	warpSub.SetEnabled(true)
	mgr.reconcile(context.Background())
	assertManagedExitCount(t, f, mgr, 1)
	if calls != 1 {
		t.Fatalf("enabling subscription made %d factory calls, want one", calls)
	}
	warpSub.SetEnabled(false)
	for _, exit := range mgr.active {
		if !f.pool.IsNodeDisabled(exit.entry.Hash) {
			t.Fatal("disabled WARP subscription left an exit eligible before cleanup")
		}
	}
	mgr.reconcile(context.Background())
	assertManagedExitCount(t, f, mgr, 0)
	if calls != 1 || candidateOutbound.closed.Load() != 1 || !f.config.Load().WarpEnabled || f.parent.Outbound.Load() == nil {
		t.Fatal("subscription-only disable failed to release WARP while preserving runtime config and original parent")
	}
}
