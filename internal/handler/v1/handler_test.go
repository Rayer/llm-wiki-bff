package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/pipelinequota"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestGetGCSClientUsesRequestContextIdentity(t *testing.T) {
	defaultClient := &gcs.Client{}
	h := New(defaultClient, nil, search.NewIndex(), conceptcache.New(), nil, nil)
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
	h := New(defaultClient, nil, search.NewIndex(), conceptcache.New(), nil, nil)
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
	h := New(&gcs.Client{}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "request-user")

	if _, err := h.GetGCSClient(c); err == nil {
		t.Fatal("GetGCSClient returned nil error for a partial request scope")
	}
}

func TestHealthReturnsOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
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

func TestListProjectsRequiresUserID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)

	h.ListProjects(c)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "user not authenticated" {
		t.Fatalf("body = %#v, want user not authenticated error", body)
	}
}

func TestProjectResponseFromFirestoreDocUsesProjectIDField(t *testing.T) {
	resp, uid, ok := projectResponseFromFirestoreDoc("user-1_Human Project Name", map[string]interface{}{
		"project_id": "project-123",
		"name":       "Human Project Name",
	})

	if !ok {
		t.Fatal("projectResponseFromFirestoreDoc returned ok=false")
	}
	if uid != "user-1" {
		t.Fatalf("uid = %q, want user-1", uid)
	}
	if resp.ID != "project-123" {
		t.Fatalf("id = %q, want project-123", resp.ID)
	}
	if resp.Name != "Human Project Name" {
		t.Fatalf("name = %q, want Human Project Name", resp.Name)
	}
}

func TestPipelineURLsUseConfiguredJobTarget(t *testing.T) {
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.SetPipelineJobURL(" https://run.googleapis.com/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev:run ")

	if got, want := h.pipelineJobURL(), "https://run.googleapis.com/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev:run"; got != want {
		t.Fatalf("pipelineJobURL() = %q, want %q", got, want)
	}
	if got, want := h.cloudRunExecutionsURL(), "https://run.googleapis.com/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev/executions?pageSize=20"; got != want {
		t.Fatalf("cloudRunExecutionsURL() = %q, want %q", got, want)
	}
	if got, want := h.cloudRunExecutionURL("exec-1"), "https://run.googleapis.com/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev/executions/exec-1"; got != want {
		t.Fatalf("cloudRunExecutionURL() = %q, want %q", got, want)
	}
}

func TestPipelineURLsKeepLegacyDefaultTarget(t *testing.T) {
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)

	if got, want := h.pipelineJobURL(), defaultCloudRunJobURL; got != want {
		t.Fatalf("pipelineJobURL() = %q, want %q", got, want)
	}
}

func TestPipelineRequestsUseConfiguredJobTarget(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case strings.HasSuffix(r.URL.Path, "/jobs/olw-pipeline-dev:run"):
			return testHTTPResponse(http.StatusOK, `{"metadata":{"execution":"projects/dev/locations/asia-east1/jobs/olw-pipeline-dev/executions/exec-1"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/jobs/olw-pipeline-dev/executions"):
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		case strings.HasSuffix(r.URL.Path, "/jobs/olw-pipeline-dev/executions/exec-1"):
			return testHTTPResponse(http.StatusOK, `{"name":"projects/dev/locations/asia-east1/jobs/olw-pipeline-dev/executions/exec-1","succeededCount":1}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.SetPipelineJobURL("https://run.test/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev:run")

	if _, err := h.invokePipelineJob(context.Background(), "user", "project"); err != nil {
		t.Fatalf("invokePipelineJob() error = %v", err)
	}
	if _, err := h.pipelineExecutionStatus(context.Background(), ""); err != nil {
		t.Fatalf("pipelineExecutionStatus(list) error = %v", err)
	}
	if _, err := h.pipelineExecutionStatus(context.Background(), "exec-1"); err != nil {
		t.Fatalf("pipelineExecutionStatus(detail) error = %v", err)
	}

	want := []string{
		"/token",
		"/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev:run",
		"/token",
		"/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev/executions",
		"/token",
		"/v2/projects/dev/locations/asia-east1/jobs/olw-pipeline-dev/executions/exec-1",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("request paths = %#v, want %#v", paths, want)
	}
}

func TestProjectResponseFromFirestoreDocSkipsIdempotencyCacheDoc(t *testing.T) {
	// init-project stores a cache doc at {userID}_{idempotencyKey} that points at
	// the real project via project_id. Listing must not emit that project twice.
	_, _, ok := projectResponseFromFirestoreDoc("user-1_idem-key-1", map[string]interface{}{
		"project_id":      "project-123",
		"name":            "Human Project Name",
		"status":          "ready",
		"status_url":      "/api/v1/projects/project-123/status",
		"idempotency_key": "idem-key-1",
	})
	if ok {
		t.Fatal("idempotency cache doc must not be treated as a listable project")
	}
}

func TestProjectResponseFromFirestoreDocKeepsRealProjectWithIdempotencyKey(t *testing.T) {
	resp, uid, ok := projectResponseFromFirestoreDoc("user-1_project-123", map[string]interface{}{
		"project_id":      "project-123",
		"name":            "Human Project Name",
		"status":          "ready",
		"idempotency_key": "idem-key-1",
	})
	if !ok {
		t.Fatal("real project doc must still be listable when it records an idempotency_key")
	}
	if uid != "user-1" || resp.ID != "project-123" {
		t.Fatalf("got uid=%q id=%q", uid, resp.ID)
	}
}

func TestIsIdempotencyCacheDoc(t *testing.T) {
	if !isIdempotencyCacheDoc("user-1_idem-key-1", map[string]interface{}{
		"project_id":      "project-123",
		"idempotency_key": "idem-key-1",
	}) {
		t.Fatal("expected cache doc detection")
	}
	if isIdempotencyCacheDoc("user-1_project-123", map[string]interface{}{
		"project_id":      "project-123",
		"idempotency_key": "idem-key-1",
	}) {
		t.Fatal("real project must not be treated as cache doc")
	}
	if isIdempotencyCacheDoc("user-1_project-123", map[string]interface{}{
		"project_id": "project-123",
	}) {
		t.Fatal("doc without idempotency_key is not a cache doc")
	}
}

func TestProjectTitleFromIndexReadsFrontmatterTitle(t *testing.T) {
	data := []byte("---\ntitle: Project Name\n---\nProject overview.")

	if got := projectTitleFromIndex(data); got != "Project Name" {
		t.Fatalf("projectTitleFromIndex = %q, want %q", got, "Project Name")
	}
	if got := projectTitleFromIndex([]byte("Project overview.")); got != "" {
		t.Fatalf("projectTitleFromIndex without frontmatter = %q, want empty", got)
	}
}

func TestReadIndexJSONReturnsRawIDMapJSON(t *testing.T) {
	reader := &fakeIndexReader{
		files: map[string][]byte{
			idMapPath: []byte(`{"concept":{"abc123def456":"alpha"},"source":{}}`),
		},
	}

	data, err := readIndexJSON(context.Background(), reader)
	if err != nil {
		t.Fatalf("read index JSON: %v", err)
	}
	if string(data) != `{"concept":{"abc123def456":"alpha"},"source":{}}` {
		t.Fatalf("data = %s", data)
	}
	if reader.readPath != idMapPath {
		t.Fatalf("read path = %q, want %q", reader.readPath, idMapPath)
	}
}

func TestReadIndexJSONReturnsNotFoundForMissingIDMap(t *testing.T) {
	_, err := readIndexJSON(context.Background(), &fakeIndexReader{})
	if !errors.Is(err, errIndexNotFound) {
		t.Fatalf("read index JSON error = %v, want errIndexNotFound", err)
	}
}

func TestMergeWikiPageIDsFillsEmptyIDsFromIDMap(t *testing.T) {
	pages := []gcs.WikiPage{
		{Slug: "alpha", ID: ""},
		{Slug: "beta", ID: "existing-id"},
		{Slug: "gamma", ID: ""},
	}
	mergeWikiPageIDs(pages, map[string]string{
		"a3f7b2c01d9d": "alpha",
		"b4c8d2e0f1a9": "beta",
	})

	if pages[0].ID != "a3f7b2c01d9d" {
		t.Fatalf("alpha id = %q, want a3f7b2c01d9d", pages[0].ID)
	}
	if pages[1].ID != "existing-id" {
		t.Fatalf("beta id = %q, want existing-id", pages[1].ID)
	}
	if pages[2].ID != "" {
		t.Fatalf("gamma id = %q, want empty", pages[2].ID)
	}
}

func TestAddWikiPageIDsFromIDMapReadsIDMapPath(t *testing.T) {
	reader := &fakeIndexReader{
		files: map[string][]byte{
			idMapPath: []byte(`{"concept":{},"source":{"a3f7b2c01d9d":"alpha"}}`),
		},
	}
	pages := []gcs.WikiPage{{Slug: "alpha"}}

	if err := addWikiPageIDsFromIDMap(context.Background(), reader, pages, "source"); err != nil {
		t.Fatalf("add wiki page IDs: %v", err)
	}

	if reader.readPath != idMapPath {
		t.Fatalf("read path = %q, want %q", reader.readPath, idMapPath)
	}
	if pages[0].ID != "a3f7b2c01d9d" {
		t.Fatalf("page id = %q, want a3f7b2c01d9d", pages[0].ID)
	}
}

type fakeIndexReader struct {
	files    map[string][]byte
	readPath string
}

func (r *fakeIndexReader) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	r.readPath = relPath
	data, ok := r.files[relPath]
	if !ok {
		return nil, storage.ErrObjectNotExist
	}
	return data, nil
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

	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
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

func (r *handlerCacheReader) ReadFile(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("no JSONL in tests — use ListConcepts path")
}

func (r *handlerCacheReader) WriteBytes(_ context.Context, data []byte, _ string) (string, error) {
	return "ok", nil
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

	contexts := cachedContexts(conceptCache, reader, []search.Result{{
		Slug:  "alpha",
		Title: "Alpha Concept",
		Type:  "concept",
	}})

	if len(contexts) != 1 {
		t.Fatalf("len(contexts) = %d, want 1", len(contexts))
	}
	if !strings.Contains(contexts[0], "Sources: [Source One, Source Two]") {
		t.Fatalf("context missing sources:\n%s", contexts[0])
	}
	if !strings.Contains(contexts[0], "Alpha body.") {
		t.Fatalf("context missing body:\n%s", contexts[0])
	}
}

type fakeWikiListReader struct {
	cacheConcepts []gcs.WikiPage
	gcsConcepts   []gcs.WikiPage
	cacheSources  []gcs.WikiPage
	gcsSources    []gcs.WikiPage

	cacheConceptsErr error
	gcsConceptsErr   error
	cacheSourcesErr  error
	gcsSourcesErr    error

	cacheConceptCalls int
	gcsConceptCalls   int
	cacheSourceCalls  int
	gcsSourceCalls    int
}

func (r *fakeWikiListReader) ListConceptsFromCache(context.Context) ([]gcs.WikiPage, error) {
	r.cacheConceptCalls++
	return r.cacheConcepts, r.cacheConceptsErr
}

func (r *fakeWikiListReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	r.gcsConceptCalls++
	return r.gcsConcepts, r.gcsConceptsErr
}

func (r *fakeWikiListReader) ListSourcesFromCache(context.Context) ([]gcs.WikiPage, error) {
	r.cacheSourceCalls++
	return r.cacheSources, r.cacheSourcesErr
}

func (r *fakeWikiListReader) ListSources(context.Context) ([]gcs.WikiPage, error) {
	r.gcsSourceCalls++
	return r.gcsSources, r.gcsSourcesErr
}

func TestListConceptsCacheFirstUsesCacheWithoutFallback(t *testing.T) {
	reader := &fakeWikiListReader{
		cacheConcepts: []gcs.WikiPage{{Slug: "cached-concept"}},
		gcsConcepts:   []gcs.WikiPage{{Slug: "gcs-concept"}},
	}

	pages, err := listConceptsCacheFirst(context.Background(), reader, true)
	if err != nil {
		t.Fatalf("listConceptsCacheFirst: %v", err)
	}

	if len(pages) != 1 || pages[0].Slug != "cached-concept" {
		t.Fatalf("pages = %#v, want cached concept", pages)
	}
	if reader.cacheConceptCalls != 1 || reader.gcsConceptCalls != 0 {
		t.Fatalf("cache calls = %d, gcs calls = %d; want 1, 0", reader.cacheConceptCalls, reader.gcsConceptCalls)
	}
}

func TestListConceptsCacheFirstFallsBackWhenCacheMissing(t *testing.T) {
	reader := &fakeWikiListReader{
		cacheConceptsErr: storage.ErrObjectNotExist,
		gcsConcepts:      []gcs.WikiPage{{Slug: "gcs-concept"}},
	}

	pages, err := listConceptsCacheFirst(context.Background(), reader, true)
	if err != nil {
		t.Fatalf("listConceptsCacheFirst: %v", err)
	}

	if len(pages) != 1 || pages[0].Slug != "gcs-concept" {
		t.Fatalf("pages = %#v, want GCS fallback concept", pages)
	}
	if reader.cacheConceptCalls != 1 || reader.gcsConceptCalls != 1 {
		t.Fatalf("cache calls = %d, gcs calls = %d; want 1, 1", reader.cacheConceptCalls, reader.gcsConceptCalls)
	}
}

func TestListSourcesCacheFirstFallsBackWhenCacheMissing(t *testing.T) {
	reader := &fakeWikiListReader{
		cacheSourcesErr: storage.ErrObjectNotExist,
		gcsSources:      []gcs.WikiPage{{Slug: "gcs-source"}},
	}

	pages, err := listSourcesCacheFirst(context.Background(), reader)
	if err != nil {
		t.Fatalf("listSourcesCacheFirst: %v", err)
	}

	if len(pages) != 1 || pages[0].Slug != "gcs-source" {
		t.Fatalf("pages = %#v, want GCS fallback source", pages)
	}
	if reader.cacheSourceCalls != 1 || reader.gcsSourceCalls != 1 {
		t.Fatalf("cache calls = %d, gcs calls = %d; want 1, 1", reader.cacheSourceCalls, reader.gcsSourceCalls)
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
			return testHTTPResponse(http.StatusOK, `{
				"metadata": {
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-abc123"
				}
			}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	// Project comes from ProjectMiddleware context (X-Project-ID), not request body.
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "accepted" || body["command"] != "run" || body["project_id"] != "demo" || body["execution_id"] != "olw-pipeline-abc123" {
		t.Fatalf("body = %#v", body)
	}
	if _, hasLegacyProject := body["project"]; hasLegacyProject {
		t.Fatalf("body still has legacy project field: %#v", body)
	}
	quota, ok := body["quota"].(map[string]any)
	if !ok {
		t.Fatalf("quota missing or wrong type: %#v", body["quota"])
	}
	if enforced, _ := quota["enforced"].(bool); enforced {
		t.Fatalf("expected enforced=false without quota store, got %#v", quota)
	}
	want := map[string]any{
		"overrides": map[string]any{
			"containerOverrides": []any{
				map[string]any{
					"args": []any{"run", defaultWorkerCommands},
					"env": []any{
						map[string]any{"name": "USER_ID", "value": "request-user"},
						map[string]any{"name": "PROJECT_ID", "value": "demo"},
						map[string]any{"name": "TASK_TYPE", "value": "pipeline"},
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
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			if err := json.NewDecoder(r.Body).Decode(&runRequest); err != nil {
				t.Errorf("decode run request: %v", err)
			}
			return testHTTPResponse(http.StatusOK, `{
				"metadata": {
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-default"
				}
			}`), nil
		default:
			// evaluateQuota → isPipelineRunning probes executions list.
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", nil)
	c.Set("projectID", "demo")
	c.Set("userID", "request-user")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["execution_id"] != "olw-pipeline-default" {
		t.Fatalf("execution_id = %q, want olw-pipeline-default", body["execution_id"])
	}
	if body["project_id"] != "demo" {
		t.Fatalf("project_id = %q, want demo", body["project_id"])
	}
	override := runRequest.Overrides.ContainerOverrides[0]
	if len(override.Args) != 2 || override.Args[0] != "run" || override.Args[1] != defaultWorkerCommands {
		t.Fatalf("args = %#v, want [run %s]", override.Args, defaultWorkerCommands)
	}
	if override.Env[0].Value != "request-user" || override.Env[1].Value != "demo" || override.Env[2].Value != "pipeline" {
		t.Fatalf("env = %#v", override.Env)
	}
}

func TestDefaultWorkerCommandsRunsPipelineWithoutInit(t *testing.T) {
	var commands [][]string
	if err := json.Unmarshal([]byte(defaultWorkerCommands), &commands); err != nil {
		t.Fatalf("decode default worker commands: %v", err)
	}
	if len(commands) == 0 {
		t.Fatal("default worker commands are empty")
	}
	want := []string{"run", "--auto-approve"}
	if len(commands[0]) != len(want) {
		t.Fatalf("first command = %#v, want %#v", commands[0], want)
	}
	for i := range want {
		if commands[0][i] != want[i] {
			t.Fatalf("first command = %#v, want %#v", commands[0], want)
		}
	}
}

func TestAdminPipelineTriggerInvokesWorkerWithoutImmediateRebuild(t *testing.T) {
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
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			if err := json.NewDecoder(r.Body).Decode(&runRequest); err != nil {
				t.Errorf("decode run request: %v", err)
			}
			return testHTTPResponse(http.StatusOK, `{
				"metadata": {
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-admin"
				}
			}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
		projectExists: func(context.Context, string) error {
			return nil
		},
		rebuildIndex: func(context.Context, string, string) (idMap, error) {
			t.Fatal("AdminPipelineTrigger must not rebuild index immediately")
			return idMap{}, nil
		},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/projects/request-user_demo/pipeline", nil)
	c.Params = gin.Params{{Key: "id", Value: "request-user_demo"}}

	h.AdminPipelineTrigger(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" || body["execution_id"] != "olw-pipeline-admin" {
		t.Fatalf("body = %#v", body)
	}
	override := runRequest.Overrides.ContainerOverrides[0]
	if len(override.Args) != 2 || override.Args[0] != "run" || override.Args[1] != defaultWorkerCommands {
		t.Fatalf("args = %#v, want [run %s]", override.Args, defaultWorkerCommands)
	}
	if override.Env[0].Value != "request-user" || override.Env[1].Value != "demo" || override.Env[2].Value != "pipeline" {
		t.Fatalf("env = %#v", override.Env)
	}
}

func TestPipelineRunRequiresProject(t *testing.T) {
	h := &Handler{index: search.NewIndex()}
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
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			return testHTTPResponse(http.StatusForbidden, "permission denied\n"), nil
		default:
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", nil)
	c.Set("projectID", "demo")
	c.Set("userID", "request-user")

	h.PipelineRun(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "pipeline failed: permission denied") {
		t.Fatalf("body = %s", body)
	}
}

// stubQuotaStore is an in-memory pipelineQuotaStore for handler tests.
type stubQuotaStore struct {
	runsToday    int
	dayKey       string
	lastRunAt    time.Time
	reserveCalls int
	refundCalls  int
	lastRefund   firestore.QuotaPrev
}

func (s *stubQuotaStore) LoadQuotaState(context.Context, string, string) (int, string, time.Time, error) {
	return s.runsToday, s.dayKey, s.lastRunAt, nil
}

func (s *stubQuotaStore) ReserveQuota(
	_ context.Context,
	_, _ string,
	limits pipelinequota.Limits,
	now time.Time,
	isDemo, alreadyRunning bool,
	newRawFiles, rawDirtyFiles, annotationDirtyFiles int,
) (prev firestore.QuotaPrev, snap pipelinequota.Snapshot, reserved bool, err error) {
	s.reserveCalls++
	now = now.UTC()
	prev = firestore.QuotaPrev{
		RunsToday: s.runsToday,
		DayKey:    s.dayKey,
		LastRunAt: s.lastRunAt,
	}
	pre := pipelinequota.Evaluate(pipelinequota.Input{
		Now:                  now,
		Limits:               limits,
		IsDemo:               isDemo,
		AlreadyRunning:       alreadyRunning,
		RunsToday:            s.runsToday,
		DayKey:               s.dayKey,
		LastRunAt:            s.lastRunAt,
		NewRawFiles:          newRawFiles,
		RawDirtyFiles:        rawDirtyFiles,
		AnnotationDirtyFiles: annotationDirtyFiles,
		Enforced:             true,
	})
	if !pre.Allowed {
		return prev, pre, false, nil
	}
	today := pipelinequota.DayKeyUTC(now)
	s.runsToday = pre.RunsToday + 1
	s.dayKey = today
	s.lastRunAt = now
	snap = pipelinequota.Evaluate(pipelinequota.Input{
		Now:                  now,
		Limits:               limits,
		IsDemo:               isDemo,
		AlreadyRunning:       alreadyRunning,
		RunsToday:            s.runsToday,
		DayKey:               s.dayKey,
		LastRunAt:            s.lastRunAt,
		NewRawFiles:          newRawFiles,
		RawDirtyFiles:        rawDirtyFiles,
		AnnotationDirtyFiles: annotationDirtyFiles,
		Enforced:             true,
	})
	return prev, snap, true, nil
}

func (s *stubQuotaStore) RefundQuotaPrev(_ context.Context, _, _ string, prev firestore.QuotaPrev) error {
	s.refundCalls++
	s.lastRefund = prev
	s.runsToday = prev.RunsToday
	s.dayKey = prev.DayKey
	s.lastRunAt = prev.LastRunAt
	return nil
}

func pipelineRunHTTPClient(t *testing.T, runHits *int) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			if runHits != nil {
				*runHits++
			}
			return testHTTPResponse(http.StatusOK, `{
				"metadata": {
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-ok"
				}
			}`), nil
		default:
			// Executions list used by isPipelineRunning — empty so not RUNNING.
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		}
	})}
}

func TestPipelineRunBlocksDemoUser(t *testing.T) {
	var runHits int
	stub := &stubQuotaStore{}
	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       pipelineRunHTTPClient(t, &runHits),
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	h.SetPipelineQuotaConfig(2, 3600, 1, []string{"demo-user"})
	h.SetPipelineQuotaStore(stub)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", nil)
	c.Set("userID", "demo-user")
	c.Set("projectID", "proj-1")

	h.PipelineRun(c)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if runHits != 0 {
		t.Fatalf("Cloud Run /run hit %d times, want 0", runHits)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "pipeline blocked: demo" {
		t.Fatalf("error = %#v", body["error"])
	}
	quota, _ := body["quota"].(map[string]any)
	if quota["reason"] != "demo" || quota["allowed"] != false {
		t.Fatalf("quota = %#v", quota)
	}
}

func TestPipelineRunBlocksDailyLimit(t *testing.T) {
	var runHits int
	now := time.Now().UTC()
	stub := &stubQuotaStore{
		runsToday: 2,
		dayKey:    pipelinequota.DayKeyUTC(now),
		lastRunAt: now.Add(-2 * time.Hour),
	}
	// Provide a raw file so no_new_raw is not the blocking reason.
	root := t.TempDir()
	rawDir := filepath.Join(root, "users", "request-user", "projects", "proj-1", "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		store:            localfs.New(root),
		index:            search.NewIndex(),
		httpClient:       pipelineRunHTTPClient(t, &runHits),
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	h.SetPipelineQuotaConfig(2, 3600, 1, nil)
	h.SetPipelineQuotaStore(stub)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "proj-1")

	h.PipelineRun(c)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
	if runHits != 0 {
		t.Fatalf("Cloud Run /run hit %d times, want 0", runHits)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "pipeline blocked: daily_limit" {
		t.Fatalf("error = %#v", body["error"])
	}
	quota, _ := body["quota"].(map[string]any)
	if quota["reason"] != "daily_limit" {
		t.Fatalf("quota = %#v", quota)
	}
	if stub.reserveCalls != 1 {
		t.Fatalf("reserveCalls = %d, want 1", stub.reserveCalls)
	}
	if stub.refundCalls != 0 {
		t.Fatalf("refundCalls = %d, want 0", stub.refundCalls)
	}
}

func TestPipelineRunRefundsOnInvokeFailure(t *testing.T) {
	now := time.Now().UTC()
	stub := &stubQuotaStore{
		runsToday: 0,
		dayKey:    pipelinequota.DayKeyUTC(now),
	}
	root := t.TempDir()
	rawDir := filepath.Join(root, "users", "request-user", "projects", "proj-1", "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			return testHTTPResponse(http.StatusForbidden, "permission denied\n"), nil
		default:
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		}
	})}

	h := &Handler{
		store:            localfs.New(root),
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	h.SetPipelineQuotaConfig(2, 3600, 1, nil)
	h.SetPipelineQuotaStore(stub)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "proj-1")

	h.PipelineRun(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if stub.reserveCalls != 1 {
		t.Fatalf("reserveCalls = %d, want 1", stub.reserveCalls)
	}
	if stub.refundCalls != 1 {
		t.Fatalf("refundCalls = %d, want 1", stub.refundCalls)
	}
	if stub.runsToday != 0 {
		t.Fatalf("runsToday after refund = %d, want 0", stub.runsToday)
	}
}

func TestPipelineStatusIncludesSuggestedQueries(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	if err := os.MkdirAll(filepath.Join(projectRoot, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	queriesJSON := `{"queries":["Beta","Alpha"],"updated_at":"2026-07-10T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(projectRoot, "cache", "suggested_queries.json"), []byte(queriesJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{}`), nil
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		SuggestedQueries []string `json:"suggested_queries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.SuggestedQueries) != 2 || body.SuggestedQueries[0] != "Beta" {
		t.Fatalf("suggested_queries = %#v, want [Beta Alpha]", body.SuggestedQueries)
	}
}

func TestPipelineStatusReturnsEmptySuggestedQueriesWhenArtifactMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "users", "request-user", "projects", "demo-project"), 0o755); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{}`), nil
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		SuggestedQueries []string `json:"suggested_queries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.SuggestedQueries == nil {
		t.Fatal("suggested_queries = nil, want empty slice")
	}
	if len(body.SuggestedQueries) != 0 {
		t.Fatalf("suggested_queries = %#v, want []", body.SuggestedQueries)
	}
}

func TestPipelineStatusIncludesQuota(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{}`), nil
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "new-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		ProjectID string                  `json:"project_id"`
		Quota     *pipelinequota.Snapshot `json:"quota"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Quota == nil {
		t.Fatal("quota is nil")
	}
	if body.Quota.Enforced {
		t.Fatalf("expected enforced=false without store, got %+v", body.Quota)
	}
	if !body.Quota.Allowed {
		t.Fatalf("expected allowed when unenforced, got %+v", body.Quota)
	}
}

func TestPipelineStatusAlreadyRunningFalseAfterSucceeded(t *testing.T) {
	stub := &stubQuotaStore{
		runsToday: 0,
		dayKey:    "2026-07-10",
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions":
			return testHTTPResponse(http.StatusOK, `{"executions":[`+pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-done", "request-user", "demo-project", "pipeline")+`]}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
	}
	h.SetPipelineQuotaConfig(2, 3600, 1, nil)
	h.SetPipelineQuotaStore(stub)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			Status string `json:"status"`
		} `json:"last_execution"`
		Quota *pipelinequota.Snapshot `json:"quota"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LastExecution == nil || body.LastExecution.Status != "SUCCEEDED" {
		t.Fatalf("last_execution = %#v", body.LastExecution)
	}
	if body.Quota == nil {
		t.Fatal("quota is nil")
	}
	if body.Quota.AlreadyRunning {
		t.Fatalf("quota.already_running must be false after SUCCEEDED, got %+v", body.Quota)
	}
}

func TestAdminPipelineTriggerBlocksAlreadyRunning(t *testing.T) {
	var runHits int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/run":
			runHits++
			return testHTTPResponse(http.StatusOK, `{
				"metadata": {
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-admin"
				}
			}`), nil
		default:
			// isPipelineRunning lists executions and sees RUNNING.
			return testHTTPResponse(http.StatusOK, `{"executions":[{
				"name":"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-running",
				"runningCount":1,
				"conditions":[{"type":"Completed","state":"CONDITION_RECONCILING"}],
				"template":{"containers":[{"env":[
					{"name":"USER_ID","value":"request-user"},
					{"name":"PROJECT_ID","value":"demo"},
					{"name":"TASK_TYPE","value":"pipeline"}
				]}]}
			}]}`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
		projectExists: func(context.Context, string) error {
			return nil
		},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/projects/request-user_demo/pipeline", nil)
	c.Params = gin.Params{{Key: "id", Value: "request-user_demo"}}

	h.AdminPipelineTrigger(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if runHits != 0 {
		t.Fatalf("Cloud Run /run hit %d times, want 0", runHits)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "already running") {
		t.Fatalf("body = %s", body)
	}
}

func TestPipelineStatusReturnsLatestExecution(t *testing.T) {
	var executionRequest *http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			if got := r.Header.Get("Metadata-Flavor"); got != "Google" {
				t.Errorf("Metadata-Flavor = %q, want Google", got)
			}
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions":
			executionRequest = r
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("Authorization = %q, want Bearer test-token", got)
			}
			return testHTTPResponse(http.StatusOK, `{"executions":[`+pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-1", "request-user", "demo-project", "pipeline")+`]}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if executionRequest == nil {
		t.Fatalf("Cloud Run executions request was not made")
	}
	if got := executionRequest.URL.Query().Get("pageSize"); got != "20" {
		t.Fatalf("pageSize = %q, want 20", got)
	}
	var body struct {
		ProjectID     string `json:"project_id"`
		LastExecution *struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Duration  string `json:"duration"`
			LogURL    string `json:"log_url"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ProjectID != "demo-project" {
		t.Fatalf("project_id = %q, want demo-project", body.ProjectID)
	}
	if body.LastExecution == nil {
		t.Fatalf("last_execution = nil, want execution")
	}
	if body.LastExecution.Name != "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-1" ||
		body.LastExecution.Status != "SUCCEEDED" ||
		body.LastExecution.StartTime != "2026-06-29T01:02:03Z" ||
		body.LastExecution.EndTime != "2026-06-29T01:02:13Z" ||
		body.LastExecution.Duration != "10s" {
		t.Fatalf("last_execution = %#v", body.LastExecution)
	}
}

func pipelineOwnershipExecution(name, userID, projectID, taskType string) string {
	return fmt.Sprintf(`{
		"name": %q,
		"startTime": "2026-06-29T01:02:03Z",
		"completionTime": "2026-06-29T01:02:13Z",
		"completionStatus": "EXECUTION_SUCCEEDED",
		"template": {"containers": [{"env": [
			{"name": "USER_ID", "value": %q},
			{"name": "PROJECT_ID", "value": %q},
			{"name": "TASK_TYPE", "value": %q}
		]}]}
	}`, name, userID, projectID, taskType)
}

func pipelineRunningOwnershipExecution(name, userID, projectID string) string {
	return fmt.Sprintf(`{
		"name": %q,
		"runningCount": 1,
		"conditions": [{"type": "Completed", "state": "CONDITION_RECONCILING"}],
		"template": {"containers": [{"env": [
			{"name": "USER_ID", "value": %q},
			{"name": "PROJECT_ID", "value": %q},
			{"name": "TASK_TYPE", "value": "pipeline"}
		]}]}
	}`, name, userID, projectID)
}

func newPipelineOwnershipHandler(client *http.Client) *Handler {
	return &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
	}
}

func TestPipelineOwnedExecutionActivityFindsOlderRunningExecutionOnSamePage(t *testing.T) {
	terminal := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/newer-terminal", "request-user", "demo-project", "pipeline")
	running := pipelineRunningOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/older-running", "request-user", "demo-project")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+terminal+`,`+running+`]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	hasOwned, allTerminal, anyRunning, err := h.pipelineOwnedExecutionActivityForOwner(t.Context(), "request-user", "demo-project")
	if err != nil {
		t.Fatalf("pipelineOwnedExecutionActivityForOwner() error = %v", err)
	}
	if !hasOwned || allTerminal || !anyRunning {
		t.Fatalf("activity = hasOwned:%t allTerminal:%t anyRunning:%t", hasOwned, allTerminal, anyRunning)
	}
	if !pipelineRunningForOwnedActivity(false, hasOwned, allTerminal, anyRunning) {
		t.Fatal("older RUNNING execution must report already running")
	}
}

func TestPipelineOwnedExecutionActivityFindsOlderRunningExecutionAcrossPages(t *testing.T) {
	terminal := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/newer-terminal-page", "request-user", "demo-project", "pipeline")
	running := pipelineRunningOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/older-running-page", "request-user", "demo-project")
	var pageTokens []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		pageToken := r.URL.Query().Get("pageToken")
		pageTokens = append(pageTokens, pageToken)
		if pageToken == "page-2" {
			return testHTTPResponse(http.StatusOK, `{"executions":[`+running+`]}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+terminal+`],"nextPageToken":"page-2"}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	hasOwned, allTerminal, anyRunning, err := h.pipelineOwnedExecutionActivityForOwner(t.Context(), "request-user", "demo-project")
	if err != nil {
		t.Fatalf("pipelineOwnedExecutionActivityForOwner() error = %v", err)
	}
	if !hasOwned || allTerminal || !anyRunning {
		t.Fatalf("activity = hasOwned:%t allTerminal:%t anyRunning:%t", hasOwned, allTerminal, anyRunning)
	}
	if len(pageTokens) != 2 || pageTokens[1] != "page-2" {
		t.Fatalf("page tokens = %#v, want second page", pageTokens)
	}
}

func TestPipelineOwnedExecutionActivityAllTerminalOverridesLock(t *testing.T) {
	terminal := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/terminal", "request-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+terminal+`]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	hasOwned, allTerminal, anyRunning, err := h.pipelineOwnedExecutionActivityForOwner(t.Context(), "request-user", "demo-project")
	if err != nil {
		t.Fatalf("pipelineOwnedExecutionActivityForOwner() error = %v", err)
	}
	if !hasOwned || !allTerminal || anyRunning {
		t.Fatalf("activity = hasOwned:%t allTerminal:%t anyRunning:%t", hasOwned, allTerminal, anyRunning)
	}
	if pipelineRunningForOwnedActivity(true, hasOwned, allTerminal, anyRunning) {
		t.Fatal("terminal owned history must override a stale lock")
	}
}

func TestPipelineOwnedExecutionActivityWithoutOwnerFallsBackToLock(t *testing.T) {
	foreign := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/foreign", "other-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+foreign+`]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	hasOwned, allTerminal, anyRunning, err := h.pipelineOwnedExecutionActivityForOwner(t.Context(), "request-user", "demo-project")
	if err != nil {
		t.Fatalf("pipelineOwnedExecutionActivityForOwner() error = %v", err)
	}
	if hasOwned || allTerminal || anyRunning {
		t.Fatalf("activity = hasOwned:%t allTerminal:%t anyRunning:%t", hasOwned, allTerminal, anyRunning)
	}
	if !pipelineRunningForOwnedActivity(true, hasOwned, allTerminal, anyRunning) {
		t.Fatal("missing owned history must fall back to active lock")
	}
}

func TestPipelineStatusFiltersInterleavedTenants(t *testing.T) {
	foreign := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/foreign", "other-user", "demo-project", "pipeline")
	own := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/own", "request-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+foreign+`,`+own+`]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "/executions/own") {
		t.Fatalf("body = %s, want matching tenant execution", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "/executions/foreign") {
		t.Fatalf("body = %s, leaked foreign execution", recorder.Body.String())
	}
}

func TestPipelineStatusPaginatesUntilOwnedExecution(t *testing.T) {
	foreign := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/foreign-page", "other-user", "demo-project", "pipeline")
	own := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/own-page", "request-user", "demo-project", "pipeline")
	var pageTokens []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		pageToken := r.URL.Query().Get("pageToken")
		pageTokens = append(pageTokens, pageToken)
		if pageToken == "page-2" {
			return testHTTPResponse(http.StatusOK, `{"executions":[`+own+`]}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+foreign+`],"nextPageToken":"page-2"}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "/executions/own-page") {
		t.Fatalf("body = %s, want paginated matching execution", recorder.Body.String())
	}
	if len(pageTokens) < 2 || pageTokens[1] != "page-2" {
		t.Fatalf("page tokens = %#v, want a follow-up request with page-2", pageTokens)
	}
}

func TestPipelineStatusReturnsNullWhenNoOwnedExecutionMatches(t *testing.T) {
	defaultEnv := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/default", "", "", "pipeline")
	foreign := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/foreign-only", "other-user", "other-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+defaultEnv+`,`+foreign+`]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"last_execution":null`) {
		t.Fatalf("body = %s, want null last_execution", recorder.Body.String())
	}
}

func TestPipelineStatusRejectsSplitContainerOwnership(t *testing.T) {
	splitOwnership := `{
		"name":"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/split-env",
		"template":{"containers":[
			{"env":[{"name":"USER_ID","value":"request-user"}]},
			{"env":[
				{"name":"PROJECT_ID","value":"demo-project"},
				{"name":"TASK_TYPE","value":"pipeline"}
			]}
		]}
	}`
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[`+splitOwnership+`]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"last_execution":null`) {
		t.Fatalf("body = %s, split-container env must not authorize", recorder.Body.String())
	}
}

func TestPipelineStatusRejectsUnsafeExplicitExecutionIDsBeforeOutboundRequests(t *testing.T) {
	for _, executionID := range []string{".", "..", "../escape", `bad\\path`, "bad%00id"} {
		t.Run(executionID, func(t *testing.T) {
			var outboundRequests int
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				outboundRequests++
				return testHTTPResponse(http.StatusNotFound, `not found`), nil
			})}

			h := newPipelineOwnershipHandler(client)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id="+executionID, nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo-project")

			h.PipelineStatus(c)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if outboundRequests != 0 {
				t.Fatalf("outbound requests = %d, want 0", outboundRequests)
			}
		})
	}
}

func TestPipelineStatusRejectsForeignExplicitExecution(t *testing.T) {
	foreign := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/foreign-explicit", "other-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "/executions/foreign-explicit") {
			return testHTTPResponse(http.StatusOK, foreign), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id=foreign-explicit", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "foreign-explicit") {
		t.Fatalf("body = %s, leaked execution identity", recorder.Body.String())
	}
}

func TestPipelineStatusReturns404ForUnknownExplicitExecution(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusNotFound, `not found`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id=unknown-execution", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "unknown-execution") {
		t.Fatalf("body = %s, leaked execution identity", recorder.Body.String())
	}
}

func TestPipelineStatusReturnsOwnedExplicitExecution(t *testing.T) {
	owned := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned-explicit", "request-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "/executions/owned-explicit") {
			return testHTTPResponse(http.StatusOK, owned), nil
		}
		return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
	})}

	h := newPipelineOwnershipHandler(client)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id=owned-explicit", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "/executions/owned-explicit") {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestPipelineLogRejectsForeignExecution(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-foreign-log.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("must not leak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/foreign-log", "other-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, foreign), nil
	})}

	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=foreign-log", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineLog(c)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "must not leak") {
		t.Fatalf("body = %s, read foreign log", recorder.Body.String())
	}
}

func TestPipelineLogReturnsOwnedExecutionLog(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-owned-log.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("owned output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owned := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned-log", "request-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, owned), nil
	})}

	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=owned-log", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineLog(c)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "owned output\n" {
		t.Fatalf("status = %d; body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestPipelineStatusReturnsSpecificExecution(t *testing.T) {
	var executionRequest *http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-abc123":
			executionRequest = r
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("Authorization = %q, want Bearer test-token", got)
			}
			return testHTTPResponse(http.StatusOK, `{
				"name": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-abc123",
				"startTime": "2026-06-29T02:00:00Z",
				"completionTime": "2026-06-29T02:00:07Z",
				"completionStatus": "EXECUTION_SUCCEEDED",
				"template": {"containers": [{"env": [
					{"name": "USER_ID", "value": "request-user"},
					{"name": "PROJECT_ID", "value": "demo-project"},
					{"name": "TASK_TYPE", "value": "pipeline"}
				]}]}
			}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id=olw-pipeline-abc123", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if executionRequest == nil {
		t.Fatalf("Cloud Run execution request was not made")
	}
	if got := executionRequest.URL.RawQuery; got != "" {
		t.Fatalf("query = %q, want empty", got)
	}
	var body struct {
		ProjectID     string `json:"project_id"`
		LastExecution *struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Duration  string `json:"duration"`
			LogURL    string `json:"log_url"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ProjectID != "demo-project" {
		t.Fatalf("project_id = %q, want demo-project", body.ProjectID)
	}
	if body.LastExecution == nil {
		t.Fatalf("last_execution = nil, want execution")
	}
	if body.LastExecution.Name != "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-abc123" ||
		body.LastExecution.Status != "SUCCEEDED" ||
		body.LastExecution.StartTime != "2026-06-29T02:00:00Z" ||
		body.LastExecution.EndTime != "2026-06-29T02:00:07Z" ||
		body.LastExecution.Duration != "7s" {
		t.Fatalf("last_execution = %#v", body.LastExecution)
	}
	if body.LastExecution.LogURL != "/api/v1/pipeline/log?execution_id=olw-pipeline-abc123" {
		t.Fatalf("log_url = %q, want pipeline log URL", body.LastExecution.LogURL)
	}
}

func TestPipelineLogReturnsProjectScopedLog(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-olw-pipeline-abc123.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("pipeline output\nstderr output\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	owned := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-abc123", "request-user", "demo-project", "pipeline")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, owned), nil
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=olw-pipeline-abc123", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineLog(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", contentType)
	}
	if body := recorder.Body.String(); body != "pipeline output\nstderr output\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestPipelineLogRejectsUnsafeExecutionID(t *testing.T) {
	h := New(localfs.New(t.TempDir()), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=../escape", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineLog(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestPipelineStatusReturnsNullWhenNoExecutions(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{}`), nil
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "new-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"project_id":"new-project"`) || !strings.Contains(body, `"last_execution":null`) {
		t.Fatalf("body = %s", body)
	}
}

func TestStatusIncludesLatestPipelineExecutionWhenAvailable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "users", "request-user", "projects", "demo-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/token":
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		case "/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions":
			return testHTTPResponse(http.StatusOK, `{
				"executions": [{
					"name": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-1",
					"startTime": "2026-06-29T01:02:03Z",
					"completionTime": "2026-06-29T01:02:13Z",
					"completionStatus": "EXECUTION_SUCCEEDED"
				}]
			}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, `not found`), nil
		}
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.Status(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			Status string `json:"status"`
			LogURL string `json:"log_url"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.LastExecution == nil {
		t.Fatal("last_execution = nil, want latest execution")
	}
	if body.LastExecution.Status != "SUCCEEDED" || body.LastExecution.LogURL != "/api/v1/pipeline/log?execution_id=exec-1" {
		t.Fatalf("last_execution = %#v", body.LastExecution)
	}
}

func TestStatusRawCountUsesLiveRawListing(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	if err := os.MkdirAll(filepath.Join(projectRoot, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stale artifact must not win over live raw/ files (LWC-129).
	statusJSON := `{"version":1,"generated_at":"2026-07-09T10:00:00Z","file_count":2,"files":{}}`
	if err := os.WriteFile(filepath.Join(projectRoot, "cache", "raw_status.json"), []byte(statusJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(projectRoot, "raw", name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.Status(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		RawCount int `json:"raw_count"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.RawCount != 3 {
		t.Fatalf("raw_count = %d, want 3 (live list, not artifact file_count=2)", body.RawCount)
	}
}

func TestStatusIgnoresPipelineExecutionLookupFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "users", "request-user", "projects", "demo-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusInternalServerError, `metadata unavailable`), nil
		}
		return testHTTPResponse(http.StatusNotFound, `not found`), nil
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.Status(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if body := recorder.Body.String(); strings.Contains(body, "last_execution") {
		t.Fatalf("body should omit last_execution on lookup failure: %s", body)
	}
}

func TestInitProjectReadyDefaults(t *testing.T) {
	resp := newInitProjectResponse("a1b2c3d4e5f6", "Demo Project")
	if resp.Status != "ready" {
		t.Fatalf("response status = %q, want ready", resp.Status)
	}
	if resp.StatusURL != "/api/v1/projects/a1b2c3d4e5f6/status" {
		t.Fatalf("status URL = %q", resp.StatusURL)
	}

	data := initProjectData("a1b2c3d4e5f6", "Demo Project", "idem-1", time.Unix(1, 0))
	if data["status"] != "ready" {
		t.Fatalf("project data status = %q, want ready", data["status"])
	}
	if data["idempotency_key"] != "idem-1" {
		t.Fatalf("idempotency key = %q", data["idempotency_key"])
	}
}

func TestRebuildIndexSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{
		index: search.NewIndex(),
		rebuildIndex: func(context.Context, string, string) (idMap, error) {
			return idMap{
				Concept:   map[string]string{"a3f7b2c01d9d": "canonical-slug"},
				Source:    map[string]string{},
				Redirects: map[string][]string{},
			}, nil
		},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/rebuild-index", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RebuildIndex(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Entries struct {
			Concept int `json:"concept"`
			Source  int `json:"source"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" || body.Entries.Concept != 1 || body.Entries.Source != 0 {
		t.Fatalf("body = %#v", body)
	}
}

func TestRebuildIndexSkipsFilesWithoutID(t *testing.T) {
	store := &fakeIDMapStore{
		files: map[string][]gcs.MarkdownFile{
			"wiki/": {
				{
					Slug: "with-id",
					Data: []byte("---\nid: a3f7b2c01d9d\ntitle: With ID\n---\nBody"),
				},
				{
					Slug: "without-id",
					Data: []byte("---\ntitle: Without ID\n---\nBody"),
				},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}

	next, err := rebuildIndex(context.Background(), store)
	if err != nil {
		t.Fatalf("rebuild index: %v", err)
	}

	if len(next.Concept) != 2 || next.Concept["a3f7b2c01d9d"] != "with-id" {
		t.Fatalf("concept map = %#v, want 2 entries including with-id", next.Concept)
	}
	// Verify auto-generated ID for file without frontmatter id
	foundAuto := false
	for id, slug := range next.Concept {
		if slug == "without-id" && id != "" && id != "a3f7b2c01d9d" {
			foundAuto = true
			break
		}
	}
	if !foundAuto {
		t.Fatalf("expected auto-generated id for 'without-id', got: %#v", next.Concept)
	}
	if _, ok := next.Concept[""]; ok {
		t.Fatal("concept with empty id was included")
	}
}

func TestRebuildIndexPreservesRedirects(t *testing.T) {
	oldMap := idMap{
		Concept: map[string]string{"a3f7b2c01d9d": "old-slug"},
		Source:  map[string]string{},
		Redirects: map[string][]string{
			"a3f7b2c01d9d": {"legacy-slug"},
		},
	}
	oldJSON, err := json.Marshal(oldMap)
	if err != nil {
		t.Fatalf("marshal old id map: %v", err)
	}
	store := &fakeIDMapStore{
		files: map[string][]gcs.MarkdownFile{
			"wiki/": {
				{
					Slug: "new-slug",
					Data: []byte("---\nid: a3f7b2c01d9d\ntitle: New Slug\n---\nBody"),
				},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{idMapPath: oldJSON},
	}

	next, err := rebuildIndex(context.Background(), store)
	if err != nil {
		t.Fatalf("rebuild index: %v", err)
	}

	want := []string{"legacy-slug", "old-slug"}
	if got := next.Redirects["a3f7b2c01d9d"]; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("redirects = %#v, want %#v", got, want)
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
	h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
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
		h.PipelineStatus,
		h.PipelineLog,
		h.RebuildIndex,
		h.InitProject,
		h.ProjectStatus,
	}
	if len(handlers) != 15 {
		t.Fatalf("handler count = %d, want 15", len(handlers))
	}
}
