package app

import (
	"net/http"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/callworker"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// legHangupCommandID is the command id the production hangup sends for a call,
// taken from the same derivation the worker uses.
func legHangupCommandID(callID string) string { return callworker.LegCommandID(callID) }

// TestMaxDurationCapEndsTheCall drives a call with a one-second cap to active
// over the signed webhooks and media stream. When the cap expires, the AI
// session must close, the call must move to ending, and exactly one hangup must
// reach Telnyx for the call's control id. A signed call.hangup then moves the
// call to ended. The test waits on the cap, not a fixed sleep.
func TestMaxDurationCapEndsTheCall(t *testing.T) {
	const ctl = "v3:ctl-cap"
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())

	callID := createCapCall(t, env, h, tpID, "cap-1")
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-cap-1", ctl)
	env.signedWebhook(callID, "call.answered", "evt-cap-2", ctl)

	mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	defer func() { _ = mediaConn.Close() }()
	sendStart(t, mediaConn, ctl)

	waitAISessionClosed(t, ai)
	waitTechnicalStatus(t, env, h, callID, "ending")

	// Exactly one hangup, for the stored control id. The command id is the
	// deterministic one, so a retry of the same leg is idempotent at Telnyx.
	waitHangups(t, tel, 1)
	got := tel.hangupsSeen()
	if len(got) != 1 || got[0] != ctl+"|"+legHangupCommandID(callID) {
		t.Fatalf("hangups = %v, want one for %s with the leg command id", got, ctl)
	}
	if reason := callReason(t, env, h, callID); reason != "max_duration" {
		t.Fatalf("outcome_reason_code = %q, want max_duration", reason)
	}

	// The hangup is terminal only once Telnyx reports it.
	env.signedWebhook(callID, "call.hangup", "evt-cap-3", ctl)
	waitTechnicalStatus(t, env, h, callID, "ended")
	if n := len(tel.hangupsSeen()); n != 1 {
		t.Fatalf("hangups after call.hangup = %d, want still 1", n)
	}
}

// TestMediaFailureEndsTheCallWithOneHangup covers the case where the AI
// provider drops mid-call. CallHarbor must end the call itself: ending, then
// one hangup for the leg, with reason media_failed. The call must not be left
// live.
func TestMediaFailureEndsTheCallWithOneHangup(t *testing.T) {
	const ctl = "v3:ctl-drop"
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())

	callID := createCapCall(t, env, h, tpID, "drop-1")
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-drop-1", ctl)
	env.signedWebhook(callID, "call.answered", "evt-drop-2", ctl)

	mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	defer func() { _ = mediaConn.Close() }()
	sendStart(t, mediaConn, ctl)

	// Wait until the session is live, so the drop happens mid-call and not
	// during setup. The instructions event is the first one the provider sees.
	waitEvents(t, ai, 1)
	ai.drop()

	waitTechnicalStatus(t, env, h, callID, "ending")
	waitHangups(t, tel, 1)
	if got := tel.hangupsSeen(); len(got) != 1 || got[0] != ctl+"|"+legHangupCommandID(callID) {
		t.Fatalf("hangups = %v, want one for %s", got, ctl)
	}
	if reason := callReason(t, env, h, callID); reason != "media_failed" {
		t.Fatalf("outcome_reason_code = %q, want media_failed", reason)
	}
}

// createCapCall queues a one-second call and returns its id.
func createCapCall(t *testing.T, env *callTestEnv, h http.Header, tpID, key string) string {
	t.Helper()
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
		"max_duration_seconds": 1,
	}, withIdempotency(h, key))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	id, _ := decodeObject(t, body)["call_id"].(string)
	return id
}

// waitAISessionClosed waits until the fake provider sees its session close.
func waitAISessionClosed(t *testing.T, ai *fakeAI) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range ai.snapshot() {
			if ev == "closed" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("AI session did not close within 5s; events = %v", ai.snapshot())
}

// waitTechnicalStatus polls the call until technical_status is want, or fails.
func waitTechnicalStatus(t *testing.T, env *callTestEnv, h http.Header, callID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		got = waitStatusNow(t, env, h, callID)
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("call %s technical_status = %q, want %q", callID, got, want)
}

// waitHangups waits until the fake Telnyx has received at least n hangups.
func waitHangups(t *testing.T, tel *fakeTelnyx, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(tel.hangupsSeen()) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake Telnyx received %d hangups, want %d", len(tel.hangupsSeen()), n)
}

// callReason returns the call's outcome_reason_code from the REST detail view.
func callReason(t *testing.T, env *callTestEnv, h http.Header, callID string) string {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
	if status != http.StatusOK {
		t.Fatalf("get call = %d %s", status, body)
	}
	details, _ := decodeObject(t, body)["details"].(map[string]any)
	reason, _ := details["outcome_reason_code"].(string)
	return reason
}
