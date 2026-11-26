package telephonyprofile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// resolveHarness creates alice's profile with an API key and a webhook key,
// then returns the harness and the profile id.
func resolveHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.goodFake()
	in := validInput()
	in.WebhookPublicKey = fakePubKeyB
	v := h.create(adminAlice, in)
	return h, v.ID
}

func TestResolveOwnerGetsDecryptedKeys(t *testing.T) {
	h, id := resolveHarness(t)
	got, err := h.svc.Resolve(context.Background(), adminAlice, id)
	if err != nil {
		t.Fatalf("owner resolve: %v", err)
	}
	if got.APIKey != goodKey || got.WebhookPublicKey != fakePubKeyB {
		t.Fatalf("owner keys = %q / %q, want the stored plaintexts", got.APIKey, got.WebhookPublicKey)
	}
	if got.OwnerUserID != "alice" || got.Access != "owner" || got.CallerNumber != testNumber || got.ConnectionID != testConnID {
		t.Fatalf("owner resolve fields = %+v", got)
	}
}

func TestResolveOmitsWebhookKeyWhenNotSet(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	got, err := h.svc.Resolve(context.Background(), adminAlice, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != goodKey || got.WebhookPublicKey != "" {
		t.Fatalf("resolve without webhook = %+v", got)
	}
}

func TestResolveGranteeUsesOwnersKey(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, id, "bob"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The memory secret service rejects any owner id but the row's owner, so
	// success here proves the key was read with alice's id, not bob's.
	got, err := h.svc.Resolve(ctx, userBob, id)
	if err != nil {
		t.Fatalf("grantee resolve: %v", err)
	}
	if got.APIKey != goodKey || got.WebhookPublicKey != fakePubKeyB || got.Access != "granted" {
		t.Fatalf("grantee resolve = %+v", got)
	}
	if got.OwnerUserID != "alice" {
		t.Fatalf("grantee resolve owner = %q, want alice", got.OwnerUserID)
	}
}

func TestResolveRevokedGranteeAndStrangerAreNotFound(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Resolve(ctx, userBob, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked grantee resolve = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Resolve(ctx, userCarol, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger resolve = %v, want ErrNotFound", err)
	}
}

func TestResolveNeverGrantedInactiveGranteeIsNotFound(t *testing.T) {
	h, id := resolveHarness(t)
	// dave is inactive; a grant to him cannot be created through the service,
	// so insert the row directly to prove resolve still refuses him.
	h.store.grants = append(h.store.grants, storageGrant(id, "dave"))
	if _, err := h.svc.Resolve(context.Background(), Actor{UserID: "dave"}, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive grantee resolve = %v, want ErrNotFound", err)
	}
}

func TestResolveFollowsOwnerAdminAndActiveState(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, id, "bob"); err != nil {
		t.Fatal(err)
	}
	usable := func(want bool, why string) {
		t.Helper()
		_, err := h.svc.Resolve(ctx, userBob, id)
		if (err == nil) != want {
			t.Fatalf("%s: bob resolve err = %v, want usable=%v", why, err, want)
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: bob resolve err = %v, want ErrNotFound", why, err)
		}
	}
	usable(true, "owner active admin")

	alice := h.store.users["alice"]
	alice.Role = "user"
	h.store.users["alice"] = alice
	usable(false, "owner demoted to user")

	alice.Role = "admin"
	h.store.users["alice"] = alice
	usable(true, "owner re-promoted")

	alice.Active = false
	h.store.users["alice"] = alice
	usable(false, "owner deactivated")
	if _, err := h.svc.Resolve(ctx, adminAlice, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deactivated owner resolves own profile: %v", err)
	}

	alice.Active = true
	h.store.users["alice"] = alice
	usable(true, "owner reactivated")

	bob := h.store.users["bob"]
	bob.Active = false
	h.store.users["bob"] = bob
	usable(false, "grantee deactivated")
}

func TestResolveRefusesInvalidActorAndID(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Resolve(ctx, Actor{}, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("empty actor = %v, want ErrForbidden", err)
	}
	if _, err := h.svc.Resolve(ctx, adminAlice, "bad id!"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Resolve(ctx, adminAlice, "tp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id = %v, want ErrNotFound", err)
	}
}

func TestResolveWrongPurposeOrForeignSecretFailsClosed(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	rec := h.store.profiles[id]

	// The API key pointer is redirected at the webhook key secret. The purpose
	// does not match, so the secret service refuses and resolve must fail
	// closed with no key returned.
	wrong := rec
	wrong.APISecretID = rec.WebhookPublicKeySecretID
	h.store.profiles[id] = wrong
	got, err := h.svc.Resolve(ctx, adminAlice, id)
	if !errors.Is(err, ErrSecretsDown) || got.APIKey != "" {
		t.Fatalf("wrong-purpose secret = %+v, %v; want ErrSecretsDown and no key", got, err)
	}

	// The API key pointer is redirected at another user's secret. Carol owns
	// it, so alice's read must not reach it.
	h.store.secrets["carol-key"] = storageSecret("carol-key", "carol", "telnyx_api_key")
	h.store.plain["carol-key"] = "KEY-carol"
	foreign := rec
	foreign.APISecretID = "carol-key"
	h.store.profiles[id] = foreign
	got, err = h.svc.Resolve(ctx, adminAlice, id)
	if !errors.Is(err, ErrSecretsDown) || got.APIKey != "" || got.APIKey == "KEY-carol" {
		t.Fatalf("foreign secret = %+v, %v; want ErrSecretsDown and no key", got, err)
	}
	h.store.profiles[id] = rec
}

func TestResolveSecretsUnavailableFailsClosed(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	h.sec.noKey = true
	if _, err := h.svc.Resolve(ctx, adminAlice, id); !errors.Is(err, ErrSecretsDown) {
		t.Fatalf("no master key = %v, want ErrSecretsDown", err)
	}
	h.sec.noKey = false
	h.sec.failResolv = true
	got, err := h.svc.Resolve(ctx, adminAlice, id)
	if !errors.Is(err, ErrSecretsDown) || got.APIKey != "" || got.WebhookPublicKey != "" {
		t.Fatalf("decrypt failure = %+v, %v; want ErrSecretsDown and no keys", got, err)
	}
}

func TestResolveWebhookFailureReturnsNoKey(t *testing.T) {
	h, id := resolveHarness(t)
	rec := h.store.profiles[id]
	bad := rec
	bad.WebhookPublicKeySecretID = "missing-webhook"
	h.store.profiles[id] = bad
	got, err := h.svc.Resolve(context.Background(), adminAlice, id)
	if !errors.Is(err, ErrSecretsDown) || got.APIKey != "" {
		t.Fatalf("webhook failure = %+v, %v; want ErrSecretsDown and no API key either", got, err)
	}
}

func TestResolvedRedactsKeysInFormatting(t *testing.T) {
	h, id := resolveHarness(t)
	got, err := h.svc.Resolve(context.Background(), adminAlice, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		fmt.Sprintf("%v", got), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got), got.String(),
	} {
		if containsAny(s, goodKey, fakePubKeyB) {
			t.Fatalf("formatted resolve leaks a key: %s", s)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// storageGrant builds a grant row directly, bypassing the service checks.
func storageGrant(profileID, grantee string) storage.TelephonyGrant {
	return storage.TelephonyGrant{ProfileID: profileID, GranteeUserID: grantee, GrantedByUserID: "alice", CreatedAt: "2026-11-25T10:00:00.000Z"}
}

// storageSecret builds a secret row owned by owner with the given purpose.
func storageSecret(id, owner, purpose string) storage.Secret {
	return storage.Secret{ID: id, OwnerUserID: owner, Name: id, Purpose: purpose, Ciphertext: []byte("sealed")}
}

// A grant revoked while its call is live must not strand the leg: the hangup
// path still reads the owner's key. The general resolver still refuses the
// revoked grantee, so the hangup path is the only exception.
// A grantee's leg is dialed on alice's profile. After alice revokes bob, the
// leg must still be hung up with alice's key. The leg carries the profile owner
// (alice), not the call owner (bob), so the lookup is by alice (R1, m2).
func TestResolveForHangupSurvivesRevokedGrant(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, id, "bob"); err != nil {
		t.Fatal(err)
	}
	// bob can dial on the grant: this is the grantee leg being placed.
	if _, err := h.svc.Resolve(ctx, userBob, id); err != nil {
		t.Fatalf("grantee dial resolve before revoke: %v", err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, id, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Resolve(ctx, userBob, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked grantee resolve = %v, want ErrNotFound", err)
	}
	// The hangup resolves by the profile owner, as a grantee's leg now does.
	got, err := h.svc.ResolveForHangup(ctx, "alice", id)
	if err != nil {
		t.Fatalf("hangup resolve after revoke: %v", err)
	}
	if got.APIKey != goodKey || got.OwnerUserID != "alice" {
		t.Fatalf("hangup resolve = %+v", got)
	}
	// Resolving by the grantee's own id, the bug R1 fixes, is refused.
	if _, err := h.svc.ResolveForHangup(ctx, "bob", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hangup keyed by grantee = %v, want ErrNotFound", err)
	}
}

// The hangup path is keyed by the owner. A stranger's id, or a profile the
// owner does not hold, gets no key.
func TestResolveForHangupRefusesNonOwner(t *testing.T) {
	h, id := resolveHarness(t)
	ctx := context.Background()
	if _, err := h.svc.ResolveForHangup(ctx, "carol", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger hangup resolve = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.ResolveForHangup(ctx, "", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty owner = %v, want ErrNotFound", err)
	}
}
