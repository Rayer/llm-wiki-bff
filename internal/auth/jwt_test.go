package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/config"
)

func TestJWTAuthRejectsEmptySubject(t *testing.T) {
	token, err := GenerateToken("", "test-secret", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	rec := performAuthenticatedRequest(t, token, config.Config{JWTSecret: "test-secret"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertJSONError(t, rec, "invalid token")
}

func TestJWTAuthRejectsUnsafeSubject(t *testing.T) {
	token, err := GenerateToken("../other-user", "test-secret", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	rec := performAuthenticatedRequest(t, token, config.Config{JWTSecret: "test-secret"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertJSONError(t, rec, "invalid token")
}

func TestJWTAuthDoesNotExposeParserErrors(t *testing.T) {
	rec := performAuthenticatedRequest(t, "not-a-jwt", config.Config{JWTSecret: "test-secret"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	assertJSONError(t, rec, "invalid token")
	if strings.Contains(rec.Body.String(), "token is malformed") {
		t.Fatalf("response leaked parser details: %s", rec.Body.String())
	}
}

func TestGenerateAccessTokenIncludesRole(t *testing.T) {
	token, err := GenerateAccessToken("admin-user", "admin", "test-secret")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	claims, err := ValidateToken(token, "test-secret")
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if claims.Role != "admin" {
		t.Fatalf("role = %q, want %q", claims.Role, "admin")
	}
}

func TestJWTAuthSetsUserRoleFromClaims(t *testing.T) {
	token, err := GenerateAccessToken("admin-user", "admin", "test-secret")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	rec := performAuthenticatedRequestWithHandler(t, token, config.Config{JWTSecret: "test-secret"}, func(c *gin.Context) {
		if got := c.GetString("userRole"); got != "admin" {
			t.Fatalf("userRole = %q, want %q", got, "admin")
		}
		c.Status(http.StatusNoContent)
	})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestJWTAuthDevJWTSetsUserRoleFromHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(JWTAuth(config.Config{DevJWT: true}))
	router.GET("/", func(c *gin.Context) {
		if got := c.GetString("userRole"); got != "admin" {
			t.Fatalf("userRole = %q, want %q", got, "admin")
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-ID", "dev-user")
	req.Header.Set("X-User-Role", "admin")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestAdminOnlyAllowsAdminRole(t *testing.T) {
	rec := performAdminOnlyRequest(func(c *gin.Context) {
		c.Set("userRole", "admin")
	})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestAdminOnlyRejectsMissingRole(t *testing.T) {
	rec := performAdminOnlyRequest(nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	assertJSONError(t, rec, "admin role required")
}

func performAuthenticatedRequest(t *testing.T, token string, cfg config.Config) *httptest.ResponseRecorder {
	return performAuthenticatedRequestWithHandler(t, token, cfg, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
}

func performAuthenticatedRequestWithHandler(t *testing.T, token string, cfg config.Config, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(JWTAuth(cfg))
	router.GET("/", handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func performAdminOnlyRequest(setup gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	if setup != nil {
		router.Use(setup)
	}
	router.Use(AdminOnly())
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != want {
		t.Fatalf("error = %q, want %q", body["error"], want)
	}
}
