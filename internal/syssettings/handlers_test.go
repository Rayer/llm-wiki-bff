package syssettings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/auth"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestPublicConfig_NoAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &FakeStore{Enabled: false}
	router := gin.New()
	router.GET("/api/v1/public/config", PublicConfigHandler(store))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/config", nil)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body Settings
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.RegistrationEnabled)
	assert.Equal(t, 1, store.GetCalls)
}

func TestAdminGetSettings_RequiresAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &FakeStore{Enabled: true}
	router := gin.New()
	admin := router.Group("/api/v1/admin")
	admin.Use(auth.JWTAuth(config.Config{JWTSecret: "test-secret"}), auth.AdminOnly())
	admin.GET("/settings", AdminGetSettingsHandler(store))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	userToken, err := auth.GenerateAccessToken("user-1", "", "test-secret")
	assert.NoError(t, err)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	adminToken, err := auth.GenerateAccessToken("admin-1", "admin", "test-secret")
	assert.NoError(t, err)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body Settings
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.RegistrationEnabled)
}

func TestAdminPatchSettings_PersistsValue(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &FakeStore{Enabled: true}
	router := gin.New()
	admin := router.Group("/api/v1/admin")
	admin.Use(auth.JWTAuth(config.Config{JWTSecret: "test-secret"}), auth.AdminOnly())
	admin.PATCH("/settings", AdminPatchSettingsHandler(store))

	adminToken, err := auth.GenerateAccessToken("admin-1", "admin", "test-secret")
	assert.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/settings", strings.NewReader(`{"registration_enabled":false}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var body Settings
	assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.False(t, body.RegistrationEnabled)
	assert.Equal(t, 1, store.SetCalls)
	assert.NotNil(t, store.Persisted)
	assert.False(t, *store.Persisted)
}

func TestAnnouncementDraftPublishesWithoutLeakingDraft(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &FakeStore{Enabled: true, Published: "old", Draft: "old"}
	router := gin.New()
	router.GET("/api/v1/public/config", PublicConfigHandler(store))
	admin := router.Group("/api/v1/admin")
	admin.Use(auth.JWTAuth(config.Config{JWTSecret: "test-secret"}), auth.AdminOnly())
	admin.GET("/settings", AdminGetSettingsHandler(store))
	admin.PATCH("/settings", AdminPatchSettingsHandler(store))
	admin.POST("/settings/announcement/publish", AdminPublishAnnouncementHandler(store))

	getPublic := func() map[string]interface{} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/public/config", nil))
		var body map[string]interface{}
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body
	}

	adminToken, err := auth.GenerateAccessToken("admin-1", "admin", "test-secret")
	assert.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/settings", strings.NewReader(`{"announcement_draft_markdown":"new"}`))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "old", getPublic()["announcement_markdown"])

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/announcement/publish", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "new", getPublic()["announcement_markdown"])
}

func TestAnnouncementPatchRejectsOversizeAndPublishAllowsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &FakeStore{Enabled: true, Draft: "keep", Published: "visible"}
	router := gin.New()
	admin := router.Group("/api/v1/admin")
	admin.Use(auth.JWTAuth(config.Config{JWTSecret: "test-secret"}), auth.AdminOnly())
	admin.PATCH("/settings", AdminPatchSettingsHandler(store))
	admin.POST("/settings/announcement/publish", AdminPublishAnnouncementHandler(store))
	token, err := auth.GenerateAccessToken("admin-1", "admin", "test-secret")
	assert.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/settings", strings.NewReader(`{"announcement_draft_markdown":"`+strings.Repeat("x", maxMarkdownBytes+1)+`"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "keep", store.Draft)
	assert.Equal(t, "visible", store.Published)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/admin/settings", strings.NewReader(`{"announcement_draft_markdown":""}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/announcement/publish", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store.Published)
}
