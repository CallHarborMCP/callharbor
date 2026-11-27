package transcript

import (
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// assessmentEvents returns the stored assessment.reported events in order.
func assessmentEvents(h *harness) []storage.CallEvent {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	var out []storage.CallEvent
	for _, e := range h.store.events {
		if e.Kind == storage.AssessmentReportedKind {
			out = append(out, e)
		}
	}
	return out
}

// A valid report is stored as its canonical validated payload, correlated to
// the function call id and attributed to the realtime protocol.
func TestTaskReportRecordsCanonicalValidatedPayload(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	args := `{"outcome":"failed","reason":"callee_refused","criteria":[],"constraints":[]}`
	h.rec.Observe(realtime.Event{Kind: realtime.EventTaskReport, FunctionCallID: "call_1", Arguments: args})
	h.close(t)

	evs := assessmentEvents(h)
	if len(evs) != 1 {
		t.Fatalf("assessment events = %d, want 1 (kinds %v)", len(evs), h.store.eventKinds())
	}
	e := evs[0]
	if e.Provider != "realtime_ws" || e.CorrelationID != "call_1" || e.CallID != "call_1" || e.OwnerUserID != "alice" {
		t.Fatalf("event attribution = %+v", e)
	}
	if e.PayloadJSON != `{"outcome":"failed","reason":"callee_refused","criteria":[],"constraints":[],"evidence_quotes":[]}` {
		t.Fatalf("payload = %s, want the canonical validated report", e.PayloadJSON)
	}
}

// An invalid report stores only the invalid marker. Its raw arguments, which
// may carry quotes or transcript text, never reach any stored event.
func TestTaskReportInvalidStoresMarkerNeverRawArguments(t *testing.T) {
	const marker = "RAW-ARGUMENT-MARKER"
	h := newHarness(t, realtime.RealtimeCapabilities)
	// Unknown field makes it invalid; the marker must not survive into storage.
	bad := `{"outcome":"success","reason":"confirmed","criteria":[],"constraints":[],"quote":"` + marker + `"}`
	h.rec.Observe(realtime.Event{Kind: realtime.EventTaskReport, FunctionCallID: "call_2", Arguments: bad})
	h.rec.Observe(realtime.Event{Kind: realtime.EventTaskReport, FunctionCallID: "call_3", ReportInvalid: true})
	h.close(t)

	evs := assessmentEvents(h)
	if len(evs) != 2 {
		t.Fatalf("assessment events = %d, want 2", len(evs))
	}
	for _, e := range evs {
		if e.PayloadJSON != outcome.InvalidPayload {
			t.Fatalf("payload = %s, want the invalid marker", e.PayloadJSON)
		}
	}
	for _, e := range h.store.events {
		if strings.Contains(e.PayloadJSON, marker) || strings.Contains(e.ReasonCode, marker) || strings.Contains(e.CorrelationID, marker) {
			t.Fatalf("raw report content stored in %s: %+v", e.Kind, e)
		}
	}
}
