package service

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

func rawCursor(json string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(json))
}

// TestCursorTamperedPayloadIsRejected changes one field of a valid cursor at a
// time. The decoder must refuse each form instead of reading a position that
// the caller did not receive.
func TestCursorTamperedPayloadIsRejected(t *testing.T) {
	valid := encodeCursor(storage.CallCursor{CreatedAt: ts, ID: "call-1"})
	raw, err := base64.RawURLEncoding.DecodeString(valid)
	if err != nil {
		t.Fatal(err)
	}
	tampered := map[string]string{
		"version bumped":  rawCursor(strings.Replace(string(raw), `"v":1`, `"v":9`, 1)),
		"id swapped":      rawCursor(strings.Replace(string(raw), `"i":"call-1"`, `"i":"bob-1"`, 1)),
		"time shifted":    rawCursor(strings.Replace(string(raw), ts, "2000-01-01T00:00:00.000Z", 1)),
		"added field":     rawCursor(`{"v":1,"c":"` + ts + `","i":"call-1","admin":true}`),
		"nested array":    rawCursor(`{"v":1,"c":"` + ts + `","i":["call-1"]}`),
		"id null":         rawCursor(`{"v":1,"c":"` + ts + `","i":null}`),
		"padded base64":   valid + "==",
		"standard base64": strings.NewReplacer("-", "+", "_", "/").Replace(valid) + "=",
	}
	for name, token := range tampered {
		_, err := decodeCursor(token)
		if name == "time shifted" || name == "id swapped" {
			// Both are still well-formed cursors. They decode, and the position
			// they name is a different keyset point. That is the intended
			// behavior of an opaque token, which carries no signature.
			if err != nil {
				t.Fatalf("%s: well-formed cursor rejected: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: decodeCursor accepted %q", name, token)
		}
		mustErrCode(t, err, CodeInvalidInput)
	}
}

// TestCursorOddOffsetIsNormalizedToUTC asserts the behavior of a timestamp
// with a non-UTC offset: it names the same instant, so it is normalized to the
// storage layout in UTC rather than rejected. Sub-millisecond digits are
// truncated to the storage precision.
func TestCursorOddOffsetIsNormalizedToUTC(t *testing.T) {
	cases := map[string]string{
		"plus two hours":           "2026-11-23T12:00:00.000+02:00",
		"minus five thirty":        "2026-11-23T04:30:00.000-05:30",
		"utc without fraction":     "2026-11-23T10:00:00Z",
		"half second":              "2026-11-23T10:00:00.5Z",
		"sub-millisecond truncate": "2026-11-23T10:00:00.123456789Z",
	}
	want := map[string]string{
		"plus two hours":           "2026-11-23T10:00:00.000Z",
		"minus five thirty":        "2026-11-23T10:00:00.000Z",
		"utc without fraction":     "2026-11-23T10:00:00.000Z",
		"half second":              "2026-11-23T10:00:00.500Z",
		"sub-millisecond truncate": "2026-11-23T10:00:00.123Z",
	}
	for name, stamp := range cases {
		got, err := decodeCursor(rawCursor(`{"v":1,"c":"` + stamp + `","i":"x"}`))
		if err != nil || got == nil {
			t.Fatalf("%s: decodeCursor(%q) err=%v", name, stamp, err)
		}
		if got.CreatedAt != want[name] {
			t.Fatalf("%s: CreatedAt = %q, want %q", name, got.CreatedAt, want[name])
		}
	}
}

// TestCursorOffsetSamePositionAsUTC checks the keyset effect: a cursor that
// names an instant with an offset must select the same page as the UTC form.
func TestCursorOffsetSamePositionAsUTC(t *testing.T) {
	store := &fakeStore{calls: []storage.Call{
		sampleCall("c2", "alice", "2026-11-23T10:00:00.000Z"),
		sampleCall("c1", "alice", "2026-11-22T10:00:00.000Z"),
	}}
	svc := New(store)
	utcCursor := rawCursor(`{"v":1,"c":"2026-11-23T10:00:00.000Z","i":"c2"}`)
	offsetCursor := rawCursor(`{"v":1,"c":"2026-11-23T12:00:00.000+02:00","i":"c2"}`)
	a, err := svc.ListCalls(context.Background(), alice, ListCallsInput{Cursor: utcCursor})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.ListCalls(context.Background(), alice, ListCallsInput{Cursor: offsetCursor})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(callIDs(a.Calls), ",") != strings.Join(callIDs(b.Calls), ",") || store.lastAfter == nil {
		t.Fatalf("utc page %v != offset page %v", callIDs(a.Calls), callIDs(b.Calls))
	}
	if store.lastAfter.CreatedAt != "2026-11-23T10:00:00.000Z" {
		t.Fatalf("store received %q, want the UTC storage layout", store.lastAfter.CreatedAt)
	}
}

// TestCursorMaxLengthAtServiceBoundary checks the 512-byte limit at the
// service. A token of exactly 512 bytes that is malformed is invalid_input, and
// a token over 512 bytes is rejected before any decode attempt.
func TestCursorMaxLengthAtServiceBoundary(t *testing.T) {
	store := &fakeStore{}
	svc := New(store)

	over := strings.Repeat("A", MaxCursorLength+1)
	_, err := svc.ListCalls(context.Background(), alice, ListCallsInput{Cursor: over})
	mustErrCode(t, err, CodeInvalidInput)
	if !strings.Contains(err.Error(), "512") {
		t.Fatalf("over-length error does not name the limit: %v", err)
	}
	if store.lastOwner != "" {
		t.Fatal("over-length cursor reached the store")
	}

	atLimit := strings.Repeat("A", MaxCursorLength)
	_, err = svc.ListCalls(context.Background(), alice, ListCallsInput{Cursor: atLimit})
	mustErrCode(t, err, CodeInvalidInput)
}

// TestCursorMissingMeansFirstPage confirms that an empty cursor is the first
// page, not an error.
func TestCursorMissingMeansFirstPage(t *testing.T) {
	store := &fakeStore{calls: []storage.Call{sampleCall("c1", "alice", ts)}}
	svc := New(store)
	res, err := svc.ListCalls(context.Background(), alice, ListCallsInput{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if store.lastAfter != nil || len(res.Calls) != 1 {
		t.Fatalf("first page after=%v calls=%d", store.lastAfter, len(res.Calls))
	}
}
