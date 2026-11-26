package telnyx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Event types from the pinned Telnyx OpenAPI webhooks that the outbound call
// lifecycle consumes.
const (
	EventCallInitiated    = "call.initiated"
	EventCallAnswered     = "call.answered"
	EventCallHangup       = "call.hangup"
	EventStreamingStarted = "streaming.started"
	EventStreamingStopped = "streaming.stopped"
	EventStreamingFailed  = "streaming.failed"
)

// MaxWebhookBody bounds webhook request bodies before parsing.
const MaxWebhookBody = 1 << 20

// WebhookEvent is the normalized subset of a Telnyx Call Control webhook.
// Unknown event types are preserved as Type with Known=false.
type WebhookEvent struct {
	ID            string
	Type          string
	Known         bool
	OccurredAt    time.Time
	CallControlID string
	CallLegID     string
	CallSessionID string
	ClientState   string
	From, To      string
	HangupCause   string
	HangupSource  string
	SIPCause      string
	StreamID      string
	FailureReason string
}

type webhookEnvelope struct {
	Data *struct {
		RecordType string          `json:"record_type"`
		EventType  string          `json:"event_type"`
		ID         string          `json:"id"`
		OccurredAt string          `json:"occurred_at"`
		Payload    json.RawMessage `json:"payload"`
	} `json:"data"`
}

type webhookPayload struct {
	CallControlID  string `json:"call_control_id"`
	CallLegID      string `json:"call_leg_id"`
	CallSessionID  string `json:"call_session_id"`
	ClientState    string `json:"client_state"`
	From           string `json:"from"`
	To             string `json:"to"`
	HangupCause    string `json:"hangup_cause"`
	HangupSource   string `json:"hangup_source"`
	SIPHangupCause string `json:"sip_hangup_cause"`
	StreamID       string `json:"stream_id"`
	FailureReason  string `json:"failure_reason"`
}

var ErrInvalidWebhook = errors.New("telnyx: invalid webhook")

var knownEvents = map[string]bool{
	EventCallInitiated: true, EventCallAnswered: true, EventCallHangup: true,
	EventStreamingStarted: true, EventStreamingStopped: true, EventStreamingFailed: true,
}

// ParseWebhook decodes an already signature-verified webhook body.
func ParseWebhook(raw []byte) (WebhookEvent, error) {
	if len(raw) > MaxWebhookBody {
		return WebhookEvent{}, fmt.Errorf("%w: body too large", ErrInvalidWebhook)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var env webhookEnvelope
	if err := dec.Decode(&env); err != nil {
		return WebhookEvent{}, fmt.Errorf("%w: %v", ErrInvalidWebhook, err)
	}
	if dec.More() {
		return WebhookEvent{}, fmt.Errorf("%w: trailing data", ErrInvalidWebhook)
	}
	d := env.Data
	if d == nil {
		return WebhookEvent{}, fmt.Errorf("%w: missing data", ErrInvalidWebhook)
	}
	if d.RecordType != "event" || d.EventType == "" || d.ID == "" {
		return WebhookEvent{}, fmt.Errorf("%w: missing record_type/event_type/id", ErrInvalidWebhook)
	}
	ev := WebhookEvent{ID: d.ID, Type: d.EventType, Known: knownEvents[d.EventType]}
	if d.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339Nano, d.OccurredAt)
		if err != nil {
			return WebhookEvent{}, fmt.Errorf("%w: occurred_at: %v", ErrInvalidWebhook, err)
		}
		ev.OccurredAt = t
	}
	if !ev.Known {
		return ev, nil
	}
	var p webhookPayload
	if len(d.Payload) == 0 || json.Unmarshal(d.Payload, &p) != nil {
		return WebhookEvent{}, fmt.Errorf("%w: payload", ErrInvalidWebhook)
	}
	if p.CallControlID == "" {
		return WebhookEvent{}, fmt.Errorf("%w: missing call_control_id", ErrInvalidWebhook)
	}
	if p.CallLegID == "" && p.CallControlID == "" {
		return WebhookEvent{}, fmt.Errorf("%w: missing call identifiers", ErrInvalidWebhook)
	}
	ev.CallControlID = p.CallControlID
	ev.CallLegID = p.CallLegID
	ev.CallSessionID = p.CallSessionID
	ev.ClientState = p.ClientState
	ev.From, ev.To = p.From, p.To
	ev.HangupCause, ev.HangupSource, ev.SIPCause = p.HangupCause, p.HangupSource, p.SIPHangupCause
	ev.StreamID, ev.FailureReason = p.StreamID, p.FailureReason
	return ev, nil
}
