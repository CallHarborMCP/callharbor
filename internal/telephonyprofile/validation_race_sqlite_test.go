package telephonyprofile

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// editingProvider stands in for Telnyx. While validation is in flight it makes
// the owner's edit on the real database, moving updated_at, then answers the
// check as verified. The stale result must then be refused.
type editingProvider struct {
	db     *sql.DB
	id     string
	edited bool
}

func (p *editingProvider) GetCallControlApplication(ctx context.Context, _ string) (telnyx.CallControlApplication, error) {
	if !p.edited {
		p.edited = true
		if _, err := p.db.ExecContext(ctx,
			`UPDATE telephony_profiles SET name = 'renamed-mid-check', updated_at = '2026-11-25T11:30:00.000Z' WHERE id = ?`, p.id); err != nil {
			return telnyx.CallControlApplication{}, err
		}
	}
	return telnyx.CallControlApplication{ID: testAppID, Active: true}, nil
}

func (p *editingProvider) FindPhoneNumber(context.Context, string) (telnyx.PhoneNumber, error) {
	return telnyx.PhoneNumber{ID: "num-1", Status: "active", ConnectionID: testConnID}, nil
}

// TestValidationRaceAuditPersistsInRealSQLite runs the validation race over the
// real SQLite store, with the real migrations and the real audit CHECK constraint.
// It asserts three things: the validation is refused with ErrValidationRace, the
// stored connectivity is not overwritten, and the audit row is actually persisted
// with outcome failed and the fixed reason detail. The previous "conflict" outcome
// violated the CHECK and was dropped silently; this test would have caught it.
func TestValidationRaceAuditPersistsInRealSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ts := "2026-11-25T10:00:00.000Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id, username, role, password_hash, active, created_at, updated_at) VALUES
		('alice', 'alice', 'admin', 'hash', 1, ?, ?)`, ts, ts); err != nil {
		t.Fatal(err)
	}

	now := func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) }
	key := bytes.Repeat([]byte{7}, secrets.KeySize)
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(ring)
	if err != nil {
		t.Fatal(err)
	}
	secIDs, profIDs := 0, 0
	sec, err := secrets.NewService(box, store, now, func() string { secIDs++; return fmt.Sprintf("sec-%d", secIDs) })
	if err != nil {
		t.Fatal(err)
	}

	// The factory hands out whichever provider is current. Create does not call
	// the provider, so the race provider is armed only just before Validate.
	var provider Provider
	factory := func(string) (Provider, error) { return provider, nil }
	svc, err := New(store, sec, factory, now, func() string { profIDs++; return fmt.Sprintf("tp-%d", profIDs) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	alice := Actor{UserID: "alice", Admin: true}
	v, err := svc.Create(ctx, alice, Input{Name: "main", CallerNumber: testNumber, ConnectionID: testConnID,
		APIKey: "KEY-race-0123456789"}) // gitleaks:allow
	if err != nil {
		t.Fatal(err)
	}

	provider = &editingProvider{db: db, id: v.ID}
	_, err = svc.Validate(ctx, alice, v.ID)
	if !errors.Is(err, ErrValidationRace) {
		t.Fatalf("validate during edit = %v, want ErrValidationRace", err)
	}

	// The stored connectivity was not written: still unknown with no checked time.
	var status, checked string
	if err := db.QueryRowContext(ctx,
		`SELECT connectivity_status, COALESCE(connectivity_checked_at, '') FROM telephony_profiles WHERE id = ?`, v.ID).
		Scan(&status, &checked); err != nil {
		t.Fatal(err)
	}
	if status != storage.ConnectivityUnknown || checked != "" {
		t.Fatalf("stale validation was stored: status=%s checked=%q", status, checked)
	}

	// The audit row is really there, with the CHECK-allowed outcome and the reason.
	var outcome, detail, action string
	err = db.QueryRowContext(ctx, `SELECT action, outcome, detail_json FROM audit_events
		WHERE action = 'telephony_profile.validate' AND resource_id = ?`, v.ID).Scan(&action, &outcome, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("validation race wrote no audit row: the insert was dropped")
	}
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" || detail != raceAuditDetail {
		t.Fatalf("audit row = action %q outcome %q detail %q, want validate/failed/%s",
			action, outcome, detail, raceAuditDetail)
	}
}
