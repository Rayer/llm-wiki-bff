package cache

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

const GCSPath = "cache/concepts.jsonl"

type Entry struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	Frontmatter map[string]interface{} `json:"frontmatter,omitempty"`
	Sources     []string               `json:"sources,omitempty"`
}

type Reader interface {
	ReadFile(ctx context.Context, relPath string) ([]byte, error)
	ListConcepts(ctx context.Context, includeDrafts bool) ([]gcs.WikiPage, error)
	GetPage(ctx context.Context, slug, category string) (*gcs.WikiPage, []byte, error)
	WriteBytes(ctx context.Context, data []byte, gcsRelPath string) (string, error)
}

type Cache struct{}

func New() *Cache { return &Cache{} }

func (c *Cache) Query(ctx context.Context, reader Reader, q string, limit int) ([]search.Result, error) {
	entries, err := loadEntries(ctx, reader)
	if err != nil { return nil, err }
	if limit <= 0 { limit = 10 }
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 { return nil, nil }
	return ranked(entries, words, limit), nil
}

func (c *Cache) All(ctx context.Context, reader Reader) ([]Entry, error) {
	return loadEntries(ctx, reader)
}

func loadEntries(ctx context.Context, reader Reader) ([]Entry, error) {
	data, err := reader.ReadFile(ctx, GCSPath)
	if err == nil && len(data) > 0 {
		if entries, e := parseEntries(data); e == nil && len(entries) > 0 {
			return entries, nil
		}
	}
	return buildAndPersist(ctx, reader)
}

func buildAndPersist(ctx context.Context, reader Reader) ([]Entry, error) {
	pages, err := reader.ListConcepts(ctx, false)
	if err != nil { return nil, err }
	entries := make([]Entry, 0, len(pages))
	for _, p := range pages {
		_, data, err := reader.GetPage(ctx, p.Slug, "concepts")
		if err != nil { continue }
		entries = append(entries, parsePage(p.Slug, p.Title, string(data)))
	}
	if len(entries) > 0 {
		var buf strings.Builder
		for _, e := range entries {
			b, _ := json.Marshal(e)
			buf.WriteString(string(b) + "\n")
		}
		reader.WriteBytes(ctx, []byte(buf.String()), GCSPath)
	}
	return entries, nil
}

func parsePage(slug, fallbackTitle, raw string) Entry {
	fm, body := splitFM(raw)
	title := fallbackTitle
	if t, ok := fm["title"].(string); ok && strings.TrimSpace(t) != "" { title = strings.TrimSpace(t) }
	src := []string{}
	if s, ok := fm["sources"].(string); ok && strings.TrimSpace(s) != "" { src = []string{strings.TrimSpace(s)} }
	return Entry{Slug: slug, Title: title, Body: body, Frontmatter: fm, Sources: src}
}

func splitFM(raw string) (map[string]interface{}, string) {
	fm := make(map[string]interface{})
	if !strings.HasPrefix(raw, "---\n") { return fm, raw }
	end := strings.Index(raw[4:], "\n---")
	if end < 0 { return fm, raw }
	for _, line := range strings.Split(raw[4:4+end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" { continue }
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 { continue }
		fm[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), "\"'")
	}
	return fm, raw[4+end+4:]
}

func parseEntries(data []byte) ([]Entry, error) {
	var entries []Entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" { continue }
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil { continue }
		entries = append(entries, e)
	}
	return entries, nil
}

func ranked(entries []Entry, words []string, limit int) []search.Result {
	type cand struct { r search.Result; w int }
	cs := make([]cand, 0, len(entries))
	for _, e := range entries {
		s := score(e, words)
		if s == 0 { continue }
		cs = append(cs, cand{r: search.Result{Slug: e.Slug, Title: e.Title, Type: "concept", Snippet: snip(e, words)}, w: s})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].w > cs[j].w })
	if len(cs) <= limit {
		out := make([]search.Result, len(cs))
		for i, c := range cs { out[i] = c.r }
		return out
	}
	out := make([]search.Result, limit)
	for i := 0; i < limit; i++ { out[i] = cs[i].r }
	return out
}

func score(e Entry, words []string) int {
	t, b := strings.ToLower(e.Title), strings.ToLower(e.Body)
	s := 0
	for _, w := range words {
		if strings.Contains(t, w) { s += 5 }
		if strings.Contains(b, w) { s += 2 }
	}
	return s
}

func snip(e Entry, words []string) string {
	text := e.Body
	if text == "" { text = e.Title }
	lower := strings.ToLower(text)
	start := 0
	for _, w := range words {
		if i := strings.Index(lower, w); i >= 0 { start = max(0, i-80); break }
	}
	return strings.TrimSpace(text[start:min(len(text), start+200)])
}
