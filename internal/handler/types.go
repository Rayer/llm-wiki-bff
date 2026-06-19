package handler

import (
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// ErrorResponse is returned on errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// HealthResponse is returned by the V1 health endpoint.
type HealthResponse struct {
	Status string `json:"status"`
}

// QueryRequest is the request body for a query endpoint.
type QueryRequest struct {
	Query string `json:"q"`
	Mode  string `json:"mode"`
}

// QueryResponse is the response for a query endpoint.
type QueryResponse struct {
	Query     string            `json:"query"`
	Mode      string            `json:"mode"`
	Results   []search.Result   `json:"results"`
	Expand    *llm.ExpandResult `json:"expand,omitempty"`
	AISynth   string            `json:"ai_synth,omitempty"`
	Citations []search.Citation `json:"citations,omitempty"`
}

// SourcesListResponse is the response for a sources list endpoint.
type SourcesListResponse struct {
	Sources []gcs.WikiPage `json:"sources"`
	Count   int            `json:"count"`
}

// SourceDetailResponse is the response for a source detail endpoint.
type SourceDetailResponse struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Type        string                 `json:"type"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Body        string                 `json:"body"`
	Raw         string                 `json:"raw"`
}

// ConceptsListResponse is the response for a concepts list endpoint.
type ConceptsListResponse struct {
	Concepts []gcs.WikiPage `json:"concepts"`
	Count    int            `json:"count"`
}

// ConceptDetailResponse is the response for a concept detail endpoint.
type ConceptDetailResponse struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Type        string                 `json:"type"`
	Status      string                 `json:"status"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
	Body        string                 `json:"body"`
	Raw         string                 `json:"raw"`
}

// ImportRequest is the body for an import endpoint.
type ImportRequest struct {
	URLs []string `json:"urls" binding:"required"`
}

// ImportResponse is the response for an import endpoint.
type ImportResponse struct {
	Message  string   `json:"message"`
	Received int      `json:"received"`
	URLs     []string `json:"urls"`
}

// StatusResponse is the response for a status endpoint.
type StatusResponse struct {
	SourcesCount     int    `json:"sources_count"`
	ConceptsCount    int    `json:"concepts_count"`
	IndexSources     int    `json:"index_sources"`
	IndexConcepts    int    `json:"index_concepts"`
	RunningPipelines int    `json:"running_pipelines"`
	Locked           bool   `json:"locked,omitempty"`
	LockWorker       string `json:"lock_worker,omitempty"`
	LockExpiry       string `json:"lock_expiry,omitempty"`
}

// MetricsResponse is for GET /api/metrics (Grafana).
type MetricsResponse struct {
	RunningPipelines int                `json:"running_pipelines"`
	RecentExecutions []ExecutionSummary `json:"recent_executions"`
	GCP              *GCPMetrics        `json:"gcp,omitempty"`
}

// GCPMetrics holds simple GCP usage stats.
type GCPMetrics struct {
	GCSTotalBytes int64 `json:"gcs_total_bytes"`
	GCSTotalFiles int64 `json:"gcs_total_files"`
}

// ExecutionSummary is a lightweight execution record for metrics.
type ExecutionSummary struct {
	StartedAt   string  `json:"started_at"`
	FinishedAt  string  `json:"finished_at,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	Status      string  `json:"status"`
}
