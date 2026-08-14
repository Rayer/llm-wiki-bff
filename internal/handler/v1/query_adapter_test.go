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
