package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
)

func newStateTestEngine(t *testing.T) *StateEngine {
	t.Helper()
	dir := t.TempDir()
	engine, closer, err := PersistenceBootstrap(
		filepath.Join(dir, "state"),
		filepath.Join(dir, "cache"),
	)
	if err != nil {
		t.Fatalf("PersistenceBootstrap: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return engine
}

func TestPlatformStatusCodeRulesPersistence(t *testing.T) {
	engine := newStateTestEngine(t)
	now := time.Now().UnixNano()
	row := model.Platform{
		ID:                     "plat-rules",
		Name:                   "rules-platform",
		StickyTTLNs:            int64(time.Hour),
		RegexFilters:           []string{},
		RegionFilters:          []string{},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
		StatusCodeRules: []model.PlatformStatusCodeRule{
			{CodeFrom: 403, CodeTo: 403, Action: "BLACKLIST"},
			{CodeFrom: 429, CodeTo: 429, Action: "COOLDOWN", CooldownNs: int64(5 * time.Minute)},
		},
		UpdatedAtNs: now,
	}
	if err := engine.UpsertPlatform(row); err != nil {
		t.Fatalf("UpsertPlatform: %v", err)
	}

	stored, err := engine.GetPlatform(row.ID)
	if err != nil {
		t.Fatalf("GetPlatform: %v", err)
	}
	if len(stored.StatusCodeRules) != 2 {
		t.Fatalf("stored rules = %+v", stored.StatusCodeRules)
	}
	if stored.StatusCodeRules[0].Action != "BLACKLIST" || stored.StatusCodeRules[1].CooldownNs != int64(5*time.Minute) {
		t.Fatalf("unexpected stored rules: %+v", stored.StatusCodeRules)
	}

	all, err := engine.ListPlatforms()
	if err != nil {
		t.Fatalf("ListPlatforms: %v", err)
	}
	if len(all) != 1 || len(all[0].StatusCodeRules) != 2 {
		t.Fatalf("listed platforms = %+v", all)
	}
}

func TestPlatformEgressExclusionCRUD(t *testing.T) {
	engine := newStateTestEngine(t)
	now := time.Now().UnixNano()

	cooldown := model.PlatformEgressExclusion{
		PlatformID:  "plat-1",
		EgressIP:    "203.0.113.60",
		Action:      "COOLDOWN",
		StatusCode:  429,
		NodeHash:    "node-a",
		CreatedAtNs: now,
		ExpiresAtNs: now - int64(time.Minute), // already expired
	}
	blacklist := model.PlatformEgressExclusion{
		PlatformID:  "plat-1",
		EgressIP:    "203.0.113.61",
		Action:      "BLACKLIST",
		StatusCode:  403,
		NodeHash:    "node-b",
		CreatedAtNs: now,
	}
	if err := engine.UpsertPlatformEgressExclusion(cooldown); err != nil {
		t.Fatalf("upsert cooldown: %v", err)
	}
	if err := engine.UpsertPlatformEgressExclusion(blacklist); err != nil {
		t.Fatalf("upsert blacklist: %v", err)
	}

	rows, err := engine.ListPlatformEgressExclusions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}

	if err := engine.DeleteExpiredPlatformEgressExclusions(now); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	rows, err = engine.ListPlatformEgressExclusions()
	if err != nil {
		t.Fatalf("list after expire: %v", err)
	}
	if len(rows) != 1 || rows[0].EgressIP != blacklist.EgressIP {
		t.Fatalf("blacklist should remain after expiry sweep: %+v", rows)
	}

	if err := engine.DeletePlatformEgressExclusion(blacklist.PlatformID, blacklist.EgressIP); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err = engine.ListPlatformEgressExclusions()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows remain after delete: %+v", rows)
	}
}

func TestDeletePlatform_CascadesEgressExclusions(t *testing.T) {
	engine := newStateTestEngine(t)
	now := time.Now().UnixNano()
	row := model.Platform{
		ID:                     "plat-cascade",
		Name:                   "cascade-platform",
		StickyTTLNs:            int64(time.Hour),
		RegexFilters:           []string{},
		RegionFilters:          []string{},
		ReverseProxyMissAction: "TREAT_AS_EMPTY",
		AllocationPolicy:       "BALANCED",
		UpdatedAtNs:            now,
	}
	if err := engine.UpsertPlatform(row); err != nil {
		t.Fatalf("UpsertPlatform: %v", err)
	}
	if err := engine.UpsertPlatformEgressExclusion(model.PlatformEgressExclusion{
		PlatformID: row.ID, EgressIP: "203.0.113.70",
		Action: "BLACKLIST", StatusCode: 403, CreatedAtNs: now,
	}); err != nil {
		t.Fatalf("upsert exclusion: %v", err)
	}

	if err := engine.DeletePlatform(row.ID); err != nil {
		t.Fatalf("DeletePlatform: %v", err)
	}
	rows, err := engine.ListPlatformEgressExclusions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("exclusions should be deleted with their platform: %+v", rows)
	}
}
