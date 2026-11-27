package app

import (
	"net/http"
	"testing"
)

// TestHangupWithoutCSRFTokenIsForbiddenAndChangesNothing covers review
// should-fix 2. A session hangup without the CSRF header must return 403, leave
// the call active, and send no hangup to Telnyx. The refusal is audited, because
// the session is authenticated, but no successful hangup row is written.
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
	if n, _ := countHangupAudit(t, env.dataDir, callID, "success"); n != 0 {
		t.Fatalf("successful hangup audit rows = %d, want 0", n)
	}
}
