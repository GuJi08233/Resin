package subscription

import "time"

// A factory attempt has a 60-second deadline. Allow scheduling and cleanup
// headroom without hiding a blocked maintenance worker for hours.
const MaintenanceStallThreshold = 90 * time.Second

// RecordMaintenance records actual worker progress. For a managed subscription,
// a check means reconciling the current node pool, including when it is full.
func (s *Subscription) RecordMaintenance(now time.Time) {
	s.LastCheckedNs.Store(now.UnixNano())
	s.LastMaintenanceNs.Store(now.UnixNano())
}

func (s *Subscription) MaintenanceStatus(enabled bool, now time.Time) string {
	if !enabled {
		return "disabled"
	}
	last := s.LastMaintenanceNs.Load()
	if last == 0 {
		if now.Sub(time.Unix(0, s.CreatedAtNs)) > MaintenanceStallThreshold {
			return "stalled"
		}
		return "starting"
	}
	if now.Sub(time.Unix(0, last)) > MaintenanceStallThreshold {
		return "stalled"
	}
	return "maintaining"
}
