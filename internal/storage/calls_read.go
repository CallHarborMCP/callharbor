package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
)

// ErrCallNotFound reports a missing call or one owned by another user.
var ErrCallNotFound = errors.New("storage: call not found")

// Call is the owner-scoped read model for one call. Idempotency keys and
// provider control identifiers are deliberately omitted.
type Call struct {
	ID                 string
	OwnerUserID        string
	TelephonyProfileID string
	AIProfileID        string
	ToNumber           string
	Status             string
	Outcome            string
	OutcomeReasonCode  string
	RecordRequested    bool
	RecordEffective    bool
	MaxDurationSeconds int
	CreatedAt          string
	UpdatedAt          string
	EndedAt            string
	// HasTranscript reports whether at least one final transcript turn exists.
	// Partial-only transcripts do not count.
	HasTranscript bool
	// RecordingStatus is the stored recording status, or "" when none exists.
	RecordingStatus string
	// Estimate is the pre-dial cost, as recorded at make_call. Its status is
	// estimated or unavailable; a zero value means none was recorded.
	Estimate CostFigure
	// Provider is the cost the provider reported through call.cost. Its status
	// is success or error; amount is set only for a known success.
	Provider CostFigure
	// OutcomeReasonCodes, OutcomeEvidence, OutcomeSource and OutcomeAssessedAt
	// are set together with Outcome, or all empty while it is unset.
	OutcomeReasonCodes []string
	OutcomeEvidence    []outcome.Evidence
	OutcomeSource      string
	OutcomeAssessedAt  string
}

// CostFigure is one stored cost figure. Amount is nil when none is known.
type CostFigure struct {
	Status       string
	AmountMicros *int64
	Currency     string
}

// CallCursor is the keyset position after the last call of a page. Calls are
// ordered by (CreatedAt DESC, ID DESC).
type CallCursor struct {
	CreatedAt string
	ID        string
}

const callReadColumns = `c.id, c.owner_user_id, c.telephony_profile_id, c.ai_profile_id, c.to_number,
	c.status, c.outcome, c.outcome_reason_code, c.record_requested, c.record_effective,
	c.max_duration_seconds, c.created_at, c.updated_at, c.ended_at,
	EXISTS (SELECT 1 FROM transcript_turns t WHERE t.call_id = c.id AND t.owner_user_id = c.owner_user_id AND t.finality = 'final'),
	(SELECT r.status FROM recordings r WHERE r.call_id = c.id AND r.owner_user_id = c.owner_user_id),
	c.cost_status, c.cost_amount_micros, c.cost_currency,
	c.provider_cost_status, c.provider_cost_amount_micros, c.provider_cost_currency,
	c.outcome_reason_codes, c.outcome_evidence, c.outcome_source, c.outcome_assessed_at`

// ListCalls returns up to limit of the owner's calls, newest first. An empty
// status matches every status; otherwise the caller must have validated it
// against the call state set. When after is non-nil, only calls strictly after
// that keyset position are returned.
func (s *Store) ListCalls(ctx context.Context, ownerUserID, status string, after *CallCursor, limit int) ([]Call, error) {
	if limit < 1 {
		return nil, fmt.Errorf("storage: list calls: limit must be positive")
	}
	query := `SELECT ` + callReadColumns + ` FROM calls c WHERE c.owner_user_id = ?`
	args := []any{ownerUserID}
	if status != "" {
		query += ` AND c.status = ?`
		args = append(args, status)
	}
	if after != nil {
		query += ` AND (c.created_at < ? OR (c.created_at = ? AND c.id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query += ` ORDER BY c.created_at DESC, c.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list calls: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Call{}
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list calls: %w", err)
	}
	return out, nil
}

// GetCall returns a call only when it belongs to ownerUserID.
func (s *Store) GetCall(ctx context.Context, ownerUserID, id string) (Call, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+callReadColumns+` FROM calls c
		WHERE c.id = ? AND c.owner_user_id = ?`, id, ownerUserID)
	c, err := scanCall(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrCallNotFound
	}
	return c, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCall(r rowScanner) (Call, error) {
	var (
		c                                       Call
		outcomeCol, reason, endedAt, recStatus  sql.NullString
		recordRequested, recordEffective        int
		hasTranscript                           int
		estStatus, estCur, provStatus, provCur  sql.NullString
		estAmt, provAmt                         sql.NullInt64
		reasonCodes, evidence, source, assessed sql.NullString
	)
	err := r.Scan(&c.ID, &c.OwnerUserID, &c.TelephonyProfileID, &c.AIProfileID, &c.ToNumber,
		&c.Status, &outcomeCol, &reason, &recordRequested, &recordEffective,
		&c.MaxDurationSeconds, &c.CreatedAt, &c.UpdatedAt, &endedAt,
		&hasTranscript, &recStatus,
		&estStatus, &estAmt, &estCur, &provStatus, &provAmt, &provCur,
		&reasonCodes, &evidence, &source, &assessed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Call{}, err
		}
		return Call{}, fmt.Errorf("storage: scan call: %w", err)
	}
	if err := decodeOutcomeFields(&c, reasonCodes, evidence, source, assessed); err != nil {
		return Call{}, err
	}
	c.Outcome = outcomeCol.String
	c.OutcomeReasonCode = reason.String
	c.EndedAt = endedAt.String
	c.RecordingStatus = recStatus.String
	c.RecordRequested = recordRequested == 1
	c.RecordEffective = recordEffective == 1
	c.HasTranscript = hasTranscript == 1
	c.Estimate = costFigure(estStatus, estAmt, estCur)
	c.Provider = costFigure(provStatus, provAmt, provCur)
	return c, nil
}

// costFigure builds a stored cost figure from its nullable columns.
func costFigure(status sql.NullString, amount sql.NullInt64, currency sql.NullString) CostFigure {
	f := CostFigure{Status: status.String, Currency: currency.String}
	if amount.Valid {
		v := amount.Int64
		f.AmountMicros = &v
	}
	return f
}
