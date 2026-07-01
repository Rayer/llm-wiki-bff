package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		setRefreshTokenCookie(c, "", -1)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
