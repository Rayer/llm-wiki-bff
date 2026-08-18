package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/buildinfo"
	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

const (
	maxExperimentCases = 1000
	maxExperimentRuns  = 100
)

type experimentOptions struct {
	snapshotPath         string
	casesPath            string
	runs                 int
	outputPath           string
	configDir            string
	service              string
	selectionLimit       int
	explorationSlots     int
	explorationSlotsSet  bool
	evidenceThreshold    int
	evidenceThresholdSet bool
	seed                 *int64
	modelFixturePath     string
	models               string
	profileFixturePath   string
	profiles             string
	promptFixturePath    string
	prompts              string
	artifactsDir         string
	summaryPath          string
}

type caseInput struct {
	ID                   string
	Query                string
	Mode                 string
	KnownPositiveSlugs   []string
	ForbiddenResultSlugs []string
	Tags                 []string
}

type dependencies struct {
	loadConfig                func(string) (config.Config, error)
	newExecutor               func(*cache.Cache, config.Config) (query.Executor, error)
	newQueryRetrievalExecutor func(*cache.Cache, config.Config, queryRetrievalOptions) (query.Executor, error)
	now                       func() time.Time
	stdout                    io.Writer
	openOutput                func(string, io.Writer) (recordSink, error)
	queryRetrievalSeed        func(string) int64
}

type preparedSnapshot struct {
	label  string
	digest string
	reader *snapshotReader
	cache  *cache.Cache
}

type resultIdentity struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

type resultRecord struct {
	SchemaVersion       int                  `json:"schema_version"`
	CaseID              string               `json:"case_id"`
	RunIndex            int                  `json:"run_index"`
	Snapshot            string               `json:"snapshot_identity"`
	CorpusSHA256        string               `json:"corpus_sha256"`
	Query               string               `json:"query"`
	Mode                string               `json:"mode"`
	Expansion           *llm.ExpandResult    `json:"expansion,omitempty"`
	Results             []resultIdentity     `json:"results"`
	Citations           []search.Citation    `json:"citations"`
	Synthesis           string               `json:"synthesis"`
	Outcome             string               `json:"outcome"`
	ErrorStage          string               `json:"error_stage,omitempty"`
	ErrorMessage        string               `json:"error_message,omitempty"`
	ElapsedMS           int64                `json:"elapsed_ms"`
	QueryReceivedAt     string               `json:"query_received_at"`
	RunCompletedAt      string               `json:"run_completed_at"`
	DurationMS          int64                `json:"duration_ms"`
	SourceRevision      string               `json:"source_revision"`
	Provider            string               `json:"provider,omitempty"`
	Model               string               `json:"model,omitempty"`
	VariantID           string               `json:"variant_id,omitempty"`
	ProfileID           string               `json:"profile_id,omitempty"`
	PromptID            string               `json:"prompt_id,omitempty"`
	EvidenceThreshold   int                  `json:"evidence_threshold,omitempty"`
	QueryRetrievalTrace *queryRetrievalTrace `json:"three_host_trace,omitempty"`
}

type recordSink interface {
	WriteRecord(resultRecord) error
	Finish() error
	Abort()
}

type writerSink struct {
	writer   io.Writer
	finishFn func() error
	abortFn  func()
}

func (s *writerSink) WriteRecord(record resultRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.writer.Write(data)
	return err
}

func (s *writerSink) Finish() error {
	if s.finishFn == nil {
		return nil
	}
	return s.finishFn()
}

func (s *writerSink) Abort() {
	if s.abortFn != nil {
		s.abortFn()
	}
}

func readCases(path string) ([]caseInput, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("cases: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("cases must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cases: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	var total int64
	cases := make([]caseInput, 0)
	seenIDs := make(map[string]struct{})
	for {
		line, readErr := reader.ReadBytes('\n')
		total += int64(len(line))
		if len(line) > 0 {
			if strings.TrimSpace(string(line)) == "" {
				return nil, errors.New("blank case lines are not allowed")
			}
			if len(cases) >= maxExperimentCases {
				return nil, fmt.Errorf("cases exceed %d-case limit", maxExperimentCases)
			}
			caseValue, err := decodeCase(line)
			if err != nil {
				return nil, fmt.Errorf("case %d: %w", len(cases)+1, err)
			}
			if _, exists := seenIDs[caseValue.ID]; exists {
				return nil, fmt.Errorf("case %d: duplicate id %q", len(cases)+1, caseValue.ID)
			}
			seenIDs[caseValue.ID] = struct{}{}
			cases = append(cases, caseValue)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read cases: %w", readErr)
		}
	}
	if len(cases) == 0 {
		return nil, errors.New("cases must contain at least one case")
	}
	return cases, nil
}

func decodeCase(line []byte) (caseInput, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	var result caseInput
	seen := make(map[string]struct{}, 3)
	token, err := decoder.Token()
	if err != nil {
		return caseInput{}, fmt.Errorf("invalid JSON object: %w", err)
	}
	if token != json.Delim('{') {
		return caseInput{}, errors.New("case must be a JSON object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return caseInput{}, fmt.Errorf("invalid field name: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return caseInput{}, errors.New("field name must be a string")
		}
		if _, exists := seen[key]; exists {
			return caseInput{}, fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = struct{}{}
		switch key {
		case "id":
			err = decoder.Decode(&result.ID)
		case "query":
			err = decoder.Decode(&result.Query)
		case "mode":
			err = decoder.Decode(&result.Mode)
		case "known_positive_slugs":
			err = decoder.Decode(&result.KnownPositiveSlugs)
		case "forbidden_result_slugs":
			err = decoder.Decode(&result.ForbiddenResultSlugs)
		case "tags":
			err = decoder.Decode(&result.Tags)
		default:
			return caseInput{}, fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			return caseInput{}, fmt.Errorf("field %q: %w", key, err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return caseInput{}, fmt.Errorf("invalid JSON object: %w", err)
	}
	if closing != json.Delim('}') {
		return caseInput{}, errors.New("invalid JSON object")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return caseInput{}, err
	}
	if strings.TrimSpace(result.ID) == "" {
		return caseInput{}, errors.New("id must not be empty")
	}
	result.ID = strings.TrimSpace(result.ID)
	result.Query = strings.TrimSpace(result.Query)
	if result.Query == "" {
		return caseInput{}, errors.New("query must not be empty")
	}
	if result.Mode != "wiki" && result.Mode != "full" {
		return caseInput{}, fmt.Errorf("unsupported mode %q", result.Mode)
	}
	for _, values := range [][]string{result.KnownPositiveSlugs, result.ForbiddenResultSlugs, result.Tags} {
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
			if values[i] == "" {
				return caseInput{}, errors.New("optional case label values must not be empty")
			}
		}
	}
	if _, ok := seen["id"]; !ok {
		return caseInput{}, errors.New("id, query, and mode are required")
	}
	if _, ok := seen["query"]; !ok {
		return caseInput{}, errors.New("id, query, and mode are required")
	}
	if _, ok := seen["mode"]; !ok {
		return caseInput{}, errors.New("id, query, and mode are required")
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values on one line")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func preflightSnapshot(ctx context.Context, root string) (preparedSnapshot, error) {
	cleanRoot := filepath.Clean(root)
	info, err := os.Lstat(cleanRoot)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return preparedSnapshot{}, errors.New("snapshot must be a regular directory")
	}
	reader := newSnapshotReader(cleanRoot)
	corpus, err := reader.ReadFile(ctx, "cache/concepts.jsonl")
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot corpus: %w", err)
	}
	digest := sha256.Sum256(corpus)
	conceptCache := cache.New()
	if _, err := conceptCache.All(ctx, reader); err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot corpus: %w", err)
	}
	label := filepath.Base(cleanRoot)
	if label == "." || label == string(filepath.Separator) || label == "" {
		label = "snapshot"
	}
	return preparedSnapshot{
		label:  label,
		digest: hex.EncodeToString(digest[:]),
		reader: reader,
		cache:  conceptCache,
	}, nil
}

func validateOutputPath(path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	parentInfo, err := os.Stat(filepath.Dir(clean))
	if err != nil {
		return fmt.Errorf("output directory: %w", err)
	}
	if !parentInfo.IsDir() {
		return errors.New("output parent must be a directory")
	}
	if info, err := os.Lstat(clean); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("output must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("output: %w", err)
	}
	return nil
}

func runExperiment(ctx context.Context, options experimentOptions, deps dependencies) error {
	if options.runs <= 0 || options.runs > maxExperimentRuns {
		return fmt.Errorf("runs must be between 1 and %d", maxExperimentRuns)
	}
	if strings.TrimSpace(options.snapshotPath) == "" {
		return errors.New("snapshot is required")
	}
	if strings.TrimSpace(options.casesPath) == "" {
		return errors.New("cases is required")
	}
	if options.service == "" {
		options.service = serviceProduction
	}
	if options.service != serviceProduction && options.service != serviceQueryRetrieval && options.service != serviceQueryRetrievalLegacy {
		return fmt.Errorf("unsupported service %q", options.service)
	}
	if options.service == serviceQueryRetrievalLegacy {
		options.service = serviceQueryRetrieval
	}
	if err := validateOutputPath(options.outputPath); err != nil {
		return err
	}
	cases, err := readCases(options.casesPath)
	if err != nil {
		return err
	}
	prepared, err := preflightSnapshot(ctx, options.snapshotPath)
	if err != nil {
		return err
	}
	if options.service == serviceQueryRetrieval && options.fixtureFlagsSet() {
		if err := options.validateFixtureFlags(); err != nil {
			return err
		}
		return runFixtureExperiment(ctx, options, prepared, cases, deps)
	}
	if deps.loadConfig == nil || (options.service == serviceProduction && deps.newExecutor == nil) || (options.service == serviceQueryRetrieval && deps.newQueryRetrievalExecutor == nil) {
		return errors.New("experiment dependencies are incomplete")
	}
	configDir := options.configDir
	if configDir == "" {
		configDir = "."
	}
	cfg, err := deps.loadConfig(configDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	var executor query.Executor
	if options.service == serviceQueryRetrieval {
		queryRetrievalOptions := defaultQueryRetrievalOptions()
		queryRetrievalOptions.selectionLimit = options.selectionLimit
		if options.explorationSlotsSet {
			queryRetrievalOptions.explorationSlots = options.explorationSlots
		}
		if options.evidenceThresholdSet {
			queryRetrievalOptions.evidenceThreshold = options.evidenceThreshold
			queryRetrievalOptions.evidenceThresholdSet = true
		}
		queryRetrievalOptions.seed = options.seed
		queryRetrievalOptions.seedFor = deps.queryRetrievalSeed
		executor, err = deps.newQueryRetrievalExecutor(prepared.cache, cfg, queryRetrievalOptions)
	} else {
		executor, err = deps.newExecutor(prepared.cache, cfg)
	}
	if err != nil {
		return fmt.Errorf("create query service: %w", err)
	}
	if executor == nil {
		return errors.New("query service is nil")
	}
	now := deps.now
	if now == nil {
		now = time.Now
	}
	var sink recordSink
	if deps.openOutput != nil {
		sink, err = deps.openOutput(options.outputPath, deps.stdout)
	} else {
		sink, err = openWriterOutput(options.outputPath, deps.stdout)
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
	metadata := buildRecordMetadata(cfg)
	for _, input := range cases {
		for runIndex := 1; runIndex <= options.runs; runIndex++ {
			queryReceivedAt := now()
			result, trace, executeErr := executeExperiment(executor, ctx, prepared.reader, query.Request{Query: input.Query, Mode: input.Mode})
			runCompletedAt := now()
			elapsed := elapsedBetween(runCompletedAt, queryReceivedAt)
			record := makeResultRecordWithTrace(input, runIndex, prepared, result, executeErr, elapsed, metadata, trace)
			record.QueryReceivedAt, record.RunCompletedAt, record.DurationMS = attemptTiming(queryReceivedAt, runCompletedAt)
			if err := sink.WriteRecord(record); err != nil {
				return fmt.Errorf("write result %d/%d for %q: %w", runIndex, options.runs, input.ID, err)
			}
		}
	}
	if err := sink.Finish(); err != nil {
		return fmt.Errorf("finalize output: %w", err)
	}
	finished = true
	return nil
}

func attemptTiming(receivedAt, completedAt time.Time) (string, string, int64) {
	return receivedAt.UTC().Format(time.RFC3339Nano), completedAt.UTC().Format(time.RFC3339Nano), elapsedBetween(completedAt, receivedAt)
}

type tracedExecutor interface {
	ExecuteWithTrace(context.Context, cache.Reader, query.Request) (query.Result, *queryRetrievalTrace, error)
}

func executeExperiment(executor query.Executor, ctx context.Context, reader cache.Reader, request query.Request) (query.Result, *queryRetrievalTrace, error) {
	if traced, ok := executor.(tracedExecutor); ok {
		return traced.ExecuteWithTrace(ctx, reader, request)
	}
	result, err := executor.Execute(ctx, reader, request)
	return result, nil, err
}

type recordMetadata struct {
	sourceRevision string
	provider       string
	model          string
	apiKey         string
}

func buildRecordMetadata(cfg config.Config) recordMetadata {
	info := buildinfo.Current()
	metadata := recordMetadata{sourceRevision: info.Commit, apiKey: cfg.DeepSeekAPIKey}
	if strings.TrimSpace(cfg.DeepSeekAPIKey) != "" {
		metadata.provider = "deepseek"
		metadata.model = "deepseek-chat"
	}
	return metadata
}

func makeResultRecord(input caseInput, runIndex int, snapshot preparedSnapshot, result query.Result, executeErr error, elapsed int64, metadata recordMetadata) resultRecord {
	return makeResultRecordWithTrace(input, runIndex, snapshot, result, executeErr, elapsed, metadata, nil)
}

func makeResultRecordWithTrace(input caseInput, runIndex int, snapshot preparedSnapshot, result query.Result, executeErr error, elapsed int64, metadata recordMetadata, trace *queryRetrievalTrace) resultRecord {
	outcome := "success"
	if executeErr != nil {
		outcome = "execution_failure"
	} else if len(result.Results) == 0 {
		outcome = "retrieval_miss"
	}
	identities := make([]resultIdentity, 0, len(result.Results))
	for _, item := range result.Results {
		identities = append(identities, resultIdentity{Slug: item.Slug, Title: item.Title, Type: item.Type})
	}
	if identities == nil {
		identities = []resultIdentity{}
	}
	citations := result.Citations
	if citations == nil {
		citations = []search.Citation{}
	}
	record := resultRecord{
		SchemaVersion:       1,
		CaseID:              input.ID,
		RunIndex:            runIndex,
		Snapshot:            snapshot.label,
		CorpusSHA256:        snapshot.digest,
		Query:               input.Query,
		Mode:                input.Mode,
		Expansion:           result.Expand,
		Results:             identities,
		Citations:           citations,
		Synthesis:           result.AISynth,
		Outcome:             outcome,
		ElapsedMS:           elapsed,
		SourceRevision:      metadata.sourceRevision,
		Provider:            metadata.provider,
		Model:               metadata.model,
		QueryRetrievalTrace: trace,
	}
	if trace != nil {
		record.EvidenceThreshold = trace.EvidenceThreshold
	}
	if executeErr != nil {
		record.ErrorStage = "execute"
		record.ErrorMessage = sanitizeError(executeErr.Error(), snapshot.reader.root, snapshot.label, metadata.apiKey)
	}
	return record
}

func sanitizeError(message, root, label, secret string) string {
	if root != "" {
		message = strings.ReplaceAll(message, root, label)
	}
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return message
}

func openWriterOutput(path string, stdout io.Writer) (recordSink, error) {
	if path != "" {
		cleanPath := filepath.Clean(path)
		temp, err := os.CreateTemp(filepath.Dir(cleanPath), "."+filepath.Base(cleanPath)+".tmp-")
		if err != nil {
			return nil, fmt.Errorf("create output temporary file: %w", err)
		}
		tempPath := temp.Name()
		return &writerSink{
			writer: temp,
			finishFn: func() error {
				if err := temp.Sync(); err != nil {
					_ = temp.Close()
					_ = os.Remove(tempPath)
					return err
				}
				if err := temp.Close(); err != nil {
					_ = os.Remove(tempPath)
					return err
				}
				if err := os.Rename(tempPath, cleanPath); err != nil {
					_ = os.Remove(tempPath)
					return err
				}
				return nil
			},
			abortFn: func() {
				_ = temp.Close()
				_ = os.Remove(tempPath)
			},
		}, nil
	}
	if stdout == nil {
		return nil, errors.New("stdout is nil")
	}
	return &writerSink{writer: stdout}, nil
}

func newProductionExecutor(conceptCache *cache.Cache, cfg config.Config) (query.Executor, error) {
	llmClient := llm.NewClient(cfg.DeepSeekAPIKey)
	expander, err := llm.NewExpander(llmClient, "lifestyle")
	if err != nil {
		return nil, fmt.Errorf("create query expander: %w", err)
	}
	return query.NewService(conceptCache, expander, llmClient), nil
}
