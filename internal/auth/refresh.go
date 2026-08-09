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
//	@Description	Requires the runtime-specific refresh cookie. The cookie is single-use and is rotated on success; the response returns a new 15-minute access token and seven-day refresh cookie.
//	@Tags			auth
//	@Produce		json
//	@Param			Cookie	header		string	true	"runtime refresh cookie=<token>"
//	@Success		200		{object}	RefreshResponse
//	@Header			200		{string}	Set-Cookie	"BFF compatibility: refresh_token; Path=/; Domain=rayer.idv.tw; Max-Age=604800; HttpOnly; Secure; SameSite=Lax"
//	@Failure		401		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse
func RefreshHandler(fsClient *firestore.Client, jwtSecret string) gin.HandlerFunc {
	return RefreshHandlerWithCookiePolicy(fsClient, jwtSecret, LegacyRefreshCookiePolicy())
}

func refreshHandler(fsClient *firestore.Client, jwtSecret string, getUser userLookupFunc) gin.HandlerFunc {
	return refreshHandlerWithCookiePolicy(fsClient, jwtSecret, getUser, LegacyRefreshCookiePolicy())
}

// RefreshHandlerWithCookiePolicy returns a refresh handler using the supplied immutable cookie policy.
func RefreshHandlerWithCookiePolicy(fsClient *firestore.Client, jwtSecret string, cookiePolicy RefreshCookiePolicy) gin.HandlerFunc {
	return refreshHandlerWithCookiePolicy(fsClient, jwtSecret, GetUser, cookiePolicy)
}

func refreshHandlerWithCookiePolicy(fsClient *firestore.Client, jwtSecret string, getUser userLookupFunc, cookiePolicy RefreshCookiePolicy) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Request.Cookie(cookiePolicy.Name)
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

		setRefreshTokenCookieWithPolicy(c, refreshToken, int(refreshTokenTTL.Seconds()), cookiePolicy)
		c.JSON(http.StatusOK, RefreshResponse{
			AccessToken: accessToken,
			User:        User{ID: claims.Sub, Email: user.Email, Role: user.Role},
		})
	}
}

func setRefreshTokenCookie(c *gin.Context, value string, maxAge int) {
	setRefreshTokenCookieWithPolicy(c, value, maxAge, LegacyRefreshCookiePolicy())
}

func setRefreshTokenCookieWithPolicy(c *gin.Context, value string, maxAge int, cookiePolicy RefreshCookiePolicy) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     cookiePolicy.Name,
		Value:    value,
		Path:     cookiePolicy.Path,
		Domain:   cookiePolicy.Domain,
		MaxAge:   maxAge,
		Secure:   cookiePolicy.Secure,
		HttpOnly: cookiePolicy.HttpOnly,
		SameSite: cookiePolicy.SameSite,
	})
}
