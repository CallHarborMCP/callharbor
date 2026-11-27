package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReconcileCall is a call the reconciliation sweep may act on. It is not
// owner-scoped: the sweep is internal and must see every owner's calls.
type ReconcileCall struct {
	ID                   string
	OwnerUserID          string
	TelephonyProfileID   string
	TelephonyOwnerUserID string
	ControlID            string
	Status               string
	// Attempts is the number of hangups CallHarbor recorded as sent for the call.
	Attempts           int
	MaxDurationSeconds int
	CreatedAt          time.Time
	// LastHangupAt is the last recorded hangup send. Until one is recorded it is
	// the call's last update, so the first retry is measured from when the call
	// began ending.
	LastHangupAt time.Time
}

// Leg returns the fields a hangup needs to resolve the owner's key.
func (c ReconcileCall) Leg() LiveCallLeg {
	return LiveCallLeg{ID: c.ID, OwnerUserID: c.OwnerUserID, TelephonyProfileID: c.TelephonyProfileID,
		TelephonyOwnerUserID: c.TelephonyOwnerUserID, ControlID: c.ControlID}
}

const reconcileSelect = `SELECT c.id, c.owner_user_id, c.telephony_profile_id, tp.owner_user_id,
	c.provider_call_control_id, c.status, c.hangup_attempts, c.max_duration_seconds, c.created_at,
	COALESCE(c.hangup_requested_at, c.updated_at)
	FROM calls c JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id`

// ListEndingForReconcile returns ending calls with a control id, no call.hangup
// seen, and no hangup sent since lastHangupBefore. Oldest first, at most limit.
func (s *Store) ListEndingForReconcile(ctx context.Context, lastHangupBefore time.Time, limit int) ([]ReconcileCall, error) {
	return s.queryReconcile(ctx, `WHERE c.status = 'ending' AND c.ended_at IS NULL
		AND c.provider_call_control_id IS NOT NULL
		AND CAST(strftime('%s', COALESCE(c.hangup_requested_at, c.updated_at)) AS INTEGER) < ?
		AND NOT EXISTS (SELECT 1 FROM call_events e WHERE e.call_id = c.id AND e.kind = 'hangup')
		ORDER BY COALESCE(c.hangup_requested_at, c.updated_at), c.id LIMIT ?`,
		lastHangupBefore.Unix(), limit)
}

// ListOverdueCalls returns live calls past their watchdog deadline. A ringing or
// active call is overdue when created_at + max_duration + slackSeconds has
// passed. An initiating call with a control id is overdue when its control id
// was recorded more than slackSeconds ago. slackSeconds is the ring timeout plus
// the grace period.
func (s *Store) ListOverdueCalls(ctx context.Context, now time.Time, slackSeconds, limit int) ([]ReconcileCall, error) {
	return s.queryReconcile(ctx, `WHERE c.ended_at IS NULL AND c.provider_call_control_id IS NOT NULL
		AND ((c.status IN ('ringing', 'active')
		      AND CAST(strftime('%s', c.created_at) AS INTEGER) + c.max_duration_seconds + ? <= ?)
		  OR (c.status = 'initiating'
		      AND CAST(strftime('%s', c.updated_at) AS INTEGER) + ? <= ?))
		ORDER BY c.created_at, c.id LIMIT ?`,
		slackSeconds, now.Unix(), slackSeconds, now.Unix(), limit)
}

// ListOrphanedTerminal returns terminal calls that have a control id, no
// call.hangup and no reconciled end, and fewer than maxAttempts hangups sent,
// with the last send before lastHangupBefore. These legs may still be carried
// by Telnyx, so the sweep retries the hangup a bounded number of times.
func (s *Store) ListOrphanedTerminal(ctx context.Context, lastHangupBefore time.Time, maxAttempts, limit int) ([]ReconcileCall, error) {
	return s.queryReconcile(ctx, `WHERE c.status IN ('ended', 'failed') AND c.provider_call_control_id IS NOT NULL
		AND c.hangup_attempts < ?
		AND CAST(strftime('%s', COALESCE(c.hangup_requested_at, c.updated_at)) AS INTEGER) < ?
		AND NOT EXISTS (SELECT 1 FROM call_events e WHERE e.call_id = c.id AND e.kind IN ('hangup', 'call.reconciled'))
		ORDER BY COALESCE(c.hangup_requested_at, c.updated_at), c.id LIMIT ?`,
		maxAttempts, lastHangupBefore.Unix(), limit)
}

// queryReconcile runs one bounded reconciliation query. The where clause and
// its arguments end with the limit.
func (s *Store) queryReconcile(ctx context.Context, where string, args ...any) ([]ReconcileCall, error) {
	if limit, ok := args[len(args)-1].(int); !ok || limit < 1 {
		return nil, errors.New("storage: reconcile query: limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, reconcileSelect+" "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("reconcile query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReconcileCall
	for rows.Next() {
		var c ReconcileCall
		var control sql.NullString
		var created, last string
		if err := rows.Scan(&c.ID, &c.OwnerUserID, &c.TelephonyProfileID, &c.TelephonyOwnerUserID,
			&control, &c.Status, &c.Attempts, &c.MaxDurationSeconds, &created, &last); err != nil {
			return nil, fmt.Errorf("scan reconcile call: %w", err)
		}
		c.ControlID = control.String
		if c.CreatedAt, err = time.Parse(storageTime, created); err != nil {
			return nil, fmt.Errorf("parse created_at for %s: %w", c.ID, err)
		}
		if c.LastHangupAt, err = time.Parse(storageTime, last); err != nil {
			return nil, fmt.Errorf("parse last hangup for %s: %w", c.ID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reconcile calls: %w", err)
	}
	return out, nil
}

// RecordHangupSent records that CallHarbor sent a hangup for a call's leg: it
// sets hangup_requested_at to now and counts the attempt. It is called after
// every send, whether or not the send succeeded, so the backoff is measured from
// the last attempt.
func (s *Store) RecordHangupSent(ctx context.Context, callID string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE calls SET hangup_requested_at = ?, hangup_attempts = hangup_attempts + 1
		WHERE id = ?`, now.UTC().Format(storageTime), callID)
	if err != nil {
		return fmt.Errorf("record hangup sent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record hangup sent rows: %w", err)
	}
	if n == 0 {
		return ErrCallNotFound
	}
	return nil
}

// MarkReconcileEnded moves an ending call to ended because the sweep found the
// leg gone, or because its expiry bound passed. An empty reasonCode keeps the
// call's existing reason. It appends a call.reconciled event. It returns false,
// with no error, when the call is no longer ending, for example because a
// webhook ended it first.
func (s *Store) MarkReconcileEnded(ctx context.Context, callID, reasonCode string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin reconcile end: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ts := now.UTC().Format(storageTime)
	n, err := execRows(ctx, tx, `UPDATE calls SET status = 'ended', ended_at = ?, updated_at = ?,
		outcome_reason_code = COALESCE(NULLIF(?, ''), outcome_reason_code)
		WHERE id = ? AND status = 'ending' AND ended_at IS NULL`, ts, ts, reasonCode, callID)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	var owner string
	var reason sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id, outcome_reason_code FROM calls WHERE id = ?`,
		callID).Scan(&owner, &reason); err != nil {
		return false, fmt.Errorf("read reconciled call: %w", err)
	}
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: callID, OwnerUserID: owner, Kind: "call.reconciled", State: "ended",
		ReasonCode: reason.String, Provider: "internal", OccurredAt: ts,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit reconcile end: %w", err)
	}
	return true, nil
}
