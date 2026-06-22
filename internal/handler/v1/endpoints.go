package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

const (
	defaultMetadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	defaultCloudRunJobURL   = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
)

// Health handles GET /api/v1/health.
//
//	@Summary		Health check
//	@Description	Returns the V1 API health status.
//	@Tags			health
//	@Produce		json
//	@Success		200		{object}	handler.HealthResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/health [get]
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, handler.HealthResponse{Status: "ok"})
}

// Query handles POST /api/v1/query using the request's GCS scope.
//
//	@Summary		Search wiki content
//	@Description	Full-text search across sources and concepts. Mode "wiki" returns raw results, "full" adds AI-synthesized answer.
//	@Tags			search
//	@Accept			json
//	@Produce		json
//	@Param			request	body		handler.QueryRequest	true	"Search query and mode"
//	@Success		200		{object}	handler.QueryResponse
//	@Failure		400		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/query [post]
func (h *Handler) Query(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	var req handler.QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	query := strings.TrimSpace(req.Query)
	mode := req.Mode
	if mode == "" {
		mode = "wiki"
	}
	if query == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "q field is required"})
		return
	}

	searchQuery := query
	var expandResult *llm.ExpandResult
	if h.expander != nil {
		if result := h.expander.Expand(query); result != nil {
			expandResult = result
			searchQuery = strings.Join(result.Keywords, " ")
		}
	}

	if h.cache == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "concept cache is not configured"})
		return
	}
	results, err := h.cache.Search(c.Request.Context(), gcsClient, searchQuery, 10)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "concept cache: " + err.Error()})
		return
	}
	log.Printf("Search query: %s, results: %v\n", searchQuery, results)

	resp := handler.QueryResponse{
		Query:   query,
		Mode:    mode,
		Results: results,
		Expand:  expandResult,
	}

	if h.llm != nil && len(results) > 0 {
		topN := min(10, len(results))
		contexts := cachedContexts(h.cache, gcsClient, results[:topN])

		if len(contexts) > 0 {
			systemPrompt := buildSystemPrompt(mode)
			userPrompt := buildUserPrompt(query, contexts)
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

func cachedContexts(conceptCache *conceptcache.Cache, reader conceptcache.Reader, results []search.Result) []string {
	contexts := make([]string, 0, len(results))
	for _, result := range results {
		entry, ok := conceptCache.Entry(reader, result.Slug)
		if !ok {
			continue
		}
		sourceContext := "Sources: none listed"
		if len(entry.Sources) > 0 {
			sourceContext = "Sources: " + strings.Join(entry.Sources, ", ")
		}
		contexts = append(contexts, fmt.Sprintf(
			"[%s] %s\n%s\n\n%s",
			entry.Title,
			entry.Slug,
			sourceContext,
			entry.Body,
		))
	}
	return contexts
}

// ListSources handles GET /api/v1/sources using the request's GCS scope.
//
//	@Summary		List wiki sources
//	@Description	Returns all compiled wiki sources.
//	@Tags			sources
//	@Produce		json
//	@Success		200		{object}	handler.SourcesListResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/sources [get]
func (h *Handler) ListSources(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	sources, err := gcsClient.ListSources(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	if sources == nil {
		sources = []gcs.WikiPage{}
	}
	c.JSON(http.StatusOK, handler.SourcesListResponse{Sources: sources, Count: len(sources)})
}

// GetSource handles GET /api/v1/sources/:slug using the request's GCS scope.
//
//	@Summary		Get a source by slug
//	@Description	Returns full content (frontmatter + body) for a wiki source.
//	@Tags			sources
//	@Produce		json
//	@Param			slug	path		string	true	"Source slug"
//	@Success		200		{object}	handler.SourceDetailResponse
//	@Failure		404		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/sources/{slug} [get]
func (h *Handler) GetSource(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	slug := c.Param("slug")
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	_, data, err := gcsClient.GetPage(c.Request.Context(), slug, "sources")
	if err != nil {
		c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "source not found: " + slug})
		return
	}

	frontmatter, body := parseFrontmatter(string(data))
	c.JSON(http.StatusOK, handler.SourceDetailResponse{
		Slug:        slug,
		Title:       slug,
		Type:        "source",
		Frontmatter: frontmatter,
		Body:        body,
		Raw:         string(data),
	})
}

// ListConcepts handles GET /api/v1/concepts using the request's GCS scope.
//
//	@Summary		List wiki concepts
//	@Description	Returns published wiki concepts by default. Set include_drafts=true to include draft concepts.
//	@Tags			concepts
//	@Produce		json
//	@Param			include_drafts	query	bool	false	"Include draft concepts"	default(false)
//	@Success		200				{object}	handler.ConceptsListResponse
//	@Failure		400				{object}	handler.ErrorResponse
//	@Failure		500				{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/concepts [get]
func (h *Handler) ListConcepts(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	includeDrafts, err := strconv.ParseBool(c.DefaultQuery("include_drafts", "false"))
	if err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "include_drafts must be a boolean"})
		return
	}

	concepts, err := gcsClient.ListConcepts(c.Request.Context(), includeDrafts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	if concepts == nil {
		concepts = []gcs.WikiPage{}
	}
	c.JSON(http.StatusOK, handler.ConceptsListResponse{Concepts: concepts, Count: len(concepts)})
}

// GetConcept handles GET /api/v1/concepts/:slug using the request's GCS scope.
//
//	@Summary		Get a concept by slug
//	@Description	Returns full content (frontmatter + body) for a wiki concept.
//	@Tags			concepts
//	@Produce		json
//	@Param			slug	path		string	true	"Concept slug"
//	@Success		200		{object}	handler.ConceptDetailResponse
//	@Failure		404		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/concepts/{slug} [get]
func (h *Handler) GetConcept(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	slug := c.Param("slug")
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	page, data, err := gcsClient.GetPage(c.Request.Context(), slug, "concepts")
	if err != nil {
		c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "concept not found: " + slug})
		return
	}

	frontmatter, body := parseFrontmatter(string(data))
	c.JSON(http.StatusOK, handler.ConceptDetailResponse{
		Slug:        slug,
		Title:       slug,
		Type:        "concept",
		Status:      page.Status,
		Frontmatter: frontmatter,
		Body:        body,
		Raw:         string(data),
	})
}

// Import handles POST /api/v1/import.
//
//	@Summary		Import bookmarks
//	@Description	Accepts a list of URLs to import (Phase 2 — placeholder).
//	@Tags			import
//	@Accept			json
//	@Produce		json
//	@Param			body	body		handler.ImportRequest	true	"URLs to import"
//	@Success		200		{object}	handler.ImportResponse
//	@Failure		400		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/import [post]
func (h *Handler) Import(c *gin.Context) {
	var req handler.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "urls array is required"})
		return
	}

	c.JSON(http.StatusOK, handler.ImportResponse{
		Message:  "Bookmark import — Phase 2 (not yet implemented)",
		Received: len(req.URLs),
		URLs:     req.URLs,
	})
}

// PipelineRun handles POST /api/v1/pipeline/run.
func (h *Handler) PipelineRun(c *gin.Context) {
	var req struct {
		Project string `json:"project" binding:"required"`
		Command string `json:"command"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "project is required"})
		return
	}
	if req.Command == "" {
		req.Command = "run"
	}
	userID := c.GetString("userID")
	if userID == "" {
		userID = h.defaultUser
	}

	token, err := h.getMetadataAccessToken(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{
			Error: fmt.Sprintf("pipeline failed: %s", err),
		})
		return
	}

	body, err := json.Marshal(gin.H{
		"overrides": gin.H{
			"containerOverrides": []gin.H{
				{
					"args": []string{req.Command},
					"env": []gin.H{
						{"name": "USER_ID", "value": userID},
						{"name": "PROJECT_ID", "value": req.Project},
					},
				},
			},
		},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "pipeline failed: " + err.Error()})
		return
	}

	runURL := h.cloudRunJobURL
	if runURL == "" {
		runURL = defaultCloudRunJobURL
	}
	runReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, runURL, bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "pipeline failed: " + err.Error()})
		return
	}
	runReq.Header.Set("Authorization", "Bearer "+token)
	runReq.Header.Set("Content-Type", "application/json")

	resp, err := h.pipelineHTTPClient().Do(runReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "pipeline failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "pipeline failed: " + err.Error()})
		return
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{
			Error: fmt.Sprintf("pipeline failed: %s", string(responseBody)),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":  "accepted",
		"command": req.Command,
		"project": req.Project,
	})
}

func (h *Handler) getMetadataAccessToken(ctx context.Context) (string, error) {
	tokenURL := h.metadataTokenURL
	if tokenURL == "" {
		tokenURL = defaultMetadataTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := h.pipelineHTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("metadata token request failed: %s", string(body))
	}

	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return "", err
	}
	if tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("metadata token response missing access_token")
	}
	return tokenResponse.AccessToken, nil
}

func (h *Handler) pipelineHTTPClient() *http.Client {
	if h.httpClient != nil {
		return h.httpClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Status handles GET /api/v1/status using the request's GCS scope.
//
//	@Summary		Pipeline status
//	@Description	Returns counts and lock status from GCS, search index, and Firestore.
//	@Tags			status
//	@Produce		json
//	@Success		200		{object}	handler.StatusResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/status [get]
func (h *Handler) Status(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	ctx := c.Request.Context()
	sources, _ := gcsClient.ListSources(ctx)
	concepts, _ := gcsClient.ListConcepts(ctx, true)

	resp := handler.StatusResponse{
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
