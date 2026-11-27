package recording

import (
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// ErrMissing reports that the recording file does not exist.
var ErrMissing = errors.New("recording: file missing")

// ErrNoKeyring reports that no master key is configured, so no file can be
// authenticated. Recovery fails closed rather than guessing.
var ErrNoKeyring = errors.New("recording: no master key configured")

// RecoverInput identifies one interrupted recording.
type RecoverInput struct {
	DataDir     string
	RelPath     string
	RecordingID string
	OwnerID     string
	KeyVersion  int
	Ring        *secrets.Keyring
}

// RecoverResult is the authenticated content of an interrupted file.
type RecoverResult struct {
	// PlainBytes is the authenticated PCM length, a whole number of frames.
	PlainBytes int64
	// Complete is true when the final segment authenticated, meaning the
	// recording had been finalized in the file but not in the database.
	Complete   bool
	SampleRate int
}

// Recover authenticates an interrupted .chrec file as far as it is intact and
// truncates the unauthenticated tail. Segments are sealed in order, so the
// authenticated prefix is exactly the segments that open with their counter
// and non-final flag. A partial trailing segment is dropped. The file is never
// returned to a reader; the caller records the result.
func Recover(in RecoverInput) (RecoverResult, error) {
	if in.Ring == nil {
		return RecoverResult{}, ErrNoKeyring
	}
	path := filepath.Join(in.DataDir, filepath.FromSlash(in.RelPath))
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- RelPath is checked against the id-derived path by the caller before recovery
	if errors.Is(err, os.ErrNotExist) {
		return RecoverResult{}, ErrMissing
	}
	if err != nil {
		return RecoverResult{}, fmt.Errorf("recording: open for recovery: %w", err)
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, head); err != nil {
		return RecoverResult{}, ErrBadHeader
	}
	h, err := DecodeHeader(head)
	if err != nil {
		return RecoverResult{}, err
	}
	key, err := in.Ring.DeriveRecordingKey(in.KeyVersion, h.Salt[:])
	if err != nil {
		return RecoverResult{}, err
	}
	aead, err := streamAEAD(key)
	if err != nil {
		return RecoverResult{}, err
	}
	aad := segmentAAD(head, in.RecordingID, in.OwnerID)

	st, err := f.Stat()
	if err != nil {
		return RecoverResult{}, fmt.Errorf("recording: stat: %w", err)
	}
	keep, plain, complete, err := scanSegments(f, st.Size(), aead, aad)
	if err != nil {
		return RecoverResult{}, err
	}
	// Truncate only when some segment authenticated. With none, a short tail is
	// indistinguishable from a wrong key over a torn write, so the file is kept.
	if keep != st.Size() && keep > HeaderSize {
		if err := f.Truncate(keep); err != nil {
			return RecoverResult{}, fmt.Errorf("recording: truncate tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			return RecoverResult{}, fmt.Errorf("recording: sync after truncate: %w", err)
		}
	}
	return RecoverResult{PlainBytes: plain, Complete: complete, SampleRate: h.SampleRate}, nil
}

// scanSegments walks the segments and returns the byte offset just past the
// last authenticated segment, the plaintext it covers, and whether the final
// flag authenticated. A non-final segment is always full, so a short segment
// is the tail and ends the walk.
func scanSegments(f io.ReaderAt, size int64, aead cipher.AEAD, aad []byte) (keep, plain int64, complete bool, err error) {
	stride := int64(SegmentSize + TagSize)
	off := int64(HeaderSize)
	var idx uint64
	buf := make([]byte, stride)
	for {
		remain := size - off
		if remain <= 0 {
			return off, plain, false, nil
		}
		n := min(remain, stride)
		sealed := buf[:n]
		if _, err := f.ReadAt(sealed, off); err != nil && (!errors.Is(err, io.EOF) || int64(n) != remain) {
			return off, plain, false, fmt.Errorf("recording: read during recovery: %w", err)
		}
		// Try as final first: a final segment is the only one shorter than full
		// that can authenticate. A full segment authenticates as non-final.
		if pt, ok := tryOpen(aead, aad, idx, true, sealed); ok {
			return off + n, plain + int64(len(pt)), true, nil
		}
		if n < stride {
			// Short and not final: a torn tail. Keep what authenticated before.
			return off, plain, false, nil
		}
		pt, ok := tryOpen(aead, aad, idx, false, sealed)
		if !ok {
			// A full-size segment that fails is damage or a wrong key, not a torn
			// write: a crash can only leave a shorter tail. Refuse the file rather
			// than truncate away the evidence.
			return off, plain, false, ErrCorrupt
		}
		plain += int64(len(pt))
		off += stride
		idx++
	}
}

func tryOpen(aead cipher.AEAD, aad []byte, idx uint64, final bool, sealed []byte) ([]byte, bool) {
	pt, err := openSegment(aead, aad, idx, final, sealed)
	if err != nil {
		return nil, false
	}
	return pt, true
}
