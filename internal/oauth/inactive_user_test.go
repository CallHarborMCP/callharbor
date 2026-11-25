package oauth

import (
	"database/sql"
	"net/http"
	"net/url"
	"testing"
)

// The tests in this file set users.active = 0 directly, with no revocation of
// the user's grants. Production deactivation (UpdateUser) revokes grants in the
// same transaction, which would make the checks below pass for that reason
// alone. Each test therefore proves that the path refuses an inactive user by
// itself.

// deactivateUserWithoutRevoking clears users.active through a second SQLite
// connection to the fixture's database. Grants are left live.
func (f *fixture) deactivateUserWithoutRevoking() {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(f.ctx, "UPDATE users SET active = 0 WHERE id = ?", f.userID)
	if err != nil {
		f.t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		f.t.Fatalf("deactivate user: %d rows, want 1", n)
	}
}

// TestInactiveUserBearerIsRejectedWithGrantLive: an access token whose grant is
// still live is refused on /mcp once the user is inactive.
func TestInactiveUserBearerIsRejectedWithGrantLive(t *testing.T) {
	f := newFixture(t)
	access, grantID := f.bearerFixture(t)
	f.deactivateUserWithoutRevoking()
	if g, err := f.store.GetOAuthGrantByID(f.ctx, grantID); err != nil || g.RevokedAt != "" {
		t.Fatalf("grant must stay live for this test: revoked=%q err=%v", g.RevokedAt, err)
	}
	rec, _, seen := f.guarded(t, bearerReq(access), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "invalid_token")
	if seen {
		t.Fatal("inactive user reached the protected handler")
	}
}

// TestInactiveUserRefreshIsInvalidGrantWithGrantLive: a refresh for an inactive
// user is refused with invalid_grant while the grant itself stays live.
func TestInactiveUserRefreshIsInvalidGrantWithGrantLive(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	f.deactivateUserWithoutRevoking()
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("refresh status = %d body = %v; want 400 invalid_grant", status, body)
	}
	grants, err := f.store.ListOAuthGrants(f.ctx, f.userID)
	if err != nil || len(grants) != 1 || grants[0].RevokedAt != "" {
		t.Fatalf("grant should still be live: %+v err=%v", grants, err)
	}
}

// TestInactiveUserCodeExchangeIsInvalidGrantWithGrantLive: a code exchange for an
// inactive user is refused with invalid_grant and issues no tokens.
func TestInactiveUserCodeExchangeIsInvalidGrantWithGrantLive(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	f.deactivateUserWithoutRevoking()
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("code exchange status = %d body = %v; want 400 invalid_grant", status, body)
	}
	if _, ok := body["access_token"]; ok {
		t.Fatal("inactive user received an access token")
	}
}
