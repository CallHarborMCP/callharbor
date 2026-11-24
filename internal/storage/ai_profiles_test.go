package storage

import (
	"context"
	"errors"
	"path/filepath"
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
		APISecretID: owner + "-key",
		CreatedAt:   testTime,
		UpdatedAt:   testTime,
	}
}

func TestAIProfileRoundTripIsOwnerScoped(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "db.sqlite"))
	seedTenants(t, s)
	ctx := context.Background()

	want := aiProfile("alice-ai-2", "alice", "live_ws", "wss://ai.example.test/live")
	if err := s.CreateAIProfile(ctx, want); err != nil {
		t.Fatalf("CreateAIProfile: %v", err)
	}
	got, err := s.GetAIProfile(ctx, "alice", "alice-ai-2")
	if err != nil {
		t.Fatalf("GetAIProfile: %v", err)
	}
	if got != want {
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
	}
	for name, p := range cases {
		if err := s.CreateAIProfile(ctx, p); err == nil {
			t.Errorf("%s: CreateAIProfile succeeded, want rejection", name)
		}
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
	p.APISecretID = "alice-key"
	if err := s.CreateAIProfile(ctx, p); err == nil {
		t.Fatal("profile referencing another owner's secret was accepted")
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
