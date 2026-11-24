package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func insertProbe(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO retention_purge_markers (scope, key, cutoff_at, created_at)
		VALUES ('call', ?, NULL, ?)`, key, testTime)
	return err
}

func probeCount(t *testing.T, s *Store, key string) int {
	t.Helper()
	return scalarInt(t, s, "SELECT COUNT(*) FROM retention_purge_markers WHERE scope = 'call' AND key = '"+key+"'")
}

func TestRecordWebhookOnceDedupesAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	s := openMigrated(t, path)
	now := time.Unix(1700000000, 0)
	runs := 0
	process := func(context.Context, *sql.Tx) error { runs++; return nil }

	dup, err := s.RecordWebhookOnce(ctx, "telnyx", "call.answered|evt-1", now, process)
	if err != nil || dup {
		t.Fatalf("first delivery: dup=%v err=%v", dup, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	dup, err = reopened.RecordWebhookOnce(ctx, "telnyx", "call.answered|evt-1", now, process)
	if err != nil || !dup {
		t.Fatalf("redelivery after reopen: dup=%v err=%v", dup, err)
	}
	if runs != 1 {
		t.Fatalf("process ran %d times, want 1", runs)
	}
}

func TestRecordWebhookOnceFailedDispatchIsRetryable(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "app.db"))
	now := time.Unix(1700000000, 0)
	boom := errors.New("dispatch failed")

	if _, err := s.RecordWebhookOnce(ctx, "telnyx", "call.hangup|evt-2", now, func(context.Context, *sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err %v, want dispatch error", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM provider_webhook_receipts WHERE provider_event_id = 'call.hangup|evt-2'"); n != 0 {
		t.Fatalf("failed dispatch left %d receipts", n)
	}
	dup, err := s.RecordWebhookOnce(ctx, "telnyx", "call.hangup|evt-2", now, func(context.Context, *sql.Tx) error { return nil })
	if err != nil || dup {
		t.Fatalf("retry after failed dispatch: dup=%v err=%v", dup, err)
	}
}

func TestRecordWebhookOnceCommitsStateWithReceipt(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "app.db"))
	now := time.Unix(1700000000, 0)

	dup, err := s.RecordWebhookOnce(ctx, "telnyx", "call.answered|evt-3", now, func(ctx context.Context, tx *sql.Tx) error {
		return insertProbe(ctx, tx, "probe-commit")
	})
	if err != nil || dup {
		t.Fatalf("dispatch writing through tx: dup=%v err=%v", dup, err)
	}
	if n := probeCount(t, s, "probe-commit"); n != 1 {
		t.Fatalf("state row count %d, want 1", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM provider_webhook_receipts WHERE provider_event_id = 'call.answered|evt-3'"); n != 1 {
		t.Fatalf("receipt count %d, want 1", n)
	}
}

func TestRecordWebhookOnceRollsBackStateOnDispatchError(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "app.db"))
	now := time.Unix(1700000000, 0)
	boom := errors.New("dispatch failed after write")

	_, err := s.RecordWebhookOnce(ctx, "telnyx", "call.hangup|evt-4", now, func(ctx context.Context, tx *sql.Tx) error {
		if err := insertProbe(ctx, tx, "probe-rollback"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v, want dispatch error", err)
	}
	if n := probeCount(t, s, "probe-rollback"); n != 0 {
		t.Fatalf("failed dispatch left %d state rows", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM provider_webhook_receipts WHERE provider_event_id = 'call.hangup|evt-4'"); n != 0 {
		t.Fatalf("failed dispatch left %d receipts", n)
	}
	dup, err := s.RecordWebhookOnce(ctx, "telnyx", "call.hangup|evt-4", now, func(ctx context.Context, tx *sql.Tx) error {
		return insertProbe(ctx, tx, "probe-rollback")
	})
	if err != nil || dup {
		t.Fatalf("retry after rolled-back dispatch: dup=%v err=%v", dup, err)
	}
	if n := probeCount(t, s, "probe-rollback"); n != 1 {
		t.Fatalf("retry state row count %d, want 1", n)
	}
}

func TestRecordWebhookOnceRejectsStoreWriteInsideDispatch(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "app.db"))
	now := time.Unix(1700000000, 0)
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err := s.RecordWebhookOnce(short, "telnyx", "call.answered|evt-5", now, func(ctx context.Context, _ *sql.Tx) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO retention_purge_markers (scope, key, cutoff_at, created_at)
			VALUES ('call', 'probe-deadlock', NULL, ?)`, testTime)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write through Store inside dispatch: err %v, want deadline exceeded", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM provider_webhook_receipts WHERE provider_event_id = 'call.answered|evt-5'"); n != 0 {
		t.Fatalf("aborted dispatch left %d receipts", n)
	}
}

func TestRecordWebhookOnceRejectsEmptyKeys(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "app.db"))
	noop := func(context.Context, *sql.Tx) error { return nil }
	if _, err := s.RecordWebhookOnce(context.Background(), "", "x", time.Now(), noop); err == nil {
		t.Fatal("accepted empty provider")
	}
	if _, err := s.RecordWebhookOnce(context.Background(), "telnyx", "", time.Now(), noop); err == nil {
		t.Fatal("accepted empty event id")
	}
}
