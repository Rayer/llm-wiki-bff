package handler

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// PrometheusMetrics handles GET /metrics — Prometheus exposition format.
// Returns current wiki stats as Prometheus gauges. Scraped by Grafana Alloy.
func (h *Handler) PrometheusMetrics(c *gin.Context) {
	var sb strings.Builder

	userID, projectID := getUserProject(c)
	log.Printf("PrometheusMetrics [user=%s project=%s]", userID, projectID)

	// Collect data
	sources, _ := h.gcs.ListSources(c.Request.Context())
	concepts, _ := h.gcs.ListConcepts(c.Request.Context(), true)
	running := 0
	if h.firestore != nil {
		running, _ = h.firestore.CountActiveLocks(c.Request.Context())
	}

	// GCS stats (cached fast listing, no byte crawl)
	gcsFiles := int64(0)
	gcsBytes := int64(0)
	if h.gcs != nil {
		gcsBytes, gcsFiles, _ = h.gcs.BucketStats(c.Request.Context())
	}

	sb.WriteString("# HELP lwc_sources_count Number of wiki sources\n")
	sb.WriteString("# TYPE lwc_sources_count gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_sources_count %d\n", len(sources)))

	sb.WriteString("# HELP lwc_concepts_count Number of wiki concepts\n")
	sb.WriteString("# TYPE lwc_concepts_count gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_concepts_count %d\n", len(concepts)))

	sb.WriteString("# HELP lwc_running_pipelines Number of active pipeline locks\n")
	sb.WriteString("# TYPE lwc_running_pipelines gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_running_pipelines %d\n", running))

	sb.WriteString("# HELP lwc_index_sources Indexed source count\n")
	sb.WriteString("# TYPE lwc_index_sources gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_index_sources %d\n", h.index.SourceCount()))

	sb.WriteString("# HELP lwc_index_concepts Indexed concept count\n")
	sb.WriteString("# TYPE lwc_index_concepts gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_index_concepts %d\n", h.index.ConceptCount()))

	sb.WriteString("# HELP lwc_gcs_files Total GCS object count\n")
	sb.WriteString("# TYPE lwc_gcs_files gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_gcs_files %d\n", gcsFiles))

	sb.WriteString("# HELP lwc_gcs_bytes Total GCS bytes\n")
	sb.WriteString("# TYPE lwc_gcs_bytes gauge\n")
	sb.WriteString(fmt.Sprintf("lwc_gcs_bytes %d\n", gcsBytes))

	c.String(http.StatusOK, sb.String())
}
