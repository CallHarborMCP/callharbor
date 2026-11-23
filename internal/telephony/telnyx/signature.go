// Package telnyx implements the Telnyx Call Control and bidirectional media
// WebSocket contracts used by CallHarbor.
package telnyx

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Header names used by Telnyx for webhook and media WebSocket signatures.
const (
	HeaderSignature = "Telnyx-Signature-Ed25519"
	HeaderTimestamp = "Telnyx-Timestamp"
)

// DefaultTolerance mirrors the official telnyx-go and telnyx-node SDKs
// (5 minutes, symmetric). Telnyx docs do not mandate a value.
const DefaultTolerance = 5 * time.Minute

var (
	ErrMissingSignature = errors.New("telnyx: missing signature headers")
	ErrMalformedHeader  = errors.New("telnyx: malformed signature header")
	ErrStaleTimestamp   = errors.New("telnyx: timestamp outside tolerance")
	ErrBadSignature     = errors.New("telnyx: signature verification failed")
	ErrReplay           = errors.New("telnyx: replayed signed message")
)

// ParsePublicKey decodes the base64 raw 32-byte Ed25519 key shown in the
// Telnyx portal.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("telnyx: public key is not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("telnyx: public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// Verifier validates Telnyx Ed25519 signatures over "timestamp|message" with
// a timestamp window and a bounded replay cache.
type Verifier struct {
	key       ed25519.PublicKey
	tolerance time.Duration
	now       func() time.Time

	mu       sync.Mutex
	seen     map[[32]byte]time.Time
	maxCache int
}

// NewVerifier constructs a verifier. now must be a deterministic clock in tests.
func NewVerifier(key ed25519.PublicKey, tolerance time.Duration, now func() time.Time) (*Verifier, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, errors.New("telnyx: invalid public key")
	}
	if tolerance <= 0 {
		return nil, errors.New("telnyx: tolerance must be positive")
	}
	if now == nil {
		return nil, errors.New("telnyx: clock is required")
	}
	return &Verifier{key: key, tolerance: tolerance, now: now, seen: map[[32]byte]time.Time{}, maxCache: 100000}, nil
}

func (v *Verifier) check(h http.Header, message []byte) ([32]byte, error) {
	sigB64 := h.Get(HeaderSignature)
	tsStr := h.Get(HeaderTimestamp)
	if sigB64 == "" || tsStr == "" {
		return [32]byte{}, ErrMissingSignature
	}
	for _, r := range tsStr {
		if r < '0' || r > '9' {
			return [32]byte{}, fmt.Errorf("%w: timestamp %q", ErrMalformedHeader, tsStr)
		}
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: timestamp: %v", ErrMalformedHeader, err)
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return [32]byte{}, fmt.Errorf("%w: signature encoding", ErrMalformedHeader)
	}
	skew := v.now().Sub(time.Unix(ts, 0))
	if skew > v.tolerance || skew < -v.tolerance {
		return [32]byte{}, fmt.Errorf("%w: skew %v", ErrStaleTimestamp, skew)
	}
	signed := make([]byte, 0, len(tsStr)+1+len(message))
	signed = append(signed, tsStr...)
	signed = append(signed, '|')
	signed = append(signed, message...)
	if !ed25519.Verify(v.key, signed, sig) {
		return [32]byte{}, ErrBadSignature
	}
	return sha256.Sum256(signed), nil
}

// VerifyWebhook authenticates raw webhook body bytes exactly as received and
// rejects replays of the same signed message within the tolerance window.
func (v *Verifier) VerifyWebhook(h http.Header, rawBody []byte) error {
	digest, err := v.check(h, rawBody)
	if err != nil {
		return err
	}
	return v.remember(digest)
}

// VerifyMediaHandshake authenticates a media WebSocket upgrade. streamURL is
// the exact stream_url given to Telnyx, including query string. Reconnects
// carry a fresh timestamp, so replay protection applies per handshake.
func (v *Verifier) VerifyMediaHandshake(h http.Header, streamURL string) error {
	digest, err := v.check(h, []byte(streamURL))
	if err != nil {
		return err
	}
	return v.remember(digest)
}

func (v *Verifier) remember(d [32]byte) error {
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, exp := range v.seen {
		if now.After(exp) {
			delete(v.seen, k)
		}
	}
	if _, dup := v.seen[d]; dup {
		return ErrReplay
	}
	if len(v.seen) >= v.maxCache {
		return fmt.Errorf("telnyx: replay cache full")
	}
	v.seen[d] = now.Add(2 * v.tolerance)
	return nil
}

// Sign produces Telnyx-compatible headers. It exists for independent test
// emitters (fake Telnyx) and must never be used with production keys.
func Sign(priv ed25519.PrivateKey, ts time.Time, message []byte) http.Header {
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	signed := append([]byte(tsStr+"|"), message...)
	h := http.Header{}
	h.Set(HeaderTimestamp, tsStr)
	h.Set(HeaderSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signed)))
	return h
}
