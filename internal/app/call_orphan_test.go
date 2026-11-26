package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/callworker"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// TestBadDialBodyThenInitiatedSendsOneHangup covers M1(d), first case. The dial
// returns 2xx with no control id, so CallHarbor cannot confirm the leg and the
// call is recorded as failed with dial_unconfirmed. Telnyx then sends a signed
// call.initiated for the leg it did create. That leg is billable, so the
// orphan path must hang it up exactly once.
func TestBadDialBodyThenInitiatedSendsOneHangup(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-orphan")
	tel.setBadDialBody(true)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "orphan-bad-body"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	waitForCallReason(t, env, h, callID, "dial_unconfirmed")
	if got := waitStatusNow(t, env, h, callID); got != "failed" {
		t.Fatalf("call status after a bad 2xx = %q, want failed", got)
	}

	// The leg Telnyx created reports in. The control id is known only here.
	env.signedWebhook(callID, "call.initiated", "evt-orphan-1", "v3:ctl-orphan")
	waitForHangups(t, tel, 1)
	waitQuiet(tel)
	got := tel.hangupsSeen()
	if len(got) != 1 {
		t.Fatalf("hangups = %v, want exactly one for the orphaned leg", got)
	}
	if got[0] != "v3:ctl-orphan|"+callworker.LegCommandID(callID) {
		t.Fatalf("hangup = %q, want the orphan control id with the call's command id", got[0])
	}
}

// TestCrashAfterDialThenInitiatedSendsOneHangup covers M1(d), second case. The
// process crashes after the dial was sent and before its response was recorded.
// On restart the call is failed with dial_unconfirmed and never redialed. The
// leg Telnyx created then reports call.initiated, which must produce exactly one
// hangup, not zero and not two.
func TestCrashAfterDialThenInitiatedSendsOneHangup(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-crash")
	first := startCallApp(t, tel, nil)
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	first.stop()

	// The crash state: the call was claimed into initiating, the dial reached
	// Telnyx, and the response was never recorded. The queued call seeded by
	// seedRestartRows is not wanted here: it would be dialed on restart and take
	// the fake's one control id, leaving the orphan leg no id of its own.
	seedUnconfirmedOnly(t, first.dataDir, tpID)

	second := restartCallApp(t, tel, first.dataDir, first.pubKey, first.privKey)
	h2 := second.admin.headers(true)
	waitForCallReason(t, second, h2, "call_unconfirmed", "dial_unconfirmed")
	if got := waitStatusNow(t, second, h2, "call_unconfirmed"); got != "failed" {
		t.Fatalf("crashed call status = %q, want failed", got)
	}

	second.signedWebhook("call_unconfirmed", "call.initiated", "evt-crash-1", "v3:ctl-crash")
	waitForHangups(t, tel, 1)
	waitQuiet(tel)
	got := tel.hangupsSeen()
	if len(got) != 1 {
		t.Fatalf("hangups after restart = %v, want exactly one", got)
	}
	if got[0] != "v3:ctl-crash|"+callworker.LegCommandID("call_unconfirmed") {
		t.Fatalf("hangup = %q, want the crashed leg's control id", got[0])
	}
}

// waitForHangups blocks until the fake has received at least n hangups, or
// fails. Hangups are sent after the webhook answers, so the test waits for them.
func waitForHangups(t *testing.T, f *fakeTelnyx, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.hangupsSeen()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake Telnyx received %d hangups, want %d", len(f.hangupsSeen()), n)
}

// seedUnconfirmedOnly inserts only the call a crash leaves in initiating with no
// control id. It is seedRestartRows without the queued call.
func seedUnconfirmedOnly(t *testing.T, dataDir, tpID string) {
	t.Helper()
	seedRestartRowsFor(t, dataDir, tpID, []restartRow{{"call_unconfirmed", "initiating", "restart-unconfirmed"}})
}

// TestHangupBeforeAnsweredEndsCallAsStateMachineDefines covers M4. A
// call.hangup with normal_clearing can arrive before call.answered. The state
// machine keeps that call failed with reason hangup, and a later answered
// completes it to ended. The reason must survive storage, because the rebuild
// reads it back to make that decision. Without the persisted reason, the
// answered would be ignored and the call would stay failed.
func TestHangupBeforeAnsweredEndsCallAsStateMachineDefines(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-m4")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "m4-hangup-first"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)

	env.signedWebhook(callID, "call.initiated", "evt-m4-1", "v3:ctl-m4")
	env.signedHangup(callID, "evt-m4-2", "v3:ctl-m4", "normal_clearing")
	waitForCallReason(t, env, h, callID, "hangup")
	if got := waitStatusNow(t, env, h, callID); got != "failed" {
		t.Fatalf("status after hangup before answered = %q, want failed", got)
	}

	env.signedWebhook(callID, "call.answered", "evt-m4-3", "v3:ctl-m4")
	waitForCallStatus(t, env, h, callID, "ended")
}

// signedHangup posts a signed call.hangup with a hangup_cause. signedWebhook
// carries no cause, so a reason-bearing hangup needs its own body.
func (e *callTestEnv) signedHangup(callID, eventID, controlID, cause string) {
	e.t.Helper()
	clientState := base64.StdEncoding.EncodeToString([]byte(callID))
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": eventID, "event_type": "call.hangup",
		"occurred_at": "2026-11-25T10:00:00Z",
		"payload": map[string]any{"call_control_id": controlID, "client_state": clientState,
			"hangup_cause": cause},
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	h := telnyx.Sign(e.privKey, time.Now(), body)
	h.Set("Content-Type", "application/json")
	status, _, resp := send(e.t, e.client, http.MethodPost, e.base+"/telnyx/calls/"+callID+"/webhook", body, h)
	if status != http.StatusOK {
		e.t.Fatalf("webhook call.hangup = %d %s", status, resp)
	}
}

// TestMakeCallReturnsBeforeTheDialIsAnswered proves make_call is asynchronous
// without relying on wall-clock time. The fake Telnyx holds the dial open. The
// REST create must answer 202 queued while the dial is still held, and only
// after that does the test release the dial and let the worker finish.
func TestMakeCallReturnsBeforeTheDialIsAnswered(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, "v3:ctl-async")
			release := tel.holdDials()
			t.Cleanup(release)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())

			// The dial is held, so the provider has not answered. If make_call dialed
			// synchronously, this request would block on the held gate until the
			// client's 10 s timeout, and the 202 check below would fail.
			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
			}, withIdempotency(h, "async-1"))
			if status != http.StatusAccepted {
				t.Fatalf("create call while the dial is held = %d %s, want 202", status, body)
			}
			if got, _ := decodeObject(t, body)["status"].(string); got != "queued" {
				t.Fatalf("create status = %q, want queued", got)
			}
			release()
			tel.waitDials(t, 1)
		})
	}
}
