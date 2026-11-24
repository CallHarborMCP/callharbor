package voice

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

type fakeSession struct {
	events    chan realtime.Event
	closed    chan struct{}
	closeOnce sync.Once
	appendMu  sync.Mutex
	appended  [][]byte
	appendErr error
}

func newFakeSession() *fakeSession {
	return &fakeSession{events: make(chan realtime.Event, 16), closed: make(chan struct{})}
}

func (f *fakeSession) AppendAudio(ctx context.Context, b []byte) error {
	f.appendMu.Lock()
	defer f.appendMu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, append([]byte(nil), b...))
	return nil
}

func (f *fakeSession) Next(ctx context.Context) (realtime.Event, error) {
	select {
	case ev := <-f.events:
		return ev, nil
	case <-ctx.Done():
		return realtime.Event{}, ctx.Err()
	case <-f.closed:
		return realtime.Event{}, realtime.ErrDisconnected
	}
}

func (f *fakeSession) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

type fakeBridge struct {
	mu     sync.Mutex
	caller chan []byte
	played [][]byte
	barges int
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{caller: make(chan []byte, 16)}
}

func (b *fakeBridge) NextCallerAudio(ctx context.Context) ([]byte, error) {
	select {
	case c := <-b.caller:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *fakeBridge) Play(ctx context.Context, pcm []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.played = append(b.played, append([]byte(nil), pcm...))
	return nil
}

func (b *fakeBridge) Interrupt(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.barges++
	return nil
}

func TestRunClosesCleanlyOnProviderClose(t *testing.T) {
	sess := newFakeSession()
	br := newFakeBridge()
	sess.events <- realtime.Event{Kind: realtime.EventAudio, Audio: []byte{1, 2}}
	sess.events <- realtime.Event{Kind: realtime.EventSpeechStarted}
	sess.events <- realtime.Event{Kind: realtime.EventClosed, CloseReason: "done"}

	var kinds []realtime.EventKind
	res, err := Run(context.Background(), sess, br, func(ev realtime.Event) { kinds = append(kinds, ev.Kind) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Closed {
		t.Fatal("Closed = false, want true on documented close event")
	}
	if len(br.played) != 1 || br.barges != 1 {
		t.Fatalf("played=%d barges=%d, want 1 and 1", len(br.played), br.barges)
	}
	if len(kinds) != 3 {
		t.Fatalf("sink saw %d events, want 3", len(kinds))
	}
}

func TestRunDisconnectIsNotCleanClose(t *testing.T) {
	sess := newFakeSession()
	br := newFakeBridge()
	_ = sess.Close()

	res, err := Run(context.Background(), sess, br, nil)
	if !errors.Is(err, realtime.ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
	if res.Closed {
		t.Fatal("disconnect reported as clean close")
	}
}

func TestRunProviderErrorEndsCall(t *testing.T) {
	sess := newFakeSession()
	br := newFakeBridge()
	sess.events <- realtime.Event{Kind: realtime.EventError, ErrCode: "rate_limit", ErrMessage: "slow down"}

	res, err := Run(context.Background(), sess, br, nil)
	if !errors.Is(err, ErrProviderError) {
		t.Fatalf("err = %v, want ErrProviderError", err)
	}
	if res.Closed {
		t.Fatal("provider error reported as clean close")
	}
}

func TestRunForwardsCallerAudioUntilCancelled(t *testing.T) {
	sess := newFakeSession()
	br := newFakeBridge()
	br.caller <- []byte{9, 9}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	res, err := Run(ctx, sess, br, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if res.Closed {
		t.Fatal("cancelled run reported as clean close")
	}
	sess.appendMu.Lock()
	defer sess.appendMu.Unlock()
	if len(sess.appended) != 1 {
		t.Fatalf("appended %d chunks, want 1", len(sess.appended))
	}
}

type failingBridge struct {
	*fakeBridge
	playErr, bargeErr, nextErr error
}

func (b *failingBridge) Play(ctx context.Context, pcm []byte) error {
	if b.playErr != nil {
		return b.playErr
	}
	return b.fakeBridge.Play(ctx, pcm)
}

func (b *failingBridge) Interrupt(ctx context.Context) error {
	if b.bargeErr != nil {
		return b.bargeErr
	}
	return b.fakeBridge.Interrupt(ctx)
}

func (b *failingBridge) NextCallerAudio(ctx context.Context) ([]byte, error) {
	if b.nextErr != nil {
		return nil, b.nextErr
	}
	return b.fakeBridge.NextCallerAudio(ctx)
}

func TestRunPropagatesBridgeFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]struct {
		setup func(*fakeSession, *failingBridge)
	}{
		"play": {func(s *fakeSession, b *failingBridge) {
			b.playErr = boom
			s.events <- realtime.Event{Kind: realtime.EventAudio, Audio: []byte{1}}
		}},
		"barge-in": {func(s *fakeSession, b *failingBridge) {
			b.bargeErr = boom
			s.events <- realtime.Event{Kind: realtime.EventSpeechStarted}
		}},
		"uplink read": {func(s *fakeSession, b *failingBridge) {
			b.nextErr = boom
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sess := newFakeSession()
			br := &failingBridge{fakeBridge: newFakeBridge()}
			tc.setup(sess, br)
			_, err := Run(context.Background(), sess, br, nil)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
		})
	}
}

func TestRunPropagatesAppendFailure(t *testing.T) {
	sess := newFakeSession()
	sess.appendErr = realtime.ErrDisconnected
	br := newFakeBridge()
	br.caller <- []byte{1}
	_, err := Run(context.Background(), sess, br, nil)
	if !errors.Is(err, realtime.ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
}

func TestRunRejectsNilDependencies(t *testing.T) {
	if _, err := Run(context.Background(), nil, newFakeBridge(), nil); err == nil {
		t.Fatal("nil session accepted")
	}
	if _, err := Run(context.Background(), newFakeSession(), nil, nil); err == nil {
		t.Fatal("nil bridge accepted")
	}
}
