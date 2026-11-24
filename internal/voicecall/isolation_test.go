package voicecall_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

// TestResolverIsOwnerScopedAndNeverLeaksKey checks that user B cannot resolve
// user A's AI profile, cannot bind their own profile to A's key, and that the
// resolved provider profile carries the key only for the owner.
func TestResolverIsOwnerScopedAndNeverLeaksKey(t *testing.T) {
	ctx := context.Background()
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/voice"

	f := newFixture(t)
	const aliceKey = "test-alice-key-not-real"
	f.addProfile(t, f.aliceID, "ai-alice", storage.AIProtocolRealtime, endpoint, aliceKey)

	bobID := f.otherID(t)
	f.addProfile(t, bobID, "ai-bob", storage.AIProtocolRealtime, endpoint, "sk-bob-own-3307")

	res, err := voicecall.NewResolver(f.store, f.secrets, pcmu, provider.Client())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := res.Resolve(ctx, bobID, "ai-alice"); !errors.Is(err, storage.ErrAIProfileNotFound) {
		t.Fatalf("bob resolving alice's profile must be not-found, got %v", err)
	}

	err = f.tryBindForeignSecret(t, bobID, "ai-bob-foreign", f.aliceSecretID(t))
	if err == nil {
		t.Fatal("storage let bob bind alice's secret to his profile")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("cross-owner bind rejected for the wrong reason: %v", err)
	}

	prof, err := res.Resolve(ctx, f.aliceID, "ai-alice")
	if err != nil {
		t.Fatalf("owner resolve: %v", err)
	}
	if prof.APIKey != aliceKey || prof.Protocol != realtime.ProtocolRealtime {
		t.Fatalf("owner profile %+v", prof)
	}
	bobProf, err := res.Resolve(ctx, bobID, "ai-bob")
	if err != nil {
		t.Fatalf("bob own profile: %v", err)
	}
	if strings.Contains(bobProf.APIKey, aliceKey) {
		t.Fatal("bob's resolved profile carries alice's key")
	}
}

// TestResolveFailsClosedWithoutAPIKey checks that a profile with no secret
// bound never starts a call with an empty bearer.
func TestResolveFailsClosedWithoutAPIKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	res, err := voicecall.NewResolver(f.store, f.secrets, pcmu, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateAIProfile(ctx, storage.AIProfile{
		ID: "ai-nokey", OwnerUserID: f.aliceID, Name: "nokey", Protocol: storage.AIProtocolRealtime,
		EndpointURL: "wss://ai.example.test/v1", Model: "m", Voice: "v",
		CreatedAt: when.Format("2006-01-02T15:04:05.000Z"), UpdatedAt: when.Format("2006-01-02T15:04:05.000Z"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Resolve(ctx, f.aliceID, "ai-nokey"); !errors.Is(err, voicecall.ErrNoAPIKey) {
		t.Fatalf("profile without key must fail closed, got %v", err)
	}
}

// TestResolverRejectsWrongPurposeSecret checks that a secret of another purpose
// cannot be used as the AI key.
func TestResolverRejectsWrongPurposeSecret(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	telnyxSecret, err := f.secrets.Put(ctx, f.aliceID, "telnyx", secrets.PurposeTelnyxAPIKey, []byte("KEY-telnyx-9"))
	if err != nil {
		t.Fatal(err)
	}
	ts := when.Format("2006-01-02T15:04:05.000Z")
	if err := f.store.CreateAIProfile(ctx, storage.AIProfile{
		ID: "ai-wrongpurpose", OwnerUserID: f.aliceID, Name: "wp", Protocol: storage.AIProtocolRealtime,
		EndpointURL: "wss://ai.example.test/v1", Model: "m", Voice: "v", APISecretID: telnyxSecret,
		CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := voicecall.NewResolver(f.store, f.secrets, pcmu, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Resolve(ctx, f.aliceID, "ai-wrongpurpose"); !errors.Is(err, voicecall.ErrNoAPIKey) {
		t.Fatalf("telnyx secret used as ai key must fail closed, got %v", err)
	}
}
