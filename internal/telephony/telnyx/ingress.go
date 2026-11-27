package telnyx

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
)

// Provider is the receipt namespace used in provider_webhook_receipts.
const Provider = "telnyx"

const maxReceiptEventID = 256

// ReceiptRecorder is the durable dedupe gate. RecordWebhookOnce inserts the
// receipt and runs process with the supplied transaction, committing both or
// neither. It returns duplicate=true without running process when the receipt
// already exists.
//
// The process callback must write only through the supplied tx. It must not
// use any other database handle, because the store holds a single connection
// for tx. It must also not run external side effects (network calls, dials,
// hangups) before commit, since those cannot be rolled back.
type ReceiptRecorder interface {
	RecordWebhookOnce(ctx context.Context, provider, eventID string, receivedAt time.Time, process func(ctx context.Context, tx *sql.Tx) error) (duplicate bool, err error)
}

// Dispatch applies one verified lifecycle signal inside the receipt
// transaction. It must persist state only through tx and must not perform
// external side effects before commit. A non-nil error makes the receipt and
// tx writes roll back, and the provider receives a 5xx so it retries.
type Dispatch func(ctx context.Context, tx *sql.Tx, ev WebhookEvent, sig call.Signal) error

// ErrRejectedEvent marks a verified, well-formed event that can never apply to
// its call: a foreign control id, a client_state naming another call, or an
// unknown call. A retry cannot change that outcome, so the ingress answers 200
// and Telnyx stops retrying. Dispatch wraps this error and writes nothing.
var ErrRejectedEvent = errors.New("telnyx: webhook event rejected")

// WebhookIngress serves the Telnyx Call Control webhook endpoint.
type WebhookIngress struct {
	verifier *Verifier
	receipts ReceiptRecorder
	dispatch Dispatch
	cost     CostDispatch
}

// CostDispatch applies one verified call.cost event inside the receipt
// transaction. A cost event is not a state transition: it writes only the
// provider cost, and it must not change the call's status. It follows the same
// rules as Dispatch: write only through tx, no external side effects, and a
// returned error rolls the receipt back.
type CostDispatch func(ctx context.Context, tx *sql.Tx, ev WebhookEvent) error

func NewWebhookIngress(v *Verifier, receipts ReceiptRecorder, dispatch Dispatch) (*WebhookIngress, error) {
	if v == nil || receipts == nil || dispatch == nil {
		return nil, errors.New("telnyx: webhook ingress requires verifier, receipts and dispatch")
	}
	return &WebhookIngress{verifier: v, receipts: receipts, dispatch: dispatch}, nil
}

// WithCost sets the call.cost dispatch. Without it a verified call.cost is
// acknowledged and not recorded, so a caller that wants provider costs must set
// it. It returns the ingress for chaining.
func (wi *WebhookIngress) WithCost(cost CostDispatch) *WebhookIngress {
	wi.cost = cost
	return wi
}

// ServeHTTP reads the raw body, verifies its signature, parses it, then records
// the receipt and dispatches the signal. Verification failures return 401 and
// parse failures return 400, with no state change. The verifier's in-memory
// replay cache is not used here, so a failed dispatch can be retried by Telnyx.
// Durable deduplication is done by the receipt table.
func (wi *WebhookIngress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBody))
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := wi.verifier.check(r.Header, raw); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ev, err := ParseWebhook(raw)
	if err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if ev.Cost != nil {
		wi.serveCost(w, r, ev)
		return
	}
	sig, mapped := ev.Signal()
	if !mapped {
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(sig.EventID) > maxReceiptEventID {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	process := func(ctx context.Context, tx *sql.Tx) error {
		return wi.dispatch(ctx, tx, ev, sig)
	}
	if _, err := wi.receipts.RecordWebhookOnce(r.Context(), Provider, sig.EventID, wi.verifier.now(), process); err != nil {
		if errors.Is(err, ErrRejectedEvent) {
			// Permanent: acknowledge so Telnyx does not retry a refused event.
			w.WriteHeader(http.StatusOK)
			return
		}
		// Transient (for example a database error or a lost race): retry.
		http.Error(w, "retry", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serveCost records a verified call.cost once. Its receipt dedups a redelivery
// exactly as a lifecycle event is deduped. Without a cost dispatch the event is
// acknowledged and nothing is stored.
func (wi *WebhookIngress) serveCost(w http.ResponseWriter, r *http.Request, ev WebhookEvent) {
	if wi.cost == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(ev.ID) > maxReceiptEventID {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	eventID := ev.Type + "|" + ev.ID
	process := func(ctx context.Context, tx *sql.Tx) error {
		return wi.cost(ctx, tx, ev)
	}
	if _, err := wi.receipts.RecordWebhookOnce(r.Context(), Provider, eventID, wi.verifier.now(), process); err != nil {
		if errors.Is(err, ErrRejectedEvent) {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "retry", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
