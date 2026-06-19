package handler

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
)

func TestGetGCSClientUsesRequestContextIdentity(t *testing.T) {
	var gotUserID, gotProjectID string
	h := &Handler{
		gcsClient: func(userID, projectID string) (*gcs.Client, error) {
			gotUserID = userID
			gotProjectID = projectID
			return nil, nil
		},
	}
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "request-project")

	if _, err := h.getGCSClient(c); err != nil {
		t.Fatalf("get GCS client: %v", err)
	}
	if gotUserID != "request-user" {
		t.Fatalf("userID = %q, want %q", gotUserID, "request-user")
	}
	if gotProjectID != "request-project" {
		t.Fatalf("projectID = %q, want %q", gotProjectID, "request-project")
	}
}
