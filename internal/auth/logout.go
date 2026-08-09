package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// LogoutResponse confirms that the client refresh token was cleared.
type LogoutResponse struct {
	OK bool `json:"ok"`
}

// LogoutHandler clears the refresh token cookie.
//
//	@Summary		Log out
//	@Description	Clears the runtime-specific refresh cookie. No access token or request body is required.
//	@Tags			auth
//	@Produce		json
//	@Success		200	{object}	LogoutResponse
//	@Header			200	{string}	Set-Cookie	"BFF compatibility: refresh_token; Path=/; Domain=rayer.idv.tw; Max-Age=0; HttpOnly; Secure; SameSite=Lax"
func LogoutHandler() gin.HandlerFunc {
	return LogoutHandlerWithCookiePolicy(LegacyRefreshCookiePolicy())
}

// LogoutHandlerWithCookiePolicy returns a logout handler using the supplied immutable cookie policy.
func LogoutHandlerWithCookiePolicy(cookiePolicy RefreshCookiePolicy) gin.HandlerFunc {
	return func(c *gin.Context) {
		setRefreshTokenCookieWithPolicy(c, "", -1, cookiePolicy)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
