// Package cache provides an in-memory, project-scoped cache of wiki concepts.
package cache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// GCSPath is the persisted JSONL concept cache written by the pipeline.
const GCSPath = "cache/concepts.jsonl"

// Entry is the cached representation of a concept page.
type Entry struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Sources     []string               `json:"sources"`
}

type conceptReader interface {
	ListConcepts(ctx context.Context, includeDrafts bool) ([]gcs.WikiPage, error)
	GetPage(ctx context.Context, slug, category string) (*gcs.WikiPage, []byte, error)
}

// Reader is the subset of the GCS client used to build a project-scoped concept cache.
type Reader interface {
	conceptReader
	Prefix() string
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

// Build loads the project concept cache, preferring the persisted JSONL file.
// When JSONL is unavailable it falls back to reading each concept page from
// storage. Individual page failures are skipped unless every listed concept
// fails.
func (c *Cache) Build(ctx context.Context, reader conceptReader) ([]Entry, error) {
	if reader == nil {
		return nil, fmt.Errorf("concept cache reader is nil")
	}

	if err := c.loadJSONL(ctx, reader); err == nil {
		if project, ok := c.project(reader); ok {
			return cloneEntries(project.entries), nil
		}
	}

	return c.buildFromPages(ctx, reader)
}

func (c *Cache) buildFromPages(ctx context.Context, reader conceptReader) ([]Entry, error) {
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
	c.projects[prefixForReader(reader)] = projectCache{entries: entries, bySlug: bySlug}
	c.mu.Unlock()

	if writer, ok := reader.(interface {
		WriteBytes(context.Context, []byte, string) (string, error)
	}); ok {
		_, _ = writer.WriteBytes(ctx, marshalJSONL(entries), GCSPath)
	}

	return cloneEntries(entries), nil
}

// IsReady reports whether the cache has a populated entry for the given prefix.
func (c *Cache) IsReady(prefix string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	project, ok := c.projects[prefix]
	return ok && len(project.entries) > 0
}

// Prefixes returns all cached project prefixes.
func (c *Cache) Prefixes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	prefixes := make([]string, 0, len(c.projects))
	for prefix := range c.projects {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// Search finds matching concepts in a project cache. The project is loaded from
// JSONL or built on first use with a 10-second timeout, then results are
// sampled without replacement using match score as the weight.
func (c *Cache) Search(ctx context.Context, reader conceptReader, query string, limit int) ([]search.Result, error) {
	project, ok := c.project(reader)
	if !ok {
		if err := c.loadJSONL(ctx, reader); err != nil {
			buildCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := c.Build(buildCtx, reader); err != nil {
				return nil, fmt.Errorf("concept cache build for %q: %w", prefixForReader(reader), err)
			}
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

// Query loads a persisted JSONL cache when available, then searches it.
func (c *Cache) Query(ctx context.Context, reader conceptReader, query string, limit int) ([]search.Result, error) {
	if err := c.loadJSONL(ctx, reader); err != nil {
		if _, buildErr := c.Build(ctx, reader); buildErr != nil {
			return nil, buildErr
		}
	}
	return c.Search(ctx, reader, query, limit)
}

// All returns all cached entries, building or loading the cache on first use.
func (c *Cache) All(ctx context.Context, reader conceptReader) ([]Entry, error) {
	if project, ok := c.project(reader); ok {
		return cloneEntries(project.entries), nil
	}
	if err := c.loadJSONL(ctx, reader); err == nil {
		if project, ok := c.project(reader); ok {
			return cloneEntries(project.entries), nil
		}
	}
	return c.Build(ctx, reader)
}

// Entry returns a cached concept for the requested project.
func (c *Cache) Entry(reader conceptReader, slug string) (Entry, bool) {
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

func (c *Cache) project(reader conceptReader) (projectCache, bool) {
	if reader == nil {
		return projectCache{}, false
	}
	c.mu.RLock()
	project, ok := c.projects[prefixForReader(reader)]
	c.mu.RUnlock()
	return project, ok
}

func (c *Cache) loadJSONL(ctx context.Context, reader conceptReader) error {
	fileReader, ok := reader.(interface {
		ReadFile(context.Context, string) ([]byte, error)
	})
	if !ok {
		return fmt.Errorf("concept cache reader cannot read JSONL")
	}
	data, err := fileReader.ReadFile(ctx, GCSPath)
	if err != nil {
		return err
	}
	entries, err := unmarshalJSONL(data)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("concept cache JSONL is empty")
	}
	bySlug := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		bySlug[entry.Slug] = entry
	}
	c.mu.Lock()
	c.projects[prefixForReader(reader)] = projectCache{entries: entries, bySlug: bySlug}
	c.mu.Unlock()
	return nil
}

func prefixForReader(reader conceptReader) string {
	if prefixed, ok := reader.(interface{ Prefix() string }); ok {
		prefix := prefixed.Prefix()
		if tokenized, ok := reader.(interface{ ViewToken() string }); ok {
			if token := tokenized.ViewToken(); token != "" && token != "legacy" {
				return prefix + ":" + token
			}
		}
		return prefix
	}
	return ""
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
	matter := make(map[string]interface{})
	if !strings.HasPrefix(raw, "---\n") {
		return matter, raw
	}
	body, err := fm.MustParse(strings.NewReader(raw), &matter)
	if err != nil {
		return make(map[string]interface{}), raw
	}
	return matter, string(body)
}

func frontmatterSources(frontmatter map[string]interface{}) []string {
	for _, key := range []string{"sources", "source"} {
		switch value := frontmatter[key].(type) {
		case []string:
			return append([]string(nil), value...)
		case []interface{}:
			sources := make([]string, 0, len(value))
			for _, item := range value {
				if source := strings.TrimSpace(fmt.Sprint(item)); source != "" {
					sources = append(sources, source)
				}
			}
			return sources
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

func marshalJSONL(entries []Entry) []byte {
	var builder strings.Builder
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		builder.Write(data)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func unmarshalJSONL(data []byte) ([]Entry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	entries := make([]Entry, 0)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(entries) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
