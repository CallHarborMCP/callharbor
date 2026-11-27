package callstream

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrSessionActive reports a second stream for a call that already has one.
// A call has at most one media session, so the second is refused.
var ErrSessionActive = errors.New("callstream: call already has an active media session")

// ErrShuttingDown reports a stream that arrived after shutdown began. No new
// session or sink setup starts once shutdown has begun.
var ErrShuttingDown = errors.New("callstream: media is shutting down")

// sessions tracks the media work in this process. It serves two purposes. It
// allows one active session per call id (m1). And it lets shutdown stop new
// work, cancel the running sessions and wait for them to finish before the
// store closes (M2).
//
// The wait group counts two things: each in-flight stream setup (enter) and
// each running session (claim). claim is only called while an enter hold is
// active, so the counter is never zero when a session is added.
type sessions struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
	// conns holds the cancel func of every accepted media connection, from the
	// upgrade until its frame loop returns. http.Server.Shutdown does not track
	// hijacked WebSocket connections, so shutdown cancels them here.
	conns  map[uint64]context.CancelFunc
	nextID uint64
	wg     sync.WaitGroup
	closed bool
	// stopped marks calls whose session must end on bind: an explicit hangup
	// arrived before the session bound its cancel. An entry is removed when
	// the claim is released.
	stopped map[string]struct{}
}

// track registers one media connection for its whole life. The returned
// context is cancelled by shutdown, which ends the connection's frame loop.
// done must run exactly once when the connection's handler returns.
func (s *sessions) track(parent context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrShuttingDown
	}
	ctx, cancel := context.WithCancel(parent)
	if s.conns == nil {
		s.conns = map[uint64]context.CancelFunc{}
	}
	s.nextID++
	id := s.nextID
	s.conns[id] = cancel
	s.wg.Add(1)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			s.mu.Lock()
			delete(s.conns, id)
			s.mu.Unlock()
			s.wg.Done()
		})
	}, nil
}

// enter registers one in-flight stream setup. It refuses once shutdown has
// begun. The returned func must run exactly once when the setup returns.
func (s *sessions) enter() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrShuttingDown
	}
	s.wg.Add(1)
	var once sync.Once
	return func() { once.Do(s.wg.Done) }, nil
}

// claim reserves the call's session slot and returns its release func. The
// release must run exactly once when the session ends or never starts.
func (s *sessions) claim(callID string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrShuttingDown
	}
	if _, busy := s.active[callID]; busy {
		return nil, ErrSessionActive
	}
	if s.active == nil {
		s.active = map[string]context.CancelFunc{}
	}
	if s.stopped == nil {
		s.stopped = map[string]struct{}{}
	}
	s.active[callID] = func() {}
	s.wg.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.active, callID)
			delete(s.stopped, callID)
			s.mu.Unlock()
			s.wg.Done()
		})
	}, nil
}

// bindCancel records the cancel func of the session that holds callID's claim,
// so shutdown can stop it. If shutdown already began, the session is cancelled
// at once.
func (s *sessions) bindCancel(callID string, cancel context.CancelFunc) {
	s.mu.Lock()
	if _, ok := s.active[callID]; ok {
		s.active[callID] = cancel
	}
	_, stopped := s.stopped[callID]
	closed := s.closed
	s.mu.Unlock()
	if closed || stopped {
		cancel()
	}
}

// stopping reports whether shutdown has begun. A session that ends during
// shutdown does not send its own local hangup: shutdown hangs up every live leg.
func (s *sessions) stopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// cancelCall cancels the running session for callID. A session whose cancel is
// not yet bound is marked, so bindCancel cancels it as soon as it is bound.
func (s *sessions) cancelCall(callID string) {
	s.mu.Lock()
	c, ok := s.active[callID]
	if ok {
		s.stopped[callID] = struct{}{}
	}
	s.mu.Unlock()
	if ok {
		c()
	}
	// c is the placeholder no-op until bindCancel runs. The stopped mark makes
	// bindCancel cancel the real session when it binds.
}

// cancelAll refuses new work and cancels every running session. It returns at
// once; use wait to see the work finish.
func (s *sessions) cancelAll() {
	s.mu.Lock()
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.active)+len(s.conns))
	for _, c := range s.active {
		cancels = append(cancels, c)
	}
	for _, c := range s.conns {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// wait blocks until all setups and sessions have released, or until timeout.
// It reports whether they all finished. On false the caller must not close the
// store, because work may still be running.
func (s *sessions) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// StopMedia refuses new media streams and cancels every running session. It
// returns at once. Shutdown calls it first, so runs end before HTTP drains.
func (m *Media) StopMedia() {
	m.sessions.cancelAll()
}

// StopCall cancels the media session for one call, if one is running, so its AI
// session closes after an explicit hangup. It does not refuse other calls and
// does not end the leg: the caller ends the leg with the owner's key.
func (m *Media) StopCall(callID string) {
	m.sessions.cancelCall(callID)
}

// RefuseOrphanHangups stops the webhook set from starting new orphan hangups.
// Shutdown calls it with StopMedia.
func (m *Media) RefuseOrphanHangups() {
	if m.Webhooks != nil {
		m.Webhooks.Refuse()
	}
}

// WaitOrphanHangups waits, up to ctx, for orphan hangups already running. It
// returns nil when none are left. Shutdown calls it before the store closes.
func (m *Media) WaitOrphanHangups(ctx context.Context) error {
	if m.Webhooks == nil {
		return nil
	}
	return m.Webhooks.Wait(ctx)
}

// WaitMedia waits up to timeout for every stream setup and session to release.
// It reports whether they all finished. Shutdown calls it before the store
// closes, so no media goroutine touches a closed store.
func (m *Media) WaitMedia(timeout time.Duration) bool {
	return m.sessions.wait(timeout)
}
