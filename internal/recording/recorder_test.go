package recording

import (
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// memSink collects written bytes. It can be told to fail after n writes.
type memSink struct {
	mu        sync.Mutex
	data      []byte
	writes    int
	failAfter int // fail writes once this many succeeded; 0 means never fail
	closed    bool
}

func (m *memSink) Write(p []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAfter > 0 && m.writes >= m.failAfter {
		return errors.New("disk gone")
	}
	m.writes++
	m.data = append(m.data, p...)
	return nil
}
func (m *memSink) Sync() error { return nil }
func (m *memSink) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// pcm decodes the sink's bytes into interleaved stereo samples.
func (m *memSink) pcm() []int16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int16, len(m.data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(m.data[2*i:])) // #nosec G115 -- round trip
	}
	return out
}

// channel extracts one channel of the interleaved output.
func channel(s []int16, c int) []int16 {
	out := make([]int16, 0, len(s)/2)
	for i := c; i < len(s); i += 2 {
		out = append(out, s[i])
	}
	return out
}

const testRate = 8000

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

var leg = audio.Format{Encoding: audio.L16, SampleRate: testRate, LittleEndian: true}

// l16 encodes n samples of value v as little-endian L16 payload bytes.
func l16(n int, v int16) []byte {
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v)) // #nosec G115 -- test encoding
	}
	return b
}

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// samplesMS is the sample count for ms milliseconds at testRate.
func samplesMS(ms int) int { return ms * testRate / 1000 }

// rtpMS is the RTP timestamp offset for ms milliseconds.
func rtpMS(ms int) uint32 { return uint32(samplesMS(ms)) }

func newRec(t *testing.T, sink Sink) *Recorder {
	t.Helper()
	r, err := New(Config{Leg: leg, Start: t0}, sink)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func finish(t *testing.T, r *Recorder, at time.Time, unconfirmed bool) Result {
	t.Helper()
	res, err := r.Finish(at, unconfirmed)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return res
}

func hasFlag(res Result, flag string) bool { return slices.Contains(res.QualityFlags, flag) }

func TestRecorderFramesLandAtWallClockPositions(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	// Two 100 ms frames, arriving on time: caller samples 0..800 and 800..1600.
	r.CallerPayload(0, l16(samplesMS(100), 1000), at(0))
	r.CallerPayload(rtpMS(100), l16(samplesMS(100), 2000), at(100))
	res := finish(t, r, at(3000), false)
	caller := channel(sink.pcm(), 0)
	if caller[0] != 1000 || caller[samplesMS(100)] != 2000 {
		t.Fatalf("caller frames misplaced: %d %d", caller[0], caller[samplesMS(100)])
	}
	if hasFlag(res, FlagCallerGapFilled) || hasFlag(res, FlagCallerFramesDropped) {
		t.Fatalf("clean stream flagged: %v", res.QualityFlags)
	}
	if res.Status != StatusComplete {
		t.Fatalf("status = %s, want complete", res.Status)
	}
}

func TestRecorderFillsRTPGapWithSilence(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.CallerPayload(0, l16(samplesMS(20), 1000), at(0))
	// Next frame is 100 ms later on the RTP clock: 80 ms missing.
	r.CallerPayload(rtpMS(100), l16(samplesMS(20), 2000), at(100))
	res := finish(t, r, at(2000), false)
	caller := channel(sink.pcm(), 0)
	gapStart := samplesMS(20)
	for i := gapStart; i < samplesMS(100); i++ {
		if caller[i] != 0 {
			t.Fatalf("gap sample %d = %d, want silence", i, caller[i])
		}
	}
	if caller[samplesMS(100)] != 2000 {
		t.Fatal("frame after gap misplaced")
	}
	if !hasFlag(res, FlagCallerGapFilled) {
		t.Fatalf("gap not flagged: %v", res.QualityFlags)
	}
}

func TestRecorderRTPWraparoundKeepsPosition(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	start := uint32(0xFFFFFFFF - samplesMS(20) + 1) // 20 ms before wrap
	r.CallerPayload(start, l16(samplesMS(20), 1000), at(0))
	r.CallerPayload(start+rtpMS(20), l16(samplesMS(20), 2000), at(20))
	finish(t, r, at(2000), false)
	caller := channel(sink.pcm(), 0)
	if caller[samplesMS(20)] != 2000 {
		t.Fatalf("frame across wraparound misplaced: %d", caller[samplesMS(20)])
	}
}

func TestRecorderDropsStaleAndDuplicateFrames(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.CallerPayload(0, l16(samplesMS(20), 1000), at(0))
	r.CallerPayload(rtpMS(20), l16(samplesMS(20), 2000), at(20))
	r.CallerPayload(rtpMS(20), l16(samplesMS(20), 9999), at(21)) // duplicate
	r.CallerPayload(0, l16(samplesMS(20), 9999), at(22))         // stale
	res := finish(t, r, at(2000), false)
	caller := channel(sink.pcm(), 0)
	for _, v := range caller {
		if v == 9999 {
			t.Fatal("stale or duplicate frame reached the recording")
		}
	}
	if !hasFlag(res, FlagCallerFramesDropped) {
		t.Fatalf("dropped frames not flagged: %v", res.QualityFlags)
	}
}

// A media clock that stalls for longer than the window re-anchors forward once
// the lag has persisted for reanchorWindow. Frames arriving during the lag are
// behind the flushed point and are dropped; frames after the re-anchor land at
// their wall-clock positions.
func TestRecorderReanchorsForwardOnSustainedLag(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.CallerPayload(0, l16(samplesMS(20), 1000), at(0))
	r.CallerPayload(rtpMS(20), l16(samplesMS(20), 1100), at(20))
	// The media clock stalls for 1.5 s: from frame 2 on, every frame arrives
	// 1.5 s late, and then the stream keeps its 20 ms cadence.
	const stall = 1500
	for j := 2; j <= 80; j++ {
		r.CallerPayload(rtpMS(20*j), l16(samplesMS(20), int16(j)), at(stall+20*(j-2)))
	}
	res := finish(t, r, at(4000), false)
	caller := channel(sink.pcm(), 0)
	// Frame 60 arrives at wall 2660 ms and is 20 ms long, so once the track has
	// re-anchored to wall-clock it occupies 2640..2660 ms.
	if got := caller[samplesMS(2640)]; got != 60 {
		t.Fatalf("frame 60 at 2640 ms = %d, want 60 after re-anchor", got)
	}
	if !hasFlag(res, FlagCallerGapFilled) {
		t.Fatal("re-anchor not flagged as a gap")
	}
	if !hasFlag(res, FlagCallerFramesDropped) {
		t.Fatal("frames behind the flushed point during the stall were not flagged")
	}
}

// One late frame is not a sustained lag: the frames after it keep their RTP
// positions, and the track is not shifted.
func TestRecorderLateFrameDoesNotShiftLaterAudio(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	for j := 0; j <= 20; j++ {
		if j == 5 {
			continue
		}
		r.CallerPayload(rtpMS(20*j), l16(samplesMS(20), int16(j+1)), at(20*j))
		if j == 6 {
			// Frame 5 is delivered 700 ms late, after frame 6.
			r.CallerPayload(rtpMS(100), l16(samplesMS(20), 99), at(700))
		}
	}
	res := finish(t, r, at(3000), false)
	caller := channel(sink.pcm(), 0)
	for j := 7; j <= 20; j++ {
		if got := caller[samplesMS(20*j)]; got != int16(j+1) {
			t.Fatalf("frame %d at %d ms = %d, want %d: a late frame shifted later audio", j, 20*j, got, j+1)
		}
	}
	if !hasFlag(res, FlagCallerFramesDropped) {
		t.Fatal("the late frame was not dropped and flagged")
	}
}

// A burst of late frames is not a sustained lag. Frames 5 to 34 all arrive at
// 700 ms with continuous RTP, so each is late but the burst lasts under the
// window only because its frames all arrive together. The later on-time frames
// must keep their RTP positions. Under a forward-on-first-lag rule, frame 5's
// lag would shift the anchor by about 580 ms at once and frame 40 would land
// late, so this test fails on that rule and passes on the windowed one.
func TestRecorderLateBurstDoesNotShiftLaterAudio(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	// Frames 0 to 4 arrive on time.
	for j := 0; j < 5; j++ {
		r.CallerPayload(rtpMS(20*j), l16(samplesMS(20), int16(j+1)), at(20*j))
	}
	// Frames 5 to 34 all arrive at 700 ms, in order, with continuous RTP.
	for j := 5; j <= 34; j++ {
		r.CallerPayload(rtpMS(20*j), l16(samplesMS(20), int16(j+1)), at(700))
	}
	// Frames 35 onward arrive on time.
	for j := 35; j <= 100; j++ {
		r.CallerPayload(rtpMS(20*j), l16(samplesMS(20), int16(j+1)), at(20*j))
	}
	res := finish(t, r, at(4000), false)
	caller := channel(sink.pcm(), 0)
	for _, j := range []int{40, 80, 100} {
		if got := caller[samplesMS(20*j)]; got != int16(j+1) {
			t.Fatalf("frame %d at RTP %d ms = %d, want %d: the late burst shifted later audio", j, 20*j, got, j+1)
		}
	}
	// The burst is ahead of the flush point, so nothing is dropped: the frames
	// are placed at their RTP positions and no drop is flagged.
	if hasFlag(res, FlagCallerFramesDropped) {
		t.Fatalf("burst frames ahead of the flush point were dropped: %v", res.QualityFlags)
	}
}

func TestRecorderQueuedAgentAudioPlaysAtPlayStart(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	// Two chunks queued back to back: the second starts when the first ends.
	r.AgentPayload(at(500), l16(samplesMS(100), 3000), at(0))
	r.AgentPayload(at(600), l16(samplesMS(100), 4000), at(0))
	finish(t, r, at(2000), false)
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(500)] != 3000 || agent[samplesMS(600)] != 4000 {
		t.Fatalf("agent audio not at play start: %d %d", agent[samplesMS(500)], agent[samplesMS(600)])
	}
	if agent[samplesMS(499)] != 0 {
		t.Fatal("agent audio before its play start")
	}
}

func TestRecorderNeverRecordsUnplayedAgentAudioAfterCut(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	// 1 s of agent audio queued from 500 ms. The cut at 800 ms keeps only the
	// first 300 ms; the remaining queued audio must not be recorded.
	r.AgentPayload(at(500), l16(samplesMS(1000), 3000), at(0))
	r.Cut(at(800))
	res := finish(t, r, at(3000), false)
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(799)] != 3000 {
		t.Fatal("audio played before the cut was lost")
	}
	if agent[samplesMS(800)] != 0 {
		t.Fatalf("audio after the cut was recorded: %d", agent[samplesMS(800)])
	}
	if !hasFlag(res, FlagAgentInterrupted) {
		t.Fatalf("interruption not flagged: %v", res.QualityFlags)
	}
}

func TestRecorderCutTruncatesStraddlingChunk(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.AgentPayload(at(0), l16(samplesMS(400), 5000), at(0))
	r.Cut(at(150))
	finish(t, r, at(2000), false)
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(149)] != 5000 || agent[samplesMS(150)] != 0 {
		t.Fatalf("straddling chunk not cut at the cut point: %d %d", agent[samplesMS(149)], agent[samplesMS(150)])
	}
}

func TestRecorderStreamEndTruncatesPendingAgentAudio(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	// Audio scheduled past the stream end is dropped; audio before it is kept.
	r.AgentPayload(at(0), l16(samplesMS(300), 6000), at(0))
	r.AgentPayload(at(1000), l16(samplesMS(300), 7000), at(0))
	finish(t, r, at(500), false)
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(299)] != 6000 {
		t.Fatal("audio before stream end lost")
	}
	for _, v := range agent {
		if v == 7000 {
			t.Fatal("agent audio scheduled after stream end was recorded")
		}
	}
}

func TestRecorderOverlappingAgentAudioDoesNotDoubleWrite(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.AgentPayload(at(0), l16(samplesMS(200), 1000), at(0))
	// Second chunk claims an earlier play start than the first ends; it is
	// placed by the queue at the queue end, so the first sample stays intact.
	r.AgentPayload(at(100), l16(samplesMS(200), 2000), at(0))
	finish(t, r, at(2000), false)
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(199)] != 1000 {
		t.Fatalf("overlap overwrote played audio: %d", agent[samplesMS(199)])
	}
}

// Agent audio that overlaps audio already written to the agent channel is
// rejected by the timeline and must set agent_overlap_dropped. The first chunk
// is committed before the second is written: its end is due at the second
// chunk's commit time, so the timeline has it when the overlap is tested.
func TestRecorderOverlapSetsOverlapDroppedFlag(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.AgentPayload(at(0), l16(samplesMS(200), 1000), at(0))
	// Wall time advances past the first chunk's end, so it is committed.
	r.CallerPayload(rtpMS(0), l16(samplesMS(20), 1), at(300))
	// Second chunk starts inside the first chunk's written span.
	r.AgentPayload(at(100), l16(samplesMS(200), 2000), at(300))
	res := finish(t, r, at(2000), false)
	if !hasFlag(res, FlagAgentOverlapDropped) {
		t.Fatalf("overlapping agent audio did not set %s: flags = %v", FlagAgentOverlapDropped, res.QualityFlags)
	}
	agent := channel(sink.pcm(), 1)
	if agent[samplesMS(199)] != 1000 {
		t.Fatalf("overlap overwrote played audio: %d", agent[samplesMS(199)])
	}
}

func TestRecorderBackpressureMakesPartialAndKeepsCall(t *testing.T) {
	// A stalled writer: the first sink write blocks until released, so the
	// bounded channel fills and the recorder stops writing rather than block.
	block := make(chan struct{})
	sink := &blockingSink{gate: block}
	r := newRec(t, sink)
	for i := 0; i < writerQueue+10; i++ {
		r.CallerPayload(uint32(i*samplesMS(1000)), l16(samplesMS(1000), 1), at(i*1000))
	}
	close(block)
	res := finish(t, r, at(writerQueue*1000+5000), false)
	if res.Status != StatusPartial || !hasFlag(res, FlagBackpressure) {
		t.Fatalf("result = %+v, want partial with backpressure", res)
	}
}

type blockingSink struct {
	gate chan struct{}
}

func (b *blockingSink) Write(p []byte) error { <-b.gate; return nil }
func (b *blockingSink) Sync() error          { return nil }
func (b *blockingSink) Close() error         { return nil }

func TestRecorderWriteFailureMakesPartialAndKeepsCall(t *testing.T) {
	sink := &memSink{failAfter: 1}
	r := newRec(t, sink)
	r.CallerPayload(0, l16(samplesMS(1000), 1), at(0))
	r.CallerPayload(uint32(samplesMS(1000)), l16(samplesMS(1000), 2), at(1000))
	r.CallerPayload(uint32(samplesMS(2000)), l16(samplesMS(1000), 3), at(2000))
	// Taps keep working after the failure: the call must not error out.
	r.Cut(at(2500))
	res, err := r.Finish(at(3000), false)
	// memSink cannot reseal, so nothing is authenticated: the recording is
	// failed with no bytes, never partial with bytes the file cannot serve.
	if res.Status != StatusFailed || res.ByteCount != 0 || !hasFlag(res, FlagWriteFailed) {
		t.Fatalf("result = %+v, err=%v, want failed with no bytes and write_failed", res, err)
	}
}

func TestRecorderUnconfirmedPlayoutFlag(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	r.AgentPayload(at(0), l16(samplesMS(100), 1), at(0))
	res := finish(t, r, at(1000), true)
	if !hasFlag(res, FlagAgentPlayoutUnconfirmed) || !hasFlag(res, FlagAgentPlayoutEstimated) {
		t.Fatalf("flags = %v", res.QualityFlags)
	}
}

func TestRecorderFinishTwiceFails(t *testing.T) {
	r := newRec(t, &memSink{})
	finish(t, r, at(100), false)
	if _, err := r.Finish(at(200), false); !errors.Is(err, ErrRecorderClosed) {
		t.Fatalf("second finish: %v", err)
	}
}

func TestRecorderConcurrentTapsUnderRace(t *testing.T) {
	sink := &memSink{}
	r := newRec(t, sink)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.CallerPayload(uint32(i*160), l16(160, 10), at(i*20))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.AgentPayload(at(i*20+100), l16(160, 20), at(i*20))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			r.Cut(at(i * 80))
		}
	}()
	wg.Wait()
	res := finish(t, r, at(6000), false)
	if res.ByteCount%4 != 0 {
		t.Fatalf("byte count %d is not whole frames", res.ByteCount)
	}
}
