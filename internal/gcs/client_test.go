package gcs

import (
	"errors"
	"strings"
	"testing"

	cloudstorage "cloud.google.com/go/storage"
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

func TestObjectNotFoundPreservesStorageSentinel(t *testing.T) {
	if !objectNotFound(cloudstorage.ErrObjectNotExist) || !objectNotFound(status.Error(codes.NotFound, "missing")) {
		t.Fatal("missing object errors were not recognized")
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
