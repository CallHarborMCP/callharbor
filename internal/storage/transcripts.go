package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Transcript speakers as stored. The caller of an outbound call is the callee.
const (
	SpeakerAssistant = "assistant"
	SpeakerCallee    = "callee"
	SpeakerSystem    = "system"
)

// Transcript finality. A partial revision may still change; a final revision's
// text is complete, though a later revision can still correct its playout
// evidence.
const (
	FinalityPartial = "partial"
	FinalityFinal   = "final"
)

// Timing sources. Provider timing is the provider's session timeline; local
// timing is when CallHarbor received the event, relative to the media start.
const (
	TimingProvider     = "provider"
	TimingLocalReceive = "local_receive"
)

// Call event kinds for the transcript summary. transcript.started is written
// when the media run begins, transcript.finalized once it ends. Both carry a
// JSON payload with a "quality_flags" string array.
const (
	TranscriptStartedKind   = "transcript.started"
	TranscriptFinalizedKind = "transcript.finalized"
)

// TranscriptRevision is one immutable revision of a transcript turn. Revision
// is assigned by the store. Fidelity takes the recording.Fidelity values.
type TranscriptRevision struct {
	Seq       int
	Speaker   string
	Finality  string
	StartedMS int64
	// EndedMS is nil when the end of the turn is unknown.
	EndedMS          *int64
	Text             string
	ProviderItemID   string
	Fidelity         string
	TimingSource     string
	SourceIDs        []string
	CorrelationID    string
	GeneratedMS      *int64
	PlayedMS         *int64
	PlayedIsEstimate bool
	Quality          []string
}

// TranscriptTurn is the latest revision of one turn, as read back.
type TranscriptTurn struct {
	TranscriptRevision
	Revision   int
	RecordedAt string
}

// RecordTranscript writes turn revisions and call events for one call in one
// transaction. Each revision gets the next revision number for its seq. Call
// events with a provider event id that is already stored are skipped, so a
// replayed provider event is recorded once. The call must belong to owner: the
// composite foreign key rejects a mismatched owner.
func (s *Store) RecordTranscript(ctx context.Context, callID, ownerUserID string, turns []TranscriptRevision, events []CallEvent) error {
	if callID == "" || ownerUserID == "" {
		return errors.New("storage: record transcript: empty call or owner")
	}
	if len(turns) == 0 && len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record transcript: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(storageTime)
	for _, t := range turns {
		if err := insertRevision(ctx, tx, callID, ownerUserID, t, now); err != nil {
			return err
		}
	}
	for _, e := range events {
		e.CallID, e.OwnerUserID = callID, ownerUserID
		if e.ProviderEventID != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM call_events WHERE call_id = ? AND provider_event_id = ?`,
				callID, e.ProviderEventID).Scan(&n); err != nil {
				return fmt.Errorf("check call event: %w", err)
			}
			if n > 0 {
				continue
			}
		}
		if e.OccurredAt == "" {
			e.OccurredAt = now
		}
		if err := appendCallEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record transcript: %w", err)
	}
	return nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, callID, owner string, t TranscriptRevision, now string) error {
	if t.Seq < 1 {
		return fmt.Errorf("storage: transcript seq %d must be positive", t.Seq)
	}
	sources, err := jsonStrings(t.SourceIDs)
	if err != nil {
		return err
	}
	quality, err := jsonStrings(t.Quality)
	if err != nil {
		return err
	}
	timing := t.TimingSource
	if timing == "" {
		timing = TimingLocalReceive
	}
	estimate := 0
	if t.PlayedIsEstimate {
		estimate = 1
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transcript_turns
		(call_id, owner_user_id, seq, revision, speaker, finality, started_ms, ended_ms, text,
		 provider_item_id, recorded_at, fidelity, timing_source, source_ids_json, correlation_id,
		 generated_ms, played_ms, played_is_estimate, quality_json)
		VALUES (?, ?, ?, (SELECT COALESCE(MAX(revision), 0) + 1 FROM transcript_turns WHERE call_id = ? AND seq = ?),
		        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		callID, owner, t.Seq, callID, t.Seq,
		t.Speaker, t.Finality, t.StartedMS, nullableInt(t.EndedMS), t.Text,
		nullableString(t.ProviderItemID), now, t.Fidelity, timing, sources, nullableString(t.CorrelationID),
		nullableInt(t.GeneratedMS), nullableInt(t.PlayedMS), estimate, quality)
	if err != nil {
		return fmt.Errorf("insert transcript revision seq %d: %w", t.Seq, err)
	}
	return nil
}

// MaxTranscriptSeq returns the highest turn seq recorded for the owner's call,
// or 0 when none is recorded. A later media run continues numbering after it,
// so earlier turns are never rewritten as revisions.
func (s *Store) MaxTranscriptSeq(ctx context.Context, ownerUserID, callID string) (int, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM transcript_turns WHERE owner_user_id = ? AND call_id = ?`,
		ownerUserID, callID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("storage: max transcript seq: %w", err)
	}
	return int(n.Int64), nil
}

// ListTranscriptTurns returns the latest revision of up to limit of the owner's
// turns for one call with seq greater than afterSeq, in seq order. Another
// owner's call yields no rows; callers check call ownership first.
func (s *Store) ListTranscriptTurns(ctx context.Context, ownerUserID, callID string, afterSeq, limit int) ([]TranscriptTurn, error) {
	if limit < 1 {
		return nil, errors.New("storage: list transcript: limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.seq, t.revision, t.speaker, t.finality, t.started_ms, t.ended_ms,
		t.text, t.provider_item_id, t.recorded_at, t.fidelity, t.timing_source, t.source_ids_json,
		t.correlation_id, t.generated_ms, t.played_ms, t.played_is_estimate, t.quality_json
		FROM transcript_turns t
		WHERE t.owner_user_id = ? AND t.call_id = ? AND t.seq > ?
		  AND t.revision = (SELECT MAX(x.revision) FROM transcript_turns x WHERE x.call_id = t.call_id AND x.seq = t.seq)
		ORDER BY t.seq LIMIT ?`, ownerUserID, callID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list transcript: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []TranscriptTurn{}
	for rows.Next() {
		var (
			t                  TranscriptTurn
			ended, gen, played sql.NullInt64
			item, corr         sql.NullString
			sources, quality   string
			estimate           int
		)
		if err := rows.Scan(&t.Seq, &t.Revision, &t.Speaker, &t.Finality, &t.StartedMS, &ended,
			&t.Text, &item, &t.RecordedAt, &t.Fidelity, &t.TimingSource, &sources,
			&corr, &gen, &played, &estimate, &quality); err != nil {
			return nil, fmt.Errorf("storage: scan transcript: %w", err)
		}
		t.EndedMS, t.GeneratedMS, t.PlayedMS = intPtr(ended), intPtr(gen), intPtr(played)
		t.ProviderItemID, t.CorrelationID = item.String, corr.String
		t.PlayedIsEstimate = estimate == 1
		if err := json.Unmarshal([]byte(sources), &t.SourceIDs); err != nil {
			return nil, fmt.Errorf("storage: transcript source ids: %w", err)
		}
		if err := json.Unmarshal([]byte(quality), &t.Quality); err != nil {
			return nil, fmt.Errorf("storage: transcript quality: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list transcript: %w", err)
	}
	return out, nil
}

// TranscriptSummary is the latest transcript.started or transcript.finalized
// event of a call. Recorded is false when the call never reached a media run.
type TranscriptSummary struct {
	Recorded     bool
	Finalized    bool
	QualityFlags []string
	PayloadJSON  string
}

// GetTranscriptSummary returns the latest transcript summary event of the
// owner's call. Another owner's call reads as not recorded.
func (s *Store) GetTranscriptSummary(ctx context.Context, ownerUserID, callID string) (TranscriptSummary, error) {
	var kind, payload string
	err := s.db.QueryRowContext(ctx, `SELECT kind, payload_json FROM call_events
		WHERE owner_user_id = ? AND call_id = ? AND kind IN (?, ?) ORDER BY seq DESC LIMIT 1`,
		ownerUserID, callID, TranscriptStartedKind, TranscriptFinalizedKind).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptSummary{QualityFlags: []string{}}, nil
	}
	if err != nil {
		return TranscriptSummary{}, fmt.Errorf("storage: transcript summary: %w", err)
	}
	var body struct {
		QualityFlags []string `json:"quality_flags"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return TranscriptSummary{}, fmt.Errorf("storage: transcript summary payload: %w", err)
	}
	if body.QualityFlags == nil {
		body.QualityFlags = []string{}
	}
	return TranscriptSummary{Recorded: true, Finalized: kind == TranscriptFinalizedKind, QualityFlags: body.QualityFlags, PayloadJSON: payload}, nil
}

func jsonStrings(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("storage: encode strings: %w", err)
	}
	return string(b), nil
}

func nullableInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func intPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
