// Package realtimews implements the OpenAI-compatible Realtime GA WebSocket
// protocol: model in the connect URL, server-first session.created, then
// session.update/session.updated before audio.
package realtimews

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// Config configures one Realtime session.
type Config struct {
	Endpoint     string // e.g. wss://api.openai.com/v1/realtime
	APIKey       string
	Model        string
	Voice        string
	Instructions string
	Format       audio.Format
	Headers      map[string]string
	HTTPClient   *http.Client
}

func formatJSON(f audio.Format) (map[string]any, error) {
	switch f {
	case audio.Format{Encoding: audio.L16, SampleRate: 24000, LittleEndian: true}:
		return map[string]any{"type": "audio/pcm", "rate": 24000}, nil
	case audio.Format{Encoding: audio.PCMU, SampleRate: 8000}:
		return map[string]any{"type": "audio/pcmu"}, nil
	case audio.Format{Encoding: audio.PCMA, SampleRate: 8000}:
		return map[string]any{"type": "audio/pcma"}, nil
	}
	return nil, fmt.Errorf("%w: realtime_ws does not support %v", realtime.ErrCapability, f)
}

// SessionUpdate builds the GA session.update client event. Input
// transcription is deliberately never set: upstream bills it separately via a
// transcription model, which V1 forbids as a hidden paid fallback.
func SessionUpdate(c Config) ([]byte, error) {
	fj, err := formatJSON(c.Format)
	if err != nil {
		return nil, err
	}
	in := map[string]any{
		"format":         fj,
		"turn_detection": map[string]any{"type": "server_vad", "create_response": true, "interrupt_response": true},
	}
	out := map[string]any{"format": fj}
	if c.Voice != "" {
		out["voice"] = c.Voice
	}
	return json.Marshal(map[string]any{
		"type":     "session.update",
		"event_id": "ch_session_update",
		"session": map[string]any{
			"type":              "realtime",
			"output_modalities": []string{"audio"},
			"instructions":      c.Instructions,
			"audio":             map[string]any{"input": in, "output": out},
		},
	})
}

// Session is a connected Realtime session.
type Session struct {
	conn   *websocket.Conn
	format audio.Format
	wmu    sync.Mutex
	ready  bool
	// SessionID is the provider session id from session.created.
	SessionID string
}

// Connect dials, validates the server-first handshake and applies the session
// configuration. It fails closed if the server does not echo the requested
// audio format.
func Connect(ctx context.Context, c Config) (*Session, error) {
	if c.Model == "" {
		return nil, errors.New("realtimews: model is required")
	}
	u, err := realtime.ValidateEndpoint(c.Endpoint)
	if err != nil {
		return nil, err
	}
	if u.Query().Has("model") {
		return nil, errors.New("realtimews: endpoint must not already contain model")
	}
	q := u.Query()
	q.Set("model", c.Model)
	u.RawQuery = q.Encode()
	update, err := SessionUpdate(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, realtime.HandshakeTimeout)
	defer cancel()
	h, err := realtime.ValidateHeaders(c.Headers)
	if err != nil {
		return nil, err
	}
	h.Set("Authorization", "Bearer "+c.APIKey)
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h, HTTPClient: c.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("realtimews: dial: %w", err)
	}
	conn.SetReadLimit(realtime.MaxServerMessage)
	s := &Session{conn: conn, format: c.Format}
	first, err := s.readRaw(ctx)
	if err != nil {
		return nil, s.fail(err)
	}
	if first.Type != "session.created" {
		return nil, s.fail(fmt.Errorf("%w: first server event %q, want session.created", realtime.ErrProtocol, first.Type))
	}
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	_ = json.Unmarshal(first.raw, &created)
	s.SessionID = created.Session.ID
	if err := s.write(ctx, update); err != nil {
		return nil, s.fail(err)
	}
	for {
		m, err := s.readRaw(ctx)
		if err != nil {
			return nil, s.fail(err)
		}
		switch m.Type {
		case "session.updated":
			if err := checkUpdated(m.raw, c.Format); err != nil {
				return nil, s.fail(err)
			}
			s.ready = true
			return s, nil
		case "error":
			ev, _ := ParseServerEvent(m.raw)
			return nil, s.fail(fmt.Errorf("realtimews: session.update rejected: %s: %s", ev.ErrCode, ev.ErrMessage))
		}
	}
}

func checkUpdated(raw []byte, f audio.Format) error {
	want, _ := formatJSON(f)
	var u struct {
		Session struct {
			Audio struct {
				Input struct {
					Format map[string]any `json:"format"`
				} `json:"input"`
				Output struct {
					Format map[string]any `json:"format"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return fmt.Errorf("%w: session.updated: %v", realtime.ErrProtocol, err)
	}
	for _, got := range []map[string]any{u.Session.Audio.Input.Format, u.Session.Audio.Output.Format} {
		if got["type"] != want["type"] {
			return fmt.Errorf("%w: server audio format %v, requested %v", realtime.ErrCapability, got, want)
		}
		if r, ok := want["rate"]; ok && fmt.Sprint(got["rate"]) != fmt.Sprint(r) {
			return fmt.Errorf("%w: server audio rate %v, requested %v", realtime.ErrCapability, got["rate"], r)
		}
	}
	return nil
}

type rawMsg struct {
	Type string
	raw  []byte
}

func (s *Session) readRaw(ctx context.Context) (rawMsg, error) {
	typ, b, err := s.conn.Read(ctx)
	if err != nil {
		return rawMsg{}, err
	}
	if typ != websocket.MessageText {
		return rawMsg{}, fmt.Errorf("%w: binary frame", realtime.ErrProtocol)
	}
	var h struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &h); err != nil || h.Type == "" {
		return rawMsg{}, fmt.Errorf("%w: message without type", realtime.ErrProtocol)
	}
	return rawMsg{Type: h.Type, raw: b}, nil
}

func (s *Session) write(ctx context.Context, b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

func (s *Session) fail(err error) error {
	_ = s.conn.Close(websocket.StatusPolicyViolation, "protocol error")
	return err
}

// AppendAudio sends caller audio in the negotiated format.
func (s *Session) AppendAudio(ctx context.Context, b []byte) error {
	if !s.ready {
		return realtime.ErrNotReady
	}
	if _, err := s.format.Duration(len(b)); err != nil || len(b) == 0 {
		return fmt.Errorf("realtimews: invalid audio chunk of %d bytes", len(b))
	}
	msg, _ := json.Marshal(map[string]string{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(b)})
	return s.write(ctx, msg)
}

// Truncate reports how much of an assistant item was actually played.
func (s *Session) Truncate(ctx context.Context, itemID string, contentIndex int, audioEndMS int64) error {
	if !s.ready {
		return realtime.ErrNotReady
	}
	if itemID == "" || audioEndMS < 0 {
		return errors.New("realtimews: truncate requires item id and non-negative audio_end_ms")
	}
	msg, _ := json.Marshal(map[string]any{"type": "conversation.item.truncate", "item_id": itemID, "content_index": contentIndex, "audio_end_ms": audioEndMS})
	return s.write(ctx, msg)
}

// Cancel cancels the in-progress response.
func (s *Session) Cancel(ctx context.Context) error {
	if !s.ready {
		return realtime.ErrNotReady
	}
	return s.write(ctx, []byte(`{"type":"response.cancel"}`))
}

// Next returns the next normalized server event.
func (s *Session) Next(ctx context.Context) (realtime.Event, error) {
	m, err := s.readRaw(ctx)
	if err != nil {
		return realtime.Event{}, realtime.Transport(err)
	}
	return ParseServerEvent(m.raw)
}

// Close closes the socket. Realtime has no close handshake event.
func (s *Session) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}
