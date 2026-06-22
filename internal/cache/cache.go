// Package cache provides an in-memory, project-scoped cache of wiki concepts.
package cache

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// GCSPath is reserved for a future persisted JSONL cache.
const GCSPath = "cache/concepts.jsonl"

// Entry is the cached representation of a concept page.
type Entry struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Sources     []string               `json:"sources"`
}

// Reader is the subset of the GCS client used to build a concept cache.
type Reader interface {
	Prefix() string
	ListConcepts(ctx context.Context, includeDrafts bool) ([]gcs.WikiPage, error)
	GetPage(ctx context.Context, slug, category string) (*gcs.WikiPage, []byte, error)
}

type projectCache struct {
	entries []Entry
	bySlug  map[string]Entry
}

// Cache stores independent concept sets for each user/project GCS prefix.
type Cache struct {
	mu       sync.RWMutex
	projects map[string]projectCache
	random   *rand.Rand
}

// New creates an empty concept cache.
func New() *Cache {
	return &Cache{
		projects: make(map[string]projectCache),
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Build reads all published concepts for a project and replaces that project's
// in-memory cache. Individual page failures are skipped unless every listed
// concept fails.
func (c *Cache) Build(ctx context.Context, reader Reader) ([]Entry, error) {
	if reader == nil {
		return nil, fmt.Errorf("concept cache reader is nil")
	}

	concepts, err := reader.ListConcepts(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("list concepts: %w", err)
	}

	type buildResult struct {
		entry Entry
		err   error
	}
	results := make(chan buildResult, len(concepts))
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup

	for _, concept := range concepts {
		concept := concept
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			page, data, err := reader.GetPage(ctx, concept.Slug, "concepts")
			if err != nil {
				results <- buildResult{err: err}
				return
			}
			titleFallback := concept.Title
			if page != nil && page.Title != "" {
				titleFallback = page.Title
			}
			results <- buildResult{entry: parseEntry(concept.Slug, titleFallback, string(data))}
		}()
	}

	wg.Wait()
	close(results)

	entries := make([]Entry, 0, len(concepts))
	for result := range results {
		if result.err == nil {
			entries = append(entries, result.entry)
		}
	}
	if len(concepts) > 0 && len(entries) == 0 {
		return nil, fmt.Errorf("failed to read any of %d concepts", len(concepts))
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Slug < entries[j].Slug
	})
	bySlug := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		bySlug[entry.Slug] = entry
	}

	c.mu.Lock()
	c.projects[reader.Prefix()] = projectCache{entries: entries, bySlug: bySlug}
	c.mu.Unlock()

	return cloneEntries(entries), nil
}

// Search finds matching concepts in a project cache. The project is built on
// first use, then results are sampled without replacement using match score as
// the weight.
func (c *Cache) Search(ctx context.Context, reader Reader, query string, limit int) ([]search.Result, error) {
	project, ok := c.project(reader)
	if !ok {
		if _, err := c.Build(ctx, reader); err != nil {
			return nil, err
		}
		project, _ = c.project(reader)
	}

	if limit <= 0 {
		limit = 10
	}
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return []search.Result{}, nil
	}

	type candidate struct {
		result search.Result
		weight int
	}
	candidates := make([]candidate, 0, len(project.entries))
	for _, entry := range project.entries {
		score := entryScore(entry, words)
		if score == 0 {
			continue
		}
		candidates = append(candidates, candidate{
			weight: score,
			result: search.Result{
				Slug:    entry.Slug,
				Title:   entry.Title,
				Type:    "concept",
				Snippet: snippet(entry, words),
			},
		})
	}

	if len(candidates) <= limit {
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].weight > candidates[j].weight
		})
		results := make([]search.Result, len(candidates))
		for i, candidate := range candidates {
			results[i] = candidate.result
		}
		return results, nil
	}

	results := make([]search.Result, 0, limit)
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(results) < limit && len(candidates) > 0 {
		total := 0
		for _, candidate := range candidates {
			total += candidate.weight
		}
		pick := c.random.Intn(total)
		selected := 0
		for i, candidate := range candidates {
			pick -= candidate.weight
			if pick < 0 {
				selected = i
				break
			}
		}
		results = append(results, candidates[selected].result)
		candidates = append(candidates[:selected], candidates[selected+1:]...)
	}
	return results, nil
}

// Entry returns a cached concept for the requested project.
func (c *Cache) Entry(reader Reader, slug string) (Entry, bool) {
	project, ok := c.project(reader)
	if !ok {
		return Entry{}, false
	}
	entry, ok := project.bySlug[slug]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(entry), true
}

func (c *Cache) project(reader Reader) (projectCache, bool) {
	if reader == nil {
		return projectCache{}, false
	}
	c.mu.RLock()
	project, ok := c.projects[reader.Prefix()]
	c.mu.RUnlock()
	return project, ok
}

func parseEntry(slug, fallbackTitle, raw string) Entry {
	frontmatter, body := parseFrontmatter(raw)
	title := fallbackTitle
	if value := strings.TrimSpace(frontmatterString(frontmatter["title"])); value != "" {
		title = value
	}
	if title == "" {
		title = slug
	}
	return Entry{
		Slug:        slug,
		Title:       title,
		Body:        body,
		Frontmatter: frontmatter,
		Sources:     frontmatterSources(frontmatter),
	}
}

func parseFrontmatter(raw string) (map[string]interface{}, string) {
	frontmatter := make(map[string]interface{})
	if !strings.HasPrefix(raw, "---\n") {
		return frontmatter, raw
	}
	content := raw[4:]
	end := strings.Index(content, "\n---")
	if end < 0 {
		return frontmatter, raw
	}

	var listKey string
	for _, rawLine := range strings.Split(content[:end], "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && listKey != "" {
			values, _ := frontmatter[listKey].([]string)
			frontmatter[listKey] = append(values, cleanYAMLValue(strings.TrimSpace(trimmed[2:])))
			continue
		}

		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			listKey = ""
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if value == "" {
			frontmatter[key] = []string{}
			listKey = key
			continue
		}
		listKey = ""
		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			frontmatter[key] = parseInlineList(value)
			continue
		}
		frontmatter[key] = cleanYAMLValue(value)
	}
	return frontmatter, content[end+4:]
}

func parseInlineList(value string) []string {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	if value == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if cleaned := cleanYAMLValue(part); cleaned != "" {
			values = append(values, cleaned)
		}
	}
	return values
}

func cleanYAMLValue(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	value = strings.TrimSuffix(strings.TrimPrefix(value, "[["), "]]")
	return strings.TrimSpace(value)
}

func frontmatterSources(frontmatter map[string]interface{}) []string {
	for _, key := range []string{"sources", "source"} {
		switch value := frontmatter[key].(type) {
		case []string:
			return append([]string(nil), value...)
		case string:
			if value != "" {
				return []string{value}
			}
		}
	}
	return []string{}
}

func frontmatterString(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func entryScore(entry Entry, words []string) int {
	title := strings.ToLower(entry.Title)
	body := strings.ToLower(entry.Body)
	sources := strings.ToLower(strings.Join(entry.Sources, " "))
	frontmatter := strings.ToLower(fmt.Sprint(entry.Frontmatter))
	score := 0
	for _, word := range words {
		if strings.Contains(title, word) {
			score += 5
		}
		if strings.Contains(sources, word) {
			score += 3
		}
		if strings.Contains(body, word) {
			score += 2
		}
		if strings.Contains(frontmatter, word) {
			score++
		}
	}
	return score
}

func snippet(entry Entry, words []string) string {
	text := strings.TrimSpace(entry.Body)
	if text == "" {
		text = entry.Title
	}
	lower := strings.ToLower(text)
	start := 0
	for _, word := range words {
		if index := strings.Index(lower, word); index >= 0 {
			start = max(0, index-80)
			break
		}
	}
	end := min(len(text), start+200)
	return strings.TrimSpace(text[start:end])
}

func cloneEntries(entries []Entry) []Entry {
	cloned := make([]Entry, len(entries))
	for i, entry := range entries {
		cloned[i] = cloneEntry(entry)
	}
	return cloned
}

func cloneEntry(entry Entry) Entry {
	frontmatter := make(map[string]interface{}, len(entry.Frontmatter))
	for key, value := range entry.Frontmatter {
		if values, ok := value.([]string); ok {
			frontmatter[key] = append([]string(nil), values...)
		} else {
			frontmatter[key] = value
		}
	}
	entry.Frontmatter = frontmatter
	entry.Sources = append([]string(nil), entry.Sources...)
	return entry
}
