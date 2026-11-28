package callstream

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// A complete five-segment file with a byte flipped in segment 1 settles as
// partial with recording_damaged and recovered_after_restart. The prefix reads
// back, the original is preserved byte-identical, and one warning is logged
// with the id and byte counts.
func TestRecoverInterruptedDamagedRowIsPartialWithFlagAndWarning(t *testing.T) {
	dataDir := t.TempDir()
	ring := testRing(t)
	plain := bytes.Repeat([]byte{1, 2, 3, 4}, 5*recording.SegmentSize/4)
	row := crashedRecording(t, dataDir, ring, "rec-damaged", plain)
	path := filepath.Join(dataDir, row.StoragePath)
	// Flip a byte in segment 1, a full-size segment after the first, so the
	// walk stops there and the rest of the file is damaged.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[recording.HeaderSize+(recording.SegmentSize+recording.TagSize)+5] ^= 0x80
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(data)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	store := &fakeRecoveryStore{rows: []storage.Recording{row}}
	now := func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) }

	if n, err := RecoverInterrupted(context.Background(), store, ring, dataDir, now, logger); err != nil || n != 1 {
		t.Fatalf("settled = %d, err = %v", n, err)
	}
	f := store.finalized[0]
	if f.Status != storage.RecordingStatusPartial || f.ByteCount != int64(recording.SegmentSize) {
		t.Fatalf("finalize = %+v, want partial with %d bytes", f, recording.SegmentSize)
	}
	if !slices.Contains(f.QualityFlags, recording.FlagRecordingDamaged) {
		t.Fatalf("flags = %v, want recording_damaged", f.QualityFlags)
	}
	if !slices.Contains(f.QualityFlags, recording.FlagRecoveredAfterRestart) {
		t.Fatalf("flags = %v, want recovered_after_restart", f.QualityFlags)
	}
	preserved, err := os.ReadFile(path + ".damaged")
	if err != nil {
		t.Fatalf("preserved original missing: %v", err)
	}
	if !bytes.Equal(preserved, original) {
		t.Fatal("preserved original is not byte-identical to the damaged file")
	}
	if strings.Count(buf.String(), "level=WARN") != 1 || !strings.Contains(buf.String(), "rec-damaged") {
		t.Fatalf("log = %q, want one warning naming the recording", buf.String())
	}
}
