package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

// Tool names. Only the implemented tools are registered. The planned
// operations are documented in api/mcp/tools.json with status "planned" and
// are never registered, so a call to them fails as an unknown tool.
const (
	ToolGetProfiles = "get_profiles"
	ToolListCalls   = "list_calls"
	ToolGetCall     = "get_call"
	ToolMakeCall    = "make_call"
)

// GetProfilesInput takes no arguments.
type GetProfilesInput struct{}

// ListCallsInput is the list_calls argument object. The owner is never an
// argument; it comes from the authenticated principal. Constraints (limits,
// enum, non-null filters) are applied in listCallsSchema so the served schema
// is exactly the one the manifest documents.
type ListCallsInput struct {
	Cursor  string       `json:"cursor,omitempty"`
	Limit   int          `json:"limit,omitempty"`
	Filters *CallFilters `json:"filters,omitempty"`
}

// CallFilters narrows list_calls.
type CallFilters struct {
	Status string `json:"status,omitempty"`
}

// MakeCallInput is the make_call argument object. Its fields are the public
// schema, and the owner is never an argument. The service validates each value.
type MakeCallInput struct {
	ProfileID       string   `json:"profile_id"`
	To              string   `json:"to"`
	Recipient       string   `json:"recipient,omitempty"`
	Language        string   `json:"language,omitempty"`
	Objective       string   `json:"objective"`
	Context         string   `json:"context,omitempty"`
	Instructions    string   `json:"instructions,omitempty"`
	Constraints     []string `json:"constraints,omitempty"`
	Fallbacks       []string `json:"fallbacks,omitempty"`
	SuccessCriteria []string `json:"success_criteria,omitempty"`
	MaxDuration     *int     `json:"max_duration_seconds,omitempty"`
	Record          bool     `json:"record,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key"`
}

// GetCallInput names one call.
type GetCallInput struct {
	CallID string `json:"call_id"`
}

// ListCallsOutput is one page of calls.
type ListCallsOutput struct {
	Calls      []service.CallView `json:"calls"`
	NextCursor string             `json:"next_cursor"`
}

// registerTools adds the implemented tools to srv. Each tool's input and output
// schema comes from the embedded manifest copy, so the served contract is the
// reviewed one. Invalid embedded schemas panic during New, not per request.
func registerTools(srv *mcp.Server, svc *service.Service, logger *slog.Logger) {
	schemas, err := loadToolSchemas()
	if err != nil {
		panic(err)
	}
	inputOf := func(name string) *jsonschema.Schema {
		s, err := decodeSchema(schemas[name].InputSchema)
		if err != nil {
			panic(fmt.Errorf("%s input schema: %w", name, err))
		}
		return s
	}
	outputOf := func(name string) *jsonschema.Schema {
		s, err := decodeSchema(schemas[name].OutputSchema)
		if err != nil {
			panic(fmt.Errorf("%s output schema: %w", name, err))
		}
		return s
	}

	addTool(srv, &mcp.Tool{
		Name:         ToolGetProfiles,
		Description:  "Return the caller's AI profiles and the telephony profiles the caller owns or was granted, with non-secret fields only. Each telephony profile reports access (owner or granted) and its last connectivity result.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema:  inputOf(ToolGetProfiles),
		OutputSchema: outputOf(ToolGetProfiles),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ GetProfilesInput) (*mcp.CallToolResult, service.ProfilesResult, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return nil, service.ProfilesResult{}, toolFailure(logger, err)
		}
		res, err := svc.GetProfiles(ctx, actor)
		if err != nil {
			return nil, service.ProfilesResult{}, toolFailure(logger, err)
		}
		return nil, res, nil
	})

	addTool(srv, &mcp.Tool{
		Name:         ToolListCalls,
		Description:  "List the caller's calls, newest first, one keyset page at a time.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema:  inputOf(ToolListCalls),
		OutputSchema: outputOf(ToolListCalls),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListCallsInput) (*mcp.CallToolResult, ListCallsOutput, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return nil, ListCallsOutput{}, toolFailure(logger, err)
		}
		status := ""
		if in.Filters != nil {
			status = in.Filters.Status
		}
		res, err := svc.ListCalls(ctx, actor, service.ListCallsInput{Cursor: in.Cursor, Limit: in.Limit, Status: status})
		if err != nil {
			return nil, ListCallsOutput{}, toolFailure(logger, err)
		}
		return nil, ListCallsOutput{Calls: res.Calls, NextCursor: res.NextCursor}, nil
	})

	addTool(srv, &mcp.Tool{
		Name:         ToolMakeCall,
		Description:  "Queue one outbound AI voice call. Returns the queued call id at once; the dial happens asynchronously. Requires an idempotency_key: the same key with the same request returns the original call, and the same key with a different request is a conflict. The telephony profile must be verified and the caller must have exactly one AI profile.",
		InputSchema:  inputOf(ToolMakeCall),
		OutputSchema: outputOf(ToolMakeCall),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in MakeCallInput) (*mcp.CallToolResult, service.MakeCallResult, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return nil, service.MakeCallResult{}, toolFailure(logger, err)
		}
		res, err := svc.MakeCall(ctx, actor, service.MakeCallInput{
			ProfileID: in.ProfileID, To: in.To, Recipient: in.Recipient, Language: in.Language,
			Objective: in.Objective, Context: in.Context, Instructions: in.Instructions,
			Constraints: in.Constraints, Fallbacks: in.Fallbacks, SuccessCriteria: in.SuccessCriteria,
			MaxDuration: in.MaxDuration, Record: in.Record, IdempotencyKey: in.IdempotencyKey,
		})
		if err != nil {
			return nil, service.MakeCallResult{}, toolFailure(logger, err)
		}
		return nil, res, nil
	})

	addTool(srv, &mcp.Tool{
		Name:         ToolGetCall,
		Description:  "Return one call owned by the caller. Foreign or missing calls return not_found.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema:  inputOf(ToolGetCall),
		OutputSchema: outputOf(ToolGetCall),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in GetCallInput) (*mcp.CallToolResult, service.CallView, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return nil, service.CallView{}, toolFailure(logger, err)
		}
		res, err := svc.GetCall(ctx, actor, in.CallID)
		if err != nil {
			return nil, service.CallView{}, toolFailure(logger, err)
		}
		return nil, res, nil
	})
}

// addTool registers a typed tool. The explicit schemas are already set on t,
// so the SDK serves them as given instead of reflecting them from Out.
func addTool[In, Out any](srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(srv, t, h)
}

// toolFailure converts a service error into a tool execution failure.
//
// The SDK turns a non-nil handler error into a result with IsError set and
// StructuredContent left nil. It does not marshal the zero-value output, so a
// client never sees an empty structured object next to a failure. The text
// content is the shared error envelope, {"error":{"code","message"}}, the same
// body the REST API returns. Cause is logged server-side and never rendered.
func toolFailure(logger *slog.Logger, err error) error {
	service.LogCause(logger, "mcp", err)
	body, mErr := json.Marshal(service.EnvelopeOf(err))
	if mErr != nil {
		// The envelope holds only two strings, so this cannot fail in
		// practice. Fall back to the generic internal envelope regardless.
		body = []byte(`{"error":{"code":"internal","message":"internal error"}}`)
	}
	return errors.New(string(body))
}
