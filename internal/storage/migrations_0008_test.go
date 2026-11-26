package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// openAtVersion7 returns a store at schema 7 with a legacy transcript turn
// already written, so the 0008 columns are added to rows that predate them.
func openAtVersion7(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "m8.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 7 {
		t.Fatalf("schema version = %d, want 7", v)
	}
	seedTenants(t, s)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO transcript_turns
		(call_id, owner_user_id, seq, revision, speaker, finality, started_ms, text, recorded_at)
		VALUES ('alice-call', 'alice', 1, 1, 'callee', 'final', 0, 'legacy', ?)`, testTime); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigration0008DefaultsExistingTurns(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion7(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v < 8 {
		t.Fatalf("schema version = %d, want at least 8", v)
	}
	turns, err := s.ListTranscriptTurns(ctx, "alice", "alice-call", 0, 10)
	if err != nil || len(turns) != 1 {
		t.Fatalf("legacy turn not listed: %d, %v", len(turns), err)
	}
	got := turns[0]
	if got.Text != "legacy" || got.Fidelity != "unavailable" || got.TimingSource != TimingLocalReceive {
		t.Fatalf("legacy defaults = %+v", got.TranscriptRevision)
	}
	if got.SourceIDs == nil || len(got.SourceIDs) != 0 || got.Quality == nil || len(got.Quality) != 0 {
		t.Fatalf("legacy arrays = %v / %v, want empty", got.SourceIDs, got.Quality)
	}
	if got.CorrelationID != "" || got.EndedMS != nil || got.GeneratedMS != nil || got.PlayedMS != nil || got.PlayedIsEstimate {
		t.Fatalf("legacy nullable provenance should be empty: %+v", got.TranscriptRevision)
	}
}

func TestMigration0008RoundTripKeepsTurns(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion7(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(ctx, 7); err != nil {
		t.Fatalf("rollback 0008: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 7 {
		t.Fatalf("schema version after rollback = %d, want 7", v)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM transcript_turns WHERE text = 'legacy'"); n != 1 {
		t.Fatalf("turn lost on rollback: %d rows", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('transcript_turns') WHERE name IN ('fidelity', 'timing_source', 'source_ids_json', 'correlation_id', 'generated_ms', 'played_ms', 'played_is_estimate', 'quality_json')"); n != 0 {
		t.Fatalf("0008 columns remain after rollback: %d", n)
	}
	if n := scalarInt(t, s, "PRAGMA foreign_keys"); n != 1 {
		t.Fatalf("foreign_keys after rollback = %d, want 1", n)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate after rollback: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM transcript_turns WHERE fidelity = 'unavailable' AND text = 'legacy'"); n != 1 {
		t.Fatalf("turn not defaulted after re-migrate: %d", n)
	}
}

func TestMigration0008ChecksSurviveUpgrade(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion7(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	expectExecError(t, s, `INSERT INTO transcript_turns (call_id, owner_user_id, seq, revision, speaker, finality, started_ms, text, recorded_at, fidelity)
		VALUES ('alice-call', 'alice', 2, 1, 'callee', 'final', 0, 'x', '`+testTime+`', 'guessed')`)
	expectExecError(t, s, `INSERT INTO transcript_turns (call_id, owner_user_id, seq, revision, speaker, finality, started_ms, text, recorded_at, source_ids_json)
		VALUES ('alice-call', 'alice', 3, 1, 'callee', 'final', 0, 'x', '`+testTime+`', '{}')`)
	expectExecError(t, s, `INSERT INTO transcript_turns (call_id, owner_user_id, seq, revision, speaker, finality, started_ms, text, recorded_at, played_ms)
		VALUES ('alice-call', 'alice', 4, 1, 'callee', 'final', 0, 'x', '`+testTime+`', -5)`)
}
