package handler

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
)

func TestGetGCSClientUsesRequestContextIdentity(t *testing.T) {
	defaultClient := &gcs.Client{}
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "request-project")

	client, err := getGCSClient(c, defaultClient)
	if err != nil {
		t.Fatalf("get GCS client: %v", err)
	}
	if got := client.Prefix(); got != "users/request-user/projects/request-project" {
		t.Fatalf("prefix = %q, want %q", got, "users/request-user/projects/request-project")
	}
	if client == defaultClient {
		t.Fatal("getGCSClient returned the default client for a scoped request")
	}
}

func TestGetGCSClientFallsBackWhenContextIdentityIsEmpty(t *testing.T) {
	defaultClient := &gcs.Client{}
	c, _ := gin.CreateTestContext(nil)

	client, err := getGCSClient(c, defaultClient)
	if err != nil {
		t.Fatalf("get GCS client: %v", err)
	}
	if client != defaultClient {
		t.Fatal("getGCSClient did not return the default client")
	}
}

func TestGetGCSClientRejectsPartialContextIdentity(t *testing.T) {
	defaultClient := &gcs.Client{}
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")

	if _, err := getGCSClient(c, defaultClient); err == nil {
		t.Fatal("getGCSClient returned nil error for a partial request scope")
	}
}

func TestV1HandlersMatchGinHandlerSignature(t *testing.T) {
	h := &Handler{}
	handlers := []gin.HandlerFunc{
		h.V1Query,
		h.V1ListSources,
		h.V1GetSource,
		h.V1ListConcepts,
		h.V1GetConcept,
		h.V1Status,
	}
	if len(handlers) != 6 {
		t.Fatalf("handler count = %d, want 6", len(handlers))
	}
}
