package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// TelephonyProfile is the owner-scoped read model for a telephony profile.
// Secret references are deliberately omitted: no read model exposes them.
type TelephonyProfile struct {
	ID                 string
	OwnerUserID        string
	Name               string
	Provider           string
	CallerNumber       string
	ConnectionID       string
	ConnectivityStatus string
	ConnectivityReason string
	CreatedAt          string
	UpdatedAt          string
}

// ListTelephonyProfiles returns the owner's telephony profiles, oldest first.
// Profiles owned by other users are never returned.
func (s *Store) ListTelephonyProfiles(ctx context.Context, ownerUserID string) ([]TelephonyProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner_user_id, name, provider, caller_number, connection_id, connectivity_status, connectivity_reason, created_at, updated_at
		FROM telephony_profiles WHERE owner_user_id = ? ORDER BY created_at, id`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("storage: list telephony profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []TelephonyProfile{}
	for rows.Next() {
		var p TelephonyProfile
		var connection sql.NullString
		if err := rows.Scan(&p.ID, &p.OwnerUserID, &p.Name, &p.Provider, &p.CallerNumber, &connection, &p.ConnectivityStatus, &p.ConnectivityReason, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan telephony profile: %w", err)
		}
		p.ConnectionID = connection.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list telephony profiles: %w", err)
	}
	return out, nil
}
