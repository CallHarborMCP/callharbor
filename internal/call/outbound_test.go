package call

import (
	"errors"
	"testing"
)

func mustApply(t *testing.T, o *Outbound, s Signal) bool {
	t.Helper()
	changed, err := o.Apply(s)
	if err != nil {
		t.Fatalf("Apply(%+v): %v", s, err)
	}
	return changed
}

func TestHappyPathDialRingAnswerHangup(t *testing.T) {
	o := NewOutbound("c1")
	if err := o.BeginDial(); err != nil {
		t.Fatal(err)
	}
	if o.State != Initiating {
		t.Fatalf("state %s", o.State)
	}
	mustApply(t, o, Signal{EventID: "e1", Kind: KindRinging, ProviderCallControlID: "v3:a"})
	mustApply(t, o, Signal{EventID: "e2", Kind: KindAnswered, ProviderCallControlID: "v3:a"})
	if o.State != Active {
		t.Fatalf("state %s, want active", o.State)
	}
	mustApply(t, o, Signal{EventID: "e3", Kind: KindHangup, ProviderCallControlID: "v3:a", Reason: ReasonHangup})
	if o.State != Ended || o.Reason != ReasonHangup {
		t.Fatalf("state %s reason %s", o.State, o.Reason)
	}
}

func TestNoDoubleDial(t *testing.T) {
	o := NewOutbound("c1")
	if err := o.BeginDial(); err != nil {
		t.Fatal(err)
	}
	if err := o.BeginDial(); !errors.Is(err, ErrAlreadyDialed) {
		t.Fatalf("second dial: %v, want ErrAlreadyDialed", err)
	}
	mustApply(t, o, Signal{EventID: "e1", Kind: KindRinging, ProviderCallControlID: "v3:a"})
	if err := o.BeginDial(); !errors.Is(err, ErrAlreadyDialed) {
		t.Fatalf("dial while ringing: %v", err)
	}
	mustApply(t, o, Signal{EventID: "e2", Kind: KindHangup, Reason: ReasonNoAnswer})
	if err := o.BeginDial(); !errors.Is(err, ErrNotDialable) {
		t.Fatalf("dial after failure: %v, want ErrNotDialable", err)
	}
}

func TestReorderedAnsweredBeforeRingingStaysActive(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e2", Kind: KindAnswered, ProviderCallControlID: "v3:a"})
	if o.State != Active {
		t.Fatalf("state %s", o.State)
	}
	if changed := mustApply(t, o, Signal{EventID: "e1", Kind: KindRinging, ProviderCallControlID: "v3:a"}); changed {
		t.Fatal("late ringing moved the call backwards")
	}
	if o.State != Active {
		t.Fatalf("state %s after late ringing", o.State)
	}
}

func TestHangupBeforeAnswerIsFailedNotEnded(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e1", Kind: KindHangup, ProviderCallControlID: "v3:a", Reason: ReasonBusy})
	if o.State != Failed || o.Reason != ReasonBusy {
		t.Fatalf("state %s reason %s", o.State, o.Reason)
	}
}

func TestNoAnswerAndUnknownReasons(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e1", Kind: KindHangup, Reason: ""})
	if o.State != Failed || o.Reason != ReasonUnknown {
		t.Fatalf("state %s reason %s", o.State, o.Reason)
	}
}

func TestDuplicateEventIgnored(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	if !mustApply(t, o, Signal{EventID: "e1", Kind: KindAnswered, ProviderCallControlID: "v3:a"}) {
		t.Fatal("first answered should change state")
	}
	if mustApply(t, o, Signal{EventID: "e1", Kind: KindAnswered, ProviderCallControlID: "v3:a"}) {
		t.Fatal("redelivered answered changed state")
	}
}

func TestTerminalStateIsFinal(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e1", Kind: KindAnswered, ProviderCallControlID: "v3:a"})
	mustApply(t, o, Signal{EventID: "e2", Kind: KindHangup, Reason: ReasonHangup})
	if mustApply(t, o, Signal{EventID: "e3", Kind: KindAnswered}) {
		t.Fatal("answered resurrected an ended call")
	}
	if o.State != Ended {
		t.Fatalf("state %s", o.State)
	}
}

func TestForeignProviderIDRejectedWithoutPoisoningDedupe(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e1", Kind: KindRinging, ProviderCallControlID: "v3:a"})
	_, err := o.Apply(Signal{EventID: "e9", Kind: KindAnswered, ProviderCallControlID: "v3:OTHER"})
	if !errors.Is(err, ErrForeignProvider) {
		t.Fatalf("err %v, want ErrForeignProvider", err)
	}
	if o.State != Ringing {
		t.Fatalf("foreign signal changed state to %s", o.State)
	}
	if !mustApply(t, o, Signal{EventID: "e9", Kind: KindAnswered, ProviderCallControlID: "v3:a"}) {
		t.Fatal("valid redelivery of the event ID was suppressed by the rejected signal")
	}
}

func TestUnknownSignalKindRejectedWithoutSideEffects(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	if _, err := o.Apply(Signal{EventID: "e1", Kind: "bogus"}); err == nil {
		t.Fatal("accepted unknown kind")
	}
	if mustApply(t, o, Signal{EventID: "e1", Kind: KindAnswered}) != true {
		t.Fatal("rejected unknown kind consumed the event ID")
	}
}

func TestLocalHangupEndsWithLocalReason(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e1", Kind: KindAnswered, ProviderCallControlID: "v3:a"})
	if err := o.BeginHangup(); err != nil {
		t.Fatal(err)
	}
	if o.State != Ending || o.Reason != ReasonLocalHangup {
		t.Fatalf("state %s reason %s", o.State, o.Reason)
	}
	mustApply(t, o, Signal{EventID: "e2", Kind: KindHangup, Reason: ReasonHangup})
	if o.State != Ended {
		t.Fatalf("state %s after confirmed hangup", o.State)
	}
}

func TestHangupOnQueuedCallRejected(t *testing.T) {
	o := NewOutbound("c1")
	if err := o.BeginHangup(); !errors.Is(err, ErrNotDialable) {
		t.Fatalf("err %v", err)
	}
}

func TestHangupOnTerminalCallIsNoop(t *testing.T) {
	o := NewOutbound("c1")
	_ = o.BeginDial()
	mustApply(t, o, Signal{EventID: "e1", Kind: KindHangup, Reason: ReasonBusy})
	if err := o.BeginHangup(); err != nil {
		t.Fatalf("hangup on failed call: %v", err)
	}
	if o.State != Failed {
		t.Fatalf("state %s", o.State)
	}
}
