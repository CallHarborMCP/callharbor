package service

import (
	"context"

	"github.com/CallHarborMCP/callharbor/internal/oauth"
)

// Channel names the transport an operation arrived through.
type Channel string

const (
	ChannelREST Channel = "rest"
	ChannelMCP  Channel = "mcp"
)

// Scopes this service enforces. They are the same strings the OAuth server
// grants.
const (
	ScopeRead = oauth.ScopeRead
	ScopeCall = oauth.ScopeCall
)

// Actor is the authenticated caller of one operation. Only the transport
// adapter builds it, from a validated session or OAuth principal.
type Actor struct {
	UserID   string
	Scopes   []string
	ClientID string
	Channel  Channel
}

// Require returns an error unless the actor is authenticated and holds scope.
func (a Actor) Require(scope string) error {
	if a.UserID == "" {
		return newError(CodeUnauthenticated, "authentication required")
	}
	for _, s := range a.Scopes {
		if s == scope {
			return nil
		}
	}
	return newError(CodeInsufficientScope, "missing scope "+scope)
}

// actorKey carries an Actor through a context for transports that cannot pass
// it explicitly. Operations still receive the actor as an argument.
type actorKey struct{}

// WithActor returns a context that carries actor.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the actor stored by WithActor, if any.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}
