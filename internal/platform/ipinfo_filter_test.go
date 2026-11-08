package platform

import (
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/ipinfo"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
)

func intPtr(v int) *int { return &v }

func TestIPInfoFilter_InactiveMatchesEverything(t *testing.T) {
	var filter IPInfoFilter
	if filter.Active() {
		t.Fatal("zero filter must be inactive")
	}
	if !filter.Match(nil) {
		t.Fatal("inactive filter must match a node without a profile")
	}
	if !filter.Match(&ipinfo.Info{FraudScore: 99}) {
		t.Fatal("inactive filter must match any profile")
	}
}

func TestIPInfoFilter_ActiveRequiresProfile(t *testing.T) {
	filter := IPInfoFilter{MaxFraudScore: intPtr(50)}
	if !filter.Active() {
		t.Fatal("score bound must activate the filter")
	}
	if filter.Match(nil) {
		t.Fatal("active filter must reject a node without a profile")
	}
}

func TestIPInfoFilter_FraudScoreBounds(t *testing.T) {
	info := &ipinfo.Info{FraudScore: 40}

	if !(IPInfoFilter{MinFraudScore: intPtr(40), MaxFraudScore: intPtr(40)}).Match(info) {
		t.Fatal("inclusive bounds must match the boundary score")
	}
	if (IPInfoFilter{MinFraudScore: intPtr(41)}).Match(info) {
		t.Fatal("score below min must not match")
	}
	if (IPInfoFilter{MaxFraudScore: intPtr(39)}).Match(info) {
		t.Fatal("score above max must not match")
	}
}

func TestIPInfoFilter_IPType(t *testing.T) {
	residential := &ipinfo.Info{IsResidential: true}
	hosting := &ipinfo.Info{IsResidential: false}

	if !(IPInfoFilter{IPType: IPUsageResidential}).Match(residential) {
		t.Fatal("residential filter must match a residential IP")
	}
	if (IPInfoFilter{IPType: IPUsageResidential}).Match(hosting) {
		t.Fatal("residential filter must reject a hosting IP")
	}
	if !(IPInfoFilter{IPType: IPUsageHosting}).Match(hosting) {
		t.Fatal("hosting filter must match a hosting IP")
	}
	if (IPInfoFilter{IPType: IPUsageHosting}).Match(residential) {
		t.Fatal("hosting filter must reject a residential IP")
	}
	if (IPInfoFilter{IPType: IPUsageAny}).Active() {
		t.Fatal("ip_type=any must not activate the filter")
	}
}

func TestIPInfoFilter_Native(t *testing.T) {
	native := &ipinfo.Info{IsBroadcast: false}
	broadcast := &ipinfo.Info{IsBroadcast: true}

	if !(IPInfoFilter{Native: IPNativeNative}).Match(native) {
		t.Fatal("native filter must match a native IP")
	}
	if (IPInfoFilter{Native: IPNativeNative}).Match(broadcast) {
		t.Fatal("native filter must reject a broadcast IP")
	}
	if !(IPInfoFilter{Native: IPNativeBroadcast}).Match(broadcast) {
		t.Fatal("broadcast filter must match a broadcast IP")
	}
}

func TestIPInfoFilter_ASNIncludeExclude(t *testing.T) {
	info := &ipinfo.Info{ASN: 64500}

	if !(IPInfoFilter{ASNIncludes: []uint32{64500}}).Match(info) {
		t.Fatal("ASN include must match")
	}
	if (IPInfoFilter{ASNIncludes: []uint32{64501}}).Match(info) {
		t.Fatal("ASN include must reject a different ASN")
	}
	if !(IPInfoFilter{ASNExcludes: []uint32{64501}}).Match(info) {
		t.Fatal("ASN exclude must allow a different ASN")
	}
	if (IPInfoFilter{ASNExcludes: []uint32{64500}}).Match(info) {
		t.Fatal("ASN exclude must reject the excluded ASN")
	}
}

func TestNormalizeIPInfoFilter_DefaultsAndWildcards(t *testing.T) {
	normalized, err := NormalizeIPInfoFilter(model.PlatformIPInfoFilter{})
	if err != nil {
		t.Fatalf("NormalizeIPInfoFilter: %v", err)
	}
	if normalized.IPType != string(IPUsageAny) || normalized.Native != string(IPNativeAny) {
		t.Fatalf("empty filter should normalize to wildcards, got %+v", normalized)
	}
	if normalized.ASNs == nil || len(normalized.ASNs) != 0 {
		t.Fatalf("ASNs should normalize to an empty slice, got %#v", normalized.ASNs)
	}
	if (IPInfoFilter{IPType: IPUsage(normalized.IPType), Native: IPNative(normalized.Native)}).Active() {
		t.Fatal("wildcard-only filter must be inactive")
	}
}

func TestNormalizeIPInfoFilter_ScoreValidation(t *testing.T) {
	if _, err := NormalizeIPInfoFilter(model.PlatformIPInfoFilter{MinFraudScore: intPtr(101)}); err == nil {
		t.Fatal("expected out-of-range min score error")
	}
	if _, err := NormalizeIPInfoFilter(model.PlatformIPInfoFilter{MaxFraudScore: intPtr(-1)}); err == nil {
		t.Fatal("expected out-of-range max score error")
	}
	if _, err := NormalizeIPInfoFilter(model.PlatformIPInfoFilter{
		MinFraudScore: intPtr(60), MaxFraudScore: intPtr(40),
	}); err == nil {
		t.Fatal("expected min > max error")
	}
}

func TestNormalizeIPInfoFilter_ASNCanonicalization(t *testing.T) {
	normalized, err := NormalizeIPInfoFilter(model.PlatformIPInfoFilter{
		ASNs: []string{"AS64500", " as64501 ", "!AS64502", "64500"},
	})
	if err != nil {
		t.Fatalf("NormalizeIPInfoFilter: %v", err)
	}
	want := []string{"64500", "64501", "!64502"}
	if len(normalized.ASNs) != len(want) {
		t.Fatalf("ASNs = %v, want %v", normalized.ASNs, want)
	}
	for i := range want {
		if normalized.ASNs[i] != want[i] {
			t.Fatalf("ASNs = %v, want %v", normalized.ASNs, want)
		}
	}
}

func TestNormalizeIPInfoFilter_ASNConflict(t *testing.T) {
	_, err := NormalizeIPInfoFilter(model.PlatformIPInfoFilter{
		ASNs: []string{"64500", "!64500"},
	})
	if err == nil {
		t.Fatal("expected conflict error for the same ASN included and excluded")
	}
	if !strings.Contains(err.Error(), "both included and excluded") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeIPInfoFilter_InvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		filter model.PlatformIPInfoFilter
		hint   string
	}{
		{name: "ip_type", filter: model.PlatformIPInfoFilter{IPType: "datacenter"}, hint: "ip_type"},
		{name: "native", filter: model.PlatformIPInfoFilter{Native: "unknown"}, hint: "native"},
		{name: "asn text", filter: model.PlatformIPInfoFilter{ASNs: []string{"ASabc"}}, hint: "asns[0]"},
		{name: "asn zero", filter: model.PlatformIPInfoFilter{ASNs: []string{"0"}}, hint: "asns[0]"},
		{name: "asn empty", filter: model.PlatformIPInfoFilter{ASNs: []string{" "}}, hint: "asns[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeIPInfoFilter(tc.filter)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Fatalf("error %q should mention %q", err, tc.hint)
			}
		})
	}
}

func TestCompileIPInfoFilter_SplitsIncludesAndExcludes(t *testing.T) {
	filter, err := CompileIPInfoFilter(model.PlatformIPInfoFilter{
		MinFraudScore: intPtr(10),
		ASNs:          []string{"AS64500", "!64501"},
	})
	if err != nil {
		t.Fatalf("CompileIPInfoFilter: %v", err)
	}
	if len(filter.ASNIncludes) != 1 || filter.ASNIncludes[0] != 64500 {
		t.Fatalf("ASNIncludes = %v", filter.ASNIncludes)
	}
	if len(filter.ASNExcludes) != 1 || filter.ASNExcludes[0] != 64501 {
		t.Fatalf("ASNExcludes = %v", filter.ASNExcludes)
	}
	if filter.MinFraudScore == nil || *filter.MinFraudScore != 10 {
		t.Fatalf("MinFraudScore = %v", filter.MinFraudScore)
	}
}

func TestParseASNFilterEntry(t *testing.T) {
	asn, exclude, err := ParseASNFilterEntry("AS132203")
	if err != nil || exclude || asn != 132203 {
		t.Fatalf("got (%d, %v, %v), want (132203, false, nil)", asn, exclude, err)
	}
	asn, exclude, err = ParseASNFilterEntry("!132203")
	if err != nil || !exclude || asn != 132203 {
		t.Fatalf("got (%d, %v, %v), want (132203, true, nil)", asn, exclude, err)
	}
	if _, _, err := ParseASNFilterEntry("!"); err == nil {
		t.Fatal("expected error for a bare negation")
	}
}

func TestPlatform_EvaluateNode_IPInfoFilter(t *testing.T) {
	h := makeHash(`{"type":"ss"}`)

	entry := makeFullyRoutableEntry(h, "sub1")
	entry.SetEgressIPInfo(&ipinfo.Info{FraudScore: 80})

	p := NewPlatform("p1", "Test", nil, nil)
	p.IPInfoFilters = IPInfoFilter{MaxFraudScore: intPtr(50)}
	p.FullRebuild(func(fn func(node.Hash, *node.NodeEntry) bool) {
		fn(h, entry)
	}, alwaysLookup, usGeoLookup)

	if p.View().Size() != 0 {
		t.Fatal("node above the fraud score ceiling should not be routable")
	}

	p.IPInfoFilters = IPInfoFilter{MaxFraudScore: intPtr(90)}
	p.FullRebuild(func(fn func(node.Hash, *node.NodeEntry) bool) {
		fn(h, entry)
	}, alwaysLookup, usGeoLookup)

	if p.View().Size() != 1 {
		t.Fatal("node within the fraud score ceiling should be routable")
	}
}

func TestPlatform_EvaluateNode_IPInfoFilter_RequiresProfile(t *testing.T) {
	p := NewPlatform("p1", "Test", nil, nil)
	p.IPInfoFilters = IPInfoFilter{IPType: IPUsageResidential}
	h := makeHash(`{"type":"ss"}`)
	entry := makeFullyRoutableEntry(h, "sub1")

	p.FullRebuild(func(fn func(node.Hash, *node.NodeEntry) bool) {
		fn(h, entry)
	}, alwaysLookup, usGeoLookup)

	if p.View().Size() != 0 {
		t.Fatal("node without a profile should not match an active IP profile filter")
	}

	entry.SetEgressIPInfo(&ipinfo.Info{IsResidential: true})
	p.FullRebuild(func(fn func(node.Hash, *node.NodeEntry) bool) {
		fn(h, entry)
	}, alwaysLookup, usGeoLookup)

	if p.View().Size() != 1 {
		t.Fatal("residential node should match the residential filter")
	}
}

func TestBuildFromModel_IPInfoFilters(t *testing.T) {
	plat, err := BuildFromModel(model.Platform{
		ID:                     "plat-1",
		Name:                   "Platform-1",
		RegexFilters:           []string{},
		RegionFilters:          []string{},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
		IPInfoFilters: model.PlatformIPInfoFilter{
			MaxFraudScore: intPtr(30),
			IPType:        "residential",
			ASNs:          []string{"AS64500"},
		},
	})
	if err != nil {
		t.Fatalf("BuildFromModel: %v", err)
	}
	if !plat.IPInfoFilters.Active() {
		t.Fatal("expected compiled IP info filter to be active")
	}
	if plat.IPInfoFilters.MaxFraudScore == nil || *plat.IPInfoFilters.MaxFraudScore != 30 {
		t.Fatalf("MaxFraudScore = %v", plat.IPInfoFilters.MaxFraudScore)
	}
	if plat.IPInfoFilters.IPType != IPUsageResidential {
		t.Fatalf("IPType = %q", plat.IPInfoFilters.IPType)
	}
	if len(plat.IPInfoFilters.ASNIncludes) != 1 || plat.IPInfoFilters.ASNIncludes[0] != 64500 {
		t.Fatalf("ASNIncludes = %v", plat.IPInfoFilters.ASNIncludes)
	}
}

func TestBuildFromModel_InvalidIPInfoFilters(t *testing.T) {
	_, err := BuildFromModel(model.Platform{
		ID:                     "plat-1",
		Name:                   "Platform-1",
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
		IPInfoFilters:          model.PlatformIPInfoFilter{IPType: "bogus"},
	})
	if err == nil {
		t.Fatal("expected IP info filter decode error")
	}
	if !strings.Contains(err.Error(), "ipinfo_filters") {
		t.Fatalf("unexpected error: %v", err)
	}
}
