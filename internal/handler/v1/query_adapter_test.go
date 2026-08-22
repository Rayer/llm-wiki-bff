package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestV1QueryDelegatesTrimmedDefaultRequestAndScopedReader(t *testing.T) {
	root := newQueryAdapterRoot(t)
	executor := &recordingQueryExecutor{result: query.Result{Query: "coffee", Mode: "wiki", Results: []search.Result{{Slug: "coffee"}}}}
	h := New(root, nil, search.NewIndex(), cache.New(), nil, nil)
	h.queryExecutor = executor

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"  coffee  "}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if executor.reader == nil || executor.reader != root.scoped {
		t.Fatalf("reader = %#v, want exact scoped reader %#v", executor.reader, root.scoped)
	}
	if executor.request != (query.Request{Query: "coffee", Mode: "wiki"}) {
		t.Fatalf("request = %#v, want exact trimmed query and default mode", executor.request)
	}
	if executor.ctx == nil {
		t.Fatal("executor did not receive request context")
	}
	var response handler.QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Query != "coffee" || response.Mode != "wiki" || len(response.Results) != 1 || response.Results[0].Slug != "coffee" {
		t.Fatalf("mapped response = %#v", response)
	}
}

func TestMapQueryResultIncludesOptionalEvidenceMetadata(t *testing.T) {
	response := mapQueryResult(query.Result{Query: "q", Mode: "full", AnswerBasis: "model_prior", WikiEvidenceStatus: "no_relevant_evidence", DisclosureRequired: true})
	if response.AnswerBasis != "model_prior" || response.WikiEvidenceStatus != "no_relevant_evidence" || !response.DisclosureRequired {
		t.Fatalf("response=%#v", response)
	}
}

func TestV1QuerySetsOnlyAllowlistedRuntimeIdentityHeaders(t *testing.T) {
	identity := query.RuntimeConfigIdentity{
		ConfigRevision: "rev", ConfigDigest: "sha256:config", EffectiveConfigDigest: "sha256:effective",
		QueryServiceImplementation: "query-retrieval-pipeline-v2", ProfileID: "profile", ProfileDigest: "sha256:profile",
		PromptID: "prompt", PromptDigest: "sha256:prompt", BindingSource: "corpus_derived_approximation", ExactBinding: true,
		GenerationID: "generation", ConceptsDigest: "sha256:concepts",
	}
	h := New(newQueryAdapterRoot(t), nil, search.NewIndex(), cache.New(), nil, nil)
	h.queryExecutor = &recordingQueryExecutor{result: query.Result{Query: "coffee", Mode: "wiki", RuntimeConfigIdentity: &identity}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee"}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for key, want := range map[string]string{
		"X-Query-Config-Revision": "rev", "X-Query-Config-Digest": "sha256:config", "X-Query-Effective-Config-Digest": "sha256:effective",
		"X-Query-Service-Implementation": "query-retrieval-pipeline-v2", "X-Query-Profile-ID": "profile", "X-Query-Profile-Digest": "sha256:profile",
		"X-Query-Prompt-ID": "prompt", "X-Query-Prompt-Digest": "sha256:prompt", "X-Query-Binding-Source": "corpus_derived_approximation",
		"X-Query-Binding-Exact": "true", "X-Query-Generation-ID": "generation", "X-Query-Concepts-Digest": "sha256:concepts",
	} {
		if got := recorder.Header().Get(key); got != want {
			t.Fatalf("header %s=%q, want %q", key, got, want)
		}
	}
}

type readbackExecutor struct {
	query.Executor
	readback query.RuntimeConfigReadback
}

func (e readbackExecutor) Readback() query.RuntimeConfigReadback { return e.readback }

func TestQueryConfigReadbackIsPublicAndLegacyIsUnavailable(t *testing.T) {
	readback := query.RuntimeConfigReadback{SchemaVersion: 2, ConfigRevision: "rev", ConfigDigest: "sha256:config", DefaultProfileID: "profile", DefaultPromptID: "prompt", BindingCount: 1, DistinctServiceCompositionCount: 1}
	h := New(nil, nil, nil, nil, nil, nil)
	h.queryExecutor = readbackExecutor{readback: readback}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/query/config", nil)
	h.QueryConfig(c)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), `"config_revision":"rev"`) {
		t.Fatalf("readback status/body=%d/%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{"project_id": true, "generation_id": true, "concepts_digest": true, "binding_count": true, "distinct_service_composition_count": true, "path": true, "system_template": true, "user_template": true, "api_key": true, "credential": true}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if forbidden[key] {
					t.Fatalf("public query config contains forbidden key %q", key)
				}
				visit(child)
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(response)
	h.queryExecutor = query.NewService(nil, nil, nil)
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/query/config", nil)
	h.QueryConfig(c)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("legacy readback status=%d, want 503", recorder.Code)
	}
}

func TestV1QueryMapsSentinelAndGenericErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		body string
	}{
		{name: "cache", err: query.ErrCacheNotConfigured, body: `{"error":"concept cache is not configured"}`},
		{name: "generic", err: errors.New("search unavailable"), body: `{"error":"generated data unavailable"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := New(newQueryAdapterRoot(t), nil, search.NewIndex(), cache.New(), nil, nil)
			h.queryExecutor = &recordingQueryExecutor{err: test.err}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee"}`))
			c.Set("userID", "user")
			c.Set("projectID", "project")
			h.Query(c)
			if recorder.Code != http.StatusInternalServerError || strings.TrimSpace(recorder.Body.String()) != test.body {
				t.Fatalf("status/body = %d/%s, want 500/%s", recorder.Code, recorder.Body.String(), test.body)
			}
		})
	}
}

func TestV1QueryNilExecutorReturnsGenericError(t *testing.T) {
	h := New(newQueryAdapterRoot(t), nil, search.NewIndex(), cache.New(), nil, nil)
	h.queryExecutor = nil
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee"}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)
	if recorder.Code != http.StatusInternalServerError || strings.TrimSpace(recorder.Body.String()) != `{"error":"generated data unavailable"}` {
		t.Fatalf("status/body = %d/%s, want 500/generated-data-unavailable", recorder.Code, recorder.Body.String())
	}
}

func TestV1QueryRejectsEmptyQueryAfterTrim(t *testing.T) {
	executor := &recordingQueryExecutor{}
	h := New(newQueryAdapterRoot(t), nil, search.NewIndex(), cache.New(), nil, nil)
	h.queryExecutor = executor
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"   "}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)
	if recorder.Code != http.StatusBadRequest || executor.request.Query != "" {
		t.Fatalf("status/request = %d/%#v, want 400 and no service call", recorder.Code, executor.request)
	}
}

func TestV1QueryStorageFailureWinsBeforeRequestValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		root    storage.RootStore
		userID  string
		project string
		body    string
	}{
		{name: "store failure malformed JSON", root: nil, userID: "user", project: "project", body: "{"},
		{name: "scope failure empty query", root: newQueryAdapterRoot(t), userID: "user", body: `{"q":"   "}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := New(test.root, nil, search.NewIndex(), cache.New(), nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(test.body))
			c.Set("userID", test.userID)
			c.Set("projectID", test.project)
			h.Query(c)
			if recorder.Code != http.StatusInternalServerError || strings.TrimSpace(recorder.Body.String()) != `{"error":"generated data unavailable"}` {
				t.Fatalf("status/body = %d/%s, want 500/generated-data-unavailable", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestV1QueryResponseMatchesApplicationResult(t *testing.T) {
	root := newQueryAdapterRoot(t)
	reader := root.RootStore.Scope("user", "project")
	if _, err := reader.WriteBytes(context.Background(), []byte(`{"slug":"coffee-shops","title":"Coffee Shops","body":"coffee body"}
`), cache.GCSPath); err != nil {
		t.Fatal(err)
	}
	conceptCache := cache.New()
	h := New(root, nil, search.NewIndex(), conceptCache, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee"}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var got handler.QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	result, err := query.NewService(conceptCache, nil, nil).Execute(context.Background(), root.scoped.(cache.Reader), query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, mapQueryResult(result)) {
		t.Fatalf("handler/application mismatch: handler=%#v application=%#v", got, result)
	}
}

func TestV1QueryModelPriorNoEvidenceSerializesExplicitEmptyCitations(t *testing.T) {
	root := newQueryAdapterRoot(t)
	h := New(root, nil, search.NewIndex(), cache.New(), nil, nil)
	h.queryExecutor = &recordingQueryExecutor{result: query.Result{
		Query: "unsupported", Mode: "full", Results: []search.Result{}, Citations: []search.Citation{},
		AISynth: "model prior answer", Status: "insufficient_evidence", Reason: "no_qualified_evidence",
		AnswerBasis: "model_prior", WikiEvidenceStatus: "no_relevant_evidence", DisclosureRequired: true,
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"unsupported","mode":"full"}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var wire map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if citations, ok := wire["citations"]; !ok || !reflect.DeepEqual(citations, []any{}) {
		t.Fatalf("citations = %#v, present=%v, want literal []: body=%s", citations, ok, recorder.Body.String())
	}
	for key, want := range map[string]any{
		"ai_synth": "model prior answer", "status": "insufficient_evidence", "reason": "no_qualified_evidence",
		"answer_basis": "model_prior", "wiki_evidence_status": "no_relevant_evidence", "disclosure_required": true,
	} {
		if wire[key] != want {
			t.Fatalf("%s = %#v, want %#v; body=%s", key, wire[key], want, recorder.Body.String())
		}
	}
}

func TestLegacyQueryResponseStillOmitsNilCitations(t *testing.T) {
	data, err := json.Marshal(handler.QueryResponse{Query: "q", Mode: "wiki", Results: []search.Result{}})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["citations"]; ok {
		t.Fatalf("legacy nil citations unexpectedly became present: %s", data)
	}
}

type recordingQueryExecutor struct {
	request query.Request
	reader  cache.Reader
	ctx     context.Context
	result  query.Result
	err     error
}

func (e *recordingQueryExecutor) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	e.ctx, e.reader, e.request = ctx, reader, request
	return e.result, e.err
}

type queryAdapterRoot struct {
	storage.RootStore
	scoped storage.Store
}

func (r *queryAdapterRoot) Scope(userID, projectID string) storage.Store {
	r.scoped = r.RootStore.Scope(userID, projectID)
	return r.scoped
}

func newQueryAdapterRoot(t *testing.T) *queryAdapterRoot {
	t.Helper()
	return &queryAdapterRoot{RootStore: localfs.New(t.TempDir())}
}

func TestQueryAdapterMappingIsExplicit(t *testing.T) {
	result := query.Result{Query: "q", Mode: "full", Results: []search.Result{{Slug: "s"}}}
	got := mapQueryResult(result)
	want := handler.QueryResponse{Query: "q", Mode: "full", Results: []search.Result{{Slug: "s"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapQueryResult = %#v, want %#v", got, want)
	}
}

func TestQueryResponseDoesNotExposeConceptSnippets(t *testing.T) {
	result := query.Result{Query: "q", Results: []search.Result{{Slug: "s", Title: "Title", Snippet: "private concept body"}}}
	data, err := json.Marshal(mapQueryResult(result))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private concept body") {
		t.Fatalf("wire response leaked concept snippet: %s", data)
	}
}
