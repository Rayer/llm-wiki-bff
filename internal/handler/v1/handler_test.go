package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
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
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo","command":"sync"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "accepted" || body["command"] != "run" || body["project"] != "demo" || body["execution_id"] != "olw-pipeline-abc123" {
		t.Fatalf("body = %#v", body)
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
						map[string]any{"name": "WORKSPACE", "value": "true"},
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
		return testHTTPResponse(http.StatusOK, `{
			"metadata": {
				"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-default"
			}
		}`), nil
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("projectID", "demo")
	c.Set("userID", "request-user")

	h.PipelineRun(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["execution_id"] != "olw-pipeline-default" {
		t.Fatalf("execution_id = %q, want olw-pipeline-default", body["execution_id"])
	}
	override := runRequest.Overrides.ContainerOverrides[0]
	if len(override.Args) != 2 || override.Args[0] != "run" || override.Args[1] != defaultWorkerCommands {
		t.Fatalf("args = %#v, want [run %s]", override.Args, defaultWorkerCommands)
	}
	if override.Env[0].Value != "request-user" || override.Env[1].Value != "demo" || override.Env[2].Value != "pipeline" || override.Env[3].Value != "true" {
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
	if override.Env[0].Value != "request-user" || override.Env[1].Value != "demo" || override.Env[2].Value != "pipeline" || override.Env[3].Value != "true" {
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
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusForbidden, "permission denied\n"), nil
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run", strings.NewReader(`{"project":"demo"}`))
	c.Request.Header.Set("Content-Type", "application/json")
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
			return testHTTPResponse(http.StatusOK, `{
				"executions": [{
					"name": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-1",
					"startTime": "2026-06-29T01:02:03Z",
					"completionTime": "2026-06-29T01:02:13Z",
					"conditions": [{"type": "Completed", "state": "CONDITION_SUCCEEDED"}]
				}]
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
	if got := executionRequest.URL.Query().Get("pageSize"); got != "1" {
		t.Fatalf("pageSize = %q, want 1", got)
	}
	var body struct {
		ProjectID     string `json:"project_id"`
		LastExecution *struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Duration  string `json:"duration"`
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
		body.LastExecution.Status != "SUCCESS" ||
		body.LastExecution.StartTime != "2026-06-29T01:02:03Z" ||
		body.LastExecution.EndTime != "2026-06-29T01:02:13Z" ||
		body.LastExecution.Duration != "10s" {
		t.Fatalf("last_execution = %#v", body.LastExecution)
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
				"completionStatus": "EXECUTION_SUCCEEDED"
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
		body.LastExecution.Status != "SUCCESS" ||
		body.LastExecution.StartTime != "2026-06-29T02:00:00Z" ||
		body.LastExecution.EndTime != "2026-06-29T02:00:07Z" ||
		body.LastExecution.Duration != "7s" {
		t.Fatalf("last_execution = %#v", body.LastExecution)
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

func TestBuildInitProjectRunBody(t *testing.T) {
	body, err := buildInitProjectRunBody("user-1", "a1b2c3d4e5f6", "Demo Project")
	if err != nil {
		t.Fatalf("buildInitProjectRunBody: %v", err)
	}

	want := map[string]any{
		"overrides": map[string]any{
			"containerOverrides": []any{
				map[string]any{
					"env": []any{
						map[string]any{"name": "TASK_TYPE", "value": "init"},
						map[string]any{"name": "USER_ID", "value": "user-1"},
						map[string]any{"name": "PROJECT_ID", "value": "a1b2c3d4e5f6"},
						map[string]any{"name": "PROJECT_NAME", "value": "Demo Project"},
					},
				},
			},
		},
	}
	if string(body) != mustJSON(t, want) {
		t.Fatalf("body = %s, want %s", body, mustJSON(t, want))
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
		h.RebuildIndex,
		h.InitProject,
		h.ProjectStatus,
	}
	if len(handlers) != 14 {
		t.Fatalf("handler count = %d, want 14", len(handlers))
	}
}
