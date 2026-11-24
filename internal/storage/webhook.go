package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RecordWebhookOnce is the durable dedupe gate for provider webhooks. It
// inserts the receipt and runs process with the same *sql.Tx in one
// transaction. process must perform all of its database writes through tx,
// never through the Store: the Store is pinned to a single connection, which
// tx holds for its whole lifetime, so a write through the Store would wait
// forever. If the receipt already exists, process is skipped and duplicate is
// true. If process fails, the receipt and every write made through tx are
// rolled back, so a provider retry is processed again.
func (s *Store) RecordWebhookOnce(ctx context.Context, provider, eventID string, receivedAt time.Time, process func(ctx context.Context, tx *sql.Tx) error) (bool, error) {
	if provider == "" || eventID == "" {
		return false, errors.New("record webhook: empty provider or event id")
	}
	if process == nil {
		return false, errors.New("record webhook: nil process")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin webhook receipt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `INSERT INTO provider_webhook_receipts (provider, provider_event_id, received_at)
		VALUES (?, ?, ?) ON CONFLICT (provider, provider_event_id) DO NOTHING`,
		provider, eventID, receivedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("insert webhook receipt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("webhook receipt rows: %w", err)
	}
	if n == 0 {
		return true, nil
	}
	if err := process(ctx, tx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit webhook receipt: %w", err)
	}
	return false, nil
}
