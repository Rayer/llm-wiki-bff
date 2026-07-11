package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/auth"
	"github.com/rayer/llm-wiki-bff/internal/config"
	handlerv1 "github.com/rayer/llm-wiki-bff/internal/handler/v1"
	"github.com/rayer/llm-wiki-bff/internal/middleware"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
)

func TestSecurityHeadersMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.SecurityHeaders(true))
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	headers := rec.Header()
	want := map[string]string{
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
	}
	for header, value := range want {
		if got := headers.Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestSecurityHeadersMiddlewareSkipsHSTSWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.SecurityHeaders(false))
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("Strict-Transport-Security = %q, want empty when HSTS disabled", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

func TestAuthRateLimitBlocksAfterLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/login", middleware.NewRateLimiter(10, time.Minute), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	rateLimited := false
	for i := 0; i < 15; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.Header.Set("CF-Connecting-IP", "203.0.113.20")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			rateLimited = true
			break
		}
	}
	if !rateLimited {
		t.Fatal("expected 429 after repeated requests, never blocked")
	}
}

func TestAdminProjectsRouteRequiresAdminRole(t *testing.T) {
	router := newAdminRouteTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/projects", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	userToken, err := auth.GenerateAccessToken("user-123", "", "test-secret")
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/projects", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAdminProjectsRouteDoesNotRequireProjectHeader(t *testing.T) {
	router := newAdminRouteTestRouter(t)
	adminToken, err := auth.GenerateAccessToken("admin-user", "admin", "test-secret")
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/projects", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("admin route was blocked by project middleware: %s", rec.Body.String())
	}
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("admin route status = %d, want registered route after admin auth", rec.Code)
	}
}

func TestAdminSettingsRouteRequiresAdminRole(t *testing.T) {
	router := newAdminRouteTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	userToken, err := auth.GenerateAccessToken("user-123", "", "test-secret")
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func newAdminRouteTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	handler := handlerv1.New(nil, nil, nil, nil, nil, nil)
	settingsStore := &syssettings.FakeStore{Enabled: true}
	registerAdminRoutes(router, config.Config{JWTSecret: "test-secret"}, handler, settingsStore)
	return router
}
