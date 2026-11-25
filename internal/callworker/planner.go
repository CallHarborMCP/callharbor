package callworker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

// ProfileResolver reads the telephony profile's keys for the call path. It is
// the same resolver the service uses, so a grantee's call dials with the
// owner's key and the owner's key is never returned to a transport.
type ProfileResolver interface {
	Resolve(ctx context.Context, actor telephonyprofile.Actor, id string) (telephonyprofile.Resolved, error)
}

// DialerFactory builds a Dialer for one plaintext Telnyx key. Production uses
// telnyx.NewClient against the production root. Tests inject a fake.
type DialerFactory func(apiKey string) (Dialer, error)

// PublicRoutes builds the per-call URLs that Telnyx calls back to. The paths
// are the contract in docs/operations: the webhook is signed, and the stream
// URL carries an unguessable token that the media ingress checks.
type PublicRoutes struct {
	PublicURL string
	// StreamToken returns the unguessable path token for one call. It is stored
	// with the call, so the media ingress can find the call from the path alone.
	StreamToken func(callID string) (string, error)
}

// ProductionPlanner builds dial plans from the owner's profiles.
type ProductionPlanner struct {
	Profiles ProfileResolver
	Dialers  DialerFactory
	Routes   PublicRoutes
	// MaxCallSeconds is the upper bound the dial's time_limit_secs carries.
	MaxCallSeconds int
	// Codec is the bidirectional stream format every dial requests. It must be
	// a format the media path serves, so the planner sets it, never the dialer.
	Codec audio.Format
}

// Plan resolves the telephony profile for the caller and returns the dial plan.
// A profile that cannot place a call returns a reason code instead of an error,
// so the call fails visibly rather than being retried.
func (p ProductionPlanner) Plan(ctx context.Context, c storage.QueuedCallRef) (Plan, string, error) {
	res, err := p.Profiles.Resolve(ctx, telephonyprofile.Actor{UserID: c.OwnerUserID}, c.TelephonyProfileID)
	if errors.Is(err, telephonyprofile.ErrNotFound) {
		return Plan{}, ReasonProfileUnusable, nil
	}
	if err != nil {
		return Plan{}, "", fmt.Errorf("resolve telephony profile: %w", err)
	}
	if res.ConnectivityStatus != storage.ConnectivityVerified || res.ConnectionID == "" || res.WebhookPublicKey == "" {
		return Plan{}, ReasonProfileUnusable, nil
	}
	if p.Routes.PublicURL == "" || p.Routes.StreamToken == nil {
		return Plan{}, ReasonURLsUnavailable, nil
	}
	token, err := p.Routes.StreamToken(c.ID)
	if err != nil {
		return Plan{}, "", fmt.Errorf("stream token: %w", err)
	}
	base := strings.TrimRight(p.Routes.PublicURL, "/")
	webhook := base + "/telnyx/calls/" + c.ID + "/webhook"
	stream, err := StreamURL(base, c.ID, token)
	if err != nil {
		return Plan{}, ReasonURLsUnavailable, nil
	}
	dialer, err := p.Dialers(res.APIKey)
	if err != nil {
		return Plan{}, "", fmt.Errorf("telnyx client: %w", err)
	}
	return Plan{
		Dialer:       dialer,
		From:         res.CallerNumber,
		ConnectionID: res.ConnectionID,
		WebhookURL:   webhook,
		StreamURL:    stream,
		Codec:        p.Codec,
		MaxSeconds:   telnyxTimeLimit(c.MaxDurationSeconds),
	}, "", nil
}

// Telnyx accepts time_limit_secs only in 30..14400. A call's own cap can be
// shorter (1..7200). The media path still enforces that cap locally, so
// raising the dial's Telnyx limit to 30 never lets a call exceed its cap. A
// cap under 30 seconds is therefore sent as 30, not refused at dial.
const (
	telnyxMinTimeLimit = 30
	telnyxMaxTimeLimit = 14400
)

// telnyxTimeLimit clamps a call's cap into Telnyx's accepted range.
func telnyxTimeLimit(seconds int) int {
	switch {
	case seconds < telnyxMinTimeLimit:
		return telnyxMinTimeLimit
	case seconds > telnyxMaxTimeLimit:
		return telnyxMaxTimeLimit
	default:
		return seconds
	}
}

// LegHanger returns the Telnyx client for a live leg, keyed by the owner's
// telephony profile. It shares Plan's key path, so a grantee's leg is ended
// with the owner's key, exactly as it was dialed. A profile that no longer
// resolves returns an error, so the leg is skipped and logged.
func (p ProductionPlanner) LegHanger(ctx context.Context, leg storage.LiveCallLeg) (Hanger, error) {
	res, err := p.Profiles.Resolve(ctx, telephonyprofile.Actor{UserID: leg.OwnerUserID}, leg.TelephonyProfileID)
	if err != nil {
		return nil, fmt.Errorf("resolve telephony profile: %w", err)
	}
	dialer, err := p.Dialers(res.APIKey)
	if err != nil {
		return nil, fmt.Errorf("telnyx client: %w", err)
	}
	h, ok := dialer.(Hanger)
	if !ok {
		return nil, errors.New("telnyx client cannot hang up")
	}
	return h, nil
}

// TelnyxDialerFactory returns the production client factory for a base URL.
// The app passes telnyx.DefaultBaseURL; tests pass a loopback fake.
func TelnyxDialerFactory(baseURL string) DialerFactory {
	return func(apiKey string) (Dialer, error) {
		c, err := telnyx.NewClient(baseURL, apiKey, nil)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// StreamURL is the exact media stream URL for one call. The dial sends it and
// the media handshake verifies against it, so both must build it here.
func StreamURL(publicURL, callID, token string) (string, error) {
	base, err := streamOrigin(strings.TrimRight(publicURL, "/"))
	if err != nil {
		return "", err
	}
	return base + "/telnyx/calls/" + callID + "/media/" + token, nil
}

// streamOrigin maps the public http(s) origin to the matching WebSocket origin.
// Any other scheme is an error, so the stream URL is never built from input
// that is not a web origin.
func streamOrigin(publicURL string) (string, error) {
	switch {
	case strings.HasPrefix(publicURL, "https://"):
		return "wss://" + strings.TrimPrefix(publicURL, "https://"), nil
	case strings.HasPrefix(publicURL, "http://"):
		return "ws://" + strings.TrimPrefix(publicURL, "http://"), nil
	default:
		return "", fmt.Errorf("public url must be http or https")
	}
}
