package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MediaCall is the stored state the media path reads before it accepts a
// stream: enough to bind the stream, resolve the AI profile and render the
// brief. The webhook key comes from the telephony profile the call uses.
type MediaCall struct {
	ID              string
	OwnerUserID     string
	AIProfileID     string
	Status          string
	ControlID       string
	MaxDurationSecs int
	BriefJSON       string
	// WebhookKeySecretID names the telephony profile's webhook key secret. The
	// key is resolved through the secrets service, never read from storage here.
	WebhookKeySecretID string
	// TelephonyOwnerUserID owns the telephony profile and its secrets. It can
	// differ from OwnerUserID when the call was made by a grantee.
	TelephonyOwnerUserID string
}

// LoadCallForMedia reads one call's media state. It is not owner-scoped: the
// stream token in the path already binds the call, and the call is read by id.
func (s *Store) LoadCallForMedia(ctx context.Context, callID string) (MediaCall, error) {
	var m MediaCall
	var control sql.NullString
	var key sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT c.id, c.owner_user_id, c.ai_profile_id, c.status,
		c.provider_call_control_id, c.max_duration_seconds, b.content_json,
		tp.owner_user_id, tp.webhook_public_key_secret_id
		FROM calls c
		JOIN call_briefs b ON b.call_id = c.id AND b.owner_user_id = c.owner_user_id
		JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id
		WHERE c.id = ?`, callID).Scan(&m.ID, &m.OwnerUserID, &m.AIProfileID, &m.Status,
		&control, &m.MaxDurationSecs, &m.BriefJSON, &m.TelephonyOwnerUserID, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaCall{}, ErrCallNotFound
	}
	if err != nil {
		return MediaCall{}, fmt.Errorf("load call for media: %w", err)
	}
	m.ControlID = control.String
	m.WebhookKeySecretID = key.String
	return m, nil
}

// FailOpenCall marks a call failed with a reason. Unlike MarkCallFailed it also
// covers ringing and active calls, because a media-path failure can happen
// after the callee has answered. A call already ended or failed is not
// changed. One event is appended.
func (s *Store) FailOpenCall(ctx context.Context, callID, reasonCode string, now time.Time) error {
	if reasonCode == "" {
		return errors.New("storage: fail open call: empty reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fail open call: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(storageTime)
	res, err := tx.ExecContext(ctx, `UPDATE calls SET status = 'failed', outcome_reason_code = ?,
		ended_at = ?, updated_at = ?
		WHERE id = ? AND status IN ('queued', 'initiating', 'ringing', 'active', 'ending') AND ended_at IS NULL`,
		reasonCode, ts, ts, callID)
	if err != nil {
		return fmt.Errorf("fail open call: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail open call rows: %w", err)
	}
	if n == 0 {
		return nil
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM calls WHERE id = ?`, callID).Scan(&owner); err != nil {
		return fmt.Errorf("read call owner: %w", err)
	}
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: callID, OwnerUserID: owner, Kind: "call.failed", State: "failed",
		ReasonCode: reasonCode, Provider: "telnyx", OccurredAt: ts,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
