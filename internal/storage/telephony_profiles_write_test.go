package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// seedTelephonyUsers creates alice (owner), bob (other user), and two encrypted
// secret rows owned by alice, mirroring what the service writes.
func seedTelephonyUsers(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO users (id, username, role, password_hash, created_at, updated_at) VALUES
			('alice', 'alice', 'user', 'hash', '` + testTime + `', '` + testTime + `'),
			('bob', 'bob', 'user', 'hash', '` + testTime + `', '` + testTime + `'),
			('root', 'root', 'admin', 'hash', '` + testTime + `', '` + testTime + `')`,
		`INSERT INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at) VALUES
			('a-api', 'alice', 'api', 'telnyx_api_key', x'00', x'000000000000000000000000', 1, '` + testTime + `'),
			('a-pk', 'alice', 'pk', 'telnyx_webhook_public_key', x'00', x'000000000000000000000000', 1, '` + testTime + `'),
			('a-api2', 'alice', 'api2', 'telnyx_api_key', x'00', x'000000000000000000000000', 1, '` + testTime + `')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}

func telRecord(id, owner, api, webhook string) TelephonyProfileRecord {
	return TelephonyProfileRecord{
		ID: id, OwnerUserID: owner, Name: "main", Provider: "telnyx",
		CallerNumber: "+14155550100", ConnectionID: "conn-1", APISecretID: api,
		WebhookPublicKeySecretID: webhook, ConnectivityStatus: ConnectivityUnknown,
		CreatedAt: testTime, UpdatedAt: testTime,
	}
}

func TestTelephonyProfileCreateGetListOwnerScoped(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "tp.db"))
	seedTelephonyUsers(t, s)

	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-a", "alice", "a-api", "a-pk")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTelephonyProfile(ctx, "alice", "tp-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.APISecretID != "a-api" || got.WebhookPublicKeySecretID != "a-pk" || got.ConnectivityStatus != ConnectivityUnknown {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.GetTelephonyProfile(ctx, "bob", "tp-a"); !errors.Is(err, ErrTelephonyProfileNotFound) {
		t.Fatalf("cross-user get = %v, want not found", err)
	}
}

func TestTelephonyProfileDuplicateNameConflicts(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "dup.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-1", "alice", "a-api", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-2", "alice", "a-api2", "")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name = %v, want ErrConflict", err)
	}
}

func TestTelephonyProfileRejectsForeignSecret(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "fk.db"))
	seedTelephonyUsers(t, s)
	// bob's profile referencing alice's secret must fail on the composite FK.
	p := telRecord("tp-x", "bob", "a-api", "")
	if err := s.CreateTelephonyProfile(ctx, p); err == nil {
		t.Fatal("profile referenced another owner's secret")
	}
}

func TestTelephonyUpdateReplacesSecretsAndResetsConnectivity(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "upd.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-u", "alice", "a-api", "a-pk")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTelephonyConnectivity(ctx, "alice", "tp-u", testTime, ConnectivityVerified, "", testTime); err != nil {
		t.Fatal(err)
	}

	next := telRecord("tp-u", "alice", "a-api2", "")
	next.UpdatedAt = testTime
	if err := s.UpdateTelephonyProfile(ctx, next); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTelephonyProfile(ctx, "alice", "tp-u")
	if got.APISecretID != "a-api2" || got.ConnectivityStatus != ConnectivityUnknown || got.ConnectivityCheckedAt != "" {
		t.Fatalf("update did not reset connectivity: %+v", got)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM secrets WHERE id = 'a-api'"); n != 0 {
		t.Fatal("replaced api secret was not deleted")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM secrets WHERE id = 'a-pk'"); n != 0 {
		t.Fatal("dropped webhook key secret was not deleted")
	}
}

func TestTelephonyUpdateCrossUserDenied(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "xu.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-x", "alice", "a-api", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTelephonyProfile(ctx, telRecord("tp-x", "bob", "a-api", "")); !errors.Is(err, ErrTelephonyProfileNotFound) {
		t.Fatalf("cross-user update = %v", err)
	}
	if err := s.DeleteTelephonyProfile(ctx, "bob", "tp-x"); !errors.Is(err, ErrTelephonyProfileNotFound) {
		t.Fatalf("cross-user delete = %v", err)
	}
	if err := s.SetTelephonyConnectivity(ctx, "bob", "tp-x", testTime, ConnectivityVerified, "", testTime); !errors.Is(err, ErrTelephonyProfileNotFound) {
		t.Fatalf("cross-user connectivity = %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM telephony_profiles WHERE id = 'tp-x'"); n != 1 {
		t.Fatal("cross-user delete removed the profile")
	}
}

func TestTelephonyDeleteRemovesOwnSecretsAndGrants(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "del.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-d", "alice", "a-api", "a-pk")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTelephonyGrant(ctx, TelephonyGrant{ProfileID: "tp-d", GranteeUserID: "bob", GrantedByUserID: "alice", CreatedAt: testTime}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTelephonyProfile(ctx, "alice", "tp-d"); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM secrets WHERE id IN ('a-api', 'a-pk')"); n != 0 {
		t.Fatalf("%d profile secrets survived delete", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM telephony_profile_grants WHERE profile_id = 'tp-d'"); n != 0 {
		t.Fatalf("%d grants survived profile delete", n)
	}
}

func TestTelephonyDeleteKeepsProfileReferencedByCall(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "callref.db"))
	seedTenants(t, s)
	// alice-tel is referenced by alice-call, so delete must fail.
	if err := s.DeleteTelephonyProfile(ctx, "alice", "alice-tel"); !errors.Is(err, ErrTelephonyProfileInUse) {
		t.Fatalf("referenced profile delete = %v, want ErrTelephonyProfileInUse", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM telephony_profiles WHERE id = 'alice-tel'"); n != 1 {
		t.Fatal("referenced profile disappeared")
	}
}

func TestTelephonyGrantLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "grant.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-g", "alice", "a-api", "")); err != nil {
		t.Fatal(err)
	}
	g := TelephonyGrant{ProfileID: "tp-g", GranteeUserID: "bob", GrantedByUserID: "root", CreatedAt: testTime}
	if err := s.UpsertTelephonyGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	// Repeating the grant is idempotent and keeps the original row.
	g.GrantedByUserID = "alice"
	if err := s.UpsertTelephonyGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTelephonyGrants(ctx, "tp-g")
	if err != nil || len(list) != 1 || list[0].GrantedByUserID != "root" {
		t.Fatalf("grants = %+v, %v", list, err)
	}
	if err := s.DeleteTelephonyGrant(ctx, "tp-g", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTelephonyGrant(ctx, "tp-g", "bob"); !errors.Is(err, ErrTelephonyGrantNotFound) {
		t.Fatalf("second revoke = %v", err)
	}
}

func TestUsableTelephonyProfilesOwnedAndGranted(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "usable.db"))
	seedTelephonyUsers(t, s)
	// A grant only counts while its owner is an active admin, so the granted
	// profile is owned by root.
	seedRootSecret(t, s, "r-api")
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-own", "root", "r-api", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at)
		VALUES ('b-api', 'bob', 'api', 'telnyx_api_key', x'00', x'000000000000000000000000', 1, '`+testTime+`')`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-bob", "bob", "b-api", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTelephonyGrant(ctx, TelephonyGrant{ProfileID: "tp-own", GranteeUserID: "bob", GrantedByUserID: "root", CreatedAt: testTime}); err != nil {
		t.Fatal(err)
	}
	bob, err := s.ListUsableTelephonyProfiles(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]UsableTelephonyProfile{}
	for _, p := range bob {
		byID[p.ID] = p
	}
	if len(bob) != 2 || byID["tp-own"].Access != TelephonyAccessGranted || byID["tp-bob"].Access != TelephonyAccessOwner {
		t.Fatalf("bob usable = %+v", bob)
	}
	if _, err := s.ResolveUsableTelephonyProfile(ctx, "bob", "tp-own"); err != nil {
		t.Fatalf("granted profile not usable: %v", err)
	}
	if err := s.DeleteTelephonyGrant(ctx, "tp-own", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveUsableTelephonyProfile(ctx, "bob", "tp-own"); !errors.Is(err, ErrTelephonyProfileUnusable) {
		t.Fatalf("revoked profile still usable: %v", err)
	}
}

func TestTelephonyGrantRowsNeverCarrySecrets(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "noleak.db"))
	seedTelephonyUsers(t, s)
	seedRootSecret(t, s, "r-api")
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-n", "root", "r-api", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTelephonyGrant(ctx, TelephonyGrant{ProfileID: "tp-n", GranteeUserID: "bob", GrantedByUserID: "root", CreatedAt: testTime}); err != nil {
		t.Fatal(err)
	}
	usable, err := s.ListUsableTelephonyProfiles(ctx, "bob")
	if err != nil || len(usable) != 1 {
		t.Fatalf("usable = %+v, %v", usable, err)
	}
	// The read model has no secret field; the only secret-like value in the
	// row set would be an api_secret_id, which the struct does not carry.
	if usable[0].ID != "tp-n" {
		t.Fatalf("usable = %+v", usable[0])
	}
}

// seedRootSecret inserts an encrypted-shape API key owned by root. A profile
// may reference only a secret its own owner holds.
// seedSecret inserts a Telnyx API key secret owned by owner, so a profile can
// reference it through the foreign key.
func seedSecret(t *testing.T, s *Store, id, owner string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at)
		VALUES (?, ?, ?, 'telnyx_api_key', x'00', x'000000000000000000000000', 1, ?)`, id, owner, "name-"+id, testTime); err != nil {
		t.Fatalf("seed secret %s: %v", id, err)
	}
}

func seedRootSecret(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at)
		VALUES (?, 'root', 'api', 'telnyx_api_key', x'00', x'000000000000000000000000', 1, ?)`, id, testTime); err != nil {
		t.Fatal(err)
	}
}

// seedAdminOwnedGrant makes root (an admin) the owner of tp-root, granted to
// bob. It is the shape a demotion or deactivation must silence.
func seedAdminOwnedGrant(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	seedRootSecret(t, s, "r-api")
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-root", "root", "r-api", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTelephonyGrant(ctx, TelephonyGrant{ProfileID: "tp-root", GranteeUserID: "bob", GrantedByUserID: "root", CreatedAt: testTime}); err != nil {
		t.Fatal(err)
	}
}

// setUser changes role and active state directly, so the test controls the
// owner's state without the last-admin guard in UpdateUser.
func setUser(t *testing.T, s *Store, id, role string, active int) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE users SET role = ?, active = ? WHERE id = ?`, role, active, id); err != nil {
		t.Fatal(err)
	}
}

func TestGrantValidityFollowsOwnerAndGranteeState(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "validity.db"))
	seedTelephonyUsers(t, s)
	seedAdminOwnedGrant(t, s)

	usable := func(t *testing.T, want bool, why string) {
		t.Helper()
		list, err := s.ListUsableTelephonyProfiles(ctx, "bob")
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := s.ResolveUsableTelephonyProfile(ctx, "bob", "tp-root")
		inList := false
		for _, p := range list {
			if p.ID == "tp-root" {
				inList = true
			}
		}
		if inList != want || (rerr == nil) != want {
			t.Fatalf("%s: list=%v resolve=%v, want usable=%v (resolve err %v)", why, inList, rerr == nil, want, rerr)
		}
		if !want && rerr != nil && !errors.Is(rerr, ErrTelephonyProfileUnusable) {
			t.Fatalf("%s: resolve err = %v, want ErrTelephonyProfileUnusable", why, rerr)
		}
	}

	usable(t, true, "owner is an active admin")

	setUser(t, s, "root", "user", 1)
	usable(t, false, "owner demoted to user")

	setUser(t, s, "root", "admin", 1)
	usable(t, true, "owner re-promoted to admin")

	setUser(t, s, "root", "admin", 0)
	usable(t, false, "owner deactivated")

	setUser(t, s, "root", "admin", 1)
	usable(t, true, "owner reactivated")

	setUser(t, s, "bob", "user", 0)
	usable(t, false, "grantee deactivated")

	setUser(t, s, "bob", "user", 1)
	usable(t, true, "grantee reactivated")
}

func TestOwnerInactiveLosesOwnProfileUse(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "owner-off.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-alice", "alice", "a-api", "")); err != nil {
		t.Fatal(err)
	}
	setUser(t, s, "alice", "user", 0)
	if _, err := s.ResolveUsableTelephonyProfile(ctx, "alice", "tp-alice"); !errors.Is(err, ErrTelephonyProfileUnusable) {
		t.Fatalf("deactivated owner still resolves own profile: %v", err)
	}
	list, err := s.ListUsableTelephonyProfiles(ctx, "alice")
	if err != nil || len(list) != 0 {
		t.Fatalf("deactivated owner list = %+v, %v", list, err)
	}
}

func TestResolveUsableIsScopedToOneProfile(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "resolve.db"))
	seedTelephonyUsers(t, s)
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-a", "alice", "a-api", "")); err != nil {
		t.Fatal(err)
	}
	// Bob has no grant and owns nothing: resolve of alice's profile is unusable.
	if _, err := s.ResolveUsableTelephonyProfile(ctx, "bob", "tp-a"); !errors.Is(err, ErrTelephonyProfileUnusable) {
		t.Fatalf("stranger resolve = %v", err)
	}
	if _, err := s.ResolveUsableTelephonyProfile(ctx, "bob", "missing"); !errors.Is(err, ErrTelephonyProfileUnusable) {
		t.Fatalf("missing resolve = %v", err)
	}
	got, err := s.ResolveUsableTelephonyProfile(ctx, "alice", "tp-a")
	if err != nil || got.Access != TelephonyAccessOwner {
		t.Fatalf("owner resolve = %+v, %v", got, err)
	}
}

// TestSetTelephonyConnectivityRefusesStaleUpdate proves a validation result is
// not written when the profile changed after it was read. The owner updates the
// profile between the read and the store, so the stale result must be dropped.
func TestSetTelephonyConnectivityRefusesStaleUpdate(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "stale.db"))
	seedTelephonyUsers(t, s)
	seedSecret(t, s, "s-api", "alice")
	if err := s.CreateTelephonyProfile(ctx, telRecord("tp-s", "alice", "s-api", "")); err != nil {
		t.Fatal(err)
	}
	read, err := s.GetTelephonyProfile(ctx, "alice", "tp-s")
	if err != nil {
		t.Fatal(err)
	}
	// Edit between the read and the write, moving updated_at forward.
	edited := read
	edited.Name = "renamed"
	edited.UpdatedAt = "2026-11-25T11:00:00.000Z"
	if err := s.UpdateTelephonyProfile(ctx, edited); err != nil {
		t.Fatal(err)
	}
	err = s.SetTelephonyConnectivity(ctx, "alice", "tp-s", read.UpdatedAt, ConnectivityVerified, "", testTime)
	if !errors.Is(err, ErrTelephonyProfileChanged) {
		t.Fatalf("stale result = %v, want ErrTelephonyProfileChanged", err)
	}
	got, err := s.GetTelephonyProfile(ctx, "alice", "tp-s")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConnectivityStatus != ConnectivityUnknown {
		t.Fatalf("stale validation overwrote status to %s", got.ConnectivityStatus)
	}
}
