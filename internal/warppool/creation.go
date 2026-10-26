package warppool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/warp"
	"github.com/sagernet/sing-box/adapter"
)

type creationJob struct {
	key           slot
	parent        *node.NodeEntry
	via           *adapter.Outbound
	cfg           settings
	ctx           context.Context
	cancel        func()
	previousVisit bool
}

type creationResult struct {
	job       *creationJob
	candidate Candidate
	err       error
}

func (m *Manager) dispatch(ctx context.Context, cfg settings, key slot, parent *node.NodeEntry) {
	via := parent.Outbound.Load()
	if via == nil || *via == nil {
		return
	}
	attemptCtx, cancel := m.attemptContext(ctx, cfg)
	job := &creationJob{key: key, parent: parent, via: via, cfg: cfg, ctx: attemptCtx, cancel: cancel, previousVisit: m.visited[key.parent]}
	m.inflight[key] = job
	m.visited[key.parent] = true
	m.lastAttempt, m.hasLastAttempt = key, true
	if m.lastFamily == key.family {
		m.familyStreak++
	} else {
		m.lastFamily, m.familyStreak = key.family, 1
	}
	m.sequence++
	m.lastTried[parentFamily{key.parent, key.family}] = m.sequence
	m.recordProgress()
	run := func() creationResult {
		candidate, err := m.opts.Create(attemptCtx, *via, key.family)
		if err == nil {
			err = attemptCtx.Err()
		}
		return creationResult{job, candidate, err}
	}
	if m.opts.Concurrency == 1 {
		m.completeAttempt(ctx, run())
		return
	}
	m.jobs.Add(1)
	go func() {
		defer m.jobs.Done()
		// The bounded channel owns every returned candidate, including during
		// shutdown. The actor either publishes it or closes it exactly once.
		m.results <- run()
	}()
}

func (m *Manager) drainResults(ctx context.Context) {
	for {
		select {
		case result := <-m.results:
			m.completeAttempt(ctx, result)
		default:
			return
		}
	}
}

func (m *Manager) shutdown() {
	for _, job := range m.inflight {
		job.cancel()
	}
	m.jobs.Wait()
	for {
		select {
		case result := <-m.results:
			result.job.cancel()
			closeCandidate(result.candidate)
		default:
			clear(m.inflight)
			m.clear()
			return
		}
	}
}

func (m *Manager) completeAttempt(ctx context.Context, result creationResult) {
	job, candidate := result.job, result.candidate
	defer func() { closeCandidate(candidate) }()
	if current := m.inflight[job.key]; current != job {
		job.cancel()
		return
	}
	delete(m.inflight, job.key)
	attemptErr := job.ctx.Err()
	job.cancel()
	m.recordProgress()
	cfg := m.currentSettings()
	if ctx.Err() != nil || errors.Is(attemptErr, context.Canceled) || cfg != job.cfg || !cfg.enabled {
		return
	}
	parent, exists := m.opts.Pool.GetEntry(job.key.parent)
	if !exists || parent != job.parent || !m.eligible(parent) || parent.Outbound.Load() != job.via {
		return
	}
	key := job.key
	failureKey := parentFamily{key.parent, key.family}
	parentName := m.opts.Pool.ResolveNodeDisplayTag(key.parent)
	if parentName == "" {
		parentName = (*job.via).Tag()
	}
	parentContext := fmt.Sprintf("parent=%s name=%q protocol=%s family=%s slot=%d", key.parent.Hex(), parentName, (*job.via).Type(), key.family, key.index+1)
	if result.err != nil {
		if delay, limited := warp.RetryAfter(result.err); limited {
			retryAt := time.Now().Add(delay)
			if retryAt.After(m.rateRetryAt) {
				m.rateRetryAt = retryAt
			}
			m.nextTry[key] = m.rateRetryAt
			if !job.previousVisit {
				delete(m.visited, key.parent)
			}
			message := fmt.Sprintf("Cloudflare registration paused until %s: %v", m.rateRetryAt.Format(time.RFC3339), result.err)
			if sub := m.managedSubscription(); sub != nil {
				sub.SetLastError(message)
			}
			m.opts.Logf("WARP %s", message)
			return
		}
		failure := m.parentFailures[failureKey]
		failure.consecutive++
		message := fmt.Sprintf("%s failures=%d/%d: %v", parentContext, failure.consecutive, parentFailureLimit, result.err)
		if failure.consecutive >= parentFailureLimit {
			failure.retryAt = time.Now().Add(parentFailureCooldown)
			message += fmt.Sprintf("; parent family paused until %s", failure.retryAt.Format(time.RFC3339))
		}
		m.parentFailures[failureKey] = failure
		m.nextTry[key] = time.Now().Add(time.Minute)
		if sub := m.managedSubscription(); sub != nil && !time.Now().Before(m.rateRetryAt) {
			sub.SetLastError(message)
		}
		m.opts.Logf("WARP %s", message)
		return
	}
	if attemptErr != nil {
		return
	}
	candidate.IP = candidate.IP.Unmap()
	if candidate.Outbound == nil || !candidate.IP.IsGlobalUnicast() ||
		(key.family == warp.IPv4 && !candidate.IP.Is4()) || (key.family == warp.IPv6 && !candidate.IP.Is6()) || candidate.Latency <= 0 {
		m.nextTry[key] = time.Now().Add(time.Minute)
		return
	}
	delete(m.parentFailures, failureKey)
	// A verified duplicate still proves this parent can carry WARP. Persist
	// that path's success even if another lower-latency route keeps the IP.
	m.history.Record(key.parent, key.family, candidate.Latency, time.Now())
	if other, duplicate, base := m.duplicate(candidate.IP); duplicate {
		m.duplicateTries[failureKey]++
		tries := m.duplicateTries[failureKey]
		retryAt := time.Now().Add(time.Minute)
		if tries >= cfg.tries {
			retryAt = time.Now().Add(5 * time.Minute)
			delete(m.duplicateTries, failureKey)
		}
		m.nextTry[key] = retryAt
		m.opts.Logf("WARP duplicate exit=%s parent=%s family=%s attempt=%d/%d; retry deferred", candidate.IP, key.parent.Hex(), key.family, tries, cfg.tries)
		if base || m.latency(m.active[other].entry) <= candidate.Latency {
			return
		}
		m.remove(other)
		m.nextTry[other] = retryAt
	} else {
		delete(m.duplicateTries, failureKey)
	}
	if len(m.active) >= cfg.limit {
		victim, ok := m.balanceVictim(cfg, key.family)
		if !ok {
			return
		}
		m.remove(victim)
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
	ob := candidate.Outbound
	entry.Outbound.Store(&ob)
	entry.SetEgressIP(candidate.IP)
	entry.LastEgressUpdate.Store(time.Now().UnixNano())
	entry.LatencyTable.Update("cloudflare.com", candidate.Latency, 10*time.Minute)
	if !m.opts.Pool.AddDerivedNode(entry, parent) {
		entry.LatencyTable.Close()
		return
	}
	m.active[key] = managedExit{entry, parent, job.via}
	if sub := m.managedSubscription(); sub != nil {
		if !time.Now().Before(m.rateRetryAt) {
			sub.SetLastError("")
		}
		sub.LastUpdatedNs.Store(time.Now().UnixNano())
	}
	m.opts.Logf("WARP ready parent=%s family=%s slot=%d exit=%s latency=%s", key.parent.Hex(), key.family, key.index+1, candidate.IP, candidate.Latency)
	candidate = Candidate{}
	// Keep the cooldown when this publication merely replaced a duplicate.
	if !time.Now().Before(m.nextTry[key]) {
		delete(m.nextTry, key)
	}
}

// A full pool may replace excess-family exits only after a new, unique exit of
// the underrepresented family has passed verification. Never publish over cap.
func (m *Manager) balanceVictim(cfg settings, incoming warp.Family) (slot, bool) {
	if cfg.stack != "dual" {
		return slot{}, false
	}
	counts := m.familyCounts(false)
	target6 := (cfg.limit + 1) / 2
	target4 := cfg.limit / 2
	other := warp.IPv4
	if incoming == warp.IPv6 {
		if counts[warp.IPv6] >= target6 || counts[warp.IPv4] <= target4 {
			return slot{}, false
		}
	} else {
		other = warp.IPv6
		if counts[warp.IPv4] >= target4 || counts[warp.IPv6] <= target6 {
			return slot{}, false
		}
	}
	var slowest slot
	var latency time.Duration
	found := false
	for key, exit := range m.active {
		if key.family == other && (!found || m.latency(exit.entry) >= latency) {
			slowest, latency, found = key, m.latency(exit.entry), true
		}
	}
	return slowest, found
}
