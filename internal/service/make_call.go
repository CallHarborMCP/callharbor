package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

// Reason codes returned by make_call and POST /api/v1/calls. They are part of
// the public contract. Each is a fixed machine code in the error message.
const (
	ReasonAIProfileRequired    = "ai_profile_required"
	ReasonAIProfileAmbiguous   = "ai_profile_ambiguous"
	ReasonTelephonyNotVerified = "telephony_not_verified"
	ReasonTelephonyNoConnID    = "telephony_connection_missing"
	ReasonTelephonyNoWebhook   = "telephony_webhook_key_missing"
	ReasonIdempotencyConflict  = "idempotency_conflict"
)

// MakeCallResult is the accepted-call response. It is returned after the call
// row and brief are committed, before any provider dial.
type MakeCallResult struct {
	CallID    string `json:"call_id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// CallResolver resolves a telephony profile the actor may use, with the owner's
// keys. Only the call path reads keys.
type CallResolver interface {
	Resolve(ctx context.Context, actor telephonyprofile.Actor, id string) (telephonyprofile.Resolved, error)
}

// CallStore is the write and read repository MakeCall needs. It extends Store.
type CallStore interface {
	Store
	ListAIProfiles(ctx context.Context, ownerUserID string) ([]storage.AIProfile, error)
	InsertCall(ctx context.Context, c storage.CallInsert) error
	GetCallByIdempotencyKey(ctx context.Context, ownerUserID, key string) (storage.Call, error)
	GetCallBrief(ctx context.Context, ownerUserID, callID string) (storage.CallBrief, error)
	GetUserCallPolicy(ctx context.Context, userID string) (storage.UserCallPolicy, error)
	CallOwner(ctx context.Context, callID string) (string, error)
	RequestHangup(ctx context.Context, callID string, audit storage.AuditEvent, now time.Time) (storage.HangupDecision, error)
	InsertAudit(ctx context.Context, e storage.AuditEvent) error
}

// MakeCallInput is the validated-at-the-edge request. Field names follow the
// public make_call schema in api/mcp/tools.json. IdempotencyKey is supplied by
// the transport: the Idempotency-Key header on REST, the idempotency_key
// argument on MCP.
type MakeCallInput struct {
	ProfileID       string   `json:"profile_id"`
	To              string   `json:"to"`
	Recipient       string   `json:"recipient,omitempty"`
	Language        string   `json:"language,omitempty"`
	Objective       string   `json:"objective"`
	Context         string   `json:"context,omitempty"`
	Instructions    string   `json:"instructions,omitempty"`
	Constraints     []string `json:"constraints,omitempty"`
	Fallbacks       []string `json:"fallbacks,omitempty"`
	SuccessCriteria []string `json:"success_criteria,omitempty"`
	MaxDuration     *int     `json:"max_duration_seconds,omitempty"`
	Record          bool     `json:"record,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
}

// CallDeps configures the call write path.
type CallDeps struct {
	Resolver      CallResolver
	MaxBriefBytes int
	// Policy is the installation call policy from the environment.
	Policy config.CallPolicy
	Now    func() time.Time
	// NewCallID returns the new call id.
	NewCallID func() (string, error)
	// Notify, when set, runs after a new call is committed. The dial worker
	// uses it to wake early. It must not block.
	Notify func()
	// Hangup sends the hangup for a leg that has a control id, using the owner's
	// key. It is called after RequestHangup decides to send one. It is
	// best-effort and must not block. Nil means no hangup is sent.
	Hangup func(ctx context.Context, leg storage.LiveCallLeg) error
	// StopMedia cancels the running media session for a call, so its AI session
	// closes. Nil means no session is stopped.
	StopMedia func(callID string)
	// Logger records hangup send failures. Nil discards them. Only the call id
	// is logged: never a number, a key, or a provider response body.
	Logger *slog.Logger
}

// CallOperator holds MakeCall. REST and MCP both call it, so there is one
// implementation of the call write path.
type CallOperator struct {
	store CallStore
	deps  CallDeps
}

// NewCallOperator returns the call write path. Every dependency is required.
func NewCallOperator(store CallStore, deps CallDeps) (*CallOperator, error) {
	if store == nil || deps.Resolver == nil || deps.Now == nil || deps.NewCallID == nil || deps.MaxBriefBytes < 1 {
		return nil, errors.New("service: call operator requires a store, resolver, clock, id source and brief limit")
	}
	return &CallOperator{store: store, deps: deps}, nil
}

// MakeCall validates the request, then answers a repeated idempotency key from
// the stored call before any profile is resolved. A retry therefore returns the
// original call even if its telephony profile has since become unverified or
// its grant was revoked, because the call row belongs to the caller. Only a new
// call resolves profiles, checks readiness and selects the AI profile.
func (o *CallOperator) MakeCall(ctx context.Context, actor Actor, in MakeCallInput) (MakeCallResult, error) {
	if err := actor.Require(ScopeCall); err != nil {
		return MakeCallResult{}, err
	}
	if in.IdempotencyKey == "" {
		return MakeCallResult{}, invalidInput("idempotency_key is required")
	}
	if err := ValidateIdempotencyKey(in.IdempotencyKey); err != nil {
		return MakeCallResult{}, err
	}
	rendered, err := ValidateMakeCall(in, o.deps.MaxBriefBytes)
	if err != nil {
		return MakeCallResult{}, err
	}
	// requested is the request as sent, before any policy clamp. Replay
	// compares against it, so a retry is judged by what the caller asked for.
	requested := rendered

	existing, err := o.store.GetCallByIdempotencyKey(ctx, actor.UserID, in.IdempotencyKey)
	if err == nil {
		return o.replay(ctx, actor, existing, in.ProfileID, requested, in.MaxDuration != nil)
	}
	if !errors.Is(err, storage.ErrCallNotFound) {
		return MakeCallResult{}, internal(fmt.Errorf("lookup idempotency key: %w", err))
	}

	profile, err := o.deps.Resolver.Resolve(ctx, telephonyprofile.Actor{UserID: actor.UserID}, in.ProfileID)
	if errors.Is(err, telephonyprofile.ErrNotFound) {
		return MakeCallResult{}, newError(CodeNotFound, "telephony profile not found")
	}
	if err != nil {
		return MakeCallResult{}, internal(fmt.Errorf("resolve telephony profile: %w", err))
	}
	if err := checkTelephonyReady(profile); err != nil {
		return MakeCallResult{}, err
	}

	effective, err := o.effectivePolicy(ctx, actor.UserID)
	if err != nil {
		return MakeCallResult{}, err
	}
	rendered, err = checkCallPolicy(effective, rendered, in.MaxDuration != nil)
	if err != nil {
		return MakeCallResult{}, err
	}
	cost, err := costEstimate(effective, rendered.MaxDuration)
	if err != nil {
		return MakeCallResult{}, err
	}

	aiProfile, err := o.selectAIProfile(ctx, actor.UserID)
	if err != nil {
		return MakeCallResult{}, err
	}
	// Refuse the full session text now, not at media time. The profile's
	// instructions plus the brief and the notice must fit the limit, so an
	// oversize request is never queued. A later edit to the profile is still
	// checked when the media session starts.
	if _, err := RenderInstructions(aiProfile.Instructions, rendered.CanonicalJSON, o.deps.MaxBriefBytes); err != nil {
		if errors.Is(err, ErrInstructionsTooLarge) {
			return MakeCallResult{}, newError(CodeInvalidInput, fmt.Sprintf(
				"brief_too_large: the session instructions exceed %d bytes with the AI profile (CALLHARBOR_MAX_CALL_BRIEF_BYTES)",
				o.deps.MaxBriefBytes))
		}
		return MakeCallResult{}, internal(fmt.Errorf("render instructions: %w", err))
	}

	callID, err := o.deps.NewCallID()
	if err != nil {
		return MakeCallResult{}, internal(fmt.Errorf("new call id: %w", err))
	}
	now := o.deps.Now().UTC()
	insert := storage.CallInsert{
		ID:                 callID,
		OwnerUserID:        actor.UserID,
		TelephonyProfileID: profile.ID,
		AIProfileID:        aiProfile.ID,
		IdempotencyKey:     in.IdempotencyKey,
		ToNumber:           rendered.NormalizedTo,
		RecordRequested:    rendered.Record,
		MaxDurationSeconds: rendered.MaxDuration,
		BriefJSON:          rendered.CanonicalJSON,
		BriefSHA256:        rendered.SHA256,
		BriefVersion:       BriefVersion,
		CreatedAt:          now,
		Limits:             limitsFor(effective),
		Cost:               cost,
	}
	err = o.store.InsertCall(ctx, insert)
	if limitErr := mapLimitError(err); limitErr != nil {
		return MakeCallResult{}, limitErr
	}
	if errors.Is(err, storage.ErrCallIdempotencyConflict) {
		// A concurrent request with the same key won the insert. Compare the
		// winner with this request, exactly as a sequential retry would.
		winner, lookupErr := o.store.GetCallByIdempotencyKey(ctx, actor.UserID, in.IdempotencyKey)
		if lookupErr != nil {
			return MakeCallResult{}, internal(fmt.Errorf("re-read idempotency key: %w", lookupErr))
		}
		return o.replay(ctx, actor, winner, in.ProfileID, requested, in.MaxDuration != nil)
	}
	if err != nil {
		return MakeCallResult{}, internal(fmt.Errorf("insert call: %w", err))
	}
	created, err := formatTimestamp(now.Format(time.RFC3339Nano))
	if err != nil {
		return MakeCallResult{}, internal(err)
	}
	if o.deps.Notify != nil {
		o.deps.Notify()
	}
	return MakeCallResult{CallID: callID, Status: "queued", CreatedAt: created}, nil
}

// checkTelephonyReady refuses a profile that cannot place a call yet. Each
// refusal has its own reason code, so an operator knows what to fix.
func checkTelephonyReady(p telephonyprofile.Resolved) error {
	if p.ConnectivityStatus != storage.ConnectivityVerified {
		return newError(CodeConflict, ReasonTelephonyNotVerified+": validate the telephony profile before placing calls")
	}
	if p.ConnectionID == "" {
		return newError(CodeConflict, ReasonTelephonyNoConnID+": the telephony profile has no connection id")
	}
	if p.WebhookPublicKey == "" {
		return newError(CodeConflict, ReasonTelephonyNoWebhook+": the telephony profile has no webhook public key")
	}
	return nil
}

// selectAIProfile returns the caller's only AI profile. Zero or several are
// refused. No input field selects one in this version.
func (o *CallOperator) selectAIProfile(ctx context.Context, userID string) (storage.AIProfile, error) {
	profiles, err := o.store.ListAIProfiles(ctx, userID)
	if err != nil {
		return storage.AIProfile{}, internal(fmt.Errorf("list ai profiles: %w", err))
	}
	switch len(profiles) {
	case 0:
		return storage.AIProfile{}, newError(CodeConflict, ReasonAIProfileRequired+": create an AI profile before placing a call")
	case 1:
		return profiles[0], nil
	default:
		return storage.AIProfile{}, newError(CodeConflict, ReasonAIProfileAmbiguous+": more than one AI profile exists; keep exactly one")
	}
}

// replay answers a repeated key from the stored call. The comparison uses only
// caller-supplied, normalized inputs: the telephony profile id, the destination,
// the brief digest (which covers the caller's brief fields), the recording flag
// and the effective time limit. The server-selected AI profile is not compared,
// so changing AI profiles between attempts cannot turn a retry into a conflict.
// A mismatch is a 409 and never returns the other request's call.
//
// An omitted max_duration_seconds is not compared. The stored duration may be
// clamped to the policy in force when the call was first queued, and the retry
// must not conflict with that. An explicit duration is never clamped (it is
// refused instead), so it is compared exactly.
func (o *CallOperator) replay(ctx context.Context, actor Actor, existing storage.Call, profileID string, r RenderedBrief, explicitDuration bool) (MakeCallResult, error) {
	stored, err := o.store.GetCallBrief(ctx, actor.UserID, existing.ID)
	if err != nil {
		return MakeCallResult{}, internal(fmt.Errorf("read original brief: %w", err))
	}
	sameDuration := !explicitDuration || existing.MaxDurationSeconds == r.MaxDuration
	same := stored.SHA256 == r.SHA256 &&
		existing.ToNumber == r.NormalizedTo &&
		existing.RecordRequested == r.Record &&
		sameDuration &&
		existing.TelephonyProfileID == profileID
	if !same {
		return MakeCallResult{}, newError(CodeConflict, ReasonIdempotencyConflict+": this idempotency key was used for a different request")
	}
	created, err := formatTimestamp(existing.CreatedAt)
	if err != nil {
		return MakeCallResult{}, internal(err)
	}
	return MakeCallResult{CallID: existing.ID, Status: existing.Status, CreatedAt: created}, nil
}

// NewCallID returns an unguessable call id: "call_" and 16 random bytes in
// URL-safe base64.
func NewCallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "call_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
