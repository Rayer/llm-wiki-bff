package auth

import (
	"context"
	"net/http"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// LoginRequest is the JSON body for POST /api/v1/auth/login.
type LoginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// LoginResponse is the JSON response for successful authentication.
type LoginResponse struct {
	AccessToken string `json:"access_token"`
	User        User   `json:"user"`
}

// User contains the authenticated user's public information.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role,omitempty"`
}

// ErrorResponse is the JSON response returned when an auth request fails.
type ErrorResponse struct {
	Error string `json:"error"`
}

// RateLimitErrorResponse is returned when an auth request exceeds its rate limit.
type RateLimitErrorResponse struct {
	Error      string `json:"error"`
	RetryAfter int    `json:"retry_after"`
}

// LoginHandler returns a Gin handler that validates credentials against Firestore and issues a JWT.
//
//	@Summary		Log in
//	@Description	Authenticates email and password, returns a 15-minute access token, and sets a seven-day refresh_token cookie. In local mode, the cookie omits Domain and Secure.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			request	body		LoginRequest	true	"Credentials"
//	@Success		200		{object}	LoginResponse
//	@Header			200		{string}	Set-Cookie	"Firestore mode: refresh_token; Path=/; Domain=rayer.idv.tw; Max-Age=604800; HttpOnly; Secure; SameSite=Lax"
//	@Failure		400		{object}	ErrorResponse
//	@Failure		401		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse
//	@Failure		429		{object}	RateLimitErrorResponse
//	@Header			429		{integer}	Retry-After	"Seconds until the rate limit window resets"
//	@Router			/api/v1/auth/login [post]
func LoginHandler(fsClient *firestore.Client, jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email and password are required"})
			return
		}
		ctx := c.Request.Context()
		userID, user, err := GetUserByEmail(ctx, fsClient, req.Email)
		if err != nil || user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		accessToken, err := GenerateAccessToken(userID, user.Role, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		refreshToken, err := GenerateRefreshToken(userID, user.Role, jwtSecret)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		setRefreshTokenCookie(c, refreshToken, int(refreshTokenTTL.Seconds()))
		c.JSON(http.StatusOK, LoginResponse{
			AccessToken: accessToken,
			User:        User{ID: userID, Email: user.Email, Role: user.Role},
		})
	}
}

// CreateTestUser creates the default test user in Firestore (for dev/CI).
func CreateTestUser(ctx context.Context, fs *firestore.Client) {
	pw := "test123"
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		// best-effort: do not block startup
		return
	}
	_ = CreateUser(ctx, fs, "test-user", "test@example.com", string(hash))
}
