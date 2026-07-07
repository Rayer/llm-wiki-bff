package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	localDevUserID   = "local-user"
	localDevEmail    = "demo@llm-wiki.dev"
	localDevPassword = "demo123456"
)

// LocalDevLoginHandler provides a filesystem-local login path for frontend
// development. It is intentionally not backed by Firestore and should only be
// mounted by BFF local mode.
func LocalDevLoginHandler(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email and password are required"})
			return
		}
		if strings.TrimSpace(req.Email) != localDevEmail || req.Password != localDevPassword {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}

		accessToken, err := GenerateAccessToken(localDevUserID, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		refreshToken, err := GenerateRefreshToken(localDevUserID, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		setLocalRefreshTokenCookie(c, refreshToken, int(refreshTokenTTL.Seconds()))
		c.JSON(http.StatusOK, LoginResponse{
			AccessToken: accessToken,
			User:        User{ID: localDevUserID, Email: localDevEmail},
		})
	}
}

// LocalDevRefreshHandler rotates local refresh tokens without Firestore.
func LocalDevRefreshHandler(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Request.Cookie(refreshTokenCookieName)
		if err != nil || cookie.Value == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
			return
		}

		claims, err := validateRefreshToken(cookie.Value, jwtSecret)
		if err != nil || claims.Sub != localDevUserID {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
			return
		}

		accessToken, err := GenerateAccessToken(localDevUserID, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		refreshToken, err := GenerateRefreshToken(localDevUserID, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		setLocalRefreshTokenCookie(c, refreshToken, int(refreshTokenTTL.Seconds()))
		c.JSON(http.StatusOK, RefreshResponse{
			AccessToken: accessToken,
			User:        User{ID: localDevUserID, Email: localDevEmail},
		})
	}
}

func setLocalRefreshTokenCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     refreshTokenCookieName,
		Value:    value,
		Path:     refreshTokenCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
