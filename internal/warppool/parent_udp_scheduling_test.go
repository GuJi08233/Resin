package warppool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func pauseWithin(t *testing.T, failure parentFailure, want time.Duration) {
	t.Helper()
	if remaining := time.Until(failure.retryAt); remaining < want-time.Minute || remaining > want {
		t.Fatalf("lane pause=%s, want about %s: %+v", remaining, want, failure)
	}
}

func TestManagerRejectedParentUDPPausesEveryFamilyAtOnce(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack = "dual" })
	calls := 0
	manager := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		calls++
		return Candidate{}, &warp.ParentUDPError{Rejected: true, Err: errors.New("socks5: request rejected, code=7")}
	})
	for index := 0; index < 6; index++ {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
	}
	if calls != 1 {
		t.Fatalf("a parent that refused UDP was retried through another family: calls=%d", calls)
	}
	for _, family := range []warp.Family{warp.IPv6, warp.IPv4} {
		pauseWithin(t, manager.parentFailures[parentFamily{f.parent.Hash, family}], parentFailureCooldown)
	}
	message := f.subs.Lookup(subscription.WarpSubscriptionID).GetLastError()
	for _, want := range []string{"scope=parent", "failures=3/3", "code=7", "parent paused for all families until "} {
		if !strings.Contains(message, want) {
			t.Fatalf("parent-wide pause lacks %q: %q", want, message)
		}
	}
}

func TestManagerUnansweredParentUDPChargesEveryFamily(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpStack = "dual" })
	var families []warp.Family
	manager := f.manager(t, func(_ context.Context, _ adapter.Outbound, family warp.Family) (Candidate, error) {
		families = append(families, family)
		return Candidate{}, &warp.ParentUDPError{Err: errors.New("no DNS reply")}
	})
	for index := 0; index < 8; index++ {
		expireSchedulingSlotCooldowns(manager)
		manager.reconcile(context.Background())
	}
	if len(families) != parentFailureLimit {
		t.Fatalf("unanswered UDP was charged per family instead of per parent: %v", families)
	}
	for _, family := range []warp.Family{warp.IPv6, warp.IPv4} {
		failure := manager.parentFailures[parentFamily{f.parent.Hash, family}]
		if failure.consecutive != parentFailureLimit {
			t.Fatalf("%s lane missed shared parent failures: %+v", family, failure)
		}
		pauseWithin(t, failure, parentFailureCooldown)
	}
}

func TestManagerRepeatedFailureStreaksEscalateUntilSuccess(t *testing.T) {
	f := newPoolFixture(t)
	fail := true
	manager := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		if fail {
			return Candidate{}, context.DeadlineExceeded
		}
		candidate, _ := testCandidate("198.51.100.20", 10*time.Millisecond)
		return candidate, nil
	})
	lane := parentFamily{f.parent.Hash, warp.IPv4}
	expire := func() {
		failure := manager.parentFailures[lane]
		failure.retryAt = time.Now().Add(-time.Second)
		manager.parentFailures[lane] = failure
	}
	for round, want := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour} {
		if round > 0 {
			expire()
		}
		for index := 0; index < parentFailureLimit; index++ {
			expireSchedulingSlotCooldowns(manager)
			manager.reconcile(context.Background())
		}
		pauseWithin(t, manager.parentFailures[lane], want)
	}
	if failureCooldown(10) != parentFailureMaxCooldown {
		t.Fatal("escalated pause exceeded its upper bound")
	}
	expire()
	fail = false
	expireSchedulingSlotCooldowns(manager)
	manager.reconcile(context.Background())
	if len(manager.active) != 1 {
		t.Fatal("expired escalated pause did not allow a retry")
	}
	if _, kept := manager.parentFailures[lane]; kept {
		t.Fatal("success did not reset the escalation")
	}
}
