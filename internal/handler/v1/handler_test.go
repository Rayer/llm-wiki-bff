package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestGetGCSClientUsesRequestContextIdentity(t *testing.T) {
	defaultClient := &gcs.Client{}
	h := New(defaultClient, nil, search.NewIndex(), nil, nil, "")
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "request-project")

	client, err := h.GetGCSClient(c)
	if err != nil {
		t.Fatalf("get GCS client: %v", err)
	}
	if got := client.Prefix(); got != "users/request-user/projects/request-project" {
		t.Fatalf("prefix = %q, want %q", got, "users/request-user/projects/request-project")
	}
	if client == defaultClient {
		t.Fatal("GetGCSClient returned the default client for a scoped request")
	}
}

func TestGetGCSClientFallsBackWhenContextIdentityIsEmpty(t *testing.T) {
	defaultClient := &gcs.Client{}
	h := New(defaultClient, nil, search.NewIndex(), nil, nil, "")
	c, _ := gin.CreateTestContext(nil)

	client, err := h.GetGCSClient(c)
	if err != nil {
		t.Fatalf("get GCS client: %v", err)
	}
	if client != defaultClient {
		t.Fatal("GetGCSClient did not return the default client")
	}
}

func TestGetGCSClientRejectsPartialContextIdentity(t *testing.T) {
	h := New(&gcs.Client{}, nil, search.NewIndex(), nil, nil, "")
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")

	if _, err := h.GetGCSClient(c); err == nil {
		t.Fatal("GetGCSClient returned nil error for a partial request scope")
	}
}

func TestHealthReturnsOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(nil, nil, search.NewIndex(), nil, nil, "")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	h.Health(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Fatalf("body = %#v, want map[status:ok]", body)
	}
}

func TestPrometheusMetricsReturnsText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(nil, nil, search.NewIndex(), nil, nil, "")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)

	h.PrometheusMetrics(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", contentType)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "lwc_sources_count 0\n") {
		t.Fatalf("body does not contain zero source metric:\n%s", body)
	}
}

func TestPipelineRunExecutesCloudRunJob(t *testing.T) {
	argsFile := installFakeGcloud(t, 0, "job submitted")
	h := &Handler{index: search.NewIndex(), defaultUser: "default-user"}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo","command":"sync"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("userID", "request-user")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "accepted" || body["command"] != "sync" || body["project"] != "demo" {
		t.Fatalf("body = %#v", body)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read gcloud args: %v", err)
	}
	want := strings.Join([]string{
		"run", "jobs", "exec", "olw-pipeline",
		"--project", "llm-wiki-cloud",
		"--region", "asia-east1",
		"--args", "sync",
		"--update-env-vars", "USER_ID=request-user,PROJECT_ID=demo",
		"",
	}, "\n")
	if string(args) != want {
		t.Fatalf("gcloud args:\n%s\nwant:\n%s", args, want)
	}
}

func TestPipelineRunDefaultsCommandAndUser(t *testing.T) {
	argsFile := installFakeGcloud(t, 0, "job submitted")
	h := &Handler{index: search.NewIndex(), defaultUser: "default-user"}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read gcloud args: %v", err)
	}
	if got := string(args); !strings.Contains(got, "--args\nrun\n") ||
		!strings.Contains(got, "USER_ID=default-user,PROJECT_ID=demo\n") {
		t.Fatalf("gcloud args do not contain defaults:\n%s", got)
	}
}

func TestPipelineRunRequiresProject(t *testing.T) {
	h := &Handler{index: search.NewIndex(), defaultUser: "default-user"}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"command":"run"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PipelineRun(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"error":"project is required"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestPipelineRunReturnsGcloudOutputOnFailure(t *testing.T) {
	installFakeGcloud(t, 1, "permission denied")
	h := &Handler{index: search.NewIndex(), defaultUser: "default-user"}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PipelineRun(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "pipeline failed: permission denied") {
		t.Fatalf("body = %s", body)
	}
}

func installFakeGcloud(t *testing.T, exitCode int, output string) string {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FAKE_GCLOUD_ARGS\"\nprintf '%s\\n' \"$FAKE_GCLOUD_OUTPUT\"\nexit \"$FAKE_GCLOUD_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GCLOUD_ARGS", argsFile)
	t.Setenv("FAKE_GCLOUD_OUTPUT", output)
	t.Setenv("FAKE_GCLOUD_EXIT", strconv.Itoa(exitCode))
	return argsFile
}

func TestHandlersMatchGinHandlerSignature(t *testing.T) {
	h := New(nil, nil, search.NewIndex(), nil, nil, "")
	handlers := []gin.HandlerFunc{
		h.Health,
		h.Query,
		h.ListSources,
		h.GetSource,
		h.ListConcepts,
		h.GetConcept,
		h.Import,
		h.Status,
		h.PrometheusMetrics,
		h.PipelineRun,
	}
	if len(handlers) != 10 {
		t.Fatalf("handler count = %d, want 10", len(handlers))
	}
}
