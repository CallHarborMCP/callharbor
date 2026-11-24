package call

import (
	"errors"
	"fmt"
)

type State string

const (
	Queued     State = "queued"
	Initiating State = "initiating"
	Ringing    State = "ringing"
	Active     State = "active"
	Ending     State = "ending"
	Ended      State = "ended"
	Failed     State = "failed"
)

type Reason string

const (
	ReasonBusy        Reason = "busy"
	ReasonNoAnswer    Reason = "no_answer"
	ReasonRejected    Reason = "rejected"
	ReasonCanceled    Reason = "canceled"
	ReasonHangup      Reason = "hangup"
	ReasonTimeLimit   Reason = "time_limit"
	ReasonLocalHangup Reason = "local_hangup"
	ReasonUnknown     Reason = "unknown"
)

type Kind string

const (
	KindDialAccepted Kind = "dial_accepted"
	KindRinging      Kind = "ringing"
	KindAnswered     Kind = "answered"
	KindHangup       Kind = "hangup"
)

var (
	ErrAlreadyDialed   = errors.New("call: outbound dial already requested")
	ErrNotDialable     = errors.New("call: outbound call is not queued")
	ErrForeignProvider = errors.New("call: provider call id does not match this call")
)

// Signal is one provider-neutral lifecycle observation. EventID deduplicates
// redelivered webhooks; Reason is set only for KindHangup.
type Signal struct {
	EventID               string
	Kind                  Kind
	ProviderCallControlID string
	Reason                Reason
}

// Outbound is the state machine of one outbound call. It is not safe for
// concurrent use; callers serialize signals per call.
type Outbound struct {
	ID                    string
	State                 State
	Reason                Reason
	ProviderCallControlID string
	seen                  map[string]struct{}
}

func NewOutbound(id string) *Outbound {
	return &Outbound{ID: id, State: Queued, seen: map[string]struct{}{}}
}

// BeginDial records that a dial command is about to be sent. It succeeds once,
// so a call can never be dialed twice by the same state machine.
func (o *Outbound) BeginDial() error {
	if o.State != Queued {
		if o.State == Initiating || o.State == Ringing || o.State == Active || o.State == Ending {
			return ErrAlreadyDialed
		}
		return ErrNotDialable
	}
	o.State = Initiating
	return nil
}

// BeginHangup marks a locally requested hangup. Terminal calls are unaffected.
func (o *Outbound) BeginHangup() error {
	switch o.State {
	case Initiating, Ringing, Active:
		o.State = Ending
		o.Reason = ReasonLocalHangup
		return nil
	case Ending, Ended, Failed:
		return nil
	default:
		return fmt.Errorf("%w: cannot hang up from %s", ErrNotDialable, o.State)
	}
}

// Apply advances the call for one signal and reports whether state changed.
// Redelivered event IDs and signals that would move the call backwards are
// ignored. Telnyx does not guarantee event ordering, so answered may arrive
// before ringing and hangup may arrive before answer.
func (o *Outbound) Apply(s Signal) (bool, error) {
	switch s.Kind {
	case KindDialAccepted, KindRinging, KindAnswered, KindHangup:
	default:
		return false, fmt.Errorf("call: unknown signal kind %q", s.Kind)
	}
	if s.ProviderCallControlID != "" && o.ProviderCallControlID != "" && o.ProviderCallControlID != s.ProviderCallControlID {
		return false, ErrForeignProvider
	}
	if s.EventID != "" {
		if _, dup := o.seen[s.EventID]; dup {
			return false, nil
		}
		o.seen[s.EventID] = struct{}{}
	}
	if s.ProviderCallControlID != "" && o.ProviderCallControlID == "" {
		o.ProviderCallControlID = s.ProviderCallControlID
	}
	if o.State == Ended || o.State == Failed {
		return false, nil
	}
	before := o.State
	switch s.Kind {
	case KindDialAccepted:
		o.advance(Initiating)
	case KindRinging:
		o.advance(Ringing)
	case KindAnswered:
		o.advance(Active)
	case KindHangup:
		o.hangup(s.Reason)
	}
	return o.State != before, nil
}

func (o *Outbound) advance(next State) {
	if rank(next) > rank(o.State) {
		o.State = next
	}
}

func (o *Outbound) hangup(reason Reason) {
	if o.State == Ending {
		o.State = Ended
		return
	}
	if o.State == Active {
		o.State = Ended
		o.Reason = reason
		return
	}
	o.State = Failed
	o.Reason = reason
	if reason == "" {
		o.Reason = ReasonUnknown
	}
}

func rank(s State) int {
	switch s {
	case Queued:
		return 0
	case Initiating:
		return 1
	case Ringing:
		return 2
	case Active:
		return 3
	case Ending:
		return 4
	default:
		return 5
	}
}
