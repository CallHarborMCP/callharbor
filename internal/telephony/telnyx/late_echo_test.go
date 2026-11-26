package telnyx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
	"github.com/coder/websocket"
)

// lateClock is a clock the test advances, shared by the sink and the test.
type lateClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lateClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Late echoes of marks the ledger has trimmed must not close the media stream.
// Telnyx echoes every mark, so after a stall the echoes can arrive long after
// their chunks expired. Before the fix, the trimmed echoes returned
// ErrUnknownMark, and dispatch closed the stream with a policy violation.
//
// The test queues more expired chunks than the ledger keeps, then echoes every
// mark late over the real signed media endpoint. The stream must stay open for
// all echoes and close normally on stop.
func TestMediaStreamSurvivesLateEchoesOfTrimmedMarks(t *testing.T) {
	const chunks = 300 // above the 256 expired marks the ledger keeps
	priv, pub := keys(t)
	clock := &lateClock{now: time.Unix(1_800_000_000, 0)}

	var (
		marks  []string
		markMu sync.Mutex
		// built is closed when the sink factory has built the sink, so the
		// test waits for the start frame to be handled, not for a sleep.
		built = make(chan *telnyx.BridgeSink, 1)
	)
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var ep http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ep.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=late"
	me, err := telnyx.NewMediaEndpoint(v, streamURL, func(_ context.Context, start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, g711, time.Millisecond, time.Second)
		if err != nil {
			return nil, err
		}
		s, err := telnyx.NewBridgeSink(b, func(ctx context.Context, msg []byte) error {
			var m struct {
				Event string `json:"event"`
				Mark  struct {
					Name string `json:"name"`
				} `json:"mark"`
			}
			if err := json.Unmarshal(msg, &m); err != nil {
				return err
			}
			if m.Event == "mark" {
				markMu.Lock()
				marks = append(marks, m.Mark.Name)
				markMu.Unlock()
			}
			return send(ctx, msg)
		})
		if err != nil {
			return nil, err
		}
		s.SetClock(clock.Now)
		s.ToProvider = func([]byte) error { return nil }
		built <- s
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ep = me

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := telnyxfake.DialMedia(ctx, streamURL, priv, time.Now())
	if err != nil {
		t.Fatalf("signed dial rejected: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send(ctx, startFrame(t)); err != nil {
		t.Fatal(err)
	}

	// Queue agent audio: each chunk is 20 ms of PCMU and gets a mark.
	var out *telnyx.BridgeSink
	select {
	case out = <-built:
	case <-ctx.Done():
		t.Fatal("sink was not built on start")
	}
	chunk := make([]byte, 160)
	for i := range chunk {
		chunk[i] = audio.LinearToUlaw(int16(i * 50))
	}
	for i := 0; i < chunks; i++ {
		if err := out.AgentAudio(ctx, audio.SourceRef{ItemID: fmt.Sprintf("late-%d", i)}, chunk); err != nil {
			t.Fatalf("chunk %d refused: %v", i, err)
		}
	}
	// Drain the outbound media and mark frames the sink wrote to the stream.
	// Their marks are what Telnyx will echo. The sink writes through send,
	// which the recorder above captures.
	markMu.Lock()
	issued := append([]string(nil), marks...)
	markMu.Unlock()
	if len(issued) != chunks {
		t.Fatalf("issued %d marks, want %d", len(issued), chunks)
	}

	// Every chunk finishes and its echo is long overdue: the marks expire, and
	// the ledger trims all but the newest MaxExpiredPending.
	clock.Set(clock.Now().Add(time.Duration(chunks)*20*time.Millisecond + time.Minute))

	for i, name := range issued {
		frame := mustJSON(t, map[string]any{
			"event": "mark", "stream_id": "s1", "sequence_number": fmt.Sprint(100 + i),
			"mark": map[string]any{"name": name},
		})
		if err := s.Send(ctx, frame); err != nil {
			t.Fatalf("send echo %d: %v", i, err)
		}
	}
	// The stream must still be open after every late echo. A policy-violation
	// close would fail the next read, so send stop and expect a normal close.
	if err := s.Send(ctx, mustJSON(t, map[string]any{"event": "stop", "stream_id": "s1"})); err != nil {
		t.Fatalf("stream closed before stop was sent: %v", err)
	}
	// After stop the server closes the stream normally. Outbound frames written
	// before the stop may still be read first, so read to the close. A
	// policy-violation close here would mean a late echo killed the stream.
	var closeErr error
	for {
		if _, err := s.Recv(ctx); err != nil {
			closeErr = err
			break
		}
	}
	if status := websocket.CloseStatus(closeErr); status != websocket.StatusNormalClosure {
		t.Fatalf("stream closed with %v (%v), want a normal closure after stop", status, closeErr)
	}
}
