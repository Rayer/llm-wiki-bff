package search

import "testing"

func TestParseCitationsResolvesDeterministicReferenceAndNormalizesTitle(t *testing.T) {
	results := []Result{{
		Slug:  "parks/yu-yi-zhi-qiu",
		Title: "中和員山公園/遊逸之丘",
		Type:  "source",
	}}

	citations, filtered := ParseCitations("適合親子放電。[CITATION_REF_0]", results)
	if len(citations) != 1 {
		t.Fatalf("expected one citation, got %#v", citations)
	}
	if citations[0].Text != "中和員山公園/遊逸之丘" {
		t.Fatalf("expected canonical citation text, got %#v", citations[0])
	}
	if len(filtered) != 1 || filtered[0].Slug != results[0].Slug {
		t.Fatalf("expected the cited ranked result, got %#v", filtered)
	}
}

func TestResolveCitationsPreservesExactCompatibilityAndConceptIdentity(t *testing.T) {
	results := []Result{{Slug: "coffee/shops", Title: "Coffee Shops", Type: "concept"}}

	normalized, citations, filtered := ResolveCitations("See [Coffee Shops] and [CITATION_REF_0].", results)
	if normalized != "See [Coffee Shops] and [Coffee Shops]." {
		t.Fatalf("unexpected normalized answer: %q", normalized)
	}
	if len(citations) != 2 || citations[0].Type != "concept" || citations[0].Slug != results[0].Slug {
		t.Fatalf("unexpected concept citations: %#v", citations)
	}
	if len(filtered) != 1 || filtered[0] != results[0] {
		t.Fatalf("unexpected filtered results: %#v", filtered)
	}
}

func TestResolveCitationsFailsClosedAndPreservesResults(t *testing.T) {
	results := []Result{
		{Slug: "source-a", Title: "Shared Title", Type: "source"},
		{Slug: "concept-a", Title: "Shared Title", Type: "concept"},
	}

	normalized, citations, filtered := ResolveCitations("Unknown [not-ranked] [CITATION_REF_9] [Shared Title]", results)
	if normalized != "Unknown [not-ranked] [CITATION_REF_9] [Shared Title]" {
		t.Fatalf("unknown or ambiguous text was changed: %q", normalized)
	}
	if len(citations) != 0 {
		t.Fatalf("expected no citations, got %#v", citations)
	}
	if len(filtered) != len(results) {
		t.Fatalf("expected original ranked results, got %#v", filtered)
	}
}

func TestResolveCitationsEscapesCanonicalRoute(t *testing.T) {
	results := []Result{{Slug: "parks/a/b !", Title: "A Park", Type: "source"}}

	_, citations, _ := ResolveCitations("[CITATION_REF_0]", results)
	if len(citations) != 1 || citations[0].Path != "/sources/parks%2Fa%2Fb%20%21" {
		t.Fatalf("unexpected escaped citation path: %#v", citations)
	}
}
