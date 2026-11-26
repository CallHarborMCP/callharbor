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
		check    func(t *testing.T, p transcriptPage)
	}{
		{
			protocol: realtime.ProtocolRealtime,
			events: []map[string]any{
				{"type": "response.output_audio.delta", "event_id": "ev_a1", "delta": "QUFBQUFBQUFBQQ==", "item_id": "msg_1", "response_id": "resp_1"},
				{"type": "response.output_audio_transcript.delta", "event_id": "ev_t1", "delta": "Hello, this is ", "item_id": "msg_1", "response_id": "resp_1"},
				{"type": "response.output_audio_transcript.done", "event_id": "ev_t2", "transcript": "Hello, this is the clinic.", "item_id": "msg_1", "response_id": "resp_1"},
				{"type": "input_audio_buffer.speech_started", "event_id": "ev_s1", "audio_start_ms": 1200, "item_id": "msg_2"},
			},
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
				if c.Speaker != "callee" || c.Text != "" || c.Fidelity != "unavailable" || !slices.Contains(c.QualityFlags, "caller_transcript_unavailable") {
					t.Fatalf("caller turn = %+v", c)
				}
				for _, f := range []string{"caller_transcript_unavailable", "two_party_incomplete"} {
					if !slices.Contains(p.QualityFlags, f) {
						t.Fatalf("page flags %v lack %s", p.QualityFlags, f)
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
