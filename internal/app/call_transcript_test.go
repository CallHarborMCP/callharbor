package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// transcriptPage is the REST transcript page as the tests read it.
type transcriptPage struct {
	CallID       string           `json:"call_id"`
	Finalized    bool             `json:"finalized"`
	QualityFlags []string         `json:"quality_flags"`
	Turns        []transcriptTurn `json:"turns"`
}

type transcriptTurn struct {
	Seq            int      `json:"seq"`
	Speaker        string   `json:"speaker"`
	Finality       string   `json:"finality"`
	Text           string   `json:"text"`
	Fidelity       string   `json:"fidelity"`
	TimingSource   string   `json:"timing_source"`
	StartedMS      int64    `json:"started_ms"`
	SourceIDs      []string `json:"source_ids"`
	ProviderItemID *string  `json:"provider_item_id"`
	GeneratedMS    *int64   `json:"generated_ms"`
	PlayedMS       *int64   `json:"played_ms"`
	QualityFlags   []string `json:"quality_flags"`
}

// TestCallTranscriptEndToEnd drives one call per protocol through the real app
// wiring, closes the media stream, and reads the persisted transcript back
// through REST as the owner. Another user gets the same not_found as an
// unknown call, and every read is audited.
func TestCallTranscriptEndToEnd(t *testing.T) {
	cases := []struct {
		protocol realtime.Protocol
		events   []map[string]any
		// oob marks a realtime case whose caller text comes from the
		// out-of-band response the app sends for the committed caller item.
		oob   bool
		check func(t *testing.T, p transcriptPage)
	}{
		{
			protocol: realtime.ProtocolRealtime,
			events: []map[string]any{
				{"type": "response.output_audio.delta", "event_id": "ev_a1", "delta": "QUFBQUFBQUFBQQ==", "item_id": "msg_1", "response_id": "resp_1"},
				{"type": "response.output_audio_transcript.delta", "event_id": "ev_t1", "delta": "Hello, this is ", "item_id": "msg_1", "response_id": "resp_1"},
				{"type": "response.output_audio_transcript.done", "event_id": "ev_t2", "transcript": "Hello, this is the clinic.", "item_id": "msg_1", "response_id": "resp_1"},
				{"type": "input_audio_buffer.speech_started", "event_id": "ev_s1", "audio_start_ms": 1200, "item_id": "msg_2"},
				// The caller's audio item is committed; the out-of-band request
				// is answered with text only, as the endpoint would.
				{"type": "conversation.item.added", "event_id": "ev_add", "item": map[string]any{
					"id": "msg_2", "type": "message", "status": "completed", "role": "user",
					"content": []map[string]any{{"type": "input_audio"}}}},
			},
			oob: true,
			check: func(t *testing.T, p transcriptPage) {
				if len(p.Turns) != 2 {
					t.Fatalf("turns = %+v, want agent and caller", p.Turns)
				}
				a, c := p.Turns[0], p.Turns[1]
				if a.Speaker != "assistant" || a.Text != "Hello, this is the clinic." || a.ProviderItemID == nil || *a.ProviderItemID != "msg_1" {
					t.Fatalf("agent turn = %+v", a)
				}
				if !slices.Contains(a.SourceIDs, "ev_t1") || !slices.Contains(a.SourceIDs, "ev_t2") || a.GeneratedMS == nil || a.PlayedMS == nil {
					t.Fatalf("agent provenance = %+v", a)
				}
				if slices.Contains(a.QualityFlags, "corrected") {
					t.Fatalf("final text that completes the deltas was flagged corrected: %+v", a)
				}
				if c.Speaker != "callee" || c.Text != "Book a table for two." || c.Fidelity != "provider_asr_approximate" {
					t.Fatalf("caller turn = %+v", c)
				}
				if !slices.Contains(c.QualityFlags, "caller_text_model_derived") || slices.Contains(c.QualityFlags, "caller_transcript_unavailable") {
					t.Fatalf("caller turn flags = %v", c.QualityFlags)
				}
				// Only the caller side is asserted here. Agent playout depends
				// on barge-in timing in the fake and is not part of #113. The
				// page must never report the caller side as unavailable or
				// unresolved, because the out-of-band text arrived.
				for _, f := range []string{"caller_transcript_unavailable", "caller_transcript_unresolved", "caller_transcript_failed", "caller_transcript_pending", "unexpected_caller_transcription_ignored"} {
					if slices.Contains(p.QualityFlags, f) {
						t.Fatalf("page flags %v carry %s on a successful caller transcript", p.QualityFlags, f)
					}
				}
				for _, turn := range p.Turns {
					if turn.Speaker == "callee" && slices.Contains(turn.QualityFlags, "caller_transcript_failed") {
						t.Fatalf("successful caller turn flagged failed: %+v", turn)
					}
				}
			},
		},
		{
			protocol: realtime.ProtocolLive,
			events: []map[string]any{
				{"type": "session.output_audio.delta", "event_id": "ev_a1", "delta": "QUFBQUFBQUFBQQ=="},
				{"type": "session.output_transcript.delta", "event_id": "ev_o1", "delta": "Hi, how can I help?", "start_ms": 0, "end_ms": 900},
				{"type": "session.input_transcript.delta", "event_id": "ev_i1", "delta": "A table ", "start_ms": 1200, "end_ms": 1500},
				{"type": "session.input_transcript.delta", "event_id": "ev_i2", "delta": "for two.", "start_ms": 1500, "end_ms": 1900},
			},
			check: func(t *testing.T, p transcriptPage) {
				if len(p.Turns) != 2 {
					t.Fatalf("turns = %+v, want agent and caller", p.Turns)
				}
				a, c := p.Turns[0], p.Turns[1]
				if a.Speaker != "assistant" || a.Text != "Hi, how can I help?" || a.TimingSource != "provider" || a.GeneratedMS == nil || a.PlayedMS == nil {
					t.Fatalf("agent turn = %+v", a)
				}
				if c.Speaker != "callee" || c.Text != "A table for two." || c.Fidelity != "provider_asr_approximate" || c.TimingSource != "provider" || c.StartedMS != 1200 {
					t.Fatalf("caller turn = %+v", c)
				}
				if !slices.Equal(c.SourceIDs, []string{"ev_i1", "ev_i2"}) {
					t.Fatalf("caller sources = %v", c.SourceIDs)
				}
				if !slices.Contains(p.QualityFlags, "turns_grouped_locally") || slices.Contains(p.QualityFlags, "caller_transcript_unavailable") {
					t.Fatalf("page flags = %v", p.QualityFlags)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, "v3:ctl-tr")
			ai := newFakeAI(t, tc.protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, tc.protocol, ai.endpoint())

			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": tpID, "to": "+14155550111", "objective": "Book a table",
			}, withIdempotency(h, "transcript-1"))
			if status != http.StatusAccepted {
				t.Fatalf("create call = %d %s", status, body)
			}
			callID, _ := decodeObject(t, body)["call_id"].(string)
			tel.waitDials(t, 1)
			env.signedWebhook(callID, "call.initiated", "evt-init-tr", "v3:ctl-tr")
			env.signedWebhook(callID, "call.answered", "evt-ans-tr", "v3:ctl-tr")
			mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
			if err != nil {
				t.Fatalf("media dial: %v", err)
			}
			sendStart(t, mediaConn, "v3:ctl-tr")
			sendMedia(t, mediaConn, "AAAAAAAAAAA=")
			waitEvents(t, ai, 2)
			for _, ev := range tc.events {
				ai.sendRaw(t, ev)
			}
			if tc.oob {
				answerOutOfBand(t, ai, "msg_2", "Book a table for two.")
			}
			assertStreamMediaPayload(t, mediaConn, "QUFBQUFBQUFBQQ==")
			// The provider events are consumed asynchronously. Wait until the
			// periodic flush shows both turns, so the close cannot overtake them.
			waitTurns(t, env.dataDir, callID, 2)
			// Closing the stream ends the run; the recorder then writes its
			// final revisions and the transcript.finalized summary.
			_ = mediaConn.Close()
			waitTranscriptFinalized(t, env.dataDir, callID)

			url := env.base + "/api/v1/calls/" + callID + "/transcript"
			status, _, body = send(t, env.client, http.MethodGet, url, nil, env.admin.headers(false))
			if status != http.StatusOK {
				t.Fatalf("owner transcript = %d %s", status, body)
			}
			var page transcriptPage
			if err := json.Unmarshal(body, &page); err != nil {
				t.Fatal(err)
			}
			if page.CallID != callID || !page.Finalized {
				t.Fatalf("page = %+v", page)
			}
			for _, turn := range page.Turns {
				if turn.Finality != "final" {
					t.Fatalf("turn %d is %s after the call ended", turn.Seq, turn.Finality)
				}
			}
			tc.check(t, page)

			other := createUser(t, env.admin, "mallory")
			status, _, body = send(t, env.client, http.MethodGet, url, nil, other.headers(false))
			if status != http.StatusNotFound {
				t.Fatalf("foreign transcript = %d %s, want 404", status, body)
			}
			if n := countAudit(t, env.dataDir, callID, "success"); n != 1 {
				t.Fatalf("success audits = %d, want 1", n)
			}
			if n := countAudit(t, env.dataDir, callID, "denied"); n != 1 {
				t.Fatalf("denied audits = %d, want 1", n)
			}
		})
	}
}

func openAppDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// waitTranscriptFinalized waits, with a deadline, for the recorder's final
// summary event of a call.
func waitTranscriptFinalized(t *testing.T, dataDir, callID string) {
	t.Helper()
	db := openAppDB(t, dataDir)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM call_events WHERE call_id = ? AND kind = 'transcript.finalized'`, callID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("transcript was never finalized")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitTurns waits, with a deadline, until a call has n distinct stored turns.
func waitTurns(t *testing.T, dataDir, callID string, n int) {
	t.Helper()
	db := openAppDB(t, dataDir)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := db.QueryRow(`SELECT COUNT(DISTINCT seq) FROM transcript_turns WHERE call_id = ?`, callID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("call has %d stored turns, want %d", got, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countAudit(t *testing.T, dataDir, callID, outcome string) int {
	t.Helper()
	var n int
	err := openAppDB(t, dataDir).QueryRow(`SELECT COUNT(*) FROM audit_events
		WHERE action = 'transcript.read' AND resource_id = ? AND outcome = ?`, callID, outcome).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// answerOutOfBand waits for the out-of-band transcription request the app sent
// for itemID, checks its shape, and answers it the way an endpoint that honours
// the request would: the response echoes the request metadata, streams text,
// and completes. The request must never carry billed transcription.
func answerOutOfBand(t *testing.T, ai *fakeAI, itemID, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var req map[string]any
	for req == nil {
		if got := ai.outOfBandRequests(); len(got) > 0 {
			req = got[0]
		} else if time.Now().After(deadline) {
			t.Fatal("app sent no out-of-band transcription request")
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if hasKey(req, "transcription") {
		t.Fatalf("out-of-band request enables billed transcription: %v", req)
	}
	r, _ := req["response"].(map[string]any)
	in, _ := r["input"].([]any)
	ref, _ := in[0].(map[string]any)
	meta, _ := r["metadata"].(map[string]any)
	if r["conversation"] != "none" || len(r["output_modalities"].([]any)) != 1 || r["output_modalities"].([]any)[0] != "text" ||
		ref["type"] != "item_reference" || ref["id"] != itemID || meta["callharbor_purpose"] != "caller_transcript" {
		t.Fatalf("out-of-band request shape = %v", req)
	}
	const respID = "resp_oob_1"
	ai.sendRaw(t, map[string]any{"type": "response.created", "event_id": "ev_oc", "response": map[string]any{
		"id": respID, "status": "in_progress", "metadata": meta, "output": []any{}}})
	half := len(text) / 2
	ai.sendRaw(t, map[string]any{"type": "response.output_text.delta", "event_id": "ev_od1", "response_id": respID, "item_id": "oob_item", "output_index": 0, "content_index": 0, "delta": text[:half]})
	ai.sendRaw(t, map[string]any{"type": "response.output_text.delta", "event_id": "ev_od2", "response_id": respID, "item_id": "oob_item", "output_index": 0, "content_index": 0, "delta": text[half:]})
	ai.sendRaw(t, map[string]any{"type": "response.output_text.done", "event_id": "ev_odn", "response_id": respID, "item_id": "oob_item", "output_index": 0, "content_index": 0, "text": text})
	ai.sendRaw(t, map[string]any{"type": "response.done", "event_id": "ev_rd", "response": map[string]any{
		"id": respID, "status": "completed", "metadata": meta, "output": []any{}}})
}

// rejectOutOfBand answers the out-of-band transcription request for the caller
// item the way an endpoint that does not support it would: a protocol error
// whose event_id names the client request. The request must carry no billed
// transcription.
func rejectOutOfBand(t *testing.T, ai *fakeAI) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var req map[string]any
	for req == nil {
		if got := ai.outOfBandRequests(); len(got) > 0 {
			req = got[0]
		} else if time.Now().After(deadline) {
			t.Fatal("app sent no out-of-band transcription request")
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if hasKey(req, "transcription") {
		t.Fatalf("out-of-band request enables billed transcription: %v", req)
	}
	eventID, _ := req["event_id"].(string)
	ai.sendRaw(t, map[string]any{"type": "error", "event_id": "ev_rej_app", "error": map[string]any{
		"type": "invalid_request_error", "code": "unsupported_parameter", "message": "out-of-band response not supported", "event_id": eventID}})
}

// TestCallTranscriptOutOfBandRejected is the failure path on the app wiring. The
// endpoint rejects the caller transcription. The call must keep running, agent
// audio must still reach the media stream, the caller turn must be failed and
// flagged, the page must be two-party incomplete, and no billed transcription
// may be requested.
func TestCallTranscriptOutOfBandRejected(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-rej")
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Book a table",
	}, withIdempotency(h, "transcript-rej"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-init-rej", "v3:ctl-rej")
	env.signedWebhook(callID, "call.answered", "evt-ans-rej", "v3:ctl-rej")
	mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	sendStart(t, mediaConn, "v3:ctl-rej")
	sendMedia(t, mediaConn, "AAAAAAAAAAA=")
	waitEvents(t, ai, 2)
	ai.sendRaw(t, map[string]any{"type": "input_audio_buffer.speech_started", "event_id": "ev_s_rej", "audio_start_ms": 1200, "item_id": "msg_r"})
	ai.sendRaw(t, map[string]any{"type": "conversation.item.added", "event_id": "ev_add_rej", "item": map[string]any{
		"id": "msg_r", "type": "message", "status": "completed", "role": "user",
		"content": []map[string]any{{"type": "input_audio"}}}})
	rejectOutOfBand(t, ai)

	// Agent audio after the rejection must still reach the caller.
	const agentChunk = "QUFBQUFBQUFBQQ=="
	ai.sendAgentAudio(t, agentChunk)
	assertStreamMediaPayload(t, mediaConn, agentChunk)

	// The call is not ended by the rejection: it is still running and the
	// media stream is still open, so the agent chunk above was delivered live.
	if status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h); status != http.StatusOK {
		t.Fatalf("call read = %d %s", status, body)
	} else if st, _ := decodeObject(t, body)["technical_status"].(string); st == "ended" || st == "failed" || st == "completed" {
		t.Fatalf("call technical_status after out-of-band rejection = %q, want still running", st)
	}
	waitTurns(t, env.dataDir, callID, 1)
	_ = mediaConn.Close()
	waitTranscriptFinalized(t, env.dataDir, callID)

	url := env.base + "/api/v1/calls/" + callID + "/transcript"
	status, _, body = send(t, env.client, http.MethodGet, url, nil, env.admin.headers(false))
	if status != http.StatusOK {
		t.Fatalf("owner transcript = %d %s", status, body)
	}
	var page transcriptPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	var caller *transcriptTurn
	for i := range page.Turns {
		if page.Turns[i].Speaker == "callee" {
			caller = &page.Turns[i]
		}
	}
	if caller == nil {
		t.Fatalf("no caller turn in %+v", page.Turns)
	}
	if caller.Fidelity != "unavailable" || !slices.Contains(caller.QualityFlags, "caller_transcript_failed") {
		t.Fatalf("caller turn = %+v, want unavailable with caller_transcript_failed", caller)
	}
	if caller.Text != "" {
		t.Fatalf("rejected caller turn carries text %q", caller.Text)
	}
	if !slices.Contains(page.QualityFlags, "two_party_incomplete") {
		t.Fatalf("page flags = %v, want two_party_incomplete", page.QualityFlags)
	}
	if ai.sawTranscriptionRequest() {
		t.Fatal("provider session requested billed transcription after rejection (#114 forbids it)")
	}

	other := createUser(t, env.admin, "mallory")
	status, _, body = send(t, env.client, http.MethodGet, url, nil, other.headers(false))
	if status != http.StatusNotFound {
		t.Fatalf("foreign transcript = %d %s, want 404", status, body)
	}
}
