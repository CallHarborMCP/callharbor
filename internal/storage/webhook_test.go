package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordWebhookOnceDedupesAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	s := openMigrated(t, path)
	now := time.Unix(1700000000, 0)
	runs := 0
	process := func(context.Context) error { runs++; return nil }

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

	if _, err := s.RecordWebhookOnce(ctx, "telnyx", "call.hangup|evt-2", now, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err %v, want dispatch error", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM provider_webhook_receipts WHERE provider_event_id = 'call.hangup|evt-2'"); n != 0 {
		t.Fatalf("failed dispatch left %d receipts", n)
	}
	dup, err := s.RecordWebhookOnce(ctx, "telnyx", "call.hangup|evt-2", now, func(context.Context) error { return nil })
	if err != nil || dup {
		t.Fatalf("retry after failed dispatch: dup=%v err=%v", dup, err)
	}
}

func TestRecordWebhookOnceRejectsEmptyKeys(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "app.db"))
	if _, err := s.RecordWebhookOnce(context.Background(), "", "x", time.Now(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("accepted empty provider")
	}
	if _, err := s.RecordWebhookOnce(context.Background(), "telnyx", "", time.Now(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("accepted empty event id")
	}
}
