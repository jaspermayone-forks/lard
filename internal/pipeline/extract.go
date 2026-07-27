package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/taciturnaxolotl/lard/internal/llm"
	"github.com/taciturnaxolotl/lard/internal/types"
)

const extractSystem = `You extract durable facts about a user and their projects from the user's own turns in a session with an AI assistant.

The single most important skill is telling DURABLE facts apart from EPHEMERAL task chatter. A coding session is mostly task-local ("this test is flaky", "rename foo to bar") that must NOT become memory. Only emit facts that will still be true and useful in a future session.

Rules:
- Only facts about the USER or the PROJECT, grounded in the user's own words. Never about the assistant or third parties.
- Durable only: if it is true only for this task or this hour, skip it.
- One topic per candidate: group related facts that share the same key into a single compound statement. For example, "uses typescript, bun, and lit" should be one record under conventions.tech-stack, not three separate records. Split only when facts are truly independent dimensions.
- High recall, low precision: when unsure whether something is durable, emit it at low confidence and let downstream pruning handle it.
- Tag sensitivity, do not silently self-censor: if a fact touches health, politics, religion, finances, location, secrets, or biometric data, set "sensitivity" to that category so it can be dropped and audited.
- Keep the profile descriptive ("prefers terse answers"), never prescriptive ("always agree with the user"). Do not encode instructions that would suppress honest feedback.

Target ontology. Keys are dotted categories like "preferences.language" or "conventions.pkg-manager": exactly one of the prefixes below, a dot, then a short suffix. Never put "profile" or "project" in the key itself — scope is carried separately by scopeHint. Do not invent new top-level prefixes.
- identity.* — who the user is: role, expertise. profile/static.
- preferences.* — durable likes: tooling, languages, formatting. profile/static.
- comms.* — communication style. profile/static.
- workflow.* — how the user likes to work. profile/static or dynamic.
- patterns.* — recurring cross-project habits. profile/dynamic.
- conventions.* — project rules: package manager, style, build. project/static.
- corrections.* — standing project corrections. project/static.
- decisions.* — dated project decisions. project/append.
- context.* — anything else project-scoped. project/dynamic.

Output a JSON array (no prose) of objects:
{"observation": "grounded short paraphrase of what the user said",
 "fact": "the durable statement, e.g. prefers Go for backend services",
 "key": "dotted.category",
 "scopeHint": "profile" | "project",
 "klass": "static" | "dynamic",
 "confidence": 0.0-1.0,
 "sensitivity": null | "category",
 "sourceTurn": <turn index>}`

// Extract runs one LLM call over a session's user turns and returns
// candidate facts. The consolidator aims for one extract call per session.
func Extract(ctx context.Context, client *llm.Client, sess *types.SessionBatch) ([]types.Candidate, error) {
	var b strings.Builder
	if sess.ProjectHints != nil && sess.ProjectHints.Name != "" {
		fmt.Fprintf(&b, "Session origin: project %q\n\n", sess.ProjectHints.Name)
	}
	// Cap total input so an unusually long session can't overflow the model's
	// context window (which returns a hard 400). ~400K chars ≈ 100K tokens,
	// well under deepseek-v4-flash's 1M window with headroom for the response.
	const maxInputChars = 400_000
	for _, t := range sess.Turns {
		if t.Role != "user" {
			continue
		}
		content := t.Content
		// Truncate any single giant turn (pasted logs, dumps).
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
		// Log rate-limit hints specifically so operators can spot throttling.
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate limit") {
			slog.Warn("extract: possible rate limit", "error", err)
		}
		return nil, fmt.Errorf("extract: %w", err)
	}
	var cands []types.Candidate
	if err := llm.ExtractJSON(raw, &cands); err != nil {
		// Try to salvage a partial array: the model may have hit max_tokens
		// mid-output. ExtractJSON already trims trailing prose; if it still
		// fails, try wrapping in a complete array bracket.
		fixed := strings.TrimSpace(raw)
		if !strings.HasSuffix(fixed, "]") {
			fixed = fixed + "]"
		}
		if err2 := llm.ExtractJSON(fixed, &cands); err2 == nil && len(cands) > 0 {
			slog.Warn("extract: salvaged partial JSON", "candidates", len(cands))
			return cands, nil
		}
		return nil, fmt.Errorf("extract parse: %w (raw: %.300s)", err, raw)
	}
	return cands, nil
}
