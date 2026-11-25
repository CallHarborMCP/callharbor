package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// openAtVersion6 returns a store migrated to 0006 with the tenant fixtures
// loaded, so the 0007 rebuild runs against rows written by the old schema.
func openAtVersion6(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "m7.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(ctx, 6); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 6 {
		t.Fatalf("schema version = %d, want 6", v)
	}
	seedTenants(t, s)
	if err := s.UpsertTelephonyGrant(ctx, TelephonyGrant{
		ProfileID: "alice-tel", GranteeUserID: "bob", GrantedByUserID: "alice", CreatedAt: testTime,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigration0007RebuildsCallsAndKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion6(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to head: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v < 7 {
		t.Fatalf("schema version = %d, want at least 7", v)
	}
	if n := scalarInt(t, s, "PRAGMA foreign_keys"); n != 1 {
		t.Fatalf("foreign_keys after migration = %d, want 1", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 1 {
		t.Fatalf("existing call lost during rebuild: %d rows", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('calls_owner_created_idx', 'calls_owner_status_idx')"); n != 2 {
		t.Fatalf("calls indexes present = %d, want 2", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'calls_guarded_delete'"); n != 1 {
		t.Fatal("calls_guarded_delete trigger missing after rebuild")
	}
	// The guarded delete still refuses a direct delete.
	expectExecError(t, s, "DELETE FROM calls WHERE id = 'alice-call'")
	// Child tables keep referencing calls and still enforce it.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at)
		VALUES ('alice-call', 'alice', 1, 'created', '`+testTime+`', '`+testTime+`')`); err != nil {
		t.Fatalf("child event insert after rebuild: %v", err)
	}
	expectExecError(t, s, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at)
		VALUES ('missing-call', 'alice', 1, 'created', '`+testTime+`', '`+testTime+`')`)
}

func TestMigration0007AllowsGranteeCallsAndStillRefusesProfileDelete(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion6(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// bob places a call with alice's granted profile and his own AI profile.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bob-via-grant', 'bob', 'alice-tel', 'bob-ai', 'kg', '+14155550112', 'queued', 300, ?, ?)`,
		testTime, testTime); err != nil {
		t.Fatalf("grantee call insert: %v", err)
	}
	// A profile that does not exist is still refused.
	expectExecError(t, s, `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bob-missing', 'bob', 'no-such-profile', 'bob-ai', 'kx', '+14155550113', 'queued', 300, ?, ?)`,
		testTime, testTime)
	// The AI profile composite key still enforces ownership.
	expectExecError(t, s, `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bob-wrong-ai', 'bob', 'alice-tel', 'alice-ai', 'kw', '+14155550114', 'queued', 300, ?, ?)`,
		testTime, testTime)
	// A profile referenced by a call cannot be deleted.
	expectExecError(t, s, "DELETE FROM telephony_profiles WHERE id = 'alice-tel'")
	// Owner-scoped idempotency is still unique.
	expectExecError(t, s, `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bob-dup', 'bob', 'alice-tel', 'bob-ai', 'kg', '+14155550115', 'queued', 300, ?, ?)`,
		testTime, testTime)
}

func TestMigration0007RollbackRefusedWhileGranteeCallExists(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion6(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bob-via-grant', 'bob', 'alice-tel', 'bob-ai', 'kg', '+14155550112', 'queued', 300, ?, ?)`,
		testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(ctx, 6); err == nil {
		t.Fatal("rollback succeeded while a grantee call exists")
	}
	// The 0007 step refuses, so the schema stays at 7 with foreign key
	// enforcement on.
	if v, _ := s.SchemaVersion(ctx); v != 7 {
		t.Fatalf("schema version after refused rollback = %d, want 7", v)
	}
	if n := scalarInt(t, s, "PRAGMA foreign_keys"); n != 1 {
		t.Fatalf("foreign_keys after refused rollback = %d, want 1", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls"); n != 2 {
		t.Fatalf("calls after refused rollback = %d, want 2", n)
	}
}

func TestMigration0007RollbackRestoresOwnerScopedKey(t *testing.T) {
	ctx := context.Background()
	s := openAtVersion6(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(ctx, 6); err != nil {
		t.Fatalf("rollback with owner-only calls: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 6 {
		t.Fatalf("schema version = %d, want 6", v)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 1 {
		t.Fatalf("call lost on rollback: %d rows", n)
	}
	if n := scalarInt(t, s, "PRAGMA foreign_keys"); n != 1 {
		t.Fatalf("foreign_keys after rollback = %d, want 1", n)
	}
}
