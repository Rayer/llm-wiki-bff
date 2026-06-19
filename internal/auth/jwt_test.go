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

func performAuthenticatedRequest(t *testing.T, token string, cfg config.Config) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(JWTAuth(cfg))
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
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
