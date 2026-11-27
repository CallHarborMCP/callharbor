package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// recControlID is the Telnyx call control id every recording test uses.
const recControlID = "v3:ctl-rec"

// Energy thresholds for the block analysis. A loud block is the 440 Hz tone
// (RMS about 8500). A silent block is digital silence or the quiet frames,
// which decode to zero.
const (
	recLoudRMS   = 1000.0
	recSilentRMS = 50.0
)

// recPlayoutWait is how long the queued agent audio plays before the barge-in.
// The recording keeps only the agent audio that played before the cut, so the
// expected agent audio is about this many 20 ms blocks.
const recPlayoutWait = 300 * time.Millisecond

// recAgentQueued is the number of agent chunks the script queues, one 20 ms
// block each, so the queued agent audio is 600 ms.
const recAgentQueued = 30

// recAgentTiming holds the times of the agent script. They come from time.Now
// in this process, the clock the server also reads, so they map onto the
// recording through its start time.
type recAgentTiming struct {
	firstSend time.Time // before the first of the queued agent chunks
	// firstMedia is when the first outbound media frame of the queued chunks
	// reached the stream. It is the earliest the audio could have started
	// playing, so the bounds are taken from it rather than from firstSend.
	firstMedia time.Time
	queued     time.Time // after the server has sent every chunk
	trigger    time.Time // immediately before the barge-in is sent
	wire       time.Time // when the cut's clear frame arrived at the stream
}

// recSetPreference sets the signed-in user's recording preference through the
// session API, with CSRF, and checks the status.
func recSetPreference(t *testing.T, env *callTestEnv, s *session, enabled, acknowledged bool) {
	t.Helper()
	status, _, body := sendJSON(t, env.client, http.MethodPut, env.base+"/api/preferences/recording",
		map[string]any{"enabled": enabled, "acknowledged": acknowledged}, s.headers(true))
	if status != http.StatusOK {
		t.Fatalf("set recording preference = %d %s", status, body)
	}
}

// recCreateCall places a call through REST with the extra body fields and
// returns its id.
func recCreateCall(t *testing.T, env *callTestEnv, tpID, key string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"profile_id": tpID, "to": "+14155550111", "objective": "Book a table"}
	for k, v := range extra {
		body[k] = v
	}
	status, _, raw := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", body,
		withIdempotency(env.admin.headers(true), key))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, raw)
	}
	id, _ := decodeObject(t, raw)["call_id"].(string)
	if id == "" {
		t.Fatalf("no call_id: %s", raw)
	}
	return id
}

// recStreamDrain reads CallHarbor's outbound frames in the background and
// counts the media frames, so the agent path never blocks on an unread stream.
// It sends the arrival time of the first clear frame on cleared, the barge-in
// cut as Telnyx receives it. It sends the arrival time of each outbound media
// frame on mediaAt, so the first one after a baseline is measured. The reader
// exits when the stream closes.
func recStreamDrain(s *telnyxfake.Stream, media *atomic.Int64, mediaAt chan<- time.Time, cleared chan<- time.Time) {
	go func() {
		for {
			b, err := s.Recv(context.Background())
			if err != nil {
				return
			}
			var f struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(b, &f) != nil {
				continue
			}
			switch f.Event {
			case "media":
				media.Add(1)
				select {
				case mediaAt <- time.Now():
				default:
				}
			case "clear":
				select {
				case cleared <- time.Now():
				default:
				}
			}
		}
	}()
}

// recWaitAgentFrames waits until at least n agent media frames reached the
// stream. The caller's barge-in must land while agent audio is playing.
func recWaitAgentFrames(t *testing.T, media *atomic.Int64, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for media.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("agent media frames = %d, want %d", media.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recRunMedia drives one call's media run through the real stream. It is paced
// near real time so the recorder's wall clock and the caller's RTP clock agree.
//
// The caller track is: 10 loud frames; 25 silent frames; an RTP gap of 20
// frames (400 ms, no frames sent); then, after the agent's 30 queued chunks have
// played for recPlayoutWait, 10 loud frames. The caller keeps sending quiet
// frames during the wait. Realtime cuts on the provider's speech_started event;
// live cuts on the local detector's onset, which needs audible agent audio.
// The result holds the measured agent times for the agent-channel checks.
func recRunMedia(t *testing.T, env *callTestEnv, tel *fakeTelnyx, ai *fakeAI, protocol realtime.Protocol, callID string) recAgentTiming {
	t.Helper()
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-init-"+callID, recControlID)
	env.signedWebhook(callID, "call.answered", "evt-ans-"+callID, recControlID)
	stream, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	sendStart(t, stream, recControlID)
	var media atomic.Int64
	// mediaAt is large enough for every outbound frame of the run, so no
	// arrival is dropped before the first one after the baseline is read.
	mediaAt := make(chan time.Time, 4096)
	cleared := make(chan time.Time, 1)
	recStreamDrain(stream, &media, mediaAt, cleared)

	loud, quiet := recLoudPCMU(t), recQuietPCMU(t)
	seq := 2
	ts := uint32(0)
	emit := func(payload string) {
		recSendMediaAt(t, stream, seq, ts, payload)
		seq++
		ts += recFrameSamples
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		emit(loud)
		if i == 0 {
			// Instructions and the first audio event must reach the provider
			// before the rest of the caller track.
			waitEvents(t, ai, 2)
		}
	}
	for i := 0; i < 25; i++ {
		emit(quiet)
	}
	// The RTP gap: timestamps advance by 20 frames and no frames are sent.
	ts += 20 * recFrameSamples
	time.Sleep(400 * time.Millisecond)

	var timing recAgentTiming
	base := media.Load()
	timing.firstSend = time.Now()
	for i := 0; i < recAgentQueued; i++ {
		ai.sendAgentAudio(t, loud)
	}
	// One outbound media frame reaches the stream per queued chunk, counted from
	// the baseline taken before the sends. Once all have arrived, the server has
	// sent every chunk.
	recWaitAgentFrames(t, &media, base+recAgentQueued)
	// The first outbound frame after the baseline is the first queued chunk's
	// arrival at the stream. Audio cannot start before it, so the bounds use it.
	// The drain sends one arrival per media frame in order, so the arrival with
	// index base is the first one after the baseline. Earlier arrivals were
	// consumed by the caller's sends before the baseline and are skipped.
	timing.firstMedia = recArrivalAt(t, mediaAt, base)
	timing.queued = time.Now()
	// The agent plays uninterrupted for recPlayoutWait. The caller stays silent
	// but keeps sending quiet frames, as a phone does, so its RTP clock moves
	// with wall time.
	for deadline := time.Now().Add(recPlayoutWait); time.Now().Before(deadline); {
		emit(quiet)
	}
	timing.trigger = time.Now()
	if protocol == realtime.ProtocolRealtime {
		ai.sendRaw(t, map[string]any{"type": "input_audio_buffer.speech_started",
			"event_id": "ev_barge", "audio_start_ms": 0, "item_id": "msg_caller"})
	}
	// Live cuts on the local detector's onset, which the first of these loud
	// frames triggers. The cut's clear frame reaches the stream after either
	// trigger, so the wire time is the measured cut.
	for i := 0; i < 10; i++ {
		emit(loud)
	}
	select {
	case timing.wire = <-cleared:
	case <-time.After(5 * time.Second):
		t.Fatal("no clear frame reached the stream after the barge-in")
	}
	if err := stream.Close(); err != nil {
		t.Logf("close stream: %v", err)
	}
	return timing
}

// recArrivalAt returns the arrival time of the outbound media frame with zero
// based index base, as the drain counted them. The drain sends one arrival per
// media frame in order, and this is the only reader, so the first base values
// are the frames before the baseline. The next value is the first frame after
// it.
func recArrivalAt(t *testing.T, mediaAt <-chan time.Time, base int64) time.Time {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for i := int64(0); i <= base; i++ {
		select {
		case at := <-mediaAt:
			if i == base {
				return at
			}
		case <-deadline:
			t.Fatalf("outbound media arrival %d was never recorded", i)
		}
	}
	return time.Time{}
}

// recWaitSettled waits until the call's recording row leaves the recording
// state and returns its final status. A bounded wait keeps a stuck writer from
// hanging the test.
func recWaitSettled(t *testing.T, dataDir, callID string) string {
	t.Helper()
	db := openAppDB(t, dataDir)
	deadline := time.Now().Add(15 * time.Second)
	for {
		var status string
		err := db.QueryRow(`SELECT status FROM recordings WHERE call_id = ?`, callID).Scan(&status)
		if err == nil && status != "recording" {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording for %s never settled (status %q, err %v)", callID, status, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// recRecordingStart returns the recording's start time, the wall time the
// recorder uses as t0. It is read from created_at, which is written from the
// same clock reading, at the storage precision of milliseconds.
func recRecordingStart(t *testing.T, dataDir, callID string) time.Time {
	t.Helper()
	var created string
	if err := openAppDB(t, dataDir).QueryRow(`SELECT created_at FROM recordings WHERE call_id = ?`, callID).Scan(&created); err != nil {
		t.Fatalf("recording created_at: %v", err)
	}
	ts, err := time.Parse(storage.TimeLayout, created)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", created, err)
	}
	return ts
}

// recRecordingRows counts the recordings rows of one call.
func recRecordingRows(t *testing.T, dataDir, callID string) int {
	t.Helper()
	var n int
	if err := openAppDB(t, dataDir).QueryRow(`SELECT COUNT(*) FROM recordings WHERE call_id = ?`, callID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// recBlockRMS returns the RMS of each consecutive n-sample block.
func recBlockRMS(s []int16, n int) []float64 {
	out := make([]float64, 0, len(s)/n)
	for i := 0; i+n <= len(s); i += n {
		out = append(out, recRMS(s, i, i+n))
	}
	return out
}

// recRunLen counts the blocks from index from for which pred holds.
func recRunLen(rms []float64, from int, pred func(float64) bool) int {
	n := 0
	for i := from; i < len(rms) && pred(rms[i]); i++ {
		n++
	}
	return n
}

func recIsLoud(v float64) bool   { return v >= recLoudRMS }
func recIsSilent(v float64) bool { return v < recSilentRMS }

// TestRecordingEndToEnd places a recorded call over the real stream for each
// protocol, then reads it back through the REST endpoints and checks the audio.
func TestRecordingEndToEnd(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, recControlID)
			ai := newFakeAI(t, protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, ai.endpoint())

			recSetPreference(t, env, env.admin, true, true)
			callID := recCreateCall(t, env, tpID, "rec-on-"+string(protocol), map[string]any{"record": true})
			timing := recRunMedia(t, env, tel, ai, protocol, callID)
			if st := recWaitSettled(t, env.dataDir, callID); st != "complete" {
				t.Fatalf("recording status = %q, want complete", st)
			}
			recCheckRecording(t, env, callID, timing)
		})
	}
}

// recCheckRecording reads the metadata, the full audio, a Range that spans a
// segment boundary, and the download disposition, then checks the audio.
func recCheckRecording(t *testing.T, env *callTestEnv, callID string, timing recAgentTiming) {
	t.Helper()
	t0 := recRecordingStart(t, env.dataDir, callID)
	admin := env.admin.headers(false)
	base := env.base + "/api/v1/calls/" + callID + "/recording"

	status, _, body := send(t, env.client, http.MethodGet, base, nil, admin)
	if status != http.StatusOK {
		t.Fatalf("metadata = %d %s", status, body)
	}
	meta := decodeObject(t, body)
	if meta["status"] != "complete" || meta["sample_rate"] != float64(8000) || meta["channels"] != float64(2) {
		t.Fatalf("metadata = %s", body)
	}
	flags := fmt.Sprint(meta["quality_flags"])
	for _, want := range []string{"caller_gap_filled", "agent_interrupted", "agent_playout_estimated"} {
		if !strings.Contains(flags, want) {
			t.Fatalf("quality_flags %s lack %s", flags, want)
		}
	}
	// byte_count is the size of the WAV the audio endpoint serves: header plus PCM.
	wavBytes := int(meta["byte_count"].(float64))

	status, hdr, full := send(t, env.client, http.MethodGet, base+"/audio", nil, admin)
	if status != http.StatusOK {
		t.Fatalf("audio = %d", status)
	}
	if ct := hdr.Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content type = %q", ct)
	}
	if len(full) != wavBytes {
		t.Fatalf("audio body = %d bytes, want byte_count %d", len(full), wavBytes)
	}
	w := recParseWAV(t, full)
	if w.rate != 8000 {
		t.Fatalf("WAV rate = %d, want 8000", w.rate)
	}
	recCheckAudioShape(t, w, timing, t0)

	// 16384 PCM bytes are one segment, so WAV offset 16428 is a boundary.
	hr := admin.Clone()
	hr.Set("Range", "bytes=16400-16499")
	status, hdr, part := send(t, env.client, http.MethodGet, base+"/audio", nil, hr)
	if status != http.StatusPartialContent || len(part) != 100 {
		t.Fatalf("range = %d with %d bytes, want 206 with 100", status, len(part))
	}
	if string(part) != string(full[16400:16500]) {
		t.Fatal("range bytes differ from the same span of the full audio")
	}
	if want := fmt.Sprintf("bytes 16400-16499/%d", len(full)); hdr.Get("Content-Range") != want {
		t.Fatalf("Content-Range = %q, want %q", hdr.Get("Content-Range"), want)
	}

	dl := admin.Clone()
	status, hdr, _ = send(t, env.client, http.MethodGet, base+"/audio?download=1", nil, dl)
	if status != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(hdr.Get("Content-Disposition"), "call-"+callID+".wav") {
		t.Fatalf("download = %d disposition %q", status, hdr.Get("Content-Disposition"))
	}
}

// recCheckAudioShape checks the block energies. Caller (channel 0): speech,
// then about 0.9 s of silence (the quiet frames plus the RTP gap), then speech.
// Agent (channel 1): at most 0.4 s of audio, because the barge-in cuts a 0.6 s
// utterance within milliseconds of its queueing. The 0.4 s limit leaves room for
// the scheduling delay between queueing and the cut.
func recCheckAudioShape(t *testing.T, w recWAV, timing recAgentTiming, t0 time.Time) {
	t.Helper()
	rms0 := recBlockRMS(w.ch0, recFrameSamples)
	// The longest silent run is the gap plus the quiet frames. Its position is
	// found, not assumed, so a leading block that lands below the threshold
	// cannot shift the check.
	bestStart, bestLen := 0, 0
	for i := 0; i < len(rms0); {
		if !recIsSilent(rms0[i]) {
			i++
			continue
		}
		n := recRunLen(rms0, i, recIsSilent)
		if n > bestLen {
			bestStart, bestLen = i, n
		}
		i += n
	}
	// The silent run is 25 quiet frames, the 20-frame RTP gap, and the quiet
	// frames sent during the agent playout wait (about 15), so about 60 blocks.
	// The threshold of 35 allows for the anchor and the flush boundary.
	if bestLen < 35 {
		t.Fatalf("caller silence = %d blocks, want at least 35", bestLen)
	}
	loudBefore, loudAfter := 0, 0
	for i := 0; i < bestStart; i++ {
		if recIsLoud(rms0[i]) {
			loudBefore++
		}
	}
	for i := bestStart + bestLen; i < len(rms0); i++ {
		if recIsLoud(rms0[i]) {
			loudAfter++
		}
	}
	if loudBefore < 5 {
		t.Fatalf("caller speech before the gap = %d blocks, want at least 5", loudBefore)
	}
	if loudAfter < 8 {
		t.Fatalf("caller speech after the gap = %d blocks, want at least 8", loudAfter)
	}

	recCheckAgentAudio(t, w, timing, t0)
}

// recCheckAgentAudio checks the agent channel against the measured script times.
//
// The agent queues recAgentQueued chunks of one 20 ms block each, starting at
// firstSend. Each chunk plays from its wall-clock play start, and the recorder
// keeps the audio that played before the cut. The cut happened at some moment
// between the barge-in trigger and the clear frame reaching the stream. So the
// audible agent audio is at least the trigger minus firstSend, and at most the
// clear's wire time minus firstSend. Both are measured, so the bounds are too.
//
// The ±3 blocks of slack on the count are: one block for the 20 ms quantization
// of counting whole blocks, one for the cut-signal latency between the trigger
// and the clear (the wire time already bounds the cut from above, but the
// sink's clock read and the first audible sample add a block), and one for the
// RMS window edge (a block that straddles the start or the cut can fall above or
// below the loud threshold).
func recCheckAgentAudio(t *testing.T, w recWAV, timing recAgentTiming, t0 time.Time) {
	t.Helper()
	const blockDur = 20 * time.Millisecond
	// The chunks play from their arrival at the stream, not from the send, so
	// the bounds are measured from the first outbound frame's arrival.
	lower := timing.trigger.Sub(timing.firstMedia)
	upper := timing.wire.Sub(timing.firstMedia)
	if lower <= 0 || upper < lower {
		t.Fatalf("measured agent times are not ordered: trigger-firstMedia=%v wire-firstMedia=%v", lower, upper)
	}
	lowBlocks := int(lower / blockDur)
	highBlocks := int((upper + blockDur - 1) / blockDur)
	if highBlocks >= recAgentQueued {
		t.Fatalf("the cut came %v after the first agent send, at or past the %d-chunk queue (%v): the cut-tail check would be vacuous. Lower recPlayoutWait or queue more chunks.",
			upper, recAgentQueued, recAgentQueued*blockDur)
	}
	wantMin := lowBlocks - 3
	wantMax := highBlocks + 3

	agent := recBlockRMS(w.ch1, recFrameSamples)
	loud, first, last := 0, -1, -1
	for i, v := range agent {
		if recIsLoud(v) {
			loud++
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	t.Logf("agent playout: firstMedia->trigger=%v firstMedia->cut(wire)=%v queued=%d bounds=[%d,%d] agentLoud=%d",
		lower, upper, recAgentQueued, wantMin, wantMax, loud)

	if loud < wantMin || loud > wantMax {
		t.Fatalf("agent audio = %d loud blocks, want between %d and %d (from %v before the cut trigger and %v before the cut reached the wire, both from the first outbound frame)",
			loud, wantMin, wantMax, lower, upper)
	}
	if loud >= recAgentQueued {
		t.Fatalf("agent audio = %d loud blocks, want fewer than the %d queued: the cut tail must be absent", loud, recAgentQueued)
	}
	if first < 0 {
		t.Fatal("no agent audio before the barge-in: the agent speech was not recorded")
	}

	// Positions are sample offsets from the recording start, the clock the
	// recorder uses for agent chunks. The recorder's t0 is read from created_at
	// at millisecond precision, so each position is off by at most 8 samples.
	firstMediaPos := recSampleAt(timing.firstMedia.Sub(t0), w.rate)
	cutPos := recSampleAt(timing.wire.Sub(t0), w.rate)
	startPos := first * recFrameSamples
	endPos := (last + 1) * recFrameSamples
	t.Logf("agent positions: firstMedia=%d start=%d end=%d cut=%d (samples from t0, block=%d)",
		firstMediaPos, startPos, endPos, cutPos, recFrameSamples)
	if startPos < firstMediaPos-recFrameSamples {
		t.Fatalf("agent audio starts at sample %d, earlier than the first outbound frame at %d by more than one block", startPos, firstMediaPos)
	}
	if endPos > cutPos+recFrameSamples {
		t.Fatalf("agent audio ends at sample %d, after the cut at %d by more than one block", endPos, cutPos)
	}
	// There is no check that agent audio ends before the caller's post-gap
	// speech. The caller's loud frames are sent at the barge-in trigger, but the
	// cut follows the trigger by the detector latency (about 40 ms live), so the
	// agent legitimately plays into the caller's onset. The cut-position check
	// above is the ordering that holds.
}

// recSampleAt converts a duration from t0 to whole samples at rate, with the
// same truncation the recorder uses.
func recSampleAt(d time.Duration, rate int) int {
	if d <= 0 {
		return 0
	}
	return int(int64(d) * int64(rate) / int64(time.Second))
}
