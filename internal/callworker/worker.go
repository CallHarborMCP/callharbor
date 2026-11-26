// Package callworker places queued outbound calls. Queuing is done by the
// service in the request path. This worker is the only place a dial is sent.
//
// A dial is claimed with a conditional queued -> initiating update, so one
// call is dialed at most once by this process. Each dial carries a command id
// derived from the call id, so a retry at Telnyx is idempotent. A call left in
// initiating without a recorded provider id after a restart is never redialed:
// it is failed with a reason code instead.
package callworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// Reason codes written to calls.outcome_reason_code. They are public, so they
// are fixed strings.
const (
	ReasonDialUnconfirmed = "dial_unconfirmed"
	ReasonDialRejected    = "dial_rejected"
	ReasonProfileUnusable = "profile_unusable"
	ReasonURLsUnavailable = "urls_unavailable"
	// ReasonPlanFailed means the dial plan could not be built, for example a
	// key could not be read. Nothing was sent to Telnyx.
	ReasonPlanFailed = "plan_failed"
)

// Store is the repository the worker needs. It is internal, so it sees every
// owner's calls.
type Store interface {
	ListQueuedCalls(ctx context.Context, limit int) ([]storage.QueuedCallRef, error)
	ListUnconfirmedInitiating(ctx context.Context) ([]string, error)
	ListLiveCallLegs(ctx context.Context) ([]storage.LiveCallLeg, error)
	ClaimQueuedCall(ctx context.Context, callID string, now time.Time) error
	MarkCallFailed(ctx context.Context, callID, reasonCode string, now time.Time) error
	RecordProviderCallControlID(ctx context.Context, callID, ownerUserID, controlID string, now time.Time) error
}

// Dialer places one outbound call. *telnyx.Client satisfies it.
type Dialer interface {
	Dial(ctx context.Context, p telnyx.DialParams) (telnyx.DialResult, error)
}

// Hanger ends one provider call leg. *telnyx.Client satisfies it.
type Hanger interface {
	Hangup(ctx context.Context, callControlID, commandID string) error
}

// LegResolver returns the hangup client for one live leg, using the owner's
// key. It is optional: without it, shutdown cannot end legs and says so.
type LegResolver interface {
	LegHanger(ctx context.Context, leg storage.LiveCallLeg) (Hanger, error)
}

// Plan is what the worker needs to build the dial for one call. The app
// resolves it from the owner's profiles and the public URLs.
type Plan struct {
	Dialer       Dialer
	From         string
	ConnectionID string
	WebhookURL   string
	StreamURL    string
	// Codec is the bidirectional RTP format the stream carries. The dial rejects
	// an unset or unsupported format, so the planner must always set it.
	Codec      audio.Format
	MaxSeconds int
}

// Planner returns the dial plan for a queued call, or a reason code naming why
// the call cannot be dialed. A non-empty reason fails the call without a dial.
type Planner interface {
	Plan(ctx context.Context, c storage.QueuedCallRef) (Plan, string, error)
}

// Options configure the worker. Enabled must be true for any dial to be sent.
// The app keeps it false until the per-call ingress that the dial depends on
// is mounted, so no call is placed to a URL nothing serves.
type Options struct {
	Enabled   bool
	BatchSize int
	Now       func() time.Time
	Logger    *slog.Logger
}

// Worker dials queued calls. It is safe to call Tick from one goroutine.
type Worker struct {
	store   Store
	planner Planner
	opts    Options
	wake    chan struct{}
	stopped chan struct{}
	// stopOnce guards close(stopped), so Stop may be called more than once.
	stopOnce sync.Once
	// cancelMu guards cancel, which Run sets and Abort calls.
	cancelMu sync.Mutex
	cancel   context.CancelFunc
	mu       sync.Mutex
}

// New returns a worker. store and planner are required.
func New(store Store, planner Planner, opts Options) (*Worker, error) {
	if store == nil || planner == nil {
		return nil, errors.New("callworker: store and planner are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BatchSize < 1 {
		opts.BatchSize = 16
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Worker{store: store, planner: planner, opts: opts,
		wake: make(chan struct{}, 1), stopped: make(chan struct{})}, nil
}

// Recover fails every call left in initiating without a provider id. Those
// calls may have reached Telnyx, so they are never redialed. It runs once,
// before the first Tick.
func (w *Worker) Recover(ctx context.Context) error {
	ids, err := w.store.ListUnconfirmedInitiating(ctx)
	if err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	for _, id := range ids {
		if err := w.store.MarkCallFailed(ctx, id, ReasonDialUnconfirmed, w.opts.Now()); err != nil {
			return fmt.Errorf("recover call %s: %w", id, err)
		}
		w.opts.Logger.Warn("call failed after restart: dial not confirmed", "call_id", id)
	}
	return nil
}

// DefaultPollInterval is how often the loop ticks without a wake. Wakes from
// new calls are immediate; the poll only recovers a missed wake.
const DefaultPollInterval = 5 * time.Second

// Wake asks the loop to tick now. It never blocks: a wake already pending is
// enough, so a burst of new calls costs one tick.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Stop makes Run take no further dials. A dial already in flight finishes.
// Stop is idempotent.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.stopped) })
}

// HangupLive asks Telnyx to end every live leg. It is best-effort: each leg is
// tried once, and a failure is logged and does not stop the rest. ctx is the
// shutdown deadline, so the whole pass is bounded by it. Hangups are idempotent
// at Telnyx because the command id is derived from the call id.
func (w *Worker) HangupLive(ctx context.Context, res LegResolver) error {
	legs, err := w.store.ListLiveCallLegs(ctx)
	if err != nil {
		return fmt.Errorf("list live legs: %w", err)
	}
	for _, leg := range legs {
		if err := ctx.Err(); err != nil {
			w.opts.Logger.Warn("hangup skipped: shutdown deadline passed", "call_id", leg.ID)
			return err
		}
		h, err := res.LegHanger(ctx, leg)
		if err != nil {
			w.opts.Logger.Warn("hangup skipped: no client for call", "call_id", leg.ID, "error", err.Error())
			continue
		}
		if err := h.Hangup(ctx, leg.ControlID, LegCommandID(leg.ID)); err != nil {
			w.opts.Logger.Warn("hangup failed", "call_id", leg.ID, "error", err.Error())
			continue
		}
		w.opts.Logger.Info("hangup sent at shutdown", "call_id", leg.ID)
	}
	return nil
}

// LegCommandID is the Telnyx command id that ends a call's leg. It derives from
// the call id, so every attempt to end the same leg sends the same id and a
// retry is idempotent at Telnyx.
func LegCommandID(callID string) string {
	return telnyx.CommandID(callID) + "-hangup"
}

// HangupLeg ends one live leg through the owner's key. The media path uses it
// when CallHarbor ends a call; shutdown uses the same command id.
func HangupLeg(ctx context.Context, res LegResolver, leg storage.LiveCallLeg) error {
	h, err := res.LegHanger(ctx, leg)
	if err != nil {
		return fmt.Errorf("resolve hangup client: %w", err)
	}
	return h.Hangup(ctx, leg.ControlID, LegCommandID(leg.ID))
}

// Abort cancels Run's context, which cancels an in-flight dial. Use it only
// after Stop, once the shutdown deadline has passed.
func (w *Worker) Abort() {
	w.cancelMu.Lock()
	defer w.cancelMu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
}

// Run recovers once, then ticks on each wake and on the poll interval until
// Stop or Abort, or until ctx is cancelled. It never spins: with no wake and
// no poll due it blocks. A Tick error is logged and retried on the next wake
// or poll.
func (w *Worker) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w.cancelMu.Lock()
	w.cancel = cancel
	w.cancelMu.Unlock()

	if err := w.Recover(ctx); err != nil {
		return err
	}
	// Dial what is already queued (for example, calls made before a restart)
	// without waiting for the first poll.
	if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
		w.opts.Logger.Error("dial tick failed", "error", err.Error())
	}
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stopped:
			return nil
		case <-w.wake:
		case <-ticker.C:
		}
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.opts.Logger.Error("dial tick failed", "error", err.Error())
		}
	}
}

// Tick dials up to BatchSize queued calls, one at a time. A failure on one call
// is logged and does not stop the others. It returns the first store error, if
// any, so the caller can retry on the next tick.
func (w *Worker) Tick(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.opts.Enabled {
		return nil
	}
	queued, err := w.store.ListQueuedCalls(ctx, w.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("list queued: %w", err)
	}
	var first error
	for _, c := range queued {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-w.stopped:
			return first
		default:
		}
		if err := w.dialOne(ctx, c); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// dialOne claims one call and dials it. The claim is the gate: a call that is
// no longer queued is skipped without a dial.
func (w *Worker) dialOne(ctx context.Context, c storage.QueuedCallRef) error {
	now := w.opts.Now()
	if err := w.store.ClaimQueuedCall(ctx, c.ID, now); err != nil {
		if errors.Is(err, storage.ErrCallNotClaimable) {
			return nil
		}
		return fmt.Errorf("claim %s: %w", c.ID, err)
	}

	plan, reason, err := w.planner.Plan(ctx, c)
	if err != nil {
		// Nothing was sent, so failing the call is safe. Leaving it claimed
		// would hold it in initiating until the next restart.
		w.opts.Logger.Error("call plan failed", "call_id", c.ID, "error", err.Error())
		if ferr := w.fail(context.WithoutCancel(ctx), c, ReasonPlanFailed); ferr != nil {
			return ferr
		}
		return fmt.Errorf("plan %s: %w", c.ID, err)
	}
	if reason != "" {
		return w.fail(ctx, c, reason)
	}

	res, err := plan.Dialer.Dial(ctx, telnyx.DialParams{
		ConnectionID: plan.ConnectionID,
		From:         plan.From,
		To:           c.ToNumber,
		WebhookURL:   plan.WebhookURL,
		StreamURL:    plan.StreamURL,
		Codec:        plan.Codec,
		// CommandID is sent so a retried request carries the same id. Telnyx's
		// handling of a repeated POST /calls is not verified, and this worker
		// never retries a dial, so this is not relied on for dedupe.
		CommandID: telnyx.CommandID(c.ID),
		// ClientState carries the call id. Webhooks check it, so an event
		// that names another call is refused rather than applied.
		ClientState: []byte(c.ID),
		TimeLimit:   plan.MaxSeconds,
	})
	// Writes after a dial use a context that survives cancellation. A dial that
	// Telnyx accepted must still record its control id during shutdown.
	durable := context.WithoutCancel(ctx)
	if err != nil {
		// Only a received 4xx answer proves Telnyx refused the dial. Anything
		// else (transport error, timeout, 5xx, shutdown, or a 2xx with a bad
		// body) may have placed a billable leg, so it is left unconfirmed.
		if rejected(err) {
			w.opts.Logger.Warn("dial rejected", "call_id", c.ID, "error", err.Error())
			return w.fail(durable, c, ReasonDialRejected)
		}
		w.opts.Logger.Warn("dial unconfirmed", "call_id", c.ID, "error", err.Error())
		return w.fail(durable, c, ReasonDialUnconfirmed)
	}
	if err := w.store.RecordProviderCallControlID(durable, c.ID, c.OwnerUserID, res.CallControlID, w.opts.Now()); err != nil {
		return fmt.Errorf("record control id %s: %w", c.ID, err)
	}
	return nil
}

// rejected reports whether a dial error is Telnyx's received 4xx answer. Only
// that proves no leg was placed. A 5xx, a transport error, a timeout or a 2xx
// with a bad body does not.
func rejected(err error) bool {
	var ae *telnyx.APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500
}

func (w *Worker) fail(ctx context.Context, c storage.QueuedCallRef, reason string) error {
	if err := w.store.MarkCallFailed(ctx, c.ID, reason, w.opts.Now()); err != nil {
		return fmt.Errorf("fail %s: %w", c.ID, err)
	}
	return nil
}
