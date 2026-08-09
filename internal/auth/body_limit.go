package auth

import (
	"bytes"
	"io"
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
		if c.Request.Body == nil {
			c.Next()
			return
		}

		if oversizedStatus == 0 || c.Request.ContentLength > 0 {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBodyBytes)
		}
		if c.Request.ContentLength > MaxRequestBodyBytes && oversizedStatus != 0 {
			c.AbortWithStatus(oversizedStatus)
			return
		}
		if oversizedStatus == 0 || c.Request.ContentLength > 0 {
			c.Next()
			return
		}

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, MaxRequestBodyBytes+1))
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		if int64(len(body)) > MaxRequestBodyBytes {
			c.AbortWithStatus(oversizedStatus)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Next()
	}
}
