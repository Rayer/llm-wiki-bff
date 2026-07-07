package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

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
