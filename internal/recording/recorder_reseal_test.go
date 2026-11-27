package recording

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// TestRecorderWriteFailureResealsAuthenticatedPrefix drives a real file writer
// into a write failure through the recorder. Two full segments are sealed before
// the failure, and the tail is buffered. Finish must report the authenticated
// PCM, status partial with write_failed, and the file must read back.
func TestRecorderWriteFailureResealsAuthenticatedPrefix(t *testing.T) {
	dataDir := t.TempDir()
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x55}, 32)})
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
	w, err := CreateFile(dataDir, "alice", "rec-fail", h, key)
	if err != nil {
		t.Fatal(err)
	}
	// Seal two full segments of plaintext directly through the writer, so the
	// file holds them before the failure. The recorder's own queue is not used
	// here: the point is the file state the failure leaves behind.
	plain := samplePlain(2*SegmentSize + 100)
	if err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if got := w.SealedBytes(); got != 2*SegmentSize {
		t.Fatalf("sealed before failure = %d, want %d", got, 2*SegmentSize)
	}
	// Make the next emit fail with a real write error: the descriptor is
	// replaced by a read-only one, so the segment write is refused. Reseal then
	// reopens the file read-write, as a production failure path would not need
	// to, and reads and seals the authenticated prefix.
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := os.OpenFile(filepath.Join(dataDir, RelPath("alice", "rec-fail")), os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.f = ro
	if err := w.Write(make([]byte, SegmentSize)); err == nil {
		t.Fatal("write on a read-only descriptor should fail")
	}
	rw, err := os.OpenFile(filepath.Join(dataDir, RelPath("alice", "rec-fail")), os.O_RDWR, 0) // #nosec G304 -- test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	_ = ro.Close()
	w.f = rw
	n, err := w.Reseal()
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if n != 2*SegmentSize {
		t.Fatalf("authenticated bytes = %d, want %d", n, 2*SegmentSize)
	}
	// The resealed file must be readable through the production reader.
	rd := openRecordingFile(t, dataDir, "alice", "rec-fail", ring, n)
	if !bytes.Equal(rd, plain[:n]) {
		t.Fatal("resealed audio differs from the authenticated prefix")
	}
}

// openRecordingFile reads a recording file through NewReader and returns its
// first plainLen bytes. The file is opened the way the production reader does.
func openRecordingFile(t *testing.T, dataDir, owner, id string, ring *secrets.Keyring, plainLen int64) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join(dataDir, RelPath(owner, id)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, HeaderSize)
	if _, err := f.ReadAt(head, 0); err != nil {
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
	r, err := NewReader(f, st.Size(), head, h, key, id, owner, plainLen)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	out := make([]byte, plainLen)
	if _, err := r.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

// failingResealSink wraps a real Writer so the recorder's own run loop hits a
// write error on the second segment. It is the production Writer behind a
// test-only fault.
type failingResealSink struct {
	*Writer
	writes int
}

func (s *failingResealSink) Write(p []byte) error {
	s.writes++
	if s.writes > 1 {
		return os.ErrClosed
	}
	return s.Writer.Write(p)
}

// TestRecorderFinishReportsResealedBytesAfterSinkFailure: the recorder's writer
// goroutine hits a write error and reseals through Finish. The reported byte
// count is the authenticated PCM, and the status is partial with write_failed.
func TestRecorderFinishReportsResealedBytesAfterSinkFailure(t *testing.T) {
	dataDir := t.TempDir()
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x66}, 32)})
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
	w, err := CreateFile(dataDir, "alice", "rec-run", h, key)
	if err != nil {
		t.Fatal(err)
	}
	sink := &failingResealSink{Writer: w}
	r := newRec(t, sink)
	// Enough caller audio for several queued chunks: the second chunk fails.
	for i := 0; i < 8; i++ {
		r.CallerPayload(uint32(i*samplesMS(1000)), l16(samplesMS(1000), int16(i+1)), at(i*1000))
	}
	res, _ := r.Finish(at(8000), false)
	if res.Status != StatusPartial && res.Status != StatusFailed {
		t.Fatalf("status = %q, want partial or failed", res.Status)
	}
	if !hasFlag(res, FlagWriteFailed) {
		t.Fatalf("flags = %v, want write_failed", res.QualityFlags)
	}
	if res.Status == StatusPartial {
		rd := openRecordingFile(t, dataDir, "alice", "rec-run", ring, res.ByteCount)
		if int64(len(rd)) != res.ByteCount {
			t.Fatalf("read %d bytes, byte count %d", len(rd), res.ByteCount)
		}
	}
}
