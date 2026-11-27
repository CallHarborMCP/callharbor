package recording

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// recoverFixture writes a recording through the production writer under a
// data directory and returns the ring and relative path for recovery.
func recoverFixture(t *testing.T, plain []byte, closeIt bool) (dataDir, rel string, ring *secrets.Keyring) {
	t.Helper()
	dataDir = t.TempDir()
	master := bytes.Repeat([]byte{0x11}, 32)
	var err error
	ring, err = secrets.NewKeyring(1, map[int][]byte{1: master})
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
	if err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if closeIt {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		// A crash: flush what was written without a final segment.
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, RelPath("alice", "rec-1"), ring
}

func recoverWith(dataDir, rel string, ring *secrets.Keyring) (RecoverResult, error) {
	return Recover(RecoverInput{
		DataDir: dataDir, RelPath: rel, RecordingID: "rec-1", OwnerID: "alice",
		KeyVersion: 1, Ring: ring,
	})
}

func TestRecoverCompleteFileReportsComplete(t *testing.T) {
	plain := samplePlain(2*SegmentSize + 40)
	dataDir, rel, ring := recoverFixture(t, plain, true)
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.PlainBytes != int64(len(plain)) {
		t.Fatalf("result = %+v, want complete %d bytes", res, len(plain))
	}
}

func TestRecoverCrashedFileKeepsAuthenticatedPrefix(t *testing.T) {
	// Full segments are written; the final segment is never sealed. Recovery
	// keeps the full authenticated segments and reports not complete.
	plain := samplePlain(2*SegmentSize + 40)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatal("crashed file reported complete")
	}
	if res.PlainBytes != 2*SegmentSize {
		t.Fatalf("authenticated bytes = %d, want %d", res.PlainBytes, 2*SegmentSize)
	}
	// The file on disk now ends at the last authenticated segment.
	st, err := os.Stat(filepath.Join(dataDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(HeaderSize) + 2*(SegmentSize+TagSize); st.Size() != want {
		t.Fatalf("size after recovery = %d, want %d", st.Size(), want)
	}
}

func TestRecoverTruncatesTornTail(t *testing.T) {
	plain := samplePlain(SegmentSize + 10)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	path := filepath.Join(dataDir, rel)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
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
	if res.PlainBytes != SegmentSize {
		t.Fatalf("authenticated bytes = %d, want %d", res.PlainBytes, SegmentSize)
	}
	st, _ := os.Stat(path)
	if st.Size() != int64(HeaderSize)+SegmentSize+TagSize {
		t.Fatalf("torn tail not truncated: size %d", st.Size())
	}
}

func TestRecoverTamperedSegmentStopsAtDamage(t *testing.T) {
	plain := samplePlain(3 * SegmentSize)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	path := filepath.Join(dataDir, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[HeaderSize+(SegmentSize+TagSize)+5] ^= 0x80 // damage segment 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := recoverWith(dataDir, rel, ring); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("mid-file damage: err = %v, want ErrCorrupt", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("damaged file was modified; evidence must be kept")
	}
}

func TestRecoverMissingFile(t *testing.T) {
	dataDir, rel, ring := recoverFixture(t, samplePlain(10), true)
	if err := os.Remove(filepath.Join(dataDir, rel)); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverWith(dataDir, rel, ring); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing file: err = %v, want ErrMissing", err)
	}
}

func TestRecoverWrongKeyFailsClosed(t *testing.T) {
	// A full segment must exist: a short tail under the wrong key is
	// indistinguishable from a torn write and is truncated by design.
	dataDir, rel, _ := recoverFixture(t, samplePlain(SegmentSize+100), false)
	other, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x22}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, rel)
	before, _ := os.ReadFile(path)
	// Under the wrong key a full segment cannot authenticate. That must fail
	// closed and leave the file untouched, never truncate it to the header.
	if _, err := recoverWith(dataDir, rel, other); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key: err = %v, want ErrCorrupt", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("wrong key truncated the recording")
	}
}
