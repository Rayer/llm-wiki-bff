package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestDeterministicFallbackIsGenericAndOptionalEvidenceDoesNotGate(t *testing.T) {
	for _, raw := range []string{"台北適合工作的咖啡廳", "小孩想玩水，有什麼室外戲水景點嗎", "unrelated astronomy query"} {
		plan, err := newDeterministicExpander().ExpandPlan(context.Background(), raw, defaultCriterionPolicy, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Fallback || len(plan.Required) != 0 || len(plan.Excluded) != 0 || len(plan.Goals) != 0 || len(plan.SupportingDimensions) != 0 || len(plan.AcceptableAlternatives) != 0 {
			t.Fatalf("fallback for %q = %#v, want one generic preferred criterion", raw, plan)
		}
		if len(plan.Preferred) != 1 || plan.Preferred[0].Kind != "query" || plan.Preferred[0].Value != strings.Join(strings.Fields(raw), " ") {
			t.Fatalf("fallback for %q = %#v, want normalized raw query", raw, plan)
		}
	}

	plan := QueryPlan{
		Required:               []Criterion{{Kind: "location", Value: "Taipei", Terms: []string{"Taipei"}}},
		Preferred:              []Criterion{{Kind: "preferred", Value: "coffee", Terms: []string{"coffee"}}},
		Goals:                  []Criterion{{Kind: "goal", Value: "quiet", Terms: []string{"quiet"}}},
		SupportingDimensions:   []Criterion{{Kind: "supporting", Value: "work", Terms: []string{"work"}}},
		AcceptableAlternatives: []Criterion{{Kind: "alternative", Value: "tea", Terms: []string{"tea"}}},
	}
	got, err := newLexicalMatcher(nil).Match(context.Background(), plan, []cache.Entry{{Slug: "taipei", Title: "Taipei", Body: ""}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Candidates[0].Eligible || got.Candidates[0].Score != 0 {
		t.Fatalf("candidate = %#v, want eligible score 0 when optional evidence misses", got.Candidates[0])
	}
}

func TestDecodeStructuredPlanSupportsExtendedDimensionsInFixtureMode(t *testing.T) {
	response := `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[{"kind":"mood","value":"quiet","terms":["quiet"],"proof":"lexical"}],"acceptable_alternatives":[{"kind":"topic","value":"tea","terms":["tea"],"proof":"lexical"}],"ambiguity":[],"fallback":false}`
	plan, err := decodeStructuredPlan(response, "coffee")
	if err != nil {
		t.Fatalf("decodeStructuredPlan() error = %v, want generic acceptance", err)
	}
	if len(plan.SupportingDimensions) != 1 || len(plan.AcceptableAlternatives) != 1 {
		t.Fatalf("plan=%#v, want fixture mode accepted extended dimensions", plan)
	}
}

func TestSemanticCriteriaNeverBecomeLexicalEvidenceOrScore(t *testing.T) {
	semantic := func(kind string) Criterion {
		return Criterion{Kind: kind, Value: "private " + kind, Terms: []string{"private", kind}, Proof: "semantic"}
	}
	plan := QueryPlan{
		Required: []Criterion{semantic("required")}, Excluded: []Criterion{semantic("excluded")}, Preferred: []Criterion{semantic("preferred")},
		Goals: []Criterion{semantic("goal")}, SupportingDimensions: []Criterion{semantic("supporting")}, AcceptableAlternatives: []Criterion{semantic("alternative")},
	}
	got, err := newLexicalMatcher(nil).Match(context.Background(), plan, []cache.Entry{{Slug: "candidate", Title: "candidate", Body: "private required private excluded private preferred private goal private supporting private alternative"}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := got.Candidates[0]
	if candidate.Eligible || candidate.Score != 0 || candidate.SemanticOutcome != "unavailable" {
		t.Fatalf("candidate = %#v, want required unavailable and optional semantics unscored", candidate)
	}
	for _, group := range candidate.Groups {
		if len(group.Matches) != 0 || group.SemanticOutcome != "unavailable" {
			t.Fatalf("semantic group = %#v, want unavailable without lexical matches", group)
		}
	}
}

func TestSemanticEvaluatorIsExplicitAndStillNeverScores(t *testing.T) {
	evaluator := &recordingSemanticEvaluator{}
	plan := QueryPlan{
		Required: []Criterion{{Kind: "required", Value: "required", Proof: "semantic"}}, Excluded: []Criterion{{Kind: "excluded", Value: "excluded", Proof: "semantic"}},
		Preferred: []Criterion{{Kind: "preferred", Value: "preferred", Proof: "semantic"}}, Goals: []Criterion{{Kind: "goal", Value: "goal", Proof: "semantic"}},
		SupportingDimensions: []Criterion{{Kind: "supporting", Value: "supporting", Proof: "semantic"}}, AcceptableAlternatives: []Criterion{{Kind: "alternative", Value: "alternative", Proof: "semantic"}},
	}
	got, err := newLexicalMatcher(evaluator).Match(context.Background(), plan, []cache.Entry{{Slug: "candidate", Title: "candidate"}})
	if err != nil || got.Candidates[0].Eligible || got.Candidates[0].Score != 0 || len(evaluator.calls) != 6 {
		t.Fatalf("candidate=%#v calls=%v err=%v", got.Candidates[0], evaluator.calls, err)
	}
	for _, group := range got.Candidates[0].Groups {
		if group.SemanticOutcome != "pass" || len(group.Matches) != 0 {
			t.Fatalf("semantic group=%#v, want evaluator outcome without lexical evidence", group)
		}
	}
}

func TestStructuredExpansionUsesOneCallAndSimpleFallback(t *testing.T) {
	provider := &recordingChatProvider{responses: []string{`{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}}
	expander := newStructuredPlanExpander(provider, newDeterministicExpander()).(structuredPlanExpander)
	plan, info, err := expander.ExpandPlanWithTrace(context.Background(), "coffee", defaultCriterionPolicy, []cache.Entry{{Slug: "espresso", Title: "Espresso Guide"}})
	if err != nil || plan.Fallback || info.source != "structured-llm" || len(provider.prompts) != 1 {
		t.Fatalf("plan=%#v info=%#v calls=%d err=%v", plan, info, len(provider.prompts), err)
	}
	for _, response := range []string{`{"unknown":1}`, `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false} {}`} {
		provider := fixedChatProvider{response: response}
		plan, info, err := newStructuredPlanExpander(provider, newDeterministicExpander()).(structuredPlanExpander).ExpandPlanWithTrace(context.Background(), "coffee", defaultCriterionPolicy, nil)
		if err != nil || !plan.Fallback || info.source != "deterministic-fallback" || info.fallbackReason != "invalid_plan" {
			t.Fatalf("response=%q plan=%#v info=%#v err=%v", response, plan, info, err)
		}
	}
}

func TestSelectionReplaysSeedAndHonorsZeroOneAndMultipleExplorationSlots(t *testing.T) {
	candidates := []CandidateEvidence{
		{Slug: "one", Title: "One", Eligible: true, Score: 5}, {Slug: "two", Title: "Two", Eligible: true, Score: 4},
		{Slug: "three", Title: "Three", Eligible: true, Score: 3}, {Slug: "four", Title: "Four", Eligible: true, Score: 2},
		{Slug: "five", Title: "Five", Eligible: true, Score: 1},
	}
	selector := newRandomSelector()
	for _, slots := range []int{0, 1, 2} {
		input := SelectionInput{Candidates: candidates, Limit: 3, ExplorationSlots: slots, Seed: 99}
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
			t.Fatalf("slots %d changed exact replay: %s != %s", slots, firstJSON, secondJSON)
		}
		selected, explored := 0, 0
		for _, candidate := range first.Selected {
			if candidate.Selected {
				selected++
			}
			if candidate.Exploration {
				explored++
			}
		}
		if selected != 3 || explored != slots {
			t.Fatalf("slots %d selected=%d explored=%d, want 3/%d", slots, selected, explored, slots)
		}
	}
}

func TestThreeHostOptionBoundsAndQueryDerivedSeed(t *testing.T) {
	if got, err := normalizeThreeHostOptions(threeHostOptions{}); err != nil || got.selectionLimit != defaultLimit || got.explorationSlots != 0 {
		t.Fatalf("zero options = %#v err=%v, want default limit and valid zero slots", got, err)
	}
	for _, options := range []threeHostOptions{{selectionLimit: -1}, {selectionLimit: maxSelectionLimit + 1}, {selectionLimit: 3, explorationSlots: -1}, {selectionLimit: 3, explorationSlots: 4}} {
		if _, err := normalizeThreeHostOptions(options); err == nil {
			t.Fatalf("options=%#v accepted, want validation error", options)
		}
	}
	if reproducibleSeed("coffee") != reproducibleSeed("coffee") || reproducibleSeed("coffee") == reproducibleSeed("tea") {
		t.Fatal("query-derived seed is not reproducible/query-specific")
	}
}

func TestThreeHostServicePassesKnobsCausallyInOrderedStages(t *testing.T) {
	var order []string
	expander := fakePlanExpander{run: func() (QueryPlan, error) {
		order = append(order, "expansion")
		return QueryPlan{RawQuery: "coffee", Preferred: []Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}}, nil
	}}
	matcher := fakeMatcher{run: func(plan QueryPlan) (EligibilityResult, error) {
		order = append(order, "matching")
		return EligibilityResult{Candidates: []CandidateEvidence{{Slug: "coffee", Title: "Coffee", Eligible: true, Score: 1}}}, nil
	}}
	selector := fakeSelector{run: func(input SelectionInput) (SelectionResult, error) {
		if input.Limit != 3 || input.ExplorationSlots != 2 || input.Seed != 17 {
			t.Fatalf("selection input=%#v", input)
		}
		order = append(order, "selection")
		return SelectionResult{Selected: []SelectedCandidate{{Slug: "coffee", Title: "Coffee", Selected: true, Reason: "selected", Score: 1}}}, nil
	}}
	seed := int64(17)
	service := newThreeHostServiceWithOptions(expander, matcher, selector, nil, threeHostOptions{selectionLimit: 3, explorationSlots: 2, seed: &seed})
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	result, trace, err := service.ExecuteWithTrace(context.Background(), newSnapshotReader(root), query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil || strings.Join(order, ",") != "expansion,matching,selection" || len(result.Results) != 1 || trace.Seed != 17 {
		t.Fatalf("order=%v result=%#v trace=%#v err=%v", order, result, trace, err)
	}
}

func TestProductionIgnoresThreeHostKnobsAndKeepsOutputContract(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
	writeTestFile(t, casesPath, `{"id":"a","query":"coffee","mode":"wiki"}`+"\n")
	var output bytes.Buffer
	threeHostCalled := false
	err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, casesPath: casesPath, runs: 1, selectionLimit: -1, explorationSlots: -1, explorationSlotsSet: true, seed: int64Ptr(-7)}, dependencies{
		loadConfig:  func(string) (config.Config, error) { return config.Config{}, nil },
		newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) { return &recordingExecutor{}, nil },
		newThreeHostExecutor: func(*cache.Cache, config.Config, threeHostOptions) (query.Executor, error) {
			threeHostCalled = true
			return nil, nil
		},
		now: time.Now, stdout: &output,
	})
	if err != nil || threeHostCalled || strings.Contains(output.String(), "three_host_trace") {
		t.Fatalf("production err=%v three_host_called=%v output=%s", err, threeHostCalled, output.String())
	}
}

func TestThreeHostTraceIsOrderedAndPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "frozen-project")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"secret","title":"Safe Title","body":"private body secret"}`+"\n")
	casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
	writeTestFile(t, casesPath, `{"id":"one","query":"secret","mode":"wiki"}`+"\n")
	var output bytes.Buffer
	if err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, casesPath: casesPath, runs: 1, service: serviceThreeHost, selectionLimit: 1, explorationSlots: 0, explorationSlotsSet: true, seed: int64Ptr(42)}, dependencies{
		loadConfig: func(string) (config.Config, error) { return config.Config{}, nil }, newThreeHostExecutor: newThreeHostExecutor, now: time.Now, stdout: &output,
	}); err != nil {
		t.Fatal(err)
	}
	var record resultRecord
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.ThreeHostTrace == nil || len(record.ThreeHostTrace.Stages) != 3 || record.ThreeHostTrace.Seed != 42 {
		t.Fatalf("trace=%#v, want three ordered stages and explicit seed", record.ThreeHostTrace)
	}
	got := []string{record.ThreeHostTrace.Stages[0].Name, record.ThreeHostTrace.Stages[1].Name, record.ThreeHostTrace.Stages[2].Name}
	if !equalStrings(got, []string{"expansion", "matching", "selection"}) || record.ThreeHostTrace.Stages[0].Source != "deterministic-fallback" {
		t.Fatalf("stage order/source=%v/%q", got, record.ThreeHostTrace.Stages[0].Source)
	}
	if strings.Contains(output.String(), "private body") || strings.Contains(output.String(), root) || strings.Contains(output.String(), "{bad") {
		t.Fatalf("trace leaked private/provider data: %s", output.String())
	}
}

func TestRecordsKeepDigestAndNoBodiesOrSecrets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opaque-snapshot")
	corpus := []byte(`{"slug":"coffee","title":"Coffee","body":"private concept body"}` + "\n")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), string(corpus))
	casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
	writeTestFile(t, casesPath, `{"id":"a","query":"coffee","mode":"wiki"}`+"\n")
	secret := "do-not-output-this-key"
	var output bytes.Buffer
	if err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, casesPath: casesPath, runs: 1}, dependencies{
		loadConfig:  func(string) (config.Config, error) { return config.Config{DeepSeekAPIKey: secret}, nil },
		newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) { return &recordingExecutor{}, nil }, now: time.Now, stdout: &output,
	}); err != nil {
		t.Fatal(err)
	}
	var record resultRecord
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(corpus)
	if record.CorpusSHA256 != fmt.Sprintf("%x", digest) || strings.Contains(output.String(), root) || strings.Contains(output.String(), secret) || strings.Contains(output.String(), "private concept body") {
		t.Fatalf("record leaked data or has wrong digest: %s", output.String())
	}
}

type recordingSemanticEvaluator struct{ calls []string }

func (e *recordingSemanticEvaluator) Evaluate(_ context.Context, criterion Criterion, _ cache.Entry) (semanticDecision, error) {
	e.calls = append(e.calls, criterion.Kind)
	return semanticDecision{Outcome: "pass"}, nil
}

type failingChatProvider struct{}

func (failingChatProvider) Chat(context.Context, string, string) (string, error) {
	return "", errors.New("provider unavailable")
}

type fixedChatProvider struct{ response string }

func (p fixedChatProvider) Chat(context.Context, string, string) (string, error) {
	return p.response, nil
}

type recordingChatProvider struct {
	responses []string
	prompts   []string
}

func (p *recordingChatProvider) Chat(_ context.Context, _, user string) (string, error) {
	p.prompts = append(p.prompts, user)
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

type fakePlanExpander struct{ run func() (QueryPlan, error) }

func (f fakePlanExpander) ExpandPlan(context.Context, string, CriterionPolicy, []cache.Entry) (QueryPlan, error) {
	return f.run()
}

type fakeMatcher struct {
	run func(QueryPlan) (EligibilityResult, error)
}

func (f fakeMatcher) Match(_ context.Context, plan QueryPlan, _ []cache.Entry) (EligibilityResult, error) {
	return f.run(plan)
}

type fakeSelector struct {
	run func(SelectionInput) (SelectionResult, error)
}

func (f fakeSelector) Select(_ context.Context, input SelectionInput) (SelectionResult, error) {
	return f.run(input)
}

type recordingExecutor struct{ calls []string }

func (e *recordingExecutor) Execute(_ context.Context, _ cache.Reader, request query.Request) (query.Result, error) {
	e.calls = append(e.calls, request.Query+":"+request.Mode)
	return query.Result{Query: request.Query, Mode: request.Mode, Results: []search.Result{{Slug: "coffee", Title: "Coffee", Type: "concept", Snippet: "private body"}}, Expand: &llm.ExpandResult{Keywords: []string{"coffee"}}}, nil
}

func int64Ptr(value int64) *int64 { return &value }

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
