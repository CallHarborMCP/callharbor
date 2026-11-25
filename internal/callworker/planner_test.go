package callworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

type fixedProfiles struct {
	p   telephonyprofile.Resolved
	err error
}

func (f fixedProfiles) Resolve(context.Context, telephonyprofile.Actor, string) (telephonyprofile.Resolved, error) {
	return f.p, f.err
}

func (f fixedProfiles) ResolveForHangup(context.Context, string, string) (telephonyprofile.Resolved, error) {
	return f.p, f.err
}

func readyResolved() telephonyprofile.Resolved {
	return telephonyprofile.Resolved{
		ID: "tp", OwnerUserID: "alice", CallerNumber: "+14155550100", ConnectionID: "conn-1",
		APIKey: "KEY-SECRET", WebhookPublicKey: "pk", ConnectivityStatus: storage.ConnectivityVerified,
	}
}

func fakeDialerFactory(t *testing.T, seen *string) DialerFactory {
	return func(apiKey string) (Dialer, error) {
		*seen = apiKey
		return &fakeDialer{}, nil
	}
}

func planner(t *testing.T, res telephonyprofile.Resolved, routes PublicRoutes) ProductionPlanner {
	t.Helper()
	var key string
	return ProductionPlanner{
		Profiles: fixedProfiles{p: res}, Dialers: fakeDialerFactory(t, &key), Routes: routes,
	}
}

func okRoutes(public string) PublicRoutes {
	return PublicRoutes{PublicURL: public, StreamToken: func(string) (string, error) { return "tok123", nil }}
}

func TestPlanBuildsPerCallURLsFromPublicOrigin(t *testing.T) {
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com/"))
	plan, reason, err := p.Plan(context.Background(), call("call_1"))
	if err != nil || reason != "" {
		t.Fatalf("plan: reason=%q err=%v", reason, err)
	}
	if plan.WebhookURL != "https://ch.example.com/telnyx/calls/call_1/webhook" {
		t.Fatalf("webhook = %s", plan.WebhookURL)
	}
	if plan.StreamURL != "wss://ch.example.com/telnyx/calls/call_1/media/tok123" {
		t.Fatalf("stream = %s", plan.StreamURL)
	}
	if plan.From != "+14155550100" || plan.ConnectionID != "conn-1" {
		t.Fatalf("plan = %+v", plan)
	}
}

// The fake dialers in this package do not validate dial parameters. This test
// checks that a planned dial satisfies the real Telnyx request builder, which
// rejects an unset stream codec. Without the codec, every real dial fails.
func TestPlannedDialPassesTelnyxRequestValidation(t *testing.T) {
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.Codec = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	plan, reason, err := p.Plan(context.Background(), call("call_1"))
	if err != nil || reason != "" {
		t.Fatalf("plan = %q, %v", reason, err)
	}
	_, err = telnyx.BuildDialRequest(telnyx.DialParams{
		ConnectionID: plan.ConnectionID, From: plan.From, To: "+14155550111",
		WebhookURL: plan.WebhookURL, StreamURL: plan.StreamURL, Codec: plan.Codec,
		CommandID: telnyx.CommandID("call_1"), TimeLimit: plan.MaxSeconds,
	})
	if err != nil {
		t.Fatalf("planned dial rejected by telnyx: %v", err)
	}
}

func TestPlanUsesWSForHTTPOriginAndRejectsOtherSchemes(t *testing.T) {
	p := planner(t, readyResolved(), okRoutes("http://localhost:8080"))
	plan, _, _ := p.Plan(context.Background(), call("call_1"))
	if !strings.HasPrefix(plan.StreamURL, "ws://localhost:8080/") {
		t.Fatalf("stream = %s", plan.StreamURL)
	}
	bad := planner(t, readyResolved(), okRoutes("ftp://example.com"))
	if _, reason, _ := bad.Plan(context.Background(), call("call_1")); reason != ReasonURLsUnavailable {
		t.Fatalf("ftp origin reason = %q", reason)
	}
}

func TestPlanRefusesUnusableProfiles(t *testing.T) {
	cases := map[string]func(*telephonyprofile.Resolved){
		"unverified": func(r *telephonyprofile.Resolved) { r.ConnectivityStatus = storage.ConnectivityUnknown },
		"no conn":    func(r *telephonyprofile.Resolved) { r.ConnectionID = "" },
		"no key":     func(r *telephonyprofile.Resolved) { r.WebhookPublicKey = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := readyResolved()
			mutate(&r)
			p := planner(t, r, okRoutes("https://ch.example.com"))
			if _, reason, _ := p.Plan(context.Background(), call("c")); reason != ReasonProfileUnusable {
				t.Fatalf("reason = %q, want %s", reason, ReasonProfileUnusable)
			}
		})
	}
}

func TestPlanRevokedProfileFailsClosed(t *testing.T) {
	p := ProductionPlanner{
		Profiles: fixedProfiles{err: telephonyprofile.ErrNotFound},
		Dialers:  fakeDialerFactory(t, new(string)), Routes: okRoutes("https://ch.example.com"),
	}
	if _, reason, _ := p.Plan(context.Background(), call("c")); reason != ReasonProfileUnusable {
		t.Fatalf("revoked reason = %q", reason)
	}
}

func TestPlanMissingPublicURLFailsClosed(t *testing.T) {
	p := planner(t, readyResolved(), PublicRoutes{})
	if _, reason, _ := p.Plan(context.Background(), call("c")); reason != ReasonURLsUnavailable {
		t.Fatalf("reason = %q", reason)
	}
}

func TestPlanStoreErrorIsReturnedNotSwallowed(t *testing.T) {
	boom := errors.New("db down")
	p := ProductionPlanner{Profiles: fixedProfiles{err: boom}, Dialers: fakeDialerFactory(t, new(string)),
		Routes: okRoutes("https://ch.example.com")}
	if _, _, err := p.Plan(context.Background(), call("c")); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanDoesNotLeakKeyInReasonOrError(t *testing.T) {
	r := readyResolved()
	r.ConnectivityStatus = storage.ConnectivityFailed
	p := planner(t, r, okRoutes("https://ch.example.com"))
	_, reason, err := p.Plan(context.Background(), call("c"))
	if strings.Contains(reason, "KEY-SECRET") || (err != nil && strings.Contains(err.Error(), "KEY-SECRET")) {
		t.Fatal("api key appeared in plan output")
	}
}

// telnyxTimeLimit keeps a call's cap inside Telnyx's accepted 30..14400 range
// and leaves an in-range cap unchanged.
func TestTelnyxTimeLimitClampsToAcceptedRange(t *testing.T) {
	cases := map[int]int{1: 30, 29: 30, 30: 30, 600: 600, 14400: 14400, 14401: 14400, 7200: 7200}
	for in, want := range cases {
		if got := telnyxTimeLimit(in); got != want {
			t.Errorf("telnyxTimeLimit(%d) = %d, want %d", in, got, want)
		}
		_, err := telnyx.BuildDialRequest(telnyx.DialParams{
			ConnectionID: "conn", From: "+14155550100", To: "+14155550111",
			WebhookURL: "https://pub.example/hook", StreamURL: "wss://pub.example/stream",
			Codec: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}, CommandID: telnyx.CommandID("call_clamp"),
			TimeLimit: telnyxTimeLimit(in),
		})
		if err != nil {
			t.Errorf("clamped %d rejected by Telnyx: %v", in, err)
		}
	}
}
