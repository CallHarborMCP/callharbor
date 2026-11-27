package telnyx

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// callStatusAlive is a 200 body shaped like the pinned RetrieveCallStatus
// response: the Call schema under data.
const callStatusAlive = `{"data":{"call_control_id":"v3:abc","call_leg_id":"leg-1","call_session_id":"sess-1","is_alive":true,"record_type":"call"}}`

const callStatusEnded = `{"data":{"call_control_id":"v3:abc","call_leg_id":"leg-1","call_session_id":"sess-1","is_alive":false,"record_type":"call","end_time":"2026-11-27T10:00:00.000Z"}}`

func TestGetCallStatusReadsAliveAndEnded(t *testing.T) {
	f, c := newFake(t, http.StatusOK, callStatusAlive)
	st, err := c.GetCallStatus(context.Background(), "v3:abc")
	if err != nil {
		t.Fatalf("alive: %v", err)
	}
	if !st.IsAlive || st.CallControlID != "v3:abc" {
		t.Fatalf("status = %+v, want alive v3:abc", st)
	}
	got := f.recorded()
	if len(got) != 1 || got[0].method != http.MethodGet || got[0].path != "/v2/calls/v3:abc" {
		t.Fatalf("requests = %+v, want one GET /v2/calls/v3:abc", got)
	}

	_, c = newFake(t, http.StatusOK, callStatusEnded)
	st, err = c.GetCallStatus(context.Background(), "v3:abc")
	if err != nil {
		t.Fatalf("ended: %v", err)
	}
	if st.IsAlive {
		t.Fatalf("ended leg reported alive")
	}
}

func TestGetCallStatusNotFoundIsErrNotFound(t *testing.T) {
	_, c := newFake(t, http.StatusNotFound, `{"errors":[{"detail":"gone"}]}`)
	_, err := c.GetCallStatus(context.Background(), "v3:gone")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: err = %v, want ErrNotFound", err)
	}
}

func TestGetCallStatusUnprocessableIsAPIError(t *testing.T) {
	// 422 is returned to the caller as an API error, not mapped here. The
	// reconciliation sweep decides what it means for the leg.
	_, c := newFake(t, http.StatusUnprocessableEntity, `{}`)
	_, err := c.GetCallStatus(context.Background(), "v3:bad")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
		t.Fatalf("422: err = %v, want APIError 422", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("422 must not match ErrNotFound")
	}
}

func TestGetCallStatusRejectsBodyWithoutIsAlive(t *testing.T) {
	_, c := newFake(t, http.StatusOK, `{"data":{"call_control_id":"v3:abc"}}`)
	if _, err := c.GetCallStatus(context.Background(), "v3:abc"); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("missing is_alive: err = %v, want ErrBadResponse", err)
	}
}

func TestGetCallStatusRequiresControlID(t *testing.T) {
	_, c := newFake(t, http.StatusOK, callStatusAlive)
	if _, err := c.GetCallStatus(context.Background(), ""); !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("empty id: err = %v, want ErrInvalidClient", err)
	}
}
