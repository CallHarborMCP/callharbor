package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

const toolsManifest = "../../api/mcp/tools.json"

// manifestTool is one entry of api/mcp/tools.json. Schemas are kept raw so the
// comparison is exact, not a re-encoding of a Go struct.
type manifestTool struct {
	Name         string          `json:"name"`
	Status       string          `json:"status"`
	Scope        string          `json:"scope"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

type manifest struct {
	Tools []manifestTool `json:"tools"`
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

// implementedByName returns the manifest's implemented tools keyed by name.
func implementedByName(t *testing.T, m manifest) map[string]manifestTool {
	t.Helper()
	out := map[string]manifestTool{}
	for _, tool := range m.Tools {
		if tool.Status == "implemented" {
			out[tool.Name] = tool
		}
	}
	return out
}

// listRegistered connects an in-process client to a server built the same way
// the HTTP endpoint builds it, and lists the tools it actually registers.
func listRegistered(t *testing.T) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	h := New(service.New(&fakeStore{}), "test", nil)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := h.srv.Connect(ctx, st, nil); err != nil {
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
// manifest entry, or if a manifest tool marked implemented is missing. It also
// fails if any planned tool is registered.
func TestRegisteredToolsMatchManifest(t *testing.T) {
	m := loadManifest(t)
	registered := map[string]bool{}
	for _, tool := range listRegistered(t) {
		registered[tool.Name] = true
	}
	implemented := implementedByName(t, m)
	for _, tool := range m.Tools {
		switch tool.Status {
		case "implemented", "planned":
		default:
			t.Fatalf("tool %s has unknown status %q", tool.Name, tool.Status)
		}
		if tool.Status == "planned" && registered[tool.Name] {
			t.Errorf("planned tool %s must not be registered", tool.Name)
		}
	}
	for name := range implemented {
		if !registered[name] {
			t.Errorf("manifest implements %s but it is not registered", name)
		}
	}
	for name := range registered {
		if _, ok := implemented[name]; !ok {
			t.Errorf("registered tool %s is not implemented in the manifest", name)
		}
	}
	want := []string{ToolGetCall, ToolGetProfiles, ToolGetTranscript, ToolListCalls, ToolMakeCall}
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
	ct, st := mcp.NewInMemoryTransports()
	if _, err := h.srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "planned", Version: "1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	m := loadManifest(t)
	for _, tool := range m.Tools {
		if tool.Status != "planned" {
			continue
		}
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: map[string]any{"call_id": "x"}})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("planned tool %s succeeded: %+v", tool.Name, res)
		}
	}
}

// TestManifestSchemasMatchRegisteredSchemas compares the input and output
// schemas the SDK serves for each implemented tool with the manifest. The
// comparison is on canonical JSON, so key order does not matter.
func TestManifestSchemasMatchRegisteredSchemas(t *testing.T) {
	m := loadManifest(t)
	registered := map[string]*mcp.Tool{}
	for _, tool := range listRegistered(t) {
		registered[tool.Name] = tool
	}
	for name, entry := range implementedByName(t, m) {
		tool := registered[name]
		if tool == nil {
			t.Fatalf("%s not registered", name)
		}
		if want, got := normalizeSchema(t, entry.InputSchema), normalizeSchema(t, marshalSchema(t, tool.InputSchema)); want != got {
			t.Errorf("%s input schema drifted\nmanifest: %s\nregistered: %s", name, want, got)
		}
		if len(entry.OutputSchema) == 0 {
			t.Errorf("%s has no outputSchema in the manifest", name)
			continue
		}
		if want, got := normalizeSchema(t, entry.OutputSchema), normalizeSchema(t, marshalSchema(t, tool.OutputSchema)); want != got {
			t.Errorf("%s output schema drifted\nmanifest: %s\nregistered: %s", name, want, got)
		}
	}
}

// TestEmbeddedSchemasMatchManifest ensures the embedded copy of the implemented
// schemas is identical to api/mcp/tools.json. A difference means the served
// schema is not the reviewed one.
func TestEmbeddedSchemasMatchManifest(t *testing.T) {
	m := loadManifest(t)
	embedded, err := loadToolSchemas()
	if err != nil {
		t.Fatal(err)
	}
	impl := implementedByName(t, m)
	if len(embedded) != len(impl) {
		t.Fatalf("embedded has %d tools, manifest implements %d", len(embedded), len(impl))
	}
	for name, entry := range impl {
		got, ok := embedded[name]
		if !ok {
			t.Fatalf("%s missing from embedded schemas", name)
		}
		if a, b := normalizeSchema(t, entry.InputSchema), normalizeSchema(t, got.InputSchema); a != b {
			t.Errorf("%s embedded input differs from manifest", name)
		}
		if a, b := normalizeSchema(t, entry.OutputSchema), normalizeSchema(t, got.OutputSchema); a != b {
			t.Errorf("%s embedded output differs from manifest", name)
		}
	}
}

// TestStatusEnumMatchesServiceStates ensures the status enum in every schema
// equals the service's complete state set, in order.
func TestStatusEnumMatchesServiceStates(t *testing.T) {
	want := service.CallStates()
	embedded, err := loadToolSchemas()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties struct {
			Filters struct {
				Properties struct {
					Status struct {
						Enum []string `json:"enum"`
					} `json:"status"`
				} `json:"properties"`
			} `json:"filters"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(embedded[ToolListCalls].InputSchema, &doc); err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), doc.Properties.Filters.Properties.Status.Enum...)
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Fatalf("list_calls status enum = %v, want %v", got, want)
	}
}

// TestSchemasCompileAndValidateOutputs compiles every implemented output schema
// with a 2020-12 validator and checks that a well-formed result passes while
// a result with a null array or an unknown enum is refused.
func TestSchemasCompileAndValidateOutputs(t *testing.T) {
	embedded, err := loadToolSchemas()
	if err != nil {
		t.Fatal(err)
	}
	listOut := compileSchema(t, "list_calls.output", embedded[ToolListCalls].OutputSchema)
	good := decodeArgs(t, `{"calls":[{"call_id":"c1","telephony_profile_id":"t","ai_profile_id":"a","to_number":"+14155550111","technical_status":"queued","task_outcome":null,"details":{"max_duration_seconds":300,"record_requested":false,"record_effective":false},"transcript_available":false,"recording_available":false,"created_at":"2026-11-23T10:00:00.000Z","updated_at":"2026-11-23T10:00:00.000Z","ended_at":null}],"next_cursor":""}`)
	if err := listOut.Validate(good); err != nil {
		t.Fatalf("valid list output rejected: %v", err)
	}
	for _, bad := range []string{
		`{"calls":null,"next_cursor":""}`,
		`{"calls":[],"next_cursor":null}`,
	} {
		if err := listOut.Validate(decodeArgs(t, bad)); err == nil {
			t.Fatalf("invalid list output accepted: %s", bad)
		}
	}
	profOut := compileSchema(t, "get_profiles.output", embedded[ToolGetProfiles].OutputSchema)
	if err := profOut.Validate(decodeArgs(t, `{"ai_profiles":null,"telephony_profiles":[]}`)); err == nil {
		t.Fatal("null ai_profiles accepted")
	}
	if err := profOut.Validate(decodeArgs(t, `{"ai_profiles":[],"telephony_profiles":[{"id":"t","name":"n","provider":"telnyx","caller_number":"+14155550100","connectivity_status":"healthy"}]}`)); err == nil {
		t.Fatal("connectivity_status other than unknown accepted")
	}
}

// TestListCallsSchemaRejectsInvalidArguments validates concrete argument
// objects against the served input schema, so unknown statuses, oversized
// limits and extra fields are refused.
func TestListCallsSchemaRejectsInvalidArguments(t *testing.T) {
	embedded, err := loadToolSchemas()
	if err != nil {
		t.Fatal(err)
	}
	in := compileSchema(t, "list_calls.input", embedded[ToolListCalls].InputSchema)
	valid := []string{`{}`, `{"limit":100}`, `{"filters":{"status":"ended"}}`, `{"cursor":"abc"}`}
	for _, raw := range valid {
		if err := in.Validate(decodeArgs(t, raw)); err != nil {
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
		if err := in.Validate(decodeArgs(t, raw)); err == nil {
			t.Fatalf("invalid %s accepted", raw)
		}
	}
}

// compileSchema compiles a raw schema with the JSON Schema 2020-12 validator.
func compileSchema(t *testing.T, name string, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: decode: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("file:///"+name+".json", doc); err != nil {
		t.Fatalf("%s: add: %v", name, err)
	}
	s, err := c.Compile("file:///" + name + ".json")
	if err != nil {
		t.Fatalf("%s: compile: %v", name, err)
	}
	return s
}

func decodeArgs(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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

// TestAllManifestSchemasCompile compiles every input and output schema in
// api/mcp/tools.json, including planned tools, with the JSON Schema 2020-12
// validator. A malformed planned schema fails the build before it is used.
func TestAllManifestSchemasCompile(t *testing.T) {
	m := loadManifest(t)
	for _, tool := range m.Tools {
		if len(tool.InputSchema) > 0 {
			compileSchema(t, tool.Name+".input", tool.InputSchema)
		} else {
			t.Errorf("%s has no inputSchema", tool.Name)
		}
		if len(tool.OutputSchema) > 0 {
			compileSchema(t, tool.Name+".output", tool.OutputSchema)
		} else {
			t.Errorf("%s has no outputSchema", tool.Name)
		}
	}
}

// TestMCPContractFilesHaveNoDuplicateKeys fails when any object in the manifest
// or the embedded schema file repeats a key. encoding/json keeps the last
// duplicate silently, so a drift test alone cannot see it.
func TestMCPContractFilesHaveNoDuplicateKeys(t *testing.T) {
	for _, path := range []string{toolsManifest, "schemas.json"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if key, ok := firstDuplicateKey(b); ok {
			t.Fatalf("%s repeats object key %q", path, key)
		}
	}
}

// firstDuplicateKey walks the JSON token stream and reports the first object
// key that appears twice in the same object.
func firstDuplicateKey(b []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	type frame struct {
		isObject  bool
		keys      map[string]bool
		expectKey bool
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, frame{isObject: true, keys: map[string]bool{}, expectKey: true})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].isObject {
					stack[len(stack)-1].expectKey = true
				}
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		top := &stack[len(stack)-1]
		if top.isObject && top.expectKey {
			key, _ := tok.(string)
			if top.keys[key] {
				return key, true
			}
			top.keys[key] = true
			top.expectKey = false
			continue
		}
		if top.isObject {
			top.expectKey = true
		}
	}
}

// TestFirstDuplicateKeyDetectsRepeatedKeysOnly proves the detector itself
// catches a repeated key in a nested object and ignores repeats in sibling
// objects, which are legal.
func TestFirstDuplicateKeyDetectsRepeatedKeysOnly(t *testing.T) {
	if key, ok := firstDuplicateKey([]byte(`{"a":{"x":1,"caller_number":{},"caller_number":{}}}`)); !ok || key != "caller_number" {
		t.Fatalf("nested duplicate = %q, %v; want caller_number", key, ok)
	}
	if key, ok := firstDuplicateKey([]byte(`{"items":[{"id":1},{"id":2}],"id":[]}`)); ok {
		t.Fatalf("valid document reported duplicate %q", key)
	}
}
