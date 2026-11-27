package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Recording statuses. The 'deleted' status is set only by retention purge.
const (
	RecordingStatusRecording = "recording"
	RecordingStatusComplete  = "complete"
	RecordingStatusPartial   = "partial"
	RecordingStatusFailed    = "failed"
	RecordingStatusDeleted   = "deleted"
)

// ErrRecordingNotFound reports that a call has no recording row.
var ErrRecordingNotFound = errors.New("storage: recording not found")

// Recording is one stored recording row. StoragePath is relative to the data
// directory and never leaves the server.
type Recording struct {
	ID           string
	CallID       string
	OwnerUserID  string
	Status       string
	StoragePath  string
	ByteCount    int64
	DurationMS   int64
	SHA256       string
	KeyVersion   int
	SampleRate   int
	Channels     int
	QualityFlags []string
	CreatedAt    string
	FinalizedAt  string
	ExpiresAt    string
}

// InsertRecording creates the recording row when a recorder starts. The row is
// the single record for a call: a second insert for the same call fails on the
// unique call_id constraint, so a second media stream cannot create one.
func (s *Store) InsertRecording(ctx context.Context, r Recording) error {
	flags, err := encodeFlags(r.QualityFlags)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO recordings
		(id, call_id, owner_user_id, status, storage_path, byte_count, duration_ms,
		 key_version, created_at, sample_rate, channels, quality_flags)
		VALUES (?, ?, ?, 'recording', ?, 0, 0, ?, ?, NULL, 2, ?)`,
		r.ID, r.CallID, r.OwnerUserID, r.StoragePath, r.KeyVersion, r.CreatedAt, flags)
	if err != nil {
		return fmt.Errorf("insert recording: %w", err)
	}
	return nil
}

// SetRecordingFormat records the leg sample rate once the writer fixes it.
func (s *Store) SetRecordingFormat(ctx context.Context, id string, sampleRate int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET sample_rate = ? WHERE id = ? AND status = 'recording'`,
		sampleRate, id)
	if err != nil {
		return fmt.Errorf("set recording format: %w", err)
	}
	return nil
}

// FinalizeRecording writes the terminal state of a recording. It is the only
// writer of a non-recording status, and it is conditional on the row still
// being 'recording', so a second finalize (for example after a crash sweep)
// changes nothing and reports false.
func (s *Store) FinalizeRecording(ctx context.Context, f RecordingFinal) (bool, error) {
	flags, err := encodeFlags(f.QualityFlags)
	if err != nil {
		return false, err
	}
	switch f.Status {
	case RecordingStatusComplete, RecordingStatusPartial, RecordingStatusFailed:
	default:
		return false, fmt.Errorf("storage: finalize recording: status %q", f.Status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE recordings
		SET status = ?, byte_count = ?, duration_ms = ?, sha256 = ?, quality_flags = ?,
		    finalized_at = ?, expires_at = ?
		WHERE id = ? AND status = 'recording'`,
		f.Status, f.ByteCount, f.DurationMS, nullIfEmpty(f.SHA256), flags,
		f.FinalizedAt, f.ExpiresAt, f.ID)
	if err != nil {
		return false, fmt.Errorf("finalize recording: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finalize recording: %w", err)
	}
	return n == 1, nil
}

// RecordingFinal is the terminal state written by FinalizeRecording.
type RecordingFinal struct {
	ID           string
	Status       string
	ByteCount    int64
	DurationMS   int64
	SHA256       string
	QualityFlags []string
	FinalizedAt  string
	ExpiresAt    string
}

// GetRecordingForCall returns the recording for one call owned by ownerUserID.
// A call owned by someone else has no rows for this owner, so it reads as
// ErrRecordingNotFound.
func (s *Store) GetRecordingForCall(ctx context.Context, ownerUserID, callID string) (Recording, error) {
	var r Recording
	var sha, finalized, expires sql.NullString
	var sampleRate sql.NullInt64
	var flags string
	err := s.db.QueryRowContext(ctx, `SELECT id, call_id, owner_user_id, status, storage_path,
		byte_count, duration_ms, sha256, key_version, sample_rate, channels, quality_flags,
		created_at, finalized_at, expires_at
		FROM recordings WHERE call_id = ? AND owner_user_id = ?`, callID, ownerUserID).Scan(
		&r.ID, &r.CallID, &r.OwnerUserID, &r.Status, &r.StoragePath,
		&r.ByteCount, &r.DurationMS, &sha, &r.KeyVersion, &sampleRate, &r.Channels, &flags,
		&r.CreatedAt, &finalized, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, ErrRecordingNotFound
	}
	if err != nil {
		return Recording{}, fmt.Errorf("get recording: %w", err)
	}
	r.SHA256 = sha.String
	r.FinalizedAt = finalized.String
	r.ExpiresAt = expires.String
	if sampleRate.Valid {
		r.SampleRate = int(sampleRate.Int64)
	}
	if err := json.Unmarshal([]byte(flags), &r.QualityFlags); err != nil {
		return Recording{}, fmt.Errorf("recording quality flags: %w", err)
	}
	if r.QualityFlags == nil {
		r.QualityFlags = []string{}
	}
	return r, nil
}

// ListRecordingsByStatus returns every recording row with the given status,
// oldest first. The crash sweep uses it for 'recording' rows at startup.
func (s *Store) ListRecordingsByStatus(ctx context.Context, status string) ([]Recording, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, call_id, owner_user_id, storage_path, key_version,
		created_at, COALESCE(sample_rate, 0), channels, quality_flags
		FROM recordings WHERE status = ? ORDER BY created_at, id`, status)
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Recording
	for rows.Next() {
		var r Recording
		var flags string
		if err := rows.Scan(&r.ID, &r.CallID, &r.OwnerUserID, &r.StoragePath, &r.KeyVersion,
			&r.CreatedAt, &r.SampleRate, &r.Channels, &flags); err != nil {
			return nil, fmt.Errorf("scan recording: %w", err)
		}
		if err := json.Unmarshal([]byte(flags), &r.QualityFlags); err != nil {
			return nil, fmt.Errorf("recording quality flags: %w", err)
		}
		r.Status = status
		out = append(out, r)
	}
	return out, rows.Err()
}

// OwnerRecordingRetentionDays returns the owner's configured retention, used to
// compute expires_at at finalize.
func (s *Store) OwnerRecordingRetentionDays(ctx context.Context, ownerUserID string) (int, error) {
	var days int
	err := s.db.QueryRowContext(ctx, `SELECT recording_retention_days FROM users WHERE id = ?`, ownerUserID).Scan(&days)
	if err != nil {
		return 0, fmt.Errorf("owner retention: %w", err)
	}
	return days, nil
}

// RecordingExpiry is created_at plus the retention window, in storage time.
func RecordingExpiry(createdAt string, days int) (string, error) {
	t, err := time.Parse(storageTime, createdAt)
	if err != nil {
		return "", fmt.Errorf("recording created_at: %w", err)
	}
	return t.Add(time.Duration(days) * 24 * time.Hour).UTC().Format(storageTime), nil
}

func encodeFlags(flags []string) (string, error) {
	if flags == nil {
		flags = []string{}
	}
	b, err := json.Marshal(flags)
	if err != nil {
		return "", fmt.Errorf("encode quality flags: %w", err)
	}
	return string(b), nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
