package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/search"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

type sourceMetadataView struct{ listed []gcs.WikiPage }

func (s sourceMetadataView) ListSourcesFromCache(context.Context) ([]gcs.WikiPage, error) {
	return nil, nil
}
func (s sourceMetadataView) ListSources(context.Context) ([]gcs.WikiPage, error) {
	return append([]gcs.WikiPage(nil), s.listed...), nil
}

func TestLegacySourceMetadataCacheIsPinnedToView(t *testing.T) {
	h := New(nil, nil, search.NewIndex(), nil, nil, nil)
	ctx := context.Background()
	aKey := store.ProjectPrefix("user", "project") + ":manifest-a"
	bKey := store.ProjectPrefix("user", "project") + ":manifest-b"
	aSources := []gcs.WikiPage{{Slug: "source"}}
	bSources := []gcs.WikiPage{{Slug: "source"}}
	if err := h.hydrateLegacySourceMetadata(ctx, sourceMetadataView{listed: []gcs.WikiPage{{Slug: "source", RawPath: "raw/a.md", Title: "A"}}}, aSources, aKey); err != nil {
		t.Fatal(err)
	}
	if err := h.hydrateLegacySourceMetadata(ctx, sourceMetadataView{listed: []gcs.WikiPage{{Slug: "source", RawPath: "raw/b.md", Title: "B"}}}, bSources, bKey); err != nil {
		t.Fatal(err)
	}
	if aSources[0].RawPath != "raw/a.md" || bSources[0].RawPath != "raw/b.md" {
		t.Fatalf("mixed view metadata: A=%+v B=%+v", aSources[0], bSources[0])
	}
	stillA := []gcs.WikiPage{{Slug: "source"}}
	if err := h.hydrateLegacySourceMetadata(ctx, sourceMetadataView{listed: []gcs.WikiPage{{Slug: "source", RawPath: "raw/incorrect.md"}}}, stillA, aKey); err != nil {
		t.Fatal(err)
	}
	if stillA[0].RawPath != "raw/a.md" {
		t.Fatalf("pinned A changed after B: %+v", stillA[0])
	}
	h.invalidateCachesAfterRebuild("user", "project")
	if _, ok := h.listCache[aKey]; ok {
		t.Fatal("A view cache survived project invalidation")
	}
	if _, ok := h.listCache[bKey]; ok {
		t.Fatal("B view cache survived project invalidation")
	}
}

func seedRebuildCaches(h *Handler, uid, pid string) {
	h.idRoutingMaps = map[string]dualIDMap{
		store.ProjectPrefix(uid, pid): buildDualIDMap(idMap{
			Concept: map[string]string{"a3f7b2c01d9d": "stale-slug"},
		}),
		"users/other-user/projects/other": buildDualIDMap(idMap{
			Concept: map[string]string{"b7e2c9a4d113": "other-slug"},
		}),
	}
	projectKey := store.ProjectPrefix(uid, pid) + ":manifest-7"
	h.listCache = map[string]cachedLists{
		projectKey: {
			concepts: []store.WikiPage{{Slug: "stale-slug"}},
		},
		"other-user_other": {
			concepts: []store.WikiPage{{Slug: "other-slug"}},
		},
	}
	h.listCacheKeys = map[string]map[string]struct{}{store.ProjectPrefix(uid, pid): {projectKey: {}}}
}

func assertProjectCachesCleared(t *testing.T, h *Handler, uid, pid string) {
	t.Helper()
	if _, ok := h.idRoutingMaps[store.ProjectPrefix(uid, pid)]; ok {
		t.Fatalf("idRoutingMaps still has key %q", store.ProjectPrefix(uid, pid))
	}
	if _, ok := h.listCache[store.ProjectPrefix(uid, pid)+":manifest-7"]; ok {
		t.Fatalf("listCache still has project view key")
	}
}

func assertOtherProjectCachesPreserved(t *testing.T, h *Handler) {
	t.Helper()
	if _, ok := h.idRoutingMaps["users/other-user/projects/other"]; !ok {
		t.Fatalf("idRoutingMaps lost unrelated project cache: %#v", h.idRoutingMaps)
	}
	if _, ok := h.listCache["other-user_other"]; !ok {
		t.Fatalf("listCache lost unrelated project cache: %#v", h.listCache)
	}
}

func TestRebuildIndexInvalidatesCaches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		uid = "request-user"
		pid = "demo"
	)
	h := &Handler{
		index:         search.NewIndex(),
		idRoutingMaps: make(map[string]dualIDMap),
		listCache:     make(map[string]cachedLists),
		rebuildIndex: func(context.Context, string, string) (idMap, error) {
			return idMap{
				Concept: map[string]string{"a3f7b2c01d9d": "canonical-slug"},
			}, nil
		},
	}
	seedRebuildCaches(h, uid, pid)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/rebuild-index", nil)
	c.Set("userID", uid)
	c.Set("projectID", pid)

	h.RebuildIndex(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertProjectCachesCleared(t, h, uid, pid)
	assertOtherProjectCachesPreserved(t, h)
}

func TestAdminRebuildIndexInvalidatesCaches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		uid   = "request-user"
		pid   = "demo"
		docID = uid + "_" + pid
	)
	h := &Handler{
		index:         search.NewIndex(),
		idRoutingMaps: make(map[string]dualIDMap),
		listCache:     make(map[string]cachedLists),
		projectExists: func(context.Context, string) error {
			return nil
		},
		rebuildIndex: func(context.Context, string, string) (idMap, error) {
			return idMap{
				Concept: map[string]string{"a3f7b2c01d9d": "canonical-slug"},
			}, nil
		},
	}
	seedRebuildCaches(h, uid, pid)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects/"+docID+"/rebuild-index", nil)
	c.Params = gin.Params{{Key: "id", Value: docID}}

	h.AdminRebuildIndex(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	assertProjectCachesCleared(t, h, uid, pid)
	assertOtherProjectCachesPreserved(t, h)
}
