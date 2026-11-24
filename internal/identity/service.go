package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	SessionAbsoluteTTL = 12 * time.Hour
	SessionIdleTTL     = 30 * time.Minute

	throttleWindow     = 15 * time.Minute
	throttleMaxFails   = 5
	throttleIPMaxFails = 50
	throttleLockout    = 15 * time.Minute

	timeLayout = "2006-01-02T15:04:05.000Z"
)

var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrInvalidLogin    = errors.New("invalid username or password")
	ErrThrottled       = errors.New("too many failed login attempts")
	ErrForbidden       = errors.New("forbidden")
	ErrInvalidInput    = errors.New("invalid input")
	ErrInactive        = errors.New("account is inactive")

	usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
)

type Service struct {
	store *storage.Store
	now   func() time.Time
	dummy string
}

type Principal struct {
	User      storage.User
	SessionID string
	CSRFToken string
}

type LoginResult struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
	User      storage.User
}

func NewService(store *storage.Store, now func() time.Time) (*Service, error) {
	if now == nil {
		now = time.Now
	}
	dummy, err := HashPassword("callharbor-timing-equalizer")
	if err != nil {
		return nil, err
	}
	return &Service{store: store, now: func() time.Time { return now().UTC() }, dummy: dummy}, nil
}

func ts(t time.Time) string { return t.UTC().Format(timeLayout) }

func validUsername(name string) bool {
	return usernamePattern.MatchString(name)
}

func (s *Service) audit(actor *Principal, target, action, resType, resID, outcome string, detail string) storage.AuditEvent {
	e := storage.AuditEvent{
		OccurredAt:   ts(s.now()),
		Action:       action,
		ResourceType: resType,
		ResourceID:   resID,
		Outcome:      outcome,
		TargetUserID: target,
		DetailJSON:   detail,
	}
	if e.DetailJSON == "" {
		e.DetailJSON = "{}"
	}
	if actor != nil {
		e.ActorUserID = actor.User.ID
		e.ActorRole = actor.User.Role
	} else {
		e.ActorRole = "system"
	}
	return e
}

// Setup creates the first administrator. It only succeeds while no users exist.
func (s *Service) Setup(ctx context.Context, username, password string) (storage.User, error) {
	if !validUsername(username) || ValidatePassword(password) != nil {
		return storage.User{}, ErrInvalidInput
	}
	hash, err := HashPassword(password)
	if err != nil {
		return storage.User{}, err
	}
	now := ts(s.now())
	u := storage.User{
		ID: newID(), Username: username, Role: RoleAdmin, PasswordHash: hash,
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	audit := s.audit(nil, u.ID, "setup.first_admin", "user", u.ID, "success", "{}")
	if err := s.store.CreateFirstAdmin(ctx, u, audit); err != nil {
		if errors.Is(err, storage.ErrSetupComplete) {
			return storage.User{}, storage.ErrSetupComplete
		}
		if errors.Is(err, storage.ErrConflict) {
			return storage.User{}, storage.ErrConflict
		}
		return storage.User{}, err
	}
	return u, nil
}

// SetupRequired reports whether no user exists yet.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

func userThrottleKey(username string) string {
	return "user:" + strings.ToLower(username)
}

func ipThrottleKey(clientIP string) string {
	return "ip:" + clientIP
}

func failureLimit(key string) int {
	if strings.HasPrefix(key, "ip:") {
		return throttleIPMaxFails
	}
	return throttleMaxFails
}

func (s *Service) isLocked(ctx context.Context, key string, now time.Time) (bool, error) {
	t, err := s.store.GetThrottle(ctx, key)
	if err != nil {
		return false, err
	}
	if t.LockedUntil == "" {
		return false, nil
	}
	until, err := time.Parse(timeLayout, t.LockedUntil)
	return err == nil && now.Before(until), nil
}

func (s *Service) recordFailure(ctx context.Context, keys []string, now time.Time) error {
	for _, k := range keys {
		limit := failureLimit(k)
		err := s.store.UpdateThrottle(ctx, k, ts(now), func(cur storage.Throttle) storage.Throttle {
			start, err := time.Parse(timeLayout, cur.WindowStartedAt)
			if cur.WindowStartedAt == "" || err != nil || now.Sub(start) > throttleWindow {
				cur.WindowStartedAt = ts(now)
				cur.Failures = 0
			}
			cur.Failures++
			if cur.Failures >= limit {
				cur.LockedUntil = ts(now.Add(throttleLockout))
				cur.Failures = 0
				cur.WindowStartedAt = ""
			}
			return cur
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clearThrottle(ctx context.Context, keys []string, now time.Time) error {
	for _, k := range keys {
		err := s.store.UpdateThrottle(ctx, k, ts(now), func(storage.Throttle) storage.Throttle {
			return storage.Throttle{}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Login verifies credentials and applies brute-force throttling. A locked
// username is denied outright. A locked client IP only denies failed attempts,
// so correct credentials are never refused because of another user's failures
// sharing the same (possibly proxy) address. Invalid usernames are not stored.
func (s *Service) Login(ctx context.Context, username, password, clientIP string) (LoginResult, error) {
	now := s.now()
	ipKey := ipThrottleKey(clientIP)
	var userKey string
	if validUsername(username) {
		userKey = userThrottleKey(username)
		locked, err := s.isLocked(ctx, userKey, now)
		if err != nil {
			return LoginResult{}, err
		}
		if locked {
			_ = s.store.InsertAudit(ctx, s.audit(nil, "", "auth.login", "session", "", "denied", `{"reason":"throttled"}`))
			return LoginResult{}, ErrThrottled
		}
	}
	ipLocked, err := s.isLocked(ctx, ipKey, now)
	if err != nil {
		return LoginResult{}, err
	}

	u, err := s.store.GetUserByUsername(ctx, username)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return LoginResult{}, err
	}
	hash := s.dummy
	if err == nil && u.PasswordHash != "" {
		hash = u.PasswordHash
	}
	ok := VerifyPassword(password, hash)
	if err != nil || !ok || !u.Active {
		keys := []string{ipKey}
		if userKey != "" {
			keys = append(keys, userKey)
		}
		if rerr := s.recordFailure(ctx, keys, now); rerr != nil {
			return LoginResult{}, rerr
		}
		reason := "invalid_credentials"
		if err == nil && u.PasswordHash != "" && ok && !u.Active {
			reason = "inactive"
		}
		_ = s.store.InsertAudit(ctx, s.audit(nil, u.ID, "auth.login", "session", "", "denied", fmt.Sprintf(`{"reason":%q}`, reason)))
		if ipLocked {
			return LoginResult{}, ErrThrottled
		}
		if reason == "inactive" {
			return LoginResult{}, ErrInactive
		}
		return LoginResult{}, ErrInvalidLogin
	}

	if userKey != "" {
		if err := s.clearThrottle(ctx, []string{userKey}, now); err != nil {
			return LoginResult{}, err
		}
	}
	token, err := randomToken(32)
	if err != nil {
		return LoginResult{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return LoginResult{}, err
	}
	expires := now.Add(SessionAbsoluteTTL)
	if err := s.store.CreateSession(ctx, storage.Session{
		TokenHash: hashToken(token), CSRFToken: csrf, UserID: u.ID,
		CreatedAt: ts(now), LastSeenAt: ts(now), ExpiresAt: ts(expires),
	}); err != nil {
		return LoginResult{}, err
	}
	p := &Principal{User: u}
	_ = s.store.InsertAudit(ctx, s.audit(p, u.ID, "auth.login", "session", "", "success", "{}"))
	return LoginResult{Token: token, CSRFToken: csrf, ExpiresAt: expires, User: u}, nil
}

// Authenticate resolves a session token to a principal, enforcing absolute
// expiry, idle timeout, revocation and active status.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	h := hashToken(token)
	sess, err := s.store.GetSession(ctx, h)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	now := s.now()
	expires, _ := time.Parse(timeLayout, sess.ExpiresAt)
	lastSeen, _ := time.Parse(timeLayout, sess.LastSeenAt)
	if sess.RevokedAt != "" || !now.Before(expires) || now.Sub(lastSeen) > SessionIdleTTL || !sess.User.Active {
		return nil, ErrUnauthenticated
	}
	if err := s.store.TouchSession(ctx, h, ts(now)); err != nil {
		return nil, err
	}
	return &Principal{User: sess.User, SessionID: h, CSRFToken: sess.CSRFToken}, nil
}

// Logout revokes the session identified by token. Revoking an already revoked
// or unknown session is a no-op.
func (s *Service) Logout(ctx context.Context, p *Principal) error {
	if p == nil {
		return nil
	}
	return s.store.RevokeSession(ctx, p.SessionID, ts(s.now()), s.audit(p, p.User.ID, "auth.logout", "session", "", "success", "{}"))
}

var (
	appearanceThemes  = map[string]bool{"system": true, "light": true, "dark": true}
	appearancePresets = map[string]bool{"console": true, "harbor": true}
	appearanceAccents = map[string]bool{"blue": true, "teal": true, "violet": true, "green": true}
	appearanceDensity = map[string]bool{"comfortable": true, "compact": true}
)

// Appearance is the per-user display preference set. Values are validated
// against closed sets before they are stored.
type Appearance struct {
	Theme   string `json:"theme"`
	Preset  string `json:"preset"`
	Accent  string `json:"accent"`
	Density string `json:"density"`
}

// DefaultAppearance is returned when a user has not saved preferences yet.
var DefaultAppearance = Appearance{Theme: "system", Preset: "console", Accent: "blue", Density: "comfortable"}

func (a Appearance) valid() bool {
	return appearanceThemes[a.Theme] && appearancePresets[a.Preset] && appearanceAccents[a.Accent] && appearanceDensity[a.Density]
}

// GetAppearance returns the caller's own appearance preferences.
func (s *Service) GetAppearance(ctx context.Context, actor *Principal) (Appearance, error) {
	if actor == nil {
		return Appearance{}, ErrUnauthenticated
	}
	a, ok, err := s.store.GetAppearance(ctx, actor.User.ID)
	if err != nil {
		return Appearance{}, err
	}
	if !ok {
		return DefaultAppearance, nil
	}
	return Appearance{Theme: a.Theme, Preset: a.Preset, Accent: a.Accent, Density: a.Density}, nil
}

// SetAppearance stores the caller's own appearance preferences. A user can only
// write their own record; the target is always the authenticated principal.
func (s *Service) SetAppearance(ctx context.Context, actor *Principal, in Appearance) (Appearance, error) {
	if actor == nil {
		return Appearance{}, ErrUnauthenticated
	}
	if !in.valid() {
		return Appearance{}, ErrInvalidInput
	}
	if err := s.store.SetAppearance(ctx, actor.User.ID, storage.Appearance{
		Theme: in.Theme, Preset: in.Preset, Accent: in.Accent, Density: in.Density,
	}, ts(s.now())); err != nil {
		return Appearance{}, err
	}
	return in, nil
}

func requireAdmin(p *Principal) error {
	if p == nil || p.User.Role != RoleAdmin {
		return ErrForbidden
	}
	return nil
}

// CreateUser lets an administrator create a local account. Admins may create
// other admins (multi-admin).
func (s *Service) CreateUser(ctx context.Context, actor *Principal, username, password, role string) (storage.User, error) {
	if err := requireAdmin(actor); err != nil {
		s.denied(ctx, actor, "user.create", "")
		return storage.User{}, err
	}
	if !validUsername(username) || ValidatePassword(password) != nil || (role != RoleAdmin && role != RoleUser) {
		return storage.User{}, ErrInvalidInput
	}
	hash, err := HashPassword(password)
	if err != nil {
		return storage.User{}, err
	}
	now := ts(s.now())
	u := storage.User{
		ID: newID(), Username: username, Role: role, PasswordHash: hash,
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.CreateUser(ctx, u, s.audit(actor, u.ID, "user.create", "user", u.ID, "success", fmt.Sprintf(`{"role":%q}`, role))); err != nil {
		return storage.User{}, err
	}
	return u, nil
}

func (s *Service) ListUsers(ctx context.Context, actor *Principal) ([]storage.User, error) {
	if err := requireAdmin(actor); err != nil {
		s.denied(ctx, actor, "user.list", "")
		return nil, err
	}
	return s.store.ListUsers(ctx)
}

// UpdateUser changes role and/or active status. Deactivation revokes sessions.
func (s *Service) UpdateUser(ctx context.Context, actor *Principal, targetID string, role *string, active *bool) error {
	if err := requireAdmin(actor); err != nil {
		s.denied(ctx, actor, "user.update", targetID)
		return err
	}
	if role != nil && *role != RoleAdmin && *role != RoleUser {
		return ErrInvalidInput
	}
	if targetID == actor.User.ID && active != nil && !*active {
		return ErrForbidden
	}
	detail := "{}"
	if role != nil {
		detail = fmt.Sprintf(`{"role":%q}`, *role)
	}
	err := s.store.UpdateUser(ctx, targetID, role, active, ts(s.now()),
		s.audit(actor, targetID, "user.update", "user", targetID, "success", detail))
	if errors.Is(err, storage.ErrLastAdmin) {
		s.denied(ctx, actor, "user.update", targetID)
	}
	return err
}

// ResetPassword lets an administrator set a new password for any user and
// revokes that user's sessions.
func (s *Service) ResetPassword(ctx context.Context, actor *Principal, targetID, password string) error {
	if err := requireAdmin(actor); err != nil {
		s.denied(ctx, actor, "user.password_reset", targetID)
		return err
	}
	if ValidatePassword(password) != nil {
		return ErrInvalidInput
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.store.SetPasswordHash(ctx, targetID, hash, ts(s.now()),
		s.audit(actor, targetID, "user.password_reset", "user", targetID, "success", "{}"))
}

// GetUser returns a user visible to the actor. Non-admins may only read themselves.
func (s *Service) GetUser(ctx context.Context, actor *Principal, targetID string) (storage.User, error) {
	if actor == nil {
		return storage.User{}, ErrUnauthenticated
	}
	if actor.User.ID != targetID && actor.User.Role != RoleAdmin {
		s.denied(ctx, actor, "user.read", targetID)
		return storage.User{}, ErrForbidden
	}
	u, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		return storage.User{}, ErrForbidden
	}
	return u, nil
}

func (s *Service) denied(ctx context.Context, actor *Principal, action, target string) {
	_ = s.store.InsertAudit(ctx, s.audit(actor, target, action, "user", target, "denied", "{}"))
}
