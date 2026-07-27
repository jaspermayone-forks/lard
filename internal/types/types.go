// Package types holds the shared data model for lard: the subject-file
// memory model, the edge-to-center turn wire contract, and project identity
// hints.
package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// --- Subject-file memory model ---

// SubjectKind is the category of a memory subject, which also picks its
// on-disk folder.
type SubjectKind string

const (
	KindProfile SubjectKind = "profile" // singleton: durable identity
	KindArea    SubjectKind = "area"    // one per project / ongoing thing
	KindTopic   SubjectKind = "topic"   // cross-cutting domain facts
	KindPeople  SubjectKind = "people"  // one per person
)

// Subject is one memory file: frontmatter plus a prose body. The body is
// the human-facing, editable artifact; the store persists it as markdown on
// disk.
type Subject struct {
	Name        string      `json:"name"` // slug, unique; path stem
	Kind        SubjectKind `json:"kind"`
	Description string      `json:"description"` // one-line retrieval key
	Aliases     []string    `json:"aliases,omitempty"`
	ProjectID   string      `json:"projectId,omitempty"` // links an area to the registry
	// Repos are the subject's git remotes, normalized. A list because one
	// project often lives in several places at once (a canonical repo plus
	// mirrors), and recorded in the file so a subject names its own code
	// without a registry lookup.
	Repos   []string  `json:"repos,omitempty"`
	Body    string    `json:"body"` // markdown, prose bullets
	Updated time.Time `json:"updated"`
	Version string    `json:"version"` // content hash for optimistic concurrency
}

// Path returns the store-relative path for a subject ("profile.md",
// "areas/crush.md", ...).
func (s Subject) Path() string {
	return SubjectPath(s.Kind, s.Name)
}

// SubjectPath builds the store-relative path for a (kind, name).
func SubjectPath(kind SubjectKind, name string) string {
	switch kind {
	case KindProfile:
		return "profile.md"
	case KindArea:
		return "areas/" + name + ".md"
	case KindTopic:
		return "topics/" + name + ".md"
	case KindPeople:
		return "people/" + name + ".md"
	default:
		return name + ".md"
	}
}

// ParseSubjectPath maps a request path ("profile", "areas/crush",
// "profile.md") back to a (kind, name). It is the inverse of SubjectPath.
func ParseSubjectPath(p string) (SubjectKind, string, error) {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".md")
	if p == "profile" || p == "" {
		return KindProfile, "profile", nil
	}
	dir, name, ok := strings.Cut(p, "/")
	if !ok {
		return "", "", fmt.Errorf("path must be profile, areas/<name>, topics/<name>, or people/<name>")
	}
	switch dir {
	case "areas":
		return KindArea, name, nil
	case "topics":
		return KindTopic, name, nil
	case "people":
		return KindPeople, name, nil
	default:
		return "", "", fmt.Errorf("unknown memory folder %q", dir)
	}
}

// ProvenanceTag marks how a fact was learned.
type ProvenanceTag string

const (
	TagStated   ProvenanceTag = "stated"   // user said it directly
	TagObserved ProvenanceTag = "observed" // inferred from behavior
	TagInferred ProvenanceTag = "inferred" // a drawn conclusion
)

// Fact is one durable observation extracted from a session, persisted so
// synthesis never has to re-extract. Facts are grouped by (SubjectKind,
// SubjectName) and synthesized into subject bodies.
type Fact struct {
	ID          int64         `json:"id"`
	Source      string        `json:"source"`    // "crush", ...
	SessionID   string        `json:"sessionId"` // provenance
	SubjectKind SubjectKind   `json:"subjectKind"`
	SubjectName string        `json:"subjectName"`
	Text        string        `json:"text"`
	Tag         ProvenanceTag `json:"tag"`
	Sensitivity string        `json:"sensitivity,omitempty"`
	SessionDate time.Time     `json:"sessionDate"` // when the fact was said
	CreatedAt   time.Time     `json:"createdAt"`
}

// SubjectListing is the lightweight index row shown at session start so a
// client can decide which files to read.
type SubjectListing struct {
	Path        string      `json:"path"`
	Kind        SubjectKind `json:"kind"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Aliases     []string    `json:"aliases,omitempty"`
	Updated     time.Time   `json:"updated"`
}

// --- Extraction candidates ---

// Candidate is a fact extracted from a session's user turns, routed to a
// subject but not yet persisted.
type Candidate struct {
	Text        string   `json:"text"`        // the durable statement
	SubjectKind string   `json:"subjectKind"` // profile | area | topic | people
	SubjectName string   `json:"subjectName"` // slug of the target subject
	Description string   `json:"description"` // one-liner if this subject is new
	Aliases     []string `json:"aliases,omitempty"`
	Tag         string   `json:"tag"` // stated | observed | inferred
	Sensitivity string   `json:"sensitivity,omitempty"`
}

// --- Edge-to-center wire contract ---

// ProjectHints are the identity signals a client sends so the service can
// canonicalize a project without ever trusting a raw path as the id.
type ProjectHints struct {
	GitRemote string `json:"gitRemote,omitempty"` // normalized origin remote; strongest
	Path      string `json:"path,omitempty"`      // machine-local, weakest
	Name      string `json:"name,omitempty"`      // human label (web frontend)
}

// Turn is one normalized turn of a session. In the common path every
// uploaded turn has role "user".
type Turn struct {
	Index   int    `json:"index"`
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      string `json:"ts"` // ISO 8601 UTC
}

// SessionBatch is one session's turns plus origin context.
type SessionBatch struct {
	SessionID    string        `json:"sessionId"`
	Source       string        `json:"source"`
	ProjectHints *ProjectHints `json:"projectHints,omitempty"`
	StartedAt    string        `json:"startedAt"`
	EndedAt      string        `json:"endedAt,omitempty"`
	Turns        []Turn        `json:"turns"`
}

// IngestRequest is the /ingest envelope.
type IngestRequest struct {
	Collector string         `json:"collector"`
	Sessions  []SessionBatch `json:"sessions"`
}

// ContextBundle is what a client injects at session start: the profile in
// full, the subject listing, and (for a project session) that project's
// area file.
type ContextBundle struct {
	Profile   string           `json:"profile"`
	Area      string           `json:"area,omitempty"`
	Listing   []SubjectListing `json:"listing"`
	ProjectID string           `json:"projectId,omitempty"`
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

// StringList decodes from either a JSON string or an array of strings, so a
// caller passing a single git remote need not remember to wrap it in
// brackets. Both of these are accepted:
//
//	"repos": "github.com/org/repo"
//	"repos": ["github.com/org/repo", "tangled.org/who/repo"]
type StringList []string

// UnmarshalJSON accepts a bare string or an array of strings.
func (l *StringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*l = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("expected a string or an array of strings: %w", err)
	}
	*l = many
	return nil
}
