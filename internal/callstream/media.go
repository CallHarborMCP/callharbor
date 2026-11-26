package callstream

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/voice"
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
	ReasonCallerSilence      = "caller_silence"
)

// SilenceTimeout is how long a call may go with neither caller speech nor
// agent playout before CallHarbor ends it. Production uses this constant.
const SilenceTimeout = 30 * time.Second

// ErrCallUnavailable reports a stream for a call that is not accepting media.
// The caller sees only that the stream is refused, never which check failed.
var ErrCallUnavailable = errors.New("callstream: call is not accepting media")

// BindControlID checks that a media start frame belongs to the call. When the
// dial response recorded a control id, the stream must carry that exact id.
//
// When no control id is recorded yet, the start frame may bind only if its
// client_state names this call: the dial sent the call id as client_state, and
// Telnyx returns it on the stream. Anything else is refused, so an unrelated
// stream can never attach to a call.
func BindControlID(callID, recorded, frameControlID, frameClientState string) error {
	if recorded != "" {
		if frameControlID != recorded {
			return fmt.Errorf("%w: stream call_control_id does not match the call", ErrCallUnavailable)
		}
		return nil
	}
	if frameControlID == "" || clientStateOf(frameClientState) != callID {
		return fmt.Errorf("%w: no recorded control id and the stream does not name this call", ErrCallUnavailable)
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
func Run(ctx context.Context, sess realtime.Session, bridge voice.AudioBridge, sink voice.EventSink, opts voice.Options, maxDur time.Duration) (res voice.Result, capped bool, err error) {
	if maxDur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxDur)
		defer cancel()
		res, err = voice.RunWith(ctx, sess, bridge, sink, opts)
		return res, errors.Is(ctx.Err(), context.DeadlineExceeded), err
	}
	res, err = voice.RunWith(ctx, sess, bridge, sink, opts)
	return res, false, err
}

// clientStateOf decodes the client_state the dial sent: the call id, base64
// encoded by the Telnyx client. A missing or malformed value names no call.
func clientStateOf(b64 string) string {
	if b64 == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	return string(raw)
}
