// Package voice runs one AI voice call over a protocol-neutral realtime.Session
// and a transport-neutral AudioBridge. It sees only normalized events, so it
// has no knowledge of Telnyx media or provider JSON.
package voice

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
	// Interrupt drops queued, unplayed model audio on caller barge-in.
	Interrupt(ctx context.Context) error
}

// EventSink receives every normalized event, including the ones the engine
// acts on, so callers can record transcripts, usage and errors.
type EventSink func(realtime.Event)

// ErrProviderError reports a provider error event that ended the call.
var ErrProviderError = errors.New("voice: provider error")

// Result is the outcome of a completed call run.
type Result struct {
	// Closed is true only when the provider sent its documented close event.
	// A disconnect is never reported as a clean close.
	Closed bool
}

// Run drives the session until the provider closes, reports an error, the
// connection drops, or ctx is cancelled. The session is always closed before
// Run returns, and the uplink goroutine is always joined.
func Run(ctx context.Context, sess realtime.Session, bridge AudioBridge, sink EventSink) (Result, error) {
	if sess == nil || bridge == nil {
		return Result{}, errors.New("voice: session and bridge are required")
	}
	if sink == nil {
		sink = func(realtime.Event) {}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var uplinkWG sync.WaitGroup
	var upErr error
	uplinkWG.Add(1)
	go func() {
		defer uplinkWG.Done()
		upErr = pumpUplink(runCtx, sess, bridge)
		if upErr != nil {
			cancel()
		}
	}()

	res, downErr := pumpDownlink(runCtx, sess, bridge, sink)

	cancel()
	closeErr := sess.Close()
	uplinkWG.Wait()

	if upErr != nil && !errors.Is(upErr, context.Canceled) {
		return res, fmt.Errorf("voice: uplink: %w", upErr)
	}
	if downErr != nil {
		return res, downErr
	}
	if closeErr != nil && !res.Closed {
		return res, fmt.Errorf("voice: close session: %w", closeErr)
	}
	return res, nil
}

func pumpUplink(ctx context.Context, sess realtime.Session, bridge AudioBridge) error {
	for {
		chunk, err := bridge.NextCallerAudio(ctx)
		if err != nil {
			return err
		}
		if err := sess.AppendAudio(ctx, chunk); err != nil {
			return realtime.Transport(err)
		}
	}
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
			if err := bridge.Interrupt(ctx); err != nil {
				return Result{}, err
			}
		case realtime.EventError:
			return Result{}, fmt.Errorf("%w: %s %s", ErrProviderError, ev.ErrCode, ev.ErrMessage)
		case realtime.EventClosed:
			return Result{Closed: true}, nil
		}
	}
}
