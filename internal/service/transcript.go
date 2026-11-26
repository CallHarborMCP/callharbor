package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// Flags the service derives from the call's media state. Stored flags come
// from the transcript summary event; these two describe the absence or the
// incompleteness of that event.
const (
	flagNoMediaSession       = "no_media_session"
	flagTranscriptInProgress = "transcript_in_progress"
	flagTranscriptNotFinal   = "transcript_not_finalized"
)

// Audit vocabulary for transcript reads.
const (
	auditTranscriptRead = "transcript.read"
	auditResourceCall   = "call"
	auditRoleUser       = "user"
	auditOutcomeSuccess = "success"
	auditOutcomeDenied  = "denied"
)

// TranscriptStore is the read and audit repository of transcripts. It is kept
// apart from Store so the call paths do not change when transcripts are added.
type TranscriptStore interface {
	ListTranscriptTurns(ctx context.Context, ownerUserID, callID string, afterSeq, limit int) ([]storage.TranscriptTurn, error)
	GetTranscriptSummary(ctx context.Context, ownerUserID, callID string) (storage.TranscriptSummary, error)
	InsertAudit(ctx context.Context, e storage.AuditEvent) error
}

// WithTranscripts attaches the transcript read path. Until it is set,
// GetTranscript returns unavailable without touching the store.
func (s *Service) WithTranscripts(ts TranscriptStore) *Service {
	s.transcripts = ts
	return s
}

// GetTranscriptInput names one call and an optional page cursor.
type GetTranscriptInput struct {
	CallID string
	Cursor string
}

// TranscriptPage is one page of a call's transcript. Turns are the latest
// revision of each turn, in seq order. NextCursor is empty on the last page.
type TranscriptPage struct {
	CallID       string           `json:"call_id"`
	Turns        []TranscriptTurn `json:"turns"`
	QualityFlags []string         `json:"quality_flags"`
	Finalized    bool             `json:"finalized"`
	NextCursor   string           `json:"next_cursor"`
}

// TranscriptTurn is one turn as the API exposes it. Nullable provenance is a
// JSON null when unknown, never an empty value.
type TranscriptTurn struct {
	Seq              int      `json:"seq"`
	Revision         int      `json:"revision"`
	Speaker          string   `json:"speaker"`
	Finality         string   `json:"finality"`
	StartedMS        int64    `json:"started_ms"`
	EndedMS          *int64   `json:"ended_ms"`
	Text             string   `json:"text"`
	Fidelity         string   `json:"fidelity"`
	TimingSource     string   `json:"timing_source"`
	SourceIDs        []string `json:"source_ids"`
	CorrelationID    *string  `json:"correlation_id"`
	ProviderItemID   *string  `json:"provider_item_id"`
	GeneratedMS      *int64   `json:"generated_ms"`
	PlayedMS         *int64   `json:"played_ms"`
	PlayedIsEstimate bool     `json:"played_is_estimate"`
	QualityFlags     []string `json:"quality_flags"`
}

// transcriptCursorPayload is the decoded form of a transcript cursor. S is the
// seq of the last turn on the previous page.
type transcriptCursorPayload struct {
	V int `json:"v"`
	S int `json:"s"`
}

// GetTranscript returns one page of a call's transcript owned by the actor. A
// call owned by another user is reported as not_found, the same as a missing
// call. Every read that returns a page, or refuses a foreign or missing call,
// writes a transcript.read audit event. A failed audit write fails the request.
func (s *Service) GetTranscript(ctx context.Context, actor Actor, in GetTranscriptInput) (TranscriptPage, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return TranscriptPage{}, err
	}
	if s.transcripts == nil {
		return TranscriptPage{}, AsErrorWith(CodeUnavailable, "transcripts are not available")
	}
	if in.CallID == "" || len(in.CallID) > MaxIDLength {
		return TranscriptPage{}, invalidInput("call_id must be 1 to %d characters", MaxIDLength)
	}
	afterSeq, err := decodeTranscriptCursor(in.Cursor)
	if err != nil {
		return TranscriptPage{}, err
	}
	// Ownership comes from the call row. The turn and summary reads return
	// nothing for another owner, so they cannot replace this check.
	call, err := s.store.GetCall(ctx, actor.UserID, in.CallID)
	if errors.Is(err, storage.ErrCallNotFound) {
		if err := s.auditTranscript(ctx, actor, in.CallID, auditOutcomeDenied, `{"reason":"not_found"}`); err != nil {
			return TranscriptPage{}, err
		}
		return TranscriptPage{}, newError(CodeNotFound, "call not found")
	}
	if err != nil {
		return TranscriptPage{}, internal(fmt.Errorf("get call: %w", err))
	}
	summary, err := s.transcripts.GetTranscriptSummary(ctx, actor.UserID, in.CallID)
	if err != nil {
		return TranscriptPage{}, internal(fmt.Errorf("transcript summary: %w", err))
	}
	// Fetch one extra turn to learn whether another page exists.
	rows, err := s.transcripts.ListTranscriptTurns(ctx, actor.UserID, in.CallID, afterSeq, MaxLimit+1)
	if err != nil {
		return TranscriptPage{}, internal(fmt.Errorf("list transcript turns: %w", err))
	}
	page := TranscriptPage{
		CallID:       in.CallID,
		Turns:        []TranscriptTurn{},
		QualityFlags: pageFlags(summary, isTerminal(call.Status)),
		Finalized:    summary.Finalized,
	}
	hasMore := len(rows) > MaxLimit
	if hasMore {
		rows = rows[:MaxLimit]
	}
	for _, t := range rows {
		page.Turns = append(page.Turns, toTranscriptTurn(t))
	}
	if hasMore {
		page.NextCursor = encodeTranscriptCursor(rows[len(rows)-1].Seq)
	}
	if err := s.auditTranscript(ctx, actor, in.CallID, auditOutcomeSuccess, fmt.Sprintf(`{"turns":%d}`, len(page.Turns))); err != nil {
		return TranscriptPage{}, err
	}
	return page, nil
}

// auditTranscript records one transcript read. The audit row is the security
// record, so a failed write returns internal rather than letting the read
// pass unrecorded.
func (s *Service) auditTranscript(ctx context.Context, actor Actor, callID, outcome, detail string) error {
	err := s.transcripts.InsertAudit(ctx, storage.AuditEvent{
		OccurredAt:   s.now().UTC().Format(storageTimeLayout),
		ActorUserID:  actor.UserID,
		ActorRole:    auditRoleUser,
		TargetUserID: actor.UserID,
		Action:       auditTranscriptRead,
		ResourceType: auditResourceCall,
		ResourceID:   callID,
		Outcome:      outcome,
		DetailJSON:   detail,
	})
	if err != nil {
		return internal(fmt.Errorf("audit transcript read: %w", err))
	}
	return nil
}

// isTerminal reports whether a call status is one a call never leaves.
func isTerminal(status string) bool {
	return status == "ended" || status == "failed"
}

// pageFlags is the page's quality flags: the summary's stored flags plus the
// flag for the media state. The result is deduplicated, sorted and never nil.
func pageFlags(sum storage.TranscriptSummary, ended bool) []string {
	set := map[string]struct{}{}
	for _, f := range sum.QualityFlags {
		set[f] = struct{}{}
	}
	if !sum.Recorded {
		set[flagNoMediaSession] = struct{}{}
	}
	// A call that has ended cannot still be in progress. If its run never
	// wrote a finalized summary, the transcript is not finalized.
	if sum.Recorded && !sum.Finalized {
		if ended {
			set[flagTranscriptNotFinal] = struct{}{}
		} else {
			set[flagTranscriptInProgress] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func toTranscriptTurn(t storage.TranscriptTurn) TranscriptTurn {
	return TranscriptTurn{
		Seq:              t.Seq,
		Revision:         t.Revision,
		Speaker:          t.Speaker,
		Finality:         t.Finality,
		StartedMS:        t.StartedMS,
		EndedMS:          t.EndedMS,
		Text:             t.Text,
		Fidelity:         t.Fidelity,
		TimingSource:     t.TimingSource,
		SourceIDs:        nonNilStrings(t.SourceIDs),
		CorrelationID:    optionalString(t.CorrelationID),
		ProviderItemID:   optionalString(t.ProviderItemID),
		GeneratedMS:      t.GeneratedMS,
		PlayedMS:         t.PlayedMS,
		PlayedIsEstimate: t.PlayedIsEstimate,
		QualityFlags:     nonNilStrings(t.Quality),
	}
}

// optionalString maps the storage empty string, which stands for NULL, to nil.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// encodeTranscriptCursor returns the opaque token for the position after seq.
func encodeTranscriptCursor(seq int) string {
	raw, _ := json.Marshal(transcriptCursorPayload{V: cursorV1, S: seq})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeTranscriptCursor returns the seq to read after. An empty token is the
// first page and returns 0. A malformed token is invalid input.
func decodeTranscriptCursor(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	var p transcriptCursorPayload
	if err := decodeOpaque(token, &p); err != nil {
		return 0, err
	}
	if p.V != cursorV1 || p.S < 1 {
		return 0, invalidInput("cursor is malformed")
	}
	return p.S, nil
}
