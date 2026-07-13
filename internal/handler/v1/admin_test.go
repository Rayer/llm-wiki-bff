package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/stretchr/testify/assert"
)

type adminStatsProjectStore struct {
	prefix          string
	concepts        []store.WikiPage
	sources         []store.WikiPage
	cacheConcepts   []store.WikiPage
	cacheSources    []store.WikiPage
	conceptErr      error
	sourceErr       error
	cacheConceptErr error
	cacheSourceErr  error
}

func (s *adminStatsProjectStore) Prefix() string { return s.prefix }

func (s *adminStatsProjectStore) ReadFile(context.Context, string) ([]byte, error) {
	return nil, storage.ErrObjectNotExist
}

func (s *adminStatsProjectStore) WriteBytes(context.Context, []byte, string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *adminStatsProjectStore) WriteBytesAtomic(context.Context, []byte, string, string) (string, error) {
	return "", errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListProjects(context.Context, string) ([]store.Project, error) {
	return nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListConcepts(context.Context, bool) ([]store.WikiPage, error) {
	return s.concepts, s.conceptErr
}

func (s *adminStatsProjectStore) ListSources(context.Context) ([]store.WikiPage, error) {
	return s.sources, s.sourceErr
}

func (s *adminStatsProjectStore) ListConceptsFromCache(context.Context) ([]store.WikiPage, error) {
	return s.cacheConcepts, s.cacheConceptErr
}

func (s *adminStatsProjectStore) ListSourcesFromCache(context.Context) ([]store.WikiPage, error) {
	return s.cacheSources, s.cacheSourceErr
}

func (s *adminStatsProjectStore) GetPage(context.Context, string, string) (*store.WikiPage, []byte, error) {
	return nil, nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListMarkdownFiles(context.Context, string) ([]store.MarkdownFile, error) {
	return nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) ListRawFiles(context.Context) ([]store.RawFile, error) {
	return nil, errors.New("not implemented")
}

func (s *adminStatsProjectStore) BucketStats(context.Context) (int64, int64, error) {
	return 0, 0, errors.New("not implemented")
}

func (s *adminStatsProjectStore) GetMetaSHA256(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}

type adminStatsRootStore struct {
	*adminStatsProjectStore
	scoped map[string]*adminStatsProjectStore
}

func (s *adminStatsRootStore) Scope(userID, projectID string) store.Store {
	return s.scoped[userID+"/"+projectID]
}

func TestLoadAdminProjectStatisticsUsesScopedCacheAndFallback(t *testing.T) {
	root := &adminStatsRootStore{
		adminStatsProjectStore: &adminStatsProjectStore{prefix: "root"},
		scoped: map[string]*adminStatsProjectStore{
			"user-a/project-a": {
				prefix:        "users/user-a/projects/project-a",
				cacheConcepts: []store.WikiPage{{Slug: "a-one"}, {Slug: "a-two"}},
				cacheSources:  []store.WikiPage{{Slug: "a-source"}},
			},
			"user-a/project-b": {
				prefix:        "users/user-a/projects/project-b",
				cacheConcepts: []store.WikiPage{{Slug: "b-one"}},
				cacheSources:  []store.WikiPage{{Slug: "b-one"}, {Slug: "b-two"}},
			},
			"user-b/empty": {
				prefix:        "users/user-b/projects/empty",
				cacheConcepts: []store.WikiPage{},
				cacheSources:  []store.WikiPage{},
			},
			"user-b/legacy": {
				prefix:          "users/user-b/projects/legacy",
				concepts:        []store.WikiPage{{Slug: "legacy-one"}, {Slug: "legacy-two"}, {Slug: "legacy-three"}},
				sources:         []store.WikiPage{{Slug: "legacy-source"}},
				cacheConceptErr: storage.ErrObjectNotExist,
				cacheSourceErr:  storage.ErrObjectNotExist,
			},
		},
	}

	got, err := loadAdminProjectStatistics(context.Background(), root, []adminProjectRecord{
		{userID: "user-a", projectID: "project-a"},
		{userID: "user-a", projectID: "project-b"},
		{userID: "user-b", projectID: "empty"},
		{userID: "user-b", projectID: "legacy"},
	})
	if err != nil {
		t.Fatalf("loadAdminProjectStatistics() error = %v", err)
	}

	assert.Equal(t, adminProjectStatistics{conceptCount: 2, sourceCount: 1}, got["user-a/project-a"])
	assert.Equal(t, adminProjectStatistics{conceptCount: 1, sourceCount: 2}, got["user-a/project-b"])
	assert.Equal(t, adminProjectStatistics{conceptCount: 0, sourceCount: 0}, got["user-b/empty"])
	assert.Equal(t, adminProjectStatistics{conceptCount: 3, sourceCount: 1}, got["user-b/legacy"])
}

func TestAdminProjectCountsByUserUsesActualOwnership(t *testing.T) {
	got := adminProjectCountsByUser([]adminProjectRecord{
		{userID: "user-a", projectID: "project-a"},
		{userID: "user-a", projectID: "project-b"},
		{userID: "user-b", projectID: "project-c"},
	})

	assert.Equal(t, 2, got["user-a"])
	assert.Equal(t, 1, got["user-b"])
	assert.Equal(t, 0, got["user-c"])
}

func TestAdminProjectRecordFromFirestoreDocUsesStoredProjectID(t *testing.T) {
	project, ok := adminProjectRecordFromFirestoreDoc("user-a_doc-suffix", map[string]interface{}{
		"project_id": "authoritative-project",
		"name":       "Authoritative Project",
	})
	if !ok {
		t.Fatal("adminProjectRecordFromFirestoreDoc returned ok=false")
	}
	assert.Equal(t, "user-a", project.userID)
	assert.Equal(t, "authoritative-project", project.projectID)
	assert.Equal(t, "Authoritative Project", project.name)
}

func TestAdminProjectRecordFromFirestoreDocRejectsMalformedProject(t *testing.T) {
	if _, ok := adminProjectRecordFromFirestoreDoc("malformed", map[string]interface{}{
		"name": "Malformed",
	}); ok {
		t.Fatal("malformed project document must not be counted")
	}
}

func TestAdminProjectRecordFromFirestoreDocRejectsUnscopedProjectID(t *testing.T) {
	if _, ok := adminProjectRecordFromFirestoreDoc("malformed", map[string]interface{}{
		"project_id": "authoritative-project",
		"name":       "Malformed",
	}); ok {
		t.Fatal("project document without an owner prefix must not be counted")
	}
}

func TestAdminProjectRecordFromFirestoreDocRejectsUnsafeStorageSegments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		docID string
		data  map[string]interface{}
	}{
		{
			name:  "unsafe owner",
			docID: "user/../other_project",
			data:  map[string]interface{}{"project_id": "project"},
		},
		{
			name:  "unsafe project",
			docID: "user-long_project",
			data:  map[string]interface{}{"project_id": "../other"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := adminProjectRecordFromFirestoreDoc(tc.docID, tc.data); ok {
				t.Fatal("unsafe storage segment must not be counted")
			}
		})
	}
}

func TestAdminStatisticsJSONFieldContract(t *testing.T) {
	projectJSON, err := json.Marshal(adminProjectEntry{
		ID:           "user-a_project-a",
		ConceptCount: 2,
		SourceCount:  1,
	})
	if err != nil {
		t.Fatalf("marshal project entry: %v", err)
	}
	userJSON, err := json.Marshal(adminUserEntry{ID: "user-a", ProjectCount: 2})
	if err != nil {
		t.Fatalf("marshal user entry: %v", err)
	}

	var project map[string]interface{}
	var user map[string]interface{}
	if err := json.Unmarshal(projectJSON, &project); err != nil {
		t.Fatalf("decode project JSON: %v", err)
	}
	if err := json.Unmarshal(userJSON, &user); err != nil {
		t.Fatalf("decode user JSON: %v", err)
	}
	assert.Equal(t, float64(2), project["concept_count"])
	assert.Equal(t, float64(1), project["source_count"])
	assert.Equal(t, float64(2), user["project_count"])
}

// =========================================================================
// AdminRenameProject tests
// =========================================================================

// TestAdminRenameProject_ValidRename_RouteLevel tests that a valid PATCH
// request with a project ID and name body returns 200 at the route level.
// This test uses a nil Handler so the handler returns 500 (firestore not
// configured); it primarily validates Gin routing and content-type handling.
func TestAdminRenameProject_ValidRoute_RouteLevel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.PATCH("/admin/projects/:id", h.AdminRenameProject)

	body := `{"name":"New Project Name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/admin/projects/user-1_proj-123", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// With nil firestore the handler returns 500, but the route matches
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// TestAdminRenameProject_MissingProjectID tests that an empty project ID
// returns 400 Bad Request.
func TestAdminRenameProject_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"name":"Some Name"}`
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "invalid project doc ID", resp["error"])
}

// TestAdminRenameProject_EmptyName tests that an empty name in the request
// body returns 400 Bad Request with "name is required".
func TestAdminRenameProject_EmptyName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"name":""}`
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/user-1_proj-123", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "name is required", resp["error"])
}

// TestAdminRenameProject_InvalidJSON tests that an invalid JSON body returns
// 400 Bad Request with an "invalid JSON" error message.
func TestAdminRenameProject_InvalidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/user-1_proj-123", strings.NewReader(`not json`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Contains(t, resp["error"], "invalid JSON")
}

// TestAdminRenameProject_NoFirestore tests that a nil firestore client
// returns 500 Internal Server Error.
func TestAdminRenameProject_NoFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{} // firestore is nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"name":"Valid Name"}`
	c.Request = httptest.NewRequest(http.MethodPatch, "/admin/projects/user-1_proj-123", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRenameProject(c)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "Firestore client is not configured", resp["error"])
}

// TestAdminRenameProject_MissingProject_Route404 tests that a PATCH request
// without a project ID in the URL returns 404 (Gin route mismatch).
func TestAdminRenameProject_MissingProject_Route404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.PATCH("/admin/projects/:id", h.AdminRenameProject)

	body := `{"name":"Some Name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PATCH", "/admin/projects/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =========================================================================
// AdminDeleteProject tests
// =========================================================================

// TestAdminDeleteProject_MissingProjectID tests that an empty project ID
// returns 400 Bad Request.
func TestAdminDeleteProject_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/admin/projects/", nil)

	h.AdminDeleteProject(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "invalid project doc ID", resp["error"])
}

// TestAdminDeleteProject_NoFirestore tests that a nil firestore client
// returns 500 Internal Server Error.
func TestAdminDeleteProject_NoFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{} // firestore is nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/admin/projects/user-1_proj-123", nil)
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminDeleteProject(c)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "Firestore client is not configured", resp["error"])
}

// TestAdminDeleteProject_MissingProject_Route404 tests that a DELETE request
// without a project ID in the URL returns 404 (Gin route mismatch).
func TestAdminDeleteProject_MissingProject_Route404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.DELETE("/admin/projects/:id", h.AdminDeleteProject)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/admin/projects/", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// =========================================================================
// AdminRebuildIndex tests
// =========================================================================

// TestAdminRebuildIndex_MissingProjectID tests that an empty project ID
// returns 400 Bad Request.
func TestAdminRebuildIndex_MissingProjectID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects//rebuild-index", nil)

	h.AdminRebuildIndex(c)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "invalid project doc ID", resp["error"])
}

// TestAdminRebuildIndex_NoFirestore tests that a nil firestore client
// returns 500 Internal Server Error.
func TestAdminRebuildIndex_NoFirestore(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{} // firestore is nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/projects/user-1_proj-123/rebuild-index", nil)
	c.Params = gin.Params{{Key: "id", Value: "user-1_proj-123"}}

	h.AdminRebuildIndex(c)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp map[string]string
	err := json.Unmarshal(recorder.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, "Firestore client is not configured", resp["error"])
}

// TestAdminRebuildIndex_MissingProject_Route404 tests that a POST request
// without a project ID in the URL returns 404 (Gin route mismatch).
func TestAdminRebuildIndex_MissingProject_Route404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	h := &Handler{}
	r.POST("/admin/projects/:id/rebuild-index", h.AdminRebuildIndex)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/admin/projects/", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
