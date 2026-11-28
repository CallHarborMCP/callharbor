package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// A complete five-segment file with a byte flipped in segment 1 is damage, not a
// torn tail. Recovery keeps segment 0, reseals it, and preserves the original
// byte-identical at <path>.damaged before truncating.
func TestRecoverDamagedMidFilePreservesOriginal(t *testing.T) {
	plain := samplePlain(5 * SegmentSize)
	dataDir, rel, ring := recoverFixture(t, plain, true)
	path := filepath.Join(dataDir, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(data)
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
		t.Fatalf("authenticated bytes = %d, want %d", res.PlainBytes, SegmentSize)
	}
	if res.DamagedCopy != path+".damaged" {
		t.Fatalf("DamagedCopy = %q, want %q", res.DamagedCopy, path+".damaged")
	}
	if res.DiscardedBytes <= 0 {
		t.Fatalf("DiscardedBytes = %d, want the bytes removed from the working file", res.DiscardedBytes)
	}

	copied, err := os.ReadFile(res.DamagedCopy)
	if err != nil {
		t.Fatalf("read preserved original: %v", err)
	}
	if !bytes.Equal(copied, data) {
		t.Fatal("preserved copy is not byte-identical to the damaged file before recovery")
	}
	if bytes.Equal(copied, original) {
		t.Fatal("preserved copy equals the clean original: the damage was not applied")
	}
	info, err := os.Stat(res.DamagedCopy)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preserved copy mode = %v, want 0600", info.Mode().Perm())
	}

	if got := readWhole(t, dataDir, rel, ring, SegmentSize); !bytes.Equal(got, plain[:SegmentSize]) {
		t.Fatal("prefix before the damage does not read back")
	}
}

// A file shorter than one segment, read under the wrong key, has bytes beyond
// the header but nothing that authenticates. That is not a torn write of an
// empty recording, so it must return ErrCorrupt and leave the file untouched,
// so the row stays unsettled for a run with the right key.
func TestRecoverShortFileWrongKeyIsCorruptAndUntouched(t *testing.T) {
	// A crash before any full segment leaves only the header, so bytes are
	// appended after it to model a short segment 0 on disk.
	plain := samplePlain(100)
	dataDir, rel, _ := recoverFixture(t, plain, false)
	other, err := secrets.NewKeyring(1, map[int][]byte{1: bytes.Repeat([]byte{0x55}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, rel)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0x9c}, 100)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoverWith(dataDir, rel, other); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short file, wrong key: err = %v, want ErrCorrupt", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("short file under the wrong key was modified")
	}
}

// Damage confined to a short tail is not preserved: nothing follows it that
// could be authentic audio.
func TestRecoverTornTailIsNotPreserved(t *testing.T) {
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
	if res.Damaged || res.DamagedCopy != "" {
		t.Fatalf("torn tail reported as damage: %+v", res)
	}
	if _, err := os.Stat(path + ".damaged"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("torn tail left a preserved copy: err = %v", err)
	}
}
