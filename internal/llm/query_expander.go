package llm

import (
	"embed"
	"encoding/json"
	"fmt"
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
// Domain "lifestyle" uses the lifestyle prompt; anything else uses default.
func NewExpander(client *Client, domain string) (*QueryExpander, error) {
	if client == nil {
		return nil, nil
	}

	prompt, err := loadPrompt(domain)
	if err != nil {
		// Fall back to default on load error
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
// On any failure, returns nil so the caller can fall back to raw query.
func (e *QueryExpander) Expand(query string) *ExpandResult {
	if e == nil {
		return nil
	}

	raw, err := e.client.Chat(e.systemPrompt, query)
	if err != nil {
		return nil
	}

	return parseExpandResult(raw)
}

func parseExpandResult(raw string) *ExpandResult {
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
		return nil
	}
	if len(r.Keywords) == 0 {
		return nil
	}
	return &r
}
