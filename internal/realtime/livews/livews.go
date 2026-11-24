// Package livews implements the GPT-Live primary WebSocket protocol: connect
// to /v1/live/sessions without a model query, send session.start first and
// wait for session.started before any audio.
package livews

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

// Config configures one GPT-Live session.
type Config struct {
	Endpoint     string // e.g. wss://api.openai.com/v1/live/sessions
	APIKey       string
	Model        string
	Voice        string
	Instructions string
	Format       audio.Format
	Headers      map[string]string
	HTTPClient   *http.Client
}

func formatJSON(f audio.Format) (map[string]any, error) {
	switch {
	case f.Encoding == audio.L16 && f.LittleEndian && (f.SampleRate == 24000 || f.SampleRate == 16000):
		return map[string]any{"type": "audio/pcm", "rate": f.SampleRate}, nil
	case f == audio.Format{Encoding: audio.PCMU, SampleRate: 8000}:
		return map[string]any{"type": "audio/pcmu", "rate": 8000}, nil
	case f == audio.Format{Encoding: audio.PCMA, SampleRate: 8000}:
		return map[string]any{"type": "audio/pcma", "rate": 8000}, nil
	}
	return nil, fmt.Errorf("%w: live_ws does not support %v", realtime.ErrCapability, f)
}

// SessionStart builds the session.start client event. Client delegation is
// declared explicitly so the server never selects a paid Responses backend.
func SessionStart(c Config) ([]byte, error) {
	if c.Model == "" {
		return nil, errors.New("livews: model is required")
	}
	fj, err := formatJSON(c.Format)
	if err != nil {
		return nil, err
	}
	a := map[string]any{"format": fj}
	if c.Voice != "" {
		a["output"] = map[string]any{"voice": c.Voice}
	}
	return json.Marshal(map[string]any{
		"type":     "session.start",
		"event_id": "ch_session_start",
		"session": map[string]any{
			"model":        c.Model,
			"instructions": c.Instructions,
			"audio":        a,
			"delegation":   map[string]any{"type": "client"},
		},
	})
}

// Session is a started GPT-Live session.
type Session struct {
	conn   *websocket.Conn
	format audio.Format
	wmu    sync.Mutex
	ready  bool
	// SessionID is the provider session id from session.started.
	SessionID string
}

// Connect dials and performs the client-first startup handshake. It fails
// closed if the server reports a different model, format or delegation type.
func Connect(ctx context.Context, c Config) (*Session, error) {
	u, err := realtime.ValidateEndpoint(c.Endpoint)
	if err != nil {
		return nil, err
	}
	if u.RawQuery != "" {
		return nil, errors.New("livews: endpoint must not contain query parameters")
	}
	start, err := SessionStart(c)
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
		return nil, fmt.Errorf("livews: dial: %w", err)
	}
	conn.SetReadLimit(realtime.MaxServerMessage)
	s := &Session{conn: conn, format: c.Format}
	if err := s.write(ctx, start); err != nil {
		return nil, s.fail(err)
	}
	typ, b, err := s.read(ctx)
	if err != nil {
		return nil, s.fail(err)
	}
	switch typ {
	case "session.started":
	case "error":
		ev, _ := ParseServerEvent(b)
		return nil, s.fail(fmt.Errorf("livews: session.start rejected: %s: %s", ev.ErrCode, ev.ErrMessage))
	case "session.closed":
		return nil, s.fail(errors.New("livews: session closed before startup"))
	default:
		return nil, s.fail(fmt.Errorf("%w: first server event %q, want session.started", realtime.ErrProtocol, typ))
	}
	var st struct {
		Session struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Audio *struct {
				Format map[string]any `json:"format"`
			} `json:"audio"`
			Delegation *struct {
				Type string `json:"type"`
			} `json:"delegation"`
		} `json:"session"`
	}
	if err := json.Unmarshal(b, &st); err != nil || st.Session.ID == "" {
		return nil, s.fail(fmt.Errorf("%w: session.started without session.id", realtime.ErrProtocol))
	}
	if st.Session.Model != "" && st.Session.Model != c.Model {
		return nil, s.fail(fmt.Errorf("%w: server started model %q, requested %q", realtime.ErrCapability, st.Session.Model, c.Model))
	}
	if st.Session.Audio != nil {
		want, _ := formatJSON(c.Format)
		got := st.Session.Audio.Format
		if got["type"] != want["type"] || fmt.Sprint(got["rate"]) != fmt.Sprint(want["rate"]) {
			return nil, s.fail(fmt.Errorf("%w: server audio format %v, requested %v", realtime.ErrCapability, got, want))
		}
	}
	if st.Session.Delegation != nil && st.Session.Delegation.Type != "client" {
		return nil, s.fail(fmt.Errorf("%w: server delegation %q, requested client", realtime.ErrCapability, st.Session.Delegation.Type))
	}
	s.SessionID = st.Session.ID
	s.ready = true
	return s, nil
}

func (s *Session) read(ctx context.Context) (string, []byte, error) {
	typ, b, err := s.conn.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	if typ != websocket.MessageText {
		return "", nil, fmt.Errorf("%w: binary frame", realtime.ErrProtocol)
	}
	var h struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &h); err != nil || h.Type == "" {
		return "", nil, fmt.Errorf("%w: message without type", realtime.ErrProtocol)
	}
	return h.Type, b, nil
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

// AppendAudio sends caller audio in the startup format.
func (s *Session) AppendAudio(ctx context.Context, b []byte) error {
	if !s.ready {
		return realtime.ErrNotReady
	}
	if _, err := s.format.Duration(len(b)); err != nil || len(b) == 0 {
		return fmt.Errorf("livews: invalid audio chunk of %d bytes", len(b))
	}
	msg, _ := json.Marshal(map[string]string{"type": "session.input_audio.append", "audio": base64.StdEncoding.EncodeToString(b)})
	return s.write(ctx, msg)
}

// DeclineDelegation answers a client delegation without any backend: the
// model is told the action is unavailable and speaks that, never a fabricated
// result. GPT-Live has no explicit decline event.
func (s *Session) DeclineDelegation(ctx context.Context, delegationID string) error {
	if !s.ready {
		return realtime.ErrNotReady
	}
	if delegationID == "" {
		return errors.New("livews: delegation id required")
	}
	msg, _ := json.Marshal(map[string]any{
		"type":          "session.commentary.append",
		"delegation_id": delegationID,
		"content":       "That action is not available on this call. Continue the conversation without it.",
	})
	return s.write(ctx, msg)
}

// Next returns the next normalized server event.
func (s *Session) Next(ctx context.Context) (realtime.Event, error) {
	_, b, err := s.read(ctx)
	if err != nil {
		return realtime.Event{}, realtime.Transport(err)
	}
	return ParseServerEvent(b)
}

// Close tears down the socket without the protocol close. Usage is then
// unconfirmed; use CloseGracefully whenever the provider is still reachable.
func (s *Session) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// RequestClose sends session.close without reading. The session's single
// reader then observes session.closed, so usage is confirmed without a second
// concurrent reader on the socket.
func (s *Session) RequestClose(ctx context.Context) error {
	return s.write(ctx, []byte(`{"type":"session.close","event_id":"ch_close"}`))
}

// CloseGracefully sends session.close and waits for session.closed so final
// usage is confirmed; the caller bounds ctx. It must not run concurrently with
// another reader.
func (s *Session) CloseGracefully(ctx context.Context) (realtime.Event, error) {
	if err := s.RequestClose(ctx); err != nil {
		return realtime.Event{}, err
	}
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			return realtime.Event{}, fmt.Errorf("livews: closed without session.closed, usage unconfirmed: %w", err)
		}
		if ev.Kind == realtime.EventClosed {
			_ = s.conn.Close(websocket.StatusNormalClosure, "")
			return ev, nil
		}
	}
}
