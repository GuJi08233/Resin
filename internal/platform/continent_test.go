package platform

import (
	"slices"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
)

func TestContinentForCountry(t *testing.T) {
	cases := []struct {
		country string
		want    string
	}{
		{"hk", ContinentAsia},
		{"JP", ContinentAsia},
		{"sg", ContinentAsia},
		{"de", ContinentEurope},
		{"gb", ContinentEurope},
		{"uk", ContinentEurope},
		{"ru", ContinentEurope},
		{"us", ContinentNorthAmerica},
		{"ca", ContinentNorthAmerica},
		{"br", ContinentSouthAmerica},
		{"au", ContinentOceania},
		{"nz", ContinentOceania},
		{"za", ContinentAfrica},
		{"eg", ContinentAfrica},
		{"aq", ContinentAntarctica},
		{"", ""},
		{"zz", ""},
	}
	for _, tc := range cases {
		if got := ContinentForCountry(tc.country); got != tc.want {
			t.Errorf("ContinentForCountry(%q) = %q, want %q", tc.country, got, tc.want)
		}
	}
}

func TestNormalizeContinentFilters(t *testing.T) {
	got, err := NormalizeContinentFilters([]string{" EU ", "as", "!AF", "eu", "!af"})
	if err != nil {
		t.Fatalf("NormalizeContinentFilters: %v", err)
	}
	want := []string{"eu", "as", "!af"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for _, invalid := range [][]string{
		{""},
		{"!"},
		{"asia"},
		{"us"},
		{"!zz"},
	} {
		if _, err := NormalizeContinentFilters(invalid); err == nil {
			t.Errorf("expected error for invalid continent filters %v", invalid)
		}
	}
}

func TestMatchContinentFilter(t *testing.T) {
	if !MatchContinentFilter("", nil) {
		t.Fatal("empty filter should match unknown country")
	}
	if MatchContinentFilter("", []string{"!eu"}) {
		t.Fatal("unknown country must not match active continent filter")
	}
	if !MatchContinentFilter("de", []string{"eu", "as"}) {
		t.Fatal("de should match eu")
	}
	if MatchContinentFilter("us", []string{"eu", "as"}) {
		t.Fatal("us should not match eu/as")
	}
	if MatchContinentFilter("de", []string{"!eu"}) {
		t.Fatal("de should be excluded by !eu")
	}
	if !MatchContinentFilter("jp", []string{"!eu"}) {
		t.Fatal("jp should match !eu")
	}
	if MatchContinentFilter("de", []string{"eu", "!eu"}) {
		t.Fatal("exclude should override include")
	}
}

func TestPlatform_ContinentFiltersInEvaluateNode(t *testing.T) {
	p := NewPlatform("p-cont", "EuropeExceptRU", nil, []string{"!ru"})
	p.ContinentFilters = []string{ContinentEurope}

	deHash := makeHash(`{"type":"ss","n":"de"}`)
	deEntry := makeFullyRoutableEntry(deHash, "sub1")
	deEntry.SetEgressRegion("de")

	ruHash := makeHash(`{"type":"ss","n":"ru"}`)
	ruEntry := makeFullyRoutableEntry(ruHash, "sub1")
	ruEntry.SetEgressRegion("ru")

	jpHash := makeHash(`{"type":"ss","n":"jp"}`)
	jpEntry := makeFullyRoutableEntry(jpHash, "sub1")
	jpEntry.SetEgressRegion("jp")

	unknownHash := makeHash(`{"type":"ss","n":"unknown"}`)
	unknownEntry := makeFullyRoutableEntry(unknownHash, "sub1")

	entries := map[node.Hash]*node.NodeEntry{
		deHash:      deEntry,
		ruHash:      ruEntry,
		jpHash:      jpEntry,
		unknownHash: unknownEntry,
	}
	p.FullRebuild(func(fn func(node.Hash, *node.NodeEntry) bool) {
		for h, e := range entries {
			fn(h, e)
		}
	}, alwaysLookup, nil)

	if p.View().Size() != 1 || !p.View().Contains(deHash) {
		t.Fatalf("expected only de node to match, view size=%d containsDE=%v", p.View().Size(), p.View().Contains(deHash))
	}
}

func TestBuildFromModel_ContinentFilters(t *testing.T) {
	plat, err := BuildFromModel(model.Platform{
		ID:                     "plat-1",
		Name:                   "Platform-1",
		ContinentFilters:       []string{"EU", "!AF"},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
	})
	if err != nil {
		t.Fatalf("BuildFromModel: %v", err)
	}
	want := []string{"eu", "!af"}
	if !slices.Equal(plat.ContinentFilters, want) {
		t.Fatalf("ContinentFilters = %v, want %v", plat.ContinentFilters, want)
	}

	_, err = BuildFromModel(model.Platform{
		ID:                     "plat-1",
		Name:                   "Platform-1",
		ContinentFilters:       []string{"bogus"},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
	})
	if err == nil || !strings.Contains(err.Error(), "continent_filters") {
		t.Fatalf("expected continent_filters error, got %v", err)
	}
}
