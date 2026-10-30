package warppool

import (
	"slices"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/warp"
)

func (m *Manager) familyCounts(pending bool) map[warp.Family]int {
	counts := map[warp.Family]int{warp.IPv4: 0, warp.IPv6: 0}
	for key := range m.active {
		counts[key.family]++
	}
	if pending {
		for key := range m.inflight {
			counts[key.family]++
		}
	}
	return counts
}

func (m *Manager) hasCapacity(cfg settings) bool {
	if len(m.active)+len(m.inflight) < cfg.limit {
		return true
	}
	if cfg.stack != "dual" {
		return false
	}
	counts := m.familyCounts(false)
	pending := m.familyCounts(true)
	target6, target4 := (cfg.limit+1)/2, cfg.limit/2
	return (counts[warp.IPv6] < target6 && pending[warp.IPv6] < target6 && counts[warp.IPv4] > target4) ||
		(counts[warp.IPv4] < target4 && pending[warp.IPv4] < target4 && counts[warp.IPv6] > target6)
}

func (m *Manager) cleanScheduling(cfg settings, now time.Time) {
	for key, retryAt := range m.nextTry {
		if _, live := m.opts.Pool.GetEntry(key.parent); !live || key.index >= cfg.limit || !cfg.accepts(key.family) || !now.Before(retryAt) {
			delete(m.nextTry, key)
		}
	}
	for key, failure := range m.parentFailures {
		if _, live := m.opts.Pool.GetEntry(key.parent); !live || !cfg.accepts(key.family) {
			delete(m.parentFailures, key)
		} else if !failure.retryAt.IsZero() && !now.Before(failure.retryAt) {
			// Start a fresh streak, but remember the escalation until success.
			failure.consecutive, failure.retryAt = 0, time.Time{}
			m.parentFailures[key] = failure
		}
	}
	for hash := range m.visited {
		if _, live := m.opts.Pool.GetEntry(hash); !live {
			delete(m.visited, hash)
		}
	}
	for key := range m.lastTried {
		if _, live := m.opts.Pool.GetEntry(key.parent); !live {
			delete(m.lastTried, key)
			delete(m.duplicateTries, key)
		}
	}
}

type rankedSlot struct {
	key         slot
	parent      *node.NodeEntry
	visited     bool
	familyRank  int
	historyTier int
	latency     time.Duration
	lastTried   uint64
}

func (m *Manager) pickCandidate(cfg settings, now time.Time) (slot, *node.NodeEntry, bool) {
	counts := m.familyCounts(true)
	preferred := warp.IPv6
	if cfg.stack == "ipv4" {
		preferred = warp.IPv4
	}
	if cfg.stack == "dual" {
		if counts[warp.IPv4] < counts[warp.IPv6] || (counts[warp.IPv4] == counts[warp.IPv6] && m.lastFamily == warp.IPv6) {
			preferred = warp.IPv4
		}
		// A family returning duplicates cannot monopolize all future work just
		// because its published count stays low. Keep probing both families.
		if m.familyStreak >= 2 {
			preferred = warp.IPv6
			if m.lastFamily == warp.IPv6 {
				preferred = warp.IPv4
			}
		}
	}
	busyParents := make(map[node.Hash]bool, len(m.inflight))
	for key := range m.inflight {
		busyParents[key.parent] = true
	}
	deferredFamilies := make(map[parentFamily]bool)
	for key, retryAt := range m.nextTry {
		if now.Before(retryAt) {
			deferredFamilies[parentFamily{key.parent, key.family}] = true
		}
	}
	full := len(m.active)+len(m.inflight) >= cfg.limit
	var candidates []rankedSlot
	m.opts.Pool.RangeNodes(func(_ node.Hash, parent *node.NodeEntry) bool {
		if busyParents[parent.Hash] || !m.eligible(parent) {
			return true
		}
		for _, family := range []warp.Family{warp.IPv6, warp.IPv4} {
			lane := parentFamily{parent.Hash, family}
			if !cfg.accepts(family) || deferredFamilies[lane] || now.Before(m.parentFailures[lane].retryAt) {
				continue
			}
			if full {
				target := cfg.limit / 2
				if family == warp.IPv6 {
					target = (cfg.limit + 1) / 2
				}
				if counts[family] >= target {
					continue
				}
			}
			// The earliest vacant slot owns retries. A deferred duplicate cannot
			// bypass its cooldown by allocating a larger slot on the same lane.
			for index := 0; index < cfg.limit; index++ {
				key := slot{parent.Hash, family, index}
				if _, active := m.active[key]; active {
					continue
				}
				if _, pending := m.inflight[key]; pending {
					break
				}
				if now.Before(m.nextTry[key]) {
					break
				}
				tier, latency := m.history.Preference(parent.Hash, family, now)
				familyRank := 1
				if family == preferred {
					familyRank = 0
				}
				candidates = append(candidates, rankedSlot{key, parent, m.visited[parent.Hash], familyRank, tier, latency, m.lastTried[parentFamily{parent.Hash, family}]})
				break
			}
		}
		return true
	})
	slices.SortFunc(candidates, func(a, b rankedSlot) int {
		if a.visited != b.visited {
			if !a.visited {
				return -1
			}
			return 1
		}
		if a.familyRank != b.familyRank {
			return a.familyRank - b.familyRank
		}
		// After initial parent coverage, rotate whole lanes. Repeated first-slot
		// failures and verified duplicates must yield to productive expansion.
		// History breaks initial ties without overriding ongoing fairness.
		if a.lastTried != b.lastTried {
			if a.lastTried < b.lastTried {
				return -1
			}
			return 1
		}
		if a.historyTier != b.historyTier {
			return a.historyTier - b.historyTier
		}
		if a.latency != b.latency {
			if a.latency < b.latency {
				return -1
			}
			return 1
		}
		return slices.Compare(a.key.parent[:], b.key.parent[:])
	})
	if len(candidates) == 0 {
		return slot{}, nil, false
	}
	return candidates[0].key, candidates[0].parent, true
}
