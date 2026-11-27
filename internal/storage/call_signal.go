package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrCallForeignProvider reports a verified signal whose provider call control
// id is not the one this call was dialed with. Nothing is written.
var ErrCallForeignProvider = errors.New("storage: signal for a different provider call")

// CallSignalState is the stored lifecycle state a signal is applied to. It is
// the minimum the call state machine needs to rebuild its position, plus the
// leg's profile so a hangup can be sent with the owner's key.
type CallSignalState struct {
	ID                   string
	OwnerUserID          string
	TelephonyProfileID   string
	TelephonyOwnerUserID string
	Status               string
	Outcome              string
	ReasonCode           string
	// ProviderCallControlID is the leg's control id, or empty when none is recorded.
	ProviderCallControlID string
	// HangupSeen reports whether a call.hangup for this call has been recorded.
	// It is the marker that stops orphan hangups for the leg.
	HangupSeen bool
}

// ReadCallForSignal reads a call's lifecycle state inside tx. It runs in the
// webhook's receipt transaction, so the state it reads is the state it writes.
func ReadCallForSignal(ctx context.Context, tx *sql.Tx, callID string) (CallSignalState, error) {
	var c CallSignalState
	var control, outcome, reason sql.NullString
	var hangupSeen int
	err := tx.QueryRowContext(ctx, `SELECT c.id, c.owner_user_id, c.telephony_profile_id, tp.owner_user_id,
		c.status, c.outcome, c.outcome_reason_code, c.provider_call_control_id,
		EXISTS (SELECT 1 FROM call_events e WHERE e.call_id = c.id AND e.kind = 'hangup')
		FROM calls c JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id
		WHERE c.id = ?`, callID).Scan(&c.ID, &c.OwnerUserID, &c.TelephonyProfileID, &c.TelephonyOwnerUserID,
		&c.Status, &outcome, &reason, &control, &hangupSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return CallSignalState{}, ErrCallNotFound
	}
	if err != nil {
		return CallSignalState{}, fmt.Errorf("read call for signal: %w", err)
	}
	c.Outcome = outcome.String
	c.ReasonCode = reason.String
	c.ProviderCallControlID = control.String
	c.HangupSeen = hangupSeen == 1
	return c, nil
}

// RecordHangupSeen appends a hangup event for a call whose state the hangup did
// not change, such as a call that already ended. Telnyx's hangup is then still
// on record, so no later orphan hangup is sent for the leg. It runs inside the
// webhook's receipt transaction.
func RecordHangupSeen(ctx context.Context, tx *sql.Tx, from CallSignalState, providerEventID string, now time.Time) error {
	return appendCallEvent(ctx, tx, CallEvent{
		CallID: from.ID, OwnerUserID: from.OwnerUserID, Kind: CallEventHangup, State: from.Status,
		Provider: "telnyx", ProviderEventID: providerEventID, OccurredAt: now.UTC().Format(storageTime),
	})
}

// RecordCallControlID records the provider control id of a call that has none,
// inside tx. A verified event can name the leg of a call whose status does not
// change (for example a failed call whose dial was never confirmed), so the leg
// must still be traceable for a later hangup. A control id already taken by
// another call is ErrCallForeignProvider.
func RecordCallControlID(ctx context.Context, tx *sql.Tx, callID, controlID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE calls SET provider_call_control_id = ?, updated_at = ?
		WHERE id = ? AND provider_call_control_id IS NULL`,
		controlID, now.UTC().Format(storageTime), callID)
	if isUniqueViolation(err) {
		return ErrCallForeignProvider
	}
	if err != nil {
		return fmt.Errorf("record call control id: %w", err)
	}
	return nil
}

// WriteCallSignal persists the result of applying one signal, inside the same
// tx. It sets the status, records the provider control id when first seen,
// records the reason code when the signal carries one, sets ended_at on a
// terminal state, and appends one call_events row. It requires the status it
// read (from), so a stale write cannot overwrite a later one.
//
// The reason is kept so the state machine can rebuild it: a hangup that arrives
// before answered is only completed by a later answered signal when its reason
// is known to be normal clearing or the time limit.
func WriteCallSignal(ctx context.Context, tx *sql.Tx, from CallSignalState, to, kind, reason, providerEventID, controlID string, now time.Time) error {
	ts := now.UTC().Format(storageTime)
	ended := sql.NullString{}
	if to == "ended" || to == "failed" {
		ended = sql.NullString{String: ts, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `UPDATE calls SET status = ?, updated_at = ?,
		provider_call_control_id = COALESCE(provider_call_control_id, NULLIF(?, '')),
		ended_at = COALESCE(ended_at, ?),
		outcome_reason_code = COALESCE(NULLIF(?, ''), outcome_reason_code)
		WHERE id = ? AND status = ?`, to, ts, controlID, ended, reason, from.ID, from.Status)
	if err != nil {
		return fmt.Errorf("write call signal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("write call signal rows: %w", err)
	}
	if n == 0 {
		return ErrCallNotClaimable
	}
	return appendCallEvent(ctx, tx, CallEvent{
		CallID: from.ID, OwnerUserID: from.OwnerUserID, Kind: kind, State: to,
		ReasonCode: reason, Provider: "telnyx", ProviderEventID: providerEventID, OccurredAt: ts,
	})
}
