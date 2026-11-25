package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	// DefaultLimit is the page size used when the caller sends no limit.
	DefaultLimit = 50
	// MinLimit is the smallest page size a caller may request.
	MinLimit = 1
	// MaxLimit is the largest page size a caller may request.
	MaxLimit = 100
	// MaxIDLength matches the storage identifier bound.
	MaxIDLength = 64
	cursorV1    = 1
)

// NormalizeLimit applies the default and enforces the bounds. Zero means
// "not supplied". Any other value outside 1..MaxLimit is invalid input.
func NormalizeLimit(n int) (int, error) {
	switch {
	case n == 0:
		return DefaultLimit, nil
	case n < 1 || n > MaxLimit:
		return 0, invalidInput("limit must be between 1 and %d", MaxLimit)
	default:
		return n, nil
	}
}

// cursorPayload is the decoded form of an opaque list cursor. The keyset is
// (created_at DESC, id DESC) of the last item on the previous page.
type cursorPayload struct {
	V int    `json:"v"`
	C string `json:"c"`
	I string `json:"i"`
}

// encodeCursor returns the opaque base64url token for a keyset position.
func encodeCursor(c storage.CallCursor) string {
	raw, _ := json.Marshal(cursorPayload{V: cursorV1, C: c.CreatedAt, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a cursor. An empty token means the first page and
// returns nil. Any malformed or tampered token is invalid input.
func decodeCursor(token string) (*storage.CallCursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, invalidInput("cursor is malformed")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p cursorPayload
	if err := dec.Decode(&p); err != nil {
		return nil, invalidInput("cursor is malformed")
	}
	if dec.More() {
		return nil, invalidInput("cursor is malformed")
	}
	if _, err := dec.Token(); err == nil {
		return nil, invalidInput("cursor is malformed")
	}
	if p.V != cursorV1 || p.I == "" || len(p.I) > MaxIDLength {
		return nil, invalidInput("cursor is malformed")
	}
	if _, err := time.Parse(time.RFC3339Nano, p.C); err != nil {
		return nil, invalidInput("cursor is malformed")
	}
	return &storage.CallCursor{CreatedAt: p.C, ID: p.I}, nil
}
