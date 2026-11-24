package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// Profile is the per-user AI voice configuration. Protocol is always explicit;
// it is never inferred from Model or Endpoint.
type Profile struct {
	Protocol     Protocol
	Endpoint     string
	APIKey       string
	Model        string
	Voice        string
	Instructions string
	Headers      map[string]string
	Format       audio.Format
	HTTPClient   *http.Client
}

// ErrNoAdapter reports a protocol with no registered adapter. Selection never
// falls back to another protocol.
var ErrNoAdapter = errors.New("realtime: no adapter for protocol")

// Dialer starts a session for one protocol. Each adapter package registers
// one; the call engine receives only the Session it returns.
type Dialer func(ctx context.Context, p Profile) (Session, error)

// Dialers maps each supported protocol to its adapter. A protocol missing here
// fails closed.
type Dialers map[Protocol]Dialer

// Dial validates the profile against the protocol's capability matrix, then
// starts the session with the adapter for exactly p.Protocol.
func (d Dialers) Dial(ctx context.Context, p Profile, need Requirements) (Session, error) {
	caps, err := For(p.Protocol)
	if err != nil {
		return nil, err
	}
	if err := caps.Check(need); err != nil {
		return nil, err
	}
	dial, ok := d[p.Protocol]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrNoAdapter, p.Protocol)
	}
	return dial(ctx, p)
}
