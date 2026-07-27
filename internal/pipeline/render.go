package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/taciturnaxolotl/lard/internal/types"
)

// docNamespaces maps a scope to the document namespaces under it.
// Each namespace groups records by dotted key prefix:
//
//	profile → profile/identity, profile/preferences, profile/patterns
//	project/<id> → project/<id>/conventions, project/<id>/decisions, project/<id>/corrections
func docNamespaces(scopePrefix string) map[string][]string {
	if scopePrefix == "profile" {
		return map[string][]string{
			"profile/identity":    {"identity."},
			"profile/preferences": {"preferences.", "comms.", "workflow.", ""}, // catch-all too
			"profile/patterns":    {"patterns."},
		}
	}
	return map[string][]string{
		scopePrefix + "/conventions": {"conventions."},
		scopePrefix + "/decisions":   {"decisions."},
		scopePrefix + "/corrections": {"corrections."},
		scopePrefix + "/context":     {""}, // catch-all
	}
}

// RenderScope rebuilds the rendered documents for a scope prefix
// ("profile" or "project/<id>") from active records.
func (c *Consolidator) RenderScope(scopePrefix string) error {
	var scopeKind, projectID string
	if scopePrefix == "profile" {
		scopeKind = string(types.ScopeProfile)
	} else {
		scopeKind = string(types.ScopeProject)
		projectID = strings.TrimPrefix(scopePrefix, "project/")
	}
	recs, err := c.store.ListRecords(scopeKind, projectID, "", string(types.StatusActive))
	if err != nil {
		return err
	}
	for ns, prefixes := range docNamespaces(scopePrefix) {
		var mine []*types.Record
		for _, r := range recs {
			if matchesAnyPrefix(r.Key, prefixes, ns, scopePrefix) {
				mine = append(mine, r)
			}
		}
		body := renderDoc(ns, mine)
		if err := c.store.PutDoc(ns, body); err != nil {
			return err
		}
	}
	return nil
}

func matchesAnyPrefix(key string, prefixes []string, ns, scopePrefix string) bool {
	// The catch-all doc gets records that match no specific doc.
	specific := false
	for docNS, pfxs := range docNamespaces(scopePrefix) {
		if docNS == ns {
			continue
		}
		for _, p := range pfxs {
			if p != "" && strings.HasPrefix(key, p) {
				specific = true
			}
		}
	}
	for _, p := range prefixes {
		if p == "" {
			return !specific
		}
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func renderDoc(namespace string, recs []*types.Record) string {
	var b strings.Builder
	title := namespace[strings.LastIndex(namespace, "/")+1:]
	fmt.Fprintf(&b, "# %s\n\n", strings.ToUpper(title[:1])+title[1:])
	if len(recs) == 0 {
		b.WriteString("_No memories yet._\n")
		return b.String()
	}
	// Cluster by the key's second segment ("editor", "pkg-manager", ...):
	// broader buckets read as prose summaries rather than hyper-specific
	// bullet spam, closer to how a human would write the note.
	byCluster := map[string][]*types.Record{}
	for _, r := range recs {
		byCluster[keyCluster(r.Key)] = append(byCluster[keyCluster(r.Key)], r)
	}
	clusters := make([]string, 0, len(byCluster))
	for k := range byCluster {
		clusters = append(clusters, k)
	}
	sort.Strings(clusters)
	for _, k := range clusters {
		rs := byCluster[k]
		sort.Slice(rs, func(i, j int) bool { return rs[i].Confidence > rs[j].Confidence })
		fmt.Fprintf(&b, "## %s\n\n", k)
		// If all records in this cluster share the exact same key, list as bullets.
		// Otherwise merge into a prose paragraph — related facts read better together.
		sameKey := true
		for i := 1; i < len(rs); i++ {
			if rs[i].Key != rs[0].Key {
				sameKey = false
				break
			}
		}
		if sameKey {
			for _, r := range rs {
				marker := ""
				if r.Status == types.StatusContradicted {
					marker = " ⚠️ (unresolved)"
				}
				fmt.Fprintf(&b, "- %s _(%.0f%%)%s_\n", r.Value, r.Confidence*100, marker)
			}
		} else {
			// Merge into prose: pick the highest-confidence record as the lead,
			// append the rest as supporting details.
			lead := rs[0]
			marker := ""
			if lead.Status == types.StatusContradicted {
				marker = " ⚠️"
			}
			fmt.Fprintf(&b, "%s _(%.0f%%)%s_", lead.Value, lead.Confidence*100, marker)
			for _, r := range rs[1:] {
				marker = ""
				if r.Status == types.StatusContradicted {
					marker = " ⚠️"
				}
				fmt.Fprintf(&b, "; %s _(%.0f%%)%s_", r.Value, r.Confidence*100, marker)
			}
			b.WriteString(".\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// keyCluster maps a dotted key to its bucket: the first two segments for
// known prefixes ("preferences.editor"), the bare prefix otherwise.
func keyCluster(key string) string {
	parts := strings.SplitN(key, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return key
}

// Decay ages out low-confidence dynamic records. Static records decay far
// more slowly; user-sourced records never auto-prune.
func (c *Consolidator) Decay() error {
	const (
		dynamicMaxAge = 30 * 24 * time.Hour
		dynamicMinConf = 0.25
		staticMaxAge  = 365 * 24 * time.Hour
		staticMinConf = 0.1
	)
	recs, err := c.store.ListRecords("", "", "", string(types.StatusActive))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, r := range recs {
		if r.Source == types.SourceUser {
			continue
		}
		age := now.Sub(r.LastSeenAt)
		var prune bool
		if r.Class == types.ClassDynamic {
			prune = age > dynamicMaxAge && r.Confidence < dynamicMinConf
		} else {
			prune = age > staticMaxAge && r.Confidence < staticMinConf
		}
		if prune {
			r.Status = types.StatusSuperseded
			if err := c.store.UpsertRecord(r); err != nil {
				return err
			}
		}
	}
	return nil
}
