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
	serviceProduction           = "production"
	serviceQueryRetrieval       = "query-retrieval"
	serviceQueryRetrievalLegacy = "three-host"
	defaultLimit                = queryquality.DefaultSelectionLimit
	maxSelectionLimit           = 1000
	semanticRequiredFailClosed  = true
	semanticExcludedFailClosed  = true
)

type queryRetrievalOptions struct {
	selectionLimit        int
	explorationSlots      int
	evidenceThreshold     int
	evidenceThresholdSet  bool
	keywordsPerAttempt    int
	expansionAttempts     int
	rareDocumentFrequency int
	seed                  *int64
	seedFor               func(string) int64
}

func defaultQueryRetrievalOptions() queryRetrievalOptions {
	return queryRetrievalOptions{selectionLimit: defaultLimit, explorationSlots: 1, evidenceThreshold: queryquality.DefaultEvidenceThreshold, keywordsPerAttempt: queryquality.DefaultKeywordsPerAttempt, expansionAttempts: queryquality.DefaultExpansionAttempts, rareDocumentFrequency: queryquality.DefaultRareDocumentFrequency}
}

func normalizeQueryRetrievalOptions(options queryRetrievalOptions) (queryRetrievalOptions, error) {
	core, err := queryquality.NormalizeOptions(toQueryRetrievalCoreOptions(options))
	if err != nil {
		return queryRetrievalOptions{}, err
	}
	return fromQueryRetrievalCoreOptions(core), nil
}

func toQueryRetrievalCoreOptions(options queryRetrievalOptions) queryquality.Options {
	return queryquality.Options{SelectionLimit: options.selectionLimit, ExplorationSlots: options.explorationSlots, EvidenceThreshold: options.evidenceThreshold, EvidenceThresholdSet: options.evidenceThresholdSet, KeywordsPerAttempt: options.keywordsPerAttempt, ExpansionAttempts: options.expansionAttempts, RareDocumentFrequency: options.rareDocumentFrequency, Seed: options.seed, SeedFor: options.seedFor}
}

func fromQueryRetrievalCoreOptions(options queryquality.Options) queryRetrievalOptions {
	return queryRetrievalOptions{selectionLimit: options.SelectionLimit, explorationSlots: options.ExplorationSlots, evidenceThreshold: options.EvidenceThreshold, evidenceThresholdSet: options.EvidenceThresholdSet, keywordsPerAttempt: options.KeywordsPerAttempt, expansionAttempts: options.ExpansionAttempts, rareDocumentFrequency: options.RareDocumentFrequency, seed: options.Seed, seedFor: options.SeedFor}
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
type queryExpander = queryquality.QueryExpander
type chatProvider = queryquality.ChatProvider
type semanticDecision = queryquality.SemanticDecision
type semanticEvaluator = queryquality.SemanticEvaluator
type candidateMatcher = queryquality.CandidateMatcher
type resultSelector = queryquality.ResultSelector
type queryRetrievalTrace = queryquality.Trace
type stageTrace = queryquality.StageTrace

func defaultCriterionPolicy() CriterionPolicy {
	return queryquality.DefaultRetrievalProfile().CriterionPolicy
}

type expansionInfo struct {
	source         string
	fallbackReason string
}

type queryPlanAdapter struct{ delegate queryquality.QueryExpander }

func newQueryPlanExpander(provider chatProvider, fallback queryExpander) queryExpander {
	return queryPlanAdapter{delegate: queryquality.NewStructuredPlanExpander(provider, fallback)}
}

func (e queryPlanAdapter) Expand(ctx context.Context, request queryquality.ExpansionRequest) (QueryPlan, error) {
	return e.delegate.Expand(ctx, request)
}

func (e queryPlanAdapter) ExpandWithTrace(ctx context.Context, request queryquality.ExpansionRequest) (QueryPlan, expansionInfo, error) {
	traced, ok := e.delegate.(queryquality.TracedQueryExpander)
	if !ok {
		plan, err := e.delegate.Expand(ctx, request)
		return plan, expansionInfo{}, err
	}
	plan, info, err := traced.ExpandWithTrace(ctx, request)
	return plan, expansionInfo{source: info.Source, fallbackReason: info.FallbackReason}, err
}

func newDeterministicExpander() queryExpander { return queryquality.NewDeterministicExpander() }
func newLexicalMatcher(semantic semanticEvaluator) candidateMatcher {
	return queryquality.NewLexicalMatcher(semantic)
}
func newRandomSelector() resultSelector { return queryquality.NewResultSelector() }

func newQueryRetrievalPipeline(expander queryExpander, matcher candidateMatcher, selector resultSelector, seedFor func(string) int64) *queryquality.QueryRetrievalPipeline {
	return queryquality.NewQueryRetrievalPipeline(expander, matcher, selector, seedFor)
}

func newQueryRetrievalPipelineWithOptions(expander queryExpander, matcher candidateMatcher, selector resultSelector, seedFor func(string) int64, options queryRetrievalOptions) *queryquality.QueryRetrievalPipeline {
	return queryquality.NewQueryRetrievalPipelineWithOptions(expander, matcher, selector, seedFor, toQueryRetrievalCoreOptions(options))
}

func newQueryRetrievalExecutor(conceptCache *cache.Cache, cfg config.Config, options queryRetrievalOptions) (query.Executor, error) {
	var provider chatProvider
	if client := llm.NewClient(cfg.DeepSeekAPIKey); client != nil {
		provider = client
	}
	return queryquality.NewExperimentExecutor(conceptCache, provider, toQueryRetrievalCoreOptions(options))
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
