package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ProjectMiddleware extracts the project identifier from the X-Project-ID header
// and sets it in the Gin context as "projectID".
// Returns 400 if the header is missing or invalid.
func ProjectMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		project := strings.TrimSpace(c.GetHeader("X-Project-ID"))
		if !ValidPathSegment(project) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid X-Project-ID header"})
			return
		}

		c.Set("projectID", project)
		c.Next()
	}
}
