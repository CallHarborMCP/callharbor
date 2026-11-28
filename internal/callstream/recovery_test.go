package callstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// fakeRecoveryStore holds interrupted rows and records each finalize.
type fakeRecoveryStore struct {
	rows      []storage.Recording
	finalized []storage.RecordingFinal
}

func (f *fakeRecoveryStore) ListRecordingsByStatus(_ context.Context, status string) ([]storage.Recording, error) {
	if status != storage.RecordingStatusRecording {
		return nil, nil
	}
	return f.rows, nil
}

func (f *fakeRecoveryStore) FinalizeRecording(_ context.Context, r storage.RecordingFinal) (bool, error) {
	f.finalized = append(f.finalized, r)
	return true, nil
}

func (f *fakeRecoveryStore) OwnerRecordingRetentionDays(context.Context, string) (int, error) {
	return 30, nil
}

// crashedRecording writes plain through the production writer and never closes
// it, the way a crash leaves a file. It returns the row that points at it.
func crashedRecording(t *testing.T, dataDir string, ring *secrets.Keyring, id string, plain []byte) storage.Recording {
	t.Helper()
	var salt [recording.SaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		t.Fatal(err)
	}
	h := recording.Header{KeyVersion: 1, Salt: salt, SampleRate: 8000}
	key, err := ring.DeriveRecordingKey(1, salt[:])
	if err != nil {
		t.Fatal(err)
	}
	w, err := recording.CreateFile(dataDir, "alice", id, h, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	return storage.Recording{
		ID: id, OwnerUserID: "alice", StoragePath: recording.RelPath("alice", id),
		KeyVersion: 1, CreatedAt: "2026-11-27T12:00:00.000Z", Status: storage.RecordingStatusRecording,
	}
}

func testRing(t *testing.T) *secrets.Keyring {
	t.Helper()
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x33}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestRecoverInterruptedSettlesCrashedRecordingAsPartial(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	plain := bytes.Repeat([]byte{1, 2, 3, 4}, recording.SegmentSize/2) // two full segments
	row := crashedRecording(t, dataDir, ring, "rec-crash", plain)
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	now := func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) }

	n, err := RecoverInterrupted(context.Background(), store, ring, dataDir, now, nil)
	if err != nil || n != 1 {
		t.Fatalf("settled = %d, err = %v", n, err)
	}
	f := store.finalized[0]
	if f.Status != storage.RecordingStatusPartial || f.ByteCount != int64(len(plain)) {
		t.Fatalf("finalize = %+v, want partial with %d bytes", f, len(plain))
	}
	if !slices.Contains(f.QualityFlags, recording.FlagRecoveredAfterRestart) {
		t.Fatalf("flags = %v, want recovered_after_restart", f.QualityFlags)
	}
	if f.ExpiresAt == "" || f.FinalizedAt == "" {
		t.Fatalf("finalize missing timestamps: %+v", f)
	}
}

func TestRecoverInterruptedMissingFileIsFailed(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	row := storage.Recording{
		ID: "rec-gone", OwnerUserID: "alice", StoragePath: recording.RelPath("alice", "rec-gone"),
		KeyVersion: 1, CreatedAt: "2026-11-27T12:00:00.000Z", Status: storage.RecordingStatusRecording,
	}
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	if _, err := RecoverInterrupted(context.Background(), store, ring, dataDir, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	f := store.finalized[0]
	if f.Status != storage.RecordingStatusFailed || f.ByteCount != 0 {
		t.Fatalf("missing file: finalize = %+v, want failed with no audio", f)
	}
	if !slices.Contains(f.QualityFlags, recording.FlagRecoveredAfterRestart) {
		t.Fatalf("flags = %v, want recovered_after_restart", f.QualityFlags)
	}
}

// Without a master key, recovery must leave the rows and the file untouched: a
// key may be configured later, and failing the row now is irreversible.
func TestRecoverInterruptedWithoutKeyLeavesRowsAndFileUntouched(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	row := crashedRecording(t, dataDir, ring, "rec-nokey", bytes.Repeat([]byte{9}, recording.SegmentSize))
	path := filepath.Join(dataDir, row.StoragePath)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	n, err := RecoverInterrupted(context.Background(), store, nil, dataDir, time.Now, nil)
	if err != nil || n != 0 {
		t.Fatalf("no key: settled = %d, err = %v, want 0 and nil", n, err)
	}
	if len(store.finalized) != 0 {
		t.Fatalf("no key: finalized %+v, want no finalize", store.finalized)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("recovery without a key modified the file")
	}
}

// The warning is logged once with the count, and the rows are still counted as
// unsettled rather than failed.
func TestRecoverInterruptedWithoutKeyLogsOneWarning(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	rows := []storage.Recording{
		crashedRecording(t, dataDir, ring, "rec-a", []byte{1, 2, 3, 4}),
		crashedRecording(t, dataDir, ring, "rec-b", []byte{5, 6, 7, 8}),
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	store := &fakeRecoveryStore{rows: rows}
	if _, err := RecoverInterrupted(context.Background(), store, nil, dataDir, time.Now, logger); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "count=2") {
		t.Fatalf("log = %q, want one warning with count=2", out)
	}
	if len(store.finalized) != 0 {
		t.Fatalf("no key: finalized %d rows, want 0", len(store.finalized))
	}
}

// A row whose stored path is not the one its id derives is skipped: nothing is
// recovered from it and nothing is finalized.
func TestRecoverInterruptedSkipsMismatchedStoragePath(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	plain := bytes.Repeat([]byte{9}, recording.SegmentSize)
	row := crashedRecording(t, dataDir, ring, "rec-mismatch", plain)
	row.StoragePath = "recordings/alice/someone-else.chrec"
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	now := func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) }

	n, err := RecoverInterrupted(context.Background(), store, ring, dataDir, now, nil)
	if err != nil || n != 0 {
		t.Fatalf("settled = %d, err = %v; want 0 and no error", n, err)
	}
	if len(store.finalized) != 0 {
		t.Fatalf("mismatched row was finalized: %+v", store.finalized)
	}
}

// A file that does not authenticate under the configured key stays 'recording'
// and is left untouched, so a later run with the right key can settle it.
func TestRecoverInterruptedWrongKeyLeavesRowUnsettled(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	plain := bytes.Repeat([]byte{7}, recording.SegmentSize+10)
	row := crashedRecording(t, dataDir, ring, "rec-wrongkey", plain)
	other, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x44}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	now := func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) }

	n, _ := RecoverInterrupted(context.Background(), store, other, dataDir, now, nil)
	if n != 0 || len(store.finalized) != 0 {
		t.Fatalf("wrong key settled the row: n=%d finalized=%+v", n, store.finalized)
	}
}

// A recovered row gets its sample rate from the authenticated header, so a row
// that never had one set is still playable with the right format.
func TestRecoverInterruptedTakesSampleRateFromHeader(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	plain := bytes.Repeat([]byte{5}, recording.SegmentSize)
	row := crashedRecording(t, dataDir, ring, "rec-rate", plain)
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	now := func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) }

	if _, err := RecoverInterrupted(context.Background(), store, ring, dataDir, now, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.finalized[0].SampleRate; got != 8000 {
		t.Fatalf("finalize sample rate = %d, want 8000 from the header", got)
	}
}
