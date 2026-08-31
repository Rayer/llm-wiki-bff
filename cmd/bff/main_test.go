package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/auth"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	handlerraw "github.com/rayer/llm-wiki-bff/internal/handler"
	handlerv1 "github.com/rayer/llm-wiki-bff/internal/handler/v1"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/middleware"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/queryruntime"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
	"gopkg.in/yaml.v3"
)

func TestDefaultProductionQueryCompositionUsesProductionExecutor(t *testing.T) {
	executor, err := newProductionQueryExecutor(config.Config{}, conceptcache.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := executor.(*queryquality.ProductionExecutor); !ok {
		t.Fatalf("production executor = %T, want queryquality.ProductionExecutor", executor)
	}
}

func TestConfiguredProductionQueryCompositionLoadsImmutableRuntime(t *testing.T) {
	executor, err := newProductionQueryExecutor(config.Config{
		QueryStageConfigPath: "../../configs/query/dev/query-dev-2026-08-31.1.json",
		DeepSeekAPIKey:       "test-key",
	}, conceptcache.New())
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := executor.(*queryruntime.Executor)
	if !ok {
		t.Fatalf("configured executor=%T, want *queryruntime.Executor", executor)
	}
	readback := runtime.Readback()
	if readback.SchemaVersion != 2 || readback.ConfigRevision != "query-dev-2026-08-31.1" || readback.ConfigDigest != "sha256:2ee1a7303c60e810c3240c966a784c4d6cc76419a37b6e0e13e2d9e80f344305" || readback.DefaultProfileID != "platform-owned-lifestyle-v1" || readback.DefaultPromptID != "minimal-v1" || readback.ExpansionModel != "deepseek-v4-flash" || readback.SynthesisModel != "deepseek-v4-pro" || readback.NoEvidencePolicy != "full-model-prior-fallback-v1" || readback.Options.SelectionLimit != 10 || readback.BindingCount != 0 || readback.DistinctServiceCompositionCount != 1 {
		t.Fatalf("readback=%+v", readback)
	}
}

func TestInjectedStageConfigDoesNotReadArtifactAgain(t *testing.T) {
	source := "../../configs/query/dev/query-dev-2026-08-31.1.json"
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/query-stage.json"
	if err := os.WriteFile(path, data, 0o444); err != nil {
		t.Fatal(err)
	}
	loaded, err := queryconfig.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := newProductionQueryExecutorWithStageConfig(config.Config{QueryStageConfigPath: path, DeepSeekAPIKey: "test-key"}, conceptcache.New(), &loaded); err != nil {
		t.Fatalf("injected stage config was read again: %v", err)
	}
}

func TestProductionQueryCompositionRejectsNonBaselineModel(t *testing.T) {
	_, err := newProductionQueryExecutor(config.Config{QueryExpansionModel: "deepseek-chat"}, conceptcache.New())
	if err == nil {
		t.Fatal("newProductionQueryExecutor() error = nil, want fixed baseline rejection")
	}
}

func TestDefaultProductionQueryCompositionRunsThroughV1QueryPath(t *testing.T) {
	root := localfs.New(t.TempDir())
	if _, err := root.Scope("user", "project").WriteBytes(context.Background(), []byte(`{"slug":"legacy-hit","title":"Legacy Hit","body":"coffee"}`+"\n"), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}
	conceptCache := conceptcache.New()
	executor, err := newProductionQueryExecutor(config.Config{}, conceptCache)
	if err != nil {
		t.Fatal(err)
	}
	h := handlerv1.New(root, nil, nil, conceptCache, nil, nil)
	h.SetQueryExecutor(executor)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewBufferString(`{"q":"coffee","mode":"wiki"}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response handlerraw.QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Slug != "legacy-hit" {
		t.Fatalf("V1 production result = %#v", response.Results)
	}
}

func TestProductionInvalidStructuredPlanUsesChatLegacyExpansion(t *testing.T) {
	root := localfs.New(t.TempDir())
	reader := root.Scope("user", "project")
	if _, err := reader.WriteBytes(context.Background(), []byte(`{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n"), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}
	transport := &productionFallbackTransport{}
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	executor, err := newProductionQueryExecutor(config.Config{DeepSeekAPIKey: "test-key"}, conceptcache.New())
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), reader, query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Slug != "coffee" {
		t.Fatalf("result = %#v", result.Results)
	}
	if len(transport.requests) != 4 {
		t.Fatalf("HTTP calls = %d, want three parallel expansions plus synthesis", len(transport.requests))
	}
	if transport.requests[0].Model != "deepseek-v4-flash" || transport.requests[0].Temperature == nil || *transport.requests[0].Temperature != 0 {
		t.Fatalf("structured request = %#v", transport.requests[0])
	}
	if transport.requests[3].Model != "deepseek-v4-pro" {
		t.Fatalf("synthesis request = %#v", transport.requests[3])
	}
	if string(transport.requests[0].Thinking) != `{"type":"disabled"}` || transport.requests[0].ReasoningEffort != "" || string(transport.requests[3].Thinking) != `{"type":"disabled"}` || transport.requests[3].ReasoningEffort != "" {
		t.Fatalf("thinking policies = %#v, want explicit disabled for both defaults", transport.requests)
	}
}

func TestDefaultProductionQueryCompositionPreservesRequestCancellation(t *testing.T) {
	root := localfs.New(t.TempDir())
	if _, err := root.Scope("user", "project").WriteBytes(context.Background(), []byte(`{"slug":"candidate","title":"Candidate","body":"coffee"}`+"\n"), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	previousTransport := http.DefaultTransport
	transport := &productionCancellationTransport{started: started, startedOnce: &sync.Once{}}
	http.DefaultTransport = transport
	defer func() {
		transport.awaitCompletion(time.Second)
		http.DefaultTransport = previousTransport
	}()
	executor, err := newProductionQueryExecutor(config.Config{DeepSeekAPIKey: "test-key"}, conceptcache.New())
	if err != nil {
		t.Fatal(err)
	}
	h := handlerv1.New(root, nil, nil, conceptcache.New(), nil, nil)
	h.SetQueryExecutor(executor)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewBufferString(`{"q":"coffee"}`)).WithContext(ctx)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	c.Set("userID", "user")
	c.Set("projectID", "project")
	done := make(chan struct{})
	go func() {
		h.Query(c)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("production expansion provider did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("production query did not return after cancellation")
	}
}

func TestProductionQueryReportsInsufficientEvidenceWithoutConcepts(t *testing.T) {
	root := localfs.New(t.TempDir())
	reader := root.Scope("user", "project")
	if _, err := reader.WriteBytes(context.Background(), []byte(`{"slug":"generic","title":"Generic","body":"coffee"}`+"\n"), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}
	executor, err := newProductionQueryExecutor(config.Config{QuerySelectionEvidenceThreshold: 2}, conceptcache.New())
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), reader, query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 0 || result.Status != "insufficient_evidence" || result.Reason != "no_qualified_evidence" {
		t.Fatalf("result=%#v, want zero concepts with truthful insufficient-evidence status", result)
	}
}

type productionCancellationTransport struct {
	started     chan struct{}
	startedOnce *sync.Once
	done        sync.WaitGroup
}

func (t *productionCancellationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.done.Add(1)
	defer t.done.Done()
	t.startedOnce.Do(func() { close(t.started) })
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func (t *productionCancellationTransport) awaitCompletion(timeout time.Duration) {
	completed := make(chan struct{})
	go func() {
		t.done.Wait()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(timeout):
	}
}

type productionRequest struct {
	Model           string          `json:"model"`
	Temperature     *float64        `json:"temperature"`
	Thinking        json.RawMessage `json:"thinking"`
	ReasoningEffort string          `json:"reasoning_effort"`
}

type productionFallbackTransport struct {
	mu       sync.Mutex
	requests []productionRequest
}

func (t *productionFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var captured productionRequest
	if err := json.Unmarshal(body, &captured); err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.requests = append(t.requests, captured)
	requestCount := len(t.requests)
	t.mu.Unlock()
	content := "not-json"
	if requestCount == 2 {
		content = `{"keywords":["coffee"]}`
	}
	response := `{"choices":[{"message":{"content":` + strconv.Quote(content) + `}}]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
}

func TestSwaggerDoesNotDocumentAuthRoutes(t *testing.T) {
	document := readSwaggerDocument(t)
	for path := range document.Paths {
		if strings.HasPrefix(path, "/api/v1/auth/") {
			t.Errorf("BFF Swagger documents auth route %s, but Auth owns that public surface", path)
		}
	}
}

func TestSwaggerDocumentsPublicVersionRoute(t *testing.T) {
	document := readSwaggerDocument(t)
	operation, ok := document.Paths["/api/v1/public/version"]
	if !ok {
		t.Fatal("Swagger document is missing /api/v1/public/version")
	}
	get, ok := operation["get"]
	if !ok {
		t.Fatal("Swagger document is missing GET /api/v1/public/version")
	}
	var response struct {
		Responses map[string]struct {
			Schema struct {
				Ref string `json:"$ref"`
			} `json:"schema"`
			Headers map[string]json.RawMessage `json:"headers"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(get, &response); err != nil {
		t.Fatalf("decode version operation: %v", err)
	}
	status, ok := response.Responses["200"]
	if !ok {
		t.Fatal("Swagger document is missing 200 response for public version")
	}
	if status.Schema.Ref != "#/definitions/buildinfo.Info" {
		t.Fatalf("public version response = %q, want buildinfo.Info", status.Schema.Ref)
	}
	if _, ok := status.Headers["Cache-Control"]; !ok {
		t.Fatal("Swagger document is missing Cache-Control response header for public version")
	}
	var definition struct {
		Definitions map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	data, err := os.ReadFile("../../docs/swagger.json")
	if err != nil {
		t.Fatalf("read generated Swagger document: %v", err)
	}
	if err := json.Unmarshal(data, &definition); err != nil {
		t.Fatalf("decode generated Swagger definitions: %v", err)
	}
	properties := definition.Definitions["buildinfo.Info"].Properties
	if _, ok := properties["branch"]; !ok {
		t.Fatal("public version Swagger schema is missing branch")
	}
	if _, ok := properties["ref"]; ok {
		t.Fatal("public version Swagger schema must not expose generic ref")
	}
}

func TestSwaggerQueryRequestDoesNotExposeProjectField(t *testing.T) {
	data, err := os.ReadFile("../../docs/swagger.json")
	if err != nil {
		t.Fatalf("read generated Swagger document: %v", err)
	}
	var artifact struct {
		Definitions map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode generated Swagger document: %v", err)
	}
	definition, ok := artifact.Definitions["handler.QueryRequest"]
	if !ok {
		t.Fatal("swagger definitions do not include handler.QueryRequest")
	}
	if _, ok := definition.Properties["project"]; ok {
		t.Fatal(`swagger schema still advertises "project" for handler.QueryRequest`)
	}
}

func TestSwaggerOwnerRenameContractIsExactAcrossArtifacts(t *testing.T) {
	data, err := os.ReadFile("../../docs/swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var jsonDoc struct {
		Paths       map[string]map[string]json.RawMessage `json:"paths"`
		Definitions map[string]struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &jsonDoc); err != nil {
		t.Fatal(err)
	}
	operation := jsonDoc.Paths["/api/v1/projects/{projectID}"]["patch"]
	var patch struct {
		Security   []map[string][]string `json:"security"`
		Parameters []struct {
			In     string `json:"in"`
			Schema struct {
				Ref string `json:"$ref"`
			} `json:"schema"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(operation, &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch.Security) != 2 || patch.Security[0]["BearerAuth"] == nil || patch.Security[1]["DevUserAuth"] == nil {
		t.Fatalf("security=%v, want BearerAuth and DevUserAuth", patch.Security)
	}
	var bodySchemaRef string
	for _, parameter := range patch.Parameters {
		if parameter.In == "body" {
			bodySchemaRef = parameter.Schema.Ref
		}
	}
	if bodySchemaRef != "#/definitions/v1.renameProjectRequest" {
		t.Fatalf("body schema=%q, want named renameProjectRequest", bodySchemaRef)
	}
	body := jsonDoc.Definitions["v1.renameProjectRequest"]
	if len(body.Required) != 1 || body.Required[0] != "name" || body.AdditionalProperties == nil || *body.AdditionalProperties {
		t.Fatalf("rename request schema=%+v, want required name and additionalProperties false", body)
	}

	for _, path := range []string{"../../docs/swagger.yaml", "../../docs/docs.go"} {
		artifact, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(path, ".yaml") {
			var yamlDoc map[string]interface{}
			if err := yaml.Unmarshal(artifact, &yamlDoc); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			if !strings.Contains(string(artifact), "v1.renameProjectRequest") || !strings.Contains(string(artifact), "additionalProperties: false") {
				t.Fatalf("%s is missing the named closed request schema", path)
			}
		} else if !strings.Contains(string(artifact), `"v1.renameProjectRequest"`) || !strings.Contains(string(artifact), `"additionalProperties": false`) {
			t.Fatalf("%s is missing the named closed request schema", path)
		}
	}
}

func TestPublicVersionRouteDoesNotRequireAuthOrProject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")
	v1.Use(auth.JWTAuth(config.Config{JWTSecret: "test-secret"}), auth.ProjectMiddleware())
	registerPublicRoutes(router, &syssettings.FakeStore{Enabled: true}, "https://auth.dev.rayer.idv.tw")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public version status = %d, want %d without authentication or project header", recorder.Code, http.StatusOK)
	}
}

func TestProductionRouterRegistersRawScrapeRouteUnderAuthAndProjectMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var capturedScopedClient *gcs.Client
	recordingFactory := func(scopedClient *gcs.Client, fsClient *firestore.Client) rawScrapeHandler {
		capturedScopedClient = scopedClient
		return rawScrapeTestHandler{}
	}

	router := newProductionRouter(
		config.Config{DevJWT: true, JWTSecret: "test-secret", AllowedOrigins: []string{"http://example.test"}},
		false,
		&gcs.Client{},
		nil,
		handlerv1.New(nil, nil, nil, nil, nil, nil),
		&syssettings.FakeStore{Enabled: true},
		recordingFactory,
	)

	registered := 0
	for _, route := range router.Routes() {
		if route.Method == http.MethodPost && route.Path == "/api/v1/raw/scrape" {
			registered++
		}
	}
	if registered != 1 {
		t.Fatalf("POST /api/v1/raw/scrape registered %d times, want exactly once", registered)
	}

	request := func(userID, projectID string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/raw/scrape", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if userID != "" {
			req.Header.Set("X-User-ID", userID)
		}
		if projectID != "" {
			req.Header.Set("X-Project-ID", projectID)
		}
		router.ServeHTTP(recorder, req)
		return recorder
	}

	if recorder := request("", "demo"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing DevJWT user status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	recorder := request("tenant-user", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing project status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var missingProjectBody map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &missingProjectBody); err != nil {
		t.Fatalf("decode missing-project response: %v", err)
	}
	if got := missingProjectBody["error"]; got != "invalid X-Project-ID header" {
		t.Fatalf("missing project error = %q, want %q", got, "invalid X-Project-ID header")
	}

	if capturedScopedClient != nil {
		t.Fatal("project-scoped raw scrape handler factory should not have been invoked without middleware passing projectID")
	}

	recorder = request("tenant-user", "demo")
	if recorder.Code == http.StatusNotFound {
		t.Fatal("unexpected 404 for production POST /api/v1/raw/scrape")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("production raw scrape status = %d, want %d from scrape handler", recorder.Code, http.StatusBadRequest)
	}
	if capturedScopedClient == nil {
		t.Fatal("raw scrape factory was not invoked for authenticated user+project request")
	}
	if got := capturedScopedClient.Prefix(); got != "users/tenant-user/projects/demo" {
		t.Fatalf("scoped gcs client prefix = %q, want %q", got, "users/tenant-user/projects/demo")
	}

	var rawBody map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &rawBody); err != nil {
		t.Fatalf("decode raw scrape response: %v", err)
	}
	if got := rawBody["error"]; got != "fake raw scrape handler" {
		t.Fatalf("raw scrape error = %q, want %q", got, "fake raw scrape handler")
	}
}

func TestProductionRouterRegistersOwnerProjectRenameRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(
		config.Config{DevJWT: true, JWTSecret: "test-secret", AllowedOrigins: []string{"http://example.test"}},
		false,
		nil,
		nil,
		handlerv1.New(nil, nil, nil, nil, nil, nil),
		&syssettings.FakeStore{Enabled: true},
		nil,
	)

	registered := 0
	for _, route := range router.Routes() {
		if route.Method == http.MethodPatch && route.Path == "/api/v1/projects/:projectID" {
			registered++
		}
	}
	if registered != 1 {
		t.Fatalf("PATCH /api/v1/projects/:projectID registered %d times, want exactly once", registered)
	}
}

func TestProductionRouterOwnerProjectRenameUsesAuthenticatedOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var gotUserID, gotProjectID, gotName string
	h := handlerv1.New(nil, nil, nil, nil, nil, nil)
	h.SetProjectRenameFunc(func(_ context.Context, userID, projectID, name string) error {
		gotUserID, gotProjectID, gotName = userID, projectID, name
		return nil
	})
	router := newProductionRouter(
		config.Config{DevJWT: true, JWTSecret: "test-secret", AllowedOrigins: []string{"http://example.test"}},
		false,
		nil,
		nil,
		h,
		&syssettings.FakeStore{Enabled: true},
		nil,
	)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/project-1", strings.NewReader(`{"name":"  Renamed  "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "owner-1")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotUserID != "owner-1" || gotProjectID != "project-1" || gotName != "Renamed" {
		t.Fatalf("rename args=(%q,%q,%q)", gotUserID, gotProjectID, gotName)
	}
}

func TestProductionRouterOwnerProjectRenameKeepsExistingAnonymousAuthBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(
		config.Config{JWTSecret: "test-secret", AllowedOrigins: []string{"http://example.test"}},
		false,
		nil,
		nil,
		handlerv1.New(nil, nil, nil, nil, nil, nil),
		&syssettings.FakeStore{Enabled: true},
		nil,
	)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/api/v1/projects/project-1", strings.NewReader(`{"name":"Renamed"}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s, want 401", recorder.Code, recorder.Body.String())
	}
}

type rawScrapeTestHandler struct{}

func (rawScrapeTestHandler) ScrapeRaw(c *gin.Context) {
	c.JSON(http.StatusBadRequest, handlerraw.ErrorResponse{Error: "fake raw scrape handler"})
}

func readSwaggerDocument(t *testing.T) struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
} {
	t.Helper()
	data, err := os.ReadFile("../../docs/swagger.json")
	if err != nil {
		t.Fatalf("read generated Swagger document: %v", err)
	}
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode generated Swagger document: %v", err)
	}
	return document
}

func TestObservabilityServiceNameUsesCloudRunService(t *testing.T) {
	tests := []struct {
		name     string
		kService string
		want     string
	}{
		{name: "production", kService: "llm-wiki-bff", want: "llm-wiki-bff"},
		{name: "development", kService: "llm-wiki-bff-dev", want: "llm-wiki-bff-dev"},
		{name: "local fallback", want: "llm-wiki-bff-dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := observabilityServiceName(tt.kService); got != tt.want {
				t.Fatalf("observabilityServiceName(%q) = %q, want %q", tt.kService, got, tt.want)
			}
		})
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.SecurityHeaders(true))
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	headers := rec.Header()
	want := map[string]string{
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
	}
	for header, value := range want {
		if got := headers.Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestSecurityHeadersMiddlewareSkipsHSTSWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.SecurityHeaders(false))
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("Strict-Transport-Security = %q, want empty when HSTS disabled", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

func TestAuthRateLimitBlocksAfterLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/login", middleware.NewRateLimiter(10, time.Minute), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	rateLimited := false
	for i := 0; i < 15; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", nil)
		req.Header.Set("CF-Connecting-IP", "203.0.113.20")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			rateLimited = true
			break
		}
	}
	if !rateLimited {
		t.Fatal("expected 429 after repeated requests, never blocked")
	}
}

func TestAdminProjectsRouteRequiresAdminRole(t *testing.T) {
	router := newAdminRouteTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/projects", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	userToken, err := auth.GenerateAccessToken("user-123", "", "test-secret")
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/projects", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAdminProjectsRouteDoesNotRequireProjectHeader(t *testing.T) {
	router := newAdminRouteTestRouter(t)
	adminToken, err := auth.GenerateAccessToken("admin-user", "admin", "test-secret")
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/projects", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("admin route was blocked by project middleware: %s", rec.Body.String())
	}
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("admin route status = %d, want registered route after admin auth", rec.Code)
	}
}

func TestAdminSettingsRouteRequiresAdminRole(t *testing.T) {
	router := newAdminRouteTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	userToken, err := auth.GenerateAccessToken("user-123", "", "test-secret")
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func newAdminRouteTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	handler := handlerv1.New(nil, nil, nil, nil, nil, nil)
	settingsStore := &syssettings.FakeStore{Enabled: true}
	registerAdminRoutes(router, config.Config{JWTSecret: "test-secret"}, handler, settingsStore)
	return router
}
