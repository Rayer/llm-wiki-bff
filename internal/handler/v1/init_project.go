package v1

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/auth"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type initProjectRequest struct {
	Name string `json:"name"`
}

type initProjectResponse struct {
	ProjectID string `json:"project_id" firestore:"project_id"`
	Name      string `json:"name" firestore:"name"`
	Status    string `json:"status" firestore:"status"`
	StatusURL string `json:"status_url" firestore:"status_url"`
}

type projectStatusResponse struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// InitProject handles POST /api/v1/init-project.
func (h *Handler) InitProject(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString("userID"))
	if userID == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}

	var req initProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "name must be 1-64 characters"})
		return
	}

	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey != "" && !auth.ValidPathSegment(idempotencyKey) {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid Idempotency-Key header"})
		return
	}

	ctx := c.Request.Context()
	fs := h.firestore.Raw()
	projects := fs.Collection("projects")
	if idempotencyKey != "" {
		snap, err := projects.Doc(projectDocID(userID, idempotencyKey)).Get(ctx)
		if err == nil && snap.Exists() {
			resp, err := initProjectResponseFromData(snap.Data())
			if err != nil {
				c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "invalid cached init-project response"})
				return
			}
			c.JSON(http.StatusAccepted, resp)
			return
		}
		if err != nil && status.Code(err) != codes.NotFound {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
			return
		}
	}

	projectID, err := generateProjectID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "generate project ID: " + err.Error()})
		return
	}
	resp := newInitProjectResponse(projectID, name)

	if err := createInitProjectDocs(ctx, fs, projects, userID, projectID, name, idempotencyKey, resp); err != nil {
		if errors.Is(err, errIdempotencyConflict) {
			snap, getErr := projects.Doc(projectDocID(userID, idempotencyKey)).Get(ctx)
			if getErr != nil {
				c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "idempotency conflict"})
				return
			}
			cached, decodeErr := initProjectResponseFromData(snap.Data())
			if decodeErr != nil {
				c.JSON(http.StatusConflict, handler.ErrorResponse{Error: "idempotency conflict"})
				return
			}
			c.JSON(http.StatusAccepted, cached)
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, resp)
}

// ProjectStatus handles GET /api/v1/projects/{pid}/status.
func (h *Handler) ProjectStatus(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString("userID"))
	if userID == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}
	projectID := strings.TrimSpace(c.Param("pid"))
	if !auth.ValidPathSegment(projectID) {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "invalid project ID"})
		return
	}
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	snap, err := h.firestore.Raw().Collection("projects").Doc(projectDocID(userID, projectID)).Get(c.Request.Context())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	data := snap.Data()
	resp := projectStatusResponse{ProjectID: projectID}
	if v, ok := data["name"].(string); ok {
		resp.Name = v
	}
	if v, ok := data["status"].(string); ok {
		resp.Status = v
	}
	if v, ok := data["error"].(string); ok {
		resp.Error = v
	}
	c.JSON(http.StatusOK, resp)
}

func generateProjectID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func projectDocID(userID, value string) string {
	return userID + "_" + value
}

var errIdempotencyConflict = errors.New("idempotency conflict")

func createInitProjectDocs(ctx context.Context, fs *firestore.Client, projects *firestore.CollectionRef, userID, projectID, name, idempotencyKey string, resp initProjectResponse) error {
	now := time.Now()
	projectData := initProjectData(projectID, name, idempotencyKey, now)

	projectRef := projects.Doc(projectDocID(userID, projectID))
	if idempotencyKey == "" {
		_, err := projectRef.Create(ctx, projectData)
		return err
	}

	keyRef := projects.Doc(projectDocID(userID, idempotencyKey))
	return fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		if snap, err := tx.Get(keyRef); err == nil && snap.Exists() {
			return errIdempotencyConflict
		} else if err != nil && status.Code(err) != codes.NotFound {
			return err
		}

		if err := tx.Create(projectRef, projectData); err != nil {
			return err
		}
		return tx.Create(keyRef, map[string]interface{}{
			"project_id":      resp.ProjectID,
			"status":          resp.Status,
			"status_url":      resp.StatusURL,
			"name":            resp.Name,
			"created_at":      now,
			"idempotency_key": idempotencyKey,
		})
	})
}

func newInitProjectResponse(projectID, name string) initProjectResponse {
	return initProjectResponse{
		ProjectID: projectID,
		Name:      name,
		Status:    "ready",
		StatusURL: "/api/v1/projects/" + projectID + "/status",
	}
}

func initProjectData(projectID, name, idempotencyKey string, now time.Time) map[string]interface{} {
	projectData := map[string]interface{}{
		"project_id": projectID,
		"name":       name,
		"status":     "ready",
		"created_at": now,
	}
	if idempotencyKey != "" {
		projectData["idempotency_key"] = idempotencyKey
	}
	return projectData
}

func initProjectResponseFromData(data map[string]interface{}) (initProjectResponse, error) {
	projectID, _ := data["project_id"].(string)
	statusValue, _ := data["status"].(string)
	statusURL, _ := data["status_url"].(string)
	if projectID == "" || statusValue == "" || statusURL == "" {
		return initProjectResponse{}, fmt.Errorf("missing cached response fields")
	}
	return initProjectResponse{
		ProjectID: projectID,
		Status:    statusValue,
		StatusURL: statusURL,
	}, nil
}
