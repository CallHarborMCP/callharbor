package telnyx

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// Media WebSocket frame types documented at
// https://developers.telnyx.com/docs/voice/programmable-voice/media-streaming
const (
	FrameConnected = "connected"
	FrameStart     = "start"
	FrameMedia     = "media"
	FrameStop      = "stop"
	FrameMark      = "mark"
	FrameDTMF      = "dtmf"
	FrameError     = "error"
)

// MaxMediaFrame bounds one inbound text frame (30 s of L16/16k base64 is
// ~1.3 MB; inbound frames are ~20 ms in practice).
const MaxMediaFrame = 2 << 20

var ErrInvalidFrame = errors.New("telnyx: invalid media frame")

// flexInt accepts both JSON numbers and decimal strings: Telnyx examples use
// strings for sequence_number/chunk/timestamp.
type flexInt struct {
	v   int64
	set bool
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(s)
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("not an integer: %q", b)
	}
	f.v, f.set = v, true
	return nil
}

// Frame is a decoded inbound media WebSocket message.
type Frame struct {
	Event    string
	StreamID string
	Sequence int64
	HasSeq   bool

	// start
	CallControlID string
	CallSessionID string
	ClientState   string
	Format        audio.Format

	// media
	Track     string
	Chunk     int64
	Timestamp uint32
	Payload   []byte

	// mark
	MarkName string
	// dtmf
	Digit string
	// error
	ErrCode   int64
	ErrTitle  string
	ErrDetail string
}

type rawFrame struct {
	Event          string  `json:"event"`
	Version        string  `json:"version"`
	StreamID       string  `json:"stream_id"`
	SequenceNumber flexInt `json:"sequence_number"`
	Start          *struct {
		CallControlID string `json:"call_control_id"`
		CallSessionID string `json:"call_session_id"`
		ClientState   string `json:"client_state"`
		MediaFormat   *struct {
			Encoding   string  `json:"encoding"`
			SampleRate flexInt `json:"sample_rate"`
			Channels   flexInt `json:"channels"`
		} `json:"media_format"`
	} `json:"start"`
	Media *struct {
		Track     string  `json:"track"`
		Chunk     flexInt `json:"chunk"`
		Timestamp flexInt `json:"timestamp"`
		Payload   *string `json:"payload"`
	} `json:"media"`
	Mark *struct {
		Name string `json:"name"`
	} `json:"mark"`
	DTMF *struct {
		Digit string `json:"digit"`
	} `json:"dtmf"`
	Payload *struct {
		Code   flexInt `json:"code"`
		Title  string  `json:"title"`
		Detail string  `json:"detail"`
	} `json:"payload"`
}

// ParseFrame decodes and validates one inbound frame. Unknown events return
// an error so the bridge can log and ignore them explicitly.
func ParseFrame(b []byte) (Frame, error) {
	if len(b) > MaxMediaFrame {
		return Frame{}, fmt.Errorf("%w: frame too large", ErrInvalidFrame)
	}
	var r rawFrame
	if err := json.Unmarshal(b, &r); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrInvalidFrame, err)
	}
	f := Frame{Event: r.Event, StreamID: r.StreamID, Sequence: r.SequenceNumber.v, HasSeq: r.SequenceNumber.set}
	switch r.Event {
	case FrameConnected:
		return f, nil
	case FrameStart:
		if r.Start == nil || r.Start.MediaFormat == nil || r.StreamID == "" || r.Start.CallControlID == "" {
			return Frame{}, fmt.Errorf("%w: start requires stream_id, call_control_id and media_format", ErrInvalidFrame)
		}
		mf := r.Start.MediaFormat
		if mf.Channels.set && mf.Channels.v != 1 {
			return Frame{}, fmt.Errorf("%w: unsupported channel count %d", ErrInvalidFrame, mf.Channels.v)
		}
		af := audio.Format{Encoding: audio.Encoding(mf.Encoding), SampleRate: int(mf.SampleRate.v)}
		if af.Encoding == audio.L16 {
			af.LittleEndian = true
		}
		if err := af.Validate(); err != nil {
			return Frame{}, fmt.Errorf("%w: %v", ErrInvalidFrame, err)
		}
		f.CallControlID = r.Start.CallControlID
		f.CallSessionID = r.Start.CallSessionID
		f.ClientState = r.Start.ClientState
		f.Format = af
	case FrameMedia:
		m := r.Media
		if m == nil || m.Payload == nil || r.StreamID == "" {
			return Frame{}, fmt.Errorf("%w: media requires stream_id and payload", ErrInvalidFrame)
		}
		switch m.Track {
		case "inbound", "outbound":
		default:
			return Frame{}, fmt.Errorf("%w: unknown media track %q", ErrInvalidFrame, m.Track)
		}
		if !m.Timestamp.set || m.Timestamp.v < 0 || m.Timestamp.v > 0xFFFFFFFF {
			return Frame{}, fmt.Errorf("%w: media timestamp missing or out of RTP range", ErrInvalidFrame)
		}
		p, err := base64.StdEncoding.DecodeString(*m.Payload)
		if err != nil || len(p) == 0 {
			return Frame{}, fmt.Errorf("%w: media payload is not non-empty base64", ErrInvalidFrame)
		}
		f.Track, f.Chunk, f.Timestamp, f.Payload = m.Track, m.Chunk.v, uint32(m.Timestamp.v), p
	case FrameStop:
		if r.StreamID == "" {
			return Frame{}, fmt.Errorf("%w: stop requires stream_id", ErrInvalidFrame)
		}
	case FrameMark:
		if r.Mark == nil || r.Mark.Name == "" {
			return Frame{}, fmt.Errorf("%w: mark requires name", ErrInvalidFrame)
		}
		f.MarkName = r.Mark.Name
	case FrameDTMF:
		if r.DTMF == nil || len(r.DTMF.Digit) != 1 || !isDTMF(r.DTMF.Digit[0]) {
			return Frame{}, fmt.Errorf("%w: invalid dtmf digit", ErrInvalidFrame)
		}
		f.Digit = r.DTMF.Digit
	case FrameError:
		if r.Payload == nil || !r.Payload.Code.set {
			return Frame{}, fmt.Errorf("%w: error requires payload.code", ErrInvalidFrame)
		}
		f.ErrCode, f.ErrTitle, f.ErrDetail = r.Payload.Code.v, r.Payload.Title, r.Payload.Detail
	default:
		return Frame{}, fmt.Errorf("%w: unknown event %q", ErrInvalidFrame, r.Event)
	}
	return f, nil
}

func isDTMF(c byte) bool {
	return (c >= '0' && c <= '9') || c == '*' || c == '#' || (c >= 'A' && c <= 'D')
}

// MediaMessage builds an outbound RTP-mode media frame carrying raw codec
// bytes (no RTP header), matching Telnyx reference implementations.
func MediaMessage(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, errors.New("telnyx: empty media payload")
	}
	return json.Marshal(map[string]any{
		"event": FrameMedia,
		"media": map[string]string{"payload": base64.StdEncoding.EncodeToString(payload)},
	})
}

// MarkMessage builds an outbound mark frame.
func MarkMessage(name string) ([]byte, error) {
	if name == "" {
		return nil, errors.New("telnyx: empty mark name")
	}
	return json.Marshal(map[string]any{"event": FrameMark, "mark": map[string]string{"name": name}})
}

// ClearMessage builds an outbound clear frame.
func ClearMessage() []byte { return []byte(`{"event":"clear"}`) }
