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
)

const (
	DefaultSelectionLimit      = 10
	maxSelectionLimit          = 1000
	semanticRequiredFailClosed = true
	semanticExcludedFailClosed = true
)

type Options struct {
	SelectionLimit   int
	ExplorationSlots int
	Seed             *int64
	SeedFor          func(string) int64
}

func DefaultOptions() Options {
	return Options{SelectionLimit: DefaultSelectionLimit, ExplorationSlots: 1}
}

func NormalizeOptions(options Options) (Options, error) {
	if options.SelectionLimit == 0 {
		options.SelectionLimit = DefaultSelectionLimit
	}
	if options.SelectionLimit < 1 || options.SelectionLimit > maxSelectionLimit {
		return Options{}, fmt.Errorf("selection limit must be between 1 and %d", maxSelectionLimit)
	}
	if options.ExplorationSlots < 0 || options.ExplorationSlots > options.SelectionLimit {
		return Options{}, fmt.Errorf("exploration slots must be between 0 and %d", options.SelectionLimit)
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
	RawQuery               string      `json:"raw_query"`
	Required               []Criterion `json:"required,omitempty"`
	Excluded               []Criterion `json:"excluded,omitempty"`
	Preferred              []Criterion `json:"preferred,omitempty"`
	Goals                  []Criterion `json:"goals,omitempty"`
	SupportingDimensions   []Criterion `json:"supporting_dimensions,omitempty"`
	AcceptableAlternatives []Criterion `json:"acceptable_alternatives,omitempty"`
	Ambiguity              []string    `json:"ambiguity,omitempty"`
	Fallback               bool        `json:"fallback,omitempty"`
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
	Slug            string          `json:"slug"`
	Title           string          `json:"title"`
	Eligible        bool            `json:"eligible"`
	Rejection       string          `json:"rejection,omitempty"`
	Groups          []GroupEvidence `json:"groups,omitempty"`
	SemanticOutcome string          `json:"semantic_outcome,omitempty"`
	Score           int             `json:"score"`
}

type EligibilityResult struct{ Candidates []CandidateEvidence }

type ExpansionRequest struct {
	Query           string
	CriterionPolicy CriterionPolicy
}

type MatchRequest struct {
	Plan          QueryPlan
	CorpusEntries []cache.Entry
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
	Source         string
	FallbackReason string
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
		response, err := e.provider.Chat(ctx, structuredPlanSystemPrompt, structuredPlanUserPrompt(request.Query, request.CriterionPolicy))
		if err == nil {
			if plan, decodeErr := decodePlan(response, request.Query); decodeErr == nil {
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

const (
	StructuredPlanPromptID     = "minimal-v1"
	structuredPlanSystemPrompt = `You produce a retrieval plan for a frozen Lifestyle concept corpus. Return exactly one JSON object and no markdown. The object fields and exact types are: raw_query string; required array of Criterion; excluded array of Criterion; preferred array of Criterion; goals array of Criterion; supporting_dimensions array of Criterion; acceptable_alternatives array of Criterion; ambiguity array of strings; fallback boolean. Every Criterion is exactly {kind:string,value:string,terms:array of strings,proof:"lexical" or "semantic"}. Every lexical Criterion needs at least one discovery term. Never output a string where an array or object is required. Be conservative: only explicit user constraints may be required or excluded; absent never means excluded. In this minimal variant, supporting_dimensions and acceptable_alternatives must be empty arrays and fallback must be false.`
	structuredPlanUserTemplate = "Raw query: {{raw_query}}\nCriterion policy: {{criterion_policy}}\nInterpret the query into required, excluded, preferred and goals. Preserve the raw query exactly in raw_query. Return the single JSON object only."
)

func structuredPlanUserPrompt(raw string, policy CriterionPolicy) string {
	rawJSON, _ := json.Marshal(raw)
	policyJSON, _ := json.Marshal(policy)
	result := strings.ReplaceAll(structuredPlanUserTemplate, "{{raw_query}}", string(rawJSON))
	return strings.ReplaceAll(result, "{{criterion_policy}}", string(policyJSON))
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
	return strings.ToLower(strings.TrimSpace(criterion.Kind) + "\x00" + strings.TrimSpace(criterion.Value))
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
	service := NewQueryRetrievalPipelineWithOptions(NewStructuredPlanExpander(provider, NewDeterministicExpander()), NewLexicalMatcher(nil), NewResultSelector(), seedFor, options)
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
	trace := &Trace{Variant: "query-retrieval-v1"}
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
	matchReq := MatchRequest{Plan: plan, CorpusEntries: entries}
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
	appendTraceStage(trace, StageTrace{Name: "matching", Outcome: "success", ElapsedMS: elapsedSince(started), InputCount: len(entries), OutputCount: EligibleCount(eligible.Candidates), TotalCount: len(eligible.Candidates), Candidates: eligible.Candidates})
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
	appendTraceStage(trace, StageTrace{Name: "selection", Outcome: "success", ElapsedMS: elapsedSince(started), InputCount: len(eligible.Candidates), OutputCount: selectedCount(selected.Selected), TotalCount: len(selected.Selected), Decisions: selected.Selected})
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
	return query.Result{Query: request.Query, Mode: request.Mode, Results: results, Expand: expandFromPlan(plan)}, nil
}

func providerIdentity(expander QueryExpander) (string, string, string) {
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
	Variant string       `json:"variant"`
	Seed    int64        `json:"seed"`
	Stages  []StageTrace `json:"stages"`
}

type StageTrace struct {
	Name           string              `json:"name"`
	Outcome        string              `json:"outcome"`
	Source         string              `json:"source,omitempty"`
	ElapsedMS      int64               `json:"elapsed_ms"`
	InputCount     int                 `json:"input_count"`
	OutputCount    int                 `json:"output_count"`
	TotalCount     int                 `json:"total_count,omitempty"`
	FallbackReason string              `json:"fallback_reason,omitempty"`
	Plan           *QueryPlan          `json:"plan,omitempty"`
	Candidates     []CandidateEvidence `json:"candidates,omitempty"`
	Decisions      []SelectedCandidate `json:"decisions,omitempty"`
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
	candidates := make([]CandidateEvidence, 0, len(request.CorpusEntries))
	for _, entry := range request.CorpusEntries {
		if err := ctx.Err(); err != nil {
			return EligibilityResult{}, err
		}
		candidate := CandidateEvidence{Slug: entry.Slug, Title: entry.Title, Eligible: true}
		fields := searchableFields(entry)
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
				if group.SemanticOutcome == "unavailable" || group.SemanticOutcome == "unknown" {
					candidate.SemanticOutcome = group.SemanticOutcome
				}
				if role.name == "required" && group.SemanticOutcome != "" && group.SemanticOutcome != "pass" && role.failClosed {
					candidate.Eligible = false
					candidate.Rejection = "semantic_required_not_satisfied"
				}
				if role.name == "excluded" && group.SemanticOutcome == "pass" {
					candidate.Eligible = false
					candidate.Rejection = "excluded_criterion_matched"
				}
				if role.name == "excluded" && (group.SemanticOutcome == "unknown" || group.SemanticOutcome == "unavailable") {
					candidate.Eligible = false
					candidate.Rejection = "semantic_excluded_unavailable"
				}
			}
		}
		for _, criterion := range request.Plan.Required {
			if criterion.Proof != "semantic" && !hasMatchedGroup(candidate.Groups, criterion) {
				candidate.Eligible = false
				candidate.Rejection = "required_" + criterion.Kind + "_not_matched"
				break
			}
		}
		if candidate.Eligible && hasAnyMatchedGroup(candidate.Groups, request.Plan.Excluded) {
			candidate.Eligible = false
			candidate.Rejection = "excluded_criterion_matched"
		}
		candidate.Score = independentDimensionScore(request.Plan, candidate.Groups)
		candidates = append(candidates, candidate)
	}
	return EligibilityResult{Candidates: candidates}, nil
}

func matchRole(ctx context.Context, role string, criteria []Criterion, fields map[string]string, entry cache.Entry, evaluator SemanticEvaluator) ([]GroupEvidence, error) {
	groups := make([]GroupEvidence, 0, len(criteria))
	for _, criterion := range criteria {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if criterion.Proof == "semantic" {
			outcome := "unavailable"
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
	case "pass", "fail", "unknown", "unavailable":
		return outcome
	default:
		return "unknown"
	}
}

func searchableFields(entry cache.Entry) map[string]string {
	frontmatter, _ := json.Marshal(entry.Frontmatter)
	return map[string]string{"title": entry.Title, "body": entry.Body, "frontmatter": string(frontmatter)}
}

func matchGroups(criteria []Criterion, fields map[string]string) []GroupEvidence {
	groups, _ := matchGroupsForRole(context.Background(), criteria, "", fields)
	return groups
}

func matchGroupsForRole(ctx context.Context, criteria []Criterion, role string, fields map[string]string) ([]GroupEvidence, error) {
	groups := make([]GroupEvidence, 0, len(criteria))
	for _, criterion := range criteria {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if criterion.Proof == "semantic" {
			groups = append(groups, GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value, SemanticOutcome: "unavailable"})
			continue
		}
		group := GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value}
		for field, value := range fields {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			matched := make([]string, 0)
			for _, term := range criterion.Terms {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if strings.Contains(strings.ToLower(value), strings.ToLower(term)) && !ContainsString(matched, term) {
					matched = append(matched, term)
				}
			}
			if len(matched) > 0 {
				group.Matches = append(group.Matches, FieldEvidence{Field: field, Terms: matched})
			}
		}
		if len(group.Matches) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

func hasMatchedGroup(groups []GroupEvidence, criterion Criterion) bool {
	for _, group := range groups {
		if group.Kind == criterion.Kind && group.Value == criterion.Value && len(group.Matches) > 0 {
			return true
		}
	}
	return false
}

func hasAnyMatchedGroup(groups []GroupEvidence, criteria []Criterion) bool {
	for _, criterion := range criteria {
		if hasMatchedGroup(groups, criterion) {
			return true
		}
	}
	return false
}

func independentDimensionScore(plan QueryPlan, groups []GroupEvidence) int {
	criteria := append([]Criterion{}, plan.Preferred...)
	criteria = append(criteria, plan.SupportingDimensions...)
	criteria = append(criteria, plan.Goals...)
	criteria = append(criteria, plan.AcceptableAlternatives...)
	seen := make(map[string]struct{})
	for _, criterion := range criteria {
		if criterion.Proof != "semantic" && hasMatchedGroup(groups, criterion) {
			seen[strings.ToLower(strings.TrimSpace(criterion.Kind))] = struct{}{}
		}
	}
	return len(seen)
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
		if candidate.Eligible {
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
			reason = "ineligible"
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
