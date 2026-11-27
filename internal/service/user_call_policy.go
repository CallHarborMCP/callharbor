package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/callpolicy"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// PolicyStore is the repository the per-user call policy operations need.
type PolicyStore interface {
	GetUserCallPolicy(ctx context.Context, userID string) (storage.UserCallPolicy, error)
	UpsertUserCallPolicy(ctx context.Context, p storage.UserCallPolicy, audit storage.AuditEvent) error
	GetUserByID(ctx context.Context, id string) (storage.User, error)
	InsertAudit(ctx context.Context, e storage.AuditEvent) error
}

// UserCallPolicyInput is the administrator's write. A nil numeric field
// inherits the installation value. Prefix lists are replaced as a whole.
type UserCallPolicyInput struct {
	MaxConcurrentCalls *int64   `json:"max_concurrent_calls"`
	MaxCallsPerHour    *int64   `json:"max_calls_per_hour"`
	MaxDurationSeconds *int64   `json:"max_duration_seconds"`
	MaxCostAmount      *string  `json:"max_cost_amount"`
	AllowPrefixes      []string `json:"allow_prefixes"`
	DenyPrefixes       []string `json:"deny_prefixes"`
}

// UserCallPolicyView is what the API shows for one user's policy: the stored
// overrides and the effective result they produce with the installation policy.
type UserCallPolicyView struct {
	UserID    string              `json:"user_id"`
	Overrides UserCallPolicyInput `json:"overrides"`
	Effective EffectivePolicyView `json:"effective"`
	UpdatedAt string              `json:"updated_at,omitempty"`
	UpdatedBy string              `json:"updated_by,omitempty"`
}

// EffectivePolicyView is the policy that applies to a user right now. Money is
// a decimal string in the cost currency, never a float.
type EffectivePolicyView struct {
	MaxConcurrentCalls    int      `json:"max_concurrent_calls"`
	MaxCallsPerHour       int      `json:"max_calls_per_hour"`
	MaxDurationSeconds    int      `json:"max_duration_seconds"`
	MaxCostAmount         *string  `json:"max_cost_amount"`
	CostCurrency          string   `json:"cost_currency,omitempty"`
	EstimateRatePerMinute *string  `json:"estimate_rate_per_minute"`
	AllowPrefixes         []string `json:"allow_prefixes"`
	DenyPrefixes          []string `json:"deny_prefixes"`
}

// maxCostDecimalLimit bounds the per-user cost override to the same range the
// installation accepts, so an override cannot be larger than the schema allows.
const maxCostDecimalLimit = 1_000_000_000_000

// GetUserCallPolicyForAdmin returns one user's policy to an administrator. A
// non-admin is refused with forbidden, and no other user's data is read.
func (s *Service) GetUserCallPolicyForAdmin(ctx context.Context, actor Actor, userID string) (UserCallPolicyView, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return UserCallPolicyView{}, err
	}
	if !actor.Admin {
		return UserCallPolicyView{}, newError(CodeForbidden, "administrator role required")
	}
	return s.policyView(ctx, userID)
}

// PutUserCallPolicy stores an administrator's override for one user. Every
// value is validated before the write. The write and its audit event share one
// transaction, so an unaudited change is never stored.
func (s *Service) PutUserCallPolicy(ctx context.Context, actor Actor, userID string, in UserCallPolicyInput) (UserCallPolicyView, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return UserCallPolicyView{}, err
	}
	if !actor.Admin {
		s.auditPolicy(ctx, actor, userID, "denied")
		return UserCallPolicyView{}, newError(CodeForbidden, "administrator role required")
	}
	if s.policies == nil {
		return UserCallPolicyView{}, internal(errors.New("user call policy store not configured"))
	}
	p, err := s.validateUserPolicy(ctx, userID, in)
	if err != nil {
		s.auditPolicy(ctx, actor, userID, "denied")
		return UserCallPolicyView{}, err
	}
	p.UpdatedAt = s.now().UTC().Format("2006-01-02T15:04:05.000Z")
	p.UpdatedBy = actor.UserID
	if err := s.policies.UpsertUserCallPolicy(ctx, p, s.policyAudit(actor, userID, "success")); err != nil {
		return UserCallPolicyView{}, internal(fmt.Errorf("store user call policy: %w", err))
	}
	return s.policyView(ctx, userID)
}

// GetOwnCallPolicy returns the caller's own effective policy. It never reveals
// another user's overrides.
func (s *Service) GetOwnCallPolicy(ctx context.Context, actor Actor) (EffectivePolicyView, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return EffectivePolicyView{}, err
	}
	view, err := s.policyView(ctx, actor.UserID)
	if err != nil {
		return EffectivePolicyView{}, err
	}
	return view.Effective, nil
}

// policyView reads a user's stored overrides and their effective policy.
func (s *Service) policyView(ctx context.Context, userID string) (UserCallPolicyView, error) {
	if s.policies == nil {
		return UserCallPolicyView{}, internal(errors.New("user call policy store not configured"))
	}
	stored, err := s.policies.GetUserCallPolicy(ctx, userID)
	if err != nil {
		return UserCallPolicyView{}, internal(fmt.Errorf("read user call policy: %w", err))
	}
	eff := callpolicy.Resolve(s.policyInstall, stored)
	view := UserCallPolicyView{
		UserID: userID,
		Overrides: UserCallPolicyInput{
			MaxConcurrentCalls: stored.MaxConcurrentCalls,
			MaxCallsPerHour:    stored.MaxCallsPerHour,
			MaxDurationSeconds: stored.MaxDurationSeconds,
			AllowPrefixes:      stored.AllowPrefixes,
			DenyPrefixes:       stored.DenyPrefixes,
		},
		Effective: effectiveView(eff),
		UpdatedAt: stored.UpdatedAt,
		UpdatedBy: stored.UpdatedBy,
	}
	if stored.MaxCostMicros != nil {
		amount := config.FormatMicros(*stored.MaxCostMicros)
		view.Overrides.MaxCostAmount = &amount
	}
	return view, nil
}

// effectiveView renders the effective policy with decimal money strings.
func effectiveView(e callpolicy.Effective) EffectivePolicyView {
	v := EffectivePolicyView{
		MaxConcurrentCalls: e.MaxConcurrentCalls,
		MaxCallsPerHour:    e.MaxCallsPerHour,
		MaxDurationSeconds: e.MaxDurationSeconds,
		CostCurrency:       e.CostCurrency,
		AllowPrefixes:      flattenAllow(e.AllowGroups),
		DenyPrefixes:       nonNilPrefixes(e.DenyPrefixes),
	}
	if e.MaxCostMicros != nil {
		amount := config.FormatMicros(*e.MaxCostMicros)
		v.MaxCostAmount = &amount
	}
	if e.EstimateMicrosPerMinute != nil {
		rate := config.FormatMicros(*e.EstimateMicrosPerMinute)
		v.EstimateRatePerMinute = &rate
	}
	return v
}

// flattenAllow lists every allow prefix that applies. Each group is a separate
// requirement, so the view shows them all; an empty result means no allow list.
func flattenAllow(groups [][]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return nonNilPrefixes(out)
}

func nonNilPrefixes(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// validateUserPolicy checks an administrator's write and converts it to the
// storage form. It refuses values the installation cannot honour and any user
// that does not exist.
func (s *Service) validateUserPolicy(ctx context.Context, userID string, in UserCallPolicyInput) (storage.UserCallPolicy, error) {
	if _, err := s.policies.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return storage.UserCallPolicy{}, newError(CodeNotFound, "user not found")
		}
		return storage.UserCallPolicy{}, internal(fmt.Errorf("read user: %w", err))
	}
	if in.MaxConcurrentCalls != nil && (*in.MaxConcurrentCalls < 1 || *in.MaxConcurrentCalls > 1000) {
		return storage.UserCallPolicy{}, invalidInput("max_concurrent_calls must be between 1 and 1000")
	}
	if in.MaxCallsPerHour != nil && (*in.MaxCallsPerHour < 1 || *in.MaxCallsPerHour > 10000) {
		return storage.UserCallPolicy{}, invalidInput("max_calls_per_hour must be between 1 and 10000")
	}
	if in.MaxDurationSeconds != nil && (*in.MaxDurationSeconds < 30 || *in.MaxDurationSeconds > 7200) {
		return storage.UserCallPolicy{}, invalidInput("max_duration_seconds must be between 30 and 7200")
	}
	p := storage.UserCallPolicy{
		UserID:             userID,
		MaxConcurrentCalls: in.MaxConcurrentCalls,
		MaxCallsPerHour:    in.MaxCallsPerHour,
		MaxDurationSeconds: in.MaxDurationSeconds,
	}
	if in.MaxCostAmount != nil {
		micros, err := config.ParseMicros(*in.MaxCostAmount)
		if err != nil || micros/config.MicrosPerUnit > maxCostDecimalLimit {
			return storage.UserCallPolicy{}, invalidInput("max_cost_amount must be a decimal amount such as 1.50 with at most 6 decimal places")
		}
		p.MaxCostMicros = &micros
	}
	if err := callpolicy.ValidatePrefixes(in.AllowPrefixes); err != nil {
		return storage.UserCallPolicy{}, invalidInput("allow_prefixes: %s", err.Error())
	}
	if err := callpolicy.ValidatePrefixes(in.DenyPrefixes); err != nil {
		return storage.UserCallPolicy{}, invalidInput("deny_prefixes: %s", err.Error())
	}
	p.AllowPrefixes = in.AllowPrefixes
	p.DenyPrefixes = in.DenyPrefixes
	return p, nil
}

// policyAudit builds the audit event for a policy write or refusal.
func (s *Service) policyAudit(actor Actor, targetUserID, outcome string) storage.AuditEvent {
	detail, _ := json.Marshal(map[string]string{"outcome": outcome})
	return storage.AuditEvent{
		OccurredAt:   s.now().UTC().Format("2006-01-02T15:04:05.000Z"),
		ActorUserID:  actor.UserID,
		ActorRole:    roleOf(actor),
		TargetUserID: targetUserID,
		Action:       "user_call_policy.update",
		ResourceType: "user_call_policy",
		ResourceID:   targetUserID,
		Outcome:      outcome,
		DetailJSON:   string(detail),
	}
}

func roleOf(a Actor) string {
	if a.Admin {
		return "admin"
	}
	return "user"
}

// auditPolicy records a refused policy write. A failed audit write does not
// change the refusal the caller receives; it is logged by the transport.
func (s *Service) auditPolicy(ctx context.Context, actor Actor, targetUserID, outcome string) {
	if s.policies == nil {
		return
	}
	_ = s.policies.InsertAudit(ctx, s.policyAudit(actor, targetUserID, outcome))
}
