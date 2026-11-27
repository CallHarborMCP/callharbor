package callworker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// TestDialSendsRingTimeoutFromInstallationPolicy covers the dial body. The
// planner's ring timeout must reach the dial as timeout_secs, and the wire body
// that Telnyx receives must carry it.
func TestDialSendsRingTimeoutFromInstallationPolicy(t *testing.T) {
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.RingTimeoutSeconds = 45
	p.Codec = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

	store := newFakeStore(call("c-ring"))
	dialer := &fakeDialer{result: telnyx.DialResult{CallControlID: "v3:ring", CallLegID: "leg", CallSessionID: "sess"}}
	p.Dialers = func(string) (Dialer, error) { return dialer, nil }
	w := newWorker(t, store, p, true)

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(dialer.dials) != 1 {
		t.Fatalf("dials = %d, want 1", len(dialer.dials))
	}
	got := dialer.dials[0]
	if got.TimeoutSecs != 45 {
		t.Fatalf("DialParams.TimeoutSecs = %d, want 45", got.TimeoutSecs)
	}

	// The body Telnyx receives is built from the same params. It must carry
	// timeout_secs with the configured value.
	req, err := telnyx.BuildDialRequest(got)
	if err != nil {
		t.Fatalf("build dial request: %v", err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if v, _ := wire["timeout_secs"].(float64); v != 45 {
		t.Fatalf("wire timeout_secs = %v, want 45: %s", wire["timeout_secs"], raw)
	}
}

// TestDialOmitsRingTimeoutWhenUnset covers a planner built without a ring
// timeout. The field is omitted from the wire body rather than sent as zero.
func TestDialOmitsRingTimeoutWhenUnset(t *testing.T) {
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.Codec = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

	store := newFakeStore(call("c-noring"))
	dialer := &fakeDialer{result: telnyx.DialResult{CallControlID: "v3:noring", CallLegID: "leg", CallSessionID: "sess"}}
	p.Dialers = func(string) (Dialer, error) { return dialer, nil }
	w := newWorker(t, store, p, true)

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(dialer.dials) != 1 {
		t.Fatalf("dials = %d, want 1", len(dialer.dials))
	}
	req, err := telnyx.BuildDialRequest(dialer.dials[0])
	if err != nil {
		t.Fatalf("build dial request: %v", err)
	}
	raw, _ := json.Marshal(req)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if _, present := wire["timeout_secs"]; present {
		t.Fatalf("timeout_secs present with no ring timeout set: %s", raw)
	}
}
