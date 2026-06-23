package llm

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

//go:embed prompts/*.txt
var promptFS embed.FS

// ExpandResult is a structured query expansion from the LLM.
type ExpandResult struct {
	Keywords          []string `json:"keywords"`
	Districts         []string `json:"districts,omitempty"`
	Facilities        []string `json:"facilities,omitempty"`
	Suggestions       []string `json:"suggestions,omitempty"`
	AdditionalRequest []string `json:"additional_request,omitempty"`
}

// QueryExpander expands user queries using a domain-specific LLM prompt.
type QueryExpander struct {
	client       *Client
	systemPrompt string
}

// NewExpander creates a QueryExpander for the given domain.
func NewExpander(client *Client, domain string) (*QueryExpander, error) {
	if client == nil {
		return nil, nil
	}

	prompt, err := loadPrompt(domain)
	if err != nil {
		prompt, _ = loadPrompt("default")
	}

	return &QueryExpander{
		client:       client,
		systemPrompt: prompt,
	}, nil
}

func loadPrompt(domain string) (string, error) {
	filename := domain + ".txt"
	if domain == "" {
		filename = "default.txt"
	}
	data, err := promptFS.ReadFile("prompts/" + filename)
	if err != nil {
		return "", fmt.Errorf("load prompt %s: %w", filename, err)
	}
	return string(data), nil
}

// Expand rewrites a user query into structured search keywords.
// Returns the expansion result; on failure returns nil AND the error
// so callers can log and gracefully degrade.
func (e *QueryExpander) Expand(query string) (*ExpandResult, error) {
	if e == nil {
		return nil, nil
	}

	raw, err := e.client.Chat(e.systemPrompt, query)
	if err != nil {
		log.Printf("[expander] LLM call failed for query %q: %v", query, err)
		return nil, fmt.Errorf("expander: chat: %w", err)
	}

	result, err := parseExpandResult(raw)
	if err != nil {
		log.Printf("[expander] parse failed for query %q (raw=%q): %v", query, raw, err)
		return nil, fmt.Errorf("expander: parse: %w", err)
	}

	return result, nil
}

func parseExpandResult(raw string) (*ExpandResult, error) {
	raw = strings.TrimSpace(raw)

	// Strip markdown code fences if present
	if strings.HasPrefix(raw, "```") {
		lines := strings.SplitN(raw, "\n", 3)
		if len(lines) >= 2 {
			raw = strings.TrimPrefix(raw, lines[0]+"\n")
			raw = strings.TrimSuffix(raw, "\n```")
		}
	}

	var r ExpandResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("json parse: %w (raw=%q)", err, raw[:min(len(raw), 200)])
	}
	if len(r.Keywords) == 0 {
		return nil, fmt.Errorf("no keywords in response (raw=%q)", raw[:min(len(raw), 200)])
	}
	return &r, nil
}
