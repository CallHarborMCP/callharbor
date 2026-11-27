package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// TestListUnconfirmedDialsSelectsOnlyDialsWithoutControlID covers the startup
// selection. A call in initiating, or in ending after a hangup during the dial,
// with no provider control id is returned. A call that has an id, or that is
// terminal, is not.
func TestListUnconfirmedDialsSelectsOnlyDialsWithoutControlID(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "u.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-initiating", "queued", "")
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET status = 'initiating' WHERE id = 'c-initiating'`); err != nil {
		t.Fatal(err)
	}
	seedLiveCall(t, s, "c-ending-no-id", "ending", "")
	seedLiveCall(t, s, "c-ending-with-id", "ending", "v3:ending")
	seedLiveCall(t, s, "c-ringing-with-id", "ringing", "v3:ringing")
	seedLiveCall(t, s, "c-failed", "failed", "")

	got, err := s.ListUnconfirmedDials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by created_at, then id. Both rows are inserted in the same test
	// second, so the id order decides the sequence here.
	want := []string{"c-ending-no-id", "c-initiating"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListUnconfirmedDials = %v, want %v", got, want)
	}
}

// TestMarkCallFailedFailsEndingWithoutControlID checks that recovery can fail an
// ending call that never got a control id, with dial_unconfirmed, and that it
// then leaves the live set.
func TestMarkCallFailedFailsEndingWithoutControlID(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "u.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-ending", "ending", "")

	if err := s.MarkCallFailed(ctx, "c-ending", "dial_unconfirmed", mustParse(testTime)); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err := s.db.QueryRow(`SELECT status, outcome_reason_code FROM calls WHERE id = ?`, "c-ending").
		Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "dial_unconfirmed" {
		t.Fatalf("status=%q reason=%q, want failed/dial_unconfirmed", status, reason)
	}
	legs, err := s.ListLiveCallLegs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 0 {
		t.Fatalf("live legs after fail = %d, want 0", len(legs))
	}
}
