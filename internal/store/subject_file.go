package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/taciturnaxolotl/lard/internal/types"
)

// renderSubjectFile serializes a subject to its on-disk markdown form:
// YAML-ish frontmatter followed by the prose body.
func renderSubjectFile(sub *types.Subject) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", sub.Name)
	fmt.Fprintf(&b, "kind: %s\n", sub.Kind)
	fmt.Fprintf(&b, "description: %s\n", sub.Description)
	if len(sub.Aliases) > 0 {
		fmt.Fprintf(&b, "aliases: [%s]\n", strings.Join(sub.Aliases, ", "))
	}
	if len(sub.Repos) > 0 {
		fmt.Fprintf(&b, "repos: [%s]\n", strings.Join(sub.Repos, ", "))
	}
	if sub.ProjectID != "" {
		fmt.Fprintf(&b, "project_id: %s\n", sub.ProjectID)
	}
	fmt.Fprintf(&b, "updated: %s\n", sub.Updated.UTC().Format(time.RFC3339))
	b.WriteString("---\n\n")
	b.WriteString(normalizeBody(sub.Body))
	b.WriteString("\n")
	return b.String()
}

// normalizeBody ensures every non-blank content line is a markdown bullet,
// so synthesized output renders consistently even when the model omits the
// leading "- ".
func normalizeBody(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var out []string
	for _, ln := range lines {
		t := strings.TrimRight(ln, " \t")
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		// Leave headings and existing bullets alone.
		if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "*") {
			out = append(out, t)
			continue
		}
		out = append(out, "- "+trimmed)
	}
	return strings.Join(out, "\n")
}

// parseSubject reads a subject file's frontmatter and body. kind and name
// are authoritative (from the path); frontmatter fills the rest.
func parseSubject(kind types.SubjectKind, name, content string) *types.Subject {
	sub := &types.Subject{Kind: kind, Name: name}
	rest := content
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---")
		if end >= 0 {
			fm := content[4 : 4+end]
			rest = content[4+end+4:] // after closing ---
			rest = strings.TrimPrefix(rest, "\n")
			parseFrontmatter(fm, sub)
		}
	}
	sub.Body = strings.TrimSpace(rest)
	sub.Version = hashBody(renderSubjectFile(sub))
	return sub
}

func parseFrontmatter(fm string, sub *types.Subject) {
	for _, line := range strings.Split(fm, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "description":
			sub.Description = val
		case "project_id":
			sub.ProjectID = val
		case "aliases":
			sub.Aliases = parseListValue(val)
		case "repos":
			sub.Repos = parseListValue(val)
		case "updated":
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				sub.Updated = t
			}
		}
	}
}

// parseListValue reads a bracketed, comma-separated frontmatter value.
func parseListValue(val string) []string {
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	var out []string
	for _, v := range strings.Split(val, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
