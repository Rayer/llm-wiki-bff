package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayert/llm-wiki-bff/internal/firestore"
	"github.com/rayert/llm-wiki-bff/internal/gcs"
	"github.com/rayert/llm-wiki-bff/internal/search"
)

// Handler holds all dependencies for API handlers.
type Handler struct {
	gcs       *gcs.Client
	firestore *firestore.Client
	index     *search.Index
}

// New creates a Handler with the given dependencies.
func New(gcs *gcs.Client, fs *firestore.Client, idx *search.Index) *Handler {
	return &Handler{gcs: gcs, firestore: fs, index: idx}
}

// ═══════════════ Shared response types ═══════════════

// ErrorResponse is returned on errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ═══════════════ QUERY ═══════════════

// QueryResponse is the response for GET /api/query.
type QueryResponse struct {
	Query   string          `json:"query"`
	Mode    string          `json:"mode"`
	Results []search.Result `json:"results"`
	AISynth string          `json:"ai_synth,omitempty"` // placeholder for LLM synthesis
}

// Query handles GET /api/query?q=...&mode=wiki|full
//
//	@Summary		Search wiki content
//	@Description	Full-text search across sources and concepts. Mode "wiki" returns raw results, "full" adds AI-synthesized answer.
//	@Tags			search
//	@Produce		json
//	@Param			q		query		string	true	"Search query"
//	@Param			mode	query		string	false	"Search mode: wiki or full"	default(wiki)
//	@Success		200		{object}	QueryResponse
//	@Failure		400		{object}	ErrorResponse
//	@Router			/api/query [get]
func (h *Handler) Query(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	mode := c.DefaultQuery("mode", "wiki")
	if q == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "q parameter is required"})
		return
	}

	results := h.index.Search(q, 10)

	resp := QueryResponse{
		Query:   q,
		Mode:    mode,
		Results: results,
	}

	if mode == "full" && len(results) > 0 {
		resp.AISynth = "AI-synthesized answer based on wiki content (placeholder — LLM integration TBD)"
	}

	c.JSON(http.StatusOK, resp)
}

// ═══════════════ SOURCES ═══════════════

// SourcesListResponse is the response for GET /api/sources.
type SourcesListResponse struct {
	Sources []gcs.WikiPage `json:"sources"`
	Count   int            `json:"count"`
}

// ListSources handles GET /api/sources
//
//	@Summary		List wiki sources
//	@Description	Returns all compiled wiki sources.
//	@Tags			sources
//	@Produce		json
//	@Success		200	{object}	SourcesListResponse
//	@Failure		500	{object}	ErrorResponse
//	@Router			/api/sources [get]
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

// SourceDetailResponse is the response for GET /api/sources/:slug.
type SourceDetailResponse struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Type        string                 `json:"type"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Body        string                 `json:"body"`
	Raw         string                 `json:"raw"`
}

// GetSource handles GET /api/sources/:slug
//
//	@Summary		Get a source by slug
//	@Description	Returns full content (frontmatter + body) for a wiki source.
//	@Tags			sources
//	@Produce		json
//	@Param			slug	path		string	true	"Source slug"
//	@Success		200		{object}	SourceDetailResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/api/sources/{slug} [get]
func (h *Handler) GetSource(c *gin.Context) {
	slug := c.Param("slug")
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

// ConceptsListResponse is the response for GET /api/concepts.
type ConceptsListResponse struct {
	Concepts []gcs.WikiPage `json:"concepts"`
	Count    int            `json:"count"`
}

// ListConcepts handles GET /api/concepts
//
//	@Summary		List wiki concepts
//	@Description	Returns published wiki concepts by default. Set include_drafts=true to include draft concepts.
//	@Tags			concepts
//	@Produce		json
//	@Param			include_drafts	query	bool	false	"Include draft concepts"	default(false)
//	@Success		200	{object}	ConceptsListResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Router			/api/concepts [get]
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

// ConceptDetailResponse is the response for GET /api/concepts/:slug.
type ConceptDetailResponse struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Type        string                 `json:"type"`
	Status      string                 `json:"status"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Body        string                 `json:"body"`
	Raw         string                 `json:"raw"`
}

// GetConcept handles GET /api/concepts/:slug
//
//	@Summary		Get a concept by slug
//	@Description	Returns full content (frontmatter + body) for a wiki concept.
//	@Tags			concepts
//	@Produce		json
//	@Param			slug	path		string	true	"Concept slug"
//	@Success		200		{object}	ConceptDetailResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/api/concepts/{slug} [get]
func (h *Handler) GetConcept(c *gin.Context) {
	slug := c.Param("slug")
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

// ImportRequest is the body for POST /api/import.
type ImportRequest struct {
	URLs []string `json:"urls" binding:"required"`
}

// ImportResponse is the response for POST /api/import.
type ImportResponse struct {
	Message  string   `json:"message"`
	Received int      `json:"received"`
	URLs     []string `json:"urls"`
}

// Import handles POST /api/import (placeholder)
//
//	@Summary		Import bookmarks
//	@Description	Accepts a list of URLs to import (Phase 2 — placeholder).
//	@Tags			import
//	@Accept			json
//	@Produce		json
//	@Param			body	body		ImportRequest	true	"URLs to import"
//	@Success		200		{object}	ImportResponse
//	@Failure		400		{object}	ErrorResponse
//	@Router			/api/import [post]
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

// StatusResponse is the response for GET /api/status.
type StatusResponse struct {
	SourcesCount  int    `json:"sources_count"`
	ConceptsCount int    `json:"concepts_count"`
	IndexSources  int    `json:"index_sources"`
	IndexConcepts int    `json:"index_concepts"`
	Locked        bool   `json:"locked,omitempty"`
	LockWorker    string `json:"lock_worker,omitempty"`
	LockExpiry    string `json:"lock_expiry,omitempty"`
}

// Status handles GET /api/status
//
//	@Summary		Pipeline status
//	@Description	Returns counts and lock status from GCS, search index, and Firestore.
//	@Tags			status
//	@Produce		json
//	@Success		200	{object}	StatusResponse
//	@Router			/api/status [get]
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
	}

	c.JSON(http.StatusOK, resp)
}

// ═══════════════ Helpers ═══════════════

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
