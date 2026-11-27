package realtimews

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// oobEndpoint scripts an independent emulator's answers to out-of-band
// response.create requests. Its messages are written from the documented
// protocol and never built with the adapter's encoders.
type oobEndpoint struct {
	mu      sync.Mutex
	reqs    []map[string]any
	all     []map[string]any
	respond func(req map[string]any) []string
	// afterUpd is written after session.updated, as the endpoint's own events.
	afterUpd []string
	// push holds endpoint events to send on the live connection, in order.
	push chan string
	// writeMu serialises the emulator's writes on its connection.
	writeMu sync.Mutex
}

func (e *oobEndpoint) fake() *fakeRealtime {
	f := &fakeRealtime{afterUpd: e.afterUpd}
	e.push = make(chan string, 16)
	f.onClient = func(ctx context.Context, c *websocket.Conn, m map[string]any) {
		e.mu.Lock()
		e.all = append(e.all, m)
		e.mu.Unlock()
		if m["type"] != "response.create" {
			return
		}
		e.mu.Lock()
		e.reqs = append(e.reqs, m)
		e.mu.Unlock()
		if e.respond == nil {
			return
		}
		for _, msg := range e.respond(m) {
			e.writeMu.Lock()
			_ = c.Write(ctx, websocket.MessageText, []byte(msg))
			e.writeMu.Unlock()
		}
	}
	f.onIdle = func(ctx context.Context, c *websocket.Conn) {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-e.push:
				e.writeMu.Lock()
				_ = c.Write(ctx, websocket.MessageText, []byte(msg))
				e.writeMu.Unlock()
			}
		}
	}
	return f
}

// sendNext queues one endpoint event for the live connection.
func (e *oobEndpoint) sendNext(msg string) { e.push <- msg }

// allClient returns every client message the endpoint has seen on its socket.
func (e *oobEndpoint) allClient() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.all...)
}

func (e *oobEndpoint) requests() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]any(nil), e.reqs...)
}

// callerItemAdded is the endpoint's conversation.item.added for a completed
// caller audio message.
const callerItemAdded = `{"type":"conversation.item.added","event_id":"ev_item","item":{"id":"item_c1","type":"message","status":"completed","role":"user","content":[{"type":"input_audio"}]}}`

func metaOf(req map[string]any) map[string]any {
	return req["response"].(map[string]any)["metadata"].(map[string]any)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func responseCreated(respID string, meta map[string]any) string {
	return mustJSON(map[string]any{"type": "response.created", "event_id": "ev_rc_" + respID, "response": map[string]any{"id": respID, "status": "in_progress", "metadata": meta, "output": []any{}}})
}

func connectOOB(t *testing.T, e *oobEndpoint, now func() time.Time) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := cfg(startFake(t, e.fake()))
	c.Now = now
	s, err := Connect(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// collect reads normalized events until stop reports true or the deadline ends.
func collect(t *testing.T, s *Session, stop func([]realtime.Event) bool) []realtime.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []realtime.Event
	for !stop(out) {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("next after %d events: %v (events %+v)", len(out), err, out)
		}
		out = append(out, ev)
	}
	return out
}

// The request for a caller audio item has the documented shape: conversation
// none, text only, an item reference, metadata that names only the purpose and
// item, a bounded token limit, and never a transcription key.
func TestOutOfBandRequestBuiltFromCallerItem(t *testing.T) {
	st := newOOBState(time.Now)
	b, err := st.request("item_c1")
	if err != nil || b == nil {
		t.Fatalf("request = %s %v", b, err)
	}
	if strings.Contains(string(b), "transcription") {
		t.Fatalf("out-of-band request enables billed transcription: %s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	r := m["response"].(map[string]any)
	in := r["input"].([]any)[0].(map[string]any)
	meta := r["metadata"].(map[string]any)
	if m["type"] != "response.create" || r["conversation"] != "none" || in["type"] != "item_reference" || in["id"] != "item_c1" {
		t.Fatalf("request shape = %s", b)
	}
	if mods := r["output_modalities"].([]any); len(mods) != 1 || mods[0] != "text" {
		t.Fatalf("output modalities = %v", mods)
	}
	if meta["callharbor_purpose"] != "caller_transcript" || meta["callharbor_item_id"] != "item_c1" || len(meta) != 2 {
		t.Fatalf("metadata must carry only purpose and item id: %v", meta)
	}
	if mt, _ := r["max_output_tokens"].(float64); mt <= 0 || mt > 1024 {
		t.Fatalf("max_output_tokens = %v", r["max_output_tokens"])
	}
}

func TestOutOfBandRejectsInvalidItemIDLocally(t *testing.T) {
	st := newOOBState(time.Now)
	if b, err := st.request("bad id with spaces"); err != nil || b != nil {
		t.Fatalf("invalid id must not be sent: %s %v", b, err)
	}
	if len(st.fifo) != 1 || st.fifo[0].Kind != realtime.EventCallerTranscriptFailed || st.fifo[0].ErrCode != "item_id_invalid" {
		t.Fatalf("fifo = %+v", st.fifo)
	}
}

func TestOutOfBandPendingLimit(t *testing.T) {
	st := newOOBState(time.Now)
	for i := 0; i < MaxPendingOutOfBand; i++ {
		if b, err := st.request("item_" + string(rune('a'+i))); err != nil || b == nil {
			t.Fatalf("request %d: %s %v", i, b, err)
		}
	}
	if b, _ := st.request("item_over"); b != nil {
		t.Fatal("request beyond the pending bound must not be sent")
	}
	if last := st.fifo[len(st.fifo)-1]; last.ErrCode != "pending_limit" {
		t.Fatalf("fifo = %+v", st.fifo)
	}
}

func TestOutOfBandDuplicateItemRequestedOnce(t *testing.T) {
	st := newOOBState(time.Now)
	if b, _ := st.request("item_d"); b == nil {
		t.Fatal("first request must be sent")
	}
	if b, _ := st.request("item_d"); b != nil {
		t.Fatal("duplicate item must not be requested twice")
	}
}

func TestOutOfBandTimeoutFailsRequest(t *testing.T) {
	clock := time.Unix(1000, 0)
	st := newOOBState(func() time.Time { return clock })
	if b, _ := st.request("item_t"); b == nil {
		t.Fatal("request not sent")
	}
	clock = clock.Add(OutOfBandTimeout - time.Second)
	st.expire()
	if len(st.fifo) != 0 {
		t.Fatalf("request failed before the timeout: %+v", st.fifo)
	}
	clock = clock.Add(2 * time.Second)
	st.expire()
	if len(st.fifo) != 1 || st.fifo[0].ErrCode != "timeout" || len(st.pending) != 0 {
		t.Fatalf("timeout fifo = %+v pending=%d", st.fifo, len(st.pending))
	}
}

// Audio from an out-of-band response is never call audio: it is dropped and
// its transcription fails.
func TestOutOfBandOutputAudioIsDroppedAndFailed(t *testing.T) {
	st := newOOBState(time.Now)
	req := &oobRequest{itemID: "item_a", eventID: "ch_oob_1", responseID: "resp_x", started: time.Now()}
	st.pending["ch_oob_1"], st.byResponse["resp_x"] = req, req
	raw := []byte(`{"type":"response.output_audio.delta","event_id":"ea","response_id":"resp_x","item_id":"oob_y","delta":"AAA="}`)
	if _, consumed, err := st.handleOOB("response.output_audio.delta", raw); err != nil || !consumed {
		t.Fatalf("audio of an out-of-band response must be consumed: %v %v", consumed, err)
	}
	if len(st.fifo) != 1 || st.fifo[0].Kind != realtime.EventCallerTranscriptFailed || st.fifo[0].ErrCode != "modalities_ignored" || st.fifo[0].Audio != nil {
		t.Fatalf("fifo = %+v", st.fifo)
	}
}

// An error naming a pending out-of-band event fails that transcription, not the
// call, and the endpoint is not used for out-of-band text again.
func TestOutOfBandRejectedEventFailsItsRequest(t *testing.T) {
	st := newOOBState(time.Now)
	st.pending["ch_oob_1"] = &oobRequest{itemID: "item_r", eventID: "ch_oob_1", started: time.Now()}
	raw := []byte(`{"type":"error","event_id":"ev_err","error":{"type":"invalid_request_error","code":"unsupported_parameter","message":"no","event_id":"ch_oob_1"}}`)
	_, consumed, _ := st.handleOOB("error", raw)
	if !consumed || len(st.fifo) != 1 || st.fifo[0].Kind != realtime.EventCallerTranscriptFailed || st.fifo[0].ErrCode != "out_of_band_rejected:unsupported_parameter" {
		t.Fatalf("rejection fifo = %+v consumed=%v", st.fifo, consumed)
	}
	if !st.unsupported {
		t.Fatal("endpoint must be marked out-of-band unsupported")
	}
	if b, _ := st.request("item_next"); b != nil || st.fifo[len(st.fifo)-1].ErrCode != "out_of_band_unsupported" {
		t.Fatal("after rejection no further request may be sent")
	}
}

// An error that names no out-of-band event stays a call error.
func TestOutOfBandOrdinaryErrorStaysCallError(t *testing.T) {
	st := newOOBState(time.Now)
	raw := []byte(`{"type":"error","event_id":"ev_err","error":{"type":"invalid_request_error","code":"x","message":"y","event_id":"ev_other"}}`)
	if _, consumed, _ := st.handleOOB("error", raw); consumed || len(st.fifo) != 0 {
		t.Fatal("an error not about an out-of-band event must stay an EventError")
	}
}

func TestOutOfBandTerminalStatusesFail(t *testing.T) {
	for _, status := range []string{"cancelled", "failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			st := newOOBState(time.Now)
			req := &oobRequest{itemID: "item_s", eventID: "ch_oob_1", responseID: "resp_s", started: time.Now()}
			st.pending["ch_oob_1"], st.byResponse["resp_s"] = req, req
			raw := []byte(`{"type":"response.done","event_id":"rd","response":{"id":"resp_s","status":"` + status + `","metadata":{},"output":[]}}`)
			if _, consumed, _ := st.handleOOB("response.done", raw); !consumed {
				t.Fatal("out-of-band response.done must be consumed")
			}
			if len(st.fifo) != 1 || st.fifo[0].Kind != realtime.EventCallerTranscriptFailed || st.fifo[0].ErrCode != "response_"+status {
				t.Fatalf("fifo = %+v", st.fifo)
			}
			if st.byResponse["resp_s"] != nil || !st.retired["resp_s"] {
				t.Fatal("terminal response must be retired")
			}
		})
	}
}

// A completed response whose output holds no text part fails: audio
// transcripts never count as caller text.
func TestOutOfBandCompletedWithoutTextFails(t *testing.T) {
	st := newOOBState(time.Now)
	req := &oobRequest{itemID: "item_m", eventID: "ch_oob_1", responseID: "resp_m", started: time.Now()}
	st.pending["ch_oob_1"], st.byResponse["resp_m"] = req, req
	raw := []byte(`{"type":"response.done","event_id":"rd","response":{"id":"resp_m","status":"completed","metadata":{},"output":[{"content":[{"type":"output_audio","text":"nope"}]}]}}`)
	if _, consumed, _ := st.handleOOB("response.done", raw); !consumed {
		t.Fatal("completed out-of-band response without text must be consumed")
	}
	if len(st.fifo) != 1 || st.fifo[0].ErrCode != "missing_text" {
		t.Fatalf("fifo = %+v", st.fifo)
	}
}

// Late events of a retired response are dropped, never delivered.
func TestOutOfBandLateEventsOfRetiredResponseDropped(t *testing.T) {
	st := newOOBState(time.Now)
	st.retire("resp_old")
	raw := []byte(`{"type":"response.output_audio.delta","event_id":"ea","response_id":"resp_old","item_id":"i","delta":"AAA="}`)
	if _, consumed, _ := st.handleOOB("response.output_audio.delta", raw); !consumed || len(st.fifo) != 0 {
		t.Fatalf("late event of a retired response must be dropped, fifo=%+v", st.fifo)
	}
}

// A response that carries CallHarbor's purpose but matches no request is
// retired, so its audio is never forwarded as call audio.
func TestOutOfBandUnmatchedMetadataResponseIsRetired(t *testing.T) {
	st := newOOBState(time.Now)
	raw := []byte(`{"type":"response.created","event_id":"rc","response":{"id":"resp_stray","status":"in_progress","metadata":{"callharbor_purpose":"caller_transcript","callharbor_item_id":"item_gone"},"output":[]}}`)
	if _, consumed, _ := st.handleOOB("response.created", raw); !consumed || !st.retired["resp_stray"] {
		t.Fatal("stray CallHarbor response must be retired, not forwarded")
	}
}

// An ordinary agent response is untouched by the coordinator.
func TestOutOfBandAgentResponseUnaffected(t *testing.T) {
	st := newOOBState(time.Now)
	raw := []byte(`{"type":"response.created","event_id":"rc","response":{"id":"resp_agent","status":"in_progress","metadata":null,"output":[]}}`)
	if _, consumed, _ := st.handleOOB("response.created", raw); consumed {
		t.Fatal("an ordinary agent response must not be consumed")
	}
	raw = []byte(`{"type":"response.done","event_id":"rd","response":{"id":"resp_agent","status":"completed","output":[]}}`)
	if _, consumed, _ := st.handleOOB("response.done", raw); consumed {
		t.Fatal("an ordinary agent response.done must not be consumed")
	}
}

// End to end through the socket: a caller audio item makes the session send one
// out-of-band request. The endpoint answers with caller text, and its ordinary
// agent response still reaches the caller as EventResponseDone. The
// out-of-band response.done is never delivered as the agent's.
func TestOutOfBandEndToEnd(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		meta := metaOf(req)
		return []string{
			responseCreated("resp_oob", meta),
			`{"type":"response.output_text.delta","event_id":"od1","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"delta":"Two "}`,
			`{"type":"response.output_text.done","event_id":"od2","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"text":"Two people."}`,
			`{"type":"response.done","event_id":"od3","response":{"id":"resp_oob","status":"completed","metadata":` + mustJSON(meta) + `,"output":[]}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	evs := collect(t, s, func(got []realtime.Event) bool {
		for _, ev := range got {
			if ev.Kind == realtime.EventCallerTranscriptFinal {
				return true
			}
		}
		return false
	})

	reqs := e.requests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly one out-of-band request, got %d", len(reqs))
	}
	var final realtime.Event
	for _, ev := range evs {
		if ev.Kind == realtime.EventResponseDone || ev.Kind == realtime.EventAudio || ev.Kind == realtime.EventAgentTranscriptDone {
			t.Fatalf("out-of-band response leaked into the call: %+v", ev)
		}
		if ev.Kind == realtime.EventCallerTranscriptFinal {
			final = ev
		}
	}
	if final.Text != "Two people." || final.ItemID != "item_c1" || !final.OutOfBand || final.ResponseID != "resp_oob" {
		t.Fatalf("caller final = %+v", final)
	}
}

// An ordinary agent response on the same socket is still delivered as agent
// events, unaffected by the out-of-band coordinator.
func TestOutOfBandAgentResponseStillDelivered(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{
		`{"type":"response.done","event_id":"rd_agent","response":{"id":"resp_agent","status":"completed","output":[]}}`,
	}}
	s := connectOOB(t, e, time.Now)
	evs := collect(t, s, func(got []realtime.Event) bool { return len(got) >= 1 })
	if evs[0].Kind != realtime.EventResponseDone || evs[0].ResponseStatus != "completed" {
		t.Fatalf("agent response.done = %+v", evs[0])
	}
}

// nextNonIgnored returns the next event that is not EventIgnored. Ordinary
// conversation item events pass through as ignored and are not under test.
func nextNonIgnored(t *testing.T, s *Session) realtime.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ev.Kind != realtime.EventIgnored {
			return ev
		}
	}
}

// mainAgentFrames are an ordinary agent response on the same socket, for a
// response id that carries no CallHarbor metadata.
func mainAgentFrames(item string) []string {
	return []string{
		`{"type":"response.created","event_id":"ev_main_c","response":{"id":"resp_main","status":"in_progress","metadata":null,"output":[]}}`,
		`{"type":"response.output_audio.delta","event_id":"ev_main_a","response_id":"resp_main","item_id":"` + item + `","output_index":0,"content_index":0,"delta":"QUFBQQ=="}`,
		`{"type":"response.output_audio_transcript.delta","event_id":"ev_main_td","response_id":"resp_main","item_id":"` + item + `","output_index":0,"content_index":0,"delta":"Hello "}`,
		`{"type":"response.output_audio_transcript.done","event_id":"ev_main_tn","response_id":"resp_main","item_id":"` + item + `","output_index":0,"content_index":0,"transcript":"Hello there."}`,
		`{"type":"response.done","event_id":"ev_main_d","response":{"id":"resp_main","status":"completed","metadata":null,"output":[]}}`,
	}
}

// Main agent events and out-of-band events interleave on one socket. Agent
// audio and agent text must reach the caller only for the main item and
// response; caller text only for the caller item and the OOB response; and the
// single EventResponseDone must be the main response's.
func TestOutOfBandInterleavedWithMainAgentResponse(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		meta := metaOf(req)
		main := mainAgentFrames("item_main")
		// Interleave: OOB created, main audio, OOB text, main text, OOB done, main done.
		return []string{
			responseCreated("resp_oob", meta),
			main[0],
			`{"type":"response.output_text.delta","event_id":"od1","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"delta":"Two "}`,
			main[1],
			`{"type":"response.output_text.done","event_id":"od2","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"text":"Two people."}`,
			main[2],
			`{"type":"response.output_audio.delta","event_id":"ea_oob","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"delta":"QUFB"}`,
			`{"type":"response.done","event_id":"od3","response":{"id":"resp_oob","status":"completed","metadata":` + mustJSON(meta) + `,"output":[]}}`,
			main[3],
			main[4],
		}
	}
	s := connectOOB(t, e, time.Now)
	var evs []realtime.Event
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v (so far %+v)", err, evs)
		}
		evs = append(evs, ev)
		if ev.Kind == realtime.EventResponseDone {
			break
		}
	}
	var audio, agentText, caller, done int
	for _, ev := range evs {
		switch ev.Kind {
		case realtime.EventAudio:
			audio++
			if ev.ItemID != "item_main" || ev.OutOfBand {
				t.Fatalf("call audio from the wrong source: %+v", ev)
			}
		case realtime.EventAgentTranscriptDelta, realtime.EventAgentTranscriptDone:
			agentText++
			if ev.ItemID != "item_main" || ev.OutOfBand {
				t.Fatalf("agent text from the wrong source: %+v", ev)
			}
		case realtime.EventCallerTranscriptDelta, realtime.EventCallerTranscriptFinal:
			caller++
			if !ev.OutOfBand || ev.ItemID != "item_c1" || ev.ResponseID != "resp_oob" {
				t.Fatalf("caller event ids = %+v", ev)
			}
		case realtime.EventResponseDone:
			done++
			if ev.ResponseID != "resp_main" {
				t.Fatalf("response.done for %q, want the main response", ev.ResponseID)
			}
		}
	}
	if audio != 1 || agentText != 2 || caller != 2 || done != 1 {
		t.Fatalf("audio=%d agentText=%d caller=%d done=%d; events %+v", audio, agentText, caller, done, evs)
	}
	// Exact order of the normalized events. The coordinator queues its own
	// events and Next returns them in the order they were queued, interleaved
	// with the main response's events as the socket delivers them. This test
	// pins that sequence. The FIFO-order guarantee for an event read while the
	// queue is non-empty is pinned separately by
	// TestOutOfBandQueuedFailureDeliveredBeforeFollowingItemEvent.
	type step struct {
		kind       realtime.EventKind
		itemID     string
		responseID string
	}
	// The emulator's OOB response also sends audio, which the coordinator turns
	// into a failed caller event (modalities_ignored) and a targeted cancel.
	// The main response's ignored lifecycle events are not under test.
	want := []step{
		{realtime.EventCallerTranscriptDelta, "item_c1", "resp_oob"},
		{realtime.EventAudio, "item_main", "resp_main"},
		{realtime.EventCallerTranscriptFinal, "item_c1", "resp_oob"},
		{realtime.EventAgentTranscriptDelta, "item_main", "resp_main"},
		{realtime.EventCallerTranscriptFailed, "item_c1", "resp_oob"},
		{realtime.EventAgentTranscriptDone, "item_main", "resp_main"},
		{realtime.EventResponseDone, "", "resp_main"},
	}
	got := make([]step, 0, len(evs))
	for _, ev := range evs {
		if ev.Kind == realtime.EventIgnored {
			continue
		}
		got = append(got, step{ev.Kind, ev.ItemID, ev.ResponseID})
	}
	if len(got) != len(want) {
		var raw []string
		for _, ev := range evs {
			raw = append(raw, ev.RawType)
		}
		t.Fatalf("event sequence length %d, want %d: raw=%v got=%+v", len(got), len(want), raw, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v (all %+v)", i, got[i], want[i], got)
		}
	}
	// The OOB audio frame must never have become call audio.
	for _, ev := range evs {
		if ev.Kind == realtime.EventAudio && ev.ResponseID == "resp_oob" {
			t.Fatalf("out-of-band audio reached the call: %+v", ev)
		}
	}
}

// The endpoint rejects the out-of-band request with an error that names its
// client event id. That is a failed caller transcript, never a call error. The
// next caller item must fail as unsupported with no second response.create.
func TestOutOfBandRejectionOverSocket(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		eid, _ := req["event_id"].(string)
		return []string{
			`{"type":"error","event_id":"ev_rej","error":{"type":"invalid_request_error","code":"unsupported_parameter","message":"not supported","event_id":"` + eid + `"}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	ev := nextNonIgnored(t, s)
	if ev.Kind == realtime.EventError {
		t.Fatalf("out-of-band rejection surfaced as a call error: %+v", ev)
	}
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ItemID != "item_c1" || ev.ErrCode != "out_of_band_rejected:unsupported_parameter" || !ev.OutOfBand {
		t.Fatalf("rejection event = %+v", ev)
	}
	if n := len(e.requests()); n != 1 {
		t.Fatalf("requests after rejection = %d, want 1", n)
	}

	// A second caller item, sent by the endpoint, must not be requested again.
	// The endpoint's reply to the second item is a plain event from the fake.
	e.sendNext(`{"type":"conversation.item.added","event_id":"ev_item2","item":{"id":"item_c2","type":"message","status":"completed","role":"user","content":[{"type":"input_audio"}]}}`)
	ev = nextNonIgnored(t, s)
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ItemID != "item_c2" || ev.ErrCode != "out_of_band_unsupported" {
		t.Fatalf("second item event = %+v", ev)
	}
	if n := len(e.requests()); n != 1 {
		t.Fatalf("a second response.create was sent after rejection: %d requests", n)
	}
}

// Two sessions on separate connections each keep their own caller item and
// response ids. Neither session's caller events may name the other's.
func TestOutOfBandSessionIsolation(t *testing.T) {
	mk := func(item, resp string) (*oobEndpoint, string) {
		e := &oobEndpoint{afterUpd: []string{`{"type":"conversation.item.added","event_id":"ev_` + item + `","item":{"id":"` + item + `","type":"message","status":"completed","role":"user","content":[{"type":"input_audio"}]}}`}}
		e.respond = func(req map[string]any) []string {
			meta := metaOf(req)
			return []string{
				responseCreated(resp, meta),
				`{"type":"response.output_text.done","event_id":"d_` + resp + `","response_id":"` + resp + `","item_id":"oob","output_index":0,"content_index":0,"text":"text of ` + item + `"}`,
				`{"type":"response.done","event_id":"rd_` + resp + `","response":{"id":"` + resp + `","status":"completed","metadata":` + mustJSON(meta) + `,"output":[]}}`,
			}
		}
		return e, ""
	}
	ea, _ := mk("item_iso_a", "resp_iso_a")
	eb, _ := mk("item_iso_b", "resp_iso_b")
	sa := connectOOB(t, ea, time.Now)
	sb := connectOOB(t, eb, time.Now)

	type result struct {
		evs []realtime.Event
		err error
	}
	run := func(s *Session, out chan<- result) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var evs []realtime.Event
		for {
			ev, err := s.Next(ctx)
			if err != nil {
				out <- result{evs, err}
				return
			}
			evs = append(evs, ev)
			if ev.Kind == realtime.EventCallerTranscriptFinal {
				out <- result{evs, nil}
				return
			}
		}
	}
	ca, cb := make(chan result, 1), make(chan result, 1)
	go run(sa, ca)
	go run(sb, cb)
	ra, rb := <-ca, <-cb
	if ra.err != nil || rb.err != nil {
		t.Fatalf("sessions failed: %v / %v", ra.err, rb.err)
	}
	check := func(name string, evs []realtime.Event, item, resp string) {
		for _, ev := range evs {
			if ev.ItemID != "" && ev.ItemID != item && ev.Kind != realtime.EventIgnored {
				t.Fatalf("%s session got item %q: %+v", name, ev.ItemID, ev)
			}
			if ev.ResponseID != "" && ev.ResponseID != resp {
				t.Fatalf("%s session got response %q: %+v", name, ev.ResponseID, ev)
			}
		}
	}
	check("a", ra.evs, "item_iso_a", "resp_iso_a")
	check("b", rb.evs, "item_iso_b", "resp_iso_b")
	if got := ra.evs[len(ra.evs)-1]; got.Text != "text of item_iso_a" {
		t.Fatalf("session a final = %+v", got)
	}
	if got := rb.evs[len(rb.evs)-1]; got.Text != "text of item_iso_b" {
		t.Fatalf("session b final = %+v", got)
	}
}

// The upstream examples, fed through the coordinator. None of them is a caller
// audio item or a correlated out-of-band response, so none may send a request,
// fail, or be consumed as out-of-band. In particular conversation.item.done is
// assistant output_audio, never caller audio.
func TestUpstreamFixturesThroughCoordinator(t *testing.T) {
	st := newOOBState(time.Now)

	// A user input_text item from the reference is not caller audio.
	if id, ok := callerAudioItem(upstream(t, "conversation.item.added")); ok || id != "" {
		t.Fatalf("user input_text item treated as caller audio: %q", id)
	}
	// Assistant output_audio item from the reference is not caller audio.
	if id, ok := callerAudioItem(upstream(t, "conversation.item.done")); ok || id != "" {
		t.Fatalf("assistant output_audio item treated as caller audio: %q", id)
	}

	for _, name := range []string{"conversation.item.added", "conversation.item.done"} {
		sends, consumed, err := st.handleOOB(name, upstream(t, name))
		if err != nil || consumed || len(sends) != 0 {
			t.Fatalf("%s: sends=%d consumed=%v err=%v; want no request and not consumed", name, len(sends), consumed, err)
		}
	}
	if len(st.pending) != 0 || len(st.fifo) != 0 || len(st.requested) != 0 {
		t.Fatalf("coordinator state changed by non-caller items: pending=%d fifo=%d requested=%d", len(st.pending), len(st.fifo), len(st.requested))
	}

	// Ordinary main-response events are not correlated and stay ordinary.
	for _, name := range []string{"response.created", "response.output_text.delta", "response.output_text.done"} {
		sends, consumed, err := st.handleOOB(name, upstream(t, name))
		if err != nil || consumed || len(sends) != 0 {
			t.Fatalf("%s: sends=%d consumed=%v err=%v; want ordinary (not consumed)", name, len(sends), consumed, err)
		}
	}
	if len(st.fifo) != 0 {
		t.Fatalf("ordinary upstream events produced out-of-band events: %+v", st.fifo)
	}
}

// sentinelAgentDone is an ordinary agent response.done sent after the
// out-of-band checks. Reading it proves nothing from the out-of-band response
// was delivered ahead of it: the next call event must be this one.
const sentinelAgentDone = `{"type":"response.done","event_id":"ev_sentinel","response":{"id":"resp_sentinel","status":"completed","metadata":null,"output":[]}}`

// A response with no purpose echo and a null conversation_id is bound to the
// oldest waiting request. Its text is the caller transcript, and nothing from
// it reaches the call as agent output.
// out-of-band response for the oldest waiting request. Its text is the caller
// transcript; nothing from it reaches the call as agent output.
func TestOutOfBandNoEchoNullConversationDeliversCallerText(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		return []string{
			`{"type":"response.created","event_id":"rc","response":{"id":"resp_oob","status":"in_progress","conversation_id":null,"output":[]}}`,
			`{"type":"response.output_text.done","event_id":"od2","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"text":"Two people."}`,
			`{"type":"response.done","event_id":"od3","response":{"id":"resp_oob","status":"completed","conversation_id":null,"output":[]}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	ev := nextNonIgnored(t, s)
	if ev.Kind != realtime.EventCallerTranscriptFinal || ev.Text != "Two people." || ev.ItemID != "item_c1" || !ev.OutOfBand {
		t.Fatalf("caller final = %+v", ev)
	}
	e.sendNext(sentinelAgentDone)
	ev = nextNonIgnored(t, s)
	if ev.Kind != realtime.EventResponseDone || ev.ResponseID != "resp_sentinel" {
		t.Fatalf("out-of-band response leaked ahead of the agent response: %+v", ev)
	}
}

// The same response with audio first. The audio must never be call audio;
// the item fails, and the response's later events are dropped.
func TestOutOfBandNoEchoNullConversationAudioNeverLeaks(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		return []string{
			`{"type":"response.created","event_id":"rc","response":{"id":"resp_oob","status":"in_progress","conversation_id":null,"output":[]}}`,
			`{"type":"response.output_audio.delta","event_id":"ea","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"delta":"QUFB"}`,
			`{"type":"response.output_audio_transcript.done","event_id":"at","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"transcript":"agent words"}`,
			`{"type":"response.done","event_id":"od3","response":{"id":"resp_oob","status":"completed","conversation_id":null,"output":[]}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	ev := nextNonIgnored(t, s)
	if ev.Kind == realtime.EventAudio || ev.Kind == realtime.EventAgentTranscriptDone || ev.Kind == realtime.EventResponseDone {
		t.Fatalf("out-of-band response leaked into the call: %+v", ev)
	}
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ErrCode != "modalities_ignored" || ev.ItemID != "item_c1" {
		t.Fatalf("audio from the out-of-band response = %+v", ev)
	}
	e.sendNext(sentinelAgentDone)
	ev = nextNonIgnored(t, s)
	if ev.Kind != realtime.EventResponseDone || ev.ResponseID != "resp_sentinel" {
		t.Fatalf("out-of-band events leaked ahead of the agent response: %+v", ev)
	}
}

// An echoed purpose with a non-null conversation_id means the endpoint
// ignored conversation "none". The item fails as conversation_not_none, the
// endpoint is latched unsupported, and the response's events are dropped.
func TestOutOfBandConversationNotNoneFailsItemAndLatches(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		meta := metaOf(req)
		return []string{
			`{"type":"response.created","event_id":"rc","response":{"id":"resp_oob","status":"in_progress","conversation_id":"conv_1","metadata":` + mustJSON(meta) + `,"output":[]}}`,
			`{"type":"response.output_audio.delta","event_id":"ea","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"delta":"QUFB"}`,
			`{"type":"response.done","event_id":"od3","response":{"id":"resp_oob","status":"completed","conversation_id":"conv_1","metadata":` + mustJSON(meta) + `,"output":[]}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	ev := nextNonIgnored(t, s)
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ErrCode != "conversation_not_none" || ev.ItemID != "item_c1" {
		t.Fatalf("conversation none ignored = %+v", ev)
	}
	e.sendNext(sentinelAgentDone)
	ev = nextNonIgnored(t, s)
	if ev.Kind != realtime.EventResponseDone || ev.ResponseID != "resp_sentinel" {
		t.Fatalf("response of an ignored conversation none leaked: %+v", ev)
	}
	// Latched: the next caller item is not requested again.
	e.sendNext(`{"type":"conversation.item.added","event_id":"ev_item2","item":{"id":"item_c2","type":"message","status":"completed","role":"user","content":[{"type":"input_audio"}]}}`)
	ev = nextNonIgnored(t, s)
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ItemID != "item_c2" || ev.ErrCode != "out_of_band_unsupported" {
		t.Fatalf("second item after conversation_not_none = %+v", ev)
	}
	if n := len(e.requests()); n != 1 {
		t.Fatalf("requests after conversation_not_none = %d, want 1", n)
	}
}

// Audio from an out-of-band response latches the endpoint and
// queues a targeted response.cancel for that response id only.
func TestOutOfBandModalitiesIgnoredSendsTargetedCancel(t *testing.T) {
	st := newOOBState(time.Now)
	req := &oobRequest{itemID: "item_a", eventID: "ch_oob_1", responseID: "resp_x", started: time.Now()}
	st.pending["ch_oob_1"], st.byResponse["resp_x"] = req, req
	raw := []byte(`{"type":"response.output_audio.delta","event_id":"ea","response_id":"resp_x","item_id":"oob_y","delta":"AAA="}`)
	sends, consumed, err := st.handleOOB("response.output_audio.delta", raw)
	if err != nil || !consumed {
		t.Fatalf("consumed=%v err=%v", consumed, err)
	}
	if !st.unsupported {
		t.Fatal("modalities_ignored must latch the endpoint unsupported")
	}
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want one cancel", len(sends))
	}
	var m map[string]any
	if err := json.Unmarshal(sends[0], &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "response.cancel" || m["response_id"] != "resp_x" || len(m) != 3 || !strings.HasPrefix(m["event_id"].(string), oobCancelPrefix) {
		t.Fatalf("cancel must name only our response and carry our event id: %s", sends[0])
	}
}

// An error with no event_id is attributed to the only pending
// request when that request has no response id and param names no other field.
// It fails the item and does not latch the endpoint.
func TestOutOfBandNoEventIDErrorAttributedToSoleRequest(t *testing.T) {
	st := newOOBState(time.Now)
	if b, _ := st.request("item_s"); b == nil {
		t.Fatal("request not sent")
	}
	raw := []byte(`{"type":"error","event_id":"ev_e","error":{"type":"invalid_request_error","code":"bad","message":"m","param":"response.metadata"}}`)
	_, consumed, _ := st.handleOOB("error", raw)
	if !consumed || len(st.fifo) != 1 || st.fifo[0].ItemID != "item_s" || st.fifo[0].ErrCode != "out_of_band_rejected:bad" {
		t.Fatalf("attributed error: consumed=%v fifo=%+v", consumed, st.fifo)
	}
	if len(st.pending) != 0 {
		t.Fatal("attributed request must be finished")
	}
	// Heuristic attribution never latches the endpoint, whatever the error type.
	if st.unsupported {
		t.Fatal("heuristic attribution must not latch the endpoint unsupported")
	}
}

// Heuristic attribution with the invalid_request_error type still fails only
// its item. Only a matched event id may latch the endpoint.
func TestOutOfBandNoEventIDInvalidRequestDoesNotLatch(t *testing.T) {
	st := newOOBState(time.Now)
	if _, err := st.request("item_h"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"error","event_id":"ev_h","error":{"type":"invalid_request_error","code":"bad","message":"m","param":"response"}}`)
	if _, consumed, _ := st.handleOOB("error", raw); !consumed {
		t.Fatal("sole-request heuristic error must be consumed")
	}
	if st.unsupported {
		t.Fatal("invalid_request_error on the heuristic path must not latch")
	}
	if b, _ := st.request("item_after"); b == nil {
		t.Fatal("the next item must still be requested after a heuristic failure")
	}
}

// Two pending requests make an error with no event_id ambiguous,
// so it stays an ordinary call error.
func TestOutOfBandNoEventIDErrorWithTwoPendingStaysCallError(t *testing.T) {
	st := newOOBState(time.Now)
	for _, id := range []string{"item_a", "item_b"} {
		if _, err := st.request(id); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte(`{"type":"error","event_id":"ev_e","error":{"type":"invalid_request_error","code":"bad","message":"m","param":"response"}}`)
	if _, consumed, _ := st.handleOOB("error", raw); consumed || len(st.fifo) != 0 || len(st.pending) != 2 {
		t.Fatalf("ambiguous error must stay a call error: consumed=%v fifo=%+v pending=%d", consumed, st.fifo, len(st.pending))
	}
}

// A correlated request (it has a response id) is not the target
// of an error with no event_id, so the error stays a call error.
func TestOutOfBandNoEventIDErrorWithCorrelatedRequestStaysCallError(t *testing.T) {
	st := newOOBState(time.Now)
	req := &oobRequest{itemID: "item_c", eventID: "ch_oob_9", responseID: "resp_c", started: time.Now()}
	st.pending["ch_oob_9"], st.byResponse["resp_c"] = req, req
	raw := []byte(`{"type":"error","event_id":"ev_e","error":{"type":"invalid_request_error","code":"bad","message":"m"}}`)
	if _, consumed, _ := st.handleOOB("error", raw); consumed || len(st.fifo) != 0 || len(st.pending) != 1 {
		t.Fatalf("error for a correlated request must stay a call error: consumed=%v fifo=%+v", consumed, st.fifo)
	}
}

// A rate limit fails only its item. It does not latch the
// endpoint, so the next caller item is still requested.
func TestOutOfBandRateLimitFailsItemWithoutLatch(t *testing.T) {
	st := newOOBState(time.Now)
	if _, err := st.request("item_r"); err != nil {
		t.Fatal(err)
	}
	eid := ""
	for k := range st.pending {
		eid = k
	}
	raw := []byte(`{"type":"error","event_id":"ev_e","error":{"type":"rate_limit_exceeded","code":"rate_limit","message":"m","event_id":"` + eid + `"}}`)
	if _, consumed, _ := st.handleOOB("error", raw); !consumed {
		t.Fatal("rate-limit error for an out-of-band event must be consumed")
	}
	if st.unsupported {
		t.Fatal("a rate limit must not latch the endpoint unsupported")
	}
	if b, _ := st.request("item_next"); b == nil {
		t.Fatal("next item must still be requested after a rate limit")
	}
}

// When output_text.done was already delivered, a timeout before
// response.done must not emit timeout. The request is finished silently.
func TestOutOfBandFinalThenTimeoutIsSilent(t *testing.T) {
	clock := time.Unix(1000, 0)
	st := newOOBState(func() time.Time { return clock })
	if _, err := st.request("item_f"); err != nil {
		t.Fatal(err)
	}
	req := st.pending["ch_oob_1"]
	req.responseID = "resp_f"
	st.byResponse["resp_f"] = req
	st.textOrAudio("response.output_text.done", req, "od", "", "Final words.")
	if ev, _ := st.pop(); ev.Kind != realtime.EventCallerTranscriptFinal {
		t.Fatalf("final not delivered: %+v", ev)
	}
	clock = clock.Add(OutOfBandTimeout + time.Second)
	st.expire()
	if len(st.fifo) != 0 {
		t.Fatalf("timeout emitted after a delivered final: %+v", st.fifo)
	}
	if len(st.pending) != 0 || !st.retired["resp_f"] {
		t.Fatal("finished request must be retired silently")
	}
	if _, consumed, _ := st.handleOOB("response.output_audio.delta", []byte(`{"type":"response.output_audio.delta","event_id":"late","response_id":"resp_f","item_id":"x","delta":"AAA="}`)); !consumed || len(st.fifo) != 0 {
		t.Fatal("late events of the finished response must be dropped")
	}
}

// A request that the endpoint never answers fails with timeout when the clock
// passes OutOfBandTimeout. The check runs when Next reads the next server
// message, so a later message after the deadline delivers the timeout first.
func TestOutOfBandSocketTimeoutFailsThroughNext(t *testing.T) {
	var mu sync.Mutex
	clock := time.Unix(1_800_000_000, 0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d)
	}
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	s := connectOOB(t, e, now)

	// The request is sent when the item arrives, and nothing answers it.
	if n := len(e.requests()); n != 0 {
		t.Fatalf("requests before Next = %d", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Read the item event once: Next processes the item and sends the request.
	first, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if first.Kind != realtime.EventIgnored {
		t.Fatalf("first event = %+v, want the ignored item event", first)
	}
	// The request is written by Next before it returns to the caller, but the
	// endpoint reads it on its own goroutine. Wait for it to arrive.
	deadline := time.Now().Add(5 * time.Second)
	for len(e.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(e.requests()); n != 1 {
		t.Fatalf("requests after the item = %d, want 1", n)
	}
	advance(OutOfBandTimeout + time.Second)
	// The endpoint now sends an ordinary agent event; reading it runs the expiry.
	e.sendNext(`{"type":"response.done","event_id":"rd_t","response":{"id":"resp_t","status":"completed","metadata":null,"output":[]}}`)
	var got []realtime.Event
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("next after timeout: %v (got %+v)", err, got)
		}
		got = append(got, ev)
		if ev.Kind == realtime.EventResponseDone {
			break
		}
	}
	if len(got) < 2 || got[0].Kind != realtime.EventCallerTranscriptFailed || got[0].ErrCode != "timeout" || got[0].ItemID != "item_c1" {
		t.Fatalf("timeout must be delivered before the next server event: %+v", got)
	}
	if got[len(got)-1].ResponseID != "resp_t" {
		t.Fatalf("agent response.done not delivered after timeout: %+v", got)
	}
}

// A provider error code is bounded before it is stored: 64 bytes at most, and
// only [A-Za-z0-9_.-]. Every other byte becomes '_'.
func TestOutOfBandProviderErrorCodeIsBounded(t *testing.T) {
	if got := providerCode("bad code<script>"); got != "bad_code_script_" {
		t.Fatalf("unsafe characters = %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := providerCode(long); len(got) != 64 {
		t.Fatalf("length = %d, want 64", len(got))
	}
	if got := providerCode(""); got != "unspecified" {
		t.Fatalf("empty code = %q", got)
	}
	st := newOOBState(time.Now)
	if _, err := st.request("item_code"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"error","event_id":"ev_e","error":{"type":"invalid_request_error","code":"x\n\"y","message":"m","event_id":"ch_oob_1"}}`)
	if _, _, err := st.handleOOB("error", raw); err != nil {
		t.Fatal(err)
	}
	if len(st.fifo) != 1 || st.fifo[0].ErrCode != "out_of_band_rejected:x__y" {
		t.Fatalf("stored code = %+v", st.fifo)
	}
}

// expire emits timeouts in sequence order, so two requests that expire together
// always produce the same event order.
func TestOutOfBandExpireIsDeterministicBySequence(t *testing.T) {
	clock := time.Unix(1000, 0)
	st := newOOBState(func() time.Time { return clock })
	for _, id := range []string{"item_1", "item_2", "item_3"} {
		if _, err := st.request(id); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(OutOfBandTimeout + time.Second)
	st.expire()
	var got []string
	for _, ev := range st.fifo {
		got = append(got, ev.ItemID)
	}
	if strings.Join(got, ",") != "item_1,item_2,item_3" {
		t.Fatalf("timeout order = %v, want sequence order", got)
	}
}

// A targeted cancel can race the response it cancels. If the response has
// already finished, the server answers the cancel with an error that names the
// cancel's event id. That error is consumed: the call keeps going, the item's
// modalities_ignored failure is reported once, and no call error is surfaced.
func TestOutOfBandCancelRaceErrorIsNotCallError(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		meta := metaOf(req)
		return []string{
			responseCreated("resp_oob", meta),
			`{"type":"response.output_audio.delta","event_id":"ea","response_id":"resp_oob","item_id":"oob_1","output_index":0,"content_index":0,"delta":"QUFB"}`,
			`{"type":"response.done","event_id":"od","response":{"id":"resp_oob","status":"completed","conversation_id":null,"metadata":` + mustJSON(meta) + `,"output":[]}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	ev := nextNonIgnored(t, s)
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ErrCode != "modalities_ignored" || ev.ItemID != "item_c1" {
		t.Fatalf("first event = %+v, want modalities_ignored once", ev)
	}
	// The cancel names our event id. The endpoint has already finished the
	// response, so it answers with an error for that cancel event.
	var cancelID string
	deadline := time.Now().Add(5 * time.Second)
	for cancelID == "" && time.Now().Before(deadline) {
		for _, m := range e.allClient() {
			if m["type"] == "response.cancel" {
				cancelID, _ = m["event_id"].(string)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cancelID == "" {
		t.Fatal("no targeted response.cancel was sent")
	}
	e.sendNext(`{"type":"error","event_id":"ev_cancel_err","error":{"type":"invalid_request_error","code":"response_cancel_not_active","message":"not active","param":null,"event_id":"` + cancelID + `"}}`)
	e.sendNext(`{"type":"response.done","event_id":"ev_sentinel","response":{"id":"resp_sentinel","status":"completed","metadata":null,"output":[]}}`)
	ev = nextNonIgnored(t, s)
	if ev.Kind == realtime.EventError {
		t.Fatalf("cancel race surfaced as a call error: %+v", ev)
	}
	if ev.Kind != realtime.EventResponseDone || ev.ResponseID != "resp_sentinel" {
		t.Fatalf("after the cancel race the call must continue with the agent response: %+v", ev)
	}
}

// A late error for a request that already timed out is dropped: it names a
// CallHarbor event id, so it is never a call error.
func TestOutOfBandLateErrorForTimedOutRequestIsDropped(t *testing.T) {
	var mu sync.Mutex
	clock := time.Unix(1_800_000_000, 0)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); clock = clock.Add(d) }

	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	s := connectOOB(t, e, now)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Next(ctx); err != nil {
		t.Fatalf("item event: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(e.requests()) != 1 {
		t.Fatal("request was not sent")
	}
	eid, _ := e.requests()[0]["event_id"].(string)

	advance(OutOfBandTimeout + time.Second)
	e.sendNext(`{"type":"response.done","event_id":"ev_t","response":{"id":"resp_t","status":"completed","metadata":null,"output":[]}}`)
	// The timeout is delivered first. The agent response that triggered the
	// expiry check is delivered after it.
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ErrCode != "timeout" {
		t.Fatalf("timeout event = %+v", ev)
	}
	ev, err = s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.EventResponseDone || ev.ResponseID != "resp_t" {
		t.Fatalf("agent response after timeout = %+v", ev)
	}

	// The late error names the timed-out request. It is dropped, and the agent
	// response that follows is delivered next.
	e.sendNext(`{"type":"error","event_id":"ev_late","error":{"type":"invalid_request_error","code":"late","message":"m","param":null,"event_id":"` + eid + `"}}`)
	e.sendNext(`{"type":"response.done","event_id":"ev_sentinel","response":{"id":"resp_sentinel","status":"completed","metadata":null,"output":[]}}`)
	ev = nextNonIgnored(t, s)
	if ev.Kind == realtime.EventError {
		t.Fatalf("late error for a timed-out request surfaced as a call error: %+v", ev)
	}
	if ev.Kind != realtime.EventResponseDone || ev.ResponseID != "resp_sentinel" {
		t.Fatalf("after the late error the call must continue: %+v", ev)
	}
}

// A completed out-of-band response with no output at all is a successful empty
// transcription: exactly one final with empty text, bound to the right item and
// response, and never a missing_text failure.
func TestOutOfBandEmptyCompletedResponseIsEmptyFinal(t *testing.T) {
	st := newOOBState(time.Now)
	if _, err := st.request("item_empty"); err != nil {
		t.Fatal(err)
	}
	var eid string
	for k := range st.pending {
		eid = k
	}
	req := st.pending[eid]
	req.responseID = "resp_empty"
	st.byResponse["resp_empty"] = req
	raw := []byte(`{"type":"response.done","event_id":"rd","response":{"id":"resp_empty","status":"completed","metadata":{},"output":[]}}`)
	if _, consumed, _ := st.handleOOB("response.done", raw); !consumed {
		t.Fatal("completed out-of-band response must be consumed")
	}
	if len(st.fifo) != 1 {
		t.Fatalf("events = %+v, want exactly one final", st.fifo)
	}
	ev := st.fifo[0]
	if ev.Kind != realtime.EventCallerTranscriptFinal || ev.Text != "" || !ev.OutOfBand || ev.ItemID != "item_empty" || ev.ResponseID != "resp_empty" {
		t.Fatalf("empty completed response = %+v, want an empty caller final", ev)
	}
}

// Once the endpoint is latched unsupported, a further caller item fails inside
// the coordinator and the item event is still passed on. Next must deliver the
// queued failure before that item event, so it appends the item event behind
// the queue rather than returning it first. The exact raw order is asserted,
// including ignored events, so removing the append reorders the sequence.
func TestOutOfBandQueuedFailureDeliveredBeforeFollowingItemEvent(t *testing.T) {
	e := &oobEndpoint{afterUpd: []string{callerItemAdded}}
	e.respond = func(req map[string]any) []string {
		eid, _ := req["event_id"].(string)
		return []string{
			`{"type":"error","event_id":"ev_rej","error":{"type":"invalid_request_error","code":"unsupported_parameter","message":"no","event_id":"` + eid + `"}}`,
		}
	}
	s := connectOOB(t, e, time.Now)
	if ev := nextNonIgnored(t, s); ev.Kind != realtime.EventCallerTranscriptFailed || ev.ItemID != "item_c1" {
		t.Fatalf("first rejection = %+v", ev)
	}
	// The endpoint is now latched unsupported. The next item is queued as a
	// failure inside the coordinator, and its own event follows it.
	e.sendNext(`{"type":"conversation.item.added","event_id":"ev_item2","item":{"id":"item_c2","type":"message","status":"completed","role":"user","content":[{"type":"input_audio"}]}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []realtime.Event
	for len(got) < 2 {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v (got %+v)", err, got)
		}
		got = append(got, ev)
	}
	if got[0].Kind != realtime.EventCallerTranscriptFailed || got[0].ItemID != "item_c2" || got[0].ErrCode != "out_of_band_unsupported" {
		t.Fatalf("queued failure must be delivered first: %+v", got)
	}
	if got[1].RawType != "conversation.item.added" || got[1].EventID != "ev_item2" {
		t.Fatalf("the item event must follow the queued failure: %+v", got)
	}
}

// The out-of-band request must not inherit the session's tools: it carries an
// empty tool list and tool_choice none, so the transcription can never call
// the report tool.
func TestOutOfBandRequestDisablesSessionTools(t *testing.T) {
	st := newOOBState(time.Now)
	b, err := st.request("item_tools")
	if err != nil || b == nil {
		t.Fatalf("request = %s %v", b, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	r := m["response"].(map[string]any)
	tools, ok := r["tools"].([]any)
	if !ok || len(tools) != 0 || r["tool_choice"] != "none" {
		t.Fatalf("out-of-band request tools = %v tool_choice = %v, want [] and none", r["tools"], r["tool_choice"])
	}
}

// A function call inside a correlated out-of-band response is never a task
// report and never acknowledged. Its arguments events are consumed, and the
// response fails as missing text, not as an empty transcription.
func TestOutOfBandFunctionCallIsNotTaskReport(t *testing.T) {
	st := newOOBState(time.Now)
	req := &oobRequest{itemID: "item_fc", eventID: "ch_oob_1", responseID: "resp_fc", started: time.Now()}
	st.pending["ch_oob_1"], st.byResponse["resp_fc"] = req, req

	args := []byte(`{"type":"response.function_call_arguments.done","event_id":"efc","response_id":"resp_fc","item_id":"fc_1","output_index":0,"call_id":"call_oob","name":"report_task_outcome","arguments":"{}"}`)
	if sends, consumed, err := st.handleOOB("response.function_call_arguments.done", args); err != nil || !consumed || len(sends) != 0 {
		t.Fatalf("function call arguments in an out-of-band response: consumed=%v sends=%d err=%v", consumed, len(sends), err)
	}
	added := []byte(`{"type":"response.output_item.added","event_id":"eoa","response_id":"resp_fc","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"report_task_outcome","call_id":"call_oob"}}`)
	if _, consumed, _ := st.handleOOB("response.output_item.added", added); !consumed {
		t.Fatal("output item of an out-of-band response must be consumed")
	}
	done := []byte(`{"type":"response.done","event_id":"rd","response":{"id":"resp_fc","status":"completed","metadata":{},"output":[{"type":"function_call","name":"report_task_outcome","call_id":"call_oob","arguments":"{}"}]}}`)
	if _, consumed, _ := st.handleOOB("response.done", done); !consumed {
		t.Fatal("completed out-of-band response with a function call must be consumed")
	}
	for _, ev := range st.fifo {
		if ev.Kind == realtime.EventTaskReport {
			t.Fatalf("function call in an out-of-band response surfaced as a task report: %+v", ev)
		}
	}
	if len(st.fifo) != 1 || st.fifo[0].Kind != realtime.EventCallerTranscriptFailed || st.fifo[0].ErrCode != "missing_text" {
		t.Fatalf("fifo = %+v, want one missing_text failure and no empty final", st.fifo)
	}
}
