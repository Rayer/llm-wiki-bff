package wikiindex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeStore struct {
	files     map[string][]MarkdownFile
	reads     map[string][]byte
	writes    map[string][]byte
	listCalls map[string]int
}

func (s *fakeStore) ListMarkdownFiles(_ context.Context, dir string) ([]MarkdownFile, error) {
	if s.listCalls != nil {
		s.listCalls[dir]++
	}
	return append([]MarkdownFile(nil), s.files[dir]...), nil
}

func TestRebuildCollectsSourcesOnce(t *testing.T) {
	store := &fakeStore{files: map[string][]MarkdownFile{"wiki/": {}, "wiki/sources/": {{Slug: "s", Path: "wiki/sources/s.md", Data: []byte("---\nid: id\nsource_file: raw/s.md\n---")}}}, reads: map[string][]byte{}, listCalls: map[string]int{}}
	if _, err := Rebuild(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.listCalls["wiki/sources/"]; got != 1 {
		t.Fatalf("source traversals = %d, want 1", got)
	}
}

func (s *fakeStore) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	data, ok := s.reads[relPath]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeStore) WriteBytesAtomic(_ context.Context, data []byte, _, finalPath string) (string, error) {
	if s.writes == nil {
		s.writes = map[string][]byte{}
	}
	s.writes[finalPath] = append([]byte(nil), data...)
	return "digest", nil
}

func TestRebuildWritesIDMapAndConceptsJSONL(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{
					Slug: "alpha",
					Path: "wiki/alpha.md",
					Data: []byte("---\nid: concept-id\ntitle: Alpha\nsources:\n  - src-one\n---\nAlpha body"),
				},
			},
			"wiki/sources/": {
				{
					Slug: "src-one",
					Path: "wiki/sources/src-one.md",
					Data: []byte("---\nid: source-id\ntitle: Source One\n---\nSource body"),
				},
			},
		},
		reads: map[string][]byte{},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got := next.Concept["concept-id"]; got != "alpha" {
		t.Fatalf("concept id maps to %q, want alpha", got)
	}
	if got := next.Source["source-id"]; got != "src-one" {
		t.Fatalf("source id maps to %q, want src-one", got)
	}
	if _, ok := store.writes[IDMapPath]; !ok {
		t.Fatalf("missing write to %s", IDMapPath)
	}

	jsonl := strings.TrimSpace(string(store.writes[ConceptsJSONLPath]))
	var entry struct {
		Slug        string                 `json:"slug"`
		Title       string                 `json:"title"`
		Body        string                 `json:"body"`
		Frontmatter map[string]interface{} `json:"frontmatter"`
		Sources     []string               `json:"sources"`
	}
	if err := json.Unmarshal([]byte(jsonl), &entry); err != nil {
		t.Fatalf("concepts jsonl entry is not valid JSON: %v\n%s", err, jsonl)
	}
	if entry.Slug != "alpha" || entry.Title != "Alpha" || strings.TrimSpace(entry.Body) != "Alpha body" {
		t.Fatalf("entry = %+v, want alpha full cache entry", entry)
	}
	if got, ok := entry.Frontmatter["id"].(string); !ok || got != "concept-id" {
		t.Fatalf("frontmatter id = %#v, want concept-id", entry.Frontmatter["id"])
	}
	if len(entry.Sources) != 1 || entry.Sources[0] != "src-one" {
		t.Fatalf("sources = %#v, want [src-one]", entry.Sources)
	}
}

func TestRebuildPreservesRedirects(t *testing.T) {
	old := IDMap{
		Concept:   map[string]string{"same-id": "old-alpha"},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{Slug: "new-alpha", Path: "wiki/new-alpha.md", Data: []byte("---\nid: same-id\ntitle: Alpha\n---\nBody")},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got := next.Redirects["same-id"]; len(got) != 1 || got[0] != "old-alpha" {
		t.Fatalf("redirects = %#v, want old-alpha", next.Redirects)
	}
}

func TestRebuildPreservesDormantConceptsAndOwnedEntityMappings(t *testing.T) {
	old := IDMap{
		Concept:         map[string]string{"stable-alpha": "alpha"},
		DormantConcept:  map[string]string{"stable-beta": "beta"},
		ConceptEntityID: map[string]string{"stable-alpha": "entity-alpha", "stable-beta": "entity-beta", "orphan": "entity-orphan"},
		Source:          map[string]string{},
		Redirects:       map[string][]string{},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("---\nid: stable-alpha\n---\nAlpha")}},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if next.DormantConcept["stable-beta"] != "beta" {
		t.Fatalf("dormant concept = %#v, want stable-beta -> beta", next.DormantConcept)
	}
	if next.ConceptEntityID["stable-alpha"] != "entity-alpha" || next.ConceptEntityID["stable-beta"] != "entity-beta" {
		t.Fatalf("owned entity mappings = %#v", next.ConceptEntityID)
	}
	if _, ok := next.ConceptEntityID["orphan"]; ok {
		t.Fatalf("orphan entity mapping was retained: %#v", next.ConceptEntityID)
	}
}

func TestRebuildFailsClosedOnLifecycleMappingCollisions(t *testing.T) {
	tests := []struct {
		name string
		old  IDMap
	}{
		{
			name: "active dormant slug",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-beta": "alpha"},
				ConceptEntityID: map[string]string{"stable-beta": "entity-beta"},
			},
		},
		{
			name: "active dormant id",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-alpha": "beta"},
				ConceptEntityID: map[string]string{"stable-alpha": "entity-alpha"},
			},
		},
		{
			name: "dormant slug collision",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-a": "beta", "stable-b": "beta"},
				ConceptEntityID: map[string]string{"stable-a": "entity-a", "stable-b": "entity-b"},
			},
		},
		{
			name: "retained entity collision",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-a": "alpha", "stable-b": "beta"},
				ConceptEntityID: map[string]string{"stable-a": "same", "stable-b": "same"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldJSON, err := json.Marshal(tc.old)
			if err != nil {
				t.Fatal(err)
			}
			store := &fakeStore{
				files: map[string][]MarkdownFile{
					"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("---\nid: stable-alpha\n---\nAlpha")}},
					"wiki/sources/": {},
				},
				reads: map[string][]byte{IDMapPath: oldJSON},
			}
			if _, err := Rebuild(context.Background(), store); err == nil {
				t.Fatal("Rebuild() error = nil, want fail-closed lifecycle mapping rejection")
			}
			if _, ok := store.writes[IDMapPath]; ok {
				t.Fatal("Rebuild() wrote id_map after lifecycle validation failure")
			}
		})
	}
}

func TestRebuildFailsClosedOnMalformedLifecycleMapping(t *testing.T) {
	old := IDMap{
		DormantConcept:  map[string]string{"../stable": "beta"},
		ConceptEntityID: map[string]string{"../stable": "entity-beta"},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{"wiki/": {}, "wiki/sources/": {}},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}
	if _, err := Rebuild(context.Background(), store); err == nil {
		t.Fatal("Rebuild() error = nil, want malformed lifecycle mapping rejection")
	}
	if _, ok := store.writes[IDMapPath]; ok {
		t.Fatal("Rebuild() wrote id_map after malformed lifecycle validation failure")
	}
}
