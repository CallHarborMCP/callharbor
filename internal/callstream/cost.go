package callstream

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// ProviderCostFromReport turns a verified call.cost report into the stored
// provider cost. A success is known only when its total parses and every part
// that names a currency names the same one. A mixed or unnamed currency leaves
// the amount unset, so the cost reads as unavailable rather than a guessed sum.
// An error report stores no amount. Amounts are exact micro-units, never floats.
func ProviderCostFromReport(rep telnyx.CostReport, eventID string) storage.ProviderCost {
	out := storage.ProviderCost{Status: rep.Status, EventID: eventID}
	if rep.Status != telnyx.CostStatusSuccess {
		return out
	}
	currency, ok := singleCurrency(rep.Parts)
	if !ok || rep.TotalCost == "" {
		return out
	}
	micros, err := config.ParseMicrosAllowZero(rep.TotalCost)
	if err != nil {
		return out
	}
	out.AmountMicros = &micros
	out.Currency = currency
	return out
}

// singleCurrency returns the one currency every part names. It reports false
// when there are no parts, when a part has no currency, or when two differ.
func singleCurrency(parts []telnyx.CostPart) (string, bool) {
	if len(parts) == 0 {
		return "", false
	}
	cur := ""
	for _, p := range parts {
		c := strings.ToUpper(strings.TrimSpace(p.Currency))
		if len(c) != 3 {
			return "", false
		}
		if cur == "" {
			cur = c
			continue
		}
		if c != cur {
			return "", false
		}
	}
	return cur, true
}

// CostDispatch returns the call.cost dispatch for the webhook ingress. A cost is
// not a state transition, so it is recorded whenever it binds to the call, in
// any state, including before the call's hangup has arrived. Telnyx can deliver
// call.cost before call.hangup, and the cost must not be lost or answered with
// a retry. The binding checks are the same as a lifecycle event: client_state
// must name this call, or a recorded control id must match the event. A call
// that was never dialed is refused, as a lifecycle event is. An unknown or
// refused call records nothing. It writes only the provider cost columns, in
// the receipt transaction.
func (w *Webhooks) CostDispatch(callID string) telnyx.CostDispatch {
	return func(ctx context.Context, tx *sql.Tx, ev telnyx.WebhookEvent) error {
		if ev.Cost == nil {
			return telnyx.ErrRejectedEvent
		}
		cur, err := storage.ReadCallForSignal(ctx, tx, callID)
		if errors.Is(err, storage.ErrCallNotFound) {
			return rejectClass(classUnknownCall, "unknown call")
		}
		if err != nil {
			return err
		}
		stateCall := clientStateOf(ev.ClientState)
		if stateCall != "" && stateCall != callID {
			return rejectClass(classClientState, "client_state names another call")
		}
		if cur.ProviderCallControlID == "" && stateCall != callID {
			return rejectClass(classClientState, "no recorded control id and client_state does not name this call")
		}
		if cur.Status == string(call.Queued) {
			// A call that was never dialed cannot have a provider cost.
			return rejectClass(classNeverDialed, "call was never dialed")
		}
		if cur.ProviderCallControlID != "" && ev.CallControlID != cur.ProviderCallControlID {
			return rejectClass(classForeignLeg, "control id belongs to another leg")
		}
		err = storage.RecordProviderCost(ctx, tx, callID, ProviderCostFromReport(*ev.Cost, ev.Type+"|"+ev.ID))
		if errors.Is(err, storage.ErrCostAlreadyRecorded) {
			return rejectClass(classCostConflict, "a different provider cost is already recorded")
		}
		return err
	}
}

// classCostConflict names a second, different cost for a call that already has
// one. It is refused, so the first recorded cost stands.
const classCostConflict = "cost_already_recorded"
