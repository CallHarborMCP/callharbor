package telephonyprofile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// Edge cases for guards, error mapping and rollback paths that the main
// matrix does not reach.

func TestEmptyActorIsForbiddenEverywhere(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	none := Actor{}
	if _, err := h.svc.Create(ctx, none, validInput()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Create = %v", err)
	}
	if _, err := h.svc.Update(ctx, none, "tp-1", validInput()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Update = %v", err)
	}
	if _, err := h.svc.Get(ctx, none, "tp-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Get = %v", err)
	}
	if _, err := h.svc.List(ctx, none); !errors.Is(err, ErrForbidden) {
		t.Fatalf("List = %v", err)
	}
	if err := h.svc.Delete(ctx, none, "tp-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := h.svc.Validate(ctx, none, "tp-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Validate = %v", err)
	}
	if _, err := h.svc.Grant(ctx, none, "tp-1", "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Grant = %v", err)
	}
	if err := h.svc.Revoke(ctx, none, "tp-1", "bob"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Revoke = %v", err)
	}
	if _, err := h.svc.ListGrants(ctx, none, "tp-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListGrants = %v", err)
	}
}

func TestMalformedIDsAreNotFound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bad := "bad id with spaces"
	if _, err := h.svc.Get(ctx, adminAlice, bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if err := h.svc.Delete(ctx, adminAlice, bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := h.svc.Validate(ctx, adminAlice, bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Validate = %v", err)
	}
	if _, err := h.svc.Grant(ctx, adminAlice, bad, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Grant profile = %v", err)
	}
	if _, err := h.svc.Grant(ctx, adminAlice, "tp-1", bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Grant grantee = %v", err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, bad, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revoke profile = %v", err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, "tp-1", bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revoke grantee = %v", err)
	}
	if _, err := h.svc.ListGrants(ctx, adminAlice, bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListGrants = %v", err)
	}
	if len(h.fake.recorded()) != 0 {
		t.Fatal("malformed id reached the provider")
	}
}

func TestMissingProfileIsNotFound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, "tp-missing", "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Grant = %v", err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, "tp-missing", "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revoke = %v", err)
	}
	if _, err := h.svc.ListGrants(ctx, adminAlice, "tp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListGrants = %v", err)
	}
	if _, err := h.svc.Get(ctx, adminAlice, "tp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if _, err := h.svc.Validate(ctx, adminAlice, "tp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Validate = %v", err)
	}
	if err := h.svc.Delete(ctx, adminAlice, "tp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete = %v", err)
	}
	if _, err := h.svc.Update(ctx, adminAlice, "tp-missing", validInput()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update = %v", err)
	}
}

func TestUpdateInvalidInputAndSecretFailuresFailClosed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())

	bad := validInput()
	bad.Name = ""
	if _, err := h.svc.Update(ctx, adminAlice, v.ID, bad); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid update = %v", err)
	}

	h.sec.noKey = true
	in := validInput()
	in.APIKey = rotatedKey
	if _, err := h.svc.Update(ctx, adminAlice, v.ID, in); !errors.Is(err, ErrSecretsDown) {
		t.Fatalf("update without master key = %v", err)
	}
	h.sec.noKey = false

	// Both keys seal, then the profile write fails: both new rows are dropped
	// and only the original api key row remains.
	in = validInput()
	in.APIKey = rotatedKey
	in.WebhookPublicKey = fakePubKeyB
	h.store.failNext = "UpdateTelephonyProfile"
	if _, err := h.svc.Update(ctx, adminAlice, v.ID, in); err == nil {
		t.Fatal("update failure was swallowed")
	}
	if len(h.store.secrets) != 1 {
		t.Fatalf("failed update left %d secrets, want the original only", len(h.store.secrets))
	}
}

func TestUpdateReplacesWebhookKeyAndSealFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	in := validInput()
	in.WebhookPublicKey = fakePubKeyB
	v := h.create(adminAlice, in)
	if len(h.store.secrets) != 2 {
		t.Fatalf("create secrets = %d, want api + webhook", len(h.store.secrets))
	}

	// Replacing both keys leaves exactly two rows: the new api and webhook.
	next := validInput()
	next.WebhookPublicKey = fakePubKeyB
	if _, err := h.svc.Update(ctx, adminAlice, v.ID, next); err != nil {
		t.Fatal(err)
	}
	if len(h.store.secrets) != 2 {
		t.Fatalf("after replace secrets = %d, want 2", len(h.store.secrets))
	}

	// The api key seals, then the webhook key fails. The new api row must be
	// dropped so the failed update leaves the rows unchanged.
	before := len(h.store.secrets)
	h.sec.failAt = h.sec.calls + 2
	bad := validInput()
	bad.APIKey = rotatedKey
	bad.WebhookPublicKey = fakePubKeyB
	if _, err := h.svc.Update(ctx, adminAlice, v.ID, bad); err == nil {
		t.Fatal("seal failure was swallowed")
	}
	if len(h.store.secrets) != before {
		t.Fatalf("failed seal changed secret rows: %d -> %d", before, len(h.store.secrets))
	}
}

func TestUpdateToUsedNameIsConflict(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.create(adminAlice, validInput())
	second := validInput()
	second.Name = "other"
	v2 := h.create(adminAlice, second)
	rename := validInput()
	rename.Name = "main"
	if _, err := h.svc.Update(ctx, adminAlice, v2.ID, rename); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename to a used name = %v, want ErrConflict", err)
	}
}

func TestCreateSecretAndInsertFailureMapping(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t)
	h.sec.failAt = 1
	if _, err := h.svc.Create(ctx, adminAlice, validInput()); err == nil {
		t.Fatal("generic secret failure was swallowed")
	}

	h = newHarness(t)
	h.sec.invalid = true
	if _, err := h.svc.Create(ctx, adminAlice, validInput()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid secret = %v, want ErrInvalidInput", err)
	}

	h = newHarness(t)
	in := validInput()
	in.WebhookPublicKey = fakePubKeyB
	h.sec.failAt = 2 // the api key seals, the webhook key fails
	if _, err := h.svc.Create(ctx, adminAlice, in); err == nil {
		t.Fatal("webhook seal failure was swallowed")
	}
	if len(h.store.secrets) != 0 {
		t.Fatalf("api key orphaned after webhook failure: %d rows", len(h.store.secrets))
	}

	h = newHarness(t)
	h.store.failNext = "CreateTelephonyProfile"
	if _, err := h.svc.Create(ctx, adminAlice, validInput()); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("generic insert failure = %v", err)
	}
}

func TestDeleteAndValidateStoreFailures(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	v := h.create(adminAlice, validInput())
	h.store.failNext = "DeleteTelephonyProfile"
	if err := h.svc.Delete(ctx, adminAlice, v.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("generic delete failure = %v", err)
	}

	h.store.failNext = "SetTelephonyConnectivity"
	h.goodFake()
	if _, err := h.svc.Validate(ctx, adminAlice, v.ID); err == nil {
		t.Fatal("connectivity write failure was swallowed")
	}
}

func TestGetReturnsAccessForOwnerAndGrantee(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	got, err := h.svc.Get(ctx, userBob, v.ID)
	if err != nil || got.Access != storage.TelephonyAccessGranted {
		t.Fatalf("granted Get = %+v, %v", got, err)
	}
	own, err := h.svc.Get(ctx, adminAlice, v.ID)
	if err != nil || own.Access != storage.TelephonyAccessOwner {
		t.Fatalf("owner Get = %+v, %v", own, err)
	}
}

func TestGrantedGranteeCannotReadGrantList(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.ListGrants(ctx, userBob, v.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("granted user list = %v, want ErrForbidden", err)
	}
	grants, err := h.svc.ListGrants(ctx, adminAlice, v.ID)
	if err != nil || len(grants) != 1 || grants[0].UserID != "bob" {
		t.Fatalf("owner grants = %+v, %v", grants, err)
	}
}

func TestGrantWriteFailureAndRevokeOfNeverGranted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	h.store.failNext = "UpsertTelephonyGrant"
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err == nil {
		t.Fatal("grant write failure was swallowed")
	}
	if err := h.svc.Revoke(ctx, adminAlice, v.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke of never-granted user = %v", err)
	}
}

func TestClassifyAPIErrorBranches(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"401 is invalid credentials": {&telnyx.APIError{Method: "GET", Path: "/x", Status: 401}, ReasonInvalidCredentials},
		"403 is invalid credentials": {&telnyx.APIError{Method: "GET", Path: "/x", Status: 403}, ReasonInvalidCredentials},
		"500 is unreachable":         {&telnyx.APIError{Method: "GET", Path: "/x", Status: 500}, ReasonProviderUnreachable},
		"plain error is unreachable": {errors.New("boom"), ReasonProviderUnreachable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := classifyAPIError(c.err); got != c.want {
				t.Fatalf("classifyAPIError = %s, want %s", got, c.want)
			}
		})
	}
}

// statusProvider answers each lookup with a canned result, so every validation
// branch can be driven without the HTTP fake.
type statusProvider struct {
	appErr  error
	numErr  error
	number  telnyx.PhoneNumber
	appLive bool
}

func (p statusProvider) GetCallControlApplication(context.Context, string) (telnyx.CallControlApplication, error) {
	if p.appErr != nil {
		return telnyx.CallControlApplication{}, p.appErr
	}
	return telnyx.CallControlApplication{ID: testAppID, Active: p.appLive}, nil
}

func (p statusProvider) FindPhoneNumber(context.Context, string) (telnyx.PhoneNumber, error) {
	if p.numErr != nil {
		return telnyx.PhoneNumber{}, p.numErr
	}
	return p.number, nil
}

func TestValidateProviderErrorBranches(t *testing.T) {
	good := telnyx.PhoneNumber{ID: "num-1", PhoneNumber: testNumber, Status: "active", ConnectionID: testConnID}
	cases := map[string]struct {
		prov   statusProvider
		reason string
	}{
		"application 401":       {statusProvider{appErr: &telnyx.APIError{Status: 401}}, ReasonInvalidCredentials},
		"application 404":       {statusProvider{appErr: telnyx.ErrNotFound}, ReasonApplicationNotFound},
		"application transport": {statusProvider{appErr: errors.New("down")}, ReasonProviderUnreachable},
		"number 403":            {statusProvider{appLive: true, numErr: &telnyx.APIError{Status: 403}}, ReasonInvalidCredentials},
		"number 500":            {statusProvider{appLive: true, numErr: &telnyx.APIError{Status: 500}}, ReasonProviderUnreachable},
		"number transport":      {statusProvider{appLive: true, numErr: errors.New("down")}, ReasonProviderUnreachable},
		"number not found":      {statusProvider{appLive: true, numErr: telnyx.ErrNotFound}, ReasonNumberNotFound},
		"number on other app":   {statusProvider{appLive: true, number: telnyx.PhoneNumber{ID: "num-1", PhoneNumber: testNumber, Status: "active", ConnectionID: otherAppID}}, ReasonNumberNotOnApp},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			v := h.create(adminAlice, validInput())
			h.svc.provider = func(string) (Provider, error) { return c.prov, nil }
			res, err := h.svc.Validate(ctx, adminAlice, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if res.ConnectivityStatus != storage.ConnectivityFailed || res.ConnectivityReason != c.reason {
				t.Fatalf("result = %+v, want failed/%s", res, c.reason)
			}
		})
	}

	t.Run("healthy stub verifies", func(t *testing.T) {
		h := newHarness(t)
		v := h.create(adminAlice, validInput())
		h.svc.provider = func(string) (Provider, error) {
			return statusProvider{appLive: true, number: good}, nil
		}
		res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
		if err != nil || res.ConnectivityStatus != storage.ConnectivityVerified {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})

	t.Run("provider factory failure fails closed", func(t *testing.T) {
		h := newHarness(t)
		v := h.create(adminAlice, validInput())
		h.svc.provider = func(string) (Provider, error) { return nil, errors.New("no client") }
		res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
		if err != nil || res.ConnectivityReason != ReasonSecretsUnavailable {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})

	t.Run("profile without a connection skips the application check", func(t *testing.T) {
		h := newHarness(t)
		in := validInput()
		in.ConnectionID = ""
		v := h.create(adminAlice, in)
		h.svc.provider = func(string) (Provider, error) {
			return statusProvider{number: good}, nil
		}
		res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
		if err != nil || res.ConnectivityStatus != storage.ConnectivityVerified {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})
}

func TestPrincipalAndHelpers(t *testing.T) {
	admin := Principal(&identity.Principal{User: storage.User{ID: "alice", Role: "admin"}})
	if admin != adminAlice {
		t.Fatalf("admin actor = %+v", admin)
	}
	user := Principal(&identity.Principal{User: storage.User{ID: "bob", Role: "user"}})
	if user.Admin || user.UserID != "bob" {
		t.Fatalf("user actor = %+v", user)
	}
	if roleOf(admin) != "admin" || roleOf(user) != "user" {
		t.Fatal("roleOf mismatch")
	}
	if access("alice", "alice") != storage.TelephonyAccessOwner || access("bob", "alice") != storage.TelephonyAccessGranted {
		t.Fatal("access mismatch")
	}
}

func TestAuditWriteFailureDoesNotFailTheAction(t *testing.T) {
	h := newHarness(t)
	h.store.auditErr = errors.New("audit down")
	v := h.create(adminAlice, validInput())
	if v.ID == "" {
		t.Fatal("create failed when only audit failed")
	}
}

func TestSecretErrorsCarryNoKeyMaterial(t *testing.T) {
	h := newHarness(t)
	h.sec.noKey = true
	_, err := h.svc.Create(context.Background(), adminAlice, validInput())
	if err == nil || strings.Contains(err.Error(), goodKey) {
		t.Fatalf("error leaked key or is empty: %v", err)
	}
	if !errors.Is(err, ErrSecretsDown) {
		t.Fatalf("err = %v, want ErrSecretsDown", err)
	}
}

func TestSecretResolveDoesNotCrossOwnersOrPurposes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	ref := h.store.profiles[v.ID].APISecretID
	if _, err := h.sec.Resolve(ctx, "bob", ref, secrets.PurposeTelnyxAPIKey); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("cross-owner resolve = %v", err)
	}
	if _, err := h.sec.Resolve(ctx, "alice", ref, secrets.PurposeTelnyxWebhookKey); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("wrong-purpose resolve = %v", err)
	}
	if n := len(h.fake.recorded()); n != 0 {
		t.Fatalf("resolve reached the provider %d times", n)
	}
}
