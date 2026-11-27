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
// The reader exits when the stream closes.
func recStreamDrain(s *telnyxfake.Stream, media *atomic.Int64) {
	go func() {
		for {
			b, err := s.Recv(context.Background())
			if err != nil {
				return
			}
			var f struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(b, &f) == nil && f.Event == "media" {
				media.Add(1)
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
// frames (400 ms, no frames sent); then 10 loud frames. The agent sends 30
// audio chunks before the caller's second speech, and the caller barges in
// while they play. Realtime cuts on the provider's speech_started event; live
// cuts on the local detector's onset, which needs audible agent audio.
func recRunMedia(t *testing.T, env *callTestEnv, tel *fakeTelnyx, ai *fakeAI, protocol realtime.Protocol, callID string) {
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
	recStreamDrain(stream, &media)

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

	for i := 0; i < 30; i++ {
		ai.sendAgentAudio(t, loud)
	}
	recWaitAgentFrames(t, &media, 3)
	if protocol == realtime.ProtocolRealtime {
		ai.sendRaw(t, map[string]any{"type": "input_audio_buffer.speech_started",
			"event_id": "ev_barge", "audio_start_ms": 0, "item_id": "msg_caller"})
	}
	for i := 0; i < 10; i++ {
		emit(loud)
	}
	if err := stream.Close(); err != nil {
		t.Logf("close stream: %v", err)
	}
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
			recRunMedia(t, env, tel, ai, protocol, callID)
			if st := recWaitSettled(t, env.dataDir, callID); st != "complete" {
				t.Fatalf("recording status = %q, want complete", st)
			}
			recCheckRecording(t, env, callID)
		})
	}
}

// recCheckRecording reads the metadata, the full audio, a Range that spans a
// segment boundary, and the download disposition, then checks the audio.
func recCheckRecording(t *testing.T, env *callTestEnv, callID string) {
	t.Helper()
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
	recCheckAudioShape(t, w)

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
func recCheckAudioShape(t *testing.T, w recWAV) {
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
	// 45 silent blocks are written (25 quiet frames and 20 gap frames). The
	// threshold allows 200 ms for the anchor and the flush boundary.
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

	agentLoud := 0
	for _, v := range recBlockRMS(w.ch1, recFrameSamples) {
		if recIsLoud(v) {
			agentLoud++
		}
	}
	if agentLoud > 20 {
		t.Fatalf("agent audio = %d loud blocks, want at most 20 (the cut tail of 30 queued is absent)", agentLoud)
	}
}
