package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestDeterministicFallbackIsGenericAndOptionalEvidenceDoesNotGate(t *testing.T) {
	for _, raw := range []string{"台北適合工作的咖啡廳", "小孩想玩水，有什麼室外戲水景點嗎", "unrelated astronomy query"} {
		plan, err := newDeterministicExpander().Expand(context.Background(), queryquality.ExpansionRequest{Query: raw, CriterionPolicy: defaultCriterionPolicy})
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
	got, err := newLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "taipei", Title: "Taipei", Body: ""}}})
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
	got, err := newLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: "candidate", Body: "private required private excluded private preferred private goal private supporting private alternative"}}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := got.Candidates[0]
	if candidate.Eligible || candidate.Score != 0 || candidate.SemanticOutcome != "unresolved" {
		t.Fatalf("candidate = %#v, want required unavailable and optional semantics unscored", candidate)
	}
	for _, group := range candidate.Groups {
		if len(group.Matches) != 0 || group.SemanticOutcome != "unresolved" {
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
	got, err := newLexicalMatcher(evaluator).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: "candidate"}}})
	if err != nil || got.Candidates[0].Eligible || got.Candidates[0].Score != 0 || len(evaluator.calls) != 6 {
		t.Fatalf("candidate=%#v calls=%v err=%v", got.Candidates[0], evaluator.calls, err)
	}
	for _, group := range got.Candidates[0].Groups {
		if group.SemanticOutcome != "matched" || len(group.Matches) != 0 {
			t.Fatalf("semantic group=%#v, want evaluator outcome without lexical evidence", group)
		}
	}
}

func TestStructuredExpansionUsesOneCallAndSimpleFallback(t *testing.T) {
	provider := &recordingChatProvider{responses: []string{`{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}}
	expander := newQueryPlanExpander(provider, newDeterministicExpander()).(queryPlanAdapter)
	plan, info, err := expander.ExpandWithTrace(context.Background(), queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: defaultCriterionPolicy})
	if err != nil || plan.Fallback || info.source != "structured-llm" || len(provider.prompts) != 1 {
		t.Fatalf("plan=%#v info=%#v calls=%d err=%v", plan, info, len(provider.prompts), err)
	}
	for _, response := range []string{`{"unknown":1}`, `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false} {}`} {
		provider := fixedChatProvider{response: response}
		plan, info, err := newQueryPlanExpander(provider, newDeterministicExpander()).(queryPlanAdapter).ExpandWithTrace(context.Background(), queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: defaultCriterionPolicy})
		if err != nil || !plan.Fallback || info.source != "deterministic-fallback" || info.fallbackReason != "invalid_plan" {
			t.Fatalf("response=%q plan=%#v info=%#v err=%v", response, plan, info, err)
		}
	}
}

func TestSelectionReplaysSeedAndHonorsZeroOneAndMultipleExplorationSlots(t *testing.T) {
	candidates := []CandidateEvidence{
		{Slug: "one", Title: "One", Eligible: true, Qualified: true, Score: 5}, {Slug: "two", Title: "Two", Eligible: true, Qualified: true, Score: 4},
		{Slug: "three", Title: "Three", Eligible: true, Qualified: true, Score: 3}, {Slug: "four", Title: "Four", Eligible: true, Qualified: true, Score: 2},
		{Slug: "five", Title: "Five", Eligible: true, Qualified: true, Score: 1},
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

func TestQueryRetrievalOptionBoundsAndQueryDerivedSeed(t *testing.T) {
	if got, err := normalizeQueryRetrievalOptions(queryRetrievalOptions{}); err != nil || got.selectionLimit != defaultLimit || got.explorationSlots != 0 {
		t.Fatalf("zero options = %#v err=%v, want default limit and valid zero slots", got, err)
	}
	for _, options := range []queryRetrievalOptions{{selectionLimit: -1}, {selectionLimit: maxSelectionLimit + 1}, {selectionLimit: 3, explorationSlots: -1}, {selectionLimit: 3, explorationSlots: 4}} {
		if _, err := normalizeQueryRetrievalOptions(options); err == nil {
			t.Fatalf("options=%#v accepted, want validation error", options)
		}
	}
	legacy, err := normalizeQueryRetrievalOptions(queryRetrievalOptions{selectionLimit: 3, explorationSlots: 0, evidenceThreshold: 0, evidenceThresholdSet: true})
	if err != nil || legacy.evidenceThreshold != 0 || !legacy.evidenceThresholdSet {
		t.Fatalf("explicit legacy threshold = %#v err=%v, want preserved zero", legacy, err)
	}
	if _, err := normalizeQueryRetrievalOptions(queryRetrievalOptions{evidenceThreshold: -1, evidenceThresholdSet: true}); err == nil {
		t.Fatal("negative explicit evidence threshold accepted")
	}
	if reproducibleSeed("coffee") != reproducibleSeed("coffee") || reproducibleSeed("coffee") == reproducibleSeed("tea") {
		t.Fatal("query-derived seed is not reproducible/query-specific")
	}
}

func TestQueryRetrievalServicePassesKnobsCausallyInOrderedStages(t *testing.T) {
	var order []string
	expander := fakeQueryExpander{run: func() (QueryPlan, error) {
		order = append(order, "expansion")
		return QueryPlan{RawQuery: "coffee", Preferred: []Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}}, nil
	}}
	matcher := fakeMatcher{run: func(request queryquality.MatchRequest) (EligibilityResult, error) {
		order = append(order, "matching")
		if request.Plan.RawQuery != "coffee" || len(request.CorpusEntries) != 1 {
			t.Fatalf("match request=%#v", request)
		}
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
	service := newQueryRetrievalPipelineWithOptions(expander, matcher, selector, nil, queryRetrievalOptions{selectionLimit: 3, explorationSlots: 2, seed: &seed})
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	result, trace, err := service.ExecuteWithTrace(context.Background(), newSnapshotReader(root), query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil || strings.Join(order, ",") != "expansion,matching,selection" || len(result.Results) != 1 || trace.Seed != 17 {
		t.Fatalf("order=%v result=%#v trace=%#v err=%v", order, result, trace, err)
	}
}

func TestProductionIgnoresQueryRetrievalKnobsAndKeepsOutputContract(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
	writeTestFile(t, casesPath, `{"id":"a","query":"coffee","mode":"wiki"}`+"\n")
	var output bytes.Buffer
	queryRetrievalExecutorCalled := false
	err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, casesPath: casesPath, runs: 1, selectionLimit: -1, explorationSlots: -1, explorationSlotsSet: true, seed: int64Ptr(-7)}, dependencies{
		loadConfig:  func(string) (config.Config, error) { return config.Config{}, nil },
		newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) { return &recordingExecutor{}, nil },
		newQueryRetrievalExecutor: func(*cache.Cache, config.Config, queryRetrievalOptions) (query.Executor, error) {
			queryRetrievalExecutorCalled = true
			return nil, nil
		},
		now: time.Now, stdout: &output,
	})
	if err != nil || queryRetrievalExecutorCalled || strings.Contains(output.String(), "three_host_trace") {
		t.Fatalf("production err=%v query_retrieval_executor_called=%v output=%s", err, queryRetrievalExecutorCalled, output.String())
	}
}

func TestRunExperimentAppendsSuggestedCasesThroughCLIComposition(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	writeTestFile(t, filepath.Join(root, "cache", "suggested_queries.json"), string(validExperimentSuggestedQueries(t)))
	var output bytes.Buffer
	executor := &recordingExecutor{}
	err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, suggestedQueryMode: "wiki", runs: 1}, dependencies{
		loadConfig:  func(string) (config.Config, error) { return config.Config{}, nil },
		newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) { return executor, nil },
		now:         time.Now, stdout: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 20 || executor.calls[0] != "Question 1?:wiki" || executor.calls[19] != "Question 20?:wiki" {
		t.Fatalf("executor calls = %v", executor.calls)
	}
	var record resultRecord
	if err := json.Unmarshal(bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})[0], &record); err != nil {
		t.Fatal(err)
	}
	if record.SuggestedQueriesSHA256 == "" || record.Snapshot != filepath.Base(root) {
		t.Fatalf("record identity = %#v", record)
	}
}

func TestRunExperimentRejectsSuggestedArtifactBeforeExecutorOrOutput(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee"}`+"\n")
	writeTestFile(t, filepath.Join(root, "cache", "suggested_queries.json"), `{"version":1}`)
	var output bytes.Buffer
	called := false
	configCalled := false
	outputOpened := false
	err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, suggestedQueryMode: "wiki", runs: 1}, dependencies{
		loadConfig: func(string) (config.Config, error) { configCalled = true; return config.Config{}, nil },
		newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) {
			called = true
			return &recordingExecutor{}, nil
		},
		openOutput: func(string, io.Writer) (recordSink, error) { outputOpened = true; return nil, nil },
		now:        time.Now, stdout: &output,
	})
	if err == nil || called || configCalled || outputOpened || output.Len() != 0 {
		t.Fatalf("err=%v executor_called=%v config_called=%v output_opened=%v output=%q", err, called, configCalled, outputOpened, output.String())
	}
}

func TestQueryRetrievalTraceIsOrderedAndPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "frozen-project")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"secret","title":"Safe Title","body":"private body secret"}`+"\n")
	casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
	writeTestFile(t, casesPath, `{"id":"one","query":"secret","mode":"wiki"}`+"\n")
	var output bytes.Buffer
	if err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, casesPath: casesPath, runs: 1, service: serviceQueryRetrieval, selectionLimit: 1, explorationSlots: 0, explorationSlotsSet: true, seed: int64Ptr(42)}, dependencies{
		loadConfig: func(string) (config.Config, error) { return config.Config{}, nil }, newQueryRetrievalExecutor: newQueryRetrievalExecutor, now: time.Now, stdout: &output,
	}); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["three_host_trace"]; !ok {
		t.Fatalf("raw trace key absent, record=%s", output.String())
	}
	if _, ok := raw["query_retrieval_trace"]; ok {
		t.Fatalf("unexpected query_retrieval_trace key in output record=%s", output.String())
	}
	var record resultRecord
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.QueryRetrievalTrace == nil || len(record.QueryRetrievalTrace.Stages) != 3 || record.QueryRetrievalTrace.Seed != 42 {
		t.Fatalf("trace=%#v, want three ordered stages and explicit seed", record.QueryRetrievalTrace)
	}
	got := []string{record.QueryRetrievalTrace.Stages[0].Name, record.QueryRetrievalTrace.Stages[1].Name, record.QueryRetrievalTrace.Stages[2].Name}
	if !equalStrings(got, []string{"expansion", "matching", "selection"}) || record.QueryRetrievalTrace.Stages[0].Source != "deterministic-fallback" {
		t.Fatalf("stage order/source=%v/%q", got, record.QueryRetrievalTrace.Stages[0].Source)
	}
	if strings.Contains(output.String(), "private body") || strings.Contains(output.String(), root) || strings.Contains(output.String(), "{bad") {
		t.Fatalf("trace leaked private/provider data: %s", output.String())
	}
}

func TestQueryExperimentRunsRecordsAttemptTimings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
	writeTestFile(t, casesPath, `{"id":"a","query":"coffee","mode":"wiki"}`+"\n")
	run := func(executor query.Executor, received, completed time.Time) resultRecord {
		var output bytes.Buffer
		clock := scriptedNow(received, completed)
		if err := runExperiment(context.Background(), experimentOptions{snapshotPath: root, casesPath: casesPath, runs: 1, selectionLimit: 1}, dependencies{
			loadConfig:  func(string) (config.Config, error) { return config.Config{}, nil },
			newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) { return executor, nil },
			now:         clock, stdout: &output,
		}); err != nil {
			t.Fatal(err)
		}
		var record resultRecord
		if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	received := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	completed := received.Add(321 * time.Millisecond)
	record := run(&recordingExecutor{}, received, completed)
	if got, want := record.QueryReceivedAt, received.Format(time.RFC3339Nano); got != want {
		t.Fatalf("query_received_at=%q want=%q", got, want)
	}
	if got, want := record.RunCompletedAt, completed.Format(time.RFC3339Nano); got != want {
		t.Fatalf("run_completed_at=%q want=%q", got, want)
	}
	if record.DurationMS != 321 {
		t.Fatalf("duration_ms=%d want=%d", record.DurationMS, 321)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.QueryReceivedAt); err != nil {
		t.Fatalf("query_received_at parse=%v", err)
	}
	record = run(&recordingExecutorWithError{err: errors.New("boom")}, received, completed.Add(100*time.Millisecond))
	if record.ErrorStage != "execute" || record.DurationMS == 0 {
		t.Fatalf("record=%#v", record)
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

type fakeQueryExpander struct{ run func() (QueryPlan, error) }

func (f fakeQueryExpander) Expand(context.Context, queryquality.ExpansionRequest) (QueryPlan, error) {
	return f.run()
}

type fakeMatcher struct {
	run func(queryquality.MatchRequest) (EligibilityResult, error)
}

func (f fakeMatcher) Match(_ context.Context, request queryquality.MatchRequest) (EligibilityResult, error) {
	return f.run(request)
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

type recordingExecutorWithError struct{ err error }

func (e *recordingExecutorWithError) Execute(_ context.Context, _ cache.Reader, _ query.Request) (query.Result, error) {
	return query.Result{}, e.err
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

func scriptedNow(times ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		if index >= len(times) {
			return times[len(times)-1]
		}
		t := times[index]
		index++
		return t
	}
}
