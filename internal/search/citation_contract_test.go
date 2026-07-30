package search

import (
	"strings"
	"testing"
)

func TestParseCitationsResolvesDeterministicReferenceAndNormalizesTitle(t *testing.T) {
	results := []Result{{
		Slug:  "yu-yi-zhi-qiu",
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
	results := []Result{{Slug: "coffee-shops", Title: "Coffee Shops", Type: "concept"}}

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
	if normalized != "Unknown [not-ranked]  [Shared Title]" {
		t.Fatalf("unknown or ambiguous text was changed: %q", normalized)
	}
	if len(citations) != 0 {
		t.Fatalf("expected no citations, got %#v", citations)
	}
	if len(filtered) != len(results) {
		t.Fatalf("expected original ranked results, got %#v", filtered)
	}
}

func TestResolveCitationsAcceptsOnlyCanonicalReferences(t *testing.T) {
	results := []Result{{Slug: "park", Title: "Park", Type: "concept"}}
	for _, token := range []string{
		"[CITATION_REF_00]", "[CITATION_REF_+0]", "[CITATION_REF_-0]",
		"[CITATION_REF_ 0]", "[CITATION_REF_0x0]", "[CITATION_REF_999999999999999999999999]",
	} {
		normalized, citations, _ := ResolveCitations(token, results)
		if normalized != "" || len(citations) != 0 {
			t.Fatalf("invalid reference was not stripped: %q -> %q, %#v", token, normalized, citations)
		}
	}
}

func TestResolveCitationsStripsMalformedReservedReferences(t *testing.T) {
	for _, answer := range []string{
		"before [CITATION_REF_1 after",
		"before [CITATION_REF_0] and [CITATION_REF_",
		"before [CITATION_REF_0 trailing] after",
		"before [CITATION_REF_0]suffix",
	} {
		normalized, _, _ := ResolveCitations(answer, nil)
		if strings.Contains(normalized, "CITATION_REF_") {
			t.Fatalf("reserved reference leaked from %q: %q", answer, normalized)
		}
	}
}

func TestResolveCitationsRejectsUnsafeResultIdentity(t *testing.T) {
	unsafe := []Result{
		{Slug: "parks/a", Title: "A", Type: "source"},
		{Slug: "parks%2Fa", Title: "B", Type: "source"},
		{Slug: "..", Title: "C", Type: "concept"},
		{Slug: "park", Title: "D", Type: "unknown"},
	}
	for i, result := range unsafe {
		normalized, citations, filtered := ResolveCitations(CitationReference(i), []Result{result})
		if normalized != "" || len(citations) != 0 || len(filtered) != 1 {
			t.Fatalf("unsafe result was cited: %#v -> %q, %#v, %#v", result, normalized, citations, filtered)
		}
	}
}

func TestCitationContextNeutralizesUntrustedReferences(t *testing.T) {
	context := BuildCitationContext(3, "Title [CITATION_REF_8]", "slug [CITATION_REF_7]", "body [CITATION_REF_6]")
	if strings.Contains(context, "[CITATION_REF_8]") || strings.Contains(context, "[CITATION_REF_7]") || strings.Contains(context, "[CITATION_REF_6]") {
		t.Fatalf("untrusted context can counterfeit a reference: %q", context)
	}
	if !strings.Contains(context, "[CITATION_REF_3]") {
		t.Fatalf("generated reference missing: %q", context)
	}
}
