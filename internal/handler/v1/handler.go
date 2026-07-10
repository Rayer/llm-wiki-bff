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

	httpClient       *http.Client
	metadataTokenURL string
	cloudRunJobURL   string
	projectExists    func(context.Context, string) error
	rebuildIndex     func(context.Context, string, string) (wikiindex.IDMap, error)
	idRoutingMu      sync.Mutex
	idRoutingMaps    map[string]dualIDMap

	// Per-project list cache (invalidated on rebuild-index)
	listCacheMu sync.RWMutex
	listCache   map[string]cachedLists // key: "uid_pid"

	// Pipeline quota (LWC-138). Zero values mean defaults via pipelineLimits().
	pipelineDailyLimit int
	pipelineCooldown   time.Duration
	pipelineMinNewRaw  int
	demoUserIDs        map[string]struct{}

	// Optional injectable quota backend for tests; nil → use firestore when available.
	quotaStore pipelineQuotaStore
}

type cachedLists struct {
	concepts []store.WikiPage
	sources  []store.WikiPage
}

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
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
}

// SetRebuildIndexFunc overrides rebuild behavior for environments that do not
// use Firestore locks, such as local filesystem development mode.
func (h *Handler) SetRebuildIndexFunc(fn func(context.Context, string, string) (wikiindex.IDMap, error)) {
	h.rebuildIndex = fn
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
	userID := c.GetString("userID")
	projectID := c.GetString("projectID")
	if userID == "" && projectID == "" {
		return h.store, nil
	}
	if userID == "" || projectID == "" {
		return nil, fmt.Errorf("incomplete storage request scope")
	}
	if h.store == nil {
		return nil, fmt.Errorf("wiki storage is not configured")
	}
	return h.store.Scope(userID, projectID), nil
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
}

func (h *Handler) listCacheInvalidate(key string) {
	h.listCacheMu.Lock()
	defer h.listCacheMu.Unlock()
	delete(h.listCache, key)
}

func (h *Handler) idRoutingCacheInvalidateForProject(uid, pid string) {
	h.idRoutingMu.Lock()
	defer h.idRoutingMu.Unlock()
	delete(h.idRoutingMaps, store.ProjectPrefix(uid, pid))
}

func (h *Handler) invalidateCachesAfterRebuild(uid, pid string) {
	h.idRoutingCacheInvalidateForProject(uid, pid)
	h.listCacheInvalidate(uid + "_" + pid)
}

func cloneWikiPages(src []store.WikiPage) []store.WikiPage {
	if src == nil {
		return nil
	}
	dst := make([]store.WikiPage, len(src))
	copy(dst, src)
	return dst
}
