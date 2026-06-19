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
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(ProjectMiddleware())
			router.GET("/", func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodGet, "/?project="+project, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d for project %q", rec.Code, http.StatusBadRequest, project)
			}
		})
	}
}
