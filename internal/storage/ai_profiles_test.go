package storage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func aiProfile(id, owner, protocol, endpoint string) AIProfile {
	return AIProfile{
		ID:          id,
		OwnerUserID: owner,
		Name:        id,
		Protocol:    AIProtocol(protocol),
		EndpointURL: endpoint,
		Model:       "voice-model",
		Voice:       "alloy",
		APISecretID: owner + "-ai-key",
		CreatedAt:   testTime,
		UpdatedAt:   testTime,
	}
}

func TestAIProfileRoundTripIsOwnerScoped(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	want := aiProfile("alice-ai-2", "alice", "live_ws", "wss://ai.example.test/live")
	want.Instructions = "Be brief."
	want.Headers = map[string]string{"X-Tenant": "alice"}
	if err := s.CreateAIProfile(ctx, want); err != nil {
		t.Fatalf("CreateAIProfile: %v", err)
	}
	got, err := s.GetAIProfile(ctx, "alice", "alice-ai-2")
	if err != nil {
		t.Fatalf("GetAIProfile: %v", err)
	}
	if got.ID != want.ID || got.Protocol != want.Protocol || got.EndpointURL != want.EndpointURL ||
		got.Model != want.Model || got.Voice != want.Voice || got.APISecretID != want.APISecretID ||
		got.Instructions != want.Instructions || got.Headers["X-Tenant"] != "alice" || len(got.Headers) != 1 {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if _, err := s.GetAIProfile(ctx, "bob", "alice-ai-2"); !errors.Is(err, ErrAIProfileNotFound) {
		t.Fatalf("cross-owner get err = %v, want ErrAIProfileNotFound", err)
	}
}

func TestAIProfileRejectsUnsafeConfig(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	cases := map[string]AIProfile{
		"unknown protocol": aiProfile("p1", "alice", "webrtc", "wss://ai.example.test/x"),
		"empty protocol":   aiProfile("p2", "alice", "", "wss://ai.example.test/x"),
		"https endpoint":   aiProfile("p3", "alice", "realtime_ws", "https://ai.example.test/x"),
		"remote plain ws":  aiProfile("p4", "alice", "realtime_ws", "ws://ai.example.test/x"),
		"no host":          aiProfile("p5", "alice", "realtime_ws", "wss:///x"),
		"unparseable":      aiProfile("p6", "alice", "realtime_ws", "://bad"),
		"missing model": func() AIProfile {
			p := aiProfile("p7", "alice", "realtime_ws", "wss://a.test/x")
			p.Model = " "
			return p
		}(),
		"missing voice": func() AIProfile { p := aiProfile("p8", "alice", "live_ws", "wss://a.test/x"); p.Voice = ""; return p }(),
		"reserved auth header": func() AIProfile {
			p := aiProfile("p9", "alice", "realtime_ws", "wss://a.test/x")
			p.Headers = map[string]string{"Authorization": "Bearer stolen"}
			return p
		}(),
		"header value with CRLF": func() AIProfile {
			p := aiProfile("p10", "alice", "realtime_ws", "wss://a.test/x")
			p.Headers = map[string]string{"X-Tenant": "a\r\nX-Evil: 1"}
			return p
		}(),
		"oversized instructions": func() AIProfile {
			p := aiProfile("p11", "alice", "live_ws", "wss://a.test/x")
			p.Instructions = strings.Repeat("a", MaxInstructions+1)
			return p
		}(),
	}
	for name, p := range cases {
		if err := s.CreateAIProfile(ctx, p); err == nil {
			t.Errorf("%s: CreateAIProfile succeeded, want rejection", name)
		}
	}
}

func TestAIProfileListAndUpdateAreOwnerScoped(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	if err := s.CreateAIProfile(ctx, aiProfile("alice-rt", "alice", "realtime_ws", "wss://ai.example.test/rt")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAIProfile(ctx, aiProfile("bob-rt", "bob", "realtime_ws", "wss://ai.example.test/rt")); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAIProfiles(ctx, "alice")
	if err != nil {
		t.Fatalf("ListAIProfiles: %v", err)
	}
	for _, p := range list {
		if p.OwnerUserID != "alice" {
			t.Fatalf("alice list leaked profile %q owned by %q", p.ID, p.OwnerUserID)
		}
	}
	if len(list) == 0 {
		t.Fatal("alice list is empty")
	}

	upd := aiProfile("alice-rt", "bob", "realtime_ws", "wss://ai.example.test/rt")
	upd.Model = "hijacked"
	if err := s.UpdateAIProfile(ctx, upd); !errors.Is(err, ErrAIProfileNotFound) {
		t.Fatalf("cross-owner update err = %v, want ErrAIProfileNotFound", err)
	}
	upd.OwnerUserID = "alice"
	upd.APISecretID = "alice-ai-key"
	upd.Model = "voice-model-2"
	if err := s.UpdateAIProfile(ctx, upd); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	got, err := s.GetAIProfile(ctx, "alice", "alice-rt")
	if err != nil || got.Model != "voice-model-2" {
		t.Fatalf("after owner update = %+v, %v", got, err)
	}
	bad := upd
	bad.Protocol = "webrtc"
	if err := s.UpdateAIProfile(ctx, bad); !errors.Is(err, ErrInvalidAIProfile) {
		t.Fatalf("invalid update err = %v, want ErrInvalidAIProfile", err)
	}
}

func TestAIProfileLoopbackPlainWSAllowed(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	if err := s.CreateAIProfile(ctx, aiProfile("local-ai-2", "alice", "realtime_ws", "ws://127.0.0.1:9000/realtime")); err != nil {
		t.Fatalf("loopback ws rejected: %v", err)
	}
}

func TestAIProfileRejectsForeignSecret(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	p := aiProfile("evil-ai", "bob", "realtime_ws", "wss://ai.example.test/x")
	p.APISecretID = "alice-ai-key"
	if err := s.CreateAIProfile(ctx, p); !errors.Is(err, ErrInvalidAIProfile) {
		t.Fatalf("foreign secret err = %v, want ErrInvalidAIProfile", err)
	}
}

func TestAIProfileRejectsWrongPurposeSecret(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	p := aiProfile("tel-as-ai", "alice", "realtime_ws", "wss://ai.example.test/x")
	p.APISecretID = "alice-key"
	if err := s.CreateAIProfile(ctx, p); !errors.Is(err, ErrInvalidAIProfile) {
		t.Fatalf("telnyx secret as ai key err = %v, want ErrInvalidAIProfile", err)
	}
	p.APISecretID = "no-such-secret"
	if err := s.CreateAIProfile(ctx, p); !errors.Is(err, ErrInvalidAIProfile) {
		t.Fatalf("missing secret err = %v, want ErrInvalidAIProfile", err)
	}
}

func TestAIProfileMissingSecretIsAllowed(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	p := aiProfile("nokey-ai", "alice", "realtime_ws", "wss://ai.example.test/x")
	p.APISecretID = ""
	if err := s.CreateAIProfile(ctx, p); err != nil {
		t.Fatalf("CreateAIProfile without secret: %v", err)
	}
	got, err := s.GetAIProfile(ctx, "alice", "nokey-ai")
	if err != nil || got.APISecretID != "" {
		t.Fatalf("get without secret = %+v, %v", got, err)
	}
}
