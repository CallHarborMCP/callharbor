package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

const idTS = "2026-11-24T00:00:00.000Z"

func newIdentityStore(t *testing.T) *Store {
	t.Helper()
	return openMigrated(t, filepath.Join(t.TempDir(), "identity.db"))
}

func idAudit(action string) AuditEvent {
	return AuditEvent{OccurredAt: idTS, ActorRole: "system", Action: action, ResourceType: "user", Outcome: "success", DetailJSON: "{}"}
}

func TestIdentityUserLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newIdentityStore(t)

	if n, err := s.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("empty CountUsers = %d, %v", n, err)
	}
	admin := User{ID: "a1", Username: "root", Role: "admin", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}
	if err := s.CreateFirstAdmin(ctx, admin, idAudit("setup.first_admin")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFirstAdmin(ctx, User{ID: "a2", Username: "other", Role: "admin", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}, idAudit("setup")); !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("second CreateFirstAdmin = %v, want ErrSetupComplete", err)
	}
	user := User{ID: "u1", Username: "alice", Role: "user", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}
	if err := s.CreateUser(ctx, user, idAudit("user.create")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, User{ID: "u2", Username: "alice", Role: "user", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}, idAudit("user.create")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username = %v, want ErrConflict", err)
	}

	list, err := s.ListUsers(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListUsers = %d, %v", len(list), err)
	}
	byName, err := s.GetUserByUsername(ctx, "alice")
	if err != nil || byName.ID != "u1" || byName.PasswordHash != "h" {
		t.Fatalf("GetUserByUsername = %+v, %v", byName, err)
	}
	if _, err := s.GetUserByID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUserByID missing = %v", err)
	}
	if _, err := s.GetUserByUsername(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUserByUsername missing = %v", err)
	}
}

func TestIdentityUpdateUserEnforcesLastAdminAndRevokesOnDeactivate(t *testing.T) {
	ctx := context.Background()
	s := newIdentityStore(t)
	admin := User{ID: "a1", Username: "root", Role: "admin", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}
	if err := s.CreateFirstAdmin(ctx, admin, idAudit("setup")); err != nil {
		t.Fatal(err)
	}
	user := User{ID: "u1", Username: "alice", Role: "user", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}
	if err := s.CreateUser(ctx, user, idAudit("user.create")); err != nil {
		t.Fatal(err)
	}

	demote := "user"
	if err := s.UpdateUser(ctx, "a1", &demote, nil, idTS, idAudit("user.update")); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting last admin = %v, want ErrLastAdmin", err)
	}
	if err := s.UpdateUser(ctx, "missing", &demote, nil, idTS, idAudit("user.update")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}

	promote := "admin"
	if err := s.UpdateUser(ctx, "u1", &promote, nil, idTS, idAudit("user.update")); err != nil {
		t.Fatal(err)
	}
	inactive := false
	if err := s.UpdateUser(ctx, "u1", nil, &inactive, idTS, idAudit("user.update")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUserByID(ctx, "u1")
	if got.Active || got.Role != "admin" {
		t.Fatalf("after update = %+v", got)
	}
}

func TestIdentitySessionsAndPasswordReset(t *testing.T) {
	ctx := context.Background()
	s := newIdentityStore(t)
	u := User{ID: "u1", Username: "alice", Role: "user", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}
	if err := s.CreateFirstAdmin(ctx, User{ID: "a1", Username: "root", Role: "admin", PasswordHash: "h", Active: true, CreatedAt: idTS, UpdatedAt: idTS}, idAudit("setup")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, u, idAudit("user.create")); err != nil {
		t.Fatal(err)
	}
	hash := "0123456789012345678901234567890123456789012345678901234567890123"
	sess := Session{TokenHash: hash, CSRFToken: "csrf-token-0123456789-abcdefghijkl", UserID: "u1", CreatedAt: idTS, LastSeenAt: idTS, ExpiresAt: "2026-11-25T00:00:00.000Z"}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, hash)
	if err != nil || got.UserID != "u1" || got.User.Username != "alice" || got.RevokedAt != "" {
		t.Fatalf("GetSession = %+v, %v", got, err)
	}
	if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSession missing = %v", err)
	}
	if err := s.TouchSession(ctx, hash, "2026-11-24T00:01:00.000Z"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, hash, "2026-11-24T00:02:00.000Z", idAudit("auth.logout")); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSession(ctx, hash)
	if got.RevokedAt == "" {
		t.Fatal("session not revoked")
	}

	sess2 := sess
	sess2.TokenHash = "1123456789012345678901234567890123456789012345678901234567890123"
	if err := s.CreateSession(ctx, sess2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPasswordHash(ctx, "u1", "new-hash", idTS, idAudit("user.password_reset")); err != nil {
		t.Fatal(err)
	}
	reset, _ := s.GetSession(ctx, sess2.TokenHash)
	if reset.RevokedAt == "" {
		t.Fatal("password reset did not revoke sessions")
	}
	if err := s.SetPasswordHash(ctx, "missing", "x", idTS, idAudit("user.password_reset")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset missing = %v", err)
	}
}

func TestIdentityLoginThrottleRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newIdentityStore(t)
	if th, err := s.GetThrottle(ctx, "user:alice"); err != nil || th.Failures != 0 || th.LockedUntil != "" {
		t.Fatalf("empty throttle = %+v, %v", th, err)
	}
	if err := s.UpdateThrottle(ctx, "user:alice", idTS, func(c Throttle) Throttle {
		c.Failures = 3
		c.WindowStartedAt = idTS
		return c
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateThrottle(ctx, "user:alice", idTS, func(c Throttle) Throttle {
		if c.Failures != 3 {
			t.Fatalf("increment lost: %+v", c)
		}
		c.Failures++
		c.LockedUntil = idTS
		return c
	}); err != nil {
		t.Fatal(err)
	}
	th, err := s.GetThrottle(ctx, "user:alice")
	if err != nil || th.Failures != 4 || th.LockedUntil != idTS {
		t.Fatalf("throttle = %+v, %v", th, err)
	}
}

func TestIdentityAuditInsertAndCount(t *testing.T) {
	ctx := context.Background()
	s := newIdentityStore(t)
	if err := s.InsertAudit(ctx, idAudit("auth.login")); err != nil {
		t.Fatal(err)
	}
	denied := idAudit("auth.login")
	denied.Outcome = "denied"
	if err := s.InsertAudit(ctx, denied); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountAuditEvents(ctx, "auth.login", "denied"); err != nil || n != 1 {
		t.Fatalf("CountAuditEvents = %d, %v", n, err)
	}
}
