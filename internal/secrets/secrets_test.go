package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

func testKey(b byte) []byte {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = b + byte(i)
	}
	return k
}

func testBox(t *testing.T, keys map[int][]byte, current int) *Box {
	t.Helper()
	ring, err := NewKeyring(current, keys)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	box, err := NewBox(ring)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

func TestSealOpenRoundTrip(t *testing.T) {
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	pt := []byte("sk-live-abc123")

	sealed, err := box.Seal("sec_1", "alice", PurposeAIAPIKey, pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, pt) {
		t.Fatal("ciphertext contains plaintext")
	}
	if len(sealed.Nonce) != NonceSize || sealed.KeyVersion != 1 {
		t.Fatalf("nonce %d bytes, version %d", len(sealed.Nonce), sealed.KeyVersion)
	}
	got, err := box.Open("sec_1", "alice", PurposeAIAPIKey, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch")
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	a, err := box.Seal("sec_1", "alice", PurposeAIAPIKey, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := box.Seal("sec_1", "alice", PurposeAIAPIKey, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("nonce or ciphertext reused across writes")
	}
}

func TestOpenRejectsTamperAndBindingChanges(t *testing.T) {
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	sealed, err := box.Seal("sec_1", "alice", PurposeAIAPIKey, []byte("secret-value"))
	if err != nil {
		t.Fatal(err)
	}

	flipCT := sealed
	flipCT.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	flipCT.Ciphertext[0] ^= 0x01

	flipNonce := sealed
	flipNonce.Nonce = append([]byte(nil), sealed.Nonce...)
	flipNonce.Nonce[0] ^= 0x01

	cases := map[string]struct {
		id, owner, purpose string
		s                  Sealed
	}{
		"ciphertext tampered": {"sec_1", "alice", PurposeAIAPIKey, flipCT},
		"nonce tampered":      {"sec_1", "alice", PurposeAIAPIKey, flipNonce},
		"row id swapped":      {"sec_2", "alice", PurposeAIAPIKey, sealed},
		"owner swapped":       {"sec_1", "bob", PurposeAIAPIKey, sealed},
		"purpose swapped":     {"sec_1", "alice", PurposeTelnyxAPIKey, sealed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := box.Open(c.id, c.owner, c.purpose, c.s)
			if !errors.Is(err, ErrDecrypt) {
				t.Fatalf("want ErrDecrypt, got %v", err)
			}
		})
	}
}

func TestOpenRejectsWrongKeyVersionAndShortNonce(t *testing.T) {
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	sealed, err := box.Seal("sec_1", "alice", PurposeAIAPIKey, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	unknown := sealed
	unknown.KeyVersion = 9
	if _, err := box.Open("sec_1", "alice", PurposeAIAPIKey, unknown); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("unknown version: %v", err)
	}
	short := sealed
	short.Nonce = sealed.Nonce[:4]
	if _, err := box.Open("sec_1", "alice", PurposeAIAPIKey, short); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("short nonce: %v", err)
	}
}

func TestRotationDecryptsOldVersionAndWritesNew(t *testing.T) {
	v1 := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	old, err := v1.Seal("sec_1", "alice", PurposeAIAPIKey, []byte("rotate-me"))
	if err != nil {
		t.Fatal(err)
	}
	rotated := testBox(t, map[int][]byte{1: testKey(1), 2: testKey(7)}, 2)
	pt, err := rotated.Open("sec_1", "alice", PurposeAIAPIKey, old)
	if err != nil || string(pt) != "rotate-me" {
		t.Fatalf("old version must still open: %v", err)
	}
	fresh, err := rotated.Seal("sec_1", "alice", PurposeAIAPIKey, []byte("rotate-me"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.KeyVersion != 2 {
		t.Fatalf("new write uses version %d, want 2", fresh.KeyVersion)
	}
}

func TestKeyringFailsClosed(t *testing.T) {
	cases := map[string]struct {
		current int
		keys    map[int][]byte
	}{
		"current missing":   {2, map[int][]byte{1: testKey(1)}},
		"zero version":      {0, map[int][]byte{0: testKey(1)}},
		"short key":         {1, map[int][]byte{1: testKey(1)[:16]}},
		"long key":          {1, map[int][]byte{1: append(testKey(1), 0)}},
		"non-positive keys": {1, map[int][]byte{1: testKey(1), -1: testKey(2)}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewKeyring(c.current, c.keys); err == nil {
				t.Fatal("NewKeyring accepted invalid input")
			}
		})
	}
	if _, err := NewBox(nil); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("nil keyring must fail closed: %v", err)
	}
}

func TestSealRejectsInvalidInput(t *testing.T) {
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	cases := map[string]func() error{
		"empty id":        func() error { _, err := box.Seal("", "alice", PurposeAIAPIKey, []byte("x")); return err },
		"empty owner":     func() error { _, err := box.Seal("s", "", PurposeAIAPIKey, []byte("x")); return err },
		"unknown purpose": func() error { _, err := box.Seal("s", "alice", "bogus", []byte("x")); return err },
		"empty value":     func() error { _, err := box.Seal("s", "alice", PurposeAIAPIKey, nil); return err },
		"oversized value": func() error {
			_, err := box.Seal("s", "alice", PurposeAIAPIKey, make([]byte, MaxPlaintext+1))
			return err
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrInvalidSecret) {
				t.Fatalf("want ErrInvalidSecret, got %v", err)
			}
		})
	}
}

func TestLoadMasterKeyFromFile(t *testing.T) {
	key := testKey(3)
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMasterKey(env(map[string]string{EnvKeyFile: path}))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("file key: %v", err)
	}
}

func TestLoadMasterKeyFromEnvFallback(t *testing.T) {
	key := testKey(4)
	got, err := LoadMasterKey(env(map[string]string{EnvKey: base64.StdEncoding.EncodeToString(key)}))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("env key: %v", err)
	}
}

func TestLoadMasterKeyFileTakesPrecedence(t *testing.T) {
	fileKey, envKey := testKey(5), testKey(6)
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(fileKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMasterKey(env(map[string]string{
		EnvKeyFile: path,
		EnvKey:     base64.StdEncoding.EncodeToString(envKey),
	}))
	if err != nil || !bytes.Equal(got, fileKey) {
		t.Fatalf("file must win over env: %v", err)
	}
}

func TestLoadMasterKeyFailsClosed(t *testing.T) {
	if _, err := LoadMasterKey(env(nil)); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("unset key: %v", err)
	}
	short := base64.StdEncoding.EncodeToString(testKey(1)[:16])
	if _, err := LoadMasterKey(env(map[string]string{EnvKey: short})); !errors.Is(err, ErrInvalidMasterKey) {
		t.Fatalf("short key: %v", err)
	}
	if _, err := LoadMasterKey(env(map[string]string{EnvKey: "not base64 !!"})); !errors.Is(err, ErrInvalidMasterKey) {
		t.Fatalf("bad encoding: %v", err)
	}
	if _, err := LoadMasterKey(env(map[string]string{EnvKeyFile: filepath.Join(t.TempDir(), "missing")})); err == nil {
		t.Fatal("missing key file must fail")
	}
}

func TestKeyNeverAppearsInErrors(t *testing.T) {
	key := testKey(8)
	enc := base64.StdEncoding.EncodeToString(key)
	cases := []map[string]string{
		{EnvKey: enc[:20] + "!!"},
		{EnvKey: enc[:len(enc)-4]},
		{EnvKeyFile: filepath.Join(t.TempDir(), "no-such-file-"+enc[:8])},
	}
	for _, c := range cases {
		_, err := LoadMasterKey(env(c))
		if err == nil {
			t.Fatal("expected failure")
		}
		if strings.Contains(err.Error(), enc) || strings.Contains(err.Error(), enc[:20]) {
			t.Fatalf("error leaks key material: %v", err)
		}
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// memStore is an in-memory owner-scoped secret repository for service tests.
type memStore struct {
	mu   sync.Mutex
	rows map[string]storage.Secret
	fail error
}

func newMemStore() *memStore { return &memStore{rows: map[string]storage.Secret{}} }

func (m *memStore) InsertSecret(_ context.Context, s storage.Secret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.rows[s.ID] = s
	return nil
}

func (m *memStore) GetSecret(_ context.Context, owner, id string) (storage.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return storage.Secret{}, m.fail
	}
	s, ok := m.rows[id]
	if !ok || s.OwnerUserID != owner {
		return storage.Secret{}, storage.ErrNotFound
	}
	return s, nil
}

func newTestService(t *testing.T, store Store) *Service {
	t.Helper()
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	svc, err := NewService(box, store, func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) }, func() string { return "sec_test" })
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestServicePutResolveIsOwnerAndPurposeScoped(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	svc := newTestService(t, ms)

	id, err := svc.Put(ctx, "alice", "openai", PurposeAIAPIKey, []byte("sk-alice"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := svc.Resolve(ctx, "alice", id, PurposeAIAPIKey)
	if err != nil || string(got) != "sk-alice" {
		t.Fatalf("owner resolve: %q %v", got, err)
	}
	if _, err := svc.Resolve(ctx, "bob", id, PurposeAIAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong owner must be NotFound, got %v", err)
	}
	if _, err := svc.Resolve(ctx, "alice", id, PurposeTelnyxAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong purpose must be NotFound, got %v", err)
	}
	if _, err := svc.Resolve(ctx, "alice", "missing", PurposeAIAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id must be NotFound, got %v", err)
	}
	stored := ms.rows[id]
	if bytes.Contains(stored.Ciphertext, []byte("sk-alice")) {
		t.Fatal("stored row contains plaintext")
	}
}

func TestServiceResolveReturnsStoreErrorsNotNotFound(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	svc := newTestService(t, ms)
	boom := errors.New("disk I/O failed")
	ms.fail = boom
	if _, err := svc.Resolve(ctx, "alice", "sec", PurposeAIAPIKey); !errors.Is(err, boom) {
		t.Fatalf("store failure must surface, got %v", err)
	}
	if _, err := svc.Put(ctx, "alice", "k", PurposeAIAPIKey, []byte("v")); !errors.Is(err, boom) {
		t.Fatalf("insert failure must surface, got %v", err)
	}
}

func TestServiceDetectsRowMovedBetweenOwners(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	svc := newTestService(t, ms)
	id, err := svc.Put(ctx, "alice", "k", PurposeAIAPIKey, []byte("sk-alice"))
	if err != nil {
		t.Fatal(err)
	}
	// A database-level swap that re-points the row at another owner must fail
	// authentication, not return Alice's plaintext to Bob.
	row := ms.rows[id]
	row.OwnerUserID = "bob"
	ms.rows[id] = row
	if _, err := svc.Resolve(ctx, "bob", id, PurposeAIAPIKey); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("moved row must fail authentication, got %v", err)
	}
}

func TestServiceNewServiceRequiresDependencies(t *testing.T) {
	box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
	now := time.Now
	id := func() string { return "x" }
	if _, err := NewService(nil, newMemStore(), now, id); err == nil {
		t.Fatal("nil box accepted")
	}
	if _, err := NewService(box, nil, now, id); err == nil {
		t.Fatal("nil store accepted")
	}
}
