package warppool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func schedulingParents(t *testing.T, f *poolFixture, count int) []*node.NodeEntry {
	t.Helper()
	parents := []*node.NodeEntry{f.parent}
	for index := 1; index < count; index++ {
		raw, err := json.Marshal(map[string]any{
			"type": "socks", "server": fmt.Sprintf("parent-%d.example", index), "server_port": 1080,
		})
		if err != nil {
			t.Fatal(err)
		}
		hash := node.HashFromRawOptions(raw)
		tag := fmt.Sprintf("parent-%d", index)
		f.sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{tag}})
		f.pool.AddNodeFromSub(hash, raw, f.sub.ID)
		parent, exists := f.pool.GetEntry(hash)
		if !exists {
			t.Fatal("additional parent was not added")
		}
		var outbound adapter.Outbound = &memoryOutbound{name: tag, networks: []string{"tcp", "udp"}}
		parent.Outbound.Store(&outbound)
		parent.SetEgressIP(netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", index+1)))
		f.pool.RecordResult(hash, true)
		parents = append(parents, parent)
		t.Cleanup(func() { f.pool.RemoveNodeFromSub(hash, f.sub.ID) })
	}
	slices.SortFunc(parents, func(a, b *node.NodeEntry) int { return slices.Compare(a.Hash[:], b.Hash[:]) })
	return parents
}

// Expiring slot cooldowns models an earlier attempt becoming due while a
// different parent spends a long time verifying its tunnel, without sleeping.
func expireSchedulingSlotCooldowns(manager *Manager) {
	for key := range manager.nextTry {
		manager.nextTry[key] = time.Now().Add(-time.Second)
	}
}

func TestManagerSlowFailuresVisitEveryParentBeforeRetrying(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) {
		cfg.WarpStack, cfg.WarpMaxNodes, cfg.WarpReconnectAttempts = "dual", 300, 3
	})
	parents := schedulingParents(t, f, 3)
	parentByOutbound := make(map[adapter.Outbound]node.Hash)
	for _, parent := range parents {
		parentByOutbound[*parent.Outbound.Load()] = parent.Hash
	}
	type attemptedParent struct {
		hash   node.Hash
		family warp.Family
	}
	var attempted []attemptedParent
	var manager *Manager
	manager = f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		hash, exists := parentByOutbound[parent]
		if !exists {
			t.Fatal("factory received an unknown parent")
		}
		expireSchedulingSlotCooldowns(manager)
		attempted = append(attempted, attemptedParent{hash, family})
		return Candidate{}, context.DeadlineExceeded
	})
	for index := 0; index < len(parents)*2; index++ {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
		if len(attempted) != index+1 {
			t.Fatalf("ordinary timeout consumed multiple factory calls: got %d after %d slots", len(attempted), index+1)
		}
		if index == 0 && attempted[index].family != warp.IPv6 {
			t.Fatal("dual-stack recovery did not prefer IPv6 on the initial tie")
		}
		if index < len(parents) {
			for previous := 0; previous < index; previous++ {
				if attempted[index].hash == attempted[previous].hash {
					t.Fatalf("retried a slow parent before visiting all available parents: history=%+v", attempted)
				}
			}
		}
	}
	if len(manager.active) != 0 || f.subs.Lookup(subscription.WarpSubscriptionID).ManagedNodes().Size() != 0 {
		t.Fatal("failed verification candidates appeared as WARP subscription nodes")
	}
	if f.pool.Size() != len(parents) {
		t.Fatalf("failed candidates changed the original pool: got %d nodes, want %d", f.pool.Size(), len(parents))
	}
}

func TestManagerBadParentsDoNotBlockAWorkingParent(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) {
		cfg.WarpStack, cfg.WarpMaxNodes, cfg.WarpReconnectAttempts = "dual", 1, 3
	})
	parents := schedulingParents(t, f, 3)
	working := parents[len(parents)-1]
	workingOutbound := *working.Outbound.Load()
	var attempts []adapter.Outbound
	var attemptedFamilies []warp.Family
	var selected *memoryOutbound
	manager := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		attempts = append(attempts, parent)
		attemptedFamilies = append(attemptedFamilies, family)
		if parent != workingOutbound {
			return Candidate{}, errors.New("verify WARP through parent: deadline exceeded")
		}
		if family != warp.IPv6 {
			t.Fatal("dual-stack recovery did not try the working parent's IPv6 route first")
		}
		candidate, outbound := testCandidate("2001:db8::200", 12*time.Millisecond)
		selected = outbound
		return candidate, nil
	})
	warpSub := f.subs.Lookup(subscription.WarpSubscriptionID)
	for index, parent := range parents {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
		if len(attempts) != index+1 || attempts[index] != *parent.Outbound.Load() {
			t.Fatalf("failed parents prevented progress: calls=%d, expected parent %s at position %d", len(attempts), parent.Hash.Hex(), index)
		}
		if index < len(parents)-1 {
			message := warpSub.GetLastError()
			for _, required := range []string{
				"parent=" + parent.Hash.Hex(),
				"name=" + fmt.Sprintf("%q", f.pool.ResolveNodeDisplayTag(parent.Hash)),
				"protocol=" + (*parent.Outbound.Load()).Type(),
				"family=" + string(attemptedFamilies[index]), "slot=1", "failures=1/3", "deadline exceeded",
			} {
				if !strings.Contains(message, required) {
					t.Fatalf("WARP error lacks actionable parent context %q: %q", required, message)
				}
			}
			if warpSub.ManagedNodes().Size() != 0 || warpSub.LastUpdatedNs.Load() != 0 {
				t.Fatal("failed candidates were counted or marked as a successful update")
			}
			if warpSub.LastCheckedNs.Load() == 0 {
				t.Fatal("attempt did not update last checked")
			}
		}
	}
	if len(manager.active) != 1 || warpSub.ManagedNodes().Size() != 1 || selected == nil || selected.closed.Load() != 0 {
		t.Fatal("working parent failed to publish a live WARP exit")
	}
	for key := range manager.active {
		if key.parent != working.Hash || key.family != warp.IPv6 {
			t.Fatalf("published exit belongs to wrong parent or family: %+v", key)
		}
	}
	if warpSub.GetLastError() != "" || warpSub.LastUpdatedNs.Load() == 0 || f.pool.Size() != len(parents)+1 {
		t.Fatal("successful publication failed to clear the error, update time, or preserve original nodes")
	}
}

func TestManagerRepeatedTransportFailuresPauseParentFamilyAcrossAllSlots(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) {
		cfg.WarpStack, cfg.WarpExitsPerFamily, cfg.WarpMaxNodes = "ipv4", 4, 8
		cfg.WarpReconnectAttempts = 10
	})
	calls := 0
	manager := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		calls++
		return Candidate{}, context.DeadlineExceeded
	})
	for index := 0; index < 3; index++ {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
		if calls != index+1 {
			t.Fatalf("transport error retried within one slot: calls=%d, slots=%d", calls, index+1)
		}
	}
	failureKey := parentFamily{f.parent.Hash, warp.IPv4}
	failure := manager.parentFailures[failureKey]
	if failure.consecutive != 3 || time.Until(failure.retryAt) < 29*time.Minute || time.Until(failure.retryAt) > 30*time.Minute {
		t.Fatalf("three failures did not pause the parent for 30 minutes: %+v", failure)
	}
	message := f.subs.Lookup(subscription.WarpSubscriptionID).GetLastError()
	if !strings.Contains(message, "failures=3/3") || !strings.Contains(message, "parent family paused until ") {
		t.Fatalf("pause was not reported in the managed subscription: %q", message)
	}
	for index := 0; index < 12; index++ {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
	}
	if calls != 3 {
		t.Fatalf("paused parent was attempted through another family/slot: got %d calls, want 3", calls)
	}
	// Advance only the parent's retry deadline; no network or wall-clock wait.
	failure.retryAt = time.Now().Add(-time.Second)
	manager.parentFailures[failureKey] = failure
	expireSchedulingSlotCooldowns(manager)
	manager.reconcile(context.Background())
	if calls != 4 {
		t.Fatalf("expired parent pause did not allow retry: got %d calls", calls)
	}
	if restarted := manager.parentFailures[failureKey]; restarted.consecutive != 1 || !restarted.retryAt.IsZero() {
		t.Fatalf("retry after pause retained exhausted failure streak: %+v", restarted)
	}
	if f.subs.Lookup(subscription.WarpSubscriptionID).ManagedNodes().Size() != 0 || f.pool.Size() != 1 {
		t.Fatal("transport failures published candidates or removed the original parent")
	}
}

func TestManagerDisableResetsParentPauseAndSchedulingState(t *testing.T) {
	f := newPoolFixture(t)
	calls := 0
	manager := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		calls++
		return Candidate{}, context.DeadlineExceeded
	})
	for index := 0; index < 3; index++ {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
	}
	if calls != 3 || manager.parentFailures[parentFamily{f.parent.Hash, warp.IPv4}].retryAt.IsZero() {
		t.Fatal("test did not establish the parent pause")
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpEnabled = false })
	manager.reconcile(context.Background())
	if len(manager.parentFailures) != 0 || len(manager.nextTry) != 0 || manager.hasLastAttempt {
		t.Fatal("disabling WARP retained parent pause or scheduling state")
	}
	if calls != 3 {
		t.Fatal("disabled WARP still attempted a registration")
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpEnabled = true })
	manager.reconcile(context.Background())
	if calls != 4 || manager.parentFailures[parentFamily{f.parent.Hash, warp.IPv4}].consecutive != 1 {
		t.Fatal("re-enabling WARP did not immediately retry the previously paused parent with a fresh streak")
	}
}
