package recording

import (
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"os"
)

// Writer appends encrypted STREAM segments to a .chrec file. It buffers at
// most one segment of plaintext. It is not safe for concurrent use; the
// recorder's writer goroutine owns it.
type Writer struct {
	f       *os.File
	aead    cipher.AEAD
	aad     []byte
	counter uint64
	buf     []byte
	// bytes counts plaintext bytes accepted, including those still buffered.
	bytes int64
	// sealed counts plaintext bytes that were sealed and written in full
	// segments. Buffered bytes are not counted: they are never in the file.
	sealed int64
	closed bool
}

// NewWriter creates the file at path with mode 0600 and writes the header.
// The header's salt and key version select the key the caller derived with
// secrets.Keyring.DeriveRecordingKey. The file is created exclusively, so an
// existing recording is never overwritten.
func NewWriter(path string, h Header, key []byte, recordingID, ownerID string) (*Writer, error) {
	headerBytes, err := h.Encode()
	if err != nil {
		return nil, err
	}
	aead, err := streamAEAD(key)
	if err != nil {
		return nil, err
	}
	// O_RDWR: a failed writer reseals in place, which reads the file back.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path comes from Path, which validates the owner and recording ids
	if err != nil {
		return nil, fmt.Errorf("recording: create file: %w", err)
	}
	if _, err := f.Write(headerBytes); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("recording: write header: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("recording: sync header: %w", err)
	}
	return &Writer{
		f:    f,
		aead: aead,
		aad:  segmentAAD(headerBytes, recordingID, ownerID),
		buf:  make([]byte, 0, SegmentSize),
	}, nil
}

// Write appends plaintext PCM. Full segments are sealed and written as they
// fill. A write error leaves the file closed for further writes.
func (w *Writer) Write(p []byte) error {
	if w.closed {
		return errors.New("recording: writer closed")
	}
	for len(p) > 0 {
		n := min(SegmentSize-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		w.bytes += int64(n)
		if len(w.buf) == SegmentSize {
			// A full segment is never final until the stream ends, so it is
			// written non-final. Sealing a full segment as final would let a
			// boundary truncation pass as complete.
			if err := w.emit(); err != nil {
				return err
			}
		}
	}
	return nil
}

// emit seals the buffered plaintext as the next segment and writes it.
func (w *Writer) emit() error {
	sealed := sealSegment(w.aead, w.aad, w.counter, flagNonFinal, w.buf)
	if _, err := w.f.Write(sealed); err != nil {
		w.closed = true
		return fmt.Errorf("recording: write segment: %w", err)
	}
	w.counter++
	w.sealed += int64(len(w.buf))
	w.buf = w.buf[:0]
	return nil
}

// SealedBytes is the plaintext byte count written in full segments so far.
// After a write failure it is the upper bound a reseal can authenticate.
func (w *Writer) SealedBytes() int64 { return w.sealed }

// Sync makes every written byte durable. It does not seal the buffered
// plaintext: a partial segment is recovered by the crash path as an
// unauthenticated tail and is dropped there.
func (w *Writer) Sync() error {
	if w.closed {
		return errors.New("recording: writer closed")
	}
	if err := w.f.Sync(); err != nil {
		w.closed = true
		return fmt.Errorf("recording: fsync: %w", err)
	}
	return nil
}

// Bytes is the plaintext byte count accepted so far.
func (w *Writer) Bytes() int64 { return w.bytes }

// Reseal closes a writer whose writes failed. It runs the same authenticated
// prefix scan as crash recovery on the file as it stands: the torn tail is
// truncated, an empty final segment is sealed after the last authenticated
// segment, and the file is synced. The buffered plaintext is discarded, since
// it never reached the file. It returns the authenticated PCM length, which the
// caller records as the byte count. An error means nothing was made readable.
func (w *Writer) Reseal() (int64, error) {
	w.closed = true
	st, err := w.f.Stat()
	if err != nil {
		_ = w.f.Close()
		return 0, fmt.Errorf("recording: stat failed writer: %w", err)
	}
	sc, err := scanSegments(w.f, st.Size(), w.aead, w.aad)
	if err != nil {
		_ = w.f.Close()
		return 0, err
	}
	if sc.complete {
		// A final segment was written before the failure. Trim any bytes after it
		// and leave the authentic final in place.
		if sc.keep != st.Size() {
			if err := w.f.Truncate(sc.keep); err != nil {
				_ = w.f.Close()
				return 0, fmt.Errorf("recording: truncate failed writer: %w", err)
			}
		}
		return sc.plain, w.f.Close()
	}
	if sc.keep <= int64(HeaderSize) {
		_ = w.f.Close()
		return 0, ErrCorrupt
	}
	if sc.keep != st.Size() {
		if err := w.f.Truncate(sc.keep); err != nil {
			_ = w.f.Close()
			return 0, fmt.Errorf("recording: truncate failed writer: %w", err)
		}
	}
	if err := sealPrefixFinal(w.f, sc.keep, sc.next, w.aead, w.aad); err != nil {
		_ = w.f.Close()
		return 0, err
	}
	return sc.plain, w.f.Close()
}

// Close seals the buffered tail as the final segment, syncs, and closes the
// file. The final segment may be empty; the final flag is what makes the
// recording complete.
func (w *Writer) Close() error {
	if w.closed {
		return w.f.Close()
	}
	w.closed = true
	if err := w.sealFinal(); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return fmt.Errorf("recording: fsync final: %w", err)
	}
	return w.f.Close()
}

func (w *Writer) sealFinal() error {
	sealed := sealSegment(w.aead, w.aad, w.counter, flagFinal, w.buf)
	if _, err := w.f.Write(sealed); err != nil {
		return fmt.Errorf("recording: write final segment: %w", err)
	}
	w.counter++
	w.buf = w.buf[:0]
	return nil
}

// Reader is an io.ReadSeeker over the plaintext of an authenticated .chrec
// file. It decrypts only the segments a read touches, and it authenticates
// each one before returning any of its bytes. A failed segment returns
// ErrCorrupt and no bytes from that segment.
type Reader struct {
	f         io.ReaderAt
	aead      cipher.AEAD
	aad       []byte
	plainLen  int64 // total plaintext bytes
	pos       int64 // plaintext offset
	cacheIdx  int64 // segment index in cache, -1 when empty
	cache     []byte
	segStride int64
}

// NewReader opens an authenticated view over ra, which is size bytes long.
// headerBytes is the raw encoded header and h its decoded form; the caller
// decoded h from headerBytes and derived key from it. plainLen is the
// plaintext length recorded for the recording. A layout that does not match
// the file size is ErrCorrupt.
func NewReader(ra io.ReaderAt, size int64, headerBytes []byte, h Header, key []byte, recordingID, ownerID string, plainLen int64) (*Reader, error) {
	aead, err := streamAEAD(key)
	if err != nil {
		return nil, err
	}
	decoded, err := DecodeHeader(headerBytes)
	if err != nil || decoded != h {
		return nil, ErrBadHeader
	}
	if plainLen < 0 {
		return nil, ErrCorrupt
	}
	want := int64(HeaderSize) + plainLen + (finalIndex(plainLen)+1)*TagSize
	if size != want {
		return nil, ErrCorrupt
	}
	return &Reader{
		f:         ra,
		aead:      aead,
		aad:       segmentAAD(headerBytes, recordingID, ownerID),
		plainLen:  plainLen,
		cacheIdx:  -1,
		segStride: SegmentSize + TagSize,
	}, nil
}

// finalIndex is the index of the final segment for plainLen plaintext bytes.
// The writer emits every full segment as non-final, then seals the remainder,
// which may be empty, as the final one. So the final index is floor(n/S).
func finalIndex(plainLen int64) int64 { return plainLen / SegmentSize }

// segmentBounds returns the file offset, sealed length, plaintext length, and
// whether segment i is the final one. Only the final segment can carry flag 1
// or flag 2; loadSegment tries those two on it and nothing else.
func (r *Reader) segmentBounds(i int64) (off, sealedLen, plainLen int64, final bool) {
	off = int64(HeaderSize) + i*r.segStride
	final = i == finalIndex(r.plainLen)
	if final {
		plainLen = r.plainLen - i*SegmentSize
	} else {
		plainLen = SegmentSize
	}
	return off, plainLen + TagSize, plainLen, final
}

// loadSegment decrypts segment i into the cache.
func (r *Reader) loadSegment(i int64) error {
	if i == r.cacheIdx {
		return nil
	}
	off, sealedLen, _, final := r.segmentBounds(i)
	sealed := make([]byte, sealedLen)
	n, err := r.f.ReadAt(sealed, off)
	if n != len(sealed) {
		if err == nil || errors.Is(err, io.EOF) {
			return ErrCorrupt
		}
		return fmt.Errorf("recording: read segment: %w", err)
	}
	pt, err := openFinalOrNon(r.aead, r.aad, uint64(i), final, sealed) // #nosec G115 -- segment index i is non-negative by construction
	if err != nil {
		r.cacheIdx = -1
		return err
	}
	r.cache = pt
	r.cacheIdx = i
	return nil
}

// Read implements io.Reader over plaintext.
func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.plainLen {
		return 0, io.EOF
	}
	total := 0
	for total < len(p) && r.pos < r.plainLen {
		idx := r.pos / SegmentSize
		if err := r.loadSegment(idx); err != nil {
			return total, err
		}
		inSeg := r.pos - idx*SegmentSize
		n := copy(p[total:], r.cache[inSeg:])
		total += n
		r.pos += int64(n)
	}
	return total, nil
}

// Seek implements io.Seeker over plaintext.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.plainLen + offset
	default:
		return 0, errors.New("recording: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("recording: negative seek")
	}
	r.pos = abs
	return abs, nil
}

// PlainLen is the plaintext length the reader serves.
func (r *Reader) PlainLen() int64 { return r.plainLen }
