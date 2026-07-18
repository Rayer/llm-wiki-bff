package v1

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

// Handler holds the dependencies for the V1 API.
type Handler struct {
	store     store.RootStore
	firestore *firestore.Client
	index     *search.Index
	cache     *conceptcache.Cache
	llm       *llm.Client
	expander  *llm.QueryExpander

	httpClient                   *http.Client
	metadataTokenURL             string
	cloudRunJobURL               string
	projectExists                func(context.Context, string) error
	rebuildIndex                 func(context.Context, string, string) (wikiindex.IDMap, error)
	adminProjectRecordsLoader    func(context.Context) ([]adminProjectRecord, error)
	adminProjectStatisticsLoader func(context.Context, store.RootStore, []adminProjectRecord) (map[string]adminProjectStatistics, error)
	idRoutingMu                  sync.Mutex
	idRoutingMaps                map[string]dualIDMap

	// Per-project list cache (invalidated on rebuild-index)
	listCacheMu   sync.RWMutex
	listCache     map[string]cachedLists // key: "uid_pid"
	listCacheKeys map[string]map[string]struct{}

	// Pipeline quota (LWC-138). Zero values mean defaults via pipelineLimits().
	pipelineDailyLimit int
	pipelineCooldown   time.Duration
	pipelineMinNewRaw  int
	demoUserIDs        map[string]struct{}

	// Optional injectable quota backend for tests; nil → use firestore when available.
	quotaStore pipelineQuotaStore
}

type cachedLists struct {
	concepts         []store.WikiPage
	sources          []store.WikiPage
	legacySourceMeta []store.WikiPage
}

const requestPinnedStoreKey = "lwc.requestPinnedStore"

// New creates a V1 Handler with the given dependencies.
func New(wikiStore store.RootStore, fs *firestore.Client, idx *search.Index, cache *conceptcache.Cache, llmClient *llm.Client, expander *llm.QueryExpander) *Handler {
	return &Handler{
		store:         wikiStore,
		firestore:     fs,
		index:         idx,
		cache:         cache,
		llm:           llmClient,
		expander:      expander,
		idRoutingMaps: make(map[string]dualIDMap),
		listCache:     make(map[string]cachedLists),
		listCacheKeys: make(map[string]map[string]struct{}),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
}

// SetRebuildIndexFunc overrides rebuild behavior for environments that do not
// use Firestore locks, such as local filesystem development mode.
func (h *Handler) SetRebuildIndexFunc(fn func(context.Context, string, string) (wikiindex.IDMap, error)) {
	h.rebuildIndex = fn
}

// SetPipelineJobURL configures the Cloud Run Job endpoint used by pipeline
// trigger and status paths. An empty value preserves the legacy target.
func (h *Handler) SetPipelineJobURL(jobURL string) {
	h.cloudRunJobURL = strings.TrimSpace(jobURL)
}

// SetPipelineQuotaConfig configures per-project pipeline rate limits and demo user IDs.
// Non-positive dailyLimit / cooldownSeconds / minNewRaw fall back to defaults (2 / 3600s / 1).
func (h *Handler) SetPipelineQuotaConfig(dailyLimit, cooldownSeconds, minNewRaw int, demoUserIDs []string) {
	if dailyLimit <= 0 {
		dailyLimit = 2
	}
	if cooldownSeconds <= 0 {
		cooldownSeconds = 3600
	}
	if minNewRaw <= 0 {
		minNewRaw = 1
	}
	h.pipelineDailyLimit = dailyLimit
	h.pipelineCooldown = time.Duration(cooldownSeconds) * time.Second
	h.pipelineMinNewRaw = minNewRaw

	demo := make(map[string]struct{}, len(demoUserIDs))
	for _, id := range demoUserIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			demo[id] = struct{}{}
		}
	}
	h.demoUserIDs = demo
}

// SetPipelineQuotaStore injects a quota backend (primarily for tests).
func (h *Handler) SetPipelineQuotaStore(store pipelineQuotaStore) {
	h.quotaStore = store
}

// GetStore returns the request-scoped wiki store.
func (h *Handler) GetStore(c *gin.Context) (store.Store, error) {
	if pinned, ok := c.Get(requestPinnedStoreKey); ok {
		if scoped, ok := pinned.(store.Store); ok {
			return scoped, nil
		}
	}
	userID := c.GetString("userID")
	projectID := c.GetString("projectID")
	if userID == "" && projectID == "" {
		return h.store, nil
	}
	if userID == "" || projectID == "" {
		return nil, fmt.Errorf("incomplete storage request scope")
	}
	if h.store == nil {
		return nil, errWikiStorageNotConfigured
	}
	scoped := h.store.Scope(userID, projectID)
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	var err error
	scoped, err = pinStore(ctx, scoped)
	if err != nil {
		return nil, err
	}
	c.Set(requestPinnedStoreKey, scoped)
	return scoped, nil
}

func pinStore(ctx context.Context, scoped store.Store) (store.Store, error) {
	if pinner, ok := scoped.(store.ViewPinner); ok {
		return pinner.Pin(ctx)
	}
	return scoped, nil
}

// GetGCSClient is kept as a compatibility wrapper while handlers migrate to
// storage-neutral naming.
func (h *Handler) GetGCSClient(c *gin.Context) (store.Store, error) {
	return h.GetStore(c)
}

// ── List cache helpers ──

func (h *Handler) listCacheGet(key string) cachedLists {
	h.listCacheMu.RLock()
	defer h.listCacheMu.RUnlock()
	return h.listCache[key]
}

func (h *Handler) listCacheSet(key string, fn func(*cachedLists)) {
	h.listCacheMu.Lock()
	defer h.listCacheMu.Unlock()
	cl := h.listCache[key]
	fn(&cl)
	h.listCache[key] = cl
	base, _, _ := strings.Cut(key, ":")
	if h.listCacheKeys == nil {
		h.listCacheKeys = make(map[string]map[string]struct{})
	}
	if h.listCacheKeys[base] == nil {
		h.listCacheKeys[base] = make(map[string]struct{})
	}
	h.listCacheKeys[base][key] = struct{}{}
}

func (h *Handler) listCacheInvalidateProject(uid, pid string) {
	base := store.ProjectPrefix(uid, pid)
	h.listCacheMu.Lock()
	defer h.listCacheMu.Unlock()
	for key := range h.listCacheKeys[base] {
		delete(h.listCache, key)
	}
	delete(h.listCacheKeys, base)
}

func (h *Handler) listCacheInvalidate(key string) {
	h.listCacheMu.Lock()
	defer h.listCacheMu.Unlock()
	delete(h.listCache, key)
}

func (h *Handler) idRoutingCacheInvalidateForProject(uid, pid string) {
	h.idRoutingMu.Lock()
	defer h.idRoutingMu.Unlock()
	prefix := store.ProjectPrefix(uid, pid)
	for key := range h.idRoutingMaps {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			delete(h.idRoutingMaps, key)
		}
	}
}

func viewCacheKey(s store.Store) string {
	key := s.Prefix()
	if view, ok := s.(store.ViewToken); ok {
		if token := view.ViewToken(); token != "" && token != "legacy" {
			return key + ":" + token
		}
	}
	return key
}

func (h *Handler) invalidateCachesAfterRebuild(uid, pid string) {
	h.idRoutingCacheInvalidateForProject(uid, pid)
	h.listCacheInvalidateProject(uid, pid)
}

func cloneWikiPages(src []store.WikiPage) []store.WikiPage {
	if src == nil {
		return nil
	}
	dst := make([]store.WikiPage, len(src))
	copy(dst, src)
	return dst
}
