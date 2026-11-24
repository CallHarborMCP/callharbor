package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFailuresForOneUserDoNotBlockOthersFromSameIP(t *testing.T) {
	svc, _, _, _ := fixtureWithUsers(t)
	ctx := context.Background()
	const ip = "198.51.100.60"
	for i := 0; i < throttleMaxFails; i++ {
		_, _ = svc.Login(ctx, "alice", "wrong wrong wrong", ip)
	}
	if _, err := svc.Login(ctx, "alice", pwUser, ip); !errors.Is(err, ErrThrottled) {
		t.Fatalf("locked user accepted: %v", err)
	}
	if _, err := svc.Login(ctx, "bob", pwUser, ip); err != nil {
		t.Fatalf("correct login for another user blocked by shared IP: %v", err)
	}
	if _, err := svc.Login(ctx, "root", pwAdmin, ip); err != nil {
		t.Fatalf("correct admin login blocked by shared IP: %v", err)
	}
}

func TestIPLockoutDeniesFailuresButNotCorrectCredentials(t *testing.T) {
	svc, _, _, _ := fixtureWithUsers(t)
	ctx := context.Background()
	const ip = "198.51.100.77"
	for i := 0; i < throttleIPMaxFails; i++ {
		_, _ = svc.Login(ctx, "ghost"+string(rune('a'+i%26))+string(rune('a'+i/26)), "wrong wrong wrong", ip)
	}
	if _, err := svc.Login(ctx, "ghostz", "wrong wrong wrong", ip); !errors.Is(err, ErrThrottled) {
		t.Fatalf("IP lockout not applied to failures: %v", err)
	}
	if _, err := svc.Login(ctx, "root", pwAdmin, ip); err != nil {
		t.Fatalf("correct admin login denied by IP lockout: %v", err)
	}
}

func TestInvalidUsernameDoesNotCreateUserThrottleRow(t *testing.T) {
	svc, _, store, _ := fixtureWithUsers(t)
	ctx := context.Background()
	long := strings.Repeat("x", 5000)
	_, _ = svc.Login(ctx, long, "wrong wrong wrong", "198.51.100.88")
	th, err := store.GetThrottle(ctx, userThrottleKey(long))
	if err != nil {
		t.Fatal(err)
	}
	if th.Failures != 0 || th.WindowStartedAt != "" {
		t.Fatalf("throttle row written for invalid username: %+v", th)
	}
}
