package suggestedqueries

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
)

func TestDecodeReturnsQueries(t *testing.T) {
	data, err := json.Marshal(Artifact{
		Queries:    []string{"One", "Two"},
		Candidates: []Candidate{},
		UpdatedAt:  "2026-07-10T00:00:00Z",
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

func TestDecodeRejectsLogicalEntryOverflow(t *testing.T) {
	data := `{"queries":[` + strings.Repeat(`"q",`, 10000) + `"overflow"],"updated_at":"2026-07-10T00:00:00Z"}`
	if _, err := Decode([]byte(data)); err == nil || err.Error() != "generated cache logical entry limit exceeded" {
		t.Fatalf("Decode() error = %v, want fixed logical-entry error", err)
	}
}

func TestQueriesReturnsEmptySliceForNil(t *testing.T) {
	got := Queries(Artifact{})
	if got == nil || len(got) != 0 {
		t.Fatalf("Queries() = %#v, want empty non-nil slice", got)
	}
}

func TestValidateCandidatesRejectsUnsafeStructuredOutput(t *testing.T) {
	concepts := []ConceptEvidence{{ID: "cafe", Title: "咖啡廳"}, {ID: "park", Title: "公園"}}
	valid := func(question string) Candidate {
		return Candidate{
			Question:               question,
			Intent:                 "recommendation",
			CorpusAnchorConceptIDs: []string{"cafe"},
			Generation:             GenerationMetadata{Model: "fixture", PromptVersion: "v1"},
		}
	}
	base := []Candidate{valid("台北有哪些適合工作的咖啡廳？"), valid("雨天可以安排哪些室內活動？"), valid("哪些地方適合家庭一起探索？")}
	for _, tc := range []struct {
		name string
		make func() []Candidate
	}{
		{name: "exact title", make: func() []Candidate { out := append([]Candidate(nil), base...); out[0] = valid("咖啡廳"); return out }},
		{name: "duplicate", make: func() []Candidate { out := append([]Candidate(nil), base...); out[1] = out[0]; return out }},
		{name: "empty", make: func() []Candidate { out := append([]Candidate(nil), base...); out[0] = valid("   "); return out }},
		{name: "overflow", make: func() []Candidate {
			return append(append([]Candidate(nil), base...), valid("還有什麼值得探索的地方？"), valid("如何比較不同選擇？"), valid("哪些選項最方便？"))
		}},
		{name: "malformed metadata", make: func() []Candidate { out := append([]Candidate(nil), base...); out[0].Generation.Model = ""; return out }},
		{name: "unknown anchor", make: func() []Candidate {
			out := append([]Candidate(nil), base...)
			out[0].CorpusAnchorConceptIDs = []string{"unknown"}
			return out
		}},
		{name: "title wrapper", make: func() []Candidate {
			out := append([]Candidate(nil), base...)
			out[0] = valid("關於咖啡廳的資訊？")
			return out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCandidates(tc.make(), concepts); err == nil {
				t.Fatal("ValidateCandidates() error = nil, want rejection")
			}
		})
	}
}

func TestValidateCandidatesRejectsLongGenericTitleWrappers(t *testing.T) {
	concepts := []ConceptEvidence{{ID: "cafe", Title: "咖啡廳"}, {ID: "coffee-shops", Title: "Coffee Shops"}}
	valid := func(question, anchor string) Candidate {
		return Candidate{
			Question:               question,
			Intent:                 "discovery",
			CorpusAnchorConceptIDs: []string{anchor},
			Generation:             GenerationMetadata{Model: "fixture", PromptVersion: "v1"},
		}
	}
	for _, tc := range []struct {
		name     string
		question string
		anchor   string
	}{
		{name: "english describe", question: "Describe Coffee Shops?", anchor: "coffee-shops"},
		{name: "english summarize", question: "Can you summarize Coffee Shops?", anchor: "coffee-shops"},
		{name: "english summary", question: "A summary of Coffee Shops?", anchor: "coffee-shops"},
		{name: "english introduction", question: "An introduction to Coffee Shops?", anchor: "coffee-shops"},
		{name: "english elaborate", question: "Could you elaborate on Coffee Shops?", anchor: "coffee-shops"},
		{name: "english outline", question: "Please outline Coffee Shops?", anchor: "coffee-shops"},
		{name: "english show", question: "Show me Coffee Shops?", anchor: "coffee-shops"},
		{name: "english define", question: "Define Coffee Shops?", anchor: "coffee-shops"},
		{name: "english overview", question: "Could you please provide an overview of Coffee Shops?", anchor: "coffee-shops"},
		{name: "english explanation", question: "Can you explain Coffee Shops?", anchor: "coffee-shops"},
		{name: "chinese describe", question: "描述咖啡廳？", anchor: "cafe"},
		{name: "chinese overview", question: "請概述咖啡廳？", anchor: "cafe"},
		{name: "chinese summary", question: "咖啡廳的摘要？", anchor: "cafe"},
		{name: "chinese intro", question: "咖啡廳簡介？", anchor: "cafe"},
		{name: "chinese conclude", question: "請總結咖啡廳？", anchor: "cafe"},
		{name: "chinese information", question: "請告訴我關於咖啡廳的所有相關資訊？", anchor: "cafe"},
		{name: "chinese topic content", question: "我想知道咖啡廳這個主題有哪些內容？", anchor: "cafe"},
		{name: "chinese concept content", question: "可以介紹一下咖啡廳這個概念的內容嗎？", anchor: "cafe"},
		{name: "chinese complete information", question: "請提供咖啡廳的完整資訊？", anchor: "cafe"},
		{name: "chinese explain", question: "可以解釋咖啡廳嗎？", anchor: "cafe"},
		{name: "chinese explanation", question: "可以說明咖啡廳嗎？", anchor: "cafe"},
		{name: "english information", question: "Please tell me all information about Coffee Shops?", anchor: "coffee-shops"},
		{name: "english topic content", question: "What content is available about the Coffee Shops topic?", anchor: "coffee-shops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidates := []Candidate{valid(tc.question, tc.anchor), valid("哪些地方值得一起探索？", "cafe"), valid("如何比較不同選擇？", "coffee-shops")}
			if err := ValidateCandidates(candidates, concepts); err == nil {
				t.Fatal("ValidateCandidates() error = nil, want generic title wrapper rejection")
			}
		})
	}
}

func TestValidateCandidatesPreservesSubstantiveUseCasesAndComparisons(t *testing.T) {
	concepts := []ConceptEvidence{{ID: "cafe", Title: "咖啡廳"}, {ID: "coffee-shops", Title: "Coffee Shops"}, {ID: "park", Title: "公園"}}
	valid := func(question string, anchors ...string) Candidate {
		return Candidate{
			Question:               question,
			Intent:                 "discovery",
			CorpusAnchorConceptIDs: anchors,
			Generation:             GenerationMetadata{Model: "fixture", PromptVersion: "v1"},
		}
	}
	substantive := []struct {
		question string
		anchors  []string
	}{
		{question: "Which Coffee Shops are suitable for remote work?", anchors: []string{"coffee-shops"}},
		{question: "Which Coffee Shops are suitable for remote work and quiet seating?", anchors: []string{"coffee-shops"}},
		{question: "Compare Coffee Shops and Parks for rainy days with children?", anchors: []string{"coffee-shops", "park"}},
		{question: "有哪些咖啡廳符合預算並且有插座？", anchors: []string{"cafe"}},
		{question: "咖啡廳和公園哪個更適合雨天帶小孩？", anchors: []string{"cafe", "park"}},
	}

	for _, tc := range substantive {
		baseCandidates := []Candidate{
			valid("哪些咖啡廳最適合遠端工作的靜謐場所？", "cafe"),
			valid("想比較不同餐飲概念時我該注意什麼？", "coffee-shops"),
			valid("親子適合的室外空間有哪些？", "park"),
		}
		testCandidates := append(baseCandidates, valid(tc.question, tc.anchors...))
		if err := ValidateCandidates(testCandidates, concepts); err != nil {
			t.Fatalf("ValidateCandidates() rejected substantive question %q: %v", tc.question, err)
		}
	}
}

func TestTitleWrapperGuardPreservesLatinWordBoundaries(t *testing.T) {
	titles := map[string]string{"coffeeshops": "Coffee Shops"}
	if isTitleWrapper("Compare Coffee Shops and Parks for rainy days with children.", titles) {
		t.Fatal("isTitleWrapper() rejected a substantive comparison")
	}
	if got := stripGenericWrapperLanguage("theater thesis alligator"); got != "theater thesis alligator" {
		t.Fatalf("stripGenericWrapperLanguage() = %q, want substantive tokens preserved", got)
	}
}

func TestParseProviderCandidatesRejectsMalformedAndOversizedOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "malformed json", raw: `{"candidates":`},
		{name: "wrong shape", raw: `[{"question":"哪些概念值得一起比較？"}]`},
		{name: "trailing json", raw: `{"candidates":[]} {"extra":true}`},
		{name: "oversized", raw: strings.Repeat("x", MaxProviderBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProviderCandidates(tc.raw); err == nil {
				t.Fatal("parseProviderCandidates() error = nil, want rejection")
			}
		})
	}
}

func TestParseProviderCandidatesRejectsDuplicateAndUnknownKeys(t *testing.T) {
	for _, raw := range []string{
		`{"candidates":[],"candidates":[]}`,
		`{"candidates":[],"extra":true}`,
		`{"candidates":[{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["c1"],"generation":{}}]}`,
		`{"candidates":[{"question":"哪些概念值得一起比較？","question":"哪些地方值得探索？","intent/use_case":"comparison","corpus_anchor_concept_ids":["c1"]}]}`,
		`{"candidates":[{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["c1"],"extra":true}]}`,
	} {
		if _, err := parseProviderCandidates(raw); err == nil {
			t.Fatalf("parseProviderCandidates(%s) error = nil, want strict rejection", raw)
		}
	}
}

func TestParseProviderCandidatesRejectsCandidateOverflow(t *testing.T) {
	raw := `{"candidates":[{}, {}, {}, {}, {}, {}]}`
	if _, err := parseProviderCandidates(raw); err == nil {
		t.Fatal("parseProviderCandidates() error = nil, want candidate overflow rejection")
	}
}

func TestGenerateAttachesTrustedMetadataAfterProviderValidation(t *testing.T) {
	entries := []conceptcache.Entry{{Slug: "c1", Title: "Concept 1", Body: "Evidence"}}
	provider := &fixtureProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["c1"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["c1"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["c1"]}
]}`}
	trusted := GenerationMetadata{Model: "fixture-model", PromptVersion: "fixture-prompt-v1"}
	artifact, err := Generate(context.Background(), provider, "", entries, nil, trusted, time.Now())
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	for _, candidate := range artifact.Candidates {
		if candidate.Generation != trusted {
			t.Fatalf("candidate generation = %#v, want trusted %#v", candidate.Generation, trusted)
		}
	}
}

func TestGeneratePassesExplicitOptionalDescription(t *testing.T) {
	provider := &fixtureProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["c1"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["c1"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["c1"]}
]}`}
	if _, err := Generate(context.Background(), provider, "explicit description seam", []conceptcache.Entry{{Slug: "c1", Title: "Concept 1"}}, nil, GenerationMetadata{Model: "fixture", PromptVersion: "v1"}, time.Now()); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if !strings.Contains(provider.user, `"project_description":"explicit description seam"`) {
		t.Fatalf("provider user payload = %q, want explicit optional description", provider.user)
	}
}

func TestDecodeRejectsDuplicateAndUnknownArtifactKeys(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"version":2,"queries":[],"candidates":[],"updated_at":""}`,
		`{"version":2,"queries":[],"candidates":[],"updated_at":"","extra":true}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("Decode(%s) error = nil, want strict rejection", raw)
		}
	}
}

func TestDecodeRejectsDuplicateAndUnknownPublishedCandidateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"queries":[],"candidates":[{"question":"q?","question":"r?","intent/use_case":"i","corpus_anchor_concept_ids":["c1"],"generation":{"model":"m","prompt_version":"p"}}],"updated_at":""}`,
		`{"version":2,"queries":[],"candidates":[{"question":"q?","intent/use_case":"i","corpus_anchor_concept_ids":["c1"],"generation":{"model":"m","prompt_version":"p","extra":true}}],"updated_at":""}`,
		`{"version":2,"queries":[],"candidates":[{"question":"q?","intent/use_case":"i","corpus_anchor_concept_ids":["c1"],"generation":{"model":"m","model":"other","prompt_version":"p"}}],"updated_at":""}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("Decode(%s) error = nil, want strict rejection", raw)
		}
	}
}

func TestStrictDecodersRejectWrongShapes(t *testing.T) {
	for _, raw := range []string{
		`{"candidates":null}`,
		`{"candidates":[{"question":null,"intent/use_case":"i","corpus_anchor_concept_ids":["c1"]}]}`,
	} {
		if _, err := parseProviderCandidates(raw); err == nil {
			t.Fatalf("parseProviderCandidates(%s) error = nil, want wrong-shape rejection", raw)
		}
	}
	for _, raw := range []string{
		`{"version":null,"queries":[],"candidates":[],"updated_at":""}`,
		`{"version":2,"queries":[null],"candidates":[],"updated_at":""}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("Decode(%s) error = nil, want wrong-shape rejection", raw)
		}
	}
}

func TestGoldenLifestyleQuestionsSeparateQuestionHypothesesFromUnsupportedAssertions(t *testing.T) {
	concepts := []ConceptEvidence{{ID: "taipei-cafes", Title: "台北咖啡廳"}, {ID: "yilan-water", Title: "宜蘭戲水地點"}}
	valid := []Candidate{
		{Question: "台北有哪些適合工作的咖啡廳？", Intent: "recommendation", CorpusAnchorConceptIDs: []string{"taipei-cafes"}, Generation: GenerationMetadata{Model: "fixture", PromptVersion: "v1"}},
		{Question: "宜蘭有沒有適合戲水的地方？", Intent: "exploration", CorpusAnchorConceptIDs: []string{"yilan-water"}, Generation: GenerationMetadata{Model: "fixture", PromptVersion: "v1"}},
		{Question: "台北和宜蘭的選擇有什麼差異？", Intent: "comparison", CorpusAnchorConceptIDs: []string{"taipei-cafes", "yilan-water"}, Generation: GenerationMetadata{Model: "fixture", PromptVersion: "v1"}},
	}
	if err := ValidateCandidates(valid, concepts); err != nil {
		t.Fatalf("valid exploratory questions rejected: %v", err)
	}
	invalid := append([]Candidate(nil), valid...)
	invalid[0].Question = "台北有適合工作的咖啡廳"
	if err := ValidateCandidates(invalid, concepts); err == nil {
		t.Fatal("unsupported factual assertion accepted without question framing")
	}
	invalid = append([]Candidate(nil), valid...)
	invalid[0].CorpusAnchorConceptIDs = []string{"unknown-taipei"}
	if err := ValidateCandidates(invalid, concepts); err == nil {
		t.Fatal("unsupported corpus anchor accepted")
	}
}

type fixtureProvider struct {
	calls int
	user  string
	raw   string
	err   error
}

func (p *fixtureProvider) Chat(_ context.Context, _, user string) (string, error) {
	p.calls++
	p.user = user
	return p.raw, p.err
}

type blockingProvider struct {
	started chan struct{}
}

func (p *blockingProvider) Chat(ctx context.Context, _, _ string) (string, error) {
	close(p.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func TestGeneratePassesCancellationToProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &blockingProvider{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := Generate(ctx, provider, "", []conceptcache.Entry{{Slug: "c1", Title: "Concept 1"}}, nil, GenerationMetadata{Model: "fixture", PromptVersion: "v1"}, time.Now())
		done <- err
	}()
	<-provider.started
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Generate() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Generate() did not interrupt the provider call after cancellation")
	}
}

func TestGenerateUsesBoundedRepresentativeConceptsAndOptionalDescription(t *testing.T) {
	entries := make([]conceptcache.Entry, 0, MaxConcepts+2)
	for i := 0; i < MaxConcepts+2; i++ {
		entries = append(entries, conceptcache.Entry{
			Slug:  "concept-" + string(rune('a'+i)),
			Title: "Concept " + string(rune('A'+i)),
			Body:  strings.Repeat("evidence ", 500),
			Frontmatter: map[string]interface{}{
				"id": "id-" + string(rune('a'+i)),
			},
		})
	}
	entries = append(entries, conceptcache.Entry{Slug: "index", Title: "System Index", Frontmatter: map[string]interface{}{"id": "system"}})
	provider := &fixtureProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["id-c"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["id-d"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["id-c"]}
]}`}
	artifact, err := Generate(context.Background(), provider, "", entries, nil, GenerationMetadata{Model: "fixture", PromptVersion: "v1"}, time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if len(artifact.Queries) != 3 || artifact.Version != 2 || len(artifact.Candidates) != 3 {
		t.Fatalf("artifact = %#v, want three published candidates", artifact)
	}
	var input struct {
		Description string            `json:"project_description"`
		Concepts    []ConceptEvidence `json:"concepts"`
	}
	if err := json.Unmarshal([]byte(provider.user), &input); err != nil {
		t.Fatalf("decode provider input: %v", err)
	}
	if len(input.Concepts) != MaxConcepts {
		t.Fatalf("provider concepts = %d, want bounded %d", len(input.Concepts), MaxConcepts)
	}
	if input.Description != "" {
		t.Fatalf("description = %q, want optional empty seam", input.Description)
	}
	if input.Concepts[0].Evidence == "" || len([]byte(input.Concepts[0].Evidence)) > 1200 {
		t.Fatalf("first evidence length = %d, want non-empty and bounded", len([]byte(input.Concepts[0].Evidence)))
	}
}

func TestDecodePreservesVersionedCandidatesAndRejectsLegacyTitleArtifact(t *testing.T) {
	candidates := []Candidate{
		{Question: "哪些概念值得一起比較？", Intent: "comparison", CorpusAnchorConceptIDs: []string{"c1"}, Generation: GenerationMetadata{Model: "fixture", PromptVersion: "v1"}},
		{Question: "如何探索這個主題的不同面向？", Intent: "exploration", CorpusAnchorConceptIDs: []string{"c1"}, Generation: GenerationMetadata{Model: "fixture", PromptVersion: "v1"}},
		{Question: "哪些選擇適合進一步查找？", Intent: "retrieval", CorpusAnchorConceptIDs: []string{"c1"}, Generation: GenerationMetadata{Model: "fixture", PromptVersion: "v1"}},
	}
	data, err := json.Marshal(ArtifactFromCandidates(candidates, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !IsPublishable(artifact) || len(artifact.Candidates) != 3 {
		t.Fatalf("artifact = %#v, want publishable versioned candidates", artifact)
	}
	legacy, err := Decode([]byte(`{"queries":["咖啡廳"]}`))
	if err != nil {
		t.Fatalf("Decode(legacy) error = %v", err)
	}
	if IsPublishable(legacy) {
		t.Fatal("legacy title-only artifact is publishable")
	}
}
