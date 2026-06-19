package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/config"
)

// Claims is the JWT claims structure for HS256 self-signed tokens.
type Claims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
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
			c.Set("userID", cfg.DefaultUserID)
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

		token := parts[1]
		claims, err := verifyToken(token, cfg.JWTSecret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
			return
		}

		c.Set("userID", claims.Sub)
		c.Next()
	}
}

// verifyToken decodes and verifies a JWT token using HS256.
// Returns the claims if the token is valid.
func verifyToken(token string, secret string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token")
	}

	// Decode header and payload (base64url, no padding)
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("bad header: %w", err)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bad payload: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("bad signature: %w", err)
	}

	// Verify algorithm in header
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("bad header JSON: %w", err)
	}
	if header.Alg != "HS256" {
		return nil, fmt.Errorf("unsupported algorithm: %s", header.Alg)
	}

	// Verify signature: HMAC-SHA256(header.payload, secret)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(signature, expectedSig) {
		return nil, fmt.Errorf("signature verification failed")
	}

	// Parse claims
	var claims Claims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("bad claims JSON: %w", err)
	}

	// Validate required fields
	if claims.Sub == "" {
		return nil, fmt.Errorf("missing sub claim")
	}

	// Check expiry (allow 0 = no expiry for dev tokens)
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("token expired at %d", claims.Exp)
	}

	return &claims, nil
}

// GenerateToken creates a self-signed HS256 JWT for development/testing.
// Exported for use in tests and dev tooling. Not used in production flows.
func GenerateToken(userID, secret string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		Sub: userID,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
	}

	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	headerJSON, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}

	headerEnc := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadEnc := base64.RawURLEncoding.EncodeToString(payloadJSON)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(headerEnc + "." + payloadEnc))
	sig := mac.Sum(nil)
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)

	return headerEnc + "." + payloadEnc + "." + sigEnc, nil
}
