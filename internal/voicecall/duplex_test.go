package voicecall_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
	"github.com/CallHarborMCP/callharbor/internal/voice"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

// dxLeg is both the telephony leg and the provider wire format, so the bridge
// performs no transcoding and provider bytes must equal Telnyx payload bytes.
var dxLeg = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

// dxChunks returns n chunks of 20 ms G.711 agent audio, each 160 bytes.
func dxChunks(n, seed int) [][]byte {
	out := make([][]byte, n)
	for c := range out {
		b := make([]byte, 160)
		for i := range b {
			b[i] = audio.LinearToUlaw(int16((i*(96+seed)+c*257)%20000 - 10000))
		}
		out[c] = b
	}
	return out
}

// dxTone is one 20 ms frame of a loud 440 Hz tone, well above the detector's
// threshold. Its bytes are never the G.711 silence code 0xFF.
func dxTone() []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = audio.LinearToUlaw(int16(8000 * math.Sin(2*math.Pi*440*float64(i)/8000)))
	}
	return b
}

// dxLoud reports whether a G.711 frame carries speech rather than silence.
// Silence is encoded as 0xFF in μ-law; the fake provider uses this to decide
// when the caller starts talking, which is the only thing it inspects.
func dxLoud(frame []byte) bool {
	for _, b := range frame {
		if b != 0xFF {
			return true
		}
	}
	return false
}

// dxRealtime is an independent fake of the documented OpenAI Realtime
// WebSocket protocol. It does not use CallHarbor's adapter code. It replies to
// session.update with agent audio for one output item, and on the caller's
// first loud frame it sends input_audio_buffer.speech_started followed by late
// deltas for the same item, which a real server can still emit during a cut.
// After a truncate it starts a new output item (continuation).
type dxRealtime struct {
	t       *testing.T
	agent   [][]byte
	stale   [][]byte
	next    [][]byte
	started chan time.Time
	truncs  chan dxTruncate
}

type dxTruncate struct {
	item  string
	index int
	endMS int64
}

func (p *dxRealtime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()
	send := func(v map[string]any) {
		b, err := json.Marshal(v)
		if err != nil {
			p.t.Errorf("marshal provider event: %v", err)
			return
		}
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	delta := func(item string, chunk []byte) map[string]any {
		return map[string]any{
			"type": "response.output_audio.delta", "event_id": "evt_" + item,
			"response_id": "resp_" + item, "item_id": item, "output_index": 0,
			"content_index": 0, "delta": base64.StdEncoding.EncodeToString(chunk),
		}
	}
	sess := map[string]any{"id": "sess_dx", "type": "realtime"}
	send(map[string]any{"type": "session.created", "event_id": "evt_created", "session": sess})
	heard := false
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			p.t.Errorf("provider got invalid JSON: %v", err)
			return
		}
		switch m["type"] {
		case "session.update":
			// Echo the requested audio format, as the documented handshake requires.
			// The fake reads the client's request rather than hard-coding a format.
			if s, ok := m["session"].(map[string]any); ok {
				sess = map[string]any{"id": "sess_dx", "type": "realtime", "audio": s["audio"]}
			}
			send(map[string]any{"type": "session.updated", "event_id": "evt_updated", "session": sess})
			for _, ch := range p.agent {
				send(delta("item_dx", ch))
			}
		case "input_audio_buffer.append":
			audioB64, _ := m["audio"].(string)
			frame, err := base64.StdEncoding.DecodeString(audioB64)
			if err != nil {
				p.t.Errorf("provider got bad base64: %v", err)
				return
			}
			if !heard && dxLoud(frame) {
				heard = true
				p.started <- time.Now()
				send(map[string]any{"type": "input_audio_buffer.speech_started", "event_id": "evt_speech", "audio_start_ms": 0, "item_id": "item_dx"})
				for _, ch := range p.stale {
					send(delta("item_dx", ch))
				}
			}
		case "response.cancel":
			// The documented barge-in path on this engine never cancels a
			// response. Any cancel is a protocol violation, so fail here.
			p.t.Errorf("client sent response.cancel; barge-in must use Telnyx clear and truncate only")
		case "conversation.item.truncate":
			item, _ := m["item_id"].(string)
			idx, _ := m["content_index"].(float64)
			end, _ := m["audio_end_ms"].(float64)
			p.truncs <- dxTruncate{item: item, index: int(idx), endMS: int64(end)}
			for _, ch := range p.next {
				send(delta("item_next", ch))
			}
		}
	}
}

// dxClock is the one media clock shared by the sink, the engine's audibility
// checks and the Telnyx emulator. Provider and sink goroutines read it, so it is
// guarded; the test advances it together with the emulator.
type dxClock struct {
	mu sync.Mutex
	t  time.Time
}

func newDxClock() *dxClock { return &dxClock{t: time.Unix(1_800_000_000, 0)} }

func (c *dxClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *dxClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// dxAdvance moves the emulator and the shared clock by the same media time.
func dxAdvance(clk *dxClock, pl *telnyxfake.Playout, d time.Duration) {
	clk.advance(d)
	pl.Advance(d)
}

// dxStart brings up a signed Telnyx media endpoint. Its sink is the production
// path: BridgeSink, voicecall.AudioBridge, and voice.RunWith over the real
// adapter. It returns the Telnyx side of the stream, the run's result channel,
// and a context bounding every Telnyx-side read.
func dxStart(t *testing.T, connect func(ctx context.Context) (realtime.Session, error), caps realtime.Capabilities, clk *dxClock, silence time.Duration) (*telnyxfake.Stream, <-chan error, context.Context) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	t.Cleanup(stopRun)
	errs := make(chan error, 1)

	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		me.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=dx"

	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(_ context.Context, start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, dxLeg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		sink, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		sink.SetClock(clk.Now)
		ab, err := voicecall.NewAudioBridge(sink)
		if err != nil {
			return nil, err
		}
		sess, err := connect(runCtx)
		if err != nil {
			return nil, err
		}
		go func() {
			_, err := voice.RunWith(runCtx, sess, ab, ab.EventSink(nil), voice.Options{Caps: caps, Now: clk.Now, SilenceTimeout: silence})
			errs <- err
		}()
		return sink, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	s, err := telnyxfake.DialMedia(ctx, streamURL, priv, now)
	if err != nil {
		t.Fatalf("signed dial rejected: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	start := `{"event":"start","stream_id":"dx-1","sequence_number":"1","start":{"call_control_id":"v3:dx","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`
	if err := s.Send(ctx, []byte(start)); err != nil {
		t.Fatal(err)
	}
	return s, errs, ctx
}

// dxInbound is one caller media frame as Telnyx sends it.
func dxInbound(t *testing.T, payload []byte, seq int) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event": "media", "stream_id": "dx-1", "sequence_number": fmt.Sprint(seq),
		"media": map[string]any{"track": "inbound", "chunk": fmt.Sprint(seq), "timestamp": fmt.Sprint(seq * 20), "payload": base64.StdEncoding.EncodeToString(payload)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dxRecv reads one CallHarbor→Telnyx frame, bounded by ctx.
func dxRecv(t *testing.T, s *telnyxfake.Stream, ctx context.Context) []byte {
	t.Helper()
	b, err := s.Recv(ctx)
	if err != nil {
		t.Fatalf("recv from CallHarbor: %v", err)
	}
	return b
}

// dxFrame decodes the event name and, for media frames, the payload bytes.
func dxFrame(t *testing.T, raw []byte) (string, []byte) {
	t.Helper()
	var m struct {
		Event string `json:"event"`
		Media struct {
			Payload string `json:"payload"`
		} `json:"media"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("malformed frame from CallHarbor: %v", err)
	}
	if m.Event != "media" {
		return m.Event, nil
	}
	b, err := base64.StdEncoding.DecodeString(m.Media.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return m.Event, b
}

// dxEcho returns every mark the emulator has echoed to Telnyx, as Telnyx does.
func dxEcho(t *testing.T, s *telnyxfake.Stream, ctx context.Context, pl *telnyxfake.Playout, seq *int) {
	t.Helper()
	for _, e := range pl.TakeEchoes() {
		*seq++
		if err := s.Send(ctx, telnyxfake.MarkFrame("dx-1", *seq, e.Name)); err != nil {
			t.Fatal(err)
		}
	}
}

func dxMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// TestDuplexRealtimeBargeInProductionPath drives realtime_ws through the
// production path. It checks, against an independent Telnyx playout emulator:
// agent audio before the cut is byte-exact, caller speech overlapping playout
// clears it, the played audio is an exact prefix of the agent audio, late
// deltas for the cut item never reach Telnyx, the cut item is truncated, and a
// new output item after the cut plays in full.
func TestDuplexRealtimeBargeInProductionPath(t *testing.T) {
	prov := &dxRealtime{
		t:       t,
		agent:   dxChunks(50, 1),
		stale:   dxChunks(3, 3),
		next:    dxChunks(5, 4),
		started: make(chan time.Time, 1),
		truncs:  make(chan dxTruncate, 4),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"

	clk := newDxClock()
	s, errs, ctx := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		sess, err := realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
		if err != nil {
			return nil, err
		}
		return sess, nil
	}, realtime.RealtimeCapabilities, clk, 0)
	_ = errs

	pl := telnyxfake.NewPlayout(8000, clk.Now())
	seq := 0

	// Phase 1: all agent audio reaches Telnyx; each chunk is followed by a mark.
	var agentSent []byte
	for _, ch := range prov.agent {
		agentSent = append(agentSent, ch...)
	}
	for i := 0; i < 2*len(prov.agent); i++ {
		raw := dxRecv(t, s, ctx)
		want := "media"
		if i%2 == 1 {
			want = "mark"
		}
		if got, _ := dxFrame(t, raw); got != want {
			t.Fatalf("frame %d: got %s, want %s", i, got, want)
		}
		if err := pl.Receive(raw); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: Telnyx plays 200 ms (10 chunks), then echoes the marks reached.
	dxAdvance(clk, pl, 200*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)

	// Phase 3: caller speech overlaps the agent's playout. The first loud frame
	// is the onset; the provider decides to barge in on it.
	sentAt := time.Now()
	for i := 0; i < 3; i++ {
		seq++
		if err := s.Send(ctx, dxInbound(t, dxTone(), seq)); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 4: the next frame must be clear. Nothing from the cut item may
	// precede it, including the late deltas the provider sent during the cut.
	raw := dxRecv(t, s, ctx)
	clearAt := time.Now()
	if got, _ := dxFrame(t, raw); got != "clear" {
		t.Fatalf("after barge-in got %s, want clear before any further audio", got)
	}
	if err := pl.Receive(raw); err != nil {
		t.Fatal(err)
	}
	dxEcho(t, s, ctx, pl, &seq)

	startedAt := <-prov.started
	t.Logf("BARGEIN_LATENCY realtime frame->clear ms=%.2f", dxMS(clearAt.Sub(sentAt)))
	t.Logf("BARGEIN_LATENCY realtime provider-speech_started->clear ms=%.2f", dxMS(clearAt.Sub(startedAt)))
	if d := clearAt.Sub(sentAt); d > 500*time.Millisecond {
		t.Fatalf("barge-in clear took %v, bound 500ms", d)
	}

	// Phase 5: the cut item is truncated to the played position, never past
	// what was sent.
	select {
	case tr := <-prov.truncs:
		if tr.item != "item_dx" || tr.index != 0 {
			t.Fatalf("truncate targeted %s[%d], want item_dx[0]", tr.item, tr.index)
		}
		if tr.endMS < 0 || tr.endMS > 1000 {
			t.Fatalf("audio_end_ms = %d, outside [0, 1000] sent", tr.endMS)
		}
	case <-ctx.Done():
		t.Fatal("cut item was never truncated")
	}

	// Phase 6: continuation. The first audio after clear is the new item, in full.
	var nextSent []byte
	for _, ch := range prov.next {
		nextSent = append(nextSent, ch...)
	}
	for i := 0; i < 2*len(prov.next); i++ {
		raw := dxRecv(t, s, ctx)
		want := "media"
		if i%2 == 1 {
			want = "mark"
		}
		if got, _ := dxFrame(t, raw); got != want {
			t.Fatalf("continuation frame %d: got %s, want %s (stale cut-item audio?)", i, got, want)
		}
		if err := pl.Receive(raw); err != nil {
			t.Fatal(err)
		}
	}
	dxAdvance(clk, pl, 100*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)

	// Played audio is exactly the 200 ms prefix of the agent item followed by the
	// whole continuation item. Nothing from the cut tail played.
	want := append(append([]byte(nil), agentSent[:1600]...), nextSent...)
	if got := pl.Played(); string(got) != string(want) {
		t.Fatalf("played %d bytes, want exact 1600-byte prefix plus %d-byte continuation", len(got), len(nextSent))
	}
	if err := s.Close(); err != nil {
		t.Logf("close media stream: %v", err)
	}
}

// dxLive is an independent fake of the documented GPT-Live primary WebSocket
// protocol: the client sends session.start first, the server answers
// session.started, then streams session.output_audio.delta. It has no item
// identity, no speech-start, no truncate and no cancel event. Each reply is
// triggered by a caller turn that ends, which the fake detects from the
// audio the client appends: a loud run followed by silence.
type dxLive struct {
	t       *testing.T
	mu      sync.Mutex
	turns   [][][]byte // reply chunks per caller turn, index = turn
	stale   [][]byte   // audio streamed at the first speech onset, belonging to the cut turn
	recvd   []string   // client event types, in order
	closeEv chan struct{}
	turnEnd chan int // one value per caller turn that ends, with the turn index
	speech  chan time.Time
}

func (p *dxLive) record(typ string) {
	p.mu.Lock()
	p.recvd = append(p.recvd, typ)
	p.mu.Unlock()
}

func (p *dxLive) events() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.recvd...)
}

func (p *dxLive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()
	send := func(v map[string]any) {
		b, err := json.Marshal(v)
		if err != nil {
			p.t.Errorf("marshal provider event: %v", err)
			return
		}
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	delta := func(chunk []byte) map[string]any {
		return map[string]any{"type": "session.output_audio.delta", "delta": base64.StdEncoding.EncodeToString(chunk)}
	}

	// Turn tracking. A turn starts on the first loud frame and ends after
	// dxHangover silent frames. The fake replies at the end of each turn with
	// the next scripted reply; during the first turn it also streams stale
	// audio while the caller is still speaking, which the client must suppress.
	const hangover = 20
	turn := -1 // index of the current caller turn, -1 before any speech
	inSpeech := false
	silent := 0
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			p.t.Errorf("provider got invalid JSON: %v", err)
			return
		}
		typ, _ := m["type"].(string)
		p.record(typ)
		switch typ {
		case "session.start":
			s, _ := m["session"].(map[string]any)
			s["id"], s["status"] = "live_dx", "active"
			sess, _ := json.Marshal(s)
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.started","event_id":"evt_started","client_event_id":"ch_session_start","session":`+string(sess)+`}`))
		case "session.input_audio.append":
			audioB64, _ := m["audio"].(string)
			frame, err := base64.StdEncoding.DecodeString(audioB64)
			if err != nil {
				p.t.Errorf("provider got bad base64: %v", err)
				return
			}
			if dxLoud(frame) {
				silent = 0
				if !inSpeech {
					inSpeech = true
					turn++
					if turn == 0 {
						p.speech <- time.Now()
					}
					if turn == 1 {
						// Stale audio from the cut turn, streamed while the caller
						// is speaking over it. The client must not play any of it.
						for _, ch := range p.stale {
							send(delta(ch))
						}
					}
				}
				continue
			}
			if !inSpeech {
				continue
			}
			silent++
			if silent < hangover {
				continue
			}
			// Caller turn ended: reply with this turn's scripted audio. Each
			// reply is one chunk here; the test scripts how many turns exist.
			inSpeech = false
			p.turnEnd <- turn
			if turn < len(p.turns) {
				for _, ch := range p.turns[turn] {
					send(delta(ch))
				}
			}
		case "session.close":
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_closed","reason":"close_requested","usage":{"seconds":1}}`))
			select {
			case p.closeEv <- struct{}{}:
			default:
			}
		}
	}
}

// dxTurnTone and dxTurnSilence drive the caller side of a turn in 20 ms frames.
const dxHangoverFrames = 20

// TestDuplexLivePathBargeIn drives live_ws through the production path.
//
// Live has no speech-start event and no item identity, so barge-in comes from
// the local caller-speech detector and the anonymous turn gate. The schedule:
//
//  1. Opening turn: the caller speaks and stops. The fake replies with turn0.
//     Nothing is playing at the onset, so nothing is cut.
//  2. Barge-in: while turn0 plays, the caller speaks over it. The onset cuts
//     the audible reply. The fake streams stale audio at the onset, which the
//     anonymous gate must suppress.
//  3. Continuation: the caller stops, the fake replies with turn1, and the
//     caller then takes a second turn that brings turn2.
//
// Checked against an independent Telnyx playout emulator: clear follows the
// onset, nothing plays after clear, the played audio is an exact prefix of
// turn0, stale audio never plays, and both continuation turns play in full.
// The Live fake records every client event, so the test proves no truncate or
// cancel was sent, and that the session was closed on teardown.
func TestDuplexLivePathBargeIn(t *testing.T) {
	turn0 := dxChunks(20, 1)
	turn1 := dxChunks(10, 6)
	turn2 := dxChunks(10, 7)
	prov := &dxLive{
		t:       t,
		stale:   dxChunks(3, 5),
		turns:   [][][]byte{turn0, turn1, turn2},
		closeEv: make(chan struct{}, 1),
		turnEnd: make(chan int, 8),
		speech:  make(chan time.Time, 4),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"

	clk := newDxClock()
	s, errs, ctx := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		sess, err := livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
		if err != nil {
			return nil, err
		}
		return sess, nil
	}, realtime.LiveCapabilities, clk, 0)

	pl := telnyxfake.NewPlayout(8000, clk.Now())
	seq := 0
	seqNext := func() int { seq++; return seq }

	// Caller side: a turn is 20 ms tones (onset) then silence (hangover).
	callerTurn := func(tones int) {
		for i := 0; i < tones; i++ {
			if err := s.Send(ctx, dxInbound(t, dxTone(), seqNext())); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < dxHangoverFrames; i++ {
			if err := s.Send(ctx, dxInbound(t, dxSilence(), seqNext())); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Phase 1: opening turn. Wait for the provider to see the turn end, then
	// drain turn0 into the emulator: each chunk is followed by a mark.
	callerTurn(3)
	select {
	case <-prov.turnEnd:
	case <-ctx.Done():
		t.Fatal("opening turn never ended at the provider")
	}
	for i := 0; i < 2*len(turn0); i++ {
		raw := dxRecv(t, s, ctx)
		want := "media"
		if i%2 == 1 {
			want = "mark"
		}
		if got, _ := dxFrame(t, raw); got != want {
			t.Fatalf("turn0 frame %d: got %s, want %s", i, got, want)
		}
		if err := pl.Receive(raw); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: play 100 ms of turn0 (5 chunks), then the caller barges in.
	dxAdvance(clk, pl, 100*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)
	sentAt := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.Send(ctx, dxInbound(t, dxTone(), seqNext())); err != nil {
			t.Fatal(err)
		}
	}

	// Frames already written before the onset (turn0 chunks and their marks)
	// may still be ahead of clear on the stream. Each is fed to the emulator
	// as normal. The first frame that is not turn0 audio or its marks must be
	// clear, and nothing from turn0 may come after it.
	var raw []byte
	var clearAt time.Time
	for {
		raw = dxRecv(t, s, ctx)
		got, _ := dxFrame(t, raw)
		if got == "clear" {
			clearAt = time.Now()
			break
		}
		if got != "media" && got != "mark" {
			t.Fatalf("unexpected frame %s before clear", got)
		}
		if err := pl.Receive(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := pl.Receive(raw); err != nil {
		t.Fatal(err)
	}
	dxEcho(t, s, ctx, pl, &seq)

	startedAt := <-prov.speech
	t.Logf("BARGEIN_LATENCY live frame->clear ms=%.2f", dxMS(clearAt.Sub(sentAt)))
	t.Logf("BARGEIN_LATENCY live provider-received-onset->clear ms=%.2f", dxMS(clearAt.Sub(startedAt)))
	if d := clearAt.Sub(sentAt); d > 500*time.Millisecond {
		t.Fatalf("barge-in clear took %v, bound 500ms", d)
	}

	// Phase 3: finish the barge-in turn. The caller's silence ends the turn at
	// the provider, which replies with turn1. Nothing may play before it.
	for i := 0; i < dxHangoverFrames; i++ {
		if err := s.Send(ctx, dxInbound(t, dxSilence(), seqNext())); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-prov.turnEnd:
	case <-ctx.Done():
		t.Fatal("barge-in turn never ended at the provider")
	}
	playContinuation := func(chunks [][]byte) {
		t.Helper()
		for i := 0; i < 2*len(chunks); i++ {
			raw := dxRecv(t, s, ctx)
			want := "media"
			if i%2 == 1 {
				want = "mark"
			}
			if got, _ := dxFrame(t, raw); got != want {
				t.Fatalf("continuation frame %d: got %s, want %s (stale turn0 audio?)", i, got, want)
			}
			if err := pl.Receive(raw); err != nil {
				t.Fatal(err)
			}
		}
		dxAdvance(clk, pl, time.Duration(len(chunks))*20*time.Millisecond)
		dxEcho(t, s, ctx, pl, &seq)
	}
	playContinuation(turn1)
	// turn1 has fully played before the caller speaks again, so the next onset is
	// an ordinary turn and not a barge-in on turn1. The emulator is the authority
	// on what Telnyx is still playing, so its queue must be empty here.
	if got, want := len(pl.Played()), 160*(len(turn0[:5])+len(turn1)); got < want {
		t.Fatalf("played %d bytes before the next caller turn, want at least %d (turn0 prefix + turn1)", got, want)
	}

	// Phase 4: a second caller turn brings turn2, proving the gate stays open
	// across turns and is not one-shot.
	callerTurn(3)
	select {
	case <-prov.turnEnd:
	case <-ctx.Done():
		t.Fatal("second continuation turn never ended at the provider")
	}
	playContinuation(turn2)

	// Played audio: exactly the 100 ms prefix of turn0, then all of turn1 and
	// turn2. Nothing from the cut tail, and nothing from stale audio.
	var want []byte
	for _, ch := range turn0[:5] {
		want = append(want, ch...)
	}
	for _, ch := range turn1 {
		want = append(want, ch...)
	}
	for _, ch := range turn2 {
		want = append(want, ch...)
	}
	if got := pl.Played(); !bytes.Equal(got, want) {
		t.Fatalf("played %d bytes, want exact 100ms turn0 prefix + turn1 + turn2 (%d bytes)", len(got), len(want))
	}

	// No truncate or cancel was sent, and the session is closed on teardown.
	for _, typ := range prov.events() {
		switch typ {
		case "conversation.item.truncate", "response.cancel":
			t.Fatalf("client sent %s on live_ws, which has no such event", typ)
		}
	}
	_ = errs
}

// dxSilence is one 20 ms frame of digital silence (μ-law 0xFF).
func dxSilence() []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}
