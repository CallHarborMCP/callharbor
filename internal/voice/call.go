// Package voice runs one AI voice call over a protocol-neutral realtime.Session
// and a transport-neutral AudioBridge. It sees only normalized events, so it
// has no knowledge of Telnyx media or provider JSON.
package voice

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// AudioBridge carries caller audio in and model audio out for one leg. It is
// implemented by the telephony media layer. Every method must return promptly
// once ctx is cancelled.
type AudioBridge interface {
	// NextCallerAudio blocks until the next caller audio chunk is available.
	NextCallerAudio(ctx context.Context) ([]byte, error)
	// Play queues model audio for playback to the caller.
	Play(ctx context.Context, pcm []byte) error
	// Interrupt drops queued, unplayed model audio on caller barge-in and
	// reports what was cut.
	Interrupt(ctx context.Context) (Cut, error)
}

// Cut describes an interruption of model audio. It is provider-neutral.
type Cut struct {
	// ItemID is the provider output item that was cut; empty when the audio
	// carried no item identity (GPT-Live).
	ItemID       string
	ContentIndex int
	// Played is the estimated audible duration of the cut item.
	Played time.Duration
	// Sent is the duration of the cut item that was handed to the telephony
	// leg. A truncate is due only when Played is strictly less than Sent.
	Sent time.Duration
	// Audible is true when model audio was still playing at the cut.
	Audible bool
}

// SpeechEvent is a caller speech edge observed by the media layer's local
// detector. At is the wall time of the edge.
type SpeechEvent struct {
	Started bool
	At      time.Time
	// Turn and Audible are decided by the transport with the edge (see
	// telnyx.SpeechEvent). Audible is meaningful on a Start edge only.
	Turn    uint64
	Audible bool
}

// SpeechBridge is implemented by bridges that report caller speech edges from
// a local detector. The engine uses the edges for silence accounting always,
// and for barge-in only when the protocol has no native speech-start event.
type SpeechBridge interface {
	// CallerSpeech returns a bounded channel of caller speech edges. Edges are
	// dropped, never blocked on, when the reader lags.
	CallerSpeech() <-chan SpeechEvent
}

// TurnInterrupter is implemented by bridges that can cut agent audio only while
// the caller is still in the turn named by a Start edge. The transport decides
// the check and the cut atomically. A refused cut (ok false) cuts nothing and
// sends nothing. Realtime's native speech-start path does not use it.
type TurnInterrupter interface {
	InterruptTurn(ctx context.Context, turn uint64) (Cut, bool, error)
}

// PlayoutClock is implemented by bridges that can say when queued agent audio
// finishes playing at real-time rate. A time at or before now means silence.
type PlayoutClock interface {
	PlayoutEnd() time.Time
}

// Truncater is implemented by sessions whose protocol can report how much of
// an output item was played, so the server drops unheard output from context.
type Truncater interface {
	Truncate(ctx context.Context, itemID string, contentIndex int, audioEndMS int64) error
}

// TaskAcker is implemented by sessions whose protocol can acknowledge a
// voice-session task report. The acknowledgement is a function output only: it
// must never start a response, so the call's conversation is not changed.
//
// status is TaskAckRecorded for a usable report and TaskAckInvalid for one
// that was received but could not be used.
type TaskAcker interface {
	AckTaskReport(ctx context.Context, callID, status string) error
}

// Task report acknowledgement statuses, sent as the function output.
const (
	TaskAckRecorded = "recorded"
	TaskAckInvalid  = "invalid"
)

// EventSink receives every normalized event, including the ones the engine
// acts on, so callers can record transcripts, usage and errors.
type EventSink func(realtime.Event)

// ErrProviderError reports a provider error event that ended the call.
var ErrProviderError = errors.New("voice: provider error")

// ErrCallerSilence reports that the run ended because neither the caller nor
// agent audio was active for Options.SilenceTimeout. The session was closed
// through the same path as a local stop.
var ErrCallerSilence = errors.New("voice: caller silence timeout")

// GracefulCloseTimeout bounds how long a locally stopped call waits for the
// provider's close acknowledgement before dropping the socket.
const GracefulCloseTimeout = 5 * time.Second

// Closer is implemented by sessions whose protocol has a documented close
// handshake. RequestClose sends the close without reading, so the downlink
// reader observes the acknowledgement.
type Closer interface {
	RequestClose(ctx context.Context) error
}

// Result is the outcome of a completed call run.
type Result struct {
	// Closed is true only when the provider sent its documented close event.
	// A disconnect is never reported as a clean close.
	Closed bool
	// UsageSeconds is the provider-confirmed usage from the close event.
	UsageSeconds float64
	// UsageConfirmed is true only when UsageSeconds came from a close event.
	UsageConfirmed bool
}

// Options configures the engine beyond the session and bridge.
type Options struct {
	// Caps is the protocol capability matrix. When its SpeechStartEvents is
	// not Native, the local detector drives barge-in and turn reopening.
	Caps realtime.Capabilities
	// SilenceTimeout ends the run after this long with no caller speech and no
	// agent playout. Zero disables the watchdog.
	SilenceTimeout time.Duration
	// Now is the clock for silence accounting. Nil means time.Now.
	Now func() time.Time
}

// Run drives the session with default options: provider speech-start events
// barge in, and no silence watchdog. See RunWith.
func Run(ctx context.Context, sess realtime.Session, bridge AudioBridge, sink EventSink) (Result, error) {
	return RunWith(ctx, sess, bridge, sink, Options{})
}

// RunWith drives the session until the provider closes, reports an error, the
// connection drops, the silence watchdog fires, or ctx is cancelled. The
// session is always closed before RunWith returns, and every goroutine it
// starts is joined.
func RunWith(ctx context.Context, sess realtime.Session, bridge AudioBridge, sink EventSink, opts Options) (Result, error) {
	if sess == nil || bridge == nil {
		return Result{}, errors.New("voice: session and bridge are required")
	}
	if opts.SilenceTimeout < 0 {
		return Result{}, fmt.Errorf("voice: negative silence timeout %v", opts.SilenceTimeout)
	}
	if sink == nil {
		sink = func(realtime.Event) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	// stopCtx is the caller side: it ends on the caller's cancellation or on a
	// local stop from the silence watchdog. runCtx also ends on any failure.
	stopCtx, stopLocal := context.WithCancel(ctx)
	defer stopLocal()
	runCtx, cancel := context.WithCancel(stopCtx)
	defer cancel()

	var clock PlayoutClock
	if pc, ok := bridge.(PlayoutClock); ok {
		clock = pc
	}
	act := newActivity(opts.Now(), clock)
	var speechErr firstError
	speechErr.cancel = cancel
	var silenced bool
	var silencedMu sync.Mutex

	localBarge := opts.Caps.SpeechStartEvents != realtime.Native
	ti, canTurnCut := bridge.(TurnInterrupter)
	sb, hasSpeech := bridge.(SpeechBridge)
	// Local barge-in is driven by caller speech edges, so it can only occur
	// with a SpeechBridge. Such a bridge without a TurnInterrupter would fail at
	// the first barge-in; reject it here instead.
	if localBarge && hasSpeech && !canTurnCut {
		// Fail at setup, not at the first barge-in. The session is closed on
		// every return, so the failed setup does not leak the connection.
		return Result{}, errors.Join(errors.New("voice: local barge-in requires a turn-scoped interrupt"), sess.Close())
	}

	// Everything that writes to the session (the uplink and the turn-cut speech
	// path) runs under sessCtx and sessWG, so a local stop can stop and join all
	// of it before the protocol close is requested. Nothing may write to the
	// session concurrently with RequestClose.
	sessCtx, stopSess := context.WithCancel(runCtx)
	defer stopSess()
	var sessWG sync.WaitGroup
	var upErr error
	sessWG.Add(1)
	go func() {
		defer sessWG.Done()
		err := pumpUplink(sessCtx, sess, bridge)
		// Cancellation is the normal end of the uplink, and an uplink failure
		// after a local stop is a consequence of the stop, not a fault.
		if err == nil || errors.Is(err, context.Canceled) || stopCtx.Err() != nil {
			return
		}
		upErr = err
		cancel()
	}()
	joinSession := func() {
		stopSess()
		sessWG.Wait()
	}

	var bgWG sync.WaitGroup
	if hasSpeech {
		sessWG.Add(1)
		go func() {
			defer sessWG.Done()
			pumpSpeech(sessCtx, sb, ti, sess, opts, act, localBarge, &speechErr)
		}()
	}
	if opts.SilenceTimeout > 0 {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			watchSilence(runCtx, opts, act, func() {
				silencedMu.Lock()
				silenced = true
				silencedMu.Unlock()
				stopLocal()
			})
		}()
	}

	res, downErr := runDownlink(runCtx, stopCtx, sess, bridge, sink, joinSession)

	cancel()
	joinSession()
	bgWG.Wait()

	closeErr := sess.Close()

	silencedMu.Lock()
	wasSilenced := silenced
	silencedMu.Unlock()

	// A local stop (silence watchdog) reports its own reason first. Uplink
	// errors that follow a stop are not reported; see the uplink goroutine.
	if wasSilenced {
		return res, ErrCallerSilence
	}
	if upErr != nil {
		return res, fmt.Errorf("voice: uplink: %w", upErr)
	}
	if err := speechErr.get(); err != nil {
		return res, fmt.Errorf("voice: caller speech: %w", err)
	}
	if downErr != nil {
		return res, downErr
	}
	if closeErr != nil && !res.Closed {
		return res, fmt.Errorf("voice: close session: %w", closeErr)
	}
	return res, nil
}

// runDownlink reads events until the provider closes, fails, or the run stops.
// On a caller-side stop with a healthy provider it requests the protocol close
// and keeps reading until session.closed arrives or GracefulCloseTimeout
// passes. Reads run on a context detached from cancellation, because cancelling
// a socket read closes the connection and would prevent the graceful close.
func runDownlink(runCtx, callerCtx context.Context, sess realtime.Session, bridge AudioBridge, sink EventSink, joinSession func()) (Result, error) {
	readCtx, stopRead := context.WithCancel(context.WithoutCancel(runCtx))
	defer stopRead()
	type step struct {
		res Result
		err error
	}
	done := make(chan step, 1)
	go func() {
		res, err := pumpDownlink(readCtx, sess, bridge, sink)
		done <- step{res, err}
	}()
	select {
	case s := <-done:
		return s.res, s.err
	case <-runCtx.Done():
	}
	// Stop and join the session writers (uplink and turn-cut path) before any
	// close is requested, so no caller audio is appended after session.close and
	// no write is in flight when the close handshake starts.
	joinSession()
	if callerCtx.Err() == nil {
		stopRead()
		<-done
		return Result{}, runCtx.Err()
	}
	rc, ok := sess.(Closer)
	if !ok {
		stopRead()
		<-done
		return Result{}, callerCtx.Err()
	}
	reqCtx, reqCancel := context.WithTimeout(context.Background(), GracefulCloseTimeout)
	defer reqCancel()
	if err := rc.RequestClose(reqCtx); err != nil {
		stopRead()
		<-done
		return Result{}, callerCtx.Err()
	}
	timer := time.NewTimer(GracefulCloseTimeout)
	defer timer.Stop()
	select {
	case s := <-done:
		if s.res.Closed {
			return s.res, nil
		}
		return Result{}, callerCtx.Err()
	case <-timer.C:
		stopRead()
		<-done
		return Result{}, callerCtx.Err()
	}
}

// uplinkWriteTimeout bounds one caller-audio append. The write deliberately
// does not inherit cancellation: coder/websocket closes the whole connection
// when a write's context ends mid-frame, which would make a local stop unable
// to complete the protocol close.
const uplinkWriteTimeout = 5 * time.Second

// pumpUplink forwards caller audio to the provider until ctx ends. A chunk is
// appended only while ctx is live, so nothing is appended after a stop.
func pumpUplink(ctx context.Context, sess realtime.Session, bridge AudioBridge) error {
	for {
		chunk, err := bridge.NextCallerAudio(ctx)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uplinkWriteTimeout)
		err = sess.AppendAudio(wctx, chunk)
		cancel()
		if err != nil {
			return realtime.Transport(err)
		}
	}
}

// activity tracks caller speech and agent playout for silence accounting. It
// is shared by the speech and watchdog goroutines.
type activity struct {
	mu       sync.Mutex
	speaking bool
	lastEdge time.Time
	// lastPlayed is the latest playout end observed to be in the past. Agent
	// audio that finished at lastPlayed was the last agent activity, so silence
	// is measured from there, not from the caller's edge alone.
	lastPlayed time.Time
	clock      PlayoutClock
}

func newActivity(start time.Time, clock PlayoutClock) *activity {
	return &activity{lastEdge: start, lastPlayed: start, clock: clock}
}

func (a *activity) onSpeech(ev SpeechEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Before the caller's edge takes effect, record any agent audio that had
	// already finished by this edge. A reply that ended before the caller spoke
	// again is then never forgotten, whatever the watchdog's poll cadence.
	if a.clock != nil {
		if end := a.clock.PlayoutEnd(); end.Before(ev.At) && end.After(a.lastPlayed) {
			a.lastPlayed = end
		}
	}
	a.speaking = ev.Started
	a.lastEdge = ev.At
}

// audible reports whether queued agent audio is still playing at now.
func (a *activity) audible(now time.Time) bool {
	return a.clock != nil && a.clock.PlayoutEnd().After(now)
}

// silentFor returns how long neither the caller nor the agent has been
// active. It is zero while the caller is speaking or agent audio is playing.
// Otherwise the silence runs from the later of the caller's last edge and the
// moment queued agent audio last finished, so a long reply is not counted as
// silence and a pause after it is measured from the end of the reply.
func (a *activity) silentFor(now time.Time) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.speaking || a.audible(now) {
		return 0
	}
	if a.clock != nil {
		if end := a.clock.PlayoutEnd(); end.Before(now) && end.After(a.lastPlayed) {
			a.lastPlayed = end
		}
	}
	since := a.lastEdge
	if a.lastPlayed.After(since) {
		since = a.lastPlayed
	}
	if d := now.Sub(since); d > 0 {
		return d
	}
	return 0
}

// pumpSpeech consumes caller speech edges. Every edge updates silence
// accounting. With localBarge (no native speech-start event) an onset cuts
// audible agent audio. Reopening the anonymous turn on an end edge is not done
// here: the transport does it synchronously in the media path, before the
// provider can reply to the caller's turn. An onset while the agent is silent
// is an ordinary turn and cuts nothing.
func pumpSpeech(ctx context.Context, sb SpeechBridge, ti TurnInterrupter, sess realtime.Session, opts Options, act *activity, localBarge bool, fail *firstError) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-sb.CallerSpeech():
			act.onSpeech(ev)
			if !localBarge || !ev.Started || !ev.Audible {
				continue
			}
			// The transport decided, with the edge, that agent audio was audible.
			// The cut is still conditional on the caller remaining in this turn:
			// a Start edge that was delayed past its own End is refused there, so
			// a reply that began after the caller stopped is never cut. RunWith
			// has already rejected local barge-in without a TurnInterrupter.
			cut, ok, err := ti.InterruptTurn(ctx, ev.Turn)
			if err != nil {
				if ctx.Err() == nil {
					fail.set(err)
				}
				return
			}
			if ok {
				// The truncate is a session write: detach it from a stop so a
				// cancelled write cannot close the socket before session.close.
				wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uplinkWriteTimeout)
				err := truncateCut(wctx, sess, cut)
				cancel()
				if err != nil && ctx.Err() == nil {
					fail.set(err)
					return
				}
			}
		}
	}
}

// watchSilence fires once the caller has been silent for SilenceTimeout. It
// polls at a quarter of the timeout (at most once a second), so a stop lands
// within one poll of the deadline.
func watchSilence(ctx context.Context, opts Options, act *activity, fire func()) {
	period := opts.SilenceTimeout / 4
	if period > time.Second {
		period = time.Second
	}
	if period <= 0 {
		period = time.Millisecond
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if act.silentFor(opts.Now()) >= opts.SilenceTimeout {
				fire()
				return
			}
		}
	}
}

// firstError records the first failure from a background goroutine and cancels
// the run so the failure ends it.
type firstError struct {
	mu     sync.Mutex
	err    error
	cancel context.CancelFunc
}

func (f *firstError) set(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
	}
}

func (f *firstError) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func pumpDownlink(ctx context.Context, sess realtime.Session, bridge AudioBridge, sink EventSink) (Result, error) {
	for {
		ev, err := sess.Next(ctx)
		if err != nil {
			return Result{}, err
		}
		sink(ev)
		switch ev.Kind {
		case realtime.EventAudio:
			if err := bridge.Play(ctx, ev.Audio); err != nil {
				return Result{}, err
			}
		case realtime.EventSpeechStarted:
			if err := bargeIn(ctx, sess, bridge); err != nil {
				return Result{}, err
			}
		case realtime.EventError:
			return Result{}, fmt.Errorf("%w: %s %s", ErrProviderError, ev.ErrCode, ev.ErrMessage)
		case realtime.EventClosed:
			return Result{Closed: true, UsageSeconds: ev.UsageSeconds, UsageConfirmed: ev.UsageSeconds > 0}, nil
		case realtime.EventTaskReport:
			// The report is recorded by the sink above. It is acknowledged here,
			// without response.create. An unusable report has no call id to ack,
			// and an ack failure is not a call failure: the report is still stored.
			ackTaskReport(ctx, sess, ev)
		}
	}
}

// ackTaskReport acknowledges a task report on sessions that support it. A
// report with a usable call id is acknowledged as recorded, or as invalid when
// it could not be used, so the model's function call never waits for output. A
// session without TaskAcker, or a report with no usable call id, sends nothing.
// A failed ack is ignored here: the protocol error, if any, surfaces on the next
// read, and the report itself was already handled.
func ackTaskReport(ctx context.Context, sess realtime.Session, ev realtime.Event) {
	acker, ok := sess.(TaskAcker)
	if !ok || ev.FunctionCallID == "" {
		return
	}
	status := TaskAckRecorded
	if ev.ReportInvalid {
		status = TaskAckInvalid
	}
	_ = acker.AckTaskReport(ctx, ev.FunctionCallID, status)
}

// bargeIn cuts model audio on a caller barge-in and, when the cut item was
// really interrupted, reports the played position to a session that can
// truncate. response.cancel is never sent: server_vad with interrupt_response
// already cancels the active response, and a cancel with no active response
// returns an error event that would end the call.
func bargeIn(ctx context.Context, sess realtime.Session, bridge AudioBridge) error {
	cut, err := bridge.Interrupt(ctx)
	if err != nil {
		return err
	}
	return truncateCut(ctx, sess, cut)
}

// truncateCut reports the played position of a cut item to the provider.
// Truncate is sent only when the item has an ID, audio was audible, and the
// caller heard less than the whole item. audio_end_ms is floor(played), and
// Played never exceeds Sent because the ledger caps it at the sent duration.
func truncateCut(ctx context.Context, sess realtime.Session, cut Cut) error {
	if cut.ItemID == "" || !cut.Audible || cut.Played >= cut.Sent {
		return nil
	}
	tr, ok := sess.(Truncater)
	if !ok {
		return nil
	}
	ms := cut.Played.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return tr.Truncate(ctx, cut.ItemID, cut.ContentIndex, ms)
}
