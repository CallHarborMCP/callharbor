package oauth

import (
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

// TestScopeConstantsMatchService keeps the OAuth scope names and the service's
// authorization scope names equal. The service enforces the scopes it defines,
// and oauth grants and checks the scopes it defines. If the two ever differ, a
// token would be issued with a scope the service never accepts, or the route
// would check a scope nothing grants.
func TestScopeConstantsMatchService(t *testing.T) {
	if ScopeRead != service.ScopeRead {
		t.Fatalf("oauth.ScopeRead = %q, service.ScopeRead = %q", ScopeRead, service.ScopeRead)
	}
	if ScopeCall != service.ScopeCall {
		t.Fatalf("oauth.ScopeCall = %q, service.ScopeCall = %q", ScopeCall, service.ScopeCall)
	}
}
