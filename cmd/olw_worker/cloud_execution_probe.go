package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	defaultCloudRunAPITimeout  = 10 * time.Second
	defaultMetadataTokenURL    = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	defaultMetadataProjectURL  = "http://metadata.google.internal/computeMetadata/v1/project/project-id"
	cloudRunJobExecutionsScope = "https://www.googleapis.com/auth/cloud-platform"
	defaultPipelineJobLocation = "asia-east1"
)

// cloudRunExecutionProbe looks up holder liveness via Cloud Run Jobs Admin API.
// Unconfigured or uncertain lookups return lookup_failed (fail closed).
type cloudRunExecutionProbe struct {
	httpClient  *http.Client
	tokenSource oauth2.TokenSource
	project     string
	location    string
	tokenURL    string
	projectURL  string
	useMetadata bool
	getToken    func(ctx context.Context) (string, error)
	get         func(ctx context.Context, rawURL, token string) (statusCode int, body []byte, err error)
}

func newCloudRunExecutionProbe() *cloudRunExecutionProbe {
	return &cloudRunExecutionProbe{
		httpClient:  &http.Client{Timeout: defaultCloudRunAPITimeout},
		tokenURL:    defaultMetadataTokenURL,
		projectURL:  defaultMetadataProjectURL,
		useMetadata: true,
	}
}

func (p *cloudRunExecutionProbe) Probe(ctx context.Context, executionID, jobName string) leaseHolderLiveness {
	executionID = strings.TrimSpace(executionID)
	jobName = strings.TrimSpace(jobName)
	if !validPipelineExecutionID(executionID) || jobName == "" || !executionBelongsToJob(executionID, jobName) {
		return leaseHolderMalformed
	}

	project, location, err := p.resolveScope(ctx)
	if err != nil || project == "" || location == "" {
		return leaseHolderLookupFailed
	}

	token, err := p.accessToken(ctx)
	if err != nil || token == "" {
		return leaseHolderLookupFailed
	}

	rawURL := fmt.Sprintf(
		"https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s/executions/%s",
		url.PathEscape(project),
		url.PathEscape(location),
		url.PathEscape(jobName),
		url.PathEscape(executionID),
	)
	statusCode, body, err := p.doGet(ctx, rawURL, token)
	if err != nil {
		return leaseHolderLookupFailed
	}
	switch {
	case statusCode == http.StatusOK:
		return classifyCloudRunExecutionJSON(body)
	case statusCode == http.StatusNotFound:
		// API proved absence for a job-scoped, well-formed id → reclaim allowed.
		return leaseHolderNotFound
	case statusCode == http.StatusForbidden, statusCode == http.StatusUnauthorized:
		return leaseHolderLookupFailed
	case statusCode >= 500:
		return leaseHolderLookupFailed
	default:
		return leaseHolderLookupFailed
	}
}

func (p *cloudRunExecutionProbe) resolveScope(ctx context.Context) (project, location string, err error) {
	if p.project != "" {
		project = p.project
	} else {
		project = firstNonEmpty(
			os.Getenv("GOOGLE_CLOUD_PROJECT"),
			os.Getenv("GCP_PROJECT"),
			os.Getenv("GCLOUD_PROJECT"),
		)
		if project == "" && p.useMetadata {
			project, err = p.metadataGET(ctx, p.projectURL)
			if err != nil {
				return "", "", err
			}
		}
	}
	if p.location != "" {
		location = p.location
	} else {
		location = firstNonEmpty(
			os.Getenv("CLOUD_RUN_LOCATION"),
			os.Getenv("PIPELINE_JOB_LOCATION"),
			defaultPipelineJobLocation,
		)
	}
	// Prefer PIPELINE_JOB_URL when present (same contract as BFF).
	if jobURL := strings.TrimSpace(os.Getenv("PIPELINE_JOB_URL")); jobURL != "" {
		if pr, loc, ok := parsePipelineJobURLScope(jobURL); ok {
			if project == "" {
				project = pr
			}
			location = loc
		}
	}
	if project == "" || location == "" {
		return "", "", fmt.Errorf("cloud run job scope unavailable")
	}
	return project, location, nil
}

func parsePipelineJobURLScope(jobURL string) (project, location string, ok bool) {
	// https://run.googleapis.com/v2/projects/{p}/locations/{l}/jobs/{j}:run
	u, err := url.Parse(strings.TrimSpace(jobURL))
	if err != nil || u.Host != "run.googleapis.com" {
		return "", "", false
	}
	path := strings.TrimSuffix(u.Path, ":run")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// v2 / projects / P / locations / L / jobs / J
	if len(parts) != 7 || parts[0] != "v2" || parts[1] != "projects" || parts[3] != "locations" || parts[5] != "jobs" {
		return "", "", false
	}
	if parts[2] == "" || parts[4] == "" {
		return "", "", false
	}
	return parts[2], parts[4], true
}

func (p *cloudRunExecutionProbe) accessToken(ctx context.Context) (string, error) {
	if p.getToken != nil {
		return p.getToken(ctx)
	}
	if p.tokenSource != nil {
		tok, err := p.tokenSource.Token()
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}
	if p.useMetadata {
		if tok, err := p.metadataToken(ctx); err == nil && tok != "" {
			return tok, nil
		}
	}
	creds, err := google.FindDefaultCredentials(ctx, cloudRunJobExecutionsScope)
	if err != nil {
		return "", err
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func (p *cloudRunExecutionProbe) metadataToken(ctx context.Context) (string, error) {
	body, err := p.metadataGET(ctx, p.tokenURL)
	if err != nil {
		return "", err
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &tokenResponse); err != nil {
		return "", err
	}
	if tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("metadata token missing access_token")
	}
	return tokenResponse.AccessToken, nil
}

func (p *cloudRunExecutionProbe) metadataGET(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: defaultCloudRunAPITimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("metadata status %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), nil
}

func (p *cloudRunExecutionProbe) doGet(ctx context.Context, rawURL, token string) (int, []byte, error) {
	if p.get != nil {
		return p.get(ctx, rawURL, token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: defaultCloudRunAPITimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// classifyCloudRunExecutionJSON maps a Cloud Run v2 Execution JSON body to liveness.
func classifyCloudRunExecutionJSON(body []byte) leaseHolderLiveness {
	var execution struct {
		// v2 uses reconciling + completionStatus; also accept v1-like counters.
		Reconciling      bool   `json:"reconciling"`
		CompletionStatus string `json:"completionStatus"`
		// Conditions from some responses.
		Conditions []struct {
			Type   string `json:"type"`
			State  string `json:"state"`
			Status string `json:"status"`
		} `json:"conditions"`
		// v1 compatibility fields if ever present.
		SucceededCount int `json:"succeededCount"`
		FailedCount    int `json:"failedCount"`
		CancelledCount int `json:"cancelledCount"`
		RunningCount   int `json:"runningCount"`
	}
	if err := json.Unmarshal(body, &execution); err != nil {
		return leaseHolderLookupFailed
	}

	if status := normalizeWorkerCloudRunStatus(execution.CompletionStatus); status != "" {
		return livenessFromStatus(status)
	}
	for _, condition := range execution.Conditions {
		if !strings.EqualFold(condition.Type, "Completed") {
			continue
		}
		if status := normalizeWorkerCloudRunStatus(firstNonEmpty(condition.State, condition.Status)); status != "" {
			return livenessFromStatus(status)
		}
	}
	if execution.FailedCount > 0 {
		return leaseHolderTerminal
	}
	if execution.CancelledCount > 0 {
		return leaseHolderTerminal
	}
	if execution.SucceededCount > 0 {
		return leaseHolderTerminal
	}
	if execution.RunningCount > 0 || execution.Reconciling {
		return leaseHolderRunning
	}
	// Empty / unknown body: fail closed (do not reclaim).
	return leaseHolderLookupFailed
}

func normalizeWorkerCloudRunStatus(value string) string {
	status := strings.ToUpper(strings.TrimSpace(value))
	status = strings.TrimPrefix(status, "CONDITION_")
	status = strings.TrimPrefix(status, "EXECUTION_")
	switch status {
	case "SUCCEEDED", "TRUE":
		return "SUCCEEDED"
	case "FAILED", "FALSE":
		return "FAILED"
	case "CANCELLED":
		return "CANCELLED"
	case "PENDING", "RECONCILING", "UNKNOWN", "RUNNING":
		return "RUNNING"
	case "":
		return ""
	default:
		return status
	}
}

func livenessFromStatus(status string) leaseHolderLiveness {
	switch status {
	case "SUCCEEDED", "FAILED", "CANCELLED":
		return leaseHolderTerminal
	case "RUNNING":
		return leaseHolderRunning
	default:
		return leaseHolderLookupFailed
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
