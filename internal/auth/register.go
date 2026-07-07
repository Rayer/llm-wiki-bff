package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type RegisterRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type RegisterResponse struct {
	Token    string `json:"token"`
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	ProjectID string `json:"default_project_id"`
}

func RegisterHandler(fs *firestore.Client, jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		token, err := GenerateToken(userID, jwtSecret, 24*3600)
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
