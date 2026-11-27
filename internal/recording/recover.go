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
	Complete bool
	// Damaged is true when a full segment after the first failed to
	// authenticate. The audio before it is kept; the rest is not recoverable.
	Damaged    bool
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
	// #nosec G304 -- path is joined from in.DataDir and in.RelPath. RelPath must be the id-derived path; callstream.recoverOne checks that before calling Recover, and the caller must do the same.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
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
	sc, err := scanSegments(f, st.Size(), aead, aad)
	if err != nil {
		return RecoverResult{}, err
	}
	// Truncate only when some segment authenticated. With none, a short tail is
	// indistinguishable from a wrong key over a torn write, so the file is kept.
	// A complete file is trimmed only after its authentic final, so the trim
	// never removes it.
	if sc.keep > HeaderSize && sc.keep != st.Size() {
		if err := f.Truncate(sc.keep); err != nil {
			return RecoverResult{}, fmt.Errorf("recording: truncate tail: %w", err)
		}
	}
	if sc.complete {
		return RecoverResult{PlainBytes: sc.plain, Complete: true, SampleRate: h.SampleRate}, nil
	}
	// The authenticated prefix has no final segment, so the reader could not
	// open it. Seal an empty resealed final (flag 2) after the prefix, at the
	// next counter, so the recovered audio is readable. A prefix is always whole
	// non-final segments, because the scan never keeps a short one.
	if sc.keep > HeaderSize {
		if err := sealPrefixFinal(f, sc.keep, sc.next, aead, aad); err != nil {
			return RecoverResult{}, err
		}
	}
	return RecoverResult{PlainBytes: sc.plain, Complete: false, SampleRate: h.SampleRate, Damaged: sc.damaged}, nil
}

// sealPrefixFinal appends an empty final segment at counter segments and syncs
// the file. The caller guarantees the prefix holds only full non-final segments.
func sealPrefixFinal(f *os.File, off int64, counter uint64, aead cipher.AEAD, aad []byte) error {
	sealed := sealSegment(aead, aad, counter, flagResealed, nil)
	if _, err := f.WriteAt(sealed, off); err != nil {
		return fmt.Errorf("recording: reseal final segment: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("recording: sync reseal: %w", err)
	}
	return nil
}

// scanSegments walks the segments and returns the byte offset just past the
// last authenticated segment, the plaintext it covers, whether the final flag
// authenticated, and the counter the next segment would carry. A non-final
// segment is always full, so a short segment is the tail and ends the walk.
// scanResult is the authenticated content of a walk over one file.
type scanResult struct {
	// keep is the byte offset just past the last authenticated segment. Bytes
	// after it are unauthenticated and are truncated.
	keep int64
	// plain is the PCM length the authenticated segments cover.
	plain int64
	// complete is true when an authentic final segment (flag 1) ends the file,
	// so no resealing is needed. Trailing bytes after it are trimmed.
	complete bool
	// next is the counter the next segment would carry.
	next uint64
	// damaged is true when a full-size segment after segment 0 failed to
	// authenticate. The prefix before it is kept and resealed; the damage is
	// real and the row is settled as partial with the recovered flag.
	damaged bool
}

// scanSegments walks the segments in order and reports the authenticated
// prefix. Segment 0 failing returns ErrCorrupt: with no authenticated segment,
// the key or file cannot be trusted, so the row stays unsettled. Any later
// failure is treated as damage, and the walk stops before it.
//
// A complete file ends in an authentic flag-1 final. The tail is tried as that
// final before it is treated as torn, so a complete file with trailing junk
// keeps its authentic final segment and only the junk is trimmed.
func scanSegments(f io.ReaderAt, size int64, aead cipher.AEAD, aad []byte) (scanResult, error) {
	stride := int64(SegmentSize + TagSize)
	off := int64(HeaderSize)
	var idx uint64
	var res scanResult
	buf := make([]byte, stride)
	for {
		remain := size - off
		if remain <= 0 {
			res.keep, res.next = off, idx
			return res, nil
		}
		n := min(remain, stride)
		sealed := buf[:n]
		if _, err := f.ReadAt(sealed, off); err != nil && (!errors.Is(err, io.EOF) || int64(n) != remain) {
			return res, fmt.Errorf("recording: read during recovery: %w", err)
		}
		// A full-size window is a non-final segment: a final is always shorter
		// than a full segment. Only a full window authenticated as non-final
		// advances the walk.
		if n == stride {
			if pt, ok := tryOpen(aead, aad, idx, flagNonFinal, sealed); ok {
				res.plain += int64(len(pt))
				off += stride
				idx++
				continue
			}
		}
		// Otherwise the window may start with an authentic final, followed by
		// bytes that are not part of the recording (a complete file with trailing
		// junk). The final's length is not stored, so findFinal tries each one.
		if cl, pl, ok := findFinal(aead, aad, idx, sealed); ok {
			res.keep, res.plain, res.complete, res.next = off+int64(cl), res.plain+int64(pl), true, idx+1
			return res, nil
		}
		if n < stride {
			// No final authenticates in a short tail: a torn write. Keep what
			// authenticated before it.
			res.keep, res.next = off, idx
			return res, nil
		}
		if idx == 0 {
			// Segment 0 failing is a wrong key, a rotated key, or damage to the
			// first segment. Nothing authenticates, so nothing is kept.
			return res, ErrCorrupt
		}
		// A later full-size segment failing is damage after authenticated audio.
		// A crash only leaves a shorter tail, so the prefix before it is real.
		res.keep, res.next, res.damaged = off, idx, true
		return res, nil
	}
}

// findFinal looks for an authentic final segment at the start of window. A
// final is shorter than a full segment and its length is not stored, so every
// length from one tag up to the window is tried with each final flag. The first
// that authenticates is the final, and bytes after it are not part of the
// recording. It returns the sealed and plaintext lengths. The cost is bounded
// by one segment: about SegmentSize authentication attempts per call, made only
// for a window that is a torn tail or a damaged segment.
func findFinal(aead cipher.AEAD, aad []byte, idx uint64, window []byte) (sealedLen, plainLen int, ok bool) {
	scratch := make([]byte, 0, len(window))
	for l := TagSize; l <= len(window); l++ {
		for _, flag := range [...]byte{flagFinal, flagResealed} {
			pt, err := aead.Open(scratch[:0], segmentNonce(idx, flag), window[:l], aad)
			if err == nil {
				return l, len(pt), true
			}
		}
	}
	return 0, 0, false
}

// tryOpen authenticates one segment with the given flag byte.
func tryOpen(aead cipher.AEAD, aad []byte, idx uint64, flag byte, sealed []byte) ([]byte, bool) {
	pt, err := openSegment(aead, aad, idx, flag, sealed)
	if err != nil {
		return nil, false
	}
	return pt, true
}
