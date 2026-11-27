package storage

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func testRecordingRow(id string) Recording {
	return Recording{
		ID: id, CallID: "alice-call", OwnerUserID: "alice",
		StoragePath: "recordings/alice/" + id + ".chrec", KeyVersion: 1, CreatedAt: testTime,
	}
}

func TestRecordingRowLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "rec.db"))
	seedTenants(t, s)

	if err := s.InsertRecording(ctx, testRecordingRow("rec-1")); err != nil {
		t.Fatal(err)
	}
	// One recording per call: a second row for the same call is refused.
	if err := s.InsertRecording(ctx, testRecordingRow("rec-2")); err == nil {
		t.Fatal("second recording for the same call was accepted")
	}
	if err := s.SetRecordingFormat(ctx, "rec-1", 16000); err != nil {
		t.Fatal(err)
	}
	ok, err := s.FinalizeRecording(ctx, RecordingFinal{
		ID: "rec-1", Status: RecordingStatusPartial, ByteCount: 44 + 64, DurationMS: 8,
		SHA256:       "ab00000000000000000000000000000000000000000000000000000000000000",
		QualityFlags: []string{"write_failed", "backpressure"},
		FinalizedAt:  testTime, ExpiresAt: testTime,
	})
	if err != nil || !ok {
		t.Fatalf("finalize: wrote=%v err=%v", ok, err)
	}
	// A second finalize changes nothing.
	ok, err = s.FinalizeRecording(ctx, RecordingFinal{ID: "rec-1", Status: RecordingStatusComplete, FinalizedAt: testTime})
	if err != nil || ok {
		t.Fatalf("second finalize: wrote=%v err=%v, want no write", ok, err)
	}

	r, err := s.GetRecordingForCall(ctx, "alice", "alice-call")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RecordingStatusPartial || r.SampleRate != 16000 || r.Channels != 2 || r.ByteCount != 108 {
		t.Fatalf("row = %+v", r)
	}
	if !slices.Equal(r.QualityFlags, []string{"write_failed", "backpressure"}) {
		t.Fatalf("flags = %v", r.QualityFlags)
	}
	if r.StoragePath != "recordings/alice/rec-1.chrec" {
		t.Fatalf("path = %q", r.StoragePath)
	}
}

func TestRecordingNotVisibleToOtherOwner(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "recowner.db"))
	seedTenants(t, s)
	if err := s.InsertRecording(ctx, testRecordingRow("rec-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRecordingForCall(ctx, "bob", "alice-call"); err != ErrRecordingNotFound {
		t.Fatalf("other owner: err = %v, want ErrRecordingNotFound", err)
	}
}

func TestListRecordingsByStatusFindsRecordingRows(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "reclist.db"))
	seedTenants(t, s)
	if err := s.InsertRecording(ctx, testRecordingRow("rec-1")); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRecordingsByStatus(ctx, RecordingStatusRecording)
	if err != nil || len(rows) != 1 || rows[0].ID != "rec-1" {
		t.Fatalf("list recording = %+v, %v", rows, err)
	}
}

func TestRecordingExpiryUsesRetentionDays(t *testing.T) {
	got, err := RecordingExpiry("2026-11-01T00:00:00.000Z", 30)
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026-12-01T00:00:00.000Z" {
		t.Fatalf("expiry = %q", got)
	}
}
