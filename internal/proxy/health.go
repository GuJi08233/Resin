package proxy

import (
	"net/netip"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
)

// HealthRecorder abstracts passive health feedback reporting.
// topology.GlobalNodePool satisfies this interface.
type HealthRecorder interface {
	RecordResult(hash node.Hash, success bool)
	RecordLatency(hash node.Hash, rawTarget string, latency *time.Duration)
}

type passiveHealthRecorder interface {
	RecordPassiveResult(platformID string, hash node.Hash, success bool)
}

// upstreamStatusRecorder receives upstream HTTP status codes for
// platform-scoped cooldown/blacklist rules. It must not affect global health.
type upstreamStatusRecorder interface {
	HasUpstreamStatusRules(platformID string) bool
	RecordUpstreamStatus(platformID string, hash node.Hash, egressIP netip.Addr, statusCode int)
}

func recordPassiveResultAsync(health HealthRecorder, route routing.RouteResult, success bool) {
	if health == nil {
		return
	}
	if recorder, ok := health.(passiveHealthRecorder); ok {
		go recorder.RecordPassiveResult(route.PlatformID, route.NodeHash, success)
		return
	}
	go health.RecordResult(route.NodeHash, success)
}

// Commit the in-memory exclusion before returning response headers to the
// client, so an immediate retry cannot race status-code feedback. Persistence
// and view rebuilds remain asynchronous in the recorder.
func recordUpstreamStatus(health HealthRecorder, route routing.RouteResult, statusCode int) {
	if health == nil || route.PlatformID == "" || !route.EgressIP.IsValid() {
		return
	}
	recorder, ok := health.(upstreamStatusRecorder)
	if !ok || !recorder.HasUpstreamStatusRules(route.PlatformID) {
		return
	}
	recorder.RecordUpstreamStatus(route.PlatformID, route.NodeHash, route.EgressIP, statusCode)
}
