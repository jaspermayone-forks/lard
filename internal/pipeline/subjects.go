package pipeline

import (
	"errors"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// SubjectStore is the slice of the store the subject write paths need.
type SubjectStore interface {
	GetSubject(kind types.SubjectKind, name string) (*types.Subject, error)
	PutSubject(sub *types.Subject, synthFactID int64) error
}

// ErrVersionConflict is returned when a write carries a stale version token.
var ErrVersionConflict = errors.New("version mismatch")

// SubjectPatch is one write to a subject file. Nil/empty fields leave the
// existing value alone.
type SubjectPatch struct {
	Body        *string
	Description string
	Aliases     []string
	Repos       []string
	Version     string // optimistic concurrency token; "" or "new" opts out
}

// ApplyPatch is the canonical subject write path shared by the HTTP and MCP
// doors: load (or create), check the version token, apply the patch, persist.
// The current subject is returned on a version conflict so the caller can
// surface it for merging. reg may be nil when repos never need linking.
func ApplyPatch(st *store.Store, reg *Registry, kind types.SubjectKind, name string, p SubjectPatch) (*types.Subject, error) {
	sub, err := st.GetSubject(kind, name)
	if err != nil {
		return nil, err
	}
	if p.Version != "" && p.Version != "new" && sub != nil && sub.Version != p.Version {
		return sub, ErrVersionConflict
	}
	if sub == nil {
		sub = &types.Subject{Kind: kind, Name: name}
	}
	if p.Body != nil {
		sub.Body = *p.Body
	}
	if p.Description != "" {
		sub.Description = p.Description
	}
	if p.Aliases != nil {
		sub.Aliases = p.Aliases
	}
	if sub.Description == "" {
		sub.Description = name
	}
	if p.Repos != nil {
		if _, err := AttachRepos(reg, sub, p.Repos); err != nil {
			return nil, err
		}
	}
	if err := st.PutSubject(sub, 0); err != nil {
		return nil, err
	}
	return sub, nil
}

// AppendLine adds one fact to a subject without resending its body, creating
// the subject if absent. A leading "- " is added if missing.
func AppendLine(st SubjectStore, kind types.SubjectKind, name, line string) (*types.Subject, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errors.New("line required")
	}
	sub, err := st.GetSubject(kind, name)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		sub = &types.Subject{Kind: kind, Name: name, Description: name}
	}
	if !strings.HasPrefix(line, "-") {
		line = "- " + line
	}
	if sub.Body == "" {
		sub.Body = line
	} else {
		sub.Body = strings.TrimRight(sub.Body, "\n") + "\n" + line
	}
	if err := st.PutSubject(sub, 0); err != nil {
		return nil, err
	}
	return sub, nil
}
