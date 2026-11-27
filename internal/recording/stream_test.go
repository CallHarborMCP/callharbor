package recording

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const (
	testRecID = "rec-1"
	testOwner = "alice"
)

func testHeader(t *testing.T) (Header, []byte) {
	t.Helper()
	var h Header
	h.KeyVersion = 1
	h.SampleRate = 16000
	if _, err := rand.Read(h.Salt[:]); err != nil {
		t.Fatal(err)
	}
	b, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return h, b
}

// writeFile writes plaintext in chunks of chunk bytes and closes the file.
func writeFile(t *testing.T, dir string, plain []byte, chunk int) (string, Header, []byte, []byte) {
	t.Helper()
	h, hb := testHeader(t)
	key := bytes.Repeat([]byte{0x42}, 32)
	path := filepath.Join(dir, "f.chrec")
	w, err := NewWriter(path, h, key, testRecID, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(plain); off += chunk {
		end := min(off+chunk, len(plain))
		if err := w.Write(plain[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path, h, hb, key
}

func openReader(t *testing.T, path string, h Header, hb, key []byte, id, owner string, plainLen int64) (*Reader, *os.File) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(f, st.Size(), hb, h, key, id, owner, plainLen)
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	return r, f
}

func samplePlain(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestStreamRoundTripAcrossSegmentBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, SegmentSize - 1, SegmentSize, SegmentSize + 1, 3 * SegmentSize, 3*SegmentSize + 17} {
		plain := samplePlain(n)
		dir := t.TempDir()
		path, h, hb, key := writeFile(t, dir, plain, 4096)
		r, f := openReader(t, path, h, hb, key, testRecID, testOwner, int64(n))
		got, err := io.ReadAll(r)
		_ = f.Close()
		if err != nil {
			t.Fatalf("n=%d: read: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: plaintext mismatch", n)
		}
	}
}

func TestStreamReadSeekRangesAcrossSegments(t *testing.T) {
	plain := samplePlain(3*SegmentSize + 100)
	path, h, hb, key := writeFile(t, t.TempDir(), plain, 1000)
	r, f := openReader(t, path, h, hb, key, testRecID, testOwner, int64(len(plain)))
	defer func() { _ = f.Close() }()

	// A read that starts in the last byte of segment 0 and spans into 1 and 2.
	start := int64(SegmentSize - 1)
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2*SegmentSize)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf[:n], plain[start:start+int64(n)]) {
		t.Fatal("range across boundaries mismatched")
	}
	// Seek from end, then read the tail.
	if _, err := r.Seek(-50, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(tail, plain[len(plain)-50:]) {
		t.Fatalf("tail mismatch: %v", err)
	}
}

func TestStreamTamperFailsClosed(t *testing.T) {
	plain := samplePlain(2*SegmentSize + 10)
	path, h, hb, key := writeFile(t, t.TempDir(), plain, SegmentSize)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte inside segment 1's ciphertext.
	data[HeaderSize+(SegmentSize+TagSize)+10] ^= 0x01
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r, f := openReader(t, path, h, hb, key, testRecID, testOwner, int64(len(plain)))
	defer func() { _ = f.Close() }()
	// Segment 0 is intact and still reads.
	seg0 := make([]byte, SegmentSize)
	if _, err := io.ReadFull(r, seg0); err != nil || !bytes.Equal(seg0, plain[:SegmentSize]) {
		t.Fatalf("intact segment: %v", err)
	}
	// Segment 1 fails, and returns no bytes from it.
	seg1 := make([]byte, SegmentSize)
	n, err := r.Read(seg1)
	if !errors.Is(err, ErrCorrupt) || n != 0 {
		t.Fatalf("tampered segment: n=%d err=%v, want 0 and ErrCorrupt", n, err)
	}
}

func TestStreamWrongAADOrKeyFails(t *testing.T) {
	plain := samplePlain(100)
	path, h, hb, key := writeFile(t, t.TempDir(), plain, 100)

	cases := map[string]struct {
		id, owner string
		key       []byte
	}{
		"wrong recording id": {"rec-2", testOwner, key},
		"wrong owner":        {testRecID, "bob", key},
		"wrong key":          {testRecID, testOwner, bytes.Repeat([]byte{0x43}, 32)},
	}
	for name, tc := range cases {
		r, f := openReader(t, path, h, hb, tc.key, tc.id, tc.owner, int64(len(plain)))
		_, err := io.ReadAll(r)
		_ = f.Close()
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestStreamDetectsTruncationAndReorder(t *testing.T) {
	plain := samplePlain(3 * SegmentSize)
	path, h, hb, key := writeFile(t, t.TempDir(), plain, SegmentSize)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	segStride := SegmentSize + TagSize
	body := data[HeaderSize:]

	// Drop the final segment: the layout check against plainLen fails.
	trunc := append([]byte(nil), data[:len(data)-segStride]...)
	if err := os.WriteFile(path, trunc, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(bytes.NewReader(trunc), int64(len(trunc)), hb, h, key, testRecID, testOwner, int64(len(plain))); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated layout: err = %v, want ErrCorrupt", err)
	}

	// Swap two complete segments: each still authenticates only at its own
	// counter, so reading the swapped file fails.
	swapped := append([]byte(nil), data[:HeaderSize]...)
	swapped = append(swapped, body[segStride:2*segStride]...)
	swapped = append(swapped, body[:segStride]...)
	swapped = append(swapped, body[2*segStride:]...)
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatal(err)
	}
	r, f := openReader(t, path, h, hb, key, testRecID, testOwner, int64(len(plain)))
	defer func() { _ = f.Close() }()
	if _, err := io.ReadAll(r); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("reordered segments: err = %v, want ErrCorrupt", err)
	}
}

func TestStreamFinalFlagBindsSegmentToPosition(t *testing.T) {
	// A segment sealed as non-final must not open as final. Otherwise a file
	// truncated at a full segment boundary would pass as a complete recording.
	key := bytes.Repeat([]byte{0x42}, 32)
	aead, err := streamAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	aad := segmentAAD([]byte("hdr"), testRecID, testOwner)
	sealed := sealSegment(aead, aad, 0, false, samplePlain(SegmentSize))
	if _, err := openSegment(aead, aad, 0, true, sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("non-final segment opened as final: err = %v", err)
	}
	if _, err := openSegment(aead, aad, 0, false, sealed); err != nil {
		t.Fatalf("non-final segment must open as non-final: %v", err)
	}
	if _, err := openSegment(aead, aad, 1, false, sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("segment opened at wrong counter: err = %v", err)
	}
}

func TestWriterRefusesToOverwriteExistingFile(t *testing.T) {
	dir := t.TempDir()
	path, h, _, key := writeFile(t, dir, samplePlain(10), 10)
	if _, err := NewWriter(path, h, key, testRecID, testOwner); err == nil {
		t.Fatal("existing file was overwritten")
	}
}

func TestWriterFilePermissions(t *testing.T) {
	path, _, _, _ := writeFile(t, t.TempDir(), samplePlain(10), 10)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", st.Mode().Perm())
	}
}
