package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func insertPrefsUser(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO users (id, username, role, password_hash, active, created_at, updated_at) VALUES (?, ?, 'user', 'h', 1, 'x', 'x')`,
		id, id); err != nil {
		t.Fatal(err)
	}
}

func TestAppearanceStoreRoundTripUpsertAndDefaults(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "prefs.db"))
	insertPrefsUser(t, s, "alice")

	if _, ok, err := s.GetAppearance(ctx, "alice"); err != nil || ok {
		t.Fatalf("unset appearance: ok=%v err=%v", ok, err)
	}
	first := Appearance{Theme: "dark", Preset: "console", Accent: "blue", Density: "comfortable"}
	if err := s.SetAppearance(ctx, "alice", first, "2026-11-24T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	second := Appearance{Theme: "light", Preset: "harbor", Accent: "green", Density: "compact"}
	if err := s.SetAppearance(ctx, "alice", second, "2026-11-24T00:00:01.000Z"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAppearance(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("get after upsert: ok=%v err=%v", ok, err)
	}
	if got != second {
		t.Fatalf("upsert kept stale value: %+v", got)
	}
}

func TestAppearanceStoreRejectsUnknownUser(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "prefs-fk.db"))
	err := s.SetAppearance(ctx, "ghost", Appearance{Theme: "dark", Preset: "console", Accent: "blue", Density: "compact"}, "x")
	if err == nil {
		t.Fatal("preference for unknown user accepted")
	}
}

func TestAppearanceStoreRejectsOutOfSetValues(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "prefs-check.db"))
	insertPrefsUser(t, s, "alice")
	err := s.SetAppearance(ctx, "alice", Appearance{Theme: "neon", Preset: "console", Accent: "blue", Density: "compact"}, "x")
	if err == nil {
		t.Fatal("CHECK constraint did not reject unknown theme")
	}
}
