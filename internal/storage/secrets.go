package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Secret is one encrypted secret row. Ciphertext and Nonce are opaque bytes;
// plaintext never reaches this layer.
type Secret struct {
	ID          string
	OwnerUserID string
	Name        string
	Purpose     string
	Ciphertext  []byte
	Nonce       []byte
	KeyVersion  int
	CreatedAt   string
	RotatedAt   string
}

// SecretMeta is the non-secret view of a secret that may be listed.
type SecretMeta struct {
	ID        string
	Name      string
	Purpose   string
	CreatedAt string
	RotatedAt string
}

// InsertSecret stores an encrypted secret for its owner. A duplicate name for
// the same owner returns ErrConflict.
func (s *Store) InsertSecret(ctx context.Context, sec Secret) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets
		(id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at, rotated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sec.ID, sec.OwnerUserID, sec.Name, sec.Purpose, sec.Ciphertext, sec.Nonce,
		sec.KeyVersion, sec.CreatedAt, nullable(sec.RotatedAt))
	if err != nil {
		return mapConflict(fmt.Errorf("storage: insert secret: %w", err))
	}
	return nil
}

// GetSecret returns a secret only when it belongs to ownerUserID. Any other
// owner, or a missing id, returns ErrNotFound.
func (s *Store) GetSecret(ctx context.Context, ownerUserID, id string) (Secret, error) {
	var sec Secret
	var rotated sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at, rotated_at
		FROM secrets WHERE id = ? AND owner_user_id = ?`, id, ownerUserID).
		Scan(&sec.ID, &sec.OwnerUserID, &sec.Name, &sec.Purpose, &sec.Ciphertext, &sec.Nonce, &sec.KeyVersion, &sec.CreatedAt, &rotated)
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, fmt.Errorf("storage: get secret: %w", err)
	}
	sec.RotatedAt = rotated.String
	return sec, nil
}

// ListSecrets returns metadata for the owner's secrets, oldest first. It never
// selects ciphertext.
func (s *Store) ListSecrets(ctx context.Context, ownerUserID string) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, purpose, created_at, rotated_at
		FROM secrets WHERE owner_user_id = ? ORDER BY created_at, id`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("storage: list secrets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		var rotated sql.NullString
		if err := rows.Scan(&m.ID, &m.Name, &m.Purpose, &m.CreatedAt, &rotated); err != nil {
			return nil, fmt.Errorf("storage: scan secret: %w", err)
		}
		m.RotatedAt = rotated.String
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list secrets: %w", err)
	}
	return out, nil
}

// CountSecrets returns the number of encrypted secret rows across all owners.
// Startup uses it to fail closed when secrets exist but no master key is set.
func (s *Store) CountSecrets(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets").Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count secrets: %w", err)
	}
	return n, nil
}
