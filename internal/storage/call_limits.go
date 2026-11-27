package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Errors returned by InsertCall when a limit refuses a new call. No row is
// written in either case. The service maps them to stable reason codes.
var (
	ErrOwnerConcurrencyLimit  = errors.New("storage: owner concurrent call limit reached")
	ErrGlobalConcurrencyLimit = errors.New("storage: installation concurrent call limit reached")
	ErrOwnerRateLimit         = errors.New("storage: owner call rate limit reached")
)

// CallLimits are the counts a new call is checked against. They are enforced
// inside InsertCall's transaction. The database uses _txlock=immediate, so
// two inserts cannot both read the same count and both pass. A zero field
// means that limit is not checked.
type CallLimits struct {
	OwnerMaxConcurrent  int
	GlobalMaxConcurrent int
	OwnerMaxPerHour     int
}

// nonTerminalStatuses are the statuses that still hold capacity. Queued calls
// count, so the queue itself is bounded.
const nonTerminalStatuses = `('queued', 'initiating', 'ringing', 'active', 'ending')`

// rateWindow is the trailing window for OwnerMaxPerHour.
const rateWindow = time.Hour

// checkCallLimits counts within tx and returns the first limit a new call
// would exceed. now is the new call's created time, so the window is
// deterministic in tests.
func checkCallLimits(ctx context.Context, tx *sql.Tx, owner string, now time.Time, l CallLimits) error {
	if l.OwnerMaxConcurrent > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls
			WHERE owner_user_id = ? AND status IN `+nonTerminalStatuses, owner).Scan(&n); err != nil {
			return fmt.Errorf("count owner concurrent calls: %w", err)
		}
		if n >= l.OwnerMaxConcurrent {
			return ErrOwnerConcurrencyLimit
		}
	}
	if l.GlobalMaxConcurrent > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls
			WHERE status IN `+nonTerminalStatuses).Scan(&n); err != nil {
			return fmt.Errorf("count installation concurrent calls: %w", err)
		}
		if n >= l.GlobalMaxConcurrent {
			return ErrGlobalConcurrencyLimit
		}
	}
	if l.OwnerMaxPerHour > 0 {
		since := now.Add(-rateWindow).UTC().Format(storageTime)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls
			WHERE owner_user_id = ? AND created_at >= ?`, owner, since).Scan(&n); err != nil {
			return fmt.Errorf("count owner recent calls: %w", err)
		}
		if n >= l.OwnerMaxPerHour {
			return ErrOwnerRateLimit
		}
	}
	return nil
}

// UserCallPolicy is one user's overrides. A nil numeric field inherits the
// installation value. Prefix lists are stored as JSON arrays.
type UserCallPolicy struct {
	UserID             string
	MaxConcurrentCalls *int64
	MaxCallsPerHour    *int64
	MaxDurationSeconds *int64
	MaxCostMicros      *int64
	AllowPrefixes      []string
	DenyPrefixes       []string
	UpdatedAt          string
	UpdatedBy          string
}

// GetUserCallPolicy returns the stored overrides for a user. A user with no row
// has empty overrides, not an error.
func (s *Store) GetUserCallPolicy(ctx context.Context, userID string) (UserCallPolicy, error) {
	var (
		p                        UserCallPolicy
		conc, perHour, dur, cost sql.NullInt64
		allowJSON, denyJSON      string
		updatedAt, updatedBy     sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_calls, max_calls_per_hour, max_duration_seconds,
		max_cost_micros, allow_prefixes_json, deny_prefixes_json, updated_at, updated_by
		FROM user_call_policies WHERE user_id = ?`, userID).
		Scan(&conc, &perHour, &dur, &cost, &allowJSON, &denyJSON, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return UserCallPolicy{UserID: userID}, nil
	}
	if err != nil {
		return UserCallPolicy{}, fmt.Errorf("read user call policy: %w", err)
	}
	p.UserID = userID
	p.MaxConcurrentCalls = intPtr(conc)
	p.MaxCallsPerHour = intPtr(perHour)
	p.MaxDurationSeconds = intPtr(dur)
	p.MaxCostMicros = intPtr(cost)
	if err := json.Unmarshal([]byte(allowJSON), &p.AllowPrefixes); err != nil {
		return UserCallPolicy{}, fmt.Errorf("decode allow prefixes: %w", err)
	}
	if err := json.Unmarshal([]byte(denyJSON), &p.DenyPrefixes); err != nil {
		return UserCallPolicy{}, fmt.Errorf("decode deny prefixes: %w", err)
	}
	p.UpdatedAt = updatedAt.String
	p.UpdatedBy = updatedBy.String
	return p, nil
}

// UpsertUserCallPolicy stores a user's overrides and writes the audit event in
// one transaction, so a change is never stored without its record.
func (s *Store) UpsertUserCallPolicy(ctx context.Context, p UserCallPolicy, audit AuditEvent) error {
	allow, err := json.Marshal(nonNilStrings(p.AllowPrefixes))
	if err != nil {
		return fmt.Errorf("encode allow prefixes: %w", err)
	}
	deny, err := json.Marshal(nonNilStrings(p.DenyPrefixes))
	if err != nil {
		return fmt.Errorf("encode deny prefixes: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user call policy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_call_policies
		(user_id, max_concurrent_calls, max_calls_per_hour, max_duration_seconds, max_cost_micros,
		 allow_prefixes_json, deny_prefixes_json, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
		 max_concurrent_calls = excluded.max_concurrent_calls,
		 max_calls_per_hour = excluded.max_calls_per_hour,
		 max_duration_seconds = excluded.max_duration_seconds,
		 max_cost_micros = excluded.max_cost_micros,
		 allow_prefixes_json = excluded.allow_prefixes_json,
		 deny_prefixes_json = excluded.deny_prefixes_json,
		 updated_at = excluded.updated_at,
		 updated_by = excluded.updated_by`,
		p.UserID, nullableInt(p.MaxConcurrentCalls), nullableInt(p.MaxCallsPerHour),
		nullableInt(p.MaxDurationSeconds), nullableInt(p.MaxCostMicros),
		string(allow), string(deny), p.UpdatedAt, p.UpdatedBy); err != nil {
		return fmt.Errorf("upsert user call policy: %w", err)
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return fmt.Errorf("audit user call policy: %w", err)
	}
	return tx.Commit()
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
