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
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

// Store is the view of storage the media path needs.
type Store interface {
	LoadCallForMedia(ctx context.Context, callID string) (storage.MediaCall, error)
	BeginLocalHangup(ctx context.Context, callID, reasonCode string, now time.Time) (storage.LiveCallLeg, bool, error)
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
		ep.ServeHTTP(w, r)
	})
}

// sinkFactory runs when a stream's start frame validates. It returns an error,
// which closes the stream, when the call cannot take media. A failure the
// caller should see marks the call failed with a reason code.
func (m *Media) sinkFactory(st storage.MediaCall) telnyx.SinkFactory {
	return func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		ctx := context.Background()
		if err := BindControlID(st.ControlID, start.CallControlID); err != nil {
			return nil, err
		}
		switch st.Status {
		case "initiating", "ringing", "active":
		default:
			return nil, ErrCallUnavailable
		}
		// The provider format is the configured wire format. The leg format is
		// the stream's own start format; the bridge converts between them.
		format := m.Format
		prof, err := m.Resolver.Resolve(ctx, st.OwnerUserID, st.AIProfileID)
		if err != nil {
			m.fail(ctx, st.ID, ReasonAIDialFailed)
			return nil, fmt.Errorf("resolve ai profile: %w", err)
		}
		prof.Instructions, err = Render(prof.Instructions, st.BriefJSON, m.MaxBytes)
		if err != nil {
			reason := ReasonAIDialFailed
			if errors.Is(err, ErrInstructionsTooLarge) {
				reason = ReasonBriefTooLarge
			}
			m.fail(ctx, st.ID, reason)
			return nil, err
		}
		sess, err := m.Dialers.Dial(ctx, prof, realtime.Requirements{Format: format})
		if err != nil {
			reason := ReasonAIDialFailed
			if errors.Is(err, realtime.ErrCapability) {
				reason = ReasonCapabilityMismatch
			}
			m.fail(ctx, st.ID, reason)
			return nil, err
		}
		return m.attach(sess, st, start.Format, format, send)
	}
}

// attach builds the bridge and runs the voice session over it. Every error
// path closes the session, so no provider connection outlives a refused stream.
func (m *Media) attach(sess realtime.Session, st storage.MediaCall, leg, provider audio.Format, send telnyx.Send) (telnyx.MediaSink, error) {
	bridge, err := voicebridge.New(leg, provider, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	bridgeSink, err := telnyx.NewBridgeSink(bridge, send)
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	ab, err := voicecall.NewAudioBridge(bridgeSink)
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	// The run is bound to the stream. Closing the stream cancels it, and
	// voice.Run always closes the session, so the provider connection ends
	// with the stream.
	runCtx, stop := context.WithCancel(context.Background())
	// streamClosed is set when Telnyx closes the stream, or when the stream
	// fails and the endpoint closes it. A run that ends after that is not a
	// CallHarbor-side end, so no local hangup is sent. Telnyx already tore the
	// leg down, or the caller left.
	stream := &stoppingSink{BridgeSink: bridgeSink, stop: stop}
	go func() {
		defer stop()
		_, capped, runErr := Run(runCtx, sess, ab, ab.EventSink(nil), time.Duration(st.MaxDurationSecs)*time.Second)
		if m.Logger != nil && runErr != nil {
			m.Logger.Info("call media ended", "call_id", st.ID, "error", runErr.Error())
		}
		if stream.streamClosed.Load() {
			return
		}
		// Telnyx does not know the AI session ended, so the leg stays up unless
		// CallHarbor hangs it up. Every run end that is not a stream close does.
		switch {
		case capped:
			m.endLocally(context.WithoutCancel(runCtx), st.ID, ReasonMaxDuration)
		case runErr != nil:
			m.endLocally(context.WithoutCancel(runCtx), st.ID, ReasonMediaFailed)
		default:
			m.endLocally(context.WithoutCancel(runCtx), st.ID, ReasonAISessionClosed)
		}
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

// fail ends a call whose AI session could not start. The Telnyx leg is already
// up, so it is ended the same way a running call is: ending, then one hangup.
func (m *Media) fail(ctx context.Context, callID, reason string) {
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
