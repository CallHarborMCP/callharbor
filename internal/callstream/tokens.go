// Package callstream issues the unguessable path token that the media stream
// URL of each call carries. The token is derived from the master key and the
// call id, so it needs no stored row and survives a restart: a call dialed
// before a restart can still connect its media afterwards.
//
// Tokens are secrets. They are never logged, and checks use a constant-time
// comparison.
package callstream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// minKeyBytes is the smallest master key accepted. The secrets package
// already requires a full-size key, so this is a guard, not the real limit.
const minKeyBytes = 16

// tokenLabel separates this derivation from any other use of the master key.
const tokenLabel = "callharbor-stream-token-v1"

// Tokens derives and checks per-call stream tokens.
type Tokens struct {
	key []byte
}

// NewTokens derives the token key from the master key.
func NewTokens(masterKey []byte) (*Tokens, error) {
	if len(masterKey) < minKeyBytes {
		return nil, errors.New("callstream: master key too short")
	}
	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte(tokenLabel))
	return &Tokens{key: mac.Sum(nil)}, nil
}

// Token returns the stream token for one call.
func (t *Tokens) Token(callID string) (string, error) {
	if callID == "" {
		return "", errors.New("callstream: call id required")
	}
	return t.mac(callID), nil
}

// Valid reports whether token is the stream token for callID. The comparison
// takes the same time whatever the input, so timing cannot reveal a prefix.
func (t *Tokens) Valid(callID, token string) bool {
	if callID == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(t.mac(callID)), []byte(token))
}

func (t *Tokens) mac(callID string) string {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(callID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
