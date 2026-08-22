package handler

import (
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

type QueryConfigResponse struct {
	QueryConfig PublicQueryConfig    `json:"query_config"`
	Build       QueryConfigBuildInfo `json:"build"`
}

// PublicQueryConfig is the unauthenticated, privacy-safe query runtime DTO.
// Internal diagnostic readback fields must be copied here explicitly.
type PublicQueryConfig struct {
	SchemaVersion              int                       `json:"schema_version"`
	ConfigRevision             string                    `json:"config_revision"`
	ConfigDigest               string                    `json:"config_digest"`
	QueryServiceImplementation string                    `json:"query_service_implementation"`
	DefaultProfileID           string                    `json:"default_profile_id"`
	DefaultProfileDigest       string                    `json:"default_profile_digest"`
	DefaultPromptID            string                    `json:"default_prompt_id"`
	DefaultPromptDigest        string                    `json:"default_prompt_digest"`
	ExpansionProvider          string                    `json:"expansion_provider"`
	ExpansionModel             string                    `json:"expansion_model"`
	ExpansionReasoning         string                    `json:"expansion_reasoning"`
	ExpansionTemperature       float64                   `json:"expansion_temperature"`
	SynthesisProvider          string                    `json:"synthesis_provider"`
	SynthesisModel             string                    `json:"synthesis_model"`
	SynthesisReasoning         string                    `json:"synthesis_reasoning"`
	SynthesisTemperature       float64                   `json:"synthesis_temperature"`
	NoEvidencePolicy           string                    `json:"no_evidence_policy"`
	Options                    query.RuntimeQueryOptions `json:"options"`
}

func PublicQueryConfigFromReadback(readback query.RuntimeConfigReadback) PublicQueryConfig {
	return PublicQueryConfig{
		SchemaVersion: readback.SchemaVersion, ConfigRevision: readback.ConfigRevision, ConfigDigest: readback.ConfigDigest,
		QueryServiceImplementation: readback.QueryServiceImplementation,
		DefaultProfileID:           readback.DefaultProfileID, DefaultProfileDigest: readback.DefaultProfileDigest,
		DefaultPromptID: readback.DefaultPromptID, DefaultPromptDigest: readback.DefaultPromptDigest,
		ExpansionProvider: readback.ExpansionProvider, ExpansionModel: readback.ExpansionModel,
		ExpansionReasoning: readback.ExpansionReasoning, ExpansionTemperature: readback.ExpansionTemperature,
		SynthesisProvider: readback.SynthesisProvider, SynthesisModel: readback.SynthesisModel,
		SynthesisReasoning: readback.SynthesisReasoning, SynthesisTemperature: readback.SynthesisTemperature,
		NoEvidencePolicy: readback.NoEvidencePolicy,
		Options:          readback.Options,
	}
}

type QueryConfigBuildInfo struct {
	Commit   string `json:"commit"`
	Service  string `json:"service"`
	Revision string `json:"revision"`
}

// ErrorResponse is returned on errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// HealthResponse is returned by the V1 health endpoint.
type HealthResponse struct {
	Status string `json:"status"`
}

// ReadyResponse is returned by the V1 readiness endpoint.
type ReadyResponse struct {
	Ready    bool     `json:"ready"`
	Prefix   string   `json:"prefix,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
	Message  string   `json:"message,omitempty"`
}

// QueryRequest is the request body for a query endpoint.
type QueryRequest struct {
	Query string `json:"q"`
	Mode  string `json:"mode"`
}

// QueryResponse is the response for a query endpoint.
type QueryResponse struct {
	Query              string            `json:"query"`
	Mode               string            `json:"mode"`
	Results            []search.Result   `json:"results"`
	Expand             *llm.ExpandResult `json:"expand,omitempty"`
	AISynth            string            `json:"ai_synth,omitempty"`
	Citations          []search.Citation `json:"citations,omitempty"`
	Status             string            `json:"status,omitempty"`
	Reason             string            `json:"reason,omitempty"`
	AnswerBasis        string            `json:"answer_basis,omitempty"`
	WikiEvidenceStatus string            `json:"wiki_evidence_status,omitempty"`
	DisclosureRequired bool              `json:"disclosure_required,omitempty"`
}

// SourcesListResponse is the response for a sources list endpoint.
type SourcesListResponse struct {
	Sources []gcs.WikiPage `json:"sources"`
	Count   int            `json:"count"`
}

// SourceDetailResponse is the response for a source detail endpoint.
type SourceDetailResponse struct {
	ID                string                 `json:"id"`
	Slug              string                 `json:"slug"`
	Title             string                 `json:"title"`
	Type              string                 `json:"type"`
	Frontmatter       map[string]interface{} `json:"frontmatter"`
	Body              string                 `json:"body"`
	Raw               string                 `json:"raw"`
	RawPath           string                 `json:"raw_path"`
	AnnotationAllowed bool                   `json:"annotation_allowed"`
	HasAnnotation     bool                   `json:"has_annotation"`
	AnnotationDirty   bool                   `json:"annotation_dirty"`
	RawDirty          bool                   `json:"raw_dirty"`
	Dirty             bool                   `json:"dirty"`
	LifecycleStatus   string                 `json:"lifecycle_status"`
	AnnUpdatedAt      string                 `json:"ann_updated_at,omitempty"`
}

// AnnotationRequest is the body for PUT /sources/:id/annotation.
type AnnotationRequest struct {
	Body               string `json:"body"`
	ExpectedGeneration string `json:"expected_generation" validate:"required"`
}

// AnnotationResponse is returned by GET and PUT source annotation endpoints.
type AnnotationResponse struct {
	SourceID        string `json:"source_id"`
	RawPath         string `json:"raw_path"`
	Body            string `json:"body"`
	SHA256          string `json:"ann_sha256"`
	UpdatedAt       string `json:"updated_at"`
	UpdatedBy       string `json:"updated_by"`
	HasAnnotation   bool   `json:"has_annotation"`
	Generation      string `json:"generation"`
	AnnotationDirty bool   `json:"annotation_dirty"`
	RawDirty        bool   `json:"raw_dirty"`
	Dirty           bool   `json:"dirty"`
	LifecycleStatus string `json:"lifecycle_status"`
}

// ConceptsListResponse is the response for a concepts list endpoint.
type ConceptsListResponse struct {
	Concepts []gcs.WikiPage `json:"concepts"`
	Count    int            `json:"count"`
}

// ProjectResponse is a project entry returned by GET /api/v1/projects.
type ProjectResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// ConceptDetailResponse is the response for a concept detail endpoint.
type ConceptDetailResponse struct {
	Slug        string                 `json:"slug"`
	ID          string                 `json:"id,omitempty"`
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
	SourcesCount     int                        `json:"sources_count"`
	ConceptsCount    int                        `json:"concepts_count"`
	RawCount         int                        `json:"raw_count"`
	IndexSources     int                        `json:"index_sources"`
	IndexConcepts    int                        `json:"index_concepts"`
	SuggestedQueries []string                   `json:"suggested_queries"`
	RunningPipelines int                        `json:"running_pipelines"`
	LastExecution    *PipelineExecutionResponse `json:"last_execution,omitempty"`
	Locked           bool                       `json:"locked,omitempty"`
	LockWorker       string                     `json:"lock_worker,omitempty"`
	LockExpiry       string                     `json:"lock_expiry,omitempty"`
}

// PipelineExecutionResponse is a normalized Cloud Run execution summary.
type PipelineExecutionResponse struct {
	Name           string                     `json:"name"`
	Status         string                     `json:"status"`
	StartTime      string                     `json:"start_time"`
	EndTime        string                     `json:"end_time"`
	Duration       string                     `json:"duration"`
	LogURL         string                     `json:"log_url,omitempty"`
	Diagnostic     *PipelineFailureDiagnostic `json:"diagnostic,omitempty"`
	LogState       string                     `json:"log_state,omitempty"`
	LogStateReason string                     `json:"log_state_reason,omitempty"`
}

// PipelineFailureDiagnostic is the allowlisted, worker-produced failure
// receipt exposed to an authenticated owner.
type PipelineFailureDiagnostic struct {
	Version    int    `json:"version"`
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	ErrorClass string `json:"error_class"`
	DetailCode string `json:"detail_code,omitempty"`
	Child      string `json:"child_command,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
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
