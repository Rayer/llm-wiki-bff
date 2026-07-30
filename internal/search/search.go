package search

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

// Index provides in-memory search over wiki metadata.
type Index struct {
	sources       []store.WikiPage
	concepts      []store.WikiPage
	entries       map[string]indexedPage
	conceptBodies map[string]string // slug → body text (for full-content grep)
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

// Build loads the generated metadata index from wiki storage.
func (idx *Index) Build(reader store.Store) error {
	ctx := context.Background()

	raw, err := reader.ReadFile(ctx, "meta/index.md")
	if err != nil {
		return err
	}

	idx.sources, idx.concepts, idx.entries = parseMetaIndex(string(raw))
	return nil
}

// LoadConceptBodies fetches all concept bodies from wiki storage for full-content grep.
// Individual fetch failures are logged and skipped — only returns error if ALL fail.
func (idx *Index) LoadConceptBodies(ctx context.Context, reader store.Store) error {
	if idx.conceptBodies == nil {
		idx.conceptBodies = make(map[string]string)
	}

	type result struct {
		slug string
		body string
	}

	sem := make(chan struct{}, 20) // max concurrent GCS reads
	results := make(chan result, len(idx.concepts))

	for _, c := range idx.concepts {
		sem <- struct{}{}
		go func(slug string) {
			defer func() { <-sem }()
			_, data, err := reader.GetPage(ctx, slug, "concepts")
			if err != nil {
				return
			}
			body := string(data)
			body = stripFrontmatter(body)
			results <- result{slug: slug, body: body}
		}(c.Slug)
	}

	// Collect results
	for range idx.concepts {
		select {
		case r := <-results:
			idx.conceptBodies[r.slug] = r.body
		default:
			// no more results (all goroutines finished without producing)
			break
		}
	}

	if len(idx.conceptBodies) == 0 {
		return fmt.Errorf("failed to load any concept bodies")
	}
	return nil
}

// stripFrontmatter removes YAML frontmatter from markdown.
func stripFrontmatter(md string) string {
	if strings.HasPrefix(md, "---") {
		if idx := strings.Index(md[3:], "\n---"); idx >= 0 {
			return md[idx+5:]
		}
	}
	return md
}

// Search performs keyword search across indexed wiki metadata.
func (idx *Index) Search(query string, limit int) []Result {
	if limit <= 0 {
		limit = 10
	}
	query = strings.ToLower(query)
	words := strings.Fields(query)
	// Chinese / no-space query: add character bigrams for partial matching
	if len(words) <= 1 && len([]rune(query)) > 2 {
		runes := []rune(query)
		for i := 0; i < len(runes)-1; i++ {
			words = append(words, string(runes[i:i+2]))
		}
	}

	var sourceResults []scoredResult
	var conceptResults []scoredResult

	// Search sources
	for _, s := range idx.sources {
		entry := idx.entries[indexKey("source", s.Slug)]
		text := idx.searchableText(s.Slug, entry)
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
		text := idx.searchableText(c.Slug, entry)
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

func parseMetaIndex(raw string) ([]store.WikiPage, []store.WikiPage, map[string]indexedPage) {
	var sources []store.WikiPage
	var concepts []store.WikiPage
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
		page := store.WikiPage{Slug: slug, Title: title}
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

func (idx *Index) searchableText(slug string, entry indexedPage) string {
	text := slug + " " + entry.title + " " + entry.description
	if idx.conceptBodies != nil {
		if body, ok := idx.conceptBodies[slug]; ok {
			text += " " + body
		}
	}
	return strings.ToLower(text)
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

// CitationReference returns the deterministic reference used in model context.
func CitationReference(rank int) string {
	return "[CITATION_REF_" + strconv.Itoa(rank) + "]"
}

// BuildCitationContext formats a ranked wiki block. Untrusted fields are
// neutralized so they cannot counterfeit a server-issued citation reference.
func BuildCitationContext(rank int, title, slug, body string) string {
	return fmt.Sprintf("[%s] %s %s\n\n%s", neutralizeCitationReferences(title), CitationReference(rank), neutralizeCitationReferences(slug), neutralizeCitationReferences(body))
}

func neutralizeCitationReferences(text string) string {
	return strings.ReplaceAll(text, "CITATION_REF_", "CITATION-REF_")
}

func citationRank(text string, resultCount int) (int, bool) {
	const prefix = "CITATION_REF_"
	if resultCount <= 0 || !strings.HasPrefix(text, prefix) {
		return 0, false
	}
	suffix := text[len(prefix):]
	if suffix == "" || (len(suffix) > 1 && suffix[0] == '0') {
		return 0, false
	}
	rank := 0
	for _, digit := range suffix {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		if rank > (resultCount-1-int(digit-'0'))/10 {
			return 0, false
		}
		rank = rank*10 + int(digit-'0')
		if rank >= resultCount {
			return 0, false
		}
	}
	return rank, CitationReference(rank) == "["+text+"]"
}

func safeCitationResult(result Result) bool {
	if result.Type != "source" && result.Type != "concept" {
		return false
	}
	if result.Slug == "" || strings.TrimSpace(result.Slug) != result.Slug || result.Slug == "." || result.Slug == ".." {
		return false
	}
	if strings.ContainsAny(result.Slug, "/\\%?#") || strings.HasPrefix(result.Slug, "//") {
		return false
	}
	for _, r := range result.Slug {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	parsed, err := url.Parse(result.Slug)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return false
	}
	escaped := url.PathEscape(result.Slug)
	unescaped, err := url.PathUnescape(escaped)
	return escaped != "" && escaped != "." && escaped != ".." && err == nil && unescaped == result.Slug && !strings.Contains(escaped, "/")
}

func stripReservedCitationFragments(text string) string {
	const prefix = "CITATION_REF_"
	for {
		idx := strings.Index(text, prefix)
		if idx < 0 {
			return text
		}
		start := idx
		if start > 0 && text[start-1] == '[' {
			start--
		}
		end := strings.IndexByte(text[idx:], ']')
		if end >= 0 {
			end += idx + 1
		} else {
			end = len(text)
		}
		text = text[:start] + text[end:]
	}
}

// ResolveCitations validates citations against the bounded ranked result set,
// normalizes valid references to canonical titles, and preserves ranked results
// when no citation validates.
func ResolveCitations(aiSynth string, results []Result) (string, []Citation, []Result) {
	if aiSynth == "" {
		return aiSynth, nil, results
	}

	// Exact display titles remain compatible, but duplicate titles are ambiguous
	// and therefore cannot identify a result.
	byTitle := make(map[string][]Result)
	for _, r := range results {
		key := strings.ToLower(r.Title)
		byTitle[key] = append(byTitle[key], r)
	}

	var citations []Citation
	cited := make(map[string]bool)
	var normalized strings.Builder
	remaining := aiSynth

	for {
		start := strings.Index(remaining, "[")
		if start < 0 {
			normalized.WriteString(stripReservedCitationFragments(remaining))
			break
		}
		end := strings.Index(remaining[start:], "]")
		if end < 0 {
			normalized.WriteString(stripReservedCitationFragments(remaining))
			break
		}
		normalized.WriteString(remaining[:start])
		text := remaining[start+1 : start+end]
		remaining = remaining[start+end+1:]

		// Skip URLs and other bracket content
		if strings.Contains(text, "http") || strings.Contains(text, "wiki") || strings.Contains(text, "general") {
			normalized.WriteString(stripReservedCitationFragments("[" + text + "]"))
			continue
		}

		var matched *Result
		if strings.HasPrefix(text, "CITATION_REF_") {
			if rank, ok := citationRank(text, len(results)); ok && safeCitationResult(results[rank]) {
				matched = &results[rank]
			}
		} else if matches := byTitle[strings.ToLower(text)]; len(matches) == 1 {
			if safeCitationResult(matches[0]) {
				matched = &matches[0]
			}
		}

		if matched != nil {
			r := *matched
			collection := "concepts"
			if r.Type == "source" {
				collection = "sources"
			}
			path := "/" + collection + "/" + url.PathEscape(r.Slug)
			citations = append(citations, Citation{Text: r.Title, Slug: r.Slug, Type: r.Type, Path: path})
			normalized.WriteString("[")
			normalized.WriteString(r.Title)
			normalized.WriteString("]")
			cited[r.Type+"\x00"+r.Slug] = true
		} else {
			if !strings.Contains(text, "CITATION_REF_") {
				normalized.WriteString("[")
				normalized.WriteString(text)
				normalized.WriteString("]")
			}
		}
	}

	if len(citations) == 0 {
		return normalized.String(), nil, results
	}

	var filtered []Result
	for _, r := range results {
		if cited[r.Type+"\x00"+r.Slug] {
			filtered = append(filtered, r)
		}
	}

	return normalized.String(), citations, filtered
}

// ParseCitations extracts [Name] citations from ai_synth and matches them to results.
func ParseCitations(aiSynth string, results []Result) ([]Citation, []Result) {
	_, citations, filtered := ResolveCitations(aiSynth, results)
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
