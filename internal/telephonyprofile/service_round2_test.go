package telephonyprofile

import (
	"context"
	"errors"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// TestValidateRefusesResultWhenProfileChangedDuringValidation moves the stored
// profile forward while the provider is being called. The stale result must not
// overwrite the stored status; the caller gets ErrValidationRace.
func TestValidateRefusesResultWhenProfileChangedDuringValidation(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	before := h.store.profiles[v.ID].UpdatedAt

	inner := h.factory
	h.svc.provider = func(key string) (Provider, error) {
		p, err := inner(key)
		if err != nil {
			return nil, err
		}
		// The owner edits the profile between the read and the write.
		rec := h.store.profiles[v.ID]
		rec.Name = "renamed-during-validation"
		rec.UpdatedAt = "2026-11-25T11:30:00.000Z"
		h.store.profiles[v.ID] = rec
		return p, nil
	}

	res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
	if !errors.Is(err, ErrValidationRace) {
		t.Fatalf("validate during edit = %+v, %v; want ErrValidationRace", res, err)
	}
	got := h.store.profiles[v.ID]
	if got.ConnectivityStatus != storage.ConnectivityUnknown || got.ConnectivityCheckedAt != "" {
		t.Fatalf("stale validation overwrote the stored result: status=%s checked=%q", got.ConnectivityStatus, got.ConnectivityCheckedAt)
	}
	if got.UpdatedAt == before {
		t.Fatal("test setup did not move updated_at")
	}
	if last := h.store.audits[len(h.store.audits)-1]; last.Outcome != "conflict" {
		t.Fatalf("audit outcome = %q, want conflict", last.Outcome)
	}
}

// TestValidateAuditOutcomeReflectsResult proves the validate audit row carries the
// real result: success when verified, failed when the check fails.
func TestValidateAuditOutcomeReflectsResult(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Validate(context.Background(), adminAlice, v.ID); err != nil {
		t.Fatal(err)
	}
	if last := h.store.audits[len(h.store.audits)-1]; last.Action != "telephony_profile.validate" || last.Outcome != "success" {
		t.Fatalf("verified audit = %+v", last)
	}

	delete(h.fake.numbers, testNumber)
	if _, err := h.svc.Validate(context.Background(), adminAlice, v.ID); err != nil {
		t.Fatal(err)
	}
	if last := h.store.audits[len(h.store.audits)-1]; last.Action != "telephony_profile.validate" || last.Outcome != "failed" {
		t.Fatalf("failed audit = %+v", last)
	}
}

// TestGrantAndRevokeAuditsNameTheGrantee proves grant and revoke audit rows carry
// the grantee as the target user.
func TestGrantAndRevokeAuditsNameTheGrantee(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	var grant, revoke *storage.AuditEvent
	for i := range h.store.audits {
		e := &h.store.audits[i]
		switch e.Action {
		case "telephony_profile.grant":
			grant = e
		case "telephony_profile.revoke":
			revoke = e
		}
	}
	if grant == nil || grant.TargetUserID != "bob" || grant.ResourceID != v.ID {
		t.Fatalf("grant audit = %+v, want target bob", grant)
	}
	if revoke == nil || revoke.TargetUserID != "bob" || revoke.ResourceID != v.ID {
		t.Fatalf("revoke audit = %+v, want target bob", revoke)
	}
}

// TestGrantUnknownGranteeIsInvalidInput maps a missing user to invalid input and
// reports any other store failure as an internal error rather than a client error.
func TestGrantUnknownGranteeIsInvalidInput(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Grant(context.Background(), adminAlice, v.ID, "nobody"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown grantee = %v, want ErrInvalidInput", err)
	}
}

// TestRevokeMapsOnlyMissingGrantToNotFound: a missing grant is not found; any
// other store failure is returned unchanged, not hidden as not found.
func TestRevokeMapsOnlyMissingGrantToNotFound(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	if err := h.svc.Revoke(context.Background(), adminAlice, v.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing grant = %v, want ErrNotFound", err)
	}
	h.store.failNext = "DeleteTelephonyGrant"
	err := h.svc.Revoke(context.Background(), adminAlice, v.ID, "bob")
	if errors.Is(err, ErrNotFound) || err == nil {
		t.Fatalf("store failure on revoke = %v, want the store error", err)
	}
}

// TestRevokedKeyOverwritesVerifiedStatusOnNextValidation: a key that worked and
// is then revoked by Telnyx must turn a verified profile into failed with
// invalid_credentials, and that failure replaces the stored result.
func TestRevokedKeyOverwritesVerifiedStatusOnNextValidation(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	ctx := context.Background()

	first, err := h.svc.Validate(ctx, adminAlice, v.ID)
	if err != nil || first.ConnectivityStatus != storage.ConnectivityVerified {
		t.Fatalf("first validate = %+v, %v; want verified", first, err)
	}

	h.fake.validKey = "KEY-revoked-by-telnyx" // gitleaks:allow
	second, err := h.svc.Validate(ctx, adminAlice, v.ID)
	if err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if second.ConnectivityStatus != storage.ConnectivityFailed || second.ConnectivityReason != ReasonInvalidCredentials {
		t.Fatalf("revoked-key validate = %+v, want failed/%s", second, ReasonInvalidCredentials)
	}
	got := h.store.profiles[v.ID]
	if got.ConnectivityStatus != storage.ConnectivityFailed || got.ConnectivityReason != ReasonInvalidCredentials {
		t.Fatalf("stored status not overwritten: %+v", got)
	}
}
