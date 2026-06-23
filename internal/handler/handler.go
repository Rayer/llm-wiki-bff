package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// Handler holds all dependencies for API handlers.
type Handler struct {
	gcs       *gcs.Client
	firestore *firestore.Client
	index     *search.Index
	llm       *llm.Client
	expander  *llm.QueryExpander
}

// New creates a Handler with the given dependencies.
func New(gcs *gcs.Client, fs *firestore.Client, idx *search.Index, llmClient *llm.Client, expander *llm.QueryExpander) *Handler {
	return &Handler{gcs: gcs, firestore: fs, index: idx, llm: llmClient, expander: expander}
}

// ═══════════════ QUERY ═══════════════

// Query handles POST /api/query with JSON body {"q": "...", "mode": "wiki|full"}
func (h *Handler) Query(c *gin.Context) {
	var req QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	q := strings.TrimSpace(req.Query)
	mode := req.Mode
	if mode == "" {
		mode = "wiki"
	}
	if q == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "q field is required"})
		return
	}

	// Query expansion: LLM rewrites natural language into structured search keywords.
	searchQuery := q
	var expandResult *llm.ExpandResult
	if h.expander != nil {
		if result, err := h.expander.Expand(q); err != nil {
			log.Printf("[expander] query expansion failed for %q: %v — falling back to raw query", q, err)
		} else if result != nil {
			expandResult = result
			searchQuery = strings.Join(result.Keywords, " ")
		}
	}

	results := h.index.Search(searchQuery, 10)
	log.Printf("Search query: %s, results: %v\n", searchQuery, results)

	resp := QueryResponse{
		Query:   q,
		Mode:    mode,
		Results: results,
		Expand:  expandResult,
	}

	if h.llm != nil && len(results) > 0 {
		// Fetch full text for top 10 results
		topN := 10
		if len(results) < topN {
			topN = len(results)
		}
		var contexts []string
		for _, r := range results[:topN] {
			category := r.Type + "s"
			_, data, err := h.gcs.GetPage(context.Background(), r.Slug, category)
			if err != nil {
				continue
			}
			contexts = append(contexts, fmt.Sprintf("[%s] %s\n\n%s", r.Title, r.Slug, string(data)))
		}

		if len(contexts) > 0 {
			systemPrompt := buildSystemPrompt(mode)
			userPrompt := buildUserPrompt(q, contexts)
			if answer, err := h.llm.Chat(systemPrompt, userPrompt); err == nil {
				// Post-process: ensure citation names are bracketed [like this]
				answer = ensureBrackets(answer, results)
				resp.AISynth = answer
				citations, filtered := search.ParseCitations(answer, results)
				resp.Citations = citations
				resp.Results = filtered
			} else {
				log.Printf("LLM synthesis failed: %v", err)
			}
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ═══════════════ SOURCES ═══════════════

// ListSources handles GET /api/sources
func (h *Handler) ListSources(c *gin.Context) {
	ctx := context.Background()
	sources, err := h.gcs.ListSources(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if sources == nil {
		sources = []gcs.WikiPage{}
	}
	c.JSON(http.StatusOK, SourcesListResponse{Sources: sources, Count: len(sources)})
}

// GetSource handles GET /api/sources/:slug
func (h *Handler) GetSource(c *gin.Context) {
	slug := c.Param("slug")
	// Decode percent-encoded characters (! → %21 etc.) that Go's HTTP server
	// doesn't decode because they're valid path chars.
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	ctx := context.Background()
	_, data, err := h.gcs.GetPage(ctx, slug, "sources")
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "source not found: " + slug})
		return
	}

	fm, body := parseFrontmatter(string(data))

	c.JSON(http.StatusOK, SourceDetailResponse{
		Slug:        slug,
		Title:       slug,
		Type:        "source",
		Frontmatter: fm,
		Body:        body,
		Raw:         string(data),
	})
}

// ═══════════════ CONCEPTS ═══════════════

// ListConcepts handles GET /api/concepts
func (h *Handler) ListConcepts(c *gin.Context) {
	ctx := context.Background()
	includeDrafts, err := strconv.ParseBool(c.DefaultQuery("include_drafts", "false"))
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "include_drafts must be a boolean"})
		return
	}

	concepts, err := h.gcs.ListConcepts(ctx, includeDrafts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if concepts == nil {
		concepts = []gcs.WikiPage{}
	}
	c.JSON(http.StatusOK, ConceptsListResponse{Concepts: concepts, Count: len(concepts)})
}

// GetConcept handles GET /api/concepts/:slug
func (h *Handler) GetConcept(c *gin.Context) {
	slug := c.Param("slug")
	// Decode percent-encoded characters that Go's HTTP server doesn't decode.
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	ctx := context.Background()
	page, data, err := h.gcs.GetPage(ctx, slug, "concepts")
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: "concept not found: " + slug})
		return
	}

	fm, body := parseFrontmatter(string(data))

	c.JSON(http.StatusOK, ConceptDetailResponse{
		Slug:        slug,
		Title:       slug,
		Type:        "concept",
		Status:      page.Status,
		Frontmatter: fm,
		Body:        body,
		Raw:         string(data),
	})
}

// ═══════════════ IMPORT ═══════════════

// Import handles POST /api/import (placeholder)
func (h *Handler) Import(c *gin.Context) {
	var req ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "urls array is required"})
		return
	}

	c.JSON(http.StatusOK, ImportResponse{
		Message:  "Bookmark import — Phase 2 (not yet implemented)",
		Received: len(req.URLs),
		URLs:     req.URLs,
	})
}

// ═══════════════ STATUS ═══════════════

// Status handles GET /api/status
func (h *Handler) Status(c *gin.Context) {
	ctx := context.Background()

	sources, _ := h.gcs.ListSources(ctx)
	concepts, _ := h.gcs.ListConcepts(ctx, true)

	resp := StatusResponse{
		SourcesCount:  len(sources),
		ConceptsCount: len(concepts),
		IndexSources:  h.index.SourceCount(),
		IndexConcepts: h.index.ConceptCount(),
	}

	if h.firestore != nil {
		lock, err := h.firestore.GetStatus(ctx)
		if err == nil {
			resp.Locked = lock.Locked
			resp.LockWorker = lock.Worker
			resp.LockExpiry = lock.LockExpiry.Format(time.RFC3339)
		}
		running, err := h.firestore.CountActiveLocks(ctx)
		if err == nil {
			resp.RunningPipelines = running
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ═══════════════ METRICS ═══════════════

// Metrics handles GET /api/metrics — pipeline metrics for Grafana dashboard.
func (h *Handler) Metrics(c *gin.Context) {
	ctx := context.Background()
	resp := MetricsResponse{
		RecentExecutions: []ExecutionSummary{},
	}

	if h.firestore != nil {
		running, err := h.firestore.CountActiveLocks(ctx)
		if err == nil {
			resp.RunningPipelines = running
		}

		execs, err := h.firestore.ListRecentExecutions(ctx, 100)
		if err == nil {
			for _, e := range execs {
				summary := ExecutionSummary{
					StartedAt:   e.StartedAt.Format(time.RFC3339),
					DurationSec: e.DurationSec,
					Status:      e.Status,
				}
				if !e.FinishedAt.IsZero() {
					summary.FinishedAt = e.FinishedAt.Format(time.RFC3339)
				}
				resp.RecentExecutions = append(resp.RecentExecutions, summary)
			}
		}
	}

	// GCP metrics — GCS bucket stats (cached, not real-time for performance)
	if h.gcs != nil {
		bytes, files, err := h.gcs.BucketStats(ctx)
		if err == nil {
			resp.GCP = &GCPMetrics{
				GCSTotalBytes: bytes,
				GCSTotalFiles: files,
			}
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ═══════════════ Helpers ═══════════════

// buildSystemPrompt returns the system prompt for the given mode.
func buildSystemPrompt(mode string) string {
	base := "CRITICAL: If the user asks about a specific location (city, district, area), ONLY include results relevant to that location. Ignore results from other locations even if they match on topic keywords." +
		"\n\nCITATION FORMAT RULES (mandatory):" +
		"\n- EVERY factual claim from wiki content MUST have a bracketed citation: [Exact Source Name]" +
		"\n- Use the EXACT full title from the wiki content inside brackets" +
		"\n- Never use **bold** instead of brackets" +
		"\n- Never append source names as plain text without brackets" +
		"\n- Correct example: 「...適合親子放電。[中和員山公園遊逸之丘]」" +
		"\n- Wrong example: 「...適合親子放電。中和員山公園遊逸之丘」" +
		"\n- Each paragraph referencing a source MUST end with its bracketed citation. "
	if mode == "full" {
		return "You are a knowledgeable assistant with access to a personal wiki. Treat the wiki as supplementary reference material — NOT as a constraint." +
			"\n- If the wiki content is RELEVANT to the user's question (same location, topic, or category), use it and cite with [Source Name]." +
			"\n- If the wiki content is NOT relevant (wrong city, different topic, etc.), IGNORE it completely and answer from your own knowledge — exactly as if you were asked this question directly with no wiki." +
			"\n- NEVER say 'I cannot find this in the wiki' or apologize for missing information. Just answer the question." +
			"\n- When mixing wiki and general knowledge, make it seamless — don't call out which is which in the text." +
			"\n\nCITATION FORMAT RULES (mandatory):" +
			"\n- EVERY factual claim from wiki content MUST have a bracketed citation: [Exact Source Name]" +
			"\n- Use the EXACT full title from the wiki content inside brackets" +
			"\n- Never use **bold** instead of brackets" +
			"\n- Correct example: 「...適合親子放電。[中和員山公園遊逸之丘]」" +
			"\n- Wrong example: 「...適合親子放電。中和員山公園遊逸之丘」"
	}
	return base + "You are a wiki Q&A assistant. Answer ONLY using the wiki content provided below. Do not use external knowledge. Cite every claim using [Source Name]."
}

// buildUserPrompt builds the user message with wiki context.
func buildUserPrompt(query string, contexts []string) string {
	var sb strings.Builder
	sb.WriteString("User question: ")
	sb.WriteString(query)
	sb.WriteString("\n\nWiki content:\n")
	for _, ctx := range contexts {
		sb.WriteString("\n---\n")
		sb.WriteString(ctx)
	}
	return sb.String()
}

// ensureBrackets post-processes LLM output to wrap known citation names
// in brackets [like this] when the LLM outputs them as plain text.
func ensureBrackets(text string, results []search.Result) string {
	// Collect unique citation names, longest first to avoid partial matches
	names := make(map[string]bool)
	var sorted []string
	for _, r := range results {
		if !names[r.Title] {
			names[r.Title] = true
			sorted = append(sorted, r.Title)
		}
	}
	// Sort by length descending so longer names are checked first
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if len(sorted[j]) > len(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	for _, name := range sorted {
		if len(name) < 3 {
			continue
		}
		bracketed := "[" + name + "]"
		// Skip if already properly bracketed
		if strings.Contains(text, bracketed) {
			continue
		}
		// Replace plain occurrences — only when not already inside brackets
		// Simple approach: replace occurrences that are not preceded by '[' and not followed by ']'
		idx := 0
		for {
			pos := strings.Index(text[idx:], name)
			if pos < 0 {
				break
			}
			absPos := idx + pos
			// Check it's not already inside brackets
			before := ""
			if absPos > 0 {
				before = text[absPos-1 : absPos]
			}
			after := ""
			if absPos+len(name) < len(text) {
				after = text[absPos+len(name) : absPos+len(name)+1]
			}
			if before == "[" && after == "]" {
				idx = absPos + len(name)
				continue
			}
			// Replace this occurrence
			text = text[:absPos] + bracketed + text[absPos+len(name):]
			idx = absPos + len(bracketed)
		}
	}
	return text
}

// parseFrontmatter extracts YAML frontmatter (between --- markers) from markdown.
// Returns frontmatter map and body string.
func parseFrontmatter(md string) (map[string]interface{}, string) {
	fm := make(map[string]interface{})
	if !strings.HasPrefix(md, "---") {
		return fm, md
	}

	// Find closing ---
	end := strings.Index(md[3:], "\n---")
	if end < 0 {
		return fm, md
	}
	end += 3

	fmRaw := md[3:end]
	lines := strings.Split(fmRaw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Strip quotes
			val = strings.Trim(val, "\"'")
			fm[key] = val
		}
	}

	body := md[end+3:]
	return fm, body
}
