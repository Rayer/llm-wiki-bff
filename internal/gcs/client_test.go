package gcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	cloudstorage "cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNewScopedClientUsesRequestedPrefixWithoutChangingDefault(t *testing.T) {
	defaultClient := &Client{userID: "default-user", projectID: "default-project"}

	scopedClient := defaultClient.NewScopedClient("request-user", "request-project")

	if got := scopedClient.Prefix(); got != "users/request-user/projects/request-project" {
		t.Fatalf("scoped prefix = %q, want %q", got, "users/request-user/projects/request-project")
	}
	if got := defaultClient.Prefix(); got != "users/default-user/projects/default-project" {
		t.Fatalf("default prefix = %q, want %q", got, "users/default-user/projects/default-project")
	}
	if scopedClient.bucket != defaultClient.bucket {
		t.Fatal("scoped client does not share the default client's bucket handle")
	}
}

func TestConditionalWriteErrorMapsGenerationPreconditions(t *testing.T) {
	for _, err := range []error{
		status.Error(codes.FailedPrecondition, "stale"),
		&googleapi.Error{Code: 412},
	} {
		if got := conditionalWriteError(err); !errors.Is(got, store.ErrGenerationMismatch) {
			t.Fatalf("conditionalWriteError(%v) = %v", err, got)
		}
	}
	if err := errors.New("boom"); conditionalWriteError(err) != err {
		t.Fatal("non-precondition error was remapped")
	}
}

func TestProductionAtomicCopyCASConflictIsNormalized(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("copy temporary to final: %w", status.Error(codes.FailedPrecondition, "provider generation detail")),
		fmt.Errorf("copy temporary to final: %w", &googleapi.Error{Code: 412, Message: "provider generation detail"}),
	} {
		if got := conditionalWriteError(err); !errors.Is(got, store.ErrGenerationMismatch) {
			t.Fatalf("wrapped production CAS error = %v", got)
		}
	}
}

func TestObjectNotFoundPreservesStorageSentinel(t *testing.T) {
	if !objectNotFound(cloudstorage.ErrObjectNotExist) || !objectNotFound(status.Error(codes.NotFound, "missing")) {
		t.Fatal("missing object errors were not recognized")
	}
}

func TestTemporaryObjectCleanupUsesExactGeneration(t *testing.T) {
	conditions := temporaryObjectDeleteConditions(42)
	if conditions.GenerationMatch != 42 || conditions.DoesNotExist {
		t.Fatalf("temporary cleanup conditions = %#v, want GenerationMatch(42)", conditions)
	}
}

type atomicCleanupProbeBackend struct {
	*memoryBackend
	cleanupGeneration int64
	hasDeadline       bool
}

func (b *atomicCleanupProbeBackend) Delete(ctx context.Context, name string, objectGeneration int64) error {
	if strings.HasSuffix(name, "/cache/id_map.json.tmp") {
		b.cleanupGeneration = objectGeneration
		_, b.hasDeadline = ctx.Deadline()
	}
	return b.memoryBackend.Delete(ctx, name, objectGeneration)
}

func TestAtomicTemporaryCleanupIsBoundedAndBestEffort(t *testing.T) {
	backend := &atomicCleanupProbeBackend{memoryBackend: &memoryBackend{objects: make(map[string]backendObject), nextGeneration: 1000}}
	client := &Client{userID: "user", projectID: "project", backend: backend}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.WriteBytesAtomic(ctx, []byte("atomic"), "cache/id_map.json.tmp", "raw/final.md"); err != nil {
		t.Fatalf("WriteBytesAtomic() error = %v, want committed success despite cleanup context", err)
	}
	if backend.cleanupGeneration <= 0 || !backend.hasDeadline {
		t.Fatalf("cleanup generation=%d deadline=%v, want exact generation and bounded fresh context", backend.cleanupGeneration, backend.hasDeadline)
	}
	if _, err := backend.Read(context.Background(), projectObject("cache/id_map.json.tmp"), 0, generation.MaxFileBytes); !errors.Is(err, cloudstorage.ErrObjectNotExist) {
		t.Fatalf("temporary object remains: %v", err)
	}
}

func TestApplyWikiPageFrontmatterIncludesID(t *testing.T) {
	page := WikiPage{
		Slug:  "alpha",
		Title: "alpha",
		Path:  "wiki/alpha.md",
	}
	data := []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha Concept\n---\nBody")

	got, err := applyWikiPageFrontmatter(page, data)
	if err != nil {
		t.Fatalf("apply frontmatter: %v", err)
	}

	if got.ID != "a3f7b2c01d9d" {
		t.Fatalf("id = %q, want %q", got.ID, "a3f7b2c01d9d")
	}
	if got.Title != "Alpha Concept" {
		t.Fatalf("title = %q, want %q", got.Title, "Alpha Concept")
	}
	if got.Slug != page.Slug || got.Path != page.Path {
		t.Fatalf("page metadata changed unexpectedly: %#v", got)
	}
}

func TestWikiPagesFromConceptsJSONLReturnsConceptPages(t *testing.T) {
	data := []byte(strings.Join([]string{
		`{"slug":"alpha","title":"Alpha","frontmatter":{"id":"concept-id"},"sources":["source-one"]}`,
		`{"slug":"beta","title":"","frontmatter":{}}`,
		``,
	}, "\n"))

	pages, err := WikiPagesFromConceptsJSONL(data)
	if err != nil {
		t.Fatalf("wikiPagesFromConceptsJSONL: %v", err)
	}

	if len(pages) != 2 {
		t.Fatalf("len(pages) = %d, want 2", len(pages))
	}
	if pages[0].Slug != "alpha" || pages[0].Title != "Alpha" || pages[0].ID != "concept-id" || pages[0].Path != "wiki/alpha.md" || pages[0].Status != "published" {
		t.Fatalf("alpha page = %#v", pages[0])
	}
	if pages[1].Slug != "beta" || pages[1].Title != "beta" || pages[1].Path != "wiki/beta.md" {
		t.Fatalf("beta page = %#v", pages[1])
	}
}

func TestWikiPagesFromSourceIDMapReturnsSourcePages(t *testing.T) {
	data := []byte(`{"concept":{"concept-id":"alpha"},"source":{"source-id":"source-one","":"ignored-empty-id","blank-slug":" "}}`)

	pages, err := WikiPagesFromSourceIDMap(data)
	if err != nil {
		t.Fatalf("wikiPagesFromSourceIDMap: %v", err)
	}

	if len(pages) != 1 {
		t.Fatalf("len(pages) = %d, want 1", len(pages))
	}
	if pages[0].Slug != "source-one" || pages[0].Title != "source-one" || pages[0].ID != "source-id" || pages[0].Path != "wiki/sources/source-one.md" {
		t.Fatalf("source page = %#v", pages[0])
	}
}

func TestGeneratedCacheLogicalEntryBounds(t *testing.T) {
	concepts := strings.Repeat(`{"slug":"concept"}`+"\n", generation.MaxFiles)
	if pages, err := WikiPagesFromConceptsJSONL([]byte(concepts)); err != nil || len(pages) != generation.MaxFiles {
		t.Fatalf("exact concepts boundary = %d, %v; want %d, nil", len(pages), err, generation.MaxFiles)
	}
	if _, err := WikiPagesFromConceptsJSONL([]byte(concepts + `{"slug":"overflow"}` + "\n")); err == nil || err.Error() != "generated cache logical entry limit exceeded" {
		t.Fatalf("concept overflow error = %v, want fixed logical-entry error", err)
	}

	var source strings.Builder
	source.WriteString(`{"source":{`)
	for i := 0; i < generation.MaxFiles; i++ {
		if i > 0 {
			source.WriteByte(',')
		}
		encoded, _ := json.Marshal(fmt.Sprintf("id-%d", i))
		fmt.Fprintf(&source, "%s:\"slug\"", encoded)
	}
	source.WriteString(",\"overflow\":\"slug\"}}")
	if _, err := WikiPagesFromSourceIDMap([]byte(source.String())); err == nil || !errors.Is(err, generation.ErrLogicalEntryLimit) {
		t.Fatalf("id-map overflow error = %v, want fixed logical-entry error", err)
	}

	source.Reset()
	source.WriteString(`{"source":{`)
	for i := 0; i < generation.MaxFiles; i++ {
		if i > 0 {
			source.WriteByte(',')
		}
		encoded, _ := json.Marshal(fmt.Sprintf("id-%d", i))
		fmt.Fprintf(&source, "%s:\"slug\"", encoded)
	}
	source.WriteString("}}")
	if pages, err := WikiPagesFromSourceIDMap([]byte(source.String())); err != nil || len(pages) != generation.MaxFiles {
		t.Fatalf("exact id-map boundary = %d, %v; want %d, nil", len(pages), err, generation.MaxFiles)
	}
}

func TestWikiPagesFromSourceIDMapBoundsRedirectListsAndKeys(t *testing.T) {
	redirects := func(listSize, keyCount int) []byte {
		var data strings.Builder
		data.WriteString(`{"redirects":{`)
		for key := 0; key < keyCount; key++ {
			if key > 0 {
				data.WriteByte(',')
			}
			keyJSON, _ := json.Marshal(fmt.Sprintf("redirect-%d", key))
			fmt.Fprintf(&data, "%s:[", keyJSON)
			for value := 0; value < listSize; value++ {
				if value > 0 {
					data.WriteByte(',')
				}
				valueJSON, _ := json.Marshal(fmt.Sprintf("target-%d", value))
				data.Write(valueJSON)
			}
			data.WriteString(`]`)
		}
		data.WriteString(`}}`)
		return []byte(data.String())
	}

	if _, err := WikiPagesFromSourceIDMap(redirects(generation.MaxFiles, 2)); err != nil {
		t.Fatalf("exact nested redirect boundary: %v", err)
	}
	if _, err := WikiPagesFromSourceIDMap(redirects(generation.MaxFiles+1, 2)); !errors.Is(err, generation.ErrLogicalEntryLimit) {
		t.Fatalf("nested redirect overflow = %v, want logical-entry limit", err)
	}
	if _, err := WikiPagesFromSourceIDMap(redirects(1, generation.MaxFiles)); err != nil {
		t.Fatalf("exact redirect-key boundary: %v", err)
	}
	if _, err := WikiPagesFromSourceIDMap(redirects(1, generation.MaxFiles+1)); !errors.Is(err, generation.ErrLogicalEntryLimit) {
		t.Fatalf("redirect-key overflow = %v, want logical-entry limit", err)
	}
}

func TestWikiPagesFromSourceIDMapRejectsMalformedOrTrailingRedirectJSON(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"redirects":{"id":["target"]}`),
		[]byte(`{"redirects":{"id":["target"]}} {"trailing":true}`),
	} {
		if _, err := WikiPagesFromSourceIDMap(data); err == nil {
			t.Fatalf("accepted malformed/trailing id map: %s", data)
		}
	}
}
