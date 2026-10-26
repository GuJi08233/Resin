package warppool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/warp"
)

const (
	parentHistoryTTL      = 7 * 24 * time.Hour
	parentHistoryLimit    = 4096
	parentHistoryMaxBytes = 2 << 20
)

type parentHistoryKey struct {
	parent node.Hash
	family warp.Family
}

type parentHistorySuccess struct {
	latency     time.Duration
	lastSuccess int64
}

type parentHistoryEntry struct {
	Parent        string      `json:"parent"`
	Family        warp.Family `json:"family"`
	LatencyNs     int64       `json:"latency_ns"`
	LastSuccessNs int64       `json:"last_success_ns"`
}

type parentHistoryFile struct {
	Version int                  `json:"version"`
	Entries []parentHistoryEntry `json:"entries"`
}

// History stores verified transport outcomes, never tunnel credentials or exit
// addresses. Record includes successful candidates later discarded by IP dedup.
type parentHistory struct {
	mu         sync.RWMutex
	flushMu    sync.Mutex
	path       string
	logf       func(string, ...any)
	entries    map[parentHistoryKey]parentHistorySuccess
	dirty      bool
	generation uint64
}

func newParentHistory(path string, logf func(string, ...any)) *parentHistory {
	if logf == nil {
		logf = log.Printf
	}
	h := &parentHistory{path: path, logf: logf, entries: make(map[parentHistoryKey]parentHistorySuccess)}
	if path != "" {
		h.load(time.Now())
	}
	return h
}

func validHistoryFamily(family warp.Family) bool {
	return family == warp.IPv4 || family == warp.IPv6
}

func historySuccessFresh(last int64, now time.Time) bool {
	// Ignore corrupt future timestamps while tolerating a small clock adjustment.
	return last > now.Add(-parentHistoryTTL).UnixNano() && last <= now.Add(5*time.Minute).UnixNano()
}

func (h *parentHistory) load(now time.Time) {
	file, err := os.Open(h.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.logf("WARP parent history: load failed: %v", err)
		}
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, parentHistoryMaxBytes+1))
	if err != nil {
		h.logf("WARP parent history: read failed: %v", err)
		return
	}
	if len(data) > parentHistoryMaxBytes {
		h.logf("WARP parent history: ignored oversized file")
		return
	}
	var saved parentHistoryFile
	if err := json.Unmarshal(data, &saved); err != nil || saved.Version != 1 {
		// Do not log file contents: a malformed file is not trusted metadata.
		h.logf("WARP parent history: ignored invalid or unsupported file")
		return
	}
	invalid := 0
	for _, entry := range saved.Entries {
		parent, err := node.ParseHex(entry.Parent)
		if err != nil || parent.IsZero() || !validHistoryFamily(entry.Family) || entry.LatencyNs <= 0 || !historySuccessFresh(entry.LastSuccessNs, now) {
			invalid++
			continue
		}
		key := parentHistoryKey{parent, entry.Family}
		success := parentHistorySuccess{time.Duration(entry.LatencyNs), entry.LastSuccessNs}
		previous, exists := h.entries[key]
		if !exists || success.lastSuccess > previous.lastSuccess ||
			(success.lastSuccess == previous.lastSuccess && success.latency < previous.latency) {
			h.entries[key] = success
		}
	}
	if invalid > 0 {
		h.logf("WARP parent history: ignored %d expired or invalid entries", invalid)
		h.dirty = true
	}
	h.pruneLocked(now)
}

func (h *parentHistory) Record(parent node.Hash, family warp.Family, latency time.Duration, now time.Time) {
	if h == nil || parent.IsZero() || !validHistoryFamily(family) || latency <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked(now)
	key := parentHistoryKey{parent, family}
	success := parentHistorySuccess{latency, now.UnixNano()}
	if previous, exists := h.entries[key]; exists && previous == success {
		return
	}
	h.entries[key] = success
	h.dirty = true
	h.generation++
	h.pruneLocked(now)
}

// Preference ranks a verified family first (0), another verified family on the
// same parent second (1), and an unknown parent last (2). Latency breaks ties;
// callers keep their own deterministic parent-hash and fairness tie breakers.
func (h *parentHistory) Preference(parent node.Hash, family warp.Family, now time.Time) (tier int, latency time.Duration) {
	const unknownLatency = time.Duration(1<<63 - 1)
	if h == nil || !validHistoryFamily(family) {
		return 2, unknownLatency
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if success, exists := h.entries[parentHistoryKey{parent, family}]; exists && historySuccessFresh(success.lastSuccess, now) {
		return 0, success.latency
	}
	other := warp.IPv4
	if family == warp.IPv4 {
		other = warp.IPv6
	}
	if success, exists := h.entries[parentHistoryKey{parent, other}]; exists && historySuccessFresh(success.lastSuccess, now) {
		return 1, success.latency
	}
	return 2, unknownLatency
}

func (h *parentHistory) pruneLocked(now time.Time) {
	changed := false
	for key, success := range h.entries {
		if !historySuccessFresh(success.lastSuccess, now) {
			delete(h.entries, key)
			changed = true
		}
	}
	if len(h.entries) > parentHistoryLimit {
		keys := make([]parentHistoryKey, 0, len(h.entries))
		for key := range h.entries {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b parentHistoryKey) int {
			x, y := h.entries[a], h.entries[b]
			if x.lastSuccess != y.lastSuccess {
				if x.lastSuccess > y.lastSuccess {
					return -1
				}
				return 1
			}
			if c := slices.Compare(a.parent[:], b.parent[:]); c != 0 {
				return c
			}
			if a.family < b.family {
				return -1
			}
			if a.family > b.family {
				return 1
			}
			return 0
		})
		for _, key := range keys[parentHistoryLimit:] {
			delete(h.entries, key)
		}
		changed = true
	}
	if changed {
		h.dirty = true
		h.generation++
	}
}

// Flush coalesces verified outcomes recorded during a reconciliation into one
// atomic replacement. Persistence failure leaves the in-memory ranking usable
// and dirty, so a later maintenance cycle can retry without losing progress.
func (h *parentHistory) Flush() {
	if h == nil || h.path == "" {
		return
	}
	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	h.mu.Lock()
	h.pruneLocked(time.Now())
	if !h.dirty {
		h.mu.Unlock()
		return
	}
	saved := parentHistoryFile{Version: 1, Entries: make([]parentHistoryEntry, 0, len(h.entries))}
	for key, success := range h.entries {
		saved.Entries = append(saved.Entries, parentHistoryEntry{key.parent.Hex(), key.family, int64(success.latency), success.lastSuccess})
	}
	generation := h.generation
	h.mu.Unlock()
	slices.SortFunc(saved.Entries, func(a, b parentHistoryEntry) int {
		if a.Parent != b.Parent {
			if a.Parent < b.Parent {
				return -1
			}
			return 1
		}
		if a.Family < b.Family {
			return -1
		}
		if a.Family > b.Family {
			return 1
		}
		return 0
	})
	if err := h.write(saved); err != nil {
		h.logf("WARP parent history: persist failed: %v", err)
		return
	}
	h.mu.Lock()
	if h.generation == generation {
		h.dirty = false
	}
	h.mu.Unlock()
}

func (h *parentHistory) write(saved parentHistoryFile) error {
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".warp-parents-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(saved); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), h.path); err != nil {
		return fmt.Errorf("replace history: %w", err)
	}
	return nil
}
