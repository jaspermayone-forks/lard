// Package types holds the shared data model for lard: scopes, records,
// the edge-to-center turn wire contract, and project identity hints.
package types

import "time"

// ScopeKind distinguishes the global user profile from per-project context.
type ScopeKind string

const (
	ScopeProfile ScopeKind = "profile"
	ScopeProject ScopeKind = "project"
)

// Scope is where a record lives. Profile scopes have no ProjectID.
type Scope struct {
	Kind      ScopeKind `json:"kind"`
	ProjectID string    `json:"projectId,omitempty"`
}

// String renders the scope as the namespace prefix used by documents:
// "profile" or "project/<id>".
func (s Scope) String() string {
	if s.Kind == ScopeProject && s.ProjectID != "" {
		return "project/" + s.ProjectID
	}
	return "profile"
}

// RecordSource is provenance. Authority order: user > batch > agent.
type RecordSource string

const (
	SourceBatch RecordSource = "batch"
	SourceAgent RecordSource = "agent"
	SourceUser  RecordSource = "user"
)

// RecordClass separates stable identity (static) from recent context
// (dynamic). They decay on different curves.
type RecordClass string

const (
	ClassStatic  RecordClass = "static"
	ClassDynamic RecordClass = "dynamic"
)

// RecordStatus tracks the reconciliation lifecycle.
type RecordStatus string

const (
	StatusActive       RecordStatus = "active"
	StatusSuperseded   RecordStatus = "superseded"
	StatusContradicted RecordStatus = "contradicted"
)

// Record is the atomic unit of memory: one fact, with provenance and edges.
type Record struct {
	ID          string       `json:"id"`
	Scope       Scope        `json:"scope"`
	Key         string       `json:"key"` // "preferences.formatting"
	Value       string       `json:"value"`
	Confidence  float64      `json:"confidence"`
	Class       RecordClass  `json:"klass"`
	Source      RecordSource `json:"source"`
	Status      RecordStatus `json:"status"`
	Supersedes  []string     `json:"supersedes,omitempty"`
	Contradicts []string     `json:"contradicts,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
	LastSeenAt  time.Time    `json:"lastSeenAt"`
}

// ProjectHints are the identity signals a client sends so the service can
// canonicalize a project without ever trusting a raw path as the id.
type ProjectHints struct {
	GitRemote string `json:"gitRemote,omitempty"` // normalized origin remote; strongest
	Path      string `json:"path,omitempty"`      // machine-local, weakest
	Name      string `json:"name,omitempty"`      // human label (web frontend)
}

// Turn is one normalized turn of a session. In the common path every
// uploaded turn has role "user"; other roles exist for the optional
// coreference-context case and non-agent sources.
type Turn struct {
	Index   int    `json:"index"`
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      string `json:"ts"` // ISO 8601 UTC
}

// SessionBatch is one session's turns plus origin context.
type SessionBatch struct {
	SessionID    string        `json:"sessionId"`
	Source       string        `json:"source"` // "crush" | "web-frontend" | ...
	ProjectHints *ProjectHints `json:"projectHints,omitempty"`
	StartedAt    string        `json:"startedAt"`
	EndedAt      string        `json:"endedAt,omitempty"`
	Turns        []Turn        `json:"turns"`
}

// IngestRequest is the /ingest envelope: one or more sessions from a
// single collector.
type IngestRequest struct {
	Collector string         `json:"collector"`
	Sessions  []SessionBatch `json:"sessions"`
}

// Candidate is a durable fact extracted from a session's user turns,
// not yet reconciled against memory.
type Candidate struct {
	Observation string  `json:"observation"` // grounded paraphrase of what the user said
	Fact        string  `json:"fact"`
	Key         string  `json:"key"` // dotted category
	ScopeHint   string  `json:"scopeHint"` // "profile" | "project"
	Class       string  `json:"klass"`     // "static" | "dynamic"
	Confidence  float64 `json:"confidence"`
	Sensitivity *string `json:"sensitivity"`
	SourceTurn  int     `json:"sourceTurn"`
}

// ContextBundle is what a client injects at session start.
type ContextBundle struct {
	Profile    string `json:"profile"`
	Project    string `json:"project,omitempty"`
	SessionLog string `json:"sessionLog,omitempty"`
	ProjectID  string `json:"projectId,omitempty"`
	Created    bool   `json:"created,omitempty"`
}

// Project is the canonical identity that aliases resolve to.
type Project struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Remotes     []string `json:"remotes"`
	Paths       []string `json:"paths"`
	Names       []string `json:"names"`
	CreatedAt   string   `json:"createdAt"`
}
