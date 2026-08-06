package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/pipelinediagnostic"
	"github.com/rayer/llm-wiki-bff/internal/pipelinequota"
	"github.com/rayer/llm-wiki-bff/internal/search"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
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

func TestGetStorePinsOneImmutableViewPerRequest(t *testing.T) {
	root := &viewPinRoot{Client: localfs.New(t.TempDir()), pins: new(int)}
	h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("userID", "user")
	c.Set("projectID", "project")

	first, err := h.GetStore(c)
	if err != nil {
		t.Fatalf("first GetStore: %v", err)
	}
	second, err := h.GetStore(c)
	if err != nil {
		t.Fatalf("second GetStore: %v", err)
	}
	if first != second || *root.pins != 1 {
		t.Fatalf("GetStore did not reuse one pinned view: first=%T second=%T pins=%d", first, second, *root.pins)
	}
}

func TestIDRoutingCacheIsPartitionedByPinnedGeneration(t *testing.T) {
	base := localfs.New(t.TempDir()).WithScope("user", "project")
	h := New(base, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	a := &routingViewStore{Store: base, token: "manifest-7", data: []byte(`{"concept":{"aaaaaaaaaaaa":"from-a"}}`)}
	b := &routingViewStore{Store: base, token: "manifest-8", data: []byte(`{"concept":{"bbbbbbbbbbbb":"from-b"}}`)}
	first, err := h.getIDRoutingMap(context.Background(), a)
	if err != nil || first.byID["aaaaaaaaaaaa"].Slug != "from-a" {
		t.Fatalf("generation A route = %#v, %v", first, err)
	}
	next, err := h.getIDRoutingMap(context.Background(), b)
	if err != nil || next.byID["bbbbbbbbbbbb"].Slug != "from-b" || next.byID["aaaaaaaaaaaa"].Slug != "" {
		t.Fatalf("generation B route reused A cache = %#v, %v", next, err)
	}
	if got := rewriteWikilinks("[[from-a]]", first); !strings.Contains(got, "aaaaaaaaaaaa-from-a") {
		t.Fatalf("generation A rewrite = %q", got)
	}
	if got := rewriteWikilinks("[[from-b]]", next); !strings.Contains(got, "bbbbbbbbbbbb-from-b") {
		t.Fatalf("generation B rewrite = %q", got)
	}
}

type viewPinRoot struct {
	*localfs.Client
	pins *int
}

func (r *viewPinRoot) Scope(userID, projectID string) store.Store {
	return &viewPinStore{Store: r.Client.Scope(userID, projectID), pins: r.pins}
}

type viewPinStore struct {
	store.Store
	pins *int
}

func (s *viewPinStore) Pin(context.Context) (store.Store, error) {
	*s.pins++
	return &viewPinStore{Store: s.Store, pins: s.pins}, nil
}

type routingViewStore struct {
	store.Store
	token string
	data  []byte
}

func (s *routingViewStore) ViewToken() string { return s.token }

func (s *routingViewStore) ReadFile(context.Context, string) ([]byte, error) {
	return append([]byte(nil), s.data...), nil
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

func TestPipelineStatusRejectsInvalidGeneratedSuggestedQueries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "users", "user", "projects", "project", "cache", "suggested_queries.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"queries":[`+strings.Repeat(`"q",`, 10000)+`"overflow"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.PipelineStatus(c)
	if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != `{"error":"pipeline status unavailable"}` {
		t.Fatalf("status=%d body=%s, want fixed 500 status error", recorder.Code, recorder.Body.String())
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

	if _, err := h.invokePipelineJob(context.Background(), "user", "project", false); err != nil {
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

func TestIndexHandlerSanitizesGeneratedReadFailures(t *testing.T) {
	root := generatedIndexErrorRoot{RootStore: localfs.New(t.TempDir()), err: errors.New("provider denied users/tenant-secret/projects/project-secret/.lwc/publish/generations/generation-secret/cache/id_map.json")}
	h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/index", nil)
	c.Set("userID", "tenant-secret")
	c.Set("projectID", "project-secret")

	h.Index(c)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != `{"error":"generated data unavailable"}` {
		t.Fatalf("body = %q, want fixed generated-data error", got)
	}
}

type generatedIndexErrorRoot struct {
	store.RootStore
	err error
}

func (r generatedIndexErrorRoot) Scope(userID, projectID string) store.Store {
	return generatedIndexErrorStore{Store: r.RootStore.Scope(userID, projectID), err: r.err}
}

type generatedIndexErrorStore struct {
	store.Store
	err error
}

func (s generatedIndexErrorStore) ReadFile(context.Context, string) ([]byte, error) {
	return nil, s.err
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
	authority, err := search.NewCitationAuthority()
	if err != nil {
		t.Fatal(err)
	}

	contexts := cachedContexts(context.Background(), conceptCache, reader, []search.Result{{
		Slug:  "alpha",
		Title: "Alpha Concept",
		Type:  "concept",
	}}, authority)

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
						map[string]any{"name": "PIPELINE_STAGE", "value": "full"},
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
	if len(override.Env) < 4 || override.Env[3].Name != "PIPELINE_STAGE" || override.Env[3].Value != "full" {
		t.Fatalf("PIPELINE_STAGE env = %#v, want full", override.Env)
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
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" || body["execution_id"] != "olw-pipeline-admin" {
		t.Fatalf("body = %#v", body)
	}
	if body["clean_rebuild"] != false {
		t.Fatalf("default clean_rebuild = %#v, want false", body["clean_rebuild"])
	}
	if body["stage"] != "full" {
		t.Fatalf("default stage = %#v, want full", body["stage"])
	}
	override := runRequest.Overrides.ContainerOverrides[0]
	if len(override.Args) != 2 || override.Args[0] != "run" || override.Args[1] != defaultWorkerCommands {
		t.Fatalf("args = %#v, want [run %s]", override.Args, defaultWorkerCommands)
	}
	if override.Env[0].Value != "request-user" || override.Env[1].Value != "demo" || override.Env[2].Value != "pipeline" {
		t.Fatalf("env = %#v", override.Env)
	}
	foundStage := false
	for _, env := range override.Env {
		if env.Name == "CLEAN_REBUILD" {
			t.Fatalf("default admin trigger must not set CLEAN_REBUILD: %#v", override.Env)
		}
		if env.Name == "PIPELINE_STAGE" {
			foundStage = true
			if env.Value != "full" {
				t.Fatalf("PIPELINE_STAGE = %q, want full", env.Value)
			}
		}
	}
	if !foundStage {
		t.Fatalf("missing PIPELINE_STAGE env: %#v", override.Env)
	}
}

func TestAdminPipelineTriggerSuggestedQueriesStage(t *testing.T) {
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
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-suggest"
				}
			}`), nil
		default:
			return testHTTPResponse(http.StatusOK, `{"executions":[]}`), nil
		}
	})}

	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       client,
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
		projectExists:    func(context.Context, string) error { return nil },
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/projects/request-user_demo/pipeline", strings.NewReader(`{"stage":"suggested-queries"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "request-user_demo"}}

	h.AdminPipelineTrigger(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["stage"] != "suggested-queries" || body["execution_id"] != "olw-pipeline-suggest" {
		t.Fatalf("body = %#v", body)
	}
	override := runRequest.Overrides.ContainerOverrides[0]
	if len(override.Args) != 1 || override.Args[0] != "suggested-queries" {
		t.Fatalf("args = %#v, want [suggested-queries]", override.Args)
	}
	foundStage := false
	for _, env := range override.Env {
		if env.Name == "PIPELINE_STAGE" {
			foundStage = true
			if env.Value != "suggested-queries" {
				t.Fatalf("PIPELINE_STAGE = %q", env.Value)
			}
		}
		if env.Name == "CLEAN_REBUILD" {
			t.Fatalf("suggested-queries must not set CLEAN_REBUILD")
		}
	}
	if !foundStage {
		t.Fatal("missing PIPELINE_STAGE")
	}
}

func TestAdminPipelineTriggerRejectsCleanRebuildWithSuggestedQueries(t *testing.T) {
	h := &Handler{
		index:            search.NewIndex(),
		httpClient:       &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return testHTTPResponse(http.StatusOK, `{}`), nil })},
		metadataTokenURL: "http://metadata.test/token",
		cloudRunJobURL:   "https://run.test/run",
		projectExists:    func(context.Context, string) error { return nil },
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/projects/request-user_demo/pipeline", strings.NewReader(`{"stage":"suggested-queries","clean_rebuild":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "request-user_demo"}}

	h.AdminPipelineTrigger(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminPipelineTriggerCleanRebuildSetsEnv(t *testing.T) {
	var runRequest struct {
		Overrides struct {
			ContainerOverrides []struct {
				Env []struct {
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
					"execution": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/olw-pipeline-clean"
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
		projectExists:    func(context.Context, string) error { return nil },
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/projects/request-user_demo/pipeline", strings.NewReader(`{"clean_rebuild":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "request-user_demo"}}

	h.AdminPipelineTrigger(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["clean_rebuild"] != true {
		t.Fatalf("body = %#v, want clean_rebuild true", body)
	}
	found := false
	for _, env := range runRequest.Overrides.ContainerOverrides[0].Env {
		if env.Name == "CLEAN_REBUILD" && env.Value == "true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("env = %#v, want CLEAN_REBUILD=true", runRequest.Overrides.ContainerOverrides[0].Env)
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
	if body := recorder.Body.String(); body != `{"error":"pipeline unavailable"}` {
		t.Fatalf("body = %s, want fixed pipeline error", body)
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
	refundErr    error
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
	return s.refundErr
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
		refundErr: errors.New(securitySentinel),
	}
	var output bytes.Buffer
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()
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
	if output.String() != "pipeline quota refund failed\n" {
		t.Fatalf("refund log = %q, want fixed event", output.String())
	}
	assertSecuritySentinelsAbsent(t, output.String())
}

func TestPipelineStatusIncludesSuggestedQueries(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	if err := os.MkdirAll(filepath.Join(projectRoot, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	queriesJSON := validSuggestedQueriesJSON()
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
	if len(body.SuggestedQueries) != 20 || body.SuggestedQueries[0] != "哪些概念值得一起比較？" {
		t.Fatalf("suggested_queries = %#v, want full twenty-item array", body.SuggestedQueries)
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

func TestPipelineStatusFailsClosedWhenSuggestedQueryStoreStateFails(t *testing.T) {
	for _, stateErr := range []error{errors.New("invalid generation manifest"), errors.New("provider state unavailable")} {
		t.Run(stateErr.Error(), func(t *testing.T) {
			root := &suggestedQueriesStateFailureRoot{Client: localfs.New(t.TempDir()), err: stateErr}
			h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
			h.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/token" {
					return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
				}
				return testHTTPResponse(http.StatusOK, `{}`), nil
			})}
			h.metadataTokenURL = "http://metadata.test/token"
			h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status", nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo-project")

			h.PipelineStatus(c)

			if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != `{"error":"pipeline status unavailable"}` {
				t.Fatalf("status=%d body=%s, want fixed 500 status error", recorder.Code, recorder.Body.String())
			}
		})
	}
}

type suggestedQueriesStateFailureRoot struct {
	*localfs.Client
	err error
}

func (r *suggestedQueriesStateFailureRoot) Scope(userID, projectID string) store.Store {
	return &suggestedQueriesStateFailureStore{Store: r.Client.Scope(userID, projectID), err: r.err}
}

type suggestedQueriesStateFailureStore struct {
	store.Store
	err error
}

func (s *suggestedQueriesStateFailureStore) Pin(context.Context) (store.Store, error) {
	return nil, s.err
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
	if err := os.WriteFile(logPath, []byte("pipeline completed\n"), 0o644); err != nil {
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

	if recorder.Code != http.StatusOK || recorder.Body.String() != "pipeline completed\n" {
		t.Fatalf("status = %d; body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestPipelineLogReturnsOwnedArbitraryOperationalOutput(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-owned-raw.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "source: notes/meeting.md\nwarning: /tmp/cache path\nprovider output: keep this line\n"
	if err := os.WriteFile(logPath, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	owned := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned-raw", "request-user", "demo-project", "pipeline")
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
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=owned-raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineLog(c)

	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("status = %d; body = %q, want %q", recorder.Code, recorder.Body.String(), want)
	}
}

func TestReadPipelineLogNormalizesInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "users", "u", "projects", "p", "cache")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "pipeline-run.log"), []byte("prefix \xff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readPipelineLog(context.Background(), localfs.New(root).Scope("u", "p"), "projects/p/locations/l/jobs/j/executions/run")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "prefix ?\n" || !utf8.Valid(got) {
		t.Fatalf("normalized log = %q, want valid replacement", got)
	}
}

func TestReadPipelineLogNormalizesInvalidUTF8WithoutExpandingBound(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "users", "u", "projects", "p", "cache")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, pipelinediagnostic.MaxPipelineLogBytes)
	for i := range data {
		if i%2 == 0 {
			data[i] = 0xff
		} else {
			data[i] = 'A'
		}
	}
	if err := os.WriteFile(filepath.Join(path, "pipeline-run.log"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readPipelineLog(context.Background(), localfs.New(root).Scope("u", "p"), "projects/p/locations/l/jobs/j/executions/run")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > pipelinediagnostic.MaxPipelineLogBytes || !utf8.Valid(got) {
		t.Fatalf("normalized log len=%d valid=%v, want valid <= %d", len(got), utf8.Valid(got), pipelinediagnostic.MaxPipelineLogBytes)
	}
	if len(got) != len(data) || got[0] != '?' || got[1] != 'A' || got[len(got)-2] != '?' || got[len(got)-1] != 'A' {
		t.Fatalf("normalized log boundary/content = %q...%q, len=%d", got[:2], got[len(got)-2:], len(got))
	}
}

func TestReadPipelineLogRejectsOverLimitArtifact(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "users", "u", "projects", "p", "cache")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("x"), pipelinediagnostic.MaxPipelineLogBytes+1)
	if err := os.WriteFile(filepath.Join(path, "pipeline-run.log"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPipelineLog(context.Background(), localfs.New(root).Scope("u", "p"), "projects/p/locations/l/jobs/j/executions/run"); err == nil {
		t.Fatal("over-limit pipeline log was accepted")
	}
}

func TestPipelineLogReturnsCompleteNearLimitWorkerArtifact(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	begin := []byte("BEGIN\n")
	middle := []byte("MIDDLE: Synto emitted the decisive diagnostic here\n")
	end := []byte("END\n")
	data := bytes.Repeat([]byte{'x'}, pipelinediagnostic.MaxPipelineLogBytes)
	copy(data, begin)
	copy(data[len(data)/2:], middle)
	copy(data[len(data)-len(end):], end)
	if err := os.WriteFile(filepath.Join(dir, "pipeline-owned-tail.log"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	owned := pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned-tail", "request-user", "demo-project", "pipeline")
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
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=owned-tail", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineLog(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != string(data) {
		t.Fatalf("body did not preserve complete worker artifact: len=%d want=%d", recorder.Body.Len(), len(data))
	}
	if !strings.Contains(recorder.Body.String(), string(begin)) || !strings.Contains(recorder.Body.String(), string(middle)) || !strings.Contains(recorder.Body.String(), string(end)) {
		t.Fatalf("body omitted BEGIN, MIDDLE, or END marker")
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

func TestPipelineStatusProjectsOwnedFailureDiagnostic(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-owned-failure.failure.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"status":"failed","stage":"concept_reconciliation","error_class":"child_exit","detail_code":"entity_mapping_article_source_missing","child_command":"run","exit_code":23}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-owned-failure.log"), []byte("child stderr: source missing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owned := strings.Replace(pipelineOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned-failure", "request-user", "demo-project", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1)
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
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id=owned-failure", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.PipelineStatus(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			LogState   string `json:"log_state"`
			Diagnostic *struct {
				Version    int    `json:"version"`
				Status     string `json:"status"`
				Stage      string `json:"stage"`
				ErrorClass string `json:"error_class"`
				DetailCode string `json:"detail_code"`
				Child      string `json:"child_command"`
				ExitCode   int    `json:"exit_code"`
			} `json:"diagnostic"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.LastExecution == nil || body.LastExecution.LogState != "available" || body.LastExecution.Diagnostic == nil {
		t.Fatalf("last_execution = %#v, want available diagnostic", body.LastExecution)
	}
	diagnostic := body.LastExecution.Diagnostic
	if diagnostic.Version != 1 || diagnostic.Status != "failed" || diagnostic.Stage != "concept_reconciliation" || diagnostic.ErrorClass != "child_exit" || diagnostic.DetailCode != "entity_mapping_article_source_missing" || diagnostic.Child != "run" || diagnostic.ExitCode != 23 {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestPipelineFailureDiagnosticAcceptsEveryWorkerProducedSchemaValue(t *testing.T) {
	stages := []string{
		"input_materialization", "synto_migration", "synto_config_normalization",
		"synto_config_validation", "synto_run", "synto_index_export",
		"source_reconciliation", "concept_reconciliation", "postprocess",
		"generation_publish", "receipt_recording", "lease_cleanup", "unknown",
	}
	classes := []string{
		"validation", "child_exit", "timeout", "cancelled", "io", "state_invalid",
		"publish_conflict", "recording_failure", "unknown",
	}
	details := []string{
		"generated_map_read_decode", "synto_index_truth", "entity_mapping",
		"entity_mapping_index_truth", "entity_mapping_source_concept_identity",
		"entity_mapping_article_identity", "entity_mapping_article_path",
		"entity_mapping_article_source_ambiguity", "entity_mapping_article_source_missing",
		"entity_mapping_article_source_disagreement", "entity_mapping_duplicate_article_id",
		"entity_mapping_duplicate_article_path", "entity_mapping_duplicate_entity_id",
		"entity_mapping_active_entity_unknown", "entity_mapping_concept_slug_case",
		"entity_mapping_concept_id_path_disagreement", "entity_mapping_concept_missing_mapping",
		"entity_mapping_concept_entity_collision", "entity_merge", "identity_reconciliation",
		"lifecycle_planning", "concept_page_rewrite", "link_rewrite", "cache_rewrite",
		"artifact_write", "artifact_remove",
	}
	children := []string{"migrate-olw", "run", "pack-export"}

	tests := make([]struct {
		name       string
		stage      string
		class      string
		detailCode string
		child      string
	}, 0, len(stages)+len(classes)+len(details)+len(children))
	for _, stage := range stages {
		tests = append(tests, struct {
			name       string
			stage      string
			class      string
			detailCode string
			child      string
		}{"stage/" + stage, stage, "unknown", "", ""})
	}
	for _, class := range classes {
		tests = append(tests, struct {
			name       string
			stage      string
			class      string
			detailCode string
			child      string
		}{"class/" + class, "unknown", class, "", ""})
	}
	for _, detail := range details {
		tests = append(tests, struct {
			name       string
			stage      string
			class      string
			detailCode string
			child      string
		}{"detail/" + detail, "concept_reconciliation", "unknown", detail, ""})
	}
	for _, child := range children {
		tests = append(tests, struct {
			name       string
			stage      string
			class      string
			detailCode string
			child      string
		}{"child/" + child, "synto_run", "child_exit", "", child})
	}

	root := t.TempDir()
	project := localfs.New(root).WithScope("u", "p")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(handler.PipelineFailureDiagnostic{
				Version: 1, Status: "failed", Stage: tc.stage, ErrorClass: tc.class,
				DetailCode: tc.detailCode, Child: tc.child,
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "users", "u", "projects", "p", "cache", "pipeline-schema.failure.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readPipelineFailureDiagnostic(context.Background(), project, "projects/p/locations/l/jobs/j/executions/schema"); err != nil {
				t.Fatalf("worker-produced value rejected: %s: %v", string(data), err)
			}
		})
	}
}

func TestPipelineStatusDiagnosticStates(t *testing.T) {
	tests := []struct {
		name       string
		statusBody string
		diagnostic string
		raw        string
		wantState  string
		wantReason string
	}{
		{name: "running pending", statusBody: pipelineRunningOwnershipExecution("projects/p/locations/l/jobs/j/executions/running", "u", "p"), wantState: "pending"},
		{name: "success available", statusBody: pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/success", "u", "p", "pipeline"), raw: "success output\n", wantState: "available"},
		{name: "succeeded missing", statusBody: pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/succeeded-missing", "u", "p", "pipeline"), wantState: "unavailable", wantReason: "log_unavailable"},
		{name: "failed delayed", statusBody: strings.Replace(pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/delayed", "u", "p", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1), wantState: "unavailable", wantReason: "log_unavailable"},
		{name: "cancelled missing", statusBody: strings.Replace(pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/cancelled-missing", "u", "p", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_CANCELLED", 1), wantState: "unavailable", wantReason: "log_unavailable"},
		{name: "failed malformed", statusBody: strings.Replace(pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/malformed", "u", "p", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1), diagnostic: `{"version":1,"status":"failed","stage":"unknown","error_class":"unknown","unknown":"nope"}`, raw: "raw survives malformed diagnostic\n", wantState: "available"},
		{name: "failed oversized", statusBody: strings.Replace(pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/oversized", "u", "p", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1), diagnostic: strings.Repeat("x", maxPipelineDiagnosticBytes+1), raw: "raw survives oversized diagnostic\n", wantState: "available"},
		{name: "succeeded oversized log", statusBody: pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/oversized-log", "u", "p", "pipeline"), raw: strings.Repeat("x", pipelinediagnostic.MaxPipelineLogBytes+1), wantState: "unavailable", wantReason: "log_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.diagnostic != "" {
				id := shortCloudRunExecutionName(executionNameFromJSON(t, tt.statusBody), true)
				path := filepath.Join(root, "users", "u", "projects", "p", "cache", "pipeline-"+id+".failure.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.diagnostic), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.raw != "" {
				id := shortCloudRunExecutionName(executionNameFromJSON(t, tt.statusBody), true)
				path := filepath.Join(root, "users", "u", "projects", "p", "cache", "pipeline-"+id+".log")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.raw), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/token" {
					return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
				}
				return testHTTPResponse(http.StatusOK, tt.statusBody), nil
			})}
			h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
			h.httpClient = client
			h.metadataTokenURL = "http://metadata.test/token"
			h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/p/locations/l/jobs/j:run"
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id="+url.QueryEscape(shortCloudRunExecutionName(executionNameFromJSON(t, tt.statusBody), true)), nil)
			c.Set("userID", "u")
			c.Set("projectID", "p")

			h.PipelineStatus(c)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
			}
			var body struct {
				LastExecution *struct {
					LogState       string `json:"log_state"`
					LogStateReason string `json:"log_state_reason"`
				} `json:"last_execution"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.LastExecution == nil || body.LastExecution.LogState != tt.wantState || body.LastExecution.LogStateReason != tt.wantReason {
				t.Fatalf("last_execution = %#v, want state=%q reason=%q", body.LastExecution, tt.wantState, tt.wantReason)
			}
		})
	}
}

func TestAttachPipelineDiagnosticTerminalUnavailableStoreIsFinite(t *testing.T) {
	for _, status := range []string{"FAILED", "SUCCEEDED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			h := New(nil, nil, search.NewIndex(), conceptcache.New(), nil, nil)
			response := &handler.PipelineExecutionResponse{Status: status}
			h.attachPipelineDiagnostic(context.Background(), response, &pipelineExecutionOwner{userID: "u", projectID: "p"})
			if response.LogState != pipelineLogStateUnavailable || response.LogStateReason != "storage_unavailable" {
				t.Fatalf("response=%+v, want finite unavailable storage state", response)
			}
		})
	}
}

type pipelineLogErrorRoot struct {
	store.RootStore
	err error
}

func (r pipelineLogErrorRoot) Scope(userID, projectID string) store.Store {
	return pipelineLogErrorStore{Store: r.RootStore.Scope(userID, projectID), err: r.err}
}

type pipelineLogErrorStore struct {
	store.Store
	err error
}

func (s pipelineLogErrorStore) ReadFileLimited(context.Context, string, int64) ([]byte, error) {
	return nil, s.err
}

type pipelineLogMetadataSpyRoot struct {
	store.RootStore
	statCalls int
	readCalls int
	statSize  int64
	statErr   error
}

func (r *pipelineLogMetadataSpyRoot) Scope(userID, projectID string) store.Store {
	return &pipelineLogMetadataSpyStore{Store: r.RootStore.Scope(userID, projectID), root: r}
}

type pipelineLogMetadataSpyStore struct {
	store.Store
	root *pipelineLogMetadataSpyRoot
}

func (s *pipelineLogMetadataSpyStore) StatFile(context.Context, string) (int64, error) {
	s.root.statCalls++
	return s.root.statSize, s.root.statErr
}

func (s *pipelineLogMetadataSpyStore) ReadFileLimited(ctx context.Context, path string, limit int64) ([]byte, error) {
	if path == suggestedqueries.Path {
		return s.Store.(interface {
			ReadFileLimited(context.Context, string, int64) ([]byte, error)
		}).ReadFileLimited(ctx, path, limit)
	}
	s.root.readCalls++
	return nil, errors.New("body read must not occur during status")
}

type pipelineStatusMetadataRoot struct {
	store.RootStore
	diagnostic      []byte
	statSize        int64
	rawReads        int
	diagnosticReads int
}

func (r *pipelineStatusMetadataRoot) Scope(userID, projectID string) store.Store {
	return &pipelineStatusMetadataStore{Store: r.RootStore.Scope(userID, projectID), root: r}
}

type pipelineStatusMetadataStore struct {
	store.Store
	root *pipelineStatusMetadataRoot
}

func (s *pipelineStatusMetadataStore) StatFile(context.Context, string) (int64, error) {
	return s.root.statSize, nil
}

func (s *pipelineStatusMetadataStore) ReadFileLimited(ctx context.Context, path string, limit int64) ([]byte, error) {
	if strings.HasSuffix(path, ".log") {
		s.root.rawReads++
		return nil, errors.New("raw log body read must not occur during status")
	}
	if strings.HasSuffix(path, ".failure.json") {
		s.root.diagnosticReads++
		return append([]byte(nil), s.root.diagnostic...), nil
	}
	return s.Store.(interface {
		ReadFileLimited(context.Context, string, int64) ([]byte, error)
	}).ReadFileLimited(ctx, path, limit)
}

type pipelineFailureDiagnosticSpyRoot struct {
	store.RootStore
	data         []byte
	genericReads int
	limitedReads int
	limit        int64
}

func (r *pipelineFailureDiagnosticSpyRoot) Scope(userID, projectID string) store.Store {
	return &pipelineFailureDiagnosticSpyStore{Store: r.RootStore.Scope(userID, projectID), root: r}
}

type pipelineFailureDiagnosticSpyStore struct {
	store.Store
	root *pipelineFailureDiagnosticSpyRoot
}

func (s *pipelineFailureDiagnosticSpyStore) ReadFile(context.Context, string) ([]byte, error) {
	s.root.genericReads++
	return append([]byte(nil), s.root.data...), nil
}

func (s *pipelineFailureDiagnosticSpyStore) ReadFileLimited(_ context.Context, _ string, limit int64) ([]byte, error) {
	s.root.limitedReads++
	s.root.limit = limit
	data := s.root.data
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return append([]byte(nil), data...), nil
}

func TestReadPipelineFailureDiagnosticUsesExactBoundedReader(t *testing.T) {
	root := &pipelineFailureDiagnosticSpyRoot{
		RootStore: localfs.New(t.TempDir()),
		data:      []byte(`{"version":1,"status":"failed","stage":"synto_run","error_class":"child_exit","child_command":"run"}`),
	}
	if _, err := readPipelineFailureDiagnostic(context.Background(), root.Scope("u", "p"), "projects/p/locations/l/jobs/j/executions/run"); err != nil {
		t.Fatal(err)
	}
	if root.genericReads != 0 || root.limitedReads != 1 || root.limit != maxPipelineDiagnosticBytes+1 {
		t.Fatalf("generic reads=%d limited reads=%d limit=%d, want 0/1/%d", root.genericReads, root.limitedReads, root.limit, maxPipelineDiagnosticBytes+1)
	}
}

func TestReadPipelineFailureDiagnosticRejectsOversizedObjectAfterBoundedRead(t *testing.T) {
	root := &pipelineFailureDiagnosticSpyRoot{
		RootStore: localfs.New(t.TempDir()),
		data:      append([]byte(`{"version":1,"status":"failed","stage":"synto_run","error_class":"child_exit","child_command":"run"}`), bytes.Repeat([]byte{'x'}, maxPipelineDiagnosticBytes)...),
	}
	if _, err := readPipelineFailureDiagnostic(context.Background(), root.Scope("u", "p"), "projects/p/locations/l/jobs/j/executions/run"); err == nil {
		t.Fatal("oversized diagnostic was accepted")
	}
	if root.genericReads != 0 || root.limitedReads != 1 || root.limit != maxPipelineDiagnosticBytes+1 || len(root.data) <= int(root.limit) {
		t.Fatalf("generic reads=%d limited reads=%d limit=%d object bytes=%d, want bounded 4097-byte request", root.genericReads, root.limitedReads, root.limit, len(root.data))
	}
}

type pipelineFailureDiagnosticGenericOnlyRoot struct {
	store.RootStore
}

func (r pipelineFailureDiagnosticGenericOnlyRoot) Scope(userID, projectID string) store.Store {
	return pipelineFailureDiagnosticGenericOnlyStore{Store: r.RootStore.Scope(userID, projectID)}
}

type pipelineFailureDiagnosticGenericOnlyStore struct {
	store.Store
}

func (pipelineFailureDiagnosticGenericOnlyStore) ReadFile(context.Context, string) ([]byte, error) {
	return []byte(`{"version":1,"status":"failed","stage":"synto_run","error_class":"child_exit","child_command":"run"}`), nil
}

func TestReadPipelineFailureDiagnosticFailsClosedWithoutLimitedReader(t *testing.T) {
	root := pipelineFailureDiagnosticGenericOnlyRoot{RootStore: localfs.New(t.TempDir())}
	if _, err := readPipelineFailureDiagnostic(context.Background(), root.Scope("u", "p"), "projects/p/locations/l/jobs/j/executions/run"); err == nil {
		t.Fatal("diagnostic reader fell back to generic ReadFile")
	}
}

func TestAttachPipelineDiagnosticUsesMetadataWithoutReadingLog(t *testing.T) {
	root := &pipelineLogMetadataSpyRoot{RootStore: localfs.New(t.TempDir()), statSize: 3}
	h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	response := &handler.PipelineExecutionResponse{
		Name:   "projects/p/locations/l/jobs/j/executions/metadata-only",
		Status: "SUCCEEDED",
	}
	h.attachPipelineDiagnostic(context.Background(), response, &pipelineExecutionOwner{userID: "u", projectID: "p"})
	if response.LogState != pipelineLogStateAvailable || response.LogStateReason != "" {
		t.Fatalf("response=%+v, want available", response)
	}
	if root.statCalls != 1 || root.readCalls != 0 {
		t.Fatalf("stat calls=%d, body reads=%d, want one metadata probe and no body reads", root.statCalls, root.readCalls)
	}
}

func TestAttachPipelineDiagnosticTerminalMalformedStoreIsFinite(t *testing.T) {
	h := New(pipelineLogErrorRoot{RootStore: localfs.New(t.TempDir()), err: errors.New("malformed provider response")}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	for _, status := range []string{"FAILED", "SUCCEEDED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			response := &handler.PipelineExecutionResponse{Status: status}
			h.attachPipelineDiagnostic(context.Background(), response, &pipelineExecutionOwner{userID: "u", projectID: "p"})
			if response.LogState != pipelineLogStateUnavailable || response.LogStateReason != "storage_unavailable" {
				t.Fatalf("response=%+v, want finite unavailable log state", response)
			}
		})
	}
}

func TestPipelineStatusDoesNotUseDiagnosticFromAnotherProject(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "users", "u", "projects", "other", "cache", "pipeline-isolated.failure.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"status":"failed","stage":"unknown","error_class":"unknown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	failed := strings.Replace(pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/isolated", "u", "p", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, failed), nil
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/p/locations/l/jobs/j:run"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/status?execution_id=isolated", nil)
	c.Set("userID", "u")
	c.Set("projectID", "p")
	h.PipelineStatus(c)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), `"stage":"unknown"`) {
		t.Fatalf("cross-project diagnostic leaked: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPipelineLogReturnsRawFailedOutputIndependentlyOfDiagnostic(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "users", "u", "projects", "p", "cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline-render.failure.json"), []byte(`{"version":1,"status":"failed","stage":"concept_reconciliation","error_class":"child_exit","detail_code":"entity_mapping_article_source_missing","child_command":"run","exit_code":23}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline-render.log"), []byte("pipeline failed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failed := strings.Replace(pipelineOwnershipExecution("projects/p/locations/l/jobs/j/executions/render", "u", "p", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, failed), nil
	})}
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = client
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/p/locations/l/jobs/j:run"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/log?execution_id=render", nil)
	c.Set("userID", "u")
	c.Set("projectID", "p")
	h.PipelineLog(c)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "pipeline failed\n" {
		t.Fatalf("raw failed log: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func executionNameFromJSON(t *testing.T, data string) string {
	t.Helper()
	var execution cloudRunExecution
	if err := json.Unmarshal([]byte(data), &execution); err != nil {
		t.Fatal(err)
	}
	return execution.Name
}

func TestPipelineLogReturnsProjectScopedLog(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "users", "request-user", "projects", "demo-project", "cache", "pipeline-olw-pipeline-abc123.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("pipeline completed\n"), 0o644); err != nil {
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
	if body := recorder.Body.String(); body != "pipeline completed\n" {
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
			return testHTTPResponse(http.StatusOK, `{"executions":[`+pipelineOwnershipExecution(
				"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/exec-1",
				"request-user", "demo-project", "pipeline")+"]}"), nil
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

func TestStatusRequiresAuthenticatedOwnerContext(t *testing.T) {
	for _, tt := range []struct {
		name      string
		userID    string
		projectID string
		wantCode  int
		wantBody  string
	}{
		{name: "missing user", projectID: "demo-project", wantCode: http.StatusUnauthorized, wantBody: `{"error":"user not authenticated"}`},
		{name: "missing project", userID: "request-user", wantCode: http.StatusBadRequest, wantBody: `{"error":"project is required"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := New(localfs.New(t.TempDir()), nil, search.NewIndex(), conceptcache.New(), nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
			if tt.userID != "" {
				c.Set("userID", tt.userID)
			}
			if tt.projectID != "" {
				c.Set("projectID", tt.projectID)
			}

			h.Status(c)

			if recorder.Code != tt.wantCode || recorder.Body.String() != tt.wantBody {
				t.Fatalf("status=%d body=%s, want status=%d body=%s", recorder.Code, recorder.Body.String(), tt.wantCode, tt.wantBody)
			}
		})
	}
}

func TestStatusProjectsOnlyOwnedExecutionAndMetadata(t *testing.T) {
	root := t.TempDir()
	unrelated := pipelineOwnershipExecution(
		"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/unrelated",
		"other-user", "other-project", "pipeline")
	owned := pipelineOwnershipExecution(
		"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned",
		"request-user", "demo-project", "pipeline")
	h := newStatusHandlerForTest(localfs.New(root), `{"executions":[`+unrelated+`,`+owned+`]}`)

	recorder := invokeStatusForTest(t, h, "request-user", "demo-project")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			Name           string `json:"name"`
			LogURL         string `json:"log_url"`
			LogState       string `json:"log_state"`
			LogStateReason string `json:"log_state_reason"`
		} `json:"last_execution"`
		SuggestedQueries []string `json:"suggested_queries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.LastExecution == nil {
		t.Fatal("last_execution = nil, want owned execution")
	}
	if body.LastExecution.Name != "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/owned" {
		t.Fatalf("last_execution.name = %q, want owned execution", body.LastExecution.Name)
	}
	if body.LastExecution.LogURL != "/api/v1/pipeline/log?execution_id=owned" {
		t.Fatalf("last_execution.log_url = %q, want authenticated owned execution URL", body.LastExecution.LogURL)
	}
	if body.LastExecution.LogState != pipelineLogStateUnavailable || body.LastExecution.LogStateReason != "log_unavailable" {
		t.Fatalf("last_execution log metadata = %#v, want unavailable/log_unavailable", body.LastExecution)
	}
	if body.SuggestedQueries == nil {
		t.Fatal("suggested_queries = nil, want explicit empty list")
	}
}

func TestStatusProjectsTerminalMissingLogWithoutReadingBody(t *testing.T) {
	root := &pipelineLogMetadataSpyRoot{
		RootStore: localfs.New(t.TempDir()),
		statErr:   store.ErrObjectNotExist,
	}
	owned := pipelineOwnershipExecution(
		"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/missing-log",
		"request-user", "demo-project", "pipeline")
	h := newStatusHandlerForTest(root, `{"executions":[`+owned+`]}`)

	recorder := invokeStatusForTest(t, h, "request-user", "demo-project")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			LogURL         string `json:"log_url"`
			LogState       string `json:"log_state"`
			LogStateReason string `json:"log_state_reason"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.LastExecution == nil || body.LastExecution.LogState != pipelineLogStateUnavailable || body.LastExecution.LogStateReason != "log_unavailable" {
		t.Fatalf("last_execution = %#v, want unavailable/log_unavailable", body.LastExecution)
	}
	if body.LastExecution.LogURL != "/api/v1/pipeline/log?execution_id=missing-log" {
		t.Fatalf("last_execution.log_url = %q, want owned execution URL", body.LastExecution.LogURL)
	}
	if root.statCalls != 1 || root.readCalls != 0 {
		t.Fatalf("stat calls=%d, body reads=%d, want one stat and no raw body read", root.statCalls, root.readCalls)
	}
}

func TestStatusProjectsAvailableLogMetadataWithoutReadingBody(t *testing.T) {
	root := &pipelineLogMetadataSpyRoot{RootStore: localfs.New(t.TempDir()), statSize: 3}
	owned := pipelineOwnershipExecution(
		"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/available-log",
		"request-user", "demo-project", "pipeline")
	h := newStatusHandlerForTest(root, `{"executions":[`+owned+`]}`)

	recorder := invokeStatusForTest(t, h, "request-user", "demo-project")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			LogState       string `json:"log_state"`
			LogStateReason string `json:"log_state_reason"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.LastExecution == nil || body.LastExecution.LogState != pipelineLogStateAvailable || body.LastExecution.LogStateReason != "" {
		t.Fatalf("last_execution = %#v, want available metadata", body.LastExecution)
	}
	if root.statCalls != 1 || root.readCalls != 0 {
		t.Fatalf("stat calls=%d, body reads=%d, want one stat and no raw body read", root.statCalls, root.readCalls)
	}
}

func TestStatusProjectsOwnedFailureDiagnostic(t *testing.T) {
	exitCode := 42
	root := &pipelineStatusMetadataRoot{
		RootStore:  localfs.New(t.TempDir()),
		statSize:   3,
		diagnostic: []byte(`{"version":1,"status":"failed","stage":"concept_reconciliation","error_class":"child_exit","detail_code":"entity_mapping_article_source_missing","child_command":"run","exit_code":42}`),
	}
	failed := strings.Replace(pipelineOwnershipExecution(
		"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/failed-diagnostic",
		"request-user", "demo-project", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_FAILED", 1)
	h := newStatusHandlerForTest(root, `{"executions":[`+failed+`]}`)

	recorder := invokeStatusForTest(t, h, "request-user", "demo-project")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var body struct {
		LastExecution *struct {
			LogState   string                             `json:"log_state"`
			Diagnostic *handler.PipelineFailureDiagnostic `json:"diagnostic"`
		} `json:"last_execution"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.LastExecution == nil || body.LastExecution.LogState != pipelineLogStateAvailable || body.LastExecution.Diagnostic == nil {
		t.Fatalf("last_execution = %#v, want available diagnostic", body.LastExecution)
	}
	diagnostic := body.LastExecution.Diagnostic
	if diagnostic.Version != 1 || diagnostic.Status != "failed" || diagnostic.Stage != "concept_reconciliation" || diagnostic.ErrorClass != "child_exit" || diagnostic.DetailCode != "entity_mapping_article_source_missing" || diagnostic.Child != "run" || diagnostic.ExitCode == nil || *diagnostic.ExitCode != exitCode {
		t.Fatalf("diagnostic = %#v, want all typed fields projected", diagnostic)
	}
	if root.rawReads != 0 || root.diagnosticReads != 1 {
		t.Fatalf("raw log reads=%d diagnostic reads=%d, want 0/1", root.rawReads, root.diagnosticReads)
	}
}

func TestStatusRetainsFiniteMetadataForRunningAndUnsupportedStates(t *testing.T) {
	unsupported := strings.Replace(pipelineOwnershipExecution(
		"projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/unsupported",
		"request-user", "demo-project", "pipeline"), "EXECUTION_SUCCEEDED", "EXECUTION_MYSTERY", 1)
	for _, tt := range []struct {
		name       string
		execution  string
		wantState  string
		wantReason string
	}{
		{name: "running", execution: pipelineRunningOwnershipExecution("projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline/executions/running", "request-user", "demo-project"), wantState: pipelineLogStatePending},
		{name: "unsupported", execution: unsupported, wantState: pipelineLogStateUnavailable, wantReason: "unsupported_execution_status"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newStatusHandlerForTest(localfs.New(t.TempDir()), `{"executions":[`+tt.execution+`]}`)
			recorder := invokeStatusForTest(t, h, "request-user", "demo-project")
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			var body struct {
				SourcesCount     int      `json:"sources_count"`
				ConceptsCount    int      `json:"concepts_count"`
				SuggestedQueries []string `json:"suggested_queries"`
				LastExecution    *struct {
					LogState       string `json:"log_state"`
					LogStateReason string `json:"log_state_reason"`
				} `json:"last_execution"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.LastExecution == nil || body.LastExecution.LogState != tt.wantState || body.LastExecution.LogStateReason != tt.wantReason {
				t.Fatalf("last_execution = %#v, want state=%q reason=%q", body.LastExecution, tt.wantState, tt.wantReason)
			}
			if body.SourcesCount != 0 || body.ConceptsCount != 0 || body.SuggestedQueries == nil {
				t.Fatalf("aggregate status fields regressed: sources=%d concepts=%d suggestions=%#v", body.SourcesCount, body.ConceptsCount, body.SuggestedQueries)
			}
		})
	}
}

func newStatusHandlerForTest(root store.RootStore, executions string) *Handler {
	h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, executions), nil
	})}
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
	return h
}

func invokeStatusForTest(t *testing.T, h *Handler, userID, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	c.Set("userID", userID)
	c.Set("projectID", projectID)
	h.Status(c)
	return recorder
}

func TestStatusIncludesEmptySuggestedQueriesWhenMissingArtifact(t *testing.T) {
	root := t.TempDir()
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
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
	var body struct {
		SuggestedQueries []string `json:"suggested_queries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.SuggestedQueries == nil {
		t.Fatal("suggested_queries = nil, want explicit empty array")
	}
	if len(body.SuggestedQueries) != 0 {
		t.Fatalf("suggested_queries = %#v, want []", body.SuggestedQueries)
	}
}

func TestStatusSuggestedQueriesPathDoesNotWriteStorage(t *testing.T) {
	writes := 0
	root := &readOnlySuggestedRoot{Client: localfs.New(t.TempDir()), writes: &writes}
	h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo-project")

	h.Status(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if writes != 0 {
		t.Fatalf("status suggested-query path writes = %d, want 0", writes)
	}
}

type readOnlySuggestedRoot struct {
	*localfs.Client
	writes *int
}

func (r *readOnlySuggestedRoot) Scope(userID, projectID string) store.Store {
	return &readOnlySuggestedStore{Store: r.Client.Scope(userID, projectID), writes: r.writes}
}

type readOnlySuggestedStore struct {
	store.Store
	writes *int
}

func (s *readOnlySuggestedStore) ReadFileLimited(ctx context.Context, path string, limit int64) ([]byte, error) {
	reader, ok := s.Store.(interface {
		ReadFileLimited(context.Context, string, int64) ([]byte, error)
	})
	if !ok {
		return nil, errors.New("bounded reader unavailable")
	}
	return reader.ReadFileLimited(ctx, path, limit)
}

func (s *readOnlySuggestedStore) WriteBytes(context.Context, []byte, string) (string, error) {
	*s.writes++
	return "", errors.New("unexpected status write")
}

func (s *readOnlySuggestedStore) WriteBytesAtomic(context.Context, []byte, string, string) (string, error) {
	*s.writes++
	return "", errors.New("unexpected status atomic write")
}

func readSuggestedQueriesFromStatusEndpoints(t *testing.T, root string) []string {
	t.Helper()

	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/token" {
			return testHTTPResponse(http.StatusOK, `{"access_token":"test-token"}`), nil
		}
		return testHTTPResponse(http.StatusOK, `{}`), nil
	})}
	h.metadataTokenURL = "http://metadata.test/token"
	h.cloudRunJobURL = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"

	read := func(path string) []string {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		c.Set("userID", "request-user")
		c.Set("projectID", "demo-project")

		switch path {
		case "/api/v1/pipeline/status":
			h.PipelineStatus(c)
		default:
			h.Status(c)
		}

		if recorder.Code != http.StatusOK {
			t.Fatalf("status endpoint %s: status = %d, want %d; body = %s", path, recorder.Code, http.StatusOK, recorder.Body.String())
		}
		var body struct {
			SuggestedQueries []string `json:"suggested_queries"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s response: %v", path, err)
		}
		if body.SuggestedQueries == nil {
			t.Fatalf("suggested_queries = nil for %s, want explicit []", path)
		}
		return body.SuggestedQueries
	}

	statusQueries := read("/api/v1/status")
	pipelineQueries := read("/api/v1/pipeline/status")
	if !reflect.DeepEqual(statusQueries, pipelineQueries) {
		t.Fatalf("parity mismatch: status=%#v pipeline=%#v", statusQueries, pipelineQueries)
	}
	return statusQueries
}

func writeSuggestionFixtures(t *testing.T, projectRoot string, suggestedJSON string, conceptsJSONL string) {
	t.Helper()

	cacheRoot := filepath.Join(projectRoot, "cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if suggestedJSON != "" {
		if err := os.WriteFile(filepath.Join(cacheRoot, "suggested_queries.json"), []byte(suggestedJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if conceptsJSONL != "" {
		if err := os.WriteFile(filepath.Join(cacheRoot, "concepts.jsonl"), []byte(conceptsJSONL), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesUsePresentArtifact(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	writeSuggestionFixtures(t, projectRoot, validSuggestedQueriesJSON(), "")

	got := readSuggestedQueriesFromStatusEndpoints(t, root)
	want := mustSuggestedQueries(t, validSuggestedQueriesJSON())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggested_queries = %#v, want published order %#v", got, want)
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesRejectNineteenItemArtifact(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	writeSuggestionFixtures(t, projectRoot, suggestedQueriesJSONWithCount(19), "")

	got := readSuggestedQueriesFromStatusEndpoints(t, root)
	if !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("suggested_queries = %#v, want [] for a non-legacy, non-current artifact", got)
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesUseBoundedReader(t *testing.T) {
	root := &suggestedQueriesReadSpyRoot{Client: localfs.New(t.TempDir()), data: []byte(validSuggestedQueriesJSON())}
	h := newStatusHandlerForTest(root, `{"executions":[]}`)

	for _, endpoint := range []string{"/api/v1/status", "/api/v1/pipeline/status"} {
		t.Run(endpoint, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, endpoint, nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo-project")
			if endpoint == "/api/v1/pipeline/status" {
				h.PipelineStatus(c)
			} else {
				h.Status(c)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s, want 200", recorder.Code, recorder.Body.String())
			}
		})
	}
	if root.genericReads != 0 {
		t.Fatalf("unbounded reads = %d, want 0", root.genericReads)
	}
	if root.limitedReads != 2 {
		t.Fatalf("bounded reads = %d, want one per endpoint", root.limitedReads)
	}
	for _, limit := range root.limits {
		if limit != 128<<10+1 {
			t.Fatalf("bounded read limit = %d, want 131073", limit)
		}
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesFailClosedWithoutBoundedReader(t *testing.T) {
	for _, endpoint := range []string{"/api/v1/status", "/api/v1/pipeline/status"} {
		t.Run(endpoint, func(t *testing.T) {
			root := &suggestedQueriesUnboundedRoot{Client: localfs.New(t.TempDir()), data: []byte(validSuggestedQueriesJSON())}
			h := newStatusHandlerForTest(root, `{"executions":[]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, endpoint, nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo-project")
			if endpoint == "/api/v1/pipeline/status" {
				h.PipelineStatus(c)
			} else {
				h.Status(c)
			}
			want := `{"error":"generated data unavailable"}`
			if endpoint == "/api/v1/pipeline/status" {
				want = `{"error":"pipeline status unavailable"}`
			}
			if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != want {
				t.Fatalf("status=%d body=%s, want fixed bounded-reader error", recorder.Code, recorder.Body.String())
			}
			if root.genericReads != 0 {
				t.Fatalf("unbounded reads = %d, want 0", root.genericReads)
			}
		})
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesFailClosedOnBoundedReaderError(t *testing.T) {
	for _, endpoint := range []string{"/api/v1/status", "/api/v1/pipeline/status"} {
		t.Run(endpoint, func(t *testing.T) {
			root := &suggestedQueriesReadSpyRoot{Client: localfs.New(t.TempDir()), limitedErr: errors.New("bounded read failed")}
			h := newStatusHandlerForTest(root, `{"executions":[]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, endpoint, nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo-project")
			if endpoint == "/api/v1/pipeline/status" {
				h.PipelineStatus(c)
			} else {
				h.Status(c)
			}
			want := `{"error":"generated data unavailable"}`
			if endpoint == "/api/v1/pipeline/status" {
				want = `{"error":"pipeline status unavailable"}`
			}
			if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != want {
				t.Fatalf("status=%d body=%s, want fixed bounded-reader error", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesRejectLegacyTitleArtifact(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	writeSuggestionFixtures(t, projectRoot, `{"queries":["咖啡廳","公園"]}`, `{"slug":"cafe","title":"咖啡廳"}`+"\n")

	got := readSuggestedQueriesFromStatusEndpoints(t, root)
	if !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("suggested_queries = %#v, want [] for legacy title-only artifact", got)
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesTreatInvalidArtifactsAsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "malformed truncated", data: `{"version":2,"queries":[`},
		{name: "duplicate key", data: `{"version":2,"version":2,"queries":[],"candidates":[],"updated_at":""}`},
		{name: "unknown key", data: `{"version":2,"queries":[],"candidates":[],"updated_at":"","extra":true}`},
		{name: "wrong shape", data: `{"version":2,"queries":{},"candidates":[],"updated_at":""}`},
		{name: "trailing json", data: validSuggestedQueriesJSON() + ` {"extra":true}`},
		{name: "unsupported version", data: `{"version":1,"queries":[],"candidates":[],"updated_at":""}`},
		{name: "legacy title-only", data: `{"queries":["咖啡廳","公園"]}`},
		{name: "invalid v2", data: `{"version":2,"queries":["q?"],"candidates":[],"updated_at":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
			writeSuggestionFixtures(t, projectRoot, tc.data, "")
			got := readSuggestedQueriesFromStatusEndpoints(t, root)
			if !reflect.DeepEqual(got, []string{}) {
				t.Fatalf("suggested_queries = %#v, want [] for invalid artifact", got)
			}
		})
	}
}

func TestStatusAndPipelineStatusKeepReadStateErrorsAsHTTP500(t *testing.T) {
	for _, endpoint := range []string{"/api/v1/status", "/api/v1/pipeline/status"} {
		t.Run(endpoint, func(t *testing.T) {
			root := &suggestedQueriesStateFailureRoot{Client: localfs.New(t.TempDir()), err: errors.New("read state unavailable")}
			h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, endpoint, nil)
			c.Set("userID", "request-user")
			c.Set("projectID", "demo-project")
			if endpoint == "/api/v1/pipeline/status" {
				h.PipelineStatus(c)
			} else {
				h.Status(c)
			}
			want := `{"error":"generated data unavailable"}`
			if endpoint == "/api/v1/pipeline/status" {
				want = `{"error":"pipeline status unavailable"}`
			}
			if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != want {
				t.Fatalf("status=%d body=%s, want fixed read-state error", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesFromConceptsWhenArtifactMissing(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	writeSuggestionFixtures(t, projectRoot, "", `{"slug":"a","title":"Newest","frontmatter":{"updated":"2026-07-12T00:00:00Z"}}`+"\n"+
		`{"slug":"b","title":"Newest-2","frontmatter":{"updated":"2026-07-11T00:00:00Z"}}`+"\n"+
		`{"slug":"c","title":"Newest-3","frontmatter":{"updated":"2026-07-10T00:00:00Z"}}`+"\n"+
		`{"slug":"d","title":"Newest-4","frontmatter":{"updated":"2026-07-09T00:00:00Z"}}`+"\n"+
		`{"slug":"e","title":"Newest-5","frontmatter":{"updated":"2026-07-08T00:00:00Z"}}`+"\n"+
		`{"slug":"f","title":"Newest-6","frontmatter":{"updated":"2026-07-07T00:00:00Z"}}`)

	got := readSuggestedQueriesFromStatusEndpoints(t, root)
	if !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("suggested_queries = %#v, want [] without a published artifact", got)
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesFromConceptsWhenArtifactEmpty(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "request-user", "projects", "demo-project")
	writeSuggestionFixtures(t, projectRoot, `{"queries":[],"updated_at":"2026-07-10T00:00:00Z"}`, `{"slug":"a","title":"Alpha","frontmatter":{"updated":"2026-07-10T00:00:00Z"}}`+"\n"+
		`{"slug":"b","title":"Beta","frontmatter":{"updated":"2026-07-09T00:00:00Z"}}`)

	got := readSuggestedQueriesFromStatusEndpoints(t, root)
	if !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("suggested_queries = %#v, want [] for an empty artifact", got)
	}
}

func TestStatusAndPipelineStatusSuggestedQueriesNoDataReturnsEmptySlice(t *testing.T) {
	root := t.TempDir()
	got := readSuggestedQueriesFromStatusEndpoints(t, root)
	if got == nil || len(got) != 0 {
		t.Fatalf("suggested_queries = %#v, want explicit []", got)
	}
}

func validSuggestedQueriesJSON() string {
	return suggestedQueriesJSONWithCount(20)
}

func suggestedQueriesJSONWithCount(count int) string {
	candidates := make([]suggestedqueries.Candidate, 0, count)
	questions := []string{"哪些概念值得一起比較？", "如何探索這個主題的不同面向？", "哪些選擇適合進一步查找？"}
	for i := 3; i < count; i++ {
		questions = append(questions, fmt.Sprintf("What else should I explore in concept %d?", i))
	}
	for _, question := range questions {
		candidates = append(candidates, suggestedqueries.Candidate{
			Question:               question,
			Intent:                 "discovery",
			CorpusAnchorConceptIDs: []string{"c1"},
			Generation:             suggestedqueries.GenerationMetadata{Model: "fixture", PromptVersion: "v1"},
		})
	}
	data, err := json.Marshal(suggestedqueries.ArtifactFromCandidates(candidates, time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		panic(err)
	}
	return string(data)
}

type suggestedQueriesReadSpyRoot struct {
	*localfs.Client
	data         []byte
	limitedErr   error
	genericReads int
	limitedReads int
	limits       []int64
}

func (r *suggestedQueriesReadSpyRoot) Scope(userID, projectID string) store.Store {
	return &suggestedQueriesReadSpyStore{Store: r.Client.Scope(userID, projectID), root: r}
}

type suggestedQueriesReadSpyStore struct {
	store.Store
	root *suggestedQueriesReadSpyRoot
}

func (s *suggestedQueriesReadSpyStore) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if path != suggestedqueries.Path {
		return s.Store.ReadFile(ctx, path)
	}
	s.root.genericReads++
	return append([]byte(nil), s.root.data...), nil
}

func (s *suggestedQueriesReadSpyStore) ReadFileLimited(ctx context.Context, path string, limit int64) ([]byte, error) {
	if path != suggestedqueries.Path {
		return s.Store.(interface {
			ReadFileLimited(context.Context, string, int64) ([]byte, error)
		}).ReadFileLimited(ctx, path, limit)
	}
	s.root.limitedReads++
	s.root.limits = append(s.root.limits, limit)
	if s.root.limitedErr != nil {
		return nil, s.root.limitedErr
	}
	return append([]byte(nil), s.root.data...), nil
}

type suggestedQueriesUnboundedRoot struct {
	*localfs.Client
	data         []byte
	genericReads int
}

func (r *suggestedQueriesUnboundedRoot) Scope(userID, projectID string) store.Store {
	return &suggestedQueriesUnboundedStore{Store: r.Client.Scope(userID, projectID), root: r}
}

type suggestedQueriesUnboundedStore struct {
	store.Store
	root *suggestedQueriesUnboundedRoot
}

func (s *suggestedQueriesUnboundedStore) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if path != suggestedqueries.Path {
		return s.Store.ReadFile(ctx, path)
	}
	s.root.genericReads++
	return append([]byte(nil), s.root.data...), nil
}

func mustSuggestedQueries(t *testing.T, data string) []string {
	t.Helper()
	artifact, err := suggestedqueries.Decode([]byte(data))
	if err != nil {
		t.Fatalf("decode suggestion fixture: %v", err)
	}
	return suggestedqueries.Queries(artifact)
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
