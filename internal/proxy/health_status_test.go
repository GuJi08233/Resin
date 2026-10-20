package proxy

import (
	"net/netip"
	"testing"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/routing"
)

type upstreamStatusCall struct {
	platformID string
	hash       node.Hash
	egressIP   netip.Addr
	statusCode int
}

type mockUpstreamStatusRecorder struct {
	mockHealthRecorder
	hasRules bool
	calls    chan upstreamStatusCall
}

func (m *mockUpstreamStatusRecorder) HasUpstreamStatusRules(platformID string) bool {
	return m.hasRules && platformID != ""
}

func (m *mockUpstreamStatusRecorder) RecordUpstreamStatus(
	platformID string,
	hash node.Hash,
	egressIP netip.Addr,
	statusCode int,
) {
	m.calls <- upstreamStatusCall{
		platformID: platformID,
		hash:       hash,
		egressIP:   egressIP,
		statusCode: statusCode,
	}
}

func TestRecordUpstreamStatus_CommitsWithoutGlobalHealth(t *testing.T) {
	rec := &mockUpstreamStatusRecorder{hasRules: true, calls: make(chan upstreamStatusCall, 1)}
	h := node.HashFromRawOptions([]byte(`{"type":"ss","n":"status"}`))
	ip := netip.MustParseAddr("203.0.113.40")

	recordUpstreamStatus(rec, routing.RouteResult{
		PlatformID: "plat-1",
		NodeHash:   h,
		EgressIP:   ip,
	}, 429)

	select {
	case call := <-rec.calls:
		if call.platformID != "plat-1" || call.hash != h || call.egressIP != ip || call.statusCode != 429 {
			t.Fatalf("unexpected status call: %+v", call)
		}
	default:
		t.Fatal("upstream status must be recorded before returning")
	}

	if rec.resultCalls.Load() != 0 {
		t.Fatalf("global RecordResult calls = %d, want 0", rec.resultCalls.Load())
	}
}

func TestRecordUpstreamStatus_SkipsMissingIdentity(t *testing.T) {
	rec := &mockUpstreamStatusRecorder{hasRules: true, calls: make(chan upstreamStatusCall, 1)}
	h := node.HashFromRawOptions([]byte(`{"type":"ss","n":"status-skip"}`))

	recordUpstreamStatus(rec, routing.RouteResult{NodeHash: h}, 403)
	recordUpstreamStatus(rec, routing.RouteResult{
		PlatformID: "plat-1",
		NodeHash:   h,
		EgressIP:   netip.Addr{},
	}, 403)

	select {
	case call := <-rec.calls:
		t.Fatalf("unexpected status call: %+v", call)
	default:
	}
}

func TestRecordUpstreamStatus_SkipsPlatformWithoutRules(t *testing.T) {
	rec := &mockUpstreamStatusRecorder{hasRules: false, calls: make(chan upstreamStatusCall, 1)}
	h := node.HashFromRawOptions([]byte(`{"type":"ss","n":"status-no-rules"}`))

	recordUpstreamStatus(rec, routing.RouteResult{
		PlatformID: "plat-1",
		NodeHash:   h,
		EgressIP:   netip.MustParseAddr("203.0.113.42"),
	}, 429)

	select {
	case call := <-rec.calls:
		t.Fatalf("unexpected status call for rule-less platform: %+v", call)
	default:
	}
}

func TestRecordUpstreamStatus_RecorderWithoutInterface(t *testing.T) {
	rec := &mockHealthRecorder{}
	h := node.HashFromRawOptions([]byte(`{"type":"ss","n":"status-plain"}`))

	// Must not panic or fall back to global health recording.
	recordUpstreamStatus(rec, routing.RouteResult{
		PlatformID: "plat-1",
		NodeHash:   h,
		EgressIP:   netip.MustParseAddr("203.0.113.41"),
	}, 403)

	if rec.resultCalls.Load() != 0 {
		t.Fatalf("global RecordResult calls = %d, want 0", rec.resultCalls.Load())
	}
}
