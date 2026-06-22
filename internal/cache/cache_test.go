package cache

import (
	"context"
	"errors"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/gcs"
)

type fakeReader struct {
	prefix   string
	concepts []gcs.WikiPage
	pages    map[string]string
	errs     map[string]error
	lists    int
}

func (f *fakeReader) Prefix() string {
	return f.prefix
}

func (f *fakeReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	f.lists++
	return f.concepts, nil
}

func (f *fakeReader) GetPage(_ context.Context, slug, category string) (*gcs.WikiPage, []byte, error) {
	if category != "concepts" {
		return nil, nil, errors.New("unexpected category")
	}
	if err := f.errs[slug]; err != nil {
		return nil, nil, err
	}
	return &gcs.WikiPage{Slug: slug, Title: slug}, []byte(f.pages[slug]), nil
}

func TestBuildCachesParsedPublishedConcepts(t *testing.T) {
	reader := &fakeReader{
		prefix: "users/u/projects/p",
		concepts: []gcs.WikiPage{
			{Slug: "alpha", Title: "alpha"},
			{Slug: "broken", Title: "broken"},
		},
		pages: map[string]string{
			"alpha": "---\ntitle: Alpha Concept\nsources:\n  - Source One\n  - \"Source Two\"\ntags: [one, two]\n---\nAlpha body text.",
		},
		errs: map[string]error{"broken": errors.New("read failed")},
	}

	conceptCache := New()
	entries, err := conceptCache.Build(context.Background(), reader)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("len(entries) = %d, want %d", got, want)
	}
	entry := entries[0]
	if entry.Slug != "alpha" || entry.Title != "Alpha Concept" {
		t.Fatalf("entry identity = %#v", entry)
	}
	if entry.Body != "\nAlpha body text." {
		t.Fatalf("Body = %q", entry.Body)
	}
	if got, want := len(entry.Sources), 2; got != want {
		t.Fatalf("len(Sources) = %d, want %d", got, want)
	}
	if entry.Sources[0] != "Source One" || entry.Sources[1] != "Source Two" {
		t.Fatalf("Sources = %#v", entry.Sources)
	}
	tags, ok := entry.Frontmatter["tags"].([]string)
	if !ok || len(tags) != 2 || tags[0] != "one" || tags[1] != "two" {
		t.Fatalf("frontmatter tags = %#v, want [one two]", entry.Frontmatter["tags"])
	}
}

func TestBuildFailsWhenNoConceptCanBeRead(t *testing.T) {
	reader := &fakeReader{
		prefix:   "users/u/projects/empty",
		concepts: []gcs.WikiPage{{Slug: "broken"}},
		errs:     map[string]error{"broken": errors.New("read failed")},
	}

	_, err := New().Build(context.Background(), reader)
	if err == nil {
		t.Fatal("Build() error = nil, want failure")
	}
}

func TestSearchMatchesCachedContentAndReturnsUniqueLimitedResults(t *testing.T) {
	reader := &fakeReader{
		prefix: "users/u/projects/search",
		concepts: []gcs.WikiPage{
			{Slug: "alpha"},
			{Slug: "beta"},
			{Slug: "gamma"},
		},
		pages: map[string]string{
			"alpha": "---\ntitle: Alpha\n---\nshared topic alpha",
			"beta":  "---\ntitle: Beta shared\n---\nbody",
			"gamma": "---\ntitle: Gamma\ncategory: shared\n---\nbody",
		},
	}
	conceptCache := New()

	results, err := conceptCache.Search(context.Background(), reader, "shared", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got, want := len(results), 2; got != want {
		t.Fatalf("len(results) = %d, want %d", got, want)
	}
	if results[0].Slug == results[1].Slug {
		t.Fatalf("Search() returned duplicate slug %q", results[0].Slug)
	}
	for _, result := range results {
		if result.Type != "concept" {
			t.Fatalf("result.Type = %q, want concept", result.Type)
		}
		if result.Snippet == "" {
			t.Fatalf("result.Snippet is empty: %#v", result)
		}
	}
}

func TestSearchBuildsEachProjectOnceAndKeepsProjectsIsolated(t *testing.T) {
	projectA := &fakeReader{
		prefix:   "users/u/projects/a",
		concepts: []gcs.WikiPage{{Slug: "alpha"}},
		pages:    map[string]string{"alpha": "---\ntitle: Alpha\n---\nprojectword"},
	}
	projectB := &fakeReader{
		prefix:   "users/u/projects/b",
		concepts: []gcs.WikiPage{{Slug: "beta"}},
		pages:    map[string]string{"beta": "---\ntitle: Beta\n---\nprojectword"},
	}
	conceptCache := New()

	for i := 0; i < 2; i++ {
		results, err := conceptCache.Search(context.Background(), projectA, "projectword", 10)
		if err != nil {
			t.Fatalf("project A Search() error = %v", err)
		}
		if len(results) != 1 || results[0].Slug != "alpha" {
			t.Fatalf("project A results = %#v", results)
		}
	}
	results, err := conceptCache.Search(context.Background(), projectB, "projectword", 10)
	if err != nil {
		t.Fatalf("project B Search() error = %v", err)
	}
	if len(results) != 1 || results[0].Slug != "beta" {
		t.Fatalf("project B results = %#v", results)
	}
	if projectA.lists != 1 || projectB.lists != 1 {
		t.Fatalf("ListConcepts calls: project A = %d, project B = %d; want 1 each", projectA.lists, projectB.lists)
	}
}

func TestEntryReturnsCachedConceptForRequestedProject(t *testing.T) {
	reader := &fakeReader{
		prefix:   "users/u/projects/p",
		concepts: []gcs.WikiPage{{Slug: "alpha"}},
		pages:    map[string]string{"alpha": "---\ntitle: Alpha\nsources: [Source One, Source Two]\n---\nbody"},
	}
	conceptCache := New()
	if _, err := conceptCache.Build(context.Background(), reader); err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	entry, ok := conceptCache.Entry(reader, "alpha")
	if !ok {
		t.Fatal("Entry() did not find alpha")
	}
	if len(entry.Sources) != 2 {
		t.Fatalf("Entry().Sources = %#v", entry.Sources)
	}
}
