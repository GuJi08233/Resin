// Package warppool adds independently probed WARP exits to Resin's node pool.
package warppool

import (
	"context"
	"encoding/json"
	"fmt"
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
	parentFailures map[node.Hash]parentFailure
	lastAttempt    slot
	hasLastAttempt bool
	mu             sync.Mutex
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	started        bool
	stopped        bool
}

func NewManager(opts Options) *Manager {
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
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
	return &Manager{opts: opts, active: make(map[slot]managedExit), nextTry: make(map[slot]time.Time), parentFailures: make(map[node.Hash]parentFailure)}
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
		defer m.clear()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			m.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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
	}
}

func (m *Manager) clear() {
	for key := range m.active {
		m.remove(key)
	}
	clear(m.nextTry)
	clear(m.parentFailures)
	m.hasLastAttempt = false
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
	cfg := m.currentSettings()
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
	if ctx.Err() != nil || len(m.active) >= cfg.limit {
		return
	}
	parents := make([]*node.NodeEntry, 0)
	m.opts.Pool.RangeNodes(func(_ node.Hash, entry *node.NodeEntry) bool {
		if m.eligible(entry) {
			parents = append(parents, entry)
		}
		return true
	})
	slices.SortFunc(parents, func(a, b *node.NodeEntry) int { return slices.Compare(a.Hash[:], b.Hash[:]) })
	families := 1
	if cfg.stack == "dual" {
		families = 2
	}
	// The subscription limit controls expansion, including when there is only
	// one parent. The per-family setting is an initial candidate allocation,
	// rather than a hidden ceiling that prevents the subscription from growing.
	rounds := cfg.count
	if len(parents) > 0 {
		perRound := len(parents) * families
		rounds = max(rounds, (cfg.limit+perRound-1)/perRound)
	}
	for key, retryAt := range m.nextTry {
		if _, exists := m.opts.Pool.GetEntry(key.parent); !exists || key.index >= rounds || !cfg.accepts(key.family) || time.Now().After(retryAt) {
			delete(m.nextTry, key)
		}
	}
	for hash, failure := range m.parentFailures {
		if _, exists := m.opts.Pool.GetEntry(hash); !exists || (!failure.retryAt.IsZero() && !time.Now().Before(failure.retryAt)) {
			delete(m.parentFailures, hash)
		}
	}
	// Keep a persistent cursor. A slow failed candidate can outlive a previous
	// slot's cooldown; restarting from the first hash would starve later nodes.
	// Try every parent before moving to the other family or another slot.
	var first, after slot
	foundFirst, foundAfter := false, false
findCandidate:
	for index := 0; index < rounds; index++ {
		for _, family := range []warp.Family{warp.IPv4, warp.IPv6} {
			for _, parent := range parents {
				if !cfg.accepts(family) {
					continue
				}
				if time.Now().Before(m.parentFailures[parent.Hash].retryAt) {
					continue
				}
				key := slot{parent.Hash, family, index}
				if _, exists := m.active[key]; exists || time.Now().Before(m.nextTry[key]) {
					continue
				}
				if !foundFirst {
					first, foundFirst = key, true
				}
				if !foundAfter && (!m.hasLastAttempt || compareSlots(key, m.lastAttempt) > 0) {
					after, foundAfter = key, true
					break findCandidate
				}
			}
		}
	}
	if !foundAfter {
		after, foundAfter = first, foundFirst
	}
	if foundAfter {
		if parent, live := m.opts.Pool.GetEntry(after.parent); live && m.eligible(parent) {
			m.lastAttempt, m.hasLastAttempt = after, true
			m.create(ctx, cfg, after, parent)
		}
	}
}

func compareSlots(a, b slot) int {
	if a.index != b.index {
		if a.index < b.index {
			return -1
		}
		return 1
	}
	if a.family != b.family {
		if a.family == warp.IPv4 {
			return -1
		}
		return 1
	}
	return slices.Compare(a.parent[:], b.parent[:])
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

func (m *Manager) create(ctx context.Context, cfg settings, key slot, parent *node.NodeEntry) {
	var managedSub *subscription.Subscription
	if m.opts.Subscriptions != nil {
		managedSub = m.opts.Subscriptions.Lookup(subscription.WarpSubscriptionID)
		if managedSub != nil {
			managedSub.LastCheckedNs.Store(time.Now().UnixNano())
		}
	}
	via := parent.Outbound.Load()
	if via == nil {
		return
	}
	parentName := m.opts.Pool.ResolveNodeDisplayTag(parent.Hash)
	if parentName == "" {
		parentName = (*via).Tag()
	}
	parentContext := fmt.Sprintf("parent=%s name=%q protocol=%s family=%s slot=%d", parent.Hash.Hex(), parentName, (*via).Type(), key.family, key.index+1)
	var best Candidate
	defer func() { closeCandidate(best) }()
	for attempt := 0; attempt < cfg.tries; attempt++ {
		if managedSub != nil {
			managedSub.LastCheckedNs.Store(time.Now().UnixNano())
		}
		attemptCtx, cancel := m.attemptContext(ctx, cfg)
		candidate, err := m.opts.Create(attemptCtx, *via, key.family)
		cancel()
		if err != nil {
			closeCandidate(candidate)
			if ctx.Err() != nil || m.currentSettings() != cfg {
				return
			}
			failure := m.parentFailures[parent.Hash]
			failure.consecutive++
			message := fmt.Sprintf("%s failures=%d/%d: %v", parentContext, failure.consecutive, parentFailureLimit, err)
			if failure.consecutive >= parentFailureLimit {
				failure.retryAt = time.Now().Add(parentFailureCooldown)
				message += fmt.Sprintf("; parent paused until %s", failure.retryAt.Format(time.RFC3339))
			}
			m.parentFailures[parent.Hash] = failure
			if managedSub != nil {
				managedSub.SetLastError(message)
			}
			m.opts.Logf("WARP %s", message)
			break // reconnect budget is for duplicate IPs, not broken transports
		}
		delete(m.parentFailures, parent.Hash)
		candidate.IP = candidate.IP.Unmap()
		if candidate.Outbound == nil || !candidate.IP.IsGlobalUnicast() ||
			(key.family == warp.IPv4 && !candidate.IP.Is4()) ||
			(key.family == warp.IPv6 && !candidate.IP.Is6()) || candidate.Latency <= 0 {
			closeCandidate(candidate)
			continue
		}
		_, duplicate, base := m.duplicate(candidate.IP)
		if !duplicate {
			closeCandidate(best)
			best = candidate
			break
		}
		m.opts.Logf("WARP duplicate exit=%s parent=%s family=%s attempt=%d/%d", candidate.IP, key.parent.Hex(), key.family, attempt+1, cfg.tries)
		if !base && (best.Outbound == nil || candidate.Latency < best.Latency) {
			closeCandidate(best)
			best = candidate
		} else {
			closeCandidate(candidate)
		}
	}
	m.nextTry[key] = time.Now().Add(time.Minute)
	if best.Outbound == nil || ctx.Err() != nil || m.currentSettings() != cfg {
		return
	}
	current, live := m.opts.Pool.GetEntry(key.parent)
	if !live || current != parent || !m.eligible(parent) || parent.Outbound.Load() != via {
		return
	}
	if other, duplicate, base := m.duplicate(best.IP); duplicate {
		m.nextTry[key] = time.Now().Add(5 * time.Minute)
		if base || m.latency(m.active[other].entry) <= best.Latency {
			return
		}
		m.remove(other)
		m.nextTry[other] = time.Now().Add(5 * time.Minute)
	}
	raw, _ := json.Marshal(struct {
		Type       string      `json:"type"`
		Parent     string      `json:"parent_hash"`
		Family     warp.Family `json:"family"`
		Slot       int         `json:"slot"`
		Generation uint64      `json:"generation"`
	}{"resin-warp", key.parent.Hex(), key.family, key.index + 1, exitGeneration.Add(1)})
	entry := node.NewNodeEntry(node.HashFromRawOptions(raw), raw, time.Now(), max(1, m.opts.MaxLatencyEntries))
	parentHash := key.parent
	entry.ParentHash, entry.WarpFamily = &parentHash, string(key.family)
	ob := best.Outbound
	entry.Outbound.Store(&ob)
	entry.SetEgressIP(best.IP)
	entry.LastEgressUpdate.Store(time.Now().UnixNano())
	entry.LatencyTable.Update("cloudflare.com", best.Latency, 10*time.Minute)
	if !m.opts.Pool.AddDerivedNode(entry, parent) {
		entry.LatencyTable.Close()
		return
	}
	m.active[key] = managedExit{entry, parent, via}
	if managedSub != nil {
		managedSub.SetLastError("")
		managedSub.LastUpdatedNs.Store(time.Now().UnixNano())
	}
	m.opts.Logf("WARP ready parent=%s family=%s slot=%d exit=%s latency=%s", key.parent.Hex(), key.family, key.index+1, best.IP, best.Latency)
	best = Candidate{} // pool removal callback now owns the outbound
	delete(m.nextTry, key)
}
