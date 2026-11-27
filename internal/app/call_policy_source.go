package app

import (
	"context"

	"github.com/CallHarborMCP/callharbor/internal/callpolicy"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// userPolicyReader is the storage read the dial-time policy needs.
type userPolicyReader interface {
	GetUserCallPolicy(ctx context.Context, userID string) (storage.UserCallPolicy, error)
}

// storedCallPolicy reads the owner's current overrides and resolves them with
// the installation policy. The worker calls it before every dial, so the
// decision uses the policy in force at dial time.
type storedCallPolicy struct {
	store   userPolicyReader
	install config.CallPolicy
}

// CallPolicy returns the effective policy for ownerUserID.
func (s storedCallPolicy) CallPolicy(ctx context.Context, ownerUserID string) (callpolicy.Effective, error) {
	user, err := s.store.GetUserCallPolicy(ctx, ownerUserID)
	if err != nil {
		return callpolicy.Effective{}, err
	}
	return callpolicy.Resolve(s.install, user), nil
}
