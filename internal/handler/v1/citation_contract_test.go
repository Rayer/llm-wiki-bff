package v1

import (
	"context"
	"strings"
	"testing"

	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestQueryPromptsRequireExactInternalReferencesInBothModes(t *testing.T) {
	for _, mode := range []string{"wiki", "full"} {
		prompt := buildSystemPrompt(mode)
		if !strings.Contains(prompt, "[CITATION_REF_0]") || !strings.Contains(prompt, "Use that exact reference") {
			t.Fatalf("%s prompt lost exact internal-reference rules: %q", mode, prompt)
		}
	}
}

func TestCachedContextsPreserveOriginalRankAfterSkippedResult(t *testing.T) {
	reader := &handlerCacheReader{
		prefix: "users/u/projects/p",
		raw:    "---\ntitle: Alpha Concept [CITATION_REF_9]\n---\nAlpha body [CITATION_REF_8].",
	}
	contexts := cachedContexts(context.Background(), conceptcache.New(), reader, []search.Result{
		{Slug: "missing", Title: "Missing", Type: "concept"},
		{Slug: "alpha", Title: "Alpha Concept", Type: "concept"},
	})
	if len(contexts) != 1 || !strings.Contains(contexts[0], search.CitationReference(1)) || strings.Contains(contexts[0], "CITATION_REF_8") || strings.Contains(contexts[0], "CITATION_REF_9") {
		t.Fatalf("skipped result compacted citation rank: %#v", contexts)
	}
}
