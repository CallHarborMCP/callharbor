package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrTelephonyProfileNotFound reports a missing profile, or one the caller may
// not see. The two cases are indistinguishable.
var ErrTelephonyProfileNotFound = errors.New("storage: telephony profile not found")

// ErrTelephonyProfileInUse reports a delete blocked because calls reference the
// profile. Its history is kept, so the profile stays.
var ErrTelephonyProfileInUse = errors.New("storage: telephony profile is referenced by calls")

// ErrTelephonyGrantNotFound reports a grant that does not exist.
var ErrTelephonyGrantNotFound = errors.New("storage: telephony profile grant not found")

// Connectivity statuses stored on a telephony profile.
const (
	ConnectivityUnknown  = "unknown"
	ConnectivityVerified = "verified"
	ConnectivityFailed   = "failed"
)

// TelephonyProfileRecord is the full persisted profile, including its secret
// references. It is internal: API and MCP views never expose the references.
type TelephonyProfileRecord struct {
	ID                       string
	OwnerUserID              string
	Name                     string
	Provider                 string
	CallerNumber             string
	ConnectionID             string
	APISecretID              string
	WebhookPublicKeySecretID string
	ConnectivityStatus       string
	ConnectivityReason       string
	ConnectivityCheckedAt    string
	CreatedAt                string
	UpdatedAt                string
}

// TelephonyGrant is one owner-granted use of a profile.
type TelephonyGrant struct {
	ProfileID       string
	GranteeUserID   string
	GrantedByUserID string
	CreatedAt       string
}

const telephonyRecordColumns = `id, owner_user_id, name, provider, caller_number, connection_id,
	api_secret_id, webhook_public_key_secret_id, connectivity_status, connectivity_reason,
	connectivity_checked_at, created_at, updated_at`

func scanTelephonyRecord(row interface{ Scan(...any) error }) (TelephonyProfileRecord, error) {
	var p TelephonyProfileRecord
	var conn, webhook, checked sql.NullString
	if err := row.Scan(&p.ID, &p.OwnerUserID, &p.Name, &p.Provider, &p.CallerNumber, &conn,
		&p.APISecretID, &webhook, &p.ConnectivityStatus, &p.ConnectivityReason,
		&checked, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return TelephonyProfileRecord{}, err
	}
	p.ConnectionID = conn.String
	p.WebhookPublicKeySecretID = webhook.String
	p.ConnectivityCheckedAt = checked.String
	return p, nil
}

// CreateTelephonyProfile inserts a profile for its owner. A duplicate name for
// the same owner returns ErrConflict. Secret references must belong to the
// owner, or the insert fails on the composite foreign keys.
func (s *Store) CreateTelephonyProfile(ctx context.Context, p TelephonyProfileRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO telephony_profiles
		(id, owner_user_id, name, provider, caller_number, connection_id, api_secret_id,
		 webhook_public_key_secret_id, connectivity_status, connectivity_reason, connectivity_checked_at,
		 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OwnerUserID, p.Name, p.Provider, p.CallerNumber, nullable(p.ConnectionID),
		p.APISecretID, nullable(p.WebhookPublicKeySecretID), p.ConnectivityStatus,
		p.ConnectivityReason, nullable(p.ConnectivityCheckedAt), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return mapConflict(fmt.Errorf("storage: create telephony profile: %w", err))
	}
	return nil
}

// GetTelephonyProfile returns a profile only when it belongs to ownerUserID.
func (s *Store) GetTelephonyProfile(ctx context.Context, ownerUserID, id string) (TelephonyProfileRecord, error) {
	p, err := scanTelephonyRecord(s.db.QueryRowContext(ctx,
		"SELECT "+telephonyRecordColumns+" FROM telephony_profiles WHERE id = ? AND owner_user_id = ?", id, ownerUserID))
	if errors.Is(err, sql.ErrNoRows) {
		return TelephonyProfileRecord{}, ErrTelephonyProfileNotFound
	}
	if err != nil {
		return TelephonyProfileRecord{}, fmt.Errorf("storage: get telephony profile: %w", err)
	}
	return p, nil
}

// UpdateTelephonyProfile replaces the mutable fields of a profile owned by
// p.OwnerUserID. It returns ErrTelephonyProfileNotFound for another owner's
// profile. Updating any field resets the connectivity result, because the
// previous check no longer describes the saved configuration. Secret rows that
// the update replaces are deleted in the same transaction, so no orphaned
// credential survives.
func (s *Store) UpdateTelephonyProfile(ctx context.Context, p TelephonyProfileRecord) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		old, err := scanTelephonyRecord(tx.QueryRowContext(ctx,
			"SELECT "+telephonyRecordColumns+" FROM telephony_profiles WHERE id = ? AND owner_user_id = ?", p.ID, p.OwnerUserID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTelephonyProfileNotFound
		}
		if err != nil {
			return fmt.Errorf("storage: read telephony profile: %w", err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE telephony_profiles SET name = ?, caller_number = ?,
			connection_id = ?, api_secret_id = ?, webhook_public_key_secret_id = ?,
			connectivity_status = ?, connectivity_reason = ?, connectivity_checked_at = ?, updated_at = ?
			WHERE id = ? AND owner_user_id = ?`,
			p.Name, p.CallerNumber, nullable(p.ConnectionID), p.APISecretID,
			nullable(p.WebhookPublicKeySecretID), p.ConnectivityStatus, p.ConnectivityReason,
			nullable(p.ConnectivityCheckedAt), p.UpdatedAt, p.ID, p.OwnerUserID)
		if err != nil {
			return mapConflict(fmt.Errorf("storage: update telephony profile: %w", err))
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("storage: update telephony profile: %w", err)
		}
		if n == 0 {
			return ErrTelephonyProfileNotFound
		}
		return deleteReplacedSecrets(ctx, tx, old, p)
	})
}

// deleteReplacedSecrets removes the telephony secret rows that an update no
// longer references. Secrets that are still referenced are kept.
func deleteReplacedSecrets(ctx context.Context, tx *sql.Tx, old TelephonyProfileRecord, updated TelephonyProfileRecord) error {
	stale := []string{}
	if old.APISecretID != "" && old.APISecretID != updated.APISecretID {
		stale = append(stale, old.APISecretID)
	}
	if old.WebhookPublicKeySecretID != "" && old.WebhookPublicKeySecretID != updated.WebhookPublicKeySecretID {
		stale = append(stale, old.WebhookPublicKeySecretID)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, "DELETE FROM secrets WHERE id = ? AND owner_user_id = ?", id, updated.OwnerUserID); err != nil {
			return fmt.Errorf("storage: delete replaced secret: %w", err)
		}
	}
	return nil
}

// ErrTelephonyProfileChanged reports that the profile was updated after the
// caller read it, so a validation result for the old version must not be stored.
var ErrTelephonyProfileChanged = errors.New("storage: telephony profile changed")

// SetTelephonyConnectivity stores the result of one read-only validation for a
// profile owned by ownerUserID. The reason is a fixed code, never provider text.
// It writes only while updated_at still equals expectedUpdatedAt, the value the
// caller read before validating, and it does not change updated_at. When the
// profile is gone the error is ErrTelephonyProfileNotFound; when it changed,
// ErrTelephonyProfileChanged, and nothing is written.
func (s *Store) SetTelephonyConnectivity(ctx context.Context, ownerUserID, id, expectedUpdatedAt, status, reason, checkedAt string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE telephony_profiles SET connectivity_status = ?,
		connectivity_reason = ?, connectivity_checked_at = ?
		WHERE id = ? AND owner_user_id = ? AND updated_at = ?`,
		status, reason, checkedAt, id, ownerUserID, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("storage: set telephony connectivity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: set telephony connectivity: %w", err)
	}
	if n == 1 {
		return nil
	}
	// Zero rows: either the profile is gone or its updated_at moved on.
	var exists int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM telephony_profiles WHERE id = ? AND owner_user_id = ?", id, ownerUserID).Scan(&exists); err != nil {
		return fmt.Errorf("storage: set telephony connectivity: %w", err)
	}
	if exists == 0 {
		return ErrTelephonyProfileNotFound
	}
	return ErrTelephonyProfileChanged
}

// DeleteTelephonyProfile removes a profile owned by ownerUserID, together with
// its own secret rows, in one transaction. Grants are removed by cascade. A
// profile that a call references is kept by the restrict foreign key on calls,
// and the error is returned unchanged.
func (s *Store) DeleteTelephonyProfile(ctx context.Context, ownerUserID, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		old, err := scanTelephonyRecord(tx.QueryRowContext(ctx,
			"SELECT "+telephonyRecordColumns+" FROM telephony_profiles WHERE id = ? AND owner_user_id = ?", id, ownerUserID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTelephonyProfileNotFound
		}
		if err != nil {
			return fmt.Errorf("storage: read telephony profile: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM telephony_profiles WHERE id = ? AND owner_user_id = ?", id, ownerUserID); err != nil {
			if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
				return ErrTelephonyProfileInUse
			}
			return fmt.Errorf("storage: delete telephony profile: %w", err)
		}
		for _, sid := range []string{old.APISecretID, old.WebhookPublicKeySecretID} {
			if sid == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM secrets WHERE id = ? AND owner_user_id = ?", sid, ownerUserID); err != nil {
				return fmt.Errorf("storage: delete profile secret: %w", err)
			}
		}
		return nil
	})
}

// DeleteOwnedSecret removes one secret row for its owner. The service uses it
// to roll back a secret that was written for a request that then failed. A
// secret that a profile still references is kept by the restrict foreign key.
func (s *Store) DeleteOwnedSecret(ctx context.Context, ownerUserID, id string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE id = ? AND owner_user_id = ?", id, ownerUserID); err != nil {
		return fmt.Errorf("storage: delete secret: %w", err)
	}
	return nil
}

// UpsertTelephonyGrant grants granteeUserID use of a profile. The caller has
// already checked that the grantor is an admin or the owner.
func (s *Store) UpsertTelephonyGrant(ctx context.Context, g TelephonyGrant) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO telephony_profile_grants
		(profile_id, grantee_user_id, granted_by_user_id, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (profile_id, grantee_user_id) DO NOTHING`,
		g.ProfileID, g.GranteeUserID, g.GrantedByUserID, g.CreatedAt)
	if err != nil {
		return fmt.Errorf("storage: grant telephony profile: %w", err)
	}
	return nil
}

// DeleteTelephonyGrant removes one grant. A missing grant returns
// ErrTelephonyGrantNotFound.
func (s *Store) DeleteTelephonyGrant(ctx context.Context, profileID, granteeUserID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM telephony_profile_grants WHERE profile_id = ? AND grantee_user_id = ?", profileID, granteeUserID)
	if err != nil {
		return fmt.Errorf("storage: revoke telephony profile: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: revoke telephony profile: %w", err)
	}
	if n == 0 {
		return ErrTelephonyGrantNotFound
	}
	return nil
}

// ListTelephonyGrants returns the grants on one profile, oldest first.
func (s *Store) ListTelephonyGrants(ctx context.Context, profileID string) ([]TelephonyGrant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id, grantee_user_id, granted_by_user_id, created_at
		FROM telephony_profile_grants WHERE profile_id = ? ORDER BY created_at, grantee_user_id`, profileID)
	if err != nil {
		return nil, fmt.Errorf("storage: list telephony grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []TelephonyGrant{}
	for rows.Next() {
		var g TelephonyGrant
		if err := rows.Scan(&g.ProfileID, &g.GranteeUserID, &g.GrantedByUserID, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan telephony grant: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list telephony grants: %w", err)
	}
	return out, nil
}
