package v1

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestGetGCSClientUsesRequestContextIdentity(t *testing.T) {
	defaultClient := &gcs.Client{}
	h := New(defaultClient, nil, search.NewIndex(), conceptcache.New(), nil, nil, "")
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
	h := New(defaultClient, nil, search.NewIndex(), conceptcache.New(), nil, nil, "")
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
	h := New(&gcs.Client{}, nil, search.NewIndex(), conceptcache.New(), nil, nil, "")
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")

	if _, err := h.GetGCSClient(c); err == nil {
		t.Fatal("GetGCSClient returned nil error for a partial request scope")
	}
}

func TestHealthReturnsOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil, "")
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
	registryMetric := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lwc_test_default_registry_total",
		Help: "Test metric from the default registry.",
	})
	prometheus.MustRegister(registryMetric)
	registryMetric.Inc()
	t.Cleanup(func() {
		prometheus.Unregister(registryMetric)
	})

	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil, "")
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
	if body := recorder.Body.String(); !strings.Contains(body, "lwc_test_default_registry_total 1\n") {
		t.Fatalf("body does not contain default registry metric:\n%s", body)
	}
}

type handlerCacheReader struct {
	prefix string
	raw    string
}

func (r *handlerCacheReader) Prefix() string {
	return r.prefix
}

func (r *handlerCacheReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	return []gcs.WikiPage{{Slug: "alpha"}}, nil
}

func (r *handlerCacheReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return &gcs.WikiPage{Slug: "alpha"}, []byte(r.raw), nil
}

func TestCachedContextsIncludeConceptSources(t *testing.T) {
	reader := &handlerCacheReader{
		prefix: "users/u/projects/p",
		raw:    "---\ntitle: Alpha Concept\nsources: [Source One, Source Two]\n---\nAlpha body.",
	}
	conceptCache := conceptcache.New()
	if _, err := conceptCache.Build(context.Background(), reader); err != nil {
		t.Fatalf("build cache: %v", err)
	}

	contexts := cachedContexts(conceptCache, reader, []search.Result{{
		Slug:  "alpha",
		Title: "Alpha Concept",
		Type:  "concept",
	}})

	if len(contexts) != 1 {
		t.Fatalf("len(contexts) = %d, want 1", len(contexts))
	}
	if !strings.Contains(contexts[0], "Sources: Source One, Source Two") {
		t.Fatalf("context missing sources:\n%s", contexts[0])
	}
	if !strings.Contains(contexts[0], "Alpha body.") {
		t.Fatalf("context missing body:\n%s", contexts[0])
	}
}

func TestPipelineRunExecutesCloudRunJob(t *testing.T) {
	var runRequest map[string]any
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			if got := r.Header.Get("Metadata-Flavor"); got != "Google" {
				t.Errorf("Metadata-Flavor = %q, want Google", got)
			}
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("Authorization = %q, want Bearer test-token", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&runRequest); err != nil {
				t.Errorf("decode run request: %v", err)
			}
			return testHTTPResponse(http.StatusOK, `{}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		defaultUser:      "default-user",
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
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
	want := map[string]any{
		"overrides": map[string]any{
			"containerOverrides": []any{
				map[string]any{
					"args": []any{"sync"},
					"env": []any{
						map[string]any{"name": "USER_ID", "value": "request-user"},
						map[string]any{"name": "PROJECT_ID", "value": "demo"},
					},
				},
			},
		},
	}
	if got, _ := json.Marshal(runRequest); string(got) != mustJSON(t, want) {
		t.Fatalf("run request = %s, want %s", got, mustJSON(t, want))
	}
}

func TestPipelineRunDefaultsCommandAndUser(t *testing.T) {
	var runRequest struct {
		Overrides struct {
			ContainerOverrides []struct {
				Args []string `json:"args"`
				Env  []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"env"`
			} `json:"containerOverrides"`
		} `json:"overrides"`
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		if err := json.NewDecoder(r.Body).Decode(&runRequest); err != nil {
			t.Errorf("decode run request: %v", err)
		}
		return testHTTPResponse(http.StatusOK, `{}`), nil
	})}

	h := &Handler{
		index:            search.NewIndex(),
		defaultUser:      "default-user",
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	override := runRequest.Overrides.ContainerOverrides[0]
	if len(override.Args) != 1 || override.Args[0] != "run" {
		t.Fatalf("args = %#v, want [run]", override.Args)
	}
	if override.Env[0].Value != "default-user" || override.Env[1].Value != "demo" {
		t.Fatalf("env = %#v", override.Env)
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

func TestPipelineRunReturnsCloudRunResponseOnFailure(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusForbidden, "permission denied\n"), nil
	})}

	h := &Handler{
		index:            search.NewIndex(),
		defaultUser:      "default-user",
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
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

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestHandlersMatchGinHandlerSignature(t *testing.T) {
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil, "")
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
