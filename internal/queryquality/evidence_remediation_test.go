package queryquality_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func TestKeywordSupportVotesOncePerConceptPerAttempt(t *testing.T) {
	expander, err := queryquality.NewParallelQueryExpander(&surfacePlanExpander{plans: []queryquality.QueryPlan{
		{RawQuery: "coffee shop", Preferred: []queryquality.Criterion{{Kind: "venue_type", Value: "coffee shop", Terms: []string{"cafe", "coffee shop"}, Proof: "lexical"}}},
	}}, nil, queryquality.Options{ExpansionAttempts: 1, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := expander.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeywordSupport) != 1 || plan.KeywordSupport[0].SupportCount != 1 {
		t.Fatalf("support = %#v, want one concept vote", plan.KeywordSupport)
	}
	if len(plan.KeywordSupport[0].SurfaceForms) != 2 || plan.KeywordSupport[0].SurfaceForms[0].Value != "cafe" || plan.KeywordSupport[0].SurfaceForms[0].AttemptIndexes[0] != 1 || plan.KeywordSupport[0].SurfaceForms[1].Value != "coffee shop" {
		t.Fatalf("surface forms = %#v, want deterministic normalized union", plan.KeywordSupport[0].SurfaceForms)
	}
}

func TestKeywordSupportUnifiesAliasesAcrossAttempts(t *testing.T) {
	expander, err := queryquality.NewParallelQueryExpander(&surfacePlanExpander{plans: []queryquality.QueryPlan{
		{RawQuery: "coffee shop", Preferred: []queryquality.Criterion{{Kind: "venue_type", Value: "coffee shop", Terms: []string{"cafe"}, Proof: "lexical"}}},
		{RawQuery: "coffee shop", Preferred: []queryquality.Criterion{{Kind: "venue_type", Value: "coffee shop", Terms: []string{"coffee shop"}, Proof: "lexical"}}},
	}}, nil, queryquality.Options{ExpansionAttempts: 2, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := expander.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeywordSupport) != 1 || plan.KeywordSupport[0].SupportCount != 2 || !reflect.DeepEqual(plan.KeywordSupport[0].AttemptIndexes, []int{1, 2}) {
		t.Fatalf("support = %#v, want one concept with two attempts", plan.KeywordSupport)
	}
	if !reflect.DeepEqual(plan.KeywordSupport[0].SurfaceForms, []queryquality.SurfaceForm{{Value: "cafe", AttemptIndexes: []int{1}}, {Value: "coffee shop", AttemptIndexes: []int{2}}}) {
		t.Fatalf("surface forms = %#v, want alias-local attempt indexes", plan.KeywordSupport[0].SurfaceForms)
	}
}

func TestAliasSurfaceFormsCannotCreateTwoRareOpportunities(t *testing.T) {
	plan := queryquality.QueryPlan{
		RawQuery:  "cafe coffee shop",
		Preferred: []queryquality.Criterion{{Kind: "venue_type", Value: "coffee shop", Terms: []string{"cafe", "coffee shop"}, Proof: "lexical"}},
		KeywordSupport: []queryquality.KeywordSupport{
			{Role: "preferred", Kind: "venue_type", Value: "coffee shop", Keyword: "cafe", SupportCount: 1, AttemptIndexes: []int{1}},
			{Role: "preferred", Kind: "venue_type", Value: "coffee shop", Keyword: "coffee shop", SupportCount: 1, AttemptIndexes: []int{1}},
		},
	}
	got, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: "Cafe Coffee Shop"}}, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates[0].KeywordEvidence) != 1 {
		t.Fatalf("candidate = %#v, aliases must create one opportunity", got.Candidates[0])
	}
}

func TestLexicalMatchingUsesUnicodeRuneBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		title string
		query string
		want  bool
	}{
		{name: "latin token", title: "Art Gallery", query: "art", want: true},
		{name: "latin substring", title: "Party Hall", query: "art", want: false},
		{name: "latin adjacent CJK", title: "Art咖啡館", query: "art", want: false},
		{name: "punctuation adjacent CJK", title: "Art，咖啡館", query: "art", want: true},
		{name: "latin substring in embargo", title: "Embargo", query: "bar", want: false},
		{name: "CJK phrase", title: "台北咖啡館", query: "咖啡館", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := queryquality.QueryPlan{RawQuery: test.query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: test.query, Terms: []string{test.query}, Proof: "lexical"}}, KeywordSupport: []queryquality.KeywordSupport{{Role: "preferred", Kind: "topic", Value: test.query, Keyword: test.query, SupportCount: 1, AttemptIndexes: []int{1}}}}
			got, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: test.title}}, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1})
			if err != nil {
				t.Fatal(err)
			}
			if got.Candidates[0].Qualified != test.want {
				t.Fatalf("candidate = %#v, want qualified=%v", got.Candidates[0], test.want)
			}
		})
	}
}

func TestExactIdentityRequiresCanonicalEqualityAndRawQueryGrounding(t *testing.T) {
	tests := []struct {
		name      string
		rawQuery  string
		value     string
		term      string
		entry     cache.Entry
		wantExact bool
	}{
		{name: "canonical title", rawQuery: "Cafe", value: "Cafe", term: "Cafe", entry: cache.Entry{Title: "Cafe"}, wantExact: true},
		{name: "explicit alias", rawQuery: "Cafe Bar", value: "Cafe Bar", term: "Cafe Bar", entry: cache.Entry{Title: "Cafe", Frontmatter: map[string]interface{}{"aliases": []string{"Cafe Bar"}}}, wantExact: true},
		{name: "title substring", rawQuery: "Cafe", value: "Cafe", term: "Cafe", entry: cache.Entry{Title: "Best Cafe Guide"}, wantExact: false},
		{name: "body only", rawQuery: "Cafe", value: "Cafe", term: "Cafe", entry: cache.Entry{Title: "Guide", Body: "Cafe"}, wantExact: false},
		{name: "provider value not grounded", rawQuery: "coffee", value: "Cafe", term: "Cafe", entry: cache.Entry{Title: "Cafe"}, wantExact: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := queryquality.QueryPlan{RawQuery: test.rawQuery, Required: []queryquality.Criterion{{Kind: "entity", Value: test.value, Terms: []string{test.term}, Proof: "lexical"}}}
			got, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: test.entry.Title, Body: test.entry.Body, Frontmatter: test.entry.Frontmatter}}, EvidenceThreshold: 3, EvidenceThresholdSet: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Candidates[0].ExactIdentityEvidence != test.wantExact {
				t.Fatalf("candidate = %#v, want exact=%v", got.Candidates[0], test.wantExact)
			}
		})
	}
}

func TestRawQueryCorpusIdentityQualifiesWhenPlanOmitsRequiredIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		fallback bool
	}{
		{name: "structured plan omission"},
		{name: "deterministic fallback", fallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := queryquality.QueryPlan{
				RawQuery:       "How does StateDB track compilation status?",
				Preferred:      []queryquality.Criterion{{Kind: "topic", Value: "compilation status", Terms: []string{"compilation status"}, Proof: "lexical"}},
				Fallback:       test.fallback,
				KeywordSupport: []queryquality.KeywordSupport{{Role: "preferred", Kind: "topic", Value: "compilation status", Keyword: "compilation status", SupportCount: 2, AttemptIndexes: []int{1, 2}}},
			}
			entries := []cache.Entry{
				{Slug: "body-a", Title: "Concept A", Body: "StateDB tracks compilation status."},
				{Slug: "state-db", Title: "StateDB", Body: "Tracks compilation status."},
				{Slug: "body-b", Title: "Concept B", Body: "StateDB records compilation status."},
			}
			matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
				Plan: plan, CorpusEntries: entries, EvidenceThreshold: 2, EvidenceThresholdSet: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			bySlug := make(map[string]queryquality.CandidateEvidence, len(matched.Candidates))
			for _, candidate := range matched.Candidates {
				bySlug[candidate.Slug] = candidate
			}
			if !bySlug["state-db"].ExactIdentityEvidence || !bySlug["state-db"].Qualified {
				t.Fatalf("exact corpus identity = %#v, want independently qualified", bySlug["state-db"])
			}
			for _, slug := range []string{"body-a", "body-b"} {
				if bySlug[slug].ExactIdentityEvidence {
					t.Fatalf("body-only candidate %q = %#v, must not establish exact identity", slug, bySlug[slug])
				}
			}
			selected, err := queryquality.NewResultSelector().Select(context.Background(), queryquality.SelectionInput{
				Candidates: matched.Candidates, Limit: 1, ExplorationSlots: 0, Seed: 7,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(selected.Selected) != len(entries) || !selected.Selected[1].Selected || selected.Selected[1].Slug != "state-db" {
				t.Fatalf("selection = %#v, want state-db selected ahead of tied body mentions", selected.Selected)
			}
		})
	}
}

func TestRawQueryCorpusIdentityRequiresTitleOrFrontmatterProof(t *testing.T) {
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan:              queryquality.QueryPlan{RawQuery: "StateDB"},
		CorpusEntries:     []cache.Entry{{Slug: "state-db", Title: "Compilation", Body: "StateDB"}},
		EvidenceThreshold: 2, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if matched.Candidates[0].ExactIdentityEvidence || matched.Candidates[0].Qualified {
		t.Fatalf("body-only identity = %#v, want no exact identity or qualification", matched.Candidates[0])
	}
}

func TestRawQueryCorpusIdentityUsesOnlyExactCanonicalTitleOrSlugProof(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		entry     cache.Entry
		wantExact bool
	}{
		{name: "slug substring in title", query: "coffee", entry: cache.Entry{Slug: "coffee", Title: "Coffee Guide"}},
		{name: "alias only", query: "coffee", entry: cache.Entry{Slug: "guide", Title: "Guide", Frontmatter: map[string]interface{}{"aliases": []string{"coffee"}}}},
		{name: "body only", query: "coffee", entry: cache.Entry{Slug: "guide", Title: "Guide", Body: "coffee"}},
		{name: "generic title substring", query: "coffee", entry: cache.Entry{Slug: "guide", Title: "Best Coffee Guide"}},
		{name: "exact title", query: "coffee guide", entry: cache.Entry{Slug: "coffee-guide", Title: "Coffee Guide"}, wantExact: true},
		{name: "safely proven slug", query: "coffee", entry: cache.Entry{Slug: "coffee", Title: "Coffee"}, wantExact: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
				Plan: queryquality.QueryPlan{RawQuery: test.query}, CorpusEntries: []cache.Entry{test.entry}, EvidenceThreshold: 2, EvidenceThresholdSet: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Candidates[0].ExactIdentityEvidence != test.wantExact {
				t.Fatalf("candidate=%#v want exact=%v", got.Candidates[0], test.wantExact)
			}
		})
	}
}

func TestQualificationRequiresLocalDiscriminativeConsensus(t *testing.T) {
	plan := queryquality.QueryPlan{
		RawQuery: "Firestore Pipeline Cloud Run Worker distributed locking",
		Preferred: []queryquality.Criterion{
			{Kind: "technology", Value: "firestore", Terms: []string{"firestore"}, Proof: "lexical"},
			{Kind: "technology", Value: "pipeline", Terms: []string{"pipeline"}, Proof: "lexical"},
			{Kind: "technology", Value: "cloud run", Terms: []string{"cloud run"}, Proof: "lexical"},
			{Kind: "role", Value: "worker", Terms: []string{"worker"}, Proof: "lexical"},
			{Kind: "topic", Value: "distributed locking", Terms: []string{"distributed locking"}, Proof: "lexical"},
		},
		KeywordSupport: []queryquality.KeywordSupport{
			{Role: "preferred", Kind: "technology", Value: "firestore", Keyword: "firestore", SupportCount: 2, AttemptIndexes: []int{1, 2}},
			{Role: "preferred", Kind: "technology", Value: "pipeline", Keyword: "pipeline", SupportCount: 2, AttemptIndexes: []int{1, 2}},
			{Role: "preferred", Kind: "technology", Value: "cloud run", Keyword: "cloud run", SupportCount: 2, AttemptIndexes: []int{1, 2}},
			{Role: "preferred", Kind: "role", Value: "worker", Keyword: "worker", SupportCount: 2, AttemptIndexes: []int{1, 2}},
			{Role: "preferred", Kind: "topic", Value: "distributed locking", Keyword: "distributed locking", SupportCount: 2, AttemptIndexes: []int{1, 2}},
		},
	}
	entries := []cache.Entry{
		{Slug: "firestore", Title: "Firestore"},
		{Slug: "pipeline", Title: "Pipeline"},
		{Slug: "cloud-run-worker", Title: "Cloud Run Worker"},
		{Slug: "pipeline-worker", Title: "Pipeline Worker"},
		{Slug: "bff", Title: "BFF", Body: "pipeline cloud run worker distributed locking"},
		{Slug: "jwt", Title: "JWT", Body: "pipeline"},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: entries, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]queryquality.CandidateEvidence, len(matched.Candidates))
	for _, candidate := range matched.Candidates {
		bySlug[candidate.Slug] = candidate
	}
	for _, slug := range []string{"firestore", "pipeline", "cloud-run-worker", "pipeline-worker"} {
		if !bySlug[slug].Qualified {
			t.Fatalf("candidate %q = %#v, want qualified", slug, bySlug[slug])
		}
	}
	for _, slug := range []string{"bff", "jwt"} {
		if bySlug[slug].Qualified {
			t.Fatalf("candidate %q = %#v, want unqualified", slug, bySlug[slug])
		}
	}
}

func TestSelectorIncludesExactIdentityBeforeHigherScoringGenericCandidate(t *testing.T) {
	selected, err := queryquality.NewResultSelector().Select(context.Background(), queryquality.SelectionInput{
		Candidates: []queryquality.CandidateEvidence{
			{Slug: "generic", Title: "Generic", Eligible: true, Qualified: true, Score: 5},
			{Slug: "named", Title: "Named", Eligible: true, Qualified: true, ExactIdentityEvidence: true, Score: 0},
		},
		Limit: 1, ExplorationSlots: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !selected.Selected[1].Selected || selected.Selected[1].Slug != "named" {
		t.Fatalf("selection = %#v, want exact identity selected within limit", selected.Selected)
	}
}

func TestFrontmatterMatchingIsAllowlistedAndDeterministic(t *testing.T) {
	plan := queryquality.QueryPlan{RawQuery: "coffee", Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}, KeywordSupport: []queryquality.KeywordSupport{{Role: "preferred", Kind: "topic", Value: "coffee", Keyword: "coffee", SupportCount: 1, AttemptIndexes: []int{1}}}}
	entries := []cache.Entry{
		{Slug: "metadata", Title: "Metadata", Frontmatter: map[string]interface{}{"description": "coffee", "source": "coffee"}},
		{Slug: "tagged", Title: "Tagged", Frontmatter: map[string]interface{}{"tags": []string{"coffee"}}},
	}
	matcher := queryquality.NewLexicalMatcher(nil)
	first, err := matcher.Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: entries, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates[0].KeywordEvidence) != 0 || first.Candidates[0].Qualified {
		t.Fatalf("arbitrary metadata matched as strong evidence: %#v", first.Candidates[0])
	}
	if len(first.Candidates[1].KeywordEvidence) != 1 || !first.Candidates[1].Qualified {
		t.Fatalf("allowlisted tag did not match: %#v", first.Candidates[1])
	}
	firstJSON, _ := json.Marshal(first)
	for i := 0; i < 20; i++ {
		repeated, err := matcher.Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: entries, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1})
		if err != nil {
			t.Fatal(err)
		}
		repeatedJSON, _ := json.Marshal(repeated)
		if !reflect.DeepEqual(firstJSON, repeatedJSON) {
			t.Fatalf("matcher output changed on repeat: %s != %s", firstJSON, repeatedJSON)
		}
	}
}

type surfacePlanExpander struct {
	plans []queryquality.QueryPlan
}

func (e *surfacePlanExpander) Expand(_ context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	return e.plans[request.Attempt-1], nil
}
