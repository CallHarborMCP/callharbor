package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// transcriptFixture adds two turns and a finalized summary to alice's call
// c-a1. Bob's call c-b1 has no transcript, so a foreign read has nothing to leak
// beyond the refusal itself.
func transcriptFixture() *fakeStore {
	f := fixtureStore()
	f.turns = map[string][]storage.TranscriptTurn{
		"c-a1": {
			{TranscriptRevision: storage.TranscriptRevision{
				Seq: 1, Speaker: storage.SpeakerAssistant, Finality: storage.FinalityFinal,
				StartedMS: 0, Text: "hello from alice agent", Fidelity: "played",
				TimingSource: storage.TimingProvider, SourceIDs: []string{}, Quality: []string{},
			}, Revision: 1, RecordedAt: tsNewer},
			{TranscriptRevision: storage.TranscriptRevision{
				Seq: 2, Speaker: storage.SpeakerCallee, Finality: storage.FinalityFinal,
				StartedMS: 900, Text: "callee reply", Fidelity: "played",
				TimingSource: storage.TimingLocalReceive, SourceIDs: []string{}, Quality: []string{},
			}, Revision: 1, RecordedAt: tsNewer},
		},
	}
	f.summary = map[string]storage.TranscriptSummary{
		"c-a1": {Recorded: true, Finalized: true, QualityFlags: []string{"turns_grouped_locally"}},
	}
	return f
}

func TestToolGetTranscriptOwnerReadsPageAndAudits(t *testing.T) {
	store := transcriptFixture()
	logger, _ := captureLogger()
	ts := testServer(t, store, logger, testTokens{"tok-alice": tokenFor("alice", service.ScopeRead)})
	sess := connect(t, ts, "tok-alice")

	page := decodeStructured[service.TranscriptPage](t, call(t, sess, ToolGetTranscript, map[string]any{"call_id": "c-a1"}))
	if page.CallID != "c-a1" || len(page.Turns) != 2 || !page.Finalized || page.NextCursor != "" {
		t.Fatalf("page = %+v", page)
	}
	if strings.Join(page.QualityFlags, ",") != "turns_grouped_locally" {
		t.Fatalf("flags = %v", page.QualityFlags)
	}
	if page.Turns[1].Speaker != storage.SpeakerCallee || page.Turns[1].TimingSource != storage.TimingLocalReceive {
		t.Fatalf("second turn = %+v", page.Turns[1])
	}
	if len(store.audits) != 1 || store.audits[0].Action != "transcript.read" || store.audits[0].Outcome != "success" {
		t.Fatalf("audit = %+v", store.audits)
	}
}

func TestToolGetTranscriptForeignCallIsNotFoundAndAudited(t *testing.T) {
	store := transcriptFixture()
	logger, _ := captureLogger()
	ts := testServer(t, store, logger, testTokens{"tok-alice": tokenFor("alice", service.ScopeRead)})
	sess := connect(t, ts, "tok-alice")

	code, msg, text := failure(t, call(t, sess, ToolGetTranscript, map[string]any{"call_id": "c-b1"}))
	if code != "not_found" || msg != "call not found" {
		t.Fatalf("envelope = %q %q", code, msg)
	}
	if strings.Contains(text, "hello from") {
		t.Fatalf("turn text leaked: %s", text)
	}
	if len(store.audits) != 1 || store.audits[0].Outcome != "denied" {
		t.Fatalf("audit = %+v", store.audits)
	}
}

func TestToolGetTranscriptRequiresReadScope(t *testing.T) {
	logger, _ := captureLogger()
	ts := testServer(t, transcriptFixture(), logger, testTokens{"tok-call": tokenFor("alice", service.ScopeCall)})
	sess := connect(t, ts, "tok-call")

	code, _, _ := failure(t, call(t, sess, ToolGetTranscript, map[string]any{"call_id": "c-a1"}))
	if code != "insufficient_scope" {
		t.Fatalf("code = %q, want insufficient_scope", code)
	}
}

func TestToolGetTranscriptPagesWithCursor(t *testing.T) {
	store := transcriptFixture()
	var turns []storage.TranscriptTurn
	for seq := 1; seq <= service.MaxLimit+1; seq++ {
		turns = append(turns, storage.TranscriptTurn{
			TranscriptRevision: storage.TranscriptRevision{
				Seq: seq, Speaker: storage.SpeakerCallee, Finality: storage.FinalityFinal,
				StartedMS: int64(seq), Text: fmt.Sprintf("t%d", seq), Fidelity: "played",
				TimingSource: storage.TimingProvider, SourceIDs: []string{}, Quality: []string{},
			},
			Revision: 1, RecordedAt: tsNewer,
		})
	}
	store.turns["c-a1"] = turns
	logger, _ := captureLogger()
	ts := testServer(t, store, logger, testTokens{"tok-alice": tokenFor("alice", service.ScopeRead)})
	sess := connect(t, ts, "tok-alice")

	first := decodeStructured[service.TranscriptPage](t, call(t, sess, ToolGetTranscript, map[string]any{"call_id": "c-a1"}))
	if len(first.Turns) != service.MaxLimit || first.NextCursor == "" {
		t.Fatalf("first page = %d turns, cursor %q", len(first.Turns), first.NextCursor)
	}
	second := decodeStructured[service.TranscriptPage](t, call(t, sess, ToolGetTranscript, map[string]any{"call_id": "c-a1", "cursor": first.NextCursor}))
	if len(second.Turns) != 1 || second.Turns[0].Seq != service.MaxLimit+1 || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
}

// TestToolGetTranscriptOutputMatchesManifestShape checks the structured result
// has every required key, so the manifest outputSchema describes what is served.
func TestToolGetTranscriptOutputMatchesManifestShape(t *testing.T) {
	logger, _ := captureLogger()
	ts := testServer(t, transcriptFixture(), logger, testTokens{"tok-alice": tokenFor("alice", service.ScopeRead)})
	sess := connect(t, ts, "tok-alice")

	res := call(t, sess, ToolGetTranscript, map[string]any{"call_id": "c-a1"})
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"call_id", "turns", "quality_flags", "finalized", "next_cursor"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing %s in %s", key, raw)
		}
	}
	turn := m["turns"].([]any)[0].(map[string]any)
	for _, key := range []string{"seq", "revision", "speaker", "finality", "started_ms", "ended_ms", "text", "fidelity", "timing_source", "source_ids", "correlation_id", "provider_item_id", "generated_ms", "played_ms", "played_is_estimate", "quality_flags"} {
		if _, ok := turn[key]; !ok {
			t.Fatalf("turn missing %s: %s", key, raw)
		}
	}
}
