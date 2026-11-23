package storage

import (
	"context"
	"errors"
	"fmt"
)

// PurgeCall deletes all retained data for one call inside a single transaction.
// The purge marker exists only for the duration of that transaction, so the
// append-only guards are reopened for this call's rows only, and they are closed
// again atomically when the transaction commits or rolls back.
func (s *Store) PurgeCall(ctx context.Context, callID, now string) error {
	if callID == "" {
		return errors.New("purge call: empty call id")
	}
	return s.withPurgeMarker(ctx, "call", callID, "", now, []string{
		"DELETE FROM recordings WHERE call_id = ?",
		"DELETE FROM transcript_turns WHERE call_id = ?",
		"DELETE FROM call_events WHERE call_id = ?",
		"DELETE FROM call_briefs WHERE call_id = ?",
		"DELETE FROM calls WHERE id = ?",
	}, callID)
}

// PurgeAudit deletes audit events that occurred strictly before cutoff. Only rows
// older than the cutoff can be removed while the marker is present.
func (s *Store) PurgeAudit(ctx context.Context, cutoff, now string) error {
	if cutoff == "" {
		return errors.New("purge audit: empty cutoff")
	}
	return s.withPurgeMarker(ctx, "audit", cutoff, cutoff, now, []string{
		"DELETE FROM audit_events WHERE occurred_at < ?",
	}, cutoff)
}

func (s *Store) withPurgeMarker(ctx context.Context, scope, key, cutoff, now string, deletes []string, arg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin purge %s: %w", scope, err)
	}
	defer func() { _ = tx.Rollback() }()

	var cutoffArg any
	if cutoff != "" {
		cutoffArg = cutoff
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO retention_purge_markers (scope, key, cutoff_at, created_at) VALUES (?, ?, ?, ?)",
		scope, key, cutoffArg, now); err != nil {
		return fmt.Errorf("open purge marker: %w", err)
	}
	for _, stmt := range deletes {
		if _, err := tx.ExecContext(ctx, stmt, arg); err != nil {
			return fmt.Errorf("purge %s: %w", scope, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM retention_purge_markers WHERE scope = ? AND key = ?", scope, key); err != nil {
		return fmt.Errorf("close purge marker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit purge %s: %w", scope, err)
	}
	return nil
}
