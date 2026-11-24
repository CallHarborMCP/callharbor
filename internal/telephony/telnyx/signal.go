package telnyx

import "github.com/CallHarborMCP/callharbor/internal/call"

// Signal maps a verified Telnyx Call Control webhook onto the provider-neutral
// outbound lifecycle. ok is false for events that do not drive call state.
//
// The pinned Call Control schema has no ringing webhook (only TeXML publishes
// call.ringing). Ringing is therefore inferred from call.initiated, the event
// that confirms the outbound leg has been placed and is alerting the callee.
// This is an assumption until verified against a live call (see
// docs/protocol/FEASIBILITY.md).
func (ev WebhookEvent) Signal() (call.Signal, bool) {
	s := call.Signal{EventID: ev.Type + "|" + ev.ID, ProviderCallControlID: ev.CallControlID}
	switch ev.Type {
	case EventCallInitiated:
		s.Kind = call.KindRinging
	case EventCallAnswered:
		s.Kind = call.KindAnswered
	case EventCallHangup:
		s.Kind = call.KindHangup
		s.Reason = hangupReason(ev.HangupCause)
	default:
		return call.Signal{}, false
	}
	return s, true
}

func hangupReason(cause string) call.Reason {
	switch cause {
	case "user_busy":
		return call.ReasonBusy
	case "no_answer", "timeout":
		return call.ReasonNoAnswer
	case "call_rejected":
		return call.ReasonRejected
	case "originator_cancel":
		return call.ReasonCanceled
	case "time_limit":
		return call.ReasonTimeLimit
	case "normal_clearing":
		return call.ReasonHangup
	default:
		return call.ReasonUnknown
	}
}
