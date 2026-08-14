package queryquality_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestCorePlanEligibilityAndSelectionContracts(t *testing.T) {
	plan, err := queryquality.DecodePlan(`{"raw_query":"Taipei cafe","required":[{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}],"excluded":[{"kind":"exclusion","value":"smoking","terms":["smoking"],"proof":"lexical"}],"preferred":[{"kind":"venue_type","value":"cafe","terms":["cafe"],"proof":"lexical"}],"goals":[{"kind":"recommendation","value":"work","terms":["work"],"proof":"lexical"}],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`, "Taipei cafe")
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

func TestDecodePlanRequiresStrictMinimalV1Contract(t *testing.T) {
	valid := `{"raw_query":"Taipei cafe","required":[{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}],"excluded":[],"preferred":[],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`
	if _, err := queryquality.DecodePlan(valid, "Taipei cafe"); err != nil {
		t.Fatalf("valid minimal-v1 plan rejected: %v", err)
	}
	for _, test := range []struct {
		name     string
		response string
		raw      string
	}{
		{name: "missing top level fields", response: `{}`, raw: "coffee"},
		{name: "unknown top level field", response: valid[:len(valid)-1] + `,"extra":1}`, raw: "Taipei cafe"},
		{name: "fallback true", response: strings.Replace(valid, `"fallback":false`, `"fallback":true`, 1), raw: "Taipei cafe"},
		{name: "supporting dimensions populated", response: strings.Replace(valid, `"supporting_dimensions":[]`, `"supporting_dimensions":[{"kind":"x","value":"y","terms":["y"],"proof":"lexical"}]`, 1), raw: "Taipei cafe"},
		{name: "empty plan", response: strings.Replace(valid, `{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}`, ``, 1), raw: "Taipei cafe"},
		{name: "raw query mismatch", response: valid, raw: "different"},
		{name: "criterion missing proof", response: strings.Replace(valid, `,"proof":"lexical"`, ``, 1), raw: "Taipei cafe"},
		{name: "lexical criterion has no terms", response: strings.Replace(valid, `"terms":["Taipei"]`, `"terms":[]`, 1), raw: "Taipei cafe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := queryquality.DecodePlan(test.response, test.raw); err == nil {
				t.Fatal("DecodePlan() error = nil, want contract rejection")
			}
		})
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

func TestStructuredPlanPromptIsMinimalV1(t *testing.T) {
	provider := &promptProvider{response: `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}
	expander := queryquality.NewStructuredPlanExpander(provider, nil)
	if _, err := expander.ExpandPlan(context.Background(), "coffee", queryquality.DefaultCriterionPolicy, nil); err != nil {
		t.Fatal(err)
	}
	wantSystem := `You produce a retrieval plan for a frozen Lifestyle concept corpus. Return exactly one JSON object and no markdown. The object fields and exact types are: raw_query string; required array of Criterion; excluded array of Criterion; preferred array of Criterion; goals array of Criterion; supporting_dimensions array of Criterion; acceptable_alternatives array of Criterion; ambiguity array of strings; fallback boolean. Every Criterion is exactly {kind:string,value:string,terms:array of strings,proof:"lexical" or "semantic"}. Every lexical Criterion needs at least one discovery term. Never output a string where an array or object is required. Be conservative: only explicit user constraints may be required or excluded; absent never means excluded. In this minimal variant, supporting_dimensions and acceptable_alternatives must be empty arrays and fallback must be false.`
	wantUser := `Raw query: "coffee"` + "\n" + `Criterion policy: {"required_when_explicit":["location","explicit_exclusion"],"preferred_by_default":["venue_type","activity","audience","setting"],"goals_to_expand":["suitability","recommendation","discovery"]}` + "\nInterpret the query into required, excluded, preferred and goals. Preserve the raw query exactly in raw_query. Return the single JSON object only."
	if provider.system != wantSystem || provider.user != wantUser {
		t.Fatalf("prompt mismatch:\nsystem=%q\nuser=%q", provider.system, provider.user)
	}
	if queryquality.StructuredPlanPromptID != "minimal-v1" {
		t.Fatalf("prompt ID = %q, want minimal-v1", queryquality.StructuredPlanPromptID)
	}
}

func TestSemanticRequiredAndExcludedFailClosedRoles(t *testing.T) {
	for _, test := range []struct {
		outcome  string
		required bool
		want     bool
	}{
		{outcome: "pass", required: true, want: true},
		{outcome: "fail", required: true, want: false},
		{outcome: "unknown", required: true, want: false},
		{outcome: "unavailable", required: true, want: false},
		{outcome: "pass", want: false},
		{outcome: "fail", want: true},
		{outcome: "unknown", want: false},
		{outcome: "unavailable", want: false},
	} {
		name := "excluded-" + test.outcome
		if test.required {
			name = "required-" + test.outcome
		}
		t.Run(name, func(t *testing.T) {
			plan := queryquality.QueryPlan{}
			criterion := queryquality.Criterion{Kind: "intent", Value: "coffee", Proof: "semantic"}
			if test.required {
				plan.Required = []queryquality.Criterion{criterion}
			} else {
				plan.Excluded = []queryquality.Criterion{criterion}
			}
			matched, err := queryquality.NewLexicalMatcher(fixedSemanticEvaluator{outcome: test.outcome}).Match(context.Background(), plan, []cache.Entry{{Slug: "candidate", Title: "Candidate"}})
			if err != nil {
				t.Fatal(err)
			}
			if matched.Candidates[0].Eligible != test.want {
				t.Fatalf("eligible = %v, want %v", matched.Candidates[0].Eligible, test.want)
			}
		})
	}
}

func TestServicePreservesCanceledCorpusRead(t *testing.T) {
	service := queryquality.NewService(
		testPlanExpander{},
		queryquality.NewLexicalMatcher(nil), queryquality.NewSelector(), nil,
	)
	_, err := service.Execute(context.Background(), &jsonlReader{readErr: context.Canceled}, query.Request{Query: "coffee"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
}

func TestMatchingAndSelectionPreserveCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := queryquality.QueryPlan{Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}}
	if _, err := queryquality.NewLexicalMatcher(nil).Match(ctx, plan, []cache.Entry{{Slug: "coffee"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Match() error = %v, want context.Canceled", err)
	}
	if _, err := queryquality.NewSelector().Select(ctx, queryquality.SelectionInput{Candidates: []queryquality.CandidateEvidence{{Slug: "coffee", Eligible: true}}, Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Select() error = %v, want context.Canceled", err)
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

func TestProductionStructuredPlanPreservesLegacyExpandWireShape(t *testing.T) {
	provider := fakeProvider{response: `{"raw_query":"coffee","required":[{"kind":"location","value":"Taipei","terms":["Taipei","coffee"],"proof":"lexical"}],"excluded":[{"kind":"avoid","value":"crowded","terms":["crowded","coffee"],"proof":"lexical"}],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee","cafe"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}
	legacy := &countingExecutor{}
	executor, err := queryquality.NewProductionExecutor(cache.New(), provider, legacy, nil, queryquality.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), &jsonlReader{data: []byte(`{"slug":"coffee","title":"Coffee","body":""}` + "\n")}, query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.called {
		t.Fatal("valid structured plan invoked legacy executor")
	}
	if result.Expand == nil || !sameStrings(result.Expand.Keywords, []string{"Taipei", "coffee", "crowded", "cafe"}) {
		t.Fatalf("Expand = %#v, want stable deduplicated plan terms", result.Expand)
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

type promptProvider struct {
	response string
	system   string
	user     string
}

type fixedSemanticEvaluator struct{ outcome string }

func (e fixedSemanticEvaluator) Evaluate(context.Context, queryquality.Criterion, cache.Entry) (queryquality.SemanticDecision, error) {
	return queryquality.SemanticDecision{Outcome: e.outcome}, nil
}

type testPlanExpander struct{}

func (testPlanExpander) ExpandPlan(context.Context, string, queryquality.CriterionPolicy, []cache.Entry) (queryquality.QueryPlan, error) {
	return queryquality.QueryPlan{}, nil
}

func (p *promptProvider) Chat(_ context.Context, system, user string) (string, error) {
	p.system, p.user = system, user
	return p.response, nil
}

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

type jsonlReader struct {
	data    []byte
	readErr error
}

func (r *jsonlReader) Prefix() string                                             { return "users/test/projects/test" }
func (r *jsonlReader) ReadFile(context.Context, string) ([]byte, error)           { return r.data, r.readErr }
func (r *jsonlReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) { return nil, nil }
func (r *jsonlReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("unexpected page read")
}
