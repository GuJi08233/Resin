package platform

import (
	"net/netip"
	"testing"
	"time"
)

func TestGuardStore_CooldownExpiry(t *testing.T) {
	store := NewGuardStore()
	now := time.Now()
	ip := netip.MustParseAddr("203.0.113.10")
	rule := StatusCodeRule{
		CodeFrom: 429, CodeTo: 429,
		Action: StatusCodeActionCooldown, Cooldown: time.Minute,
	}

	entry, changed := store.Apply("plat-1", ip, rule, 429, "node-a", now)
	if !changed {
		t.Fatal("first cooldown should change state")
	}
	if !entry.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("expires at %v, want %v", entry.ExpiresAt, now.Add(time.Minute))
	}
	if !store.Excluded("plat-1", ip, now) {
		t.Fatal("ip should be excluded during cooldown")
	}
	if store.Excluded("plat-1", ip, now.Add(time.Minute)) {
		t.Fatal("ip should be excluded no longer at expiry")
	}
	if store.Excluded("plat-2", ip, now) {
		t.Fatal("cooldown must be platform-scoped")
	}
}

func TestGuardStore_RepeatedCooldownDoesNotExtend(t *testing.T) {
	store := NewGuardStore()
	now := time.Now()
	ip := netip.MustParseAddr("203.0.113.11")
	rule := StatusCodeRule{
		CodeFrom: 429, CodeTo: 429,
		Action: StatusCodeActionCooldown, Cooldown: time.Minute,
	}

	first, _ := store.Apply("plat-1", ip, rule, 429, "node-a", now)
	if _, changed := store.Apply("plat-1", ip, rule, 429, "node-a", now.Add(30*time.Second)); changed {
		t.Fatal("repeated cooldown should be a no-op")
	}
	entries := store.List("plat-1")
	if len(entries) != 1 || !entries[0].ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("cooldown should not be extended: %+v", entries)
	}
}

func TestGuardStore_ExpiredCooldownCanMatchAgainWithoutSweep(t *testing.T) {
	for _, action := range []StatusCodeAction{StatusCodeActionCooldown, StatusCodeActionBlacklist} {
		t.Run(string(action), func(t *testing.T) {
			store := NewGuardStore()
			now := time.Now()
			ip := netip.MustParseAddr("203.0.113.17")
			cooldown := StatusCodeRule{
				CodeFrom: 429, CodeTo: 429,
				Action: StatusCodeActionCooldown, Cooldown: time.Minute,
			}
			first, _ := store.Apply("plat-1", ip, cooldown, 429, "node-a", now)
			retriggeredAt := first.ExpiresAt
			rule := StatusCodeRule{CodeFrom: 403, CodeTo: 403, Action: action}
			if action == StatusCodeActionCooldown {
				rule.Cooldown = 2 * time.Minute
			}

			entry, changed := store.Apply("plat-1", ip, rule, 403, "node-b", retriggeredAt)
			if !changed {
				t.Fatal("a match at cooldown expiry should create a new exclusion without an expiry sweep")
			}
			if entry.Action != action || entry.StatusCode != 403 || entry.NodeHash != "node-b" || !entry.CreatedAt.Equal(retriggeredAt) {
				t.Fatalf("expected a fresh exclusion for the new match, got %+v", entry)
			}
			if !store.Excluded("plat-1", ip, retriggeredAt) {
				t.Fatal("the new exclusion must take effect immediately")
			}
			if action == StatusCodeActionCooldown {
				if !entry.ExpiresAt.Equal(retriggeredAt.Add(rule.Cooldown)) {
					t.Fatalf("new cooldown expires at %v, want %v", entry.ExpiresAt, retriggeredAt.Add(rule.Cooldown))
				}
			} else if !entry.ExpiresAt.IsZero() || !store.Excluded("plat-1", ip, retriggeredAt.Add(24*time.Hour)) {
				t.Fatalf("new blacklist must be permanent, got %+v", entry)
			}
			if entries := store.List("plat-1"); len(entries) != 1 {
				t.Fatalf("expected one replacement exclusion, got %+v", entries)
			}
		})
	}
}

func TestGuardStore_BlacklistUpgradesAndWins(t *testing.T) {
	store := NewGuardStore()
	now := time.Now()
	ip := netip.MustParseAddr("203.0.113.12")
	cooldown := StatusCodeRule{
		CodeFrom: 429, CodeTo: 429,
		Action: StatusCodeActionCooldown, Cooldown: time.Minute,
	}
	blacklist := StatusCodeRule{CodeFrom: 403, CodeTo: 403, Action: StatusCodeActionBlacklist}

	store.Apply("plat-1", ip, cooldown, 429, "node-a", now)
	upgraded, changed := store.Apply("plat-1", ip, blacklist, 403, "node-a", now.Add(time.Second))
	if !changed {
		t.Fatal("blacklist should upgrade an existing cooldown")
	}
	if upgraded.Action != StatusCodeActionBlacklist || !upgraded.ExpiresAt.IsZero() {
		t.Fatalf("unexpected upgraded entry: %+v", upgraded)
	}
	if !store.Excluded("plat-1", ip, now.Add(24*time.Hour)) {
		t.Fatal("blacklist must be permanent")
	}
	if _, changed := store.Apply("plat-1", ip, blacklist, 403, "node-a", now); changed {
		t.Fatal("repeated blacklist should be a no-op")
	}
}

func TestGuardStore_UnblockAndExpire(t *testing.T) {
	store := NewGuardStore()
	now := time.Now()
	ip := netip.MustParseAddr("203.0.113.13")
	rule := StatusCodeRule{
		CodeFrom: 429, CodeTo: 429,
		Action: StatusCodeActionCooldown, Cooldown: time.Minute,
	}
	store.Apply("plat-1", ip, rule, 429, "node-a", now)

	if _, ok := store.Unblock("plat-1", ip); !ok {
		t.Fatal("unblock should find the entry")
	}
	if store.Excluded("plat-1", ip, now) {
		t.Fatal("unblocked ip should not be excluded")
	}

	store.Apply("plat-1", ip, rule, 429, "node-a", now)
	expired := store.Expire(now.Add(2 * time.Minute))
	if len(expired) != 1 || expired[0].EgressIP != ip {
		t.Fatalf("expected one expired entry, got %+v", expired)
	}
	if len(store.List("plat-1")) != 0 {
		t.Fatal("expired entries should be removed")
	}
}

func TestGuardStore_LoadDropsExpired(t *testing.T) {
	store := NewGuardStore()
	now := time.Now()
	validIP := netip.MustParseAddr("203.0.113.14")
	expiredIP := netip.MustParseAddr("203.0.113.15")

	loaded, expired := store.Load([]Exclusion{
		{
			PlatformID: "plat-1", EgressIP: validIP,
			Action: StatusCodeActionCooldown, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		{
			PlatformID: "plat-1", EgressIP: expiredIP,
			Action: StatusCodeActionCooldown, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
		},
		{
			PlatformID: "plat-1", EgressIP: netip.MustParseAddr("203.0.113.16"),
			Action: StatusCodeActionBlacklist, CreatedAt: now,
		},
	}, now)

	if len(loaded) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(loaded))
	}
	if len(expired) != 1 || expired[0].EgressIP != expiredIP {
		t.Fatalf("unexpected expired entries: %+v", expired)
	}
	if !store.Excluded("plat-1", netip.MustParseAddr("203.0.113.16"), now.Add(1000*time.Hour)) {
		t.Fatal("loaded blacklist should be permanent")
	}
}
