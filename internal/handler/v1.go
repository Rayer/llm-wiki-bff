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
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func getGCSClient(c *gin.Context, defaultClient *gcs.Client) (*gcs.Client, error) {
	userID := c.GetString("userID")
	projectID := c.GetString("projectID")
	if userID == "" && projectID == "" {
		return defaultClient, nil
	}
	if userID == "" || projectID == "" {
		return nil, fmt.Errorf("incomplete GCS request scope")
	}
	return defaultClient.WithScope(userID, projectID), nil
}

// V1Query handles POST /api/v1/query using the request's GCS scope.
//
//	@Summary		Search wiki content
//	@Description	Full-text search across sources and concepts. Mode "wiki" returns raw results, "full" adds AI-synthesized answer.
//	@Tags			search
//	@Accept			json
//	@Produce		json
//	@Param			request	body		QueryRequest	true	"Search query and mode"
//	@Success		200		{object}	QueryResponse
//	@Failure		400		{object}	ErrorResponse
//	@Router			/api/v1/query [post]
func (h *Handler) V1Query(c *gin.Context) {
	gcsClient, err := getGCSClient(c, h.gcs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

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

	searchQuery := q
	var expandResult *llm.ExpandResult
	if h.expander != nil {
		if result := h.expander.Expand(q); result != nil {
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
		topN := 10
		if len(results) < topN {
			topN = len(results)
		}
		var contexts []string
		for _, r := range results[:topN] {
			category := r.Type + "s"
			_, data, err := gcsClient.GetPage(context.Background(), r.Slug, category)
			if err != nil {
				continue
			}
			contexts = append(contexts, fmt.Sprintf("[%s] %s\n\n%s", r.Title, r.Slug, string(data)))
		}

		if len(contexts) > 0 {
			systemPrompt := buildSystemPrompt(mode)
			userPrompt := buildUserPrompt(q, contexts)
			if answer, err := h.llm.Chat(systemPrompt, userPrompt); err == nil {
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

// V1ListSources handles GET /api/v1/sources using the request's GCS scope.
//
//	@Summary		List wiki sources
//	@Description	Returns all compiled wiki sources.
//	@Tags			sources
//	@Produce		json
//	@Success		200	{object}	SourcesListResponse
//	@Failure		500	{object}	ErrorResponse
//	@Router			/api/v1/sources [get]
func (h *Handler) V1ListSources(c *gin.Context) {
	gcsClient, err := getGCSClient(c, h.gcs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	sources, err := gcsClient.ListSources(context.Background())
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if sources == nil {
		sources = []gcs.WikiPage{}
	}
	c.JSON(http.StatusOK, SourcesListResponse{Sources: sources, Count: len(sources)})
}

// V1GetSource handles GET /api/v1/sources/:slug using the request's GCS scope.
//
//	@Summary		Get a source by slug
//	@Description	Returns full content (frontmatter + body) for a wiki source.
//	@Tags			sources
//	@Produce		json
//	@Param			slug	path		string	true	"Source slug"
//	@Success		200		{object}	SourceDetailResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/api/v1/sources/{slug} [get]
func (h *Handler) V1GetSource(c *gin.Context) {
	gcsClient, err := getGCSClient(c, h.gcs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	slug := c.Param("slug")
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	_, data, err := gcsClient.GetPage(context.Background(), slug, "sources")
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

// V1ListConcepts handles GET /api/v1/concepts using the request's GCS scope.
//
//	@Summary		List wiki concepts
//	@Description	Returns published wiki concepts by default. Set include_drafts=true to include draft concepts.
//	@Tags			concepts
//	@Produce		json
//	@Param			include_drafts	query	bool	false	"Include draft concepts"	default(false)
//	@Success		200	{object}	ConceptsListResponse
//	@Failure		400	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Router			/api/v1/concepts [get]
func (h *Handler) V1ListConcepts(c *gin.Context) {
	gcsClient, err := getGCSClient(c, h.gcs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	includeDrafts, err := strconv.ParseBool(c.DefaultQuery("include_drafts", "false"))
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: "include_drafts must be a boolean"})
		return
	}

	concepts, err := gcsClient.ListConcepts(context.Background(), includeDrafts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if concepts == nil {
		concepts = []gcs.WikiPage{}
	}
	c.JSON(http.StatusOK, ConceptsListResponse{Concepts: concepts, Count: len(concepts)})
}

// V1GetConcept handles GET /api/v1/concepts/:slug using the request's GCS scope.
//
//	@Summary		Get a concept by slug
//	@Description	Returns full content (frontmatter + body) for a wiki concept.
//	@Tags			concepts
//	@Produce		json
//	@Param			slug	path		string	true	"Concept slug"
//	@Success		200		{object}	ConceptDetailResponse
//	@Failure		404		{object}	ErrorResponse
//	@Router			/api/v1/concepts/{slug} [get]
func (h *Handler) V1GetConcept(c *gin.Context) {
	gcsClient, err := getGCSClient(c, h.gcs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	slug := c.Param("slug")
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	page, data, err := gcsClient.GetPage(context.Background(), slug, "concepts")
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

// V1Status handles GET /api/v1/status using the request's GCS scope.
//
//	@Summary		Pipeline status
//	@Description	Returns counts and lock status from GCS, search index, and Firestore.
//	@Tags			status
//	@Produce		json
//	@Success		200	{object}	StatusResponse
//	@Router			/api/v1/status [get]
func (h *Handler) V1Status(c *gin.Context) {
	gcsClient, err := getGCSClient(c, h.gcs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	ctx := context.Background()
	sources, _ := gcsClient.ListSources(ctx)
	concepts, _ := gcsClient.ListConcepts(ctx, true)

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
