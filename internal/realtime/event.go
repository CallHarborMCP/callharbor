package realtime

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
)

// EventKind is a protocol-neutral voice session event.
type EventKind int

const (
	EventIgnored EventKind = iota
	EventAudio
	EventAgentTranscriptDelta
	EventAgentTranscriptDone
	EventCallerTranscriptDelta
	EventCallerTranscriptFinal
	EventCallerTranscriptFailed
	EventSpeechStarted
	EventResponseDone
	EventTruncated
	EventUsage
	EventDelegation
	EventError
	EventClosed
	// EventTaskReport is a voice-session self-report: the model called the
	// report_task_outcome tool. Appended last so existing kind values do not move.
	EventTaskReport
)

// MaxTaskReportArgs bounds the arguments of one task report. Larger arguments
// are not kept: the event is still delivered, flagged ReportInvalid. It is the
// same limit the outcome parser enforces, so the two cannot drift apart.
const MaxTaskReportArgs = outcome.MaxReportBytes

// MaxTaskReportCallID bounds the function call id of a task report. A longer id
// is treated as unusable: the report is flagged and not acknowledged.
const MaxTaskReportCallID = 128

// Event is a normalized server event. Provider IDs are kept verbatim for
// provenance; fields not supplied by the protocol stay zero and HasTiming
// says whether StartMS/EndMS came from the provider.
type Event struct {
	Kind EventKind
	// RawType is the provider event type.
	RawType string
	// EventID is the provider event_id (empty when the protocol omits it).
	EventID string
	// ItemID/ResponseID are Realtime identifiers; empty on Live.
	ItemID       string
	ResponseID   string
	ContentIndex int
	Audio        []byte
	Text         string
	HasTiming    bool
	StartMS      int64
	EndMS        int64
	// ResponseStatus is Realtime response.done status.
	ResponseStatus string
	UsageSeconds   float64
	UsageTokens    int64
	DelegationID   string
	ErrCode        string
	ErrMessage     string
	CloseReason    string
	// FunctionCallID is the function call id of an EventTaskReport. Arguments holds
	// its raw JSON arguments, empty when they were oversized. ReportInvalid is set
	// when the report cannot be used, so it is recorded as invalid, never parsed.
	FunctionCallID string
	Arguments      string
	ReportInvalid  bool
	// OutOfBand marks caller text produced by an out-of-band model response
	// that references the caller audio item. ResponseID is that response. Such
	// text is model-derived and approximate, never provider ASR.
	OutOfBand bool
}

// ErrProtocol reports a malformed or out-of-contract provider message.
var ErrProtocol = errors.New("realtime: protocol violation")

// ErrNotReady reports a client action before the session handshake.
var ErrNotReady = errors.New("realtime: session not ready")

// MaxServerMessage bounds one provider JSON message.
const MaxServerMessage = 4 << 20

// HandshakeTimeout bounds the protocol startup exchange.
const HandshakeTimeout = 10 * time.Second

// ValidateEndpoint requires wss, except ws to a loopback host for local
// emulators. Credentials, fragments and (for Live) query strings are rejected.
func ValidateEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("realtime: endpoint: %w", err)
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("realtime: endpoint must be an absolute URL without credentials or fragment")
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("realtime: plaintext ws is only allowed to loopback emulators")
		}
	default:
		return nil, fmt.Errorf("realtime: endpoint scheme %q is not a WebSocket", u.Scheme)
	}
	return u, nil
}
