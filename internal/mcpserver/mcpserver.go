// Package mcpserver exposes lard's live agent surface over MCP: reading the
// context bundle and the subject memory files, plus writing/appending/editing
// them. A thin wrapper over the same store the HTTP API serves.
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
	s := mcp.NewServer(&mcp.Implementation{Name: "lard", Version: "0.2.0"}, nil)

	// get_context — the session-start injection bundle.
	type getContextArgs struct {
		Project   string `json:"project,omitempty" jsonschema:"canonical project id; omit for profile only"`
		GitRemote string `json:"gitRemote,omitempty" jsonschema:"git origin remote; normalized server-side (strongest hint)"`
		Path      string `json:"path,omitempty" jsonschema:"absolute workspace path"`
		Name      string `json:"name,omitempty" jsonschema:"human project label"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_context",
		Description: `Fetch the session-start bundle: the user profile in full, the list of all memory subjects (path + description + aliases), and this project's area file if identified. Use the listing to decide which other subjects to read with memory_read.`,
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

	// memory_list — the retrieval index.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_list",
		Description: `List all memory subjects: path, kind, description, aliases. The retrieval surface — decide what to read from descriptions alone.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		listing, err := api.Store().ListSubjects()
		if err != nil {
			return errResult(err), nil, nil
		}
		out, _ := json.MarshalIndent(listing, "", "  ")
		return textResult(string(out)), nil, nil
	})

	// memory_read — read a subject file.
	type readArgs struct {
		Path string `json:"path" jsonschema:"subject path: profile, areas/<name>, topics/<name>, people/<name>"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_read",
		Description: `Read one subject file's body and version token. Pass the version to memory_write for safe concurrent edits.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args readArgs) (*mcp.CallToolResult, any, error) {
		kind, name, err := parsePath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		sub, err := api.Store().GetSubject(kind, name)
		if err != nil {
			return errResult(err), nil, nil
		}
		if sub == nil {
			return errResult(fmt.Errorf("subject not found: %s", args.Path)), nil, nil
		}
		return textResult(fmt.Sprintf("[version: %s]\n%s", sub.Version, sub.Body)), nil, nil
	})

	// memory_write — create or fully overwrite a subject.
	type writeArgs struct {
		Path        string `json:"path" jsonschema:"subject path"`
		Body        string `json:"body" jsonschema:"full markdown body (bullet lines with provenance tags)"`
		Description string `json:"description,omitempty" jsonschema:"one-line subject description"`
		Version     string `json:"version,omitempty" jsonschema:"version token from memory_read, or 'new' to create"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_write",
		Description: `Create or fully overwrite a subject file. Read first (for the version token) and merge, rather than clobbering. Not an append — omitted lines are deleted.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args writeArgs) (*mcp.CallToolResult, any, error) {
		kind, name, err := parsePath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		existing, err := api.Store().GetSubject(kind, name)
		if err != nil {
			return errResult(err), nil, nil
		}
		if args.Version != "" && args.Version != "new" && existing != nil && existing.Version != args.Version {
			return errResult(fmt.Errorf("version mismatch; re-read %s and merge", args.Path)), nil, nil
		}
		sub := existing
		if sub == nil {
			sub = &types.Subject{Kind: kind, Name: name}
		}
		sub.Body = args.Body
		if args.Description != "" {
			sub.Description = args.Description
		}
		if sub.Description == "" {
			sub.Description = name
		}
		if err := api.Store().PutSubject(sub, 0); err != nil {
			return errResult(err), nil, nil
		}
		return textResult(fmt.Sprintf("wrote %s [version: %s]", args.Path, sub.Version)), nil, nil
	})

	// memory_append — add one line without resending the file.
	type appendArgs struct {
		Path string `json:"path" jsonschema:"subject path"`
		Line string `json:"line" jsonschema:"one fact to append; a leading '- ' and [stated] tag are added if missing"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_append",
		Description: `Append a single fact to a subject without resending its whole body. Creates the subject if absent.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args appendArgs) (*mcp.CallToolResult, any, error) {
		kind, name, err := parsePath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		sub, err := api.Store().GetSubject(kind, name)
		if err != nil {
			return errResult(err), nil, nil
		}
		if sub == nil {
			sub = &types.Subject{Kind: kind, Name: name, Description: name}
		}
		line := strings.TrimSpace(args.Line)
		if !strings.HasPrefix(line, "-") {
			line = "- " + line
		}
		if sub.Body == "" {
			sub.Body = line
		} else {
			sub.Body = strings.TrimRight(sub.Body, "\n") + "\n" + line
		}
		if err := api.Store().PutSubject(sub, 0); err != nil {
			return errResult(err), nil, nil
		}
		return textResult(fmt.Sprintf("appended to %s [version: %s]", args.Path, sub.Version)), nil, nil
	})

	// memory_delete — remove a subject.
	type deleteArgs struct {
		Path string `json:"path" jsonschema:"subject path to delete"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_delete",
		Description: `Delete a whole subject file. Only on explicit user request.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args deleteArgs) (*mcp.CallToolResult, any, error) {
		kind, name, err := parsePath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		if err := api.Store().DeleteSubject(kind, name); err != nil {
			return errResult(err), nil, nil
		}
		return textResult("deleted " + args.Path), nil, nil
	})

	return s
}

// parsePath maps a subject path to (kind, name).
func parsePath(p string) (types.SubjectKind, string, error) {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".md")
	if p == "profile" || p == "" {
		return types.KindProfile, "profile", nil
	}
	dir, name, ok := strings.Cut(p, "/")
	if !ok {
		return "", "", fmt.Errorf("path must be profile, areas/<name>, topics/<name>, or people/<name>")
	}
	switch dir {
	case "areas":
		return types.KindArea, name, nil
	case "topics":
		return types.KindTopic, name, nil
	case "people":
		return types.KindPeople, name, nil
	default:
		return "", "", fmt.Errorf("unknown memory folder %q", dir)
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// HTTPHandler serves the MCP server over streamable HTTP.
func HTTPHandler(s *mcp.Server) *mcp.StreamableHTTPHandler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
}
