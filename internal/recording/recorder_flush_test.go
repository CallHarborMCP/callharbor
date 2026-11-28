package recording

import (
	"testing"
)

// A queued agent chunk longer than the flush lag must survive: the flush guard
// holds the timeline back until the chunk's play time has passed.
func TestRecorderLongQueuedAgentChunkSurvivesFlush(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	// 3 s of agent audio starting at 100 ms, queued before any caller frame.
	r.AgentPayload(at(100), l16(samplesMS(3000), 7000), at(0))
	// Caller frames keep the wall clock moving past the flush lag.
	for i := 0; i < 40; i++ {
		r.CallerPayload(rtpMS(i*100), l16(samplesMS(100), 1), at(i*100))
	}
	res := finish(t, r, at(4500), false)
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(100)] != 7000 || agent[samplesMS(3000)] != 7000 {
		t.Fatalf("long agent chunk lost at flush: start=%d mid=%d", agent[samplesMS(100)], agent[samplesMS(3000)])
	}
	if res.Status != StatusComplete {
		t.Fatalf("status = %s, want complete", res.Status)
	}
}

// Abort closes a recorder that never took media, and a second Abort or Finish
// reports the recorder as finished rather than writing again.
func TestRecorderAbortClosesWithoutWriting(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	if err := r.Abort(); err != nil {
		t.Fatal(err)
	}
	if len(sink.pcm()) != 0 || !sink.closed {
		t.Fatalf("abort wrote %d samples or left the sink open", len(sink.pcm()))
	}
	if _, err := r.Finish(at(100), false); err == nil {
		t.Fatal("finish after abort succeeded")
	}
}
