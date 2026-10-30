package warppool

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

func historyTestParent(index uint64) node.Hash {
	var parent node.Hash
	binary.LittleEndian.PutUint64(parent[:], index+1)
	return parent
}

func readHistoryFile(t *testing.T, path string) parentHistoryFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved parentHistoryFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("history is not a complete JSON document: %v", err)
	}
	return saved
}

func TestParentHistoryPersistsVerifiedFamilyPreferenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", "warp-parents.json")
	now := time.Now()
	parent, unknown := historyTestParent(1), historyTestParent(2)
	history := newParentHistory(path, t.Logf)
	history.Record(parent, warp.IPv6, 25*time.Millisecond, now)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Record should coalesce disk writes until Flush: %v", err)
	}
	history.Flush()
	restarted := newParentHistory(path, t.Logf)
	for _, tc := range []struct {
		parent  node.Hash
		family  warp.Family
		tier    int
		latency time.Duration
	}{
		{parent, warp.IPv6, 0, 25 * time.Millisecond},
		{parent, warp.IPv4, 1, 25 * time.Millisecond},
		{unknown, warp.IPv4, 2, time.Duration(1<<63 - 1)},
	} {
		if tier, latency := restarted.Preference(tc.parent, tc.family, now); tier != tc.tier || latency != tc.latency {
			t.Fatalf("parent=%s family=%s preference=(%d,%s), want (%d,%s)", tc.parent, tc.family, tier, latency, tc.tier, tc.latency)
		}
	}
	if tier, _ := restarted.Preference(parent, warp.IPv6, now.Add(parentHistoryTTL)); tier != 2 {
		t.Fatal("expired history still influenced allocation")
	}
	// Only verified outcome metadata is persisted, with an explicit small schema.
	data, _ := os.ReadFile(path)
	var schema struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &schema); err != nil || len(schema.Entries) != 1 {
		t.Fatalf("unexpected schema: %s, %v", data, err)
	}
	allowed := map[string]bool{"parent": true, "family": true, "latency_ns": true, "last_success_ns": true}
	for field := range schema.Entries[0] {
		if !allowed[field] {
			t.Fatalf("history persisted unrelated transport data: %s", field)
		}
	}
	if schema.Entries[0]["parent"] == nil || schema.Entries[0]["last_success_ns"] == nil {
		t.Fatal("history omitted parent identity or last verified success")
	}
}

func TestParentHistoryCorruptFilesDoNotPreventNewSuccesses(t *testing.T) {
	for _, content := range []string{`{"version":1,"entries":[`, `{"version":99,"entries":[]}`, strings.Repeat("x", parentHistoryMaxBytes+1)} {
		t.Run(fmt.Sprintf("bytes=%d", len(content)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "warp-parents.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs []string
			h := newParentHistory(path, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
			if len(logs) == 0 || len(h.entries) != 0 {
				t.Fatal("corrupt history was not ignored and reported")
			}
			parent := historyTestParent(1)
			h.Record(parent, warp.IPv4, time.Millisecond, time.Now())
			h.Flush()
			if len(readHistoryFile(t, path).Entries) != 1 {
				t.Fatal("a valid success could not replace corrupt history")
			}
		})
	}
}

func TestParentHistoryFiltersExpiredAndInvalidMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	now := time.Now()
	entries := []parentHistoryEntry{
		{historyTestParent(1).Hex(), warp.IPv4, int64(20 * time.Millisecond), now.UnixNano()},
		{historyTestParent(1).Hex(), warp.IPv4, int64(time.Millisecond), now.Add(-time.Hour).UnixNano()},
		{historyTestParent(2).Hex(), warp.IPv4, 1, now.Add(-parentHistoryTTL - time.Second).UnixNano()},
		{historyTestParent(3).Hex(), warp.IPv4, 1, now.Add(time.Hour).UnixNano()},
		{historyTestParent(4).Hex(), "unknown", 1, now.UnixNano()},
		{historyTestParent(5).Hex(), warp.IPv4, -1, now.UnixNano()},
		{"invalid-parent", warp.IPv4, 1, now.UnixNano()},
		{node.Zero.Hex(), warp.IPv4, 1, now.UnixNano()},
	}
	data, _ := json.Marshal(parentHistoryFile{Version: 1, Entries: entries})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newParentHistory(path, t.Logf)
	if len(h.entries) != 1 {
		t.Fatalf("retained %d entries, want only one fresh valid outcome", len(h.entries))
	}
	if tier, latency := h.Preference(historyTestParent(1), warp.IPv4, now); tier != 0 || latency != 20*time.Millisecond {
		t.Fatal("an older duplicate overwrote the most recent successful latency")
	}
	h.Record(historyTestParent(6), warp.IPv4, 0, now)
	h.Record(historyTestParent(7), "invalid", time.Millisecond, now)
	h.Record(node.Zero, warp.IPv4, time.Millisecond, now)
	h.Flush()
	if len(readHistoryFile(t, path).Entries) != 1 {
		t.Fatal("invalid or expired outcomes survived cleanup")
	}
}

func TestParentHistoryBoundsEntriesAndRetainsNewestSuccesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	now := time.Now()
	entries := make([]parentHistoryEntry, parentHistoryLimit+32)
	for index := range entries {
		entries[index] = parentHistoryEntry{historyTestParent(uint64(index)).Hex(), warp.IPv4, 1, now.Add(time.Duration(index)).UnixNano()}
	}
	data, _ := json.Marshal(parentHistoryFile{Version: 1, Entries: entries})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newParentHistory(path, t.Logf)
	if len(h.entries) != parentHistoryLimit {
		t.Fatalf("history grew to %d entries", len(h.entries))
	}
	if tier, _ := h.Preference(historyTestParent(0), warp.IPv4, now); tier != 2 {
		t.Fatal("entry limit retained the oldest outcome")
	}
	if tier, _ := h.Preference(historyTestParent(parentHistoryLimit+31), warp.IPv4, now); tier != 0 {
		t.Fatal("entry limit dropped the newest outcome")
	}
	h.Record(historyTestParent(parentHistoryLimit+100), warp.IPv6, time.Millisecond, now.Add(time.Second))
	h.Flush()
	if saved := readHistoryFile(t, path); len(saved.Entries) != parentHistoryLimit {
		t.Fatalf("persisted %d entries despite the limit", len(saved.Entries))
	}
}

func TestParentHistoryFlushReplacesAtomicallyWithPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits and rename over an open file are POSIX semantics")
	}
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newParentHistory(path, t.Logf)
	h.Record(historyTestParent(1), warp.IPv4, time.Millisecond, time.Now())
	h.Flush()
	before, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != 0o600 {
		t.Fatalf("history mode=%v, error=%v", before, err)
	}
	h.Flush()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("a clean Flush unnecessarily rewrote the history file")
	}
	stop, ready, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		started := false
		for {
			select {
			case <-stop:
				finished <- nil
				return
			default:
			}
			data, err := os.ReadFile(path)
			var saved parentHistoryFile
			if err == nil {
				err = json.Unmarshal(data, &saved)
			}
			if err != nil || saved.Version != 1 || len(saved.Entries) == 0 {
				finished <- fmt.Errorf("reader observed incomplete history: version=%d entries=%d err=%v", saved.Version, len(saved.Entries), err)
				return
			}
			if !started {
				close(ready)
				started = true
			}
		}
	}()
	select {
	case <-ready:
	case err := <-finished:
		t.Fatal(err)
	}
	for index := 2; index < 24; index++ {
		h.Record(historyTestParent(uint64(index)), warp.IPv6, time.Millisecond, time.Now())
		h.Flush()
	}
	close(stop)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	temp, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".warp-parents-*"))
	if err != nil || len(temp) != 0 {
		t.Fatalf("temporary history files were left behind: %v %v", temp, err)
	}
}

func TestParentHistoryWriteFailureRetainsUsableRankingAndCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "existing-data")
	if err := os.WriteFile(sentinel, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	h := newParentHistory(path, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	parent, now := historyTestParent(1), time.Now()
	h.Record(parent, warp.IPv6, time.Millisecond, now)
	h.Flush() // A file must not replace the existing nonempty directory.
	if tier, _ := h.Preference(parent, warp.IPv6, now); tier != 0 {
		t.Fatal("persistence failure lost a verified in-memory ranking")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "kept" {
		t.Fatal("a failed atomic replacement changed existing data")
	}
	found := false
	for _, entry := range logs {
		found = found || strings.Contains(entry, "persist failed")
	}
	if !found {
		t.Fatal("persistence failure was not reported")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	h.Flush()
	if len(readHistoryFile(t, path).Entries) != 1 {
		t.Fatal("dirty history was not retried after the write failure was removed")
	}
}

func TestParentHistoryConcurrentRecordsAndFlushesKeepAllSuccesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	h := newParentHistory(path, t.Logf)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := 0; index < 8; index++ {
				parent := historyTestParent(uint64(worker*8 + index))
				h.Record(parent, warp.IPv6, time.Millisecond, time.Now())
				h.Flush()
				if tier, _ := h.Preference(parent, warp.IPv6, time.Now()); tier != 0 {
					t.Errorf("concurrent update lost parent %s", parent)
				}
			}
		}()
	}
	wg.Wait()
	h.Flush()
	if saved := readHistoryFile(t, path); len(saved.Entries) != 32 {
		t.Fatalf("concurrent persistence retained %d of 32 verified outcomes", len(saved.Entries))
	}
}

func TestManagerRestoresVerifiedDuplicateParentHistory(t *testing.T) {
	f := newPoolFixture(t)
	path := filepath.Join(t.TempDir(), "warp-parents.json")
	manager := NewManager(Options{
		Pool: f.pool, Subscriptions: f.subs, Settings: f.config.Load,
		HistoryPath: path, Concurrency: 1, Logf: t.Logf,
		Create: func(context.Context, adapter.Outbound, warp.Family) (Candidate, error) {
			// Verification succeeded, even though this address duplicates the
			// original node and therefore cannot add a WARP exit to the pool.
			candidate, _ := testCandidate(f.parent.GetEgressIP().String(), 5*time.Millisecond)
			return candidate, nil
		},
	})
	t.Cleanup(manager.Stop)
	manager.reconcile(context.Background())
	manager.history.Flush()
	if len(manager.active) != 0 {
		t.Fatal("duplicate verification unexpectedly added an exit")
	}
	restarted := NewManager(Options{HistoryPath: path, Concurrency: 1, Logf: t.Logf})
	if tier, latency := restarted.history.Preference(f.parent.Hash, warp.IPv4, time.Now()); tier != 0 || latency != 5*time.Millisecond {
		t.Fatalf("manager restart forgot a verified duplicate parent: (%d,%s)", tier, latency)
	}
}
