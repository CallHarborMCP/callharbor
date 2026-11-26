package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// fakeTranscripts is an in-memory TranscriptStore. Turns and the summary read
// as empty for a caller who does not own the call, the same as the storage
// layer. Audit writes are recorded.
type fakeTranscripts struct {
	turns    map[string][]storage.TranscriptTurn // by call id
	summary  storage.TranscriptSummary
	owners   map[string]string // call id -> owner
	audits   []storage.AuditEvent
	auditErr error
	turnsErr error
	summErr  error

	lastAfter int
	lastLimit int
}

func (f *fakeTranscripts) ListTranscriptTurns(_ context.Context, owner, callID string, afterSeq, limit int) ([]storage.TranscriptTurn, error) {
	f.lastAfter, f.lastLimit = afterSeq, limit
	if f.turnsErr != nil {
		return nil, f.turnsErr
	}
	if f.owners[callID] != owner {
		return []storage.TranscriptTurn{}, nil
	}
	var out []storage.TranscriptTurn
	for _, t := range f.turns[callID] {
		if t.Seq > afterSeq && len(out) < limit {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeTranscripts) GetTranscriptSummary(_ context.Context, owner, callID string) (storage.TranscriptSummary, error) {
	if f.summErr != nil {
		return storage.TranscriptSummary{}, f.summErr
	}
	if f.owners[callID] != owner {
		return storage.TranscriptSummary{QualityFlags: []string{}}, nil
	}
	return f.summary, nil
}

func (f *fakeTranscripts) InsertAudit(_ context.Context, e storage.AuditEvent) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

// turnsFor builds n final callee turns with seq 1..n.
func turnsFor(n int) []storage.TranscriptTurn {
	out := make([]storage.TranscriptTurn, 0, n)
	for seq := 1; seq <= n; seq++ {
		out = append(out, storage.TranscriptTurn{
			TranscriptRevision: storage.TranscriptRevision{
				Seq: seq, Speaker: storage.SpeakerCallee, Finality: storage.FinalityFinal,
				StartedMS: int64(seq) * 10, Text: fmt.Sprintf("turn %d", seq), Fidelity: "played",
				TimingSource: storage.TimingProvider, SourceIDs: []string{}, Quality: []string{},
			},
			Revision: 1, RecordedAt: ts,
		})
	}
	return out
}

// transcriptService returns a service over an owned call "c1" for alice and a
// foreign call "bob-call" for bob, with the given transcript store.
func transcriptService(tr *fakeTranscripts) *Service {
	store := &fakeStore{calls: []storage.Call{
		sampleCall("c1", "alice", ts),
		sampleCall("bob-call", "bob", ts),
	}}
	if tr.owners == nil {
		tr.owners = map[string]string{"c1": "alice", "bob-call": "bob"}
	}
	svc := New(store).WithTranscripts(tr)
	svc.now = func() time.Time { return time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC) }
	return svc
}

func TestGetTranscriptRequiresReadScope(t *testing.T) {
	svc := transcriptService(&fakeTranscripts{})
	_, err := svc.GetTranscript(context.Background(), Actor{UserID: "alice"}, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeInsufficientScope)
	_, err = svc.GetTranscript(context.Background(), Actor{}, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeUnauthenticated)
}

func TestGetTranscriptUnavailableWithoutStore(t *testing.T) {
	store := &fakeStore{calls: []storage.Call{sampleCall("c1", "alice", ts)}}
	_, err := New(store).GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeUnavailable)
}

func TestGetTranscriptRejectsInvalidCallID(t *testing.T) {
	tr := &fakeTranscripts{}
	svc := transcriptService(tr)
	for _, id := range []string{"", strings.Repeat("c", 65)} {
		_, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: id})
		mustErrCode(t, err, CodeInvalidInput)
	}
	if len(tr.audits) != 0 {
		t.Fatalf("invalid id wrote %d audit events; it must not reach the store", len(tr.audits))
	}
}

func TestGetTranscriptForeignAndMissingCallAreNotFound(t *testing.T) {
	tr := &fakeTranscripts{turns: map[string][]storage.TranscriptTurn{"bob-call": turnsFor(2)}}
	svc := transcriptService(tr)

	for _, id := range []string{"bob-call", "missing"} {
		_, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: id})
		mustErrCode(t, err, CodeNotFound)
		if err.Error() != "not_found: call not found" {
			t.Fatalf("message for %s = %q, want the GetCall message", id, err.Error())
		}
	}
	if len(tr.audits) != 2 {
		t.Fatalf("audit events = %d, want 2 denied", len(tr.audits))
	}
	for _, a := range tr.audits {
		if a.Outcome != "denied" || a.DetailJSON != `{"reason":"not_found"}` || a.ActorUserID != "alice" {
			t.Fatalf("denied audit = %+v", a)
		}
	}
}

func TestGetTranscriptSuccessAuditsAndShape(t *testing.T) {
	tr := &fakeTranscripts{
		turns:   map[string][]storage.TranscriptTurn{"c1": turnsFor(2)},
		summary: storage.TranscriptSummary{Recorded: true, Finalized: true, QualityFlags: []string{"corrected", "two_party_incomplete"}},
	}
	svc := transcriptService(tr)
	page, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	if page.CallID != "c1" || len(page.Turns) != 2 || page.NextCursor != "" || !page.Finalized {
		t.Fatalf("page = %+v", page)
	}
	if strings.Join(page.QualityFlags, ",") != "corrected,two_party_incomplete" {
		t.Fatalf("flags = %v", page.QualityFlags)
	}
	turn := page.Turns[0]
	if turn.CorrelationID != nil || turn.ProviderItemID != nil || turn.EndedMS != nil || turn.SourceIDs == nil || turn.QualityFlags == nil {
		t.Fatalf("unset provenance must be null and arrays non-nil: %+v", turn)
	}

	if len(tr.audits) != 1 {
		t.Fatalf("audit events = %d, want 1", len(tr.audits))
	}
	a := tr.audits[0]
	want := storage.AuditEvent{
		OccurredAt: "2026-11-23T10:00:00.000Z", ActorUserID: "alice", ActorRole: "user", TargetUserID: "alice",
		Action: "transcript.read", ResourceType: "call", ResourceID: "c1", Outcome: "success", DetailJSON: `{"turns":2}`,
	}
	if a != want {
		t.Fatalf("success audit = %+v, want %+v", a, want)
	}
}

func TestGetTranscriptPagesWithOpaqueCursor(t *testing.T) {
	tr := &fakeTranscripts{turns: map[string][]storage.TranscriptTurn{"c1": turnsFor(MaxLimit + 3)}}
	svc := transcriptService(tr)

	first, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Turns) != MaxLimit || first.NextCursor == "" {
		t.Fatalf("first page = %d turns, cursor %q", len(first.Turns), first.NextCursor)
	}
	if tr.lastLimit != MaxLimit+1 || tr.lastAfter != 0 {
		t.Fatalf("store read limit=%d after=%d, want limit+1 after 0", tr.lastLimit, tr.lastAfter)
	}
	if strings.ContainsAny(first.NextCursor, "+/=") {
		t.Fatalf("cursor %q is not base64url unpadded", first.NextCursor)
	}

	second, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1", Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Turns) != 3 || second.Turns[0].Seq != MaxLimit+1 || second.NextCursor != "" {
		t.Fatalf("second page = %d turns from seq %d, cursor %q", len(second.Turns), second.Turns[0].Seq, second.NextCursor)
	}
	if tr.lastAfter != MaxLimit {
		t.Fatalf("store read after=%d, want %d", tr.lastAfter, MaxLimit)
	}
}

func TestGetTranscriptEmptyTurnsAreNonNil(t *testing.T) {
	tr := &fakeTranscripts{}
	page, err := transcriptService(tr).GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Turns == nil || page.QualityFlags == nil {
		t.Fatalf("empty arrays must be non-nil: %+v", page)
	}
	body := fmtJSON(t, page)
	for _, want := range []string{`"turns":[]`, `"quality_flags":["no_media_session"]`, `"next_cursor":""`, `"finalized":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s lacks %s", body, want)
		}
	}
}

func TestGetTranscriptMalformedCursorIsInvalidBeforeStore(t *testing.T) {
	tr := &fakeTranscripts{}
	svc := transcriptService(tr)
	bad := map[string]string{
		"not base64":    "!!!",
		"not json":      base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"unknown field": base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":2,"x":1}`)),
		"wrong version": base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"s":2}`)),
		"zero seq":      base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":0}`)),
		"negative seq":  base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":-4}`)),
		"call cursor":   encodeCursor(storage.CallCursor{CreatedAt: ts, ID: "c1"}),
		"trailing junk": base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":2} junk`)),
		"oversized":     strings.Repeat("a", MaxCursorLength+1),
	}
	for name, token := range bad {
		_, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1", Cursor: token})
		if err == nil {
			t.Fatalf("%s: cursor accepted", name)
		}
		mustErrCode(t, err, CodeInvalidInput)
	}
	if len(tr.audits) != 0 {
		t.Fatalf("malformed cursor wrote %d audit events", len(tr.audits))
	}
}

func TestGetTranscriptAuditFailureOnSuccessIsInternal(t *testing.T) {
	tr := &fakeTranscripts{
		turns:    map[string][]storage.TranscriptTurn{"c1": turnsFor(1)},
		auditErr: errors.New("disk full: secret path /var/x"),
	}
	_, err := transcriptService(tr).GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeInternal)
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("audit cause leaked: %v", err)
	}
}

func TestGetTranscriptAuditFailureOnDeniedIsInternal(t *testing.T) {
	tr := &fakeTranscripts{auditErr: errors.New("db locked")}
	_, err := transcriptService(tr).GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "missing"})
	mustErrCode(t, err, CodeInternal)
}

func TestGetTranscriptStoreFailuresAreInternal(t *testing.T) {
	owned := &fakeTranscripts{turnsErr: errors.New("sql: secret detail")}
	_, err := transcriptService(owned).GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeInternal)
	if strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("store cause leaked: %v", err)
	}

	summ := &fakeTranscripts{summErr: errors.New("boom")}
	_, err = transcriptService(summ).GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeInternal)
}

func TestGetTranscriptCallLookupFailureIsInternal(t *testing.T) {
	svc := New(&fakeStore{getErr: errors.New("io")}).WithTranscripts(&fakeTranscripts{})
	_, err := svc.GetTranscript(context.Background(), alice, GetTranscriptInput{CallID: "c1"})
	mustErrCode(t, err, CodeInternal)
}

func TestPageFlagsDerivation(t *testing.T) {
	cases := []struct {
		name string
		sum  storage.TranscriptSummary
		want string
	}{
		{"no media run", storage.TranscriptSummary{}, "no_media_session"},
		{"in progress", storage.TranscriptSummary{Recorded: true}, "transcript_in_progress"},
		{"finalized clean", storage.TranscriptSummary{Recorded: true, Finalized: true}, ""},
		{
			"stored flags deduped and sorted",
			storage.TranscriptSummary{Recorded: true, Finalized: true, QualityFlags: []string{"events_dropped", "corrected", "corrected"}},
			"corrected,events_dropped",
		},
		{
			"in progress adds its flag to stored ones",
			storage.TranscriptSummary{Recorded: true, QualityFlags: []string{"provider_turn_incomplete"}},
			"provider_turn_incomplete,transcript_in_progress",
		},
	}
	for _, c := range cases {
		got := pageFlags(c.sum)
		if got == nil {
			t.Fatalf("%s: flags nil", c.name)
		}
		if strings.Join(got, ",") != c.want {
			t.Fatalf("%s: flags = %v, want %q", c.name, got, c.want)
		}
	}
}

func TestDecodeTranscriptCursorRoundTrip(t *testing.T) {
	seq, err := decodeTranscriptCursor(encodeTranscriptCursor(42))
	if err != nil || seq != 42 {
		t.Fatalf("round trip = %d, %v; want 42", seq, err)
	}
	if seq, err := decodeTranscriptCursor(""); err != nil || seq != 0 {
		t.Fatalf("empty = %d, %v; want 0, nil", seq, err)
	}
}
