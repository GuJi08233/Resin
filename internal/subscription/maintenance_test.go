package subscription

import (
	"testing"
	"time"
)

func TestMaintenanceStatusTracksWorkerProgress(t *testing.T) {
	now := time.Now()
	sub := NewWarpSubscription(true)
	sub.CreatedAtNs = now.UnixNano()
	if got := sub.MaintenanceStatus(true, now); got != "starting" {
		t.Fatalf("new worker status=%s", got)
	}
	if got := sub.MaintenanceStatus(true, now.Add(MaintenanceStallThreshold+time.Second)); got != "stalled" {
		t.Fatalf("worker that never starts status=%s", got)
	}
	sub.RecordMaintenance(now)
	for _, tc := range []struct {
		enabled bool
		elapsed time.Duration
		want    string
	}{
		{true, 60 * time.Second, "maintaining"},
		{true, MaintenanceStallThreshold + time.Second, "stalled"},
		{false, time.Hour, "disabled"},
	} {
		if got := sub.MaintenanceStatus(tc.enabled, now.Add(tc.elapsed)); got != tc.want {
			t.Fatalf("enabled=%v elapsed=%s: status=%s want=%s", tc.enabled, tc.elapsed, got, tc.want)
		}
	}
	if sub.LastCheckedNs.Load() != now.UnixNano() || sub.LastUpdatedNs.Load() != 0 {
		t.Fatal("maintenance did not record a check, or incorrectly marked node membership updated")
	}
	sub.RecordMaintenance(now.Add(time.Hour))
	if got := sub.MaintenanceStatus(true, now.Add(time.Hour)); got != "maintaining" {
		t.Fatalf("recovered worker status=%s", got)
	}
}
