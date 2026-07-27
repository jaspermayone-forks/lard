package pipeline

import (
	"net/url"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/store"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// NormalizeRemote canonicalizes a git remote so ssh and https forms of the
// same repo collapse to one alias: strip scheme/user/.git/trailing slash,
// lowercase host.
//
//	git@github.com:org/repo.git     → github.com/org/repo
//	https://github.com/org/repo/    → github.com/org/repo
func NormalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// SCP-like ssh syntax: git@host:path
	if !strings.Contains(raw, "://") {
		if i := strings.Index(raw, ":"); i > 0 && strings.Contains(raw[:i], "@") {
			host := raw[strings.Index(raw, "@")+1 : i]
			path := raw[i+1:]
			raw = "https://" + host + "/" + path
		} else {
			raw = "https://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimPrefix(path, "/")
	if host == "" {
		return strings.ToLower(path)
	}
	if path == "" {
		return host
	}
	return host + "/" + path
}

// Registry resolves project identity hints to canonical project ids.
type Registry struct {
	store *store.Store
}

// NewRegistry builds a Registry.
func NewRegistry(st *store.Store) *Registry { return &Registry{store: st} }

// Resolve maps hints to a canonical project id, creating the project on
// first contact. Resolution order: git remote → name → path → mint.
func (r *Registry) Resolve(hints *types.ProjectHints) (string, error) {
	if hints == nil {
		return "", nil
	}
	remote := NormalizeRemote(hints.GitRemote)

	// 1. Git remote, normalized. Strongest portable signal.
	if remote != "" {
		if p, err := r.store.FindProjectByAlias("remote", remote); err != nil {
			return "", err
		} else if p != nil {
			r.bindLearned(p.ID, remote, hints)
			return p.ID, nil
		}
	}

	// 2. Name or path alias.
	if hints.Name != "" {
		if p, err := r.store.FindProjectByAlias("name", hints.Name); err != nil {
			return "", err
		} else if p != nil {
			r.bindLearned(p.ID, remote, hints)
			return p.ID, nil
		}
	}
	if hints.Path != "" {
		if p, err := r.store.FindProjectByAlias("path", hints.Path); err != nil {
			return "", err
		} else if p != nil {
			r.bindLearned(p.ID, remote, hints)
			return p.ID, nil
		}
	}

	// 3. First contact: mint, seed every hint as an alias.
	p, err := r.store.CreateProject(displayName(hints), aliasMap(remote, hints))
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// AddRemoteAlias binds an additional git remote to an existing project, so a
// project with several mirrors resolves to the same id from any of them.
func (r *Registry) AddRemoteAlias(projectID, remote string) error {
	remote = NormalizeRemote(remote)
	if projectID == "" || remote == "" {
		return nil
	}
	return r.store.AddAlias(projectID, "remote", remote)
}

// bindLearned attaches any new hints as aliases on an existing project so
// later sessions resolve faster and from more signals.
func (r *Registry) bindLearned(projectID, remote string, hints *types.ProjectHints) {
	for kind, v := range aliasMap(remote, hints) {
		for _, value := range v {
			_ = r.store.AddAlias(projectID, kind, value)
		}
	}
}

func aliasMap(remote string, hints *types.ProjectHints) map[string][]string {
	m := map[string][]string{}
	if remote != "" {
		m["remote"] = []string{remote}
	}
	if hints.Path != "" {
		m["path"] = []string{hints.Path}
	}
	if hints.Name != "" {
		m["name"] = []string{hints.Name}
	}
	return m
}

func displayName(hints *types.ProjectHints) string {
	if hints.Name != "" {
		return hints.Name
	}
	if hints.GitRemote != "" {
		remote := NormalizeRemote(hints.GitRemote)
		if i := strings.LastIndex(remote, "/"); i >= 0 && i < len(remote)-1 {
			return remote[i+1:]
		}
		return remote
	}
	if hints.Path != "" {
		if i := strings.LastIndex(hints.Path, "/"); i >= 0 && i < len(hints.Path)-1 {
			return hints.Path[i+1:]
		}
	}
	return "unnamed project"
}
