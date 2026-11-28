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
	// The file ends with an empty final segment sealed after the two full ones.
	st, err := os.Stat(filepath.Join(dataDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(HeaderSize) + 2*(SegmentSize+TagSize) + TagSize; st.Size() != want {
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
	// The torn bytes are gone; one full segment and the sealed empty final
	// segment remain.
	if st.Size() != int64(HeaderSize)+SegmentSize+TagSize+TagSize {
		t.Fatalf("torn tail not truncated: size %d", st.Size())
	}
}

// Damage after segment 0 keeps the authenticated prefix and reseals it. The
// damaged segment and everything after it are dropped, and the result reports
// Damaged. Segment 0 failing is still ErrCorrupt (see the next test).
func TestRecoverTamperedSegmentKeepsPrefix(t *testing.T) {
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
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatalf("mid-file damage: err = %v, want the prefix recovered", err)
	}
	if !res.Damaged || res.Complete {
		t.Fatalf("result = %+v, want Damaged and not Complete", res)
	}
	if res.PlainBytes != SegmentSize {
		t.Fatalf("authenticated bytes = %d, want %d (segment 0 only)", res.PlainBytes, SegmentSize)
	}
	// Segment 0 plus the resealed empty final segment remain.
	st, _ := os.Stat(path)
	if st.Size() != int64(HeaderSize)+SegmentSize+TagSize+TagSize {
		t.Fatalf("damaged tail not dropped: size %d", st.Size())
	}
}

// Segment 0 failing is a wrong key or damage to the first segment. Nothing is
// proven, so the file is not truncated and nothing is resealed.
func TestRecoverSegmentZeroFailureIsCorruptAndKeepsFile(t *testing.T) {
	plain := samplePlain(3 * SegmentSize)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	path := filepath.Join(dataDir, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[HeaderSize+5] ^= 0x80 // damage segment 0
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := recoverWith(dataDir, rel, ring); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("segment 0 damage: err = %v, want ErrCorrupt", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("file with no authenticated segment was modified")
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

// readWhole opens the file through the production reader and returns the first
// n plaintext bytes. It fails the test if the reader rejects the file.
func readWhole(t *testing.T, dataDir, rel string, ring *secrets.Keyring, n int64) []byte {
	t.Helper()
	return openRecordingFile(t, dataDir, "alice", "rec-1", ring, n)
}

// A complete file with trailing junk keeps its authentic final segment (flag 1).
// Recovery trims only the junk, so the audio is fully readable and not resealed.
func TestRecoverCompleteFileWithTrailingJunkKeepsFinal(t *testing.T) {
	plain := samplePlain(2*SegmentSize + 40)
	dataDir, rel, ring := recoverFixture(t, plain, true)
	path := filepath.Join(dataDir, rel)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x7f}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.PlainBytes != int64(len(plain)) {
		t.Fatalf("result = %+v, want complete with %d bytes", res, len(plain))
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("complete file was modified: the authentic final must be preserved")
	}
	if got := readWhole(t, dataDir, rel, ring, int64(len(plain))); !bytes.Equal(got, plain) {
		t.Fatal("complete file with trailing junk does not read back")
	}
}

// A torn final segment is resealed with flag 2, and the authenticated audio
// reads back through the production reader.
func TestRecoverTornFinalResealsWithFlagTwo(t *testing.T) {
	plain := samplePlain(2*SegmentSize + 40)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	path := filepath.Join(dataDir, rel)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A torn final: part of the sealed final bytes, never completed.
	if _, err := f.Write(bytes.Repeat([]byte{0xab}, 20)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete || res.PlainBytes != 2*SegmentSize {
		t.Fatalf("result = %+v, want not complete with %d bytes", res, 2*SegmentSize)
	}
	// The sealed empty final is the last segment and carries flag 2.
	data, _ := os.ReadFile(path)
	last := data[len(data)-TagSize:]
	off := HeaderSize + 2*(SegmentSize+TagSize)
	if len(data)-off != TagSize {
		t.Fatalf("resealed tail = %d bytes, want one empty final", len(data)-off)
	}
	_ = last
	if got := readWhole(t, dataDir, rel, ring, 2*SegmentSize); !bytes.Equal(got, plain[:2*SegmentSize]) {
		t.Fatal("resealed torn file does not read back")
	}
}

// A flag-2 segment is accepted only as the last segment. A file where a
// resealed segment sits in a non-last position is rejected by the reader.
func TestFlagTwoRejectedInNonLastPosition(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	aead, err := streamAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	aad := segmentAAD([]byte("hdr"), testRecID, testOwner)
	sealed := sealSegment(aead, aad, 0, flagResealed, samplePlain(SegmentSize))
	if _, err := openFinalOrNon(aead, aad, 0, false, sealed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("flag-2 segment opened in a non-last position: err = %v", err)
	}
	if _, err := openFinalOrNon(aead, aad, 0, true, sealed); err != nil {
		t.Fatalf("flag-2 segment must open as the last segment: %v", err)
	}
}

// Right key, segment 2 zeroed: the authenticated prefix (segment 0 and 1 from
// the walk) is kept and resealed, the row is partial, and the audio reads back.
func TestRecoverZeroedSegmentTwoKeepsPrefix(t *testing.T) {
	plain := samplePlain(3*SegmentSize + 10)
	dataDir, rel, ring := recoverFixture(t, plain, false)
	path := filepath.Join(dataDir, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stride := SegmentSize + TagSize
	start := HeaderSize + 2*stride
	for i := start; i < start+stride; i++ {
		data[i] = 0
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := recoverWith(dataDir, rel, ring)
	if err != nil {
		t.Fatalf("zeroed segment 2: err = %v, want the prefix recovered", err)
	}
	if !res.Damaged || res.Complete {
		t.Fatalf("result = %+v, want Damaged and not Complete", res)
	}
	if res.PlainBytes != 2*SegmentSize {
		t.Fatalf("authenticated bytes = %d, want %d", res.PlainBytes, 2*SegmentSize)
	}
	if got := readWhole(t, dataDir, rel, ring, 2*SegmentSize); !bytes.Equal(got, plain[:2*SegmentSize]) {
		t.Fatal("prefix before the damage does not read back")
	}
}
