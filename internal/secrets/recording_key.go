package secrets

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
)

// RecordingSaltSize is the per-recording random salt length in bytes. It is
// stored in the plaintext file header, so a recording can be re-derived after
// a restart without any SQL-side key material.
const RecordingSaltSize = 32

// recordingKeyInfo domain-separates recording keys from every other use of the
// master key. Changing it makes every existing recording undecryptable, so it
// is part of the on-disk format and is versioned by the file header.
const recordingKeyInfo = "callharbor recording file key v1"

// DeriveRecordingKey returns the AES-256 key for one recording file. The key is
// HKDF-SHA256 over the master key at keyVersion, with the recording's random
// salt. Keys are never stored, and each recording gets an independent key, so
// no single file key protects another recording.
//
// It fails with ErrUnknownKeyVersion when the keyring lacks keyVersion, and
// with ErrInvalidSecret when the salt has the wrong length. Errors never carry
// key material.
func (k *Keyring) DeriveRecordingKey(keyVersion int, salt []byte) ([]byte, error) {
	master, ok := k.keys[keyVersion]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, keyVersion)
	}
	if len(salt) != RecordingSaltSize {
		return nil, ErrInvalidSecret
	}
	key, err := hkdf.Key(sha256.New, master, salt, recordingKeyInfo, KeySize)
	if err != nil {
		return nil, fmt.Errorf("secrets: derive recording key: %w", err)
	}
	return key, nil
}
