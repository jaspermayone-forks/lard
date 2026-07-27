package pipeline

import (
	"slices"

	"github.com/taciturnaxolotl/lard/internal/types"
)

// AttachRepos records git remotes on a subject and links it to the project
// registry.
//
// Every remote is normalized, so the ssh and https spellings of one repo
// collapse to a single entry. A project may legitimately have several remotes
// (a canonical repo plus mirrors), so all of them are kept, and all of them
// are registered as aliases pointing at the same project. That way resolving
// by any one mirror later finds the same project.
//
// The first remote that resolves sets the subject's ProjectID. Only areas get
// linked: topics and people are not projects, so a repo on them is descriptive
// only. Returns the remotes actually stored.
func AttachRepos(reg *Registry, sub *types.Subject, remotes []string) ([]string, error) {
	normalized := NormalizeRemotes(remotes)
	sub.Repos = normalized
	if len(normalized) == 0 || sub.Kind != types.KindArea {
		return normalized, nil
	}

	// Resolve on the first remote, then bind the rest as aliases of the same
	// project. Resolving each in turn would mint a separate project per
	// mirror, which is the opposite of what we want.
	projectID, err := reg.Resolve(&types.ProjectHints{
		GitRemote: normalized[0],
		Name:      sub.Name,
	})
	if err != nil {
		return normalized, err
	}
	sub.ProjectID = projectID
	for _, remote := range normalized[1:] {
		if err := reg.AddRemoteAlias(projectID, remote); err != nil {
			return normalized, err
		}
	}
	return normalized, nil
}

// NormalizeRemotes canonicalizes a list of git remotes, dropping blanks and
// duplicates while preserving the caller's order. The first entry is treated
// as canonical elsewhere, so order is meaningful.
func NormalizeRemotes(remotes []string) []string {
	var out []string
	for _, raw := range remotes {
		n := NormalizeRemote(raw)
		if n == "" || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	return out
}
