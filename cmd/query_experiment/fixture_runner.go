package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/rayer/llm-wiki-bff/internal/buildinfo"
	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

const underFiveThreshold = 5
const artifactDigestTokenLength = 32
const maxPortablePathSegmentBytes = 200
const maxPOSIXPathSegmentBytes = 255
const fixtureVariantIDPrefix = "v-"

func (options experimentOptions) fixtureFlagsSet() bool {
	return options.modelFixturePath != "" || options.models != "" || options.profileFixturePath != "" || options.profiles != "" || options.promptFixturePath != "" || options.prompts != "" || options.artifactsDir != "" || options.summaryPath != "" || options.stageConfigOutput != ""
}

func (options experimentOptions) validateStageConfigFlags() error {
	if options.stageConfigOutput == "" {
		return nil
	}
	if options.service != serviceQueryRetrieval {
		return errors.New("--stage-config-output requires --service query-retrieval")
	}
	if strings.TrimSpace(options.configRevision) == "" {
		return errors.New("--config-revision is required with --stage-config-output")
	}
	if err := validateOutputPath(options.stageConfigOutput); err != nil {
		return err
	}
	return nil
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
	AttemptID              string `json:"attempt_id"`
	VariantID              string `json:"variant_id"`
	ProfileID              string `json:"profile_id"`
	ProfileDigest          string `json:"profile_digest"`
	PromptID               string `json:"prompt_id"`
	PromptDigest           string `json:"prompt_digest"`
	Provider               string `json:"provider"`
	Model                  string `json:"model"`
	CaseID                 string `json:"case_id"`
	RunIndex               int    `json:"run_index"`
	EvidenceThreshold      int    `json:"evidence_threshold"`
	SnapshotIdentity       string `json:"snapshot_identity"`
	CorpusSHA256           string `json:"corpus_sha256"`
	ManifestGeneration     int64  `json:"manifest_generation,omitempty"`
	ManifestSHA256         string `json:"manifest_sha256,omitempty"`
	GenerationID           string `json:"generation_id,omitempty"`
	InputFingerprint       string `json:"input_fingerprint,omitempty"`
	SuggestedQueriesSHA256 string `json:"suggested_queries_sha256"`
	ConfigSchemaVersion    int    `json:"config_schema_version,omitempty"`
	ConfigRevision         string `json:"config_revision,omitempty"`
	ConfigDigest           string `json:"config_digest,omitempty"`
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
	ProfileID                       string `json:"profile_id"`
	ProfileDigest                   string `json:"profile_digest"`
	ConfigSchemaVersion             int    `json:"config_schema_version,omitempty"`
	ConfigRevision                  string `json:"config_revision,omitempty"`
	ConfigDigest                    string `json:"config_digest,omitempty"`
}

type fixtureExpansionInput struct {
	Query                string          `json:"query"`
	CriterionPolicy      CriterionPolicy `json:"criterion_policy"`
	PromptID             string          `json:"prompt_id"`
	Provider             string          `json:"provider"`
	Model                string          `json:"model"`
	KeywordsPerAttempt   int             `json:"keywords_per_attempt"`
	ExpansionAttempts    int             `json:"expansion_attempts"`
	RenderedSystemPrompt string          `json:"rendered_system_prompt"`
	RenderedUserPrompt   string          `json:"rendered_user_prompt"`
}

type fixtureExpansionOutput struct {
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
	UsagePresent bool         `json:"usage_present"`
	RawResponse  string       `json:"raw_response,omitempty"`
	Error        string       `json:"error,omitempty"`
}

type fixtureQueryService interface {
	ExecuteWithTrace(context.Context, cache.Reader, query.Request) (query.Result, *queryquality.Trace, error)
}

type fixtureChatProvider struct {
	model    modelFixtureEntry
	expected queryquality.RenderedPrompt
	mu       sync.Mutex
	calls    map[int]fixtureModelCall
}

func newFixtureChatProvider(model modelFixtureEntry, promptID string, expected queryquality.RenderedPrompt) (*fixtureChatProvider, error) {
	identity, ok := queryquality.LookupPrompt(promptID)
	if !ok {
		return nil, fmt.Errorf("unsupported prompt id %q", promptID)
	}
	if err := queryquality.ValidatePrompt(identity.ID, identity.TemplateDigest); err != nil {
		return nil, err
	}
	return &fixtureChatProvider{model: model, expected: expected, calls: make(map[int]fixtureModelCall)}, nil
}

func (p *fixtureChatProvider) Chat(ctx context.Context, system, user string) (string, error) {
	if system != p.expected.System || user != p.expected.User {
		return "", errors.New("production-rendered prompt mismatch")
	}
	attempt, ok := queryquality.ExpansionAttemptFromContext(ctx)
	if !ok || attempt < 1 {
		return "", errors.New("fixture expansion attempt identity missing")
	}
	call, err := callFixtureModel(ctx, p.model, system, user)
	if err != nil {
		call.Error = err.Error()
	}
	p.mu.Lock()
	p.calls[attempt] = call
	p.mu.Unlock()
	if err != nil {
		return "", err
	}
	return call.Content, nil
}

func (p *fixtureChatProvider) call(attempt int) fixtureModelCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[attempt]
}

type fixtureMatchingInput struct {
	Plan                            QueryPlan                   `json:"plan"`
	SnapshotIdentity                string                      `json:"snapshot_identity"`
	CorpusSHA256                    string                      `json:"corpus_sha256"`
	Parameters                      queryquality.MatchingPolicy `json:"parameters"`
	EvidenceThreshold               int                         `json:"-"`
	RareKeywordMaxDocumentFrequency int                         `json:"-"`
	FallbackQualificationAllowed    bool                        `json:"-"`
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
	Outcome             string            `json:"outcome"`
	Status              string            `json:"status"`
	Reason              string            `json:"reason"`
	FinalIdentities     []resultIdentity  `json:"final_identities"`
	Receipts            map[string]string `json:"receipts"`
	QueryReceivedAt     string            `json:"query_received_at"`
	RunCompletedAt      string            `json:"run_completed_at"`
	DurationMS          int64             `json:"duration_ms"`
	ProfileID           string            `json:"profile_id"`
	ProfileDigest       string            `json:"profile_digest"`
	ConfigSchemaVersion int               `json:"config_schema_version,omitempty"`
	ConfigRevision      string            `json:"config_revision,omitempty"`
	ConfigDigest        string            `json:"config_digest,omitempty"`
	Error               string            `json:"error,omitempty"`
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
	fixtureModel         modelFixtureEntry
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
	// Frozen fixture artifacts use an explicit evidence-threshold policy.
	retrievalOptions.evidenceThresholdSet = true
	options.selectionLimit = retrievalOptions.selectionLimit
	options.explorationSlots = retrievalOptions.explorationSlots
	options.keywordsPerAttempt = retrievalOptions.keywordsPerAttempt
	options.expansionAttempts = retrievalOptions.expansionAttempts
	options.rareDocumentFrequency = retrievalOptions.rareDocumentFrequency
	for i := range variants {
		variantID, err := fixtureVariantID(variants[i].VariantID, options, prepared)
		if err != nil {
			return err
		}
		variants[i].VariantID = variantID
	}
	var stageConfig queryconfig.Config
	if options.stageConfigOutput != "" {
		if len(variants) != 1 {
			return fmt.Errorf("--stage-config-output requires exactly one selected profile, prompt, and model variant; got %d", len(variants))
		}
		stageConfig, err = buildStageConfig(options, variants[0], prepared, retrievalOptions)
		if err != nil {
			return err
		}
	}
	if len(prepared.suggestedData) == 0 {
		reader, ok := prepared.reader.(interface {
			ReadFile(context.Context, string) ([]byte, error)
		})
		if !ok {
			return errors.New("snapshot reader does not support suggested queries")
		}
		prepared.suggestedData, err = reader.ReadFile(ctx, suggestedPath)
		if err != nil {
			return fmt.Errorf("snapshot suggested queries: %w", err)
		}
	}
	suggestedDigest := sha256.Sum256(prepared.suggestedData)
	prepared.suggestedDigest = hex.EncodeToString(suggestedDigest[:])
	if err := os.MkdirAll(filepath.Clean(options.artifactsDir), 0o755); err != nil {
		return fmt.Errorf("create artifacts directory: %w", err)
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
	if options.stageConfigOutput != "" {
		metadata.configSchemaVersion = stageConfig.SchemaVersion
		metadata.configRevision = stageConfig.ConfigRevision
		metadata.configDigest = stageConfig.ConfigDigest
	}
	attempts := make([]fixtureAttempt, 0, len(variants)*len(cases)*options.runs)
	for i := range variants {
		for _, input := range cases {
			for runIndex := 1; runIndex <= options.runs; runIndex++ {
				attempt, err := runFixtureAttempt(ctx, options, retrievalOptions, variants[i], input, runIndex, prepared, now, metadata, deps)
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
	if options.stageConfigOutput != "" {
		if err := writeStageConfig(options.stageConfigOutput, stageConfig); err != nil {
			return fmt.Errorf("write stage config: %w", err)
		}
	}
	if options.summaryPath != "" {
		if err := writeFixtureSummary(options.summaryPath, attempts); err != nil {
			return err
		}
	}
	return nil
}

func fixtureVariantID(base string, options experimentOptions, prepared preparedSnapshot) (string, error) {
	if strings.TrimSpace(base) == "" {
		return "", errors.New("fixture variant identity base is empty")
	}
	seed := "auto"
	if options.seed != nil {
		seed = fmt.Sprintf("%d", *options.seed)
	}
	threshold := options.evidenceThreshold
	if !options.evidenceThresholdSet {
		threshold = queryquality.DefaultEvidenceThreshold
	}
	corpusToken, err := digestToken(prepared.digest)
	if err != nil {
		return "", fmt.Errorf("corpus digest: %w", err)
	}
	material := fmt.Sprintf("base=%s|limit=%d|exploration=%d|threshold=%d|keywords=%d|attempts=%d|rare_df=%d|seed=%s|corpus=%s", base, options.selectionLimit, options.explorationSlots, threshold, options.keywordsPerAttempt, options.expansionAttempts, options.rareDocumentFrequency, seed, corpusToken)
	hash := sha256.Sum256([]byte(material))
	return fixtureVariantIDPrefix + hex.EncodeToString(hash[:])[:artifactDigestTokenLength], nil
}

func digestToken(digest string) (string, error) {
	raw := strings.TrimPrefix(digest, "sha256:")
	if len(raw) != 64 {
		return "", fmt.Errorf("invalid sha256 digest length")
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return "", fmt.Errorf("invalid sha256 digest: %w", err)
	}
	return raw[:artifactDigestTokenLength], nil
}

func runFixtureAttempt(ctx context.Context, options experimentOptions, retrievalOptions queryRetrievalOptions, variant fixtureVariant, input caseInput, runIndex int, prepared preparedSnapshot, now func() time.Time, metadata recordMetadata, deps dependencies) (fixtureAttempt, error) {
	attemptID := fmt.Sprintf("%s__case=%s__run=%d", variant.VariantID, input.ID, runIndex)
	profile, err := variant.Profile.retrievalProfile()
	if err != nil {
		return fixtureAttempt{}, err
	}
	prompt, ok := queryquality.LookupPrompt(variant.Prompt.ID)
	if !ok {
		return fixtureAttempt{}, fmt.Errorf("unsupported prompt id %q", variant.Prompt.ID)
	}
	if err := queryquality.ValidatePromptTemplate(variant.Prompt.ID, variant.Prompt.SystemTemplate, variant.Prompt.UserTemplate); err != nil {
		return fixtureAttempt{}, fmt.Errorf("selected prompt: %w", err)
	}
	if variant.Prompt.TemplateDigest == "" || variant.Prompt.TemplateDigest != prompt.TemplateDigest {
		return fixtureAttempt{}, errors.New("selected prompt template digest mismatch")
	}
	profileDigest := variant.ProfileDigest
	meta := fixtureReceiptMeta{AttemptID: attemptID, VariantID: variant.VariantID, ProfileID: profile.ID, ProfileDigest: profileDigest, PromptID: variant.Prompt.ID, Provider: variant.Model.Provider, Model: variant.Model.Model, CaseID: input.ID, RunIndex: runIndex, EvidenceThreshold: retrievalOptions.evidenceThreshold}
	if promptIdentity, ok := queryquality.LookupPrompt(variant.Prompt.ID); ok {
		meta.PromptDigest = promptIdentity.TemplateDigest
	}
	meta.SnapshotIdentity = prepared.label
	meta.CorpusSHA256 = prepared.digest
	meta.ManifestGeneration = prepared.manifestGeneration
	meta.ManifestSHA256 = prepared.manifestDigest
	meta.GenerationID = prepared.generationID
	meta.InputFingerprint = prepared.inputFingerprint
	meta.SuggestedQueriesSHA256 = prepared.suggestedDigest
	if metadata.configDigest != "" {
		meta.ConfigSchemaVersion = metadata.configSchemaVersion
		meta.ConfigRevision = metadata.configRevision
		meta.ConfigDigest = metadata.configDigest
	}
	metadata.promptID = variant.Prompt.ID
	metadata.promptDigest = meta.PromptDigest
	dir := filepath.Join(filepath.Clean(options.artifactsDir), variant.VariantID, receiptSegment(input.ID), fmt.Sprintf("run-%d", runIndex))
	queryReceivedAt := now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fixtureAttempt{}, fmt.Errorf("create attempt artifacts: %w", err)
	}
	write := func(name string, payload any) error {
		return writeFixtureReceipt(filepath.Join(dir, name), meta, payload, variant.Model.APIKey, variant.Model.BaseURL)
	}
	seedMode := "query-derived"
	if retrievalOptions.seed != nil {
		seedMode = "explicit"
	}
	if err := write("request.json", fixtureRequestReceipt{Query: input.Query, Mode: input.Mode, SnapshotIdentity: prepared.label, CorpusSHA256: prepared.digest, SelectionLimit: retrievalOptions.selectionLimit, ExplorationSlots: retrievalOptions.explorationSlots, EvidenceThreshold: retrievalOptions.evidenceThreshold, KeywordsPerAttempt: retrievalOptions.keywordsPerAttempt, ExpansionAttempts: retrievalOptions.expansionAttempts, RareKeywordMaxDocumentFrequency: retrievalOptions.rareDocumentFrequency, SeedMode: seedMode, Model: variant.Model.Model, ProfileID: profile.ID, ProfileDigest: profileDigest}); err != nil {
		return fixtureAttempt{}, err
	}
	policy := profile.CriterionPolicy
	rendered, err := queryquality.RenderPrompt(variant.Prompt.ID, input.Query, policy, retrievalOptions.keywordsPerAttempt)
	if err != nil {
		return fixtureAttempt{}, fmt.Errorf("render prompt: %w", err)
	}
	if err := write("expansion.input.json", fixtureExpansionInput{Query: input.Query, CriterionPolicy: policy, PromptID: variant.Prompt.ID, Provider: variant.Model.Provider, Model: variant.Model.Model, KeywordsPerAttempt: retrievalOptions.keywordsPerAttempt, ExpansionAttempts: retrievalOptions.expansionAttempts, RenderedSystemPrompt: rendered.System, RenderedUserPrompt: rendered.User}); err != nil {
		return fixtureAttempt{}, err
	}
	provider, err := newFixtureChatProvider(variant.Model, prompt.ID, rendered)
	if err != nil {
		return fixtureAttempt{}, err
	}
	serviceConfig := queryquality.QueryRetrievalServiceConfig{
		Cache: prepared.cache, ChatProvider: provider, Options: toQueryRetrievalCoreOptions(retrievalOptions), RetrievalProfile: profile,
		PromptID: prompt.ID, AllowDeterministicFallback: true,
	}
	newService := deps.newFixtureQueryService
	if newService == nil {
		newService = func(config queryquality.QueryRetrievalServiceConfig) (fixtureQueryService, error) {
			return queryquality.NewQueryRetrievalService(config)
		}
	}
	service, err := newService(serviceConfig)
	if err != nil {
		return fixtureAttempt{}, err
	}
	result, trace, err := service.ExecuteWithTrace(ctx, prepared.reader, query.Request{Query: input.Query, Mode: input.Mode})
	if err != nil {
		return fixtureAttempt{}, fmt.Errorf("fixture query retrieval: %w", err)
	}
	_ = now()
	_ = now()
	_ = now()
	_ = now()
	_ = now()
	runCompletedAt := now()
	if trace == nil {
		return fixtureAttempt{}, errors.New("query-retrieval trace is missing")
	}
	trace, err = cloneTraceForFixture(trace)
	if err != nil {
		return fixtureAttempt{}, err
	}
	trace.Variant = variant.VariantID
	canonicalizeFixtureTracePolicy(trace)
	meta.EvidenceThreshold = trace.MatchingPolicy.EvidenceThreshold
	expansionStage, err := fixtureTraceStage(trace, "expansion")
	if err != nil {
		return fixtureAttempt{}, err
	}
	matchingStage, err := fixtureTraceStage(trace, "matching")
	if err != nil {
		return fixtureAttempt{}, err
	}
	selectionStage, err := fixtureTraceStage(trace, "selection")
	if err != nil {
		return fixtureAttempt{}, err
	}
	if expansionStage.Plan == nil {
		return fixtureAttempt{}, errors.New("query-retrieval trace expansion plan is missing")
	}
	plan := *expansionStage.Plan
	attemptReceipts := make([]fixtureExpansionAttemptReceipt, 0, len(trace.Expansion.AttemptOutcomes))
	var expansionUsage fixtureUsage
	for _, outcome := range trace.Expansion.AttemptOutcomes {
		call := provider.call(outcome.AttemptIndex)
		attemptReceipts = append(attemptReceipts, fixtureExpansionAttemptReceipt{AttemptIndex: outcome.AttemptIndex, Outcome: outcome.Outcome, LatencyMS: call.LatencyMS, Usage: call.Usage, UsagePresent: call.UsagePresent, RawResponse: scrubFixtureEvidence(call.RawResponse, variant.Model), Error: scrubFixtureEvidence(call.Error, variant.Model)})
		expansionUsage = addFixtureUsage(expansionUsage, call.Usage)
	}
	validation := "valid"
	if plan.Fallback {
		validation = "fallback"
	}
	if err := write("expansion.output.json", fixtureExpansionOutput{ParsedPlan: plan, Source: expansionStage.Source, Validation: validation, Fallback: plan.Fallback, FallbackReason: expansionStage.FallbackReason, Error: "", LatencyMS: expansionStage.ElapsedMS, Usage: expansionUsage, RequestedAttempts: trace.Expansion.RequestedAttempts, SuccessfulAttempts: trace.Expansion.SuccessfulAttempts, ProviderFailedAttempts: trace.Expansion.ProviderFailedAttempts, FallbackCount: trace.Expansion.FallbackCount, KeywordsPerAttempt: trace.Expansion.KeywordsPerAttempt, KeywordSupport: plan.KeywordSupport, Attempts: attemptReceipts}); err != nil {
		return fixtureAttempt{}, err
	}
	if err := write("matching.input.json", fixtureMatchingInput{
		Plan: plan, SnapshotIdentity: prepared.label, CorpusSHA256: prepared.digest, EvidenceThreshold: trace.MatchingPolicy.EvidenceThreshold, RareKeywordMaxDocumentFrequency: trace.MatchingPolicy.RareKeywordMaxDocumentFrequency, FallbackQualificationAllowed: trace.MatchingPolicy.FallbackQualificationAllowed,
		Parameters: trace.MatchingPolicy,
	}); err != nil {
		return fixtureAttempt{}, err
	}
	identities := make([]resultIdentity, 0, len(matchingStage.Candidates))
	for _, candidate := range matchingStage.Candidates {
		identities = append(identities, resultIdentity{Slug: candidate.Slug, Title: candidate.Title, Type: "concept"})
	}
	if err := write("matching.output.json", fixtureMatchingOutput{CandidateIdentities: identities, Candidates: matchingStage.Candidates}); err != nil {
		return fixtureAttempt{}, err
	}
	selectionInput := fixtureSelectionInput{Candidates: matchingStage.Candidates, Limit: trace.SelectionLimit, ExplorationSlots: trace.ExplorationSlots, EvidenceThreshold: trace.MatchingPolicy.EvidenceThreshold, EffectiveSeed: trace.Seed}
	if err := write("selection.input.json", selectionInput); err != nil {
		return fixtureAttempt{}, err
	}
	finalOrder := make([]string, 0)
	resultIdentities := make([]resultIdentity, 0, len(result.Results))
	for _, item := range result.Results {
		identity := resultIdentity{Slug: item.Slug, Title: item.Title, Type: item.Type}
		resultIdentities = append(resultIdentities, identity)
		finalOrder = append(finalOrder, identity.Slug)
	}
	if err := write("selection.output.json", fixtureSelectionOutput{Decisions: selectionStage.Decisions, FinalOrder: finalOrder, EvidenceThreshold: trace.MatchingPolicy.EvidenceThreshold}); err != nil {
		return fixtureAttempt{}, err
	}
	outcome := "success"
	if result.Status != "ok" {
		outcome = "retrieval_miss"
	}
	queryReceivedAtStr, runCompletedAtStr, durationMS := attemptTiming(queryReceivedAt, runCompletedAt)
	if err := write("final.json", fixtureFinalReceipt{Outcome: outcome, Status: result.Status, Reason: result.Reason, FinalIdentities: resultIdentities, Receipts: map[string]string{"request": "request.json", "expansion_input": "expansion.input.json", "expansion_output": "expansion.output.json", "matching_input": "matching.input.json", "matching_output": "matching.output.json", "selection_input": "selection.input.json", "selection_output": "selection.output.json", "final": "final.json"}, QueryReceivedAt: queryReceivedAtStr, RunCompletedAt: runCompletedAtStr, DurationMS: durationMS, ProfileID: profile.ID, ProfileDigest: profileDigest, ConfigSchemaVersion: metadata.configSchemaVersion, ConfigRevision: metadata.configRevision, ConfigDigest: metadata.configDigest}); err != nil {
		return fixtureAttempt{}, err
	}
	record := makeResultRecordWithTrace(input, runIndex, prepared, result, nil, expansionStage.ElapsedMS+matchingStage.ElapsedMS+selectionStage.ElapsedMS, metadata, trace)
	record.QueryReceivedAt, record.RunCompletedAt, record.DurationMS = queryReceivedAtStr, runCompletedAtStr, durationMS
	record.VariantID, record.ProfileID, record.ProfileDigest, record.PromptID, record.Provider, record.Model = variant.VariantID, profile.ID, profileDigest, variant.Prompt.ID, variant.Model.Provider, variant.Model.Model
	record.fixtureAPIKey, record.fixtureBaseURL = variant.Model.APIKey, variant.Model.BaseURL
	selectionDigest := digestJSON(selectionInput)
	return fixtureAttempt{Record: record, Case: input, Candidates: matchingStage.Candidates, Decisions: selectionStage.Decisions, EffectiveSeed: trace.Seed, SelectionInputDigest: selectionDigest, Fallback: plan.Fallback, LatencyMS: expansionStage.ElapsedMS, Usage: expansionUsage, EvidenceThreshold: trace.MatchingPolicy.EvidenceThreshold, fixtureModel: variant.Model}, nil
}

func fixtureTraceStage(trace *queryRetrievalTrace, name string) (stageTrace, error) {
	if trace == nil {
		return stageTrace{}, errors.New("query-retrieval trace is missing")
	}
	for _, stage := range trace.Stages {
		if stage.Name == name {
			return stage, nil
		}
	}
	return stageTrace{}, fmt.Errorf("query-retrieval trace stage %q is missing", name)
}

func cloneTraceForFixture(trace *queryRetrievalTrace) (*queryRetrievalTrace, error) {
	data, err := json.Marshal(trace)
	if err != nil {
		return nil, err
	}
	var clone queryRetrievalTrace
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func canonicalizeFixtureTracePolicy(trace *queryRetrievalTrace) {
	policy := trace.MatchingPolicy
	trace.Expansion.EvidenceThreshold = policy.EvidenceThreshold
	trace.Expansion.RareKeywordMaxDocumentFrequency = policy.RareKeywordMaxDocumentFrequency
	trace.Expansion.FallbackQualificationAllowed = policy.FallbackQualificationAllowed
	trace.Expansion.SemanticRequiredFailClosed = policy.SemanticRequiredFailClosed
	trace.Expansion.SemanticExcludedFailClosed = policy.SemanticExcludedFailClosed
	for stageIndex := range trace.Stages {
		trace.Stages[stageIndex].EvidenceThreshold = policy.EvidenceThreshold
		for candidateIndex := range trace.Stages[stageIndex].Candidates {
			trace.Stages[stageIndex].Candidates[candidateIndex].EvidenceThreshold = policy.EvidenceThreshold
		}
	}
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

func writeFixtureReceipt(path string, meta fixtureReceiptMeta, payload any, secret, baseURL string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var metaObject map[string]any
	if err := json.Unmarshal(metaData, &metaObject); err != nil {
		return err
	}
	for key, value := range metaObject {
		object[key] = value
	}
	data, err = marshalSanitizedFixtureJSON(object, modelFixtureEntry{APIKey: secret, BaseURL: baseURL})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Clean(path), data, 0o644)
}

func scrubFixtureEvidence(value string, model modelFixtureEntry) string {
	if sanitized, ok := scrubFixtureJSONDocument(value, model); ok {
		return sanitized
	}
	normalized := normalizeJSONEscapes(value)
	if fixtureContainsSensitiveValue(normalized, model) {
		return "[redacted]"
	}
	return scrubFixturePlaintext(value, model)
}

func scrubFixtureJSONDocument(value string, model modelFixtureEntry) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return "", false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", false
	}
	tree = scrubFixtureJSONValue(tree, model)
	data, err := json.Marshal(tree)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func fixtureContainsSensitiveValue(value string, model modelFixtureEntry) bool {
	return (model.APIKey != "" && strings.Contains(value, model.APIKey)) || (model.BaseURL != "" && strings.Contains(value, model.BaseURL))
}

func scrubFixturePlaintext(value string, model modelFixtureEntry) string {
	if model.APIKey != "" {
		value = strings.ReplaceAll(value, model.APIKey, "[redacted]")
	}
	if model.BaseURL != "" {
		value = strings.ReplaceAll(value, model.BaseURL, "[redacted]")
	}
	return value
}

func normalizeJSONEscapes(value string) string {
	var normalized strings.Builder
	normalized.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 >= len(value) {
			normalized.WriteByte(value[index])
			continue
		}
		switch value[index+1] {
		case '"', '\\', '/':
			normalized.WriteByte(value[index+1])
			index++
		case 'b':
			normalized.WriteByte('\b')
			index++
		case 'f':
			normalized.WriteByte('\f')
			index++
		case 'n':
			normalized.WriteByte('\n')
			index++
		case 'r':
			normalized.WriteByte('\r')
			index++
		case 't':
			normalized.WriteByte('\t')
			index++
		case 'u':
			first, ok := decodeJSONUnicodeEscape(value[index+2:])
			if !ok {
				normalized.WriteByte(value[index])
				continue
			}
			index += 5
			if first >= 0xD800 && first <= 0xDBFF && index+2 < len(value) && value[index+1] == '\\' && value[index+2] == 'u' {
				second, pairOK := decodeJSONUnicodeEscape(value[index+3:])
				if pairOK && second >= 0xDC00 && second <= 0xDFFF {
					normalized.WriteString(string(utf16.Decode([]uint16{first, second})))
					index += 5
					continue
				}
			}
			normalized.WriteString(string(utf16.Decode([]uint16{first})))
		default:
			normalized.WriteByte(value[index])
		}
	}
	return normalized.String()
}

func decodeJSONUnicodeEscape(value string) (uint16, bool) {
	if len(value) < 4 {
		return 0, false
	}
	var result uint16
	for _, digit := range []byte(value[:4]) {
		result <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			result |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			result |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			result |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

func marshalSanitizedFixtureJSON(value any, model modelFixtureEntry) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		return nil, err
	}
	tree = scrubFixtureJSONValue(tree, model)
	return json.Marshal(tree)
}

func scrubFixtureJSONValue(value any, model modelFixtureEntry) any {
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, nested := range current {
			result[scrubFixturePlaintext(key, model)] = scrubFixtureJSONValue(nested, model)
		}
		return result
	case []any:
		for index, nested := range current {
			current[index] = scrubFixtureJSONValue(nested, model)
		}
	case string:
		return scrubFixtureEvidence(current, model)
	}
	return value
}

func scrubSecret(value, secret string) string {
	return scrubFixturePlaintext(value, modelFixtureEntry{APIKey: secret})
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
	SchemaVersion       int               `json:"schema_version"`
	EvidenceThreshold   int               `json:"evidence_threshold"`
	AttemptCount        int               `json:"attempt_count"`
	ConfigSchemaVersion int               `json:"config_schema_version,omitempty"`
	ConfigRevision      string            `json:"config_revision,omitempty"`
	ConfigDigest        string            `json:"config_digest,omitempty"`
	Variants            []variantSummary  `json:"variants"`
	Totals              summaryMetrics    `json:"totals"`
	MetricDefinitions   map[string]string `json:"metric_definitions"`
}

type variantSummary struct {
	VariantID           string         `json:"variant_id"`
	ProfileID           string         `json:"profile_id"`
	ProfileDigest       string         `json:"profile_digest"`
	EvidenceThreshold   int            `json:"evidence_threshold"`
	ConfigSchemaVersion int            `json:"config_schema_version,omitempty"`
	ConfigRevision      string         `json:"config_revision,omitempty"`
	ConfigDigest        string         `json:"config_digest,omitempty"`
	Cases               []caseSummary  `json:"cases"`
	Totals              summaryMetrics `json:"totals"`
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
		document.ConfigSchemaVersion = attempts[0].Record.ConfigSchemaVersion
		document.ConfigRevision = attempts[0].Record.ConfigRevision
		document.ConfigDigest = attempts[0].Record.ConfigDigest
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
			variant.ProfileID = group[0].Record.ProfileID
			variant.ProfileDigest = group[0].Record.ProfileDigest
			variant.ConfigSchemaVersion = group[0].Record.ConfigSchemaVersion
			variant.ConfigRevision = group[0].Record.ConfigRevision
			variant.ConfigDigest = group[0].Record.ConfigDigest
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
	for _, attempt := range attempts {
		data, err = marshalSanitizedFixtureJSON(json.RawMessage(data), attempt.fixtureModel)
		if err != nil {
			return err
		}
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
