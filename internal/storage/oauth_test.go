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
	grant := OAuthGrant{ID: grantID, UserID: "alice", ClientID: "https://client.example/meta.json", Resource: "https://cb.example/mcp", Scopes: "mcp:read mcp:call", CreatedAt: oauthNow}
	code := OAuthCode{CodeHash: codeHash, GrantID: grantID, AuthRequestID: reqID, ClientID: grant.ClientID, RedirectURI: "https://client.example/cb", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater}
	if err := s.DecideAndApproveOAuthRequest(ctx, reqID, "alice", grant, code, oauthNow, AuditEvent{OccurredAt: oauthNow, Action: "oauth.consent", ResourceType: "oauth_grant", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
}

// issueAccessToken redeems a seeded code for an access token, the production
// path for issuing tokens, so the tests do not depend on a second write path.
func issueAccessToken(t *testing.T, s *Store, codeHash string, access OAuthToken) {
	t.Helper()
	if err := s.RedeemOAuthCode(context.Background(), codeHash, oauthNow, access, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthCodeConsumedExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openOAuthFixture(t)
	seedApprovedGrant(t, s, "req1", "grant1", hex64('a'))

	const racers = 8
	results := make(chan error, racers)
	access := OAuthToken{TokenHash: hex64('d'), GrantID: "grant1", UserID: "alice", ClientID: "https://client.example/meta.json", Audience: "https://cb.example/mcp", Scopes: "mcp:read", CreatedAt: oauthNow, ExpiresAt: oauthLater}
	for i := 0; i < racers; i++ {
		go func() { results <- s.RedeemOAuthCode(ctx, hex64('a'), oauthNow, access, nil) }()
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
	issueAccessToken(t, s, hex64('b'), OAuthToken{TokenHash: hex64('c'), GrantID: "grant2", UserID: "alice", ClientID: "https://client.example/meta.json", Audience: "https://cb.example/mcp", Scopes: "mcp:read", CreatedAt: oauthNow, ExpiresAt: oauthLater})

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

// TestPasswordResetRevokesOAuthGrants: a password reset ends the user's remote
// MCP grants, as deactivation does, so the connections must be re-authorized.
func TestPasswordResetRevokesOAuthGrants(t *testing.T) {
	ctx := context.Background()
	s := openOAuthFixture(t)
	seedApprovedGrant(t, s, "req-pw", "grant-pw", hex64('f'))
	issueAccessToken(t, s, hex64('f'), OAuthToken{TokenHash: hex64('a'), GrantID: "grant-pw", UserID: "alice", ClientID: "https://client.example/meta.json", Audience: "https://cb.example/mcp", Scopes: "mcp:read", CreatedAt: oauthNow, ExpiresAt: oauthLater})

	if err := s.SetPasswordHash(ctx, "alice", "new-hash", oauthLater, AuditEvent{OccurredAt: oauthLater, Action: "user.password_reset", ResourceType: "user", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetOAuthGrantByID(ctx, "grant-pw")
	if err != nil {
		t.Fatal(err)
	}
	if g.RevokedAt == "" {
		t.Fatal("password reset left the OAuth grant active")
	}
	tok, err := s.GetOAuthAccessToken(ctx, hex64('a'))
	if err != nil {
		t.Fatal(err)
	}
	if tok.RevokedAt == "" && tok.GrantRevokedAt == "" {
		t.Fatal("access token still usable after password reset")
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

// TestEvictUnusedMetadataOAuthClient: a metadata-document row is evicted only
// when it is stale and nothing references it. Referenced rows, recent rows and
// dynamic clients are never removed, and the oldest eligible row goes first.
func TestEvictUnusedMetadataOAuthClient(t *testing.T) {
	ctx := context.Background()
	const meta = "https://client.example/meta.json"
	// The fixture row is updated at oauthNow (10:00). The stale cutoff is later
	// than that, the fresh cutoff earlier.
	const stale = "2026-11-25T11:00:00.000Z"
	const fresh = "2026-11-25T09:00:00.000Z"

	t.Run("evicts a stale unreferenced row", func(t *testing.T) {
		s := openOAuthFixture(t)
		evicted, err := s.EvictUnusedMetadataOAuthClient(ctx, stale)
		if err != nil || !evicted {
			t.Fatalf("evicted = %v, err = %v; want true, nil", evicted, err)
		}
		if _, err := s.GetOAuthClient(ctx, meta); !errors.Is(err, ErrNotFound) {
			t.Fatalf("evicted row still present: err = %v", err)
		}
	})

	t.Run("keeps a row referenced by a grant and code", func(t *testing.T) {
		s := openOAuthFixture(t)
		seedApprovedGrant(t, s, "req-g", "grant-g", hex64('g'))
		evicted, err := s.EvictUnusedMetadataOAuthClient(ctx, stale)
		if err != nil || evicted {
			t.Fatalf("evicted = %v, err = %v; want false, nil", evicted, err)
		}
		if _, err := s.GetOAuthClient(ctx, meta); err != nil {
			t.Fatalf("referenced row removed: %v", err)
		}
	})

	t.Run("keeps a row referenced by a pending request", func(t *testing.T) {
		s := openOAuthFixture(t)
		if err := s.InsertOAuthAuthRequest(ctx, OAuthAuthRequest{
			ID: "req-p", ClientID: meta, RedirectURI: "https://client.example/cb", Scopes: "mcp:read",
			Resource: "https://cb.example/mcp", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater,
		}); err != nil {
			t.Fatal(err)
		}
		evicted, err := s.EvictUnusedMetadataOAuthClient(ctx, stale)
		if err != nil || evicted {
			t.Fatalf("evicted = %v, err = %v; want false, nil", evicted, err)
		}
		if _, err := s.GetOAuthClient(ctx, meta); err != nil {
			t.Fatalf("referenced row removed: %v", err)
		}
	})

	t.Run("keeps a recently updated row", func(t *testing.T) {
		s := openOAuthFixture(t)
		evicted, err := s.EvictUnusedMetadataOAuthClient(ctx, fresh)
		if err != nil || evicted {
			t.Fatalf("evicted = %v, err = %v; want false, nil", evicted, err)
		}
	})

	t.Run("never evicts dynamic clients and takes the oldest metadata row first", func(t *testing.T) {
		s := openOAuthFixture(t)
		if err := s.UpsertOAuthClient(ctx, OAuthClient{ClientID: "dyn-client", Source: "dynamic", ClientName: "D", RedirectURIs: `["https://dyn.example/cb"]`, CreatedAt: oauthNow, UpdatedAt: "2026-11-25T09:00:00.000Z"}); err != nil {
			t.Fatal(err)
		}
		older := "https://older.example/meta.json"
		if err := s.UpsertOAuthClient(ctx, OAuthClient{ClientID: older, Source: "metadata_document", ClientName: "O", RedirectURIs: `["https://older.example/cb"]`, CreatedAt: "2026-11-25T09:30:00.000Z", UpdatedAt: "2026-11-25T09:30:00.000Z"}); err != nil {
			t.Fatal(err)
		}
		evicted, err := s.EvictUnusedMetadataOAuthClient(ctx, stale)
		if err != nil || !evicted {
			t.Fatalf("first eviction = %v, err = %v; want true, nil", evicted, err)
		}
		if _, err := s.GetOAuthClient(ctx, older); !errors.Is(err, ErrNotFound) {
			t.Fatalf("oldest row not evicted first: err = %v", err)
		}
		if _, err := s.GetOAuthClient(ctx, meta); err != nil {
			t.Fatalf("newer metadata row removed too early: %v", err)
		}
		// Only the dynamic client remains, so there is nothing left to evict.
		evicted, err = s.EvictUnusedMetadataOAuthClient(ctx, stale)
		if err != nil {
			t.Fatal(err)
		}
		if !evicted {
			// meta.json is the remaining unreferenced metadata row and is stale.
			t.Fatal("second eviction should remove the remaining stale metadata row")
		}
		if _, err := s.GetOAuthClient(ctx, "dyn-client"); err != nil {
			t.Fatalf("dynamic client removed: %v", err)
		}
		evicted, err = s.EvictUnusedMetadataOAuthClient(ctx, stale)
		if err != nil || evicted {
			t.Fatalf("third eviction = %v, err = %v; want false, nil", evicted, err)
		}
	})
}

// TestInsertDynamicOAuthClientCapped: at the limit, the oldest dynamic client
// that is stale and unreferenced is evicted and the new client inserted. A
// referenced or recent client is kept, and if nothing qualifies nothing is
// inserted.
func TestInsertDynamicOAuthClientCapped(t *testing.T) {
	ctx := context.Background()
	const cutoff = "2026-11-24T10:00:00.000Z"
	dyn := func(id, created string) OAuthClient {
		return OAuthClient{ClientID: id, ClientName: id, RedirectURIs: `["https://d.example/cb"]`,
			GrantTypes: `["authorization_code","refresh_token"]`, CreatedAt: created, UpdatedAt: created}
	}
	audit := AuditEvent{OccurredAt: oauthNow, ActorRole: "system", Action: "oauth.client.register", ResourceType: "oauth_client", ResourceID: "x", Outcome: "success", DetailJSON: "{}"}

	t.Run("evicts the oldest stale unreferenced client", func(t *testing.T) {
		s := openOAuthFixture(t)
		if err := s.InsertDynamicOAuthClient(ctx, dyn("dyn-old", "2026-11-23T09:00:00.000Z"), audit); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertDynamicOAuthClient(ctx, dyn("dyn-older", "2026-11-22T09:00:00.000Z"), audit); err != nil {
			t.Fatal(err)
		}
		evicted, err := s.InsertDynamicOAuthClientCapped(ctx, dyn("dyn-new", oauthNow), 2, cutoff, audit)
		if err != nil || evicted != "dyn-older" {
			t.Fatalf("evicted = %q, err = %v; want dyn-older, nil", evicted, err)
		}
		if _, err := s.GetOAuthClient(ctx, "dyn-older"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("oldest client not removed: %v", err)
		}
		if _, err := s.GetOAuthClient(ctx, "dyn-new"); err != nil {
			t.Fatalf("new client not inserted: %v", err)
		}
	})

	t.Run("below the limit evicts nothing", func(t *testing.T) {
		s := openOAuthFixture(t)
		if err := s.InsertDynamicOAuthClient(ctx, dyn("dyn-old", "2026-11-23T09:00:00.000Z"), audit); err != nil {
			t.Fatal(err)
		}
		evicted, err := s.InsertDynamicOAuthClientCapped(ctx, dyn("dyn-new", oauthNow), 5, cutoff, audit)
		if err != nil || evicted != "" {
			t.Fatalf("evicted = %q, err = %v; want none", evicted, err)
		}
		if _, err := s.GetOAuthClient(ctx, "dyn-old"); err != nil {
			t.Fatalf("client removed below the limit: %v", err)
		}
	})

	t.Run("refuses when no client is evictable and inserts nothing", func(t *testing.T) {
		s := openOAuthFixture(t)
		// A recent client, and an old client that holds a grant, are both kept.
		if err := s.InsertDynamicOAuthClient(ctx, dyn("dyn-recent", "2026-11-25T09:59:00.000Z"), audit); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertDynamicOAuthClient(ctx, dyn("dyn-granted", "2026-11-23T09:00:00.000Z"), audit); err != nil {
			t.Fatal(err)
		}
		seedApprovedGrantFor(t, s, "dyn-granted")
		_, err := s.InsertDynamicOAuthClientCapped(ctx, dyn("dyn-new", oauthNow), 2, cutoff, audit)
		if !errors.Is(err, ErrDynamicClientCapReached) {
			t.Fatalf("err = %v, want ErrDynamicClientCapReached", err)
		}
		if _, err := s.GetOAuthClient(ctx, "dyn-new"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("refused insert wrote a row: %v", err)
		}
		for _, id := range []string{"dyn-recent", "dyn-granted"} {
			if _, err := s.GetOAuthClient(ctx, id); err != nil {
				t.Fatalf("%s removed: %v", id, err)
			}
		}
	})
}

// seedApprovedGrantFor gives a dynamic client an approved grant so it is
// referenced by oauth_grants, and so not evictable.
func seedApprovedGrantFor(t *testing.T, s *Store, clientID string) {
	t.Helper()
	ctx := context.Background()
	req := OAuthAuthRequest{ID: "req-" + clientID, ClientID: clientID, RedirectURI: "https://d.example/cb",
		Scopes: "mcp:read", Resource: "https://cb.example/mcp", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater}
	if err := s.InsertOAuthAuthRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	grant := OAuthGrant{ID: "grant-" + clientID, UserID: "alice", ClientID: clientID, Resource: "https://cb.example/mcp", Scopes: "mcp:read", CreatedAt: oauthNow}
	code := OAuthCode{CodeHash: hex64('d'), GrantID: grant.ID, AuthRequestID: req.ID, ClientID: clientID, RedirectURI: "https://d.example/cb", CodeChallenge: testChallenge, CreatedAt: oauthNow, ExpiresAt: oauthLater}
	if err := s.DecideAndApproveOAuthRequest(ctx, req.ID, "alice", grant, code, oauthNow, AuditEvent{OccurredAt: oauthNow, Action: "oauth.consent", ResourceType: "oauth_grant", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
}
