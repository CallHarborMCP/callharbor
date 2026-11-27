package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCallIdempotencyConflict reports that the owner already has a call with the
// same idempotency key. The caller compares the stored request with the new one.
var ErrCallIdempotencyConflict = errors.New("storage: call idempotency key already used")

// ErrCallNotClaimable reports that a conditional status transition found the
// call in a state that does not allow it. Another worker or a webhook got there
// first, so the caller must not dial.
var ErrCallNotClaimable = errors.New("storage: call is not in the expected state")

// CallInsert is one new outbound call and its brief, written together.
type CallInsert struct {
	ID                 string
	OwnerUserID        string
	TelephonyProfileID string
	AIProfileID        string
	IdempotencyKey     string
	ToNumber           string
	RecordRequested    bool
	MaxDurationSeconds int
	BriefJSON          string
	BriefSHA256        string
	BriefVersion       int
	CreatedAt          time.Time
	// Limits are checked in the same transaction as the insert. A refused call
	// writes nothing.
	Limits CallLimits
	// Cost is the pre-dial estimate or unavailable marker recorded with the call.
	// A zero CostStatus records no cost columns.
	Cost CallCost
}

// CallCost is the cost record written with a new call. Status is one of
// known, estimated or unavailable. Amount and currency are set only when a
// figure exists.
type CallCost struct {
	Status       string
	AmountMicros *int64
	Currency     string
}

// InsertCall writes the call row, its immutable brief, and the queued event in
// one transaction. It returns ErrCallIdempotencyConflict when the owner's key is
// already used, and a limit error when a concurrency or rate limit refuses the
// call. Nothing is dialed here, so a failed write leaves no provider side
// effect.
func (s *Store) InsertCall(ctx context.Context, c CallInsert) error {
	if err := validateCallInsert(c); err != nil {
		return err
	}
	ts := c.CreatedAt.UTC().Format(storageTime)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin call insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkCallLimits(ctx, tx, c.OwnerUserID, c.CreatedAt, c.Limits); err != nil {
		return err
	}

	record := 0
	if c.RecordRequested {
		record = 1
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO calls
		(id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number,
		 status, record_requested, record_effective, max_duration_seconds, created_at, updated_at,
		 cost_status, cost_amount_micros, cost_currency)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, 0, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OwnerUserID, c.TelephonyProfileID, c.AIProfileID, c.IdempotencyKey, c.ToNumber,
		record, c.MaxDurationSeconds, ts, ts,
		nullableString(c.Cost.Status), nullableInt(c.Cost.AmountMicros), nullableString(c.Cost.Currency))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrCallIdempotencyConflict
		}
		return fmt.Errorf("insert call: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO call_briefs
		(call_id, owner_user_id, version, content_json, content_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.OwnerUserID, c.BriefVersion, c.BriefJSON, c.BriefSHA256, ts); err != nil {
		return fmt.Errorf("insert call brief: %w", err)
	}
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: c.ID, OwnerUserID: c.OwnerUserID, Kind: "call.queued", State: "queued",
		Provider: "internal", OccurredAt: ts,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit call insert: %w", err)
	}
	return nil
}

// TimeLayout is the timestamp layout of every row this package writes. Callers
// outside the package format timestamps with it, so they match the stored form.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// storageTime is TimeLayout under the name this package uses internally.
const storageTime = TimeLayout

func validateCallInsert(c CallInsert) error {
	switch {
	case c.ID == "", c.OwnerUserID == "", c.TelephonyProfileID == "", c.AIProfileID == "",
		c.IdempotencyKey == "", c.ToNumber == "", c.BriefJSON == "", c.BriefSHA256 == "":
		return errors.New("storage: insert call: required field is empty")
	case c.BriefVersion < 1:
		return errors.New("storage: insert call: brief version must be positive")
	case c.MaxDurationSeconds < 1 || c.MaxDurationSeconds > 7200:
		return errors.New("storage: insert call: max duration out of range")
	case c.CreatedAt.IsZero():
		return errors.New("storage: insert call: created time is zero")
	}
	return nil
}

// CallEvent is one append-only lifecycle record. Seq is assigned by appendCallEvent.
type CallEvent struct {
	CallID          string
	OwnerUserID     string
	Kind            string
	State           string
	ReasonCode      string
	Provider        string
	ProviderEventID string
	CorrelationID   string
	PayloadJSON     string
	OccurredAt      string
}

// appendCallEvent writes the next event for a call. Seq is one more than the
// current maximum, computed inside the same transaction that writes the row.
func appendCallEvent(ctx context.Context, tx *sql.Tx, e CallEvent) error {
	payload := e.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	now := time.Now().UTC().Format(storageTime)
	_, err := tx.ExecContext(ctx, `INSERT INTO call_events
		(call_id, owner_user_id, seq, kind, state, reason_code, provider, provider_event_id,
		 correlation_id, payload_json, occurred_at, recorded_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM call_events WHERE call_id = ?),
		        ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.CallID, e.OwnerUserID, e.CallID,
		e.Kind, nullableString(e.State), nullableString(e.ReasonCode), nullableString(e.Provider),
		nullableString(e.ProviderEventID), nullableString(e.CorrelationID), payload,
		e.OccurredAt, now)
	if err != nil {
		return fmt.Errorf("append call event: %w", err)
	}
	return nil
}

// GetCallByIdempotencyKey returns the owner's call that used the key. It is
// owner-scoped, so another user's key never resolves.
func (s *Store) GetCallByIdempotencyKey(ctx context.Context, ownerUserID, key string) (Call, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+callReadColumns+` FROM calls c
		WHERE c.owner_user_id = ? AND c.idempotency_key = ?`, ownerUserID, key)
	c, err := scanCall(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrCallNotFound
	}
	return c, err
}

// CallBrief is the stored immutable brief of one call.
type CallBrief struct {
	Version     int
	ContentJSON string
	SHA256      string
}

// GetCallBrief returns the brief for a call the owner holds.
func (s *Store) GetCallBrief(ctx context.Context, ownerUserID, callID string) (CallBrief, error) {
	var b CallBrief
	err := s.db.QueryRowContext(ctx, `SELECT version, content_json, content_sha256 FROM call_briefs
		WHERE call_id = ? AND owner_user_id = ?`, callID, ownerUserID).Scan(&b.Version, &b.ContentJSON, &b.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return CallBrief{}, ErrCallNotFound
	}
	if err != nil {
		return CallBrief{}, fmt.Errorf("read call brief: %w", err)
	}
	return b, nil
}

// ClaimQueuedCall moves a queued call to initiating, the only state from which
// a dial may be sent. It is a conditional update, so two workers can never both
// claim the same call. It returns ErrCallNotClaimable when the call is not queued.
func (s *Store) ClaimQueuedCall(ctx context.Context, callID string, now time.Time) error {
	return s.transitionCall(ctx, callID, "queued", "initiating", "call.initiating", "", "", now)
}

// MarkCallFailed moves a call that is not yet terminal to failed with a fixed
// reason code and records the event. It is used for dial refusals and for calls
// left in initiating across a restart. It never redials.
func (s *Store) MarkCallFailed(ctx context.Context, callID, reasonCode string, now time.Time) error {
	if reasonCode == "" {
		return errors.New("storage: mark call failed: empty reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark failed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	ts := now.UTC().Format(storageTime)
	res, err := tx.ExecContext(ctx, `UPDATE calls SET status = 'failed', outcome_reason_code = ?,
		ended_at = ?, updated_at = ?
		WHERE id = ? AND ended_at IS NULL
		  AND (status IN ('queued', 'initiating')
		       OR (status = 'ending' AND provider_call_control_id IS NULL))`,
		reasonCode, ts, ts, callID)
	if err != nil {
		return fmt.Errorf("mark call failed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark call failed rows: %w", err)
	}
	if n == 0 {
		return ErrCallNotClaimable
	}
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM calls WHERE id = ?`, callID).Scan(&owner); err != nil {
		return fmt.Errorf("read call owner: %w", err)
	}
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: callID, OwnerUserID: owner, Kind: CallEventFailed, State: "failed",
		ReasonCode: reasonCode, Provider: "internal", OccurredAt: ts,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordProviderCallControlID stores the Telnyx call_control_id returned by the
// dial command. The column is unique, so an id can belong to one call only.
// It is written while the call is initiating, or ending because a hangup was
// requested during the dial. A repeat with the same value is not an error.
//
// It returns the call's leg and status after the write, so a caller that
// finds the call ending can send the hangup for the id it just recorded.
func (s *Store) RecordProviderCallControlID(ctx context.Context, callID, ownerUserID, controlID string, now time.Time) (LiveCallLeg, string, error) {
	if controlID == "" {
		return LiveCallLeg{}, "", errors.New("storage: empty provider call control id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LiveCallLeg{}, "", fmt.Errorf("begin record control id: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(storageTime)
	// Only a call with no control id yet is written. A repeat with the same
	// value matches no row, so it appends no event and is not an error.
	res, err := tx.ExecContext(ctx, `UPDATE calls SET provider_call_control_id = ?, updated_at = ?
		WHERE id = ? AND owner_user_id = ? AND status IN ('initiating', 'ending')
		  AND provider_call_control_id IS NULL`,
		controlID, ts, callID, ownerUserID)
	if err != nil {
		if isUniqueViolation(err) {
			return LiveCallLeg{}, "", ErrCallNotClaimable
		}
		return LiveCallLeg{}, "", fmt.Errorf("record control id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return LiveCallLeg{}, "", fmt.Errorf("record control id rows: %w", err)
	}
	if n == 0 {
		var existing sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT provider_call_control_id FROM calls WHERE id = ? AND owner_user_id = ?`,
			callID, ownerUserID).Scan(&existing)
		if err == nil && existing.String == controlID {
			leg, status, err := readCallLeg(ctx, tx, callID)
			if err != nil {
				return LiveCallLeg{}, "", err
			}
			return leg, status, tx.Commit()
		}
		return LiveCallLeg{}, "", ErrCallNotClaimable
	}
	leg, status, err := readCallLeg(ctx, tx, callID)
	if err != nil {
		return LiveCallLeg{}, "", err
	}
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: callID, OwnerUserID: ownerUserID, Kind: "call.dial_accepted", State: status,
		Provider: "telnyx", OccurredAt: ts,
	}); err != nil {
		return LiveCallLeg{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return LiveCallLeg{}, "", fmt.Errorf("commit record control id: %w", err)
	}
	return leg, status, nil
}

// transitionCall is the single conditional status change. The WHERE clause
// names the expected state, so a stale caller cannot overwrite a later state.
func (s *Store) transitionCall(ctx context.Context, callID, from, to, kind, reason, provider string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin call transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now.UTC().Format(storageTime)
	res, err := tx.ExecContext(ctx, `UPDATE calls SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`, to, ts, callID, from)
	if err != nil {
		return fmt.Errorf("transition call: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition call rows: %w", err)
	}
	if n == 0 {
		return ErrCallNotClaimable
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM calls WHERE id = ?`, callID).Scan(&owner); err != nil {
		return fmt.Errorf("read call owner: %w", err)
	}
	if err := appendCallEvent(ctx, tx, CallEvent{
		CallID: callID, OwnerUserID: owner, Kind: kind, State: to,
		ReasonCode: reason, Provider: provider, OccurredAt: ts,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// isUniqueViolation reports a SQLite UNIQUE or PRIMARY KEY constraint failure,
// matched the same way identity.go matches it.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// QueuedCallRef is a call the dial worker may claim. It is not owner-scoped:
// the worker is internal and must see every owner's queued calls.
type QueuedCallRef struct {
	ID                 string
	OwnerUserID        string
	TelephonyProfileID string
	AIProfileID        string
	ToNumber           string
	RecordRequested    bool
	MaxDurationSeconds int
	CreatedAt          string
}

// ListQueuedCalls returns up to limit queued calls, oldest first. Calls are
// only read here. Claiming one is a separate conditional update, so two
// workers can never both dial the same call.
func (s *Store) ListQueuedCalls(ctx context.Context, limit int) ([]QueuedCallRef, error) {
	if limit < 1 {
		return nil, errors.New("storage: list queued calls: limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner_user_id, telephony_profile_id, ai_profile_id,
		to_number, record_requested, max_duration_seconds, created_at
		FROM calls WHERE status = 'queued' AND ended_at IS NULL
		ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list queued calls: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []QueuedCallRef
	for rows.Next() {
		var c QueuedCallRef
		var record int
		if err := rows.Scan(&c.ID, &c.OwnerUserID, &c.TelephonyProfileID, &c.AIProfileID,
			&c.ToNumber, &record, &c.MaxDurationSeconds, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan queued call: %w", err)
		}
		c.RecordRequested = record == 1
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queued calls: %w", err)
	}
	return out, nil
}

// LiveCallLeg is one call with a provider leg that has not ended. Shutdown
// hangs these up. The owner is included so the caller's key can be resolved.
type LiveCallLeg struct {
	ID          string
	OwnerUserID string
	// TelephonyProfileID is the profile the leg was dialed on. It can belong to
	// someone other than the call's owner, when a grantee placed the call.
	TelephonyProfileID string
	// TelephonyOwnerUserID owns that profile. Its key ends the leg, so a hangup
	// resolves the profile by this id, never by the call owner.
	TelephonyOwnerUserID string
	ControlID            string
}

// ListLiveCallLegs returns calls that Telnyx may still be carrying: a recorded
// control id and a status before a terminal one.
func (s *Store) ListLiveCallLegs(ctx context.Context) ([]LiveCallLeg, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.owner_user_id, c.telephony_profile_id, tp.owner_user_id, c.provider_call_control_id
		FROM calls c
		JOIN telephony_profiles tp ON tp.id = c.telephony_profile_id
		WHERE c.status IN ('initiating', 'ringing', 'active', 'ending')
		  AND c.provider_call_control_id IS NOT NULL AND c.ended_at IS NULL
		ORDER BY c.created_at, c.id`)
	if err != nil {
		return nil, fmt.Errorf("list live call legs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LiveCallLeg
	for rows.Next() {
		var l LiveCallLeg
		if err := rows.Scan(&l.ID, &l.OwnerUserID, &l.TelephonyProfileID, &l.TelephonyOwnerUserID, &l.ControlID); err != nil {
			return nil, fmt.Errorf("scan live call leg: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live call legs: %w", err)
	}
	return out, nil
}

// ListUnconfirmedDials returns calls in initiating, or ending after a hangup
// requested during the dial, with no provider control id. After a restart these
// may have been dialed without the response being recorded, so they must never
// be redialed. Recovery fails them with dial_unconfirmed, which also releases
// their concurrency slot. This is startup-only: it must not be used while a dial
// may still be in flight in this process.
func (s *Store) ListUnconfirmedDials(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM calls
		WHERE status IN ('initiating', 'ending') AND provider_call_control_id IS NULL AND ended_at IS NULL
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list unconfirmed dials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan unconfirmed call: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unconfirmed calls: %w", err)
	}
	return ids, nil
}
