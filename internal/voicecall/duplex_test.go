package voicecall_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	// closed, when non-nil, receives one value when the provider connection ends.
	closed chan struct{}
	// oob enables the caller-transcription out-of-band path (#113): the caller's
	// committed item is announced, and the endpoint answers the out-of-band
	// response.create with a stray audio delta, text, and done.
	oob bool
	// oobReq receives one value when an out-of-band response.create arrives.
	oobReq chan struct{}
}

type dxTruncate struct {
	item  string
	index int
	endMS int64
}

func (p *dxRealtime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer p.signalClosed()
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
				send(map[string]any{"type": "input_audio_buffer.speech_started", "event_id": "evt_speech", "audio_start_ms": 0, "item_id": "item_user_dx"})
				for _, ch := range p.stale {
					send(delta("item_dx", ch))
				}
				if p.oob {
					// The caller's audio item is committed; the endpoint announces it.
					send(map[string]any{"type": "conversation.item.added", "event_id": "evt_added_dx", "item": map[string]any{
						"id": "item_user_dx", "type": "message", "status": "completed", "role": "user",
						"content": []map[string]any{{"type": "input_audio"}}}})
				}
			}
		case "response.create":
			// Only the out-of-band caller transcription sends this. It must ask
			// for text only, with no conversation and no billed transcription.
			if !p.oob {
				p.t.Errorf("unexpected response.create in an ordinary barge-in run")
				return
			}
			resp, _ := m["response"].(map[string]any)
			meta := resp["metadata"]
			_, billed := m["transcription"]
			_, billedSess := m["session"]
			if resp["conversation"] != "none" || resp["output_modalities"] == nil || billed || billedSess {
				p.t.Errorf("out-of-band request shape = %v", m)
			}
			select {
			case p.oobReq <- struct{}{}:
			default:
			}
			send(map[string]any{"type": "response.created", "event_id": "evt_oob_c", "response": map[string]any{
				"id": "resp_oob_dx", "status": "in_progress", "metadata": meta, "output": []any{}}})
			// Stray audio for the out-of-band response: it must never reach the
			// caller's media stream, the playout ledger, or the played bytes.
			send(map[string]any{"type": "response.output_audio.delta", "event_id": "evt_oob_a", "response_id": "resp_oob_dx",
				"item_id": "item_oob_dx", "output_index": 0, "content_index": 0,
				"delta": base64.StdEncoding.EncodeToString(dxChunks(1, 9)[0])})
			send(map[string]any{"type": "response.output_text.done", "event_id": "evt_oob_t", "response_id": "resp_oob_dx",
				"item_id": "item_oob_dx", "output_index": 0, "content_index": 0, "text": "Book a table."})
			send(map[string]any{"type": "response.done", "event_id": "evt_oob_d", "response": map[string]any{
				"id": "resp_oob_dx", "status": "completed", "metadata": meta, "output": []any{}}})
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

// signalClosed reports that the realtime fake's connection handler returned.
func (p *dxRealtime) signalClosed() {
	if p.closed != nil {
		select {
		case p.closed <- struct{}{}:
		default:
		}
	}
}

// dxStopSink is the test twin of callstream's stoppingSink: closing the Telnyx
// stream cancels the voice run, so the provider session is torn down with it.
type dxStopSink struct {
	*telnyx.BridgeSink
	stop context.CancelFunc
	// done is closed by Close, which the media endpoint calls only after its
	// frame loop has returned.
	done chan struct{}
}

func (s *dxStopSink) Close() error {
	s.stop()
	close(s.done)
	return s.BridgeSink.Close()
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
func dxStart(t *testing.T, connect func(ctx context.Context) (realtime.Session, error), caps realtime.Capabilities, clk *dxClock, silence time.Duration) (*telnyxfake.Stream, <-chan error, context.Context, context.CancelFunc) {
	t.Helper()
	return dxStartWith(t, connect, caps, clk, silence, nil, nil)
}

// dxHandle exposes the production voicebridge.Bridge behind a run, so a test
// can read the playout ledger, and a DTMF sync frame. The Telnyx read loop
// handles frames in order, so a DTMF sent after playout echoes is observed only
// after every earlier echo has been classified.
type dxHandle struct {
	bridge *voicebridge.Bridge
	ready  chan struct{}
	dtmf   chan string
	// stopOnClose makes closing the Telnyx stream cancel the run (production
	// callstream behaviour). Without it the sink's Close is a no-op.
	stopOnClose bool
	sinkDone    chan struct{}
	// result receives the voice.Result of the run, so a test can assert the
	// outcome (for example UsageConfirmed after a provider close).
	result chan voice.Result
}

func newDxHandle() *dxHandle {
	return &dxHandle{ready: make(chan struct{}), dtmf: make(chan string, 4), sinkDone: make(chan struct{}), result: make(chan voice.Result, 1)}
}

// dxSync sends a DTMF frame and waits until CallHarbor has processed it.
func dxSync(t *testing.T, s *telnyxfake.Stream, ctx context.Context, h *dxHandle, seq *int) {
	t.Helper()
	*seq++
	frame := fmt.Sprintf(`{"event":"dtmf","stream_id":"dx-1","sequence_number":"%d","dtmf":{"digit":"9"}}`, *seq)
	if err := s.Send(ctx, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.dtmf:
	case <-ctx.Done():
		t.Fatal("CallHarbor did not process the playout echoes before the sync frame")
	}
}

// dxAssertLedger checks the production playout ledger through its public
// accessors: every mark is classified (none pending), and each item's
// accounting equals the exact expected value.
func dxAssertLedger(t *testing.T, b *voicebridge.Bridge, want ...audio.ItemPlayout) {
	t.Helper()
	if n := b.Pending(); n != 0 {
		t.Fatalf("%d playout mark(s) were never classified", n)
	}
	if got := len(b.Items()); got != len(want) {
		t.Fatalf("ledger has %d item(s), want %d", got, len(want))
	}
	for _, w := range want {
		got, ok := b.Item(w.Source)
		if !ok || got != w {
			t.Fatalf("ledger item %+v = %+v (present %v), want %+v", w.Source, got, ok, w)
		}
	}
}

// dxStartWith is dxStart with an observer on the run's event sink. observe, if
// non-nil, is called for every provider event after the bridge has seen it and
// before the event is played, in the same goroutine as the downlink. A test
// uses it to learn that the client has processed an event (see the sentinel in
// TestDuplexLivePathBargeIn).
func dxStartWith(t *testing.T, connect func(ctx context.Context) (realtime.Session, error), caps realtime.Capabilities, clk *dxClock, silence time.Duration, observe func(realtime.Event), h *dxHandle) (*telnyxfake.Stream, <-chan error, context.Context, context.CancelFunc) {
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
	stopRunFn := stopRun

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
		if h != nil {
			h.bridge = b
			sink.OnDTMF = func(d string) { h.dtmf <- d }
			close(h.ready)
		}
		ab, err := voicecall.NewAudioBridge(sink)
		if err != nil {
			return nil, err
		}
		sess, err := connect(runCtx)
		if err != nil {
			return nil, err
		}
		go func() {
			sink := ab.EventSink(nil)
			if observe != nil {
				sink = ab.EventSink(observe)
			}
			res, err := voice.RunWith(runCtx, sess, ab, sink, voice.Options{Caps: caps, Now: clk.Now, SilenceTimeout: silence})
			if h != nil {
				h.result <- res
			}
			errs <- err
		}()
		if h != nil && h.stopOnClose {
			return &dxStopSink{BridgeSink: sink, stop: stopRunFn, done: h.sinkDone}, nil
		}
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
	if h != nil {
		select {
		case <-h.ready:
		case <-ctx.Done():
			t.Fatal("media stream never bound a bridge")
		}
	}
	return s, errs, ctx, stopRunFn
}

// dxStopRun ends the production run from the Telnyx side: the stream is closed
// and the run's context is cancelled, the same way a stopped leg ends the run.
// It waits, bounded, for Run to return, and asserts the terminal error. A run
// that ends for any reason other than that cancellation fails the test.
func dxStopRun(t *testing.T, s *telnyxfake.Stream, errs <-chan error, stop context.CancelFunc) {
	t.Helper()
	_ = s.Close()
	stop()
	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("run ended with %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the leg stopped")
	}
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

// dxDur is n milliseconds as a duration, for exact ledger expectations.
func dxDur(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// TestDuplexRealtimeBargeInProductionPath drives realtime_ws through the
// production path. It checks, against an independent Telnyx playout emulator:
// agent audio before the cut is byte-exact, caller speech overlapping playout
// clears it, the played audio is an exact prefix of the agent audio, late
// deltas for the cut item never reach Telnyx, the cut item is truncated, and a
// new output item after the cut plays in full.
func TestDuplexRealtimeBargeInProductionPath(t *testing.T) {
	dxBargeInRealtime(t, false)
}

// TestDuplexRealtimeBargeInWithOutOfBandCaller runs the same barge-in path with
// the out-of-band caller transcription (#113) active. The endpoint answers the
// caller's out-of-band request with a stray audio delta. That audio must never
// reach Telnyx, and barge-in, clear, truncate and continuation must behave as
// they do without it.
func TestDuplexRealtimeBargeInWithOutOfBandCaller(t *testing.T) {
	dxBargeInRealtime(t, true)
}

func dxBargeInRealtime(t *testing.T, oob bool) {
	prov := &dxRealtime{
		t:       t,
		agent:   dxChunks(50, 1),
		stale:   dxChunks(3, 3),
		next:    dxChunks(5, 4),
		started: make(chan time.Time, 1),
		truncs:  make(chan dxTruncate, 4),
		oob:     oob,
		oobReq:  make(chan struct{}, 1),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"

	clk := newDxClock()
	h := newDxHandle()
	s, errs, ctx, stop := dxStartWith(t, func(ctx context.Context) (realtime.Session, error) {
		sess, err := realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
		if err != nil {
			return nil, err
		}
		return sess, nil
	}, realtime.RealtimeCapabilities, clk, 0, nil, h)

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
		// Exactly 200 ms of the item reached the caller before the onset: the
		// emulator echoed the marks for 10 chunks at 200 ms and nothing more had
		// finished. audio_end_ms is the cut-time position, floored to whole ms.
		if want := int64(200); tr.endMS != want {
			t.Fatalf("audio_end_ms = %d, want exactly %d (200 ms played before the onset)", tr.endMS, want)
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

	// Ledger classification, through the production voicebridge ledger. Item
	// item_dx: 1000 ms sent; 10 marks echoed as played (200 ms confirmed); the
	// 40 chunks still queued at the cut are discarded (800 ms, flushed by Telnyx's
	// clear echoes). item_next: 100 ms sent and confirmed, nothing discarded.
	dxSync(t, s, ctx, h, &seq)
	dxAssertLedger(t, h.bridge,
		audio.ItemPlayout{Source: audio.SourceRef{ItemID: "item_dx"}, Sent: dxDur(1000), Confirmed: dxDur(200), Estimated: dxDur(200), Discarded: dxDur(800), Interrupted: true},
		audio.ItemPlayout{Source: audio.SourceRef{ItemID: "item_next"}, Sent: dxDur(100), Confirmed: dxDur(100), Estimated: dxDur(100)},
	)

	// Played audio is exactly the 200 ms prefix of the agent item followed by the
	// whole continuation item. Nothing from the cut tail played.
	want := append(append([]byte(nil), agentSent[:1600]...), nextSent...)
	if got := pl.Played(); string(got) != string(want) {
		t.Fatalf("played %d bytes, want exact 1600-byte prefix plus %d-byte continuation", len(got), len(nextSent))
	}
	if oob {
		// The out-of-band request must really have been sent, so the stray
		// audio above was exercised, not skipped.
		select {
		case <-prov.oobReq:
		default:
			t.Fatal("no out-of-band transcription request reached the endpoint")
		}
	}
	dxStopRun(t, s, errs, stop)
}

// dxLive is an independent fake of the documented GPT-Live primary WebSocket
// protocol: the client sends session.start first, the server answers
// session.started, then streams session.output_audio.delta. It has no item
// identity, no speech-start, no truncate and no cancel event. Each reply is
// triggered by a caller turn that ends, which the fake detects from the
// audio the client appends: a loud run followed by silence.
type dxLive struct {
	t     *testing.T
	mu    sync.Mutex
	turns [][][]byte // reply chunks per caller turn, index = turn
	stale [][]byte   // audio streamed at the first speech onset, belonging to the cut turn
	// sentinel, when set, is sent right after the stale audio. It is a
	// documented output transcript delta, which the client hands to its event
	// sink before it plays the audio that precedes it. Waiting for it proves
	// the stale audio has been processed.
	sentinel bool
	recvd    []string // client event types, in order
	closeEv  chan struct{}
	turnEnd  chan int // one value per caller turn that ends, with the turn index
	speech   chan time.Time
	// late is streamed after lateGate is closed. It carries no item ID and was
	// generated before any cut, so it models a cut reply that arrives after the
	// caller's end edge.
	late     [][]byte
	lateGate chan struct{}
	// closed, when non-nil, receives one value when the connection handler ends.
	closed chan struct{}
}

// dxProviderTurnFrames is the silent frames after which the fake provider ends
// a caller turn. It is a scripted provider value, not derived from the local
// detector's hangover (audio.VADHangover).
const dxProviderTurnFrames = 20

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
	defer func() {
		if p.closed != nil {
			select {
			case p.closed <- struct{}{}:
			default:
			}
		}
	}()
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

	if p.lateGate != nil {
		go func() {
			select {
			case <-p.lateGate:
			case <-ctx.Done():
				return
			}
			for _, ch := range p.late {
				send(delta(ch))
			}
		}()
	}

	// Turn tracking. A turn starts on the first loud frame and ends after
	// dxHangover silent frames. The fake replies at the end of each turn with
	// the next scripted reply; during the first turn it also streams stale
	// audio while the caller is still speaking, which the client must suppress.
	hangover := dxProviderTurnFrames
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
					if turn == 1 {
						// The barge-in onset: the first loud frame of the turn that
						// speaks over the reply. Stamp it for the latency metric,
						// then stream the stale audio of the cut reply, which the
						// client must not play.
						p.speech <- time.Now()
						for _, ch := range p.stale {
							send(delta(ch))
						}
						if p.sentinel {
							send(map[string]any{"type": "session.output_transcript.delta", "event_id": "evt_sentinel", "delta": "sentinel", "start_ms": 0, "end_ms": 0})
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

// dxHangoverFrames is the scripted caller's silence that ends its turn, in
// 20 ms frames. It is a fixed script value, not derived from audio.VADHangover,
// so the fakes do not change timing when CallHarbor's detector constants do.
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
		t:        t,
		stale:    dxChunks(3, 5),
		sentinel: true,
		turns:    [][][]byte{turn0, turn1, turn2},
		closeEv:  make(chan struct{}, 1),
		turnEnd:  make(chan int, 8),
		speech:   make(chan time.Time, 4),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"

	// sentinelSeen is closed when the client's downlink processes the sentinel.
	// The stale audio precedes it on the same read path, so by then the stale
	// chunks have been handed to the bridge (sink runs before Play).
	sentinelSeen := make(chan struct{})
	var sentinelOnce sync.Once

	clk := newDxClock()
	h := newDxHandle()
	s, errs, ctx, stop := dxStartWith(t, func(ctx context.Context) (realtime.Session, error) {
		sess, err := livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
		if err != nil {
			return nil, err
		}
		return sess, nil
	}, realtime.LiveCapabilities, clk, 0, func(ev realtime.Event) {
		if ev.RawType == "session.output_transcript.delta" {
			sentinelOnce.Do(func() { close(sentinelSeen) })
		}
	}, h)

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
	var sentAt time.Time
	for i := 0; i < 3; i++ {
		if i == 0 {
			// Taken immediately before the first barge-in frame is written, so
			// "first barge-in frame -> clear" measures what its label says.
			sentAt = time.Now()
		}
		if err := s.Send(ctx, dxInbound(t, dxTone(), seqNext())); err != nil {
			t.Fatal(err)
		}
	}

	// Frames already written before the onset (opening-turn audio and its marks)
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

	// The provider's receive stamp (prov.speech) is not a latency bound: its
	// goroutine can parse the onset after the client has already read clear, so
	// a difference against it can be negative. Only the frame-based figure is
	// reported.
	<-prov.speech
	t.Logf("BARGEIN_LATENCY live first-barge-in-frame->clear ms=%.2f", dxMS(clearAt.Sub(sentAt)))
	if d := clearAt.Sub(sentAt); d > 500*time.Millisecond {
		t.Fatalf("barge-in clear took %v, bound 500ms", d)
	}

	// Phase 3: finish the barge-in turn. The stale audio of the cut reply is in
	// flight ahead of the sentinel. Wait until the client has processed the
	// sentinel, so every stale chunk has reached the bridge before the caller's
	// silence can reopen the anonymous gate. Without this wait the silence
	// frames race the downlink, and stale chunks can pass the reopened gate.
	select {
	case <-sentinelSeen:
	case <-ctx.Done():
		t.Fatal("the client never processed the stale-audio sentinel")
	}
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
	// expected is what the bridge sends for a scripted chunk: the production
	// decode and encode of the leg format. It is not always the raw bytes, since
	// the mu-law round trip can change a code point.
	expected := func(c []byte) []byte {
		lin, err := audio.Decode(nil, dxLeg, c)
		if err != nil {
			t.Fatal(err)
		}
		out, err := audio.Encode(nil, dxLeg, lin)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// playContinuation receives one reply, checking each media frame's payload
	// against the expected chunk, in order, and each mark's position. A stale or
	// foreign chunk fails here by identity, not by frame type.
	playContinuation := func(phase string, chunks [][]byte) {
		t.Helper()
		for i := 0; i < len(chunks); i++ {
			for _, wantMark := range []bool{false, true} {
				raw := dxRecv(t, s, ctx)
				got, payload := dxFrame(t, raw)
				if wantMark {
					if got != "mark" {
						t.Fatalf("%s: chunk %d: got %s, want mark", phase, i, got)
					}
				} else {
					if got != "media" {
						t.Fatalf("%s: chunk %d: got %s, want media", phase, i, got)
					}
					if want := expected(chunks[i]); !bytes.Equal(payload, want) {
						t.Fatalf("%s: chunk %d payload is not the expected reply chunk (played %d bytes)", phase, i, len(pl.Played()))
					}
				}
				if err := pl.Receive(raw); err != nil {
					t.Fatal(err)
				}
			}
		}
		dxAdvance(clk, pl, time.Duration(len(chunks))*20*time.Millisecond)
		dxEcho(t, s, ctx, pl, &seq)
	}
	playContinuation("phase 3 (turn1)", turn1)
	// turn1 has fully played, so the emulator queue is empty and the played audio
	// is exactly turn0's 100 ms prefix followed by all of turn1. The next caller
	// onset is then an ordinary turn, not a barge-in on turn1.
	var wantPrefixTurn1 []byte
	for _, ch := range turn0[:5] {
		wantPrefixTurn1 = append(wantPrefixTurn1, expected(ch)...)
	}
	for _, ch := range turn1 {
		wantPrefixTurn1 = append(wantPrefixTurn1, expected(ch)...)
	}
	if got := pl.Played(); !bytes.Equal(got, wantPrefixTurn1) {
		t.Fatalf("before phase 4: played %d bytes, want exactly turn0 prefix + turn1 (%d bytes)", len(got), len(wantPrefixTurn1))
	}

	// Phase 4: a second caller turn brings turn2, proving the gate stays open
	// across turns and is not one-shot.
	callerTurn(3)
	select {
	case <-prov.turnEnd:
	case <-ctx.Done():
		t.Fatal("second continuation turn never ended at the provider")
	}
	playContinuation("phase 4 (turn2)", turn2)

	// Played audio: exactly the 100 ms prefix of turn0, then all of turn1 and
	// turn2. Nothing from the cut tail, and nothing from stale audio.
	var want []byte
	for _, ch := range turn0[:5] {
		want = append(want, dxExpect(t, ch)...)
	}
	for _, ch := range turn1 {
		want = append(want, dxExpect(t, ch)...)
	}
	for _, ch := range turn2 {
		want = append(want, dxExpect(t, ch)...)
	}
	if got := pl.Played(); !bytes.Equal(got, want) {
		t.Fatalf("played %d bytes, want exact 100ms turn0 prefix + turn1 + turn2 (%d bytes)", len(got), len(want))
	}

	// Ledger classification. Live has no item ID, so each reply is accounted
	// under the local utterance number the bridge assigned when the caller's
	// turn ended: turn0 is utterance 2 (after the opening turn), turn1 is 3
	// (after the barge-in turn) and turn2 is 4. Confirmed is exact: 100 ms of
	// turn0, then all of turn1 and turn2, matching the played byte stream above.
	// turn0's Sent is its 400 ms of scripted audio, plus any stale cut-reply
	// chunk that reached the bridge before the cut landed. That race is real on
	// this path: a stale chunk sent before the clear is queued, then discarded
	// by it. So Sent is bounded, not exact, and the ledger must still close:
	// everything sent is either estimated as played or discarded.
	dxSync(t, s, ctx, h, &seq)
	if items := h.bridge.Items(); len(items) != 3 {
		t.Fatalf("ledger has %d item(s), want one per reply utterance", len(items))
	}
	if n := h.bridge.Pending(); n != 0 {
		t.Fatalf("%d playout mark(s) were never classified", n)
	}
	it, ok := h.bridge.Item(audio.SourceRef{Utterance: 2})
	if !ok {
		t.Fatal("ledger has no entry for the interrupted utterance")
	}
	if it.Confirmed != dxDur(100) || it.Estimated != dxDur(100) {
		t.Fatalf("interrupted utterance confirmed %v estimated %v, want exactly 100ms", it.Confirmed, it.Estimated)
	}
	if it.Sent < dxDur(400) || it.Sent > dxDur(460) {
		t.Fatalf("sent %v, want 400ms of scripted reply plus at most the 60ms stale window", it.Sent)
	}
	if it.Estimated+it.Discarded != it.Sent {
		t.Fatalf("ledger does not close: estimated %v + discarded %v != sent %v", it.Estimated, it.Discarded, it.Sent)
	}
	if !it.Interrupted {
		t.Fatal("the barge-in cut was not recorded on the interrupted utterance")
	}
	for _, u := range []uint64{3, 4} {
		got, ok := h.bridge.Item(audio.SourceRef{Utterance: u})
		if !ok || got.Interrupted || got.Sent != dxDur(200) || got.Confirmed != dxDur(200) {
			t.Fatalf("utterance %d = %+v (present %v), want 200ms sent and confirmed, not interrupted", u, got, ok)
		}
	}
	if n := len(pl.Played()); n != 4000 {
		t.Fatalf("played %d bytes, want 4000 (500 ms at 8000 B/s, the ledger's confirmed duration)", n)
	}

	// No truncate or cancel was sent, and the session is closed on teardown.
	for _, typ := range prov.events() {
		switch typ {
		case "conversation.item.truncate", "response.cancel":
			t.Fatalf("client sent %s on live_ws, which has no such event", typ)
		}
	}
	dxStopRun(t, s, errs, stop)
}

// dxSilence is one 20 ms frame of digital silence (μ-law 0xFF).
func dxSilence() []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}
