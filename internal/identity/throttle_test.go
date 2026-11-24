package identity

import (
	"context"
	"errors"
	"testing"
)

func TestSuccessfulLoginDoesNotResetIPThrottle(t *testing.T) {
	svc, _, _, _ := fixtureWithUsers(t)
	ctx := context.Background()
	const ip = "198.51.100.77"
	for i := 0; i < throttleMaxFails-1; i++ {
		_, _ = svc.Login(ctx, "ghost"+string(rune('a'+i)), "wrong wrong wrong", ip)
	}
	if _, err := svc.Login(ctx, "alice", pwUser, ip); err != nil {
		t.Fatalf("valid login: %v", err)
	}
	_, _ = svc.Login(ctx, "ghostz", "wrong wrong wrong", ip)
	if _, err := svc.Login(ctx, "bob", pwUser, ip); !errors.Is(err, ErrThrottled) {
		t.Fatalf("IP throttle reset by a successful login: %v", err)
	}
}
