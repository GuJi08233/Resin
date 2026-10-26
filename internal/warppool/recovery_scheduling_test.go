package warppool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

type recoveryAttempt struct {
	parent   adapter.Outbound
	family   warp.Family
	outbound *memoryOutbound
	release  chan struct{}
}

type recoveryFactory struct {
	entered chan recoveryAttempt
	active  atomic.Int32
	peak    atomic.Int32
	calls   atomic.Int32
}

func newRecoveryFactory() *recoveryFactory {
	return &recoveryFactory{entered: make(chan recoveryAttempt, 32)}
}

func (f *recoveryFactory) create(ctx context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
	current := f.active.Add(1)
	defer f.active.Add(-1)
	for previous := f.peak.Load(); current > previous && !f.peak.CompareAndSwap(previous, current); previous = f.peak.Load() {
	}
	sequence := f.calls.Add(1)
	ip := fmt.Sprintf("198.51.100.%d", sequence)
	if family == warp.IPv6 {
		ip = fmt.Sprintf("2001:db8::%x", sequence)
	}
	candidate, outbound := testCandidate(ip, time.Millisecond)
	attempt := recoveryAttempt{parent, family, outbound, make(chan struct{})}
	f.entered <- attempt
	select {
	case <-ctx.Done():
		return candidate, ctx.Err()
	case <-attempt.release:
		return candidate, nil
	}
}

func takeRecoveryAttempt(t *testing.T, entered <-chan recoveryAttempt) recoveryAttempt {
	t.Helper()
	select {
	case attempt := <-entered:
		return attempt
	case <-time.After(3 * time.Second):
		t.Fatal("WARP recovery did not start an available parent")
		return recoveryAttempt{}
	}
}

func awaitRecoveryCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("WARP recovery did not reach the expected state")
		}
	}
}

func TestRecoveryDefaultWorkersReserveCapacityAndDifferentParents(t *testing.T) {
	for _, limit := range []int{1, 3, 8} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			f := newPoolFixture(t)
			f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", limit })
			parents := schedulingParents(t, f, 6)
			factory := newRecoveryFactory()
			mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Create: factory.create, Logf: func(string, ...any) {}})
			t.Cleanup(mgr.Stop)
			mgr.Start()
			workers := min(limit, 4)
			var started []recoveryAttempt
			seen := make(map[adapter.Outbound]bool)
			families := make(map[warp.Family]int)
			for index := 0; index < workers; index++ {
				attempt := takeRecoveryAttempt(t, factory.entered)
				started = append(started, attempt)
				if seen[attempt.parent] {
					t.Fatal("allocated another tunnel before giving distinct available parents their first attempt")
				}
				seen[attempt.parent] = true
				families[attempt.family]++
			}
			if families[warp.IPv6] != (workers+1)/2 || families[warp.IPv4] != workers/2 {
				t.Fatalf("in-flight reservations did not balance families: %v", families)
			}
			select {
			case <-factory.entered:
				t.Fatal("dispatched beyond the worker or subscription capacity while all attempts were outstanding")
			case <-time.After(40 * time.Millisecond):
			}
			if factory.peak.Load() != int32(workers) {
				t.Fatalf("peak parallel attempts=%d want=%d", factory.peak.Load(), workers)
			}
			mgr.Stop()
			for _, attempt := range started {
				if attempt.outbound.closed.Load() != 1 {
					t.Fatal("shutdown failed to release an outstanding candidate exactly once")
				}
			}
			if factory.active.Load() != 0 || f.pool.Size() != len(parents) || f.subs.Lookup(subscription.WarpSubscriptionID).ManagedNodes().Size() != 0 {
				t.Fatal("shutdown retained work or published canceled candidates")
			}
		})
	}
}

func TestRecoveryParallelConfigDisableCancelsAllAttempts(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 8 })
	schedulingParents(t, f, 6)
	factory := newRecoveryFactory()
	mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Create: factory.create, Concurrency: 4, Logf: func(string, ...any) {}})
	t.Cleanup(mgr.Stop)
	mgr.Start()
	var attempts []recoveryAttempt
	for index := 0; index < 4; index++ {
		attempts = append(attempts, takeRecoveryAttempt(t, factory.entered))
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpEnabled = false })
	awaitRecoveryCondition(t, func() bool {
		for _, attempt := range attempts {
			if attempt.outbound.closed.Load() != 1 {
				return false
			}
		}
		return true
	})
	mgr.Stop()
	if factory.calls.Load() != 4 || factory.active.Load() != 0 || f.subs.Lookup(subscription.WarpSubscriptionID).ManagedNodes().Size() != 0 {
		t.Fatal("disabled WARP retained or relaunched a candidate")
	}
}

func TestRecoveryDualStackIsBalancedWithIPv6First(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", 7 })
	var attempted []warp.Family
	mgr := f.manager(t, func(_ context.Context, _ adapter.Outbound, family warp.Family) (Candidate, error) {
		attempted = append(attempted, family)
		ip := fmt.Sprintf("198.51.100.%d", len(attempted))
		if family == warp.IPv6 {
			ip = fmt.Sprintf("2001:db8::%x", len(attempted))
		}
		candidate, _ := testCandidate(ip, time.Millisecond)
		return candidate, nil
	})
	for index := 0; index < 7; index++ {
		mgr.reconcile(context.Background())
	}
	if len(attempted) != 7 || attempted[0] != warp.IPv6 {
		t.Fatalf("dual-stack recovery order=%v", attempted)
	}
	counts := make(map[warp.Family]int)
	for _, family := range attempted {
		counts[family]++
		if difference := counts[warp.IPv6] - counts[warp.IPv4]; difference < 0 || difference > 1 {
			t.Fatalf("a successful family filled ahead of the other: %v", attempted)
		}
	}
	if len(mgr.active) != 7 || counts[warp.IPv6] != 4 || counts[warp.IPv4] != 3 {
		t.Fatalf("dual-stack capacity=%d families=%v", len(mgr.active), counts)
	}
}

func TestRecoveryKnownParentPriorityDoesNotSkipUnvisitedParents(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", 12 })
	parents := schedulingParents(t, f, 6)
	known := parents[len(parents)-1]
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	history := newParentHistory(path, t.Logf)
	history.Record(known.Hash, warp.IPv6, time.Millisecond, time.Now())
	history.Flush()
	byOutbound := make(map[adapter.Outbound]node.Hash)
	for _, parent := range parents {
		byOutbound[*parent.Outbound.Load()] = parent.Hash
	}
	var attempts []node.Hash
	mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Concurrency: 1, HistoryPath: path,
		Create: func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
			attempts = append(attempts, byOutbound[parent])
			ip := fmt.Sprintf("198.51.100.%d", len(attempts))
			if family == warp.IPv6 {
				ip = fmt.Sprintf("2001:db8::%x", len(attempts))
			}
			candidate, _ := testCandidate(ip, time.Millisecond)
			return candidate, nil
		}, Logf: func(string, ...any) {}})
	t.Cleanup(func() { mgr.Stop(); mgr.clear() })
	seen := make(map[node.Hash]bool)
	for index := range parents {
		mgr.reconcile(context.Background())
		if len(attempts) != index+1 || seen[attempts[index]] {
			t.Fatalf("expanded a known parent before covering unknown parents: %v", attempts)
		}
		seen[attempts[index]] = true
	}
	if attempts[0] != known.Hash {
		t.Fatal("restart ignored the previous successful parent in favor of hash order")
	}
	for index := 0; index < 6; index++ {
		mgr.reconcile(context.Background())
	}
	if len(mgr.active) != 12 {
		t.Fatal("parent coverage prevented subsequent expansion to the configured limit")
	}
}

func TestRecoveryDuplicateYieldsToOtherParents(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 4 })
	parents := schedulingParents(t, f, 3)
	byOutbound := make(map[adapter.Outbound]node.Hash)
	for _, parent := range parents {
		byOutbound[*parent.Outbound.Load()] = parent.Hash
	}
	var attempts []node.Hash
	var discarded []*memoryOutbound
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, _ warp.Family) (Candidate, error) {
		attempts = append(attempts, byOutbound[parent])
		candidate, outbound := testCandidate(f.parent.GetEgressIP().String(), time.Millisecond)
		discarded = append(discarded, outbound)
		return candidate, nil
	})
	for index := range parents {
		mgr.reconcile(context.Background())
		if len(attempts) != index+1 {
			t.Fatal("a duplicate IP retried within the same factory quantum")
		}
	}
	seen := make(map[node.Hash]bool)
	for _, parent := range attempts {
		if seen[parent] {
			t.Fatalf("duplicate retries starved an unvisited parent: %v", attempts)
		}
		seen[parent] = true
	}
	for _, outbound := range discarded {
		if outbound.closed.Load() != 1 {
			t.Fatal("a duplicate candidate was retained between scheduling quanta")
		}
	}
}

func TestRecoveryParallelDuplicatePublicationKeepsFastest(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 2 })
	parents := schedulingParents(t, f, 2)
	slowParent := *parents[0].Outbound.Load()
	entered := make(chan recoveryAttempt, 16)
	mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Concurrency: 4,
		Create: func(ctx context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
			latency := 5 * time.Millisecond
			if parent == slowParent {
				latency = 30 * time.Millisecond
			}
			candidate, outbound := testCandidate("198.51.100.200", latency)
			attempt := recoveryAttempt{parent, family, outbound, make(chan struct{})}
			entered <- attempt
			select {
			case <-ctx.Done():
				return candidate, ctx.Err()
			case <-attempt.release:
				return candidate, nil
			}
		}, Logf: func(string, ...any) {}})
	t.Cleanup(mgr.Stop)
	mgr.Start()
	first, second := takeRecoveryAttempt(t, entered), takeRecoveryAttempt(t, entered)
	slow, fast := first, second
	if slow.parent != slowParent {
		slow, fast = second, first
	}
	if slow.parent == fast.parent {
		t.Fatal("parallel recovery selected the same parent twice before covering both")
	}
	managed := f.subs.Lookup(subscription.WarpSubscriptionID).ManagedNodes()
	close(slow.release)
	awaitRecoveryCondition(t, func() bool { return managed.Size() == 1 })
	close(fast.release)
	awaitRecoveryCondition(t, func() bool {
		return slow.outbound.closed.Load() == 1 && managed.Size() == 1
	})
	if fast.outbound.closed.Load() != 0 {
		t.Fatal("faster concurrently-created route was discarded")
	}
	managed.RangeNodes(func(hash node.Hash, _ subscription.ManagedNode) bool {
		entry, exists := f.pool.GetEntry(hash)
		if !exists || entry.GetEgressIP().String() != "198.51.100.200" {
			t.Fatal("duplicate publication lost the verified exit")
		}
		stats, exists := entry.LatencyTable.GetDomainStats("cloudflare.com")
		if !exists || stats.Ewma != 5*time.Millisecond {
			t.Fatal("duplicate publication retained the slower candidate")
		}
		return true
	})
	mgr.Stop()
	if slow.outbound.closed.Load() != 1 || fast.outbound.closed.Load() != 1 {
		t.Fatal("replacement or shutdown closed a candidate more than once")
	}
}

func TestRecoveryOutstandingWorkersDoNotGenerateIdleHeartbeats(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 4 })
	schedulingParents(t, f, 4)
	factory := newRecoveryFactory()
	mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Create: factory.create, Concurrency: 4, Logf: func(string, ...any) {}})
	// Drive the actor synchronously while the actual factory jobs run in their
	// bounded workers. No background actor reads its maps during assertions.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { cancel(); mgr.Stop() })
	mgr.reconcile(ctx)
	for index := 0; index < 4; index++ {
		takeRecoveryAttempt(t, factory.entered)
	}
	sub := f.subs.Lookup(subscription.WarpSubscriptionID)
	stale := time.Now().Add(-subscription.MaintenanceStallThreshold - time.Second).UnixNano()
	sub.LastMaintenanceNs.Store(stale)
	sub.LastCheckedNs.Store(stale)
	mgr.reconcile(ctx)
	if sub.LastMaintenanceNs.Load() != stale || sub.MaintenanceStatus(true, time.Now()) != "stalled" {
		t.Fatal("an idle scheduler hid four outstanding workers behind a fresh heartbeat")
	}
	cancel()
	awaitRecoveryCondition(t, func() bool { return factory.active.Load() == 0 })
	mgr.drainResults(context.Background())
}

func TestRecoveryFallbackPoolRebalancesOnlyAfterFamilyVerification(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", 6 })
	ipv6Available := false
	calls := 0
	mgr := f.manager(t, func(_ context.Context, _ adapter.Outbound, family warp.Family) (Candidate, error) {
		calls++
		if family == warp.IPv6 && !ipv6Available {
			return Candidate{}, context.DeadlineExceeded
		}
		ip := fmt.Sprintf("198.51.100.%d", calls)
		if family == warp.IPv6 {
			ip = fmt.Sprintf("2001:db8::%x", calls)
		}
		candidate, _ := testCandidate(ip, time.Duration(calls)*time.Millisecond)
		return candidate, nil
	})
	for index := 0; index < 10 && len(mgr.active) < 6; index++ {
		mgr.reconcile(context.Background())
	}
	if len(mgr.active) != 6 || mgr.familyCounts(false)[warp.IPv4] != 6 {
		t.Fatal("failed IPv6 family stranded usable IPv4 capacity")
	}
	previous := make(map[node.Hash]bool)
	for _, exit := range mgr.active {
		previous[exit.entry.Hash] = true
	}
	expireSchedulingSlotCooldowns(mgr)
	mgr.reconcile(context.Background())
	for _, exit := range mgr.active {
		if !previous[exit.entry.Hash] {
			t.Fatal("failed verification replaced an established fallback route")
		}
	}
	if len(mgr.active) != 6 {
		t.Fatal("family balancing removed a working route before verifying its replacement")
	}
	ipv6Available = true
	for index := 0; index < 3; index++ {
		expireSchedulingSlotCooldowns(mgr)
		mgr.reconcile(context.Background())
		if len(mgr.active) != 6 || f.pool.Size() != 7 {
			t.Fatal("family balancing published beyond the cap or temporarily shrank the pool")
		}
	}
	counts := mgr.familyCounts(false)
	if counts[warp.IPv6] != 3 || counts[warp.IPv4] != 3 {
		t.Fatalf("recovered family did not rebalance full pool: %v", counts)
	}
}

func TestRecoveryRateLimitPausesGloballyWithoutPenalizingParents(t *testing.T) {
	f := newPoolFixture(t)
	parents := schedulingParents(t, f, 3)
	calls := 0
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		calls++
		if calls == 1 {
			return Candidate{}, fmt.Errorf("registration: %w", &warp.RateLimitError{RetryAt: time.Now().Add(time.Minute)})
		}
		candidate, _ := testCandidate("198.51.100.200", time.Millisecond)
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	if calls != 1 || len(mgr.parentFailures) != 0 || len(mgr.visited) != 0 || !time.Now().Before(mgr.rateRetryAt) {
		t.Fatal("API rate limit was treated as an attempted or failed parent")
	}
	for range parents {
		mgr.reconcile(context.Background())
	}
	if calls != 1 || len(mgr.active) != 0 {
		t.Fatal("global registration pause was bypassed using another parent")
	}
	mgr.rateRetryAt = time.Now().Add(-time.Second)
	expireSchedulingSlotCooldowns(mgr)
	mgr.reconcile(context.Background())
	if calls != 2 || len(mgr.active) != 1 || len(mgr.parentFailures) != 0 {
		t.Fatal("expired API pause did not resume normal parent recovery")
	}
}

func TestRecoveryWinningDuplicateCannotBypassItsCooldown(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 4 })
	var created []*memoryOutbound
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		latency := 30 * time.Millisecond
		if len(created) > 0 {
			latency = 5 * time.Millisecond
		}
		candidate, outbound := testCandidate("198.51.100.200", latency)
		created = append(created, outbound)
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	mgr.reconcile(context.Background())
	if len(created) != 2 || len(mgr.active) != 1 || created[0].closed.Load() != 1 || created[1].closed.Load() != 0 {
		t.Fatal("faster duplicate was not selected for the existing IP")
	}
	for index := 0; index < 5; index++ {
		mgr.reconcile(context.Background())
	}
	if len(created) != 2 {
		t.Fatal("published duplicate bypassed its cooldown by allocating another slot")
	}
	expireSchedulingSlotCooldowns(mgr)
	mgr.reconcile(context.Background())
	if len(created) != 3 || created[2].closed.Load() != 1 || len(mgr.active) != 1 {
		t.Fatal("duplicate retry did not resume safely after its cooldown")
	}
}

func TestRecoveryDuplicateFamilyDoesNotStarveProductiveFamily(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack, cfg.WarpMaxNodes = "dual", 16 })
	parents := schedulingParents(t, f, 8)
	families := make(map[warp.Family]int)
	seen := make(map[adapter.Outbound]bool)
	calls := 0
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		calls++
		families[family]++
		if seen[parent] {
			t.Fatal("retried a parent before initial recovery covered all available parents")
		}
		seen[parent] = true
		ip := f.parent.GetEgressIP().String()
		if family == warp.IPv6 {
			ip = fmt.Sprintf("2001:db8::%x", calls)
		}
		candidate, _ := testCandidate(ip, time.Millisecond)
		return candidate, nil
	})
	for range parents {
		mgr.reconcile(context.Background())
	}
	if calls != len(parents) || families[warp.IPv6] < 2 || len(mgr.active) != families[warp.IPv6] {
		t.Fatalf("duplicate-family deficit starved productive IPv6 recovery: calls=%d families=%v active=%d", calls, families, len(mgr.active))
	}
}

func TestRecoveryConcurrentSuccessPreservesGlobalRateLimitError(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 4 })
	schedulingParents(t, f, 3)
	type limitedAttempt struct {
		outcome chan error
	}
	entered := make(chan limitedAttempt, 4)
	var calls atomic.Int32
	mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Concurrency: 2,
		Create: func(ctx context.Context, _ adapter.Outbound, _ warp.Family) (Candidate, error) {
			sequence := calls.Add(1)
			candidate, _ := testCandidate(fmt.Sprintf("198.51.100.%d", sequence), time.Millisecond)
			attempt := limitedAttempt{make(chan error, 1)}
			entered <- attempt
			select {
			case <-ctx.Done():
				return candidate, ctx.Err()
			case err := <-attempt.outcome:
				return candidate, err
			}
		}, Logf: func(string, ...any) {}})
	t.Cleanup(mgr.Stop)
	mgr.Start()
	var attempts []limitedAttempt
	for index := 0; index < 2; index++ {
		select {
		case attempt := <-entered:
			attempts = append(attempts, attempt)
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent registration did not start")
		}
	}
	sub := f.subs.Lookup(subscription.WarpSubscriptionID)
	attempts[0].outcome <- &warp.RateLimitError{RetryAt: time.Now().Add(time.Minute)}
	awaitRecoveryCondition(t, func() bool { return strings.Contains(sub.GetLastError(), "registration paused") })
	attempts[1].outcome <- nil
	awaitRecoveryCondition(t, func() bool { return sub.ManagedNodes().Size() == 1 })
	if !strings.Contains(sub.GetLastError(), "registration paused") || calls.Load() != 2 {
		t.Fatal("a previously admitted successful registration hid or bypassed the global API pause")
	}
	mgr.Stop()
}
