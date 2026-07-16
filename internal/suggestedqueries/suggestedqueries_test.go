package suggestedqueries

import (
	"encoding/json"
	"testing"
	"time"

	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
)

func TestBuildPicksMostRecentlyUpdatedConceptTitles(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	entries := []conceptcache.Entry{
		{Slug: "old", Title: "Old Concept", Frontmatter: map[string]interface{}{"updated": "2026-07-01T00:00:00Z"}},
		{Slug: "newest", Title: "Newest Concept", Frontmatter: map[string]interface{}{"updated": "2026-07-10T00:00:00Z"}},
		{Slug: "middle", Title: "Middle Concept", Frontmatter: map[string]interface{}{"updated": "2026-07-05T00:00:00Z"}},
	}

	artifact := Build(entries, nil, now)

	if len(artifact.Queries) != 3 {
		t.Fatalf("queries = %#v, want 3 entries", artifact.Queries)
	}
	if artifact.Queries[0] != "Newest Concept" {
		t.Fatalf("queries[0] = %q, want Newest Concept", artifact.Queries[0])
	}
	if artifact.Queries[1] != "Middle Concept" {
		t.Fatalf("queries[1] = %q, want Middle Concept", artifact.Queries[1])
	}
	if artifact.Queries[2] != "Old Concept" {
		t.Fatalf("queries[2] = %q, want Old Concept", artifact.Queries[2])
	}
	if artifact.UpdatedAt != now.UTC().Format(time.RFC3339) {
		t.Fatalf("updated_at = %q, want RFC3339 timestamp", artifact.UpdatedAt)
	}
}

func TestBuildLimitsToFiveQueries(t *testing.T) {
	now := time.Now().UTC()
	entries := make([]conceptcache.Entry, 0, 7)
	for i := 0; i < 7; i++ {
		entries = append(entries, conceptcache.Entry{
			Slug:  "concept-" + string(rune('a'+i)),
			Title: "Concept " + string(rune('A'+i)),
			Frontmatter: map[string]interface{}{
				"updated": time.Date(2026, 7, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339),
			},
		})
	}

	artifact := Build(entries, nil, now)
	if len(artifact.Queries) != MaxQueries {
		t.Fatalf("len(queries) = %d, want %d", len(artifact.Queries), MaxQueries)
	}
}

func TestBuildFallsBackToFileMtimeWhenUpdatedMissing(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	entries := []conceptcache.Entry{
		{Slug: "alpha", Title: "Alpha"},
		{Slug: "beta", Title: "Beta"},
	}
	mtimes := map[string]time.Time{
		"alpha": time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC),
		"beta":  time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
	}

	artifact := Build(entries, mtimes, now)
	if len(artifact.Queries) != 2 {
		t.Fatalf("queries = %#v, want 2 entries", artifact.Queries)
	}
	if artifact.Queries[0] != "Beta" {
		t.Fatalf("queries[0] = %q, want Beta", artifact.Queries[0])
	}
	if artifact.Queries[1] != "Alpha" {
		t.Fatalf("queries[1] = %q, want Alpha", artifact.Queries[1])
	}
}

func TestBuildSkipsEmptyTitles(t *testing.T) {
	now := time.Now().UTC()
	entries := []conceptcache.Entry{
		{Slug: "blank", Title: "   "},
		{Slug: "valid", Title: "Valid Concept"},
	}

	artifact := Build(entries, nil, now)
	if len(artifact.Queries) != 1 || artifact.Queries[0] != "Valid Concept" {
		t.Fatalf("queries = %#v, want only Valid Concept", artifact.Queries)
	}
}

func TestBuildFromConceptsJSONL(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	data := []byte(`{"slug":"beta","title":"Beta","frontmatter":{"updated":"2026-07-10T00:00:00Z"}}
{"slug":"alpha","title":"Alpha","frontmatter":{"updated":"2026-07-01T00:00:00Z"}}
`)

	artifact, err := BuildFromConceptsJSONL(data, nil, now)
	if err != nil {
		t.Fatalf("BuildFromConceptsJSONL() error = %v", err)
	}
	if len(artifact.Queries) != 2 || artifact.Queries[0] != "Beta" {
		t.Fatalf("queries = %#v, want Beta first", artifact.Queries)
	}
}

func TestDecodeReturnsQueries(t *testing.T) {
	data, err := json.Marshal(Artifact{
		Queries:   []string{"One", "Two"},
		UpdatedAt: "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(artifact.Queries) != 2 || artifact.Queries[0] != "One" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestQueriesReturnsEmptySliceForNil(t *testing.T) {
	got := Queries(Artifact{})
	if got == nil || len(got) != 0 {
		t.Fatalf("Queries() = %#v, want empty non-nil slice", got)
	}
}