package callworker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// Reason codes for reconciliation. They are public, so they are fixed strings.
const (
	// ReasonRestartReconciled ends a call that was live when the process
	// restarted. Its media session died with the old process.
	ReasonRestartReconciled = "restart_reconciled"
	// ReasonMaxDuration ends a ringing or active call past its Telnyx limit.
	ReasonMaxDuration = "max_duration"
	// ReasonReconcileTimeout ends an initiating call whose dial returned an id
	// but whose webhooks never arrived.
	ReasonReconcileTimeout = "reconcile_timeout"
	// ReasonReconcileExpired ends a call whose hangup retries are exhausted and
	// whose Telnyx time limit has passed, so Telnyx must have ended the leg.
	ReasonReconcileExpired = "reconcile_expired"
)

// Reconciliation timing. A hangup is sent again no sooner than
// ReconcileRetryInterval after the last one. After ReconcileMaxAttempts the
// retries continue, but a leg is only marked ended once its Telnyx time limit
// has passed too.
const (
	ReconcileRetryInterval = 60 * time.Second
	ReconcileGraceSeconds  = 60
	ReconcileMaxAttempts   = 5
	// RecoverBudget bounds the restart hangups as a whole. Each send also has
	// the Telnyx client timeout, so a dead provider cannot stall startup.
	RecoverBudget = 2 * time.Minute
	// DefaultRingTimeoutSeconds matches the config default. It is used for
	// deadlines when Options.RingTimeoutSeconds is zero.
	DefaultRingTimeoutSeconds = 30
)

// StatusChecker reads a leg's state at Telnyx. *telnyx.Client satisfies it.
type StatusChecker interface {
	GetCallStatus(ctx context.Context, callControlID string) (telnyx.CallStatus, error)
}

// HangupRecorder persists the hangup attempt marker. *storage.Store satisfies
// it.
type HangupRecorder interface {
	RecordHangupSent(ctx context.Context, callID string, now time.Time) error
}

// HangupLegRecorded sends the one hangup for a leg with the owner's key and
// records the attempt, whether or not Telnyx answered. Every path that ends a
// leg uses it, so the reconciliation sweep sees each send.
func HangupLegRecorded(ctx context.Context, res LegResolver, rec HangupRecorder, leg storage.LiveCallLeg, now time.Time) error {
	h, err := res.LegHanger(ctx, leg)
	if err != nil {
		return errors.Join(fmt.Errorf("resolve hangup client: %w", err), recordAttempt(ctx, rec, leg.ID, now))
	}
	return sendRecorded(ctx, rec, h, leg, now)
}

// sendRecorded sends the hangup through an already resolved client, then
// records the attempt.
func sendRecorded(ctx context.Context, rec HangupRecorder, h Hanger, leg storage.LiveCallLeg, now time.Time) error {
	sendErr := h.Hangup(ctx, leg.ControlID, LegCommandID(leg.ID))
	return errors.Join(sendErr, recordAttempt(ctx, rec, leg.ID, now))
}

// recordAttempt writes the marker. It runs even after cancellation, so an
// attempt that reached Telnyx is always counted.
func recordAttempt(ctx context.Context, rec HangupRecorder, callID string, now time.Time) error {
	if err := rec.RecordHangupSent(context.WithoutCancel(ctx), callID, now); err != nil {
		return fmt.Errorf("record hangup attempt: %w", err)
	}
	return nil
}

// apiStatus returns the HTTP status of a Telnyx API error, or 0. It is logged
// instead of the error text, which can contain provider detail.
func apiStatus(err error) int {
	var ae *telnyx.APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// ringTimeout is the ring timeout used for deadlines.
func (w *Worker) ringTimeout() int {
	if w.opts.RingTimeoutSeconds > 0 {
		return w.opts.RingTimeoutSeconds
	}
	return DefaultRingTimeoutSeconds
}

// expiryDeadline is when Telnyx must have ended the leg: the call's time limit
// (clamped as the dial sends it), plus the ring timeout and grace.
func (w *Worker) expiryDeadline(c storage.ReconcileCall) time.Time {
	limit := telnyxTimeLimit(c.MaxDurationSeconds)
	return c.CreatedAt.Add(time.Duration(limit+w.ringTimeout()+ReconcileGraceSeconds) * time.Second)
}

// Reconcile runs one bounded pass of the reconciliation sweep at the clock's
// current time: ending calls whose hangup is unconfirmed, overdue live calls,
// and terminal calls whose leg may still be up. Each query returns at most
// BatchSize calls. Tick calls it under the same lock as dialing, so no dial
// is in flight while it runs.
func (w *Worker) Reconcile(ctx context.Context) error {
	now := w.opts.Now()
	var first error
	for _, step := range []func(context.Context, time.Time) error{
		w.reconcileEnding, w.reconcileOverdue, w.reconcileOrphans,
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(ctx, now); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// reconcileEnding checks ending calls whose hangup has gone unconfirmed for the
// retry interval. A leg that Telnyx reports gone is ended. Any other outcome
// sends the same hangup again, unless the expiry bound applies.
func (w *Worker) reconcileEnding(ctx context.Context, now time.Time) error {
	calls, err := w.store.ListEndingForReconcile(ctx, now.Add(-ReconcileRetryInterval), w.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("list ending calls: %w", err)
	}
	for _, c := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.reconcileEndingCall(ctx, c, now); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) reconcileEndingCall(ctx context.Context, c storage.ReconcileCall, now time.Time) error {
	if w.opts.Legs == nil {
		w.opts.Logger.Error("reconcile skipped: no leg resolver", "call_id", c.ID)
		return nil
	}
	leg := c.Leg()
	h, err := w.opts.Legs.LegHanger(ctx, leg)
	if err != nil {
		// Counted as an attempt, so an unresolvable profile still reaches the
		// expiry bound instead of being retried forever.
		w.opts.Logger.Warn("reconcile: no client for call", "call_id", c.ID)
		return recordAttempt(ctx, w.store, c.ID, now)
	}
	sc, ok := h.(StatusChecker)
	if !ok {
		w.opts.Logger.Warn("reconcile: client cannot read call status", "call_id", c.ID)
		return recordAttempt(ctx, w.store, c.ID, now)
	}

	st, err := sc.GetCallStatus(ctx, c.ControlID)
	// A 404 or 422 for the leg is treated as not alive. That is not verified
	// against live Telnyx responses, so the expiry bound still applies to it.
	notAlive := (err == nil && !st.IsAlive) || errors.Is(err, telnyx.ErrNotFound) || apiStatus(err) == 422
	if notAlive {
		ended, err := w.store.MarkReconcileEnded(ctx, c.ID, "", now)
		if err != nil {
			return fmt.Errorf("end reconciled call %s: %w", c.ID, err)
		}
		if ended {
			w.opts.Logger.Info("call reconciled: leg not alive at provider", "call_id", c.ID)
		}
		return nil
	}

	if c.Attempts >= ReconcileMaxAttempts && !now.Before(w.expiryDeadline(c)) {
		ended, err := w.store.MarkReconcileEnded(ctx, c.ID, ReasonReconcileExpired, now)
		if err != nil {
			return fmt.Errorf("expire call %s: %w", c.ID, err)
		}
		if ended {
			w.opts.Logger.Error("call reconciled as expired: provider leg unverified past time limit", "call_id", c.ID)
		}
		return nil
	}
	if c.Attempts >= ReconcileMaxAttempts {
		w.opts.Logger.Warn("hangup retry after max attempts", "call_id", c.ID, "attempts", c.Attempts)
	}
	if err := sendRecorded(ctx, w.store, h, leg, now); err != nil {
		w.opts.Logger.Warn("reconcile hangup failed", "call_id", c.ID, "status", apiStatus(err))
	}
	return nil
}

// reconcileOverdue ends live calls past their watchdog deadline. A ringing or
// active call past its limit is ended with max_duration. An initiating call
// whose dial returned an id but never progressed is ended with
// reconcile_timeout. Each is ended once, by the conditional BeginLocalHangup.
func (w *Worker) reconcileOverdue(ctx context.Context, now time.Time) error {
	slack := w.ringTimeout() + ReconcileGraceSeconds
	calls, err := w.store.ListOverdueCalls(ctx, now, slack, w.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("list overdue calls: %w", err)
	}
	for _, c := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}
		reason := ReasonMaxDuration
		if c.Status == "initiating" {
			reason = ReasonReconcileTimeout
		}
		leg, ok, err := w.store.BeginLocalHangup(ctx, c.ID, reason, now)
		if err != nil {
			return fmt.Errorf("end overdue call %s: %w", c.ID, err)
		}
		if !ok {
			continue
		}
		w.opts.Logger.Warn("overdue call ended", "call_id", c.ID, "reason", reason)
		if w.opts.Legs == nil {
			w.opts.Logger.Error("overdue hangup skipped: no leg resolver", "call_id", c.ID)
			continue
		}
		if err := HangupLegRecorded(ctx, w.opts.Legs, w.store, leg, now); err != nil {
			w.opts.Logger.Warn("overdue hangup failed", "call_id", c.ID, "status", apiStatus(err))
		}
	}
	return nil
}

// reconcileOrphans resends the hangup for terminal calls whose leg has no
// confirmed hangup. The attempts are bounded by ReconcileMaxAttempts.
func (w *Worker) reconcileOrphans(ctx context.Context, now time.Time) error {
	calls, err := w.store.ListOrphanedTerminal(ctx, storage.OrphanBounds{
		LastHangupBefore: now.Add(-ReconcileRetryInterval),
		Now:              now,
		MaxAttempts:      ReconcileMaxAttempts,
		LimitMin:         telnyxMinTimeLimit,
		LimitMax:         telnyxMaxTimeLimit,
		ExpiryPadSeconds: w.ringTimeout() + ReconcileGraceSeconds,
	}, w.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("list orphaned calls: %w", err)
	}
	for _, c := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}
		if w.opts.Legs == nil {
			w.opts.Logger.Error("orphan hangup skipped: no leg resolver", "call_id", c.ID)
			continue
		}
		if err := HangupLegRecorded(ctx, w.opts.Legs, w.store, c.Leg(), now); err != nil {
			w.opts.Logger.Warn("orphan hangup failed", "call_id", c.ID, "status", apiStatus(err))
		}
	}
	return nil
}
