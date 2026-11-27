package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigration0011AddsRecordingMetadataColumns(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m11.db"))
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('recordings') WHERE name IN ('sample_rate', 'channels', 'quality_flags')"); n != 3 {
		t.Fatalf("recording metadata columns = %d, want 3", n)
	}
}

func TestMigration0011RecordingMetadataChecks(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m11check.db"))
	seedTenants(t, s)
	const prefix = `INSERT INTO recordings (id, call_id, owner_user_id, status, storage_path, key_version, created_at`
	const suffix = `) VALUES (?, 'alice-call', 'alice', 'recording', ?, 1, ?`
	bad := map[string]string{
		"bad channels":      prefix + `, channels` + suffix + `, 1)`,
		"bad sample rate":   prefix + `, sample_rate` + suffix + `, 1234)`,
		"quality not array": prefix + `, quality_flags` + suffix + `, '{}')`,
		"quality bad json":  prefix + `, quality_flags` + suffix + `, '[')`,
	}
	for name, stmt := range bad {
		err := o10ExecErr(s, stmt, "rec-bad", "recordings/alice/rec-bad.chrec", testTime)
		if err == nil {
			t.Fatalf("%s: insert accepted, want CHECK failure", name)
		}
	}
	if err := o10ExecErr(s, `INSERT INTO recordings (id, call_id, owner_user_id, status, storage_path, key_version, created_at, sample_rate, quality_flags)
		VALUES ('rec-ok', 'alice-call', 'alice', 'recording', 'recordings/alice/rec-ok.chrec', 1, ?, 16000, '["backpressure"]')`, testTime); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
}

func TestMigration0011DefaultsKeepExistingRows(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m11def.db"))
	seedTenants(t, s)
	if err := o10ExecErr(s, `INSERT INTO recordings (id, call_id, owner_user_id, status, storage_path, key_version, created_at)
		VALUES ('rec-def', 'alice-call', 'alice', 'recording', 'recordings/alice/rec-def.chrec', 1, ?)`, testTime); err != nil {
		t.Fatal(err)
	}
	if got := scalarString(t, s, "SELECT quality_flags FROM recordings WHERE id = 'rec-def'"); got != "[]" {
		t.Fatalf("default quality_flags = %q, want []", got)
	}
	if n := scalarInt(t, s, "SELECT channels FROM recordings WHERE id = 'rec-def'"); n != 2 {
		t.Fatalf("default channels = %d, want 2", n)
	}
	if err := s.MigrateDown(ctx, 10); err != nil {
		t.Fatalf("rollback 0011: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 10 {
		t.Fatalf("schema after rollback = %d, want 10", v)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('recordings') WHERE name IN ('sample_rate', 'channels', 'quality_flags')"); n != 0 {
		t.Fatalf("0011 columns after rollback = %d, want 0", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM recordings WHERE id = 'rec-def'"); n != 1 {
		t.Fatal("recording row lost on rollback")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('recordings') WHERE name IN ('sample_rate', 'channels', 'quality_flags')"); n != 3 {
		t.Fatalf("0011 columns after re-migrate = %d, want 3", n)
	}
}
