package livews

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

type serverEvent struct {
	Type    string   `json:"type"`
	EventID string   `json:"event_id"`
	Delta   *string  `json:"delta"`
	StartMS *float64 `json:"start_ms"`
	EndMS   *float64 `json:"end_ms"`
	Reason  string   `json:"reason"`
	Usage   *struct {
		Seconds *float64 `json:"seconds"`
	} `json:"usage"`
	Delegation *struct {
		ID     string `json:"id"`
		Target string `json:"target"`
	} `json:"delegation"`
	Error *struct {
		Type    string  `json:"type"`
		Code    *string `json:"code"`
		Message string  `json:"message"`
	} `json:"error"`
}

// ParseServerEvent normalizes one GPT-Live primary WebSocket server event.
// Realtime-protocol events are rejected rather than tolerated.
func ParseServerEvent(b []byte) (realtime.Event, error) {
	var e serverEvent
	if err := json.Unmarshal(b, &e); err != nil {
		return realtime.Event{}, fmt.Errorf("%w: %v", realtime.ErrProtocol, err)
	}
	ev := realtime.Event{RawType: e.Type, EventID: e.EventID}
	bad := func(what string) error {
		return fmt.Errorf("%w: %s missing %s", realtime.ErrProtocol, e.Type, what)
	}
	timed := func() error {
		if e.Delta == nil || e.StartMS == nil || e.EndMS == nil || e.EventID == "" {
			return bad("delta/start_ms/end_ms/event_id")
		}
		if *e.EndMS < *e.StartMS || *e.StartMS < 0 {
			return fmt.Errorf("%w: %s invalid timing [%v,%v)", realtime.ErrProtocol, e.Type, *e.StartMS, *e.EndMS)
		}
		ev.Text, ev.HasTiming = *e.Delta, true
		ev.StartMS, ev.EndMS = int64(*e.StartMS), int64(*e.EndMS)
		return nil
	}
	switch e.Type {
	case "session.output_audio.delta":
		if e.Delta == nil {
			return ev, bad("delta")
		}
		a, err := base64.StdEncoding.DecodeString(*e.Delta)
		if err != nil || len(a) == 0 {
			return ev, fmt.Errorf("%w: output audio not base64", realtime.ErrProtocol)
		}
		ev.Kind, ev.Audio = realtime.EventAudio, a
	case "session.input_transcript.delta":
		if err := timed(); err != nil {
			return ev, err
		}
		ev.Kind = realtime.EventCallerTranscriptDelta
	case "session.output_transcript.delta":
		if err := timed(); err != nil {
			return ev, err
		}
		ev.Kind = realtime.EventAgentTranscriptDelta
	case "session.usage.updated":
		if e.Usage == nil || e.Usage.Seconds == nil {
			return ev, bad("usage.seconds")
		}
		ev.Kind, ev.UsageSeconds = realtime.EventUsage, *e.Usage.Seconds
	case "session.delegation.created":
		if e.Delegation == nil || e.Delegation.ID == "" {
			return ev, bad("delegation.id")
		}
		ev.Kind, ev.DelegationID = realtime.EventDelegation, e.Delegation.ID
	case "session.closed":
		ev.Kind, ev.CloseReason = realtime.EventClosed, e.Reason
		if e.Usage != nil && e.Usage.Seconds != nil {
			ev.UsageSeconds = *e.Usage.Seconds
		}
	case "error":
		if e.Error == nil {
			return ev, bad("error")
		}
		ev.Kind, ev.ErrMessage = realtime.EventError, e.Error.Message
		if e.Error.Code != nil {
			ev.ErrCode = *e.Error.Code
		}
	case "session.created", "response.output_audio.delta", "response.done",
		"conversation.item.truncated", "input_audio_buffer.speech_started",
		"conversation.item.input_audio_transcription.completed":
		return ev, fmt.Errorf("%w: Realtime event %q on live_ws session", realtime.ErrProtocol, e.Type)
	case "":
		return ev, fmt.Errorf("%w: event without type", realtime.ErrProtocol)
	default:
		ev.Kind = realtime.EventIgnored
	}
	return ev, nil
}
