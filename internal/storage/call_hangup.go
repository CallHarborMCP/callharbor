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

// HangupReasonCode is the stable reason a caller's explicit hangup records.
const HangupReasonCode = "hangup_requested"

// HangupAction is what the caller must do after RequestHangup.
type HangupAction string

const (
	// HangupActionNone means the call is terminal or was never live. Nothing
	// is sent to Telnyx.
	HangupActionNone HangupAction = "none"
	// HangupActionFailed means a queued call was failed before any dial. The
	// worker claim is conditional, so it can never dial this call.
	HangupActionFailed HangupAction = "failed"
	// HangupActionAwaitingDial means a dial is in flight and has no control id
	// yet. The worker sends the hangup when the dial returns one.
	HangupActionAwaitingDial HangupAction = "awaiting_dial"
	// HangupActionSendLeg means the leg has a control id and the caller sends
	// one hangup with LegCommandID. It is also used to resend a hangup to a
	// call that is already ending, which is safe because the command id is
	// stable.
	HangupActionSendLeg HangupAction = "send_leg"
)

// HangupDecision reports the transition RequestHangup made and what the caller
// must still do. Leg is set for HangupActionSendLeg and HangupActionAwaitingDial.
type HangupDecision struct {
	Action  HangupAction
	Status  string
	Outcome string
	Leg     LiveCallLeg
}

// RequestHangup applies an explicit hangup in one transaction. Each step is a
// conditional update on the state it expects, so it is race-safe against the
// worker claim: a queued call becomes failed before a claim can match it, and a
// call the worker already claimed becomes ending instead. The audit row commits
// with the decision. The caller must authorize the actor first.
func (s *Store) RequestHangup(ctx context.Context, callID string, audit AuditEvent, now time.Time) (HangupDecision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HangupDecision{}, fmt.Errorf("begin request hangup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := now.UTC().Format(storageTime)
	failed, err := execRows(ctx, tx, `UPDATE calls SET status = 'failed', outcome_reason_code = ?,
		ended_at = ?, updated_at = ?
		WHERE id = ? AND status = 'queued' AND ended_at IS NULL`,
		HangupReasonCode, ts, ts, callID)
	if err != nil {
		return HangupDecision{}, err
	}
	if failed == 1 {
		owner, err := ownerOf(ctx, tx, callID)
		if err != nil {
			return HangupDecision{}, err
		}
		if err := appendCallEvent(ctx, tx, CallEvent{
			CallID: callID, OwnerUserID: owner, Kind: "call.failed", State: "failed",
			ReasonCode: HangupReasonCode, Provider: "internal", OccurredAt: ts,
		}); err != nil {
			return HangupDecision{}, err
		}
	} else {
		ending, err := execRows(ctx, tx, `UPDATE calls SET status = 'ending', outcome_reason_code = ?, updated_at = ?
			WHERE id = ? AND status IN ('initiating', 'ringing', 'active') AND ended_at IS NULL`,
			HangupReasonCode, ts, callID)
		if err != nil {
			return HangupDecision{}, err
		}
		if ending == 1 {
			owner, err := ownerOf(ctx, tx, callID)
			if err != nil {
				return HangupDecision{}, err
			}
			if err := appendCallEvent(ctx, tx, CallEvent{
				CallID: callID, OwnerUserID: owner, Kind: "call.ending", State: "ending",
				ReasonCode: HangupReasonCode, Provider: "internal", OccurredAt: ts,
			}); err != nil {
				return HangupDecision{}, err
			}
		}
	}

	leg, status, err := readCallLeg(ctx, tx, callID)
	if err != nil {
		return HangupDecision{}, err
	}
	var outcome sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT outcome FROM calls WHERE id = ?`, callID).Scan(&outcome); err != nil {
		return HangupDecision{}, fmt.Errorf("read call outcome: %w", err)
	}
	d := HangupDecision{Status: status, Outcome: outcome.String, Leg: leg}
	switch {
	case status == "failed" && failed == 1:
		d.Action = HangupActionFailed
	case status == "ending" && leg.ControlID != "":
		d.Action = HangupActionSendLeg
	case status == "ending":
		d.Action = HangupActionAwaitingDial
	default:
		d.Action = HangupActionNone
		d.Leg = LiveCallLeg{}
	}

	if err := insertAudit(ctx, tx, audit); err != nil {
		return HangupDecision{}, fmt.Errorf("audit hangup: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return HangupDecision{}, fmt.Errorf("commit request hangup: %w", err)
	}
	return d, nil
}

// execRows runs one conditional update and returns how many rows it changed.
func execRows(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("conditional update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("conditional update rows: %w", err)
	}
	return n, nil
}

// ownerOf reads a call's owner inside a transaction.
func ownerOf(ctx context.Context, tx *sql.Tx, callID string) (string, error) {
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM calls WHERE id = ?`, callID).Scan(&owner); err != nil {
		return "", fmt.Errorf("read call owner: %w", err)
	}
	return owner, nil
}

// CallOwner returns the owner of a call, for authorization before a mutation.
// It reads no other column, so a caller can check ownership without the call's
// state. A missing call is ErrCallNotFound.
func (s *Store) CallOwner(ctx context.Context, callID string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner_user_id FROM calls WHERE id = ?`, callID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrCallNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read call owner: %w", err)
	}
	return owner, nil
}

// rowQuerier is the part of *sql.DB and *sql.Tx that readCallLeg needs.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readCallLeg reads a call's leg and its status by call id. It is internal,
// so it does not filter by owner. A missing call is ErrCallNotFound.
func readCallLeg(ctx context.Context, q rowQuerier, callID string) (LiveCallLeg, string, error) {
	var leg LiveCallLeg
	var status string
	var control sql.NullString
	err := q.QueryRowContext(ctx, `SELECT c.id, c.owner_user_id, c.telephony_profile_id, tp.owner_user_id,
		c.provider_call_control_id, c.status
		FROM calls c JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id
		WHERE c.id = ?`, callID).Scan(&leg.ID, &leg.OwnerUserID, &leg.TelephonyProfileID,
		&leg.TelephonyOwnerUserID, &control, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveCallLeg{}, "", ErrCallNotFound
	}
	if err != nil {
		return LiveCallLeg{}, "", fmt.Errorf("read call leg: %w", err)
	}
	leg.ControlID = control.String
	return leg, status, nil
}
