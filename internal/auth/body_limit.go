package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxRequestBodyBytes is the maximum body accepted by an Auth request.
const MaxRequestBodyBytes int64 = 64 << 10

// RequestBodyLimit applies the standalone Auth request-body limit and keeps
// its existing handler-level response behavior for oversized JSON.
func RequestBodyLimit() gin.HandlerFunc {
	return requestBodyLimit(0)
}

// CompatibilityBodyLimit applies the Auth limit to the temporary BFF lane and
// rejects a declared oversized request before the compatibility handler runs.
func CompatibilityBodyLimit() gin.HandlerFunc {
	return requestBodyLimit(http.StatusRequestEntityTooLarge)
}

func requestBodyLimit(oversizedStatus int) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBodyBytes)
		}
		if c.Request.ContentLength > MaxRequestBodyBytes && oversizedStatus != 0 {
			c.AbortWithStatus(oversizedStatus)
			return
		}
		c.Next()
	}
}
