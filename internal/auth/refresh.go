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

// RefreshHandler rotates the refresh token and issues a new access token.
//
//	@Summary		Refresh an access token
//	@Description	Requires the refresh_token cookie. The cookie is single-use and is rotated on success; the response returns a new 15-minute access token and seven-day refresh_token cookie. In local mode, the cookie omits Domain and Secure.
//	@Tags			auth
//	@Produce		json
//	@Param			Cookie	header		string	true	"refresh_token=<token>"
//	@Success		200		{object}	RefreshResponse
//	@Header			200		{string}	Set-Cookie	"Firestore mode: refresh_token; Path=/; Domain=rayer.idv.tw; Max-Age=604800; HttpOnly; Secure; SameSite=Lax"
//	@Failure		401		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse
//	@Router			/api/v1/auth/refresh [post]
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

		user, err := getUser(c.Request.Context(), fsClient, claims.Sub)
		if err != nil || user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
			return
		}

		accessToken, err := GenerateAccessToken(claims.Sub, user.Role, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		refreshToken, err := GenerateRefreshToken(claims.Sub, user.Role, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}

		setRefreshTokenCookie(c, refreshToken, int(refreshTokenTTL.Seconds()))
		c.JSON(http.StatusOK, RefreshResponse{
			AccessToken: accessToken,
			User:        User{ID: claims.Sub, Email: user.Email, Role: user.Role},
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
