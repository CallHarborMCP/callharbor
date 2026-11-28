package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// recordingState reads the stored opt-in and acknowledgement straight from the
// users table, so a test checks what is persisted, not what the handler says.
func recordingState(t *testing.T, store *storage.Store, username string) storage.RecordingPreference {
	t.Helper()
	u, err := store.GetUserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.GetRecordingPreference(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRecordingPreferenceDefaultsOff(t *testing.T) {
	svc, _, _, sess := fixtureWithUsers(t)
	got, err := svc.GetRecordingPreference(context.Background(), sess["alice"].p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.AcknowledgedAt != nil || got.RetentionDays != 30 {
		t.Fatalf("default recording preference = %+v", got)
	}
}

func TestRecordingPreferenceOffToOnNeedsAcknowledgement(t *testing.T) {
	svc, _, _, sess := fixtureWithUsers(t)
	ctx := context.Background()
	if _, err := svc.SetRecordingPreference(ctx, sess["alice"].p, true, false); err != ErrInvalidInput {
		t.Fatalf("enable without acknowledgement = %v, want ErrInvalidInput", err)
	}
	got, _ := svc.GetRecordingPreference(ctx, sess["alice"].p)
	if got.Enabled || got.AcknowledgedAt != nil {
		t.Fatalf("refused enable changed state: %+v", got)
	}
}

func TestRecordingPreferenceRoundTripAndAudit(t *testing.T) {
	svc, _, store, sess := fixtureWithUsers(t)
	ctx := context.Background()
	alice := sess["alice"].p

	on, err := svc.SetRecordingPreference(ctx, alice, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !on.Enabled || on.AcknowledgedAt == nil {
		t.Fatalf("enable = %+v", on)
	}
	// ON to OFF needs no acknowledgement and keeps the acknowledgement time, so
	// the record of consent is not erased.
	off, err := svc.SetRecordingPreference(ctx, alice, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if off.Enabled || off.AcknowledgedAt == nil || *off.AcknowledgedAt != *on.AcknowledgedAt {
		t.Fatalf("disable = %+v, want the earlier acknowledgement kept", off)
	}
	if _, err := svc.SetRecordingPreference(ctx, alice, true, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountAuditEvents(ctx, "recording.preference_changed", "success"); n != 3 {
		t.Fatalf("preference audit rows = %d, want 3", n)
	}
}

func TestRecordingPreferenceAuditDetailHoldsNoSecret(t *testing.T) {
	if got := auditDetail(false, true); got != `{"from_enabled":false,"to_enabled":true}` {
		t.Fatalf("audit detail = %s", got)
	}
	if strings.Contains(auditDetail(true, false), "password") {
		t.Fatal("audit detail carries a secret field name")
	}
}

func TestRecordingPreferenceHTTPSessionCSRFAndStrictBody(t *testing.T) {
	mux, _, store := newHTTP(t)
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d", rec.Code)
	}
	rootCk, rootCSRF := loginHTTP(t, mux, "root", pwAdmin)
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "alice", "password": pwUser, "role": "user"}, rootCk, rootCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create alice = %d %s", rec.Code, rec.Body.String())
	}
	ck, csrf := loginHTTP(t, mux, "alice", pwUser)

	body := map[string]bool{"enabled": true, "acknowledged": true}
	if rec := do(t, mux, http.MethodGet, "/api/preferences/recording", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/recording", body, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT = %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/recording", body, ck, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/recording", map[string]bool{"enabled": true}, ck, csrf); rec.Code != http.StatusBadRequest {
		t.Fatalf("ON without acknowledged = %d, want 400", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/recording", map[string]any{"enabled": true, "acknowledged": true, "user_id": "root"}, ck, csrf); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", rec.Code)
	}
	rec := do(t, mux, http.MethodPut, "/api/preferences/recording", body, ck, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["enabled"] != true || out["acknowledged_at"] == nil || out["retention_days"] != float64(30) {
		t.Fatalf("response = %v", out)
	}
	if _, ok := out["user_id"]; ok {
		t.Fatalf("response exposes a user id: %v", out)
	}
	got := recordingState(t, store, "alice")
	if !got.Enabled || got.AcknowledgedAt == "" {
		t.Fatalf("stored preference = %+v", got)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/recording", map[string]bool{"enabled": false}, ck, csrf); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, mux, http.MethodGet, "/api/preferences/recording", nil, ck, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
}

func TestAdminCannotSetRecordingFieldsForAnotherUser(t *testing.T) {
	mux, _, store := newHTTP(t)
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d", rec.Code)
	}
	rootCk, rootCSRF := loginHTTP(t, mux, "root", pwAdmin)
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "bob", "password": pwUser, "role": "user"}, rootCk, rootCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create bob = %d %s", rec.Code, rec.Body.String())
	}
	bob, err := store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	bobID := bob.ID
	// Admin PATCH is strict: the recording fields are not part of it, so a
	// request naming them is refused and nothing changes.
	for _, body := range []map[string]any{
		{"recording_opt_in": true},
		{"recording_opt_in": true, "recording_acknowledged_at": "2026-11-27T00:00:00.000Z"},
		{"recording_retention_days": 3650},
	} {
		rec := do(t, mux, http.MethodPatch, "/api/users/"+bobID, body, rootCk, rootCSRF)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("admin PATCH %v = %d %s, want 400", body, rec.Code, rec.Body.String())
		}
	}
	got := recordingState(t, store, "bob")
	if got.Enabled || got.AcknowledgedAt != "" || got.RetentionDays != 30 {
		t.Fatalf("admin changed recording fields: %+v", got)
	}
}
