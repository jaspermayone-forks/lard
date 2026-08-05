// Package mcpserver exposes lard's live agent surface over MCP: reading the
// context bundle and the subject memory files, plus writing/appending/editing
// them. A thin wrapper over the same store the HTTP API serves.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/taciturnaxolotl/lard/internal/httpapi"
	"github.com/taciturnaxolotl/lard/internal/pipeline"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// instructions is returned at initialize and concatenated into the agent's
// system prompt. It can't carry the context bundle itself (the project isn't
// known yet), so it just teaches the agent to call get_context first.
const instructions = `lard is the user's long-term memory: markdown subject files about the user (profile), their projects (areas/), cross-cutting interests and preferences (topics/), and people.

Read:
1. At session start, before any project work, call get_context with the workspace's git origin remote (gitRemote) and absolute path (path). It returns the profile, this project's area file, and an index of every other subject (path + description + aliases).
2. Treat what you get as established context: let the profile and area shape defaults and choices instead of asking the user again.
3. When the task plausibly touches a topic or another project in the index, memory_read that subject before working. Descriptions are retrieval keys.

Write:
- When the user states a durable fact — a decision, convention, deploy target, host, version, preference — persist it in the same session with memory_append. One specific statement per call, routed to the right subject: project specifics → its area, cross-project preferences → a topic, identity → profile, people → people/<name>.
- Carry the specifics: "deploys to Cloudflare Pages at s.dunkirk.sh" beats "has a deployment".
- Skip anything true only for the current task, and anything the repo already shows.
- memory_write is for full rewrites; memory_read first for the version token.`

// New builds an MCP server bound to one tenant's memory. Tool handlers do not
// see the HTTP request, so the tenant is resolved before the server is built
// rather than inside each tool; HTTPHandler does that per request.
func New(mem *httpapi.Tenant) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "lard", Version: "0.2.0"},
		&mcp.ServerOptions{Instructions: instructions},
	)

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
				pid, err := mem.Registry().Resolve(hints)
				if err != nil {
					return errResult(err), nil, nil
				}
				projectID = pid
			}
		}
		bundle, err := mem.ContextBundle(projectID)
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
		listing, err := mem.Store().ListSubjects()
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
		kind, name, err := types.ParseSubjectPath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		sub, err := mem.Store().GetSubject(kind, name)
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
		Path        string   `json:"path" jsonschema:"subject path"`
		Body        string   `json:"body" jsonschema:"full markdown body (bullet lines with provenance tags)"`
		Description string   `json:"description,omitempty" jsonschema:"one-line subject description"`
		Aliases     []string `json:"aliases,omitempty" jsonschema:"other names this subject goes by; replaces the existing set"`
		Repos       []string `json:"repos,omitempty" jsonschema:"git remote urls for this subject; any form (ssh or https) is normalized. list all of them when a project has mirrors; the first is treated as canonical. replaces the existing set"`
		Version     string   `json:"version,omitempty" jsonschema:"version token from memory_read, or 'new' to create"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "memory_write",
		Description: `Create or fully overwrite a subject file. Read first (for the version token) and merge, rather than clobbering. Not an append — omitted lines are deleted. Passing repos on an area also links it to the project registry.`,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args writeArgs) (*mcp.CallToolResult, any, error) {
		kind, name, err := types.ParseSubjectPath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		sub, err := pipeline.ApplyPatch(mem.Store(), mem.Registry(), kind, name, pipeline.SubjectPatch{
			Body:        &args.Body,
			Description: args.Description,
			Aliases:     args.Aliases,
			Repos:       args.Repos,
			Version:     args.Version,
		})
		if errors.Is(err, pipeline.ErrVersionConflict) {
			return errResult(fmt.Errorf("version mismatch; re-read %s and merge", args.Path)), nil, nil
		}
		if err != nil {
			return errResult(err), nil, nil
		}
		msg := fmt.Sprintf("wrote %s [version: %s]", args.Path, sub.Version)
		if len(sub.Repos) > 0 {
			msg += "\nrepos: " + strings.Join(sub.Repos, ", ")
		}
		return textResult(msg), nil, nil
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
		kind, name, err := types.ParseSubjectPath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		sub, err := pipeline.AppendLine(mem.Store(), kind, name, args.Line)
		if err != nil {
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
		kind, name, err := types.ParseSubjectPath(args.Path)
		if err != nil {
			return errResult(err), nil, nil
		}
		if err := mem.Store().DeleteSubject(kind, name); err != nil {
			return errResult(err), nil, nil
		}
		return textResult("deleted " + args.Path), nil, nil
	})

	return s
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// HTTPHandler serves MCP over streamable HTTP, one MCP server per tenant.
//
// Tool handlers run on the session's goroutine and never see the HTTP request,
// so the caller's identity cannot be read inside a tool. Instead the tenant is
// resolved here, per request, and the MCP server bound to it is reused for
// every later call from that identity. Sessions therefore belong to a tenant,
// which is exactly the isolation we want: a session id minted for one user is
// unknown to another's server.
//
// The SDK's DNS-rebinding guard rejects any request arriving on a loopback
// socket that carries a non-loopback Host header. Behind a reverse proxy
// that describes every legitimate request, so the guard is disabled here.
// What it defends against — a browser tricked into reaching an
// unauthenticated local server — does not apply: lard authenticates every
// request and is reached through the proxy rather than directly.
func HTTPHandler(api *httpapi.Server) http.Handler {
	var (
		mu      sync.Mutex
		servers = map[string]*mcp.Server{}
	)
	forTenant := func(t *httpapi.Tenant) *mcp.Server {
		mu.Lock()
		defer mu.Unlock()
		if s, ok := servers[t.Key()]; ok {
			return s
		}
		s := New(t)
		servers[t.Key()] = s
		return s
	}

	inner := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			t, err := api.TenantFor(r.Context())
			if err != nil {
				return nil
			}
			return forTenant(t)
		},
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)

	// Resolve once up front so an unidentified caller gets a 401 (which tells
	// an MCP client to go get a token) instead of the SDK's bare 400.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := api.TenantFor(r.Context()); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, httpapi.ErrNoIdentity) {
				status = http.StatusUnauthorized
			}
			http.Error(w, err.Error(), status)
			return
		}
		inner.ServeHTTP(w, r)
	})
}
