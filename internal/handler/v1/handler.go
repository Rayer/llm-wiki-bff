package v1

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// Handler holds the dependencies for the V1 API.
type Handler struct {
	gcs       *gcs.Client
	firestore *firestore.Client
	index     *search.Index
	llm       *llm.Client
	expander  *llm.QueryExpander
}

// New creates a V1 Handler with the given dependencies.
func New(gcsClient *gcs.Client, fs *firestore.Client, idx *search.Index, llmClient *llm.Client, expander *llm.QueryExpander) *Handler {
	return &Handler{
		gcs:       gcsClient,
		firestore: fs,
		index:     idx,
		llm:       llmClient,
		expander:  expander,
	}
}

// GetGCSClient returns the request-scoped GCS client.
func (h *Handler) GetGCSClient(c *gin.Context) (*gcs.Client, error) {
	userID := c.GetString("userID")
	projectID := c.GetString("projectID")
	if userID == "" && projectID == "" {
		return h.gcs, nil
	}
	if userID == "" || projectID == "" {
		return nil, fmt.Errorf("incomplete GCS request scope")
	}
	if h.gcs == nil {
		return nil, fmt.Errorf("GCS client is not configured")
	}
	return h.gcs.WithScope(userID, projectID), nil
}
