package auth

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"golang.org/x/crypto/bcrypt"
)

// LoginRequest is the JSON body for POST /api/v1/auth/login.
type LoginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResponse is the JSON response for successful authentication.
type LoginResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// User contains the authenticated user's public information.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// LoginHandler returns a Gin handler that validates credentials and issues a JWT.
func LoginHandler(cfg config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email and password are required"})
			return
		}
		user, ok := lookupUser(cfg, req.Email)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		token, err := GenerateToken(user.ID, cfg.JWTSecret, 24*time.Hour)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		c.JSON(http.StatusOK, LoginResponse{
			Token: token,
			User:  User{ID: user.ID, Email: user.Email},
		})
	}
}

type storedUser struct {
	ID           string
	Email        string
	PasswordHash string
}

func lookupUser(cfg config.Config, email string) (storedUser, bool) {
	for _, u := range cfg.Users {
		if u.Email == email {
			return storedUser{ID: u.ID, Email: u.Email, PasswordHash: u.PasswordHash}, true
		}
	}
	return storedUser{}, false
}
