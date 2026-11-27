package platform

import (
	"fmt"

	"github.com/Resinat/Resin/internal/model"
)

// LatencyFilter constrains platform routability by the node's reference
// latency (average EWMA across configured authority domains, in milliseconds).
// A zero filter (both bounds nil) is inactive. When active, nodes without a
// reference latency sample never match, mirroring how region and IP profile
// filters treat unknown values.
type LatencyFilter struct {
	MinMs *int
	MaxMs *int
}

// Active reports whether the filter constrains anything.
func (f LatencyFilter) Active() bool {
	return f.MinMs != nil || f.MaxMs != nil
}

// Match reports whether the node's reference latency satisfies the filter.
func (f LatencyFilter) Match(latencyMs float64, ok bool) bool {
	if !f.Active() {
		return true
	}
	if !ok {
		return false
	}
	if f.MinMs != nil && latencyMs < float64(*f.MinMs) {
		return false
	}
	if f.MaxMs != nil && latencyMs > float64(*f.MaxMs) {
		return false
	}
	return true
}

// NormalizeLatencyFilter validates and clones a persisted latency filter.
func NormalizeLatencyFilter(raw model.PlatformLatencyFilter) (model.PlatformLatencyFilter, error) {
	var out model.PlatformLatencyFilter

	if raw.MinMs != nil {
		v := *raw.MinMs
		if v < 0 {
			return model.PlatformLatencyFilter{}, fmt.Errorf("latency_filters.min_ms: must be >= 0")
		}
		out.MinMs = &v
	}
	if raw.MaxMs != nil {
		v := *raw.MaxMs
		if v < 0 {
			return model.PlatformLatencyFilter{}, fmt.Errorf("latency_filters.max_ms: must be >= 0")
		}
		out.MaxMs = &v
	}
	if out.MinMs != nil && out.MaxMs != nil && *out.MinMs > *out.MaxMs {
		return model.PlatformLatencyFilter{}, fmt.Errorf("latency_filters: min_ms must be <= max_ms")
	}

	return out, nil
}

// CompileLatencyFilter builds the runtime filter from a persisted filter.
func CompileLatencyFilter(raw model.PlatformLatencyFilter) (LatencyFilter, error) {
	normalized, err := NormalizeLatencyFilter(raw)
	if err != nil {
		return LatencyFilter{}, err
	}
	return LatencyFilter{
		MinMs: normalized.MinMs,
		MaxMs: normalized.MaxMs,
	}, nil
}
