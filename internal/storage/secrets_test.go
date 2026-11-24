package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func testSecret(id, owner, name, purpose string) Secret {
	return Secret{
		ID:          id,
		OwnerUserID: owner,
		Name:        name,
		Purpose:     purpose,
		Ciphertext:  []byte{0xde, 0xad, 0xbe, 0xef},
		Nonce:       make([]byte, 12),
		KeyVersion:  1,
		CreatedAt:   testTime,
	}
}

func TestSecretInsertGetIsOwnerScoped(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	if err := s.InsertSecret(ctx, testSecret("sec-a1", "alice", "openai", "ai_api_key")); err != nil {
		t.Fatalf("InsertSecret: %v", err)
	}
	got, err := s.GetSecret(ctx, "alice", "sec-a1")
	if err != nil {
		t.Fatalf("GetSecret owner: %v", err)
	}
	if got.Name != "openai" || got.KeyVersion != 1 || string(got.Ciphertext) != string([]byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if _, err := s.GetSecret(ctx, "bob", "sec-a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner must be ErrNotFound, got %v", err)
	}
}

func TestSecretDuplicateNameConflicts(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()
	if err := s.InsertSecret(ctx, testSecret("sec-1", "alice", "openai", "ai_api_key")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSecret(ctx, testSecret("sec-2", "alice", "openai", "ai_api_key")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name must conflict, got %v", err)
	}
}

func TestSecretListOmitsCiphertextAndIsOwnerScoped(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()
	if err := s.InsertSecret(ctx, testSecret("sec-a1", "alice", "openai", "ai_api_key")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSecrets(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range list {
		if m.ID == "sec-a1" {
			found = true
			if m.Name != "openai" || m.Purpose != "ai_api_key" {
				t.Fatalf("meta %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("alice's secret missing from her list")
	}
	bob, err := s.ListSecrets(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range bob {
		if m.ID == "sec-a1" {
			t.Fatal("bob listed alice's secret")
		}
	}
}

func TestCountSecretsCountsAllOwners(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()
	before, err := s.CountSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSecret(ctx, testSecret("sec-a1", "alice", "openai", "ai_api_key")); err != nil {
		t.Fatal(err)
	}
	after, err := s.CountSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("count %d -> %d, want +1", before, after)
	}
}
