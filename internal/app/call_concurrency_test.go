package app

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// TestRetriedAndConcurrentMakeCallDialsOnce checks the one-dial guarantee. The
// same idempotency key sent concurrently, then sent again, must create one call
// and reach Telnyx with exactly one dial.
func TestRetriedAndConcurrentMakeCallDialsOnce(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			retriedAndConcurrentMakeCall(t, protocol)
		})
	}
}

func retriedAndConcurrentMakeCall(t *testing.T, protocol realtime.Protocol) {
	tel := newFakeTelnyx(t, "v3:ctl-retry")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())

	req := map[string]any{"profile_id": tpID, "to": "+14155550111", "objective": "Confirm"}
	const workers = 4
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", req, withIdempotency(h, "retry-1"))
			if status != http.StatusAccepted {
				t.Errorf("concurrent create = %d %s", status, body)
				return
			}
			ids[i], _ = decodeObject(t, body)["call_id"].(string)
		}()
	}
	wg.Wait()
	// A later retry after the dial must also return the same call.
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", req, withIdempotency(h, "retry-1"))
	if status != http.StatusAccepted {
		t.Fatalf("retry = %d %s", status, body)
	}
	retryID, _ := decodeObject(t, body)["call_id"].(string)
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("concurrent ids[%d] = %q, want %q", i, id, ids[0])
		}
	}
	if retryID != ids[0] {
		t.Fatalf("retry id = %q, want %q", retryID, ids[0])
	}
	tel.waitDials(t, 1)
	// Give a second dial a chance to appear before counting.
	waitQuiet(tel)
	if n := len(tel.dialBodies()); n != 1 {
		t.Fatalf("dials = %d, want exactly one", n)
	}
}

// waitQuiet lets the worker settle for a short interval, so a stray second
// dial would be seen. It only ever makes a missing dial more likely to appear.
func waitQuiet(f *fakeTelnyx) {
	for range 20 {
		_ = f.dialBodies()
		time.Sleep(10 * time.Millisecond)
	}
}
