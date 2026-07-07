package v1

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

// Handler holds the dependencies for the V1 API.
type Handler struct {
	gcs       *gcs.Client
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
}

type cachedLists struct {
	concepts []gcs.WikiPage
	sources  []gcs.WikiPage
}

// New creates a V1 Handler with the given dependencies.
func New(gcsClient *gcs.Client, fs *firestore.Client, idx *search.Index, cache *conceptcache.Cache, llmClient *llm.Client, expander *llm.QueryExpander) *Handler {
	return &Handler{
		gcs:           gcsClient,
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

// GetGCSClient returns the request-scoped GCS client.
func (h *Handler) GetGCSClient(c *gin.Context) (*gcs.Client, error) {
	userID := c.GetString("userID")
	projectID := c.GetString("projectID")
	if userID == "" && projectID == "" {
		return h.gcs, nil
	}
	if userID == "" || projectID == "" {
		return nil, fmt.Errorf("incomplete GCS request scope")
	}
	if h.gcs == nil {
		return nil, fmt.Errorf("GCS client is not configured")
	}
	return h.gcs.WithScope(userID, projectID), nil
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

func cloneWikiPages(src []gcs.WikiPage) []gcs.WikiPage {
	if src == nil {
		return nil
	}
	dst := make([]gcs.WikiPage, len(src))
	copy(dst, src)
	return dst
}
