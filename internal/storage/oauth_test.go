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
