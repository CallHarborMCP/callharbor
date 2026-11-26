package voice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// speechOnlyBridge reports caller speech edges but cannot cut turn-scoped. It
// is the shape that local barge-in cannot serve.
type speechOnlyBridge struct {
	*fakeBridge
	speech chan SpeechEvent
}

func (b *speechOnlyBridge) CallerSpeech() <-chan SpeechEvent { return b.speech }

// Local barge-in without a TurnInterrupter must fail at setup, before any run
// starts, and the session must be closed. Previously the failure surfaced only
// at the first barge-in, mid-call.
func TestRunWithRejectsLocalBargeInWithoutTurnInterrupterAtSetup(t *testing.T) {
	sess := newFakeSession()
	br := &speechOnlyBridge{fakeBridge: newFakeBridge(), speech: make(chan SpeechEvent)}
	_, err := RunWith(context.Background(), sess, br, nil, Options{Caps: realtime.LiveCapabilities})
	if err == nil || !strings.Contains(err.Error(), "turn-scoped interrupt") {
		t.Fatalf("RunWith = %v, want a setup error naming the turn-scoped interrupt", err)
	}
	select {
	case <-sess.closed:
	case <-time.After(time.Second):
		t.Fatal("setup failure did not close the session")
	}
}

// A native speech-start protocol does not need a TurnInterrupter, so the same
// bridge is accepted at setup. The run then ends on the provider close.
func TestRunWithAcceptsNativeBargeInWithoutTurnInterrupter(t *testing.T) {
	sess := newFakeSession()
	br := &speechOnlyBridge{fakeBridge: newFakeBridge(), speech: make(chan SpeechEvent)}
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := RunWith(context.Background(), sess, br, nil, Options{Caps: realtime.RealtimeCapabilities})
		done <- outcome{res, err}
	}()
	// Setup accepted the bridge only if the run is still live, so it does not
	// return the setup error. The provider close ends it.
	_ = sess.Close()
	select {
	case o := <-done:
		if o.err != nil && strings.Contains(o.err.Error(), "turn-scoped interrupt") {
			t.Fatalf("native speech start rejected at setup: %v", o.err)
		}
		if o.res.Closed {
			t.Fatal("a session closed without a provider close event reported a clean close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not end after the session closed")
	}
}
