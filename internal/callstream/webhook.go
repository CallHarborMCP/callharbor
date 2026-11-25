package callstream

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// webhookTolerance bounds how far a signed webhook timestamp may drift.
const webhookTolerance = 5 * time.Minute

// Webhooks serves the per-call Telnyx webhook. The verifier is keyed by the
// telephony profile's public key and cached per key, so many calls on one
// profile share one verifier and its replay state.
type Webhooks struct {
	receipts  ReceiptStore
	now       func() time.Time
	mu        sync.Mutex
	verifiers map[string]*telnyx.Verifier
}

// ReceiptStore is the durable dedupe gate. *storage.Store satisfies it.
type ReceiptStore interface {
	RecordWebhookOnce(ctx context.Context, provider, eventID string, receivedAt time.Time, process func(ctx context.Context, tx *sql.Tx) error) (bool, error)
}

// NewWebhooks returns the webhook handler set. receipts and now are required.
func NewWebhooks(receipts ReceiptStore, now func() time.Time) (*Webhooks, error) {
	if receipts == nil || now == nil {
		return nil, errors.New("callstream: webhooks require receipts and a clock")
	}
	return &Webhooks{receipts: receipts, now: now, verifiers: map[string]*telnyx.Verifier{}}, nil
}

// Verifier returns the cached verifier for one public key (base64, as stored
// on the telephony profile). A key that does not parse is refused.
func (w *Webhooks) Verifier(publicKeyB64 string) (*telnyx.Verifier, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if v, ok := w.verifiers[publicKeyB64]; ok {
		return v, nil
	}
	key, err := telnyx.ParsePublicKey(publicKeyB64)
	if err != nil {
		return nil, err
	}
	v, err := telnyx.NewVerifier(ed25519.PublicKey(key), webhookTolerance, w.now)
	if err != nil {
		return nil, err
	}
	w.verifiers[publicKeyB64] = v
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
			// Unknown call or unusable key: no information about which.
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		v, err := w.Verifier(key)
		if err != nil {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		ingress, err := telnyx.NewWebhookIngress(v, w.receipts, w.dispatch(callID))
		if err != nil {
			http.Error(rw, "unavailable", http.StatusServiceUnavailable)
			return
		}
		ingress.ServeHTTP(rw, r)
	})
}

// dispatch applies one verified signal to its call. The URL call id must match
// the event's call, and the event's control id must match the stored one.
// call.Outbound refuses a foreign control id, which rejects the event. The
// rejection is an error, so the receipt rolls back and nothing changes.
func (w *Webhooks) dispatch(callID string) telnyx.Dispatch {
	return func(ctx context.Context, tx *sql.Tx, ev telnyx.WebhookEvent, sig call.Signal) error {
		cur, err := storage.ReadCallForSignal(ctx, tx, callID)
		if err != nil {
			return err
		}
		ob := rebuild(cur)
		changed, err := ob.Apply(sig)
		if err != nil {
			return fmt.Errorf("apply signal: %w", err)
		}
		if !changed {
			return nil
		}
		return storage.WriteCallSignal(ctx, tx, cur, string(ob.State), string(sig.Kind),
			string(ob.Reason), sig.EventID, ob.ProviderCallControlID, w.now())
	}
}

// rebuild returns the call's position from its stored row. The stored status
// is the only state; the state machine derives the rest.
func rebuild(cur storage.CallSignalState) *call.Outbound {
	ob := call.NewOutbound(cur.ID)
	ob.State = call.State(cur.Status)
	ob.ProviderCallControlID = cur.ProviderCallControlID
	return ob
}
