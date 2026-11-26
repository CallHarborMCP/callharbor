package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CallHarborMCP/callharbor/internal/oauth"
	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// These tests run the real Handler, the real tools, the real service, and the
// go-sdk client over HTTP. The only stand-in is the token verifier: testTokens
// maps a bearer token to the principal the OAuth layer would have validated.
// The OAuth wiring itself is covered in internal/oauth. The go-sdk in-memory
// transport cannot carry TokenInfo, so an HTTP server is the honest path.

// testTokens maps a bearer token to the TokenInfo its verifier reports.
type testTokens map[string]auth.TokenInfo

func (tt testTokens) verifier() auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		ti, ok := tt[token]
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &ti, nil
	}
}

func tokenFor(userID string, scopes ...string) auth.TokenInfo {
	return auth.TokenInfo{UserID: userID, Scopes: scopes, Expiration: time.Now().Add(time.Hour)}
}

const (
	tsNewer = "2026-11-23T10:00:00.000Z"
	tsOlder = "2026-11-22T10:00:00.000Z"
)

// fixtureStore holds two owners' data. Alice's calls are c-a1 (newer) and
// c-a2 (older); Bob's call c-b1 must never appear in Alice's results.
func fixtureStore() *fakeStore {
	return &fakeStore{
		telephony: []storage.TelephonyProfile{
			{ID: "tel-a", OwnerUserID: "alice", Name: "Alice line", Provider: "telnyx", CallerNumber: "+14155550100", ConnectionID: "conn-secret-a", CreatedAt: tsOlder, UpdatedAt: tsOlder},
			{ID: "tel-b", OwnerUserID: "bob", Name: "Bob line", Provider: "telnyx", CallerNumber: "+14155550200", ConnectionID: "conn-secret-b", CreatedAt: tsOlder, UpdatedAt: tsOlder},
		},
		ai: []storage.AIProfile{
			{ID: "ai-a", OwnerUserID: "alice", Name: "Alice voice", Protocol: storage.AIProtocolRealtime, EndpointURL: "wss://alice.example/secret", Model: "m1", Voice: "v1", Instructions: "alice secret instructions", Headers: map[string]string{"X-Hidden": "h"}, APISecretID: "sec-a", CreatedAt: tsOlder, UpdatedAt: tsOlder},
		},
		calls: []storage.Call{
			{ID: "c-a1", OwnerUserID: "alice", TelephonyProfileID: "tel-a", AIProfileID: "ai-a", ToNumber: "+14155550111", Status: "ended", Outcome: "success", CreatedAt: tsNewer, UpdatedAt: tsNewer, EndedAt: tsNewer},
			{ID: "c-a2", OwnerUserID: "alice", TelephonyProfileID: "tel-a", AIProfileID: "ai-a", ToNumber: "+14155550112", Status: "failed", CreatedAt: tsOlder, UpdatedAt: tsOlder},
			{ID: "c-b1", OwnerUserID: "bob", TelephonyProfileID: "tel-b", AIProfileID: "ai-a", ToNumber: "+14155550222", Status: "ended", CreatedAt: tsNewer, UpdatedAt: tsNewer, EndedAt: tsNewer},
		},
	}
}

// testServer serves a real Handler behind the SDK bearer check, using the test
// verifier in place of the OAuth verifier.
func testServer(t *testing.T, store *fakeStore, logger *slog.Logger, tokens testTokens) *httptest.Server {
	t.Helper()
	h := New(service.New(store).WithTranscripts(store), "test", logger)
	ts := httptest.NewServer(auth.RequireBearerToken(tokens.verifier(), nil)(h))
	t.Cleanup(ts.Close)
	return ts
}

type bearerRoundTripper struct{ token string }

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// connect opens a go-sdk client session to ts with the bearer token.
func connect(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token}},
		MaxRetries: 0,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func call(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// failure asserts a tool error: IsError set, no structured content, and a text
// body that is the shared envelope. It returns the code, message and raw text.
func failure(t *testing.T, res *mcp.CallToolResult) (string, string, string) {
	t.Helper()
	if res == nil || !res.IsError {
		t.Fatalf("want tool error, got %+v", res)
	}
	if res.StructuredContent != nil {
		t.Fatalf("error result carries structured content: %v", res.StructuredContent)
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("error content is %T, want text", res.Content[0])
	}
	var env service.Envelope
	if err := json.Unmarshal([]byte(tc.Text), &env); err != nil {
		t.Fatalf("error text is not the envelope: %v: %s", err, tc.Text)
	}
	return env.Error.Code, env.Error.Message, tc.Text
}

func decodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool failed: %+v", res)
	}
	var out T
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content: %v: %s", err, raw)
	}
	return out
}

func callIDs(calls []service.CallView) string {
	ids := make([]string, 0, len(calls))
	for _, c := range calls {
		ids = append(ids, c.CallID)
	}
	return strings.Join(ids, ",")
}

// toolsListBody is a stateless JSON-RPC request that needs no initialize.
const toolsListBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// TestToolSuccessReturnsOwnedDataOnly covers the success path of all three
// tools with the real SDK and service. Alice must see only her own rows, and
// get_profiles must omit secret and endpoint fields. The owner's connection_id
// is allowed here: the owner reads their own configuration. Grantee views omit
// it (see the service and REST tests).
func TestToolSuccessReturnsOwnedDataOnly(t *testing.T) {
	logger, _ := captureLogger()
	ts := testServer(t, fixtureStore(), logger, testTokens{"tok-alice": tokenFor("alice", service.ScopeRead)})
	sess := connect(t, ts, "tok-alice")

	profRes := call(t, sess, ToolGetProfiles, map[string]any{})
	raw, err := json.Marshal(profRes.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"connection_id":"conn-secret-a"`) {
		t.Fatalf("owner get_profiles lacks connection_id: %s", raw)
	}
	for _, banned := range []string{"alice secret instructions", "wss://alice", "X-Hidden", "sec-a", "Bob line", "+14155550200"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("get_profiles leaked %q: %s", banned, raw)
		}
	}
	prof := decodeStructured[service.ProfilesResult](t, profRes)
	if len(prof.TelephonyProfiles) != 1 || len(prof.AIProfiles) != 1 {
		t.Fatalf("profiles = %d telephony, %d ai; want 1 and 1", len(prof.TelephonyProfiles), len(prof.AIProfiles))
	}
	if prof.TelephonyProfiles[0].ConnectivityStatus != "unknown" {
		t.Fatalf("connectivity_status = %q, want unknown", prof.TelephonyProfiles[0].ConnectivityStatus)
	}

	list := decodeStructured[ListCallsOutput](t, call(t, sess, ToolListCalls, map[string]any{}))
	if got := callIDs(list.Calls); got != "c-a1,c-a2" {
		t.Fatalf("list_calls = %q, want only Alice's calls newest first", got)
	}

	one := decodeStructured[service.CallView](t, call(t, sess, ToolGetCall, map[string]any{"call_id": "c-a1"}))
	if one.CallID != "c-a1" || one.TechnicalStatus != "ended" {
		t.Fatalf("get_call = %+v", one)
	}
}

// TestToolListCallsPaginatesAndFilters covers the keyset cursor, the status
// filter, and the input rejections that the MCP schema and service enforce.
func TestToolListCallsPaginatesAndFilters(t *testing.T) {
	logger, _ := captureLogger()
	ts := testServer(t, fixtureStore(), logger, testTokens{"tok-alice": tokenFor("alice", service.ScopeRead)})
	sess := connect(t, ts, "tok-alice")

	p1 := decodeStructured[ListCallsOutput](t, call(t, sess, ToolListCalls, map[string]any{"limit": 1}))
	if callIDs(p1.Calls) != "c-a1" || p1.NextCursor == "" {
		t.Fatalf("page 1 = %q cursor=%q", callIDs(p1.Calls), p1.NextCursor)
	}
	p2 := decodeStructured[ListCallsOutput](t, call(t, sess, ToolListCalls, map[string]any{"limit": 1, "cursor": p1.NextCursor}))
	if callIDs(p2.Calls) != "c-a2" || p2.NextCursor != "" {
		t.Fatalf("page 2 = %q cursor=%q", callIDs(p2.Calls), p2.NextCursor)
	}

	failed := decodeStructured[ListCallsOutput](t, call(t, sess, ToolListCalls, map[string]any{"filters": map[string]any{"status": "failed"}}))
	if callIDs(failed.Calls) != "c-a2" {
		t.Fatalf("status filter = %q, want c-a2", callIDs(failed.Calls))
	}

	badCursor := call(t, sess, ToolListCalls, map[string]any{"cursor": "not a cursor!"})
	if code, _, _ := failure(t, badCursor); code != "invalid_input" {
		t.Fatalf("bad cursor code = %q, want invalid_input", code)
	}

	// Over the 512-byte cap, the MCP schema rejects the request before the
	// handler runs, so the result is an SDK error rather than an envelope.
	long := call(t, sess, ToolListCalls, map[string]any{"cursor": strings.Repeat("A", service.MaxCursorLength+1)})
	if !long.IsError {
		t.Fatalf("over-length cursor was accepted: %+v", long)
	}

	bad := call(t, sess, ToolListCalls, map[string]any{"filters": map[string]any{"status": "bogus"}})
	if !bad.IsError {
		t.Fatalf("unknown status was accepted: %+v", bad)
	}
}

// TestToolErrorsAreEnvelopeWithoutStructuredContent is the B2 guard: every
// service failure is an IsError result with nil StructuredContent and a text
// body that parses as the shared envelope with the expected code.
func TestToolErrorsAreEnvelopeWithoutStructuredContent(t *testing.T) {
	logger, _ := captureLogger()
	ts := testServer(t, fixtureStore(), logger, testTokens{
		"tok-alice": tokenFor("alice", service.ScopeRead),
		"tok-call":  tokenFor("alice", service.ScopeCall),
		"tok-anon":  tokenFor("", service.ScopeRead),
	})
	cases := []struct {
		name  string
		token string
		tool  string
		args  map[string]any
		code  string
	}{
		{"foreign call is not_found", "tok-alice", ToolGetCall, map[string]any{"call_id": "c-b1"}, "not_found"},
		{"missing call is not_found", "tok-alice", ToolGetCall, map[string]any{"call_id": "nope"}, "not_found"},
		{"token without mcp:read is insufficient_scope", "tok-call", ToolGetProfiles, map[string]any{}, "insufficient_scope"},
		{"empty principal is unauthenticated", "tok-anon", ToolListCalls, map[string]any{}, "unauthenticated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := connect(t, ts, tc.token)
			code, msg, _ := failure(t, call(t, sess, tc.tool, tc.args))
			if code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			if msg == "" {
				t.Fatal("envelope message is empty")
			}
		})
	}
}

// TestToolInternalFailureLogsCauseAndNeverRendersIt is the N11 guard for the
// internal path. A store failure carries secret text. The caller must see only
// the generic envelope, and the server log must keep the cause.
func TestToolInternalFailureLogsCauseAndNeverRendersIt(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		args   map[string]any
		inject func(*fakeStore)
	}{
		{"get_call store failure", ToolGetCall, map[string]any{"call_id": "c-a1"}, func(s *fakeStore) { s.getErr = errInjected }},
		{"list_calls store failure", ToolListCalls, map[string]any{}, func(s *fakeStore) { s.listErr = errInjected }},
		{"get_profiles store failure", ToolGetProfiles, map[string]any{}, func(s *fakeStore) { s.telErr = errInjected }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore()
			tc.inject(store)
			logger, buf := captureLogger()
			ts := testServer(t, store, logger, testTokens{"tok": tokenFor("alice", service.ScopeRead)})
			sess := connect(t, ts, "tok")

			code, msg, text := failure(t, call(t, sess, tc.tool, tc.args))
			if code != "internal" || msg != "internal error" {
				t.Fatalf("envelope = %q %q, want internal and generic message", code, msg)
			}
			for _, secret := range []string{"hunter2", "pq:", "connection string"} {
				if strings.Contains(text, secret) {
					t.Fatalf("internal cause rendered to caller: %s", text)
				}
			}
			logged := buf.String()
			if !strings.Contains(logged, "hunter2") {
				t.Fatalf("cause was not logged server-side: %s", logged)
			}
			if !strings.Contains(logged, "code=internal") || !strings.Contains(logged, "transport=mcp") {
				t.Fatalf("log line lacks code and transport: %s", logged)
			}
		})
	}
}

// TestMissingPrincipalIsServerErrorNeverOK is the N12 guard. Bearer middleware
// that accepts a request without attaching a principal is a wiring bug. The
// request must fail as a server error, never 200, and must be logged.
// TestIncompletePrincipalIsLoggedWiringFault checks the verifier mapping for a
// principal that is attached but has no user or no expiry. Both are wiring
// faults: the result must be a logged, non-ErrInvalidToken error, which the SDK
// turns into 500 rather than 401 or 200.
func TestIncompletePrincipalIsLoggedWiringFault(t *testing.T) {
	logger, buf := captureLogger()
	good := oauth.Principal{UserID: "u-alice", Scopes: []string{"mcp:read"}, ExpiresAt: time.Now().Add(time.Hour)}
	cases := map[string]oauth.Principal{
		"empty user":   {UserID: "", Scopes: good.Scopes, ExpiresAt: good.ExpiresAt},
		"zero expiry":  {UserID: good.UserID, Scopes: good.Scopes},
		"both missing": {},
	}
	for name, p := range cases {
		info, err := principalTokenInfo(logger, p, true)
		if err == nil || info != nil {
			t.Fatalf("%s: got info=%v err=%v, want an error and no info", name, info, err)
		}
		if errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("%s: returned ErrInvalidToken, which the SDK maps to 401", name)
		}
		if !errors.Is(err, errIncompletePrincipal) {
			t.Fatalf("%s: err = %v, want errIncompletePrincipal", name, err)
		}
	}
	if !strings.Contains(buf.String(), "principal is incomplete") {
		t.Fatalf("incomplete principal was not logged: %s", buf.String())
	}
	info, err := principalTokenInfo(logger, good, true)
	if err != nil || info == nil || info.UserID != good.UserID {
		t.Fatalf("complete principal: info=%v err=%v", info, err)
	}
}

func TestMissingPrincipalIsServerErrorNeverOK(t *testing.T) {
	logger, buf := captureLogger()
	h := New(service.New(fixtureStore()), "test", logger)
	passthrough := func(next http.Handler) http.Handler { return next }
	ts := httptest.NewServer(h.Wrap(passthrough, "https://callharbor.example/.well-known/oauth-protected-resource/mcp"))
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(toolsListBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer anything")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("request without a principal returned 200")
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), "without a principal") {
		t.Fatalf("missing principal was not logged: %s", buf.String())
	}
}

// loopbackRequest builds a tools/list request whose connection arrives on
// loopback but whose Host is a public hostname, the shape of a reverse proxy.
func loopbackRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(toolsListBody))
	req.Host = "callharbor.example"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}
	return req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.Addr(local)))
}

// TestLoopbackWithPublicHostIsServed is the B3 guard. The handler disables the
// SDK localhost rebinding check, so a loopback connection with a public Host
// and a valid bearer is served.
func TestLoopbackWithPublicHostIsServed(t *testing.T) {
	h := New(service.New(fixtureStore()), "test", slog.New(slog.DiscardHandler))
	handler := auth.RequireBearerToken(testTokens{"tok": tokenFor("alice", service.ScopeRead)}.verifier(), nil)(h)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest("tok"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ToolGetProfiles) {
		t.Fatalf("loopback with public host = %d %s", rec.Code, rec.Body.String())
	}
}

// TestLoopbackControlDefaultProtectionRejectsPublicHost is the control for B3.
// It shows that the same request is refused by the SDK default, so the B3 test
// above depends on DisableLocalhostProtection and not on the request shape.
func TestLoopbackControlDefaultProtectionRejectsPublicHost(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "control", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, loopbackRequest("tok"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("default SDK protection = %d, want 403", rec.Code)
	}
}
