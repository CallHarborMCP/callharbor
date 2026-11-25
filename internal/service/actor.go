package service

// Scopes this service enforces. The OAuth server grants the same strings. They
// are defined here so the service does not import the oauth package.
const (
	ScopeRead = "mcp:read"
	ScopeCall = "mcp:call"
)

// Actor is the authenticated caller of one operation. Only a transport adapter
// builds it, from a validated session or OAuth principal, never from input.
type Actor struct {
	UserID string
	Scopes []string
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
