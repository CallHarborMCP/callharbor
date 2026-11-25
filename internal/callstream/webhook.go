package callstream

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// webhookTolerance bounds how far a signed webhook timestamp may drift.
const webhookTolerance = 5 * time.Minute

// maxVerifiers bounds the verifier cache. Each entry is one profile public key.
// Eviction is safe because a verifier is cheap to rebuild from its key.
const maxVerifiers = 64

// orphanHangupTimeout bounds the best-effort hangup a webhook sends for a leg
// that CallHarbor no longer tracks as live.
const orphanHangupTimeout = 10 * time.Second

// Webhooks serves the per-call Telnyx webhook. The verifier is keyed by the
// telephony profile's public key and cached per key, so many calls on one
// profile share one verifier and its replay state.
type Webhooks struct {
	receipts  ReceiptStore
	now       func() time.Time
	hangup    LegHangup
	logger    *slog.Logger
	mu        sync.Mutex
	verifiers map[string]*telnyx.Verifier
	order     []string
}

// ReceiptStore is the durable dedupe gate. *storage.Store satisfies it.
type ReceiptStore interface {
	RecordWebhookOnce(ctx context.Context, provider, eventID string, receivedAt time.Time, process func(ctx context.Context, tx *sql.Tx) error) (bool, error)
}

// NewWebhooks returns the webhook handler set. receipts and now are required.
// hangup ends a leg that a webhook finds still alive after its call ended. nil
// disables that step, and the leg is then only logged.
func NewWebhooks(receipts ReceiptStore, now func() time.Time, hangup LegHangup, logger *slog.Logger) (*Webhooks, error) {
	if receipts == nil || now == nil {
		return nil, errors.New("callstream: webhooks require receipts and a clock")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Webhooks{receipts: receipts, now: now, hangup: hangup, logger: logger,
		verifiers: map[string]*telnyx.Verifier{}}, nil
}

// Verifier returns the cached verifier for one public key (base64, as stored
// on the telephony profile). A key that does not parse is refused. The cache is
// keyed by the raw key bytes and holds at most maxVerifiers keys.
func (w *Webhooks) Verifier(publicKeyB64 string) (*telnyx.Verifier, error) {
	raw, err := telnyx.ParsePublicKey(publicKeyB64)
	if err != nil {
		return nil, err
	}
	cacheKey := string(raw)
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.verifiers[cacheKey]; ok {
		return v, nil
	}
	v, err := telnyx.NewVerifier(ed25519.PublicKey(raw), webhookTolerance, w.now)
	if err != nil {
		return nil, err
	}
	if len(w.order) >= maxVerifiers {
		delete(w.verifiers, w.order[0])
		w.order = w.order[1:]
	}
	w.verifiers[cacheKey] = v
	w.order = append(w.order, cacheKey)
	return v, nil
}

// KeyLookup returns the telephony profile's webhook public key for a call. It
// is read per request, so a rotated key on the profile takes effect.
type KeyLookup func(ctx context.Context, callID string) (string, error)

// Handler returns the webhook handler for one request path. The call id comes
// from the path, and the verifier comes from the call's profile key. Every
// verification happens before any state change.
func (w *Webhooks) Handler(lookup KeyLookup) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		callID := r.PathValue("call_id")
		if callID == "" {
			http.NotFound(rw, r)
			return
		}
		key, err := lookup(r.Context(), callID)
		if err != nil {
			// Before the signature is checked, an unknown call looks the same
			// as any other refusal: no information about which.
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		v, err := w.Verifier(key)
		if err != nil {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		var orphan *storage.LiveCallLeg
		ingress, err := telnyx.NewWebhookIngress(v, w.receipts, w.dispatch(callID, &orphan))
		if err != nil {
			http.Error(rw, "unavailable", http.StatusServiceUnavailable)
			return
		}
		sr := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}
		ingress.ServeHTTP(sr, r)
		// A 200 means the receipt committed. A 500 means it rolled back, and
		// Telnyx will retry, so no hangup is sent for a write that did not happen.
		if orphan != nil && sr.status == http.StatusOK {
			w.hangupOrphan(r, *orphan)
		}
	})
}

// statusRecorder remembers the status the ingress wrote, so the handler can
// tell a committed webhook from a rolled-back one.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.written {
		s.status = http.StatusOK
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}

// hangupOrphan sends one best-effort hangup for a leg after its webhook has
// committed. It runs outside the transaction, under a bounded timeout that is
// detached from the request, so a client disconnect does not cancel it.
func (w *Webhooks) hangupOrphan(r *http.Request, leg storage.LiveCallLeg) {
	if w.hangup == nil {
		w.logger.Warn("leg ended by webhook but hangup is not configured", "call_id", leg.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), orphanHangupTimeout)
	defer cancel()
	if err := w.hangup(ctx, leg); err != nil {
		w.logger.Warn("hangup of orphaned leg failed", "call_id", leg.ID, "error", err.Error())
		return
	}
	w.logger.Info("hangup sent for orphaned leg", "call_id", leg.ID)
}

// dispatch applies one verified signal to its call. The event's client_state
// must name the URL's call, and the event's control id must match the stored
// one. Those refusals, and an unknown call, are permanent: they return
// ErrRejectedEvent, so nothing is written and Telnyx is answered 200.
//
// Two effects reach past the signal itself, and both are decided here but
// performed after commit:
//   - A leg whose control id is not yet recorded takes this event's id when the
//     call is initiating or failed. A failed call whose dial was never
//     confirmed can still have placed the leg, and the id makes the leg
//     traceable.
//   - A non-hangup event for a call already ended or failed names a leg that
//     is still alive at Telnyx. It is recorded in *orphan for one hangup.
func (w *Webhooks) dispatch(callID string, orphan **storage.LiveCallLeg) telnyx.Dispatch {
	return func(ctx context.Context, tx *sql.Tx, ev telnyx.WebhookEvent, sig call.Signal) error {
		*orphan = nil
		if clientStateOf(ev.ClientState) != callID {
			return fmt.Errorf("%w: client_state names another call", telnyx.ErrRejectedEvent)
		}
		cur, err := storage.ReadCallForSignal(ctx, tx, callID)
		if errors.Is(err, storage.ErrCallNotFound) {
			return fmt.Errorf("%w: unknown call", telnyx.ErrRejectedEvent)
		}
		if err != nil {
			return err
		}
		if cur.Status == string(call.Queued) {
			// A call that was never dialed cannot have a live leg.
			return fmt.Errorf("%w: call was never dialed", telnyx.ErrRejectedEvent)
		}
		terminal := cur.Status == string(call.Ended) || cur.Status == string(call.Failed)
		if cur.ProviderCallControlID == "" && sig.ProviderCallControlID != "" {
			if err := storage.RecordCallControlID(ctx, tx, callID, sig.ProviderCallControlID, w.now()); err != nil {
				return rejectOrFault(err)
			}
			cur.ProviderCallControlID = sig.ProviderCallControlID
		}
		ob := rebuild(cur)
		changed, err := ob.Apply(sig)
		if err != nil {
			return rejectOrFault(fmt.Errorf("apply signal: %w", err))
		}
		if changed {
			if err := storage.WriteCallSignal(ctx, tx, cur, string(ob.State), string(sig.Kind),
				string(ob.Reason), sig.EventID, ob.ProviderCallControlID, w.now()); err != nil {
				return err
			}
		}
		if terminal && sig.Kind != call.KindHangup && sig.ProviderCallControlID != "" {
			*orphan = &storage.LiveCallLeg{ID: cur.ID, OwnerUserID: cur.OwnerUserID,
				TelephonyProfileID: cur.TelephonyProfileID, ControlID: sig.ProviderCallControlID}
		}
		return nil
	}
}

// rejectOrFault maps a signal refusal to ErrRejectedEvent. A control id that
// belongs to another call or leg is permanent; anything else keeps its error
// and is retried.
func rejectOrFault(err error) error {
	if errors.Is(err, call.ErrForeignProvider) || errors.Is(err, storage.ErrCallForeignProvider) {
		return fmt.Errorf("%w: %v", telnyx.ErrRejectedEvent, err)
	}
	return err
}

// rebuild returns the call's position from its stored row. The stored status
// and reason are the only state; the state machine derives the rest.
func rebuild(cur storage.CallSignalState) *call.Outbound {
	ob := call.NewOutbound(cur.ID)
	ob.State = call.State(cur.Status)
	ob.Reason = call.Reason(cur.ReasonCode)
	ob.ProviderCallControlID = cur.ProviderCallControlID
	return ob
}
