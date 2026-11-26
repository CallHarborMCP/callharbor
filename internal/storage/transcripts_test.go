package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// txRevision is a final, played callee turn with provider timing. Each test
// changes only the fields it exercises.
func txRevision(seq int, text string) TranscriptRevision {
	return TranscriptRevision{
		Seq: seq, Speaker: SpeakerCallee, Finality: FinalityFinal, StartedMS: int64(seq) * 100,
		Text: text, Fidelity: "played", TimingSource: TimingProvider,
	}
}

func TestRecordTranscriptAssignsRevisionsPerSeq(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "rev.db"))
	seedTenants(t, s)

	for _, text := range []string{"first", "second", "third"} {
		if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{txRevision(1, text)}, nil); err != nil {
			t.Fatalf("RecordTranscript(%s): %v", text, err)
		}
	}
	if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{txRevision(2, "other")}, nil); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, "SELECT MAX(revision) FROM transcript_turns WHERE call_id = 'alice-call' AND seq = 1"); n != 3 {
		t.Fatalf("seq 1 max revision = %d, want 3", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM transcript_turns WHERE call_id = 'alice-call' AND seq = 1"); n != 3 {
		t.Fatalf("seq 1 rows = %d, want 3 immutable revisions", n)
	}
	if n := scalarInt(t, s, "SELECT revision FROM transcript_turns WHERE call_id = 'alice-call' AND seq = 2"); n != 1 {
		t.Fatalf("seq 2 revision = %d, want 1 (revisions are per seq)", n)
	}

	turns, err := s.ListTranscriptTurns(ctx, "alice", "alice-call", 0, 10)
	if err != nil {
		t.Fatalf("ListTranscriptTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("listed %d turns, want 2 (latest revision per seq)", len(turns))
	}
	if turns[0].Seq != 1 || turns[0].Revision != 3 || turns[0].Text != "third" {
		t.Fatalf("seq 1 latest = %+v, want revision 3 text third", turns[0])
	}
	if turns[1].Seq != 2 || turns[1].Revision != 1 {
		t.Fatalf("seq 2 latest = %+v", turns[1])
	}
}

func TestRecordTranscriptRoundTripsAllFields(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "fields.db"))
	seedTenants(t, s)

	end, gen, played := int64(1500), int64(900), int64(850)
	in := TranscriptRevision{
		Seq: 1, Speaker: SpeakerAssistant, Finality: FinalityFinal, StartedMS: 200, EndedMS: &end,
		Text: "hello there", ProviderItemID: "item_9", Fidelity: "interrupted_text_includes_unplayed",
		TimingSource: TimingProvider, SourceIDs: []string{"evt_1", "evt_2"}, CorrelationID: "corr-1",
		GeneratedMS: &gen, PlayedMS: &played, PlayedIsEstimate: true, Quality: []string{"agent_text_includes_unplayed"},
	}
	if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{in}, nil); err != nil {
		t.Fatal(err)
	}
	turns, err := s.ListTranscriptTurns(ctx, "alice", "alice-call", 0, 10)
	if err != nil || len(turns) != 1 {
		t.Fatalf("list = %d turns, %v", len(turns), err)
	}
	got := turns[0]
	if got.Speaker != SpeakerAssistant || got.Finality != FinalityFinal || got.StartedMS != 200 || got.Text != "hello there" {
		t.Fatalf("core fields = %+v", got.TranscriptRevision)
	}
	if got.EndedMS == nil || *got.EndedMS != 1500 || got.GeneratedMS == nil || *got.GeneratedMS != 900 ||
		got.PlayedMS == nil || *got.PlayedMS != 850 || !got.PlayedIsEstimate {
		t.Fatalf("timing fields = %+v", got.TranscriptRevision)
	}
	if got.ProviderItemID != "item_9" || got.CorrelationID != "corr-1" || got.Fidelity != in.Fidelity || got.TimingSource != TimingProvider {
		t.Fatalf("provenance fields = %+v", got.TranscriptRevision)
	}
	if strings.Join(got.SourceIDs, ",") != "evt_1,evt_2" || strings.Join(got.Quality, ",") != "agent_text_includes_unplayed" {
		t.Fatalf("arrays = %v / %v", got.SourceIDs, got.Quality)
	}
}

func TestRecordTranscriptNullsAndEmptyArrays(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "nulls.db"))
	seedTenants(t, s)

	if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{txRevision(1, "bare")}, nil); err != nil {
		t.Fatal(err)
	}
	turns, err := s.ListTranscriptTurns(ctx, "alice", "alice-call", 0, 10)
	if err != nil || len(turns) != 1 {
		t.Fatalf("list = %d, %v", len(turns), err)
	}
	got := turns[0]
	if got.EndedMS != nil || got.GeneratedMS != nil || got.PlayedMS != nil || got.ProviderItemID != "" || got.CorrelationID != "" {
		t.Fatalf("unset optionals should read back empty/nil: %+v", got.TranscriptRevision)
	}
	if got.SourceIDs == nil || got.Quality == nil || len(got.SourceIDs) != 0 || len(got.Quality) != 0 {
		t.Fatalf("arrays must be non-nil and empty: %v / %v", got.SourceIDs, got.Quality)
	}
}

func TestListTranscriptTurnsPagesAfterSeq(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "page.db"))
	seedTenants(t, s)

	var turns []TranscriptRevision
	for seq := 1; seq <= 5; seq++ {
		turns = append(turns, txRevision(seq, "t"))
	}
	if err := s.RecordTranscript(ctx, "alice-call", "alice", turns, nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		after, limit int
		want         string
	}{
		{0, 2, "1,2"}, {2, 2, "3,4"}, {4, 2, "5"}, {5, 2, ""}, {0, 100, "1,2,3,4,5"},
	}
	for _, c := range cases {
		got, err := s.ListTranscriptTurns(ctx, "alice", "alice-call", c.after, c.limit)
		if err != nil {
			t.Fatalf("after %d limit %d: %v", c.after, c.limit, err)
		}
		var seqs []string
		for _, turn := range got {
			seqs = append(seqs, strconv.Itoa(turn.Seq))
		}
		if strings.Join(seqs, ",") != c.want {
			t.Fatalf("after %d limit %d = %v, want %q", c.after, c.limit, seqs, c.want)
		}
	}
	if _, err := s.ListTranscriptTurns(ctx, "alice", "alice-call", 0, 0); err == nil {
		t.Fatal("zero limit accepted")
	}
}

func TestTranscriptReadsAreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "tenant.db"))
	seedTenants(t, s)
	if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{txRevision(1, "private")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTranscript(ctx, "alice-call", "alice", nil, []CallEvent{{Kind: TranscriptStartedKind, PayloadJSON: `{"quality_flags":["x"]}`}}); err != nil {
		t.Fatal(err)
	}

	turns, err := s.ListTranscriptTurns(ctx, "bob", "alice-call", 0, 10)
	if err != nil || len(turns) != 0 {
		t.Fatalf("bob listed %d turns of alice's call, err %v", len(turns), err)
	}
	sum, err := s.GetTranscriptSummary(ctx, "bob", "alice-call")
	if err != nil || sum.Recorded {
		t.Fatalf("bob summary = %+v, %v; want not recorded", sum, err)
	}
}

func TestRecordTranscriptRejectsWrongOwnerAndBadFidelity(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "owner.db"))
	seedTenants(t, s)

	if err := s.RecordTranscript(ctx, "alice-call", "bob", []TranscriptRevision{txRevision(1, "x")}, nil); err == nil {
		t.Fatal("wrong owner accepted")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM transcript_turns"); n != 0 {
		t.Fatalf("rows written for wrong owner: %d", n)
	}
	bad := txRevision(1, "x")
	bad.Fidelity = "guessed"
	if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{bad}, nil); err == nil {
		t.Fatal("unknown fidelity accepted")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM transcript_turns"); n != 0 {
		t.Fatalf("rows written despite rejected fidelity: %d", n)
	}
}

func TestTranscriptRowsAreImmutable(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "immutable.db"))
	seedTenants(t, s)
	if err := s.RecordTranscript(ctx, "alice-call", "alice", []TranscriptRevision{txRevision(1, "fixed")}, nil); err != nil {
		t.Fatal(err)
	}
	expectExecError(t, s, "UPDATE transcript_turns SET text = 'forged'")
	expectExecError(t, s, "UPDATE transcript_turns SET fidelity = 'played'")
	expectExecError(t, s, "DELETE FROM transcript_turns")
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM transcript_turns WHERE text = 'fixed'"); n != 1 {
		t.Fatalf("original revision lost: %d", n)
	}
}

func TestRecordTranscriptSkipsReplayedProviderEvents(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "replay.db"))
	seedTenants(t, s)

	replayed := CallEvent{Kind: "provider.event", Provider: "telnyx", ProviderEventID: "evt-1", PayloadJSON: "{}"}
	for i := 0; i < 2; i++ {
		if err := s.RecordTranscript(ctx, "alice-call", "alice", nil, []CallEvent{replayed}); err != nil {
			t.Fatal(err)
		}
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events WHERE provider_event_id = 'evt-1'"); n != 1 {
		t.Fatalf("replayed provider event recorded %d times, want 1", n)
	}
	unkeyed := CallEvent{Kind: "provider.event", PayloadJSON: "{}"}
	for i := 0; i < 2; i++ {
		if err := s.RecordTranscript(ctx, "alice-call", "alice", nil, []CallEvent{unkeyed}); err != nil {
			t.Fatal(err)
		}
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events WHERE provider_event_id IS NULL AND kind = 'provider.event'"); n != 2 {
		t.Fatalf("events without provider id = %d, want 2 (never deduplicated)", n)
	}
}

func TestGetTranscriptSummaryStartedFinalizedAndNone(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "summary.db"))
	seedTenants(t, s)

	none, err := s.GetTranscriptSummary(ctx, "alice", "alice-call")
	if err != nil || none.Recorded || none.Finalized || none.QualityFlags == nil || len(none.QualityFlags) != 0 {
		t.Fatalf("no media run = %+v, %v", none, err)
	}

	started := CallEvent{Kind: TranscriptStartedKind, PayloadJSON: `{"quality_flags":["two_party_incomplete"]}`}
	if err := s.RecordTranscript(ctx, "alice-call", "alice", nil, []CallEvent{started}); err != nil {
		t.Fatal(err)
	}
	sum, err := s.GetTranscriptSummary(ctx, "alice", "alice-call")
	if err != nil || !sum.Recorded || sum.Finalized || strings.Join(sum.QualityFlags, ",") != "two_party_incomplete" {
		t.Fatalf("started = %+v, %v", sum, err)
	}

	finalized := CallEvent{Kind: TranscriptFinalizedKind, PayloadJSON: `{"quality_flags":["corrected"]}`}
	if err := s.RecordTranscript(ctx, "alice-call", "alice", nil, []CallEvent{finalized}); err != nil {
		t.Fatal(err)
	}
	sum, err = s.GetTranscriptSummary(ctx, "alice", "alice-call")
	if err != nil || !sum.Recorded || !sum.Finalized || strings.Join(sum.QualityFlags, ",") != "corrected" {
		t.Fatalf("finalized = %+v, %v; want the latest summary event", sum, err)
	}
}

func TestTranscriptColumnChecksRejectBadRows(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "checks.db"))
	seedTenants(t, s)
	// Each case is the fidelity, timing_source, source_ids_json, correlation_id,
	// generated_ms, played_is_estimate and quality_json values, in that order.
	cases := map[string]string{
		"unknown fidelity":       `'guessed', 'provider', '[]', NULL, NULL, 0, '[]'`,
		"unknown timing source":  `'played', 'wallclock', '[]', NULL, NULL, 0, '[]'`,
		"object source ids":      `'played', 'provider', '{}', NULL, NULL, 0, '[]'`,
		"invalid quality json":   `'played', 'provider', '[]', NULL, NULL, 0, 'nope'`,
		"estimate flag not 0/1":  `'played', 'provider', '[]', NULL, NULL, 2, '[]'`,
		"negative generated ms":  `'played', 'provider', '[]', NULL, -1, 0, '[]'`,
		"empty correlation id":   `'played', 'provider', '[]', '', NULL, 0, '[]'`,
		"object quality entries": `'played', 'provider', '[]', NULL, NULL, 0, '{}'`,
	}
	seq := 1
	for _, values := range cases {
		stmt := fmt.Sprintf(`INSERT INTO transcript_turns (call_id, owner_user_id, seq, revision, speaker, finality, started_ms, text, recorded_at,
			fidelity, timing_source, source_ids_json, correlation_id, generated_ms, played_is_estimate, quality_json)
			VALUES ('alice-call', 'alice', %d, 1, 'callee', 'final', 0, 'x', '%s', %s)`, seq, testTime, values)
		seq++
		expectExecError(t, s, stmt)
	}
}
