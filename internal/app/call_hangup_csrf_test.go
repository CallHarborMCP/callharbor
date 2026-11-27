package app

import (
	"net/http"
	"testing"
)

// TestHangupWithoutCSRFTokenIsForbiddenAndChangesNothing covers review
// should-fix 2. A session hangup without the CSRF header must return 403, leave
// the call active, and send no hangup to Telnyx. The CSRF check runs in the REST
// handler before the service, so the refusal writes no call.hangup audit row of
// any outcome.
func TestHangupWithoutCSRFTokenIsForbiddenAndChangesNothing(t *testing.T) {
	const ctl = "v3:ctl-csrf"
	env, tel, _, callID, _ := startOwnerCall(t, ctl, "csrf-hangup")

	// Cookie only: no X-CSRF-Token header.
	status, _, body := send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, env.admin.headers(false))
	if status != http.StatusForbidden {
		t.Fatalf("hangup without CSRF = %d %s, want 403", status, body)
	}
	if got := waitStatusNow(t, env, env.admin.headers(true), callID); got != "active" {
		t.Fatalf("call status after refused hangup = %q, want active (unchanged)", got)
	}
	if n := len(tel.hangupsSeen()); n != 0 {
		t.Fatalf("hangups sent to Telnyx after CSRF refusal = %d, want 0", n)
	}
	if n := countAllHangupAudit(t, env.dataDir, callID); n != 0 {
		t.Fatalf("call.hangup audit rows of any outcome = %d, want 0", n)
	}
}

// countAllHangupAudit counts every call.hangup audit row for one call, of any
// outcome.
func countAllHangupAudit(t *testing.T, dataDir, callID string) int {
	t.Helper()
	db := openAppDB(t, dataDir)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events
		WHERE action = 'call.hangup' AND resource_id = ?`, callID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
