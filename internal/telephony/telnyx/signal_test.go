package telnyx

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
)

type webhookHarness struct {
	priv ed25519.PrivateKey
	v    *Verifier
	now  time.Time
}

func newWebhookHarness(t *testing.T) *webhookHarness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := &webhookHarness{priv: priv, now: time.Unix(1700000000, 0)}
	v, err := NewVerifier(pub, DefaultTolerance, func() time.Time { return h.now })
	if err != nil {
		t.Fatal(err)
	}
	h.v = v
	return h
}

func (h *webhookHarness) deliver(t *testing.T, name string, body []byte) (call.Signal, bool) {
	t.Helper()
	hdr := Sign(h.priv, h.now, body)
	if err := h.v.VerifyWebhook(hdr, body); err != nil {
		t.Fatalf("verify %s: %v", name, err)
	}
	ev, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return ev.Signal()
}

func loadWebhook(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignedWebhookDrivesOutboundLifecycle(t *testing.T) {
	h := newWebhookHarness(t)
	o := call.NewOutbound("call-7f3a")
	if err := o.BeginDial(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"callInitiated", "callAnswered"} {
		s, ok := h.deliver(t, name, loadWebhook(t, name))
		if !ok {
			t.Fatalf("%s not mapped", name)
		}
		if _, err := o.Apply(s); err != nil {
			t.Fatal(err)
		}
	}
	if o.State != call.Active {
		t.Fatalf("state %s, want active", o.State)
	}
	if o.ProviderCallControlID == "" {
		t.Fatal("provider call id not bound")
	}
}

func TestHangupWebhookMapsReason(t *testing.T) {
	h := newWebhookHarness(t)
	s, ok := h.deliver(t, "callHangup", loadWebhook(t, "callHangup"))
	if !ok || s.Kind != call.KindHangup || s.Reason != call.ReasonRejected {
		t.Fatalf("signal %+v ok=%v", s, ok)
	}
}

func TestRedeliveredSignedWebhookIsDeduplicated(t *testing.T) {
	h := newWebhookHarness(t)
	o := call.NewOutbound("call-7f3a")
	_ = o.BeginDial()
	body := loadWebhook(t, "callAnswered")
	first, _ := h.deliver(t, "first", body)
	h.now = h.now.Add(10 * time.Second)
	redelivered, _ := h.deliver(t, "redelivered", body)
	if changed, _ := o.Apply(first); !changed {
		t.Fatal("first delivery did not change state")
	}
	if changed, _ := o.Apply(redelivered); changed {
		t.Fatal("redelivered answered event changed state again")
	}
}

func TestUnverifiedWebhookNeverReachesStateMachine(t *testing.T) {
	h := newWebhookHarness(t)
	body := loadWebhook(t, "callHangup")
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0x01
	if err := h.v.VerifyWebhook(Sign(h.priv, h.now, body), tampered); err == nil {
		t.Fatal("tampered raw body verified")
	}
}

func TestUnknownWebhookIsNotAStateSignal(t *testing.T) {
	ev := WebhookEvent{ID: "x", Type: "call.speak.started", Known: false}
	if _, ok := ev.Signal(); ok {
		t.Fatal("unknown event mapped to a state signal")
	}
}

func TestHangupCauseMapsToOutcomeReason(t *testing.T) {
	cases := map[string]call.Reason{
		"user_busy":         call.ReasonBusy,
		"no_answer":         call.ReasonNoAnswer,
		"timeout":           call.ReasonNoAnswer,
		"call_rejected":     call.ReasonRejected,
		"originator_cancel": call.ReasonCanceled,
		"time_limit":        call.ReasonTimeLimit,
		"normal_clearing":   call.ReasonHangup,
		"not_found":         call.ReasonUnknown,
		"":                  call.ReasonUnknown,
	}
	for cause, want := range cases {
		if got := hangupReason(cause); got != want {
			t.Errorf("hangupReason(%q) = %q, want %q", cause, got, want)
		}
	}
}

func TestSignalCarriesEventIDForDedupe(t *testing.T) {
	ev := WebhookEvent{ID: "evt-1", Type: EventCallAnswered, Known: true, CallControlID: "v3:a"}
	s, ok := ev.Signal()
	if !ok || s.EventID != EventCallAnswered+"|evt-1" || s.ProviderCallControlID != "v3:a" || s.Kind != call.KindAnswered {
		t.Fatalf("signal %+v", s)
	}
}
