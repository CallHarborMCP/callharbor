package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

type fakeStore struct {
	telephony []storage.TelephonyProfile
	ai        []storage.AIProfile
	calls     []storage.Call
	telErr    error
	aiErr     error
	listErr   error
	getErr    error

	lastOwner  string
	lastStatus string
	lastLimit  int
	lastAfter  *storage.CallCursor
}

func (f *fakeStore) ListUsableTelephonyProfiles(_ context.Context, owner string) ([]storage.UsableTelephonyProfile, error) {
	f.lastOwner = owner
	if f.telErr != nil {
		return nil, f.telErr
	}
	var out []storage.UsableTelephonyProfile
	for _, p := range f.telephony {
		if p.OwnerUserID == owner {
			if p.ConnectivityStatus == "" {
				p.ConnectivityStatus = "unknown" // the stored default
			}
			out = append(out, storage.UsableTelephonyProfile{TelephonyProfile: p, Access: "owner"})
		}
	}
	return out, nil
}

func (f *fakeStore) ListAIProfiles(_ context.Context, owner string) ([]storage.AIProfile, error) {
	f.lastOwner = owner
	if f.aiErr != nil {
		return nil, f.aiErr
	}
	var out []storage.AIProfile
	for _, p := range f.ai {
		if p.OwnerUserID == owner {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) ListCalls(_ context.Context, owner, status string, after *storage.CallCursor, limit int) ([]storage.Call, error) {
	f.lastOwner, f.lastStatus, f.lastAfter, f.lastLimit = owner, status, after, limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []storage.Call
	for _, c := range f.calls {
		if c.OwnerUserID != owner || (status != "" && c.Status != status) {
			continue
		}
		if after != nil && c.CreatedAt >= after.CreatedAt && (c.CreatedAt != after.CreatedAt || c.ID >= after.ID) {
			continue
		}
		out = append(out, c)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) GetCall(_ context.Context, owner, id string) (storage.Call, error) {
	if f.getErr != nil {
		return storage.Call{}, f.getErr
	}
	for _, c := range f.calls {
		if c.ID == id && c.OwnerUserID == owner {
			return c, nil
		}
	}
	return storage.Call{}, storage.ErrCallNotFound
}

const ts = "2026-11-23T10:00:00.000Z"

var alice = Actor{UserID: "alice", Scopes: []string{ScopeRead, ScopeCall}}

func sampleCall(id, owner, created string) storage.Call {
	return storage.Call{
		ID: id, OwnerUserID: owner, TelephonyProfileID: "tel", AIProfileID: "ai",
		ToNumber: "+14155550111", Status: "queued", MaxDurationSeconds: 300,
		CreatedAt: created, UpdatedAt: created,
	}
}

func mustErrCode(t *testing.T, err error, want Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s", want)
	}
	if got := CodeOf(err); got != want {
		t.Fatalf("code = %s (%v), want %s", got, err, want)
	}
}

func TestNormalizeLimit(t *testing.T) {
	cases := map[int]int{0: DefaultLimit, 1: 1, 50: 50, 100: 100}
	for in, want := range cases {
		got, err := NormalizeLimit(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeLimit(%d) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []int{-1, 101, 1000} {
		if _, err := NormalizeLimit(bad); err == nil {
			t.Fatalf("NormalizeLimit(%d) returned no error", bad)
		} else {
			mustErrCode(t, err, CodeInvalidInput)
		}
	}
}

func TestCursorRoundTripAndRejection(t *testing.T) {
	want := storage.CallCursor{CreatedAt: ts, ID: "call-1"}
	token := encodeCursor(want)
	if strings.ContainsAny(token, "+/=") {
		t.Fatalf("cursor %q is not base64url unpadded", token)
	}
	got, err := decodeCursor(token)
	if err != nil || got == nil || *got != want {
		t.Fatalf("decodeCursor = %+v, %v; want %+v", got, err, want)
	}
	if got, err := decodeCursor(""); err != nil || got != nil {
		t.Fatalf("empty cursor = %+v, %v; want nil, nil", got, err)
	}

	bad := map[string]string{
		"not base64":       "!!!",
		"not json":         base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"unknown field":    base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"c":"` + ts + `","i":"x","z":1}`)),
		"wrong version":    base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"c":"` + ts + `","i":"x"}`)),
		"missing id":       base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"c":"` + ts + `","i":""}`)),
		"bad time":         base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"c":"yesterday","i":"x"}`)),
		"oversized id":     base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"c":"` + ts + `","i":"` + strings.Repeat("a", 65) + `"}`)),
		"trailing garbage": base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"c":"` + ts + `","i":"x"} junk`)),
	}
	for name, token := range bad {
		if _, err := decodeCursor(token); err == nil {
			t.Fatalf("%s: decodeCursor accepted %q", name, token)
		} else {
			mustErrCode(t, err, CodeInvalidInput)
		}
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	valid := []string{"a", "key-1", "k_" + strings.Repeat("x", 126), "ünïcode-ok", "!~"}
	for _, k := range valid {
		if err := ValidateIdempotencyKey(k); err != nil {
			t.Fatalf("ValidateIdempotencyKey(%q) = %v, want nil", k, err)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("x", 129),
		"has space",
		"tab\there",
		"new\nline",
		"nul\x00byte",
		"del\x7f",
		"bad-utf8-\xff",
	}
	for _, k := range invalid {
		err := ValidateIdempotencyKey(k)
		if err == nil {
			t.Fatalf("ValidateIdempotencyKey(%q) = nil, want error", k)
		}
		mustErrCode(t, err, CodeInvalidInput)
	}
	// 128 runes is allowed even when the byte length is larger.
	if err := ValidateIdempotencyKey(strings.Repeat("é", 128)); err != nil {
		t.Fatalf("128 multibyte runes rejected: %v", err)
	}
}

func TestGetProfilesReturnsOnlyNonSecretFieldsForOwner(t *testing.T) {
	store := &fakeStore{
		telephony: []storage.TelephonyProfile{
			{ID: "tel-a", OwnerUserID: "alice", Name: "main", Provider: "telnyx", CallerNumber: "+14155550100", CreatedAt: ts, UpdatedAt: ts},
			{ID: "tel-b", OwnerUserID: "bob", Name: "other", Provider: "telnyx", CallerNumber: "+14155550200", CreatedAt: ts, UpdatedAt: ts},
		},
		ai: []storage.AIProfile{
			{
				ID: "ai-a", OwnerUserID: "alice", Name: "voice", Protocol: storage.AIProtocolLive,
				EndpointURL: "wss://secret.example.test/path?token=abc", Model: "m1", Voice: "alloy",
				Instructions: "private instructions", Headers: map[string]string{"X-Secret": "yes"},
				APISecretID: "alice-ai-key", CreatedAt: ts, UpdatedAt: ts,
			},
		},
	}
	res, err := New(store).GetProfiles(context.Background(), alice)
	if err != nil {
		t.Fatalf("GetProfiles: %v", err)
	}
	if store.lastOwner != "alice" {
		t.Fatalf("store queried owner %q, want alice", store.lastOwner)
	}
	if len(res.TelephonyProfiles) != 1 || res.TelephonyProfiles[0].ID != "tel-a" ||
		res.TelephonyProfiles[0].ConnectivityStatus != storage.ConnectivityUnknown ||
		res.TelephonyProfiles[0].Access != "owner" {
		t.Fatalf("telephony = %+v", res.TelephonyProfiles)
	}
	if len(res.AIProfiles) != 1 {
		t.Fatalf("ai = %+v", res.AIProfiles)
	}
	ai := res.AIProfiles[0]
	if ai.PrimaryTransport != "websocket" || ai.Protocol != "live_ws" || ai.Model != "m1" || ai.Voice != "alloy" {
		t.Fatalf("ai view = %+v", ai)
	}
	// Serialized output must not contain the endpoint, headers, instructions or secret ids.
	for _, forbidden := range []string{"secret.example.test", "X-Secret", "private instructions", "alice-ai-key", "endpoint", "headers", "api_secret"} {
		if strings.Contains(fmtJSON(t, res), forbidden) {
			t.Fatalf("profiles output leaks %q: %s", forbidden, fmtJSON(t, res))
		}
	}
}

func TestGetProfilesEmptyIsNonNilAndPropagatesErrors(t *testing.T) {
	res, err := New(&fakeStore{}).GetProfiles(context.Background(), alice)
	if err != nil {
		t.Fatalf("GetProfiles empty: %v", err)
	}
	if res.AIProfiles == nil || res.TelephonyProfiles == nil {
		t.Fatalf("empty lists must be non-nil so they serialize as []: %+v", res)
	}
	_, err = New(&fakeStore{telErr: errors.New("db down: secret=xyz")}).GetProfiles(context.Background(), alice)
	mustErrCode(t, err, CodeInternal)
	if strings.Contains(AsError(err).Message, "xyz") {
		t.Fatalf("internal message leaked cause: %q", AsError(err).Message)
	}
	_, err = New(&fakeStore{aiErr: errors.New("boom")}).GetProfiles(context.Background(), alice)
	mustErrCode(t, err, CodeInternal)
}

func TestListCallsPaginatesWithOpaqueCursor(t *testing.T) {
	store := &fakeStore{calls: []storage.Call{
		sampleCall("c3", "alice", "2026-11-24T10:00:00.000Z"),
		sampleCall("c2", "alice", ts),
		sampleCall("c1", "alice", ts),
		sampleCall("bob-1", "bob", "2026-11-24T11:00:00.000Z"),
	}}
	svc := New(store)

	page1, err := svc.ListCalls(context.Background(), alice, ListCallsInput{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if got := callIDs(page1.Calls); strings.Join(got, ",") != "c3,c2" || page1.NextCursor == "" {
		t.Fatalf("page1 = %v cursor=%q", got, page1.NextCursor)
	}
	if store.lastLimit != 3 {
		t.Fatalf("store fetched %d rows, want limit+1 = 3", store.lastLimit)
	}
	page2, err := svc.ListCalls(context.Background(), alice, ListCallsInput{Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if got := callIDs(page2.Calls); strings.Join(got, ",") != "c1" || page2.NextCursor != "" {
		t.Fatalf("page2 = %v cursor=%q", got, page2.NextCursor)
	}
	for _, c := range append(page1.Calls, page2.Calls...) {
		if strings.HasPrefix(c.CallID, "bob") {
			t.Fatalf("foreign call leaked: %s", c.CallID)
		}
	}
}

func TestListCallsDefaultLimitAndStatusFilter(t *testing.T) {
	store := &fakeStore{calls: []storage.Call{sampleCall("c1", "alice", ts)}}
	svc := New(store)
	res, err := svc.ListCalls(context.Background(), alice, ListCallsInput{Status: "queued"})
	if err != nil {
		t.Fatalf("ListCalls: %v", err)
	}
	if store.lastLimit != DefaultLimit+1 || store.lastStatus != "queued" {
		t.Fatalf("store limit=%d status=%q", store.lastLimit, store.lastStatus)
	}
	if len(res.Calls) != 1 || res.Calls[0].CallID != "c1" {
		t.Fatalf("calls = %+v", res.Calls)
	}
}

func TestListCallsRejectsInvalidInputBeforeStore(t *testing.T) {
	store := &fakeStore{}
	svc := New(store)
	cases := map[string]ListCallsInput{
		"bad limit":   {Limit: 101},
		"bad cursor":  {Cursor: "%%%"},
		"bad status":  {Status: "dialing"},
		"status case": {Status: "Queued"},
		"negative":    {Limit: -5},
	}
	for name, in := range cases {
		_, err := svc.ListCalls(context.Background(), alice, in)
		mustErrCode(t, err, CodeInvalidInput)
		if store.lastOwner != "" {
			t.Fatalf("%s: store was reached", name)
		}
	}
	for _, status := range CallStates() {
		if _, ok := callStates[status]; !ok {
			t.Fatalf("status %q missing from the state set", status)
		}
	}
	if got := len(CallStates()); got != len(callStates) {
		t.Fatalf("CallStates has %d entries, set has %d", got, len(callStates))
	}
}

func TestListCallsStoreFailureIsInternal(t *testing.T) {
	_, err := New(&fakeStore{listErr: errors.New("sql: secret detail")}).ListCalls(context.Background(), alice, ListCallsInput{})
	mustErrCode(t, err, CodeInternal)
	if strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("error text leaked the cause: %v", err)
	}
}

func TestGetCallShapeOwnershipAndNotFound(t *testing.T) {
	ended := sampleCall("c1", "alice", ts)
	ended.Status = "ended"
	ended.Outcome = "success"
	ended.EndedAt = ts
	ended.HasTranscript = true
	ended.RecordingStatus = "complete"
	ended.RecordRequested = true
	ended.RecordEffective = true
	ended.OutcomeReasonCode = "completed"
	store := &fakeStore{calls: []storage.Call{ended, sampleCall("bob-call", "bob", ts)}}
	svc := New(store)

	got, err := svc.GetCall(context.Background(), alice, "c1")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if got.CallID != "c1" || got.TechnicalStatus != "ended" || got.TaskOutcome == nil || *got.TaskOutcome != "success" {
		t.Fatalf("call = %+v", got)
	}
	if !got.TranscriptAvailable || !got.RecordingAvailable || got.EndedAt == nil || *got.EndedAt != ts {
		t.Fatalf("availability/ended = %+v", got)
	}
	if got.Details.OutcomeReasonCode != "completed" || !got.Details.RecordEffective || got.Details.MaxDurationSeconds != 300 {
		t.Fatalf("details = %+v", got.Details)
	}
	if !strings.Contains(fmtJSON(t, got), `"call_id":"c1"`) || strings.Contains(fmtJSON(t, got), `"id":`) {
		t.Fatalf("get_call must expose call_id and not id: %s", fmtJSON(t, got))
	}

	_, err = svc.GetCall(context.Background(), alice, "bob-call")
	mustErrCode(t, err, CodeNotFound)
	_, err = svc.GetCall(context.Background(), alice, "missing")
	mustErrCode(t, err, CodeNotFound)
	_, err = svc.GetCall(context.Background(), alice, "")
	mustErrCode(t, err, CodeInvalidInput)
	_, err = svc.GetCall(context.Background(), alice, strings.Repeat("c", 65))
	mustErrCode(t, err, CodeInvalidInput)
	_, err = New(&fakeStore{getErr: errors.New("io")}).GetCall(context.Background(), alice, "c1")
	mustErrCode(t, err, CodeInternal)
}

func TestOpenCallFieldsOmittedWhenUnset(t *testing.T) {
	store := &fakeStore{calls: []storage.Call{sampleCall("c1", "alice", ts)}}
	got, err := New(store).GetCall(context.Background(), alice, "c1")
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if got.TaskOutcome != nil || got.EndedAt != nil {
		t.Fatalf("open call must report null outcome and ended_at: %+v", got)
	}
	body := fmtJSON(t, got)
	if !strings.Contains(body, `"task_outcome":null`) || !strings.Contains(body, `"ended_at":null`) {
		t.Fatalf("nulls missing from %s", body)
	}
}

func TestActorRequireEnforcesIdentityAndScope(t *testing.T) {
	if err := (Actor{}).Require(ScopeRead); err == nil {
		t.Fatal("anonymous actor accepted")
	} else {
		mustErrCode(t, err, CodeUnauthenticated)
	}
	readOnly := Actor{UserID: "alice", Scopes: []string{ScopeRead}}
	if err := readOnly.Require(ScopeRead); err != nil {
		t.Fatalf("read scope rejected: %v", err)
	}
	if err := readOnly.Require(ScopeCall); err == nil {
		t.Fatal("missing call scope accepted")
	} else {
		mustErrCode(t, err, CodeInsufficientScope)
	}
	noScope := Actor{UserID: "alice"}
	_, err := New(&fakeStore{}).GetProfiles(context.Background(), noScope)
	mustErrCode(t, err, CodeInsufficientScope)
	_, err = New(&fakeStore{}).ListCalls(context.Background(), noScope, ListCallsInput{})
	mustErrCode(t, err, CodeInsufficientScope)
	_, err = New(&fakeStore{}).GetCall(context.Background(), noScope, "c1")
	mustErrCode(t, err, CodeInsufficientScope)
}

func TestErrorTaxonomyStatusMapping(t *testing.T) {
	want := map[Code]int{
		CodeUnauthenticated:   401,
		CodeForbidden:         403,
		CodeInsufficientScope: 403,
		CodeNotFound:          404,
		CodeInvalidInput:      400,
		CodeConflict:          409,
		CodeInternal:          500,
	}
	for code, status := range want {
		if got := code.HTTPStatus(); got != status {
			t.Fatalf("%s status = %d, want %d", code, got, status)
		}
	}
	if CodeOf(errors.New("raw")) != CodeInternal {
		t.Fatal("unknown error not mapped to internal")
	}
	if AsError(errors.New("raw")).Code != CodeInternal {
		t.Fatal("AsError did not wrap unknown error")
	}
	wrapped := fmt.Errorf("ctx: %w", newError(CodeConflict, "x"))
	if CodeOf(wrapped) != CodeConflict {
		t.Fatal("wrapped service error lost its code")
	}
	if e := internal(errors.New("cause")); !errors.Is(e, e.Cause) || e.Error() != "internal: internal error" {
		t.Fatalf("internal error = %v", e)
	}
}

func callIDs(calls []CallView) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.CallID)
	}
	return out
}

func fmtJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := marshalJSON(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
