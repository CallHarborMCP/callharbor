package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BeginLocalHangup moves a live call to ending because CallHarbor ends it:
// the max_duration cap expired, or the media or AI session failed while the
// callee was connected. It is the storage half of call.Outbound.BeginHangup,
// and the conditional update makes it idempotent. The call's own status is the
// precondition, so a call that already ended or failed is left alone.
//
// It returns the leg to hang up, with the control id and owner needed to resolve
// the key. ok is false, with no error, when the call was not live: the caller
// then sends no hangup. The later call.hangup webhook moves ending to ended.
func (s *Store) BeginLocalHangup(ctx context.Context, callID, reasonCode string, now time.Time) (leg LiveCallLeg, ok bool, err error) {
	if reasonCode == "" {
		return LiveCallLeg{}, false, errors.New("storage: begin local hangup: empty reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LiveCallLeg{}, false, fmt.Errorf("begin local hangup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := now.UTC().Format(storageTime)
	res, err := tx.ExecContext(ctx, `UPDATE calls SET status = 'ending', outcome_reason_code = ?, updated_at = ?
		WHERE id = ? AND status IN ('initiating', 'ringing', 'active') AND ended_at IS NULL`,
		reasonCode, ts, callID)
	if err != nil {
		return LiveCallLeg{}, false, fmt.Errorf("begin local hangup update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return LiveCallLeg{}, false, fmt.Errorf("begin local hangup rows: %w", err)
	}
	if n == 0 {
		return LiveCallLeg{}, false, nil
	}

	// A call with no provider control id still moves to ending, but leg.ControlID
	// stays empty, and the caller sends nothing to Telnyx.
	var control sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT c.id, c.owner_user_id, c.telephony_profile_id, tp.owner_user_id, c.provider_call_control_id
		FROM calls c JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id
		WHERE c.id = ?`, callID).Scan(&leg.ID, &leg.OwnerUserID, &leg.TelephonyProfileID, &leg.TelephonyOwnerUserID, &control); err != nil {
		return LiveCallLeg{}, false, fmt.Errorf("read leg for local hangup: %w", err)
	}
	leg.ControlID = control.String
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: callID, OwnerUserID: leg.OwnerUserID, Kind: "call.ending", State: "ending",
		ReasonCode: reasonCode, Provider: "internal", OccurredAt: ts,
	}); err != nil {
		return LiveCallLeg{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return LiveCallLeg{}, false, fmt.Errorf("commit local hangup: %w", err)
	}
	return leg, true, nil
}
