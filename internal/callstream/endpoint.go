package callstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voice"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

// Store is the view of storage the media path needs.
type Store interface {
	LoadCallForMedia(ctx context.Context, callID string) (storage.MediaCall, error)
	BeginLocalHangup(ctx context.Context, callID, reasonCode string, now time.Time) (storage.LiveCallLeg, bool, error)
	BindStreamControlID(ctx context.Context, callID, controlID string, now time.Time) (bool, error)
}

// LegHangup ends one live Telnyx leg with the owner's key. app.go builds it
// from the same resolver shutdown uses, so both paths send the same command id.
type LegHangup func(ctx context.Context, leg storage.LiveCallLeg) error

// WebhookKeys resolves a telephony owner's webhook public key (base64) for one
// secret id. It goes through the secrets service, so the key is decrypted only
// when a verifier is built.
type WebhookKeys func(ctx context.Context, ownerUserID, secretID string) (string, error)

// Media serves per-call media streams. It is built once and shared; each
// stream gets its own telnyx.MediaEndpoint, bound to that call's stream URL.
type Media struct {
	Store    Store
	Tokens   *Tokens
	Resolver *voicecall.Resolver
	Dialers  realtime.Dialers
	// Format is the provider wire format every AI session is dialed with.
	Format   audio.Format
	MaxBytes int
	Now      func() time.Time
	Logger   *slog.Logger
	Keys     WebhookKeys
	// Verifier returns the cached webhook verifier for a public key. The same
	// verifier serves webhooks and media handshakes for one profile key.
	Verifier func(publicKeyB64 string) (*telnyx.Verifier, error)
	// StreamURL builds the exact stream URL the call was dialed with, so the
	// handshake check matches what Telnyx connects to.
	StreamURL func(callID, token string) string
	// Hangup ends the Telnyx leg when CallHarbor ends a call. It is best-effort:
	// a failure is logged and the call stays in ending.
	Hangup LegHangup
	// Webhooks is the webhook set sharing this media service. Its orphan
	// hangups are stopped with the media sessions, so none starts after shutdown.
	Webhooks *Webhooks
	// sessions holds the active media sessions: one per call id (m1), and the
	// set shutdown cancels and waits for before the store closes (M2).
	sessions sessions
}

// Handler serves GET /telnyx/calls/{call_id}/media/{token}. A token that does
// not match the call looks like an unknown call. The stream is authenticated by
// the Telnyx handshake, checked against this exact stream URL.
func (m *Media) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callID := r.PathValue("call_id")
		token := r.PathValue("token")
		if callID == "" || !m.Tokens.Valid(callID, token) {
			http.NotFound(w, r)
			return
		}
		ctx := r.Context()
		st, err := m.Store.LoadCallForMedia(ctx, callID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		keyB64, err := m.Keys(ctx, st.TelephonyOwnerUserID, st.WebhookKeySecretID)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		v, err := m.Verifier(keyB64)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ep, err := telnyx.NewMediaEndpoint(v, m.StreamURL(callID, token), m.sinkFactory(st))
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		// The connection is tracked from before the upgrade until its frame
		// loop returns. Shutdown cancels connCtx, which ends the loop and closes
		// the socket whether or not a start frame has arrived yet.
		connCtx, done, err := m.sessions.track(ctx)
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		defer done()
		ep.ServeHTTP(w, r.WithContext(connCtx))
	})
}

// sinkFactory runs when a stream's start frame validates. It returns an error,
// which closes the stream, when the call cannot take media. A failure the
// caller should see marks the call failed with a reason code.
//
// frameCtx is the stream's frame-loop context. Shutdown cancels it, so setup
// (the store reads and the AI dial) stops with the connection rather than
// running on a context nothing cancels.
func (m *Media) sinkFactory(upgrade storage.MediaCall) telnyx.SinkFactory {
	return func(frameCtx context.Context, start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		ctx := frameCtx
		// Setup counts as in flight until it returns, so shutdown waits for it
		// before the store closes. A stream that starts after shutdown is refused.
		exit, err := m.sessions.enter()
		if err != nil {
			return nil, err
		}
		defer exit()
		// The upgrade snapshot can be stale: the webhook may have recorded the
		// control id, or the call may have ended, since the upgrade. The start
		// frame is judged against the row as it is now.
		st, err := m.Store.LoadCallForMedia(ctx, upgrade.ID)
		if err != nil {
			return nil, ErrCallUnavailable
		}
		if err := BindControlID(st.ID, st.ControlID, start.CallControlID, start.ClientState); err != nil {
			// A stream that cannot bind is refused, and the live call is ended
			// visibly: a leg that dialed but cannot be bound is never left up.
			// During shutdown the local end is skipped, as for the run's end:
			// shutdown's hangup pass covers every live leg.
			if st.ControlID == "" && isLive(st.Status) && !m.sessions.stopping() {
				m.endLocally(ctx, st.ID, ReasonMediaFailed)
			}
			return nil, err
		}
		if !isLive(st.Status) {
			return nil, ErrCallUnavailable
		}
		// A start frame that bound with no recorded id is the proof Telnyx gave
		// for this leg. Record it, so a later local hangup can end the leg.
		if st.ControlID == "" {
			if _, err := m.Store.BindStreamControlID(ctx, st.ID, start.CallControlID, m.Now()); err != nil {
				return nil, fmt.Errorf("record stream control id: %w", err)
			}
		}
		release, err := m.sessions.claim(st.ID)
		if err != nil {
			return nil, err
		}
		// buildSink owns release on every error path, so it is not called again.
		sink, err := m.buildSink(ctx, st, start, send, release)
		if err != nil {
			return nil, err
		}
		return sink, nil
	}
}

// endReason maps the end of a media run to the reason CallHarbor records when
// it hangs up the leg. The duration cap, the caller silence watchdog, any other
// run failure, and a clean provider close are distinct reasons.
func endReason(capped bool, runErr error) string {
	switch {
	case capped:
		return ReasonMaxDuration
	case errors.Is(runErr, voice.ErrCallerSilence):
		return ReasonCallerSilence
	case runErr != nil:
		return ReasonMediaFailed
	default:
		return ReasonAISessionClosed
	}
}

// isLive reports whether a call can take media: the leg is placed and not yet
// ended.
func isLive(status string) bool {
	switch status {
	case "initiating", "ringing", "active":
		return true
	}
	return false
}

// buildSink resolves the AI session and attaches the bridge. The caller holds
// the per-call session claim. buildSink releases it on every error path, and on
// success attach releases it when the run ends, so the caller never releases.
func (m *Media) buildSink(ctx context.Context, st storage.MediaCall, start telnyx.Frame, send telnyx.Send, release func()) (telnyx.MediaSink, error) {
	// The provider format is the configured wire format. The leg format is
	// the stream's own start format; the bridge converts between them.
	format := m.Format
	prof, err := m.Resolver.Resolve(ctx, st.OwnerUserID, st.AIProfileID)
	if err != nil {
		release()
		m.failSetup(ctx, st.ID, ReasonAIDialFailed)
		return nil, fmt.Errorf("resolve ai profile: %w", err)
	}
	prof.Instructions, err = Render(prof.Instructions, st.BriefJSON, m.MaxBytes)
	if err != nil {
		release()
		reason := ReasonAIDialFailed
		if errors.Is(err, ErrInstructionsTooLarge) {
			reason = ReasonBriefTooLarge
		}
		m.failSetup(ctx, st.ID, reason)
		return nil, err
	}
	// Capabilities are resolved before any provider connection is opened, so an
	// unsupported protocol fails here without dialing. Setup failures end the
	// call through failSetup and release the claim exactly once.
	caps, err := realtime.For(prof.Protocol)
	if err != nil {
		release()
		m.failSetup(ctx, st.ID, ReasonCapabilityMismatch)
		return nil, err
	}
	sess, err := m.Dialers.Dial(ctx, prof, realtime.Requirements{Format: format})
	if err != nil {
		release()
		reason := ReasonAIDialFailed
		if errors.Is(err, realtime.ErrCapability) {
			reason = ReasonCapabilityMismatch
		}
		m.failSetup(ctx, st.ID, reason)
		return nil, err
	}
	return m.attach(sess, st, start.Format, format, caps, send, release)
}

// attach builds the bridge and runs the voice session over it. Every error
// path closes the session and releases the claim, so no provider connection or
// session claim outlives a refused stream. On success the run owns release and
// calls it after the run's end is handled, so shutdown's wait covers the final
// local hangup too.
func (m *Media) attach(sess realtime.Session, st storage.MediaCall, leg, provider audio.Format, caps realtime.Capabilities, send telnyx.Send, release func()) (telnyx.MediaSink, error) {
	bridge, err := voicebridge.New(leg, provider, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		_ = sess.Close()
		release()
		return nil, err
	}
	bridgeSink, err := telnyx.NewBridgeSink(bridge, send)
	if err != nil {
		_ = sess.Close()
		release()
		return nil, err
	}
	ab, err := voicecall.NewAudioBridge(bridgeSink)
	if err != nil {
		_ = sess.Close()
		release()
		return nil, err
	}
	// The run is bound to the stream. Closing the stream cancels it, and
	// voice.Run always closes the session, so the provider connection ends
	// with the stream.
	runCtx, stop := context.WithCancel(context.Background())
	m.sessions.bindCancel(st.ID, stop)
	// streamClosed is set when Telnyx closes the stream, or when the stream
	// fails and the endpoint closes it. A run that ends after that is not a
	// CallHarbor-side end, so no local hangup is sent. Telnyx already tore the
	// leg down, or the caller left.
	stream := &stoppingSink{BridgeSink: bridgeSink, stop: stop}
	go func() {
		// Deferred in this order: stop runs first, then release, so the claim
		// is freed only after the run and its local hangup have finished.
		defer release()
		defer stop()
		opts := voice.Options{Caps: caps, SilenceTimeout: SilenceTimeout}
		_, capped, runErr := Run(runCtx, sess, ab, ab.EventSink(nil), opts, time.Duration(st.MaxDurationSecs)*time.Second)
		if m.Logger != nil && runErr != nil {
			m.Logger.Info("call media ended", "call_id", st.ID, "error", runErr.Error())
		}
		if stream.streamClosed.Load() {
			return
		}
		// Telnyx does not know the AI session ended, so the leg stays up unless
		// CallHarbor hangs it up. Every run end that is not a stream close does.
		// During shutdown the run is cancelled, not ended by the call. Shutdown's
		// HangupLive ends every live leg, so this run must not record a reason
		// that is not true.
		if m.sessions.stopping() {
			return
		}
		m.endLocally(context.WithoutCancel(runCtx), st.ID, endReason(capped, runErr))
	}()
	return stream, nil
}

// stoppingSink is the bridge sink with two extra duties. When the stream closes,
// it cancels the voice run, so the session is torn down with the stream. It
// also records that the stream closed first, so the run's end is not mistaken
// for a CallHarbor-side end.
type stoppingSink struct {
	*telnyx.BridgeSink
	stop         context.CancelFunc
	streamClosed atomic.Bool
}

func (s *stoppingSink) Close() error {
	s.streamClosed.Store(true)
	s.stop()
	return s.BridgeSink.Close()
}

// failSetup ends a call whose AI session could not start. The Telnyx leg is
// already up, so it is ended the same way a running call is: ending, then one
// hangup. This runs on a setup path, which the frame loop may cancel during
// shutdown. During shutdown the local end is skipped, matching the run
// goroutine: HangupLive ends every live leg, and a reason written now would not
// be true.
func (m *Media) failSetup(ctx context.Context, callID, reason string) {
	if m.sessions.stopping() {
		return
	}
	m.endLocally(ctx, callID, reason)
}

// endLocally is the one path by which CallHarbor ends a live call. It moves the
// call to ending with reason, then sends one best-effort Telnyx hangup with the
// owner's key. The call stays in ending if the hangup fails. The later
// call.hangup webhook moves it to ended. Calls that are not live are left alone.
func (m *Media) endLocally(ctx context.Context, callID, reason string) {
	leg, ok, err := m.Store.BeginLocalHangup(ctx, callID, reason, m.Now())
	if err != nil {
		if m.Logger != nil {
			m.Logger.Error("could not end call locally", "call_id", callID, "reason", reason, "error", err.Error())
		}
		return
	}
	if !ok {
		return
	}
	if leg.ControlID == "" || m.Hangup == nil {
		// No control id means Telnyx never connected the leg to this call, so
		// there is nothing to hang up. ending stays, and the call is not left live.
		return
	}
	if err := m.Hangup(ctx, leg); err != nil {
		if m.Logger != nil {
			m.Logger.Warn("local hangup failed; call stays ending", "call_id", callID, "reason", reason, "error", err.Error())
		}
		return
	}
	if m.Logger != nil {
		m.Logger.Info("local hangup sent", "call_id", callID, "reason", reason)
	}
}
