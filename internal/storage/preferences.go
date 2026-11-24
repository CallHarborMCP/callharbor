package storage

import (
	"context"
	"database/sql"
	"errors"
)

type Appearance struct {
	Theme   string
	Preset  string
	Accent  string
	Density string
}

// GetAppearance returns the stored appearance for userID. ok is false when the
// user has never saved preferences.
func (s *Store) GetAppearance(ctx context.Context, userID string) (Appearance, bool, error) {
	var a Appearance
	err := s.db.QueryRowContext(ctx,
		"SELECT theme, preset, accent, density FROM user_preferences WHERE user_id = ?", userID).Scan(
		&a.Theme, &a.Preset, &a.Accent, &a.Density)
	if errors.Is(err, sql.ErrNoRows) {
		return Appearance{}, false, nil
	}
	if err != nil {
		return Appearance{}, false, err
	}
	return a, true, nil
}

func (s *Store) SetAppearance(ctx context.Context, userID string, a Appearance, now string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, theme, preset, accent, density, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET theme = excluded.theme, preset = excluded.preset,
		 accent = excluded.accent, density = excluded.density, updated_at = excluded.updated_at`,
		userID, a.Theme, a.Preset, a.Accent, a.Density, now)
	return err
}
