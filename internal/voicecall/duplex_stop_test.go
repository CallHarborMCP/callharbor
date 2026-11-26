package voicecall_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// dxStopFrame is the Telnyx stream stop event for dx-1.
func dxStopFrame(seq int) []byte {
	return []byte(fmt.Sprintf(`{"event":"stop","stream_id":"dx-1","sequence_number":"%d"}`, seq))
}

// dxSentClose reports whether the provider received session.close.
func dxSentClose(evs []string) bool {
	for _, e := range evs {
		if e == "session.close" {
			return true
		}
	}
	return false
}

// dxExpect returns what the bridge sends for a scripted chunk: the production
// decode and encode of the leg format.
func dxExpect(t *testing.T, c []byte) []byte {
	t.Helper()
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

// dxReceiveReply reads one reply's media and marks, feeding the emulator, and
// checks each media payload against the expected chunk in order.
func dxReceiveReply(t *testing.T, s *telnyxfake.Stream, ctx context.Context, pl *telnyxfake.Playout, chunks [][]byte, phase string) {
	t.Helper()
	for i, c := range chunks {
		for _, wantMark := range []bool{false, true} {
			raw := dxRecv(t, s, ctx)
			got, payload := dxFrame(t, raw)
			switch {
			case wantMark && got != "mark":
				t.Fatalf("%s: chunk %d: got %s, want mark", phase, i, got)
			case !wantMark && got != "media":
				t.Fatalf("%s: chunk %d: got %s, want media", phase, i, got)
			case !wantMark && !bytes.Equal(payload, dxExpect(t, c)):
				t.Fatalf("%s: chunk %d payload is not the expected reply chunk", phase, i)
			}
			if err := pl.Receive(raw); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// dxReceiveUntilClear feeds media and marks to the emulator until the clear.
// Any other frame is a failure.
func dxReceiveUntilClear(t *testing.T, s *telnyxfake.Stream, ctx context.Context, pl *telnyxfake.Playout) {
	t.Helper()
	for {
		raw := dxRecv(t, s, ctx)
		got, _ := dxFrame(t, raw)
		if err := pl.Receive(raw); err != nil {
			t.Fatal(err)
		}
		if got == "clear" {
			return
		}
		if got != "media" && got != "mark" {
			t.Fatalf("unexpected frame %s before clear", got)
		}
	}
}

// dxAwaitStopped checks the stop contract after the caller leg has ended:
// the run returns promptly with its reason, the provider connection is closed,
// the Telnyx media handler has returned, and no outbound frame follows.
func dxAwaitStopped(t *testing.T, s *telnyxfake.Stream, errs <-chan error, providerClosed <-chan struct{}, h *dxHandle, ctx context.Context) error {
	t.Helper()
	// The server's close handshake waits for the peer's close frame, so the
	// Telnyx side must keep reading. Reads end when the handshake completes.
	// writes counts outbound media, mark and clear frames read after the stop.
	// Every frame CallHarbor wrote before the stop was already consumed by the
	// test, so any frame counted here was sent after the stop.
	drained := make(chan int, 1)
	go func() {
		n := 0
		for {
			raw, err := s.Recv(ctx)
			if err != nil {
				drained <- n
				return
			}
			var m struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(raw, &m) == nil && (m.Event == "media" || m.Event == "mark" || m.Event == "clear") {
				n++
			}
		}
	}()
	var runErr error
	select {
	case runErr = <-errs:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return promptly after Telnyx stop")
	}
	select {
	case <-providerClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("provider connection was not closed after Telnyx stop")
	}
	select {
	case <-h.sinkDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Telnyx media handler did not return after stop")
	}
	select {
	case n := <-drained:
		if n != 0 {
			t.Fatalf("CallHarbor wrote %d media/mark/clear frame(s) after Telnyx stop", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Telnyx stream did not finish closing after stop")
	}
	return runErr
}

// TestDuplexRealtimeStopMidBargeIn ends the realtime_ws call with a Telnyx
// stop frame while the barge-in cut is in progress, on the production path.
// The run must end with context.Canceled, the provider connection must close,
// and no media may follow the stop. The stale cut-item deltas the provider
// streams at the onset are gated and never reach Telnyx.
func TestDuplexRealtimeStopMidBargeIn(t *testing.T) {
	prov := &dxRealtime{
		t:       t,
		agent:   dxChunks(50, 1),
		stale:   dxChunks(3, 3),
		started: make(chan time.Time, 1),
		truncs:  make(chan dxTruncate, 4),
		closed:  make(chan struct{}, 1),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"

	clk := newDxClock()
	h := newDxHandle()
	h.stopOnClose = true
	s, errs, ctx, _ := dxStartWith(t, func(ctx context.Context) (realtime.Session, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.RealtimeCapabilities, clk, 0, nil, h)

	pl := telnyxfake.NewPlayout(8000, clk.Now())
	seq := 0
	dxReceiveReply(t, s, ctx, pl, prov.agent, "agent")
	dxAdvance(clk, pl, 200*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)
	for i := 0; i < 3; i++ {
		seq++
		if err := s.Send(ctx, dxInbound(t, dxTone(), seq)); err != nil {
			t.Fatal(err)
		}
	}
	dxReceiveUntilClear(t, s, ctx, pl)
	dxEcho(t, s, ctx, pl, &seq)
	<-prov.started

	// The caller hangs up while the cut is in progress.
	seq++
	if err := s.Send(ctx, dxStopFrame(seq)); err != nil {
		t.Fatal(err)
	}
	if err := dxAwaitStopped(t, s, errs, prov.closed, h, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v, want context.Canceled after Telnyx stop", err)
	}
}

// TestDuplexLiveStopMidBargeIn ends the live_ws call with a Telnyx stop frame
// while the barge-in cut is in progress. The run must request the protocol
// close, end cleanly on the provider's session.closed, and close the connection.
// No media may follow the stop, and no truncate or cancel may be sent.
func TestDuplexLiveStopMidBargeIn(t *testing.T) {
	turn0 := dxChunks(20, 1)
	prov := &dxLive{
		t:       t,
		turns:   [][][]byte{turn0},
		stale:   dxChunks(3, 5),
		closeEv: make(chan struct{}, 1),
		closed:  make(chan struct{}, 1),
		turnEnd: make(chan int, 8),
		speech:  make(chan time.Time, 4),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"

	clk := newDxClock()
	h := newDxHandle()
	h.stopOnClose = true
	s, errs, ctx, _ := dxStartWith(t, func(ctx context.Context) (realtime.Session, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.LiveCapabilities, clk, 0, nil, h)

	pl := telnyxfake.NewPlayout(8000, clk.Now())
	seq := 0
	seqNext := func() int { seq++; return seq }
	for i := 0; i < dxHangoverFrames+3; i++ {
		frame := dxSilence()
		if i < 3 {
			frame = dxTone()
		}
		if err := s.Send(ctx, dxInbound(t, frame, seqNext())); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-prov.turnEnd:
	case <-ctx.Done():
		t.Fatal("opening turn never ended at the provider")
	}
	dxReceiveReply(t, s, ctx, pl, turn0, "turn0")
	dxAdvance(clk, pl, 100*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)
	for i := 0; i < 3; i++ {
		if err := s.Send(ctx, dxInbound(t, dxTone(), seqNext())); err != nil {
			t.Fatal(err)
		}
	}
	dxReceiveUntilClear(t, s, ctx, pl)
	dxEcho(t, s, ctx, pl, &seq)

	// The caller hangs up while the cut is in progress.
	if err := s.Send(ctx, dxStopFrame(seqNext())); err != nil {
		t.Fatal(err)
	}
	// The stop cancels the run. Production has two valid ends: the graceful
	// session.close completes and the run returns nil, or the socket is torn down
	// first and the run returns context.Canceled. Any other error fails. Nil is
	// only valid when the provider was asked to close.
	err := dxAwaitStopped(t, s, errs, prov.closed, h, ctx)
	evs := prov.events()
	switch {
	case err == nil:
		if !dxSentClose(evs) {
			t.Fatalf("run returned nil without a session.close request (provider events %v)", evs)
		}
	case errors.Is(err, context.Canceled):
	default:
		t.Fatalf("run ended with %v, want nil after a graceful close or context.Canceled", err)
	}
	for _, typ := range evs {
		if typ == "conversation.item.truncate" || typ == "response.cancel" {
			t.Fatalf("client sent %s on live_ws", typ)
		}
	}
}

// TestDuplexLiveStaleCutReplyAfterEndPlaysPinned pins the documented Live
// limitation. Audio the provider produced for a cut reply carries no item ID.
// When it reaches the bridge after the caller's speech-end edge has reopened
// the anonymous gate, it plays. Suppressing it would require an identity the
// protocol does not provide, so this test fixes the current behaviour and a
// future fix must flip it consciously.
//
// See docs/protocol/FEASIBILITY.md, Release blockers, item 3 ("Live stale
// cut-reply audio") and the Limitations entry "Live stale audio".
//
// Determinism: the late audio is released only after the client has processed
// the speech-end edge. The DTMF sync frame is read in order after the silence
// frames, so the edge has reopened the gate before the provider sends the late
// audio. No sleeps are used.
func TestDuplexLiveStaleCutReplyAfterEndPlaysPinned(t *testing.T) {
	turn0 := dxChunks(20, 1)
	late := dxChunks(5, 8)
	prov := &dxLive{
		t:        t,
		turns:    [][][]byte{turn0},
		late:     late,
		lateGate: make(chan struct{}),
		closeEv:  make(chan struct{}, 1),
		turnEnd:  make(chan int, 8),
		speech:   make(chan time.Time, 4),
	}
	srv := httptest.NewServer(http.HandlerFunc(prov.ServeHTTP))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"

	clk := newDxClock()
	h := newDxHandle()
	s, errs, ctx, stop := dxStartWith(t, func(ctx context.Context) (realtime.Session, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.LiveCapabilities, clk, 0, nil, h)

	pl := telnyxfake.NewPlayout(8000, clk.Now())
	seq := 0
	seqNext := func() int { seq++; return seq }

	// Opening turn, then turn0 plays for 100 ms.
	for i := 0; i < 3+dxHangoverFrames; i++ {
		frame := dxSilence()
		if i < 3 {
			frame = dxTone()
		}
		if err := s.Send(ctx, dxInbound(t, frame, seqNext())); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-prov.turnEnd:
	case <-ctx.Done():
		t.Fatal("opening turn never ended at the provider")
	}
	dxReceiveReply(t, s, ctx, pl, turn0, "turn0")
	dxAdvance(clk, pl, 100*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)

	// Barge-in cuts turn0.
	for i := 0; i < 3; i++ {
		if err := s.Send(ctx, dxInbound(t, dxTone(), seqNext())); err != nil {
			t.Fatal(err)
		}
	}
	dxReceiveUntilClear(t, s, ctx, pl)
	dxEcho(t, s, ctx, pl, &seq)

	// The caller stops. The end edge reopens the anonymous gate. The DTMF sync
	// frame is handled after every silence frame, so once it returns the gate
	// is open. Only then is the late audio released.
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
	dxSync(t, s, ctx, h, &seq)
	close(prov.lateGate)

	// Pinned behaviour: the late audio plays in full, byte-exact.
	dxReceiveReply(t, s, ctx, pl, late, "late cut-reply audio")
	dxAdvance(clk, pl, time.Duration(len(late))*20*time.Millisecond)
	dxEcho(t, s, ctx, pl, &seq)
	dxSync(t, s, ctx, h, &seq)

	var want []byte
	for _, c := range turn0[:5] {
		want = append(want, dxExpect(t, c)...)
	}
	for _, c := range late {
		want = append(want, dxExpect(t, c)...)
	}
	if got := pl.Played(); !bytes.Equal(got, want) {
		t.Fatalf("played %d bytes, want turn0 prefix + late cut-reply audio (%d bytes)", len(got), len(want))
	}
	dxStopRun(t, s, errs, stop)
}
