package auth

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ProjectBinding is used to extract "project" from JSON POST bodies.
type ProjectBinding struct {
	Project string `json:"project"`
}

// ProjectMiddleware extracts the "project" identifier from the request and sets it
// in the Gin context as "projectID". For GET requests, it reads the "project" query
// parameter. For POST/PUT/PATCH requests, it reads "project" from the JSON body.
// Returns 400 if "project" is missing or empty.
func ProjectMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var project string

		switch c.Request.Method {
		case "GET":
			project = c.Query("project")
		default:
			// Try JSON body first, fallback to query param
			project = c.Query("project")
			if project == "" {
				// Read body into a reusable form
				bodyBytes, err := c.GetRawData()
				if err == nil && len(bodyBytes) > 0 {
					var pb ProjectBinding
					if json.Unmarshal(bodyBytes, &pb) == nil {
						project = pb.Project
					}
					// Restore body for downstream handlers
					c.Request.Body = ioReadCloser(bodyBytes)
				}
			}
		}

		project = strings.TrimSpace(project)
		if !ValidPathSegment(project) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid project parameter"})
			return
		}

		c.Set("projectID", project)
		c.Next()
	}
}

// ioReadCloser wraps a byte slice into an io.ReadCloser so the request body
// can be re-read by downstream middleware/handlers.
func ioReadCloser(b []byte) *bytesReadCloser {
	return &bytesReadCloser{strings.NewReader(string(b))}
}

type bytesReadCloser struct {
	*strings.Reader
}

func (b *bytesReadCloser) Close() error {
	return nil
}
