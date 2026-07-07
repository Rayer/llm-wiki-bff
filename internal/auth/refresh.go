package auth

import (
	"context"
	"net/http"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
)

type RefreshResponse struct {
	AccessToken string `json:"access_token"`
	User        User   `json:"user"`
}

type userLookupFunc func(ctx context.Context, fs *firestore.Client, userID string) (*UserRecord, error)

func RefreshHandler(fsClient *firestore.Client, jwtSecret string) gin.HandlerFunc {
	return refreshHandler(fsClient, jwtSecret, GetUser)
}

func refreshHandler(fsClient *firestore.Client, jwtSecret string, getUser userLookupFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Request.Cookie(refreshTokenCookieName)
		if err != nil || cookie.Value == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
			return
		}

		claims, err := validateRefreshToken(cookie.Value, jwtSecret)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
			return
		}

		accessToken, err := GenerateAccessToken(claims.Sub, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		refreshToken, err := GenerateRefreshToken(claims.Sub, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		user, err := getUser(c.Request.Context(), fsClient, claims.Sub)
		if err != nil || user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
			return
		}

		setRefreshTokenCookie(c, refreshToken, int(refreshTokenTTL.Seconds()))
		c.JSON(http.StatusOK, RefreshResponse{
			AccessToken: accessToken,
			User:        User{ID: claims.Sub, Email: user.Email},
		})
	}
}

func setRefreshTokenCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     refreshTokenCookieName,
		Value:    value,
		Path:     refreshTokenCookiePath,
		Domain:   refreshTokenDomain,
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
