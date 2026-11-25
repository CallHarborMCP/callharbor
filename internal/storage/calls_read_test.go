package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func seedCallRow(t *testing.T, s *Store, id, owner, tel, ai, key, createdAt string) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '+14155550111', 'queued', 300, ?, ?)`,
		id, owner, tel, ai, key, createdAt, createdAt)
	if err != nil {
		t.Fatalf("seed call %s: %v", id, err)
	}
}

func TestListTelephonyProfilesIsOwnerScoped(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	got, err := s.ListTelephonyProfiles(ctx, "alice")
	if err != nil {
		t.Fatalf("ListTelephonyProfiles: %v", err)
	}
	if len(got) != 1 || got[0].ID != "alice-tel" || got[0].Provider != "telnyx" || got[0].CallerNumber != "+14155550100" {
		t.Fatalf("alice profiles = %+v", got)
	}
	if got[0].ConnectionID != "" {
		t.Fatalf("connection id = %q, want empty", got[0].ConnectionID)
	}
	none, err := s.ListTelephonyProfiles(ctx, "nobody")
	if err != nil {
		t.Fatalf("ListTelephonyProfiles(nobody): %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("unknown owner profiles = %#v, want empty non-nil slice", none)
	}
}

func TestListCallsKeysetPaginationAndOwnerScope(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()
	// alice-call is at testTime. Add two calls at the same instant and one newer.
	seedCallRow(t, s, "alice-call-b", "alice", "alice-tel", "alice-ai", "k2", testTime)
	seedCallRow(t, s, "alice-call-new", "alice", "alice-tel", "alice-ai", "k3", "2026-11-24T10:00:00.000Z")
	seedCallRow(t, s, "bob-call", "bob", "bob-tel", "bob-ai", "k4", "2026-11-24T11:00:00.000Z")

	page1, err := s.ListCalls(ctx, "alice", "", nil, 2)
	if err != nil {
		t.Fatalf("ListCalls page1: %v", err)
	}
	wantOrder := []string{"alice-call-new", "alice-call-b"}
	if len(page1) != 2 || page1[0].ID != wantOrder[0] || page1[1].ID != wantOrder[1] {
		t.Fatalf("page1 ids = %v, want %v", ids(page1), wantOrder)
	}
	last := page1[1]
	page2, err := s.ListCalls(ctx, "alice", "", &CallCursor{CreatedAt: last.CreatedAt, ID: last.ID}, 2)
	if err != nil {
		t.Fatalf("ListCalls page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != "alice-call" {
		t.Fatalf("page2 ids = %v, want [alice-call]", ids(page2))
	}
	for _, c := range append(page1, page2...) {
		if c.OwnerUserID != "alice" {
			t.Fatalf("foreign call leaked: %+v", c)
		}
	}
	if _, err := s.ListCalls(ctx, "alice", "", nil, 0); err == nil {
		t.Fatal("ListCalls limit 0 returned nil error")
	}
}

func TestListCallsStatusFilter(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()
	seedCallRow(t, s, "alice-call-b", "alice", "alice-tel", "alice-ai", "k2", "2026-11-24T10:00:00.000Z")
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET status = 'ended', ended_at = ? WHERE id = 'alice-call-b'`, testTime); err != nil {
		t.Fatalf("set status: %v", err)
	}

	queued, err := s.ListCalls(ctx, "alice", "queued", nil, 10)
	if err != nil {
		t.Fatalf("ListCalls queued: %v", err)
	}
	if len(queued) != 1 || queued[0].ID != "alice-call" {
		t.Fatalf("queued ids = %v, want [alice-call]", ids(queued))
	}
	ended, err := s.ListCalls(ctx, "alice", "ended", nil, 10)
	if err != nil {
		t.Fatalf("ListCalls ended: %v", err)
	}
	if len(ended) != 1 || ended[0].ID != "alice-call-b" {
		t.Fatalf("ended ids = %v, want [alice-call-b]", ids(ended))
	}
	none, err := s.ListCalls(ctx, "alice", "failed", nil, 10)
	if err != nil {
		t.Fatalf("ListCalls failed: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("failed ids = %v, want none", ids(none))
	}
}

func TestGetCallReportsRecordingPresence(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	c, err := s.GetCall(ctx, "alice", "alice-call")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if c.HasTranscript || c.RecordingStatus != "" || c.Outcome != "" || c.EndedAt != "" {
		t.Fatalf("fresh call flags = %+v", c)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO recordings
		(id, call_id, owner_user_id, status, storage_path, key_version, created_at, finalized_at)
		VALUES ('alice-rec', 'alice-call', 'alice', 'complete', 'alice/rec.bin', 1, ?, ?)`, testTime, testTime); err != nil {
		t.Fatalf("seed recording: %v", err)
	}
	c, err = s.GetCall(ctx, "alice", "alice-call")
	if err != nil {
		t.Fatalf("GetCall after seed: %v", err)
	}
	if c.RecordingStatus != "complete" {
		t.Fatalf("recording status = %q, want complete", c.RecordingStatus)
	}
}

func TestGetCallHasTranscriptRequiresFinalTurn(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx, `INSERT INTO transcript_turns
		(call_id, owner_user_id, seq, speaker, finality, started_ms, text, recorded_at)
		VALUES ('alice-call', 'alice', 1, 'callee', 'partial', 0, 'hel', ?)`, testTime); err != nil {
		t.Fatalf("seed partial transcript: %v", err)
	}
	c, err := s.GetCall(ctx, "alice", "alice-call")
	if err != nil {
		t.Fatalf("GetCall partial-only: %v", err)
	}
	if c.HasTranscript {
		t.Fatal("partial-only transcript reported HasTranscript = true")
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO transcript_turns
		(call_id, owner_user_id, seq, revision, speaker, finality, started_ms, text, recorded_at)
		VALUES ('alice-call', 'alice', 1, 2, 'callee', 'final', 0, 'hello', ?)`, testTime); err != nil {
		t.Fatalf("seed final transcript: %v", err)
	}
	c, err = s.GetCall(ctx, "alice", "alice-call")
	if err != nil {
		t.Fatalf("GetCall final: %v", err)
	}
	if !c.HasTranscript {
		t.Fatal("final transcript reported HasTranscript = false")
	}
}

func TestGetCallHidesForeignAndMissingCalls(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	if _, err := s.GetCall(ctx, "bob", "alice-call"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("cross-owner err = %v, want ErrCallNotFound", err)
	}
	if _, err := s.GetCall(ctx, "alice", "missing"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("missing err = %v, want ErrCallNotFound", err)
	}
}

func ids(calls []Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.ID)
	}
	return out
}
