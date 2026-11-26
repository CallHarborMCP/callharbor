// Package telephonyprofile manages per-user Telnyx telephony profiles: their
// encrypted credentials, read-only validation, and owner-granted use. Every
// operation acts on the authenticated principal only. Callers never supply the
// owner, and no operation reveals a secret value.
package telephonyprofile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// Validation reason codes. They are fixed machine codes. Provider text is never
// stored or returned.
const (
	ReasonNone                = ""
	ReasonSecretsUnavailable  = "secrets_unavailable"
	ReasonInvalidCredentials  = "invalid_credentials" // #nosec G101 -- fixed reason code, not a credential
	ReasonApplicationNotFound = "application_not_found"
	ReasonApplicationInactive = "application_inactive"
	ReasonNumberNotFound      = "number_not_found"
	ReasonNumberInactive      = "number_inactive"
	ReasonNumberNotOnApp      = "number_not_on_application"
	ReasonProviderUnreachable = "provider_unreachable"
	validationTimeout         = 10 * time.Second
	maxName                   = 128
	maxConnectionID           = 128
	maxSecretPlaintext        = secrets.MaxPlaintext
	activeNumberStatus        = "active"
	providerNameTelnyx        = "telnyx"
)

// Sentinel errors. Transports map them to response codes. None carries provider
// text or secret material.
var (
	ErrNotFound     = errors.New("telephonyprofile: not found")
	ErrForbidden    = errors.New("telephonyprofile: forbidden")
	ErrInvalidInput = errors.New("telephonyprofile: invalid input")
	ErrConflict     = errors.New("telephonyprofile: conflict")
	ErrInUse        = errors.New("telephonyprofile: profile referenced by calls")
	ErrSecretsDown  = errors.New("telephonyprofile: secrets unavailable")
	// ErrValidationRace reports that the profile changed while it was being
	// validated, so the result was not stored. The caller may validate again.
	ErrValidationRace = errors.New("telephonyprofile: changed during validation")
)

// raceAuditDetail is the fixed audit detail for a validation that lost a race
// with an edit. It is a constant, so no input or provider text reaches the row.
const raceAuditDetail = `{"reason":"changed_during_validation"}`

var (
	e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// Store is the owner-scoped persistence the service needs.
type Store interface {
	CreateTelephonyProfile(ctx context.Context, p storage.TelephonyProfileRecord) error
	GetTelephonyProfile(ctx context.Context, ownerUserID, id string) (storage.TelephonyProfileRecord, error)
	ListUsableTelephonyProfiles(ctx context.Context, userID string) ([]storage.UsableTelephonyProfile, error)
	ResolveUsableTelephonyProfile(ctx context.Context, userID, profileID string) (storage.UsableTelephonyProfile, error)
	UpdateTelephonyProfile(ctx context.Context, p storage.TelephonyProfileRecord) error
	DeleteTelephonyProfile(ctx context.Context, ownerUserID, id string) error
	DeleteOwnedSecret(ctx context.Context, ownerUserID, id string) error
	SetTelephonyConnectivity(ctx context.Context, ownerUserID, id, expectedUpdatedAt, status, reason, checkedAt string) error
	UpsertTelephonyGrant(ctx context.Context, g storage.TelephonyGrant) error
	DeleteTelephonyGrant(ctx context.Context, profileID, granteeUserID string) error
	ListTelephonyGrants(ctx context.Context, profileID string) ([]storage.TelephonyGrant, error)
	GetUserByID(ctx context.Context, id string) (storage.User, error)
	InsertAudit(ctx context.Context, e storage.AuditEvent) error
}

// SecretWriter stores encrypted secrets for an owner.
type SecretWriter interface {
	Put(ctx context.Context, owner, name, purpose string, plaintext []byte) (string, error)
	Resolve(ctx context.Context, owner, id, purpose string) ([]byte, error)
}

// ProviderFactory builds a read-only Telnyx client for one plaintext key.
type ProviderFactory func(apiKey string) (Provider, error)

// Provider is the read-only subset of the Telnyx REST client that validation
// uses. It never dials.
type Provider interface {
	GetCallControlApplication(ctx context.Context, id string) (telnyx.CallControlApplication, error)
	FindPhoneNumber(ctx context.Context, e164 string) (telnyx.PhoneNumber, error)
}

// Service is the telephony profile domain service.
type Service struct {
	store    Store
	secrets  SecretWriter
	provider ProviderFactory
	now      func() time.Time
	newID    func() string
	logger   *slog.Logger
}

// New builds the service. Every dependency must be non-nil.
func New(store Store, sec SecretWriter, provider ProviderFactory, now func() time.Time, newID func() string, logger *slog.Logger) (*Service, error) {
	if store == nil || sec == nil || provider == nil || now == nil || newID == nil {
		return nil, errors.New("telephonyprofile: missing dependency")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, secrets: sec, provider: provider, now: now, newID: newID, logger: logger}, nil
}

// Actor is the authenticated caller. Only a transport builds it, from a
// validated session.
type Actor struct {
	UserID string
	Admin  bool
}

// Input is the write payload for create and replace. Secret fields are
// plaintext and are written once; they are never returned.
type Input struct {
	Name         string
	CallerNumber string
	ConnectionID string
	APIKey       string
	// WebhookPublicKey stores a new webhook key. Empty keeps the stored key.
	WebhookPublicKey string
	// ClearWebhookPublicKey removes the stored webhook key on update. It is set
	// only for an explicit JSON null, so an empty string never removes a key.
	ClearWebhookPublicKey bool
}

// View is one profile as the API returns it. Secret references never appear.
type View struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Provider           string `json:"provider"`
	CallerNumber       string `json:"caller_number"`
	ConnectionID       string `json:"connection_id,omitempty"`
	ConnectivityStatus string `json:"connectivity_status"`
	ConnectivityReason string `json:"connectivity_reason"`
	Access             string `json:"access"`
}

// Validation is the result of one read-only check.
type Validation struct {
	ProfileID          string `json:"profile_id"`
	ConnectivityStatus string `json:"connectivity_status"`
	ConnectivityReason string `json:"connectivity_reason"`
	CheckedAt          string `json:"checked_at"`
}

// Grant is one owner-granted use of a profile.
type Grant struct {
	UserID    string `json:"user_id"`
	GrantedBy string `json:"granted_by"`
	CreatedAt string `json:"created_at"`
}

func (a Actor) valid() error {
	if a.UserID == "" {
		return ErrForbidden
	}
	return nil
}

func validID(id string) bool { return idPattern.MatchString(id) }

// validateInput checks the non-secret fields and the presence of the API key.
// The key length is checked when it is sealed.
func validateInput(in Input, create bool) error {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxName {
		return fmt.Errorf("%w: name", ErrInvalidInput)
	}
	if !e164Pattern.MatchString(in.CallerNumber) {
		return fmt.Errorf("%w: caller_number must be E.164", ErrInvalidInput)
	}
	// connection_id is required: calls dial through this Call Control
	// application, so a profile without one can never place a call.
	if in.ConnectionID == "" || len(in.ConnectionID) > maxConnectionID {
		return fmt.Errorf("%w: connection_id required, at most %d bytes", ErrInvalidInput, maxConnectionID)
	}
	if create && in.APIKey == "" {
		return fmt.Errorf("%w: api_key required", ErrInvalidInput)
	}
	if len(in.APIKey) > maxSecretPlaintext {
		return fmt.Errorf("%w: api_key too long", ErrInvalidInput)
	}
	if in.WebhookPublicKey != "" {
		if err := checkPublicKey(in.WebhookPublicKey); err != nil {
			return err
		}
	}
	return nil
}

// checkPublicKey accepts the base64 raw 32-byte Ed25519 key that Telnyx shows.
func checkPublicKey(b64 string) error {
	if len(b64) > maxSecretPlaintext {
		return fmt.Errorf("%w: webhook_public_key too long", ErrInvalidInput)
	}
	if _, err := telnyx.ParsePublicKey(b64); err != nil {
		return fmt.Errorf("%w: webhook_public_key must be a base64 Ed25519 key", ErrInvalidInput)
	}
	return nil
}

func toView(r storage.TelephonyProfileRecord, access string) View {
	return View{
		ID: r.ID, Name: r.Name, Provider: r.Provider, CallerNumber: r.CallerNumber,
		ConnectionID: r.ConnectionID, ConnectivityStatus: r.ConnectivityStatus,
		ConnectivityReason: r.ConnectivityReason, Access: access,
	}
}

func (s *Service) now3339() string {
	return s.now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// audit records one owner-scoped action. It stores no secret material and no
// provider text, only the action, the profile id, the outcome and a fixed
// detail string. outcome must be one of the audit_events CHECK values:
// success, denied or failed.
func (s *Service) audit(ctx context.Context, actor Actor, action, profileID, outcome string) {
	s.writeAudit(ctx, actor, action, profileID, "", outcome, "{}")
}

// auditTarget records a grant or revoke. targetUserID names the grantee.
func (s *Service) auditTarget(ctx context.Context, actor Actor, action, profileID, targetUserID string) {
	s.writeAudit(ctx, actor, action, profileID, targetUserID, "success", "{}")
}

func (s *Service) writeAudit(ctx context.Context, actor Actor, action, profileID, targetUserID, outcome, detail string) {
	if err := s.store.InsertAudit(ctx, storage.AuditEvent{
		OccurredAt: s.now3339(), ActorUserID: actor.UserID, ActorRole: roleOf(actor),
		TargetUserID: targetUserID, ResourceType: "telephony_profile", ResourceID: profileID,
		Action: action, Outcome: outcome, DetailJSON: detail,
	}); err != nil {
		s.logger.Error("audit write failed", "action", action, "err", err.Error())
	}
}

func roleOf(a Actor) string {
	if a.Admin {
		return "admin"
	}
	return "user"
}

// Create stores a new profile for the actor. The API key is encrypted before
// the profile row is written. A failed insert removes the secrets it wrote.
// Creating a profile never validates it; it starts as unknown.
func (s *Service) Create(ctx context.Context, actor Actor, in Input) (View, error) {
	if err := actor.valid(); err != nil {
		return View{}, err
	}
	if err := validateInput(in, true); err != nil {
		return View{}, err
	}
	if in.ClearWebhookPublicKey {
		// Removal means something only for an existing profile. On create there
		// is no stored key to remove, so null is refused instead of ignored.
		return View{}, fmt.Errorf("%w: webhook_public_key cannot be null on create", ErrInvalidInput)
	}
	id := s.newID()
	apiSecret, err := s.putSecret(ctx, actor.UserID, "telnyx-api-"+id, secrets.PurposeTelnyxAPIKey, in.APIKey)
	if err != nil {
		return View{}, err
	}
	webhookSecret := ""
	if in.WebhookPublicKey != "" {
		webhookSecret, err = s.putSecret(ctx, actor.UserID, "telnyx-webhook-"+id, secrets.PurposeTelnyxWebhookKey, in.WebhookPublicKey)
		if err != nil {
			s.dropSecret(ctx, actor.UserID, apiSecret)
			return View{}, err
		}
	}
	now := s.now3339()
	rec := storage.TelephonyProfileRecord{
		ID: id, OwnerUserID: actor.UserID, Name: strings.TrimSpace(in.Name), Provider: providerNameTelnyx,
		CallerNumber: in.CallerNumber, ConnectionID: in.ConnectionID, APISecretID: apiSecret,
		WebhookPublicKeySecretID: webhookSecret, ConnectivityStatus: storage.ConnectivityUnknown,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateTelephonyProfile(ctx, rec); err != nil {
		s.dropSecret(ctx, actor.UserID, apiSecret)
		s.dropSecret(ctx, actor.UserID, webhookSecret)
		if errors.Is(err, storage.ErrConflict) {
			return View{}, fmt.Errorf("%w: name already used", ErrConflict)
		}
		return View{}, err
	}
	s.audit(ctx, actor, "telephony_profile.create", id, "success")
	return toView(rec, access(actor.UserID, rec.OwnerUserID)), nil
}

// Update replaces the settings of a profile the actor owns. Omitted secret
// values keep the stored secret. Any change resets connectivity to unknown.
func (s *Service) Update(ctx context.Context, actor Actor, id string, in Input) (View, error) {
	if err := actor.valid(); err != nil {
		return View{}, err
	}
	if err := validateInput(in, false); err != nil {
		return View{}, err
	}
	old, err := s.store.GetTelephonyProfile(ctx, actor.UserID, id)
	if err != nil {
		return View{}, mapNotFound(err)
	}
	next := old
	next.Name = strings.TrimSpace(in.Name)
	next.CallerNumber = in.CallerNumber
	next.ConnectionID = in.ConnectionID
	var newAPI, newWebhook string
	if in.APIKey != "" {
		newAPI, err = s.putSecret(ctx, actor.UserID, "telnyx-api-"+id+"-"+s.newID(), secrets.PurposeTelnyxAPIKey, in.APIKey)
		if err != nil {
			return View{}, err
		}
		next.APISecretID = newAPI
	}
	if in.WebhookPublicKey != "" {
		newWebhook, err = s.putSecret(ctx, actor.UserID, "telnyx-webhook-"+id+"-"+s.newID(), secrets.PurposeTelnyxWebhookKey, in.WebhookPublicKey)
		if err != nil {
			s.dropSecret(ctx, actor.UserID, newAPI)
			return View{}, err
		}
		next.WebhookPublicKeySecretID = newWebhook
	} else if in.ClearWebhookPublicKey {
		next.WebhookPublicKeySecretID = ""
	}
	next.ConnectivityStatus = storage.ConnectivityUnknown
	next.ConnectivityReason = ""
	next.ConnectivityCheckedAt = ""
	next.UpdatedAt = s.now3339()
	if err := s.store.UpdateTelephonyProfile(ctx, next); err != nil {
		s.dropSecret(ctx, actor.UserID, newAPI)
		s.dropSecret(ctx, actor.UserID, newWebhook)
		if errors.Is(err, storage.ErrConflict) {
			return View{}, fmt.Errorf("%w: name already used", ErrConflict)
		}
		return View{}, mapNotFound(err)
	}
	s.audit(ctx, actor, "telephony_profile.update", id, "success")
	return toView(next, access(actor.UserID, next.OwnerUserID)), nil
}

// Get returns one profile the actor owns or was granted.
func (s *Service) Get(ctx context.Context, actor Actor, id string) (View, error) {
	if err := actor.valid(); err != nil {
		return View{}, err
	}
	if !validID(id) {
		return View{}, ErrNotFound
	}
	u, err := s.store.ResolveUsableTelephonyProfile(ctx, actor.UserID, id)
	if errors.Is(err, storage.ErrTelephonyProfileUnusable) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	return toView(recordFromUsable(u), u.Access), nil
}

// Resolved is a usable profile with its owner's plaintext Telnyx keys. It is
// for the call path (#79) only. No REST or MCP transport returns it. Its
// formatting is redacted, so a %v or a log line cannot print a key.
type Resolved struct {
	ID               string
	OwnerUserID      string
	CallerNumber     string
	ConnectionID     string
	Access           string
	APIKey           string
	WebhookPublicKey string
	// ConnectivityStatus is the last validation result: unknown, verified or
	// failed. Only the call path gates on it.
	ConnectivityStatus string
}

// String redacts the keys. fmt uses it for %v and %+v.
func (r Resolved) String() string {
	return fmt.Sprintf("telephonyprofile.Resolved{id=%s access=%s keys=redacted}", r.ID, r.Access)
}

// GoString redacts the keys for %#v.
func (r Resolved) GoString() string { return r.String() }

// Resolve returns a profile the actor may use, with the owner's keys decrypted
// on the server. The key is read with the owner's id, never the caller's, so a
// grantee reaches the owner's key only through this path. A profile the actor
// cannot use returns ErrNotFound. A secret that cannot be opened, or that does
// not match its owner or purpose, returns ErrSecretsDown and no key.
func (s *Service) Resolve(ctx context.Context, actor Actor, id string) (Resolved, error) {
	if err := actor.valid(); err != nil {
		return Resolved{}, err
	}
	if !validID(id) {
		return Resolved{}, ErrNotFound
	}
	u, err := s.store.ResolveUsableTelephonyProfile(ctx, actor.UserID, id)
	if errors.Is(err, storage.ErrTelephonyProfileUnusable) {
		return Resolved{}, ErrNotFound
	}
	if err != nil {
		return Resolved{}, err
	}
	// The usable read model carries no secret references. The owner's own
	// record does, and it is read with the owner's id.
	rec, err := s.store.GetTelephonyProfile(ctx, u.OwnerUserID, id)
	if err != nil {
		return Resolved{}, mapNotFound(err)
	}
	apiKey, err := s.secrets.Resolve(ctx, u.OwnerUserID, rec.APISecretID, secrets.PurposeTelnyxAPIKey)
	if err != nil {
		return Resolved{}, s.secretFault(err)
	}
	out := Resolved{
		ID: rec.ID, OwnerUserID: rec.OwnerUserID, CallerNumber: rec.CallerNumber,
		ConnectionID: rec.ConnectionID, Access: u.Access, APIKey: string(apiKey),
		ConnectivityStatus: rec.ConnectivityStatus,
	}
	if rec.WebhookPublicKeySecretID != "" {
		webhook, err := s.secrets.Resolve(ctx, u.OwnerUserID, rec.WebhookPublicKeySecretID, secrets.PurposeTelnyxWebhookKey)
		if err != nil {
			return Resolved{}, s.secretFault(err)
		}
		out.WebhookPublicKey = string(webhook)
	}
	return out, nil
}

// ResolveForHangup returns the owner's key for a profile that placed a call
// the owner owns. It is for ending a live leg only, never for placing a call.
//
// It reads the profile by its owner and id and does not re-check a grantee,
// so a grant revoked mid-call still lets CallHarbor end the leg that grant
// dialed. A profile the owner no longer owns still returns ErrNotFound. The
// dial path keeps using Resolve, which checks the actor each time.
func (s *Service) ResolveForHangup(ctx context.Context, ownerUserID, id string) (Resolved, error) {
	if ownerUserID == "" || !validID(id) {
		return Resolved{}, ErrNotFound
	}
	rec, err := s.store.GetTelephonyProfile(ctx, ownerUserID, id)
	if err != nil {
		return Resolved{}, mapNotFound(err)
	}
	apiKey, err := s.secrets.Resolve(ctx, ownerUserID, rec.APISecretID, secrets.PurposeTelnyxAPIKey)
	if err != nil {
		return Resolved{}, s.secretFault(err)
	}
	return Resolved{
		ID: rec.ID, OwnerUserID: rec.OwnerUserID, CallerNumber: rec.CallerNumber,
		ConnectionID: rec.ConnectionID, APIKey: string(apiKey),
		ConnectivityStatus: rec.ConnectivityStatus,
	}, nil
}

// secretFault maps every failure to open a stored secret to ErrSecretsDown.
// The cause is logged, and it never contains key material.
func (s *Service) secretFault(err error) error {
	s.logger.Error("telephony secret unavailable", "err", err.Error())
	return ErrSecretsDown
}

// List returns every profile the actor owns or was granted, oldest first.
func (s *Service) List(ctx context.Context, actor Actor) ([]View, error) {
	if err := actor.valid(); err != nil {
		return nil, err
	}
	usable, err := s.store.ListUsableTelephonyProfiles(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(usable))
	for _, u := range usable {
		out = append(out, toView(recordFromUsable(u), u.Access))
	}
	return out, nil
}

func recordFromUsable(u storage.UsableTelephonyProfile) storage.TelephonyProfileRecord {
	return storage.TelephonyProfileRecord{
		ID: u.ID, OwnerUserID: u.OwnerUserID, Name: u.Name, Provider: u.Provider,
		CallerNumber: u.CallerNumber, ConnectionID: u.ConnectionID,
		ConnectivityStatus: u.ConnectivityStatus, ConnectivityReason: u.ConnectivityReason,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func access(actor, owner string) string {
	if actor == owner {
		return storage.TelephonyAccessOwner
	}
	return storage.TelephonyAccessGranted
}

// Delete removes a profile the actor owns, with its grants and secrets. A
// profile that calls reference is kept and returns ErrInUse.
func (s *Service) Delete(ctx context.Context, actor Actor, id string) error {
	if err := actor.valid(); err != nil {
		return err
	}
	if !validID(id) {
		return ErrNotFound
	}
	if err := s.store.DeleteTelephonyProfile(ctx, actor.UserID, id); err != nil {
		if errors.Is(err, storage.ErrTelephonyProfileInUse) {
			return ErrInUse
		}
		return mapNotFound(err)
	}
	s.audit(ctx, actor, "telephony_profile.delete", id, "success")
	return nil
}

// Validate runs read-only provider checks for a profile the actor owns and
// stores the result. It issues GET requests only and never dials. The provider
// key is resolved from the owner's encrypted secret inside this call.
func (s *Service) Validate(ctx context.Context, actor Actor, id string) (Validation, error) {
	if err := actor.valid(); err != nil {
		return Validation{}, err
	}
	if !validID(id) {
		return Validation{}, ErrNotFound
	}
	rec, err := s.store.GetTelephonyProfile(ctx, actor.UserID, id)
	if err != nil {
		return Validation{}, mapNotFound(err)
	}
	status, reason := s.check(ctx, actor.UserID, rec)
	checked := s.now3339()
	// The result describes the profile as it was read. If the profile changed
	// during validation, the stale result is refused rather than stored.
	if err := s.store.SetTelephonyConnectivity(ctx, actor.UserID, id, rec.UpdatedAt, status, reason, checked); err != nil {
		if errors.Is(err, storage.ErrTelephonyProfileChanged) {
			// The audit enum allows success, denied and failed only. A race is a
			// failed validation; the fixed detail records why.
			s.writeAudit(ctx, actor, "telephony_profile.validate", id, "", "failed", raceAuditDetail)
			return Validation{}, ErrValidationRace
		}
		return Validation{}, mapNotFound(err)
	}
	outcome := "success"
	if status == storage.ConnectivityFailed {
		outcome = "failed"
	}
	s.audit(ctx, actor, "telephony_profile.validate", id, outcome)
	return Validation{ProfileID: id, ConnectivityStatus: status, ConnectivityReason: reason, CheckedAt: checked}, nil
}

// check maps one read-only validation to a stored status and a fixed reason.
// The first failing check decides the reason. Errors are never recorded.
func (s *Service) check(ctx context.Context, owner string, rec storage.TelephonyProfileRecord) (string, string) {
	apiKey, err := s.secrets.Resolve(ctx, owner, rec.APISecretID, secrets.PurposeTelnyxAPIKey)
	if err != nil {
		return storage.ConnectivityFailed, ReasonSecretsUnavailable
	}
	p, err := s.provider(string(apiKey))
	if err != nil {
		return storage.ConnectivityFailed, ReasonSecretsUnavailable
	}
	vctx, cancel := context.WithTimeout(ctx, validationTimeout)
	defer cancel()

	// Rows written before connection_id became required may have none. Such a
	// profile cannot dial, so it is reported as an application that is not
	// found, with no new reason code.
	if rec.ConnectionID == "" {
		return storage.ConnectivityFailed, ReasonApplicationNotFound
	}
	app, err := p.GetCallControlApplication(vctx, rec.ConnectionID)
	switch {
	case errors.Is(err, telnyx.ErrNotFound):
		return storage.ConnectivityFailed, ReasonApplicationNotFound
	case errors.Is(err, telnyx.ErrAPI):
		return storage.ConnectivityFailed, classifyAPIError(err)
	case err != nil:
		return storage.ConnectivityFailed, ReasonProviderUnreachable
	case !app.Active:
		return storage.ConnectivityFailed, ReasonApplicationInactive
	}
	num, err := p.FindPhoneNumber(vctx, rec.CallerNumber)
	switch {
	case errors.Is(err, telnyx.ErrNotFound):
		return storage.ConnectivityFailed, ReasonNumberNotFound
	case errors.Is(err, telnyx.ErrAPI):
		return storage.ConnectivityFailed, classifyAPIError(err)
	case err != nil:
		return storage.ConnectivityFailed, ReasonProviderUnreachable
	case num.Status != activeNumberStatus:
		return storage.ConnectivityFailed, ReasonNumberInactive
	case rec.ConnectionID != "" && num.ConnectionID != rec.ConnectionID:
		return storage.ConnectivityFailed, ReasonNumberNotOnApp
	}
	return storage.ConnectivityVerified, ReasonNone
}

// classifyAPIError maps a typed Telnyx status to a reason. A 401 or 403 means
// the key was refused. Any other rejected request is reported as the provider
// being unavailable for this check, so no provider text leaks.
func classifyAPIError(err error) string {
	var ae *telnyx.APIError
	if errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) {
		return ReasonInvalidCredentials
	}
	return ReasonProviderUnreachable
}

// Grant gives granteeID use of a profile. Only an admin who owns the profile
// may grant. The grantee must be an active user.
func (s *Service) Grant(ctx context.Context, actor Actor, profileID, granteeID string) (Grant, error) {
	if err := s.requireGrantor(actor); err != nil {
		return Grant{}, err
	}
	if !validID(profileID) || !validID(granteeID) {
		return Grant{}, ErrNotFound
	}
	if _, err := s.store.GetTelephonyProfile(ctx, actor.UserID, profileID); err != nil {
		return Grant{}, mapNotFound(err)
	}
	u, err := s.store.GetUserByID(ctx, granteeID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return Grant{}, fmt.Errorf("%w: grantee", ErrInvalidInput)
	case err != nil:
		return Grant{}, err
	case !u.Active:
		return Grant{}, fmt.Errorf("%w: grantee", ErrInvalidInput)
	}
	now := s.now3339()
	if err := s.store.UpsertTelephonyGrant(ctx, storage.TelephonyGrant{
		ProfileID: profileID, GranteeUserID: granteeID, GrantedByUserID: actor.UserID, CreatedAt: now,
	}); err != nil {
		return Grant{}, err
	}
	s.auditTarget(ctx, actor, "telephony_profile.grant", profileID, granteeID)
	return Grant{UserID: granteeID, GrantedBy: actor.UserID, CreatedAt: now}, nil
}

// Revoke removes a grant. Only an admin who owns the profile may revoke.
func (s *Service) Revoke(ctx context.Context, actor Actor, profileID, granteeID string) error {
	if err := s.requireGrantor(actor); err != nil {
		return err
	}
	if !validID(profileID) || !validID(granteeID) {
		return ErrNotFound
	}
	if _, err := s.store.GetTelephonyProfile(ctx, actor.UserID, profileID); err != nil {
		return mapNotFound(err)
	}
	err := s.store.DeleteTelephonyGrant(ctx, profileID, granteeID)
	switch {
	case errors.Is(err, storage.ErrTelephonyGrantNotFound):
		return ErrNotFound
	case err != nil:
		return err
	}
	s.auditTarget(ctx, actor, "telephony_profile.revoke", profileID, granteeID)
	return nil
}

// ListGrants returns the grants on a profile. Only an admin who owns it may read them.
func (s *Service) ListGrants(ctx context.Context, actor Actor, profileID string) ([]Grant, error) {
	if err := s.requireGrantor(actor); err != nil {
		return nil, err
	}
	if !validID(profileID) {
		return nil, ErrNotFound
	}
	if _, err := s.store.GetTelephonyProfile(ctx, actor.UserID, profileID); err != nil {
		return nil, mapNotFound(err)
	}
	rows, err := s.store.ListTelephonyGrants(ctx, profileID)
	if err != nil {
		return nil, err
	}
	out := make([]Grant, 0, len(rows))
	for _, g := range rows {
		out = append(out, Grant{UserID: g.GranteeUserID, GrantedBy: g.GrantedByUserID, CreatedAt: g.CreatedAt})
	}
	return out, nil
}

// requireGrantor enforces the admin-only grant rule.
func (s *Service) requireGrantor(actor Actor) error {
	if err := actor.valid(); err != nil {
		return err
	}
	if !actor.Admin {
		return ErrForbidden
	}
	return nil
}

func (s *Service) putSecret(ctx context.Context, owner, name, purpose, plaintext string) (string, error) {
	id, err := s.secrets.Put(ctx, owner, name, purpose, []byte(plaintext))
	if err != nil {
		if errors.Is(err, secrets.ErrNoMasterKey) {
			return "", ErrSecretsDown
		}
		if errors.Is(err, secrets.ErrInvalidSecret) {
			return "", fmt.Errorf("%w: secret", ErrInvalidInput)
		}
		return "", err
	}
	return id, nil
}

func (s *Service) dropSecret(ctx context.Context, owner, id string) {
	if id == "" {
		return
	}
	if err := s.store.DeleteOwnedSecret(ctx, owner, id); err != nil {
		s.logger.Error("secret rollback failed", "err", err.Error())
	}
}

func mapNotFound(err error) error {
	if errors.Is(err, storage.ErrTelephonyProfileNotFound) {
		return ErrNotFound
	}
	return err
}

// Principal adapts an identity principal to an Actor.
func Principal(p *identity.Principal) Actor {
	return Actor{UserID: p.User.ID, Admin: p.User.Role == "admin"}
}
