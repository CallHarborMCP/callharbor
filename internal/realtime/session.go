package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrDisconnected reports that the provider connection ended. Callers must
// treat the AI leg as unknown and reconcile, never as a clean finish.
var ErrDisconnected = errors.New("realtime: provider disconnected")

// Session is the protocol-neutral surface of a started voice session. Both
// primary WebSocket adapters implement it; the call engine depends only on
// this interface and on Event, never on provider JSON.
type Session interface {
	AppendAudio(ctx context.Context, b []byte) error
	Next(ctx context.Context) (Event, error)
	Close() error
}

// MaxHeaders bounds the number of user-configured provider headers.
const MaxHeaders = 16

// MaxHeaderValue bounds one user-configured provider header value.
const MaxHeaderValue = 1024

var reservedHeaders = map[string]bool{
	"Authorization":            true,
	"Connection":               true,
	"Content-Length":           true,
	"Host":                     true,
	"Sec-Websocket-Accept":     true,
	"Sec-Websocket-Extensions": true,
	"Sec-Websocket-Key":        true,
	"Sec-Websocket-Protocol":   true,
	"Sec-Websocket-Version":    true,
	"Te":                       true,
	"Trailer":                  true,
	"Transfer-Encoding":        true,
	"Upgrade":                  true,
}

// ValidateHeaders converts user-configured provider headers into a fresh
// http.Header. Authorization is reserved for the profile API key, and
// WebSocket and hop-by-hop headers are rejected so a profile cannot alter the
// upgrade handshake.
func ValidateHeaders(in map[string]string) (http.Header, error) {
	if len(in) > MaxHeaders {
		return nil, fmt.Errorf("realtime: at most %d custom headers, got %d", MaxHeaders, len(in))
	}
	out := http.Header{}
	for name, value := range in {
		if !isToken(name) {
			return nil, fmt.Errorf("realtime: invalid header name %q", name)
		}
		canon := http.CanonicalHeaderKey(name)
		if reservedHeaders[canon] {
			return nil, fmt.Errorf("realtime: header %q is reserved", canon)
		}
		if len(value) > MaxHeaderValue || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("realtime: invalid value for header %q", canon)
		}
		out.Set(canon, value)
	}
	return out, nil
}

// Transport classifies a socket read error. Caller cancellation and deadlines
// are returned unchanged because they are not provider failures. Every other
// transport error means the provider leg is gone and wraps ErrDisconnected.
func Transport(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, ErrProtocol) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrDisconnected, err)
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}
