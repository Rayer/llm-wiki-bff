package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestAnnotationCreateUpdateNoopAndClear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	put := func(body, generation string) (int, handler.AnnotationResponse) {
		payload, _ := json.Marshal(handler.AnnotationRequest{Body: body, ExpectedGeneration: generation})
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPut, "/sources/s1/annotation", bytes.NewReader(payload))
		c.Params = gin.Params{{Key: "id", Value: "s1"}}
		c.Set("userID", "u")
		c.Set("projectID", "p")
		h.PutAnnotation(c)
		var response handler.AnnotationResponse
		_ = json.Unmarshal(r.Body.Bytes(), &response)
		return r.Code, response
	}
	status, created := put("one\r\ntwo", "0")
	if status != 200 || created.Body != "one\ntwo" || created.Generation == "0" {
		t.Fatalf("create: %d %+v", status, created)
	}
	status, unchanged := put("one\ntwo", created.Generation)
	if status != 200 || unchanged.Generation != created.Generation || unchanged.UpdatedAt != created.UpdatedAt {
		t.Fatalf("noop: %d %+v", status, unchanged)
	}
	status, updated := put("next", created.Generation)
	if status != 200 || updated.Generation == created.Generation || updated.Body != "next" {
		t.Fatalf("update: %d %+v", status, updated)
	}
	status, cleared := put("", updated.Generation)
	if status != 200 || cleared.HasAnnotation || cleared.SHA256 == "" {
		t.Fatalf("clear: %d %+v", status, cleared)
	}
	if status, _ := put("stale", created.Generation); status != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d", status)
	}
}

func TestListSourcesCacheDoesNotReadEachSourcePage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	path := filepath.Join(root, "users", "u", "projects", "p", "cache", "id_map.json")
	if err := os.WriteFile(path, []byte(`{"source":{"s1":"source"},"source_meta":{"s1":{"slug":"source","source_file":"raw/source.md"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int
	h := New(countingRoot{Client: localfs.New(root), getPageCalls: &calls}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/sources", nil)
	c.Set("userID", "u")
	c.Set("projectID", "p")
	h.ListSources(c)
	if r.Code != http.StatusOK || calls != 0 {
		t.Fatalf("status=%d GetPage calls=%d body=%s", r.Code, calls, r.Body.String())
	}
}

func TestListSourcesLegacyMetadataFallbackIsCached(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	var calls int
	h := New(countingRoot{Client: localfs.New(root), listSourceCalls: &calls}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	request := func() int {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodGet, "/sources", nil)
		c.Set("userID", "u")
		c.Set("projectID", "p")
		h.ListSources(c)
		return r.Code
	}
	if request() != http.StatusOK || calls != 1 {
		t.Fatalf("first fallback calls=%d", calls)
	}
	if request() != http.StatusOK || calls != 1 {
		t.Fatalf("second request repeated source collection read: %d", calls)
	}
}

func TestAnnotationHandlerContractFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	call := func(method, id, payload string) int {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(method, "/sources/"+id+"/annotation", bytes.NewBufferString(payload))
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Set("userID", "u")
		c.Set("projectID", "p")
		if method == http.MethodGet {
			h.GetAnnotation(c)
		} else {
			h.PutAnnotation(c)
		}
		return r.Code
	}
	if got := call(http.MethodPut, "s1", "{"); got != http.StatusBadRequest {
		t.Fatalf("invalid JSON = %d", got)
	}
	if got := call(http.MethodPut, "s1", `{"body":"note"}`); got != http.StatusBadRequest {
		t.Fatalf("missing generation = %d", got)
	}
	if got := call(http.MethodPut, "missing", `{"body":"note","expected_generation":"0"}`); got != http.StatusNotFound {
		t.Fatalf("unknown source = %d", got)
	}
	if got := call(http.MethodPut, "s1", `{"body":"`+strings.Repeat("x", maxAnnotationBytes+1)+`","expected_generation":"0"}`); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d", got)
	}
	annotationPath := filepath.Join(root, "users", "u", "projects", "p", "cache", "annotations", "s1.json")
	if err := os.MkdirAll(filepath.Dir(annotationPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(annotationPath, []byte(`{"version":1,"source_id":"s1","raw_path":"raw/source.md","body":"bad\\r","ann_sha256":"bad","updated_at":"bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := call(http.MethodGet, "s1", ""); got != http.StatusInternalServerError {
		t.Fatalf("malformed stored annotation = %d", got)
	}
	if got := call(http.MethodPut, "s1", `{"body":"bad\\r","expected_generation":"1"}`); got != http.StatusOK {
		t.Fatalf("malformed existing object was not repaired = %d", got)
	}
	unsafeMap := filepath.Join(root, "users", "u", "projects", "p", "wiki", "sources", "source.md")
	if err := os.WriteFile(unsafeMap, []byte("---\nid: s1\nsource_file: raw/../outside.md\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Use a fresh handler to avoid the intentionally cached routing map.
	h = New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	if got := call(http.MethodPut, "s1", `{"body":"note","expected_generation":"0"}`); got != http.StatusConflict {
		t.Fatalf("unsafe mapping = %d", got)
	}
}

func TestAnnotationRejectsInvalidUnicodeJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	call := func(payload []byte) int {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPut, "/sources/s1/annotation", bytes.NewReader(payload))
		c.Params = gin.Params{{Key: "id", Value: "s1"}}
		c.Set("userID", "u")
		c.Set("projectID", "p")
		h.PutAnnotation(c)
		return r.Code
	}
	if got := call([]byte{'{', '"', 'b', 'o', 'd', 'y', '"', ':', '"', 0xff, '"', ',', '"', 'e', 'x', 'p', 'e', 'c', 't', 'e', 'd', '_', 'g', 'e', 'n', 'e', 'r', 'a', 't', 'i', 'o', 'n', '"', ':', '"', '0', '"', '}'}); got != http.StatusBadRequest {
		t.Fatalf("invalid UTF-8 = %d", got)
	}
	for _, payload := range []string{`{"body":"\uD800","expected_generation":"0"}`, `{"body":"\uDC00","expected_generation":"0"}`} {
		if got := call([]byte(payload)); got != http.StatusBadRequest {
			t.Fatalf("%s = %d", payload, got)
		}
	}
	if got := call([]byte(`{"body":"\uD83D\uDE00","expected_generation":"0"}`)); got != http.StatusOK {
		t.Fatalf("valid surrogate pair = %d", got)
	}
}

func TestPendingWorkUsesLegacySourceMetadataCache(t *testing.T) {
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	var calls int
	h := New(countingRoot{Client: localfs.New(root), listSourceCalls: &calls}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	for i := 0; i < 2; i++ {
		if _, _, _, err := h.pendingWorkForProject(context.Background(), "u", "p"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("legacy source collection reads = %d, want 1", calls)
	}
}

func TestSourceLifecycleFallsBackToLegacyArtifact(t *testing.T) {
	for name, sourceStatus := range map[string]string{
		"absent":          "",
		"malformed":       `{"version":`,
		"wrong version":   `{"version":2,"sources":{}}`,
		"no receipt":      `{"version":1,"sources":{}}`,
		"partial receipt": `{"version":1,"sources":{"s1":{"raw_path":"raw/source.md"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAnnotationFixture(t, root, "u", "p")
			cacheDir := filepath.Join(root, "users", "u", "projects", "p", "cache")
			if err := os.WriteFile(filepath.Join(cacheDir, "raw_status.json"), []byte(`{"version":1,"files":{"source.md":{"path":"raw/source.md","olw_status":"compiled"}}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if sourceStatus != "" {
				if err := os.WriteFile(filepath.Join(cacheDir, "source_status.json"), []byte(sourceStatus), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			pages, _, err := sourceLifecycle(context.Background(), localfs.New(root).WithScope("u", "p"), []store.WikiPage{{ID: "s1", RawPath: "raw/source.md"}})
			if err != nil || len(pages) != 1 || pages[0].LifecycleStatus != "synced" {
				t.Fatalf("pages=%+v err=%v", pages, err)
			}
		})
	}
}

func TestSourceDetailResponseDecoratesCompiledSourceWithPendingRawFiles(t *testing.T) {
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	if err := os.WriteFile(filepath.Join(root, "users", "u", "projects", "p", "raw", "pending.md"), []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/sources/source", nil)
	response, err := h.sourceDetailResponse(c, localfs.New(root).WithScope("u", "p"), store.WikiPage{ID: "s1", Slug: "source", RawPath: "raw/source.md"}, []byte("---\nid: s1\nsource_file: raw/source.md\n---\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !response.AnnotationAllowed || response.LifecycleStatus != "new" || response.RawDirty || response.AnnotationDirty || response.Dirty {
		t.Fatalf("response = %+v", response)
	}
}

func TestGetSourceReturnsInternalServerErrorForLifecycleStorageErrors(t *testing.T) {
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	if err := os.WriteFile(filepath.Join(root, "users", "u", "projects", "p", "cache", "id_map.json"), []byte(`{"source":{"aaaaaaaaaaaa":"source"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "users", "u", "projects", "p", "wiki", "sources", "source.md"), []byte("---\nid: aaaaaaaaaaaa\nsource_file: raw/source.md\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"source", "aaaaaaaaaaaa-source"} {
		t.Run(path, func(t *testing.T) {
			h := New(lifecycleErrorRoot{RootStore: localfs.New(root), err: errors.New("read lifecycle")}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/sources/"+path, nil)
			c.Params = gin.Params{{Key: "id", Value: path}}
			c.Set("userID", "u")
			c.Set("projectID", "p")

			h.GetSource(c)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
			}
		})
	}
}

func TestAnnotationBackendFailuresAndProjectIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeAnnotationFixture(t, root, "u", "p")
	writeAnnotationFixture(t, root, "u", "other")
	var rebuilds int
	h := New(localfs.New(root), nil, search.NewIndex(), conceptcache.New(), nil, nil)
	h.SetRebuildIndexFunc(func(context.Context, string, string) (idMap, error) {
		rebuilds++
		return idMap{}, nil
	})
	call := func(method, project, payload string) (int, handler.AnnotationResponse) {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(method, "/sources/s1/annotation", bytes.NewBufferString(payload))
		c.Params = gin.Params{{Key: "id", Value: "s1"}}
		c.Set("userID", "u")
		c.Set("projectID", project)
		if method == http.MethodGet {
			h.GetAnnotation(c)
		} else {
			h.PutAnnotation(c)
		}
		var response handler.AnnotationResponse
		_ = json.Unmarshal(r.Body.Bytes(), &response)
		return r.Code, response
	}
	if status, created := call(http.MethodPut, "p", `{"body":"private","expected_generation":"0"}`); status != http.StatusOK || created.Generation == "0" {
		t.Fatalf("create status=%d response=%+v", status, created)
	}
	if status, other := call(http.MethodGet, "other", ""); status != http.StatusOK || other.Generation != "0" || other.Body != "" {
		t.Fatalf("cross-project annotation leaked: status=%d response=%+v", status, other)
	}
	if rebuilds != 0 {
		t.Fatalf("annotation PUT triggered pipeline/rebuild work %d times", rebuilds)
	}
	failingRead := New(annotationErrorRoot{Client: localfs.New(root), readErr: true}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/sources/s1/annotation", nil)
	c.Params = gin.Params{{Key: "id", Value: "s1"}}
	c.Set("userID", "u")
	c.Set("projectID", "p")
	failingRead.GetAnnotation(c)
	if r.Code != http.StatusInternalServerError {
		t.Fatalf("backend read error = %d", r.Code)
	}
	failingWrite := New(annotationErrorRoot{Client: localfs.New(root), writeErr: true}, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	r = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPut, "/sources/s1/annotation", bytes.NewBufferString(`{"body":"new","expected_generation":"1"}`))
	c.Params = gin.Params{{Key: "id", Value: "s1"}}
	c.Set("userID", "u")
	c.Set("projectID", "p")
	failingWrite.PutAnnotation(c)
	if r.Code != http.StatusInternalServerError {
		t.Fatalf("backend write error = %d", r.Code)
	}
}

type countingRoot struct {
	*localfs.Client
	getPageCalls    *int
	listSourceCalls *int
}

type annotationErrorRoot struct {
	*localfs.Client
	readErr  bool
	writeErr bool
}

func (r annotationErrorRoot) Scope(userID, projectID string) store.Store {
	return annotationErrorStore{Store: r.Client.Scope(userID, projectID), readErr: r.readErr, writeErr: r.writeErr}
}

type annotationErrorStore struct {
	store.Store
	readErr  bool
	writeErr bool
}

func (s annotationErrorStore) ReadFileWithGeneration(ctx context.Context, path string) ([]byte, int64, error) {
	if s.readErr {
		return nil, 0, errors.New("read failed")
	}
	return s.Store.(store.ConditionalWriter).ReadFileWithGeneration(ctx, path)
}

func (s annotationErrorStore) WriteFileIfGeneration(ctx context.Context, data []byte, path string, generation int64) (int64, error) {
	if s.writeErr {
		return 0, errors.New("write failed")
	}
	return s.Store.(store.ConditionalWriter).WriteFileIfGeneration(ctx, data, path, generation)
}

func (r countingRoot) Scope(userID, projectID string) store.Store {
	return countingStore{Store: r.Client.Scope(userID, projectID), getPageCalls: r.getPageCalls, listSourceCalls: r.listSourceCalls}
}

type countingStore struct {
	store.Store
	getPageCalls    *int
	listSourceCalls *int
}

type lifecycleErrorRoot struct {
	store.RootStore
	err error
}

func (s lifecycleErrorRoot) Scope(userID, projectID string) store.Store {
	return lifecycleErrorStore{Store: s.RootStore.Scope(userID, projectID), err: s.err}
}

type lifecycleErrorStore struct {
	store.Store
	err error
}

func (s lifecycleErrorStore) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if relPath == sourcestatus.Path {
		return nil, s.err
	}
	return s.Store.ReadFile(ctx, relPath)
}

func (s countingStore) GetPage(ctx context.Context, slug, category string) (*store.WikiPage, []byte, error) {
	if s.getPageCalls != nil {
		*s.getPageCalls++
	}
	return s.Store.GetPage(ctx, slug, category)
}

func (s countingStore) ListSources(ctx context.Context) ([]store.WikiPage, error) {
	if s.listSourceCalls != nil {
		*s.listSourceCalls++
	}
	return s.Store.ListSources(ctx)
}

func writeAnnotationFixture(t *testing.T, root, user, project string) {
	t.Helper()
	files := map[string]string{
		"cache/id_map.json":      `{"source":{"s1":"source"}}`,
		"wiki/sources/source.md": "---\nid: s1\nsource_file: raw/source.md\n---\nbody\n",
		"raw/source.md":          "raw\n",
	}
	for rel, data := range files {
		path := filepath.Join(root, "users", user, "projects", project, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
