package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
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
	// hangupKeys holds the bearer key each hangup was sent with, in the same
	// order as hangups. A grantee's leg must be hung up with the owner's key.
	hangupKeys []string
	// badDialBody makes POST /v2/calls answer 200 with a body that has no
	// call_control_id. The dial was accepted but the response cannot confirm it.
	badDialBody bool
	// dialGate, when set, holds POST /v2/calls until it is closed, so a test can
	// prove make_call answers before the provider has even answered the dial.
	dialGate chan struct{}
	// unexpected holds "METHOD path" for every request outside the handled
	// Telnyx routes. Such a request means the app reached for a Telnyx feature
	// CallHarbor does not implement (#114 telephony-only boundary).
	unexpected []string
	// statusReads holds "control_id|key" for each GET /v2/calls/{id}, in order.
	statusReads []string
	// alive scripts is_alive per control id. An unscripted id reports alive.
	alive map[string]bool
	// gone marks control ids whose status read answers 404.
	gone map[string]bool
}

// setCallAlive scripts the is_alive answer for one control id.
func (f *fakeTelnyx) setCallAlive(controlID string, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alive == nil {
		f.alive = map[string]bool{}
	}
	f.alive[controlID] = alive
}

// setCallGone makes the status read for one control id answer 404.
func (f *fakeTelnyx) setCallGone(controlID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[controlID] = true
}

// statusReadsSeen returns a copy of every status read as "control_id|key".
func (f *fakeTelnyx) statusReadsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusReads...)
}

// unexpectedRequests returns a copy of every unexpected "METHOD path" request.
func (f *fakeTelnyx) unexpectedRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unexpected...)
}

// setBadDialBody switches the dial response to a 2xx with no control id.
func (f *fakeTelnyx) setBadDialBody(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.badDialBody = v
}

// holdDials makes the next dials wait until the returned release func runs.
// The release is idempotent, so a test may call it early and again in cleanup.
func (f *fakeTelnyx) holdDials() func() {
	f.mu.Lock()
	gate := make(chan struct{})
	f.dialGate = gate
	f.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// hangupsSeen returns a copy of every hangup the fake received.
func (f *fakeTelnyx) hangupsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hangups...)
}

// hangupKeysSeen returns the bearer key of each hangup, in arrival order.
func (f *fakeTelnyx) hangupKeysSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hangupKeys...)
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
		bad := f.badDialBody
		gate := f.dialGate
		f.mu.Unlock()
		// The dial is recorded before the gate, so a test can see it was sent
		// while the provider has not yet answered.
		if gate != nil {
			<-gate
		}
		if bad {
			writeFakeJSON(w, map[string]any{"data": map[string]any{"call_leg_id": "leg-1"}})
			return
		}
		writeFakeJSON(w, map[string]any{"data": map[string]any{
			"call_control_id": f.controlID, "call_leg_id": "leg-1", "call_session_id": "sess-1",
		}})
	})
	// GET /v2/calls/{id} is the status read reconciliation uses. Its answer is
	// scripted per control id: alive by default, or 404 when marked gone. Each
	// read records "control_id|key", so a test can check the owner's key.
	mux.HandleFunc("GET /v2/calls/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.statusReads = append(f.statusReads, id+"|"+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		gone := f.gone[id]
		alive, scripted := f.alive[id]
		f.mu.Unlock()
		if gone {
			http.Error(w, `{"errors":[{"code":"10005","title":"Resource not found"}]}`, http.StatusNotFound)
			return
		}
		if !scripted {
			alive = true
		}
		writeFakeJSON(w, map[string]any{"data": map[string]any{"call_control_id": id, "is_alive": alive}})
	})
	mux.HandleFunc("POST /v2/calls/{id}/actions/hangup", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.hangups = append(f.hangups, r.PathValue("id")+"|"+fmt.Sprint(body["command_id"]))
		f.hangupKeys = append(f.hangupKeys, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		writeFakeJSON(w, map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.unexpected = append(f.unexpected, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		http.NotFound(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	// Cleanups run last-in first-out, so this runs before srv.Close. Any request
	// to a Telnyx route the fake does not implement fails every test that made it.
	t.Cleanup(func() {
		if u := f.unexpectedRequests(); len(u) != 0 {
			t.Errorf("unexpected Telnyx requests (telephony-only boundary, #114): %v", u)
		}
	})
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

// dialBodyAllowlist is the set of keys a dial body may carry, computed from the
// json tags of telnyx.DialRequest so it cannot drift from production. The
// telnyx package's boundary test pins DialRequest to its own explicit allowlist.
var dialBodyAllowlist = dialRequestKeys()

func dialRequestKeys() map[string]bool {
	rt := reflect.TypeOf(telnyx.DialRequest{})
	out := make(map[string]bool, rt.NumField())
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// dialBodyForbiddenKeys are pinned CallRequest add-on keys (AMD, transcription,
// recording, assistants, deepfake detection and similar). None may be sent.
var dialBodyForbiddenKeys = []string{
	"answering_machine_detection", "answering_machine_detection_config",
	"assistant", "transcription", "transcription_config", "record",
	"record_channels", "record_format", "record_max_length", "record_timeout_secs",
	"record_track", "record_trim", "record_custom_file_name",
	"conversation_relay_config", "dialogflow_config", "enable_dialogflow",
	"deepfake_detection", "sound_modifications", "audio_url", "media_name",
	"send_silence_when_idle",
}

// assertDialBodiesTelephonyOnly fails for any dial body with a key outside the
// allowlist or any forbidden add-on key.
func assertDialBodiesTelephonyOnly(t *testing.T, bodies []map[string]any) {
	t.Helper()
	for i, body := range bodies {
		for k := range body {
			if !dialBodyAllowlist[k] {
				t.Errorf("dial %d key %q is outside the telephony-only allowlist", i, k)
			}
		}
		for _, bad := range dialBodyForbiddenKeys {
			if _, ok := body[bad]; ok {
				t.Errorf("dial %d carries forbidden add-on key %q", i, bad)
			}
		}
	}
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
	// transcriptionRequested is set when a session setup message carries a
	// "transcription" key anywhere (billed caller transcription, #114).
	transcriptionRequested bool
	// oobRequests holds every out-of-band response.create the app sent, in
	// arrival order (realtime_ws caller transcription, #113).
	oobRequests []map[string]any
	// toolOutputs holds every function_call_output item the app sent back for
	// a task report, in arrival order (the ack, #64).
	toolOutputs []map[string]any
	// plainResponses counts response.create messages that are not out-of-band.
	// A task report must never make the model speak, so this stays zero for
	// report cases.
	plainResponses int
}

// outOfBandRequests returns the out-of-band response.create messages recorded.
func (f *fakeAI) outOfBandRequests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.oobRequests...)
}

// toolOutputsSeen returns the function_call_output items the app sent, in order.
func (f *fakeAI) toolOutputsSeen() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.toolOutputs...)
}

// plainResponseCount returns how many non-out-of-band response.create messages
// the app sent.
func (f *fakeAI) plainResponseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plainResponses
}

// hasKey reports whether key appears as an object key anywhere in v.
func hasKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x[key]; ok {
			return true
		}
		for _, c := range x {
			if hasKey(c, key) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if hasKey(c, key) {
				return true
			}
		}
	}
	return false
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

// sawTranscriptionRequest reports whether any session setup asked for billed
// transcription.
func (f *fakeAI) sawTranscriptionRequest() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transcriptionRequested
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
	if (m["type"] == "session.update" || m["type"] == "session.start") && hasKey(m, "transcription") {
		f.mu.Lock()
		f.transcriptionRequested = true
		f.mu.Unlock()
	}
	if m["type"] == "response.create" {
		resp, _ := m["response"].(map[string]any)
		f.mu.Lock()
		if resp != nil && resp["conversation"] == "none" {
			f.oobRequests = append(f.oobRequests, m)
		} else {
			f.plainResponses++
		}
		f.mu.Unlock()
	}
	if m["type"] == "conversation.item.create" {
		if item, _ := m["item"].(map[string]any); item != nil && item["type"] == "function_call_output" {
			f.mu.Lock()
			f.toolOutputs = append(f.toolOutputs, item)
			f.mu.Unlock()
		}
	}
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
	case "session.close": // live_ws close handshake: acknowledge, then the caller drops
		// The app sends session.close on a cap or a stop and waits for this
		// acknowledgement before dropping the socket. A fake that never answers
		// makes each close wait the full graceful timeout.
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_closed","client_event_id":"ch_close"}`))
	}
}

// sendRaw pushes one provider server message down the open session.
func (f *fakeAI) sendRaw(t *testing.T, msg map[string]any) {
	t.Helper()
	f.mu.Lock()
	c := f.live
	f.mu.Unlock()
	if c == nil {
		t.Fatal("no provider session open")
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatalf("send provider event: %v", err)
	}
}

// sendAgentAudio pushes one agent audio chunk down the open session, in the
// active protocol's output-audio event. The chunk is base64 PCMU.
func (f *fakeAI) sendAgentAudio(t *testing.T, payload string) {
	t.Helper()
	f.mu.Lock()
	c := f.live
	f.mu.Unlock()
	if c == nil {
		t.Fatal("no provider session open to send agent audio")
	}
	var msg map[string]any
	if f.protocol == realtime.ProtocolLive {
		msg = map[string]any{"type": "session.output_audio.delta", "event_id": "evt_out",
			"delta": payload, "start_ms": 0, "end_ms": 20}
	} else {
		msg = map[string]any{"type": "response.output_audio.delta", "event_id": "evt_out",
			"delta": payload, "item_id": "item_1", "response_id": "resp_1"}
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatalf("send agent audio: %v", err)
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
