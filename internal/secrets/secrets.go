// Package secrets encrypts user telco and AI API keys at rest with AES-256-GCM
// under an app master key that lives outside SQL. Every ciphertext is bound to
// its row through additional authenticated data, so it cannot be moved between
// rows, owners or purposes. Plaintext is never returned to an HTTP caller.
package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	// KeySize is the required master key length in bytes (AES-256).
	KeySize = 32
	// NonceSize is the AES-GCM standard nonce length in bytes.
	NonceSize = 12
	// MaxPlaintext bounds one secret value.
	MaxPlaintext = 4096

	// EnvKeyFile names a file holding the base64-encoded master key. Preferred
	// over EnvKey because it can carry strict file permissions.
	EnvKeyFile = "CALLHARBOR_MASTER_KEY_FILE"
	// EnvKey holds the base64-encoded master key for a fallback deployment.
	EnvKey = "CALLHARBOR_MASTER_KEY"
)

var (
	// ErrNoMasterKey reports that the master key is unavailable. Writes and
	// reads fail closed.
	ErrNoMasterKey = errors.New("secrets: master key is not configured")
	// ErrInvalidMasterKey reports a master key of the wrong length or encoding.
	// The message never contains key material.
	ErrInvalidMasterKey = errors.New("secrets: master key must be 32 bytes")
	// ErrNotFound reports a missing secret, or one owned by another user or
	// bound to another purpose. The two cases are indistinguishable.
	ErrNotFound = errors.New("secrets: not found")
	// ErrInvalidSecret reports an invalid name, purpose or plaintext.
	ErrInvalidSecret = errors.New("secrets: invalid secret")
	// ErrDecrypt reports a ciphertext that failed authentication. It means
	// tampering, a wrong key, or a row moved between owners or purposes.
	ErrDecrypt = errors.New("secrets: decryption failed")
	// ErrUnknownKeyVersion reports a row encrypted under a key version that
	// is not in the keyring.
	ErrUnknownKeyVersion = errors.New("secrets: unknown key version")
)

// Purposes match the CHECK constraint on the secrets table.
const (
	PurposeTelnyxAPIKey     = "telnyx_api_key" // #nosec G101 -- purpose name, not a credential
	PurposeTelnyxWebhookKey = "telnyx_webhook_public_key"
	PurposeAIAPIKey         = "ai_api_key"
)

var validPurposes = map[string]bool{
	PurposeTelnyxAPIKey:     true,
	PurposeTelnyxWebhookKey: true,
	PurposeAIAPIKey:         true,
}

// Keyring maps a key version to its master key. The current version is used
// for new writes; older versions remain to decrypt rows until they are rotated.
type Keyring struct {
	current int
	keys    map[int][]byte
}

// NewKeyring builds a keyring whose writes use current. It fails closed if the
// current version is absent or any key has the wrong length.
func NewKeyring(current int, keys map[int][]byte) (*Keyring, error) {
	if current < 1 {
		return nil, fmt.Errorf("%w: key version must be at least 1", ErrInvalidMasterKey)
	}
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("%w: current key version %d is missing", ErrNoMasterKey, current)
	}
	copied := make(map[int][]byte, len(keys))
	for v, k := range keys {
		if v < 1 {
			return nil, fmt.Errorf("%w: key version must be at least 1", ErrInvalidMasterKey)
		}
		if len(k) != KeySize {
			return nil, ErrInvalidMasterKey
		}
		copied[v] = append([]byte(nil), k...)
	}
	return &Keyring{current: current, keys: copied}, nil
}

// Current returns the version used for new writes.
func (k *Keyring) Current() int { return k.current }

// LoadMasterKey reads the base64-encoded 32-byte master key from
// CALLHARBOR_MASTER_KEY_FILE, or from CALLHARBOR_MASTER_KEY when no file is
// set. It returns ErrNoMasterKey when neither is set. Key material never
// appears in any error.
func LoadMasterKey(getenv func(string) string) ([]byte, error) {
	if path := strings.TrimSpace(getenv(EnvKeyFile)); path != "" {
		raw, err := readKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("secrets: read %s: %w", EnvKeyFile, sanitizeIO(err))
		}
		return decodeKey(bytes.TrimSpace(raw))
	}
	if v := strings.TrimSpace(getenv(EnvKey)); v != "" {
		return decodeKey([]byte(v))
	}
	return nil, ErrNoMasterKey
}

// readKeyFile opens the key file through os.Root so the read is confined to the
// file's own directory. The operator names the file; this only prevents the
// path from escaping its parent via symlinks or "..".
func readKeyFile(path string) ([]byte, error) {
	dir, name := filepath.Split(filepath.Clean(path))
	if dir == "" {
		dir = "."
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 4096))
}

func decodeKey(text []byte) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(string(text))
	if err != nil || len(key) != KeySize {
		return nil, ErrInvalidMasterKey
	}
	return key, nil
}

func sanitizeIO(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return errors.New("io error")
}

// Box seals and opens secret values for rows in the secrets table.
type Box struct {
	ring *Keyring
	rand io.Reader
}

// NewBox builds a Box over a keyring. A nil keyring is an error: callers must
// not construct a Box without a master key.
func NewBox(ring *Keyring) (*Box, error) {
	if ring == nil {
		return nil, ErrNoMasterKey
	}
	return &Box{ring: ring, rand: rand.Reader}, nil
}

// Sealed is the encrypted form of one secret, ready for storage.
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
}

// aad binds a ciphertext to its row identity. Changing any field makes
// authentication fail.
func aad(id, ownerUserID, purpose string) []byte {
	return []byte(id + "\x00" + ownerUserID + "\x00" + purpose)
}

// Seal encrypts plaintext for the row (id, ownerUserID, purpose) under the
// current key version with a fresh random nonce.
func (b *Box) Seal(id, ownerUserID, purpose string, plaintext []byte) (Sealed, error) {
	if err := validate(id, ownerUserID, purpose, plaintext); err != nil {
		return Sealed{}, err
	}
	key := b.ring.keys[b.ring.current]
	gcm, err := newGCM(key)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(b.rand, nonce); err != nil {
		return Sealed{}, fmt.Errorf("secrets: nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad(id, ownerUserID, purpose))
	return Sealed{Ciphertext: ct, Nonce: nonce, KeyVersion: b.ring.current}, nil
}

// Open decrypts a stored row. It fails with ErrDecrypt if the ciphertext,
// nonce, AAD or key do not match what was sealed.
func (b *Box) Open(id, ownerUserID, purpose string, s Sealed) ([]byte, error) {
	if err := validate(id, ownerUserID, purpose, []byte{0}); err != nil {
		return nil, err
	}
	key, ok := b.ring.keys[s.KeyVersion]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, s.KeyVersion)
	}
	if len(s.Nonce) != NonceSize {
		return nil, ErrDecrypt
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, s.Nonce, s.Ciphertext, aad(id, ownerUserID, purpose))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher: %w", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, NonceSize)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	return gcm, nil
}

func validate(id, ownerUserID, purpose string, plaintext []byte) error {
	if id == "" || ownerUserID == "" || !validPurposes[purpose] {
		return ErrInvalidSecret
	}
	if len(plaintext) == 0 || len(plaintext) > MaxPlaintext {
		return ErrInvalidSecret
	}
	return nil
}

// Store is the owner-scoped secret repository the Service needs.
type Store interface {
	InsertSecret(ctx context.Context, s storage.Secret) error
	GetSecret(ctx context.Context, ownerUserID, id string) (storage.Secret, error)
}

// Service stores and resolves owner-scoped secrets.
type Service struct {
	box   *Box
	store Store
	now   func() time.Time
	newID func() string
}

// NewService builds the secret service. box must be non-nil.
func NewService(box *Box, store Store, now func() time.Time, newID func() string) (*Service, error) {
	if box == nil || store == nil || now == nil || newID == nil {
		return nil, ErrNoMasterKey
	}
	return &Service{box: box, store: store, now: now, newID: newID}, nil
}

// NewUnkeyedService builds a service with no master key. Every write and
// resolve returns ErrNoMasterKey, so the API works without leaking or storing
// credentials in clear.
func NewUnkeyedService(store Store, now func() time.Time, newID func() string) *Service {
	return &Service{store: store, now: now, newID: newID}
}

// Put encrypts plaintext and stores it for owner, returning the new secret id.
// The plaintext is never persisted or returned.
func (s *Service) Put(ctx context.Context, owner, name, purpose string, plaintext []byte) (string, error) {
	if owner == "" || name == "" || len(name) > 128 {
		return "", ErrInvalidSecret
	}
	if s.box == nil {
		return "", ErrNoMasterKey
	}
	id := s.newID()
	sealed, err := s.box.Seal(id, owner, purpose, plaintext)
	if err != nil {
		return "", err
	}
	row := storage.Secret{
		ID:          id,
		OwnerUserID: owner,
		Name:        name,
		Purpose:     purpose,
		Ciphertext:  sealed.Ciphertext,
		Nonce:       sealed.Nonce,
		KeyVersion:  sealed.KeyVersion,
		CreatedAt:   s.now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if err := s.store.InsertSecret(ctx, row); err != nil {
		return "", err
	}
	return id, nil
}

// Resolve returns the plaintext of secret id for owner, provided it has the
// requested purpose. A wrong owner or wrong purpose returns ErrNotFound; other
// storage failures are returned as-is.
func (s *Service) Resolve(ctx context.Context, owner, id, purpose string) ([]byte, error) {
	if s.box == nil {
		return nil, ErrNoMasterKey
	}
	row, err := s.store.GetSecret(ctx, owner, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.Purpose != purpose {
		return nil, ErrNotFound
	}
	pt, err := s.box.Open(row.ID, row.OwnerUserID, row.Purpose, Sealed{
		Ciphertext: row.Ciphertext,
		Nonce:      row.Nonce,
		KeyVersion: row.KeyVersion,
	})
	if err != nil {
		return nil, err
	}
	return pt, nil
}
