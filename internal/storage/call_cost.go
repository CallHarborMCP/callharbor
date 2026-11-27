package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProviderCost is the provider's reported cost for a call, as stored. Amount
// and currency are set only when the provider reported a success whose parts
// agree on one currency. Otherwise the call's provider cost is unavailable and
// the estimate, if any, stands.
type ProviderCost struct {
	Status       string // success or error, as the provider reported it
	AmountMicros *int64
	Currency     string
	EventID      string
}

// ErrCostAlreadyRecorded reports that a different provider cost is already
// stored for the call. A redelivery of the same event is deduplicated by its
// receipt before this point, so reaching it means two distinct cost events.
var ErrCostAlreadyRecorded = errors.New("storage: provider cost already recorded")

// RecordProviderCost stores the provider's cost for a call inside tx. It writes
// only the provider cost columns: the call's status, outcome and events are not
// changed. It is idempotent for the same event id. A second, different event for
// a call that already has a provider cost is refused rather than overwritten.
func RecordProviderCost(ctx context.Context, tx *sql.Tx, callID string, cost ProviderCost) error {
	if cost.Status != "success" && cost.Status != "error" {
		return fmt.Errorf("storage: provider cost status %q", cost.Status)
	}
	var existing sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT provider_cost_event_id FROM calls WHERE id = ?`, callID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCallNotFound
	}
	if err != nil {
		return fmt.Errorf("read provider cost: %w", err)
	}
	if existing.Valid {
		if existing.String == cost.EventID {
			return nil
		}
		return ErrCostAlreadyRecorded
	}
	_, err = tx.ExecContext(ctx, `UPDATE calls SET provider_cost_status = ?, provider_cost_amount_micros = ?,
		provider_cost_currency = ?, provider_cost_event_id = ? WHERE id = ?`,
		cost.Status, nullableInt(cost.AmountMicros), nullableString(cost.Currency), cost.EventID, callID)
	if err != nil {
		return fmt.Errorf("record provider cost: %w", err)
	}
	return nil
}
