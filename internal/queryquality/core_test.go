package queryquality_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestCorePlanEligibilityAndSelectionContracts(t *testing.T) {
	plan, err := queryquality.DecodePlan(`{"required":[{"kind":"location","value":"Taipei","terms":["Taipei"]}],"excluded":[{"kind":"exclusion","value":"smoking","terms":["smoking"]}],"preferred":[{"kind":"venue_type","value":"cafe","terms":["cafe"]}],"goals":[{"kind":"recommendation","value":"work","terms":["work"]}]}`, "Taipei cafe")
	if err != nil {
		t.Fatal(err)
	}
	entries := []cache.Entry{
		{Slug: "positive", Title: "Taipei cafe", Body: "Taipei cafe for work"},
		{Slug: "optional-miss", Title: "Taipei place", Body: "Taipei"},
		{Slug: "required-miss", Title: "Kaohsiung cafe", Body: "cafe for work"},
		{Slug: "excluded", Title: "Taipei smoking cafe", Body: "Taipei cafe smoking"},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), plan, entries)
	if err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]queryquality.CandidateEvidence, len(matched.Candidates))
	for _, candidate := range matched.Candidates {
		bySlug[candidate.Slug] = candidate
	}
	if !bySlug["positive"].Eligible || !bySlug["optional-miss"].Eligible {
		t.Fatal("optional misses must not hard-gate an eligible candidate")
	}
	if bySlug["required-miss"].Eligible || bySlug["excluded"].Eligible {
		t.Fatal("required/excluded criteria were not enforced conservatively")
	}

	semanticPlan := queryquality.QueryPlan{RawQuery: "semantic", Required: []queryquality.Criterion{{Kind: "activity", Value: "skiing", Terms: []string{"skiing"}, Proof: "semantic"}}}
	semanticMatched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), semanticPlan, []cache.Entry{{Slug: "lexical", Title: "Lexical", Body: "skiing"}})
	if err != nil {
		t.Fatal(err)
	}
	if semanticMatched.Candidates[0].Eligible || semanticMatched.Candidates[0].Groups[0].SemanticOutcome != "unavailable" {
		t.Fatal("lexical text must not infer semantic proof")
	}

	candidates := append(matched.Candidates, queryquality.CandidateEvidence{Slug: "ineligible-high-score", Title: "bad", Eligible: false, Score: 99})
	selector := queryquality.NewSelector()
	input := queryquality.SelectionInput{Candidates: candidates, Limit: 2, ExplorationSlots: 1, Seed: 42}
	first, err := selector.Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := selector.Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("selection replay differs: %s vs %s", firstJSON, secondJSON)
	}
	for _, candidate := range first.Selected {
		if candidate.Selected && candidate.Slug == "ineligible-high-score" {
			t.Fatal("exploration admitted an ineligible candidate")
		}
	}
	if !containsSelected(first.Selected, "positive") {
		t.Fatal("known-positive fixture was not selected")
	}
}

func TestDefaultCriterionPolicyIsPlatformOwnedLifestyleV1(t *testing.T) {
	policy := queryquality.DefaultCriterionPolicy
	if !sameStrings(policy.RequiredWhenExplicit, []string{"location", "explicit_exclusion"}) {
		t.Fatalf("required policy = %v", policy.RequiredWhenExplicit)
	}
	if !sameStrings(policy.PreferredByDefault, []string{"venue_type", "activity", "audience", "setting"}) {
		t.Fatalf("preferred policy = %v", policy.PreferredByDefault)
	}
	if !sameStrings(policy.GoalsToExpand, []string{"suitability", "recommendation", "discovery"}) {
		t.Fatalf("goal policy = %v", policy.GoalsToExpand)
	}
}

func TestProductionExpansionFailureAndInvalidJSONUseLegacyBoundedFallback(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider queryquality.ChatProvider
	}{
		{name: "provider absent", provider: nil},
		{name: "provider failure", provider: fakeProvider{err: errors.New("provider unavailable")}},
		{name: "invalid JSON", provider: fakeProvider{response: "not-json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			legacy := fakeExecutor{result: query.Result{Query: "q", Mode: "wiki", Results: []search.Result{{Slug: "legacy-hit"}}}}
			executor, err := queryquality.NewProductionExecutor(cache.New(), test.provider, legacy, nil, queryquality.DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			got, err := executor.Execute(context.Background(), &jsonlReader{data: []byte(`{"slug":"all-candidate","title":"all","body":""}` + "\n")}, query.Request{Query: "q", Mode: "wiki"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Results) != 1 || got.Results[0].Slug != "legacy-hit" {
				t.Fatalf("fallback result = %#v, want legacy bounded result", got.Results)
			}
		})
	}
}

func TestProductionExpansionCancellationDoesNotFallback(t *testing.T) {
	started := make(chan struct{})
	provider := blockingProvider{started: started}
	legacy := &countingExecutor{}
	executor, err := queryquality.NewProductionExecutor(cache.New(), provider, legacy, nil, queryquality.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, &jsonlReader{data: []byte(`{"slug":"candidate","title":"candidate","body":""}` + "\n")}, query.Request{Query: "q"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not return after cancellation")
	}
	if legacy.called {
		t.Fatal("cancellation must not invoke legacy fallback")
	}
}

func containsSelected(values []queryquality.SelectedCandidate, slug string) bool {
	for _, value := range values {
		if value.Selected && value.Slug == slug {
			return true
		}
	}
	return false
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type fakeProvider struct {
	response string
	err      error
}

func (f fakeProvider) Chat(context.Context, string, string) (string, error) { return f.response, f.err }

type fakeExecutor struct{ result query.Result }

func (f fakeExecutor) Execute(context.Context, cache.Reader, query.Request) (query.Result, error) {
	return f.result, nil
}

type countingExecutor struct{ called bool }

func (e *countingExecutor) Execute(context.Context, cache.Reader, query.Request) (query.Result, error) {
	e.called = true
	return query.Result{}, nil
}

type blockingProvider struct{ started chan struct{} }

func (p blockingProvider) Chat(ctx context.Context, _, _ string) (string, error) {
	close(p.started)
	<-ctx.Done()
	return "", ctx.Err()
}

type jsonlReader struct{ data []byte }

func (r *jsonlReader) Prefix() string                                             { return "users/test/projects/test" }
func (r *jsonlReader) ReadFile(context.Context, string) ([]byte, error)           { return r.data, nil }
func (r *jsonlReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) { return nil, nil }
func (r *jsonlReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("unexpected page read")
}
