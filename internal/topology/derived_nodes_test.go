package topology

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func derivedTestSetup(t *testing.T) (*GlobalNodePool, *SubscriptionManager, *subscription.Subscription, *node.NodeEntry) {
	t.Helper()
	mgr := NewSubscriptionManager()
	mgr.Register(subscription.NewWarpSubscription(true))
	sub := subscription.NewSubscription("parent-sub", "MySub", "", true, false)
	mgr.Register(sub)
	pool := newTestPool(mgr)
	raw := json.RawMessage(`{"type":"socks","server":"parent.example","server_port":1080}`)
	hash := node.HashFromRawOptions(raw)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"US-original"}})
	pool.AddNodeFromSub(hash, raw, sub.ID)
	parent, _ := pool.GetEntry(hash)
	ob, err := (&testutil.StubOutboundBuilder{}).Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	parent.Outbound.Store(&ob)
	pool.RecordResult(hash, true)
	parent.SetEgressIP(netip.MustParseAddr("192.0.2.1"))
	latency := 15 * time.Millisecond
	pool.RecordLatency(hash, "cloudflare.com", &latency)
	t.Cleanup(func() { parent.LatencyTable.Close() })
	return pool, mgr, sub, parent
}

func derivedTestEntry(t *testing.T, parent *node.NodeEntry, family string) *node.NodeEntry {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"type": "resin-warp", "parent_hash": parent.Hash.Hex(), "family": family,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := node.NewNodeEntry(node.HashFromRawOptions(raw), raw, time.Now(), 16)
	parentHash := parent.Hash
	entry.ParentHash = &parentHash
	entry.WarpFamily = family
	ob, err := (&testutil.StubOutboundBuilder{}).Build(raw)
	if err != nil {
		t.Fatal(err)
	}
	entry.Outbound.Store(&ob)
	entry.SetEgressIP(netip.MustParseAddr("198.51.100.1"))
	entry.LatencyTable.Update("cloudflare.com", 20*time.Millisecond, time.Minute)
	t.Cleanup(func() { entry.LatencyTable.Close() })
	return entry
}

func TestDerivedNodeInheritsFiltersAndSubscriptionChanges(t *testing.T) {
	pool, mgr, sub, parent := derivedTestSetup(t)
	child := derivedTestEntry(t, parent, "ipv4")
	plat := platform.NewPlatform("filtered", "Filtered", []*regexp.Regexp{regexp.MustCompile(`^WARP/MySub/US-original/WARP-ipv4$`)}, nil)
	pool.RegisterPlatform(plat)
	excludeWarp := platform.NewPlatformWithTagFilter("without-warp", "NoWARP", node.TagFilter{
		MustNot: []*regexp.Regexp{regexp.MustCompile(`^WARP/`)},
	}, nil)
	pool.RegisterPlatform(excludeWarp)
	pool.RebuildAllPlatforms()
	if !pool.AddDerivedNode(child) {
		t.Fatal("healthy derived node was rejected")
	}
	if !plat.View().Contains(child.Hash) {
		t.Fatal("derived node did not match the unified WARP subscription tag")
	}
	if !excludeWarp.View().Contains(parent.Hash) || excludeWarp.View().Contains(child.Hash) {
		t.Fatal("excluding WARP also excluded the parent or retained a WARP child")
	}
	if got := pool.ResolveNodeDisplayTag(child.Hash); got != "WARP/MySub/US-original/WARP-ipv4" {
		t.Fatalf("derived display tag = %q", got)
	}
	if _, exists := sub.ManagedNodes().LoadNode(child.Hash); exists {
		t.Fatal("runtime node was inserted into the subscription's managed nodes")
	}
	warp := mgr.Lookup(subscription.WarpSubscriptionID)
	if _, exists := warp.ManagedNodes().LoadNode(child.Hash); !exists {
		t.Fatal("runtime node was not added to the WARP subscription")
	}
	if got := string(parent.RawOptions); got != `{"type":"socks","server":"parent.example","server_port":1080}` {
		t.Fatalf("parent configuration changed: %s", got)
	}

	// Kept parent nodes are re-added on refresh/rename. The derived view must
	// immediately re-evaluate the changed original tags as well.
	sub.ManagedNodes().StoreNode(parent.Hash, subscription.ManagedNode{Tags: []string{"EU-new"}})
	pool.AddNodeFromSub(parent.Hash, parent.RawOptions, sub.ID)
	if plat.View().Contains(child.Hash) {
		t.Fatal("derived node retained stale tags after parent refresh")
	}
	if got := pool.ResolveNodeDisplayTag(child.Hash); got != "WARP/MySub/EU-new/WARP-ipv4" {
		t.Fatalf("updated derived display tag = %q", got)
	}
	warp.SetEnabled(false)
	pool.RebuildAllPlatforms()
	if !pool.IsNodeDisabled(child.Hash) || pool.IsNodeDisabled(parent.Hash) {
		t.Fatal("disabling WARP must disable only derived nodes")
	}
	warp.SetEnabled(true)
	sub.SetEnabled(false)
	pool.RebuildAllPlatforms()
	if !pool.IsNodeDisabled(child.Hash) {
		t.Fatal("derived node did not inherit disabled subscription")
	}
	sub.SetEnabled(true)
	sub.ManagedNodes().StoreNode(parent.Hash, subscription.ManagedNode{Tags: []string{"US-original"}, Evicted: true})
	pool.NotifyNodeDirty(parent.Hash)
	if !pool.IsNodeDisabled(child.Hash) {
		t.Fatal("derived node did not inherit parent eviction")
	}
}

func TestDerivedNodeReferencesAndParentRemoval(t *testing.T) {
	pool, mgr, firstSub, parent := derivedTestSetup(t)
	var added, subChanges int
	var removed []node.Hash
	pool.SetOnNodeAdded(func(node.Hash) { added++ })
	pool.onSubNodeChanged = func(string, node.Hash, bool) { subChanges++ }
	pool.SetOnNodeRemoved(func(hash node.Hash, _ *node.NodeEntry) { removed = append(removed, hash) })
	child := derivedTestEntry(t, parent, "ipv6")
	if !pool.AddDerivedNode(child) {
		t.Fatal("derived node rejected")
	}
	if added != 0 || subChanges != 0 {
		t.Fatal("publishing runtime node invoked normal build/persistence callbacks")
	}
	secondSub := subscription.NewSubscription("second", "Second", "", true, false)
	secondSub.ManagedNodes().StoreNode(parent.Hash, subscription.ManagedNode{Tags: []string{"same-parent"}})
	mgr.Register(secondSub)
	pool.AddNodeFromSub(parent.Hash, parent.RawOptions, secondSub.ID)
	if child.SubscriptionCount() != 1 || child.SubscriptionIDs()[0] != subscription.WarpSubscriptionID {
		t.Fatalf("derived references = %v, want only WARP subscription", child.SubscriptionIDs())
	}
	pool.RemoveNodeFromSub(parent.Hash, firstSub.ID)
	if child.SubscriptionCount() != 1 || pool.IsNodeDisabled(child.Hash) {
		t.Fatal("removing one parent subscription lost surviving derived ownership")
	}
	pool.RemoveNodeFromSub(parent.Hash, secondSub.ID)
	if _, exists := pool.GetEntry(child.Hash); exists {
		t.Fatal("derived node survived final parent removal")
	}
	if _, exists := mgr.Lookup(subscription.WarpSubscriptionID).ManagedNodes().LoadNode(child.Hash); exists {
		t.Fatal("removed child retained stale WARP managed membership")
	}
	if len(removed) != 2 || removed[0] != child.Hash || removed[1] != parent.Hash {
		t.Fatalf("removal order = %v, want child before parent", removed)
	}
	if subChanges != 3 {
		t.Fatalf("subscription persistence callbacks = %d, want only three parent mutations", subChanges)
	}
	if pool.RemoveDerivedNode(parent.Hash) || pool.RemoveDerivedNode(child.Hash) {
		t.Fatal("runtime removal accepted original or already removed node")
	}
}

func TestDerivedTagsIgnoreDisabledParentSubscriptions(t *testing.T) {
	pool, mgr, first, parent := derivedTestSetup(t)
	second := subscription.NewSubscription("second", "Second", "", true, false)
	second.ManagedNodes().StoreNode(parent.Hash, subscription.ManagedNode{Tags: []string{"available"}})
	mgr.Register(second)
	pool.AddNodeFromSub(parent.Hash, parent.RawOptions, second.ID)
	child := derivedTestEntry(t, parent, "ipv4")
	if !pool.AddDerivedNode(child) {
		t.Fatal("failed to publish WARP child")
	}
	first.SetEnabled(false)
	pool.RebuildAllPlatforms()
	name, enabled, tags, ok := pool.MakeSubLookup()(subscription.WarpSubscriptionID, child.Hash)
	if !ok || !enabled || name != subscription.WarpSubscriptionName || len(tags) != 1 ||
		tags[0] != "Second/available/WARP-ipv4" {
		t.Fatalf("disabled parent holder still contributed WARP tags: %q %v %v %v", name, enabled, tags, ok)
	}
}

func TestDerivedNodeFollowsParentCircuitInRoutableView(t *testing.T) {
	pool, _, _, parent := derivedTestSetup(t)
	child := derivedTestEntry(t, parent, "ipv4")
	plat := platform.NewPlatform("all", "All", nil, nil)
	pool.RegisterPlatform(plat)
	if !pool.AddDerivedNode(child) || !plat.View().Contains(child.Hash) {
		t.Fatal("derived node should initially be routable")
	}
	for i := 0; i < 3; i++ {
		pool.RecordResult(parent.Hash, false)
	}
	if plat.View().Contains(child.Hash) || !pool.IsNodeDisabled(child.Hash) {
		t.Fatal("parent circuit opening left derived node routable")
	}
	pool.RecordResult(parent.Hash, true)
	if !plat.View().Contains(child.Hash) {
		t.Fatal("parent recovery did not restore derived route")
	}
}

func TestDerivedNodeUpdatesDoNotPersist(t *testing.T) {
	pool, _, _, parent := derivedTestSetup(t)
	child := derivedTestEntry(t, parent, "ipv4")
	if !pool.AddDerivedNode(child) {
		t.Fatal("derived node rejected")
	}
	var dynamic, latency int
	pool.onNodeDynamicChanged = func(node.Hash) { dynamic++ }
	pool.onNodeLatencyChanged = func(node.Hash, string) { latency++ }
	pool.RecordResult(child.Hash, false)
	duration := 10 * time.Millisecond
	pool.RecordLatency(child.Hash, "example.com", &duration)
	ip := netip.MustParseAddr("198.51.100.2")
	pool.UpdateNodeEgressIP(child.Hash, &ip, nil)
	if dynamic != 0 || latency != 0 {
		t.Fatalf("runtime updates emitted persistence callbacks: dynamic=%d latency=%d", dynamic, latency)
	}
	if child.FailureCount.Load() != 1 || child.GetEgressIP() != ip {
		t.Fatal("runtime health/egress updates were lost")
	}
	pool.RecordResult(parent.Hash, false)
	pool.RecordLatency(parent.Hash, "example.com", &duration)
	pool.UpdateNodeEgressIP(parent.Hash, &ip, nil)
	if dynamic == 0 || latency == 0 {
		t.Fatal("original node persistence callbacks were suppressed")
	}
}

func TestDerivedNodeRejectsInvalidOrStaleParents(t *testing.T) {
	pool, _, sub, parent := derivedTestSetup(t)
	if pool.AddDerivedNode(nil) || pool.AddDerivedNode(parent) {
		t.Fatal("invalid derived node accepted")
	}
	child := derivedTestEntry(t, parent, "ipv4")
	sub.SetEnabled(false)
	if pool.AddDerivedNode(child) {
		t.Fatal("disabled parent accepted")
	}
	sub.SetEnabled(true)
	parent.CircuitOpenSince.Store(time.Now().UnixNano())
	if pool.AddDerivedNode(child) {
		t.Fatal("unhealthy parent accepted")
	}
	pool.RecordResult(parent.Hash, true)
	if !pool.AddDerivedNode(child) || pool.AddDerivedNode(child) {
		t.Fatal("derived publication is not unique")
	}
	nested := derivedTestEntry(t, child, "ipv6")
	if pool.AddDerivedNode(nested) {
		t.Fatal("nested derived node accepted")
	}
	if !pool.RemoveDerivedNode(child.Hash) {
		t.Fatal("derived removal failed")
	}
	pool.RemoveNodeFromSub(parent.Hash, sub.ID)
	if pool.AddDerivedNode(child) {
		t.Fatal("removed parent accepted")
	}
}

func TestDerivedPublishRacingParentRemovalCannotLeaveOrphans(t *testing.T) {
	for i := 0; i < 20; i++ {
		pool, _, sub, parent := derivedTestSetup(t)
		child := derivedTestEntry(t, parent, "ipv4")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); pool.AddDerivedNode(child) }()
		go func() { defer wg.Done(); pool.RemoveNodeFromSub(parent.Hash, sub.ID) }()
		wg.Wait()
		if pool.Size() != 0 {
			t.Fatalf("iteration %d left %d nodes after parent removal", i, pool.Size())
		}
	}
}

func TestDerivedNodeRejectsRecreatedParentWithSameHash(t *testing.T) {
	pool, _, sub, oldParent := derivedTestSetup(t)
	child := derivedTestEntry(t, oldParent, "ipv4")
	pool.RemoveNodeFromSub(oldParent.Hash, sub.ID)
	pool.AddNodeFromSub(oldParent.Hash, oldParent.RawOptions, sub.ID)
	newParent, _ := pool.GetEntry(oldParent.Hash)
	if newParent == oldParent {
		t.Fatal("test did not replace the parent object")
	}
	ob, err := (&testutil.StubOutboundBuilder{}).Build(newParent.RawOptions)
	if err != nil {
		t.Fatal(err)
	}
	newParent.Outbound.Store(&ob)
	pool.RecordResult(newParent.Hash, true)
	t.Cleanup(func() { newParent.LatencyTable.Close() })

	if pool.AddDerivedNode(child, oldParent) {
		t.Fatal("derived outbound using old parent attached to its same-hash replacement")
	}
	if _, exists := pool.GetEntry(child.Hash); exists {
		t.Fatal("rejected derived node was still published")
	}
	if !pool.AddDerivedNode(child, newParent) {
		t.Fatal("derived node with the current parent object was rejected")
	}
}
