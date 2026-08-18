package queryquality_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func TestNormalizeQueryPlanBoundsPositiveKeywordsWithoutDroppingConstraints(t *testing.T) {
	plan := queryquality.QueryPlan{
		Required:  []queryquality.Criterion{{Kind: "location", Value: "Taipei", Terms: []string{"Taipei", "north"}, Proof: "lexical"}},
		Excluded:  []queryquality.Criterion{{Kind: "exclusion", Value: "smoking", Terms: []string{"smoking", "bar"}, Proof: "lexical"}},
		Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{" Cafe ", "cafe", "espresso"}, Proof: "lexical"}},
		Goals:     []queryquality.Criterion{{Kind: "goal", Value: "work", Terms: []string{"quiet", "desk"}, Proof: "lexical"}},
	}
	got, err := queryquality.NormalizeQueryPlan(plan, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Required, plan.Required) || !reflect.DeepEqual(got.Excluded, plan.Excluded) {
		t.Fatalf("constraints changed: got required=%#v excluded=%#v", got.Required, got.Excluded)
	}
	positiveCount := 0
	for _, criterion := range append(got.Preferred, got.Goals...) {
		positiveCount += len(criterion.Terms)
	}
	if positiveCount != 2 {
		t.Fatalf("positive terms = %#v %#v, want exactly two normalized terms", got.Preferred, got.Goals)
	}
	for _, criterion := range append(got.Preferred, got.Goals...) {
		if len(criterion.Terms) == 0 {
			t.Fatalf("normalization created empty lexical criterion: %#v", criterion)
		}
	}
}

func TestParallelExpansionStartsEveryAttemptBeforeRelease(t *testing.T) {
	provider := newBarrierExpander(3)
	expander, err := queryquality.NewParallelQueryExpander(provider, queryquality.NewDeterministicExpander(), queryquality.Options{ExpansionAttempts: 3, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := expander.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee"})
		done <- err
	}()
	select {
	case <-provider.allStarted:
	case <-time.After(time.Second):
		t.Fatal("not all expansion attempts started before release")
	}
	provider.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("parallel expansion did not finish")
	}
}

func TestParallelExpansionAggregationIsStableAcrossCompletionOrder(t *testing.T) {
	first := newOrderedExpander(false)
	second := newOrderedExpander(true)
	left := runParallel(t, first)
	right := runParallel(t, second)
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("completion order changed aggregate: left=%#v right=%#v", left, right)
	}
	if len(left.KeywordSupport) != 2 || left.KeywordSupport[0].SupportCount != 2 || left.KeywordSupport[1].SupportCount != 1 {
		t.Fatalf("keyword support = %#v", left.KeywordSupport)
	}
}

func TestParallelExpansionRepeatedAliasesWithinAttemptCountOnce(t *testing.T) {
	provider := &aliasExpander{}
	parallel, err := queryquality.NewParallelQueryExpander(provider, nil, queryquality.Options{ExpansionAttempts: 2, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := parallel.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.KeywordSupport) != 1 || plan.KeywordSupport[0].SupportCount != 2 || !reflect.DeepEqual(plan.KeywordSupport[0].AttemptIndexes, []int{1, 2}) {
		t.Fatalf("alias support = %#v, want one vote per attempt", plan.KeywordSupport)
	}
}

func TestParallelExpansionCancellationCancelsEveryAttempt(t *testing.T) {
	provider := newBarrierExpander(3)
	expander, err := queryquality.NewParallelQueryExpander(provider, nil, queryquality.Options{ExpansionAttempts: 3, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := expander.Expand(ctx, queryquality.ExpansionRequest{Query: "coffee"})
		done <- err
	}()
	select {
	case <-provider.allStarted:
	case <-time.After(time.Second):
		t.Fatal("attempts did not start")
	}
	cancel()
	select {
	case <-provider.allCanceled:
	case <-time.After(time.Second):
		t.Fatal("not every attempt observed cancellation")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled expansion did not return promptly")
	}
}

func TestParallelExpansionPartialAndAllFailureFallbackSemantics(t *testing.T) {
	partial := &failureExpander{fail: map[int]bool{1: true}}
	expander, err := queryquality.NewParallelQueryExpander(partial, queryquality.NewDeterministicExpander(), queryquality.Options{ExpansionAttempts: 3, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	plan, info, err := expander.(queryquality.TracedQueryExpander).ExpandWithTrace(context.Background(), queryquality.ExpansionRequest{Query: "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	if info.SuccessfulAttempts != 2 || info.ProviderFailedAttempts != 1 || info.FallbackCount != 0 || plan.Fallback {
		t.Fatalf("partial result=%#v info=%#v", plan, info)
	}

	allFailed := &failureExpander{fail: map[int]bool{1: true, 2: true, 3: true}}
	expander, err = queryquality.NewParallelQueryExpander(allFailed, queryquality.NewDeterministicExpander(), queryquality.Options{ExpansionAttempts: 3, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	plan, info, err = expander.(queryquality.TracedQueryExpander).ExpandWithTrace(context.Background(), queryquality.ExpansionRequest{Query: "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Fallback || info.FallbackCount != 1 || len(plan.KeywordSupport) != 0 {
		t.Fatalf("fallback result=%#v info=%#v", plan, info)
	}
}

func TestParallelExpansionPromptReceivesConfiguredKeywordMaximum(t *testing.T) {
	provider := &captureChatProvider{response: `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}
	expander, err := queryquality.NewParallelMinimalStructuredPlanExpander(provider, nil, queryquality.Options{ExpansionAttempts: 1, KeywordsPerAttempt: 12})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expander.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provider.user, "12") || !strings.Contains(strings.ToLower(provider.user), "maximum") {
		t.Fatalf("prompt = %q, want configured positive-keyword maximum", provider.user)
	}
}

func TestConsensusAndRareQualificationPaths(t *testing.T) {
	plan := queryquality.QueryPlan{
		RawQuery:       "rare cafe",
		Preferred:      []queryquality.Criterion{{Kind: "topic", Value: "rare", Terms: []string{"rare"}, Proof: "lexical"}},
		KeywordSupport: []queryquality.KeywordSupport{{Role: "preferred", Kind: "topic", Value: "rare", Keyword: "rare", SupportCount: 2, AttemptIndexes: []int{1, 2}}},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "consensus", Title: "Rare Cafe", Body: ""}, {Slug: "other", Title: "Other", Body: ""}}, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !matched.Candidates[0].Qualified || matched.Candidates[0].QualificationPath != "keyword_consensus" || matched.Candidates[0].KeywordEvidence[0].SupportCount != 2 {
		t.Fatalf("consensus candidate=%#v", matched.Candidates[0])
	}

	plan.KeywordSupport = []queryquality.KeywordSupport{{Role: "preferred", Kind: "topic", Value: "rare", Keyword: "rare", SupportCount: 1, AttemptIndexes: []int{1}}}
	matched, err = queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "title", Title: "Rare Cafe", Body: ""}, {Slug: "other", Title: "Other", Body: ""}}, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1,
	})
	if err != nil || !matched.Candidates[0].Qualified || matched.Candidates[0].QualificationPath != "rare_discriminative_lexical" {
		t.Fatalf("rare title candidate=%#v err=%v", matched.Candidates[0], err)
	}

	plan.KeywordSupport = []queryquality.KeywordSupport{{Role: "preferred", Kind: "topic", Value: "rare", Keyword: "common", SupportCount: 1, AttemptIndexes: []int{1}}}
	plan.Preferred[0].Terms = []string{"common"}
	matched, err = queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "one", Title: "Common Cafe", Body: ""}, {Slug: "two", Title: "Common Place", Body: ""}}, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1,
	})
	if err != nil || matched.Candidates[0].Qualified {
		t.Fatalf("common single-vote candidate=%#v err=%v, want below threshold", matched.Candidates[0], err)
	}

	matched, err = queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "body", Title: "Cafe", Body: "rare"}}, EvidenceThreshold: 2, EvidenceThresholdSet: true, RareKeywordMaxDocumentFrequency: 1,
	})
	if err != nil || matched.Candidates[0].Qualified || matched.Candidates[0].QualificationPath == "rare_discriminative_lexical" {
		t.Fatalf("rare body candidate=%#v err=%v", matched.Candidates[0], err)
	}
}

func runParallel(t *testing.T, expander queryquality.QueryExpander) queryquality.QueryPlan {
	t.Helper()
	parallel, err := queryquality.NewParallelQueryExpander(expander, nil, queryquality.Options{ExpansionAttempts: 3, KeywordsPerAttempt: 24})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := parallel.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee"})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

type barrierExpander struct {
	allStarted  chan struct{}
	allCanceled chan struct{}
	releaseOnce sync.Once
	started     chan struct{}
	canceled    chan struct{}
	releaseCh   chan struct{}
	count       int
}

func newBarrierExpander(count int) *barrierExpander {
	return &barrierExpander{allStarted: make(chan struct{}), allCanceled: make(chan struct{}), started: make(chan struct{}, count), canceled: make(chan struct{}, count), releaseCh: make(chan struct{}), count: count}
}
func (e *barrierExpander) Expand(ctx context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	e.started <- struct{}{}
	if len(e.started) == e.count {
		close(e.allStarted)
	}
	select {
	case <-ctx.Done():
		e.canceled <- struct{}{}
		if len(e.canceled) == e.count {
			close(e.allCanceled)
		}
		return queryquality.QueryPlan{}, ctx.Err()
	case <-e.releaseCh:
		return queryquality.QueryPlan{RawQuery: request.Query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}}, nil
	}
}
func (e *barrierExpander) release() { e.releaseOnce.Do(func() { close(e.releaseCh) }) }

type orderedExpander struct {
	reverse    bool
	started    chan struct{}
	allStarted chan struct{}
	startOnce  sync.Once
	firstDone  chan struct{}
	secondDone chan struct{}
	thirdDone  chan struct{}
}

func newOrderedExpander(reverse bool) *orderedExpander {
	return &orderedExpander{reverse: reverse, started: make(chan struct{}, 3), allStarted: make(chan struct{}), firstDone: make(chan struct{}), secondDone: make(chan struct{}), thirdDone: make(chan struct{})}
}

func (e *orderedExpander) Expand(ctx context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	e.started <- struct{}{}
	if len(e.started) == 3 {
		e.startOnce.Do(func() { close(e.allStarted) })
	}
	select {
	case <-ctx.Done():
		return queryquality.QueryPlan{}, ctx.Err()
	case <-e.allStarted:
	}
	if e.reverse {
		switch request.Attempt {
		case 3:
			close(e.secondDone)
		case 2:
			<-e.secondDone
			close(e.firstDone)
		case 1:
			<-e.firstDone
		}
	} else {
		switch request.Attempt {
		case 1:
			close(e.secondDone)
		case 2:
			<-e.secondDone
			close(e.thirdDone)
		case 3:
			<-e.thirdDone
		}
	}
	terms := []string(nil)
	if request.Attempt != 3 {
		terms = []string{"common"}
	}
	if request.Attempt == 1 {
		terms = append(terms, "first")
	}
	return queryquality.QueryPlan{RawQuery: request.Query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: terms, Proof: "lexical"}}}, nil
}

type failureExpander struct{ fail map[int]bool }

func (e *failureExpander) Expand(_ context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	if e.fail[request.Attempt] {
		return queryquality.QueryPlan{}, errors.New("provider failed")
	}
	return queryquality.QueryPlan{RawQuery: request.Query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}}, nil
}

type aliasExpander struct{}

func (*aliasExpander) Expand(_ context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	return queryquality.QueryPlan{RawQuery: request.Query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"Coffee", " coffee "}, Proof: "lexical"}}}, nil
}

type captureChatProvider struct {
	user     string
	response string
}

func (p *captureChatProvider) Chat(_ context.Context, _ string, user string) (string, error) {
	p.user = user
	return p.response, nil
}
