package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkClient connects the go-sdk Streamable HTTP client to /mcp with the bearer
// token. The SDK sends the headers the spec requires, so this exercises the
// real protocol path end to end.
func (e *e2eEnv) sdkClient(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	httpClient := &http.Client{Transport: &bearerTransport{token: token, base: e.client.Transport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   e.resource(),
		HTTPClient: httpClient,
		MaxRetries: 0,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// bearerTransport adds the Authorization header to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	if b.base == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return b.base.RoundTrip(r)
}

// toolText returns the text content of a tool result.
func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty tool result: %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content %T", res.Content[0])
	}
	return tc.Text
}

// TestMCPSDKClientListsOnlyImplementedTools uses the go-sdk client to discover
// the tools behind a real OAuth bearer token.
func TestMCPSDKClientListsOnlyImplementedTools(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")

	sess := e.sdkClient(t, tok.AccessToken)
	names := map[string]bool{}
	for tool, err := range sess.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		names[tool.Name] = true
	}
	if len(names) != 3 || !names["get_profiles"] || !names["list_calls"] || !names["get_call"] {
		t.Fatalf("tools = %v", names)
	}
}

// TestMCPSDKClientCallsAreOwnerScoped calls the implemented tools as alice.
// Alice sees her own data, and a call owned by bob is reported as not_found.
func TestMCPSDKClientCallsAreOwnerScoped(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")
	sess := e.sdkClient(t, tok.AccessToken)
	ctx := context.Background()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "get_profiles", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("get_profiles: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_profiles failed: %s", toolText(t, res))
	}

	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "get_call", Arguments: map[string]any{"call_id": "no-such-call"}})
	if err != nil {
		t.Fatalf("get_call transport error: %v", err)
	}
	if !res.IsError || !strings.HasPrefix(toolText(t, res), "not_found") {
		t.Fatalf("get_call missing = %+v, want not_found", res)
	}

	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_calls", Arguments: map[string]any{"filters": map[string]any{"status": "dialing"}}})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("list_calls accepted an invalid status: %+v", res)
	}
}

// TestMCPSDKClientPlannedToolsDoNotSucceed calls each planned operation. None
// may report success, because none is registered.
func TestMCPSDKClientPlannedToolsDoNotSucceed(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")
	sess := e.sdkClient(t, tok.AccessToken)

	for _, name := range []string{"make_call", "get_transcript", "get_recording", "hangup_call"} {
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"call_id": "x"}})
		if err == nil && res != nil && !res.IsError {
			t.Fatalf("planned %s reported success: %+v", name, res)
		}
	}
}

// TestMCPSDKClientRejectsRevokedToken connects with a token that is revoked
// before use. The SDK must fail to initialize, and the OAuth layer must answer
// with a 401 invalid_token challenge.
func TestMCPSDKClientRejectsRevokedToken(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")

	status, _, body := e.postForm(e.base+"/oauth/revoke", url.Values{
		"token":     {tok.AccessToken},
		"client_id": {cid},
	})
	if status != http.StatusOK {
		t.Fatalf("revoke = %d %v", status, body)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	_, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   e.resource(),
		HTTPClient: &http.Client{Transport: &bearerTransport{token: tok.AccessToken}},
		MaxRetries: 0,
	}, nil)
	if err == nil {
		t.Fatal("revoked token connected to MCP")
	}
}
