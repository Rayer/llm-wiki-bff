package search

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"github.com/rayert/llm-wiki-bff/internal/gcs"
)

// Index provides in-memory search over wiki metadata.
type Index struct {
	sources  []gcs.WikiPage
	concepts []gcs.WikiPage
	entries  map[string]indexedPage
}

type indexedPage struct {
	title       string
	description string
}

// Result is a single search hit.
type Result struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Type    string `json:"type"` // "source" or "concept"
	Snippet string `json:"snippet"`
}

// Citation links a mention in ai_synth back to a wiki page.
type Citation struct {
	Text string `json:"text"`
	Slug string `json:"slug"`
	Type string `json:"type"` // "source" or "concept"
	Path string `json:"path"` // pre-encoded URL path: /concepts/xxx or /sources/xxx
}

type scoredResult struct {
	result Result
	score  int
}

// NewIndex creates an empty search index.
func NewIndex() *Index {
	return &Index{entries: make(map[string]indexedPage)}
}

// SourceCount returns the number of indexed sources.
func (idx *Index) SourceCount() int { return len(idx.sources) }

// ConceptCount returns the number of indexed concepts.
func (idx *Index) ConceptCount() int { return len(idx.concepts) }

// Build loads the generated metadata index from GCS.
func (idx *Index) Build(gcsClient *gcs.Client) error {
	ctx := context.Background()

	raw, err := gcsClient.ReadMetaIndex(ctx)
	if err != nil {
		return err
	}

	idx.sources, idx.concepts, idx.entries = parseMetaIndex(raw)
	return nil
}

// Search performs keyword search across indexed wiki metadata.
func (idx *Index) Search(query string, limit int) []Result {
	if limit <= 0 {
		limit = 10
	}
	query = strings.ToLower(query)
	words := strings.Fields(query)

	var sourceResults []scoredResult
	var conceptResults []scoredResult

	// Search sources
	for _, s := range idx.sources {
		entry := idx.entries[indexKey("source", s.Slug)]
		text := searchableText(s.Slug, entry)
		score := matchScore(text, words)
		if score > 0 {
			sourceResults = append(sourceResults, scoredResult{
				score: score,
				result: Result{
					Slug:    s.Slug,
					Title:   entryTitle(s.Slug, entry),
					Type:    "source",
					Snippet: makeSnippet(displayText(s.Slug, entry), words, 200),
				},
			})
		}
	}

	// Search concepts
	for _, c := range idx.concepts {
		entry := idx.entries[indexKey("concept", c.Slug)]
		text := searchableText(c.Slug, entry)
		score := matchScore(text, words)
		if score > 0 {
			conceptResults = append(conceptResults, scoredResult{
				score: score,
				result: Result{
					Slug:    c.Slug,
					Title:   entryTitle(c.Slug, entry),
					Type:    "concept",
					Snippet: makeSnippet(displayText(c.Slug, entry), words, 200),
				},
			})
		}
	}

	sortScoredResults(sourceResults)
	sortScoredResults(conceptResults)
	sourceResults = limitScoredResults(sourceResults, limit)
	conceptResults = limitScoredResults(conceptResults, limit)

	results := interleaveResults(sourceResults, conceptResults, limit)
	return results
}

func sortScoredResults(results []scoredResult) {
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})
}

func limitScoredResults(results []scoredResult, limit int) []scoredResult {
	if len(results) > limit {
		return results[:limit]
	}
	return results
}

func interleaveResults(sources, concepts []scoredResult, limit int) []Result {
	results := make([]Result, 0, limit)
	for i := 0; len(results) < limit && (i < len(sources) || i < len(concepts)); i++ {
		if i < len(sources) {
			results = append(results, sources[i].result)
			if len(results) == limit {
				break
			}
		}
		if i < len(concepts) {
			results = append(results, concepts[i].result)
		}
	}
	return results
}

func parseMetaIndex(raw string) ([]gcs.WikiPage, []gcs.WikiPage, map[string]indexedPage) {
	var sources []gcs.WikiPage
	var concepts []gcs.WikiPage
	entries := make(map[string]indexedPage)
	section := ""
	inFrontmatter := false

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "---" {
			inFrontmatter = !inFrontmatter
			continue
		}
		if inFrontmatter || strings.HasPrefix(line, "_") {
			continue
		}

		if strings.HasPrefix(line, "#") {
			header := strings.TrimSpace(strings.TrimLeft(line, "#"))
			switch strings.ToLower(header) {
			case "sources", "source":
				section = "source"
			case "concepts", "concept":
				section = "concept"
			}
			continue
		}

		pageType, slug, title, description, ok := parseIndexLine(line, section)
		if !ok {
			continue
		}
		page := gcs.WikiPage{Slug: slug, Title: title}
		key := indexKey(pageType, slug)
		entries[key] = indexedPage{title: title, description: description}
		if pageType == "source" {
			page.Path = "wiki/sources/" + slug + ".md"
			sources = append(sources, page)
		} else {
			page.Path = "wiki/" + slug + ".md"
			page.Status = "published"
			concepts = append(concepts, page)
		}
	}

	return sources, concepts, entries
}

func parseIndexLine(line, section string) (string, string, string, string, bool) {
	line = strings.TrimSpace(strings.TrimLeft(line, "-*"))
	pageType := section

	lower := strings.ToLower(line)
	for _, prefix := range []string{"source:", "sources:", "concept:", "concepts:"} {
		if strings.HasPrefix(lower, prefix) {
			if strings.HasPrefix(prefix, "source") {
				pageType = "source"
			} else {
				pageType = "concept"
			}
			line = strings.TrimSpace(line[len(prefix):])
			break
		}
	}

	slug, title, rest, ok := parseLinkedLine(line)
	if !ok {
		slug, title, rest, ok = parsePlainLine(line)
	}
	if !ok {
		return "", "", "", "", false
	}

	if strings.HasPrefix(slug, "wiki/sources/") {
		pageType = "source"
		slug = strings.TrimSuffix(strings.TrimPrefix(slug, "wiki/sources/"), ".md")
	} else if strings.HasPrefix(slug, "sources/") {
		pageType = "source"
		slug = strings.TrimSuffix(strings.TrimPrefix(slug, "sources/"), ".md")
	} else if strings.HasPrefix(slug, "wiki/") {
		pageType = "concept"
		slug = strings.TrimSuffix(strings.TrimPrefix(slug, "wiki/"), ".md")
	}

	if pageType == "" {
		return "", "", "", "", false
	}

	slug = strings.TrimSuffix(strings.TrimSpace(slug), ".md")
	title = strings.TrimSpace(title)
	if title == "" {
		title = slug
	}

	return pageType, slug, title, cleanDescription(rest), true
}

func parseLinkedLine(line string) (string, string, string, bool) {
	if start := strings.Index(line, "[["); start >= 0 {
		if end := strings.Index(line[start+2:], "]]"); end >= 0 {
			target := line[start+2 : start+2+end]
			rest := line[start+2+end+2:]
			parts := strings.SplitN(target, "|", 2)
			slug := strings.TrimSpace(parts[0])
			title := slug
			if len(parts) == 2 {
				title = strings.TrimSpace(parts[1])
			}
			return slug, title, rest, slug != ""
		}
	}

	if start := strings.Index(line, "["); start >= 0 {
		mid := strings.Index(line[start:], "](")
		if mid >= 0 {
			mid += start
			end := strings.Index(line[mid+2:], ")")
			if end >= 0 {
				title := strings.TrimSpace(line[start+1 : mid])
				slug := strings.TrimSpace(line[mid+2 : mid+2+end])
				rest := line[mid+2+end+1:]
				return slug, title, rest, slug != ""
			}
		}
	}

	return "", "", "", false
}

func parsePlainLine(line string) (string, string, string, bool) {
	fields := splitMetadataFields(line)
	if len(fields) == 0 {
		return "", "", "", false
	}
	slug := fields[0]
	title := slug
	description := ""
	if len(fields) > 1 {
		title = fields[1]
	}
	if len(fields) > 2 {
		description = strings.Join(fields[2:], " - ")
	}
	return slug, title, description, true
}

func splitMetadataFields(line string) []string {
	normalized := strings.ReplaceAll(line, "\t", " | ")
	normalized = strings.ReplaceAll(normalized, " \u2014 ", " | ")
	normalized = strings.ReplaceAll(normalized, " -- ", " | ")
	normalized = strings.ReplaceAll(normalized, " - ", " | ")
	normalized = strings.ReplaceAll(normalized, " | ", "|")
	if !strings.Contains(normalized, "|") && strings.Count(normalized, ":") >= 2 {
		normalized = strings.ReplaceAll(normalized, ":", "|")
	}
	parts := strings.Split(normalized, "|")
	var fields []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			fields = append(fields, part)
		}
	}
	return fields
}

func cleanDescription(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "-:\u2014")
	return strings.TrimSpace(s)
}

func indexKey(pageType, slug string) string {
	return pageType + "\x00" + slug
}

func searchableText(slug string, entry indexedPage) string {
	return strings.ToLower(slug + " " + entry.title + " " + entry.description)
}

func displayText(slug string, entry indexedPage) string {
	if entry.description != "" {
		return entry.description
	}
	return entryTitle(slug, entry)
}

func entryTitle(slug string, entry indexedPage) string {
	if entry.title != "" {
		return entry.title
	}
	return slug
}

// ParseCitations extracts [Name] citations from ai_synth and matches them to results.
func ParseCitations(aiSynth string, results []Result) ([]Citation, []Result) {
	if aiSynth == "" {
		return nil, results
	}

	// Build lookup by title
	byTitle := make(map[string]Result)
	for _, r := range results {
		byTitle[strings.ToLower(r.Title)] = r
	}

	var citations []Citation
	cited := make(map[string]bool)
	remaining := aiSynth

	for {
		start := strings.Index(remaining, "[")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], "]")
		if end < 0 {
			break
		}
		text := remaining[start+1 : start+end]
		remaining = remaining[start+end+1:]

		// Skip URLs and other bracket content
		if strings.Contains(text, "http") || strings.Contains(text, "wiki") || strings.Contains(text, "general") {
			continue
		}

		if r, ok := byTitle[strings.ToLower(text)]; ok {
			collection := "concepts"
			if r.Type == "source" {
				collection = "sources"
			}
			path := "/" + collection + "/" + url.PathEscape(r.Slug)
			citations = append(citations, Citation{Text: text, Slug: r.Slug, Type: r.Type, Path: path})
			cited[r.Slug] = true
		}
	}

	// Filter results to only cited
	var filtered []Result
	for _, r := range results {
		if cited[r.Slug] {
			filtered = append(filtered, r)
		}
	}

	return citations, filtered
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
