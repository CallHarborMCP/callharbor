package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestShutdownHangsUpLiveCallWithOwnersKey checks decision 7 end to end: when
// run's context is cancelled during an active call, the worker sends one
// hangup for the stored provider control id before run returns.
func TestShutdownHangsUpLiveCallWithOwnersKey(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, "v3:ctl-live")
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())

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
		})
	}
}

// TestShutdownWaitsForMediaBeforeStoreCloses covers M2 end to end. A live media
// session is running when run's context is cancelled. run must not return until
// that session has released: the store closes only after the media work is
// done. The shutdown hangup still goes out for the live leg.
func TestShutdownWaitsForMediaBeforeStoreCloses(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, "v3:ctl-m2")
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())

			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
			}, withIdempotency(h, "m2-wait"))
			if status != http.StatusAccepted {
				t.Fatalf("create call = %d %s", status, body)
			}
			callID, _ := decodeObject(t, body)["call_id"].(string)
			tel.waitDials(t, 1)
			env.signedWebhook(callID, "call.initiated", "evt-m2-1", "v3:ctl-m2")
			mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
			if err != nil {
				t.Fatalf("media dial: %v", err)
			}
			defer func() { _ = mediaConn.Close() }()
			sendStart(t, mediaConn, "v3:ctl-m2")

			env.stop()

			// run has returned. Shutdown cancels the session and waits for it before
			// the store closes, so the stream is already closed. A Recv must fail at
			// once, not wait for a frame, so a bounded context keeps it from hanging.
			recvCtx, cancelRecv := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelRecv()
			if _, err := mediaConn.Recv(recvCtx); err == nil {
				t.Fatal("media stream still open after run returned")
			} else if recvCtx.Err() != nil {
				t.Fatal("media stream did not close after run returned")
			}
			got := tel.hangupsSeen()
			if len(got) != 1 || !strings.HasPrefix(got[0], "v3:ctl-m2|") {
				t.Fatalf("hangups = %v, want one for the live leg", got)
			}
		})
	}
}

// TestShutdownClosesMediaStreamBeforeStart covers the window before a start
// frame: the stream is upgraded but no session exists yet. Shutdown must still
// close it, because http.Server.Shutdown does not track hijacked connections.
func TestShutdownClosesMediaStreamBeforeStart(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, "v3:ctl-pre")
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())

			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
			}, withIdempotency(h, "pre-start"))
			if status != http.StatusAccepted {
				t.Fatalf("create call = %d %s", status, body)
			}
			tel.waitDials(t, 1)
			mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
			if err != nil {
				t.Fatalf("media dial: %v", err)
			}
			defer func() { _ = mediaConn.Close() }()

			env.stop()

			recvCtx, cancelRecv := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelRecv()
			if _, err := mediaConn.Recv(recvCtx); err == nil {
				t.Fatal("media stream still open after run returned")
			} else if recvCtx.Err() != nil {
				t.Fatal("media stream without a start frame did not close after run returned")
			}
		})
	}
}
