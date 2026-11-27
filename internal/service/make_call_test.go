package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

func ptr(n int) *int { return &n }

func validInput() MakeCallInput {
	return MakeCallInput{
		ProfileID: "tp-1", To: "+14155550111", Objective: "Confirm the appointment",
		IdempotencyKey: "req-1",
	}
}

func TestValidateMakeCallAcceptsMinimalAndDefaultsDuration(t *testing.T) {
	r, err := ValidateMakeCall(validInput(), 4096)
	if err != nil {
		t.Fatalf("minimal request: %v", err)
	}
	if r.MaxDuration != 600 {
		t.Fatalf("default max duration = %d, want 600", r.MaxDuration)
	}
	if len(r.SHA256) != 64 {
		t.Fatalf("digest length = %d", len(r.SHA256))
	}
}

func TestValidateMakeCallRejectsInvalidFields(t *testing.T) {
	cases := map[string]func(*MakeCallInput){
		"bad to":            func(in *MakeCallInput) { in.To = "4155550111" },
		"missing objective": func(in *MakeCallInput) { in.Objective = "  " },
		"bad language":      func(in *MakeCallInput) { in.Language = "english please" },
		"duration high":     func(in *MakeCallInput) { in.MaxDuration = ptr(7201) },
		"duration zero":     func(in *MakeCallInput) { in.MaxDuration = ptr(0) },
		"too many items": func(in *MakeCallInput) {
			in.Constraints = make([]string, maxListItems+1)
			for i := range in.Constraints {
				in.Constraints[i] = "c"
			}
		},
		"empty item":    func(in *MakeCallInput) { in.Fallbacks = []string{" "} },
		"long context":  func(in *MakeCallInput) { in.Context = strings.Repeat("x", maxContextRunes+1) },
		"bad profile":   func(in *MakeCallInput) { in.ProfileID = "" },
		"long instruct": func(in *MakeCallInput) { in.Instructions = strings.Repeat("y", maxInstructionsRunes+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			mutate(&in)
			if _, err := ValidateMakeCall(in, 1<<20); CodeOf(err) != CodeInvalidInput {
				t.Fatalf("err = %v, want invalid_input", err)
			}
		})
	}
}

func TestValidateMakeCallRejectsOversizedBriefWithoutTruncation(t *testing.T) {
	in := validInput()
	in.Context = strings.Repeat("c", 3000)
	_, err := ValidateMakeCall(in, 2048)
	if CodeOf(err) != CodeInvalidInput {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "brief_too_large") || !strings.Contains(err.Error(), "CALLHARBOR_MAX_CALL_BRIEF_BYTES") {
		t.Fatalf("message does not name the limit: %v", err)
	}
}

func TestBriefCanonicalJSONIsStableAndDigestMatches(t *testing.T) {
	a, err := ValidateMakeCall(validInput(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ValidateMakeCall(validInput(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a.CanonicalJSON != b.CanonicalJSON || a.SHA256 != b.SHA256 {
		t.Fatal("same request produced different canonical brief")
	}
	if !strings.Contains(a.CanonicalJSON, `"version":1`) {
		t.Fatalf("brief lacks version: %s", a.CanonicalJSON)
	}
}

// fakeCallStore is an in-memory CallStore for the operator tests.
type fakeCallStore struct {
	ais     []storage.AIProfile
	calls   map[string]storage.Call
	briefs  map[string]storage.CallBrief
	keys    map[string]string // call id -> owner + "\x00" + idempotency key
	inserts int
	// conflictOnInsert simulates a concurrent winner for the same key.
	conflictOnInsert bool
	// policies holds per-user overrides returned by GetUserCallPolicy.
	policies map[string]storage.UserCallPolicy
	// limitErr, when set, is returned by InsertCall before any write, as the
	// real store does when a concurrency or rate limit refuses the call.
	limitErr error
	// lastCost is the cost record of the most recent insert attempt.
	lastCost storage.CallCost
	// audits collects the audit rows written through RequestHangup and InsertAudit.
	audits []storage.AuditEvent
}

func newFakeCallStore(ais ...storage.AIProfile) *fakeCallStore {
	return &fakeCallStore{ais: ais, calls: map[string]storage.Call{}, briefs: map[string]storage.CallBrief{}, keys: map[string]string{}}
}

func (f *fakeCallStore) ListUsableTelephonyProfiles(context.Context, string) ([]storage.UsableTelephonyProfile, error) {
	return nil, nil
}
func (f *fakeCallStore) ListAIProfiles(context.Context, string) ([]storage.AIProfile, error) {
	return f.ais, nil
}
func (f *fakeCallStore) ListCalls(context.Context, string, string, *storage.CallCursor, int) ([]storage.Call, error) {
	return nil, nil
}
func (f *fakeCallStore) GetCall(context.Context, string, string) (storage.Call, error) {
	return storage.Call{}, storage.ErrCallNotFound
}
func (f *fakeCallStore) InsertCall(_ context.Context, c storage.CallInsert) error {
	f.inserts++
	if f.limitErr != nil {
		return f.limitErr
	}
	f.lastCost = c.Cost
	if f.conflictOnInsert {
		return storage.ErrCallIdempotencyConflict
	}
	kk := c.OwnerUserID + "\x00" + c.IdempotencyKey
	for _, existing := range f.keys {
		if existing == kk {
			return storage.ErrCallIdempotencyConflict
		}
	}
	f.keys[c.ID] = kk
	created := c.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	f.calls[c.ID] = storage.Call{
		ID: c.ID, OwnerUserID: c.OwnerUserID, TelephonyProfileID: c.TelephonyProfileID, AIProfileID: c.AIProfileID,
		ToNumber: c.ToNumber, Status: "queued", RecordRequested: c.RecordRequested,
		MaxDurationSeconds: c.MaxDurationSeconds, CreatedAt: created, UpdatedAt: created,
	}
	f.briefs[c.ID] = storage.CallBrief{Version: c.BriefVersion, ContentJSON: c.BriefJSON, SHA256: c.BriefSHA256}
	return nil
}
func (f *fakeCallStore) GetCallByIdempotencyKey(_ context.Context, owner, key string) (storage.Call, error) {
	want := owner + "\x00" + key
	for id, kk := range f.keys {
		if kk == want {
			return f.calls[id], nil
		}
	}
	return storage.Call{}, storage.ErrCallNotFound
}
func (f *fakeCallStore) GetUserCallPolicy(_ context.Context, userID string) (storage.UserCallPolicy, error) {
	if p, ok := f.policies[userID]; ok {
		return p, nil
	}
	return storage.UserCallPolicy{UserID: userID}, nil
}

// CallOwner reads the owner from the stored calls, as the real store does.
func (f *fakeCallStore) CallOwner(_ context.Context, callID string) (string, error) {
	c, ok := f.calls[callID]
	if !ok {
		return "", storage.ErrCallNotFound
	}
	return c.OwnerUserID, nil
}

// RequestHangup is a minimal stand-in: it records the audit row and reports the
// call's current status. The full transitions are tested in the storage package.
func (f *fakeCallStore) RequestHangup(_ context.Context, callID string, audit storage.AuditEvent, _ time.Time) (storage.HangupDecision, error) {
	c, ok := f.calls[callID]
	if !ok {
		return storage.HangupDecision{}, storage.ErrCallNotFound
	}
	f.audits = append(f.audits, audit)
	return storage.HangupDecision{Action: storage.HangupActionNone, Status: c.Status, Outcome: c.Outcome}, nil
}

// InsertAudit appends an audit row for inspection.
func (f *fakeCallStore) InsertAudit(_ context.Context, e storage.AuditEvent) error {
	f.audits = append(f.audits, e)
	return nil
}

func (f *fakeCallStore) GetCallBrief(_ context.Context, _, callID string) (storage.CallBrief, error) {
	b, ok := f.briefs[callID]
	if !ok {
		return storage.CallBrief{}, storage.ErrCallNotFound
	}
	return b, nil
}

// fakeResolver returns one fixed profile for every id.
type fakeResolver struct {
	p   telephonyprofile.Resolved
	err error
}

func (f fakeResolver) Resolve(context.Context, telephonyprofile.Actor, string) (telephonyprofile.Resolved, error) {
	return f.p, f.err
}

func readyProfile() telephonyprofile.Resolved {
	return telephonyprofile.Resolved{
		ID: "tp-1", OwnerUserID: "alice", ConnectionID: "conn-1", Access: "owner",
		WebhookPublicKey: "key", ConnectivityStatus: storage.ConnectivityVerified,
	}
}

func oneAI() storage.AIProfile {
	return storage.AIProfile{ID: "ai-1", OwnerUserID: "alice", Protocol: storage.AIProtocolRealtime}
}

func newOperator(t *testing.T, store CallStore, res CallResolver) *CallOperator {
	t.Helper()
	op, err := NewCallOperator(store, CallDeps{
		Resolver: res, MaxBriefBytes: 1 << 20,
		Now:       func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
		NewCallID: func() (string, error) { return "call_test", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

var callerAlice = Actor{UserID: "alice", Scopes: []string{ScopeRead, ScopeCall}}

func TestMakeCallQueuesOnceAndReplaysSameRequest(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := newOperator(t, store, fakeResolver{p: readyProfile()})
	first, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("MakeCall: %v", err)
	}
	if first.Status != "queued" || first.CallID != "call_test" {
		t.Fatalf("result = %+v", first)
	}
	second, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.CallID != first.CallID {
		t.Fatalf("replay call id = %s, want %s", second.CallID, first.CallID)
	}
	if store.inserts != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts)
	}
}

func TestMakeCallSameKeyDifferentRequestConflicts(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := newOperator(t, store, fakeResolver{p: readyProfile()})
	if _, err := op.MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatal(err)
	}
	changed := validInput()
	changed.To = "+14155550199"
	_, err := op.MakeCall(context.Background(), callerAlice, changed)
	if CodeOf(err) != CodeConflict || !strings.Contains(err.Error(), ReasonIdempotencyConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestMakeCallRaceOnKeyReturnsWinner(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := newOperator(t, store, fakeResolver{p: readyProfile()})
	if _, err := op.MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatal(err)
	}
	// Simulate a concurrent insert: the lookup misses, the insert reports a
	// conflict, and the re-read finds the winner.
	store.conflictOnInsert = true
	key := validInput()
	key.To = "+14155550111"
	winnerAgain, err := op.MakeCall(context.Background(), callerAlice, key)
	if err != nil {
		t.Fatalf("race replay with same request: %v", err)
	}
	if winnerAgain.CallID != "call_test" {
		t.Fatalf("race returned %s", winnerAgain.CallID)
	}
}

func TestMakeCallRefusesUnreadyTelephony(t *testing.T) {
	cases := map[string]struct {
		mutate func(*telephonyprofile.Resolved)
		code   string
	}{
		"unverified": {func(p *telephonyprofile.Resolved) { p.ConnectivityStatus = storage.ConnectivityUnknown }, ReasonTelephonyNotVerified},
		"failed":     {func(p *telephonyprofile.Resolved) { p.ConnectivityStatus = storage.ConnectivityFailed }, ReasonTelephonyNotVerified},
		"no conn":    {func(p *telephonyprofile.Resolved) { p.ConnectionID = "" }, ReasonTelephonyNoConnID},
		"no webhook": {func(p *telephonyprofile.Resolved) { p.WebhookPublicKey = "" }, ReasonTelephonyNoWebhook},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := readyProfile()
			tc.mutate(&p)
			store := newFakeCallStore(oneAI())
			op := newOperator(t, store, fakeResolver{p: p})
			_, err := op.MakeCall(context.Background(), callerAlice, validInput())
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if store.inserts != 0 {
				t.Fatal("refused call was written")
			}
		})
	}
}

func TestMakeCallAIProfileSelection(t *testing.T) {
	ctx := context.Background()
	none := newOperator(t, newFakeCallStore(), fakeResolver{p: readyProfile()})
	if _, err := none.MakeCall(ctx, callerAlice, validInput()); err == nil || !strings.Contains(err.Error(), ReasonAIProfileRequired) {
		t.Fatalf("zero AI profiles: %v", err)
	}
	two := newOperator(t, newFakeCallStore(oneAI(), storage.AIProfile{ID: "ai-2", OwnerUserID: "alice"}), fakeResolver{p: readyProfile()})
	if _, err := two.MakeCall(ctx, callerAlice, validInput()); err == nil || !strings.Contains(err.Error(), ReasonAIProfileAmbiguous) {
		t.Fatalf("two AI profiles: %v", err)
	}
}

func TestMakeCallForeignOrMissingProfileIsNotFound(t *testing.T) {
	op := newOperator(t, newFakeCallStore(oneAI()), fakeResolver{err: telephonyprofile.ErrNotFound})
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if CodeOf(err) != CodeNotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
}

func TestMakeCallNeedsActorAndKey(t *testing.T) {
	op := newOperator(t, newFakeCallStore(oneAI()), fakeResolver{p: readyProfile()})
	if _, err := op.MakeCall(context.Background(), Actor{}, validInput()); CodeOf(err) != CodeUnauthenticated {
		t.Fatalf("no actor = %v", err)
	}
	in := validInput()
	in.IdempotencyKey = ""
	if _, err := op.MakeCall(context.Background(), callerAlice, in); CodeOf(err) != CodeInvalidInput {
		t.Fatalf("no key = %v", err)
	}
}

// With no operator attached (no public URL or no master key), MakeCall refuses
// with calls_unavailable. It never reaches the store, so no row is written.
func TestMakeCallRefusesWhenCallsAreNotRunning(t *testing.T) {
	store := newFakeCallStore(oneAI())
	s := New(store)
	_, err := s.MakeCall(context.Background(), callerAlice, validInput())
	if store.inserts != 0 {
		t.Fatalf("refused call wrote %d rows", store.inserts)
	}
	if CodeOf(err) != CodeUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), ReasonCallsUnavailable) {
		t.Fatalf("refusal must carry the reason code: %v", err)
	}
	if CodeOf(err).HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", CodeOf(err).HTTPStatus())
	}
}

func TestMakeCallRequiresCallScope(t *testing.T) {
	op := newOperator(t, newFakeCallStore(oneAI()), fakeResolver{p: readyProfile()})
	readOnly := Actor{UserID: "alice", Scopes: []string{ScopeRead}}
	_, err := op.MakeCall(context.Background(), readOnly, validInput())
	if CodeOf(err) != CodeInsufficientScope {
		t.Fatalf("read-only actor: err = %v, want insufficient_scope", err)
	}
}

// countingResolver records each resolution, so a test can prove a replay never
// reaches profile resolution.
type countingResolver struct {
	inner fakeResolver
	calls int
}

func (c *countingResolver) Resolve(ctx context.Context, a telephonyprofile.Actor, id string) (telephonyprofile.Resolved, error) {
	c.calls++
	return c.inner.Resolve(ctx, a, id)
}

func TestReplayReturnsOriginalCallWithoutResolvingProfiles(t *testing.T) {
	store := newFakeCallStore(oneAI())
	first := &countingResolver{inner: fakeResolver{p: readyProfile()}}
	op := newOperator(t, store, first)
	orig, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatal(err)
	}
	// The profile is now unverified and its grant is gone. A retry must still
	// return the caller's own call, and must not consult the resolver at all.
	revoked := &countingResolver{inner: fakeResolver{err: telephonyprofile.ErrNotFound}}
	op2 := newOperator(t, store, revoked)
	again, err := op2.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("retry after revoke: %v", err)
	}
	if again.CallID != orig.CallID {
		t.Fatalf("retry call id = %s, want %s", again.CallID, orig.CallID)
	}
	if revoked.calls != 0 {
		t.Fatalf("retry resolved profiles %d times, want 0", revoked.calls)
	}
}

func TestReplayIgnoresAIProfileChanges(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := newOperator(t, store, fakeResolver{p: readyProfile()})
	orig, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatal(err)
	}
	// The user adds a second AI profile after the call was made. The retry
	// must still return the original call; it is not a conflict.
	store.ais = append(store.ais, storage.AIProfile{ID: "ai-2", OwnerUserID: "alice"})
	again, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("retry after AI change: %v", err)
	}
	if again.CallID != orig.CallID {
		t.Fatalf("retry call id = %s, want %s", again.CallID, orig.CallID)
	}
}

func TestReplayConflictsOnEachCompared(t *testing.T) {
	cases := map[string]func(*MakeCallInput){
		"profile":    func(in *MakeCallInput) { in.ProfileID = "tp-other" },
		"to":         func(in *MakeCallInput) { in.To = "+14155550199" },
		"record":     func(in *MakeCallInput) { in.Record = true },
		"duration":   func(in *MakeCallInput) { in.MaxDuration = ptr(300) },
		"objective":  func(in *MakeCallInput) { in.Objective = "Something else" },
		"context":    func(in *MakeCallInput) { in.Context = "new context" },
		"language":   func(in *MakeCallInput) { in.Language = "pt-BR" },
		"constraint": func(in *MakeCallInput) { in.Constraints = []string{"no upsell"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeCallStore(oneAI())
			op := newOperator(t, store, fakeResolver{p: readyProfile()})
			if _, err := op.MakeCall(context.Background(), callerAlice, validInput()); err != nil {
				t.Fatal(err)
			}
			changed := validInput()
			mutate(&changed)
			_, err := op.MakeCall(context.Background(), callerAlice, changed)
			if CodeOf(err) != CodeConflict || !strings.Contains(err.Error(), ReasonIdempotencyConflict) {
				t.Fatalf("err = %v, want idempotency_conflict", err)
			}
		})
	}
}

func TestReplayValidatesBeforeLookup(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := newOperator(t, store, fakeResolver{p: readyProfile()})
	bad := validInput()
	bad.To = "nope"
	if _, err := op.MakeCall(context.Background(), callerAlice, bad); CodeOf(err) != CodeInvalidInput {
		t.Fatalf("invalid input err = %v", err)
	}
	if store.inserts != 0 {
		t.Fatal("invalid request was written")
	}
}

// The limit applies to the full session text, profile instructions included.
// A request whose brief fits alone but whose profile pushes the rendered text
// over the limit is refused with brief_too_large before any insert, so it never
// gets a 202 for a call that media would later refuse.
func TestMakeCallRefusesFullRenderedTextOverLimitWithoutInsert(t *testing.T) {
	ai := oneAI()
	ai.Instructions = strings.Repeat("p", 4000)
	store := newFakeCallStore(ai)
	op, err := NewCallOperator(store, CallDeps{
		Resolver: fakeResolver{p: readyProfile()}, MaxBriefBytes: 4096,
		Now:       func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
		NewCallID: func() (string, error) { return "call_test", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	_, err = op.MakeCall(context.Background(), callerAlice, in)
	if err == nil || !strings.Contains(err.Error(), "brief_too_large") {
		t.Fatalf("oversize rendered text = %v, want brief_too_large", err)
	}
	if store.inserts != 0 {
		t.Fatalf("inserts = %d, want 0: an oversize request must not queue", store.inserts)
	}
}

// A request that fits with the profile instructions still queues.
func TestMakeCallQueuesWhenRenderedTextFits(t *testing.T) {
	ai := oneAI()
	ai.Instructions = "short profile"
	store := newFakeCallStore(ai)
	op := newOperator(t, store, fakeResolver{p: readyProfile()})
	if _, err := op.MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatalf("fitting request = %v", err)
	}
	if store.inserts != 1 {
		t.Fatalf("inserts = %d, want 1", store.inserts)
	}
}
