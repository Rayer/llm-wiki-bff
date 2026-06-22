package v1

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusMetrics handles GET /api/v1/metrics using the request's GCS scope.
//
//	@Summary		Prometheus metrics
//	@Description	Returns scoped wiki statistics in Prometheus exposition format.
//	@Tags			metrics
//	@Produce		plain
//	@Success		200	{string}	string
//	@Failure		500	{string}	string
//	@Security		ProjectHeader
//	@Router			/api/v1/metrics [get]
func (h *Handler) PrometheusMetrics(c *gin.Context) {
	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	ctx := c.Request.Context()
	var sourcesCount, conceptsCount int
	var gcsFiles, gcsBytes int64
	if gcsClient != nil {
		sources, _ := gcsClient.ListSources(ctx)
		concepts, _ := gcsClient.ListConcepts(ctx, true)
		sourcesCount = len(sources)
		conceptsCount = len(concepts)
		gcsBytes, gcsFiles, _ = gcsClient.BucketStats(ctx)
	}

	running := 0
	if h.firestore != nil {
		running, _ = h.firestore.CountActiveLocks(ctx)
	}

	indexSources, indexConcepts := 0, 0
	if h.index != nil {
		indexSources = h.index.SourceCount()
		indexConcepts = h.index.ConceptCount()
	}

	var body strings.Builder
	writeGauge(&body, "lwc_sources_count", "Number of wiki sources", sourcesCount)
	writeGauge(&body, "lwc_concepts_count", "Number of wiki concepts", conceptsCount)
	writeGauge(&body, "lwc_running_pipelines", "Number of active pipeline locks", running)
	writeGauge(&body, "lwc_index_sources", "Indexed source count", indexSources)
	writeGauge(&body, "lwc_index_concepts", "Indexed concept count", indexConcepts)
	writeGauge(&body, "lwc_gcs_files", "Total GCS object count", gcsFiles)
	writeGauge(&body, "lwc_gcs_bytes", "Total GCS bytes", gcsBytes)

	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(body.String()))
	promhttp.Handler().ServeHTTP(c.Writer, c.Request)
}

func writeGauge(body *strings.Builder, name, help string, value interface{}) {
	fmt.Fprintf(body, "# HELP %s %s\n", name, help)
	fmt.Fprintf(body, "# TYPE %s gauge\n", name)
	fmt.Fprintf(body, "%s %v\n", name, value)
}
