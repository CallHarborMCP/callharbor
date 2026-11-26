package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// seedTranscript writes a started summary and two callee turns on alice's
// call. The summary keeps the run open, so the page is not finalized.
func seedTranscript(t *testing.T, f *fixture, turns int) {
	t.Helper()
	revs := make([]storage.TranscriptRevision, 0, turns)
	for seq := 1; seq <= turns; seq++ {
		revs = append(revs, storage.TranscriptRevision{
			Seq: seq, Speaker: storage.SpeakerCallee, Finality: storage.FinalityFinal,
			StartedMS: int64(seq) * 100, Text: fmt.Sprintf("turn %d", seq),
			Fidelity: "played", TimingSource: storage.TimingProvider,
		})
	}
	started := storage.CallEvent{Kind: storage.TranscriptStartedKind, PayloadJSON: `{"quality_flags":["two_party_incomplete"]}`}
	if err := f.store.RecordTranscript(context.Background(), "alice-call", "alice", revs, []storage.CallEvent{started}); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
}

func TestV1TranscriptOwnerPageMatchesContract(t *testing.T) {
	f := newFixture(t)
	seedTranscript(t, f, 2)

	rec := get(t, f.mux, "/api/v1/calls/alice-call/transcript", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcript = %d %s", rec.Code, rec.Body.String())
	}
	validateBody(t, compileComponent(t, "TranscriptPage"), rec)
	m := decodeMap(t, rec)
	if m["call_id"] != "alice-call" || m["finalized"] != false || m["next_cursor"] != "" {
		t.Fatalf("page = %v", m)
	}
	if !strings.Contains(rec.Body.String(), `"transcript_in_progress"`) || !strings.Contains(rec.Body.String(), `"two_party_incomplete"`) {
		t.Fatalf("flags missing from %s", rec.Body.String())
	}
	turns, _ := m["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
}

func TestV1TranscriptNoMediaRunIsEmptyAndFlagged(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, "/api/v1/calls/alice-call/transcript", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("transcript = %d %s", rec.Code, rec.Body.String())
	}
	validateBody(t, compileComponent(t, "TranscriptPage"), rec)
	if !strings.Contains(rec.Body.String(), `"turns":[]`) || !strings.Contains(rec.Body.String(), `"quality_flags":["no_media_session"]`) {
		t.Fatalf("empty page = %s", rec.Body.String())
	}
}

func TestV1TranscriptPagesWithOpaqueCursor(t *testing.T) {
	f := newFixture(t)
	seedTranscript(t, f, 101)

	first := get(t, f.mux, "/api/v1/calls/alice-call/transcript", f.aliceCk)
	if first.Code != http.StatusOK {
		t.Fatalf("first page = %d %s", first.Code, first.Body.String())
	}
	m := decodeMap(t, first)
	cursor, _ := m["next_cursor"].(string)
	if cursor == "" || len(m["turns"].([]any)) != 100 {
		t.Fatalf("first page cursor %q, turns %d", cursor, len(m["turns"].([]any)))
	}

	second := get(t, f.mux, "/api/v1/calls/alice-call/transcript?cursor="+cursor, f.aliceCk)
	if second.Code != http.StatusOK {
		t.Fatalf("second page = %d %s", second.Code, second.Body.String())
	}
	validateBody(t, compileComponent(t, "TranscriptPage"), second)
	m2 := decodeMap(t, second)
	turns := m2["turns"].([]any)
	if len(turns) != 1 || m2["next_cursor"] != "" {
		t.Fatalf("second page turns %d cursor %v", len(turns), m2["next_cursor"])
	}
}

func TestV1TranscriptForeignAndMissingAreNotFound(t *testing.T) {
	f := newFixture(t)
	seedTranscript(t, f, 1)
	for _, path := range []string{"/api/v1/calls/alice-call/transcript", "/api/v1/calls/missing/transcript"} {
		ck := f.bobCk
		if strings.Contains(path, "missing") {
			ck = f.aliceCk
		}
		rec := get(t, f.mux, path, ck)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
		validateBody(t, compileComponent(t, "Error"), rec)
		if strings.Contains(rec.Body.String(), "turn 1") {
			t.Fatalf("turn text leaked on refusal: %s", rec.Body.String())
		}
	}
}

func TestV1TranscriptRequiresSessionAndValidCursor(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, "/api/v1/calls/alice-call/transcript", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d %s", rec.Code, rec.Body.String())
	}

	bad := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":0}`))
	rec = get(t, f.mux, "/api/v1/calls/alice-call/transcript?cursor="+bad, f.aliceCk)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("bad cursor = %d %s", rec.Code, rec.Body.String())
	}

	long := strings.Repeat("a", 513)
	rec = get(t, f.mux, "/api/v1/calls/alice-call/transcript?cursor="+long, f.aliceCk)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("oversized cursor = %d %s", rec.Code, rec.Body.String())
	}
}
