package auth

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rayer/llm-wiki-bff/internal/config"
)

// Claims is the JWT claims structure for HS256 tokens.
// Sub is kept at the outer level for backward compatibility (claims.Sub).
// RegisteredClaims is embedded for standard JWT fields (exp, iat, etc.).
type Claims struct {
	Sub string `json:"sub"`
	jwt.RegisteredClaims
}

// JWTAuth returns a Gin middleware that validates a JWT from the Authorization header.
// Config-driven: uses cfg.JWTSecret for HS256 verification.
// DEV mode: if cfg.DevJWT is set AND no Authorization header is present,
// it injects cfg.DefaultUserID into the context.
func JWTAuth(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")

		// DEV mode: inject default user when DevJWT is configured and no auth header
		if cfg.DevJWT && authHeader == "" {
			// Allow X-User-ID header override for multi-user testing
			userID := c.GetHeader("X-User-ID")
			if userID == "" {
				userID = cfg.DefaultUserID
			}
			c.Set("userID", userID)
			c.Next()
			return
		}

		// Production / normal mode: require Bearer token
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing Authorization header"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid Authorization header format, expected: Bearer <token>"})
			return
		}

		tokenString := parts[1]
		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			// Validate signing method is HMAC-based (HS256)
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return []byte(cfg.JWTSecret), nil
		}, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}
		if !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		c.Set("userID", claims.Sub)
		c.Next()
	}
}

// GenerateToken creates a self-signed HS256 JWT for development/testing.
// Exported for use in tests and dev tooling. Not used in production flows.
func GenerateToken(userID, secret string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := &Claims{
		Sub: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}
