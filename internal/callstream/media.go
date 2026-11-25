package callstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/voice"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

// Reason codes for a call CallHarbor ends or fails on the media path. Public
// strings. The local-hangup reasons go in outcome_reason_code, as the failure
// reasons do.
const (
	ReasonCapabilityMismatch = "capability_mismatch"
	ReasonAIDialFailed       = "ai_dial_failed"
	ReasonBriefTooLarge      = "brief_too_large"
	ReasonMaxDuration        = "max_duration"
	ReasonMediaFailed        = "media_failed"
	ReasonAISessionClosed    = "ai_session_closed"
)

// ErrCallUnavailable reports a stream for a call that is not accepting media.
// The caller sees only that the stream is refused, never which check failed.
var ErrCallUnavailable = errors.New("callstream: call is not accepting media")

// BindControlID checks that a media start frame belongs to the call. The
// stream must carry the call_control_id the dial response recorded. Until
// that is recorded the call has no control id, so the stream is refused and
// Telnyx may retry it.
func BindControlID(recorded, frameControlID string) error {
	if recorded == "" {
		return ErrCallUnavailable
	}
	if frameControlID != recorded {
		return fmt.Errorf("%w: stream call_control_id does not match the call", ErrCallUnavailable)
	}
	return nil
}

// Dial opens the AI session for one call with the call's resolved profile.
// Tests replace the realtime adapters through realtime.Dialers.
type Dial func(ctx context.Context, prof realtime.Profile, need realtime.Requirements) (realtime.Session, error)

// Run drives one call's voice session until the session ends or the max
// duration timer fires. voice.Run always closes the session. The timer is a
// server-side cap: it cancels the run, so a call never outlives its limit.
//
// capped reports whether the cap ended the run. It is the only way to tell a
// cap from a caller stop, since voice.Run returns a nil error for a clean
// provider close. The caller uses it to choose the hangup reason.
func Run(ctx context.Context, sess realtime.Session, bridge voice.AudioBridge, sink voice.EventSink, maxDur time.Duration) (res voice.Result, capped bool, err error) {
	if maxDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxDur)
		defer cancel()
		res, err = voice.Run(ctx, sess, bridge, sink)
		return res, errors.Is(ctx.Err(), context.DeadlineExceeded), err
	}
	res, err = voice.Run(ctx, sess, bridge, sink)
	return res, false, err
}

// Resolve returns the AI profile for a call and the session requirements for
// the stream's format. It fails with the voicecall error, so the caller can map
// a missing key or profile to a visible reason.
func Resolve(ctx context.Context, r *voicecall.Resolver, ownerUserID, aiProfileID string) (realtime.Profile, error) {
	prof, err := r.Resolve(ctx, ownerUserID, aiProfileID)
	if err != nil {
		return realtime.Profile{}, err
	}
	return prof, nil
}
