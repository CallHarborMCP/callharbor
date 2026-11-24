package telnyx

import (
	"context"
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
// receipt and runs process atomically. It returns duplicate=true without running
// process when the receipt already exists.
type ReceiptRecorder interface {
	RecordWebhookOnce(ctx context.Context, provider, eventID string, receivedAt time.Time, process func(ctx context.Context) error) (duplicate bool, err error)
}

// Dispatch applies one verified lifecycle signal. A non-nil error makes the
// receipt roll back and the provider receive a 5xx so it retries.
type Dispatch func(ctx context.Context, ev WebhookEvent, sig call.Signal) error

// WebhookIngress serves the Telnyx Call Control webhook endpoint.
type WebhookIngress struct {
	verifier *Verifier
	receipts ReceiptRecorder
	dispatch Dispatch
}

func NewWebhookIngress(v *Verifier, receipts ReceiptRecorder, dispatch Dispatch) (*WebhookIngress, error) {
	if v == nil || receipts == nil || dispatch == nil {
		return nil, errors.New("telnyx: webhook ingress requires verifier, receipts and dispatch")
	}
	return &WebhookIngress{verifier: v, receipts: receipts, dispatch: dispatch}, nil
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
	sig, mapped := ev.Signal()
	if !mapped {
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(sig.EventID) > maxReceiptEventID {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	process := func(ctx context.Context) error {
		return wi.dispatch(ctx, ev, sig)
	}
	if _, err := wi.receipts.RecordWebhookOnce(r.Context(), Provider, sig.EventID, wi.verifier.now(), process); err != nil {
		http.Error(w, "retry", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
