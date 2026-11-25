package telephonyprofile

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// TestResolveDecryptsRealSealedKeys runs the service over the real SQLite
// store and the real AEAD box. It proves the stored ciphertext does not contain
// the key, and that Resolve returns the exact plaintext for the owner and for a
// grantee.
func TestResolveDecryptsRealSealedKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "roundtrip.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := "2026-11-25T10:00:00.000Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, username, role, password_hash, active, created_at, updated_at) VALUES
		('alice', 'alice', 'admin', 'hash', 1, ?, ?),
		('bob', 'bob', 'user', 'hash', 1, ?, ?)`, ts, ts, ts, ts); err != nil {
		t.Fatal(err)
	}

	now := func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) }
	key := bytes.Repeat([]byte{7}, secrets.KeySize)
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(ring)
	if err != nil {
		t.Fatal(err)
	}
	secIDs, profIDs := 0, 0
	sec, err := secrets.NewService(box, store, now, func() string { secIDs++; return fmt.Sprintf("sec-%d", secIDs) })
	if err != nil {
		t.Fatal(err)
	}
	const plainKey = "KEY-real-roundtrip-0123456789"
	const plainHook = fakePubKeyB
	noNetwork := func(string) (Provider, error) { return nil, errors.New("no provider in this test") }
	svc, err := New(store, sec, noNetwork, now, func() string { profIDs++; return fmt.Sprintf("tp-%d", profIDs) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	alice := Actor{UserID: "alice", Admin: true}
	bob := Actor{UserID: "bob"}
	v, err := svc.Create(ctx, alice, Input{Name: "main", CallerNumber: testNumber, ConnectionID: testConnID,
		APIKey: plainKey, WebhookPublicKey: plainHook})
	if err != nil {
		t.Fatal(err)
	}

	// The stored ciphertext must not contain either plaintext.
	var ciphertext []byte
	if err := db.QueryRowContext(ctx, `SELECT s.ciphertext FROM secrets s
		JOIN telephony_profiles p ON p.api_secret_id = s.id WHERE p.id = ?`, v.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(plainKey)) || len(ciphertext) == 0 {
		t.Fatal("stored API key ciphertext is not sealed")
	}

	if _, err := svc.Grant(ctx, alice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	for name, actor := range map[string]Actor{"owner": alice, "grantee": bob} {
		got, err := svc.Resolve(ctx, actor, v.ID)
		if err != nil {
			t.Fatalf("%s resolve: %v", name, err)
		}
		if got.APIKey != plainKey || got.WebhookPublicKey != plainHook {
			t.Fatalf("%s resolved keys do not match the stored plaintext", name)
		}
	}
}
