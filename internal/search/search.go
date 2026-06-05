package search

import (
	"context"
	"log"
	"strings"

	"github.com/rayert/llm-wiki-bff/internal/gcs"
)

// Index provides in-memory full-text search over wiki content.
type Index struct {
	sources  []gcs.WikiPage
	concepts []gcs.WikiPage
	content  map[string]string // slug -> plain text content
}

// Result is a single search hit.
type Result struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Type    string `json:"type"` // "source" or "concept"
	Snippet string `json:"snippet"`
}

// NewIndex creates an empty search index.
func NewIndex() *Index {
	return &Index{content: make(map[string]string)}
}

// SourceCount returns the number of indexed sources.
func (idx *Index) SourceCount() int { return len(idx.sources) }

// ConceptCount returns the number of indexed concepts.
func (idx *Index) ConceptCount() int { return len(idx.concepts) }

// Build loads all wiki content from GCS into the index.
func (idx *Index) Build(gcsClient *gcs.Client) error {
	ctx := context.Background()

	// Load sources
	sources, err := gcsClient.ListSources(ctx)
	if err != nil {
		return err
	}
	idx.sources = sources
	for _, s := range sources {
		_, data, err := gcsClient.GetPage(ctx, s.Slug, "sources")
		if err != nil {
			log.Printf("index: skip source %s: %v", s.Slug, err)
			continue
		}
		idx.content[s.Slug] = stripMarkdown(string(data))
	}

	// Load concepts
	concepts, err := gcsClient.ListConcepts(ctx, true)
	if err != nil {
		return err
	}
	idx.concepts = concepts
	for _, c := range concepts {
		_, data, err := gcsClient.GetPage(ctx, c.Slug, "concepts")
		if err != nil {
			log.Printf("index: skip concept %s: %v", c.Slug, err)
			continue
		}
		idx.content[c.Slug] = stripMarkdown(string(data))
	}

	return nil
}

// Search performs full-text search across sources and concepts.
func (idx *Index) Search(query string, limit int) []Result {
	if limit <= 0 {
		limit = 10
	}
	query = strings.ToLower(query)
	words := strings.Fields(query)

	var results []Result

	// Search sources
	for _, s := range idx.sources {
		text := strings.ToLower(idx.content[s.Slug])
		score := matchScore(text, words)
		if score > 0 {
			results = append(results, Result{
				Slug:    s.Slug,
				Title:   s.Title,
				Type:    "source",
				Snippet: makeSnippet(idx.content[s.Slug], words, 200),
			})
		}
	}

	// Search concepts
	for _, c := range idx.concepts {
		text := strings.ToLower(idx.content[c.Slug])
		score := matchScore(text, words)
		if score > 0 {
			results = append(results, Result{
				Slug:    c.Slug,
				Title:   c.Title,
				Type:    "concept",
				Snippet: makeSnippet(idx.content[c.Slug], words, 200),
			})
		}
		_ = score // suppress unused
	}

	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// matchScore returns the number of matching words found in text.
func matchScore(text string, words []string) int {
	score := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			score++
		}
	}
	return score
}

// makeSnippet extracts a context snippet around the first matching word.
func makeSnippet(text string, words []string, maxLen int) string {
	lower := strings.ToLower(text)
	best := -1
	for _, w := range words {
		if idx := strings.Index(lower, w); idx >= 0 {
			if best < 0 || idx < best {
				best = idx
			}
		}
	}
	if best < 0 {
		if len(text) > maxLen {
			return text[:maxLen] + "..."
		}
		return text
	}

	start := best - 40
	if start < 0 {
		start = 0
	}
	end := start + maxLen
	if end > len(text) {
		end = len(text)
	}
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(text) {
		suffix = "..."
	}
	return prefix + text[start:end] + suffix
}

// stripMarkdown removes basic markdown syntax for FTS.
func stripMarkdown(s string) string {
	// Remove frontmatter
	if strings.HasPrefix(s, "---") {
		if idx := strings.Index(s[3:], "---"); idx >= 0 {
			s = s[idx+6:]
		}
	}
	// Remove headers (###, ##, #)
	for _, prefix := range []string{"### ", "## ", "# "} {
		s = strings.ReplaceAll(s, prefix, "")
	}
	// Remove wikilinks [[...]]
	for {
		start := strings.Index(s, "[[")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "]]")
		if end < 0 {
			break
		}
		s = s[:start] + s[start+2:start+end] + s[start+end+2:]
	}
	return s
}
