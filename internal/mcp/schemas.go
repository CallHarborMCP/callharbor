package mcp

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemasJSON holds the input and output schemas of the implemented tools.
// It is a copy of the implemented entries in api/mcp/tools.json. The drift
// test fails if the two differ, so the served schema is always the reviewed
// one. The copy exists only because go:embed cannot reach outside this package.
//
//go:embed schemas.json
var schemasJSON []byte

// toolSchemas is the parsed form of schemasJSON, keyed by tool name.
type toolSchemas struct {
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

// loadToolSchemas parses the embedded schemas and compiles each one into a
// *jsonschema.Schema. A malformed schema is a build error, so it panics at
// startup rather than being served.
func loadToolSchemas() (map[string]toolSchemas, error) {
	var raw map[string]toolSchemas
	if err := json.Unmarshal(schemasJSON, &raw); err != nil {
		return nil, fmt.Errorf("parse embedded tool schemas: %w", err)
	}
	return raw, nil
}

// decodeSchema turns a raw JSON Schema document into the SDK's schema type.
func decodeSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
