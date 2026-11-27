package voice

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// ackSession is a fake session that also acknowledges task reports. It records
// every ack and every audio append, so a test can see all session writes the
// engine made.
type ackSession struct {
	*fakeSession
	mu       sync.Mutex
	acks     []string
	statuses []string
	ackErr   error
}

func newAckSession(ackErr error) *ackSession {
	return &ackSession{fakeSession: newFakeSession(), ackErr: ackErr}
}

func (s *ackSession) AckTaskReport(_ context.Context, callID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acks = append(s.acks, callID)
	s.statuses = append(s.statuses, status)
	return s.ackErr
}

func (s *ackSession) ackedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acks...)
}

func (s *ackSession) ackedStatuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statuses...)
}

const validReportArgs = `{"outcome":"failed","reason":"callee_refused","criteria":[],"constraints":[]}`

// A usable report is passed to the sink and acknowledged once. The engine
// writes nothing else to the session: the ack is the only session write.
func TestEngineAcksValidReportOnce(t *testing.T) {
	sess := newAckSession(nil)
	br := newFakeBridge()
	sess.events <- realtime.Event{Kind: realtime.EventTaskReport, FunctionCallID: "call_1", Arguments: validReportArgs}
	sess.events <- realtime.Event{Kind: realtime.EventClosed, CloseReason: "done"}

	var kinds []realtime.EventKind
	res, err := Run(context.Background(), sess, br, func(ev realtime.Event) { kinds = append(kinds, ev.Kind) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Closed {
		t.Fatal("Closed = false, want the documented close after the report")
	}
	if got := sess.ackedIDs(); len(got) != 1 || got[0] != "call_1" {
		t.Fatalf("acks = %v, want exactly [call_1]", got)
	}
	if len(kinds) != 2 || kinds[0] != realtime.EventTaskReport {
		t.Fatalf("sink kinds = %v, want the report then the close", kinds)
	}
	sess.appendMu.Lock()
	defer sess.appendMu.Unlock()
	if len(sess.appended) != 0 {
		t.Fatalf("session received %d audio appends, want none", len(sess.appended))
	}
}

// An invalid report that has a usable call id is acknowledged with the invalid
// status, so the model's function call is never left without output. A report
// with no usable call id has nothing to answer and gets no ack.
func TestEngineAcksInvalidReportWithCallIDAsInvalid(t *testing.T) {
	sess := newAckSession(nil)
	br := newFakeBridge()
	sess.events <- realtime.Event{Kind: realtime.EventTaskReport, FunctionCallID: "call_bad", ReportInvalid: true}
	sess.events <- realtime.Event{Kind: realtime.EventTaskReport, ReportInvalid: true}
	sess.events <- realtime.Event{Kind: realtime.EventClosed, CloseReason: "done"}

	res, err := Run(context.Background(), sess, br, nil)
	if err != nil || !res.Closed {
		t.Fatalf("Run = %+v, %v; an invalid report must not end the call", res, err)
	}
	if got := sess.ackedIDs(); len(got) != 1 || got[0] != "call_bad" {
		t.Fatalf("acks = %v, want exactly [call_bad]", got)
	}
	if got := sess.ackedStatuses(); len(got) != 1 || got[0] != TaskAckInvalid {
		t.Fatalf("statuses = %v, want [%s]", got, TaskAckInvalid)
	}
}

// A failed ack is not a call failure. The report was already passed to the sink,
// and the run still ends on the provider's close.
func TestEngineAckErrorDoesNotEndRun(t *testing.T) {
	sess := newAckSession(errors.New("socket write failed"))
	br := newFakeBridge()
	sess.events <- realtime.Event{Kind: realtime.EventTaskReport, FunctionCallID: "call_9", Arguments: validReportArgs}
	sess.events <- realtime.Event{Kind: realtime.EventClosed, CloseReason: "done"}

	var sawReport bool
	res, err := Run(context.Background(), sess, br, func(ev realtime.Event) {
		if ev.Kind == realtime.EventTaskReport {
			sawReport = true
		}
	})
	if err != nil {
		t.Fatalf("ack error ended the run: %v", err)
	}
	if !res.Closed || !sawReport {
		t.Fatalf("closed=%v sawReport=%v, want the run to finish after the report", res.Closed, sawReport)
	}
	if got := sess.ackedIDs(); len(got) != 1 {
		t.Fatalf("ack was not attempted: %v", got)
	}
}
