package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testTime = "2026-11-23T10:00:00.000Z"

func openMigrated(t *testing.T, path string) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedTenants(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO users (id, username, role, password_hash, created_at, updated_at) VALUES
			('alice', 'alice', 'user', 'hash', '` + testTime + `', '` + testTime + `'),
			('bob', 'bob', 'user', 'hash', '` + testTime + `', '` + testTime + `')`,
		`INSERT INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at) VALUES
			('alice-key', 'alice', 'key', 'telnyx_api_key', x'00', x'00', 1, '` + testTime + `'),
			('bob-key', 'bob', 'key', 'telnyx_api_key', x'00', x'00', 1, '` + testTime + `')`,
		`INSERT INTO telephony_profiles (id, owner_user_id, name, provider, caller_number, api_secret_id, created_at, updated_at) VALUES
			('alice-tel', 'alice', 'main', 'telnyx', '+14155550100', 'alice-key', '` + testTime + `', '` + testTime + `'),
			('bob-tel', 'bob', 'main', 'telnyx', '+14155550200', 'bob-key', '` + testTime + `', '` + testTime + `')`,
		`INSERT INTO ai_profiles (id, owner_user_id, name, protocol, endpoint_url, model, voice, created_at, updated_at) VALUES
			('alice-ai', 'alice', 'live', 'live_ws', 'https://ai.example.test', 'm', 'v', '` + testTime + `', '` + testTime + `'),
			('bob-ai', 'bob', 'live', 'live_ws', 'https://ai.example.test', 'm', 'v', '` + testTime + `', '` + testTime + `')`,
		`INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at) VALUES
			('alice-call', 'alice', 'alice-tel', 'alice-ai', 'k1', '+14155550111', 'queued', 300, '` + testTime + `', '` + testTime + `')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}

func expectExecError(t *testing.T, s *Store, stmt string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), stmt, args...); err == nil {
		t.Fatalf("expected constraint failure for %q", stmt)
	}
}

func scalarInt(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func TestOpenEnablesWALAndForeignKeys(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "callharbor.db"))
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if n := scalarInt(t, s, "PRAGMA foreign_keys"); n != 1 {
		t.Fatalf("foreign_keys = %d, want 1", n)
	}
	if n := scalarInt(t, s, "PRAGMA synchronous"); n != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", n)
	}
	if err := s.IntegrityCheck(context.Background()); err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open with empty path succeeded")
	}
}

func TestOpenRejectsDSNMetacharactersInPath(t *testing.T) {
	for _, p := range []string{"/tmp/a?mode=memory", "/tmp/a#frag"} {
		if _, err := Open(context.Background(), p); err == nil {
			t.Fatalf("Open(%q) accepted a path that changes the DSN", p)
		}
	}
}

func TestMigrationsRoundTripUpAndDown(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 1 {
		t.Fatalf("schema version = %d, want 1", v)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate should be a no-op: %v", err)
	}
	if err := s.MigrateDown(ctx, 0); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 0 {
		t.Fatalf("schema version after rollback = %d, want 0", v)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations'"); n != 0 {
		t.Fatalf("%d domain tables remain after rollback", n)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate after rollback: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'calls'"); n != 1 {
		t.Fatal("calls table missing after re-migrate")
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "newer.db"))
	if _, err := s.db.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES (2, 'future')"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Migrate with newer schema: %v", err)
	}
}

func TestMigrateDownRejectsOutOfRangeTarget(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "range.db"))
	if err := s.MigrateDown(ctx, 5); err == nil {
		t.Fatal("rollback target above current version accepted")
	}
	if err := s.MigrateDown(ctx, -1); err == nil {
		t.Fatal("negative rollback target accepted")
	}
}

func TestFailedMigrationRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.SchemaVersion(ctx); err != nil {
		t.Fatal(err)
	}
	bad := "CREATE TABLE partial_probe (id INTEGER); THIS IS NOT SQL;"
	if err := s.runMigration(ctx, bad, 1, "broken", true); err == nil {
		t.Fatal("broken migration succeeded")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'partial_probe'"); n != 0 {
		t.Fatal("partial DDL from failed migration was committed")
	}
	if v, _ := s.SchemaVersion(ctx); v != 0 {
		t.Fatalf("failed migration recorded version %d", v)
	}
}

func TestSchemaIndexesExist(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "idx.db"))
	for _, name := range []string{
		"calls_owner_created_idx",
		"calls_owner_status_idx",
		"call_events_owner_occurred_idx",
		"transcript_turns_owner_call_idx",
		"recordings_owner_expires_idx",
		"audit_events_target_occurred_idx",
		"audit_events_actor_occurred_idx",
		"provider_webhook_receipts_received_idx",
	} {
		if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", name); n != 1 {
			t.Fatalf("index %s missing", name)
		}
	}
}

func TestCrossTenantReferencesAreRejected(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "tenant.db"))
	seedTenants(t, s)

	expectExecError(t, s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('evil', 'bob', 'alice-tel', 'bob-ai', 'k', '+14155550111', 'queued', 60, '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO telephony_profiles (id, owner_user_id, name, provider, caller_number, api_secret_id, created_at, updated_at)
		VALUES ('evil-tel', 'bob', 'x', 'telnyx', '+14155550300', 'alice-key', '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO ai_profiles (id, owner_user_id, name, protocol, endpoint_url, model, voice, api_secret_id, created_at, updated_at)
		VALUES ('evil-ai', 'bob', 'x', 'realtime_ws', 'https://ai.example.test', 'm', 'v', 'alice-key', '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at)
		VALUES ('alice-call', 'bob', 1, 'x', '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO transcript_turns (call_id, owner_user_id, seq, speaker, finality, started_ms, text, recorded_at)
		VALUES ('alice-call', 'bob', 1, 'callee', 'final', 0, 'x', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO recordings (id, call_id, owner_user_id, status, storage_path, key_version, created_at)
		VALUES ('r1', 'alice-call', 'bob', 'recording', 'r1.bin', 1, '`+testTime+`')`)
}

func TestSameTenantReferencesAreAccepted(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "same.db"))
	seedTenants(t, s)
	if _, err := s.db.Exec(`INSERT INTO call_events (call_id, owner_user_id, seq, kind, state, occurred_at, recorded_at)
		VALUES ('alice-call', 'alice', 1, 'call.created', 'queued', '` + testTime + `', '` + testTime + `')`); err != nil {
		t.Fatalf("valid same-tenant event rejected: %v", err)
	}
}

func TestOwnerDeletionRestrictedWhileDataExists(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "restrict.db"))
	seedTenants(t, s)
	expectExecError(t, s, "DELETE FROM users WHERE id = 'alice'")
	expectExecError(t, s, "DELETE FROM secrets WHERE id = 'alice-key'")
}

func TestCallInvariantsAreEnforced(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "inv.db"))
	seedTenants(t, s)

	expectExecError(t, s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('dup-key', 'alice', 'alice-tel', 'alice-ai', 'k1', '+14155550111', 'queued', 60, '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bad-status', 'alice', 'alice-tel', 'alice-ai', 'k2', '+14155550111', 'dialing', 60, '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('no-end', 'alice', 'alice-tel', 'alice-ai', 'k3', '+14155550111', 'ended', 60, '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES ('bad-e164', 'alice', 'alice-tel', 'alice-ai', 'k4', '4155550111', 'queued', 60, '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at, record_requested, record_effective)
		VALUES ('rec', 'alice', 'alice-tel', 'alice-ai', 'k5', '+14155550111', 'queued', 60, '`+testTime+`', '`+testTime+`', 0, 1)`)
	expectExecError(t, s, `INSERT INTO ai_profiles (id, owner_user_id, name, protocol, endpoint_url, model, voice, created_at, updated_at)
		VALUES ('inferred', 'alice', 'x', 'auto', 'https://ai.example.test', 'm', 'v', '`+testTime+`', '`+testTime+`')`)
	expectExecError(t, s, `INSERT INTO users (id, username, role, password_hash, recording_opt_in, created_at, updated_at)
		VALUES ('rec-user', 'rec-user', 'user', 'h', 1, '`+testTime+`', '`+testTime+`')`)
}

func TestNewUsersDefaultRecordingOff(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "defaults.db"))
	if _, err := s.db.Exec(`INSERT INTO users (id, username, role, password_hash, created_at, updated_at)
		VALUES ('carol', 'carol', 'user', 'h', '` + testTime + `', '` + testTime + `')`); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, "SELECT recording_opt_in FROM users WHERE id = 'carol'"); n != 0 {
		t.Fatalf("recording_opt_in default = %d, want 0", n)
	}
}

func TestAppendOnlyTablesRejectUpdatesAndDeletes(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "append.db"))
	seedTenants(t, s)
	if _, err := s.db.Exec(`INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at)
		VALUES ('alice-call', 'alice', 1, 'call.created', '` + testTime + `', '` + testTime + `')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO call_briefs (call_id, owner_user_id, version, content_json, content_sha256, created_at)
		VALUES ('alice-call', 'alice', 1, '{"objective":"x"}', '` + strings.Repeat("a", 64) + `', '` + testTime + `')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO audit_events (occurred_at, actor_user_id, actor_role, action, resource_type, outcome)
		VALUES ('` + testTime + `', 'alice', 'user', 'login', 'session', 'success')`); err != nil {
		t.Fatal(err)
	}

	expectExecError(t, s, "UPDATE call_events SET kind = 'forged'")
	expectExecError(t, s, "DELETE FROM call_events")
	expectExecError(t, s, "UPDATE call_briefs SET content_json = '{}'")
	expectExecError(t, s, "DELETE FROM call_briefs")
	expectExecError(t, s, "UPDATE audit_events SET outcome = 'success'")
	expectExecError(t, s, "DELETE FROM audit_events")
	expectExecError(t, s, "DELETE FROM calls WHERE id = 'alice-call'")
}

func seedCallWithEvents(t *testing.T, s *Store, callID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at)
		VALUES (?, 'alice', 'alice-tel', 'alice-ai', ?, '+14155550111', 'queued', 300, ?, ?)`, callID, "k-"+callID, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at)
		VALUES (?, 'alice', 1, 'call.created', ?, ?)`, callID, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO call_briefs (call_id, owner_user_id, version, content_json, content_sha256, created_at)
		VALUES (?, 'alice', 1, '{}', ?, ?)`, callID, strings.Repeat("b", 64), testTime); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeCallDeletesOnlyTargetAndCloseWindow(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "purge.db"))
	seedTenants(t, s)
	seedCallWithEvents(t, s, "old-call")
	seedCallWithEvents(t, s, "kept-call")

	if err := s.PurgeCall(ctx, "old-call", testTime); err != nil {
		t.Fatalf("PurgeCall: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'old-call'"); n != 0 {
		t.Fatalf("old-call still present after purge")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events WHERE call_id = 'kept-call'"); n != 1 {
		t.Fatalf("kept-call events = %d, want 1", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM retention_purge_markers"); n != 0 {
		t.Fatalf("purge marker not closed at commit: %d remain", n)
	}
	expectExecError(t, s, "DELETE FROM call_events WHERE call_id = 'kept-call'")
	expectExecError(t, s, "DELETE FROM call_briefs WHERE call_id = 'kept-call'")
	expectExecError(t, s, "DELETE FROM calls WHERE id = 'kept-call'")
}

func TestPurgeCallRejectsEmptyIDWithoutOpeningWindow(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "purge-empty.db"))
	seedTenants(t, s)
	seedCallWithEvents(t, s, "kept-call")

	if err := s.PurgeCall(ctx, "", testTime); err == nil {
		t.Fatal("empty call id accepted")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM retention_purge_markers"); n != 0 {
		t.Fatalf("markers left after rejected purge: %d", n)
	}
	expectExecError(t, s, "DELETE FROM call_events WHERE call_id = 'kept-call'")
}

func TestPurgeWindowIsFailClosedOnStatementError(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "purge-fail.db"))
	seedTenants(t, s)
	seedCallWithEvents(t, s, "stuck-call")
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER test_block_brief_purge BEFORE DELETE ON call_briefs
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeCall(ctx, "stuck-call", testTime); err == nil {
		t.Fatal("purge with injected mid-transaction failure unexpectedly succeeded")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM retention_purge_markers"); n != 0 {
		t.Fatalf("marker left after failed purge: %d", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events WHERE call_id = 'stuck-call'"); n != 1 {
		t.Fatalf("events deleted despite rollback: %d", n)
	}
	expectExecError(t, s, "DELETE FROM call_events WHERE call_id = 'stuck-call'")
}

func TestPurgeMarkerCannotBeRetargeted(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "marker.db"))
	seedTenants(t, s)
	seedCallWithEvents(t, s, "guarded-call")

	if _, err := s.db.ExecContext(ctx, "INSERT INTO retention_purge_markers (scope, key, created_at) VALUES ('call', 'bob-call', ?)", testTime); err != nil {
		t.Fatal(err)
	}
	expectExecError(t, s, "UPDATE retention_purge_markers SET key = 'guarded-call'")
	expectExecError(t, s, "DELETE FROM call_events WHERE call_id = 'guarded-call'")
	expectExecError(t, s, "INSERT INTO retention_purge_markers (scope, key, created_at) VALUES ('audit', 'global', ?)", testTime)
}

func TestAuditPurgeIsScopedByCutoff(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "audit.db"))
	seedTenants(t, s)
	const oldTime = "2026-01-01T00:00:00.000Z"
	const cutoff = "2026-06-01T00:00:00.000Z"
	const newTime = "2026-11-01T00:00:00.000Z"
	if _, err := s.db.Exec(`INSERT INTO audit_events (occurred_at, actor_user_id, actor_role, action, resource_type, outcome) VALUES
		(?, 'alice', 'user', 'login', 'session', 'success'),
		(?, 'alice', 'user', 'login', 'session', 'success')`, oldTime, newTime); err != nil {
		t.Fatal(err)
	}

	if err := s.PurgeAudit(ctx, cutoff, testTime); err != nil {
		t.Fatalf("PurgeAudit: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM audit_events"); n != 1 {
		t.Fatalf("audit rows after purge = %d, want 1 newer row", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM retention_purge_markers"); n != 0 {
		t.Fatalf("audit marker not closed at commit: %d remain", n)
	}
	expectExecError(t, s, "DELETE FROM audit_events")
	if _, err := s.db.Exec(`INSERT INTO audit_events (occurred_at, actor_user_id, actor_role, action, resource_type, outcome)
		VALUES (?, 'alice', 'user', 'login', 'session', 'success')`, oldTime); err != nil {
		t.Fatal(err)
	}
	expectExecError(t, s, "DELETE FROM audit_events WHERE occurred_at < '"+cutoff+"'")
	if _, err := s.db.ExecContext(ctx, "INSERT INTO retention_purge_markers (scope, key, cutoff_at, created_at) VALUES ('audit', ?, ?, ?)", cutoff, cutoff, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit_events (occurred_at, actor_user_id, actor_role, action, resource_type, outcome)
		VALUES (?, 'alice', 'user', 'login', 'session', 'success')`, newTime); err != nil {
		t.Fatal(err)
	}
	expectExecError(t, s, "UPDATE retention_purge_markers SET cutoff_at = '2026-11-30T00:00:00.000Z'")
	expectExecError(t, s, "UPDATE retention_purge_markers SET key = 'global'")
	expectExecError(t, s, "DELETE FROM audit_events WHERE occurred_at >= '"+cutoff+"'")
}

func TestDataSurvivesRestartAndCrashCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "durable.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seedTenants(t, s)

	crashDir := t.TempDir()
	crashPath := filepath.Join(crashDir, "crash.db")
	copyFile(t, path, crashPath)
	copyFile(t, path+"-wal", crashPath+"-wal")
	if info, err := os.Stat(crashPath + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty uncheckpointed WAL for crash copy: %v", err)
	}
	crashed, err := Open(ctx, crashPath)
	if err != nil {
		t.Fatalf("open crash copy: %v", err)
	}
	t.Cleanup(func() { _ = crashed.Close() })
	if n := scalarInt(t, crashed, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 1 {
		t.Fatal("committed call lost in WAL crash recovery")
	}
	if err := crashed.IntegrityCheck(ctx); err != nil {
		t.Fatalf("crash copy integrity: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if v, _ := reopened.SchemaVersion(ctx); v != 1 {
		t.Fatalf("schema version after restart = %d", v)
	}
	if n := scalarInt(t, reopened, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 1 {
		t.Fatal("committed call lost across restart")
	}
}

func TestBackupCapturesWALAndRestoresConsistently(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openMigrated(t, filepath.Join(dir, "live.db"))
	seedTenants(t, s)

	backupPath := filepath.Join(dir, "backup.db")
	if err := s.Backup(ctx, backupPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := s.Backup(ctx, backupPath); err == nil {
		t.Fatal("backup overwrote an existing file")
	}

	if _, err := s.db.Exec(`INSERT INTO users (id, username, role, password_hash, created_at, updated_at)
		VALUES ('after-backup', 'late', 'user', 'h', '` + testTime + `', '` + testTime + `')`); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(ctx, backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer restored.Close()
	if err := restored.IntegrityCheck(ctx); err != nil {
		t.Fatalf("backup integrity: %v", err)
	}
	if v, _ := restored.SchemaVersion(ctx); v != 1 {
		t.Fatalf("backup schema version = %d", v)
	}
	if n := scalarInt(t, restored, "SELECT COUNT(*) FROM users"); n != 2 {
		t.Fatalf("backup users = %d, want 2 (post-backup write must be excluded)", n)
	}
	if n := scalarInt(t, restored, "SELECT COUNT(*) FROM call_events"); n != 0 {
		t.Fatalf("unexpected events in backup: %d", n)
	}
	if n := scalarInt(t, restored, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'call_events_immutable_update'"); n != 1 {
		t.Fatal("append-only trigger missing from restored backup")
	}
}

func TestRestoredBackupRollsBackToPriorSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openMigrated(t, filepath.Join(dir, "rollback.db"))
	seedTenants(t, s)
	backupPath := filepath.Join(dir, "pre-rollback.db")
	if err := s.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateDown(ctx, 0); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	restored, err := Open(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if v, _ := restored.SchemaVersion(ctx); v != 1 {
		t.Fatalf("restored version = %d", v)
	}
	if n := scalarInt(t, restored, "SELECT COUNT(*) FROM calls"); n != 1 {
		t.Fatal("restore did not preserve pre-rollback data")
	}
}

func TestConcurrentWritersDoNotCorruptOrDeadlock(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "concurrent.db"))
	seedTenants(t, s)

	const writers = 8
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				errs <- err
				return
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at)
				VALUES ('alice-call', 'alice', (SELECT COALESCE(MAX(seq), 0) + 1 FROM call_events WHERE call_id = 'alice-call'), 'probe', ?, ?)`,
				testTime, testTime)
			if err != nil {
				_ = tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}(i)
	}
	for i := 0; i < writers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events"); n != writers {
		t.Fatalf("events = %d, want %d", n, writers)
	}
	if n := scalarInt(t, s, "SELECT COUNT(DISTINCT seq) FROM call_events"); n != writers {
		t.Fatalf("distinct seq = %d, want %d (lost update)", n, writers)
	}
}

func TestBackupRequiresOpenStore(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := s.Backup(context.Background(), filepath.Join(t.TempDir(), "x.db")); err == nil {
		t.Fatal("backup on closed store succeeded")
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}
