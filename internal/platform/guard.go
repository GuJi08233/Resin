package platform

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

// MaxPlatformExclusions bounds per-platform runtime exclusion state so a
// misbehaving upstream returning distinct status codes cannot grow it without
// limit. Cooldowns are evicted first; permanent blacklists are never evicted.
const MaxPlatformExclusions = 4096

// Exclusion is one platform-scoped egress IP cooldown or blacklist entry.
// ExpiresAt is zero for permanent blacklists.
type Exclusion struct {
	PlatformID string
	EgressIP   netip.Addr
	Action     StatusCodeAction
	StatusCode int
	NodeHash   string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// IsExpired reports whether this entry is an expired cooldown.
func (e Exclusion) IsExpired(now time.Time) bool {
	return e.Action == StatusCodeActionCooldown && !now.Before(e.ExpiresAt)
}

// GuardStore holds platform-scoped egress exclusions in memory. It is safe for
// concurrent use; callers persist changes separately.
type GuardStore struct {
	mu         sync.RWMutex
	byPlatform map[string]map[netip.Addr]Exclusion
}

// NewGuardStore creates an empty guard store.
func NewGuardStore() *GuardStore {
	return &GuardStore{byPlatform: make(map[string]map[netip.Addr]Exclusion)}
}

// Load restores persisted entries. Expired cooldowns are returned separately so
// the caller can delete them from persistence. Entries beyond the per-platform
// cap are dropped.
func (s *GuardStore) Load(entries []Exclusion, now time.Time) (loaded []Exclusion, expired []Exclusion) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		if entry.PlatformID == "" || !entry.EgressIP.IsValid() {
			continue
		}
		if entry.Action != StatusCodeActionCooldown && entry.Action != StatusCodeActionBlacklist {
			continue
		}
		if entry.IsExpired(now) {
			expired = append(expired, entry)
			continue
		}
		platformEntries := s.byPlatform[entry.PlatformID]
		if platformEntries == nil {
			platformEntries = make(map[netip.Addr]Exclusion)
			s.byPlatform[entry.PlatformID] = platformEntries
		}
		if _, exists := platformEntries[entry.EgressIP]; exists {
			continue
		}
		if len(platformEntries) >= MaxPlatformExclusions {
			continue
		}
		platformEntries[entry.EgressIP] = entry
		loaded = append(loaded, entry)
	}
	return loaded, expired
}

// Excluded reports whether the egress IP is currently excluded for the platform.
func (s *GuardStore) Excluded(platformID string, ip netip.Addr, now time.Time) bool {
	if platformID == "" || !ip.IsValid() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.byPlatform[platformID][ip]
	if !ok {
		return false
	}
	if entry.Action == StatusCodeActionBlacklist {
		return true
	}
	return now.Before(entry.ExpiresAt)
}

// Apply records a rule match. It returns the new or upgraded entry and whether
// state changed. Repeated matches during an active cooldown are no-ops (the
// first trigger wins); an expired cooldown can match again before expiry cleanup.
// A blacklist upgrades an existing cooldown and is otherwise idempotent.
func (s *GuardStore) Apply(
	platformID string,
	ip netip.Addr,
	rule StatusCodeRule,
	statusCode int,
	nodeHash string,
	now time.Time,
) (Exclusion, bool) {
	if platformID == "" || !ip.IsValid() || !rule.Action.IsValid() {
		return Exclusion{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	platformEntries := s.byPlatform[platformID]
	if platformEntries == nil {
		platformEntries = make(map[netip.Addr]Exclusion)
		s.byPlatform[platformID] = platformEntries
	}

	if existing, ok := platformEntries[ip]; ok {
		if existing.IsExpired(now) {
			delete(platformEntries, ip)
		} else {
			if existing.Action == StatusCodeActionBlacklist {
				return Exclusion{}, false
			}
			if rule.Action == StatusCodeActionBlacklist {
				existing.Action = StatusCodeActionBlacklist
				existing.StatusCode = statusCode
				existing.NodeHash = nodeHash
				existing.ExpiresAt = time.Time{}
				platformEntries[ip] = existing
				return existing, true
			}
			return Exclusion{}, false
		}
	}

	if len(platformEntries) >= MaxPlatformExclusions {
		evictedIP, _, ok := oldestCooldown(platformEntries)
		if !ok {
			return Exclusion{}, false
		}
		delete(platformEntries, evictedIP)
	}

	entry := Exclusion{
		PlatformID: platformID,
		EgressIP:   ip,
		Action:     rule.Action,
		StatusCode: statusCode,
		NodeHash:   nodeHash,
		CreatedAt:  now,
	}
	if rule.Action == StatusCodeActionCooldown {
		entry.ExpiresAt = now.Add(rule.Cooldown)
	}
	platformEntries[ip] = entry
	return entry, true
}

func oldestCooldown(entries map[netip.Addr]Exclusion) (netip.Addr, Exclusion, bool) {
	var oldestIP netip.Addr
	var oldest Exclusion
	found := false
	for ip, entry := range entries {
		if entry.Action != StatusCodeActionCooldown {
			continue
		}
		if !found || entry.ExpiresAt.Before(oldest.ExpiresAt) {
			oldestIP = ip
			oldest = entry
			found = true
		}
	}
	return oldestIP, oldest, found
}

// Unblock removes an entry and returns it when it existed.
func (s *GuardStore) Unblock(platformID string, ip netip.Addr) (Exclusion, bool) {
	if platformID == "" || !ip.IsValid() {
		return Exclusion{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	platformEntries := s.byPlatform[platformID]
	entry, ok := platformEntries[ip]
	if !ok {
		return Exclusion{}, false
	}
	delete(platformEntries, ip)
	if len(platformEntries) == 0 {
		delete(s.byPlatform, platformID)
	}
	return entry, true
}

// List returns a snapshot of a platform's exclusions, newest first.
func (s *GuardStore) List(platformID string) []Exclusion {
	s.mu.RLock()
	defer s.mu.RUnlock()

	platformEntries := s.byPlatform[platformID]
	if len(platformEntries) == 0 {
		return []Exclusion{}
	}
	result := make([]Exclusion, 0, len(platformEntries))
	for _, entry := range platformEntries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].EgressIP.String() < result[j].EgressIP.String()
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

// Expire removes all expired cooldowns and returns them.
func (s *GuardStore) Expire(now time.Time) []Exclusion {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []Exclusion
	for platformID, platformEntries := range s.byPlatform {
		for ip, entry := range platformEntries {
			if entry.IsExpired(now) {
				expired = append(expired, entry)
				delete(platformEntries, ip)
			}
		}
		if len(platformEntries) == 0 {
			delete(s.byPlatform, platformID)
		}
	}
	return expired
}

// ClearPlatform removes all entries for a platform and returns them.
func (s *GuardStore) ClearPlatform(platformID string) []Exclusion {
	s.mu.Lock()
	defer s.mu.Unlock()

	platformEntries := s.byPlatform[platformID]
	if len(platformEntries) == 0 {
		return nil
	}
	removed := make([]Exclusion, 0, len(platformEntries))
	for _, entry := range platformEntries {
		removed = append(removed, entry)
	}
	delete(s.byPlatform, platformID)
	return removed
}

// Count returns the number of entries for a platform.
func (s *GuardStore) Count(platformID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byPlatform[platformID])
}
