package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rayer/llm-wiki-bff/internal/config"
)

// Claims is the JWT claims structure for HS256 tokens.
// Sub is kept at the outer level for backward compatibility (claims.Sub).
// RegisteredClaims is embedded for standard JWT fields (exp, iat, etc.).
type Claims struct {
	Sub       string `json:"sub"`
	Role      string `json:"role,omitempty"`
	TokenType string `json:"token_type,omitempty"`
	jwt.RegisteredClaims
}

const (
	accessTokenTTL         = 15 * time.Minute
	refreshTokenTTL        = 7 * 24 * time.Hour
	accessTokenType        = "access"
	refreshTokenType       = "refresh"
	refreshTokenCookieName = "refresh_token"
	refreshTokenCookiePath = "/"
	refreshTokenDomain     = "rayer.idv.tw"
)

var refreshTokenStore = struct {
	sync.Mutex
	active map[string]time.Time
}{
	active: make(map[string]time.Time),
}

// JWTAuth returns a Gin middleware that validates a JWT from the Authorization header.
// Config-driven: uses cfg.JWTSecret for HS256 verification.
// DEV mode: if cfg.DevJWT is set AND no Authorization header is present,
// it injects cfg.DefaultUserID into the context.
func JWTAuth(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")

		// DEV mode: inject user from X-User-ID header when DevJWT is configured and no auth header
		if cfg.DevJWT && authHeader == "" {
			userID := strings.TrimSpace(c.GetHeader("X-User-ID"))
			if !ValidPathSegment(userID) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
				return
			}
			userRole := strings.TrimSpace(c.GetHeader("X-User-Role"))
			if userRole == "" {
				userRole = "admin"
			}
			c.Set("userID", userID)
			c.Set("userRole", userRole)
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

		claims, err := ValidateToken(parts[1], cfg.JWTSecret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		claims.Sub = strings.TrimSpace(claims.Sub)
		if !ValidPathSegment(claims.Sub) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		c.Set("userID", claims.Sub)
		c.Set("userRole", claims.Role)
		c.Next()
	}
}

// AdminOnly returns a Gin middleware that allows only users with the admin role.
func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("userRole") != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin role required"})
			return
		}
		c.Next()
	}
}

// ValidPathSegment reports whether value can be safely used as one path segment.
func ValidPathSegment(value string) bool {
	return value != "" &&
		value != "." &&
		value != ".." &&
		!strings.ContainsAny(value, `/\`+"\x00")
}

// GenerateToken creates a self-signed HS256 JWT for development/testing.
// Exported for use in tests and dev tooling. Not used in production flows.
func GenerateToken(userID, secret string, ttl time.Duration) (string, error) {
	return GenerateTokenWithRole(userID, "", secret, ttl)
}

// GenerateTokenWithRole creates a self-signed HS256 JWT with a role claim.
func GenerateTokenWithRole(userID, role, secret string, ttl time.Duration) (string, error) {
	return generateToken(userID, role, secret, ttl, "", "")
}

// GenerateAccessToken creates a short-lived HS256 JWT for API authorization.
func GenerateAccessToken(userID, role, secret string) (string, error) {
	return generateToken(userID, role, secret, accessTokenTTL, accessTokenType, "")
}

// GenerateRefreshToken creates and records a refresh token for cookie-based rotation.
func GenerateRefreshToken(userID, role, secret string) (string, error) {
	jti, err := randomTokenID()
	if err != nil {
		return "", err
	}
	token, err := generateToken(userID, role, secret, refreshTokenTTL, refreshTokenType, jti)
	if err != nil {
		return "", err
	}
	refreshTokenStore.Lock()
	refreshTokenStore.active[jti] = time.Now().Add(refreshTokenTTL)
	refreshTokenStore.Unlock()
	return token, nil
}

// ValidateToken validates an HS256 access token and returns its claims.
func ValidateToken(tokenString, secret string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !token.Valid || claims.TokenType == refreshTokenType {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}

func validateRefreshToken(tokenString, secret string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !token.Valid || claims.TokenType != refreshTokenType || claims.ID == "" {
		return nil, fmt.Errorf("invalid refresh token")
	}
	now := time.Now()
	refreshTokenStore.Lock()
	expiresAt, ok := refreshTokenStore.active[claims.ID]
	if ok {
		delete(refreshTokenStore.active, claims.ID)
	}
	refreshTokenStore.Unlock()
	if !ok || now.After(expiresAt) {
		return nil, fmt.Errorf("invalid refresh token")
	}
	return claims, nil
}

func generateToken(userID, role, secret string, ttl time.Duration, tokenType, jti string) (string, error) {
	now := time.Now()
	claims := &Claims{
		Sub:       userID,
		Role:      role,
		TokenType: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        jti,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

func randomTokenID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func resetRefreshTokensForTest() {
	refreshTokenStore.Lock()
	refreshTokenStore.active = make(map[string]time.Time)
	refreshTokenStore.Unlock()
}
