package callstream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A session holds its call's slot until its release runs. A second stream for
// the same call is refused until then (m1).
func TestSessionOnePerCallUntilReleased(t *testing.T) {
	var s sessions
	release, err := s.claim("call_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim("call_1"); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("second claim = %v, want ErrSessionActive", err)
	}
	if _, err := s.claim("call_2"); err != nil {
		t.Fatalf("other call claim = %v", err)
	}
	release()
	release() // a second release is a no-op, not a negative counter
	if _, err := s.claim("call_1"); err != nil {
		t.Fatalf("claim after release = %v", err)
	}
}

// Shutdown refuses new work at once and cancels the running sessions. It then
// waits for them to release, so no session outlives the wait (M2).
func TestStopCancelsSessionsAndRefusesNewWork(t *testing.T) {
	var s sessions
	release, err := s.claim("call_1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.bindCancel("call_1", cancel)

	var finished atomic.Bool
	go func() {
		<-ctx.Done()
		finished.Store(true)
		release()
	}()

	s.cancelAll()
	if !s.wait(2 * time.Second) {
		t.Fatal("sessions did not release after cancel")
	}
	if !finished.Load() {
		t.Fatal("the running session was not cancelled")
	}
	if _, err := s.enter(); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("enter after stop = %v, want ErrShuttingDown", err)
	}
	if _, err := s.claim("call_2"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("claim after stop = %v, want ErrShuttingDown", err)
	}
}

// wait returns false when a session never releases, so the caller knows the
// store must not be closed under it. It must not block past the bound.
func TestWaitIsBoundedWhenSessionNeverReleases(t *testing.T) {
	var s sessions
	if _, err := s.claim("stuck"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if s.wait(50 * time.Millisecond) {
		t.Fatal("wait reported a stuck session as finished")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait took %v past its bound", elapsed)
	}
}

// A stream in setup counts as in flight: shutdown waits for it, not only for
// sessions that already started.
func TestSetupCountsAsInFlightUntilExit(t *testing.T) {
	var s sessions
	exit, err := s.enter()
	if err != nil {
		t.Fatal(err)
	}
	s.cancelAll()
	if s.wait(50 * time.Millisecond) {
		t.Fatal("wait returned while a stream was still in setup")
	}
	exit()
	exit() // idempotent
	if !s.wait(2 * time.Second) {
		t.Fatal("wait did not return after setup exited")
	}
}

// A tracked connection is cancelled by shutdown even when no start frame has
// arrived, and shutdown waits for it to finish (M2). After shutdown, a new
// connection is refused before any upgrade.
func TestStopCancelsTrackedConnectionWithoutSession(t *testing.T) {
	var s sessions
	ctx, done, err := s.track(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		<-ctx.Done() // the frame loop ends when its context is cancelled
		done()
		done() // a second done is a no-op
		close(finished)
	}()
	s.cancelAll()
	if !s.wait(2 * time.Second) {
		t.Fatal("wait returned before the tracked connection finished")
	}
	<-finished
	if _, _, err := s.track(context.Background()); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("track after stop = %v, want ErrShuttingDown", err)
	}
}
