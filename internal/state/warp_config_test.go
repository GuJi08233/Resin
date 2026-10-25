package state

import "testing"

func TestStateRepo_OldConfigGetsWarpDefaults(t *testing.T) {
	repo := newTestStateRepo(t)
	_, err := repo.db.Exec(`INSERT INTO system_config (id, config_json, version, updated_at_ns) VALUES (1, ?, 7, 1)`,
		`{"request_log_enabled":false,"max_consecutive_failures":9}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, version, err := repo.GetSystemConfig()
	if err != nil {
		t.Fatal(err)
	}
	if version != 7 || cfg.RequestLogEnabled || cfg.MaxConsecutiveFailures != 9 {
		t.Fatalf("existing settings were changed: version=%d config=%+v", version, cfg)
	}
	if cfg.WarpEnabled || cfg.WarpStack != "dual" || cfg.WarpExitsPerFamily != 1 ||
		cfg.WarpMaxNodes != 300 || cfg.WarpReconnectAttempts != 3 {
		t.Fatalf("missing WARP defaults: %+v", cfg)
	}
}

func TestStateRepo_PreservesExplicitWarpLimit(t *testing.T) {
	repo := newTestStateRepo(t)
	_, err := repo.db.Exec(`INSERT INTO system_config (id, config_json, version, updated_at_ns) VALUES (1, ?, 2, 1)`,
		`{"warp_max_nodes":32}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := repo.GetSystemConfig()
	if err != nil || cfg.WarpMaxNodes != 32 {
		t.Fatalf("explicit configured limit was replaced by the new default: %+v, %v", cfg, err)
	}
}
