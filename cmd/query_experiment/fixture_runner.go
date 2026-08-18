package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/buildinfo"
	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

const underFiveThreshold = 5

func (options experimentOptions) fixtureFlagsSet() bool {
	return options.modelFixturePath != "" || options.models != "" || options.profileFixturePath != "" || options.profiles != "" || options.promptFixturePath != "" || options.prompts != "" || options.artifactsDir != "" || options.summaryPath != ""
}

func (options experimentOptions) validateFixtureFlags() error {
	if options.modelFixturePath == "" || options.profileFixturePath == "" || options.promptFixturePath == "" {
		return errors.New("query-retrieval fixture mode requires --model-fixture, --profile-fixture, and --prompt-fixture")
	}
	if strings.TrimSpace(options.artifactsDir) == "" {
		return errors.New("query-retrieval fixture mode requires --artifacts-dir")
	}
	return validateOutputPath(options.summaryPath)
}

type fixtureReceiptMeta struct {
	AttemptID         string `json:"attempt_id"`
	VariantID         string `json:"variant_id"`
	ProfileID         string `json:"profile_id"`
	PromptID          string `json:"prompt_id"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	CaseID            string `json:"case_id"`
	RunIndex          int    `json:"run_index"`
	EvidenceThreshold int    `json:"evidence_threshold"`
}

type fixtureRequestReceipt struct {
	Query                           string `json:"query"`
	Mode                            string `json:"mode"`
	SnapshotIdentity                string `json:"snapshot_identity"`
	CorpusSHA256                    string `json:"corpus_sha256"`
	SelectionLimit                  int    `json:"selection_limit"`
	ExplorationSlots                int    `json:"exploration_slots"`
	EvidenceThreshold               int    `json:"evidence_threshold"`
	KeywordsPerAttempt              int    `json:"keywords_per_attempt"`
	ExpansionAttempts               int    `json:"expansion_attempts"`
	RareKeywordMaxDocumentFrequency int    `json:"rare_keyword_max_document_frequency"`
	SeedMode                        string `json:"seed_mode"`
	Model                           string `json:"model"`
}

type fixtureExpansionInput struct {
	Query              string          `json:"query"`
	CriterionPolicy    CriterionPolicy `json:"criterion_policy"`
	PromptID           string          `json:"prompt_id"`
	Provider           string          `json:"provider"`
	Model              string          `json:"model"`
	BaseURL            string          `json:"base_url"`
	SystemPrompt       string          `json:"system_prompt"`
	UserPrompt         string          `json:"user_prompt"`
	KeywordsPerAttempt int             `json:"keywords_per_attempt"`
	ExpansionAttempts  int             `json:"expansion_attempts"`
}

type fixtureExpansionOutput struct {
	RawModelResponse       string                           `json:"raw_model_response,omitempty"`
	ParsedPlan             QueryPlan                        `json:"parsed_plan"`
	Source                 string                           `json:"source"`
	Validation             string                           `json:"validation"`
	Fallback               bool                             `json:"fallback"`
	FallbackReason         string                           `json:"fallback_reason,omitempty"`
	Error                  string                           `json:"error,omitempty"`
	LatencyMS              int64                            `json:"latency_ms"`
	Usage                  fixtureUsage                     `json:"usage,omitempty"`
	RequestedAttempts      int                              `json:"requested_attempts"`
	SuccessfulAttempts     int                              `json:"successful_attempts"`
	ProviderFailedAttempts int                              `json:"provider_failed_attempts"`
	FallbackCount          int                              `json:"fallback_count"`
	KeywordsPerAttempt     int                              `json:"keywords_per_attempt"`
	KeywordSupport         []queryquality.KeywordSupport    `json:"keyword_support,omitempty"`
	Attempts               []fixtureExpansionAttemptReceipt `json:"attempts"`
}

type fixtureExpansionAttemptReceipt struct {
	AttemptIndex int          `json:"attempt_index"`
	Outcome      string       `json:"outcome"`
	LatencyMS    int64        `json:"latency_ms"`
	Usage        fixtureUsage `json:"usage,omitempty"`
}

type fixtureModelExpander struct {
	model  modelFixtureEntry
	system string
	user   string
	mu     sync.Mutex
	calls  map[int]fixtureModelCall
}

func (e *fixtureModelExpander) Expand(ctx context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	call, err := callFixtureModel(ctx, e.model, e.system, e.user)
	e.mu.Lock()
	e.calls[request.Attempt] = call
	e.mu.Unlock()
	if err != nil {
		return queryquality.QueryPlan{}, err
	}
	return decodeStructuredPlan(call.Content, request.Query)
}

func (e *fixtureModelExpander) call(attempt int) fixtureModelCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[attempt]
}

type fixtureMatchingInput struct {
	Plan                            QueryPlan      `json:"plan"`
	SnapshotIdentity                string         `json:"snapshot_identity"`
	CorpusSHA256                    string         `json:"corpus_sha256"`
	Parameters                      map[string]any `json:"parameters"`
	EvidenceThreshold               int            `json:"evidence_threshold"`
	RareKeywordMaxDocumentFrequency int            `json:"rare_keyword_max_document_frequency"`
	FallbackQualificationAllowed    bool           `json:"fallback_qualification_allowed"`
}

type fixtureMatchingOutput struct {
	CandidateIdentities []resultIdentity    `json:"candidate_identities"`
	Candidates          []CandidateEvidence `json:"candidates"`
}

type fixtureSelectionInput struct {
	Candidates        []CandidateEvidence `json:"candidates"`
	Limit             int                 `json:"limit"`
	ExplorationSlots  int                 `json:"exploration_slots"`
	EvidenceThreshold int                 `json:"evidence_threshold"`
	EffectiveSeed     int64               `json:"effective_seed"`
}

type fixtureSelectionOutput struct {
	Decisions         []SelectedCandidate `json:"decisions"`
	FinalOrder        []string            `json:"final_order"`
	EvidenceThreshold int                 `json:"evidence_threshold"`
}

type fixtureFinalReceipt struct {
	Outcome         string            `json:"outcome"`
	Status          string            `json:"status"`
	Reason          string            `json:"reason"`
	FinalIdentities []resultIdentity  `json:"final_identities"`
	Receipts        map[string]string `json:"receipts"`
	QueryReceivedAt string            `json:"query_received_at"`
	RunCompletedAt  string            `json:"run_completed_at"`
	DurationMS      int64             `json:"duration_ms"`
	Error           string            `json:"error,omitempty"`
}

type fixtureAttempt struct {
	Record               resultRecord
	Case                 caseInput
	Candidates           []CandidateEvidence
	Decisions            []SelectedCandidate
	EffectiveSeed        int64
	SelectionInputDigest string
	Fallback             bool
	LatencyMS            int64
	Usage                fixtureUsage
	EvidenceThreshold    int
}

func runFixtureExperiment(ctx context.Context, options experimentOptions, prepared preparedSnapshot, cases []caseInput, deps dependencies) error {
	models, err := readModelFixture(options.modelFixturePath)
	if err != nil {
		return err
	}
	profiles, err := readProfileFixture(options.profileFixturePath)
	if err != nil {
		return err
	}
	prompts, err := readPromptFixture(options.promptFixturePath)
	if err != nil {
		return err
	}
	variants, err := selectFixtureMatrix(models, profiles, prompts, options.models, options.profiles, options.prompts)
	if err != nil {
		return err
	}
	retrievalOptions := defaultQueryRetrievalOptions()
	retrievalOptions.selectionLimit = options.selectionLimit
	if options.keywordsPerAttempt > 0 {
		retrievalOptions.keywordsPerAttempt = options.keywordsPerAttempt
	}
	if options.expansionAttempts > 0 {
		retrievalOptions.expansionAttempts = options.expansionAttempts
	}
	if options.rareDocumentFrequency > 0 {
		retrievalOptions.rareDocumentFrequency = options.rareDocumentFrequency
	}
	if options.explorationSlotsSet {
		retrievalOptions.explorationSlots = options.explorationSlots
	}
	if options.evidenceThresholdSet {
		retrievalOptions.evidenceThreshold = options.evidenceThreshold
		retrievalOptions.evidenceThresholdSet = true
	}
	retrievalOptions.seed = options.seed
	retrievalOptions, err = normalizeQueryRetrievalOptions(retrievalOptions)
	if err != nil {
		return err
	}
	options.selectionLimit = retrievalOptions.selectionLimit
	options.explorationSlots = retrievalOptions.explorationSlots
	options.keywordsPerAttempt = retrievalOptions.keywordsPerAttempt
	options.expansionAttempts = retrievalOptions.expansionAttempts
	options.rareDocumentFrequency = retrievalOptions.rareDocumentFrequency
	if err := os.MkdirAll(filepath.Clean(options.artifactsDir), 0o755); err != nil {
		return fmt.Errorf("create artifacts directory: %w", err)
	}
	entries, err := prepared.cache.All(ctx, prepared.reader)
	if err != nil {
		return fmt.Errorf("snapshot corpus: %w", err)
	}
	now := deps.now
	if now == nil {
		now = time.Now
	}
	stdout := deps.stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	var sink recordSink
	if deps.openOutput != nil {
		sink, err = deps.openOutput(options.outputPath, stdout)
	} else {
		sink, err = openWriterOutput(options.outputPath, stdout)
	}
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			sink.Abort()
		}
	}()
	metadata := buildRecordMetadata(config.Config{})
	metadata.sourceRevision = buildinfo.Current().Commit
	attempts := make([]fixtureAttempt, 0, len(variants)*len(cases)*options.runs)
	for i := range variants {
		variants[i].VariantID = fixtureVariantID(variants[i].VariantID, options, prepared)
		for _, input := range cases {
			for runIndex := 1; runIndex <= options.runs; runIndex++ {
				attempt, err := runFixtureAttempt(ctx, options, retrievalOptions, variants[i], input, runIndex, prepared, entries, now, metadata)
				if err != nil {
					return err
				}
				if err := sink.WriteRecord(attempt.Record); err != nil {
					return fmt.Errorf("write result %d/%d for %q: %w", runIndex, options.runs, input.ID, err)
				}
				attempts = append(attempts, attempt)
			}
		}
	}
	if err := sink.Finish(); err != nil {
		return fmt.Errorf("finalize output: %w", err)
	}
	finished = true
	if options.summaryPath != "" {
		if err := writeFixtureSummary(options.summaryPath, attempts); err != nil {
			return err
		}
	}
	return nil
}

func fixtureVariantID(base string, options experimentOptions, prepared preparedSnapshot) string {
	seed := "auto"
	if options.seed != nil {
		seed = fmt.Sprintf("%d", *options.seed)
	}
	threshold := options.evidenceThreshold
	if !options.evidenceThresholdSet {
		threshold = queryquality.DefaultEvidenceThreshold
	}
	return fmt.Sprintf("%s__limit=%d__exploration=%d__threshold=%d__keywords=%d__attempts=%d__rare-df=%d__seed=%s__corpus=%s", base, options.selectionLimit, options.explorationSlots, threshold, options.keywordsPerAttempt, options.expansionAttempts, options.rareDocumentFrequency, seed, prepared.digest)
}

func runFixtureAttempt(ctx context.Context, options experimentOptions, retrievalOptions queryRetrievalOptions, variant fixtureVariant, input caseInput, runIndex int, prepared preparedSnapshot, entries []cache.Entry, now func() time.Time, metadata recordMetadata) (fixtureAttempt, error) {
	attemptID := fmt.Sprintf("%s__case=%s__run=%d", variant.VariantID, input.ID, runIndex)
	meta := fixtureReceiptMeta{AttemptID: attemptID, VariantID: variant.VariantID, ProfileID: variant.Profile.ID, PromptID: variant.Prompt.ID, Provider: variant.Model.Provider, Model: variant.Model.Model, CaseID: input.ID, RunIndex: runIndex, EvidenceThreshold: retrievalOptions.evidenceThreshold}
	dir := filepath.Join(filepath.Clean(options.artifactsDir), variant.VariantID, receiptSegment(input.ID), fmt.Sprintf("run-%d", runIndex))
	queryReceivedAt := now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fixtureAttempt{}, fmt.Errorf("create attempt artifacts: %w", err)
	}
	write := func(name string, payload any) error {
		return writeFixtureReceipt(filepath.Join(dir, name), meta, payload, variant.Model.APIKey)
	}
	seed := reproducibleSeed(input.Query)
	seedMode := "query-derived"
	if retrievalOptions.seed != nil {
		seed = *retrievalOptions.seed
		seedMode = "explicit"
	}
	if err := write("request.json", fixtureRequestReceipt{Query: input.Query, Mode: input.Mode, SnapshotIdentity: prepared.label, CorpusSHA256: prepared.digest, SelectionLimit: retrievalOptions.selectionLimit, ExplorationSlots: retrievalOptions.explorationSlots, EvidenceThreshold: retrievalOptions.evidenceThreshold, KeywordsPerAttempt: retrievalOptions.keywordsPerAttempt, ExpansionAttempts: retrievalOptions.expansionAttempts, RareKeywordMaxDocumentFrequency: retrievalOptions.rareDocumentFrequency, SeedMode: seedMode, Model: variant.Model.Model}); err != nil {
		return fixtureAttempt{}, err
	}
	policy := CriterionPolicy{RequiredWhenExplicit: append([]string(nil), variant.Profile.RequiredWhenExplicit...), PreferredByDefault: append([]string(nil), variant.Profile.PreferredByDefault...), GoalsToExpand: append([]string(nil), variant.Profile.GoalsToExpand...)}
	rendered, err := renderFixturePrompt(variant.Prompt, input.Query, policy)
	if err != nil {
		return fixtureAttempt{}, fmt.Errorf("render prompt: %w", err)
	}
	rendered.User += fmt.Sprintf("\nMaximum normalized positive discovery keywords for this attempt: %d.", retrievalOptions.keywordsPerAttempt)
	if err := write("expansion.input.json", fixtureExpansionInput{Query: input.Query, CriterionPolicy: policy, PromptID: variant.Prompt.ID, Provider: variant.Model.Provider, Model: variant.Model.Model, BaseURL: variant.Model.BaseURL, SystemPrompt: rendered.System, UserPrompt: rendered.User, KeywordsPerAttempt: retrievalOptions.keywordsPerAttempt, ExpansionAttempts: retrievalOptions.expansionAttempts}); err != nil {
		return fixtureAttempt{}, err
	}
	expander := &fixtureModelExpander{model: variant.Model, system: rendered.System, user: rendered.User, calls: make(map[int]fixtureModelCall)}
	parallel, err := queryquality.NewParallelQueryExpander(expander, newDeterministicExpander(), toQueryRetrievalCoreOptions(retrievalOptions))
	if err != nil {
		return fixtureAttempt{}, err
	}
	expansionStarted := now()
	plan, expansionInfo, expansionErr := parallel.(queryquality.TracedQueryExpander).ExpandWithTrace(ctx, queryquality.ExpansionRequest{Query: input.Query, CriterionPolicy: policy})
	expansionElapsed := elapsedBetween(now(), expansionStarted)
	if expansionErr != nil {
		return fixtureAttempt{}, fmt.Errorf("fixture expansion: %w", expansionErr)
	}
	firstCall := fixtureModelCall{}
	for attempt := 1; attempt <= expansionInfo.RequestedAttempts; attempt++ {
		if call := expander.call(attempt); call.RawResponse != "" || call.Content != "" || call.LatencyMS != 0 {
			firstCall = call
			break
		}
	}
	attemptReceipts := make([]fixtureExpansionAttemptReceipt, 0, expansionInfo.RequestedAttempts)
	var expansionUsage fixtureUsage
	for _, outcome := range expansionInfo.AttemptOutcomes {
		call := expander.call(outcome.AttemptIndex)
		attemptReceipts = append(attemptReceipts, fixtureExpansionAttemptReceipt{AttemptIndex: outcome.AttemptIndex, Outcome: outcome.Outcome, LatencyMS: call.LatencyMS, Usage: call.Usage})
		expansionUsage = addFixtureUsage(expansionUsage, call.Usage)
	}
	validation := "valid"
	if plan.Fallback {
		validation = "fallback"
	}
	if err := write("expansion.output.json", fixtureExpansionOutput{RawModelResponse: scrubSecret(firstCall.RawResponse, variant.Model.APIKey), ParsedPlan: plan, Source: expansionInfo.Source, Validation: validation, Fallback: plan.Fallback, FallbackReason: expansionInfo.FallbackReason, Error: "", LatencyMS: expansionElapsed, Usage: expansionUsage, RequestedAttempts: expansionInfo.RequestedAttempts, SuccessfulAttempts: expansionInfo.SuccessfulAttempts, ProviderFailedAttempts: expansionInfo.ProviderFailedAttempts, FallbackCount: expansionInfo.FallbackCount, KeywordsPerAttempt: expansionInfo.KeywordsPerAttempt, KeywordSupport: plan.KeywordSupport, Attempts: attemptReceipts}); err != nil {
		return fixtureAttempt{}, err
	}
	trace := &queryRetrievalTrace{Variant: variant.VariantID, Seed: seed, Stages: []stageTrace{{Name: "expansion", Outcome: planOutcome(plan), Source: expansionInfo.Source, FallbackReason: expansionInfo.FallbackReason, ElapsedMS: expansionElapsed, InputCount: expansionInfo.RequestedAttempts, OutputCount: criterionCount(plan), Plan: &plan}}}
	trace.Expansion = queryquality.ExpansionTrace{RequestedAttempts: expansionInfo.RequestedAttempts, SuccessfulAttempts: expansionInfo.SuccessfulAttempts, ProviderFailedAttempts: expansionInfo.ProviderFailedAttempts, FallbackCount: expansionInfo.FallbackCount, KeywordsPerAttempt: expansionInfo.KeywordsPerAttempt, RareKeywordMaxDocumentFrequency: retrievalOptions.rareDocumentFrequency, EvidenceThreshold: retrievalOptions.evidenceThreshold, KeywordSupport: append([]queryquality.KeywordSupport(nil), plan.KeywordSupport...)}
	matchReq := queryquality.MatchRequest{
		Plan: plan, CorpusEntries: entries, EvidenceThreshold: retrievalOptions.evidenceThreshold, EvidenceThresholdSet: true,
		RareKeywordMaxDocumentFrequency: retrievalOptions.rareDocumentFrequency, FallbackQualificationAllowed: false,
	}
	if err := write("matching.input.json", fixtureMatchingInput{
		Plan: plan, SnapshotIdentity: prepared.label, CorpusSHA256: prepared.digest, EvidenceThreshold: matchReq.EvidenceThreshold, RareKeywordMaxDocumentFrequency: matchReq.RareKeywordMaxDocumentFrequency, FallbackQualificationAllowed: matchReq.FallbackQualificationAllowed,
		Parameters: map[string]any{"semantic_required_fail_closed": semanticRequiredFailClosed, "semantic_excluded_fail_closed": semanticExcludedFailClosed},
	}); err != nil {
		return fixtureAttempt{}, err
	}
	matchingStarted := now()
	eligible, err := newLexicalMatcher(nil).Match(ctx, matchReq)
	matchingElapsed := elapsedBetween(now(), matchingStarted)
	if err != nil {
		return fixtureAttempt{}, err
	}
	identities := make([]resultIdentity, 0, len(eligible.Candidates))
	for _, candidate := range eligible.Candidates {
		identities = append(identities, resultIdentity{Slug: candidate.Slug, Title: candidate.Title, Type: "concept"})
	}
	trace.EvidenceThreshold = retrievalOptions.evidenceThreshold
	trace.Stages = append(trace.Stages, stageTrace{Name: "matching", Outcome: "success", ElapsedMS: matchingElapsed, InputCount: len(entries), OutputCount: queryquality.QualifiedCount(eligible.Candidates), TotalCount: len(eligible.Candidates), Candidates: eligible.Candidates, EvidenceThreshold: retrievalOptions.evidenceThreshold})
	if err := write("matching.output.json", fixtureMatchingOutput{CandidateIdentities: identities, Candidates: eligible.Candidates}); err != nil {
		return fixtureAttempt{}, err
	}
	selectionInput := fixtureSelectionInput{Candidates: eligible.Candidates, Limit: retrievalOptions.selectionLimit, ExplorationSlots: retrievalOptions.explorationSlots, EvidenceThreshold: retrievalOptions.evidenceThreshold, EffectiveSeed: seed}
	if err := write("selection.input.json", selectionInput); err != nil {
		return fixtureAttempt{}, err
	}
	selectionStarted := now()
	selected, err := newRandomSelector().Select(ctx, SelectionInput{Candidates: eligible.Candidates, Limit: retrievalOptions.selectionLimit, ExplorationSlots: retrievalOptions.explorationSlots, Seed: seed})
	selectionElapsed := elapsedBetween(now(), selectionStarted)
	if err != nil {
		return fixtureAttempt{}, err
	}
	finalOrder := make([]string, 0)
	resultIdentities := make([]resultIdentity, 0)
	for _, decision := range selected.Selected {
		if decision.Selected {
			finalOrder = append(finalOrder, decision.Slug)
			resultIdentities = append(resultIdentities, resultIdentity{Slug: decision.Slug, Title: decision.Title, Type: "concept"})
		}
	}
	trace.Stages = append(trace.Stages, stageTrace{Name: "selection", Outcome: "success", ElapsedMS: selectionElapsed, InputCount: len(eligible.Candidates), OutputCount: len(finalOrder), TotalCount: len(selected.Selected), Decisions: selected.Selected, EvidenceThreshold: retrievalOptions.evidenceThreshold})
	if err := write("selection.output.json", fixtureSelectionOutput{Decisions: selected.Selected, FinalOrder: finalOrder, EvidenceThreshold: retrievalOptions.evidenceThreshold}); err != nil {
		return fixtureAttempt{}, err
	}
	runCompletedAt := now()
	outcome := "success"
	resultStatus, resultReason := queryquality.ResultStatus(len(resultIdentities))
	if resultStatus != "ok" {
		outcome = "retrieval_miss"
	}
	queryReceivedAtStr, runCompletedAtStr, durationMS := attemptTiming(queryReceivedAt, runCompletedAt)
	if err := write("final.json", fixtureFinalReceipt{Outcome: outcome, Status: resultStatus, Reason: resultReason, FinalIdentities: resultIdentities, Receipts: map[string]string{"request": "request.json", "expansion_input": "expansion.input.json", "expansion_output": "expansion.output.json", "matching_input": "matching.input.json", "matching_output": "matching.output.json", "selection_input": "selection.input.json", "selection_output": "selection.output.json", "final": "final.json"}, QueryReceivedAt: queryReceivedAtStr, RunCompletedAt: runCompletedAtStr, DurationMS: durationMS}); err != nil {
		return fixtureAttempt{}, err
	}
	result := query.Result{Query: input.Query, Mode: input.Mode, Status: resultStatus, Reason: resultReason}
	for _, identity := range resultIdentities {
		result.Results = append(result.Results, search.Result{Slug: identity.Slug, Title: identity.Title, Type: identity.Type})
	}
	record := makeResultRecordWithTrace(input, runIndex, prepared, result, nil, expansionElapsed+matchingElapsed+selectionElapsed, metadata, trace)
	record.QueryReceivedAt, record.RunCompletedAt, record.DurationMS = queryReceivedAtStr, runCompletedAtStr, durationMS
	record.VariantID, record.ProfileID, record.PromptID, record.Provider, record.Model = variant.VariantID, variant.Profile.ID, variant.Prompt.ID, variant.Model.Provider, variant.Model.Model
	selectionDigest := digestJSON(selectionInput)
	return fixtureAttempt{Record: record, Case: input, Candidates: eligible.Candidates, Decisions: selected.Selected, EffectiveSeed: seed, SelectionInputDigest: selectionDigest, Fallback: plan.Fallback, LatencyMS: expansionElapsed, Usage: expansionUsage, EvidenceThreshold: retrievalOptions.evidenceThreshold}, nil
}

func addFixtureUsage(total, next fixtureUsage) fixtureUsage {
	total.PromptTokens += next.PromptTokens
	total.CompletionTokens += next.CompletionTokens
	total.TotalTokens += next.TotalTokens
	return total
}

func elapsedBetween(end, start time.Time) int64 {
	value := end.Sub(start).Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func writeFixtureReceipt(path string, meta fixtureReceiptMeta, payload any, secret string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	metaData, _ := json.Marshal(meta)
	var metaObject map[string]any
	_ = json.Unmarshal(metaData, &metaObject)
	for key, value := range metaObject {
		object[key] = value
	}
	data, err = json.Marshal(object)
	if err != nil {
		return err
	}
	data = []byte(scrubSecret(string(data), secret))
	data = append(data, '\n')
	return os.WriteFile(filepath.Clean(path), data, 0o644)
}

func scrubSecret(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[redacted]")
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func receiptSegment(value string) string {
	if value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\\`) {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return "case-" + hex.EncodeToString(digest[:8])
}

func digestJSON(value any) string {
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type summaryDocument struct {
	SchemaVersion     int               `json:"schema_version"`
	EvidenceThreshold int               `json:"evidence_threshold"`
	AttemptCount      int               `json:"attempt_count"`
	Variants          []variantSummary  `json:"variants"`
	Totals            summaryMetrics    `json:"totals"`
	MetricDefinitions map[string]string `json:"metric_definitions"`
}

type variantSummary struct {
	VariantID         string         `json:"variant_id"`
	EvidenceThreshold int            `json:"evidence_threshold"`
	Cases             []caseSummary  `json:"cases"`
	Totals            summaryMetrics `json:"totals"`
}

type caseSummary struct {
	CaseID  string         `json:"case_id"`
	Metrics summaryMetrics `json:"metrics"`
}

type summaryMetrics struct {
	AttemptCount                        int     `json:"attempt_count"`
	ZeroResultCount                     int     `json:"zero_result_count"`
	ZeroResultRate                      float64 `json:"zero_result_rate"`
	Under5Count                         int     `json:"under_5_count"`
	Under5Rate                          float64 `json:"under_5_rate"`
	RecoverableUnder5CaseCount          int     `json:"recoverable_under_5_case_count"`
	RecoverableUnder5CaseRate           float64 `json:"recoverable_under_5_case_rate"`
	AlwaysUnder5CaseCount               int     `json:"always_under_5_case_count"`
	AlwaysUnder5CaseRate                float64 `json:"always_under_5_case_rate"`
	ResultCountMin                      int     `json:"result_count_min"`
	ResultCountMax                      int     `json:"result_count_max"`
	ResultCountMean                     float64 `json:"result_count_mean"`
	ResultCountStddev                   float64 `json:"result_count_stddev"`
	ExactResultSetMatchCount            int     `json:"exact_result_set_match_count"`
	ExactResultSetMatchDenominator      int     `json:"exact_result_set_match_denominator"`
	ExactResultSetMatchRate             float64 `json:"exact_result_set_match_rate"`
	MeanPairwiseTop5Jaccard             float64 `json:"mean_pairwise_top_5_jaccard"`
	MeanPairwiseTop10Jaccard            float64 `json:"mean_pairwise_top_10_jaccard"`
	PairwiseComparisonCount             int     `json:"pairwise_comparison_count"`
	ScoreChangedCandidateCount          int     `json:"score_changed_candidate_count"`
	ScoreChangedCandidateDenominator    int     `json:"score_changed_candidate_denominator"`
	ScoreChangedCandidateRate           float64 `json:"score_changed_candidate_rate"`
	ExactSelectionReplayCount           int     `json:"exact_selection_replay_count"`
	ExactSelectionReplayDenominator     int     `json:"exact_selection_replay_denominator"`
	ExactSelectionReplayRate            float64 `json:"exact_selection_replay_rate"`
	FallbackCount                       int     `json:"fallback_count"`
	FallbackRate                        float64 `json:"fallback_rate"`
	LatencyMinMS                        int64   `json:"latency_min_ms"`
	LatencyMeanMS                       float64 `json:"latency_mean_ms"`
	LatencyP95MS                        int64   `json:"latency_p95_ms"`
	PromptTokensTotal                   int     `json:"prompt_tokens_total"`
	CompletionTokensTotal               int     `json:"completion_tokens_total"`
	TotalTokensTotal                    int     `json:"total_tokens_total"`
	TokenUsageAttemptCount              int     `json:"token_usage_attempt_count"`
	KnownPositiveRecallAt5              float64 `json:"known_positive_recall_at_5"`
	KnownPositiveRecallAt5Numerator     int     `json:"known_positive_recall_at_5_numerator"`
	KnownPositiveRecallAt5Denominator   int     `json:"known_positive_recall_at_5_denominator"`
	KnownPositiveRecallAt10             float64 `json:"known_positive_recall_at_10"`
	KnownPositiveRecallAt10Numerator    int     `json:"known_positive_recall_at_10_numerator"`
	KnownPositiveRecallAt10Denominator  int     `json:"known_positive_recall_at_10_denominator"`
	ForbiddenResultViolationCount       int     `json:"forbidden_result_violation_count"`
	ForbiddenResultViolationDenominator int     `json:"forbidden_result_violation_denominator"`
	ForbiddenResultViolationRate        float64 `json:"forbidden_result_violation_rate"`
}

func writeFixtureSummary(path string, attempts []fixtureAttempt) error {
	byVariant := map[string][]fixtureAttempt{}
	for _, attempt := range attempts {
		byVariant[attempt.Record.VariantID] = append(byVariant[attempt.Record.VariantID], attempt)
	}
	variantIDs := make([]string, 0, len(byVariant))
	for id := range byVariant {
		variantIDs = append(variantIDs, id)
	}
	sort.Strings(variantIDs)
	document := summaryDocument{SchemaVersion: 1, AttemptCount: len(attempts), Variants: make([]variantSummary, 0, len(variantIDs)), MetricDefinitions: map[string]string{
		"zero_result_rate":              "zero_result_count / attempt_count",
		"under_5_rate":                  "under_5_count / attempt_count",
		"recoverable_under_5_case_rate": "cases with min result count < 5 and max >= 5 / cases",
		"always_under_5_case_rate":      "cases whose every run has fewer than 5 results / cases",
		"exact_result_set_match_rate":   "runs after the first matching the first run's result set / (runs - 1) for repeated cases",
		"score_changed_candidate_rate":  "candidate slugs with differing scores across 2+ runs / candidate slugs observed in 2+ runs",
		"exact_selection_replay_rate":   "identical selection outputs / repeated attempts with identical effective seed and selection input digest",
	}}
	if len(attempts) > 0 {
		document.EvidenceThreshold = attempts[0].EvidenceThreshold
	}
	all := make([]fixtureAttempt, 0, len(attempts))
	for _, id := range variantIDs {
		group := byVariant[id]
		all = append(all, group...)
		casesByID := map[string][]fixtureAttempt{}
		for _, attempt := range group {
			casesByID[attempt.Record.CaseID] = append(casesByID[attempt.Record.CaseID], attempt)
		}
		caseIDs := make([]string, 0, len(casesByID))
		for caseID := range casesByID {
			caseIDs = append(caseIDs, caseID)
		}
		sort.Strings(caseIDs)
		variant := variantSummary{VariantID: id, Cases: make([]caseSummary, 0, len(caseIDs)), Totals: aggregateSummaryMetrics(group)}
		if len(group) > 0 {
			variant.EvidenceThreshold = group[0].EvidenceThreshold
		}
		for _, caseID := range caseIDs {
			variant.Cases = append(variant.Cases, caseSummary{CaseID: caseID, Metrics: aggregateSummaryMetrics(casesByID[caseID])})
		}
		document.Variants = append(document.Variants, variant)
	}
	document.Totals = aggregateSummaryMetrics(all)
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Clean(path), data, 0o644)
}

func aggregateSummaryMetrics(attempts []fixtureAttempt) summaryMetrics {
	metrics := summaryMetrics{AttemptCount: len(attempts)}
	if len(attempts) == 0 {
		return metrics
	}
	counts := make([]int, 0, len(attempts))
	latencies := make([]int64, 0, len(attempts))
	resultTotal := 0
	byCase := map[string][]fixtureAttempt{}
	for _, attempt := range attempts {
		count := len(attempt.Record.Results)
		counts = append(counts, count)
		resultTotal += count
		if count == 0 {
			metrics.ZeroResultCount++
		}
		if count < underFiveThreshold {
			metrics.Under5Count++
		}
		latencies = append(latencies, attempt.LatencyMS)
		if attempt.Fallback {
			metrics.FallbackCount++
		}
		metrics.PromptTokensTotal += attempt.Usage.PromptTokens
		metrics.CompletionTokensTotal += attempt.Usage.CompletionTokens
		metrics.TotalTokensTotal += attempt.Usage.TotalTokens
		if attempt.Usage.TotalTokens > 0 {
			metrics.TokenUsageAttemptCount++
		}
		caseKey := attempt.Record.VariantID + "\x00" + attempt.Record.CaseID
		byCase[caseKey] = append(byCase[caseKey], attempt)
	}
	metrics.ZeroResultRate = ratio(metrics.ZeroResultCount, len(attempts))
	metrics.Under5Rate = ratio(metrics.Under5Count, len(attempts))
	metrics.FallbackRate = ratio(metrics.FallbackCount, len(attempts))
	sort.Ints(counts)
	metrics.ResultCountMin, metrics.ResultCountMax = counts[0], counts[len(counts)-1]
	metrics.ResultCountMean = float64(resultTotal) / float64(len(counts))
	variance := 0.0
	for _, count := range counts {
		delta := float64(count) - metrics.ResultCountMean
		variance += delta * delta
	}
	metrics.ResultCountStddev = math.Sqrt(variance / float64(len(counts)))
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	metrics.LatencyMinMS, metrics.LatencyMeanMS, metrics.LatencyP95MS = latencies[0], meanInt64(latencies), percentile95(latencies)
	for _, group := range byCase {
		min, max := len(group[0].Record.Results), len(group[0].Record.Results)
		allUnder := true
		for _, attempt := range group {
			count := len(attempt.Record.Results)
			if count < min {
				min = count
			}
			if count > max {
				max = count
			}
			if count >= underFiveThreshold {
				allUnder = false
			}
		}
		if min < underFiveThreshold && max >= underFiveThreshold {
			metrics.RecoverableUnder5CaseCount++
		}
		if allUnder {
			metrics.AlwaysUnder5CaseCount++
		}
		compareRepeatedRuns(group, &metrics)
		labelMetrics(group, &metrics)
	}
	if metrics.PairwiseComparisonCount > 0 {
		metrics.MeanPairwiseTop5Jaccard /= float64(metrics.PairwiseComparisonCount)
		metrics.MeanPairwiseTop10Jaccard /= float64(metrics.PairwiseComparisonCount)
	}
	metrics.RecoverableUnder5CaseRate = ratio(metrics.RecoverableUnder5CaseCount, len(byCase))
	metrics.AlwaysUnder5CaseRate = ratio(metrics.AlwaysUnder5CaseCount, len(byCase))
	return metrics
}

func compareRepeatedRuns(group []fixtureAttempt, metrics *summaryMetrics) {
	for i := 1; i < len(group); i++ {
		metrics.ExactResultSetMatchDenominator++
		if sameSlugSet(group[0].Record.Results, group[i].Record.Results) {
			metrics.ExactResultSetMatchCount++
		}
	}
	for i := 0; i < len(group); i++ {
		for j := i + 1; j < len(group); j++ {
			metrics.PairwiseComparisonCount++
			metrics.MeanPairwiseTop5Jaccard += jaccard(topSlugs(group[i].Record.Results, 5), topSlugs(group[j].Record.Results, 5))
			metrics.MeanPairwiseTop10Jaccard += jaccard(topSlugs(group[i].Record.Results, 10), topSlugs(group[j].Record.Results, 10))
		}
	}
	bySlug := map[string][]int{}
	for _, attempt := range group {
		for _, candidate := range attempt.Candidates {
			bySlug[candidate.Slug] = append(bySlug[candidate.Slug], candidate.Score)
		}
	}
	for _, scores := range bySlug {
		if len(scores) < 2 {
			continue
		}
		metrics.ScoreChangedCandidateDenominator++
		for _, score := range scores[1:] {
			if score != scores[0] {
				metrics.ScoreChangedCandidateCount++
				break
			}
		}
	}
	byInput := map[string][]fixtureAttempt{}
	for _, attempt := range group {
		key := fmt.Sprintf("%d:%s", attempt.EffectiveSeed, attempt.SelectionInputDigest)
		byInput[key] = append(byInput[key], attempt)
	}
	for _, repeated := range byInput {
		for i := 1; i < len(repeated); i++ {
			metrics.ExactSelectionReplayDenominator++
			if sameDecisions(repeated[0].Decisions, repeated[i].Decisions) {
				metrics.ExactSelectionReplayCount++
			}
		}
	}
	metrics.ExactResultSetMatchRate = ratio(metrics.ExactResultSetMatchCount, metrics.ExactResultSetMatchDenominator)
	metrics.ScoreChangedCandidateRate = ratio(metrics.ScoreChangedCandidateCount, metrics.ScoreChangedCandidateDenominator)
	metrics.ExactSelectionReplayRate = ratio(metrics.ExactSelectionReplayCount, metrics.ExactSelectionReplayDenominator)
}

func labelMetrics(group []fixtureAttempt, metrics *summaryMetrics) {
	for _, attempt := range group {
		positives := uniqueStrings(attempt.Case.KnownPositiveSlugs)
		result5, result10 := topSlugs(attempt.Record.Results, 5), topSlugs(attempt.Record.Results, 10)
		for _, slug := range positives {
			metrics.KnownPositiveRecallAt5Denominator++
			if containsString(result5, slug) {
				metrics.KnownPositiveRecallAt5Numerator++
			}
			metrics.KnownPositiveRecallAt10Denominator++
			if containsString(result10, slug) {
				metrics.KnownPositiveRecallAt10Numerator++
			}
		}
		forbidden := uniqueStrings(attempt.Case.ForbiddenResultSlugs)
		if len(forbidden) > 0 {
			metrics.ForbiddenResultViolationDenominator++
			for _, result := range attempt.Record.Results {
				if containsString(forbidden, result.Slug) {
					metrics.ForbiddenResultViolationCount++
					break
				}
			}
		}
	}
	metrics.KnownPositiveRecallAt5 = ratio(metrics.KnownPositiveRecallAt5Numerator, metrics.KnownPositiveRecallAt5Denominator)
	metrics.KnownPositiveRecallAt10 = ratio(metrics.KnownPositiveRecallAt10Numerator, metrics.KnownPositiveRecallAt10Denominator)
	metrics.ForbiddenResultViolationRate = ratio(metrics.ForbiddenResultViolationCount, metrics.ForbiddenResultViolationDenominator)
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
func meanInt64(values []int64) float64 {
	total := int64(0)
	for _, value := range values {
		total += value
	}
	return float64(total) / float64(len(values))
}
func percentile95(values []int64) int64 {
	index := int(math.Ceil(float64(len(values))*0.95)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}
func topSlugs(results []resultIdentity, limit int) []string {
	if len(results) > limit {
		results = results[:limit]
	}
	slugs := make([]string, 0, len(results))
	for _, result := range results {
		slugs = append(slugs, result.Slug)
	}
	return slugs
}
func sameSlugSet(a, b []resultIdentity) bool {
	left, right := append([]string(nil), topSlugs(a, len(a))...), append([]string(nil), topSlugs(b, len(b))...)
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}
func jaccard(left, right []string) float64 {
	if len(left) == 0 && len(right) == 0 {
		return 1
	}
	leftSet, rightSet := map[string]struct{}{}, map[string]struct{}{}
	for _, value := range left {
		leftSet[value] = struct{}{}
	}
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	intersection, union := 0, len(leftSet)
	for value := range rightSet {
		if _, ok := leftSet[value]; ok {
			intersection++
		} else {
			union++
		}
	}
	return ratio(intersection, union)
}
func sameDecisions(a, b []SelectedCandidate) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}
