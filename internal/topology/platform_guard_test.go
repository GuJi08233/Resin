package topology

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func addRoutableNodeForGuard(
	t *testing.T,
	pool *GlobalNodePool,
	sub *subscription.Subscription,
	raw json.RawMessage,
	egressIP string,
) node.Hash {
	t.Helper()
	h := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(h, subscription.ManagedNode{Tags: []string{h.Hex()}})
	pool.AddNodeFromSub(h, raw, sub.ID)
	entry, ok := pool.GetEntry(h)
	if !ok {
		t.Fatal("node entry not found")
	}
	entry.LatencyTable.LoadEntry("example.com", node.DomainLatencyStats{
		Ewma:        100 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	entry.SetEgressIP(netip.MustParseAddr(egressIP))
	pool.RecordResult(h, true)
	return h
}

func TestPlatformGuard_CooldownExcludesByEgressIPForPlatformOnly(t *testing.T) {
	subMgr := NewSubscriptionManager()
	sub := subscription.NewSubscription("s1", "Sub", "url", true, false)
	subMgr.Register(sub)
	pool := newTestPool(subMgr)

	guarded := platform.NewPlatform("p-guard", "Guarded", nil, nil)
	guarded.StatusCodeRules = []platform.StatusCodeRule{{
		CodeFrom: 429, CodeTo: 429,
		Action: platform.StatusCodeActionCooldown, Cooldown: time.Minute,
	}}
	other := platform.NewPlatform("p-other", "Other", nil, nil)
	pool.RegisterPlatform(guarded)
	pool.RegisterPlatform(other)

	guard := NewPlatformGuard(pool)
	pool.SetPlatformGuard(guard)

	h1 := addRoutableNodeForGuard(t, pool, sub, json.RawMessage(`{"type":"ss","server":"1.1.1.1"}`), "203.0.113.20")
	h2 := addRoutableNodeForGuard(t, pool, sub, json.RawMessage(`{"type":"ss","server":"1.1.1.2"}`), "203.0.113.20")
	pool.RebuildAllPlatforms()

	if !guarded.View().Contains(h1) || !guarded.View().Contains(h2) {
		t.Fatal("both nodes should be routable before the 429")
	}

	pool.RecordUpstreamStatus(guarded.ID, h1, netip.MustParseAddr("203.0.113.20"), 429)
	guard.rebuildDirty()

	if guarded.View().Contains(h1) || guarded.View().Contains(h2) {
		t.Fatal("all nodes sharing the cooled egress IP must leave the platform view")
	}
	if !other.View().Contains(h1) || !other.View().Contains(h2) {
		t.Fatal("other platforms must not be affected")
	}
	entries := pool.ListPlatformExclusions(guarded.ID)
	if len(entries) != 1 || entries[0].EgressIP.String() != "203.0.113.20" {
		t.Fatalf("unexpected exclusions: %+v", entries)
	}
	if entries[0].Action != platform.StatusCodeActionCooldown || entries[0].ExpiresAt.IsZero() {
		t.Fatalf("expected a timed cooldown, got %+v", entries[0])
	}

	if _, ok := pool.UnblockPlatformEgress(guarded.ID, "203.0.113.20"); !ok {
		t.Fatal("unblock should remove the exclusion")
	}
	guard.rebuildDirty()
	if !guarded.View().Contains(h1) || !guarded.View().Contains(h2) {
		t.Fatal("nodes should return after the cooldown is cleared")
	}
}

func TestPlatformGuard_BlacklistIsPermanentAndTakesPrecedence(t *testing.T) {
	subMgr := NewSubscriptionManager()
	sub := subscription.NewSubscription("s1", "Sub", "url", true, false)
	subMgr.Register(sub)
	pool := newTestPool(subMgr)

	plat := platform.NewPlatform("p-guard", "Guarded", nil, nil)
	plat.StatusCodeRules = []platform.StatusCodeRule{
		{CodeFrom: 429, CodeTo: 429, Action: platform.StatusCodeActionCooldown, Cooldown: time.Minute},
		{CodeFrom: 403, CodeTo: 403, Action: platform.StatusCodeActionBlacklist},
	}
	pool.RegisterPlatform(plat)
	guard := NewPlatformGuard(pool)
	pool.SetPlatformGuard(guard)

	h := addRoutableNodeForGuard(t, pool, sub, json.RawMessage(`{"type":"ss","server":"9.9.9.9"}`), "203.0.113.30")
	pool.RebuildAllPlatforms()
	ip := netip.MustParseAddr("203.0.113.30")

	pool.RecordUpstreamStatus(plat.ID, h, ip, 429)
	guard.rebuildDirty()
	if plat.View().Contains(h) {
		t.Fatal("429 should cool down the node")
	}

	pool.RecordUpstreamStatus(plat.ID, h, ip, 403)
	guard.rebuildDirty()
	entries := pool.ListPlatformExclusions(plat.ID)
	if len(entries) != 1 || entries[0].Action != platform.StatusCodeActionBlacklist {
		t.Fatalf("403 should upgrade the cooldown to a blacklist, got %+v", entries)
	}
	if !entries[0].ExpiresAt.IsZero() {
		t.Fatalf("blacklist must not expire, got %v", entries[0].ExpiresAt)
	}

	guard.expire(time.Now().Add(24 * time.Hour))
	guard.rebuildDirty()
	if plat.View().Contains(h) {
		t.Fatal("blacklist must survive expiry sweeps")
	}

	pool.UnblockPlatformEgress(plat.ID, ip.String())
	guard.rebuildDirty()
	if !plat.View().Contains(h) {
		t.Fatal("manual unblock should restore the node")
	}
}
