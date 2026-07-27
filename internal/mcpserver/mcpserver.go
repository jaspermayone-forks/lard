// Package mcpserver exposes lard's live agent surface over MCP:
// get_context, remember, forget. It is a thin ergonomic wrapper over the
// same store the HTTP API serves.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/taciturnaxolotl/lard/internal/httpapi"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// New builds the MCP server backed by the HTTP API server's store.
func New(api *httpapi.Server) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "lard", Version: "0.1.0"}, nil)

	type getContextArgs struct {
		Project   string `json:"project,omitempty" jsonschema:"canonical project id; omit for profile only"`
		GitRemote string `json:"gitRemote,omitempty" jsonschema:"git origin remote; normalized server-side (strongest hint)"`
		Path      string `json:"path,omitempty" jsonschema:"absolute workspace path; weakest hint"`
		Name      string `json:"name,omitempty" jsonschema:"human project label"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_context",
		Description: `Fetch the injection bundle for the start of a session: the global user profile, the project document, and the recent session log. Identify the project with hints (gitRemote > name/path) or a canonical project id; hints resolve and mint server-side.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getContextArgs) (*mcp.CallToolResult, any, error) {
		projectID := args.Project
		if projectID == "" {
			hints := &types.ProjectHints{GitRemote: args.GitRemote, Path: args.Path, Name: args.Name}
			if *hints != (types.ProjectHints{}) {
				pid, err := api.Registry().Resolve(hints)
				if err != nil {
					return errResult(err), nil, nil
				}
				projectID = pid
			}
		}
		bundle, err := api.ContextBundle(projectID)
		if err != nil {
			return errResult(err), nil, nil
		}
		out, _ := json.MarshalIndent(bundle, "", "  ")
		return textResult(string(out)), nil, nil
	})

	type rememberArgs struct {
		Scope     string `json:"scope" jsonschema:"profile or project/<id>"`
		Key       string `json:"key" jsonschema:"dotted category, e.g. preferences.language or conventions.pkg-manager"`
		Value     string `json:"value" jsonschema:"the fact, in natural language"`
		Class     string `json:"klass,omitempty" jsonschema:"static (durable) or dynamic (recent); default dynamic"`
		Namespace string `json:"namespace,omitempty" jsonschema:"doc namespace for session-log writes, e.g. project/<id>/session-log/2026-07-26"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "remember",
		Description: `Write a fact directly without waiting for the nightly pass. Agent-authored records carry source "agent": they are reconciled on the next consolidation run and never override user-pinned facts. Use a session-log namespace at session close to leave a tight "did X, decided Y, next Z" note.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args rememberArgs) (*mcp.CallToolResult, any, error) {
		scope, err := parseScopeArg(args.Scope)
		if err != nil {
			return errResult(err), nil, nil
		}
		klass := types.ClassDynamic
		if args.Class == "static" {
			klass = types.ClassStatic
		}
		rec := &types.Record{
			Scope:      scope,
			Key:        args.Key,
			Value:      args.Value,
			Confidence: 0.8,
			Class:      klass,
			Source:     types.SourceAgent,
			Status:     types.StatusActive,
		}
		if err := api.Store().UpsertRecord(rec); err != nil {
			return errResult(err), nil, nil
		}
		// Session-log notes render under their own namespace immediately;
		// everything else re-renders its scope so get_context sees it now.
		if args.Namespace != "" {
			_ = api.RenderSessionLog(args.Namespace, args.Value)
		} else {
			_ = api.RenderScope(scope)
		}
		return textResult(fmt.Sprintf("remembered (%s): %s", args.Key, rec.ID)), nil, nil
	})

	type forgetArgs struct {
		Namespace string `json:"namespace" jsonschema:"profile/... or project/<id>/..."`
		Key       string `json:"key" jsonschema:"record key to soft-delete"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "forget",
		Description: `Soft-delete all active records at (namespace, key). The records leave rendered documents but stay in the store for audit.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args forgetArgs) (*mcp.CallToolResult, any, error) {
		scope, err := parseScopeArg(args.Namespace)
		if err != nil {
			return errResult(err), nil, nil
		}
		n, err := api.Store().SoftDeleteKey(scope, args.Key)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(fmt.Sprintf("forgot %d record(s) at %s", n, args.Key)), nil, nil
	})

	return s
}

// parseScopeArg accepts "profile", "profile/<doc>", "project/<id>", or
// "project/<id>/<doc...>" and returns the corresponding scope.
func parseScopeArg(s string) (types.Scope, error) {
	parts := strings.Split(strings.Trim(s, "/"), "/")
	switch parts[0] {
	case "profile":
		return types.Scope{Kind: types.ScopeProfile}, nil
	case "project":
		if len(parts) < 2 || parts[1] == "" {
			return types.Scope{}, fmt.Errorf("project scope needs an id: project/<id>")
		}
		return types.Scope{Kind: types.ScopeProject, ProjectID: parts[1]}, nil
	default:
		return types.Scope{}, fmt.Errorf("scope must be profile or project/<id>, got %q", s)
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// HTTPHandler serves the MCP server over streamable HTTP.
func HTTPHandler(s *mcp.Server) *mcp.StreamableHTTPHandler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
