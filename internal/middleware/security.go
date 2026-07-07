package middleware

import "github.com/gin-gonic/gin"

const (
	hstsHeaderValue           = "max-age=31536000; includeSubDomains"
	contentTypeOptionsValue   = "nosniff"
	frameOptionsValue         = "DENY"
	referrerPolicyHeaderValue = "strict-origin-when-cross-origin"
)

// SecurityHeaders adds browser security headers.
func SecurityHeaders(enableHSTS bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if enableHSTS {
			c.Header("Strict-Transport-Security", hstsHeaderValue)
		}
		c.Header("X-Content-Type-Options", contentTypeOptionsValue)
		c.Header("X-Frame-Options", frameOptionsValue)
		c.Header("Referrer-Policy", referrerPolicyHeaderValue)
		c.Next()
	}
}
