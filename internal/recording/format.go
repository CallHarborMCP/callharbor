package recording

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// On-disk format of a .chrec file.
//
//	header (plaintext, HeaderSize bytes):
//	  magic        8 bytes  "CHRECv01" (the version is part of the magic)
//	  key_version  4 bytes  big-endian master key version
//	  salt         32 bytes random per recording, feeds HKDF
//	  sample_rate  4 bytes  big-endian leg sample rate
//	  channels     1 byte   always 2
//	  segment_size 4 bytes  plaintext bytes per segment, always SegmentSize
//	segments: AES-256-GCM STREAM. Every non-final segment carries exactly
//	  SegmentSize plaintext bytes. The final segment may be shorter, including
//	  empty. Ciphertext of segment i is SegmentSize+TagSize bytes, or less for
//	  the final one.
//
// Nonce (12 bytes): big-endian segment counter in bytes 0..10, then one flag
// byte at 11. Flag 0 is a non-final segment. Flag 1 is the authentic final
// segment written by the writer. Flag 2 is an empty final segment sealed by
// recovery or reseal after an authenticated prefix. Flags 1 and 2 are valid
// only on the last segment, and flag 2 is never valid elsewhere. So a
// truncation at a segment boundary cannot pass as complete, and segments cannot
// be reordered.
//
// A flag-2 reseal can repeat at the same counter: recovery of an already
// resealed file, or a second reseal, seals the same empty final again. That
// reuses a nonce, but only with identical plaintext (empty), the same key and
// the same AAD, so the output bytes are identical and no keystream is reused
// with different data. Flag 1 and flag 2 differ in the nonce, so a writer final
// and a resealed final never share a nonce.
//
// AAD: magic || header || 0x00 || recording id || 0x00 || owner id, so a file
// moved to another recording or owner fails authentication.
const (
	magic       = "CHRECv01"
	HeaderSize  = 8 + 4 + SaltSize + 4 + 1 + 4
	SaltSize    = secrets.RecordingSaltSize
	SegmentSize = 16 << 10
	TagSize     = 16
	nonceSize   = 12
)

var (
	// ErrCorrupt reports a file that fails authentication or structure checks.
	// The reader fails closed; no plaintext from a failed segment is returned.
	ErrCorrupt = errors.New("recording: file failed authentication")
	// ErrBadHeader reports a header that is not a valid CHRec header.
	ErrBadHeader = errors.New("recording: bad file header")
)

// Header is the plaintext file header.
type Header struct {
	KeyVersion int
	Salt       [SaltSize]byte
	SampleRate int
}

// Encode returns the HeaderSize-byte encoding of h.
func (h Header) Encode() ([]byte, error) {
	if h.KeyVersion < 1 || h.KeyVersion > 0xFFFFFFFF {
		return nil, fmt.Errorf("%w: key version %d", ErrBadHeader, h.KeyVersion)
	}
	if h.SampleRate < 8000 || h.SampleRate > 48000 {
		return nil, fmt.Errorf("%w: sample rate %d", ErrBadHeader, h.SampleRate)
	}
	b := make([]byte, 0, HeaderSize)
	b = append(b, magic...)
	b = binary.BigEndian.AppendUint32(b, uint32(h.KeyVersion)) // #nosec G115 -- range checked
	b = append(b, h.Salt[:]...)
	b = binary.BigEndian.AppendUint32(b, uint32(h.SampleRate)) // #nosec G115 -- range checked
	b = append(b, Channels)
	b = binary.BigEndian.AppendUint32(b, SegmentSize)
	return b, nil
}

// DecodeHeader parses and validates a header. It does not authenticate; the
// segments do that, using the raw header bytes as AAD.
func DecodeHeader(b []byte) (Header, error) {
	if len(b) != HeaderSize || string(b[:8]) != magic {
		return Header{}, ErrBadHeader
	}
	var h Header
	h.KeyVersion = int(binary.BigEndian.Uint32(b[8:12]))
	copy(h.Salt[:], b[12:12+SaltSize])
	h.SampleRate = int(binary.BigEndian.Uint32(b[12+SaltSize : 16+SaltSize]))
	if b[16+SaltSize] != Channels || binary.BigEndian.Uint32(b[17+SaltSize:]) != SegmentSize {
		return Header{}, ErrBadHeader
	}
	if h.KeyVersion < 1 || h.SampleRate < 8000 || h.SampleRate > 48000 {
		return Header{}, ErrBadHeader
	}
	return h, nil
}

// segmentAAD binds a segment to the recording it belongs to.
func segmentAAD(headerBytes []byte, recordingID, ownerID string) []byte {
	aad := make([]byte, 0, len(headerBytes)+len(recordingID)+len(ownerID)+2)
	aad = append(aad, headerBytes...)
	aad = append(aad, 0)
	aad = append(aad, recordingID...)
	aad = append(aad, 0)
	aad = append(aad, ownerID...)
	return aad
}

// Segment flag bytes, the last nonce byte. They are distinct so that a segment
// sealed by recovery or reseal never shares a nonce with an authentic final
// segment written by the writer.
const (
	flagNonFinal byte = 0
	// flagFinal is written by the writer on the authentic final segment.
	flagFinal byte = 1
	// flagResealed is written by recovery and reseal when they seal an empty
	// final segment after an authenticated prefix. It is valid only on the last
	// segment of a file.
	flagResealed byte = 2
)

// segmentNonce builds the STREAM nonce for segment counter and flag byte.
func segmentNonce(counter uint64, flag byte) []byte {
	n := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(n[3:11], counter)
	n[11] = flag
	return n
}

// streamAEAD is the AES-256-GCM instance for one recording's derived key.
func streamAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != secrets.KeySize {
		return nil, ErrBadHeader
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("recording: cipher: %w", err)
	}
	return cipher.NewGCMWithNonceSize(block, nonceSize)
}

// sealSegment encrypts one segment with the given flag byte.
func sealSegment(aead cipher.AEAD, aad []byte, counter uint64, flag byte, plain []byte) []byte {
	return aead.Seal(nil, segmentNonce(counter, flag), plain, aad)
}

// openFinalOrNon authenticates segment counter. A non-final segment opens only
// with flag 0. The last segment opens with flag 1 (writer) or flag 2 (resealed).
// Any other position is ErrCorrupt for flags 1 and 2.
func openFinalOrNon(aead cipher.AEAD, aad []byte, counter uint64, final bool, sealed []byte) ([]byte, error) {
	if !final {
		return openSegment(aead, aad, counter, flagNonFinal, sealed)
	}
	if pt, err := openSegment(aead, aad, counter, flagFinal, sealed); err == nil {
		return pt, nil
	}
	return openSegment(aead, aad, counter, flagResealed, sealed)
}

// openSegment authenticates and decrypts one segment with the given flag byte.
// It returns ErrCorrupt on any mismatch, including a wrong flag.
func openSegment(aead cipher.AEAD, aad []byte, counter uint64, flag byte, sealed []byte) ([]byte, error) {
	pt, err := aead.Open(nil, segmentNonce(counter, flag), sealed, aad)
	if err != nil {
		return nil, ErrCorrupt
	}
	return pt, nil
}
