// Package llm is the consolidator's model client, built on fantasy with
// Hyper (hyper.charm.land) as the provider. This is a cheap-but-capable-
// model job (deepseek-v4-flash by default), not a frontier one.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

const defaultHyperBaseURL = "https://hyper.charm.land"
const defaultModel = "deepseek-v4-flash"

// Client wraps a fantasy language model.
type Client struct {
	model fantasy.LanguageModel
}

// NewFromEnv builds a client from the environment:
//
//	LARD_HYPER_API_KEY (or HYPER_API_KEY) — required; a static hyper API key.
//	    A .env file in the working directory is honored.
//	LARD_MODEL          — default deepseek-v4-flash
//	LARD_HYPER_BASE_URL — default https://hyper.charm.land
//
// Hyper's OpenAI-compatible endpoint drives the model, same as crush.
func NewFromEnv(ctx context.Context) (*Client, error) {
	loadDotEnv()
	key := os.Getenv("LARD_HYPER_API_KEY")
	if key == "" {
		key = os.Getenv("HYPER_API_KEY")
	}
	if key == "" {
		return nil, fmt.Errorf("no hyper API key: set LARD_HYPER_API_KEY or HYPER_API_KEY")
	}
	modelID := os.Getenv("LARD_MODEL")
	if modelID == "" {
		modelID = defaultModel
	}
	provider, err := openaicompat.New(
		openaicompat.WithName("hyper"),
		openaicompat.WithAPIKey(key),
		openaicompat.WithBaseURL(hyperBaseURL()+"/v1"),
	)
	if err != nil {
		return nil, fmt.Errorf("hyper provider: %w", err)
	}
	model, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return nil, fmt.Errorf("model %q: %w", modelID, err)
	}
	return &Client{model: model}, nil
}

func hyperBaseURL() string {
	base := os.Getenv("LARD_HYPER_BASE_URL")
	if base == "" {
		base = defaultHyperBaseURL
	}
	return strings.TrimRight(base, "/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Complete runs a single-turn completion and returns the text.
func (c *Client) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	resp, err := c.model.Generate(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{
			fantasy.NewSystemMessage(system),
			fantasy.NewUserMessage(user),
		},
		MaxOutputTokens: fantasy.Opt(int64(maxTokens)),
	})
	if err != nil {
		return "", err
	}
	return resp.Content.Text(), nil
}

// ExtractJSON parses a JSON array from a model completion, tolerating
// markdown fences, surrounding prose, and even truncated output (the model
// hitting max_tokens mid-array). It closes unclosed brackets before parsing.
func ExtractJSON(raw string, out any) error {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "```"); i >= 0 {
		raw = strings.TrimPrefix(raw[i:], "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
	}
	start := strings.IndexAny(raw, "[{")
	if start < 0 {
		return fmt.Errorf("no JSON in completion")
	}
	raw = raw[start:]
	// Close unclosed brackets for truncated output.
	brackets := map[byte]byte{'[': ']', '{': '}'}
	var stack []byte
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '[', '{':
			stack = append(stack, raw[i])
		case ']', '}':
			if len(stack) > 0 && brackets[stack[len(stack)-1]] == raw[i] {
				stack = stack[:len(stack)-1]
			}
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		raw += string(brackets[stack[i]])
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("parse completion JSON: %w", err)
	}
	return nil
}
