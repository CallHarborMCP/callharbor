package realtimews

import (
	"context"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

var _ realtime.Session = (*Session)(nil)

// Dialer adapts Connect to the protocol-neutral realtime.Dialer contract.
func Dialer(ctx context.Context, p realtime.Profile) (realtime.Session, error) {
	s, err := Connect(ctx, Config{
		Endpoint:     p.Endpoint,
		APIKey:       p.APIKey,
		Model:        p.Model,
		Voice:        p.Voice,
		Instructions: p.Instructions,
		Format:       p.Format,
		Headers:      p.Headers,
		HTTPClient:   p.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}
