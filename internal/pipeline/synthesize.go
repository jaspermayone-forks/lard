package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/types"
)

const synthesizeSystem = `You maintain one memory file about a single subject. You are given the subject's kind, its current file body (may be empty), and a list of facts gathered from the user's sessions. Rewrite the file body so a future assistant reading it knows the subject without any other context.

Rules:
- Produce a tight set of bullet points, each a self-contained fact. Merge related facts into single coherent bullets; do not just concatenate.
- Keep the specifics. Names, URLs, domains, hosts, paths, versions, stack components, deploy targets, and the reasoning behind decisions are the value of memory — merging bullets must never mean stripping them out. "Cloudflare Workers-based PWA deployed on Cloudflare Pages at s.dunkirk.sh" is the right level; "a web project" is not.
- Preserve everything in the current body that is not contradicted; integrate the new facts. This is a revise-in-place, not a fresh write — respect prior content (it may include the user's own edits).
- When new facts supersede old ones (a project changed stack, a role changed), replace the stale statement rather than keeping both.
- Prefer durable phrasing over specifics that go stale, but never use that as an excuse to drop load-bearing specifics. Drop task-local noise that slipped through.
- For an "area" subject, aim to cover, when known: what the project is, its stack and key libraries, where it runs/deploys, conventions and decisions (with reasons where given), and current goals or open threads.
- Every bullet starts with a provenance tag in brackets: [stated] (user said it), [observed], or [inferred]. Use [stated] unless the input fact says otherwise.
- For a "profile" subject: keep ONLY durable identity (name, role, education, location, contact, pronouns). Move anything project-specific out (omit it — it lives elsewhere).
- Keep it concise — a memory file, not a transcript — but favor completeness over brevity when they conflict. A rich 15-bullet subject beats a vague 4-bullet one.

Output ONLY the file body (the bullet lines). No frontmatter, no headings, no preamble.`

// Synthesize rewrites a subject's body from the current body plus its facts.
// It returns the new body; the caller persists it with the fact watermark.
func Synthesize(ctx context.Context, client *llm.Client, sub *types.Subject, facts []types.Fact) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "SUBJECT: %s (kind: %s)\n", sub.Name, sub.Kind)
	if sub.Description != "" {
		fmt.Fprintf(&b, "DESCRIPTION: %s\n", sub.Description)
	}
	b.WriteString("\nCURRENT BODY:\n")
	if strings.TrimSpace(sub.Body) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(sub.Body)
		b.WriteString("\n")
	}
	b.WriteString("\nNEW FACTS (chronological):\n")
	for _, f := range facts {
		fmt.Fprintf(&b, "- %s\n", f.Text)
	}
	body, err := client.Complete(ctx, synthesizeSystem, b.String(), 4096)
	if err != nil {
		return "", fmt.Errorf("synthesize %s/%s: %w", sub.Kind, sub.Name, err)
	}
	body = strings.TrimSpace(stripFrontmatter(body))
	if body == "" {
		// Thin subjects (one or two facts) sometimes come back blank. Falling
		// back to the facts verbatim beats leaving a subject as an empty stub.
		if fallback := factBullets(facts); fallback != "" {
			return fallback, nil
		}
		return "", fmt.Errorf("synthesize %s/%s: empty body", sub.Kind, sub.Name)
	}
	return body, nil
}

// factBullets renders facts in the on-disk bullet format, tags included.
func factBullets(facts []types.Fact) string {
	var b strings.Builder
	for _, f := range facts {
		text := strings.TrimSpace(f.Text)
		if text == "" {
			continue
		}
		tag := f.Tag
		if tag == "" {
			tag = types.TagStated
		}
		fmt.Fprintf(&b, "- [%s] %s\n", tag, text)
	}
	return strings.TrimSpace(b.String())
}

// stripFrontmatter removes any accidental frontmatter or code fences the
// model may have wrapped the body in.
func stripFrontmatter(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	if strings.HasPrefix(s, "---") {
		if end := strings.Index(s[3:], "---"); end >= 0 {
			s = s[3+end+3:]
		}
	}
	return strings.TrimSpace(s)
}
