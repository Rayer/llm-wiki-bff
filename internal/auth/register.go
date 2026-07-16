package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type RegisterRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

const registrationTokenTTL = 24 * time.Hour

type RegisterResponse struct {
	Token     string `json:"token"`
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	ProjectID string `json:"default_project_id"`
}

// RegistrationGate reports whether self-serve registration is currently allowed.
type RegistrationGate interface {
	IsRegistrationEnabled(ctx context.Context) (bool, error)
}

// RegisterHandler creates a user and its default project when registration is enabled.
//
//	@Summary		Register an account
//	@Description	Returns a JWT in the response body and does not set a refresh cookie.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			request	body		RegisterRequest	true	"Registration details"
//	@Success		201		{object}	RegisterResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse
//	@Failure		429		{object}	RateLimitErrorResponse
//	@Header			429		{integer}	Retry-After	"Seconds until the rate limit window resets"
//	@Router			/api/v1/auth/register [post]
func RegisterHandler(fs *firestore.Client, jwtSecret string, gate RegistrationGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		if gate != nil {
			enabled, err := gate.IsRegistrationEnabled(c.Request.Context())
			if err != nil {
				log.Printf("[register] registration gate check failed: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}
			if !enabled {
				c.JSON(http.StatusForbidden, gin.H{"error": "registration is disabled"})
				return
			}
		}

		var req RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "email and password required"})
			return
		}
		email := strings.TrimSpace(req.Email)
		password := req.Password

		if len(password) < 8 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 8 characters"})
			return
		}

		ctx := c.Request.Context()

		// Check if email already exists
		if _, _, err := GetUserByEmail(ctx, fs, email); err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
			return
		}

		// Generate user ID
		userID := generateUserID()

		// Hash password
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("[register] bcrypt hash failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}

		// Create user
		if err := CreateUser(ctx, fs, userID, email, string(hash)); err != nil {
			log.Printf("[register] create user failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}

		// Auto-create default project
		defaultProjectID := "default"
		if err := createDefaultProject(ctx, fs, userID, defaultProjectID); err != nil {
			log.Printf("[register] auto-create project failed: %v", err)
			// Non-fatal — user can create project later
		}

		// Issue JWT
		token, err := GenerateToken(userID, jwtSecret, registrationTokenTTL)
		if err != nil {
			log.Printf("[register] JWT generation failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}

		c.JSON(http.StatusCreated, RegisterResponse{
			Token:     token,
			UserID:    userID,
			Email:     email,
			ProjectID: defaultProjectID,
		})
	}
}

func generateUserID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func createDefaultProject(ctx context.Context, fs *firestore.Client, userID, projectID string) error {
	_, err := fs.Collection("users").Doc(userID).Collection("projects").Doc(projectID).Set(ctx, map[string]interface{}{
		"name":       "My First Wiki",
		"created_at": nil,
	})
	return err
}
