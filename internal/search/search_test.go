package search

import (
	"strings"
	"testing"
)

func TestParseMetaIndexSectionedWikiLinks(t *testing.T) {
	raw := `---
title: Wiki Index
---

# Wiki Index

## Concepts
- [[Retrieval-Augmented Generation]]

## Sources
- [[SQLite Durable Workflows]] ` + "\u2014" + ` high
`

	sources, concepts, entries := parseMetaIndex(raw)

	if got, want := len(sources), 1; got != want {
		t.Fatalf("sources len = %d, want %d", got, want)
	}
	if got, want := len(concepts), 1; got != want {
		t.Fatalf("concepts len = %d, want %d", got, want)
	}
	if got, want := sources[0].Slug, "SQLite Durable Workflows"; got != want {
		t.Fatalf("source slug = %q, want %q", got, want)
	}
	if got, want := concepts[0].Slug, "Retrieval-Augmented Generation"; got != want {
		t.Fatalf("concept slug = %q, want %q", got, want)
	}
	if got, want := entries[indexKey("source", sources[0].Slug)].description, "high"; got != want {
		t.Fatalf("source description = %q, want %q", got, want)
	}
}

func TestParseMetaIndexTypedPlainLines(t *testing.T) {
	raw := `source: source-slug | Source Title | Source description
concept: concept-slug | Concept Title | Concept description
`

	sources, concepts, entries := parseMetaIndex(raw)

	if got, want := len(sources), 1; got != want {
		t.Fatalf("sources len = %d, want %d", got, want)
	}
	if got, want := len(concepts), 1; got != want {
		t.Fatalf("concepts len = %d, want %d", got, want)
	}
	sourceEntry := entries[indexKey("source", "source-slug")]
	if got, want := sourceEntry.title, "Source Title"; got != want {
		t.Fatalf("source title = %q, want %q", got, want)
	}
	if got, want := sourceEntry.description, "Source description"; got != want {
		t.Fatalf("source description = %q, want %q", got, want)
	}
	conceptEntry := entries[indexKey("concept", "concept-slug")]
	if got, want := conceptEntry.title, "Concept Title"; got != want {
		t.Fatalf("concept title = %q, want %q", got, want)
	}
	if got, want := conceptEntry.description, "Concept description"; got != want {
		t.Fatalf("concept description = %q, want %q", got, want)
	}
}

func TestSearchUsesMetadataOnly(t *testing.T) {
	idx := NewIndex()
	idx.sources, idx.concepts, idx.entries = parseMetaIndex(`source: source-slug | Source Title | Source description
concept: concept-slug | Concept Title | Concept description
`)

	results := idx.Search("description", 10)
	if got, want := len(results), 2; got != want {
		t.Fatalf("results len = %d, want %d", got, want)
	}
	if got, want := results[0].Snippet, "Source description"; got != want {
		t.Fatalf("first snippet = %q, want %q", got, want)
	}
}

func TestSearchInterleavesSourcesAndConcepts(t *testing.T) {
	var raw strings.Builder
	for i := 0; i < 12; i++ {
		raw.WriteString("source: source-")
		raw.WriteString(string(rune('a' + i)))
		raw.WriteString(" | Source | shared description\n")
	}
	for i := 0; i < 3; i++ {
		raw.WriteString("concept: concept-")
		raw.WriteString(string(rune('a' + i)))
		raw.WriteString(" | Concept | shared description\n")
	}

	idx := NewIndex()
	idx.sources, idx.concepts, idx.entries = parseMetaIndex(raw.String())

	results := idx.Search("shared", 10)
	if got, want := len(results), 10; got != want {
		t.Fatalf("results len = %d, want %d", got, want)
	}
	if got, want := results[0].Type, "source"; got != want {
		t.Fatalf("first result type = %q, want %q", got, want)
	}
	if got, want := results[1].Type, "concept"; got != want {
		t.Fatalf("second result type = %q, want %q", got, want)
	}

	concepts := 0
	for _, result := range results {
		if result.Type == "concept" {
			concepts++
		}
	}
	if got, want := concepts, 3; got != want {
		t.Fatalf("concept result count = %d, want %d", got, want)
	}
}
