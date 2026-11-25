package mcp

import (
	"context"
	"errors"
	"fmt"

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

// GetCallInput names one call.
type GetCallInput struct {
	CallID string `json:"call_id"`
}

// ListCallsOutput is one page of calls.
type ListCallsOutput struct {
	Calls      []service.CallView `json:"calls"`
	NextCursor string             `json:"next_cursor"`
}

// callStatusEnum is the complete technical status vocabulary.
var callStatusEnum = []any{"queued", "initiating", "ringing", "active", "ending", "ended", "failed"}

// listCallsSchema derives the list_calls input schema from the struct and then
// applies the documented constraints: bounded limit, bounded cursor, a closed
// status enum, and a non-null filters object. The SDK validates arguments
// against this schema before the handler runs.
func listCallsSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[ListCallsInput](nil)
	if err != nil {
		return nil, err
	}
	s.Properties["limit"].Minimum = jsonschema.Ptr(float64(service.MinLimit))
	s.Properties["limit"].Maximum = jsonschema.Ptr(float64(service.MaxLimit))
	s.Properties["cursor"].MaxLength = jsonschema.Ptr(maxCursorLength)
	filters := s.Properties["filters"]
	filters.Types = nil
	filters.Type = "object"
	status := filters.Properties["status"]
	if status == nil {
		return nil, errors.New("list_calls schema has no filters.status")
	}
	status.Enum = callStatusEnum
	return s, nil
}

// maxCursorLength bounds the opaque cursor token a client may send.
const maxCursorLength = 512

// getCallSchema constrains call_id to the identifier bounds.
func getCallSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[GetCallInput](nil)
	if err != nil {
		return nil, err
	}
	id := s.Properties["call_id"]
	if id == nil {
		return nil, errors.New("get_call schema has no call_id")
	}
	id.MinLength = jsonschema.Ptr(1)
	id.MaxLength = jsonschema.Ptr(service.MaxIDLength)
	return s, nil
}

func registerTools(srv *mcp.Server, svc *service.Service) {
	listSchema, err := listCallsSchema()
	if err != nil {
		panic(fmt.Errorf("list_calls input schema: %w", err))
	}
	callSchema, err := getCallSchema()
	if err != nil {
		panic(fmt.Errorf("get_call input schema: %w", err))
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        ToolGetProfiles,
		Description: "Return the caller's AI and telephony profiles with non-secret fields only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ GetProfilesInput) (*mcp.CallToolResult, service.ProfilesResult, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return toolError(err), service.ProfilesResult{}, nil
		}
		res, err := svc.GetProfiles(ctx, actor)
		if err != nil {
			return toolError(err), service.ProfilesResult{}, nil
		}
		return nil, res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        ToolListCalls,
		Description: "List the caller's calls, newest first, one keyset page at a time.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: listSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ListCallsInput) (*mcp.CallToolResult, ListCallsOutput, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return toolError(err), ListCallsOutput{}, nil
		}
		status := ""
		if in.Filters != nil {
			status = in.Filters.Status
		}
		res, err := svc.ListCalls(ctx, actor, service.ListCallsInput{Cursor: in.Cursor, Limit: in.Limit, Status: status})
		if err != nil {
			return toolError(err), ListCallsOutput{}, nil
		}
		return nil, ListCallsOutput{Calls: res.Calls, NextCursor: res.NextCursor}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        ToolGetCall,
		Description: "Return one call owned by the caller. Foreign or missing calls return not_found.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: callSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in GetCallInput) (*mcp.CallToolResult, service.CallView, error) {
		actor, err := actorFromRequest(req)
		if err != nil {
			return toolError(err), service.CallView{}, nil
		}
		res, err := svc.GetCall(ctx, actor, in.CallID)
		if err != nil {
			return toolError(err), service.CallView{}, nil
		}
		return nil, res, nil
	})
}

// toolError returns a tool-level failure: IsError is set and the message carries
// the stable code, so the caller sees a failure and never a success.
func toolError(err error) *mcp.CallToolResult {
	se := service.AsError(err)
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(se.Code) + ": " + se.Message}},
	}
}
