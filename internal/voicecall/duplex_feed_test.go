package voicecall_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// dxFeedPace is the real-time cadence of inbound audio: one 20 ms frame.
const dxFeedPace = 20 * time.Millisecond

// dxFeedWarm is how many frames a stop test writes before the stop, so the stop
// arrives while inbound audio is already flowing.
const dxFeedWarm = 25

// dxFeedWrite bounds one feeder write, so a stalled stream cannot hang stop.
const dxFeedWrite = time.Second

// dxFeedSeqBase keeps feeder sequence numbers clear of the scripted frames,
// which count up from 1.
const dxFeedSeqBase = 1_000_000

// dxFeeder streams inbound frames on one Telnyx stream, as Telnyx does for the
// whole call. The websocket connection documents every method except Read as
// safe for concurrent use, so the feeder runs beside the test's own sends.
//
// Back-to-back feeding (pace zero) is bounded to one frame in flight: a single
// goroutine writes synchronously, and each Send returns only once the socket
// has accepted the frame, so the feeder never queues frames of its own. How
// many frames the server has read but not yet processed is not observable from
// the test, so no numeric bound on the server's queue is claimed here.
type dxFeeder struct {
	ctx     context.Context
	s       *telnyxfake.Stream
	payload []byte
	seq     atomic.Int64
	mu      sync.Mutex
	cond    *sync.Cond
	sent    int
	exited  bool
	stopCh  chan struct{}
	once    sync.Once
	done    chan struct{}
}

// startDxFeeder streams frames carrying payload until stop is called, ctx ends,
// or a write fails (for example when the stream closes). pace is the gap
// between frames; zero sends back to back, so frames queue on the uplink.
func startDxFeeder(t *testing.T, ctx context.Context, s *telnyxfake.Stream, pace time.Duration, payload []byte) *dxFeeder {
	t.Helper()
	f := &dxFeeder{ctx: ctx, s: s, payload: payload, stopCh: make(chan struct{}), done: make(chan struct{})}
	f.seq.Store(dxFeedSeqBase)
	f.cond = sync.NewCond(&f.mu)
	go f.run(pace)
	return f
}

func (f *dxFeeder) run(pace time.Duration) {
	defer func() {
		f.mu.Lock()
		f.exited = true
		f.cond.Broadcast()
		f.mu.Unlock()
		close(f.done)
	}()
	var tick <-chan time.Time
	if pace > 0 {
		t := time.NewTicker(pace)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-f.stopCh:
			return
		case <-f.ctx.Done():
			return
		default:
		}
		if tick != nil {
			select {
			case <-f.stopCh:
				return
			case <-f.ctx.Done():
				return
			case <-tick:
			}
		}
		wctx, cancel := context.WithTimeout(f.ctx, dxFeedWrite)
		err := f.s.Send(wctx, dxInboundFrame(f.payload, int(f.seq.Add(1))))
		cancel()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.sent++
		f.cond.Broadcast()
		f.mu.Unlock()
	}
}

// awaitSent blocks until n frames have been written or the feeder has exited,
// and reports whether n frames were written. It waits on the feeder's progress,
// not on wall time.
func (f *dxFeeder) awaitSent(n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.sent < n && !f.exited {
		f.cond.Wait()
	}
	return f.sent >= n
}

// stop ends the feed and waits until its goroutine has returned, so no feeder
// write can follow the call. It is idempotent.
func (f *dxFeeder) stop() {
	f.once.Do(func() { close(f.stopCh) })
	<-f.done
}

// sendStop writes the Telnyx stop frame for dx-1 with the next feeder sequence
// number.
func (f *dxFeeder) sendStop() error {
	return f.s.Send(f.ctx, dxStopFrame(int(f.seq.Add(1))))
}

// dxInboundFrame builds the same caller media frame as dxInbound. It has no
// *testing.T, so the feeder goroutine can call it. The map cannot fail to
// marshal.
func dxInboundFrame(payload []byte, seq int) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "media", "stream_id": "dx-1", "sequence_number": fmt.Sprint(seq),
		"media": map[string]any{"track": "inbound", "chunk": fmt.Sprint(seq), "timestamp": fmt.Sprint(seq * 20), "payload": base64.StdEncoding.EncodeToString(payload)},
	})
	return b
}

// dxStopWithInboundAudio sends the Telnyx stop frame while the caller is still
// speaking. The stop tests stream caller-speech tone, not silence, and send it
// back to back so frames queue on the uplink at the stop. Tone keeps the
// barge-in turn open through the stop, which is the scenario under test: a stop
// mid-barge-in. Silence would end that turn before the stop, reopen the
// anonymous gate, and let the pinned stale cut-reply audio reach the stream
// before the stop. The silence tests use silence because they need the caller
// to stop speaking.
func dxStopWithInboundAudio(t *testing.T, s *telnyxfake.Stream, ctx context.Context) {
	t.Helper()
	feed := startDxFeeder(t, ctx, s, 0, dxTone())
	if !feed.awaitSent(dxFeedWarm) {
		feed.stop()
		t.Fatal("inbound tone feed ended before the stop")
	}
	err := feed.sendStop()
	feed.stop()
	if err != nil {
		t.Fatal(err)
	}
}
