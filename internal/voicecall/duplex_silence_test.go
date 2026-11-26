package voicecall_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/voice"
)

// dxSilenceTimeout is the caller-silence bound for (j). It is small so the
// test is quick, and the test drives it on the shared media clock, not wall time.
const dxSilenceTimeout = 2 * time.Second

// awaitSilenceRun waits for the engine to return on the caller-silence path. It
// advances the media clock in steps so the watchdog's ticker observes the deadline.
func awaitSilenceRun(t *testing.T, clk *dxClock, errs <-chan error, ctx context.Context) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-errs:
			return err
		case <-ctx.Done():
			t.Fatal("engine did not end on caller silence")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not end on caller silence within the test bound")
		}
		clk.advance(dxSilenceTimeout / 4)
		time.Sleep(time.Millisecond)
	}
}

// TestDuplexRealtimeCallerSilenceEnds checks (j) for realtime_ws: with no
// caller speech and no agent playout past the timeout, the run ends with
// voice.ErrCallerSilence and closes the provider session.
func TestDuplexRealtimeCallerSilenceEnds(t *testing.T) {
	prov := &dxRealtime{t: t, agent: nil, started: make(chan time.Time, 1), truncs: make(chan dxTruncate, 1)}
	srv := httptest.NewServer(prov)
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"

	clk := newDxClock()
	_, errs, ctx := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.RealtimeCapabilities, clk, dxSilenceTimeout)

	if err := awaitSilenceRun(t, clk, errs, ctx); !errors.Is(err, voice.ErrCallerSilence) {
		t.Fatalf("run returned %v, want voice.ErrCallerSilence", err)
	}
}

// TestDuplexLiveCallerSilenceEnds checks (j) for live_ws: the run ends with
// voice.ErrCallerSilence and the Live fake receives session.close.
func TestDuplexLiveCallerSilenceEnds(t *testing.T) {
	prov := &dxLive{
		t:       t,
		closeEv: make(chan struct{}, 1),
		turnEnd: make(chan int, 4),
		speech:  make(chan time.Time, 4),
	}
	srv := httptest.NewServer(prov)
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"

	clk := newDxClock()
	_, errs, ctx := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.LiveCapabilities, clk, dxSilenceTimeout)

	if err := awaitSilenceRun(t, clk, errs, ctx); !errors.Is(err, voice.ErrCallerSilence) {
		t.Fatalf("run returned %v, want voice.ErrCallerSilence", err)
	}
	select {
	case <-prov.closeEv:
	case <-ctx.Done():
		t.Fatal("Live fake never received session.close")
	}
}
