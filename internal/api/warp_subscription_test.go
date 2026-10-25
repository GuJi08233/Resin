package api

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestWarpSubscriptionHTTPSettingsAndProtectedActions(t *testing.T) {
	srv, cp, cfg := newControlPlaneTestServer(t)
	path := "/api/v1/subscriptions/" + subscription.WarpSubscriptionID
	rec := doJSONRequest(t, srv, http.MethodGet, path, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get managed subscription: %d %s", rec.Code, rec.Body)
	}
	body := decodeJSONMap(t, rec)
	if body["source_type"] != "warp" || body["managed"] != true || body["max_nodes"] != float64(300) || body["enabled"] != false {
		t.Fatalf("incorrect default managed subscription: %+v", body)
	}
	rec = doJSONRequest(t, srv, http.MethodPatch, path, map[string]any{"enabled": true, "max_nodes": 500}, true)
	if rec.Code != http.StatusOK || cfg.Load().WarpMaxNodes != 500 || !cp.SubMgr.Lookup(subscription.WarpSubscriptionID).Enabled() {
		t.Fatalf("managed subscription settings did not apply: %d %s", rec.Code, rec.Body)
	}
	for _, operation := range []struct{ method, suffix string }{
		{http.MethodDelete, ""},
		{http.MethodPost, "/actions/refresh"},
		{http.MethodPost, "/actions/cleanup-circuit-open-nodes"},
	} {
		rec = doJSONRequest(t, srv, operation.method, path+operation.suffix, nil, true)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "valid UUID") {
			t.Fatalf("managed operation must reach its explicit service protection: %d %s", rec.Code, rec.Body)
		}
	}
	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions/not-an-id", nil, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ordinary malformed subscription ID was accepted: %d", rec.Code)
	}
}

func TestWarpSubscriptionHTTPNodeFilterAndPlatformIsolation(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)
	rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/subscriptions/system-warp", map[string]any{"enabled": true}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable WARP subscription: %d %s", rec.Code, rec.Body)
	}
	parentSub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "Source", "", true, false)
	cp.SubMgr.Register(parentSub)
	raw := []byte(`{"type":"socks","server":"parent.example","server_port":1080}`)
	parentHash := node.HashFromRawOptions(raw)
	parentSub.ManagedNodes().StoreNode(parentHash, subscription.ManagedNode{Tags: []string{"original"}})
	cp.Pool.AddNodeFromSub(parentHash, raw, parentSub.ID)
	parent, _ := cp.Pool.GetEntry(parentHash)
	via := testutil.NewNoopOutbound()
	parent.Outbound.Store(&via)
	parent.CircuitOpenSince.Store(0)
	parent.SetEgressIP(netip.MustParseAddr("198.51.100.1"))
	parent.LatencyTable.Update("cloudflare.com", time.Millisecond, time.Minute)
	defer parent.LatencyTable.Close()
	childRaw := []byte(`{"type":"resin-warp","family":"ipv4","generation":1}`)
	child := node.NewNodeEntry(node.HashFromRawOptions(childRaw), childRaw, time.Now(), 8)
	defer child.LatencyTable.Close()
	child.ParentHash, child.WarpFamily = &parentHash, "ipv4"
	exit := testutil.NewNoopOutbound()
	child.Outbound.Store(&exit)
	child.SetEgressIP(netip.MustParseAddr("198.51.100.2"))
	child.LatencyTable.Update("cloudflare.com", time.Millisecond, time.Minute)
	if !cp.Pool.AddDerivedNode(child, parent) {
		t.Fatal("could not publish managed WARP exit")
	}
	for _, filter := range []struct{ id, want string }{
		{subscription.WarpSubscriptionID, child.Hash.Hex()},
		{parentSub.ID, parentHash.Hex()},
	} {
		rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?subscription_id="+filter.id, nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("filter %s: %d %s", filter.id, rec.Code, rec.Body)
		}
		body := decodeJSONMap(t, rec)
		items, ok := body["items"].([]any)
		if !ok || len(items) != 1 || items[0].(map[string]any)["node_hash"] != filter.want {
			t.Fatalf("subscription filter mixed original and WARP nodes: %+v", body)
		}
	}
	for _, allowWarp := range []bool{false, true} {
		filters := []string{}
		if !allowWarp {
			filters = []string{"!^WARP/"}
		}
		name := "original-only"
		if allowWarp {
			name = "including-warp"
		}
		rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/platforms", map[string]any{"name": name, "regex_filters": filters}, true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create platform: %d %s", rec.Code, rec.Body)
		}
		id := decodeJSONMap(t, rec)["id"].(string)
		plat, _ := cp.Pool.GetPlatform(id)
		if !plat.View().Contains(parentHash) || plat.View().Contains(child.Hash) != allowWarp {
			t.Fatalf("platform %s did not isolate WARP while preserving original", name)
		}
	}
}
