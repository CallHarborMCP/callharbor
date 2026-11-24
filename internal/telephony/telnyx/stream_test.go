package telnyx_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

var pcmu8k = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t       *testing.T
	clk     *clock
	auth    *telnyx.StreamAuth
	priv    ed25519.PrivateKey
	verif   *telnyx.Verifier
	srv     *httptest.Server
	sess    chan *telnyx.MediaSession
	baseURL string
}

func newHarness(t *testing.T, mutate func(*telnyx.StreamConfig)) *harness {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	v, err := telnyx.NewVerifier(priv.Public().(ed25519.PublicKey), telnyx.DefaultTolerance, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, clk: clk, auth: telnyx.NewStreamAuth(), priv: priv, verif: v, sess: make(chan *telnyx.MediaSession, 4)}
	cfg := telnyx.StreamConfig{
		Format: pcmu8k, InboundQueue: 64, JitterFrames: 2, OutboundQueue: 8,
		MaxChunkBytes: 1600, IdleTimeout: 5 * time.Second, ExpireEvery: 10 * time.Millisecond,
		PlayoutTolerance: 5 * time.Millisecond, PlayoutGrace: 2 * time.Second, Now: clk.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.srv = srv
	h.baseURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/telnyx/media/"
	mux.Handle("GET /telnyx/media/{token}", &telnyx.MediaHandler{
		Verifier: v, Auth: h.auth, BaseURL: h.baseURL, Config: cfg,
		Accept: func(s *telnyx.MediaSession) { h.sess <- s },
	})
	return h
}

// dial performs a Telnyx-style signed media WebSocket handshake for a fresh
// per-call token and returns the client connection.
func (h *harness) dial(callID string, ts time.Time) (*websocket.Conn, string) {
	h.t.Helper()
	tok, err := h.auth.Issue(callID, time.Minute, h.clk.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return h.dialToken(tok, ts, nil)
}

func (h *harness) dialToken(tok string, ts time.Time, mutate func(http.Header)) (*websocket.Conn, string) {
	h.t.Helper()
	u := h.baseURL + tok
	hdr := telnyx.Sign(h.priv, ts, []byte(u))
	if mutate != nil {
		mutate(hdr)
	}
	c, resp, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, fmt.Sprint(status, " ", err)
	}
	return c, ""
}

func (h *harness) session() *telnyx.MediaSession {
	h.t.Helper()
	select {
	case s := <-h.sess:
		return s
	case <-time.After(5 * time.Second):
		h.t.Fatal("no session accepted")
	}
	return nil
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func startFrame(callID, stream string, enc string, rate int) map[string]any {
	return map[string]any{
		"event": "start", "sequence_number": "1", "stream_id": stream,
		"start": map[string]any{
			"call_control_id": callID, "call_session_id": "sess",
			"media_format": map[string]any{"encoding": enc, "sample_rate": rate, "channels": 1},
		},
	}
}

func wsMediaFrame(stream, track string, ts uint32, payload []byte) map[string]any {
	return map[string]any{
		"event": "media", "stream_id": stream,
		"media": map[string]any{"track": track, "chunk": fmt.Sprint(ts), "timestamp": fmt.Sprint(ts), "payload": base64.StdEncoding.EncodeToString(payload)},
	}
}

func silence(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

func TestStreamAuthRequiresSignedHandshakeAndSingleUseToken(t *testing.T) {
	h := newHarness(t, nil)
	now := h.clk.Now()
	tok, err := h.auth.Issue("v2:call-1", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}

	if _, msg := h.dialToken(tok, now, func(hdr http.Header) { hdr.Del(telnyx.HeaderSignature) }); !strings.HasPrefix(msg, "401") {
		t.Fatalf("missing signature accepted: %s", msg)
	}
	if _, msg := h.dialToken(tok, now.Add(-time.Hour), nil); !strings.HasPrefix(msg, "401") {
		t.Fatalf("stale signature accepted: %s", msg)
	}
	if _, msg := h.dialToken("not-issued", now, nil); !strings.HasPrefix(msg, "401") {
		t.Fatalf("unissued token accepted: %s", msg)
	}

	c, msg := h.dialToken(tok, now, nil)
	if c == nil {
		t.Fatalf("valid handshake rejected: %s", msg)
	}
	s := h.session()
	if s.CallControlID() != "v2:call-1" {
		t.Fatalf("session bound to %q", s.CallControlID())
	}
	_ = c.CloseNow()
	<-s.Done()

	if _, msg := h.dialToken(tok, now, nil); !strings.HasPrefix(msg, "401") {
		t.Fatalf("token reused: %s", msg)
	}
}

func TestStreamAuthRejectsSignatureOverDifferentURL(t *testing.T) {
	h := newHarness(t, nil)
	now := h.clk.Now()
	tok, _ := h.auth.Issue("v2:call-2", time.Minute, now)
	other := h.baseURL + "other-token"
	hdr := telnyx.Sign(h.priv, now, []byte(other))
	_, resp, err := websocket.Dial(context.Background(), h.baseURL+tok, &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		t.Fatal("signature over a different URL accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resp %v err %v", resp, err)
	}
}

func TestStreamAuthTokenExpiry(t *testing.T) {
	a := telnyx.NewStreamAuth()
	now := time.Unix(1_700_000_000, 0)
	tok, err := a.Issue("v2:x", time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Redeem(tok, now.Add(2*time.Second)); err == nil {
		t.Fatal("expired token redeemed")
	}
	if _, err := a.Issue("", time.Second, now); err == nil {
		t.Fatal("empty call issued")
	}
	if _, err := a.Redeem("", now); err == nil {
		t.Fatal("empty token redeemed")
	}
}

func TestMediaLifecycleInboundOutboundAndStop(t *testing.T) {
	h := newHarness(t, nil)
	now := h.clk.Now()
	c, msg := h.dial("v2:call-3", now)
	if c == nil {
		t.Fatal(msg)
	}
	defer func() { _ = c.CloseNow() }()
	s := h.session()
	ctx := context.Background()

	send(t, c, map[string]any{"event": "connected", "version": "1.0.0"})
	send(t, c, startFrame("v2:call-3", "st-1", "PCMU", 8000))
	for i := 0; i < 5; i++ {
		send(t, c, wsMediaFrame("st-1", "inbound", uint32(160*i), silence(160)))
	}
	// Reordered: 4 arrives after 5 and is delivered in RTP order once 3 lands.
	send(t, c, wsMediaFrame("st-1", "inbound", 160*6, silence(160)))
	send(t, c, wsMediaFrame("st-1", "inbound", 160*5, silence(160)))

	var got []telnyx.InboundAudio
	deadline := time.After(3 * time.Second)
	for len(got) < 5 {
		select {
		case a := <-s.Audio():
			got = append(got, a)
		case <-deadline:
			t.Fatalf("received %d chunks, want 5 before stop", len(got))
		}
	}
	for i, a := range got {
		if a.Position != int64(160*i) || a.Gap != 0 {
			t.Fatalf("chunk %d position %d gap %d", i, a.Position, a.Gap)
		}
	}

	if err := s.Enqueue(audio.SourceRef{ItemID: "item_1"}, silence(160)); err != nil {
		t.Fatal(err)
	}
	mediaMsg := readText(t, ctx, c)
	if !strings.Contains(mediaMsg, `"event":"media"`) {
		t.Fatalf("expected media, got %s", mediaMsg)
	}
	markMsg := readText(t, ctx, c)
	if !strings.Contains(markMsg, `"event":"mark"`) {
		t.Fatalf("expected mark, got %s", markMsg)
	}
	var m struct {
		Mark struct {
			Name string `json:"name"`
		} `json:"mark"`
	}
	if err := json.Unmarshal([]byte(markMsg), &m); err != nil {
		t.Fatal(err)
	}
	h.clk.Add(20 * time.Millisecond)
	send(t, c, map[string]any{"event": "mark", "stream_id": "st-1", "mark": map[string]any{"name": m.Mark.Name}})

	send(t, c, wsMediaFrame("st-1", "outbound", 0, silence(160)))
	send(t, c, map[string]any{"event": "dtmf", "stream_id": "st-1", "dtmf": map[string]any{"digit": "5"}})
	select {
	case d := <-s.DTMF():
		if d != "5" {
			t.Fatalf("dtmf %q", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dtmf not delivered")
	}

	send(t, c, map[string]any{"event": "stop", "stream_id": "st-1"})
	<-s.Done()
	if err := s.Err(); err != nil {
		t.Fatalf("clean stop returned %v", err)
	}
	r := s.Result()
	if r.Stats.InboundChunks != 7 || r.Stats.GapSamples != 0 || r.Stats.OtherTrack != 1 || r.Stats.DTMF != 1 {
		t.Fatalf("stats %+v", r.Stats)
	}
	if r.Stats.OutboundChunks != 1 || r.Stats.MarksPlayed != 1 {
		t.Fatalf("outbound accounting %+v", r.Stats)
	}
}

func readText(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, b, err := c.Read(rctx)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLossIsReportedAsGapAndDuplicatesDropped(t *testing.T) {
	h := newHarness(t, func(c *telnyx.StreamConfig) { c.JitterFrames = 0 })
	now := h.clk.Now()
	c, msg := h.dial("v2:call-4", now)
	if c == nil {
		t.Fatal(msg)
	}
	defer func() { _ = c.CloseNow() }()
	s := h.session()
	send(t, c, map[string]any{"event": "connected"})
	send(t, c, startFrame("v2:call-4", "st-2", "PCMU", 8000))
	send(t, c, wsMediaFrame("st-2", "inbound", 0, silence(160)))
	send(t, c, wsMediaFrame("st-2", "inbound", 0, silence(160)))
	send(t, c, wsMediaFrame("st-2", "inbound", 480, silence(160)))
	send(t, c, map[string]any{"event": "stop", "stream_id": "st-2"})
	<-s.Done()
	r := s.Result()
	if r.Stats.Stale != 1 {
		t.Fatalf("second copy of timestamp 0 must be stale with no jitter buffer: %+v", r.Stats)
	}
	if r.Stats.GapSamples != 320 {
		t.Fatalf("gap samples %d, want 320 for two lost 20 ms frames", r.Stats.GapSamples)
	}
}

func TestDuplicateInJitterBufferCountedOnce(t *testing.T) {
	h := newHarness(t, func(c *telnyx.StreamConfig) { c.JitterFrames = 4 })
	c, msg := h.dial("v2:call-5", h.clk.Now())
	if c == nil {
		t.Fatal(msg)
	}
	defer func() { _ = c.CloseNow() }()
	s := h.session()
	send(t, c, map[string]any{"event": "connected"})
	send(t, c, startFrame("v2:call-5", "st-3", "PCMU", 8000))
	send(t, c, wsMediaFrame("st-3", "inbound", 160, silence(160)))
	send(t, c, wsMediaFrame("st-3", "inbound", 160, silence(160)))
	send(t, c, wsMediaFrame("st-3", "inbound", 0, silence(160)))
	send(t, c, map[string]any{"event": "stop", "stream_id": "st-3"})
	<-s.Done()
	r := s.Result()
	if r.Stats.Duplicates != 1 || r.Stats.InboundChunks != 2 {
		t.Fatalf("stats %+v", r.Stats)
	}
}

func TestProtocolViolationsCloseSession(t *testing.T) {
	cases := map[string]func(c *websocket.Conn){
		"media before start": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "connected"})
			send(t, c, wsMediaFrame("st", "inbound", 0, silence(160)))
		},
		"start for other call": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "connected"})
			send(t, c, startFrame("v2:other", "st", "PCMU", 8000))
		},
		"format mismatch": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "connected"})
			send(t, c, startFrame("v2:call-6", "st", "L16", 16000))
		},
		"binary frame": func(c *websocket.Conn) {
			_ = c.Write(context.Background(), websocket.MessageBinary, []byte{1, 2, 3})
		},
		"duplicate start": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "connected"})
			send(t, c, startFrame("v2:call-6", "st", "PCMU", 8000))
			send(t, c, startFrame("v2:call-6", "st", "PCMU", 8000))
		},
		"malformed json": func(c *websocket.Conn) {
			_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"event":`))
		},
		"unknown event": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "pause"})
		},
		"telnyx error frame": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "error", "payload": map[string]any{"code": 100003, "title": "bad"}})
		},
		"dtmf before start": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "dtmf", "stream_id": "st", "dtmf": map[string]any{"digit": "1"}})
		},
		"unknown mark echo": func(c *websocket.Conn) {
			send(t, c, map[string]any{"event": "connected"})
			send(t, c, startFrame("v2:call-6", "st", "PCMU", 8000))
			send(t, c, map[string]any{"event": "mark", "stream_id": "st", "mark": map[string]any{"name": "forged"}})
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			c, msg := h.dial("v2:call-6", h.clk.Now())
			if c == nil {
				t.Fatal(msg)
			}
			defer func() { _ = c.CloseNow() }()
			s := h.session()
			run(c)
			<-s.Done()
			if s.Err() == nil {
				t.Fatal("violation not reported")
			}
			if !errors.Is(s.Err(), telnyx.ErrStreamProtocol) && !errors.Is(s.Err(), telnyx.ErrInvalidFrame) && !errors.Is(s.Err(), telnyx.ErrUnauthorizedStream) {
				t.Fatalf("unexpected error %v", s.Err())
			}
		})
	}
}

func TestIdleStreamTimesOut(t *testing.T) {
	h := newHarness(t, func(c *telnyx.StreamConfig) { c.IdleTimeout = 50 * time.Millisecond })
	c, msg := h.dial("v2:call-7", h.clk.Now())
	if c == nil {
		t.Fatal(msg)
	}
	defer func() { _ = c.CloseNow() }()
	s := h.session()
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("idle stream never ended")
	}
	if !errors.Is(s.Err(), telnyx.ErrStreamIdle) {
		t.Fatalf("err %v", s.Err())
	}
}

func TestEnqueueRejectsInvalidChunks(t *testing.T) {
	h := newHarness(t, func(c *telnyx.StreamConfig) { c.OutboundQueue = 2 })
	c, msg := h.dial("v2:call-8", h.clk.Now())
	if c == nil {
		t.Fatal(msg)
	}
	defer func() { _ = c.CloseNow() }()
	s := h.session()
	send(t, c, map[string]any{"event": "connected"})
	send(t, c, startFrame("v2:call-8", "st-8", "PCMU", 8000))
	src := audio.SourceRef{ItemID: "item_9"}
	if err := s.Enqueue(src, nil); err == nil {
		t.Fatal("empty chunk accepted")
	}
	if err := s.Enqueue(src, silence(1601)); err == nil {
		t.Fatal("oversized chunk accepted")
	}
	send(t, c, map[string]any{"event": "stop", "stream_id": "st-8"})
	<-s.Done()
	if err := s.Enqueue(src, silence(160)); !errors.Is(err, telnyx.ErrStreamClosed) {
		t.Fatalf("enqueue after close: %v", err)
	}
}

func TestNegotiatedFormatTableAcceptsOnlyExactMatch(t *testing.T) {
	le := func(r int) audio.Format { return audio.Format{Encoding: audio.L16, SampleRate: r, LittleEndian: true} }
	cases := []struct {
		name       string
		negotiated audio.Format
		enc        string
		rate       int
		wantOK     bool
	}{
		{"pcmu 8k", pcmu8k, "PCMU", 8000, true},
		{"pcma 8k", audio.Format{Encoding: audio.PCMA, SampleRate: 8000}, "PCMA", 8000, true},
		{"l16 8k", le(8000), "L16", 8000, true},
		{"l16 16k", le(16000), "L16", 16000, true},
		{"l16 24k", le(24000), "L16", 24000, true},
		{"l16 48k", le(48000), "L16", 48000, true},
		{"pcmu negotiated, l16 sent", pcmu8k, "L16", 8000, false},
		{"l16 16k negotiated, 8k sent", le(16000), "L16", 8000, false},
		{"l16 16k negotiated, pcmu 8k sent", le(16000), "PCMU", 8000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *telnyx.StreamConfig) { c.Format = tc.negotiated })
			c, msg := h.dial("v2:call-fmt", h.clk.Now())
			if c == nil {
				t.Fatal(msg)
			}
			defer func() { _ = c.CloseNow() }()
			s := h.session()
			send(t, c, map[string]any{"event": "connected"})
			send(t, c, startFrame("v2:call-fmt", "st-f", tc.enc, tc.rate))
			if tc.wantOK {
				send(t, c, map[string]any{"event": "stop", "stream_id": "st-f"})
				<-s.Done()
				if err := s.Err(); err != nil {
					t.Fatalf("matching format rejected: %v", err)
				}
				return
			}
			<-s.Done()
			if !errors.Is(s.Err(), telnyx.ErrStreamProtocol) {
				t.Fatalf("mismatched format not rejected as protocol error: %v", s.Err())
			}
		})
	}
}

func TestUnsupportedStreamFormatRejectedByConfig(t *testing.T) {
	cfg := telnyx.StreamConfig{Format: audio.Format{Encoding: audio.PCMU, SampleRate: 16000}}
	if _, err := telnyx.NewMediaSessionForTest(cfg, "v2:x"); err == nil {
		t.Fatal("PCMU at 16 kHz accepted")
	}
}
