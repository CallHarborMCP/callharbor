package app

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// seedSecretRow stores one real encrypted secret for a real owner, so the
// startup guard sees a non-empty secrets table.
func seedSecretRow(t *testing.T, store *storage.Store) {
	t.Helper()
	ctx := context.Background()
	ts := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	idSvc, err := identity.NewService(store, func() time.Time { return ts })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idSvc.Setup(ctx, "root", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	root, err := idSvc.Login(ctx, "root", "correct horse battery", "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	p, err := idSvc.Authenticate(ctx, root.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertSecret(ctx, storage.Secret{
		ID: "sec_seed", OwnerUserID: p.User.ID, Name: "seed", Purpose: secrets.PurposeAIAPIKey,
		Ciphertext: []byte{1, 2, 3}, Nonce: make([]byte, secrets.NonceSize), KeyVersion: 1,
		CreatedAt: ts.Format("2006-01-02T15:04:05.000Z"),
	}); err != nil {
		t.Fatal(err)
	}
}

// ownerID returns the user id that seedSecretRow created the secret for.
func ownerID(t *testing.T, store *storage.Store) string {
	t.Helper()
	u, err := store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func validKeyB64() string {
	k := make([]byte, secrets.KeySize)
	for i := range k {
		k[i] = byte(i + 9)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// TestOpenSecretsFailsClosedWhenRowsExistWithoutKey is the startup guard: with
// stored secrets present and no master key, the server must refuse to start
// rather than run with credentials it cannot read or rotate.
func TestOpenSecretsFailsClosedWhenRowsExistWithoutKey(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSecretRow(t, store)
	_, err := openSecrets(ctx, store, envOf(nil))
	if err == nil {
		t.Fatal("startup must fail when secrets exist and no master key is set")
	}
	if !strings.Contains(err.Error(), secrets.EnvKeyFile) || !strings.Contains(err.Error(), secrets.EnvKey) {
		t.Fatalf("error must name the key settings, got %v", err)
	}
}

// TestOpenSecretsStartsWithoutKeyWhenEmpty keeps a fresh install usable: with
// no secrets and no key the API starts, and writes fail closed.
func TestOpenSecretsStartsWithoutKeyWhenEmpty(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	svc, err := openSecrets(ctx, store, envOf(nil))
	if err != nil {
		t.Fatalf("empty install must start: %v", err)
	}
	if _, err := svc.Put(ctx, "usr_x", "k", secrets.PurposeAIAPIKey, []byte("v")); !errors.Is(err, secrets.ErrNoMasterKey) {
		t.Fatalf("write without key must fail closed, got %v", err)
	}
}

func TestOpenSecretsWithKeyRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seedSecretRow(t, store)
	owner := ownerID(t, store)
	svc, err := openSecrets(ctx, store, envOf(map[string]string{secrets.EnvKey: validKeyB64()}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.Put(ctx, owner, "k", secrets.PurposeAIAPIKey, []byte("sk-app-wiring"))
	if err != nil {
		t.Fatalf("Put with key: %v", err)
	}
	got, err := svc.Resolve(ctx, owner, id, secrets.PurposeAIAPIKey)
	if err != nil || string(got) != "sk-app-wiring" {
		t.Fatalf("Resolve: %q %v", got, err)
	}
}

func TestOpenSecretsRejectsMalformedKey(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := openSecrets(ctx, store, envOf(map[string]string{secrets.EnvKey: short})); !errors.Is(err, secrets.ErrInvalidMasterKey) {
		t.Fatalf("short key must be rejected, got %v", err)
	}
}

func TestOpenSecretsKeyFromFile(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := filepath.Join(t.TempDir(), "master.key")
	if err := writeFile(path, validKeyB64()); err != nil {
		t.Fatal(err)
	}
	if _, err := openSecrets(ctx, store, envOf(map[string]string{secrets.EnvKeyFile: path})); err != nil {
		t.Fatalf("file key: %v", err)
	}
}

func TestNewSecretIDIsUniqueAndPrefixed(t *testing.T) {
	a, b := newSecretID(), newSecretID()
	if a == b || !strings.HasPrefix(a, "sec_") {
		t.Fatalf("ids %q %q", a, b)
	}
}
