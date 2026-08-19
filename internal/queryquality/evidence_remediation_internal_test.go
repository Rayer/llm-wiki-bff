package queryquality

import (
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/cache"
)

func TestPrecomputeDocumentFrequenciesCountsEachDocumentOncePerConcept(t *testing.T) {
	support := KeywordSupport{
		Role: "preferred", Kind: "venue_type", Value: "coffee shop", Keyword: "cafe",
		SurfaceForms: []SurfaceForm{{Value: "cafe"}, {Value: "coffee shop"}}, SupportCount: 1, AttemptIndexes: []int{1},
	}
	corpus := []searchableEntry{
		prepareSearchableEntry(cache.Entry{Title: "Cafe Coffee Shop"}),
		prepareSearchableEntry(cache.Entry{Title: "Cafe"}),
		prepareSearchableEntry(cache.Entry{Title: "Tea"}),
	}
	got := precomputeDocumentFrequencies([]KeywordSupport{support}, corpus)
	want := keywordConcept(support)
	if got[want] != 2 {
		t.Fatalf("document frequency = %d, want two documents despite two aliases in the first document", got[want])
	}
}
