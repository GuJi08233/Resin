// Package warppool adds independently probed WARP exits to Resin's node pool.
package warppool

import (
	"context"
	"io"
	"log"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/topology"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
)

// Candidate contains a live tunnel whose actual public address was verified.
type Candidate struct {
	Outbound adapter.Outbound
	IP       netip.Addr
	Latency  time.Duration
}

type CreateFunc func(context.Context, adapter.Outbound, warp.Family) (Candidate, error)

// Every tunnel incarnation gets a distinct node identity. In-flight requests
// and probes from an older tunnel must not write back into its replacement.
var exitGeneration atomic.Uint64

type Options struct {
	Pool              *topology.GlobalNodePool
	Subscriptions     *topology.SubscriptionManager
	Settings          func() *config.RuntimeConfig
	MaxLatencyEntries int
	Create            CreateFunc // nil uses the real WARP factory
	Logf              func(string, ...any)
	Concurrency       int // bounded simultaneous creation attempts; one gives synchronous reconciliation
	HistoryPath       string
}

type slot struct {
	parent node.Hash
	family warp.Family
	index  int
}

type managedExit struct {
	entry  *node.NodeEntry
	parent *node.NodeEntry
	via    *adapter.Outbound
}

const (
	parentFailureLimit    = 3
	parentFailureCooldown = 30 * time.Minute
)

type parentFailure struct {
	consecutive int
	retryAt     time.Time
}

type parentFamily struct {
	parent node.Hash
	family warp.Family
}

type settings struct {
	enabled bool
	stack   string
	limit   int
	count   int
	tries   int
}

// Manager owns tunnel creation and retirement. All map mutations are serialized
// on its worker; parent deletion can independently remove entries from the pool.
type Manager struct {
	opts           Options
	active         map[slot]managedExit
	nextTry        map[slot]time.Time
	parentFailures map[parentFamily]parentFailure
	lastAttempt    slot
	hasLastAttempt bool
	mu             sync.Mutex
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	started        bool
	stopped        bool
	history        *parentHistory
	inflight       map[slot]*creationJob
	results        chan creationResult
	jobs           sync.WaitGroup
	visited        map[node.Hash]bool
	duplicateTries map[parentFamily]int
	lastTried      map[parentFamily]uint64
	sequence       uint64
	lastFamily     warp.Family
	familyStreak   int
	rateRetryAt    time.Time
}

func NewManager(opts Options) *Manager {
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 4
	}
	opts.Concurrency = min(opts.Concurrency, 8)
	if opts.Create == nil {
		factory := warp.NewFactory()
		opts.Create = func(ctx context.Context, parent adapter.Outbound, family warp.Family) (Candidate, error) {
			exit, err := factory.Create(ctx, parent, family)
			if err != nil {
				return Candidate{}, err
			}
			return Candidate{Outbound: exit, IP: exit.EgressIP, Latency: exit.Latency}, nil
		}
	}
	return &Manager{
		opts: opts, active: make(map[slot]managedExit), nextTry: make(map[slot]time.Time),
		parentFailures: make(map[parentFamily]parentFailure), history: newParentHistory(opts.HistoryPath, opts.Logf),
		inflight: make(map[slot]*creationJob), results: make(chan creationResult, opts.Concurrency),
		visited: make(map[node.Hash]bool), duplicateTries: make(map[parentFamily]int), lastTried: make(map[parentFamily]uint64),
	}
}

func (m *Manager) currentSettings() settings {
	cfg := config.NewDefaultRuntimeConfig()
	if m.opts.Settings != nil {
		if current := m.opts.Settings(); current != nil {
			cfg = current
		}
	}
	enabled := cfg.WarpEnabled
	if m.opts.Subscriptions != nil {
		sub := m.opts.Subscriptions.Lookup(subscription.WarpSubscriptionID)
		enabled = enabled && sub != nil && sub.Enabled()
	}
	return settings{enabled, cfg.WarpStack, cfg.WarpMaxNodes, cfg.WarpExitsPerFamily, cfg.WarpReconnectAttempts}
}

func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.stopped {
		return
	}
	m.started = true
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.shutdown()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			m.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case result := <-m.results:
				m.completeAttempt(ctx, result)
			}
		}
	}()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	if !m.started {
		m.shutdown()
	}
}

func closeCandidate(candidate Candidate) {
	if closer, ok := candidate.Outbound.(io.Closer); ok {
		_ = closer.Close()
	}
}

func (m *Manager) remove(key slot) {
	if exit, ok := m.active[key]; ok {
		m.opts.Pool.RemoveDerivedNode(exit.entry.Hash)
		delete(m.active, key)
		if sub := m.managedSubscription(); sub != nil {
			now := time.Now()
			sub.LastUpdatedNs.Store(now.UnixNano())
			sub.RecordMaintenance(now)
		}
	}
}

func (m *Manager) managedSubscription() *subscription.Subscription {
	if m.opts.Subscriptions != nil {
		return m.opts.Subscriptions.Lookup(subscription.WarpSubscriptionID)
	}
	return nil
}

func (m *Manager) recordProgress() {
	if sub := m.managedSubscription(); sub != nil {
		sub.RecordMaintenance(time.Now())
	}
}

func (m *Manager) clear() {
	for _, job := range m.inflight {
		job.cancel()
	}
	for key := range m.active {
		m.remove(key)
	}
	clear(m.nextTry)
	clear(m.parentFailures)
	m.hasLastAttempt = false
	clear(m.visited)
	clear(m.duplicateTries)
	clear(m.lastTried)
	m.lastFamily = ""
	m.familyStreak = 0
	m.history.Flush()
}

func (s settings) accepts(family warp.Family) bool {
	return s.stack == "dual" || s.stack == string(family)
}

func (m *Manager) eligible(parent *node.NodeEntry) bool {
	if parent == nil || parent.ParentHash != nil || !parent.IsHealthy() || m.opts.Pool.IsNodeDisabled(parent.Hash) {
		return false
	}
	via := parent.Outbound.Load()
	return via != nil && *via != nil && slices.Contains((*via).Network(), N.NetworkUDP)
}

func (m *Manager) latency(entry *node.NodeEntry) time.Duration {
	if entry.LatencyTable != nil {
		if stats, ok := entry.LatencyTable.GetDomainStats("cloudflare.com"); ok && stats.Ewma > 0 {
			return stats.Ewma
		}
	}
	return time.Duration(1<<63 - 1)
}

func (m *Manager) reconcile(ctx context.Context) {
	m.drainResults(ctx)
	if len(m.inflight) == 0 {
		m.recordProgress()
	}
	defer func() {
		if len(m.inflight) == 0 {
			m.recordProgress()
		}
		m.history.Flush()
	}()
	cfg := m.currentSettings()
	for _, job := range m.inflight {
		parent, exists := m.opts.Pool.GetEntry(job.key.parent)
		if cfg != job.cfg || !exists || parent != job.parent || !m.eligible(parent) || parent.Outbound.Load() != job.via {
			job.cancel()
		}
	}
	if !cfg.enabled {
		m.clear()
		return
	}
	if cfg.count < 1 || cfg.limit < 1 || cfg.tries < 1 {
		return // invalid persisted settings must never create unbounded tunnels
	}
	// Retire exits immediately after a configuration, parent, or health change.
	for key, exit := range m.active {
		parent, exists := m.opts.Pool.GetEntry(key.parent)
		current, live := m.opts.Pool.GetEntry(exit.entry.Hash)
		if !exists || parent != exit.parent || !m.eligible(parent) ||
			parent.Outbound.Load() != exit.via || !live || current != exit.entry ||
			!exit.entry.IsHealthy() || !cfg.accepts(key.family) {
			m.remove(key)
			delete(m.nextTry, key)
			continue
		}
		// Use successful egress-probe evidence, not this loop's heartbeat, so
		// a long-running full pool keeps useful restart hints fresh honestly.
		checked := exit.entry.LastEgressUpdate.Load()
		latency := m.latency(exit.entry)
		if checked > 0 && latency > 0 && latency != time.Duration(1<<63-1) {
			m.history.Record(key.parent, key.family, latency, time.Unix(0, checked))
		}
	}
	// Regular egress probes can discover that a previously unique exit changed.
	// Keep the fastest WARP path for each actual address, preserving base nodes.
	seen := make(map[netip.Addr]slot)
	baseIPs := make(map[netip.Addr]bool)
	m.opts.Pool.RangeNodes(func(_ node.Hash, entry *node.NodeEntry) bool {
		if entry.ParentHash == nil && entry.GetEgressIP().IsValid() {
			baseIPs[entry.GetEgressIP().Unmap()] = true
		}
		return true
	})
	for key, exit := range m.active {
		ip := exit.entry.GetEgressIP().Unmap()
		if baseIPs[ip] {
			m.remove(key)
			delete(m.nextTry, key)
			continue
		}
		if other, duplicate := seen[ip]; duplicate {
			if m.latency(exit.entry) < m.latency(m.active[other].entry) {
				m.remove(other)
				delete(m.nextTry, other)
				seen[ip] = key
			} else {
				m.remove(key)
				delete(m.nextTry, key)
			}
		} else {
			seen[ip] = key
		}
	}
	for len(m.active) > cfg.limit {
		var slowest slot
		var latency time.Duration
		for key, exit := range m.active {
			if value := m.latency(exit.entry); value >= latency {
				slowest, latency = key, value
			}
		}
		m.remove(slowest)
	}
	if ctx.Err() != nil || time.Now().Before(m.rateRetryAt) {
		return
	}
	m.cleanScheduling(cfg, time.Now())
	for len(m.inflight) < m.opts.Concurrency && m.hasCapacity(cfg) {
		key, parent, found := m.pickCandidate(cfg, time.Now())
		if !found {
			break
		}
		m.dispatch(ctx, cfg, key, parent)
		if m.opts.Concurrency == 1 {
			break
		}
	}
}

// attemptContext cancels a registration promptly when settings change, so a
// disabled WARP feature cannot leave a long-running factory call publishing.
func (m *Manager) attemptContext(ctx context.Context, expected settings) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if m.currentSettings() != expected {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}

func (m *Manager) duplicate(ip netip.Addr) (slot, bool, bool) {
	ip = ip.Unmap()
	baseDuplicate := false
	m.opts.Pool.RangeNodes(func(_ node.Hash, entry *node.NodeEntry) bool {
		if entry.ParentHash == nil && entry.GetEgressIP().Unmap() == ip {
			baseDuplicate = true
			return false
		}
		return true
	})
	if baseDuplicate {
		return slot{}, true, true
	}
	for key, exit := range m.active {
		if exit.entry.GetEgressIP().Unmap() == ip {
			return key, true, false
		}
	}
	return slot{}, false, false
}
