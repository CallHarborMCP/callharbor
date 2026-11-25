package mcp

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

const toolsManifest = "../../api/mcp/tools.json"

type manifest struct {
	Tools []struct {
		Name        string          `json:"name"`
		Status      string          `json:"status"`
		Scope       string          `json:"scope"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"tools"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile(toolsManifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

// listRegistered connects an in-process client to a server built the same way
// the HTTP endpoint builds it, and lists the tools it actually registers.
func listRegistered(t *testing.T) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	h := New(service.New(&fakeStore{}), "test", nil)
	srv := h.newServer()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "drift", Version: "1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = sess.Close() }()
	var out []*mcp.Tool
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		out = append(out, tool)
	}
	return out
}

// TestRegisteredToolsMatchManifest fails if a tool is registered without a
// manifest entry, or if a manifest tool marked implemented is missing.
func TestRegisteredToolsMatchManifest(t *testing.T) {
	m := loadManifest(t)
	registered := map[string]bool{}
	for _, tool := range listRegistered(t) {
		registered[tool.Name] = true
	}
	implemented := map[string]bool{}
	planned := map[string]bool{}
	for _, tool := range m.Tools {
		switch tool.Status {
		case "implemented":
			implemented[tool.Name] = true
		case "planned":
			planned[tool.Name] = true
		default:
			t.Fatalf("tool %s has unknown status %q", tool.Name, tool.Status)
		}
	}
	for name := range implemented {
		if !registered[name] {
			t.Errorf("manifest implements %s but it is not registered", name)
		}
	}
	for name := range registered {
		if !implemented[name] {
			t.Errorf("registered tool %s is not implemented in the manifest", name)
		}
	}
	for name := range planned {
		if registered[name] {
			t.Errorf("planned tool %s must not be registered", name)
		}
	}
	want := []string{ToolGetCall, ToolGetProfiles, ToolListCalls}
	got := sortedKeys(registered)
	if len(got) != len(want) {
		t.Fatalf("registered = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registered = %v, want %v", got, want)
		}
	}
}

// TestPlannedToolsAreUnknownAndNeverSucceed calls each planned operation by name.
// The SDK must report the tool as unknown, which is an error, not a success.
func TestPlannedToolsAreUnknownAndNeverSucceed(t *testing.T) {
	ctx := context.Background()
	h := New(service.New(&fakeStore{}), "test", nil)
	srv := h.newServer()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "planned", Version: "1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	for _, name := range []string{"make_call", "get_transcript", "get_recording", "hangup_call"} {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"call_id": "x"}})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("planned tool %s succeeded: %+v", name, res)
		}
	}
}

// TestManifestSchemasMatchRegisteredSchemas compares the advertised input
// schema of each implemented tool with the schema the SDK actually serves.
func TestManifestSchemasMatchRegisteredSchemas(t *testing.T) {
	m := loadManifest(t)
	registered := map[string]*mcp.Tool{}
	for _, tool := range listRegistered(t) {
		registered[tool.Name] = tool
	}
	for _, entry := range m.Tools {
		if entry.Status != "implemented" {
			continue
		}
		tool := registered[entry.Name]
		if tool == nil {
			t.Fatalf("%s not registered", entry.Name)
		}
		want := normalizeSchema(t, entry.InputSchema)
		got := normalizeSchema(t, marshalSchema(t, tool.InputSchema))
		if want != got {
			t.Errorf("%s input schema drifted\nmanifest: %s\nregistered: %s", entry.Name, want, got)
		}
	}
}

// TestStatusEnumMatchesServiceStates ensures the list_calls enum equals the
// service's complete state set, in order.
func TestStatusEnumMatchesServiceStates(t *testing.T) {
	var got []string
	for _, v := range callStatusEnum {
		got = append(got, v.(string))
	}
	want := []string{"queued", "initiating", "ringing", "active", "ending", "ended", "failed"}
	if len(got) != len(want) {
		t.Fatalf("enum = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("enum = %v, want %v", got, want)
		}
	}
}

// TestListCallsSchemaRejectsInvalidArguments validates concrete argument
// objects against the served schema with a JSON Schema 2020-12 validator, so
// unknown statuses, oversized limits and extra fields are refused.
func TestListCallsSchemaRejectsInvalidArguments(t *testing.T) {
	s, err := listCallsSchema()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Resolve(&jsonschema.ResolveOptions{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	valid := []string{`{}`, `{"limit":100}`, `{"filters":{"status":"ended"}}`, `{"cursor":"abc"}`}
	for _, raw := range valid {
		if err := resolved.Validate(decodeArgs(t, raw)); err != nil {
			t.Fatalf("valid %s rejected: %v", raw, err)
		}
	}
	invalid := []string{
		`{"filters":{"status":"dialing"}}`,
		`{"limit":0}`,
		`{"limit":101}`,
		`{"owner_user_id":"bob"}`,
		`{"filters":null}`,
	}
	for _, raw := range invalid {
		if err := resolved.Validate(decodeArgs(t, raw)); err == nil {
			t.Fatalf("invalid %s accepted", raw)
		}
	}
}

func decodeArgs(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func marshalSchema(t *testing.T, s any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return b
}

// normalizeSchema reduces a schema to a canonical string so key order does
// not matter.
func normalizeSchema(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normalize marshal: %v", err)
	}
	return string(b)
}

var _ = jsonschema.Schema{}
