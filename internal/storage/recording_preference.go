package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RecordingPreference is one user's recording opt-in. AcknowledgedAt is the
// time the owner accepted the consent notice for the current opt-in; it is
// empty while the opt-in is OFF.
type RecordingPreference struct {
	Enabled        bool
	AcknowledgedAt string
	RetentionDays  int
}

// GetRecordingPreference returns the owner's recording opt-in and retention.
func (s *Store) GetRecordingPreference(ctx context.Context, userID string) (RecordingPreference, error) {
	var optIn int
	var ack sql.NullString
	var days int
	err := s.db.QueryRowContext(ctx, `SELECT recording_opt_in, recording_acknowledged_at, recording_retention_days
		FROM users WHERE id = ?`, userID).Scan(&optIn, &ack, &days)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordingPreference{}, ErrNotFound
	}
	if err != nil {
		return RecordingPreference{}, fmt.Errorf("get recording preference: %w", err)
	}
	return RecordingPreference{Enabled: optIn == 1, AcknowledgedAt: ack.String, RetentionDays: days}, nil
}

// SetRecordingEnabled writes the owner's opt-in with its audit event in one
// transaction. Turning recording on stamps acknowledgedAt; turning it off keeps
// the previous acknowledgement, so the audit trail of consent is not erased.
// It returns the stored preference after the write.
func (s *Store) SetRecordingEnabled(ctx context.Context, userID string, enabled bool, acknowledgedAt, now string, audit AuditEvent) (RecordingPreference, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordingPreference{}, fmt.Errorf("begin recording preference: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if enabled {
		_, err = tx.ExecContext(ctx, `UPDATE users SET recording_opt_in = 1,
			recording_acknowledged_at = ?, updated_at = ? WHERE id = ?`, acknowledgedAt, now, userID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE users SET recording_opt_in = 0, updated_at = ? WHERE id = ?`, now, userID)
	}
	if err != nil {
		return RecordingPreference{}, fmt.Errorf("set recording preference: %w", err)
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return RecordingPreference{}, err
	}
	var pref RecordingPreference
	var optIn int
	var ack sql.NullString
	var days int
	if err := tx.QueryRowContext(ctx, `SELECT recording_opt_in, recording_acknowledged_at, recording_retention_days
		FROM users WHERE id = ?`, userID).Scan(&optIn, &ack, &days); err != nil {
		return RecordingPreference{}, fmt.Errorf("read recording preference: %w", err)
	}
	pref = RecordingPreference{Enabled: optIn == 1, AcknowledgedAt: ack.String, RetentionDays: days}
	if err := tx.Commit(); err != nil {
		return RecordingPreference{}, fmt.Errorf("commit recording preference: %w", err)
	}
	return pref, nil
}
