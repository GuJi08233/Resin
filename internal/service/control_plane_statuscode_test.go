package service

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/state"
	"github.com/Resinat/Resin/internal/topology"
)

func newStatusCodeServiceFixture(t *testing.T) (*ControlPlaneService, *state.StateEngine, *topology.GlobalNodePool) {
	t.Helper()
	dir := t.TempDir()
	engine, closer, err := state.PersistenceBootstrap(
		filepath.Join(dir, "state"),
		filepath.Join(dir, "cache"),
	)
	if err != nil {
		t.Fatalf("PersistenceBootstrap: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	subMgr := topology.NewSubscriptionManager()
	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})
	cp := &ControlPlaneService{
		Engine: engine,
		Pool:   pool,
		SubMgr: subMgr,
		EnvCfg: &config.EnvConfig{
			DefaultPlatformStickyTTL:              30 * time.Minute,
			DefaultPlatformRegexFilters:           []string{},
			DefaultPlatformRegionFilters:          []string{},
			DefaultPlatformReverseProxyMissAction: "TREAT_AS_EMPTY",
			DefaultPlatformAllocationPolicy:       "BALANCED",
		},
	}
	return cp, engine, pool
}

func TestCreateAndUpdatePlatform_StatusCodeRulesRoundTrip(t *testing.T) {
	cp, engine, pool := newStatusCodeServiceFixture(t)

	name := "status-code-platform"
	created, err := cp.CreatePlatform(CreatePlatformRequest{
		Name: &name,
		StatusCodeRules: []StatusCodeRuleRequest{
			{CodeFrom: 429, CodeTo: 429, Action: "COOLDOWN", Cooldown: "5m"},
			{CodeFrom: 403, CodeTo: 403, Action: "BLACKLIST"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}
	if len(created.StatusCodeRules) != 2 {
		t.Fatalf("response rules = %+v, want 2", created.StatusCodeRules)
	}
	if created.StatusCodeRules[0].CodeFrom != 403 || created.StatusCodeRules[0].Action != "BLACKLIST" {
		t.Fatalf("rules should be sorted by range start: %+v", created.StatusCodeRules)
	}
	if created.StatusCodeRules[1].Cooldown != "5m0s" {
		t.Fatalf("cooldown = %q, want 5m0s", created.StatusCodeRules[1].Cooldown)
	}

	stored, err := engine.GetPlatform(created.ID)
	if err != nil {
		t.Fatalf("GetPlatform: %v", err)
	}
	if len(stored.StatusCodeRules) != 2 || stored.StatusCodeRules[1].CooldownNs != int64(5*time.Minute) {
		t.Fatalf("stored rules = %+v", stored.StatusCodeRules)
	}
	runtimePlat, ok := pool.GetPlatform(created.ID)
	if !ok || len(runtimePlat.StatusCodeRules) != 2 {
		t.Fatalf("runtime platform missing rules: %+v", runtimePlat)
	}

	updated, err := cp.UpdatePlatform(created.ID, []byte(
		`{"status_code_rules":[{"code_from":500,"code_to":599,"action":"COOLDOWN","cooldown":"30s"}]}`,
	))
	if err != nil {
		t.Fatalf("UpdatePlatform: %v", err)
	}
	if len(updated.StatusCodeRules) != 1 || updated.StatusCodeRules[0].CodeFrom != 500 {
		t.Fatalf("updated rules = %+v", updated.StatusCodeRules)
	}
	runtimePlat, _ = pool.GetPlatform(created.ID)
	if len(runtimePlat.StatusCodeRules) != 1 || runtimePlat.StatusCodeRules[0].CodeTo != 599 {
		t.Fatalf("runtime rules not replaced: %+v", runtimePlat.StatusCodeRules)
	}

	_, err = cp.UpdatePlatform(created.ID, []byte(
		`{"status_code_rules":[`+
			`{"code_from":400,"code_to":499,"action":"COOLDOWN","cooldown":"1m"},`+
			`{"code_from":450,"code_to":450,"action":"BLACKLIST"}]}`,
	))
	if err == nil {
		t.Fatal("expected overlapping ranges to be rejected")
	}
	var svcErr *ServiceError
	if !errors.As(err, &svcErr) || svcErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestUnblockPlatformEgress_RemovesRuntimeAndPersistedState(t *testing.T) {
	cp, engine, pool := newStatusCodeServiceFixture(t)

	name := "unblock-platform"
	created, err := cp.CreatePlatform(CreatePlatformRequest{
		Name: &name,
		StatusCodeRules: []StatusCodeRuleRequest{
			{CodeFrom: 403, CodeTo: 403, Action: "BLACKLIST"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePlatform: %v", err)
	}

	guard := topology.NewPlatformGuard(pool)
	pool.SetPlatformGuard(guard)

	ip := netip.MustParseAddr("203.0.113.50")
	hash := node.HashFromRawOptions([]byte(`{"type":"ss","server":"203.0.113.50"}`))
	pool.RecordUpstreamStatus(created.ID, hash, ip, 403)

	exclusions, err := cp.ListPlatformExclusions(created.ID)
	if err != nil {
		t.Fatalf("ListPlatformExclusions: %v", err)
	}
	if len(exclusions) != 1 || exclusions[0].EgressIP != ip.String() || exclusions[0].Action != "BLACKLIST" {
		t.Fatalf("unexpected exclusions: %+v", exclusions)
	}

	// Simulate the persisted row written by the guard's persister.
	if err := engine.UpsertPlatformEgressExclusion(model.PlatformEgressExclusion{
		PlatformID:  created.ID,
		EgressIP:    ip.String(),
		Action:      "BLACKLIST",
		StatusCode:  403,
		NodeHash:    hash.Hex(),
		CreatedAtNs: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("UpsertPlatformEgressExclusion: %v", err)
	}

	if err := cp.UnblockPlatformEgress(created.ID, ip.String()); err != nil {
		t.Fatalf("UnblockPlatformEgress: %v", err)
	}
	exclusions, err = cp.ListPlatformExclusions(created.ID)
	if err != nil {
		t.Fatalf("ListPlatformExclusions after unblock: %v", err)
	}
	if len(exclusions) != 0 {
		t.Fatalf("runtime exclusions remain after unblock: %+v", exclusions)
	}
	rows, err := engine.ListPlatformEgressExclusions()
	if err != nil {
		t.Fatalf("ListPlatformEgressExclusions: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted exclusions remain after unblock: %+v", rows)
	}

	if err := cp.UnblockPlatformEgress(created.ID, "not-an-ip"); err == nil {
		t.Fatal("expected invalid egress IP to be rejected")
	}
}
