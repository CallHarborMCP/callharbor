package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// aiFlowProtocols are the two protocols the acceptance flow runs over.
var aiFlowProtocols = []realtime.Protocol{realtime.ProtocolRealtime, realtime.ProtocolLive}

// TestCallFlowEndToEnd is the step 6 acceptance flow for each protocol. It
// creates the profiles, queues one call through REST, dials it once, and drives
// the signed answer and media stream. Instructions must reach the provider
// before any caller audio.
func TestCallFlowEndToEnd(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, "v3:ctl-1")
			ai := newFakeAI(t, protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)

			// Telephony profile. The webhook key is the Ed25519 public key the
			// test signs with, so the app verifies the test's signatures.
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, ai.endpoint())

			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": tpID, "to": "+14155550111", "objective": "Confirm the appointment",
				// context and instructions are each under their own rune limit, but
				// together with the constraints they pass 20 KB of rendered brief.
				"context": strings.Repeat("c", 19000), "instructions": strings.Repeat("i", 8000),
				"language": "en-US", "constraints": []string{"Do not share payment details"},
			}, withIdempotency(h, "flow-1"))
			if status != http.StatusAccepted {
				t.Fatalf("create call = %d %s", status, body)
			}
			callID, _ := decodeObject(t, body)["call_id"].(string)
			if callID == "" {
				t.Fatalf("no call_id: %s", body)
			}
			tel.waitDials(t, 1)
			if n := len(tel.dialBodies()); n != 1 {
				t.Fatalf("dials = %d, want exactly one", n)
			}
			env.signedWebhook(callID, "call.initiated", "evt-init-1", "v3:ctl-1")
			env.signedWebhook(callID, "call.answered", "evt-ans-1", "v3:ctl-1")

			mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
			if err != nil {
				t.Fatalf("media dial: %v", err)
			}
			defer func() { _ = mediaConn.Close() }()
			sendStart(t, mediaConn, "v3:ctl-1")
			sendMedia(t, mediaConn, "AAAAAAAAAAA=")

			// Instructions must reach the provider before the first caller audio.
			got := waitEvents(t, ai, 2)
			if !strings.HasPrefix(got[0], aiEventInstructions+":") {
				t.Fatalf("first provider event = %q, want instructions", got[0])
			}
			if got[1] != aiEventAudio {
				t.Fatalf("second provider event = %q, want caller audio", got[1])
			}
			instr := strings.TrimPrefix(got[0], aiEventInstructions+":")
			if !strings.Contains(instr, "Confirm the appointment") ||
				!strings.Contains(instr, "Do not share payment details") ||
				!strings.Contains(instr, "en-US") {
				t.Fatalf("instructions lack brief fields: %q", instr)
			}
			if len(instr) < 20*1024 {
				t.Fatalf("instructions = %d bytes, want the full over-20KB context", len(instr))
			}
		})
	}
}

// createTelephonyFixture creates and validates a telephony profile against the
// fake Telnyx. It returns the profile id.
func createTelephonyFixture(t *testing.T, env *callTestEnv, h http.Header) string {
	t.Helper()
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/telephony-profiles", map[string]any{
		"name": "main", "caller_number": fakeCallerNumber, "connection_id": fakeConnectionID,
		"api_key": "KEY-flow-0123456789", "webhook_public_key": env.webhookKeyB64(),
	}, h)
	if status != http.StatusCreated {
		t.Fatalf("create telephony = %d %s", status, body)
	}
	id, _ := decodeObject(t, body)["id"].(string)
	status, _, body = send(t, env.client, http.MethodPost, env.base+"/api/v1/telephony-profiles/"+id+"/validate", nil, h)
	if status != http.StatusOK {
		t.Fatalf("validate = %d %s", status, body)
	}
	if got := decodeObject(t, body)["connectivity_status"]; got != "verified" {
		t.Fatalf("validation status = %v, want verified: %s", got, body)
	}
	return id
}

// aiFixtureSeq makes each fixture's secret name and profile id unique, so one
// test can create several AI profiles for the same owner.
var aiFixtureSeq atomic.Int64

// createAIFixture creates the AI profile the call will use. Its API key is a
// test value; the fake provider does not check it.
func createAIFixture(t *testing.T, env *callTestEnv, h http.Header, protocol realtime.Protocol, endpoint string) {
	t.Helper()
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/secrets", map[string]any{
		"name": fmt.Sprintf("ai-%d", aiFixtureSeq.Add(1)), "purpose": "ai_api_key", "value": "sk-flow-0123456789",
	}, h)
	if status != http.StatusCreated {
		t.Fatalf("create ai secret = %d %s", status, body)
	}
	secretID, _ := decodeObject(t, body)["id"].(string)
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/ai-profiles", map[string]any{
		"id": "ai-" + secretID, "name": "agent-" + secretID, "protocol": string(protocol), "endpoint_url": endpoint,
		"model": "model-x", "voice": "alloy", "instructions": "You are a careful assistant.",
		"api_secret_id": secretID,
	}, h)
	if status != http.StatusCreated {
		t.Fatalf("create ai profile = %d %s", status, body)
	}
}

// withIdempotency returns a copy of h with the Idempotency-Key header set.
func withIdempotency(h http.Header, key string) http.Header {
	c := h.Clone()
	c.Set("Idempotency-Key", key)
	return c
}

// sendStart sends the Telnyx start frame for the stream. Its control id must
// match the one the webhooks carried, or the stream is refused.
func sendStart(t *testing.T, s *telnyxfake.Stream, controlID string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event": "start", "sequence_number": "1", "stream_id": "s1",
		"start": map[string]any{
			"call_control_id": controlID,
			"media_format":    map[string]any{"encoding": "PCMU", "sample_rate": 8000, "channels": 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), b); err != nil {
		t.Fatalf("send start: %v", err)
	}
}

// sendMedia sends one inbound Telnyx media frame carrying base64 PCMU audio.
func sendMedia(t *testing.T, s *telnyxfake.Stream, payload string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event": "media", "sequence_number": "2", "stream_id": "s1",
		"media": map[string]any{"track": "inbound", "chunk": "1", "timestamp": "0", "payload": payload},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), b); err != nil {
		t.Fatalf("send media: %v", err)
	}
}

// waitEvents waits until the fake provider has recorded at least n events.
func waitEvents(t *testing.T, ai *fakeAI, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev := ai.snapshot(); len(ev) >= n {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("provider recorded %v, want at least %d events", ai.snapshot(), n)
	return nil
}
