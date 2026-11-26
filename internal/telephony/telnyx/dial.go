package telnyx

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// DialParams are the validated inputs for an outbound Call Control dial
// (POST /v2/calls) with a bidirectional RTP media stream attached before the
// callee answers.
type DialParams struct {
	ConnectionID string
	From, To     string
	WebhookURL   string
	StreamURL    string
	// StreamAuthToken is optional; how Telnyx presents it on the WebSocket
	// is undocumented, so CallHarbor authenticates the stream by signature and
	// an unguessable stream URL path instead of relying on it.
	StreamAuthToken string
	Codec           audio.Format
	// CommandID makes the dial idempotent at Telnyx.
	CommandID   string
	ClientState []byte
	TimeoutSecs int
	TimeLimit   int
}

// DialRequest is the JSON body; field names and enums follow the pinned
// Telnyx OpenAPI CallRequest schema. It deliberately carries only call-control
// and stream fields: no Telnyx add-ons (AMD, transcription, recording,
// assistants, deepfake detection, etc.) per the telephony-only boundary (#114).
type DialRequest struct {
	ConnectionID                    string `json:"connection_id"`
	To                              string `json:"to"`
	From                            string `json:"from"`
	WebhookURL                      string `json:"webhook_url,omitempty"`
	WebhookURLMethod                string `json:"webhook_url_method,omitempty"`
	CommandID                       string `json:"command_id,omitempty"`
	ClientState                     string `json:"client_state,omitempty"`
	TimeoutSecs                     int    `json:"timeout_secs,omitempty"`
	TimeLimitSecs                   int    `json:"time_limit_secs,omitempty"`
	StreamURL                       string `json:"stream_url"`
	StreamTrack                     string `json:"stream_track"`
	StreamCodec                     string `json:"stream_codec"`
	StreamBidirectionalMode         string `json:"stream_bidirectional_mode"`
	StreamBidirectionalCodec        string `json:"stream_bidirectional_codec"`
	StreamBidirectionalSamplingRate int    `json:"stream_bidirectional_sampling_rate"`
	StreamBidirectionalTargetLegs   string `json:"stream_bidirectional_target_legs"`
	StreamAuthToken                 string `json:"stream_auth_token,omitempty"`
	StreamEstablishBeforeOriginate  bool   `json:"stream_establish_before_call_originate"`
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

var ErrInvalidDial = errors.New("telnyx: invalid dial parameters")

// supportedStreamFormats lists bidirectional RTP codecs CallHarbor can time
// and convert exactly. G722/OPUS/AMR-WB are valid Telnyx enums but are not
// implemented and are rejected rather than silently substituted.
var supportedStreamFormats = map[audio.Format]bool{
	{Encoding: audio.PCMU, SampleRate: 8000}:                     true,
	{Encoding: audio.PCMA, SampleRate: 8000}:                     true,
	{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true}: true,
}

// BuildDialRequest validates p and returns the request body.
func BuildDialRequest(p DialParams) (DialRequest, error) {
	if p.ConnectionID == "" {
		return DialRequest{}, fmt.Errorf("%w: connection_id required", ErrInvalidDial)
	}
	if !e164.MatchString(p.To) || !e164.MatchString(p.From) {
		return DialRequest{}, fmt.Errorf("%w: from/to must be E.164", ErrInvalidDial)
	}
	if err := requireURL(p.WebhookURL, "https"); err != nil {
		return DialRequest{}, fmt.Errorf("%w: webhook_url: %v", ErrInvalidDial, err)
	}
	if err := requireURL(p.StreamURL, "wss"); err != nil {
		return DialRequest{}, fmt.Errorf("%w: stream_url: %v", ErrInvalidDial, err)
	}
	if !supportedStreamFormats[p.Codec] {
		return DialRequest{}, fmt.Errorf("%w: unsupported bidirectional format %v", ErrInvalidDial, p.Codec)
	}
	if p.CommandID == "" {
		return DialRequest{}, fmt.Errorf("%w: command_id required for idempotent dial", ErrInvalidDial)
	}
	if len(p.StreamAuthToken) > 4000 {
		return DialRequest{}, fmt.Errorf("%w: stream_auth_token exceeds 4000 chars", ErrInvalidDial)
	}
	if p.TimeLimit != 0 && (p.TimeLimit < 30 || p.TimeLimit > 14400) {
		return DialRequest{}, fmt.Errorf("%w: time_limit_secs must be 30..14400", ErrInvalidDial)
	}
	if p.TimeoutSecs < 0 {
		return DialRequest{}, fmt.Errorf("%w: timeout_secs negative", ErrInvalidDial)
	}
	r := DialRequest{
		ConnectionID:     p.ConnectionID,
		To:               p.To,
		From:             p.From,
		WebhookURL:       p.WebhookURL,
		WebhookURLMethod: "POST",
		CommandID:        p.CommandID,
		TimeoutSecs:      p.TimeoutSecs,
		TimeLimitSecs:    p.TimeLimit,
		StreamURL:        p.StreamURL,
		// inbound_track = audio from the callee. Telnyx does not document
		// whether outbound_track contains injected bidirectional audio, so it
		// is not used as played-audio evidence.
		StreamTrack:                     "inbound_track",
		StreamCodec:                     string(p.Codec.Encoding),
		StreamBidirectionalMode:         "rtp",
		StreamBidirectionalCodec:        string(p.Codec.Encoding),
		StreamBidirectionalSamplingRate: p.Codec.SampleRate,
		// Default "opposite" targets a leg that does not exist on a single
		// outbound API call (reported by team-telnyx/realtime-ai-demo).
		StreamBidirectionalTargetLegs:  "self",
		StreamAuthToken:                p.StreamAuthToken,
		StreamEstablishBeforeOriginate: true,
	}
	if len(p.ClientState) > 0 {
		r.ClientState = base64.StdEncoding.EncodeToString(p.ClientState)
	}
	return r, nil
}

func requireURL(raw, scheme string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != scheme || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("must be an absolute %s URL without credentials or fragment", scheme)
	}
	return nil
}
