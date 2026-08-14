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

type PlanExpander interface {
	ExpandPlan(context.Context, string, CriterionPolicy, []cache.Entry) (QueryPlan, error)
}

type ChatProvider interface {
	Chat(context.Context, string, string) (string, error)
}

type ExpansionInfo struct {
	Source         string
	FallbackReason string
}

type TracedPlanExpander interface {
	ExpandPlanWithTrace(context.Context, string, CriterionPolicy, []cache.Entry) (QueryPlan, ExpansionInfo, error)
}

type StructuredPlanExpander struct {
	provider ChatProvider
	fallback PlanExpander
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

func NewStructuredPlanExpander(provider ChatProvider, fallback PlanExpander) PlanExpander {
	return StructuredPlanExpander{provider: provider, fallback: fallback}
}

func (e StructuredPlanExpander) ExpandPlan(ctx context.Context, raw string, policy CriterionPolicy, entries []cache.Entry) (QueryPlan, error) {
	plan, _, err := e.ExpandPlanWithTrace(ctx, raw, policy, entries)
	return plan, err
}

func (e StructuredPlanExpander) ExpandPlanWithTrace(ctx context.Context, raw string, policy CriterionPolicy, entries []cache.Entry) (QueryPlan, ExpansionInfo, error) {
	if e.provider != nil {
		response, err := e.provider.Chat(ctx, structuredPlanSystemPrompt, structuredPlanUserPrompt(raw, policy))
		if err == nil {
			if plan, decodeErr := DecodePlan(response, raw); decodeErr == nil {
				return plan, ExpansionInfo{Source: "structured-llm"}, nil
			}
			if e.fallback == nil {
				return QueryPlan{}, ExpansionInfo{}, &ExpansionError{Reason: "invalid_plan"}
			}
			return e.fallbackPlan(ctx, raw, policy, entries, "invalid_plan")
		}
		if e.fallback == nil {
			return QueryPlan{}, ExpansionInfo{}, &ExpansionError{Reason: "provider_error", Err: err}
		}
		return e.fallbackPlan(ctx, raw, policy, entries, "provider_error")
	}
	if e.fallback == nil {
		return QueryPlan{}, ExpansionInfo{}, &ExpansionError{Reason: "provider_not_configured"}
	}
	return e.fallbackPlan(ctx, raw, policy, entries, "provider_not_configured")
}

func (e StructuredPlanExpander) fallbackPlan(ctx context.Context, raw string, policy CriterionPolicy, entries []cache.Entry, reason string) (QueryPlan, ExpansionInfo, error) {
	if e.fallback == nil {
		return QueryPlan{}, ExpansionInfo{}, errors.New("fallback unavailable")
	}
	plan, err := e.fallback.ExpandPlan(ctx, raw, policy, entries)
	if err != nil {
		return QueryPlan{}, ExpansionInfo{}, fmt.Errorf("fallback: %w", err)
	}
	plan.RawQuery = raw
	plan.Fallback = true
	if err := ValidateQueryPlan(plan); err != nil {
		return QueryPlan{}, ExpansionInfo{}, fmt.Errorf("fallback validation: %w", err)
	}
	return plan, ExpansionInfo{Source: "deterministic-fallback", FallbackReason: reason}, nil
}

const structuredPlanSystemPrompt = `Return one JSON query plan. Use only the fields raw_query, required, excluded, preferred, goals, supporting_dimensions, acceptable_alternatives, ambiguity, and fallback. Required and excluded criteria must be conservative: absence is not exclusion. Do not invent project ontology.`

func structuredPlanUserPrompt(raw string, policy CriterionPolicy) string {
	return fmt.Sprintf("query=%q policy_required=%v policy_preferred=%v policy_goals=%v", raw, policy.RequiredWhenExplicit, policy.PreferredByDefault, policy.GoalsToExpand)
}

func ValidateQueryPlan(plan QueryPlan) error {
	if strings.TrimSpace(plan.RawQuery) == "" {
		return errors.New("plan raw query is empty")
	}
	seenRequired := make(map[string]struct{})
	groups := []struct {
		name     string
		criteria []Criterion
	}{
		{"required", plan.Required}, {"excluded", plan.Excluded}, {"preferred", plan.Preferred},
		{"goals", plan.Goals}, {"supporting", plan.SupportingDimensions}, {"alternatives", plan.AcceptableAlternatives},
	}
	for _, group := range groups {
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
	return nil
}

func validateCriterion(criterion Criterion) error {
	if strings.TrimSpace(criterion.Kind) == "" || strings.TrimSpace(criterion.Value) == "" {
		return errors.New("kind and value are required")
	}
	if criterion.Proof != "" && criterion.Proof != "lexical" && criterion.Proof != "semantic" {
		return errors.New("unsupported proof")
	}
	if criterion.Proof != "semantic" && len(criterion.Terms) == 0 {
		return errors.New("lexical criterion requires terms")
	}
	return nil
}

func DecodePlan(response, raw string) (QueryPlan, error) {
	decoder := json.NewDecoder(strings.NewReader(response))
	decoder.DisallowUnknownFields()
	var plan QueryPlan
	if err := decoder.Decode(&plan); err != nil {
		return QueryPlan{}, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return QueryPlan{}, errors.New("trailing JSON")
		}
		return QueryPlan{}, fmt.Errorf("trailing JSON: %w", err)
	}
	plan.RawQuery = raw
	if err := ValidateQueryPlan(plan); err != nil {
		return QueryPlan{}, err
	}
	return plan, nil
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

type EligibilityMatcher interface {
	Match(context.Context, QueryPlan, []cache.Entry) (EligibilityResult, error)
}

type CandidateSelector interface {
	Select(context.Context, SelectionInput) (SelectionResult, error)
}

type Service struct {
	cache    *cache.Cache
	expander PlanExpander
	matcher  EligibilityMatcher
	selector CandidateSelector
	seedFor  func(string) int64
	options  Options
}

func NewService(expander PlanExpander, matcher EligibilityMatcher, selector CandidateSelector, seedFor func(string) int64) *Service {
	return NewServiceWithOptions(expander, matcher, selector, seedFor, DefaultOptions())
}

func NewServiceWithOptions(expander PlanExpander, matcher EligibilityMatcher, selector CandidateSelector, seedFor func(string) int64, options Options) *Service {
	if seedFor == nil {
		seedFor = ReproducibleSeed
	}
	return &Service{cache: cache.New(), expander: expander, matcher: matcher, selector: selector, seedFor: seedFor, options: options}
}

func NewExperimentExecutor(conceptCache *cache.Cache, provider ChatProvider, options Options) (query.Executor, error) {
	if conceptCache == nil {
		return nil, errors.New("three-host cache is nil")
	}
	options, err := NormalizeOptions(options)
	if err != nil {
		return nil, err
	}
	seedFor := options.SeedFor
	if seedFor == nil {
		seedFor = ReproducibleSeed
	}
	service := NewServiceWithOptions(NewStructuredPlanExpander(provider, NewDeterministicExpander()), NewLexicalMatcher(nil), NewSelector(), seedFor, options)
	service.cache = conceptCache
	return service, nil
}

func (s *Service) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	result, _, err := s.ExecuteWithTrace(ctx, reader, request)
	return result, err
}

func (s *Service) ExecuteWithTrace(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, *Trace, error) {
	if s == nil || s.cache == nil || s.expander == nil || s.matcher == nil || s.selector == nil {
		return query.Result{}, nil, errors.New("three-host service is incomplete")
	}
	entries, err := s.cache.All(ctx, reader)
	if err != nil {
		return query.Result{}, nil, errors.New("three-host corpus unavailable")
	}
	trace := &Trace{Variant: "three-host-v1"}
	started := time.Now()
	var plan QueryPlan
	info := ExpansionInfo{}
	if traced, ok := s.expander.(TracedPlanExpander); ok {
		plan, info, err = traced.ExpandPlanWithTrace(ctx, request.Query, DefaultCriterionPolicy, entries)
	} else {
		plan, err = s.expander.ExpandPlan(ctx, request.Query, DefaultCriterionPolicy, entries)
	}
	if err != nil {
		trace.Stages = append(trace.Stages, StageTrace{Name: "expansion", Outcome: "failure", ElapsedMS: elapsedSince(started), FallbackReason: "expansion_failure"})
		return query.Result{}, trace, err
	}
	if strings.TrimSpace(plan.RawQuery) == "" {
		plan.RawQuery = request.Query
	}
	if err := ValidateQueryPlan(plan); err != nil {
		trace.Stages = append(trace.Stages, StageTrace{Name: "expansion", Outcome: "invalid", ElapsedMS: elapsedSince(started), FallbackReason: "invalid_plan"})
		return query.Result{}, trace, errors.New("three-host expansion invalid")
	}
	trace.Stages = append(trace.Stages, StageTrace{
		Name: "expansion", Outcome: PlanOutcome(plan), Source: info.Source, FallbackReason: info.FallbackReason,
		ElapsedMS: elapsedSince(started), InputCount: 1, OutputCount: CriterionCount(plan), Plan: &plan,
	})

	seed := ReproducibleSeed(request.Query)
	if s.options.Seed != nil {
		seed = *s.options.Seed
	} else if s.seedFor != nil {
		seed = s.seedFor(request.Query)
	}
	trace.Seed = seed

	started = time.Now()
	eligible, err := s.matcher.Match(ctx, plan, entries)
	if err != nil {
		trace.Stages = append(trace.Stages, StageTrace{Name: "matching", Outcome: "failure", ElapsedMS: elapsedSince(started), InputCount: len(entries)})
		return query.Result{}, trace, errors.New("three-host matching failed")
	}
	trace.Stages = append(trace.Stages, StageTrace{Name: "matching", Outcome: "success", ElapsedMS: elapsedSince(started), InputCount: len(entries), OutputCount: EligibleCount(eligible.Candidates), TotalCount: len(eligible.Candidates), Candidates: eligible.Candidates})

	started = time.Now()
	selected, err := s.selector.Select(ctx, SelectionInput{Candidates: eligible.Candidates, Limit: s.options.SelectionLimit, ExplorationSlots: s.options.ExplorationSlots, Seed: seed})
	if err != nil {
		trace.Stages = append(trace.Stages, StageTrace{Name: "selection", Outcome: "failure", ElapsedMS: elapsedSince(started), InputCount: len(eligible.Candidates)})
		return query.Result{}, trace, errors.New("three-host selection failed")
	}
	trace.Stages = append(trace.Stages, StageTrace{Name: "selection", Outcome: "success", ElapsedMS: elapsedSince(started), InputCount: len(eligible.Candidates), OutputCount: selectedCount(selected.Selected), TotalCount: len(selected.Selected), Decisions: selected.Selected})
	results := make([]search.Result, 0, len(selected.Selected))
	for _, candidate := range selected.Selected {
		if candidate.Selected {
			results = append(results, search.Result{Slug: candidate.Slug, Title: candidate.Title, Type: "concept"})
		}
	}
	return query.Result{Query: request.Query, Mode: request.Mode, Results: results}, trace, nil
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

func NewDeterministicExpander() PlanExpander { return deterministicExpander{} }

func (deterministicExpander) ExpandPlan(_ context.Context, raw string, _ CriterionPolicy, _ []cache.Entry) (QueryPlan, error) {
	query := strings.Join(strings.Fields(raw), " ")
	if query == "" {
		return QueryPlan{}, errors.New("empty query")
	}
	plan := QueryPlan{RawQuery: raw, Fallback: true, Preferred: []Criterion{{Kind: "query", Value: query, Terms: queryTerms(query), Proof: "lexical"}}}
	return plan, ValidateQueryPlan(plan)
}

type SemanticDecision struct{ Outcome string }

type SemanticEvaluator interface {
	Evaluate(context.Context, Criterion, cache.Entry) (SemanticDecision, error)
}

type lexicalMatcher struct{ semantic SemanticEvaluator }

func NewLexicalMatcher(semantic SemanticEvaluator) EligibilityMatcher {
	return lexicalMatcher{semantic: semantic}
}

func (m lexicalMatcher) Match(ctx context.Context, plan QueryPlan, entries []cache.Entry) (EligibilityResult, error) {
	candidates := make([]CandidateEvidence, 0, len(entries))
	for _, entry := range entries {
		candidate := CandidateEvidence{Slug: entry.Slug, Title: entry.Title, Eligible: true}
		fields := searchableFields(entry)
		for _, role := range []struct {
			name       string
			criteria   []Criterion
			failClosed bool
		}{
			{"required", plan.Required, semanticRequiredFailClosed}, {"excluded", plan.Excluded, semanticExcludedFailClosed},
			{"preferred", plan.Preferred, false}, {"goal", plan.Goals, false}, {"supporting", plan.SupportingDimensions, false}, {"alternative", plan.AcceptableAlternatives, false},
		} {
			groups, err := matchRole(ctx, role.name, role.criteria, fields, entry, m.semantic)
			if err != nil {
				return EligibilityResult{}, err
			}
			candidate.Groups = append(candidate.Groups, groups...)
			for _, group := range groups {
				if group.SemanticOutcome == "unavailable" || group.SemanticOutcome == "unknown" {
					candidate.SemanticOutcome = group.SemanticOutcome
				}
				if group.SemanticOutcome != "" && group.SemanticOutcome != "pass" && role.failClosed {
					candidate.Eligible = false
					candidate.Rejection = "semantic_" + role.name + "_unavailable"
				}
				if role.name == "excluded" && group.SemanticOutcome == "pass" {
					candidate.Eligible = false
					candidate.Rejection = "excluded_criterion_matched"
				}
			}
		}
		for _, criterion := range plan.Required {
			if criterion.Proof != "semantic" && !hasMatchedGroup(candidate.Groups, criterion) {
				candidate.Eligible = false
				candidate.Rejection = "required_" + criterion.Kind + "_not_matched"
				break
			}
		}
		if candidate.Eligible && hasAnyMatchedGroup(candidate.Groups, plan.Excluded) {
			candidate.Eligible = false
			candidate.Rejection = "excluded_criterion_matched"
		}
		candidate.Score = independentDimensionScore(plan, candidate.Groups)
		candidates = append(candidates, candidate)
	}
	return EligibilityResult{Candidates: candidates}, nil
}

func matchRole(ctx context.Context, role string, criteria []Criterion, fields map[string]string, entry cache.Entry, evaluator SemanticEvaluator) ([]GroupEvidence, error) {
	groups := make([]GroupEvidence, 0, len(criteria))
	for _, criterion := range criteria {
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
		groups = append(groups, matchGroupsForRole([]Criterion{criterion}, role, fields)...)
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
	return matchGroupsForRole(criteria, "", fields)
}

func matchGroupsForRole(criteria []Criterion, role string, fields map[string]string) []GroupEvidence {
	groups := make([]GroupEvidence, 0, len(criteria))
	for _, criterion := range criteria {
		if criterion.Proof == "semantic" {
			groups = append(groups, GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value, SemanticOutcome: "unavailable"})
			continue
		}
		group := GroupEvidence{Role: role, Kind: criterion.Kind, Value: criterion.Value}
		for field, value := range fields {
			matched := make([]string, 0)
			for _, term := range criterion.Terms {
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
	return groups
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

func NewSelector() CandidateSelector { return randomSelector{} }

func (randomSelector) Select(_ context.Context, input SelectionInput) (SelectionResult, error) {
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
		if candidate.Eligible {
			eligible = append(eligible, candidate)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Score != eligible[j].Score {
			return eligible[i].Score > eligible[j].Score
		}
		return eligible[i].Slug < eligible[j].Slug
	})
	selected := make(map[string]SelectedCandidate)
	if len(eligible) <= limit {
		for _, candidate := range eligible {
			selected[candidate.Slug] = selectionDecision(candidate, true, "selected", false)
		}
	} else {
		exploitCount := limit - slots
		for _, candidate := range eligible[:exploitCount] {
			selected[candidate.Slug] = selectionDecision(candidate, true, "selected", false)
		}
		remaining := eligible[exploitCount:]
		picks := rand.New(rand.NewSource(input.Seed)).Perm(len(remaining))
		for _, index := range picks[:minInt(slots, len(remaining))] {
			candidate := remaining[index]
			selected[candidate.Slug] = selectionDecision(candidate, true, "selected_for_exploration", true)
		}
	}
	decisions := make([]SelectedCandidate, 0, len(input.Candidates))
	for _, candidate := range input.Candidates {
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
