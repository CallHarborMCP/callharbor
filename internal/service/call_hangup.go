package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// HangupResult is the outcome of an explicit hangup. TechnicalStatus is the
// call's status after the request. TaskOutcome is nil until the call has one.
type HangupResult struct {
	CallID          string  `json:"call_id"`
	TechnicalStatus string  `json:"technical_status"`
	TaskOutcome     *string `json:"task_outcome"`
}

// HangupCall ends a call on request. The owner may hang up their own call, and
// an administrator may hang up any call; every other caller gets not_found, so
// the existence of another user's call is not revealed. An administrator's
// hangup records the target user in the audit row. The hangup uses the telephony
// owner's key and does not re-check a grant, so a revoked grant cannot strand a
// leg.
//
// The state change and its audit row commit in one transaction. A call that is
// already ending, ended or failed is returned unchanged with no Telnyx request.
func (o *CallOperator) HangupCall(ctx context.Context, actor Actor, callID string) (HangupResult, error) {
	if err := actor.Require(ScopeCall); err != nil {
		return HangupResult{}, err
	}
	if callID == "" || len(callID) > MaxIDLength {
		return HangupResult{}, invalidInput("call_id must be 1 to %d characters", MaxIDLength)
	}
	owner, err := o.store.CallOwner(ctx, callID)
	if errors.Is(err, storage.ErrCallNotFound) {
		return HangupResult{}, newError(CodeNotFound, "call not found")
	}
	if err != nil {
		return HangupResult{}, internal(fmt.Errorf("read call owner: %w", err))
	}
	if owner != actor.UserID && !actor.Admin {
		// Another user's call is indistinguishable from a missing one. The
		// refusal is audited with no target, so it does not reveal the owner.
		o.auditHangup(ctx, actor, owner, callID, "denied")
		return HangupResult{}, newError(CodeNotFound, "call not found")
	}

	audit := o.hangupAudit(actor, owner, callID, "success")
	decision, err := o.store.RequestHangup(ctx, callID, audit, o.deps.Now())
	if errors.Is(err, storage.ErrCallNotFound) {
		return HangupResult{}, newError(CodeNotFound, "call not found")
	}
	if err != nil {
		return HangupResult{}, internal(fmt.Errorf("request hangup: %w", err))
	}

	if decision.Action == storage.HangupActionSendLeg && o.deps.Hangup != nil {
		// Best-effort: a failed send leaves the call ending. The webhook or the
		// shutdown pass can still end the leg, and the command id is stable.
		// The error is not returned, so it is not shown to the caller. Only the
		// call id is logged: the cause can carry provider detail.
		if err := o.deps.Hangup(ctx, decision.Leg); err != nil && o.deps.Logger != nil {
			o.deps.Logger.Warn("call hangup send failed", "call_id", callID)
		}
	}
	if decision.Action == storage.HangupActionSendLeg || decision.Action == storage.HangupActionAwaitingDial {
		if o.deps.StopMedia != nil {
			o.deps.StopMedia(callID)
		}
	}
	return HangupResult{CallID: callID, TechnicalStatus: decision.Status, TaskOutcome: outcomeOf(decision)}, nil
}

// outcomeOf returns the task outcome for the view, or nil when the call has none.
// A hangup never sets one, so this reports whatever the call already had.
func outcomeOf(d storage.HangupDecision) *string {
	if d.Outcome == "" {
		return nil
	}
	o := d.Outcome
	return &o
}

// auditHangup records a refused hangup. A failed audit write does not change
// the refusal the caller receives.
func (o *CallOperator) auditHangup(ctx context.Context, actor Actor, owner, callID, outcome string) {
	if o.store == nil {
		return
	}
	_ = o.store.InsertAudit(ctx, o.hangupAudit(actor, owner, callID, outcome))
}

// hangupAudit builds the call.hangup audit row. Only a successful hangup by an
// administrator on another user's call records that user as the target. A
// refusal never names the owner, so the audit trail cannot reveal whose call
// it was to a non-admin.
func (o *CallOperator) hangupAudit(actor Actor, owner, callID, outcome string) storage.AuditEvent {
	target := ""
	if outcome == "success" && actor.Admin && owner != actor.UserID {
		target = owner
	}
	detail, _ := json.Marshal(map[string]string{"outcome": outcome})
	return storage.AuditEvent{
		OccurredAt:   o.deps.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		ActorUserID:  actor.UserID,
		ActorRole:    roleOf(actor),
		TargetUserID: target,
		Action:       "call.hangup",
		ResourceType: "call",
		ResourceID:   callID,
		Outcome:      outcome,
		DetailJSON:   string(detail),
	}
}
