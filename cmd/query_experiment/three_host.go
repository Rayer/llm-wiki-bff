package main

import (
	"context"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

const (
	serviceProduction          = "production"
	serviceThreeHost           = "three-host"
	defaultLimit               = queryquality.DefaultSelectionLimit
	maxSelectionLimit          = 1000
	semanticRequiredFailClosed = true
	semanticExcludedFailClosed = true
)

type threeHostOptions struct {
	selectionLimit   int
	explorationSlots int
	seed             *int64
	seedFor          func(string) int64
}

func defaultThreeHostOptions() threeHostOptions {
	return threeHostOptions{selectionLimit: defaultLimit, explorationSlots: 1}
}

func normalizeThreeHostOptions(options threeHostOptions) (threeHostOptions, error) {
	core, err := queryquality.NormalizeOptions(toCoreOptions(options))
	if err != nil {
		return threeHostOptions{}, err
	}
	return fromCoreOptions(core), nil
}

func toCoreOptions(options threeHostOptions) queryquality.Options {
	return queryquality.Options{SelectionLimit: options.selectionLimit, ExplorationSlots: options.explorationSlots, Seed: options.seed, SeedFor: options.seedFor}
}

func fromCoreOptions(options queryquality.Options) threeHostOptions {
	return threeHostOptions{selectionLimit: options.SelectionLimit, explorationSlots: options.ExplorationSlots, seed: options.Seed, seedFor: options.SeedFor}
}

type CriterionPolicy = queryquality.CriterionPolicy
type Criterion = queryquality.Criterion
type QueryPlan = queryquality.QueryPlan
type FieldEvidence = queryquality.FieldEvidence
type GroupEvidence = queryquality.GroupEvidence
type CandidateEvidence = queryquality.CandidateEvidence
type EligibilityResult = queryquality.EligibilityResult
type SelectionInput = queryquality.SelectionInput
type SelectedCandidate = queryquality.SelectedCandidate
type SelectionResult = queryquality.SelectionResult
type planExpander = queryquality.PlanExpander
type chatProvider = queryquality.ChatProvider
type semanticDecision = queryquality.SemanticDecision
type semanticEvaluator = queryquality.SemanticEvaluator
type eligibilityMatcher = queryquality.EligibilityMatcher
type candidateSelector = queryquality.CandidateSelector
type threeHostTrace = queryquality.Trace
type stageTrace = queryquality.StageTrace

var defaultCriterionPolicy = queryquality.DefaultCriterionPolicy

type expansionInfo struct {
	source         string
	fallbackReason string
}

type tracedPlanExpander interface {
	ExpandPlanWithTrace(context.Context, string, CriterionPolicy, []cache.Entry) (QueryPlan, expansionInfo, error)
}

type structuredPlanExpander struct{ delegate queryquality.PlanExpander }

func newStructuredPlanExpander(provider chatProvider, fallback planExpander) planExpander {
	return structuredPlanExpander{delegate: queryquality.NewStructuredPlanExpander(provider, fallback)}
}

func (e structuredPlanExpander) ExpandPlan(ctx context.Context, raw string, policy CriterionPolicy, entries []cache.Entry) (QueryPlan, error) {
	return e.delegate.ExpandPlan(ctx, raw, policy, entries)
}

func (e structuredPlanExpander) ExpandPlanWithTrace(ctx context.Context, raw string, policy CriterionPolicy, entries []cache.Entry) (QueryPlan, expansionInfo, error) {
	traced, ok := e.delegate.(queryquality.TracedPlanExpander)
	if !ok {
		plan, err := e.delegate.ExpandPlan(ctx, raw, policy, entries)
		return plan, expansionInfo{}, err
	}
	plan, info, err := traced.ExpandPlanWithTrace(ctx, raw, policy, entries)
	return plan, expansionInfo{source: info.Source, fallbackReason: info.FallbackReason}, err
}

func newDeterministicExpander() planExpander { return queryquality.NewDeterministicExpander() }
func newLexicalMatcher(semantic semanticEvaluator) eligibilityMatcher {
	return queryquality.NewLexicalMatcher(semantic)
}
func newRandomSelector() candidateSelector { return queryquality.NewSelector() }

func newThreeHostService(expander planExpander, matcher eligibilityMatcher, selector candidateSelector, seedFor func(string) int64) *queryquality.Service {
	return queryquality.NewService(expander, matcher, selector, seedFor)
}

func newThreeHostServiceWithOptions(expander planExpander, matcher eligibilityMatcher, selector candidateSelector, seedFor func(string) int64, options threeHostOptions) *queryquality.Service {
	return queryquality.NewServiceWithOptions(expander, matcher, selector, seedFor, toCoreOptions(options))
}

func newThreeHostExecutor(conceptCache *cache.Cache, cfg config.Config, options threeHostOptions) (query.Executor, error) {
	var provider chatProvider
	if client := llm.NewClient(cfg.DeepSeekAPIKey); client != nil {
		provider = client
	}
	return queryquality.NewExperimentExecutor(conceptCache, provider, toCoreOptions(options))
}

func decodeStructuredPlan(response, raw string) (QueryPlan, error) {
	return queryquality.DecodePlan(response, raw)
}

func validateQueryPlan(plan QueryPlan) error { return queryquality.ValidateQueryPlan(plan) }

func reproducibleSeed(query string) int64              { return queryquality.ReproducibleSeed(query) }
func planOutcome(plan QueryPlan) string                { return queryquality.PlanOutcome(plan) }
func criterionCount(plan QueryPlan) int                { return queryquality.CriterionCount(plan) }
func eligibleCount(candidates []CandidateEvidence) int { return queryquality.EligibleCount(candidates) }
func containsString(values []string, want string) bool {
	return queryquality.ContainsString(values, want)
}
