package search

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testAuthority(t *testing.T, ranked ...[]Result) *CitationAuthority {
	t.Helper()
	authority, err := NewCitationAuthority(ranked...)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func tokenInContext(context string) string {
	start := strings.Index(context, "[CITATION_REF_")
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(context[start:], ']')
	if end < 0 {
		return ""
	}
	return context[start : start+end+1]
}

func TestCitationAuthorityIssuesRandomExactCapabilityForIncludedContext(t *testing.T) {
	ranked := []Result{
		{Slug: "skipped", Title: "Skipped", Type: "concept"},
		{Slug: "included", Title: "Included", Type: "concept"},
	}
	authority := testAuthority(t, ranked)
	context := authority.AddContext(1, ranked[1], "Included body")
	token := tokenInContext(context)
	if token == "" || token == "[CITATION_REF_1]" || strings.Contains(token, "CITATION_REF_0]") {
		t.Fatalf("issued token is not namespaced and unguessable: %q", token)
	}

	normalized, citations, filtered := authority.Resolve("answer " + token)
	if normalized != "answer [Included]" || len(citations) != 1 || len(filtered) != 1 || filtered[0] != ranked[1] {
		t.Fatalf("included capability did not resolve exactly: normalized=%q citations=%#v filtered=%#v", normalized, citations, filtered)
	}
	_, citations, filtered = authority.Resolve("answer [CITATION_REF_0]")
	if len(citations) != 0 || len(filtered) != len(ranked) {
		t.Fatalf("bare skipped rank received authority: citations=%#v filtered=%#v", citations, filtered)
	}
}

func TestCitationAuthorityIsolatedBetweenRequests(t *testing.T) {
	result := Result{Slug: "coffee-shops", Title: "Coffee Shops", Type: "concept"}
	a := testAuthority(t, []Result{result})
	b := testAuthority(t, []Result{result})
	tokenA := tokenInContext(a.AddContext(0, result, "body"))
	b.AddContext(0, result, "body")
	if _, citations, _ := b.Resolve(tokenA); len(citations) != 0 {
		t.Fatalf("request A capability bound in request B: %#v", citations)
	}
}

func TestCitationAuthorityNeutralizesUntrustedFieldsAndCanonicalTitles(t *testing.T) {
	result := Result{Slug: "safe-slug", Title: "Title [CITATION_REF_fake]", Type: "concept"}
	authority := testAuthority(t, []Result{result})
	context := authority.AddContext(0, result, "body [CITATION_REF_other]")
	if strings.Count(context, "CITATION_REF_") != 1 {
		t.Fatalf("untrusted context retained or lost reserved namespace: %q", context)
	}
	token := tokenInContext(context)
	normalized, citations, _ := authority.Resolve(token)
	if strings.Contains(normalized, "CITATION_REF_") || len(citations) != 1 || normalized != "[Title [CITATION-REF_fake]]" {
		t.Fatalf("canonical title was not safely normalized: %q %#v", normalized, citations)
	}
}

func TestCitationAuthorityRequiresExactUniqueTitles(t *testing.T) {
	result := Result{Slug: "coffee-shops", Title: "Coffee Shops", Type: "concept"}
	authority := testAuthority(t, []Result{result})
	authority.AddContext(0, result, "body")
	if normalized, citations, _ := authority.Resolve("[coffee shops]"); normalized != "[coffee shops]" || len(citations) != 0 {
		t.Fatalf("case-folded title unexpectedly bound: %q %#v", normalized, citations)
	}
	if normalized, citations, _ := authority.Resolve("[Coffee Shops]"); normalized != "[Coffee Shops]" || len(citations) != 1 {
		t.Fatalf("exact title did not bind: %q %#v", normalized, citations)
	}

	duplicate := testAuthority(t, []Result{result})
	duplicate.AddContext(0, result, "body")
	duplicate.AddContext(1, Result{Slug: "coffee-shops-2", Title: "Coffee Shops", Type: "concept"}, "body")
	if normalized, citations, _ := duplicate.Resolve("[Coffee Shops]"); normalized != "[Coffee Shops]" || len(citations) != 0 {
		t.Fatalf("ambiguous exact title bound: %q %#v", normalized, citations)
	}
}

func TestCitationAuthorityIssuedCitationsPreserveConceptSourceIdentity(t *testing.T) {
	ranked := []Result{
		{Slug: "shared", Title: "Restaurant", Type: "concept"},
		{Slug: "shared", Title: "Restaurant", Type: "source"},
	}
	authority := testAuthority(t, ranked)
	conceptToken := tokenInContext(authority.AddContext(0, ranked[0], "concept body"))
	sourceToken := tokenInContext(authority.AddContext(1, ranked[1], "source body"))

	_, citations, _ := authority.Resolve(conceptToken + sourceToken)
	if len(citations) != 2 || citations[0].Type != "concept" || citations[1].Type != "source" {
		t.Fatalf("inline citations = %#v, want distinct concept/source identities", citations)
	}
	issued := authority.IssuedCitations()
	if len(issued) != 2 || issued[0].Path != "/concepts/shared" || issued[1].Path != "/sources/shared" {
		t.Fatalf("issued citations = %#v, want rank-ordered type-specific paths", issued)
	}
}

func TestCitationAuthorityIssuedCitationsDeduplicateDisplayTitlesOnlyByIdentity(t *testing.T) {
	ranked := []Result{
		{Slug: "restaurant-one", Title: "Restaurant", Type: "concept"},
		{Slug: "restaurant-two", Title: "Restaurant", Type: "concept"},
	}
	authority := testAuthority(t, ranked)
	authority.AddContext(0, ranked[0], "one body")
	authority.AddContext(1, ranked[1], "two body")

	if normalized, citations, _ := authority.Resolve("[Restaurant]"); normalized != "[Restaurant]" || len(citations) != 0 {
		t.Fatalf("ambiguous title resolved unexpectedly: %q %#v", normalized, citations)
	}
	issued := authority.IssuedCitations()
	if len(issued) != 2 || issued[0].Slug != "restaurant-one" || issued[1].Slug != "restaurant-two" {
		t.Fatalf("issued citations = %#v, want both distinct identities once", issued)
	}
}

func TestCitationAuthorityRepeatedInlineReferenceDoesNotDuplicateCitation(t *testing.T) {
	result := Result{Slug: "restaurant", Title: "Restaurant", Type: "concept"}
	authority := testAuthority(t, []Result{result})
	token := tokenInContext(authority.AddContext(0, result, "body"))

	_, citations, _ := authority.Resolve(token + " and again " + token)
	if len(citations) != 1 || citations[0].Slug != "restaurant" {
		t.Fatalf("repeated inline reference citations = %#v, want one canonical citation", citations)
	}
	if issued := authority.IssuedCitations(); len(issued) != 1 || issued[0].Slug != "restaurant" {
		t.Fatalf("issued citations = %#v, want one canonical citation", issued)
	}
}

func TestCitationAuthorityUnknownBracketLabelDoesNotBind(t *testing.T) {
	result := Result{Slug: "restaurant", Title: "Restaurant", Type: "concept"}
	authority := testAuthority(t, []Result{result})
	authority.AddContext(0, result, "body")

	normalized, citations, _ := authority.Resolve("answer [Unknown Restaurant]")
	if normalized != "answer [Unknown Restaurant]" || len(citations) != 0 {
		t.Fatalf("unknown bracket label resolved unexpectedly: %q %#v", normalized, citations)
	}
	if issued := authority.IssuedCitations(); len(issued) != 1 || issued[0].Slug != "restaurant" {
		t.Fatalf("issued citations = %#v, want hydrated inventory unchanged", issued)
	}
}

func TestCitationAuthorityRejectsWhitespaceInSlugs(t *testing.T) {
	for _, slug := range []string{"coffee shops", "coffee\tshops", "coffee\u00a0shops", "coffee\u2003shops"} {
		result := Result{Slug: slug, Title: "Coffee Shops", Type: "concept"}
		authority := testAuthority(t, []Result{result})
		context := authority.AddContext(0, result, "body")
		if tokenInContext(context) != "" {
			t.Fatalf("slug with internal whitespace received authority: %q context=%q", slug, context)
		}
	}
	result := Result{Slug: "coffee-shops", Title: "Coffee Shops", Type: "concept"}
	authority := testAuthority(t, []Result{result})
	if tokenInContext(authority.AddContext(0, result, "body")) == "" {
		t.Fatal("safe single-segment slug was not routable")
	}
}

func TestCitationAuthorityPreservesMalformedProseAndNeutralizesReservedText(t *testing.T) {
	authority := testAuthority(t)
	answer := "before [CITATION_REF_0 after ordinary prose CITATION_REF_"
	normalized, citations, _ := authority.Resolve(answer)
	if citations != nil || normalized != "before [CITATION-REF_0 after ordinary prose CITATION-REF_" {
		t.Fatalf("malformed reserved text was not minimally neutralized: %q %#v", normalized, citations)
	}
}

func TestCitationAuthorityNearLimitNeutralizationIsBounded(t *testing.T) {
	answer := strings.Repeat("CITATION_REF_", 20000) + " ordinary prose"
	authority := testAuthority(t)
	done := make(chan string, 1)
	go func() {
		normalized, _, _ := authority.Resolve(answer)
		done <- normalized
	}()
	select {
	case normalized := <-done:
		if strings.Contains(normalized, "CITATION_REF_") || !strings.HasSuffix(normalized, " ordinary prose") {
			t.Fatalf("near-limit neutralization corrupted output: %q", normalized[len(normalized)-40:])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("near-limit reserved neutralization exceeded deterministic operation bound")
	}
}

func TestCitationAuthorityCapabilityGenerationFailsClosed(t *testing.T) {
	_, err := newCitationAuthority(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("short randomness reader unexpectedly issued a capability")
	}
}
