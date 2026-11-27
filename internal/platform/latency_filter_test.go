package platform

import (
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
)

func TestLatencyFilter_InactiveMatchesEverything(t *testing.T) {
	var filter LatencyFilter
	if filter.Active() {
		t.Fatal("zero filter must be inactive")
	}
	if !filter.Match(0, false) {
		t.Fatal("inactive filter must match missing latency")
	}
	if !filter.Match(9999, true) {
		t.Fatal("inactive filter must match any latency")
	}
}

func TestLatencyFilter_ActiveBounds(t *testing.T) {
	filter := LatencyFilter{MinMs: intPtr(50), MaxMs: intPtr(200)}
	if !filter.Active() {
		t.Fatal("expected filter to be active")
	}
	if filter.Match(0, false) {
		t.Fatal("active filter must reject missing latency")
	}
	if filter.Match(49.9, true) {
		t.Fatal("below min_ms must be rejected")
	}
	if !filter.Match(50, true) {
		t.Fatal("exact min_ms must match")
	}
	if !filter.Match(200, true) {
		t.Fatal("exact max_ms must match")
	}
	if filter.Match(200.1, true) {
		t.Fatal("above max_ms must be rejected")
	}
}

func TestNormalizeLatencyFilter_Validation(t *testing.T) {
	valid, err := NormalizeLatencyFilter(model.PlatformLatencyFilter{
		MinMs: intPtr(0),
		MaxMs: intPtr(350),
	})
	if err != nil {
		t.Fatalf("NormalizeLatencyFilter valid: %v", err)
	}
	if valid.MinMs == nil || *valid.MinMs != 0 || valid.MaxMs == nil || *valid.MaxMs != 350 {
		t.Fatalf("normalized = %+v", valid)
	}

	invalidCases := []model.PlatformLatencyFilter{
		{MinMs: intPtr(-1)},
		{MaxMs: intPtr(-5)},
		{MinMs: intPtr(300), MaxMs: intPtr(100)},
	}
	for _, tc := range invalidCases {
		if _, err := NormalizeLatencyFilter(tc); err == nil {
			t.Errorf("expected error for %+v", tc)
		}
	}
}

func TestPlatform_LatencyFiltersInEvaluateNode(t *testing.T) {
	p := NewPlatform("p-lat", "LowLatency", nil, nil)
	p.LatencyFilters = LatencyFilter{MinMs: intPtr(20), MaxMs: intPtr(150)}

	fastHash := makeHash(`{"type":"ss","n":"fast"}`)
	fastEntry := makeFullyRoutableEntry(fastHash, "sub1")
	fastEntry.LatencyTable.LoadEntry("gstatic.com", node.DomainLatencyStats{
		Ewma:        80 * time.Millisecond,
		LastUpdated: time.Now(),
	})

	slowHash := makeHash(`{"type":"ss","n":"slow"}`)
	slowEntry := makeFullyRoutableEntry(slowHash, "sub1")
	slowEntry.LatencyTable.LoadEntry("gstatic.com", node.DomainLatencyStats{
		Ewma:        300 * time.Millisecond,
		LastUpdated: time.Now(),
	})

	noAuthHash := makeHash(`{"type":"ss","n":"noauth"}`)
	noAuthEntry := makeFullyRoutableEntry(noAuthHash, "sub1")

	entries := map[node.Hash]*node.NodeEntry{
		fastHash:   fastEntry,
		slowHash:   slowEntry,
		noAuthHash: noAuthEntry,
	}

	lookup := func(entry *node.NodeEntry) (float64, bool) {
		return node.AverageEWMAForDomainsMs(entry, []string{"gstatic.com"})
	}

	p.FullRebuildWithOptions(func(fn func(node.Hash, *node.NodeEntry) bool) {
		for h, e := range entries {
			fn(h, e)
		}
	}, alwaysLookup, usGeoLookup, EvalOptions{LatencyLookup: lookup})

	if p.View().Size() != 1 || !p.View().Contains(fastHash) {
		t.Fatalf("expected only fast node, size=%d fast=%v slow=%v noAuth=%v",
			p.View().Size(), p.View().Contains(fastHash), p.View().Contains(slowHash), p.View().Contains(noAuthHash))
	}

	// Simulate slow node improving into range and NotifyDirtyWithOptions re-evaluating it.
	slowEntry.LatencyTable.LoadEntry("gstatic.com", node.DomainLatencyStats{
		Ewma:        100 * time.Millisecond,
		LastUpdated: time.Now(),
	})
	p.NotifyDirtyWithOptions(slowHash, func(h node.Hash) (*node.NodeEntry, bool) {
		e, ok := entries[h]
		return e, ok
	}, alwaysLookup, usGeoLookup, EvalOptions{LatencyLookup: lookup})

	if p.View().Size() != 2 || !p.View().Contains(slowHash) {
		t.Fatalf("expected improved node to be added, size=%d", p.View().Size())
	}
}

func TestBuildFromModel_LatencyFilters(t *testing.T) {
	plat, err := BuildFromModel(model.Platform{
		ID:                     "plat-1",
		Name:                   "Platform-1",
		LatencyFilters:         model.PlatformLatencyFilter{MinMs: intPtr(10), MaxMs: intPtr(250)},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
	})
	if err != nil {
		t.Fatalf("BuildFromModel: %v", err)
	}
	if !plat.LatencyFilters.Active() || *plat.LatencyFilters.MinMs != 10 || *plat.LatencyFilters.MaxMs != 250 {
		t.Fatalf("LatencyFilters = %+v", plat.LatencyFilters)
	}

	_, err = BuildFromModel(model.Platform{
		ID:                     "plat-1",
		Name:                   "Platform-1",
		LatencyFilters:         model.PlatformLatencyFilter{MinMs: intPtr(500), MaxMs: intPtr(100)},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
	})
	if err == nil || !strings.Contains(err.Error(), "latency_filters") {
		t.Fatalf("expected latency_filters error, got %v", err)
	}
}
