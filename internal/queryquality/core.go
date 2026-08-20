package queryquality

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
	"golang.org/x/text/unicode/norm"
)

const (
	DefaultSelectionLimit        = 10
	DefaultEvidenceThreshold     = 2
	DefaultKeywordsPerAttempt    = 24
	DefaultExpansionAttempts     = 3
	DefaultRareDocumentFrequency = 1
	maxSelectionLimit            = 1000
	maxKeywordsPerAttempt        = 100
	maxExpansionAttempts         = 10
	maxRareDocumentFrequency     = 1000
	semanticRequiredFailClosed   = true
	semanticExcludedFailClosed   = true
)

const MinimumKeywordConsensusSupport int = 2

// ResultStatus returns the canonical status and reason for the selected result count.
func ResultStatus(count int) (string, string) {
	if count == 0 {
		return "insufficient_evidence", "no_qualified_evidence"
	}
	return "ok", "qualified_evidence"
}

type Options struct {
	SelectionLimit        int
	ExplorationSlots      int
	EvidenceThreshold     int
	EvidenceThresholdSet  bool
	KeywordsPerAttempt    int
	ExpansionAttempts     int
	RareDocumentFrequency int
	Seed                  *int64
	SeedFor               func(string) int64
}

func DefaultOptions() Options {
	return Options{SelectionLimit: DefaultSelectionLimit, ExplorationSlots: 1, EvidenceThreshold: DefaultEvidenceThreshold, KeywordsPerAttempt: DefaultKeywordsPerAttempt, ExpansionAttempts: DefaultExpansionAttempts, RareDocumentFrequency: DefaultRareDocumentFrequency}
}

func NormalizeOptions(options Options) (Options, error) {
	if options.SelectionLimit == 0 {
		options.SelectionLimit = DefaultSelectionLimit
	}
	if !options.EvidenceThresholdSet {
		options.EvidenceThreshold = DefaultEvidenceThreshold
	}
	if options.KeywordsPerAttempt == 0 {
		options.KeywordsPerAttempt = DefaultKeywordsPerAttempt
	}
	if options.ExpansionAttempts == 0 {
		options.ExpansionAttempts = DefaultExpansionAttempts
	}
	if options.RareDocumentFrequency == 0 {
		options.RareDocumentFrequency = DefaultRareDocumentFrequency
	}
	if options.EvidenceThreshold < 0 {
		return Options{}, errors.New("evidence threshold must not be negative")
	}
	if options.SelectionLimit < 1 || options.SelectionLimit > maxSelectionLimit {
		return Options{}, fmt.Errorf("selection limit must be between 1 and %d", maxSelectionLimit)
	}
	if options.ExplorationSlots < 0 || options.ExplorationSlots > options.SelectionLimit {
		return Options{}, fmt.Errorf("exploration slots must be between 0 and %d", options.SelectionLimit)
	}
	if options.KeywordsPerAttempt < 1 || options.KeywordsPerAttempt > maxKeywordsPerAttempt {
		return Options{}, fmt.Errorf("keywords per attempt must be between 1 and %d", maxKeywordsPerAttempt)
	}
	if options.ExpansionAttempts < 1 || options.ExpansionAttempts > maxExpansionAttempts {
		return Options{}, fmt.Errorf("expansion attempts must be between 1 and %d", maxExpansionAttempts)
	}
	if options.RareDocumentFrequency < 1 || options.RareDocumentFrequency > maxRareDocumentFrequency {
		return Options{}, fmt.Errorf("rare document frequency must be between 1 and %d", maxRareDocumentFrequency)
	}
	return options, nil
}

type CriterionPolicy struct {
	RequiredWhenExplicit []string `json:"required_when_explicit"`
	PreferredByDefault   []string `json:"preferred_by_default"`
	GoalsToExpand        []string `json:"goals_to_expand"`
}

var DefaultCriterionPolicy = CriterionPolicy{
	RequiredWhenExplicit: []string{"location", "explicit_exclusion"},
	PreferredByDefault:   []string{"venue_type", "activity", "audience", "setting"},
	GoalsToExpand:        []string{"suitability", "recommendation", "discovery"},
}

type Criterion struct {
	Kind  string   `json:"kind"`
	Value string   `json:"value"`
	Terms []string `json:"terms,omitempty"`
	Proof string   `json:"proof,omitempty"`
}

type QueryPlan struct {
	RawQuery               string           `json:"raw_query"`
	Required               []Criterion      `json:"required,omitempty"`
	Excluded               []Criterion      `json:"excluded,omitempty"`
	Preferred              []Criterion      `json:"preferred,omitempty"`
	Goals                  []Criterion      `json:"goals,omitempty"`
	SupportingDimensions   []Criterion      `json:"supporting_dimensions,omitempty"`
	AcceptableAlternatives []Criterion      `json:"acceptable_alternatives,omitempty"`
	Ambiguity              []string         `json:"ambiguity,omitempty"`
	KeywordSupport         []KeywordSupport `json:"keyword_support,omitempty"`
	Fallback               bool             `json:"fallback,omitempty"`
}

type KeywordSupport struct {
	Role           string        `json:"role"`
	Kind           string        `json:"kind"`
	Value          string        `json:"value"`
	Keyword        string        `json:"keyword"`
	SupportCount   int           `json:"support_count"`
	AttemptIndexes []int         `json:"attempt_indexes"`
	SurfaceForms   []SurfaceForm `json:"surface_forms,omitempty"`
}

type SurfaceForm struct {
	Value          string `json:"value"`
	AttemptIndexes []int  `json:"attempt_indexes"`
}

type FieldEvidence struct {
	Field string   `json:"field"`
	Terms []string `json:"terms,omitempty"`
}

type GroupEvidence struct {
	Role            string          `json:"role,omitempty"`
	Kind            string          `json:"kind"`
	Value           string          `json:"value"`
	Matches         []FieldEvidence `json:"matches,omitempty"`
	SemanticOutcome string          `json:"semantic_outcome,omitempty"`
}

type CandidateEvidence struct {
	Slug                       string            `json:"slug"`
	Title                      string            `json:"title"`
	Eligible                   bool              `json:"eligible"`
	Rejection                  string            `json:"rejection,omitempty"`
	Groups                     []GroupEvidence   `json:"groups,omitempty"`
	SemanticOutcome            string            `json:"semantic_outcome,omitempty"`
	SemanticResolution         string            `json:"semantic_resolution"`
	ExactIdentityEvidence      bool              `json:"exact_identity_evidence"`
	PositiveEvidenceDimensions []string          `json:"positive_evidence_dimensions,omitempty"`
	PositiveEvidenceCount      int               `json:"positive_evidence_count"`
	Qualified                  bool              `json:"qualified"`
	QualificationReason        string            `json:"qualification_reason"`
	QualificationPath          string            `json:"qualification_path"`
	EvidenceThreshold          int               `json:"evidence_threshold"`
	KeywordEvidence            []KeywordEvidence `json:"keyword_evidence,omitempty"`
	MatchedFields              []string          `json:"matched_fields,omitempty"`
	Score                      int               `json:"score"`
}

type KeywordEvidence struct {
	Role              string        `json:"role"`
	Kind              string        `json:"kind"`
	Value             string        `json:"value"`
	Keyword           string        `json:"keyword"`
	SurfaceForms      []SurfaceForm `json:"surface_forms,omitempty"`
	SupportCount      int           `json:"support_count"`
	AttemptIndexes    []int         `json:"attempt_indexes"`
	DocumentFrequency int           `json:"document_frequency"`
	MatchedFields     []string      `json:"matched_fields"`
}

type EligibilityResult struct{ Candidates []CandidateEvidence }

type ExpansionRequest struct {
	Query              string
	CriterionPolicy    CriterionPolicy
	Attempt            int
	KeywordsPerAttempt int
}

type MatchRequest struct {
	Plan                            QueryPlan
	CorpusEntries                   []cache.Entry
	EvidenceThreshold               int
	EvidenceThresholdSet            bool
	RareKeywordMaxDocumentFrequency int
	FallbackQualificationAllowed    bool
}

type SelectionInput struct {
	Candidates       []CandidateEvidence
	Limit            int
	ExplorationSlots int
	Seed             int64
}

type SelectedCandidate struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Selected    bool   `json:"selected"`
	Reason      string `json:"reason"`
	Score       int    `json:"score"`
	Tier        string `json:"tier,omitempty"`
	Exploration bool   `json:"exploration,omitempty"`
}

type SelectionResult struct{ Selected []SelectedCandidate }

type QueryExpander interface {
	Expand(context.Context, ExpansionRequest) (QueryPlan, error)
}

type ChatProvider interface {
	Chat(context.Context, string, string) (string, error)
}

type ExpansionInfo struct {
	Source                 string
	FallbackReason         string
	RequestedAttempts      int
	SuccessfulAttempts     int
	ProviderFailedAttempts int
	FallbackCount          int
	KeywordsPerAttempt     int
	AttemptOutcomes        []ExpansionAttemptInfo
}

type ExpansionAttemptInfo struct {
	AttemptIndex int    `json:"attempt_index"`
	Outcome      string `json:"outcome"`
}

type TracedQueryExpander interface {
	ExpandWithTrace(context.Context, ExpansionRequest) (QueryPlan, ExpansionInfo, error)
}

type StructuredPlanExpander struct {
	provider   ChatProvider
	fallback   QueryExpander
	decodePlan func(string, string) (QueryPlan, error)
}

type ExpansionError struct {
	Reason string
	Err    error
}

func (e *ExpansionError) Error() string {
	if e.Err == nil {
		return "query expansion " + e.Reason
	}
	return "query expansion " + e.Reason + ": " + e.Err.Error()
}

func (e *ExpansionError) Unwrap() error { return e.Err }

func NewStructuredPlanExpander(provider ChatProvider, fallback QueryExpander) QueryExpander {
	return StructuredPlanExpander{provider: provider, fallback: fallback, decodePlan: DecodePlan}
}

func NewMinimalStructuredPlanExpander(provider ChatProvider, fallback QueryExpander) QueryExpander {
	return StructuredPlanExpander{provider: provider, fallback: fallback, decodePlan: DecodeMinimalV1Plan}
}

func (e StructuredPlanExpander) Expand(ctx context.Context, request ExpansionRequest) (QueryPlan, error) {
	plan, _, err := e.ExpandWithTrace(ctx, request)
	return plan, err
}

func (e StructuredPlanExpander) ExpandWithTrace(ctx context.Context, request ExpansionRequest) (QueryPlan, ExpansionInfo, error) {
	if err := ctx.Err(); err != nil {
		return QueryPlan{}, ExpansionInfo{}, err
	}
	decodePlan := e.decodePlan
	if decodePlan == nil {
		decodePlan = DecodePlan
	}
	if e.provider != nil {
		if client, ok := e.provider.(*llm.Client); ok && client.Reasoning() != llm.ReasoningNone {
			return QueryPlan{}, ExpansionInfo{}, errors.New("query expansion reasoning must be none")
		}
		limit := request.KeywordsPerAttempt
		if limit == 0 {
			limit = DefaultKeywordsPerAttempt
		}
		response, err := e.provider.Chat(ctx, structuredPlanSystemPrompt, structuredPlanUserPromptWithLimit(request.Query, request.CriterionPolicy, limit))
		if err == nil {
			if plan, decodeErr := decodePlan(response, request.Query); decodeErr == nil {
				plan, normalizeErr := NormalizeQueryPlan(plan, limit)
				if normalizeErr != nil {
					return QueryPlan{}, ExpansionInfo{}, normalizeErr
				}
				return plan, ExpansionInfo{Source: "structured-llm"}, nil
			}
			if e.fallback == nil {
				return QueryPlan{}, ExpansionInfo{}, &ExpansionError{Reason: "invalid_plan"}
			}
			return e.fallbackPlan(ctx, request, "invalid_plan")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return QueryPlan{}, ExpansionInfo{}, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return QueryPlan{}, ExpansionInfo{}, err
		}
		if e.fallback == nil {
			return QueryPlan{}, ExpansionInfo{}, &ExpansionError{Reason: "provider_error", Err: err}
		}
		return e.fallbackPlan(ctx, request, "provider_error")
	}
	if e.fallback == nil {
		return QueryPlan{}, ExpansionInfo{}, &ExpansionError{Reason: "provider_not_configured"}
	}
	return e.fallbackPlan(ctx, request, "provider_not_configured")
}

func (e StructuredPlanExpander) fallbackPlan(ctx context.Context, request ExpansionRequest, reason string) (QueryPlan, ExpansionInfo, error) {
	if err := ctx.Err(); err != nil {
		return QueryPlan{}, ExpansionInfo{}, err
	}
	if e.fallback == nil {
		return QueryPlan{}, ExpansionInfo{}, errors.New("fallback unavailable")
	}
	plan, err := e.fallback.Expand(ctx, request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return QueryPlan{}, ExpansionInfo{}, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return QueryPlan{}, ExpansionInfo{}, err
		}
		return QueryPlan{}, ExpansionInfo{}, fmt.Errorf("fallback: %w", err)
	}
	plan.RawQuery = request.Query
	plan.Fallback = true
	if err := validateDeterministicFallbackPlan(plan); err != nil {
		return QueryPlan{}, ExpansionInfo{}, fmt.Errorf("fallback validation: %w", err)
	}
	return plan, ExpansionInfo{Source: "deterministic-fallback", FallbackReason: reason}, nil
}

// NormalizeQueryPlan bounds only positive discovery terms. Required and
// excluded constraints remain complete and role-local.
func NormalizeQueryPlan(plan QueryPlan, keywordsPerAttempt int) (QueryPlan, error) {
	if keywordsPerAttempt < 1 || keywordsPerAttempt > maxKeywordsPerAttempt {
		return QueryPlan{}, fmt.Errorf("keywords per attempt must be between 1 and %d", maxKeywordsPerAttempt)
	}
	plan.Preferred, plan.Goals = normalizePositiveCriteriaWithLimit(plan.Preferred, plan.Goals, keywordsPerAttempt)
	return plan, nil
}

func normalizePositiveCriteriaWithLimit(preferred, goals []Criterion, limit int) ([]Criterion, []Criterion) {
	seen := make(map[string]struct{})
	used := 0
	normalize := func(criteria []Criterion) []Criterion {
		result := make([]Criterion, 0, len(criteria))
		for _, criterion := range criteria {
			if criterion.Proof == "semantic" {
				result = append(result, criterion)
				continue
			}
			terms := make([]string, 0, len(criterion.Terms))
			for _, term := range criterion.Terms {
				keyword := normalizeKeyword(term)
				if keyword == "" {
					continue
				}
				if _, exists := seen[keyword]; exists || used >= limit {
					continue
				}
				seen[keyword] = struct{}{}
				used++
				terms = append(terms, keyword)
			}
			if len(terms) > 0 {
				criterion.Terms = terms
				result = append(result, criterion)
			}
		}
		return result
	}
	return normalize(preferred), normalize(goals)
}

func normalizeKeyword(value string) string {
	return normalizeIdentity(value)
}

func normalizeIdentity(value string) string {
	value = norm.NFKC.String(value)
	var normalized strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsPunct(r) {
			normalized.WriteByte(' ')
		} else {
			normalized.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(normalized.String()), " ")
}

// NewParallelQueryExpander runs bounded attempts concurrently and aggregates
// them by stable attempt index.
func NewParallelQueryExpander(expander QueryExpander, fallback QueryExpander, options Options) (QueryExpander, error) {
	options, err := NormalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if expander == nil && fallback == nil {
		return nil, errors.New("parallel query expander is incomplete")
	}
	return parallelQueryExpander{expander: expander, fallback: fallback, options: options}, nil
}

func NewParallelMinimalStructuredPlanExpander(provider ChatProvider, fallback QueryExpander, options Options) (QueryExpander, error) {
	structured := StructuredPlanExpander{provider: provider, decodePlan: DecodeMinimalV1Plan}
	return NewParallelQueryExpander(structured, fallback, options)
}

type parallelQueryExpander struct {
	expander QueryExpander
	fallback QueryExpander
	options  Options
}

func (e parallelQueryExpander) Expand(ctx context.Context, request ExpansionRequest) (QueryPlan, error) {
	plan, _, err := e.ExpandWithTrace(ctx, request)
	return plan, err
}

func (e parallelQueryExpander) ExpandWithTrace(ctx context.Context, request ExpansionRequest) (QueryPlan, ExpansionInfo, error) {
	if err := ctx.Err(); err != nil {
		return QueryPlan{}, ExpansionInfo{}, err
	}
	type indexedExpansionResult struct {
		index int
		value parallelExpansionResult
	}
	outcomes := make(chan indexedExpansionResult, e.options.ExpansionAttempts)
	results := make([]parallelExpansionResult, e.options.ExpansionAttempts)
	for index := range results {
		go func(index int) {
			attempt := request
			attempt.Attempt = index + 1
			attempt.KeywordsPerAttempt = e.options.KeywordsPerAttempt
			outcomes <- indexedExpansionResult{index: index, value: e.expandOne(ctx, attempt)}
		}(index)
	}
	for completed := 0; completed < e.options.ExpansionAttempts; completed++ {
		select {
		case outcome := <-outcomes:
			results[outcome.index] = outcome.value
		case <-ctx.Done():
			return QueryPlan{}, ExpansionInfo{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return QueryPlan{}, ExpansionInfo{}, err
	}
	info := ExpansionInfo{RequestedAttempts: len(results), KeywordsPerAttempt: e.options.KeywordsPerAttempt, AttemptOutcomes: make([]ExpansionAttemptInfo, 0, len(results))}
	successes := make([]indexedPlan, 0, len(results))
	for index, result := range results {
		outcome := "provider_failed"
		if result.err == nil && !result.plan.Fallback && result.info.Source != "deterministic-fallback" {
			plan, err := NormalizeQueryPlan(result.plan, e.options.KeywordsPerAttempt)
			if err == nil {
				successes = append(successes, indexedPlan{index: index + 1, plan: plan})
				info.SuccessfulAttempts++
				outcome = "success"
			}
		}
		if outcome != "success" {
			info.ProviderFailedAttempts++
		}
		info.AttemptOutcomes = append(info.AttemptOutcomes, ExpansionAttemptInfo{AttemptIndex: index + 1, Outcome: outcome})
	}
	if len(successes) == 0 {
		if e.fallback == nil {
			return QueryPlan{}, info, &ExpansionError{Reason: "provider_error"}
		}
		plan, err := e.fallback.Expand(ctx, ExpansionRequest{Query: request.Query, CriterionPolicy: request.CriterionPolicy, KeywordsPerAttempt: e.options.KeywordsPerAttempt})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return QueryPlan{}, info, ctxErr
			}
			return QueryPlan{}, info, err
		}
		plan.RawQuery = request.Query
		plan.Fallback = true
		plan.KeywordSupport = nil
		info.FallbackCount = 1
		info.Source = "deterministic-fallback"
		info.FallbackReason = "all_provider_attempts_failed"
		return plan, info, nil
	}
	if hasConflictingHardCriteria(successes) {
		if e.fallback == nil {
			return QueryPlan{}, info, &ExpansionError{Reason: "conflicting_hard_constraints"}
		}
		plan, err := e.fallback.Expand(ctx, ExpansionRequest{Query: request.Query, CriterionPolicy: request.CriterionPolicy, KeywordsPerAttempt: e.options.KeywordsPerAttempt})
		if err != nil {
			return QueryPlan{}, info, err
		}
		plan.RawQuery = request.Query
		plan.Fallback = true
		plan.KeywordSupport = nil
		info.FallbackCount = 1
		info.Source = "deterministic-fallback"
		info.FallbackReason = "conflicting_hard_constraints"
		return plan, info, nil
	}
	plan := aggregatePlans(request.Query, successes)
	info.Source = "parallel-structured-llm"
	return plan, info, nil
}

type parallelExpansionResult struct {
	plan QueryPlan
	info ExpansionInfo
	err  error
}

type indexedPlan struct {
	index int
	plan  QueryPlan
}

func (e parallelQueryExpander) expandOne(ctx context.Context, request ExpansionRequest) parallelExpansionResult {
	if traced, ok := e.expander.(TracedQueryExpander); ok {
		plan, info, err := traced.ExpandWithTrace(ctx, request)
		return parallelExpansionResult{plan: plan, info: info, err: err}
	}
	plan, err := e.expander.Expand(ctx, request)
	return parallelExpansionResult{plan: plan, err: err}
}

func aggregatePlans(rawQuery string, attempts []indexedPlan) QueryPlan {
	plan := attempts[0].plan
	plan.RawQuery = rawQuery
	plan.Fallback = false
	plan.KeywordSupport = nil
	for _, attempt := range attempts[1:] {
		plan.Required = mergeCriteria(plan.Required, attempt.plan.Required)
		plan.Excluded = mergeCriteria(plan.Excluded, attempt.plan.Excluded)
		plan.Preferred = mergeCriteria(plan.Preferred, attempt.plan.Preferred)
		plan.Goals = mergeCriteria(plan.Goals, attempt.plan.Goals)
		plan.SupportingDimensions = mergeCriteria(plan.SupportingDimensions, attempt.plan.SupportingDimensions)
		plan.AcceptableAlternatives = mergeCriteria(plan.AcceptableAlternatives, attempt.plan.AcceptableAlternatives)
		plan.Ambiguity = uniqueStrings(append(plan.Ambiguity, attempt.plan.Ambiguity...))
	}
	plan.KeywordSupport = keywordSupport(attempts)
	return plan
}

func mergeCriteria(existing, additions []Criterion) []Criterion {
	result := append([]Criterion(nil), existing...)
	for _, addition := range additions {
		found := false
		for index := range result {
			if criterionKey(result[index]) != criterionKey(addition) || result[index].Proof != addition.Proof {
				continue
			}
			result[index].Terms = uniqueTerms(append(result[index].Terms, addition.Terms...))
			found = true
			break
		}
		if !found {
			result = append(result, addition)
		}
	}
	return result
}

func keywordSupport(attempts []indexedPlan) []KeywordSupport {
	type conceptKey struct{ role, kind, value string }
	counts := make(map[conceptKey]*KeywordSupport)
	forms := make(map[conceptKey]map[string]map[int]struct{})
	order := make([]conceptKey, 0)
	for _, attempt := range attempts {
		attemptForms := make(map[conceptKey]map[string]struct{})
		attemptOrder := make([]conceptKey, 0)
		for _, role := range []struct {
			name     string
			criteria []Criterion
		}{{"preferred", attempt.plan.Preferred}, {"goal", attempt.plan.Goals}} {
			for _, criterion := range role.criteria {
				for _, term := range criterion.Terms {
					keyword := normalizeKeyword(term)
					if keyword == "" {
						continue
					}
					concept := conceptKey{normalizeIdentity(role.name), normalizeIdentity(criterion.Kind), normalizeIdentity(criterion.Value)}
					if _, exists := attemptForms[concept]; !exists {
						attemptForms[concept] = make(map[string]struct{})
						attemptOrder = append(attemptOrder, concept)
					}
					attemptForms[concept][keyword] = struct{}{}
				}
			}
		}
		for _, concept := range attemptOrder {
			item, exists := counts[concept]
			if !exists {
				item = &KeywordSupport{Role: concept.role, Kind: concept.kind, Value: concept.value}
				counts[concept] = item
				forms[concept] = make(map[string]map[int]struct{})
				order = append(order, concept)
			}
			item.SupportCount++
			item.AttemptIndexes = append(item.AttemptIndexes, attempt.index)
			for surface := range attemptForms[concept] {
				if _, exists := forms[concept][surface]; !exists {
					forms[concept][surface] = make(map[int]struct{})
				}
				forms[concept][surface][attempt.index] = struct{}{}
			}
		}
	}
	result := make([]KeywordSupport, 0, len(order))
	for _, concept := range order {
		item := *counts[concept]
		for surface, attempts := range forms[concept] {
			indexes := make([]int, 0, len(attempts))
			for index := range attempts {
				indexes = append(indexes, index)
			}
			sort.Ints(indexes)
			item.SurfaceForms = append(item.SurfaceForms, SurfaceForm{Value: surface, AttemptIndexes: indexes})
		}
		sort.Slice(item.SurfaceForms, func(i, j int) bool { return item.SurfaceForms[i].Value < item.SurfaceForms[j].Value })
		if len(item.SurfaceForms) > 0 {
			item.Keyword = item.SurfaceForms[0].Value
		}
		result = append(result, item)
	}
	return result
}

func hasConflictingHardCriteria(attempts []indexedPlan) bool {
	required, excluded := map[string]struct{}{}, map[string]struct{}{}
	for _, attempt := range attempts {
		for _, criterion := range attempt.plan.Required {
			required[normalizeIdentity(criterion.Kind)+"\x00"+normalizeIdentity(criterion.Value)] = struct{}{}
		}
		for _, criterion := range attempt.plan.Excluded {
			excluded[normalizeIdentity(criterion.Kind)+"\x00"+normalizeIdentity(criterion.Value)] = struct{}{}
		}
	}
	for identity := range required {
		if _, ok := excluded[identity]; ok {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

const (
	StructuredPlanPromptID     = "minimal-v1"
	structuredPlanSystemPrompt = `You produce a retrieval plan for a frozen Lifestyle concept corpus. Return exactly one JSON object and no markdown. The object fields and exact types are: raw_query string; required array of Criterion; excluded array of Criterion; preferred array of Criterion; goals array of Criterion; supporting_dimensions array of Criterion; acceptable_alternatives array of Criterion; ambiguity array of strings; fallback boolean. Every Criterion is exactly {kind:string,value:string,terms:array of strings,proof:"lexical" or "semantic"}. Every lexical Criterion needs at least one discovery term. Never output a string where an array or object is required. Be conservative: only explicit user constraints may be required or excluded; absent never means excluded. In this minimal variant, supporting_dimensions and acceptable_alternatives must be empty arrays and fallback must be false.`
	structuredPlanUserTemplate = "Raw query: {{raw_query}}\nCriterion policy: {{criterion_policy}}\nInterpret the query into required, excluded, preferred and goals. Preserve the raw query exactly in raw_query. Return the single JSON object only."
)

func structuredPlanUserPrompt(raw string, policy CriterionPolicy) string {
	return structuredPlanUserPromptWithLimit(raw, policy, DefaultKeywordsPerAttempt)
}

func structuredPlanUserPromptWithLimit(raw string, policy CriterionPolicy, keywordsPerAttempt int) string {
	rawJSON, _ := json.Marshal(raw)
	policyJSON, _ := json.Marshal(policy)
	result := strings.ReplaceAll(structuredPlanUserTemplate, "{{raw_query}}", string(rawJSON))
	result = strings.ReplaceAll(result, "{{criterion_policy}}", string(policyJSON))
	return result + fmt.Sprintf("\nMaximum normalized positive discovery keywords for this attempt: %d.", keywordsPerAttempt)
}

func ValidateQueryPlan(plan QueryPlan) error {
	return validateQueryPlan(plan, false)
}

func validateQueryPlan(plan QueryPlan, allowFallback bool) error {
	return validateQueryPlanBase(plan, allowFallback, false)
}

func validateMinimalV1QueryPlan(plan QueryPlan, allowFallback bool) error {
	return validateQueryPlanBase(plan, allowFallback, true)
}

func validateQueryPlanBase(plan QueryPlan, allowFallback bool, requireMinimal bool) error {
	if strings.TrimSpace(plan.RawQuery) == "" {
		return errors.New("plan raw query is empty")
	}
	if !allowFallback && plan.Fallback {
		return errors.New("provider plan fallback must be false")
	}
	if requireMinimal && (len(plan.SupportingDimensions) != 0 || len(plan.AcceptableAlternatives) != 0) {
		return errors.New("minimal-v1 does not support supporting dimensions or alternatives")
	}
	seenRequired := make(map[string]struct{})
	criterionTotal := 0
	groups := []struct {
		name     string
		criteria []Criterion
	}{
		{"required", plan.Required}, {"excluded", plan.Excluded}, {"preferred", plan.Preferred}, {"goals", plan.Goals},
	}
	for _, group := range groups {
		criterionTotal += len(group.criteria)
		for _, criterion := range group.criteria {
			if err := validateCriterion(criterion); err != nil {
				return fmt.Errorf("%s criterion: %w", group.name, err)
			}
			key := criterionKey(criterion)
			if group.name == "required" {
				seenRequired[key] = struct{}{}
			}
			if group.name == "excluded" {
				if _, exists := seenRequired[key]; exists {
					return errors.New("criterion is both required and excluded")
				}
			}
		}
	}
	if criterionTotal == 0 {
		return errors.New("plan has no criteria")
	}
	return nil
}

func validateCriterion(criterion Criterion) error {
	if strings.TrimSpace(criterion.Kind) == "" || strings.TrimSpace(criterion.Value) == "" {
		return errors.New("kind and value are required")
	}
	if criterion.Proof != "lexical" && criterion.Proof != "semantic" {
		return errors.New("unsupported proof")
	}
	if criterion.Proof == "lexical" && len(criterion.Terms) == 0 {
		return errors.New("lexical criterion requires terms")
	}
	if criterion.Proof == "lexical" {
		for _, term := range criterion.Terms {
			if strings.TrimSpace(term) == "" {
				return errors.New("lexical criterion term cannot be empty")
			}
		}
	}
	return nil
}

func DecodePlan(response, raw string) (QueryPlan, error) {
	decoder := json.NewDecoder(strings.NewReader(response))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return QueryPlan{}, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return QueryPlan{}, errors.New("trailing JSON")
		}
		return QueryPlan{}, fmt.Errorf("trailing JSON: %w", err)
	}
	if object == nil {
		return QueryPlan{}, errors.New("plan must be a JSON object")
	}
	allowed := map[string]struct{}{
		"raw_query": {}, "required": {}, "excluded": {}, "preferred": {}, "goals": {},
		"supporting_dimensions": {}, "acceptable_alternatives": {}, "ambiguity": {}, "fallback": {},
	}
	for name := range object {
		if _, ok := allowed[name]; !ok {
			return QueryPlan{}, fmt.Errorf("unknown plan field %q", name)
		}
	}
	for _, name := range []string{"raw_query", "required", "excluded", "preferred", "goals", "supporting_dimensions", "acceptable_alternatives", "ambiguity", "fallback"} {
		if _, ok := object[name]; !ok {
			return QueryPlan{}, fmt.Errorf("missing plan field %q", name)
		}
	}
	var plan QueryPlan
	var err error
	if plan.RawQuery, err = decodeJSONString(object["raw_query"], "raw_query"); err != nil {
		return QueryPlan{}, err
	}
	for _, field := range []struct {
		name string
		into *[]Criterion
	}{
		{"required", &plan.Required}, {"excluded", &plan.Excluded}, {"preferred", &plan.Preferred}, {"goals", &plan.Goals},
		{"supporting_dimensions", &plan.SupportingDimensions}, {"acceptable_alternatives", &plan.AcceptableAlternatives},
	} {
		if *field.into, err = decodeCriteria(object[field.name], field.name); err != nil {
			return QueryPlan{}, err
		}
	}
	if plan.Ambiguity, err = decodeStringArray(object["ambiguity"], "ambiguity"); err != nil {
		return QueryPlan{}, err
	}
	if plan.Fallback, err = decodeJSONBool(object["fallback"], "fallback"); err != nil {
		return QueryPlan{}, err
	}
	if plan.RawQuery != raw {
		return QueryPlan{}, errors.New("plan raw query does not match request")
	}
	if err := validateQueryPlan(plan, false); err != nil {
		return QueryPlan{}, err
	}
	return plan, nil
}

func DecodeMinimalV1Plan(response, raw string) (QueryPlan, error) {
	plan, err := DecodePlan(response, raw)
	if err != nil {
		return QueryPlan{}, err
	}
	if err := ValidateMinimalV1Plan(plan); err != nil {
		return QueryPlan{}, err
	}
	return plan, nil
}

func ValidateMinimalV1Plan(plan QueryPlan) error {
	return validateMinimalV1QueryPlan(plan, false)
}

func decodeJSONString(raw json.RawMessage, field string) (string, error) {
	if firstJSONByte(raw) != '"' {
		return "", fmt.Errorf("%s must be a string", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", field, err)
	}
	return value, nil
}

func decodeJSONBool(raw json.RawMessage, field string) (bool, error) {
	first := firstJSONByte(raw)
	if first != 't' && first != 'f' {
		return false, fmt.Errorf("%s must be a boolean", field)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", field, err)
	}
	return value, nil
}

func decodeStringArray(raw json.RawMessage, field string) ([]string, error) {
	if firstJSONByte(raw) != '[' {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s must be an array: %w", field, err)
	}
	result := make([]string, len(values))
	for i, value := range values {
		decoded, err := decodeJSONString(value, fmt.Sprintf("%s[%d]", field, i))
		if err != nil {
			return nil, err
		}
		result[i] = decoded
	}
	return result, nil
}

func decodeCriteria(raw json.RawMessage, field string) ([]Criterion, error) {
	if firstJSONByte(raw) != '[' {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s must be an array: %w", field, err)
	}
	result := make([]Criterion, len(values))
	for i, value := range values {
		if firstJSONByte(value) != '{' {
			return nil, fmt.Errorf("%s[%d] must be an object", field, i)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(value, &object); err != nil {
			return nil, fmt.Errorf("%s[%d] must be an object: %w", field, i, err)
		}
		for name := range object {
			if name != "kind" && name != "value" && name != "terms" && name != "proof" {
				return nil, fmt.Errorf("unknown %s[%d] criterion field %q", field, i, name)
			}
		}
		for _, name := range []string{"kind", "value", "terms", "proof"} {
			if _, ok := object[name]; !ok {
				return nil, fmt.Errorf("missing %s[%d] criterion field %q", field, i, name)
			}
		}
		kind, err := decodeJSONString(object["kind"], field+" criterion kind")
		if err != nil {
			return nil, err
		}
		valueText, err := decodeJSONString(object["value"], field+" criterion value")
		if err != nil {
			return nil, err
		}
		terms, err := decodeStringArray(object["terms"], field+" criterion terms")
		if err != nil {
			return nil, err
		}
		proof, err := decodeJSONString(object["proof"], field+" criterion proof")
		if err != nil {
			return nil, err
		}
		result[i] = Criterion{Kind: kind, Value: valueText, Terms: terms, Proof: proof}
		if err := validateCriterion(result[i]); err != nil {
			return nil, fmt.Errorf("%s criterion: %w", field, err)
		}
	}
	return result, nil
}

func firstJSONByte(raw []byte) byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return 0
	}
	return trimmed[0]
}

func validateDeterministicFallbackPlan(plan QueryPlan) error {
	if !plan.Fallback {
		return errors.New("deterministic fallback must set fallback true")
	}
	if len(plan.Required) != 0 || len(plan.Excluded) != 0 || len(plan.Goals) != 0 || len(plan.SupportingDimensions) != 0 || len(plan.AcceptableAlternatives) != 0 || len(plan.Preferred) != 1 {
		return errors.New("deterministic fallback shape is invalid")
	}
	return validateQueryPlan(plan, true)
}

func queryTerms(value string) []string {
	fields := strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, value))
	if len(fields) == 0 && strings.TrimSpace(value) != "" {
		return []string{strings.TrimSpace(value)}
	}
	return uniqueTerms(fields)
}

func uniqueTerms(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func criterionKey(criterion Criterion) string {
	return normalizeIdentity(criterion.Kind) + "\x00" + normalizeIdentity(criterion.Value)
}

type CandidateMatcher interface {
	Match(context.Context, MatchRequest) (EligibilityResult, error)
}

type ResultSelector interface {
	Select(context.Context, SelectionInput) (SelectionResult, error)
}

type QueryRetrievalPipeline struct {
	cache                      *cache.Cache
	queryExpander              QueryExpander
	candidateMatcher           CandidateMatcher
	resultSelector             ResultSelector
	seedFor                    func(string) int64
	options                    Options
	allowDeterministicFallback bool
}

func NewQueryRetrievalPipeline(queryExpander QueryExpander, candidateMatcher CandidateMatcher, resultSelector ResultSelector, seedFor func(string) int64) *QueryRetrievalPipeline {
	if seedFor == nil {
		seedFor = ReproducibleSeed
	}
	return &QueryRetrievalPipeline{cache: cache.New(), queryExpander: queryExpander, candidateMatcher: candidateMatcher, resultSelector: resultSelector, seedFor: seedFor, options: DefaultOptions()}
}

func NewQueryRetrievalPipelineWithOptions(queryExpander QueryExpander, candidateMatcher CandidateMatcher, resultSelector ResultSelector, seedFor func(string) int64, options Options) *QueryRetrievalPipeline {
	if seedFor == nil {
		seedFor = ReproducibleSeed
	}
	return &QueryRetrievalPipeline{cache: cache.New(), queryExpander: queryExpander, candidateMatcher: candidateMatcher, resultSelector: resultSelector, seedFor: seedFor, options: options}
}

func NewExperimentExecutor(conceptCache *cache.Cache, provider ChatProvider, options Options) (query.Executor, error) {
	if conceptCache == nil {
		return nil, errors.New("query-retrieval cache is nil")
	}
	options, err := NormalizeOptions(options)
	if err != nil {
		return nil, err
	}
	seedFor := options.SeedFor
	if seedFor == nil {
		seedFor = ReproducibleSeed
	}
	expander, err := NewParallelMinimalStructuredPlanExpander(provider, NewDeterministicExpander(), options)
	if err != nil {
		return nil, err
	}
	service := NewQueryRetrievalPipelineWithOptions(expander, NewLexicalMatcher(nil), NewResultSelector(), seedFor, options)
	service.cache = conceptCache
	service.allowDeterministicFallback = true
	return service, nil
}

func (s *QueryRetrievalPipeline) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	if err := s.validate(); err != nil {
		return query.Result{}, err
	}
	return s.execute(ctx, reader, request, nil)
}

func (s *QueryRetrievalPipeline) ExecuteWithTrace(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, *Trace, error) {
	if err := s.validate(); err != nil {
		return query.Result{}, nil, err
	}
	trace := &Trace{Variant: "query-retrieval-v1", EvidenceThreshold: resolvedEvidenceThreshold(s.options), Expansion: ExpansionTrace{RequestedAttempts: s.options.ExpansionAttempts, KeywordsPerAttempt: s.options.KeywordsPerAttempt, RareKeywordMaxDocumentFrequency: s.options.RareDocumentFrequency, KeywordConsensusMinimum: MinimumKeywordConsensusSupport}}
	result, err := s.execute(ctx, reader, request, trace)
	return result, trace, err
}

func (s *QueryRetrievalPipeline) validate() error {
	if s == nil || s.cache == nil || s.queryExpander == nil || s.candidateMatcher == nil || s.resultSelector == nil {
		return errors.New("query-retrieval pipeline is incomplete")
	}
	return nil
}

func (s *QueryRetrievalPipeline) execute(ctx context.Context, reader cache.Reader, request query.Request, trace *Trace) (query.Result, error) {
	if recorder := query.ReceiptRecorderFromContext(ctx); recorder != nil {
		recorder.SetRetrievalConfig(s.options.SelectionLimit, s.options.ExplorationSlots, resolvedEvidenceThreshold(s.options))
		recorder.SetExpansionConfig(s.options.ExpansionAttempts, 0, 0, s.options.KeywordsPerAttempt, resolvedEvidenceThreshold(s.options), s.options.RareDocumentFrequency, MinimumKeywordConsensusSupport, nil)
	}
	cacheCtx := ctx
	if recorder := query.ReceiptRecorderFromContext(ctx); recorder != nil {
		cacheCtx = recorder.StartStage(ctx, "cache_load", "", "", "")
	}
	entries, err := s.cache.All(cacheCtx, reader)
	query.FinishStage(cacheCtx, map[bool]string{true: "failure", false: "success"}[err != nil])
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return query.Result{}, err
		}
		return query.Result{}, fmt.Errorf("query-retrieval corpus unavailable: %w", err)
	}
	started := time.Now()
	var plan QueryPlan
	info := ExpansionInfo{}
	requestWithPolicy := ExpansionRequest{Query: request.Query, CriterionPolicy: DefaultCriterionPolicy}
	stageCtx := ctx
	if recorder := query.ReceiptRecorderFromContext(ctx); recorder != nil {
		provider, model, reasoning := providerIdentity(s.queryExpander)
		stageCtx = recorder.StartStage(ctx, "query_expansion", provider, model, reasoning)
	}
	if traced, ok := s.queryExpander.(TracedQueryExpander); ok {
		plan, info, err = traced.ExpandWithTrace(stageCtx, requestWithPolicy)
	} else {
		plan, err = s.queryExpander.Expand(stageCtx, requestWithPolicy)
	}
	if err != nil {
		query.FinishStage(stageCtx, "failure")
		appendTraceStage(trace, StageTrace{Name: "expansion", Outcome: "failure", ElapsedMS: elapsedSince(started), FallbackReason: "expansion_failure"})
		return query.Result{}, err
	}
	if strings.TrimSpace(plan.RawQuery) == "" {
		plan.RawQuery = request.Query
	}
	validate := ValidateQueryPlan
	if plan.Fallback && s.allowDeterministicFallback {
		validate = validateDeterministicFallbackPlan
	}
	if err := validate(plan); err != nil {
		query.FinishStage(stageCtx, "failure")
		appendTraceStage(trace, StageTrace{Name: "expansion", Outcome: "invalid", ElapsedMS: elapsedSince(started), FallbackReason: "invalid_plan"})
		return query.Result{}, errors.New("query-retrieval expansion invalid")
	}
	if recorder := query.ReceiptRecorderFromContext(ctx); recorder != nil {
		support := make([]query.KeywordSupportReceipt, 0, len(plan.KeywordSupport))
		for _, item := range plan.KeywordSupport {
			surfaces := make([]string, 0, len(item.SurfaceForms))
			for _, surface := range item.SurfaceForms {
				surfaces = append(surfaces, surface.Value)
			}
			support = append(support, query.KeywordSupportReceipt{Role: item.Role, Kind: item.Kind, Value: item.Value, Keyword: item.Keyword, SurfaceForms: surfaces, SupportCount: item.SupportCount, AttemptIndexes: append([]int(nil), item.AttemptIndexes...)})
		}
		recorder.SetExpansionConfig(info.RequestedAttempts, info.SuccessfulAttempts, info.ProviderFailedAttempts, info.KeywordsPerAttempt, resolvedEvidenceThreshold(s.options), s.options.RareDocumentFrequency, MinimumKeywordConsensusSupport, support)
		outcomes := make([]query.ExpansionAttemptReceipt, 0, len(info.AttemptOutcomes))
		for _, outcome := range info.AttemptOutcomes {
			outcomes = append(outcomes, query.ExpansionAttemptReceipt{AttemptIndex: outcome.AttemptIndex, Outcome: outcome.Outcome})
		}
		recorder.SetExpansionAttemptOutcomes(outcomes)
		recorder.SetFallbackExpansionCount(info.FallbackCount)
	}
	if trace != nil {
		trace.Expansion = ExpansionTrace{RequestedAttempts: info.RequestedAttempts, SuccessfulAttempts: info.SuccessfulAttempts, ProviderFailedAttempts: info.ProviderFailedAttempts, FallbackCount: info.FallbackCount, KeywordsPerAttempt: info.KeywordsPerAttempt, RareKeywordMaxDocumentFrequency: s.options.RareDocumentFrequency, EvidenceThreshold: resolvedEvidenceThreshold(s.options), KeywordConsensusMinimum: MinimumKeywordConsensusSupport, KeywordSupport: append([]KeywordSupport(nil), plan.KeywordSupport...)}
	}
	appendTraceStage(trace, StageTrace{
		Name: "expansion", Outcome: PlanOutcome(plan), Source: info.Source, FallbackReason: info.FallbackReason,
		ElapsedMS: elapsedSince(started), InputCount: 1, OutputCount: CriterionCount(plan), Plan: &plan,
	})
	query.FinishStage(stageCtx, map[bool]string{true: "fallback", false: "success"}[plan.Fallback])

	seed := ReproducibleSeed(request.Query)
	if s.options.Seed != nil {
		seed = *s.options.Seed
	} else if s.seedFor != nil {
		seed = s.seedFor(request.Query)
	}
	if trace != nil {
		trace.Seed = seed
	}

	started = time.Now()
	matchCtx := ctx
	if recorder := query.ReceiptRecorderFromContext(ctx); recorder != nil {
		matchCtx = recorder.StartStage(ctx, "candidate_matching", "", "", "")
	}
	matchReq := MatchRequest{Plan: plan, CorpusEntries: entries, EvidenceThreshold: s.options.EvidenceThreshold, EvidenceThresholdSet: s.options.EvidenceThresholdSet, RareKeywordMaxDocumentFrequency: s.options.RareDocumentFrequency, FallbackQualificationAllowed: !s.options.EvidenceThresholdSet}
	eligible, err := s.candidateMatcher.Match(matchCtx, matchReq)
	if err != nil {
		query.FinishStage(matchCtx, "failure")
		appendTraceStage(trace, StageTrace{Name: "matching", Outcome: "failure", ElapsedMS: elapsedSince(started), InputCount: len(entries)})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return query.Result{}, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return query.Result{}, err
		}
		return query.Result{}, fmt.Errorf("query-retrieval matching failed: %w", err)
	}
	appendTraceStage(trace, StageTrace{Name: "matching", Outcome: "success", ElapsedMS: elapsedSince(started), InputCount: len(entries), OutputCount: QualifiedCount(eligible.Candidates), TotalCount: len(eligible.Candidates), Candidates: eligible.Candidates, EvidenceThreshold: resolvedEvidenceThreshold(s.options)})
	query.FinishStage(matchCtx, "success")

	started = time.Now()
	selectionCtx := ctx
	if recorder := query.ReceiptRecorderFromContext(ctx); recorder != nil {
		selectionCtx = recorder.StartStage(ctx, "result_selection", "", "", "")
	}
	selected, err := s.resultSelector.Select(selectionCtx, SelectionInput{Candidates: eligible.Candidates, Limit: s.options.SelectionLimit, ExplorationSlots: s.options.ExplorationSlots, Seed: seed})
	if err != nil {
		query.FinishStage(selectionCtx, "failure")
		appendTraceStage(trace, StageTrace{Name: "selection", Outcome: "failure", ElapsedMS: elapsedSince(started), InputCount: len(eligible.Candidates)})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return query.Result{}, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return query.Result{}, err
		}
		return query.Result{}, fmt.Errorf("query-retrieval selection failed: %w", err)
	}
	appendTraceStage(trace, StageTrace{Name: "selection", Outcome: "success", ElapsedMS: elapsedSince(started), InputCount: len(eligible.Candidates), OutputCount: selectedCount(selected.Selected), TotalCount: len(selected.Selected), Decisions: selected.Selected, EvidenceThreshold: resolvedEvidenceThreshold(s.options)})
	query.FinishStage(selectionCtx, "success")
	results := make([]search.Result, 0, len(selected.Selected))
	for _, candidate := range selected.Selected {
		if err := ctx.Err(); err != nil {
			return query.Result{}, err
		}
		if candidate.Selected {
			results = append(results, search.Result{Slug: candidate.Slug, Title: candidate.Title, Type: "concept"})
		}
	}
	status, reason := ResultStatus(len(results))
	return query.Result{Query: request.Query, Mode: request.Mode, Results: results, Expand: expandFromPlan(plan), Status: status, Reason: reason}, nil
}

func providerIdentity(expander QueryExpander) (string, string, string) {
	if parallel, ok := expander.(parallelQueryExpander); ok {
		return providerIdentity(parallel.expander)
	}
	if structured, ok := expander.(StructuredPlanExpander); ok {
		if client, ok := structured.provider.(*llm.Client); ok {
			return "deepseek", client.Model(), string(client.Reasoning())
		}
	}
	return "", "", ""
}

func appendTraceStage(trace *Trace, stage StageTrace) {
	if trace != nil {
		trace.Stages = append(trace.Stages, stage)
	}
}

func expandFromPlan(plan QueryPlan) *llm.ExpandResult {
	keywords := make([]string, 0)
	seen := make(map[string]struct{})
	for _, criteria := range [][]Criterion{plan.Required, plan.Excluded, plan.Preferred, plan.Goals, plan.SupportingDimensions, plan.AcceptableAlternatives} {
		for _, criterion := range criteria {
			for _, term := range criterion.Terms {
				term = strings.TrimSpace(term)
				key := strings.ToLower(term)
				if term == "" {
					continue
				}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				keywords = append(keywords, term)
			}
		}
	}
	return &llm.ExpandResult{Keywords: keywords}
}

type Trace struct {
	Variant           string         `json:"variant"`
	Seed              int64          `json:"seed"`
	EvidenceThreshold int            `json:"evidence_threshold"`
	Expansion         ExpansionTrace `json:"expansion"`
	Stages            []StageTrace   `json:"stages"`
}

type ExpansionTrace struct {
	RequestedAttempts               int              `json:"requested_attempts"`
	SuccessfulAttempts              int              `json:"successful_attempts"`
	ProviderFailedAttempts          int              `json:"provider_failed_attempts"`
	FallbackCount                   int              `json:"fallback_count"`
	KeywordsPerAttempt              int              `json:"keywords_per_attempt"`
	RareKeywordMaxDocumentFrequency int              `json:"rare_keyword_max_document_frequency"`
	EvidenceThreshold               int              `json:"evidence_threshold"`
	KeywordConsensusMinimum         int              `json:"keyword_consensus_minimum"`
	KeywordSupport                  []KeywordSupport `json:"keyword_support,omitempty"`
}

func resolvedEvidenceThreshold(options Options) int {
	if options.EvidenceThresholdSet {
		return options.EvidenceThreshold
	}
	return DefaultEvidenceThreshold
}

type StageTrace struct {
	Name              string              `json:"name"`
	Outcome           string              `json:"outcome"`
	Source            string              `json:"source,omitempty"`
	ElapsedMS         int64               `json:"elapsed_ms"`
	InputCount        int                 `json:"input_count"`
	OutputCount       int                 `json:"output_count"`
	TotalCount        int                 `json:"total_count,omitempty"`
	FallbackReason    string              `json:"fallback_reason,omitempty"`
	Plan              *QueryPlan          `json:"plan,omitempty"`
	Candidates        []CandidateEvidence `json:"candidates,omitempty"`
	Decisions         []SelectedCandidate `json:"decisions,omitempty"`
	EvidenceThreshold int                 `json:"evidence_threshold,omitempty"`
}

func elapsedSince(start time.Time) int64 {
	value := time.Since(start).Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func PlanOutcome(plan QueryPlan) string {
	if plan.Fallback {
		return "fallback"
	}
	return "success"
}

func CriterionCount(plan QueryPlan) int {
	return len(plan.Required) + len(plan.Excluded) + len(plan.Preferred) + len(plan.Goals) + len(plan.SupportingDimensions) + len(plan.AcceptableAlternatives)
}

func EligibleCount(candidates []CandidateEvidence) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.Eligible {
			count++
		}
	}
	return count
}

func QualifiedCount(candidates []CandidateEvidence) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.Eligible && candidate.Qualified {
			count++
		}
	}
	return count
}

func selectedCount(candidates []SelectedCandidate) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.Selected {
			count++
		}
	}
	return count
}

func ReproducibleSeed(query string) int64 {
	digest := sha256.Sum256([]byte(strings.TrimSpace(query)))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

type deterministicExpander struct{}

func NewDeterministicExpander() QueryExpander { return deterministicExpander{} }

func (deterministicExpander) Expand(_ context.Context, request ExpansionRequest) (QueryPlan, error) {
	raw := request.Query
	query := strings.Join(strings.Fields(raw), " ")
	if query == "" {
		return QueryPlan{}, errors.New("empty query")
	}
	plan := QueryPlan{RawQuery: raw, Fallback: true, Preferred: []Criterion{{Kind: "query", Value: query, Terms: queryTerms(query), Proof: "lexical"}}}
	return plan, validateDeterministicFallbackPlan(plan)
}

type SemanticDecision struct{ Outcome string }

type SemanticEvaluator interface {
	Evaluate(context.Context, Criterion, cache.Entry) (SemanticDecision, error)
}

type lexicalMatcher struct{ semantic SemanticEvaluator }

func NewLexicalMatcher(semantic SemanticEvaluator) CandidateMatcher {
	return lexicalMatcher{semantic: semantic}
}

func (m lexicalMatcher) Match(ctx context.Context, request MatchRequest) (EligibilityResult, error) {
	threshold := request.EvidenceThreshold
	if !request.EvidenceThresholdSet {
		threshold = DefaultEvidenceThreshold
	}
	if threshold < 0 {
		return EligibilityResult{}, errors.New("evidence threshold must not be negative")
	}
	rareDocumentFrequency := request.RareKeywordMaxDocumentFrequency
	if rareDocumentFrequency == 0 {
		rareDocumentFrequency = DefaultRareDocumentFrequency
	}
	if rareDocumentFrequency < 1 || rareDocumentFrequency > maxRareDocumentFrequency {
		return EligibilityResult{}, fmt.Errorf("rare document frequency must be between 1 and %d", maxRareDocumentFrequency)
	}
	searchCorpus := make([]searchableEntry, len(request.CorpusEntries))
	for index, entry := range request.CorpusEntries {
		searchCorpus[index] = prepareSearchableEntry(entry)
	}
	documentFrequencies := precomputeDocumentFrequencies(request.Plan.KeywordSupport, searchCorpus)
	candidates := make([]CandidateEvidence, 0, len(request.CorpusEntries))
	for index, entry := range request.CorpusEntries {
		if err := ctx.Err(); err != nil {
			return EligibilityResult{}, err
		}
		candidate := CandidateEvidence{Slug: entry.Slug, Title: entry.Title, Eligible: true, EvidenceThreshold: threshold}
		fields := searchCorpus[index].Fields
		for _, role := range []struct {
			name       string
			criteria   []Criterion
			failClosed bool
		}{
			{"required", request.Plan.Required, semanticRequiredFailClosed}, {"excluded", request.Plan.Excluded, semanticExcludedFailClosed},
			{"preferred", request.Plan.Preferred, false}, {"goal", request.Plan.Goals, false}, {"supporting", request.Plan.SupportingDimensions, false}, {"alternative", request.Plan.AcceptableAlternatives, false},
		} {
			if err := ctx.Err(); err != nil {
				return EligibilityResult{}, err
			}
			groups, err := matchRole(ctx, role.name, role.criteria, fields, entry, m.semantic)
			if err != nil {
				return EligibilityResult{}, err
			}
			candidate.Groups = append(candidate.Groups, groups...)
			for _, group := range groups {
				if role.name == "required" && group.SemanticOutcome != "" && group.SemanticOutcome != "matched" && role.failClosed {
					candidate.Eligible = false
					candidate.Rejection = "semantic_required_" + group.SemanticOutcome
				}
				if role.name == "excluded" && group.SemanticOutcome == "matched" {
					candidate.Eligible = false
					candidate.Rejection = "excluded_criterion_matched"
				}
			}
		}
		for _, criterion := range request.Plan.Required {
			if criterion.Proof != "semantic" && !hasMatchedGroup(candidate.Groups, "required", criterion) {
				candidate.Eligible = false
				candidate.Rejection = "required_" + criterion.Kind + "_not_matched"
				break
			}
		}
		if candidate.Eligible && hasAnyMatchedGroup(candidate.Groups, "excluded", request.Plan.Excluded) {
			candidate.Eligible = false
			candidate.Rejection = "excluded_criterion_matched"
		}
		candidate.SemanticResolution = semanticResolution(candidate.Groups)
		candidate.SemanticOutcome = candidate.SemanticResolution
		candidate.ExactIdentityEvidence = hasExactRequiredIdentity(request.Plan, searchCorpus[index], candidate.Groups) || hasExactRawQueryIdentity(request.Plan, searchCorpus[index])
		candidate.PositiveEvidenceDimensions = positiveEvidenceDimensions(request.Plan, candidate.Groups)
		candidate.PositiveEvidenceCount = len(candidate.PositiveEvidenceDimensions)
		candidate.Score = independentDimensionScore(request.Plan, candidate.Groups)
		candidate.KeywordEvidence, candidate.MatchedFields = keywordEvidence(request.Plan, candidate.Groups, searchCorpus, index, documentFrequencies)
		candidate.Qualified, candidate.QualificationReason, candidate.QualificationPath = qualifyCandidate(candidate, threshold, request.Plan.RawQuery, rareDocumentFrequency, request.Plan.Fallback && request.FallbackQualificationAllowed)
		candidates = append(candidates, candidate)
	}
	return EligibilityResult{Candidates: candidates}, nil
}

func matchRole(ctx context.Context, role string, criteria []Criterion, fields []searchableField, entry cache.Entry, evaluator SemanticEvaluator) ([]GroupEvidence, error) {
	groups := make([]GroupEvidence, 0, len(criteria))
	for _, criterion := range criteria {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if criterion.Proof == "semantic" {
			outcome := "unresolved"
			if evaluator != nil {
				decision, err := evaluator.Evaluate(ctx, criterion, entry)
				if err != nil {
					return nil, err
				}
				outcome = safeSemanticOutcome(decision.Outcome)
			}
			groups = append(groups, GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value, SemanticOutcome: outcome})
			continue
		}
		matched, err := matchGroupsForRole(ctx, []Criterion{criterion}, role, fields)
		if err != nil {
			return nil, err
		}
		groups = append(groups, matched...)
	}
	return groups, nil
}

func safeSemanticOutcome(outcome string) string {
	switch outcome {
	case "pass", "matched":
		return "matched"
	case "fail", "not_matched":
		return "not_matched"
	case "unknown", "unavailable", "unresolved":
		return "unresolved"
	default:
		return "unresolved"
	}
}

type searchableField struct {
	Name  string
	Value string
}

type searchableEntry struct {
	Fields          []searchableField
	Identities      []string
	RawIdentities   []string
	CanonicalProofs []string
}

var searchableFrontmatterKeys = []string{"title", "name", "aliases", "tags", "keywords", "category", "categories", "type", "location"}

func prepareSearchableEntry(entry cache.Entry) searchableEntry {
	values := make([]string, 0)
	for _, key := range searchableFrontmatterKeys {
		values = appendSearchableValues(values, entry.Frontmatter[key])
	}
	identities := make([]string, 0, 2+len(values))
	if normalizeIdentity(entry.Title) != "" {
		identities = append(identities, entry.Title)
	}
	if normalizeIdentity(entry.Slug) != "" {
		identities = append(identities, entry.Slug)
	}
	identities = append(identities, frontmatterIdentityAliases(entry.Frontmatter)...)
	proofs := []string{entry.Title}
	for _, key := range []string{"title", "name", "slug"} {
		value, _ := entry.Frontmatter[key].(string)
		if value = strings.TrimSpace(value); value != "" {
			proofs = append(proofs, value)
		}
	}
	return searchableEntry{
		Fields:          []searchableField{{Name: "title", Value: entry.Title}, {Name: "frontmatter", Value: strings.Join(values, " ")}, {Name: "body", Value: entry.Body}},
		Identities:      identities,
		RawIdentities:   []string{entry.Title, entry.Slug},
		CanonicalProofs: proofs,
	}
}

func appendSearchableValues(values []string, value interface{}) []string {
	switch value := value.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			return append(values, value)
		}
	case []string:
		for _, item := range value {
			values = appendSearchableValues(values, item)
		}
	case []interface{}:
		for _, item := range value {
			if text, ok := item.(string); ok {
				values = appendSearchableValues(values, text)
			}
		}
	case json.Number:
		return append(values, string(value))
	case bool:
		return append(values, fmt.Sprint(value))
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return append(values, fmt.Sprint(value))
	}
	return values
}

func frontmatterIdentityAliases(frontmatter map[string]interface{}) []string {
	return appendSearchableValues(nil, frontmatter["aliases"])
}

func matchGroups(criteria []Criterion, fields []searchableField) []GroupEvidence {
	groups, _ := matchGroupsForRole(context.Background(), criteria, "", fields)
	return groups
}

func matchGroupsForRole(ctx context.Context, criteria []Criterion, role string, fields []searchableField) ([]GroupEvidence, error) {
	groups := make([]GroupEvidence, 0, len(criteria))
	for _, criterion := range criteria {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if criterion.Proof == "semantic" {
			groups = append(groups, GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value, SemanticOutcome: "unresolved"})
			continue
		}
		group := GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value}
		for _, field := range fields {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			matched := make([]string, 0)
			for _, term := range criterion.Terms {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				keyword := normalizeKeyword(term)
				if containsNormalizedPhrase(field.Value, keyword) && !ContainsString(matched, keyword) {
					matched = append(matched, keyword)
				}
			}
			if len(matched) > 0 {
				group.Matches = append(group.Matches, FieldEvidence{Field: field.Name, Terms: matched})
			}
		}
		if len(group.Matches) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

func hasMatchedGroup(groups []GroupEvidence, role string, criterion Criterion) bool {
	for _, group := range groups {
		if normalizeIdentity(group.Role) != normalizeIdentity(role) || normalizeKind(group.Kind) != normalizeKind(criterion.Kind) || normalizeIdentity(group.Value) != normalizeIdentity(criterion.Value) {
			continue
		}
		for _, match := range group.Matches {
			for _, matchedTerm := range match.Terms {
				for _, term := range criterion.Terms {
					if normalizeKeyword(matchedTerm) == normalizeKeyword(term) {
						return true
					}
				}
			}
		}
	}
	return false
}

func hasAnyMatchedGroup(groups []GroupEvidence, role string, criteria []Criterion) bool {
	for _, criterion := range criteria {
		if hasMatchedGroup(groups, role, criterion) {
			return true
		}
	}
	return false
}

func independentDimensionScore(plan QueryPlan, groups []GroupEvidence) int {
	return len(positiveEvidenceDimensions(plan, groups))
}

func normalizeKind(kind string) string {
	return strings.ReplaceAll(normalizeIdentity(kind), " ", "-")
}

func exactIdentityKind(kind string) bool {
	switch normalizeKind(kind) {
	case "entity", "name", "entity-name", "venue-name":
		return true
	default:
		return false
	}
}

func hasExactRequiredIdentity(plan QueryPlan, candidate searchableEntry, groups []GroupEvidence) bool {
	rawQuery := normalizeIdentity(plan.RawQuery)
	if rawQuery == "" {
		return false
	}
	for _, criterion := range plan.Required {
		if exactIdentityKind(criterion.Kind) && criterion.Proof != "semantic" && hasMatchedGroup(groups, "required", criterion) {
			for _, group := range groups {
				if normalizeIdentity(group.Role) != "required" || normalizeKind(group.Kind) != normalizeKind(criterion.Kind) || normalizeIdentity(group.Value) != normalizeIdentity(criterion.Value) {
					continue
				}
				for _, match := range group.Matches {
					if match.Field != "title" && match.Field != "frontmatter" {
						continue
					}
					for _, identity := range candidate.Identities {
						canonical := normalizeIdentity(identity)
						if canonical == "" || !containsNormalizedPhrase(rawQuery, canonical) {
							continue
						}
						if canonical == normalizeIdentity(criterion.Value) || containsNormalizedString(criterion.Terms, canonical) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func hasExactRawQueryIdentity(plan QueryPlan, candidate searchableEntry) bool {
	rawQuery := normalizeIdentity(plan.RawQuery)
	if rawQuery == "" {
		return false
	}
	for _, identity := range candidate.RawIdentities {
		identity = normalizeIdentity(identity)
		if identity == "" || !containsNormalizedPhrase(rawQuery, identity) {
			continue
		}
		for _, proof := range candidate.CanonicalProofs {
			if normalizeIdentity(proof) == identity {
				return true
			}
		}
	}
	return false
}

func containsNormalizedString(values []string, want string) bool {
	for _, value := range values {
		if normalizeIdentity(value) == want {
			return true
		}
	}
	return false
}

func positiveEvidenceDimensions(plan QueryPlan, groups []GroupEvidence) []string {
	roleCriteria := []struct {
		role     string
		criteria []Criterion
	}{
		{"preferred", plan.Preferred},
		{"supporting", plan.SupportingDimensions},
		{"goal", plan.Goals},
		{"alternative", plan.AcceptableAlternatives},
	}
	seen := make(map[string]struct{})
	dimensions := make([]string, 0, len(plan.Preferred)+len(plan.SupportingDimensions)+len(plan.Goals)+len(plan.AcceptableAlternatives))
	for _, rc := range roleCriteria {
		for _, criterion := range rc.criteria {
			if criterion.Proof == "semantic" || !hasMatchedGroup(groups, rc.role, criterion) {
				continue
			}
			kind := normalizeKind(criterion.Kind)
			if _, exists := seen[kind]; exists {
				continue
			}
			seen[kind] = struct{}{}
			dimensions = append(dimensions, kind)
		}
	}
	return dimensions
}

func semanticResolution(groups []GroupEvidence) string {
	seen := ""
	for _, group := range groups {
		switch group.SemanticOutcome {
		case "unresolved":
			return "unresolved"
		case "matched":
			seen = "matched"
		case "not_matched":
			if seen == "" {
				seen = "not_matched"
			}
		}
	}
	return seen
}

type keywordConceptKey struct{ role, kind, value string }

func keywordConcept(support KeywordSupport) keywordConceptKey {
	return keywordConceptKey{normalizeIdentity(support.Role), normalizeIdentity(support.Kind), normalizeIdentity(support.Value)}
}

func supportSurfaceForms(support KeywordSupport) []SurfaceForm {
	forms := make(map[string]map[int]struct{})
	for _, form := range support.SurfaceForms {
		value := normalizeKeyword(form.Value)
		if value == "" {
			continue
		}
		if _, exists := forms[value]; !exists {
			forms[value] = make(map[int]struct{})
		}
		for _, index := range form.AttemptIndexes {
			forms[value][index] = struct{}{}
		}
	}
	if keyword := normalizeKeyword(support.Keyword); keyword != "" {
		if _, exists := forms[keyword]; !exists {
			forms[keyword] = make(map[int]struct{})
		}
	}
	result := make([]SurfaceForm, 0, len(forms))
	for value, indexes := range forms {
		attempts := make([]int, 0, len(indexes))
		for index := range indexes {
			attempts = append(attempts, index)
		}
		sort.Ints(attempts)
		result = append(result, SurfaceForm{Value: value, AttemptIndexes: attempts})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Value < result[j].Value })
	return result
}

func mergeKeywordSupports(supports []KeywordSupport) []KeywordSupport {
	merged := make(map[keywordConceptKey]*KeywordSupport)
	order := make([]keywordConceptKey, 0, len(supports))
	for _, support := range supports {
		key := keywordConcept(support)
		item, exists := merged[key]
		if !exists {
			copy := support
			copy.SurfaceForms = nil
			copy.AttemptIndexes = append([]int(nil), support.AttemptIndexes...)
			item = &copy
			merged[key] = item
			order = append(order, key)
		}
		if !exists {
			item.SurfaceForms = append(item.SurfaceForms, supportSurfaceForms(support)...)
			continue
		}
		attempts := make(map[int]struct{}, len(item.AttemptIndexes)+len(support.AttemptIndexes))
		for _, index := range item.AttemptIndexes {
			attempts[index] = struct{}{}
		}
		for _, index := range support.AttemptIndexes {
			attempts[index] = struct{}{}
		}
		item.AttemptIndexes = item.AttemptIndexes[:0]
		for index := range attempts {
			item.AttemptIndexes = append(item.AttemptIndexes, index)
		}
		sort.Ints(item.AttemptIndexes)
		if len(item.AttemptIndexes) > 0 {
			item.SupportCount = len(item.AttemptIndexes)
		} else if support.SupportCount > item.SupportCount {
			item.SupportCount = support.SupportCount
		}
		item.SurfaceForms = append(item.SurfaceForms, supportSurfaceForms(support)...)
		if item.Keyword == "" {
			item.Keyword = support.Keyword
		}
	}
	for _, key := range order {
		item := merged[key]
		item.SurfaceForms = supportSurfaceForms(*item)
		if len(item.SurfaceForms) > 0 {
			item.Keyword = item.SurfaceForms[0].Value
		}
	}
	result := make([]KeywordSupport, 0, len(order))
	for _, key := range order {
		result = append(result, *merged[key])
	}
	return result
}

func precomputeDocumentFrequencies(supports []KeywordSupport, corpus []searchableEntry) map[keywordConceptKey]int {
	frequencies := make(map[keywordConceptKey]int)
	for _, support := range mergeKeywordSupports(supports) {
		key := keywordConcept(support)
		forms := supportSurfaceForms(support)
		for _, entry := range corpus {
			matched := false
			for _, field := range entry.Fields {
				for _, form := range forms {
					if containsNormalizedPhrase(field.Value, form.Value) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				frequencies[key]++
			}
		}
	}
	return frequencies
}

func keywordEvidence(plan QueryPlan, groups []GroupEvidence, corpus []searchableEntry, candidateIndex int, documentFrequencies map[keywordConceptKey]int) ([]KeywordEvidence, []string) {
	matchedFields := make([]string, 0)
	result := make([]KeywordEvidence, 0)
	for _, support := range mergeKeywordSupports(plan.KeywordSupport) {
		if support.Role != "preferred" && support.Role != "goal" {
			continue
		}
		forms := supportSurfaceForms(support)
		matched := false
		for _, group := range groups {
			if normalizeIdentity(group.Role) != normalizeIdentity(support.Role) || normalizeKind(group.Kind) != normalizeKind(support.Kind) || normalizeIdentity(group.Value) != normalizeIdentity(support.Value) {
				continue
			}
			for _, field := range group.Matches {
				for _, term := range field.Terms {
					for _, form := range forms {
						if normalizeKeyword(term) != form.Value {
							continue
						}
						matched = true
						matchedFields = appendUnique(matchedFields, field.Field)
						break
					}
				}
			}
		}
		if !matched {
			continue
		}
		fieldsForKeyword := make([]string, 0, len(corpus[candidateIndex].Fields))
		for _, field := range corpus[candidateIndex].Fields {
			for _, form := range forms {
				if containsNormalizedPhrase(field.Value, form.Value) {
					fieldsForKeyword = appendUnique(fieldsForKeyword, field.Name)
					break
				}
			}
		}
		sort.Strings(fieldsForKeyword)
		result = append(result, KeywordEvidence{Role: support.Role, Kind: support.Kind, Value: support.Value, Keyword: support.Keyword, SurfaceForms: append([]SurfaceForm(nil), forms...), SupportCount: support.SupportCount, AttemptIndexes: append([]int(nil), support.AttemptIndexes...), DocumentFrequency: documentFrequencies[keywordConcept(support)], MatchedFields: fieldsForKeyword})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Role != result[j].Role {
			return result[i].Role < result[j].Role
		}
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Value < result[j].Value
	})
	sort.Strings(matchedFields)
	return result, matchedFields
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func qualifyCandidate(candidate CandidateEvidence, threshold int, rawQuery string, rareDocumentFrequency int, fallback bool) (bool, string, string) {
	if !candidate.Eligible {
		return false, "hard_ineligible", "hard_ineligible"
	}
	if candidate.ExactIdentityEvidence {
		return true, "exact_identity_evidence", "exact_identity"
	}
	if candidate.SemanticResolution == "unresolved" && candidate.PositiveEvidenceCount == 0 {
		return false, "unresolved_semantic_evidence", "semantic_unresolved"
	}
	if len(candidate.KeywordEvidence) > 0 {
		for _, evidence := range candidate.KeywordEvidence {
			if evidence.SupportCount >= MinimumKeywordConsensusSupport {
				return true, "keyword_consensus", "keyword_consensus"
			}
		}
		raw := normalizeKeyword(rawQuery)
		for _, evidence := range candidate.KeywordEvidence {
			grounded := false
			for _, form := range evidence.SurfaceForms {
				if containsNormalizedPhrase(raw, form.Value) {
					grounded = true
					break
				}
			}
			if !grounded {
				grounded = containsNormalizedPhrase(raw, evidence.Keyword)
			}
			if evidence.SupportCount != 1 || evidence.DocumentFrequency > rareDocumentFrequency || !grounded {
				continue
			}
			if containsString(evidence.MatchedFields, "title") || containsString(evidence.MatchedFields, "frontmatter") {
				return true, "rare_discriminative_lexical", "rare_discriminative_lexical"
			}
		}
		return false, "evidence_below_threshold", "keyword_support_below_threshold"
	}
	if fallback && candidate.PositiveEvidenceCount > 0 {
		return true, "fallback_single_attempt", "fallback_single_attempt"
	}
	if threshold == 0 {
		return true, "legacy_threshold_zero", "legacy_threshold_zero"
	}
	if candidate.PositiveEvidenceCount >= threshold {
		return true, "evidence_threshold_met", "legacy_positive_evidence"
	}
	return false, "evidence_below_threshold", "legacy_positive_evidence"
}

func containsNormalizedPhrase(value, phrase string) bool {
	valueRunes := []rune(normalizeIdentity(value))
	phraseRunes := []rune(normalizeIdentity(phrase))
	if len(valueRunes) == 0 || len(phraseRunes) == 0 || len(phraseRunes) > len(valueRunes) {
		return false
	}
	if containsCJK(string(phraseRunes)) {
		for start := 0; start+len(phraseRunes) <= len(valueRunes); start++ {
			if string(valueRunes[start:start+len(phraseRunes)]) == string(phraseRunes) {
				return true
			}
		}
		return false
	}
	for index := 0; index+len(phraseRunes) <= len(valueRunes); index++ {
		if string(valueRunes[index:index+len(phraseRunes)]) != string(phraseRunes) {
			continue
		}
		end := index + len(phraseRunes)
		beforeOK := index == 0 || !unicode.IsLetter(valueRunes[index-1]) && !unicode.IsDigit(valueRunes[index-1])
		afterOK := end == len(valueRunes) || !unicode.IsLetter(valueRunes[end]) && !unicode.IsDigit(valueRunes[end])
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func containsCJK(value string) bool {
	for _, r := range value {
		if (r >= 0x3400 && r <= 0x9fff) || (r >= 0xf900 && r <= 0xfaff) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func ContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type randomSelector struct{}

func NewResultSelector() ResultSelector { return randomSelector{} }

func (randomSelector) Select(ctx context.Context, input SelectionInput) (SelectionResult, error) {
	if err := ctx.Err(); err != nil {
		return SelectionResult{}, err
	}
	limit := input.Limit
	if limit <= 0 {
		limit = DefaultSelectionLimit
	}
	slots := input.ExplorationSlots
	if slots < 0 {
		slots = 0
	}
	if slots > limit {
		slots = limit
	}
	eligible := make([]CandidateEvidence, 0, len(input.Candidates))
	for _, candidate := range input.Candidates {
		if err := ctx.Err(); err != nil {
			return SelectionResult{}, err
		}
		if candidate.Eligible && candidate.Qualified {
			eligible = append(eligible, candidate)
		}
	}
	sortCanceled := false
	sort.SliceStable(eligible, func(i, j int) bool {
		if ctx.Err() != nil {
			sortCanceled = true
			return false
		}
		if eligible[i].Score != eligible[j].Score {
			return eligible[i].Score > eligible[j].Score
		}
		if eligible[i].ExactIdentityEvidence != eligible[j].ExactIdentityEvidence {
			return eligible[i].ExactIdentityEvidence
		}
		return eligible[i].Slug < eligible[j].Slug
	})
	if sortCanceled {
		return SelectionResult{}, ctx.Err()
	}
	selected := make(map[string]SelectedCandidate)
	if len(eligible) <= limit {
		for _, candidate := range eligible {
			if err := ctx.Err(); err != nil {
				return SelectionResult{}, err
			}
			selected[candidate.Slug] = selectionDecision(candidate, true, "selected", false)
		}
	} else {
		exploitCount := limit - slots
		for _, candidate := range eligible[:exploitCount] {
			if err := ctx.Err(); err != nil {
				return SelectionResult{}, err
			}
			selected[candidate.Slug] = selectionDecision(candidate, true, "selected", false)
		}
		remaining := eligible[exploitCount:]
		explorationCount := minInt(slots, len(remaining))
		if err := appendExplorationSelections(ctx, rand.New(rand.NewSource(input.Seed)), remaining, explorationCount, selected); err != nil {
			return SelectionResult{}, err
		}
	}
	decisions := make([]SelectedCandidate, 0, len(input.Candidates))
	for _, candidate := range input.Candidates {
		if err := ctx.Err(); err != nil {
			return SelectionResult{}, err
		}
		if decision, ok := selected[candidate.Slug]; ok {
			decisions = append(decisions, decision)
			continue
		}
		reason := "selection_omission"
		if !candidate.Eligible {
			reason = candidate.Rejection
			if reason == "" {
				reason = "ineligible"
			}
		} else if !candidate.Qualified {
			reason = candidate.QualificationReason
		}
		decisions = append(decisions, selectionDecision(candidate, false, reason, false))
	}
	return SelectionResult{Selected: decisions}, nil
}

func selectionDecision(candidate CandidateEvidence, selected bool, reason string, exploration bool) SelectedCandidate {
	tier := "standard"
	if exploration {
		tier = "exploration"
	} else if candidate.Score >= 3 {
		tier = "high"
	}
	return SelectedCandidate{Slug: candidate.Slug, Title: candidate.Title, Selected: selected, Reason: reason, Score: candidate.Score, Tier: tier, Exploration: exploration}
}

func appendExplorationSelections(ctx context.Context, rng *rand.Rand, candidates []CandidateEvidence, explorationCount int, selected map[string]SelectedCandidate) error {
	for index := 0; index < explorationCount; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(candidates) <= index {
			return nil
		}
		swap := index + rng.Intn(len(candidates)-index)
		candidates[index], candidates[swap] = candidates[swap], candidates[index]
		candidate := candidates[index]
		selected[candidate.Slug] = selectionDecision(candidate, true, "selected_for_exploration", true)
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
