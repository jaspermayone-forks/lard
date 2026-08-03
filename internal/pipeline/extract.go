// Package pipeline is lard's memory synthesizer. Consolidation runs in
// three checkpointed phases: extract facts from each session (parallel,
// durable), group facts by subject, then synthesize each dirty subject
// file from its facts (parallel per subject). Persisting extracted facts
// means synthesis never re-extracts, so the expensive LLM work survives
// crashes and re-runs.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/types"
)

// sensitivityBlocklist holds inference categories never persisted. Facts
// tagged with any of these are dropped at extraction, before storage.
var sensitivityBlocklist = map[string]bool{
	"race": true, "ethnicity": true, "religion": true, "orientation": true,
	"gender-identity": true, "immigration": true, "disability": true,
	"illness": true, "health": true, "politics": true, "sexual": true,
	"abuse": true, "financial": true, "finances": true, "criminal": true,
	"location": true, "biometric": true, "secrets": true, "dob": true,
}

const extractSystem = `You extract durable facts about a user and their projects from the user's own turns in a coding session, and route each fact to a memory subject.

The single most important skill is telling DURABLE facts apart from EPHEMERAL task chatter. A coding session is mostly task-local ("this test is flaky", "rename foo to bar") that must NOT become memory. Only emit facts that will still be true and useful in a future session.

DURABLE MEANS CONCRETE. Memory is worth nothing if it stays vague. Keep the specifics that survive the session: repo names and URLs, domains, deploy targets and paths, hosts and ports, framework/library choices and versions, config conventions, file locations, API endpoints, credentials locations (never the secrets themselves), and the reasoning behind decisions. Prefer "deploys to Cloudflare Pages at s.dunkirk.sh" over "has a deployment"; "prefers Bun and Bun.serve over Node" over "prefers modern tooling". If the user explains WHY they chose something, fold the reason in.

SUBJECTS. Every fact belongs to exactly one subject, chosen by the NATURE of the fact (not where it was said):
- kind "profile" (name "profile"): durable identity only — name, role, employer, education, location, contact, pronouns. The test: "still true in 3 months, independent of any project?" Keep this SMALL. Anything dated, "currently", or tied to one project does NOT go here.
- kind "area": one subject per project or ongoing thing. name = a short slug of the project (e.g. "crush", "battleship-arena"). Facts about what a project is, its stack, conventions, decisions, architecture, deployment, and status. "user is a developer of X" is an area fact for X, NOT profile.
- kind "topic": cross-cutting domain facts spanning projects. name = the domain slug (e.g. "software-projects", "ctf-security", "frc-robotics", "hardware", "photography"). Durable preferences and skills that aren't project-specific go here (e.g. "prefers Bun over React" → software-projects).
- kind "people": one subject per person. name = a slug of their name. Facts about who they are and how the user works with them.

You are given the EXISTING SUBJECTS (name + description + aliases). ALWAYS route a fact to an existing subject when it fits — match on name or alias — instead of inventing a near-duplicate. Only create a new subject when nothing fits; then give it a short "description" (one line: what it covers) and optional "aliases".

Rules:
- Only facts about the USER or the PROJECT, grounded in the user's own words. Never about the assistant.
- Durable only. Skip anything true only for this task or hour — bug fixes in progress, one-off commands, "currently debugging X".
- One clear statement per fact, but group tightly-related details into ONE fact rather than splitting hairs ("uses Bun, Drizzle, and Lit" is one stack fact, not three).
- Prefer durable phrasing over specifics that go stale ("meeting-heavy mornings" > "10:00 standup"), but never use that as an excuse to drop load-bearing specifics.
- Calibrate to evidence: one mention → "mentioned X once", not "X expert".
- Tag sensitivity: if a fact touches race, ethnicity, religion, sexual orientation, gender identity, immigration, disability, illness/health, politics, sexual history, abuse, finances, criminal history, real-time location, biometric data, or a date of birth, set "sensitivity" to that category. It will be dropped.
- Provenance "tag" is always "stated" here (these are the user's own words).
- Keep facts descriptive, never prescriptive instructions that would suppress honest feedback.

Output a JSON array (no prose) of objects:
{"text": "the durable fact, with its specifics",
 "subjectKind": "profile" | "area" | "topic" | "people",
 "subjectName": "slug",
 "description": "one-line subject description (only if creating a new subject)",
 "aliases": ["optional", "synonyms"],
 "tag": "stated",
 "sensitivity": null | "category"}`

// Extract runs one LLM call over a session's user turns and returns routed
// candidate facts. existingSubjects is the current listing, injected so
// facts route to existing subjects instead of spawning duplicates.
func Extract(ctx context.Context, client *llm.Client, sess *types.SessionBatch, existingSubjects []types.SubjectListing) ([]types.Candidate, error) {
	var b strings.Builder
	if len(existingSubjects) > 0 {
		b.WriteString("EXISTING SUBJECTS (route to these when a fact fits):\n")
		for _, s := range existingSubjects {
			fmt.Fprintf(&b, "- %s/%s", s.Kind, s.Name)
			if s.Description != "" {
				fmt.Fprintf(&b, " — %s", s.Description)
			}
			if len(s.Aliases) > 0 {
				fmt.Fprintf(&b, " (aka %s)", strings.Join(s.Aliases, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if sess.ProjectHints != nil && sess.ProjectHints.Name != "" {
		fmt.Fprintf(&b, "This session happened in project %q.\n\n", sess.ProjectHints.Name)
	}
	b.WriteString("USER TURNS:\n\n")
	const maxInputChars = 400_000
	for _, t := range sess.Turns {
		if t.Role != "user" {
			continue
		}
		content := t.Content
		if len(content) > 20_000 {
			content = content[:20_000] + "\n…[truncated]"
		}
		if b.Len()+len(content) > maxInputChars {
			break
		}
		fmt.Fprintf(&b, "[turn %d] %s\n\n", t.Index, content)
	}
	user := b.String()
	if strings.TrimSpace(user) == "" {
		return nil, nil
	}
	raw, err := client.Complete(ctx, extractSystem, user, 8192)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	var cands []types.Candidate
	if err := llm.ExtractJSON(raw, &cands); err != nil {
		return nil, fmt.Errorf("extract parse: %w (raw: %.200s)", err, raw)
	}
	return cands, nil
}

// slugify normalizes a subject name to a stable file-safe slug.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '.' || r == '_' || r == '/' || r == '-' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
