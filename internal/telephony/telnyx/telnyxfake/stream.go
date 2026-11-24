package telnyxfake

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// Stream is the Telnyx side of one bidirectional media WebSocket. It dials
// CallHarbor's media endpoint and signs the upgrade exactly as Telnyx does.
type Stream struct {
	conn *websocket.Conn
}

// DialMedia opens a signed media stream to streamURL. The signature covers the
// exact streamURL, so the server must be configured with the same string.
func DialMedia(ctx context.Context, streamURL string, key ed25519.PrivateKey, now time.Time) (*Stream, error) {
	h := telnyx.Sign(key, now, []byte(streamURL))
	c, resp, err := websocket.Dial(ctx, streamURL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("telnyxfake: dial media: %w", err)
	}
	c.SetReadLimit(telnyx.MaxMediaFrame)
	return &Stream{conn: c}, nil
}

// Send writes one client→CallHarbor text frame.
func (s *Stream) Send(ctx context.Context, frame []byte) error {
	return s.conn.Write(ctx, websocket.MessageText, frame)
}

// Recv reads one CallHarbor→Telnyx text frame.
func (s *Stream) Recv(ctx context.Context) ([]byte, error) {
	typ, b, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("telnyxfake: unexpected binary frame from CallHarbor")
	}
	return b, nil
}

// Close closes the media stream.
func (s *Stream) Close() error {
	return s.conn.CloseNow()
}
