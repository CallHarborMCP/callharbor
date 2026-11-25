package app

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// TestOAuthE2EInactiveUserWithLiveGrantIsRefusedAtMCP sets users.active = 0
// through SQL, with no revocation of the grant, then checks that the running
// app refuses the still-live access token on /mcp. Deactivation through the API
// also revokes grants, so only this test shows the bearer path's own check.
func TestOAuthE2EInactiveUserWithLiveGrantIsRefusedAtMCP(t *testing.T) {
	dir := t.TempDir()
	e := startE2E(t, dir)
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")
	if status, _, _ := e.mcpWith(tok.AccessToken); status != http.StatusNotImplemented {
		t.Fatalf("/mcp before deactivation = %d, want 501", status)
	}

	uid := e.userID(alice)
	deactivateWithoutRevoking(t, dir, uid)
	// Read the grant from storage: alice's session is already invalid, so the
	// API cannot answer. The grant must still be live, or the 401 could come
	// from revocation rather than the user check under test.
	grants, err := storageGrantsFor(t, dir, uid)
	if err != nil || len(grants) != 1 || grants[0].RevokedAt != "" {
		t.Fatalf("grant must stay live for this test: %+v err=%v", grants, err)
	}

	status, hdr, _ := e.mcpWith(tok.AccessToken)
	if status != http.StatusUnauthorized || !strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("/mcp for inactive user with live grant = %d %q, want 401 invalid_token", status, hdr.Get("WWW-Authenticate"))
	}
}

// deactivateWithoutRevoking clears users.active through a second SQLite
// connection to the app's database, leaving the user's OAuth grants live. The
// app is idle while the test runs, and WAL allows the second connection.
func deactivateWithoutRevoking(t *testing.T, dataDir, userID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(context.Background(), "UPDATE users SET active = 0 WHERE id = ?", userID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("deactivate user: %d rows, want 1", n)
	}
}

// storageGrantsFor lists the user's grants, revoked or not, from the app's
// database.
func storageGrantsFor(t *testing.T, dataDir, userID string) ([]storage.OAuthGrant, error) {
	t.Helper()
	st, err := storage.Open(context.Background(), filepath.Join(dataDir, databaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	return st.ListOAuthGrants(context.Background(), userID)
}
