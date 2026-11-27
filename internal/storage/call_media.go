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
	// RecordEffective is true only when the call was accepted with recording
	// requested AND the owner still has recording opted in at media start. The
	// owner's opt-in is re-read here, so an opt-out takes effect on the next call.
	RecordEffective bool
}

// LoadCallForMedia reads one call's media state. It is not owner-scoped: the
// stream token in the path already binds the call, and the call is read by id.
func (s *Store) LoadCallForMedia(ctx context.Context, callID string) (MediaCall, error) {
	var m MediaCall
	var control sql.NullString
	var key sql.NullString
	var recordEffective, optIn int
	err := s.db.QueryRowContext(ctx, `SELECT c.id, c.owner_user_id, c.ai_profile_id, c.status,
		c.provider_call_control_id, c.max_duration_seconds, b.content_json,
		tp.owner_user_id, tp.webhook_public_key_secret_id, c.record_effective, u.recording_opt_in
		FROM calls c
		JOIN call_briefs b ON b.call_id = c.id AND b.owner_user_id = c.owner_user_id
		JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id
		JOIN users u ON u.id = c.owner_user_id
		WHERE c.id = ?`, callID).Scan(&m.ID, &m.OwnerUserID, &m.AIProfileID, &m.Status,
		&control, &m.MaxDurationSecs, &m.BriefJSON, &m.TelephonyOwnerUserID, &key,
		&recordEffective, &optIn)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaCall{}, ErrCallNotFound
	}
	if err != nil {
		return MediaCall{}, fmt.Errorf("load call for media: %w", err)
	}
	m.ControlID = control.String
	m.WebhookKeySecretID = key.String
	m.RecordEffective = recordEffective == 1 && optIn == 1
	if recordEffective == 1 && optIn == 0 {
		// The owner opted out before media start. The stored effective flag is
		// cleared, so the call view stops reporting recording for this call.
		// The clear is conditional, so a repeated read changes nothing.
		if _, err := s.db.ExecContext(ctx, `UPDATE calls SET record_effective = 0
			WHERE id = ? AND record_effective = 1`, callID); err != nil {
			return MediaCall{}, fmt.Errorf("clear record effective: %w", err)
		}
	}
	return m, nil
}

// BindStreamControlID records the control id a media start frame proved, when
// the call has none yet. It is conditional: it writes only while the id is
// empty and the call is still live, so it never replaces a recorded id. It
// reports whether a row was bound. A stream that binds from its start frame is
// the only way a call gets a control id without a dial response, and the local
// hangup needs that id to end the leg.
func (s *Store) BindStreamControlID(ctx context.Context, callID, controlID string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE calls SET provider_call_control_id = ?, updated_at = ?
		WHERE id = ? AND provider_call_control_id IS NULL
		AND status IN ('initiating', 'ringing', 'active')`,
		controlID, now.UTC().Format(storageTime), callID)
	if err != nil {
		return false, fmt.Errorf("bind stream control id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("bind stream control id: %w", err)
	}
	return n == 1, nil
}
