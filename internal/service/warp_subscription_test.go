package service

import (
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/topology"
)

func newWarpSubscriptionHarness(t *testing.T) patchHarness {
	t.Helper()
	h := newPatchHarness(t)
	h.cp.SubMgr = topology.NewSubscriptionManager()
	h.cp.Pool = topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup: h.cp.SubMgr.Lookup, MaxConsecutiveFailures: func() int { return 3 },
	})
	return h
}

func TestWarpSubscriptionAlwaysListedAndRuntimeOnly(t *testing.T) {
	h := newWarpSubscriptionHarness(t)
	items, err := h.cp.ListSubscriptions(nil)
	if err != nil || len(items) != 1 {
		t.Fatalf("managed subscription missing: %+v, %v", items, err)
	}
	got := items[0]
	if got.ID != subscription.WarpSubscriptionID || got.Name != "WARP" || got.SourceType != "warp" ||
		!got.Managed || got.MaxNodes != 300 || got.Enabled || got.NodeCount != 0 {
		t.Fatalf("incorrect managed subscription defaults: %+v", got)
	}
	if sub := h.cp.SubMgr.Lookup(got.ID); sub == nil || sub.SourceType() != subscription.SourceTypeWarp {
		t.Fatal("WARP subscription is not available to topology lookup")
	}
	for _, enabled := range []bool{false, true} {
		filtered, err := h.cp.ListSubscriptions(&enabled)
		if err != nil || (len(filtered) == 1) != !enabled {
			t.Fatalf("enabled=%v filtering: %+v, %v", enabled, filtered, err)
		}
	}
	persisted, err := h.engine.ListSubscriptions()
	if err != nil || len(persisted) != 0 {
		t.Fatalf("runtime WARP subscription must not enter fetched subscriptions: %+v, %v", persisted, err)
	}
}

func TestWarpSubscriptionLimitAndEnableSettingsPersist(t *testing.T) {
	h := newWarpSubscriptionHarness(t)
	for index, patch := range []string{
		`{"enabled":true,"max_nodes":500}`,
		`{"max_nodes":120}`,
		`{"enabled":false}`,
	} {
		got, err := h.cp.UpdateSubscription(subscription.WarpSubscriptionID, []byte(patch))
		if err != nil {
			t.Fatal(err)
		}
		limit := 120
		if index == 0 {
			limit = 500
		}
		if got.Enabled != (index != 2) || got.MaxNodes != limit || !got.Managed {
			t.Fatalf("patch %s: %+v", patch, got)
		}
		cfg, version, err := h.engine.GetSystemConfig()
		if err != nil || cfg.WarpEnabled != got.Enabled || cfg.WarpMaxNodes != got.MaxNodes || version != index+1 {
			t.Fatalf("settings did not persist: %+v, version=%d, err=%v", cfg, version, err)
		}
		if h.cp.SubMgr.Lookup(subscription.WarpSubscriptionID).Enabled() != got.Enabled {
			t.Fatal("runtime subscription enable state was not synchronized")
		}
	}
	persisted, _, err := h.engine.GetSystemConfig()
	if err != nil {
		t.Fatal(err)
	}
	restartedCfg := &atomic.Pointer[config.RuntimeConfig]{}
	restartedCfg.Store(persisted)
	restarted := &ControlPlaneService{RuntimeCfg: restartedCfg, SubMgr: topology.NewSubscriptionManager()}
	got, err := restarted.GetSubscription(subscription.WarpSubscriptionID)
	if err != nil || got.Enabled || got.MaxNodes != 120 {
		t.Fatalf("restart lost settings: %+v, %v", got, err)
	}
}

func TestWarpSubscriptionRejectsInvalidChangesWithoutPersistence(t *testing.T) {
	h := newWarpSubscriptionHarness(t)
	original := cloneRuntimeConfig(h.runtimeCfg.Load())
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"max_nodes":null}`, `{"max_nodes":0}`, `{"max_nodes":1025}`,
		`{"max_nodes":1.5}`, `{"max_nodes":"500"}`, `{"enabled":null}`, `{"enabled":"true"}`,
		`{"url":"https://example.com"}`, `{"content":"proxy"}`, `{"name":"renamed"}`,
		`{"source_type":"local"}`, `{"ephemeral":true}`, `{"update_interval":"30s"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := h.cp.UpdateSubscription(subscription.WarpSubscriptionID, []byte(body)); err == nil {
				t.Fatal("invalid settings were accepted")
			}
			if !reflect.DeepEqual(original, h.runtimeCfg.Load()) {
				t.Fatal("invalid settings changed runtime config")
			}
			cfg, version, err := h.engine.GetSystemConfig()
			if err != nil || cfg != nil || version != 0 {
				t.Fatalf("invalid settings persisted: %+v %d %v", cfg, version, err)
			}
		})
	}
}

func TestWarpSubscriptionProtectedFromFetchedSubscriptionActions(t *testing.T) {
	h := newWarpSubscriptionHarness(t)
	got, err := h.cp.GetSubscription(subscription.WarpSubscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cp.DeleteSubscription(got.ID); err == nil {
		t.Fatal("managed subscription deletion was accepted")
	}
	if err := h.cp.RefreshSubscription(got.ID); err == nil {
		t.Fatal("managed subscription refresh was accepted")
	}
	if _, err := h.cp.CleanupSubscriptionCircuitOpenNodes(got.ID); err == nil {
		t.Fatal("managed subscription fetched-node cleanup was accepted")
	}
	if h.cp.SubMgr.Lookup(got.ID) == nil {
		t.Fatal("a rejected action removed the managed subscription")
	}
	name, content := "WARP", "127.0.0.1:1080"
	source := subscription.SourceTypeLocal
	if _, err := h.cp.CreateSubscription(CreateSubscriptionRequest{Name: &name, SourceType: &source, Content: &content}); err == nil {
		t.Fatal("regular subscription was allowed to shadow the WARP name")
	}
	ordinary := subscription.NewSubscription("ordinary", "Original", "https://example.com", true, false)
	h.cp.SubMgr.Register(ordinary)
	if _, err := h.cp.UpdateSubscription(ordinary.ID, []byte(`{"max_nodes":300}`)); err == nil {
		t.Fatal("regular subscription accepted a WARP-only limit")
	}
}

func TestWarpSubscriptionLegacyConfigUpdatesPreserveNodeView(t *testing.T) {
	h := newWarpSubscriptionHarness(t)
	if _, err := h.cp.GetSubscription(subscription.WarpSubscriptionID); err != nil {
		t.Fatal(err)
	}
	sub := h.cp.SubMgr.Lookup(subscription.WarpSubscriptionID)
	hash := node.HashFromRawOptions([]byte(`{"runtime":"warp"}`))
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"exit"}})
	if _, err := h.cp.PatchRuntimeConfig([]byte(`{"warp_enabled":true,"warp_max_nodes":600}`)); err != nil {
		t.Fatal(err)
	}
	got, err := h.cp.GetSubscription(subscription.WarpSubscriptionID)
	if err != nil || !got.Enabled || got.MaxNodes != 600 {
		t.Fatalf("legacy settings not reflected: %+v, %v", got, err)
	}
	if sub != h.cp.SubMgr.Lookup(subscription.WarpSubscriptionID) {
		t.Fatal("updating settings replaced the subscription node view")
	}
	if _, ok := sub.ManagedNodes().LoadNode(hash); !ok {
		t.Fatal("updating settings discarded managed nodes")
	}
	data, _ := json.Marshal(got)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	if decoded["managed"] != true || decoded["max_nodes"] != float64(600) {
		t.Fatalf("managed metadata missing from JSON: %s", data)
	}
}
