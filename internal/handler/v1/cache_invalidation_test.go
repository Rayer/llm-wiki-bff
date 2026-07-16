package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/search"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func seedRebuildCaches(h *Handler, uid, pid string) {
	h.idRoutingMaps = map[string]dualIDMap{
		store.ProjectPrefix(uid, pid): buildDualIDMap(idMap{
			Concept: map[string]string{"a3f7b2c01d9d": "stale-slug"},
		}),
		"users/other-user/projects/other": buildDualIDMap(idMap{
			Concept: map[string]string{"b7e2c9a4d113": "other-slug"},
		}),
	}
	h.listCache = map[string]cachedLists{
		uid + "_" + pid: {
			concepts: []store.WikiPage{{Slug: "stale-slug"}},
		},
		"other-user_other": {
			concepts: []store.WikiPage{{Slug: "other-slug"}},
		},
	}
}

func assertProjectCachesCleared(t *testing.T, h *Handler, uid, pid string) {
	t.Helper()
	if _, ok := h.idRoutingMaps[store.ProjectPrefix(uid, pid)]; ok {
		t.Fatalf("idRoutingMaps still has key %q", store.ProjectPrefix(uid, pid))
	}
	if _, ok := h.listCache[uid+"_"+pid]; ok {
		t.Fatalf("listCache still has key %q", uid+"_"+pid)
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