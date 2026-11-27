package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/callstream"
	"github.com/CallHarborMCP/callharbor/internal/recording"
)

// TestV1RecoveredCrashIsServedThroughREST: a recording whose process crashed
// before finalize is settled by the startup sweep, and then the owner can read
// its metadata and every recovered PCM byte over the REST endpoints. Before the
// reseal fix the audio read failed for this row.
func TestV1RecoveredCrashIsServedThroughREST(t *testing.T) {
	f := newFixture(t)
	const id = "rec-crashed"
	var salt [recording.SaltSize]byte
	for i := range salt {
		salt[i] = byte(0x90 + i)
	}
	h := recording.Header{KeyVersion: 1, Salt: salt, SampleRate: 8000}
	key, err := f.ring.DeriveRecordingKey(1, salt[:])
	if err != nil {
		t.Fatal(err)
	}
	w, err := recording.CreateFile(f.dataDir, "alice", id, h, key)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 2*recording.SegmentSize)
	if _, err := rand.Read(pcm); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(pcm); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	// The crash: the writer is never closed, and the row stays 'recording'.
	ctx := context.Background()
	if _, err := f.raw.ExecContext(ctx, `INSERT INTO recordings
		(id, call_id, owner_user_id, status, storage_path, byte_count, duration_ms,
		 key_version, created_at, sample_rate, channels, quality_flags)
		VALUES (?, 'alice-call', 'alice', 'recording', ?, 0, 0, 1, ?, NULL, 2, '[]')`,
		id, recording.RelPath("alice", id), seedTime); err != nil {
		t.Fatalf("insert recording row: %v", err)
	}

	now := func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) }
	n, err := callstream.RecoverInterrupted(ctx, f.store, f.ring, f.dataDir, now, slog.New(slog.DiscardHandler))
	if err != nil || n != 1 {
		t.Fatalf("sweep settled %d rows, err %v; want 1", n, err)
	}

	meta := get(t, f.mux, "/api/v1/calls/alice-call/recording", f.aliceCk)
	if meta.Code != http.StatusOK {
		t.Fatalf("metadata = %d %s", meta.Code, meta.Body.String())
	}
	rec := get(t, f.mux, audioPath("alice-call"), f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("audio = %d %s; a recovered row must be readable", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if len(body) != 44+len(pcm) {
		t.Fatalf("body = %d bytes, want %d", len(body), 44+len(pcm))
	}
	if !bytes.Equal(body[44:], pcm) {
		t.Fatal("recovered audio differs from the crashed PCM")
	}
}
