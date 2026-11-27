package app

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/callpolicy"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// policyCallBody is a REST make-call body to the given destination.
func policyCallBody(profileID, to string) map[string]any {
	return map[string]any{"profile_id": profileID, "to": to, "objective": "Confirm"}
}

// postCall queues one call over REST with the given idempotency key and
// returns the status and body.
func postCall(t *testing.T, env *callTestEnv, h http.Header, body map[string]any, key string) (int, []byte) {
	t.Helper()
	status, _, resp := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", body, withIdempotency(h, key))
	return status, resp
}

// callStatus reads one call over REST and returns its technical status and its
// outcome reason code. The view nests the reason under details.
func callStatus(t *testing.T, env *callTestEnv, h http.Header, callID string) (string, string) {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
	if status != http.StatusOK {
		t.Fatalf("get call = %d %s", status, body)
	}
	m := decodeObject(t, body)
	s, _ := m["technical_status"].(string)
	details, _ := m["details"].(map[string]any)
	r, _ := details["outcome_reason_code"].(string)
	return s, r
}

// requireRefused asserts a REST refusal: the status, the stable reason prefix
// in the error message, and that the fake Telnyx saw no dial.
func requireRefused(t *testing.T, tel *fakeTelnyx, status int, body []byte, wantStatus int, reason string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d: %s", status, wantStatus, body)
	}
	if !strings.Contains(string(body), reason) {
		t.Fatalf("body lacks reason %q: %s", reason, body)
	}
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("refused call reached Telnyx with %d dials", n)
	}
}

// TestRESTDeniedDestinationNeverDials: an installation deny prefix refuses the
// call with a 403 and a stable reason, before any row or dial.
func TestRESTDeniedDestinationNeverDials(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-deny")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.DenyPrefixes = []string{"+1415"}
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	status, body := postCall(t, env, h, policyCallBody(tpID, "+14155550111"), "deny-1")
	requireRefused(t, tel, status, body, http.StatusForbidden, callpolicy.ReasonDestinationDenied)
	if strings.Contains(string(body), "+14155550111") {
		t.Fatalf("refusal echoes the full phone number: %s", body)
	}
}

// TestRESTAllowListRefusesUnlistedDestination: an allow list refuses a number
// outside it with destination_not_allowed and no dial.
func TestRESTAllowListRefusesUnlistedDestination(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-allow")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.AllowPrefixes = []string{"+44"}
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	status, body := postCall(t, env, h, policyCallBody(tpID, "+14155550111"), "allow-1")
	requireRefused(t, tel, status, body, http.StatusForbidden, callpolicy.ReasonDestinationNotAllowed)
}

// TestRESTOwnerOverrideCannotLoosenInstallationDeny: an owner allow list cannot
// reopen a destination the installation denies. The admin's policy write is
// what the owner sees; the owner's own call is refused.
func TestRESTOwnerOverrideCannotLoosenInstallationDeny(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-loosen")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.DenyPrefixes = []string{"+1415"}
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	alice := createUser(t, env.admin, "alice")
	ah := alice.headers(true)
	createAIFixture(t, env, ah, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	status, _, grant := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, alice), nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("grant = %d %s", status, grant)
	}

	// The admin's per-user override tries to allow +1415, which the
	// installation denies. The effective policy must still deny it.
	status, _, body := sendJSON(t, env.client, http.MethodPut, env.base+"/api/v1/users/"+bobID(t, alice)+"/call-policy",
		map[string]any{"allow_prefixes": []string{"+1415"}}, h)
	if status != http.StatusOK {
		t.Fatalf("put policy = %d %s", status, body)
	}
	eff := decodeObject(t, body)["effective"].(map[string]any)
	if !containsAny(eff["deny_prefixes"], "+1415") {
		t.Fatalf("effective policy lost the installation deny: %s", body)
	}

	status, resp := postCall(t, env, ah, policyCallBody(tpID, "+14155550111"), "loosen-1")
	requireRefused(t, tel, status, resp, http.StatusForbidden, callpolicy.ReasonDestinationDenied)
}

// TestRESTExplicitDurationAbovePolicyNeverDials: an explicit duration over the
// installation maximum is refused with a 400 and no dial.
func TestRESTExplicitDurationAbovePolicyNeverDials(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-dur")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.MaxCallDurationSeconds = 300
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	body := policyCallBody(tpID, "+14155550111")
	body["max_duration_seconds"] = 600
	status, resp := postCall(t, env, h, body, "dur-1")
	requireRefused(t, tel, status, resp, http.StatusBadRequest, callpolicy.ReasonDurationExceedsPolicy)
}

// TestRESTCostLimitWithoutRateNeverDials: an enforced cost limit with no
// estimate rate fails safe. The unknown cost is refused and nothing is dialed.
func TestRESTCostLimitWithoutRateNeverDials(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-cost")
	limit, err := config.ParseMicros("1.00")
	if err != nil {
		t.Fatal(err)
	}
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.MaxCallCostMicros = &limit
		c.CallPolicy.CallCostCurrency = "USD"
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	status, body := postCall(t, env, h, policyCallBody(tpID, "+14155550111"), "cost-1")
	requireRefused(t, tel, status, body, http.StatusConflict, callpolicy.ReasonCostUnavailable)
}

// TestRESTCostEstimateOverLimitNeverDials: with a rate, the estimate for the
// call's duration above the limit is refused with cost_limit_exceeded.
func TestRESTCostEstimateOverLimitNeverDials(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-cost2")
	limit, rate := int64(1_000_000), int64(250_000)
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.MaxCallCostMicros = &limit
		c.CallPolicy.CallCostEstimateMicrosPerMinute = &rate
		c.CallPolicy.CallCostCurrency = "USD"
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	body := policyCallBody(tpID, "+14155550111")
	body["max_duration_seconds"] = 300 // 5 min × 0.25 = 1.25 > 1.00
	status, resp := postCall(t, env, h, body, "cost-2")
	requireRefused(t, tel, status, resp, http.StatusForbidden, callpolicy.ReasonCostLimitExceeded)
}

// TestRESTRateLimitReturns429: the hourly per-user limit returns 429 with
// rate_limited on the second call, and the second call writes no row.
func TestRESTRateLimitReturns429(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-rate")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.UserMaxCallsPerHour = 1
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	if status, body := postCall(t, env, h, policyCallBody(tpID, "+14155550111"), "rate-1"); status != http.StatusAccepted {
		t.Fatalf("first call = %d %s", status, body)
	}
	tel.waitDials(t, 1)
	status, body := postCall(t, env, h, policyCallBody(tpID, "+14155550111"), "rate-2")
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), callpolicy.ReasonRateLimited) {
		t.Fatalf("second call = %d %s, want 429 %s", status, body, callpolicy.ReasonRateLimited)
	}
	if n := len(tel.dialBodies()); n != 1 {
		t.Fatalf("dials = %d, want exactly the first call", n)
	}
}

// TestRESTConcurrencyLimitUnderParallelRequests: with a per-user concurrency
// limit of 2, eight parallel requests with distinct keys accept exactly 2. The
// refusals write no rows and never dial.
func TestRESTConcurrencyLimitUnderParallelRequests(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-conc")
	release := tel.holdDials()
	t.Cleanup(release)
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.UserMaxConcurrentCalls = 2
		c.CallPolicy.UserMaxCallsPerHour = 1000
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	const n = 8
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, _, _ := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
				policyCallBody(tpID, "+14155550111"), withIdempotency(h, "conc-"+string(rune('a'+i))))
			statuses[i] = s
		}(i)
	}
	wg.Wait()
	accepted, refused := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			refused++
		default:
			t.Fatalf("unexpected status %d in %v", s, statuses)
		}
	}
	if accepted != 2 || refused != n-2 {
		t.Fatalf("accepted=%d refused=%d, want 2 and %d (statuses %v)", accepted, refused, n-2, statuses)
	}
	release()
	tel.waitDials(t, 2)
}

// TestPolicyTightenedAfterQueueSendsNothing: a call queued under a permissive
// policy is not dialed once an administrator denies the destination before the
// worker dials. The call fails with destination_denied and Telnyx sees nothing.
func TestPolicyTightenedAfterQueueSendsNothing(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-late")
	release := tel.holdDials()
	t.Cleanup(release)
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	// First call holds the worker in its dial, so the second stays queued.
	if s, body := postCall(t, env, h, policyCallBody(tpID, "+14155550111"), "late-1"); s != http.StatusAccepted {
		t.Fatalf("first = %d %s", s, body)
	}
	tel.waitDials(t, 1)
	status, body := postCall(t, env, h, policyCallBody(tpID, "+14155550122"), "late-2")
	if status != http.StatusAccepted {
		t.Fatalf("queued call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)

	// The admin denies the destination's prefix before the worker reaches the
	// queued call. Installation policy is fixed at start, so the change is made
	// through the owner's own override path: deny the owner's prefix.
	ownerID := bobID(t, env.admin)
	status, _, pb := sendJSON(t, env.client, http.MethodPut, env.base+"/api/v1/users/"+ownerID+"/call-policy",
		map[string]any{"deny_prefixes": []string{"+1415"}}, h)
	if status != http.StatusOK {
		t.Fatalf("tighten = %d %s", status, pb)
	}

	release()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		s, r := callStatus(t, env, h, callID)
		if s == "failed" {
			got = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != callpolicy.ReasonDestinationDenied {
		t.Fatalf("queued call reason = %q, want %s", got, callpolicy.ReasonDestinationDenied)
	}
	// Only the first call, queued before the tightening, may have dialed. The
	// queued destination must never reach Telnyx.
	for _, d := range tel.dialBodies() {
		if to, _ := d["to"].(string); to == "+14155550122" {
			t.Fatalf("the denied queued destination was dialed")
		}
	}
	if n := len(tel.dialBodies()); n != 1 {
		t.Fatalf("dials = %d, want 1 (only the call queued before the tightening)", n)
	}
}

// containsAny reports whether list (a decoded JSON array) holds want.
func containsAny(list any, want string) bool {
	items, _ := list.([]any)
	for _, p := range items {
		if s, _ := p.(string); s == want {
			return true
		}
	}
	return false
}

// TestRESTSelfReadShowsOnlyOwnEffectivePolicy: a user reads her effective
// policy, and the response never includes another user's overrides.
func TestRESTSelfReadShowsOnlyOwnEffectivePolicy(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-self")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
	})
	alice := createUser(t, env.admin, "alice")
	createUser(t, env.admin, "carol")
	carolID := bobID(t, login(t, env.client, env.base, "carol"))
	status, _, body := sendJSON(t, env.client, http.MethodPut, env.base+"/api/v1/users/"+carolID+"/call-policy",
		map[string]any{"deny_prefixes": []string{"+1999"}}, env.admin.headers(true))
	if status != http.StatusOK {
		t.Fatalf("admin put = %d %s", status, body)
	}

	status, _, body = send(t, env.client, http.MethodGet, env.base+"/api/v1/call-policy", nil, alice.headers(false))
	if status != http.StatusOK {
		t.Fatalf("self read = %d %s", status, body)
	}
	if strings.Contains(string(body), "+1999") {
		t.Fatalf("self read leaks another user's override: %s", body)
	}
}

// TestRESTPolicyWriteNeedsAdminAndCSRF: a non-admin cannot write a policy, and
// an admin write without the CSRF token is refused.
func TestRESTPolicyWriteNeedsAdminAndCSRF(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-authz")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
	})
	alice := createUser(t, env.admin, "alice")
	aliceID := bobID(t, alice)
	status, _, body := sendJSON(t, env.client, http.MethodPut, env.base+"/api/v1/users/"+aliceID+"/call-policy",
		map[string]any{"deny_prefixes": []string{"+1415"}}, alice.headers(true))
	if status != http.StatusForbidden {
		t.Fatalf("non-admin write = %d %s, want 403", status, body)
	}
	status, _, body = sendJSON(t, env.client, http.MethodPut, env.base+"/api/v1/users/"+aliceID+"/call-policy",
		map[string]any{"deny_prefixes": []string{"+1415"}}, env.admin.headers(false))
	if status != http.StatusForbidden {
		t.Fatalf("admin write without CSRF = %d %s, want 403", status, body)
	}
}
