package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/auth"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/pipelinequota"
	"github.com/rayer/llm-wiki-bff/internal/search"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultMetadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	defaultCloudRunJobURL   = config.DefaultPipelineJobURL
	defaultWorkerCommands   = `[["run","--auto-approve"]]`
)

var (
	errIndexNotFound             = errors.New("index not found")
	errFirestoreNotConfigured    = errors.New("Firestore client is not configured")
	errPipelineExecutionNotFound = errors.New("pipeline execution not found")
	errWikiStorageNotConfigured  = errors.New("wiki storage is not configured")
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

// Index handles GET /api/v1/index.
//
//	@Summary		Read generated index
//	@Description	Returns the cached ID map JSON for the request scope.
//	@Tags			index
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Failure		404	{object}	handler.ErrorResponse
//	@Failure		500	{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/index [get]
func (h *Handler) Index(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}

	data, err := readIndexJSON(c.Request.Context(), gcsClient)
	if err != nil {
		if errors.Is(err, errIndexNotFound) {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "index not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}

	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

type indexReader interface {
	ReadFile(context.Context, string) ([]byte, error)
}

func readIndexJSON(ctx context.Context, reader indexReader) ([]byte, error) {
	data, err := reader.ReadFile(ctx, idMapPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, errIndexNotFound
		}
		return nil, fmt.Errorf("read index: %w", err)
	}
	return data, nil
}

// ListProjects handles GET /api/v1/projects.
//
//	@Summary		List user projects
//	@Description	Returns all projects for the authenticated user.
//	@Tags			projects
//	@Produce		json
//	@Success		200	{array}		handler.ProjectResponse
//	@Failure		401	{object}	handler.ErrorResponse
//	@Failure		500	{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Router			/api/v1/projects [get]
func (h *Handler) ListProjects(c *gin.Context) {
	userID := c.GetString("userID")
	if strings.TrimSpace(userID) == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}
	if h.firestore != nil && h.firestore.Raw() != nil {
		resp, err := h.listFirestoreProjects(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "projects unavailable"})
			return
		}
		if len(resp) > 0 {
			c.JSON(http.StatusOK, resp)
			return
		}
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "wiki storage is not configured"})
		return
	}

	projects, err := h.store.ListProjects(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "projects unavailable"})
		return
	}

	resp := make([]handler.ProjectResponse, 0, len(projects))
	for _, project := range projects {
		name := project.ID
		// Try project index.md title first
		if data, err := h.store.Scope(userID, project.ID).ReadFile(c.Request.Context(), "index.md"); err == nil {
			if title := projectTitleFromIndex(data); title != "" {
				name = title
			}
		}
		// If still using project ID, try Firestore for the actual name
		if name == project.ID && h.firestore != nil && h.firestore.Raw() != nil {
			docID := userID + "_" + project.ID
			if doc, err := h.firestore.Raw().Collection("projects").Doc(docID).Get(c.Request.Context()); err == nil {
				if fsName, ok := doc.Data()["name"].(string); ok && strings.TrimSpace(fsName) != "" {
					name = fsName
				}
			}
		}
		resp = append(resp, handler.ProjectResponse{
			ID:        project.ID,
			Name:      name,
			CreatedAt: project.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, resp)
}

func (h *Handler) listFirestoreProjects(ctx context.Context, userID string) ([]handler.ProjectResponse, error) {
	iter := h.firestore.Raw().Collection("projects").Documents(ctx)
	defer iter.Stop()

	resp := make([]handler.ProjectResponse, 0)
	for {
		doc, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, fmt.Errorf("list projects: %w", err)
		}
		project, uid, ok := projectResponseFromFirestoreDoc(doc.Ref.ID, doc.Data())
		if !ok || uid != userID {
			continue
		}
		resp = append(resp, project)
	}
	return resp, nil
}

func projectResponseFromFirestoreDoc(docID string, data map[string]interface{}) (handler.ProjectResponse, string, bool) {
	userID, docProjectID := splitProjectDocID(docID)
	if userID == "" {
		return handler.ProjectResponse{}, "", false
	}
	// Idempotency cache docs live in the same collection and share project_id with
	// the real project; skip them so list endpoints do not emit duplicate IDs.
	if isIdempotencyCacheDoc(docID, data) {
		return handler.ProjectResponse{}, "", false
	}
	projectID, _ := data["project_id"].(string)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = strings.TrimSpace(docProjectID)
	}
	if projectID == "" {
		return handler.ProjectResponse{}, "", false
	}

	name, _ := data["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		name = projectID
	}

	return handler.ProjectResponse{
		ID:        projectID,
		Name:      name,
		CreatedAt: firestoreCreatedAt(data["created_at"]),
	}, userID, true
}

// isIdempotencyCacheDoc reports whether a Firestore projects collection document is
// the init-project idempotency cache entry rather than the real project.
// Cache docs are stored at {userID}_{idempotencyKey} and still carry project_id.
func isIdempotencyCacheDoc(docID string, data map[string]interface{}) bool {
	key, _ := data["idempotency_key"].(string)
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	_, docSuffix := splitProjectDocID(docID)
	return strings.TrimSpace(docSuffix) == key
}

func firestoreCreatedAt(value interface{}) string {
	switch v := value.(type) {
	case time.Time:
		if !v.IsZero() {
			return v.UTC().Format(time.RFC3339)
		}
	case *time.Time:
		if v != nil && !v.IsZero() {
			return v.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func projectTitleFromIndex(data []byte) string {
	frontmatter, _ := parseFrontmatter(string(data))
	title, _ := frontmatter["title"].(string)
	return strings.TrimSpace(title)
}

// Ready handles GET /api/v1/ready — returns 200 when the concept cache is warm
// for the requesting project, 503 otherwise.
//
//	@Summary		Readiness check
//	@Description	Returns 200 when the concept cache is warm for the request scope. Returns 503 if the cache is not ready.
//	@Tags			health
//	@Produce		json
//	@Success		200	{object}	handler.ReadyResponse
//	@Failure		503	{object}	handler.ReadyResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/ready [get]
func (h *Handler) Ready(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, handler.ReadyResponse{
			Ready:   false,
			Message: "generated data unavailable",
		})
		return
	}
	prefix := gcsClient.Prefix()
	if h.cache == nil {
		c.JSON(http.StatusServiceUnavailable, handler.ReadyResponse{
			Ready:   false,
			Prefix:  prefix,
			Message: "concept cache is not configured",
		})
		return
	}
	if h.cache.IsReady(viewCacheKey(gcsClient)) {
		c.JSON(http.StatusOK, handler.ReadyResponse{
			Ready:  true,
			Prefix: prefix,
		})
		return
	}
	c.JSON(http.StatusServiceUnavailable, handler.ReadyResponse{
		Ready:    false,
		Prefix:   prefix,
		Prefixes: h.cache.Prefixes(),
		Message:  "concept cache not warm for this project",
	})
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
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
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
		if result, err := h.expander.Expand(query); err != nil {
			log.Printf("[expander] query expansion failed for %q: %v — falling back to raw query", query, err)
		} else if result != nil {
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
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
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
			if _, err := conceptCache.Build(context.Background(), reader); err == nil {
				entry, ok = conceptCache.Entry(reader, result.Slug)
			}
		}
		if !ok {
			continue
		}
		sourceContext := "Sources: none listed"
		if len(entry.Sources) > 0 {
			sourceContext = "Sources: [" + strings.Join(entry.Sources, ", ") + "]"
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
//	@Summary		List compiled sources and pending raw files
//	@Description	Returns compiled wiki sources plus pending raw rows that have not been compiled.
//	@Tags			sources
//	@Produce		json
//	@Success		200		{object}	handler.SourcesListResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/sources [get]
func (h *Handler) ListSources(c *gin.Context) {
	ctx := c.Request.Context()

	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}

	sources, err := listSourcesCacheFirst(ctx, gcsClient)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}
	if err := addWikiPageIDsFromIDMap(ctx, gcsClient, sources, "source"); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}
	if missingSourceRawPath(sources) {
		key := viewCacheKey(gcsClient)
		if err := h.hydrateLegacySourceMetadata(ctx, gcsClient, sources, key); err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
			return
		}
	}
	if sources, _, err = sourceLifecycle(ctx, gcsClient, sources); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}
	if sources == nil {
		sources = []gcs.WikiPage{}
	}

	c.JSON(http.StatusOK, handler.SourcesListResponse{Sources: sources, Count: len(sources)})
}

// hydrateLegacySourceMetadata shares the one-per-project fallback for id maps
// produced before source_meta existed. Both listing and quota evaluation must
// use it so a legacy project never re-scans source Markdown on every request.
func (h *Handler) hydrateLegacySourceMetadata(ctx context.Context, reader sourceListReader, sources []gcs.WikiPage, key string) error {
	legacy := h.listCacheGet(key).legacySourceMeta
	if len(legacy) == 0 {
		listed, err := reader.ListSources(ctx)
		if err != nil {
			return err
		}
		legacy = listed
		h.listCacheSet(key, func(cache *cachedLists) { cache.legacySourceMeta = cloneWikiPages(listed) })
	}
	hydrateSourceMetadata(sources, legacy)
	return nil
}

func missingSourceRawPath(sources []gcs.WikiPage) bool {
	for _, source := range sources {
		if strings.TrimSpace(source.RawPath) == "" {
			return true
		}
	}
	return false
}

type sourceListReader interface {
	ListSourcesFromCache(context.Context) ([]gcs.WikiPage, error)
	ListSources(context.Context) ([]gcs.WikiPage, error)
}

type conceptListReader interface {
	ListConceptsFromCache(context.Context) ([]gcs.WikiPage, error)
	ListConcepts(context.Context, bool) ([]gcs.WikiPage, error)
}

func listSourcesCacheFirst(ctx context.Context, reader sourceListReader) ([]gcs.WikiPage, error) {
	sources, err := reader.ListSourcesFromCache(ctx)
	if err == nil {
		return sources, nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return reader.ListSources(ctx)
	}
	return nil, err
}

func listConceptsCacheFirst(ctx context.Context, reader conceptListReader, includeDrafts bool) ([]gcs.WikiPage, error) {
	concepts, err := reader.ListConceptsFromCache(ctx)
	if err == nil {
		return concepts, nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return reader.ListConcepts(ctx, includeDrafts)
	}
	return nil, err
}

func addWikiPageIDsFromIDMap(ctx context.Context, reader indexReader, pages []gcs.WikiPage, pageType string) error {
	data, err := reader.ReadFile(ctx, idMapPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return fmt.Errorf("read id map: %w", err)
	}

	source, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return fmt.Errorf("decode id map: %w", err)
	}
	switch pageType {
	case "concept":
		mergeWikiPageIDs(pages, source.Concept)
	case "source":
		mergeWikiPageIDs(pages, source.Source)
		mergeSourceMetadata(pages, source.SourceMeta)
	default:
		return fmt.Errorf("unknown wiki page type: %s", pageType)
	}
	return nil
}

func mergeSourceMetadata(pages []gcs.WikiPage, entries map[string]wikiindex.SourceMeta) {
	if len(pages) == 0 || len(entries) == 0 {
		return
	}
	bySlug := make(map[string]wikiindex.SourceMeta, len(entries))
	for _, meta := range entries {
		if slug := strings.TrimSpace(meta.Slug); slug != "" {
			bySlug[slug] = meta
		}
	}
	for i := range pages {
		meta, ok := bySlug[pages[i].Slug]
		if !ok {
			continue
		}
		if pages[i].RawPath == "" {
			pages[i].RawPath = strings.TrimSpace(meta.SourceFile)
		}
		if pages[i].Title == pages[i].Slug && strings.TrimSpace(meta.Title) != "" {
			pages[i].Title = strings.TrimSpace(meta.Title)
		}
	}
}

func hydrateSourceMetadata(pages, metadata []gcs.WikiPage) {
	bySlug := make(map[string]gcs.WikiPage, len(metadata))
	for _, page := range metadata {
		bySlug[page.Slug] = page
	}
	for i := range pages {
		if page, ok := bySlug[pages[i].Slug]; ok {
			pages[i].RawPath = page.RawPath
			if pages[i].Title == pages[i].Slug {
				pages[i].Title = page.Title
			}
		}
	}
}

func mergeWikiPageIDs(pages []gcs.WikiPage, entries map[string]string) {
	if len(pages) == 0 || len(entries) == 0 {
		return
	}

	idsBySlug := make(map[string]string, len(entries))
	for id, slug := range entries {
		id = strings.TrimSpace(id)
		slug = strings.TrimSpace(slug)
		if id == "" || slug == "" {
			continue
		}
		idsBySlug[slug] = id
	}

	for i := range pages {
		if strings.TrimSpace(pages[i].ID) != "" {
			continue
		}
		if id := idsBySlug[pages[i].Slug]; id != "" {
			pages[i].ID = id
		}
	}
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
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}

	slug := c.Param("id")
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	if h.handleIDRoutedPage(c, gcsClient, "source", slug) {
		return
	}
	page, data, err := gcsClient.GetPage(c.Request.Context(), slug, "sources")
	if err != nil {
		writeGeneratedReadError(c, err, "source not found: "+slug)
		return
	}
	if rewritten, ok := h.rewriteMarkdownForResponse(c, gcsClient, data); ok {
		data = rewritten
	} else {
		return
	}

	response, err := h.sourceDetailResponse(c, gcsClient, *page, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}
	c.JSON(http.StatusOK, response)
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
	ctx := c.Request.Context()

	includeDrafts, err := strconv.ParseBool(c.DefaultQuery("include_drafts", "false"))
	if err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "include_drafts must be a boolean"})
		return
	}

	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}

	concepts, err := listConceptsCacheFirst(ctx, gcsClient, includeDrafts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}
	if err := addWikiPageIDsFromIDMap(ctx, gcsClient, concepts, "concept"); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
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
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}

	slug := c.Param("id")
	if decoded, err := url.PathUnescape(slug); err == nil {
		slug = decoded
	}
	if h.handleIDRoutedPage(c, gcsClient, "concept", slug) {
		return
	}
	page, data, err := gcsClient.GetPage(c.Request.Context(), slug, "concepts")
	if err != nil {
		writeGeneratedReadError(c, err, "concept not found: "+slug)
		return
	}
	// Set ID from id_map
	if page.ID == "" {
		pages := []gcs.WikiPage{*page}
		_ = addWikiPageIDsFromIDMap(c.Request.Context(), gcsClient, pages, "concept")
		page.ID = pages[0].ID
	}
	if rewritten, ok := h.rewriteMarkdownForResponse(c, gcsClient, data); ok {
		data = rewritten
	} else {
		return
	}

	frontmatter, body := parseFrontmatter(string(data))
	c.JSON(http.StatusOK, handler.ConceptDetailResponse{
		Slug:        slug,
		ID:          page.ID,
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
	projectID := c.GetString("projectID")
	if projectID == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "project is required"})
		return
	}
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "user is required"})
		return
	}

	ctx := c.Request.Context()
	snap, reserved, prev, err := h.evaluateQuota(ctx, userID, projectID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{
			Error: pipelineUnavailableMessage,
		})
		return
	}
	// Post-reserve snapshots re-evaluate the *next* run and often have Allowed=false
	// (cooldown). Use reserved for the accept path; only block when nothing was reserved
	// and the pre-reserve evaluation denied the run. Unenforced (no store) is Allowed + !reserved.
	if !reserved && !snap.Allowed {
		c.JSON(httpStatusForReason(snap.Reason), gin.H{
			"error": fmt.Sprintf("pipeline blocked: %s", snap.Reason),
			"quota": snap,
		})
		return
	}

	executionID, err := h.invokePipelineJob(ctx, userID, projectID)
	if err != nil {
		if reserved {
			if qs := h.effectiveQuotaStore(); qs != nil {
				if refundErr := qs.RefundQuotaPrev(ctx, userID, projectID, prev); refundErr != nil {
					log.Print("pipeline quota refund failed")
				}
			}
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{
			Error: pipelineUnavailableMessage,
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":       "accepted",
		"command":      "run",
		"project_id":   projectID,
		"execution_id": executionID,
		"quota":        snap,
	})
}

// invokePipelineJob starts the shared Cloud Run pipeline job for userID/projectID.
// Used by both user PipelineRun and AdminPipelineTrigger.
func (h *Handler) invokePipelineJob(ctx context.Context, userID, projectID string) (executionID string, err error) {
	token, err := h.getMetadataAccessToken(ctx)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(gin.H{
		"overrides": gin.H{
			"containerOverrides": []gin.H{
				{
					"args": []string{"run", defaultWorkerCommands},
					"env": []gin.H{
						{"name": "USER_ID", "value": userID},
						{"name": "PROJECT_ID", "value": projectID},
						{"name": "TASK_TYPE", "value": "pipeline"},
					},
				},
			},
		},
	})
	if err != nil {
		return "", err
	}

	runURL := h.pipelineJobURL()
	runReq, err := http.NewRequestWithContext(ctx, http.MethodPost, runURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	runReq.Header.Set("Authorization", "Bearer "+token)
	runReq.Header.Set("Content-Type", "application/json")

	resp, err := h.pipelineHTTPClient().Do(runReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("%s", string(responseBody))
	}
	executionID, err = cloudRunExecutionIDFromRunResponse(responseBody)
	if err != nil {
		return "", err
	}
	return executionID, nil
}

type cloudRunJobRunResponse struct {
	Name     string `json:"name"`
	Metadata struct {
		Execution string `json:"execution"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Response struct {
		Name string `json:"name"`
	} `json:"response"`
}

type pipelineStatusResponse struct {
	LastExecution    *handler.PipelineExecutionResponse `json:"last_execution"`
	ProjectID        string                             `json:"project_id"`
	Quota            *pipelinequota.Snapshot            `json:"quota,omitempty"`
	SuggestedQueries []string                           `json:"suggested_queries"`
}

type cloudRunExecutionsResponse struct {
	Executions    []cloudRunExecution `json:"executions"`
	NextPageToken string              `json:"nextPageToken"`
}

type cloudRunExecution struct {
	Name             string                    `json:"name"`
	StartTime        string                    `json:"startTime"`
	CompletionTime   string                    `json:"completionTime"`
	EndTime          string                    `json:"endTime"`
	CompletionStatus string                    `json:"completionStatus"`
	Conditions       []cloudRunCondition       `json:"conditions"`
	RunningCount     int                       `json:"runningCount"`
	SucceededCount   int                       `json:"succeededCount"`
	FailedCount      int                       `json:"failedCount"`
	CancelledCount   int                       `json:"cancelledCount"`
	Template         cloudRunExecutionTemplate `json:"template"`
}

type cloudRunExecutionTemplate struct {
	Containers []cloudRunContainer `json:"containers"`
}

type cloudRunContainer struct {
	Env []cloudRunEnv `json:"env"`
}

type cloudRunEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cloudRunCondition struct {
	Type   string `json:"type"`
	State  string `json:"state"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// PipelineStatus handles GET /api/v1/pipeline/status.
//
//	@Summary		Pipeline execution status
//	@Description	Returns the latest Cloud Run pipeline execution for the current project. Pass execution_id to fetch a specific execution. When an execution is available, last_execution.log_url points to the authenticated log endpoint.
//	@Tags			pipeline
//	@Produce		json
//	@Param			execution_id	query		string	false	"Cloud Run execution ID"
//	@Success		200				{object}	pipelineStatusResponse
//	@Failure		400				{object}	handler.ErrorResponse
//	@Failure		401				{object}	handler.ErrorResponse
//	@Failure		500				{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/pipeline/status [get]
func (h *Handler) PipelineStatus(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString("userID"))
	if userID == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}
	projectID := strings.TrimSpace(c.GetString("projectID"))
	if projectID == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "project is required"})
		return
	}

	ctx := c.Request.Context()
	executionID := strings.TrimSpace(c.Query("execution_id"))
	if executionID != "" {
		var err error
		executionID, err = validatePipelineExecutionID(executionID)
		if err != nil {
			c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: err.Error()})
			return
		}
	}
	response := pipelineStatusResponse{ProjectID: projectID}
	var suggestedErr error
	response.SuggestedQueries, suggestedErr = h.loadSuggestedQueries(ctx, c)
	if suggestedErr != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineStatusUnavailableMessage})
		return
	}
	lastExecution, err := h.pipelineExecutionStatusForOwner(ctx, executionID, userID, projectID)
	if err != nil {
		if errors.Is(err, errPipelineExecutionNotFound) {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: errPipelineExecutionNotFound.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineStatusUnavailableMessage})
		return
	}
	response.LastExecution = lastExecution

	// Evaluate-only quota snapshot for frontend Run-button gating (never mutates).
	snap, _, _, err := h.evaluateQuota(ctx, userID, projectID, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineStatusUnavailableMessage})
		return
	}
	response.Quota = &snap
	c.JSON(http.StatusOK, response)
}

// RebuildIndex handles POST /api/v1/pipeline/rebuild-index.
func (h *Handler) RebuildIndex(c *gin.Context) {
	// Header first — this route is outside JWTAuth, defaultRequestScope may set global values
	userID := strings.TrimSpace(c.GetHeader("X-User-ID"))
	if userID == "" {
		userID = strings.TrimSpace(c.GetString("userID"))
	}
	projectID := strings.TrimSpace(c.GetHeader("X-Project-ID"))
	if projectID == "" {
		projectID = strings.TrimSpace(c.GetString("projectID"))
	}
	if userID == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}
	if projectID == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "project is required"})
		return
	}
	if h.store != nil {
		if guarded, ok := h.store.Scope(userID, projectID).(store.GenerationAware); ok {
			exists, err := guarded.HasCurrentManifest(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated output unavailable"})
				return
			}
			if exists {
				c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "generated output is managed by the pipeline; run the pipeline"})
				return
			}
		}
	}

	ctx := c.Request.Context()
	if h.rebuildIndex != nil {
		next, err := h.rebuildIndex(ctx, userID, projectID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			return
		}
		h.invalidateCachesAfterRebuild(userID, projectID)
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"entries": gin.H{
				"concept": len(next.Concept),
				"source":  len(next.Source),
			},
		})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "wiki storage is not configured"})
		return
	}
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	fs := h.firestore.Raw()
	if err := acquireRebuildIndexLock(ctx, fs, userID, projectID, time.Now()); err != nil {
		if errors.Is(err, errRebuildIndexLocked) {
			c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "rebuild index already running"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	wikiStore := h.store.Scope(userID, projectID)
	var releaseLegacy func() error
	if session, ok := wikiStore.(store.LegacyGenerationWriteSession); ok {
		locked, release, err := session.BeginLegacyGenerationWrite(ctx)
		if err != nil {
			cleanupErr := finishRebuild(nil, func() error { return releaseRebuildIndexLock(context.Background(), fs, userID, projectID) })
			if cleanupErr != nil || errors.Is(err, store.ErrLeaseCleanup) {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
				return
			}
			if errors.Is(err, store.ErrGenerationManaged) {
				c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "generated output is managed by the pipeline; run the pipeline"})
			} else {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			}
			return
		}
		releaseLegacy = func() error { return release(context.Background()) }
		wikiStore = locked
	}
	next, err := wikiindex.Rebuild(ctx, newWikiIndexStore(wikiStore))
	if err = finishRebuild(err, releaseLegacy, func() error { return releaseRebuildIndexLock(context.Background(), fs, userID, projectID) }); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}

	h.invalidateCachesAfterRebuild(userID, projectID)
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"entries": gin.H{
			"concept": len(next.Concept),
			"source":  len(next.Source),
		},
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

func (h *Handler) cloudRunExecutionsURL() string {
	return h.cloudRunExecutionsURLWithPageToken("")
}

func (h *Handler) cloudRunExecutionsURLWithPageToken(pageToken string) string {
	runURL := h.pipelineJobURL()
	baseURL := strings.TrimSuffix(runURL, ":run")
	values := url.Values{}
	values.Set("pageSize", "20")
	if pageToken != "" {
		values.Set("pageToken", pageToken)
	}
	return baseURL + "/executions?" + values.Encode()
}

func (h *Handler) cloudRunExecutionURL(executionID string) string {
	runURL := h.pipelineJobURL()
	baseURL := strings.TrimSuffix(runURL, ":run")
	return baseURL + "/executions/" + url.PathEscape(executionID)
}

func (h *Handler) pipelineJobURL() string {
	if h.cloudRunJobURL != "" {
		return h.cloudRunJobURL
	}
	return defaultCloudRunJobURL
}

func cloudRunExecutionIDFromRunResponse(body []byte) (string, error) {
	var runResponse cloudRunJobRunResponse
	if err := json.Unmarshal(body, &runResponse); err != nil {
		return "", err
	}
	if executionID := shortCloudRunExecutionName(runResponse.Metadata.Execution, true); executionID != "" {
		return executionID, nil
	}
	if executionID := shortCloudRunExecutionName(runResponse.Metadata.Name, false); executionID != "" {
		return executionID, nil
	}
	if executionID := shortCloudRunExecutionName(runResponse.Response.Name, false); executionID != "" {
		return executionID, nil
	}
	if executionID := shortCloudRunExecutionName(runResponse.Name, false); executionID != "" {
		return executionID, nil
	}
	return "", fmt.Errorf("Cloud Run response missing execution name")
}

func shortCloudRunExecutionName(name string, allowBare bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	const marker = "/executions/"
	if index := strings.LastIndex(name, marker); index >= 0 {
		executionID := name[index+len(marker):]
		if slash := strings.IndexByte(executionID, '/'); slash >= 0 {
			executionID = executionID[:slash]
		}
		return strings.TrimSpace(executionID)
	}
	if allowBare && !strings.Contains(name, "/") {
		return name
	}
	return ""
}

func (h *Handler) pipelineExecutionStatus(ctx context.Context, executionID string) (*handler.PipelineExecutionResponse, error) {
	return h.pipelineExecutionStatusWithOwner(ctx, executionID, nil)
}

func (h *Handler) pipelineExecutionStatusForOwner(ctx context.Context, executionID, userID, projectID string) (*handler.PipelineExecutionResponse, error) {
	return h.pipelineExecutionStatusWithOwner(ctx, executionID, &pipelineExecutionOwner{
		userID:    userID,
		projectID: projectID,
	})
}

type pipelineExecutionOwner struct {
	userID    string
	projectID string
}

func (h *Handler) pipelineExecutionStatusWithOwner(ctx context.Context, executionID string, owner *pipelineExecutionOwner) (*handler.PipelineExecutionResponse, error) {
	token, err := h.getMetadataAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	if executionID != "" {
		execution, err := h.fetchCloudRunExecution(ctx, token, executionID)
		if err != nil {
			return nil, err
		}
		if owner != nil && !cloudRunExecutionOwnedBy(execution, owner.userID, owner.projectID) {
			return nil, errPipelineExecutionNotFound
		}
		return newPipelineExecutionResponse(execution), nil
	}

	pageToken := ""
	for {
		executions, err := h.listCloudRunExecutions(ctx, token, pageToken)
		if err != nil {
			return nil, err
		}
		if owner == nil {
			if len(executions.Executions) == 0 {
				return nil, nil
			}
			return newPipelineExecutionResponse(executions.Executions[0]), nil
		}
		for _, execution := range executions.Executions {
			if cloudRunExecutionOwnedBy(execution, owner.userID, owner.projectID) {
				return newPipelineExecutionResponse(execution), nil
			}
		}
		if executions.NextPageToken == "" {
			return nil, nil
		}
		pageToken = executions.NextPageToken
	}
}

func (h *Handler) fetchCloudRunExecution(ctx context.Context, token, executionID string) (cloudRunExecution, error) {
	statusURL := h.cloudRunExecutionURL(executionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return cloudRunExecution{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.pipelineHTTPClient().Do(req)
	if err != nil {
		return cloudRunExecution{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cloudRunExecution{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return cloudRunExecution{}, errPipelineExecutionNotFound
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return cloudRunExecution{}, fmt.Errorf("%s", string(body))
	}

	var execution cloudRunExecution
	if err := json.Unmarshal(body, &execution); err != nil {
		return cloudRunExecution{}, err
	}
	return execution, nil
}

func (h *Handler) listCloudRunExecutions(ctx context.Context, token, pageToken string) (cloudRunExecutionsResponse, error) {
	statusURL := h.cloudRunExecutionsURLWithPageToken(pageToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return cloudRunExecutionsResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.pipelineHTTPClient().Do(req)
	if err != nil {
		return cloudRunExecutionsResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cloudRunExecutionsResponse{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return cloudRunExecutionsResponse{}, fmt.Errorf("%s", string(body))
	}

	var executions cloudRunExecutionsResponse
	if err := json.Unmarshal(body, &executions); err != nil {
		return cloudRunExecutionsResponse{}, err
	}
	return executions, nil
}

func cloudRunExecutionOwnedBy(execution cloudRunExecution, userID, projectID string) bool {
	userID = strings.TrimSpace(userID)
	projectID = strings.TrimSpace(projectID)
	if userID == "" || projectID == "" {
		return false
	}

	for _, container := range execution.Template.Containers {
		env := make(map[string]string)
		for _, variable := range container.Env {
			env[variable.Name] = variable.Value
		}
		if env["USER_ID"] == userID &&
			env["PROJECT_ID"] == projectID &&
			env["TASK_TYPE"] == "pipeline" {
			return true
		}
	}
	return false
}

func newPipelineExecutionResponse(execution cloudRunExecution) *handler.PipelineExecutionResponse {
	endTime := execution.CompletionTime
	if endTime == "" {
		endTime = execution.EndTime
	}
	return &handler.PipelineExecutionResponse{
		Name:      execution.Name,
		Status:    cloudRunExecutionStatus(execution),
		StartTime: execution.StartTime,
		EndTime:   endTime,
		Duration:  executionDuration(execution.StartTime, endTime),
		LogURL:    pipelineLogURLForExecution(execution),
	}
}

func pipelineLogURLForExecution(execution cloudRunExecution) string {
	executionID := shortCloudRunExecutionName(execution.Name, true)
	if executionID == "" {
		return ""
	}
	return "/api/v1/pipeline/log?execution_id=" + url.QueryEscape(executionID)
}

// PipelineLog handles GET /api/v1/pipeline/log.
//
//	@Summary		Read pipeline log
//	@Description	Returns the stdout and stderr log captured by the pipeline worker for the current project execution.
//	@Tags			pipeline
//	@Produce		plain
//	@Param			execution_id	query		string	true	"Cloud Run execution ID"
//	@Success		200				{string}	string
//	@Failure		400				{object}	handler.ErrorResponse
//	@Failure		401				{object}	handler.ErrorResponse
//	@Failure		404				{object}	handler.ErrorResponse
//	@Failure		500				{object}	handler.ErrorResponse
//	@Security		DevUserAuth
//	@Security		ProjectHeader
//	@Router			/api/v1/pipeline/log [get]
func (h *Handler) PipelineLog(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString("userID"))
	if userID == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}
	projectID := strings.TrimSpace(c.GetString("projectID"))
	if projectID == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "project is required"})
		return
	}

	executionID := strings.TrimSpace(c.Query("execution_id"))
	logPath, err := pipelineLogPath(executionID)
	if err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: err.Error()})
		return
	}
	if _, err := h.pipelineExecutionStatusForOwner(c.Request.Context(), executionID, userID, projectID); err != nil {
		if errors.Is(err, errPipelineExecutionNotFound) {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: errPipelineExecutionNotFound.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineExecutionUnavailableMessage})
		return
	}

	wikiStore, err := h.GetStore(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	data, err := wikiStore.ReadFile(c.Request.Context(), logPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "pipeline log not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", data)
}

func pipelineLogPath(executionID string) (string, error) {
	var err error
	executionID, err = validatePipelineExecutionID(executionID)
	if err != nil {
		return "", err
	}
	return "cache/pipeline-" + executionID + ".log", nil
}

func validatePipelineExecutionID(executionID string) (string, error) {
	executionID = strings.TrimSpace(executionID)
	if executionID == "" {
		return "", errors.New("execution_id is required")
	}
	if strings.ContainsAny(executionID, `/\`+"\x00") || executionID == "." || executionID == ".." || strings.Contains(executionID, "..") {
		return "", errors.New("invalid execution_id")
	}
	return executionID, nil
}

func cloudRunExecutionStatus(execution cloudRunExecution) string {
	if execution.CompletionStatus != "" {
		return normalizeCloudRunStatus(execution.CompletionStatus)
	}
	for _, condition := range execution.Conditions {
		if condition.Type != "Completed" {
			continue
		}
		if condition.State != "" {
			return normalizeCloudRunStatus(condition.State)
		}
		if condition.Status != "" {
			return normalizeCloudRunStatus(condition.Status)
		}
		if condition.Reason != "" {
			return normalizeCloudRunStatus(condition.Reason)
		}
	}
	if execution.FailedCount > 0 {
		return "FAILED"
	}
	if execution.CancelledCount > 0 {
		return "CANCELLED"
	}
	if execution.RunningCount > 0 {
		return "RUNNING"
	}
	if execution.SucceededCount > 0 {
		return "SUCCEEDED"
	}
	return "UNKNOWN"
}

func normalizeCloudRunStatus(value string) string {
	status := strings.ToUpper(strings.TrimSpace(value))
	status = strings.TrimPrefix(status, "CONDITION_")
	status = strings.TrimPrefix(status, "EXECUTION_")
	switch status {
	case "SUCCEEDED", "TRUE":
		return "SUCCEEDED"
	case "FAILED", "FALSE":
		return "FAILED"
	case "CANCELLED":
		return "CANCELLED"
	case "PENDING", "RECONCILING", "UNKNOWN":
		return "RUNNING"
	default:
		return status
	}
}

func executionDuration(startTime, endTime string) string {
	if startTime == "" || endTime == "" {
		return ""
	}
	start, err := time.Parse(time.RFC3339Nano, startTime)
	if err != nil {
		return ""
	}
	end, err := time.Parse(time.RFC3339Nano, endTime)
	if err != nil {
		return ""
	}
	if end.Before(start) {
		return ""
	}
	return end.Sub(start).String()
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
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}

	ctx := c.Request.Context()
	sources, _ := listSourcesCacheFirst(ctx, gcsClient)
	concepts, _ := listConceptsCacheFirst(ctx, gcsClient, true)

	resp := handler.StatusResponse{
		SourcesCount:  len(sources),
		ConceptsCount: len(concepts),
		RawCount:      rawFileCount(ctx, gcsClient),
		IndexSources:  h.index.SourceCount(),
		IndexConcepts: h.index.ConceptCount(),
	}

	if lastExecution, err := h.pipelineExecutionStatus(ctx, ""); err == nil {
		resp.LastExecution = lastExecution
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

func (h *Handler) loadSuggestedQueries(ctx context.Context, c *gin.Context) ([]string, error) {
	wikiStore, err := h.GetStore(c)
	if err != nil {
		if errors.Is(err, errWikiStorageNotConfigured) {
			return []string{}, nil
		}
		return nil, err
	}
	data, err := wikiStore.ReadFile(ctx, suggestedqueries.Path)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	artifact, err := suggestedqueries.Decode(data)
	if err != nil {
		return nil, err
	}
	return suggestedqueries.Queries(artifact), nil
}

// rawFileCount returns the live number of files under project raw/ (ListRawFiles).
// Intentionally ignores cache/raw_status.json so nav/status counts update after upload
// without waiting for pipeline postprocess (LWC-129). List errors yield 0.
func rawFileCount(ctx context.Context, wikiStore store.Store) int {
	files, err := wikiStore.ListRawFiles(ctx)
	if err != nil {
		return 0
	}
	return len(files)
}

// splitProjectDocID parses a Firestore doc ID in the format "{userID}_{projectID}".
func splitProjectDocID(docID string) (userID, projectID string) {
	if len(docID) < 14 {
		return docID, ""
	}
	lastUnderscore := strings.LastIndex(docID, "_")
	if lastUnderscore < 0 || lastUnderscore == len(docID)-1 {
		return docID, ""
	}
	return docID[:lastUnderscore], docID[lastUnderscore+1:]
}

func (h *Handler) verifyAdminProjectExists(ctx context.Context, docID string) error {
	if h.projectExists != nil {
		return h.projectExists(ctx, docID)
	}
	if h.firestore == nil || h.firestore.Raw() == nil {
		return errFirestoreNotConfigured
	}
	_, err := h.firestore.Raw().Collection("projects").Doc(docID).Get(ctx)
	return err
}

type adminProjectEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
	UserEmail    string `json:"user_email"`
	ProjectID    string `json:"project_id"`
	ConceptCount int    `json:"concept_count"`
	SourceCount  int    `json:"source_count"`
}

type adminUserEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Role         string `json:"role"`
	ProjectCount int    `json:"project_count"`
}

type adminProjectRecord struct {
	id        string
	name      string
	userID    string
	projectID string
}

type adminProjectStatistics struct {
	conceptCount int
	sourceCount  int
}

func adminProjectRecordFromFirestoreDoc(docID string, data map[string]interface{}) (adminProjectRecord, bool) {
	userID, docProjectID := splitProjectDocID(docID)
	if !auth.ValidPathSegment(userID) || !auth.ValidPathSegment(docProjectID) || !strings.Contains(docID, "_") {
		return adminProjectRecord{}, false
	}
	project, userID, ok := projectResponseFromFirestoreDoc(docID, data)
	if !ok {
		return adminProjectRecord{}, false
	}
	if !auth.ValidPathSegment(userID) || !auth.ValidPathSegment(project.ID) {
		return adminProjectRecord{}, false
	}
	return adminProjectRecord{
		id:        docID,
		name:      project.Name,
		userID:    userID,
		projectID: project.ID,
	}, true
}

func adminProjectRecordKey(userID, projectID string) string {
	return userID + "/" + projectID
}

func loadAdminProjectStatistics(ctx context.Context, root store.RootStore, projects []adminProjectRecord) (map[string]adminProjectStatistics, error) {
	if root == nil {
		return nil, fmt.Errorf("wiki storage is not configured")
	}

	statistics := make(map[string]adminProjectStatistics, len(projects))
	for _, project := range projects {
		scoped := root.Scope(project.userID, project.projectID)
		if scoped == nil {
			return nil, fmt.Errorf("wiki storage is not configured for project %s", adminProjectRecordKey(project.userID, project.projectID))
		}
		var err error
		scoped, err = pinStore(ctx, scoped)
		if err != nil {
			return nil, fmt.Errorf("pin project %s: %w", adminProjectRecordKey(project.userID, project.projectID), err)
		}

		concepts, err := listConceptsCacheFirst(ctx, scoped, false)
		if err != nil {
			return nil, fmt.Errorf("list concepts for project %s: %w", adminProjectRecordKey(project.userID, project.projectID), err)
		}
		sources, err := listSourcesCacheFirst(ctx, scoped)
		if err != nil {
			return nil, fmt.Errorf("list sources for project %s: %w", adminProjectRecordKey(project.userID, project.projectID), err)
		}

		statistics[adminProjectRecordKey(project.userID, project.projectID)] = adminProjectStatistics{
			conceptCount: len(concepts),
			sourceCount:  len(sources),
		}
	}
	return statistics, nil
}

func adminProjectCountsByUser(projects []adminProjectRecord) map[string]int {
	counts := make(map[string]int)
	for _, project := range projects {
		counts[project.userID]++
	}
	return counts
}

func (h *Handler) listAdminProjectRecords(ctx context.Context) ([]adminProjectRecord, error) {
	if h.firestore == nil || h.firestore.Raw() == nil {
		return nil, errFirestoreNotConfigured
	}

	iter := h.firestore.Raw().Collection("projects").Documents(ctx)
	defer iter.Stop()

	projects := make([]adminProjectRecord, 0)
	for {
		doc, err := iter.Next()
		if err != nil {
			if status.Code(err) == codes.NotFound || errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		project, ok := adminProjectRecordFromFirestoreDoc(doc.Ref.ID, doc.Data())
		if !ok {
			continue
		}
		projects = append(projects, project)
	}
	return projects, nil
}

// AdminProjects handles GET /admin/projects.
//
//	@Summary		List all projects (admin)
//	@Description	Returns all projects from Firestore. Supports optional ?user_id query filter.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			user_id	query		string	false	"Filter by user ID"
//	@Success		200		{object}	map[string]any
//	@Failure		401		{object}	handler.ErrorResponse
//	@Failure		403		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/projects [get]
func (h *Handler) AdminProjects(c *gin.Context) {
	if (h.firestore == nil || h.firestore.Raw() == nil) && (h.adminProjectRecordsLoader == nil || h.adminProjectStatisticsLoader == nil) {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	var fs *firestore.Client
	if h.firestore != nil {
		fs = h.firestore.Raw()
	}
	ctx := c.Request.Context()
	filterUserID := strings.TrimSpace(c.Query("user_id"))

	rawProjects := make([]adminProjectRecord, 0)
	userIDs := make(map[string]bool)
	projectRecordsLoader := h.listAdminProjectRecords
	if h.adminProjectRecordsLoader != nil {
		projectRecordsLoader = h.adminProjectRecordsLoader
	}
	projectRecords, err := projectRecordsLoader(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "project list unavailable"})
		return
	}
	for _, project := range projectRecords {
		if filterUserID != "" && project.userID != filterUserID {
			continue
		}
		rawProjects = append(rawProjects, project)
		userIDs[project.userID] = true
	}

	statisticsLoader := loadAdminProjectStatistics
	if h.adminProjectStatisticsLoader != nil {
		statisticsLoader = h.adminProjectStatisticsLoader
	}
	statistics, err := statisticsLoader(ctx, h.store, rawProjects)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: projectStatisticsUnavailableMessage})
		return
	}

	// Batch-fetch user names and emails
	userMap := make(map[string]struct{ name, email string })
	for uid := range userIDs {
		if fs == nil {
			break
		}
		userDoc, err := fs.Collection("users").Doc(uid).Get(ctx)
		if err != nil {
			continue // user might be deleted
		}
		data := userDoc.Data()
		name, _ := data["name"].(string)
		email, _ := data["email"].(string)
		if name == "" && email != "" {
			if at := strings.Index(email, "@"); at > 0 {
				name = email[:at]
			}
		}
		userMap[uid] = struct{ name, email string }{name, email}
	}

	projects := make([]adminProjectEntry, 0, len(rawProjects))
	for _, rp := range rawProjects {
		u := userMap[rp.userID]
		stats := statistics[adminProjectRecordKey(rp.userID, rp.projectID)]
		projects = append(projects, adminProjectEntry{
			ID:           rp.id,
			Name:         rp.name,
			UserID:       rp.userID,
			UserName:     u.name,
			UserEmail:    u.email,
			ProjectID:    rp.projectID,
			ConceptCount: stats.conceptCount,
			SourceCount:  stats.sourceCount,
		})
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// AdminDeleteProject handles DELETE /admin/projects/{id}.
//
//	@Summary		Delete a project (admin)
//	@Description	Deletes a project: removes GCS data, Firestore project doc, and lock doc.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			id	path		string	true	"Project doc ID ({userID}_{projectID})"
//	@Success		200	{object}	map[string]any
//	@Failure		401	{object}	handler.ErrorResponse
//	@Failure		403	{object}	handler.ErrorResponse
//	@Failure		404	{object}	handler.ErrorResponse
//	@Failure		500	{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/projects/{id} [delete]
func (h *Handler) AdminDeleteProject(c *gin.Context) {
	docID := c.Param("id")
	uid, pid := splitProjectDocID(docID)
	if pid == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid project doc ID"})
		return
	}
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	fs := h.firestore.Raw()
	ctx := c.Request.Context()

	docRef := fs.Collection("projects").Doc(docID)
	dsnap, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "project unavailable"})
		return
	}

	data := dsnap.Data()
	name, _ := data["name"].(string)

	// Delete GCS data
	if h.store != nil {
		prefix := store.ProjectPrefixWithSlash(uid, pid)
		if err := deleteGCSPrefix(ctx, h.store, prefix); err != nil {
			log.Print("admin generated cleanup warning")
		}
	}

	// Delete lock doc
	lockRef := fs.Collection("locks").Doc(fmt.Sprintf("%s__%s", uid, pid))
	if _, err := lockRef.Delete(ctx); err != nil && status.Code(err) != codes.NotFound {
		log.Print("admin lock cleanup warning")
	}

	// Delete project doc
	if _, err := docRef.Delete(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "project delete unavailable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "deleted",
		"id":      docID,
		"user_id": uid,
		"name":    name,
	})
}

// AdminRenameProject handles PATCH /admin/projects/{id}.
//
//	@Summary		Rename a project (admin)
//	@Description	Updates the name field on a Firestore project document.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string	true	"Project doc ID ({userID}_{projectID})"
//	@Param			body	body		object{name=string}	true	"New project name"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	handler.ErrorResponse
//	@Failure		401		{object}	handler.ErrorResponse
//	@Failure		403		{object}	handler.ErrorResponse
//	@Failure		404		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/projects/{id} [patch]
func (h *Handler) AdminRenameProject(c *gin.Context) {
	docID := c.Param("id")
	uid, pid := splitProjectDocID(docID)
	if pid == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid project doc ID"})
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "name is required"})
		return
	}
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	fs := h.firestore.Raw()
	ctx := c.Request.Context()

	docRef := fs.Collection("projects").Doc(docID)
	_, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "project unavailable"})
		return
	}

	if _, err := docRef.Update(ctx, []firestore.Update{
		{Path: "name", Value: body.Name},
	}); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "project update unavailable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":      docID,
		"name":    body.Name,
		"user_id": uid,
	})
}

// AdminRebuildIndex handles POST /admin/projects/{id}/rebuild-index.
//
//	@Summary		Rebuild index for a project (admin)
//	@Description	Triggers an index rebuild for the specified project using GCS-scoped data.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			id	path		string	true	"Project doc ID ({userID}_{projectID})"
//	@Success		200	{object}	map[string]any
//	@Failure		400	{object}	handler.ErrorResponse
//	@Failure		401	{object}	handler.ErrorResponse
//	@Failure		403	{object}	handler.ErrorResponse
//	@Failure		404	{object}	handler.ErrorResponse
//	@Failure		409	{object}	handler.ErrorResponse
//	@Failure		500	{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/projects/{id}/rebuild-index [post]
func (h *Handler) AdminRebuildIndex(c *gin.Context) {
	docID := c.Param("id")
	uid, pid := splitProjectDocID(docID)
	if pid == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid project doc ID"})
		return
	}
	ctx := c.Request.Context()
	// Production verifies the project before touching storage. The injected
	// rebuild seam is a local/test-only authorization substitute.
	if h.rebuildIndex == nil || h.projectExists != nil {
		if err := h.verifyAdminProjectExists(ctx, docID); err != nil {
			if errors.Is(err, errFirestoreNotConfigured) {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
				return
			}
			if status.Code(err) == codes.NotFound {
				c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "project not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			return
		}
	}
	if h.store != nil {
		if guarded, ok := h.store.Scope(uid, pid).(store.GenerationAware); ok {
			exists, err := guarded.HasCurrentManifest(ctx)
			if err != nil {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated output unavailable"})
				return
			}
			if exists {
				c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "generated output is managed by the pipeline; run the pipeline"})
				return
			}
		}
	}

	// Use injected rebuildIndex if available
	if h.rebuildIndex != nil {
		next, err := h.rebuildIndex(ctx, uid, pid)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			return
		}
		h.invalidateCachesAfterRebuild(uid, pid)
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"entries": gin.H{
				"concept": len(next.Concept),
				"source":  len(next.Source),
			},
		})
		return
	}

	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "wiki storage is not configured"})
		return
	}
	fs := h.firestore.Raw()

	// Acquire lock
	if err := acquireRebuildIndexLock(ctx, fs, uid, pid, time.Now()); err != nil {
		if errors.Is(err, errRebuildIndexLocked) {
			c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "rebuild index already running"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	wikiStore := h.store.Scope(uid, pid)
	var releaseLegacy func() error
	if session, ok := wikiStore.(store.LegacyGenerationWriteSession); ok {
		locked, release, err := session.BeginLegacyGenerationWrite(ctx)
		if err != nil {
			cleanupErr := finishRebuild(nil, func() error { return releaseRebuildIndexLock(context.Background(), fs, uid, pid) })
			if cleanupErr != nil || errors.Is(err, store.ErrLeaseCleanup) {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
				return
			}
			if errors.Is(err, store.ErrGenerationManaged) {
				c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "generated output is managed by the pipeline; run the pipeline"})
			} else {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
			}
			return
		}
		releaseLegacy = func() error { return release(context.Background()) }
		wikiStore = locked
	}
	next, err := wikiindex.Rebuild(ctx, newWikiIndexStore(wikiStore))
	if err = finishRebuild(err, releaseLegacy, func() error { return releaseRebuildIndexLock(context.Background(), fs, uid, pid) }); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generated data unavailable"})
		return
	}

	h.invalidateCachesAfterRebuild(uid, pid)
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"entries": gin.H{
			"concept": len(next.Concept),
			"source":  len(next.Source),
		},
	})
}

// AdminPipelineTrigger handles POST /admin/projects/{id}/pipeline.
//
//	@Summary		Trigger pipeline + rebuild for a project (admin)
//	@Description	Invokes the Cloud Run worker job for the specified project, then rebuilds the search index.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			id	path		string	true	"Project doc ID ({userID}_{projectID})"
//	@Success		200	{object}	map[string]any
//	@Failure		400	{object}	handler.ErrorResponse
//	@Failure		401	{object}	handler.ErrorResponse
//	@Failure		403	{object}	handler.ErrorResponse
//	@Failure		404	{object}	handler.ErrorResponse
//	@Failure		409	{object}	handler.ErrorResponse
//	@Failure		500	{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/projects/{id}/pipeline [post]
func (h *Handler) AdminPipelineTrigger(c *gin.Context) {
	docID := c.Param("id")
	uid, pid := splitProjectDocID(docID)
	if pid == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid project doc ID"})
		return
	}
	ctx := c.Request.Context()
	if err := h.verifyAdminProjectExists(ctx, docID); err != nil {
		if errors.Is(err, errFirestoreNotConfigured) {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
			return
		}
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineUnavailableMessage})
		return
	}

	// Admin bypasses daily/cooldown/new-raw/demo quota but still blocks concurrent runs.
	alreadyRunning, err := h.isPipelineRunning(ctx, uid, pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineStatusUnavailableMessage})
		return
	}
	if alreadyRunning {
		c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "pipeline is already running"})
		return
	}

	executionID, err := h.invokePipelineJob(ctx, uid, pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: pipelineUnavailableMessage})
		return
	}
	log.Print("admin pipeline triggered")

	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"execution_id": executionID,
	})
}

// AdminListUsers handles GET /admin/users.
//
//	@Summary		List all users (admin)
//	@Description	Returns all users from Firestore. Supports optional ?role=admin query filter.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			role	query		string	false	"Filter by role (e.g. admin)"
//	@Success		200		{object}	map[string]any
//	@Failure		401		{object}	handler.ErrorResponse
//	@Failure		403		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/users [get]
func (h *Handler) AdminListUsers(c *gin.Context) {
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	fs := h.firestore.Raw()
	ctx := c.Request.Context()
	filterRole := strings.TrimSpace(c.Query("role"))
	projectRecords, err := h.listAdminProjectRecords(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "user project list unavailable"})
		return
	}
	projectCounts := adminProjectCountsByUser(projectRecords)

	var iter *firestore.DocumentIterator
	if filterRole != "" {
		iter = fs.Collection("users").Where("role", "==", filterRole).Documents(ctx)
	} else {
		iter = fs.Collection("users").Documents(ctx)
	}
	defer iter.Stop()

	users := make([]adminUserEntry, 0)
	for {
		doc, err := iter.Next()
		if err != nil {
			if status.Code(err) == codes.NotFound || errors.Is(err, iterator.Done) {
				break
			}
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "user list unavailable"})
			return
		}
		data := doc.Data()
		name, _ := data["name"].(string)
		email, _ := data["email"].(string)
		role, _ := data["role"].(string)
		// Fallback: derive display name from email if name is empty
		if name == "" && email != "" {
			if at := strings.Index(email, "@"); at > 0 {
				name = email[:at]
			} else {
				name = email
			}
		}
		users = append(users, adminUserEntry{
			ID:           doc.Ref.ID,
			Name:         name,
			Email:        email,
			Role:         role,
			ProjectCount: projectCounts[doc.Ref.ID],
		})
	}

	c.JSON(http.StatusOK, gin.H{"users": users})
}

// AdminUpdateUser handles PATCH /admin/users/{id}.
//
//	@Summary		Update a user (admin)
//	@Description	Updates a user's role in Firestore.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string	true	"User ID"
//	@Param			body	body		object{role=string}	true	"New role (e.g. admin)"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	handler.ErrorResponse
//	@Failure		401		{object}	handler.ErrorResponse
//	@Failure		403		{object}	handler.ErrorResponse
//	@Failure		404		{object}	handler.ErrorResponse
//	@Failure		500		{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/users/{id} [patch]
func (h *Handler) AdminUpdateUser(c *gin.Context) {
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	userID := c.Param("id")

	var body struct {
		Role string `json:"role"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Role) == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "role is required"})
		return
	}

	fs := h.firestore.Raw()
	ctx := c.Request.Context()

	docRef := fs.Collection("users").Doc(userID)
	_, err := docRef.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "user not found: " + userID})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "read user: " + err.Error()})
		return
	}

	if _, err := docRef.Update(ctx, []firestore.Update{
		{Path: "role", Value: body.Role},
	}); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "update user: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "updated",
		"id":     userID,
	})
}

// AdminDeleteUser handles DELETE /admin/users/{id}.
//
//	@Summary		Delete a user (admin)
//	@Description	Deletes a user and all their projects (GCS data + Firestore docs + locks).
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			id	path		string	true	"User ID"
//	@Success		200	{object}	map[string]any
//	@Failure		401	{object}	handler.ErrorResponse
//	@Failure		403	{object}	handler.ErrorResponse
//	@Failure		404	{object}	handler.ErrorResponse
//	@Failure		500	{object}	handler.ErrorResponse
//	@Security		BearerAuth
//	@Router			/api/v1/admin/users/{id} [delete]
func (h *Handler) AdminDeleteUser(c *gin.Context) {
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	userID := c.Param("id")
	fs := h.firestore.Raw()
	ctx := c.Request.Context()

	// Verify user exists
	_, err := fs.Collection("users").Doc(userID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "user not found: " + userID})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "read user: " + err.Error()})
		return
	}

	// Find and delete all projects belonging to this user
	iter := fs.Collection("projects").Documents(ctx)
	defer iter.Stop()

	for {
		doc, err := iter.Next()
		if err != nil {
			if status.Code(err) == codes.NotFound || errors.Is(err, iterator.Done) {
				break
			}
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "project list unavailable"})
			return
		}

		uid, pid := splitProjectDocID(doc.Ref.ID)
		if uid != userID {
			continue
		}

		// Delete GCS data
		if h.store != nil && pid != "" {
			prefix := store.ProjectPrefixWithSlash(userID, pid)
			if err := deleteGCSPrefix(ctx, h.store, prefix); err != nil {
				log.Print("admin generated cleanup warning")
			}
		}

		// Delete lock doc
		lockRef := fs.Collection("locks").Doc(fmt.Sprintf("%s__%s", userID, pid))
		if _, err := lockRef.Delete(ctx); err != nil && status.Code(err) != codes.NotFound {
			log.Print("admin lock cleanup warning")
		}

		// Delete project doc
		if _, err := doc.Ref.Delete(ctx); err != nil {
			log.Print("admin project delete warning")
		}
	}

	// Delete user doc
	if _, err := fs.Collection("users").Doc(userID).Delete(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "user delete unavailable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "deleted",
		"id":     userID,
	})
}

type gcsPrefixDeleter interface {
	DeletePrefix(context.Context, string) (int, error)
}

func deleteGCSPrefix(ctx context.Context, client any, prefix string) error {
	deleter, ok := client.(gcsPrefixDeleter)
	if !ok {
		return nil
	}
	_, err := deleter.DeletePrefix(ctx, prefix)
	return err
}
