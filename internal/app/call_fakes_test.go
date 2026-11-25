package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// fakeConnectionID is the Telnyx connection the fake account reports. The test
// telephony profile must use the same value for validation to pass.
const fakeConnectionID = "conn-1"

// fakeCallerNumber is the number the fake account owns.
const fakeCallerNumber = "+14155550100"

// fakeTelnyx is a loopback stand-in for the Telnyx REST API. It serves the
// read-only validation calls and records every dial body. It never leaves the
// loopback interface.
type fakeTelnyx struct {
	srv       *httptest.Server
	controlID string

	mu      sync.Mutex
	dials   []map[string]any
	hangups []string // "control_id|command_id", in arrival order
}

// hangupsSeen returns a copy of every hangup the fake received.
func (f *fakeTelnyx) hangupsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hangups...)
}

// newFakeTelnyx starts the fake. The base URL it serves includes /v2, the
// production prefix, so the app's client paths resolve unchanged.
func newFakeTelnyx(t *testing.T, controlID string) *fakeTelnyx {
	t.Helper()
	f := &fakeTelnyx{controlID: controlID}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/call_control_applications/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]any{"data": map[string]any{"id": r.PathValue("id"), "active": true}})
	})
	mux.HandleFunc("GET /v2/phone_numbers", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, map[string]any{"data": []map[string]any{{
			"id": "pn-1", "phone_number": fakeCallerNumber, "status": "active", "connection_id": fakeConnectionID,
		}}})
	})
	mux.HandleFunc("POST /v2/calls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.dials = append(f.dials, body)
		f.mu.Unlock()
		writeFakeJSON(w, map[string]any{"data": map[string]any{
			"call_control_id": f.controlID, "call_leg_id": "leg-1", "call_session_id": "sess-1",
		}})
	})
	mux.HandleFunc("POST /v2/calls/{id}/actions/hangup", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.hangups = append(f.hangups, r.PathValue("id")+"|"+fmt.Sprint(body["command_id"]))
		f.mu.Unlock()
		writeFakeJSON(w, map[string]any{"data": map[string]any{}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// baseURL is the REST root the app's Telnyx client should use.
func (f *fakeTelnyx) baseURL() string { return f.srv.URL + "/v2" }

// waitDials blocks until the fake has received at least n dials, or fails the
// test. Dials happen on the worker, so the test waits for them.
func (f *fakeTelnyx) waitDials(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.dialBodies()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake Telnyx received %d dials, want %d", len(f.dialBodies()), n)
}

// dialBodies returns a copy of every dial body the fake received, oldest first.
func (f *fakeTelnyx) dialBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.dials...)
}

// fakeAI is a loopback realtime_ws or live_ws provider. It records, in arrival
// order, the session instructions it was configured with and every caller
// audio chunk. The order shows whether instructions came before any audio.
type fakeAI struct {
	srv      *httptest.Server
	protocol realtime.Protocol

	mu     sync.Mutex
	events []string
	// live is the open provider connection, so a test can drop it mid-call.
	live *websocket.Conn
}

// drop closes the provider's side of the open session, as if the AI service
// went away mid-call. It is a no-op when no session is open.
func (f *fakeAI) drop() {
	f.mu.Lock()
	c := f.live
	f.mu.Unlock()
	if c != nil {
		_ = c.CloseNow()
	}
}

// Event labels recorded by fakeAI.
const (
	aiEventInstructions = "instructions"
	aiEventAudio        = "audio"
)

// newFakeAI starts a loopback WebSocket provider that speaks the named
// protocol's session setup and audio messages.
func newFakeAI(t *testing.T, protocol realtime.Protocol) *fakeAI {
	t.Helper()
	f := &fakeAI{protocol: protocol}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// endpoint is the ws:// URL a profile must carry to reach this fake. Plaintext
// ws is allowed only for loopback, and the fake listens on loopback only.
func (f *fakeAI) endpoint() string {
	base := "ws" + strings.TrimPrefix(f.srv.URL, "http")
	if f.protocol == realtime.ProtocolLive {
		return base + "/v1/live/sessions"
	}
	return base + "/v1/realtime"
}

// snapshot returns the recorded events in arrival order.
func (f *fakeAI) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeAI) record(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeAI) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	f.mu.Lock()
	f.live = c
	f.mu.Unlock()
	// Record when the client's session ends, so a test can see the AI side
	// close, for example at the max-duration cap.
	defer f.record("closed")
	ctx := r.Context()
	if f.protocol == realtime.ProtocolRealtime {
		// The realtime protocol is server-first: it announces the session
		// before the client configures it.
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","event_id":"evt_created","session":{"id":"sess_fake","type":"realtime"}}`))
	}
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return
		}
		f.handle(ctx, c, m)
	}
}

// handle answers one client event. Setup events echo the session so the
// adapter's format check passes. Audio events are recorded only.
func (f *fakeAI) handle(ctx context.Context, c *websocket.Conn, m map[string]any) {
	switch m["type"] {
	case "session.update": // realtime_ws setup
		sess, _ := m["session"].(map[string]any)
		f.recordInstructions(sess)
		out, _ := json.Marshal(map[string]any{"type": "session.updated", "event_id": "evt_upd", "session": sess})
		_ = c.Write(ctx, websocket.MessageText, out)
	case "session.start": // live_ws setup
		sess, _ := m["session"].(map[string]any)
		f.recordInstructions(sess)
		sess["id"], sess["status"] = "live_fake", "active"
		out, _ := json.Marshal(map[string]any{"type": "session.started", "event_id": "evt_s", "client_event_id": "ch_session_start", "session": sess})
		_ = c.Write(ctx, websocket.MessageText, out)
	case "input_audio_buffer.append", "session.input_audio.append":
		f.record(aiEventAudio)
	}
}

func (f *fakeAI) recordInstructions(sess map[string]any) {
	text, _ := sess["instructions"].(string)
	f.record(aiEventInstructions + ":" + text)
}

// writeFakeJSON writes v as a 200 JSON response.
func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
