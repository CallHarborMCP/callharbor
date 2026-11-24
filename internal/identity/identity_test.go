package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	pwAdmin = "correct horse battery"
	pwUser  = "staple battery horse"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type session struct {
	p     *Principal
	token string
}

func newFixture(t *testing.T) (*Service, *clock, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clk := &clock{t: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)}
	svc, err := NewService(store, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return svc, clk, store
}

func loginSession(t *testing.T, svc *Service, username, password string) session {
	t.Helper()
	res, err := svc.Login(context.Background(), username, password, "203.0.113.7")
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	p, err := svc.Authenticate(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("authenticate %s: %v", username, err)
	}
	return session{p: p, token: res.Token}
}

// fixtureWithUsers creates root (first admin), ops (second admin), and two
// regular users alice and bob, returning a live session for each.
func fixtureWithUsers(t *testing.T) (*Service, *clock, *storage.Store, map[string]session) {
	t.Helper()
	svc, clk, store := newFixture(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	root := loginSession(t, svc, "root", pwAdmin)
	if _, err := svc.CreateUser(ctx, root.p, "ops", pwAdmin, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateUser(ctx, root.p, "alice", pwUser, RoleUser); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateUser(ctx, root.p, "bob", pwUser, RoleUser); err != nil {
		t.Fatal(err)
	}
	return svc, clk, store, map[string]session{
		"root":  root,
		"ops":   loginSession(t, svc, "ops", pwAdmin),
		"alice": loginSession(t, svc, "alice", pwUser),
		"bob":   loginSession(t, svc, "bob", pwUser),
	}
}

func userID(t *testing.T, store *storage.Store, username string) string {
	t.Helper()
	u, err := store.GetUserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestPasswordHashRoundTripAndPolicy(t *testing.T) {
	h, err := HashPassword(pwUser)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash is not argon2id: %s", h)
	}
	if !VerifyPassword(pwUser, h) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong password here", h) {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword(pwUser, "not-a-hash") {
		t.Fatal("malformed hash accepted")
	}
	if _, err := HashPassword("short"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("short password: %v", err)
	}
}

func TestSetupCreatesOnlyFirstAdmin(t *testing.T) {
	svc, _, store := newFixture(t)
	ctx := context.Background()
	if required, _ := svc.SetupRequired(ctx); !required {
		t.Fatal("setup not required on empty database")
	}
	u, err := svc.Setup(ctx, "root", pwAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleAdmin || !u.Active {
		t.Fatalf("first user = %+v", u)
	}
	if _, err := svc.Setup(ctx, "intruder", pwAdmin); !errors.Is(err, storage.ErrSetupComplete) {
		t.Fatalf("second setup: %v", err)
	}
	if n, _ := store.CountUsers(ctx); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _, _, _ := fixtureWithUsers(t)
	ctx := context.Background()
	_, errWrong := svc.Login(ctx, "alice", "not the password!", "198.51.100.1")
	_, errUnknown := svc.Login(ctx, "nobody", "not the password!", "198.51.100.2")
	if !errors.Is(errWrong, ErrInvalidLogin) || !errors.Is(errUnknown, ErrInvalidLogin) {
		t.Fatalf("wrong=%v unknown=%v", errWrong, errUnknown)
	}
}

func TestLoginThrottlesPerUsernameAndLocksOut(t *testing.T) {
	svc, clk, _, _ := fixtureWithUsers(t)
	ctx := context.Background()
	for i := 0; i < throttleMaxFails; i++ {
		if _, err := svc.Login(ctx, "alice", "wrong wrong wrong", "198.51.100.10"); !errors.Is(err, ErrInvalidLogin) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := svc.Login(ctx, "alice", pwUser, "198.51.100.99"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("correct password during lockout: %v", err)
	}
	clk.advance(throttleLockout + time.Second)
	if _, err := svc.Login(ctx, "alice", pwUser, "198.51.100.99"); err != nil {
		t.Fatalf("login after lockout expiry: %v", err)
	}
}

func TestLoginThrottlesPerIPAcrossUsernames(t *testing.T) {
	svc, _, _, _ := fixtureWithUsers(t)
	ctx := context.Background()
	for i := 0; i < throttleIPMaxFails; i++ {
		_, _ = svc.Login(ctx, "ghost"+string(rune('a'+i%26))+string(rune('a'+i/26)), "wrong wrong wrong", "198.51.100.50")
	}
	if _, err := svc.Login(ctx, "bob", "wrong wrong wrong", "198.51.100.50"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("IP-based lockout not applied to failures: %v", err)
	}
	if _, err := svc.Login(ctx, "bob", pwUser, "198.51.100.50"); err != nil {
		t.Fatalf("correct login denied by IP lockout: %v", err)
	}
}

func TestSessionAbsoluteAndIdleExpiry(t *testing.T) {
	svc, clk, _, _ := fixtureWithUsers(t)
	ctx := context.Background()

	res, err := svc.Login(ctx, "alice", pwUser, "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(SessionIdleTTL - time.Minute)
	if _, err := svc.Authenticate(ctx, res.Token); err != nil {
		t.Fatalf("active session rejected: %v", err)
	}
	clk.advance(SessionIdleTTL - time.Minute)
	if _, err := svc.Authenticate(ctx, res.Token); err != nil {
		t.Fatalf("touched session should not idle out: %v", err)
	}
	clk.advance(SessionIdleTTL + time.Second)
	if _, err := svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("idle session accepted: %v", err)
	}

	res2, err := svc.Login(ctx, "bob", pwUser, "203.0.113.2")
	if err != nil {
		t.Fatal(err)
	}
	for elapsed := time.Duration(0); elapsed < SessionAbsoluteTTL; elapsed += 10 * time.Minute {
		clk.advance(10 * time.Minute)
		_, _ = svc.Authenticate(ctx, res2.Token)
	}
	if _, err := svc.Authenticate(ctx, res2.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("session past absolute expiry accepted")
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	svc, _, _, ss := fixtureWithUsers(t)
	ctx := context.Background()
	res, err := svc.Login(ctx, "alice", pwUser, "203.0.113.3")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked session still accepted")
	}
	if _, err := svc.Authenticate(ctx, ss["alice"].token); err != nil {
		t.Fatalf("unrelated session for same user revoked: %v", err)
	}
}

func TestDeactivationRevokesSessionsAndBlocksLogin(t *testing.T) {
	svc, _, store, ss := fixtureWithUsers(t)
	ctx := context.Background()
	aliceID := userID(t, store, "alice")
	inactive := false
	if err := svc.UpdateUser(ctx, ss["root"].p, aliceID, nil, &inactive); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, ss["alice"].token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("deactivated user session still valid")
	}
	if _, err := svc.Login(ctx, "alice", pwUser, "203.0.113.4"); !errors.Is(err, ErrInactive) {
		t.Fatalf("deactivated login: %v", err)
	}
}

func TestPasswordResetRevokesExistingSessions(t *testing.T) {
	svc, _, store, ss := fixtureWithUsers(t)
	ctx := context.Background()
	bobID := userID(t, store, "bob")
	if err := svc.ResetPassword(ctx, ss["ops"].p, bobID, "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, ss["bob"].token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("session survived password reset")
	}
	if _, err := svc.Login(ctx, "bob", "a brand new password", "203.0.113.5"); err != nil {
		t.Fatalf("login with reset password: %v", err)
	}
}

func TestIDORDeniesCrossUserAccess(t *testing.T) {
	svc, _, store, ss := fixtureWithUsers(t)
	ctx := context.Background()
	aliceID := userID(t, store, "alice")
	bobID := userID(t, store, "bob")

	if _, err := svc.GetUser(ctx, ss["bob"].p, aliceID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user reads other user: %v", err)
	}
	if _, err := svc.ListUsers(ctx, ss["alice"].p); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user lists users: %v", err)
	}
	if _, err := svc.CreateUser(ctx, ss["alice"].p, "mallory", pwUser, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user creates admin: %v", err)
	}
	role := RoleAdmin
	if err := svc.UpdateUser(ctx, ss["bob"].p, aliceID, &role, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user promotes other user: %v", err)
	}
	if err := svc.ResetPassword(ctx, ss["alice"].p, bobID, "hijacked password!"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("user resets other password: %v", err)
	}
	if u, err := svc.GetUser(ctx, ss["alice"].p, aliceID); err != nil || u.Username != "alice" {
		t.Fatalf("user reads self: %v", err)
	}
	if u, err := svc.GetUser(ctx, ss["ops"].p, bobID); err != nil || u.Username != "bob" {
		t.Fatalf("second admin reads user: %v", err)
	}
	if _, err := svc.Authenticate(ctx, ss["bob"].token); err != nil {
		t.Fatalf("bob's session affected by denied attempts: %v", err)
	}
}

func TestMultipleAdminsCanGrantAndRevokeRoles(t *testing.T) {
	svc, _, store, ss := fixtureWithUsers(t)
	ctx := context.Background()
	aliceID := userID(t, store, "alice")
	rootID := userID(t, store, "root")
	opsID := userID(t, store, "ops")

	promote := RoleAdmin
	if err := svc.UpdateUser(ctx, ss["ops"].p, aliceID, &promote, nil); err != nil {
		t.Fatalf("second admin grants admin: %v", err)
	}
	aliceAdmin := loginSession(t, svc, "alice", pwUser)
	if _, err := svc.ListUsers(ctx, aliceAdmin.p); err != nil {
		t.Fatalf("granted admin cannot list users: %v", err)
	}

	demote := RoleUser
	if err := svc.UpdateUser(ctx, aliceAdmin.p, rootID, &demote, nil); err != nil {
		t.Fatalf("admin demotes another admin: %v", err)
	}
	rootNow := loginSession(t, svc, "root", pwAdmin)
	if _, err := svc.ListUsers(ctx, rootNow.p); !errors.Is(err, ErrForbidden) {
		t.Fatalf("demoted admin retains access: %v", err)
	}
	if err := svc.UpdateUser(ctx, aliceAdmin.p, aliceID, &demote, nil); err != nil {
		t.Fatalf("admin demotes self while another admin remains: %v", err)
	}
	opsSession := loginSession(t, svc, "ops", pwAdmin)
	if err := svc.UpdateUser(ctx, opsSession.p, opsID, &demote, nil); !errors.Is(err, storage.ErrLastAdmin) {
		t.Fatalf("last admin demoted itself: %v", err)
	}
}

func TestLastActiveAdminCannotBeDeactivated(t *testing.T) {
	svc, _, store, ss := fixtureWithUsers(t)
	ctx := context.Background()
	rootID := userID(t, store, "root")
	opsID := userID(t, store, "ops")
	inactive := false
	if err := svc.UpdateUser(ctx, ss["ops"].p, rootID, nil, &inactive); err != nil {
		t.Fatalf("deactivate one of two admins: %v", err)
	}
	if err := svc.UpdateUser(ctx, ss["ops"].p, opsID, nil, &inactive); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self-deactivation not refused: %v", err)
	}
	demote := RoleUser
	if err := svc.UpdateUser(ctx, ss["ops"].p, opsID, &demote, nil); !errors.Is(err, storage.ErrLastAdmin) {
		t.Fatalf("removed the last active admin: %v", err)
	}
}

func TestCreateUserRejectsInvalidInput(t *testing.T) {
	svc, _, _, ss := fixtureWithUsers(t)
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, ss["root"].p, "bad name!", pwUser, RoleUser); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad username: %v", err)
	}
	if _, err := svc.CreateUser(ctx, ss["root"].p, "carol", pwUser, "superuser"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad role: %v", err)
	}
	if _, err := svc.CreateUser(ctx, ss["root"].p, "alice", pwUser, RoleUser); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("duplicate username: %v", err)
	}
}

func TestAuditRecordsDeniedActions(t *testing.T) {
	svc, _, store, ss := fixtureWithUsers(t)
	ctx := context.Background()
	aliceID := userID(t, store, "alice")
	_, _ = svc.GetUser(ctx, ss["bob"].p, aliceID)
	n, err := store.CountAuditEvents(ctx, "user.read", "denied")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("denied audit rows = %d, want 1", n)
	}
}

func newHTTP(t *testing.T) (*http.ServeMux, *Service, *storage.Store) {
	t.Helper()
	svc, _, store := newFixture(t)
	mux := http.NewServeMux()
	NewHandler(svc).Register(mux)
	return mux, svc, store
}

func do(t *testing.T, mux http.Handler, method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.20:5555"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(CSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func loginHTTP(t *testing.T, mux http.Handler, username, password string) (*http.Cookie, string) {
	t.Helper()
	rec := do(t, mux, http.MethodPost, "/api/session", map[string]string{"username": username, "password": password}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", username, rec.Code, rec.Body.String())
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c, out.CSRFToken
		}
	}
	t.Fatal("no session cookie issued")
	return nil, ""
}

func TestHTTPSetupLoginAndSessionCookieFlags(t *testing.T) {
	mux, _, _ := newHTTP(t)
	rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "again", "password": pwAdmin}, nil, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup: %d", rec.Code)
	}
	rec = do(t, mux, http.MethodPost, "/api/session", map[string]string{"username": "root", "password": pwAdmin}, nil, "")
	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			found = c
		}
	}
	if found == nil || !found.HttpOnly || !found.Secure || found.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie flags wrong: %+v", found)
	}
}

func TestHTTPMutationsRequireCSRFToken(t *testing.T) {
	mux, _, _ := newHTTP(t)
	do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, "")
	cookie, csrf := loginHTTP(t, mux, "root", pwAdmin)

	body := map[string]string{"username": "alice", "password": pwUser, "role": "user"}
	if rec := do(t, mux, http.MethodPost, "/api/users", body, cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/users", body, cookie, "wrong-token"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/users", body, cookie, csrf); rec.Code != http.StatusCreated {
		t.Fatalf("valid CSRF: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPUnauthenticatedAndIDORStatuses(t *testing.T) {
	mux, svc, store := newHTTP(t)
	ctx := context.Background()
	if _, err := svc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	rootCookie, rootCSRF := loginHTTP(t, mux, "root", pwAdmin)
	do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "alice", "password": pwUser, "role": "user"}, rootCookie, rootCSRF)
	do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "bob", "password": pwUser, "role": "user"}, rootCookie, rootCSRF)
	aliceCookie, aliceCSRF := loginHTTP(t, mux, "alice", pwUser)
	bobID := userID(t, store, "bob")

	if rec := do(t, mux, http.MethodGet, "/api/users", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodGet, "/api/users/"+bobID, nil, aliceCookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("IDOR read: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodGet, "/api/users", nil, aliceCookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("user list: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/users/"+bobID+"/password", map[string]string{"password": "hijacked password!"}, aliceCookie, aliceCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("IDOR password reset by user: %d", rec.Code)
	}
}

func TestHTTPLoginThrottleReturns429(t *testing.T) {
	mux, svc, _ := newHTTP(t)
	if _, err := svc.Setup(context.Background(), "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < throttleMaxFails; i++ {
		do(t, mux, http.MethodPost, "/api/session", map[string]string{"username": "root", "password": "wrong wrong wrong"}, nil, "")
	}
	rec := do(t, mux, http.MethodPost, "/api/session", map[string]string{"username": "root", "password": pwAdmin}, nil, "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("throttled login: %d", rec.Code)
	}
}

func TestHTTPRejectsUnknownFieldsAndWrongContentType(t *testing.T) {
	mux, _, _ := newHTTP(t)
	req := httptest.NewRequest(http.MethodPost, "/api/setup", strings.NewReader(`{"username":"root","password":"x","extra":1}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain body: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]any{"username": "root", "password": pwAdmin, "extra": 1}, nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
}
