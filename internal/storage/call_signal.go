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
// the minimum the call state machine needs to rebuild its position.
type CallSignalState struct {
	ID                    string
	OwnerUserID           string
	Status                string
	Outcome               string
	ProviderCallControlID string
}

// ReadCallForSignal reads a call's lifecycle state inside tx. It runs in the
// webhook's receipt transaction, so the state it reads is the state it writes.
func ReadCallForSignal(ctx context.Context, tx *sql.Tx, callID string) (CallSignalState, error) {
	var c CallSignalState
	var control, outcome sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, owner_user_id, status, outcome, provider_call_control_id
		FROM calls WHERE id = ?`, callID).Scan(&c.ID, &c.OwnerUserID, &c.Status, &outcome, &control)
	if errors.Is(err, sql.ErrNoRows) {
		return CallSignalState{}, ErrCallNotFound
	}
	if err != nil {
		return CallSignalState{}, fmt.Errorf("read call for signal: %w", err)
	}
	c.Outcome = outcome.String
	c.ProviderCallControlID = control.String
	return c, nil
}

// WriteCallSignal persists the result of applying one signal, inside the same
// tx. It sets the status, records the provider control id when first seen,
// sets ended_at on a terminal state, and appends one call_events row. It
// requires the status it read (from), so a stale write cannot overwrite a
// later one.
func WriteCallSignal(ctx context.Context, tx *sql.Tx, from CallSignalState, to, kind, reason, providerEventID, controlID string, now time.Time) error {
	ts := now.UTC().Format(storageTime)
	ended := sql.NullString{}
	if to == "ended" || to == "failed" {
		ended = sql.NullString{String: ts, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `UPDATE calls SET status = ?, updated_at = ?,
		provider_call_control_id = COALESCE(provider_call_control_id, NULLIF(?, '')),
		ended_at = COALESCE(ended_at, ?)
		WHERE id = ? AND status = ?`, to, ts, controlID, ended, from.ID, from.Status)
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
