package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProjectMiddlewareRejectsUnsafePathSegments(t *testing.T) {
	tests := []string{"", ".", "..", "../other-project", `project\other`}

	for _, project := range tests {
		t.Run(project, func(t *testing.T) {
			router := projectRouter(t)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Project-ID", project)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d for project %q", rec.Code, http.StatusBadRequest, project)
			}
		})
	}
}

func TestProjectMiddlewareUsesProjectHeader(t *testing.T) {
	router := projectRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/?project=legacy-project", nil)
	req.Header.Set("X-Project-ID", "header-project")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("X-Test-Project-ID"); got != "header-project" {
		t.Fatalf("projectID = %q, want %q", got, "header-project")
	}
}

func TestProjectMiddlewareRejectsLegacyQueryParameter(t *testing.T) {
	router := projectRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/?project=legacy-project", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func projectRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ProjectMiddleware())
	router.Any("/", func(c *gin.Context) {
		c.Header("X-Test-Project-ID", c.GetString("projectID"))
		c.Status(http.StatusNoContent)
	})
	return router
}
