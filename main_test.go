package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/auth"
	"github.com/rayer/llm-wiki-bff/internal/config"
	handlerv1 "github.com/rayer/llm-wiki-bff/internal/handler/v1"
	"github.com/rayer/llm-wiki-bff/internal/middleware"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
)

func TestSwaggerDocumentsPublicAuthRoutes(t *testing.T) {
	document := readSwaggerDocument(t)
	for path := range registeredAuthPOSTRoutes(t) {
		operations, ok := document.Paths[path]
		if !ok {
			t.Errorf("Swagger document is missing registered auth route %s", path)
			continue
		}
		if _, ok := operations["post"]; !ok {
			t.Errorf("Swagger document is missing POST operation for registered auth route %s", path)
		}
	}
}

func TestSwaggerDocumentsAuthRateLimitResponses(t *testing.T) {
	document := readSwaggerDocument(t)
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/register"} {
		var operation struct {
			Responses map[string]struct {
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
				Headers map[string]json.RawMessage `json:"headers"`
			} `json:"responses"`
		}
		if err := json.Unmarshal(document.Paths[path]["post"], &operation); err != nil {
			t.Fatalf("decode POST operation for %s: %v", path, err)
		}
		response, ok := operation.Responses["429"]
		if !ok {
			t.Errorf("Swagger document is missing 429 rate-limit response for %s", path)
			continue
		}
		if response.Schema.Ref != "#/definitions/auth.RateLimitErrorResponse" {
			t.Errorf("429 schema for %s = %q, want rate-limit error response", path, response.Schema.Ref)
		}
		if _, ok := response.Headers["Retry-After"]; !ok {
			t.Errorf("Swagger document is missing Retry-After header for %s 429 response", path)
		}
	}
}

func TestSwaggerDocumentsRegisterResponseDescription(t *testing.T) {
	document := readSwaggerDocument(t)
	var operation struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(document.Paths["/api/v1/auth/register"]["post"], &operation); err != nil {
		t.Fatalf("decode register POST operation: %v", err)
	}
	if operation.Description != "Returns a JWT in the response body and does not set a refresh cookie." {
		t.Errorf("register description = %q, want JWT response body and no refresh cookie only", operation.Description)
	}
}

func TestSwaggerDocumentsPublicVersionRoute(t *testing.T) {
	document := readSwaggerDocument(t)
	operation, ok := document.Paths["/api/v1/public/version"]
	if !ok {
		t.Fatal("Swagger document is missing /api/v1/public/version")
	}
	get, ok := operation["get"]
	if !ok {
		t.Fatal("Swagger document is missing GET /api/v1/public/version")
	}
	var response struct {
		Responses map[string]struct {
			Schema struct {
				Ref string `json:"$ref"`
			} `json:"schema"`
			Headers map[string]json.RawMessage `json:"headers"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(get, &response); err != nil {
		t.Fatalf("decode version operation: %v", err)
	}
	status, ok := response.Responses["200"]
	if !ok {
		t.Fatal("Swagger document is missing 200 response for public version")
	}
	if status.Schema.Ref != "#/definitions/buildinfo.Info" {
		t.Fatalf("public version response = %q, want buildinfo.Info", status.Schema.Ref)
	}
	if _, ok := status.Headers["Cache-Control"]; !ok {
		t.Fatal("Swagger document is missing Cache-Control response header for public version")
	}
	var definition struct {
		Definitions map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	data, err := os.ReadFile("docs/swagger.json")
	if err != nil {
		t.Fatalf("read generated Swagger document: %v", err)
	}
	if err := json.Unmarshal(data, &definition); err != nil {
		t.Fatalf("decode generated Swagger definitions: %v", err)
	}
	properties := definition.Definitions["buildinfo.Info"].Properties
	if _, ok := properties["branch"]; !ok {
		t.Fatal("public version Swagger schema is missing branch")
	}
	if _, ok := properties["ref"]; ok {
		t.Fatal("public version Swagger schema must not expose generic ref")
	}
}

func TestPublicVersionRouteDoesNotRequireAuthOrProject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")
	v1.Use(auth.JWTAuth(config.Config{JWTSecret: "test-secret"}), auth.ProjectMiddleware())
	registerPublicRoutes(router, &syssettings.FakeStore{Enabled: true})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public version status = %d, want %d without authentication or project header", recorder.Code, http.StatusOK)
	}
}

func readSwaggerDocument(t *testing.T) struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
} {
	t.Helper()
	data, err := os.ReadFile("docs/swagger.json")
	if err != nil {
		t.Fatalf("read generated Swagger document: %v", err)
	}
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode generated Swagger document: %v", err)
	}
	return document
}

func registeredAuthPOSTRoutes(t *testing.T) map[string]struct{} {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse route registration: %v", err)
	}

	routes := make(map[string]struct{})
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != http.MethodPost {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "authRoutes" {
			return true
		}
		path, ok := call.Args[0].(*ast.BasicLit)
		if !ok || path.Kind != token.STRING {
			t.Errorf("auth route registration does not use a string path")
			return true
		}
		route, err := strconv.Unquote(path.Value)
		if err != nil {
			t.Errorf("unquote auth route %q: %v", path.Value, err)
			return true
		}
		routes["/api/v1/auth"+route] = struct{}{}
		return true
	})
	if len(routes) == 0 {
		t.Fatal("no auth POST routes found in main.go")
	}
	return routes
}

func TestObservabilityServiceNameUsesCloudRunService(t *testing.T) {
	tests := []struct {
		name     string
		kService string
		want     string
	}{
		{name: "production", kService: "llm-wiki-bff", want: "llm-wiki-bff"},
		{name: "development", kService: "llm-wiki-bff-dev", want: "llm-wiki-bff-dev"},
		{name: "local fallback", want: "llm-wiki-bff-dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := observabilityServiceName(tt.kService); got != tt.want {
				t.Fatalf("observabilityServiceName(%q) = %q, want %q", tt.kService, got, tt.want)
			}
		})
	}
}

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
