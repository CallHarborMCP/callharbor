package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// TestShutdownHangsUpLiveCallWithOwnersKey checks decision 7 end to end: when
// run's context is cancelled during an active call, the worker sends one
// hangup for the stored provider control id before run returns.
func TestShutdownHangsUpLiveCallWithOwnersKey(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-live")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "shutdown-1"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-sd-1", "v3:ctl-live")
	env.signedWebhook(callID, "call.answered", "evt-sd-2", "v3:ctl-live")
	waitForCallStatus(t, env, h, callID, "active")

	env.stop()

	got := tel.hangupsSeen()
	if len(got) != 1 {
		t.Fatalf("hangups = %v, want exactly one for the live leg", got)
	}
	if !strings.HasPrefix(got[0], "v3:ctl-live|") {
		t.Fatalf("hangup = %q, want the stored control id", got[0])
	}
}
