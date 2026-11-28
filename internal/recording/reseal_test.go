package recording

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// readAllPlain opens a recovered or sealed file through NewReader and returns
// every plaintext byte it serves. plainLen is the byte count the row records.
func readAllPlain(t *testing.T, dataDir, rel string, ring *secrets.Keyring, plainLen int64) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join(dataDir, rel)) // #nosec G304 -- test-only path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, head); err != nil {
		t.Fatal(err)
	}
	h, err := DecodeHeader(head)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ring.DeriveRecordingKey(h.KeyVersion, h.Salt[:])
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(f, st.Size(), head, h, key, "rec-1", "alice", plainLen)
	if err != nil {
		t.Fatalf("NewReader over %d bytes, plainLen %d: %v", st.Size(), plainLen, err)
	}
	out := make([]byte, plainLen)
	if _, err := io.ReadFull(r, out); err != nil {
		t.Fatalf("read recovered audio: %v", err)
	}
	return out
}

// TestRecoveredCrashIsReadable: a crashed recording, after Recover, serves every
// authenticated PCM byte through the reader. Before the reseal the reader
// refused the file with ErrCorrupt.
func TestRecoveredCrashIsReadable(t *testing.T) {
	plain := samplePlain(2*SegmentSize + 40)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || res.PlainBytes != 2*SegmentSize {
		t.Fatalf("recover = %+v, want incomplete with %d bytes", res, 2*SegmentSize)
	}
	got := readAllPlain(t, dataDir, rel, ring, res.PlainBytes)
	if !bytes.Equal(got, plain[:res.PlainBytes]) {
		t.Fatal("recovered audio differs from the authenticated prefix")
	}
}

// TestRecoveredCrashWithTornTailIsReadable: a torn tail after a full segment is
// dropped, and the authenticated segment before it reads back.
func TestRecoveredCrashWithTornTailIsReadable(t *testing.T) {
	plain := samplePlain(SegmentSize + 10)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	f, err := os.OpenFile(filepath.Join(dataDir, rel), os.O_APPEND|os.O_WRONLY, 0) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("torn-garbage-bytes")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatal(err)
	}
	got := readAllPlain(t, dataDir, rel, ring, res.PlainBytes)
	if !bytes.Equal(got, plain[:SegmentSize]) {
		t.Fatal("recovered audio differs from the first full segment")
	}
}

// TestFailedWriterResealsAuthenticatedPrefix: a writer whose file breaks after
// two full segments reseals on Reseal. The byte count is the authenticated PCM,
// and the reader serves it.
func TestFailedWriterResealsAuthenticatedPrefix(t *testing.T) {
	dataDir := t.TempDir()
	master := bytes.Repeat([]byte{0x11}, 32)
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: master})
	if err != nil {
		t.Fatal(err)
	}
	var h Header
	h.KeyVersion = 1
	h.SampleRate = testRate
	if _, err := rand.Read(h.Salt[:]); err != nil {
		t.Fatal(err)
	}
	key, err := ring.DeriveRecordingKey(1, h.Salt[:])
	if err != nil {
		t.Fatal(err)
	}
	w, err := CreateFile(dataDir, "alice", "rec-1", h, key)
	if err != nil {
		t.Fatal(err)
	}
	plain := samplePlain(2*SegmentSize + 100)
	// Two full segments are sealed and synced; the 100-byte tail stays buffered.
	if err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	n, err := w.Reseal()
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if n != 2*SegmentSize {
		t.Fatalf("authenticated bytes = %d, want %d (the buffered tail is never in the file)", n, 2*SegmentSize)
	}
	got := readAllPlain(t, dataDir, filepath.Join("recordings", "alice", "rec-1.chrec"), ring, n)
	if !bytes.Equal(got, plain[:n]) {
		t.Fatal("resealed audio differs from the authenticated prefix")
	}
}

// TestFailedWriterWithNoSealedSegmentsReportsNothing: a writer that never sealed
// a segment reseals to zero bytes, so the recorder finalizes the row as failed.
func TestFailedWriterWithNoSealedSegmentsReportsNothing(t *testing.T) {
	dataDir := t.TempDir()
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x11}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	var h Header
	h.KeyVersion = 1
	h.SampleRate = testRate
	if _, err := rand.Read(h.Salt[:]); err != nil {
		t.Fatal(err)
	}
	key, err := ring.DeriveRecordingKey(1, h.Salt[:])
	if err != nil {
		t.Fatal(err)
	}
	w, err := CreateFile(dataDir, "alice", "rec-2", h, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(samplePlain(100)); err != nil {
		t.Fatal(err)
	}
	n, err := w.Reseal()
	if err == nil && n != 0 {
		t.Fatalf("authenticated bytes = %d, want 0", n)
	}
}
