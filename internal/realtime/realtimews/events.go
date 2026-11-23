package realtimews

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

type serverEvent struct {
	Type         string  `json:"type"`
	EventID      string  `json:"event_id"`
	ItemID       *string `json:"item_id"`
	ResponseID   string  `json:"response_id"`
	ContentIndex int     `json:"content_index"`
	Delta        *string `json:"delta"`
	Transcript   *string `json:"transcript"`
	AudioStartMS *int64  `json:"audio_start_ms"`
	AudioEndMS   *int64  `json:"audio_end_ms"`
	Response     *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Usage  *struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
	Error *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseServerEvent normalizes one Realtime GA server event. Beta event names
// (response.audio.delta etc.) are rejected: a profile speaking the beta
// protocol is not the GA contract this adapter negotiated.
func ParseServerEvent(b []byte) (realtime.Event, error) {
	var e serverEvent
	if err := json.Unmarshal(b, &e); err != nil {
		return realtime.Event{}, fmt.Errorf("%w: %v", realtime.ErrProtocol, err)
	}
	ev := realtime.Event{RawType: e.Type, EventID: e.EventID, ResponseID: e.ResponseID, ContentIndex: e.ContentIndex}
	if e.ItemID != nil {
		ev.ItemID = *e.ItemID
	}
	need := func(cond bool, what string) error {
		if !cond {
			return fmt.Errorf("%w: %s missing %s", realtime.ErrProtocol, e.Type, what)
		}
		return nil
	}
	switch e.Type {
	case "response.output_audio.delta":
		if err := need(e.Delta != nil && ev.ItemID != "" && e.ResponseID != "", "delta/item_id/response_id"); err != nil {
			return ev, err
		}
		a, err := base64.StdEncoding.DecodeString(*e.Delta)
		if err != nil {
			return ev, fmt.Errorf("%w: audio delta not base64", realtime.ErrProtocol)
		}
		ev.Kind, ev.Audio = realtime.EventAudio, a
	case "response.output_audio_transcript.delta":
		if err := need(e.Delta != nil && ev.ItemID != "", "delta/item_id"); err != nil {
			return ev, err
		}
		ev.Kind, ev.Text = realtime.EventAgentTranscriptDelta, *e.Delta
	case "response.output_audio_transcript.done":
		if err := need(e.Transcript != nil && ev.ItemID != "", "transcript/item_id"); err != nil {
			return ev, err
		}
		ev.Kind, ev.Text = realtime.EventAgentTranscriptDone, *e.Transcript
	case "conversation.item.input_audio_transcription.delta":
		if err := need(e.Delta != nil && ev.ItemID != "", "delta/item_id"); err != nil {
			return ev, err
		}
		ev.Kind, ev.Text = realtime.EventCallerTranscriptDelta, *e.Delta
	case "conversation.item.input_audio_transcription.completed":
		if err := need(e.Transcript != nil && ev.ItemID != "", "transcript/item_id"); err != nil {
			return ev, err
		}
		ev.Kind, ev.Text = realtime.EventCallerTranscriptFinal, *e.Transcript
	case "conversation.item.input_audio_transcription.failed":
		if err := need(ev.ItemID != "", "item_id"); err != nil {
			return ev, err
		}
		ev.Kind = realtime.EventCallerTranscriptFailed
		if e.Error != nil {
			ev.ErrCode, ev.ErrMessage = e.Error.Code, e.Error.Message
		}
	case "input_audio_buffer.speech_started":
		if err := need(e.AudioStartMS != nil, "audio_start_ms"); err != nil {
			return ev, err
		}
		ev.Kind, ev.HasTiming, ev.StartMS = realtime.EventSpeechStarted, true, *e.AudioStartMS
	case "conversation.item.truncated":
		if err := need(ev.ItemID != "" && e.AudioEndMS != nil, "item_id/audio_end_ms"); err != nil {
			return ev, err
		}
		ev.Kind, ev.EndMS = realtime.EventTruncated, *e.AudioEndMS
	case "response.done":
		if err := need(e.Response != nil && e.Response.Status != "", "response.status"); err != nil {
			return ev, err
		}
		ev.Kind, ev.ResponseID, ev.ResponseStatus = realtime.EventResponseDone, e.Response.ID, e.Response.Status
		if e.Response.Usage != nil {
			ev.UsageTokens = e.Response.Usage.TotalTokens
		}
	case "error":
		if err := need(e.Error != nil, "error"); err != nil {
			return ev, err
		}
		ev.Kind, ev.ErrCode, ev.ErrMessage = realtime.EventError, e.Error.Code, e.Error.Message
	case "response.audio.delta", "response.audio_transcript.delta", "response.audio.done", "response.audio_transcript.done":
		return ev, fmt.Errorf("%w: beta event %q on GA session", realtime.ErrProtocol, e.Type)
	case "session.start", "session.started", "session.output_audio.delta", "session.input_transcript.delta", "session.output_transcript.delta", "session.closed":
		return ev, fmt.Errorf("%w: GPT-Live event %q on realtime_ws session", realtime.ErrProtocol, e.Type)
	case "":
		return ev, fmt.Errorf("%w: event without type", realtime.ErrProtocol)
	default:
		ev.Kind = realtime.EventIgnored
	}
	return ev, nil
}
