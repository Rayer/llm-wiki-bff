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
//	@Description	Clears the refresh_token cookie. No access token or request body is required.
//	@Tags			auth
//	@Produce		json
//	@Success		200	{object}	LogoutResponse
//	@Header			200	{string}	Set-Cookie	"refresh_token; Path=/; Domain=rayer.idv.tw; Max-Age=0; HttpOnly; Secure; SameSite=Lax"
//	@Router			/api/v1/auth/logout [post]
func LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		setRefreshTokenCookie(c, "", -1)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
