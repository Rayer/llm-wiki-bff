package queryquality_test

import (
	"context"
	"sync"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

type countingProvider struct {
	mu     sync.Mutex
	calls  int
	system string
	user   string
}

func (p *countingProvider) Chat(_ context.Context, system, user string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.system, p.user = system, user
	return `{"raw_query":"deploy docs","preferred":[{"kind":"topic","value":"deploy","terms":["deploy"],"proof":"lexical"}]}`, nil
}

type countingMatcher struct{ calls int }

func (m *countingMatcher) Match(_ context.Context, request queryquality.MatchRequest) (queryquality.EligibilityResult, error) {
	m.calls++
	return queryquality.EligibilityResult{Candidates: []queryquality.CandidateEvidence{{Slug: "deploy", Title: "Deploy", Eligible: true, Qualified: true, Score: 1}}}, nil
}

type policyMatcher struct{ request queryquality.MatchRequest }

func (m *policyMatcher) Match(_ context.Context, request queryquality.MatchRequest) (queryquality.EligibilityResult, error) {
	m.request = request
	return queryquality.EligibilityResult{Candidates: []queryquality.CandidateEvidence{{Slug: "deploy", Title: "Deploy", Eligible: true, Qualified: true, Score: 1}}}, nil
}

type countingSelector struct{ calls int }

func (s *countingSelector) Select(_ context.Context, input queryquality.SelectionInput) (queryquality.SelectionResult, error) {
	s.calls++
	return queryquality.SelectionResult{Selected: []queryquality.SelectedCandidate{{Slug: input.Candidates[0].Slug, Title: input.Candidates[0].Title, Selected: true}}}, nil
}

func TestQueryRetrievalServiceComposesAllStagesOnceAndCarriesPromptIdentity(t *testing.T) {
	provider := &countingProvider{}
	matcher := &countingMatcher{}
	selector := &countingSelector{}
	options := queryquality.Options{SelectionLimit: 1, KeywordsPerAttempt: 12, ExpansionAttempts: 1, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareDocumentFrequency: 1, Seed: int64Ptr(9)}
	profile := queryquality.RetrievalProfile{ID: "technical", CriterionPolicy: queryquality.CriterionPolicy{PreferredByDefault: []string{"topic"}}}
	service, err := queryquality.NewQueryRetrievalService(queryquality.QueryRetrievalServiceConfig{
		Cache: cache.New(), ChatProvider: provider, Options: options, RetrievalProfile: profile,
		PromptID: queryquality.DomainNeutralTechnicalPromptID, AllowDeterministicFallback: true,
		CandidateMatcher: matcher, ResultSelector: selector,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, trace, err := service.ExecuteWithTrace(context.Background(), &jsonlReader{data: []byte(`{"slug":"deploy","title":"Deploy","body":""}` + "\n")}, query.Request{Query: "deploy docs", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || matcher.calls != 1 || selector.calls != 1 {
		t.Fatalf("stage calls provider=%d matcher=%d selector=%d", provider.calls, matcher.calls, selector.calls)
	}
	if result.Status != "ok" || len(result.Results) != 1 || result.Results[0].Slug != "deploy" {
		t.Fatalf("result=%#v", result)
	}
	profileDigest, err := profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	prompt, _ := queryquality.LookupPrompt(queryquality.DomainNeutralTechnicalPromptID)
	if trace.ProfileID != profile.ID || trace.ProfileDigest != profileDigest || trace.PromptID != prompt.ID || trace.PromptDigest != prompt.TemplateDigest || trace.Seed != 9 {
		t.Fatalf("trace identity/config=%#v", trace)
	}
	if got := trace.MatchingPolicy; got.EvidenceThreshold != 2 || got.RareKeywordMaxDocumentFrequency != 1 || got.FallbackQualificationAllowed || !got.SemanticRequiredFailClosed || !got.SemanticExcludedFailClosed {
		t.Fatalf("matching policy=%#v", got)
	}
	expected, err := queryquality.RenderPrompt(prompt.ID, "deploy docs", profile.CriterionPolicy, options.KeywordsPerAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if provider.system != expected.System || provider.user != expected.User {
		t.Fatalf("prompt mismatch system=%q user=%q", provider.system, provider.user)
	}
}

func TestQueryRetrievalServiceCopiesConfigAndUsesLifestyleDefaults(t *testing.T) {
	seed := int64(7)
	profile := queryquality.DefaultRetrievalProfile()
	service, err := queryquality.NewQueryRetrievalService(queryquality.QueryRetrievalServiceConfig{
		Cache: cache.New(), Options: queryquality.Options{SelectionLimit: 1, ExpansionAttempts: 1, KeywordsPerAttempt: 12, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareDocumentFrequency: 1, Seed: &seed},
		RetrievalProfile: profile, PromptID: queryquality.StructuredPlanPromptID, AllowDeterministicFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seed = 99
	profile.CriterionPolicy.PreferredByDefault[0] = "mutated"
	_, trace, err := service.ExecuteWithTrace(context.Background(), &jsonlReader{data: []byte(`{"slug":"coffee","title":"Coffee","body":"coffee"}` + "\n")}, query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	defaultPrompt, _ := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	if trace.PromptID != defaultPrompt.ID || trace.PromptDigest != defaultPrompt.TemplateDigest || trace.Seed != 7 || trace.ProfileID != queryquality.DefaultRetrievalProfile().ID {
		t.Fatalf("default trace=%#v", trace)
	}
}

func TestQueryRetrievalServiceNormalizesRareDocumentFrequencyOnce(t *testing.T) {
	for _, test := range []struct {
		name  string
		want  int
		input int
	}{
		{name: "default", want: queryquality.DefaultRareDocumentFrequency, input: 0},
		{name: "explicit", want: 7, input: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &countingProvider{}
			matcher := &policyMatcher{}
			options := queryquality.Options{SelectionLimit: 1, KeywordsPerAttempt: 12, ExpansionAttempts: 1, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareDocumentFrequency: test.input}
			service, err := queryquality.NewQueryRetrievalService(queryquality.QueryRetrievalServiceConfig{
				Cache: cache.New(), ChatProvider: provider, Options: options, RetrievalProfile: queryquality.DefaultRetrievalProfile(),
				PromptID: queryquality.StructuredPlanPromptID, AllowDeterministicFallback: true, CandidateMatcher: matcher, ResultSelector: &countingSelector{},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, trace, err := service.ExecuteWithTrace(context.Background(), &jsonlReader{data: []byte(`{"slug":"deploy","title":"Deploy","body":""}` + "\n")}, query.Request{Query: "deploy", Mode: "wiki"})
			if err != nil {
				t.Fatal(err)
			}
			if matcher.request.RareKeywordMaxDocumentFrequency != test.want || trace.MatchingPolicy.RareKeywordMaxDocumentFrequency != test.want {
				t.Fatalf("request=%+v trace=%+v want rare document frequency %d", matcher.request, trace.MatchingPolicy, test.want)
			}
		})
	}
}

func int64Ptr(value int64) *int64 { return &value }
