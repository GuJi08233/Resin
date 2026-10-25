package warppool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/topology"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// memoryOutbound never opens a socket. Close accounting catches ownership
// leaks when candidate selection, parent cleanup and manager cleanup overlap.
type memoryOutbound struct {
	name     string
	networks []string
	closed   atomic.Int32
}

func (o *memoryOutbound) Type() string { return "memory" }
func (o *memoryOutbound) Tag() string  { return o.name }
func (o *memoryOutbound) Network() []string {
	if o.networks != nil {
		return o.networks
	}
	return []string{"tcp", "udp"}
}
func (o *memoryOutbound) Dependencies() []string { return nil }
func (o *memoryOutbound) Close() error           { o.closed.Add(1); return nil }
func (*memoryOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, errors.New("test must not dial")
}
func (*memoryOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("test must not open a packet socket")
}

type poolFixture struct {
	pool   *topology.GlobalNodePool
	subs   *topology.SubscriptionManager
	sub    *subscription.Subscription
	parent *node.NodeEntry
	config atomic.Pointer[config.RuntimeConfig]
}

func newPoolFixture(t *testing.T) *poolFixture {
	t.Helper()
	f := &poolFixture{}
	subs := topology.NewSubscriptionManager()
	f.subs = subs
	f.sub = subscription.NewSubscription("sub", "Original", "", true, false)
	subs.Register(f.sub)
	subs.Register(subscription.NewWarpSubscription(true))
	f.pool = topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup: subs.Lookup, MaxConsecutiveFailures: func() int { return 3 }, MaxLatencyTableEntries: 8,
		OnNodeRemoved: func(_ node.Hash, entry *node.NodeEntry) {
			if value := entry.Outbound.Swap(nil); value != nil && *value != nil {
				if closer, ok := (*value).(interface{ Close() error }); ok {
					_ = closer.Close()
				}
			}
			if entry.LatencyTable != nil {
				entry.LatencyTable.Close()
			}
		},
	})
	raw := json.RawMessage(`{"type":"socks","server":"parent.example","server_port":1080}`)
	hash := node.HashFromRawOptions(raw)
	f.sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"parent"}})
	f.pool.AddNodeFromSub(hash, raw, f.sub.ID)
	f.parent, _ = f.pool.GetEntry(hash)
	var outbound adapter.Outbound = &memoryOutbound{name: "parent"}
	f.parent.Outbound.Store(&outbound)
	f.pool.RecordResult(hash, true)
	f.parent.SetEgressIP(netip.MustParseAddr("192.0.2.1"))
	cfg := config.NewDefaultRuntimeConfig()
	cfg.WarpEnabled, cfg.WarpStack = true, "ipv4"
	cfg.WarpMaxNodes, cfg.WarpExitsPerFamily, cfg.WarpReconnectAttempts = 8, 1, 3
	f.config.Store(cfg)
	t.Cleanup(func() { f.pool.RemoveNodeFromSub(hash, f.sub.ID) })
	return f
}

func (f *poolFixture) configure(change func(*config.RuntimeConfig)) {
	previous := f.config.Load()
	next := *previous
	change(&next)
	f.config.Store(&next)
	if next.WarpEnabled != previous.WarpEnabled {
		f.subs.Lookup(subscription.WarpSubscriptionID).SetEnabled(next.WarpEnabled)
	}
}

func (f *poolFixture) manager(t *testing.T, create CreateFunc) *Manager {
	t.Helper()
	mgr := NewManager(Options{Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load, Create: create, Logf: func(string, ...any) {}})
	t.Cleanup(func() { mgr.Stop(); mgr.clear() })
	return mgr
}

func testCandidate(ip string, latency time.Duration) (Candidate, *memoryOutbound) {
	outbound := &memoryOutbound{name: ip}
	return Candidate{Outbound: outbound, IP: netip.MustParseAddr(ip), Latency: latency}, outbound
}

func TestManagerDualStackSlotsAndLimit(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) {
		cfg.WarpStack, cfg.WarpExitsPerFamily, cfg.WarpMaxNodes = "dual", 2, 4
	})
	var created []*memoryOutbound
	mgr := f.manager(t, func(_ context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
		if parent != *f.parent.Outbound.Load() {
			t.Fatal("factory did not receive the original parent outbound")
		}
		ip := fmt.Sprintf("198.51.100.%d", len(created)+1)
		if family == warp.IPv6 {
			ip = fmt.Sprintf("2001:db8::%d", len(created)+1)
		}
		candidate, outbound := testCandidate(ip, time.Duration(len(created)+1)*10*time.Millisecond)
		created = append(created, outbound)
		return candidate, nil
	})
	for i := 0; i < 6; i++ {
		mgr.reconcile(context.Background())
	}
	if len(mgr.active) != 4 || len(created) != 4 || f.pool.Size() != 5 {
		t.Fatalf("active=%d created=%d pool=%d; want four derived plus original", len(mgr.active), len(created), f.pool.Size())
	}
	for _, family := range []warp.Family{warp.IPv4, warp.IPv6} {
		for index := 0; index < 2; index++ {
			exit, exists := mgr.active[slot{f.parent.Hash, family, index}]
			if !exists {
				t.Fatalf("missing family=%s index=%d", family, index)
			}
			if value := exit.entry.Outbound.Load(); value == nil || *value == nil {
				t.Fatal("published outbound was cleared with the local candidate")
			}
		}
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpMaxNodes = 2 })
	mgr.reconcile(context.Background())
	if len(mgr.active) != 2 || created[2].closed.Load() != 1 || created[3].closed.Load() != 1 {
		t.Fatal("lowering the limit did not close the two slowest exits")
	}
	if f.parent.Outbound.Load() == nil {
		t.Fatal("limiting derived exits removed the original outbound")
	}
}

func TestManagerReconnectsDuplicateUntilUnique(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpExitsPerFamily, cfg.WarpMaxNodes = 2, 2 })
	first, firstOB := testCandidate("198.51.100.1", 40*time.Millisecond)
	duplicateBest, duplicateBestOB := testCandidate("198.51.100.1", 10*time.Millisecond)
	duplicateSlow, duplicateSlowOB := testCandidate("198.51.100.1", 20*time.Millisecond)
	unique, uniqueOB := testCandidate("198.51.100.2", 30*time.Millisecond)
	queue := []Candidate{first, duplicateBest, duplicateSlow, unique}
	calls := 0
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		if calls >= len(queue) {
			t.Fatal("factory exceeded the bounded duplicate retry budget")
		}
		candidate := queue[calls]
		calls++
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	mgr.reconcile(context.Background())
	if calls != 4 || len(mgr.active) != 2 {
		t.Fatalf("factory calls=%d active=%d", calls, len(mgr.active))
	}
	if duplicateBestOB.closed.Load() != 1 || duplicateSlowOB.closed.Load() != 1 || firstOB.closed.Load() != 0 || uniqueOB.closed.Load() != 0 {
		t.Fatal("duplicate retries leaked candidates or closed selected unique exits")
	}
}

func TestManagerExhaustedDuplicatesKeepFastestRoute(t *testing.T) {
	f := newPoolFixture(t)
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpExitsPerFamily, cfg.WarpMaxNodes = 2, 2 })
	var queue []Candidate
	var outbounds []*memoryOutbound
	for _, latency := range []time.Duration{50, 40, 10, 30} {
		candidate, outbound := testCandidate("198.51.100.1", latency*time.Millisecond)
		queue, outbounds = append(queue, candidate), append(outbounds, outbound)
	}
	calls := 0
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		if calls >= len(queue) {
			t.Fatal("cooldown did not prevent repeated duplicate churn")
		}
		candidate := queue[calls]
		calls++
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	mgr.reconcile(context.Background())
	mgr.reconcile(context.Background())
	if len(mgr.active) != 1 || calls != 4 || f.pool.Size() != 2 {
		t.Fatalf("active=%d calls=%d pool=%d", len(mgr.active), calls, f.pool.Size())
	}
	for _, exit := range mgr.active {
		if mgr.latency(exit.entry) != 10*time.Millisecond {
			t.Fatalf("kept latency=%s instead of fastest duplicate", mgr.latency(exit.entry))
		}
	}
	for i, outbound := range outbounds {
		want := int32(1)
		if i == 2 {
			want = 0
		}
		if got := outbound.closed.Load(); got != want {
			t.Fatalf("candidate %d close count=%d want=%d", i, got, want)
		}
	}
}

func TestManagerNeverReplacesOriginalIP(t *testing.T) {
	f := newPoolFixture(t)
	var created []*memoryOutbound
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		candidate, outbound := testCandidate(f.parent.GetEgressIP().String(), time.Millisecond)
		created = append(created, outbound)
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	if len(created) != 3 || len(mgr.active) != 0 || f.pool.Size() != 1 || f.parent.Outbound.Load() == nil {
		t.Fatal("original-IP collision replaced the original route or ignored retry bound")
	}
	for _, outbound := range created {
		if outbound.closed.Load() != 1 {
			t.Fatal("discarded original-IP collision leaked its tunnel")
		}
	}
}

func TestManagerSkipsParentsWithoutUDP(t *testing.T) {
	f := newPoolFixture(t)
	(*f.parent.Outbound.Load()).(*memoryOutbound).networks = []string{"tcp"}
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		t.Fatal("TCP-only parent must not invoke the WARP factory")
		return Candidate{}, errors.New("unexpected factory call")
	})
	mgr.reconcile(context.Background())
	if len(mgr.active) != 0 || f.pool.Size() != 1 || f.parent.Outbound.Load() == nil {
		t.Fatal("skipping a TCP-only parent changed its original route")
	}
}

func TestManagerDeduplicatesAddressesChangedByRegularProbe(t *testing.T) {
	for _, collision := range []string{"derived", "original"} {
		for _, reconnect := range []string{"unique", "exhausted"} {
			t.Run(collision+"/"+reconnect, func(t *testing.T) {
				f := newPoolFixture(t)
				f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpExitsPerFamily, cfg.WarpMaxNodes = 2, 2 })
				var created []*memoryOutbound
				mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
					ip := fmt.Sprintf("198.51.100.%d", len(created)+1)
					if len(created) >= 2 && reconnect == "exhausted" {
						ip = "198.51.100.1"
						if collision == "original" {
							ip = f.parent.GetEgressIP().String()
						}
					}
					if len(created) >= 5 {
						t.Fatal("regular-probe collision exceeded the finite reconnect budget")
					}
					candidate, outbound := testCandidate(ip, time.Duration(len(created)+1)*10*time.Millisecond)
					created = append(created, outbound)
					return candidate, nil
				})
				mgr.reconcile(context.Background())
				mgr.reconcile(context.Background())
				first := mgr.active[slot{f.parent.Hash, warp.IPv4, 0}]
				second := mgr.active[slot{f.parent.Hash, warp.IPv4, 1}]
				if collision == "derived" {
					second.entry.SetEgressIP(first.entry.GetEgressIP())
				} else {
					second.entry.SetEgressIP(f.parent.GetEgressIP())
				}
				mgr.reconcile(context.Background())
				if created[1].closed.Load() != 1 || created[0].closed.Load() != 0 {
					t.Fatal("regular-probe collision did not close the duplicate while preserving the fastest original-address-safe route")
				}
				if reconnect == "unique" {
					if len(mgr.active) != 2 || len(created) != 3 || created[2].closed.Load() != 0 {
						t.Fatalf("unique reconnect: active=%d created=%d; want two exits and one immediate successful reconnect", len(mgr.active), len(created))
					}
					if got := mgr.active[slot{f.parent.Hash, warp.IPv4, 1}].entry.GetEgressIP(); got != netip.MustParseAddr("198.51.100.3") {
						t.Fatalf("replacement IP = %s", got)
					}
				} else {
					if len(mgr.active) != 1 || len(created) != 5 {
						t.Fatalf("exhausted reconnect: active=%d created=%d; want fastest exit after three duplicate attempts", len(mgr.active), len(created))
					}
					for _, outbound := range created[2:] {
						if outbound.closed.Load() != 1 {
							t.Fatal("exhausted regular-probe reconnect leaked a duplicate candidate")
						}
					}
					mgr.reconcile(context.Background())
					if len(created) != 5 {
						t.Fatal("exhausted reconnect ignored cooldown and restarted an infinite retry loop")
					}
				}
			})
		}
	}
}

func TestManagerRetiresExitAfterParentLifecycleChanges(t *testing.T) {
	for _, action := range []string{"delete", "disable", "replace-outbound"} {
		t.Run(action, func(t *testing.T) {
			f := newPoolFixture(t)
			var created []*memoryOutbound
			mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
				candidate, outbound := testCandidate(fmt.Sprintf("198.51.100.%d", len(created)+1), 10*time.Millisecond)
				created = append(created, outbound)
				return candidate, nil
			})
			mgr.reconcile(context.Background())
			switch action {
			case "delete":
				f.pool.RemoveNodeFromSub(f.parent.Hash, f.sub.ID)
			case "disable":
				f.sub.SetEnabled(false)
			case "replace-outbound":
				var replacement adapter.Outbound = &memoryOutbound{name: "replacement-parent"}
				f.parent.Outbound.Store(&replacement)
			}
			mgr.reconcile(context.Background())
			if created[0].closed.Load() != 1 {
				t.Fatalf("old tunnel closed %d times", created[0].closed.Load())
			}
			want := 0
			if action == "replace-outbound" {
				want = 1
			}
			if len(mgr.active) != want {
				t.Fatalf("active after %s = %d want %d", action, len(mgr.active), want)
			}
		})
	}
}

func TestManagerDoesNotPublishForParentRecreatedDuringRegistration(t *testing.T) {
	f := newPoolFixture(t)
	candidate, outbound := testCandidate("198.51.100.1", time.Millisecond)
	mgr := f.manager(t, func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
		// The same subscription can remove and re-add identical raw options
		// while a registration is in flight; its hash alone is not ownership.
		f.pool.RemoveNodeFromSub(f.parent.Hash, f.sub.ID)
		f.pool.AddNodeFromSub(f.parent.Hash, f.parent.RawOptions, f.sub.ID)
		replacement, _ := f.pool.GetEntry(f.parent.Hash)
		var parentOB adapter.Outbound = &memoryOutbound{name: "recreated-parent"}
		replacement.Outbound.Store(&parentOB)
		f.pool.RecordResult(replacement.Hash, true)
		return candidate, nil
	})
	mgr.reconcile(context.Background())
	if len(mgr.active) != 0 || f.pool.Size() != 1 || outbound.closed.Load() != 1 {
		t.Fatal("candidate attached to a recreated parent or leaked its stale tunnel")
	}
}

func TestManagerStopCancelsInflightFactoryAndClosesCandidate(t *testing.T) {
	f := newPoolFixture(t)
	entered := make(chan struct{})
	candidate, outbound := testCandidate("198.51.100.1", time.Millisecond)
	mgr := f.manager(t, func(ctx context.Context, _ adapter.Outbound, _ warp.Family) (Candidate, error) {
		close(entered)
		<-ctx.Done()
		return candidate, ctx.Err()
	})
	mgr.Start()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("factory never started")
	}
	mgr.Stop()
	if len(mgr.active) != 0 || f.pool.Size() != 1 || outbound.closed.Load() != 1 {
		t.Fatal("stop published or leaked an in-flight candidate")
	}
}

func TestManagerConfigChangeCancelsInflightFactory(t *testing.T) {
	f := newPoolFixture(t)
	entered, finished := make(chan struct{}), make(chan struct{})
	candidate, outbound := testCandidate("198.51.100.1", time.Millisecond)
	mgr := f.manager(t, func(ctx context.Context, _ adapter.Outbound, _ warp.Family) (Candidate, error) {
		close(entered)
		<-ctx.Done()
		return candidate, ctx.Err()
	})
	go func() { mgr.reconcile(context.Background()); close(finished) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("factory never started")
	}
	f.configure(func(cfg *config.RuntimeConfig) { cfg.WarpEnabled = false })
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("config change did not cancel an in-flight factory call")
	}
	if len(mgr.active) != 0 || outbound.closed.Load() != 1 {
		t.Fatal("disabled feature published or leaked an in-flight candidate")
	}
}
