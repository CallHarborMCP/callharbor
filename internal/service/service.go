package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// ConnectivityUnknown is the only connectivity status reported until live
// provider checks ship in issue #81. It is never reported as healthy.
const ConnectivityUnknown = "unknown"

// callStates is the complete call status vocabulary from the calls table.
var callStates = map[string]struct{}{
	"queued": {}, "initiating": {}, "ringing": {}, "active": {},
	"ending": {}, "ended": {}, "failed": {},
}

// CallStates returns the complete call status vocabulary, sorted, so tests and
// schema drift checks compare against the service's own list.
func CallStates() []string {
	out := make([]string, 0, len(callStates))
	for s := range callStates {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Store is the owner-scoped read repository the service needs.
type Store interface {
	ListTelephonyProfiles(ctx context.Context, ownerUserID string) ([]storage.TelephonyProfile, error)
	ListAIProfiles(ctx context.Context, ownerUserID string) ([]storage.AIProfile, error)
	ListCalls(ctx context.Context, ownerUserID, status string, after *storage.CallCursor, limit int) ([]storage.Call, error)
	GetCall(ctx context.Context, ownerUserID, id string) (storage.Call, error)
}

// Service implements the shared operations. It holds no caller state: every
// call receives its Actor explicitly.
type Service struct {
	store Store
}

// New returns a Service backed by store.
func New(store Store) *Service {
	return &Service{store: store}
}

// TelephonyProfileView is one telephony profile as the API exposes it. Secret
// references never appear here.
type TelephonyProfileView struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Provider           string `json:"provider"`
	CallerNumber       string `json:"caller_number"`
	ConnectivityStatus string `json:"connectivity_status"`
}

// AIProfileView is one AI voice profile as the API exposes it. Endpoint URLs,
// headers, instructions and secret references are deliberately absent.
type AIProfileView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Protocol         string `json:"protocol"`
	PrimaryTransport string `json:"primary_transport"`
	Model            string `json:"model"`
	Voice            string `json:"voice"`
}

// ProfilesResult is the response of GetProfiles.
type ProfilesResult struct {
	AIProfiles        []AIProfileView        `json:"ai_profiles"`
	TelephonyProfiles []TelephonyProfileView `json:"telephony_profiles"`
}

// PrimaryTransportWebSocket is the only transport implemented today.
const PrimaryTransportWebSocket = "websocket"

// CallDetails carries the non-state facts of a call.
type CallDetails struct {
	OutcomeReasonCode  string `json:"outcome_reason_code,omitempty"`
	MaxDurationSeconds int    `json:"max_duration_seconds"`
	RecordRequested    bool   `json:"record_requested"`
	RecordEffective    bool   `json:"record_effective"`
}

// CallView is one call as the API exposes it. The same shape is used for
// get_call and for list_calls summaries, so clients parse both identically.
type CallView struct {
	CallID              string      `json:"call_id"`
	TelephonyProfileID  string      `json:"telephony_profile_id"`
	AIProfileID         string      `json:"ai_profile_id"`
	ToNumber            string      `json:"to_number"`
	TechnicalStatus     string      `json:"technical_status"`
	TaskOutcome         *string     `json:"task_outcome"`
	Details             CallDetails `json:"details"`
	TranscriptAvailable bool        `json:"transcript_available"`
	RecordingAvailable  bool        `json:"recording_available"`
	CreatedAt           string      `json:"created_at"`
	UpdatedAt           string      `json:"updated_at"`
	EndedAt             *string     `json:"ended_at"`
}

// ListCallsInput is the validated-at-the-edge input of ListCalls.
type ListCallsInput struct {
	Cursor string
	Limit  int
	Status string
}

// CallsResult is one page of calls. NextCursor is empty on the last page.
type CallsResult struct {
	Calls      []CallView `json:"calls"`
	NextCursor string     `json:"next_cursor"`
}

// GetProfiles returns the caller's AI and telephony profiles, oldest first.
// Only non-secret fields are returned.
func (s *Service) GetProfiles(ctx context.Context, actor Actor) (ProfilesResult, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return ProfilesResult{}, err
	}
	telRows, err := s.store.ListTelephonyProfiles(ctx, actor.UserID)
	if err != nil {
		return ProfilesResult{}, internal(fmt.Errorf("list telephony profiles: %w", err))
	}
	aiRows, err := s.store.ListAIProfiles(ctx, actor.UserID)
	if err != nil {
		return ProfilesResult{}, internal(fmt.Errorf("list ai profiles: %w", err))
	}
	out := ProfilesResult{
		AIProfiles:        make([]AIProfileView, 0, len(aiRows)),
		TelephonyProfiles: make([]TelephonyProfileView, 0, len(telRows)),
	}
	for _, p := range telRows {
		out.TelephonyProfiles = append(out.TelephonyProfiles, TelephonyProfileView{
			ID: p.ID, Name: p.Name, Provider: p.Provider, CallerNumber: p.CallerNumber,
			ConnectivityStatus: ConnectivityUnknown,
		})
	}
	for _, p := range aiRows {
		out.AIProfiles = append(out.AIProfiles, AIProfileView{
			ID: p.ID, Name: p.Name, Protocol: string(p.Protocol),
			PrimaryTransport: PrimaryTransportWebSocket, Model: p.Model, Voice: p.Voice,
		})
	}
	return out, nil
}

// ListCalls returns one keyset page of the caller's calls, newest first.
func (s *Service) ListCalls(ctx context.Context, actor Actor, in ListCallsInput) (CallsResult, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return CallsResult{}, err
	}
	limit, err := NormalizeLimit(in.Limit)
	if err != nil {
		return CallsResult{}, err
	}
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return CallsResult{}, err
	}
	if in.Status != "" {
		if _, ok := callStates[in.Status]; !ok {
			return CallsResult{}, invalidInput("unknown status filter %q", in.Status)
		}
	}
	// Fetch one extra row to learn whether another page exists.
	rows, err := s.store.ListCalls(ctx, actor.UserID, in.Status, after, limit+1)
	if err != nil {
		return CallsResult{}, internal(fmt.Errorf("list calls: %w", err))
	}
	out := CallsResult{Calls: make([]CallView, 0, len(rows))}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	for _, c := range rows {
		view, err := toCallView(c)
		if err != nil {
			return CallsResult{}, internal(err)
		}
		out.Calls = append(out.Calls, view)
	}
	if hasMore {
		last := rows[len(rows)-1]
		out.NextCursor = encodeCursor(storage.CallCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return out, nil
}

// GetCall returns one call owned by the actor. A call owned by another user
// is reported as not_found, the same as a missing call.
func (s *Service) GetCall(ctx context.Context, actor Actor, callID string) (CallView, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return CallView{}, err
	}
	if callID == "" || len(callID) > MaxIDLength {
		return CallView{}, invalidInput("call_id must be 1 to %d characters", MaxIDLength)
	}
	c, err := s.store.GetCall(ctx, actor.UserID, callID)
	if errors.Is(err, storage.ErrCallNotFound) {
		return CallView{}, newError(CodeNotFound, "call not found")
	}
	if err != nil {
		return CallView{}, internal(fmt.Errorf("get call: %w", err))
	}
	view, err := toCallView(c)
	if err != nil {
		return CallView{}, internal(err)
	}
	return view, nil
}

func toCallView(c storage.Call) (CallView, error) {
	created, err := formatTimestamp(c.CreatedAt)
	if err != nil {
		return CallView{}, err
	}
	updated, err := formatTimestamp(c.UpdatedAt)
	if err != nil {
		return CallView{}, err
	}
	view := CallView{
		CallID: c.ID, TelephonyProfileID: c.TelephonyProfileID, AIProfileID: c.AIProfileID,
		ToNumber: c.ToNumber, TechnicalStatus: c.Status,
		Details: CallDetails{
			OutcomeReasonCode:  c.OutcomeReasonCode,
			MaxDurationSeconds: c.MaxDurationSeconds,
			RecordRequested:    c.RecordRequested,
			RecordEffective:    c.RecordEffective,
		},
		TranscriptAvailable: c.HasTranscript,
		RecordingAvailable:  c.RecordingStatus == "complete",
		CreatedAt:           created,
		UpdatedAt:           updated,
	}
	if c.Outcome != "" {
		o := c.Outcome
		view.TaskOutcome = &o
	}
	if c.EndedAt != "" {
		ended, err := formatTimestamp(c.EndedAt)
		if err != nil {
			return CallView{}, err
		}
		view.EndedAt = &ended
	}
	return view, nil
}

// formatTimestamp converts a stored timestamp to RFC 3339 in UTC with a
// literal Z and millisecond precision.
func formatTimestamp(stored string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		return "", fmt.Errorf("parse stored timestamp: %w", err)
	}
	return t.UTC().Format(storageTimeLayout), nil
}
