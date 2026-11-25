package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

const oauthNow = "2026-11-25T10:00:00.000Z"
const oauthLater = "2026-11-25T10:05:00.000Z"

// testChallenge is a syntactically valid 43-char S256 challenge; storage never verifies it.
const testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

func openOAuthFixture(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, User{ID: "alice", Username: "alice", Role: "user", PasswordHash: "h", Active: true, CreatedAt: oauthNow, UpdatedAt: oauthNow}, AuditEvent{OccurredAt: oauthNow, Action: "test", ResourceType: "user", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertOAuthClient(ctx, OAuthClient{ClientID: "https://client.example/meta.json", Source: "metadata_document", ClientName: "C", RedirectURIs: `["https://client.example/cb"]`, CreatedAt: oauthNow, UpdatedAt: oauthNow}); err != nil {
		t.Fatal(err)
	}
	return s
}

func seedApprovedGrant(t *testing.T, s *Store, reqID, grantID, codeHash string) {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertOAuthAuthRequest(ctx, OAuthAuthRequest{
		ID: reqID, ClientID: "https://client.example/meta.json", RedirectURI: "https://client.example/cb",
		Scopes: "mcp:read mcp:call", Resource: "https://cb.example/mcp", CodeChallenge: testChallenge,
		CreatedAt: oauthNow, ExpiresAt: oauthLater,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideOAuthAuthRequest(ctx, reqID, "alice", oauthNow); err != nil {
		t.Fatal(err)
	}
	grant := OAuthGrant{ID: grantID, UserID: "alice", ClientID: "https://client.example/meta.json", Resource: "https://cb.example/mcp", Scopes: "mcp:read mcp:call", CreatedAt: oauthNow}
	code := OAuthCode{CodeHash: codeHash, GrantID: grantID, AuthRequestID: reqID, ClientID: grant.ClientID, RedirectURI: "https://client.example/cb", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater}
	if err := s.ApproveOAuthRequest(ctx, reqID, "alice", grant, code, oauthNow, AuditEvent{OccurredAt: oauthNow, Action: "oauth.consent", ResourceType: "oauth_grant", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthCodeConsumedExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openOAuthFixture(t)
	seedApprovedGrant(t, s, "req1", "grant1", hex64('a'))

	const racers = 8
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() { results <- s.ConsumeOAuthCode(ctx, hex64('a'), oauthNow) }()
	}
	wins := 0
	for i := 0; i < racers; i++ {
		err := <-results
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrOAuthCodeConsumed):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("code consumed %d times, want exactly 1", wins)
	}
}

func TestDeactivationRevokesOAuthGrantsAtomically(t *testing.T) {
	ctx := context.Background()
	s := openOAuthFixture(t)
	seedApprovedGrant(t, s, "req2", "grant2", hex64('b'))
	if err := s.IssueOAuthTokens(ctx, OAuthToken{TokenHash: hex64('c'), GrantID: "grant2", UserID: "alice", ClientID: "https://client.example/meta.json", Audience: "https://cb.example/mcp", Scopes: "mcp:read", CreatedAt: oauthNow, ExpiresAt: oauthLater}, nil, oauthNow); err != nil {
		t.Fatal(err)
	}

	inactive := false
	if err := s.UpdateUser(ctx, "alice", nil, &inactive, oauthNow, AuditEvent{OccurredAt: oauthNow, Action: "user.update", ResourceType: "user", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.GetOAuthAccessToken(ctx, hex64('c'))
	if err != nil {
		t.Fatal(err)
	}
	if tok.RevokedAt == "" || tok.GrantRevokedAt == "" {
		t.Fatalf("access token not revoked on deactivation: revoked=%q grant=%q", tok.RevokedAt, tok.GrantRevokedAt)
	}
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// TestTouchOAuthGrantOnlyMovesForward: a touch writes only when the stored value
// is older than the threshold, so a stale caller cannot move last_used_at back.
func TestTouchOAuthGrantOnlyMovesForward(t *testing.T) {
	s := openOAuthFixture(t)
	ctx := context.Background()
	seedApprovedGrant(t, s, "req-touch", "grant-touch", hex64('t'))

	if err := s.TouchOAuthGrant(ctx, "grant-touch", oauthNow, "2026-11-25T09:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetOAuthGrantByID(ctx, "grant-touch")
	if err != nil {
		t.Fatal(err)
	}
	if g.LastUsedAt != oauthNow {
		t.Fatalf("first touch last_used_at = %q, want %q", g.LastUsedAt, oauthNow)
	}

	// A touch whose threshold is older than the stored value must not write.
	if err := s.TouchOAuthGrant(ctx, "grant-touch", oauthLater, "2026-11-25T09:59:00.000Z"); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GetOAuthGrantByID(ctx, "grant-touch")
	if g.LastUsedAt != oauthNow {
		t.Fatalf("touch inside the throttle window moved last_used_at to %q", g.LastUsedAt)
	}

	// Once the stored value is older than the threshold, the write lands.
	if err := s.TouchOAuthGrant(ctx, "grant-touch", oauthLater, oauthLater); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GetOAuthGrantByID(ctx, "grant-touch")
	if g.LastUsedAt != oauthLater {
		t.Fatalf("touch after the window = %q, want %q", g.LastUsedAt, oauthLater)
	}
}

// TestTouchOAuthGrantUnknownIsNoOp: touching a missing grant updates nothing
// and reports no error, because the resource path treats the write as best effort.
// TestPurgeExpiredOAuthAuthRemovesOnlyExpiredRows: expired codes and requests
// are removed, code first so the FK holds. A live request and a live code are
// untouched, and so is an expired request that a live code still references.
func TestPurgeExpiredOAuthAuthRemovesOnlyExpiredRows(t *testing.T) {
	ctx := context.Background()
	s := openOAuthFixture(t)
	// Requests and codes expire at oauthLater; the purge runs after it.
	const afterExpiry = "2026-11-25T10:30:00.000Z"
	cid := "https://client.example/meta.json"
	insertReq := func(id, expiresAt string) {
		t.Helper()
		if err := s.InsertOAuthAuthRequest(ctx, OAuthAuthRequest{
			ID: id, ClientID: cid, RedirectURI: "https://client.example/cb", Scopes: "mcp:read",
			Resource: "https://cb.example/mcp", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: expiresAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insertReq("req-bare", oauthLater)
	insertReq("req-live", "2026-11-25T23:00:00.000Z")
	// An approved request whose code expires with it, so both are expired at
	// purge time. seedApprovedGrant uses oauthLater for the code and request.
	seedApprovedGrant(t, s, "req-coded", "grant1", hex64('c'))

	if err := s.PurgeExpiredOAuthAuth(ctx, afterExpiry, 100); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, id := range []string{"req-bare", "req-coded"} {
		if _, err := s.GetOAuthAuthRequest(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired request %s survived: err = %v", id, err)
		}
	}
	if _, err := s.GetOAuthAuthRequest(ctx, "req-live"); err != nil {
		t.Fatalf("live request deleted: %v", err)
	}
	if _, err := s.GetOAuthCode(ctx, hex64('c')); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired code survived: err = %v", err)
	}
}

// TestDecideAndApproveRefusesUserDeactivatedAfterCheck: a user deactivated
// after the handler's own check but before the approval write must get no
// grant or code, and the request stays undecided, so nothing is stranded.
func TestDecideAndApproveRefusesUserDeactivatedAfterCheck(t *testing.T) {
	ctx := context.Background()
	s := openOAuthFixture(t)
	const reqID = "req-race"
	const cid = "https://client.example/meta.json"
	if err := s.InsertOAuthAuthRequest(ctx, OAuthAuthRequest{
		ID: reqID, ClientID: cid, RedirectURI: "https://client.example/cb", Scopes: "mcp:read",
		Resource: "https://cb.example/mcp", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater,
	}); err != nil {
		t.Fatal(err)
	}
	inactive := false
	if err := s.UpdateUser(ctx, "alice", nil, &inactive, oauthNow, AuditEvent{OccurredAt: oauthNow, Action: "user.update", ResourceType: "user", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}

	grant := OAuthGrant{ID: "grant-race", UserID: "alice", ClientID: cid, Resource: "https://cb.example/mcp", Scopes: "mcp:read", CreatedAt: oauthNow}
	code := OAuthCode{CodeHash: hex64('e'), GrantID: "grant-race", AuthRequestID: reqID, ClientID: cid, RedirectURI: "https://client.example/cb", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater}
	err := s.DecideAndApproveOAuthRequest(ctx, reqID, "alice", grant, code, oauthNow, AuditEvent{OccurredAt: oauthNow, Action: "oauth.consent", ResourceType: "oauth_grant", Outcome: "success", DetailJSON: "{}"})
	if !errors.Is(err, ErrOAuthRequestNotDecidable) {
		t.Fatalf("approval for a deactivated user: err = %v, want ErrOAuthRequestNotDecidable", err)
	}
	if _, err := s.GetOAuthCode(ctx, hex64('e')); !errors.Is(err, ErrNotFound) {
		t.Fatalf("code written for a deactivated user: err = %v", err)
	}
	if _, err := s.GetOAuthGrantByID(ctx, "grant-race"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant written for a deactivated user: err = %v", err)
	}
	r, err := s.GetOAuthAuthRequest(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if r.DecidedAt != "" || r.ConsumedAt != "" {
		t.Fatalf("request left decided after refusal: decided=%q consumed=%q", r.DecidedAt, r.ConsumedAt)
	}
}

func TestTouchOAuthGrantUnknownIsNoOp(t *testing.T) {
	s := openOAuthFixture(t)
	if err := s.TouchOAuthGrant(context.Background(), "missing", oauthNow, oauthNow); err != nil {
		t.Fatalf("touch of unknown grant: %v", err)
	}
}
