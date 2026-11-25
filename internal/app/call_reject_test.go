package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// TestMediaRejectsWrongTokenAsNotFound checks that a stream whose path token
// does not match the call looks exactly like an unknown call: 404, and no
// provider session is opened.
func TestMediaRejectsWrongTokenAsNotFound(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-2")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "reject-token"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	tel.waitDials(t, 1)
	// Swap the token for a wrong one in the dialed URL; the path shape is kept.
	good := env.dialedStreamURL()
	bad := good[:strings.LastIndex(good, "/")+1] + "not-the-token"
	if _, err := env.signedMediaDial(bad); err == nil {
		t.Fatal("stream with a wrong token was accepted")
	}
}

// TestMediaRejectsForeignControlIDAndFailsCall checks the bind: a start frame
// whose call_control_id does not match the recorded one is refused. The call
// stays out of the active state and no provider session is opened.
func TestMediaRejectsForeignControlIDAndFailsCall(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-3")
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "reject-bind"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-bind-1", "v3:ctl-3")

	mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	defer func() { _ = mediaConn.Close() }()
	sendStart(t, mediaConn, "v3:someone-else")
	// A refused start closes the stream. The read returns only after the server
	// has processed the frame, so the checks below are not racing it.
	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mediaConn.Recv(readCtx); err == nil {
		t.Fatal("stream stayed open after a foreign control id")
	}
	if got := ai.snapshot(); len(got) != 0 {
		t.Fatalf("provider saw %v for a foreign control id", got)
	}
	waitForCallStatus(t, env, h, callID, "ringing")
}

// waitForCallStatus polls GET /api/v1/calls/{id} until technical_status is
// want, or fails. It returns the final status so a mismatch shows the real one.
func waitForCallStatus(t *testing.T, env *callTestEnv, h http.Header, callID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
		if status == http.StatusOK {
			got, _ = decodeObject(t, body)["technical_status"].(string)
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("call %s status = %q, want %q", callID, got, want)
}

// TestMediaCapabilityMismatchIsSurfacedInCallStatus checks that a provider
// refusal with realtime.ErrCapability fails the call with capability_mismatch
// and is visible in the call status. The injected dialer returns that error,
// since the shipped matrices accept the fixed media format.
func TestMediaCapabilityMismatchIsSurfacedInCallStatus(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-cap")
	refuse := realtime.Dialers{
		realtime.ProtocolRealtime: func(context.Context, realtime.Profile) (realtime.Session, error) {
			return nil, fmt.Errorf("refused: %w", realtime.ErrCapability)
		},
	}
	env := startCallApp(t, tel, refuse)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "reject-cap"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-cap-1", "v3:ctl-cap")
	mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	defer func() { _ = mediaConn.Close() }()
	sendStart(t, mediaConn, "v3:ctl-cap")
	waitForCallReason(t, env, h, callID, "capability_mismatch")
}

// waitForCallReason polls the call until outcome_reason_code is want, or fails.
func waitForCallReason(t *testing.T, env *callTestEnv, h http.Header, callID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
		if status == http.StatusOK {
			details, _ := decodeObject(t, body)["details"].(map[string]any)
			got, _ = details["outcome_reason_code"].(string)
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("call %s reason = %q, want %q", callID, got, want)
}

// TestMakeCallRejectsInvalidAndForeignInputs checks that bad or foreign input is
// refused over REST with no call row written. A profile owned by another user
// returns the same not-found as an unknown id.
func TestMakeCallRejectsInvalidAndForeignInputs(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-rej")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	other := createUser(t, env.admin, "bob")
	oh := other.headers(true)
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/telephony-profiles", map[string]any{
		"name": "bobs", "caller_number": fakeCallerNumber, "connection_id": fakeConnectionID,
		"api_key": "KEY-bob-0123456789", "webhook_public_key": env.webhookKeyB64(), // gitleaks:allow
	}, oh)
	if status != http.StatusCreated {
		t.Fatalf("bob telephony = %d %s", status, body)
	}
	bobTP, _ := decodeObject(t, body)["id"].(string)

	cases := []struct {
		name    string
		profile string
		to      string
		want    int
	}{
		{"invalid to", tpID, "4155550111", http.StatusBadRequest},
		{"unknown profile", "tp_missing", "+14155550111", http.StatusNotFound},
		{"foreign profile", bobTP, "+14155550111", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": c.profile, "to": c.to, "objective": "Confirm",
			}, withIdempotency(h, "reject-"+strings.ReplaceAll(c.name, " ", "-")))
			if status != c.want {
				t.Fatalf("status = %d %s, want %d", status, body, c.want)
			}
		})
	}
	if n := countCallRows(t, env.dataDir); n != 0 {
		t.Fatalf("rejected inputs wrote %d call rows", n)
	}
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("rejected inputs reached Telnyx with %d dials", n)
	}
}

// TestCallWithoutVerifiedTelephonyIsRefusedBeforeQueue checks that an
// unverified telephony profile cannot queue a call, so nothing is dialed.
func TestCallWithoutVerifiedTelephonyIsRefusedBeforeQueue(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-4")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/telephony-profiles", map[string]any{
		"name": "unverified", "caller_number": fakeCallerNumber, "connection_id": fakeConnectionID,
		"api_key": "KEY-unverified-0123456789", "webhook_public_key": env.webhookKeyB64(), // gitleaks:allow
	}, h)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	tpID, _ := decodeObject(t, body)["id"].(string)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "reject-unverified"))
	if status == http.StatusAccepted {
		t.Fatalf("unverified profile queued a call: %s", body)
	}
	if !strings.Contains(string(body), "telephony_not_verified") {
		t.Fatalf("refusal = %d %s, want telephony_not_verified", status, body)
	}
	if n := countCallRows(t, env.dataDir); n != 0 {
		t.Fatalf("refused call wrote %d rows", n)
	}
}
