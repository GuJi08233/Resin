package topology

import (
	"log"
	"net/netip"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/platform"
)

// PlatformGuard owns runtime platform-scoped egress exclusions (status-code
// cooldowns and permanent blacklists). It coalesces view rebuilds so a burst of
// matching responses does not trigger a full platform rebuild per response.
type PlatformGuard struct {
	pool  *GlobalNodePool
	store *platform.GuardStore

	persist       func(platform.Exclusion) error
	deletePersist func(platformID, egressIP string) error

	dirtyMu sync.Mutex
	dirty   map[string]struct{}
	wakeCh  chan struct{}

	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	persistWG sync.WaitGroup
}

// NewPlatformGuard creates a guard that rebuilds views through pool.
func NewPlatformGuard(pool *GlobalNodePool) *PlatformGuard {
	return &PlatformGuard{
		pool:   pool,
		store:  platform.NewGuardStore(),
		dirty:  make(map[string]struct{}),
		wakeCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
}

// SetPersisters wires optional persistence callbacks. Both are called from
// background goroutines; errors are logged and do not block routing.
func (g *PlatformGuard) SetPersisters(
	persist func(platform.Exclusion) error,
	deletePersist func(platformID, egressIP string) error,
) {
	g.persist = persist
	g.deletePersist = deletePersist
}

// Load restores persisted exclusions. Expired cooldowns are deleted from
// persistence and not retained in memory.
func (g *PlatformGuard) Load(entries []platform.Exclusion) {
	loaded, expired := g.store.Load(entries, time.Now())
	for _, entry := range expired {
		g.deletePersisted(entry.PlatformID, entry.EgressIP.String())
	}
	if len(loaded) > 0 {
		log.Printf("Loaded %d platform egress exclusions", len(loaded))
	}
}

// Excluded reports whether an egress IP is currently excluded for a platform.
func (g *PlatformGuard) Excluded(platformID string, ip netip.Addr, now time.Time) bool {
	return g.store.Excluded(platformID, ip, now)
}

// RecordMatch applies a status code rule match. It returns true when state
// changed; the platform view is rebuilt asynchronously.
func (g *PlatformGuard) RecordMatch(
	platformID string,
	ip netip.Addr,
	rule platform.StatusCodeRule,
	statusCode int,
	nodeHash string,
) bool {
	entry, changed := g.store.Apply(platformID, ip, rule, statusCode, nodeHash, time.Now())
	if !changed {
		return false
	}
	g.markDirty(platformID)
	if g.persist != nil {
		g.persistWG.Add(1)
		go func() {
			defer g.persistWG.Done()
			if err := g.persist(entry); err != nil {
				log.Printf("[platform-guard] persist exclusion %s/%s: %v", entry.PlatformID, entry.EgressIP, err)
			}
		}()
	}
	return true
}

// Unblock removes one exclusion and schedules a view rebuild.
func (g *PlatformGuard) Unblock(platformID, egressIP string) (platform.Exclusion, bool) {
	ip, err := netip.ParseAddr(egressIP)
	if err != nil {
		return platform.Exclusion{}, false
	}
	entry, ok := g.store.Unblock(platformID, ip)
	if ok {
		g.markDirty(platformID)
	}
	return entry, ok
}

// List returns a platform's current exclusions.
func (g *PlatformGuard) List(platformID string) []platform.Exclusion {
	return g.store.List(platformID)
}

// ClearPlatform drops all state for a deleted platform without scheduling a rebuild.
func (g *PlatformGuard) ClearPlatform(platformID string) {
	g.store.ClearPlatform(platformID)
	g.dirtyMu.Lock()
	delete(g.dirty, platformID)
	g.dirtyMu.Unlock()
}

// Start launches the coalescing rebuild and expiry worker.
func (g *PlatformGuard) Start() {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.run()
	}()
}

// Stop signals the worker, waits for it, and drains pending persistence writes.
func (g *PlatformGuard) Stop() {
	g.stopOnce.Do(func() { close(g.stopCh) })
	g.wg.Wait()
	g.persistWG.Wait()
}

func (g *PlatformGuard) run() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-g.wakeCh:
			// Coalesce bursts of matching responses into one rebuild.
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-g.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			g.rebuildDirty()
		case <-ticker.C:
			g.expire(time.Now())
			g.rebuildDirty()
		}
	}
}

func (g *PlatformGuard) expire(now time.Time) {
	for _, entry := range g.store.Expire(now) {
		g.deletePersisted(entry.PlatformID, entry.EgressIP.String())
		g.markDirty(entry.PlatformID)
	}
}

func (g *PlatformGuard) deletePersisted(platformID, egressIP string) {
	if g.deletePersist == nil {
		return
	}
	g.persistWG.Add(1)
	go func() {
		defer g.persistWG.Done()
		if err := g.deletePersist(platformID, egressIP); err != nil {
			log.Printf("[platform-guard] delete exclusion %s/%s: %v", platformID, egressIP, err)
		}
	}()
}

func (g *PlatformGuard) markDirty(platformID string) {
	if platformID == "" {
		return
	}
	g.dirtyMu.Lock()
	g.dirty[platformID] = struct{}{}
	g.dirtyMu.Unlock()

	select {
	case g.wakeCh <- struct{}{}:
	default:
	}
}

func (g *PlatformGuard) rebuildDirty() {
	g.dirtyMu.Lock()
	if len(g.dirty) == 0 {
		g.dirtyMu.Unlock()
		return
	}
	platformIDs := make([]string, 0, len(g.dirty))
	for platformID := range g.dirty {
		platformIDs = append(platformIDs, platformID)
	}
	g.dirty = make(map[string]struct{})
	g.dirtyMu.Unlock()

	for _, platformID := range platformIDs {
		plat, ok := g.pool.GetPlatform(platformID)
		if !ok || plat == nil {
			continue
		}
		g.pool.RebuildPlatform(plat)
	}
}
