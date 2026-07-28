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

func TestParseProviderCandidatesRejectsMalformedAndOversizedOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "malformed json", raw: `{"candidates":`},
		{name: "wrong shape", raw: `[{"question":"哪些概念值得一起比較？"}]`},
		{name: "oversized", raw: strings.Repeat("x", MaxProviderBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProviderCandidates(tc.raw); err == nil {
				t.Fatal("parseProviderCandidates() error = nil, want rejection")
			}
		})
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

func (p *fixtureProvider) Chat(_, user string) (string, error) {
	p.calls++
	p.user = user
	return p.raw, p.err
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
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["id-c"],"generation":{"model":"fixture","prompt_version":"v1"}},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["id-d"],"generation":{"model":"fixture","prompt_version":"v1"}},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["id-c"],"generation":{"model":"fixture","prompt_version":"v1"}}
]}`}
	artifact, err := Generate(context.Background(), provider, "", entries, nil, time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC))
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
