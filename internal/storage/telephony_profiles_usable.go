package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Access values describe how a user may use a telephony profile.
const (
	TelephonyAccessOwner   = "owner"
	TelephonyAccessGranted = "granted"
)

// usablePredicate is the single rule for whether a user may use a profile. It
// is shared by the list and the resolve query so the two cannot drift.
//
// The owner path needs the user to be active. The grant path needs the grantee
// to be active, and the owner to be an active admin right now. A grant is only
// meaningful while the owner could create one, so a demoted or deactivated
// owner silences their grants without deleting them. Re-promotion restores
// them. Both branches are joined against the users table in SQL.
//
// It has four placeholders, all bound to userID. The owner branch binds two
// (ownership and the user's active check). The grant branch binds two (the
// grantee match and the grantee's active check). The owner's active-admin check
// binds none. usableSelect adds one more userID placeholder for its access CASE.
const usablePredicate = `(
	(p.owner_user_id = ? AND EXISTS (SELECT 1 FROM users u WHERE u.id = ? AND u.active = 1))
	OR (
		EXISTS (SELECT 1 FROM telephony_profile_grants g
			WHERE g.profile_id = p.id AND g.grantee_user_id = ?)
		AND EXISTS (SELECT 1 FROM users me WHERE me.id = ? AND me.active = 1)
		AND EXISTS (SELECT 1 FROM users owner
			WHERE owner.id = p.owner_user_id AND owner.role = 'admin' AND owner.active = 1)
	)
)`

// usableSelect lists the columns for a usable profile. The access value is
// owner only when the user owns the profile, and granted otherwise.
const usableSelect = `SELECT p.id, p.owner_user_id, p.name, p.provider, p.caller_number,
		p.connection_id, p.connectivity_status, p.connectivity_reason, p.created_at, p.updated_at,
		CASE WHEN p.owner_user_id = ? THEN 'owner' ELSE 'granted' END AS access
	FROM telephony_profiles p`

// UsableTelephonyProfile is a profile the user may place calls through, with
// how the user holds it. The row never includes secret references.
type UsableTelephonyProfile struct {
	TelephonyProfile
	Access string
}

// ListUsableTelephonyProfiles returns profiles the user owns or holds a valid
// grant on, oldest first. A profile reached both ways is reported as owner.
func (s *Store) ListUsableTelephonyProfiles(ctx context.Context, userID string) ([]UsableTelephonyProfile, error) {
	rows, err := s.db.QueryContext(ctx, usableSelect+` WHERE `+usablePredicate+`
		ORDER BY p.created_at, p.id`,
		userID, userID, userID, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("storage: list usable telephony profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []UsableTelephonyProfile{}
	for rows.Next() {
		u, err := scanUsable(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list usable telephony profiles: %w", err)
	}
	return out, nil
}

// ErrTelephonyProfileUnusable reports that a profile is missing or is not
// usable by the user. The cases are not distinguished.
var ErrTelephonyProfileUnusable = errors.New("storage: telephony profile is not usable")

// ResolveUsableTelephonyProfile returns one profile when userID may use it. It
// is a single indexed lookup by profile id, not a scan of the user's profiles.
// It does not check connectivity; call gating on connectivity_status belongs
// to the call path (#79).
func (s *Store) ResolveUsableTelephonyProfile(ctx context.Context, userID, profileID string) (UsableTelephonyProfile, error) {
	row := s.db.QueryRowContext(ctx, usableSelect+` WHERE p.id = ? AND `+usablePredicate,
		userID, profileID, userID, userID, userID, userID)
	u, err := scanUsable(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UsableTelephonyProfile{}, ErrTelephonyProfileUnusable
	}
	if err != nil {
		return UsableTelephonyProfile{}, err
	}
	return u, nil
}

func scanUsable(r rowScanner) (UsableTelephonyProfile, error) {
	var u UsableTelephonyProfile
	var connection sql.NullString
	if err := r.Scan(&u.ID, &u.OwnerUserID, &u.Name, &u.Provider, &u.CallerNumber, &connection,
		&u.ConnectivityStatus, &u.ConnectivityReason, &u.CreatedAt, &u.UpdatedAt, &u.Access); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UsableTelephonyProfile{}, err
		}
		return UsableTelephonyProfile{}, fmt.Errorf("storage: scan usable telephony profile: %w", err)
	}
	u.ConnectionID = connection.String
	return u, nil
}
