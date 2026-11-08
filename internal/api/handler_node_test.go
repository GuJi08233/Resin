package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/ipinfo"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/probe"
	"github.com/Resinat/Resin/internal/service"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func addNodeForNodeListTest(t *testing.T, cp *service.ControlPlaneService, sub *subscription.Subscription, raw string, egressIP string) {
	addNodeForNodeListTestWithTag(t, cp, sub, raw, egressIP, "tag")
}

func addNodeForNodeListTestWithTag(
	t *testing.T,
	cp *service.ControlPlaneService,
	sub *subscription.Subscription,
	raw string,
	egressIP string,
	tag string,
) {
	t.Helper()

	hash := node.HashFromRawOptions([]byte(raw))
	cp.Pool.AddNodeFromSub(hash, []byte(raw), sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{tag}})

	if egressIP == "" {
		return
	}
	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	entry.SetEgressIP(netip.MustParseAddr(egressIP))
}

func markNodeHealthyForNodeListTest(t *testing.T, cp *service.ControlPlaneService, raw string) {
	t.Helper()

	hash := node.HashFromRawOptions([]byte(raw))
	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)
	entry.CircuitOpenSince.Store(0)
}

func TestHandleListNodes_TagKeywordFiltersByNodeName(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(subA)

	addNodeForNodeListTestWithTag(t, cp, subA, `{"type":"ss","server":"1.1.1.1","port":443}`, "", "hongkong-fast-01")
	addNodeForNodeListTestWithTag(t, cp, subA, `{"type":"ss","server":"2.2.2.2","port":443}`, "", "japan-slow-01")

	rec := doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+subA.ID+"&tag_keyword=FAST",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes with tag_keyword status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["total"] != float64(1) {
		t.Fatalf("tag_keyword total: got %v, want 1", body["total"])
	}
}

func TestHandleListNodes_UniqueEgressIPsUsesFilteredResult(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	subB := subscription.NewSubscription("22222222-2222-2222-2222-222222222222", "sub-b", "https://example.com/b", true, false)
	cp.SubMgr.Register(subA)
	cp.SubMgr.Register(subB)

	const rawA1 = `{"type":"ss","server":"1.1.1.1","port":443}`
	const rawA2 = `{"type":"ss","server":"2.2.2.2","port":443}`
	const rawA3 = `{"type":"ss","server":"3.3.3.3","port":443}`
	const rawA4 = `{"type":"ss","server":"4.4.4.4","port":443}`
	const rawB1 = `{"type":"ss","server":"5.5.5.5","port":443}`

	addNodeForNodeListTest(t, cp, subA, rawA1, "203.0.113.10")
	addNodeForNodeListTest(t, cp, subA, rawA2, "203.0.113.10")
	addNodeForNodeListTest(t, cp, subA, rawA3, "203.0.113.11")
	addNodeForNodeListTest(t, cp, subA, rawA4, "")
	addNodeForNodeListTest(t, cp, subB, rawB1, "203.0.113.99")

	// Healthy condition: has outbound + not circuit-open.
	markNodeHealthyForNodeListTest(t, cp, rawA1)
	markNodeHealthyForNodeListTest(t, cp, rawA2)

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?subscription_id="+subA.ID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["total"] != float64(4) {
		t.Fatalf("total: got %v, want 4", body["total"])
	}
	if body["unique_egress_ips"] != float64(2) {
		t.Fatalf("unique_egress_ips: got %v, want 2", body["unique_egress_ips"])
	}
	if body["unique_healthy_egress_ips"] != float64(1) {
		t.Fatalf("unique_healthy_egress_ips: got %v, want 1", body["unique_healthy_egress_ips"])
	}

	rec = doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+subA.ID+"&limit=1",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes paged status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["total"] != float64(4) {
		t.Fatalf("paged total: got %v, want 4", body["total"])
	}
	if body["unique_egress_ips"] != float64(2) {
		t.Fatalf("paged unique_egress_ips: got %v, want 2", body["unique_egress_ips"])
	}
	if body["unique_healthy_egress_ips"] != float64(1) {
		t.Fatalf("paged unique_healthy_egress_ips: got %v, want 1", body["unique_healthy_egress_ips"])
	}

	rec = doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+subA.ID+"&egress_ip=203.0.113.10",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes with egress filter status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["total"] != float64(2) {
		t.Fatalf("filtered total: got %v, want 2", body["total"])
	}
	if body["unique_egress_ips"] != float64(1) {
		t.Fatalf("filtered unique_egress_ips: got %v, want 1", body["unique_egress_ips"])
	}
	if body["unique_healthy_egress_ips"] != float64(1) {
		t.Fatalf("filtered unique_healthy_egress_ips: got %v, want 1", body["unique_healthy_egress_ips"])
	}
}

func TestHandleListNodes_IncludesReferenceLatencyMs(t *testing.T) {
	srv, cp, runtimeCfg := newControlPlaneTestServer(t)

	cfg := config.NewDefaultRuntimeConfig()
	cfg.LatencyAuthorities = []string{"cloudflare.com", "github.com", "google.com"}
	runtimeCfg.Store(cfg)

	subA := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(subA)

	raw := `{"type":"ss","server":"1.1.1.1","port":443}`
	hash := node.HashFromRawOptions([]byte(raw))
	addNodeForNodeListTest(t, cp, subA, raw, "203.0.113.10")

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	entry.LatencyTable.LoadEntry("cloudflare.com", node.DomainLatencyStats{
		Ewma:        40 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	entry.LatencyTable.LoadEntry("github.com", node.DomainLatencyStats{
		Ewma:        80 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	entry.LatencyTable.LoadEntry("example.com", node.DomainLatencyStats{
		Ewma:        10 * time.Millisecond,
		LastUpdated: time.Now(),
	})

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?subscription_id="+subA.ID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := decodeJSONMap(t, rec)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items mismatch: got %T len=%d", body["items"], len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item type: got %T", items[0])
	}
	if item["reference_latency_ms"] != float64(60) {
		t.Fatalf("reference_latency_ms: got %v, want 60", item["reference_latency_ms"])
	}
}

func TestHandleProbeEgress_ReturnsRegion(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	raw := []byte(`{"type":"ss","server":"1.1.1.1","port":443}`)
	hash := node.HashFromRawOptions(raw)
	cp.Pool.AddNodeFromSub(hash, raw, sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)

	cp.ProbeMgr = probe.NewProbeManager(probe.ProbeConfig{
		Pool: cp.Pool,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			return []byte("ip=203.0.113.88\nloc=JP"), 25 * time.Millisecond, nil
		},
	})

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/"+hash.Hex()+"/actions/probe-egress", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe-egress status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["egress_ip"] != "203.0.113.88" {
		t.Fatalf("egress_ip: got %v, want %q", body["egress_ip"], "203.0.113.88")
	}
	if body["region"] != "jp" {
		t.Fatalf("region: got %v, want %q", body["region"], "jp")
	}
}

func TestHandleListNodes_EnabledFilter(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	subEnabled := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-enabled", "https://example.com/a", true, false)
	subDisabled := subscription.NewSubscription("22222222-2222-2222-2222-222222222222", "sub-disabled", "https://example.com/b", false, false)
	cp.SubMgr.Register(subEnabled)
	cp.SubMgr.Register(subDisabled)

	addNodeForNodeListTestWithTag(t, cp, subEnabled, `{"type":"ss","server":"1.1.1.1","port":443}`, "", "enabled-tag")
	addNodeForNodeListTestWithTag(t, cp, subDisabled, `{"type":"ss","server":"2.2.2.2","port":443}`, "", "disabled-tag")

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?enabled=true", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled=true status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["total"] != float64(1) {
		t.Fatalf("enabled=true total: got %v, want 1", body["total"])
	}

	rec = doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?enabled=false", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled=false status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	if body["total"] != float64(1) {
		t.Fatalf("enabled=false total: got %v, want 1", body["total"])
	}
}

func TestHandleProbeIPInfo_ReturnsProfile(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	raw := []byte(`{"type":"ss","server":"1.1.1.1","port":443}`)
	hash := node.HashFromRawOptions(raw)
	cp.Pool.AddNodeFromSub(hash, raw, sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	ob := testutil.NewNoopOutbound()
	entry.Outbound.Store(&ob)

	cp.ProbeMgr = probe.NewProbeManager(probe.ProbeConfig{
		Pool: cp.Pool,
		Fetcher: func(_ node.Hash, _ string) ([]byte, time.Duration, error) {
			return []byte(`{"ip":"203.0.113.88","asn":64500,"asOrganization":"Example ISP",` +
				`"countryCode":"JP","fraudScore":56,"isResidential":false,"isBroadcast":true}`), 0, nil
		},
	})

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/nodes/"+hash.Hex()+"/actions/probe-ipinfo", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe-ipinfo status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	if body["ip"] != "203.0.113.88" {
		t.Fatalf("ip: got %v", body["ip"])
	}
	if body["asn"] != float64(64500) {
		t.Fatalf("asn: got %v", body["asn"])
	}
	if body["fraud_score"] != float64(56) {
		t.Fatalf("fraud_score: got %v", body["fraud_score"])
	}
	if body["risk_level"] != "medium_risk" {
		t.Fatalf("risk_level: got %v", body["risk_level"])
	}
	if body["is_broadcast"] != true {
		t.Fatalf("is_broadcast: got %v", body["is_broadcast"])
	}
}

func TestListNodes_IncludesIPInfo(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	raw := []byte(`{"type":"ss","server":"1.1.1.1","port":443}`)
	hash := node.HashFromRawOptions(raw)
	cp.Pool.AddNodeFromSub(hash, raw, sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	entry.SetEgressIPInfo(&ipinfo.Info{
		IP:         "203.0.113.88",
		ASN:        64500,
		FraudScore: 10,
		RiskLevel:  ipinfo.RiskLevelVeryClean,
	})
	entry.LastIPInfoProbeAttempt.Store(time.Now().UnixNano())

	rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?subscription_id="+sub.ID, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list nodes status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items mismatch: got %T len=%d", body["items"], len(items))
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item type: got %T", items[0])
	}
	info, ok := item["ipinfo"].(map[string]any)
	if !ok {
		t.Fatalf("ipinfo missing: got %T", item["ipinfo"])
	}
	if info["ip"] != "203.0.113.88" || info["risk_level"] != "very_clean" {
		t.Fatalf("ipinfo: got %+v", info)
	}
	if item["last_ipinfo_probe_attempt"] == nil || item["last_ipinfo_probe_attempt"] == "" {
		t.Fatalf("last_ipinfo_probe_attempt missing: %v", item["last_ipinfo_probe_attempt"])
	}
}

func addNodeWithIPInfoForTest(
	t *testing.T,
	cp *service.ControlPlaneService,
	sub *subscription.Subscription,
	raw string,
	info *ipinfo.Info,
) node.Hash {
	t.Helper()

	hash := node.HashFromRawOptions([]byte(raw))
	cp.Pool.AddNodeFromSub(hash, []byte(raw), sub.ID)
	sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})

	entry, ok := cp.Pool.GetEntry(hash)
	if !ok {
		t.Fatalf("node %s missing after add", hash.Hex())
	}
	entry.SetEgressIPInfo(info)
	return hash
}

func TestHandleListNodes_IPInfoFilters(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	addNodeWithIPInfoForTest(t, cp, sub, `{"type":"ss","server":"1.1.1.1","port":443}`, &ipinfo.Info{
		IP: "203.0.113.1", ASN: 64500, FraudScore: 5, IsResidential: true,
	})
	addNodeWithIPInfoForTest(t, cp, sub, `{"type":"ss","server":"2.2.2.2","port":443}`, &ipinfo.Info{
		IP: "203.0.113.2", ASN: 64501, FraudScore: 45, IsBroadcast: true,
	})
	addNodeWithIPInfoForTest(t, cp, sub, `{"type":"ss","server":"3.3.3.3","port":443}`, &ipinfo.Info{
		IP: "203.0.113.3", ASN: 64502, FraudScore: 95,
	})
	addNodeForNodeListTest(t, cp, sub, `{"type":"ss","server":"4.4.4.4","port":443}`, "")

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{name: "max score", query: "&max_fraud_score=50", want: 2},
		{name: "min score", query: "&min_fraud_score=45", want: 2},
		{name: "score range", query: "&min_fraud_score=10&max_fraud_score=50", want: 1},
		{name: "residential", query: "&ip_type=residential", want: 1},
		{name: "hosting", query: "&ip_type=hosting", want: 2},
		{name: "broadcast", query: "&native=broadcast", want: 1},
		{name: "native", query: "&native=native", want: 2},
		{name: "asn include", query: "&asn=AS64501", want: 1},
		{name: "asn repeated include", query: "&asn=64500&asn=64502", want: 2},
		{name: "asn comma include", query: "&asn=64500,64502", want: 2},
		{name: "asn exclude", query: "&asn=!64500", want: 2},
		{name: "has ipinfo", query: "&has_ipinfo=true", want: 3},
		{name: "missing ipinfo", query: "&has_ipinfo=false", want: 1},
		{name: "active filter excludes unknown profile", query: "&max_fraud_score=100", want: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSONRequest(
				t,
				srv,
				http.MethodGet,
				"/api/v1/nodes?subscription_id="+sub.ID+tc.query,
				nil,
				true,
			)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			body := decodeJSONMap(t, rec)
			if body["total"] != float64(tc.want) {
				t.Fatalf("total: got %v, want %d", body["total"], tc.want)
			}
		})
	}
}

func TestHandleListNodes_IPInfoFilterValidation(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	invalidQueries := []string{
		"max_fraud_score=101",
		"max_fraud_score=-1",
		"max_fraud_score=abc",
		"min_fraud_score=60&max_fraud_score=40",
		"ip_type=datacenter",
		"native=unknown",
		"asn=ASabc",
		"asn=0",
		"has_ipinfo=maybe",
		"sort_by=fraud_score_nope",
	}
	for _, query := range invalidQueries {
		t.Run(query, func(t *testing.T) {
			rec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/nodes?"+query, nil, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			assertErrorCode(t, rec, "INVALID_ARGUMENT")
		})
	}
}

func TestHandleListNodes_SortByFraudScore(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	sub := subscription.NewSubscription("11111111-1111-1111-1111-111111111111", "sub-a", "https://example.com/a", true, false)
	cp.SubMgr.Register(sub)

	addNodeWithIPInfoForTest(t, cp, sub, `{"type":"ss","server":"1.1.1.1","port":443}`, &ipinfo.Info{IP: "1.1.1.1", FraudScore: 70})
	addNodeWithIPInfoForTest(t, cp, sub, `{"type":"ss","server":"2.2.2.2","port":443}`, &ipinfo.Info{IP: "2.2.2.2", FraudScore: 10})
	addNodeWithIPInfoForTest(t, cp, sub, `{"type":"ss","server":"3.3.3.3","port":443}`, &ipinfo.Info{IP: "3.3.3.3", FraudScore: 40})
	addNodeForNodeListTest(t, cp, sub, `{"type":"ss","server":"4.4.4.4","port":443}`, "")

	rec := doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+sub.ID+"&sort_by=fraud_score&sort_order=asc",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := decodeJSONMap(t, rec)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 4 {
		t.Fatalf("items: got %T len=%d", body["items"], len(items))
	}
	scores := make([]float64, 0, len(items))
	for _, item := range items {
		nodeItem := item.(map[string]any)
		info, _ := nodeItem["ipinfo"].(map[string]any)
		if info == nil {
			scores = append(scores, -1)
			continue
		}
		scores = append(scores, info["fraud_score"].(float64))
	}
	want := []float64{10, 40, 70, -1}
	for i := range want {
		if scores[i] != want[i] {
			t.Fatalf("asc scores = %v, want %v", scores, want)
		}
	}

	rec = doJSONRequest(
		t,
		srv,
		http.MethodGet,
		"/api/v1/nodes?subscription_id="+sub.ID+"&sort_by=fraud_score&sort_order=desc",
		nil,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("desc status: got %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = decodeJSONMap(t, rec)
	items = body["items"].([]any)
	for i, item := range items {
		nodeItem := item.(map[string]any)
		info, _ := nodeItem["ipinfo"].(map[string]any)
		if info == nil {
			if i != len(items)-1 {
				t.Fatalf("node without profile should sort last, got index %d", i)
			}
			continue
		}
	}
}
