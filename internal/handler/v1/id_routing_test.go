package v1

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
)

func TestBuildDualIDMapPrefersConceptForDuplicateSlug(t *testing.T) {
	source := idMap{
		Concept: map[string]string{
			"a3f7b2c01d9d": "shared-slug",
		},
		Source: map[string]string{
			"c5d9e3f1a028": "shared-slug",
		},
	}

	dual := buildDualIDMap(source)

	if got := dual.byID["a3f7b2c01d9d"]; got.Type != "concept" || got.Slug != "shared-slug" {
		t.Fatalf("concept id route = %#v", got)
	}
	if got := dual.byID["c5d9e3f1a028"]; got.Type != "source" || got.Slug != "shared-slug" {
		t.Fatalf("source id route = %#v", got)
	}
	entries := dual.bySlug["shared-slug"]
	if len(entries) != 2 {
		t.Fatalf("shared slug entries = %#v, want 2 entries", entries)
	}
	if entries[0].Type != "concept" || entries[1].Type != "source" {
		t.Fatalf("shared slug order = %#v, want concept then source", entries)
	}
}

func TestRewriteWikilinksEmitsCanonicalTargetsWithSlugLabels(t *testing.T) {
	dual := buildDualIDMap(idMap{
		Concept: map[string]string{
			"a3f7b2c01d9d": "alpha",
			"b7e2c9a4d113": "shared",
		},
		Source: map[string]string{
			"c5d9e3f1a028": "source-one",
			"d4c8f9b0a177": "shared",
		},
	})
	input := "[[alpha]] [[alpha|Alias]] [[alpha#part|Section]] [[source-one]] [[missing]] [[concepts/a3f7b2c01d9d-alpha|Already]] [[sources/c5d9e3f1a028-source-one]] [[shared|Shared]]"

	got := rewriteWikilinks(input, dual)

	want := "[[concepts/a3f7b2c01d9d-alpha|alpha]] [[concepts/a3f7b2c01d9d-alpha|Alias]] [[concepts/a3f7b2c01d9d-alpha#part|Section]] [[sources/c5d9e3f1a028-source-one|source-one]] [[missing]] [[concepts/a3f7b2c01d9d-alpha|Already]] [[sources/c5d9e3f1a028-source-one|source-one]] [[concepts/b7e2c9a4d113-shared|Shared]]"
	if got != want {
		t.Fatalf("rewriteWikilinks = %q, want %q", got, want)
	}
}

func TestIDRouteRedirectStatusIsTemporary(t *testing.T) {
	if idRouteRedirectStatus != http.StatusFound {
		t.Fatalf("idRouteRedirectStatus = %d, want %d", idRouteRedirectStatus, http.StatusFound)
	}
}

func TestParseIDSlug(t *testing.T) {
	id, slug, ok := parseIDSlug("a3f7b2c01d9d-alpha")
	if !ok || id != "a3f7b2c01d9d" || slug != "alpha" {
		t.Fatalf("parseIDSlug valid = (%q, %q, %v)", id, slug, ok)
	}
	if _, _, ok := parseIDSlug("a3f7b2c01d9d"); ok {
		t.Fatal("parseIDSlug accepted id-only path")
	}
	if _, _, ok := parseIDSlug("not-an-id-alpha"); ok {
		t.Fatal("parseIDSlug accepted invalid id")
	}
}

func TestCanonicalIDRouteRedirectsSlugAndTypeMismatches(t *testing.T) {
	dual := buildDualIDMap(idMap{
		Concept: map[string]string{"a3f7b2c01d9d": "alpha"},
		Source:  map[string]string{"c5d9e3f1a028": "source-one"},
	})

	target, ok := canonicalIDRoute("source", "a3f7b2c01d9d-alpha", dual)
	if !ok || target != "/concepts/a3f7b2c01d9d-alpha" {
		t.Fatalf("type mismatch route = %q, %v", target, ok)
	}
	target, ok = canonicalIDRoute("concept", "a3f7b2c01d9d-old-alpha", dual)
	if !ok || target != "/concepts/a3f7b2c01d9d-alpha" {
		t.Fatalf("slug mismatch route = %q, %v", target, ok)
	}
	target, ok = canonicalIDRoute("source", "c5d9e3f1a028", dual)
	if !ok || target != "/sources/c5d9e3f1a028-source-one" {
		t.Fatalf("id-only route = %q, %v", target, ok)
	}
	if target, ok = canonicalIDRoute("concept", "ffffffffffff-missing", dual); ok || target != "" {
		t.Fatalf("missing id route = %q, %v", target, ok)
	}
}

func TestLoadDualIDMapReturnsNotFoundForMissingFile(t *testing.T) {
	_, err := loadDualIDMap(context.Background(), missingIDMapStore{})
	if !errors.Is(err, errIDMapNotFound) {
		t.Fatalf("loadDualIDMap error = %v, want errIDMapNotFound", err)
	}
}

type missingIDMapStore struct{}

func (missingIDMapStore) ListMarkdownFiles(context.Context, string) ([]gcs.MarkdownFile, error) {
	return nil, nil
}

func (missingIDMapStore) ReadFile(context.Context, string) ([]byte, error) {
	return nil, storage.ErrObjectNotExist
}
