// Package queryconfig owns the sealed, production-shaped query stage config
// artifact. It deliberately depends on queryquality, never the reverse.
package queryconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

const (
	SchemaVersionMin           = 2
	SchemaVersionMax           = 2
	SchemaVersion              = 2
	QueryServiceImplementation = "query-retrieval-pipeline-v2"
	ProviderDeepSeek           = "deepseek"

	QueryExpanderImplementation     = "parallel-minimal-structured-plan-v1"
	CandidateMatcherImplementation  = "lexical-evidence-v1"
	ResultSelectorImplementation    = "evidence-selector-v1"
	AnswerSynthesizerImplementation = "citation-answer-synthesis-v1"
	SeedPolicy                      = "query-derived-or-explicit-v1"
	NoEvidencePolicy                = "typed-insufficient-evidence-terminal-v1"
	ModelPriorFallbackPolicy        = "full-model-prior-fallback-v1"

	SourceLegacyCompatibility        = "legacy_compatibility"
	SourceCorpusDerivedApproximation = "corpus_derived_approximation"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var ErrSchemaV1Unsupported = errors.New("query config schema v1 is unsupported")

const MaxFileBytes int64 = 1 << 20

type Config struct {
	SchemaVersion              int              `json:"schema_version"`
	ConfigRevision             string           `json:"config_revision"`
	ConfigDigest               string           `json:"config_digest"`
	QueryServiceImplementation string           `json:"query_service_implementation"`
	Stages                     Stages           `json:"stages"`
	Profiles                   []Profile        `json:"profiles"`
	ProjectBindings            []ProjectBinding `json:"project_bindings"`
}

// StageConfig is the public name used by callers that persist query stage
// artifacts; Config remains as the concise package-level name.
type StageConfig = Config

type Stages struct {
	QueryExpander     QueryExpanderStage     `json:"query_expander"`
	CandidateMatcher  CandidateMatcherStage  `json:"candidate_matcher"`
	ResultSelector    ResultSelectorStage    `json:"result_selector"`
	AnswerSynthesizer AnswerSynthesizerStage `json:"answer_synthesizer"`
}

type QueryExpanderStage struct {
	Provider             string  `json:"provider"`
	Implementation       string  `json:"implementation"`
	Model                string  `json:"model"`
	Reasoning            string  `json:"reasoning"`
	Temperature          float64 `json:"temperature"`
	DefaultProfileID     string  `json:"default_profile_id"`
	DefaultProfileDigest string  `json:"default_profile_digest"`
	DefaultPromptID      string  `json:"default_prompt_id"`
	DefaultPromptDigest  string  `json:"default_prompt_digest"`
	KeywordsPerAttempt   int     `json:"keywords_per_attempt"`
	Attempts             int     `json:"attempts"`
}

type CandidateMatcherStage struct {
	Implementation                  string `json:"implementation"`
	EvidenceThreshold               int    `json:"evidence_threshold"`
	RareKeywordMaxDocumentFrequency int    `json:"rare_keyword_max_document_frequency"`
}

type ResultSelectorStage struct {
	Implementation   string `json:"implementation"`
	Limit            int    `json:"limit"`
	ExplorationSlots int    `json:"exploration_slots"`
	SeedPolicy       string `json:"seed_policy"`
}

type AnswerSynthesizerStage struct {
	Provider         string  `json:"provider"`
	Implementation   string  `json:"implementation"`
	Model            string  `json:"model"`
	Reasoning        string  `json:"reasoning"`
	Temperature      float64 `json:"temperature"`
	NoEvidencePolicy string  `json:"no_evidence_policy"`
}

type Profile struct {
	ID              string                       `json:"id"`
	CriterionPolicy queryquality.CriterionPolicy `json:"criterion_policy"`
	ProfileDigest   string                       `json:"profile_digest"`
}

type ProjectBinding struct {
	ProjectID      string `json:"project_id"`
	GenerationID   string `json:"generation_id"`
	ConceptsDigest string `json:"concepts_digest"`
	ProfileID      string `json:"profile_id"`
	ProfileDigest  string `json:"profile_digest"`
	PromptID       string `json:"prompt_id"`
	PromptDigest   string `json:"prompt_digest"`
	Source         string `json:"source"`
}

// Seal defensively normalizes input, computes the SHA-256 over the normalized
// document with config_digest omitted, and returns the immutable sealed copy.
func Seal(input Config) (Config, error) {
	normalized, err := Normalize(input)
	if err != nil {
		return Config{}, err
	}
	return normalized, nil
}

// Normalize returns a defensive, deterministic semantic normalization. Profile
// criterion policy order remains untouched because profile digests are order-sensitive.
func Normalize(input Config) (Config, error) {
	config, err := normalizeWithoutDigest(input)
	if err != nil {
		return Config{}, err
	}
	data, err := canonicalWithoutDigest(config)
	if err != nil {
		return Config{}, err
	}
	digest := sha256.Sum256(data)
	config.ConfigDigest = "sha256:" + hex.EncodeToString(digest[:])
	if input.ConfigDigest != "" && input.ConfigDigest != config.ConfigDigest {
		return Config{}, errors.New("config digest mismatch")
	}
	return config, nil
}

func DecodeStrict(data []byte) (Config, error) {
	object, err := strictObject(data)
	if err != nil {
		return Config{}, err
	}
	return decodeConfig(object)
}

// LoadFile reads one bounded, regular, non-symlink sealed artifact. The path
// is intentionally absent from errors so startup logs cannot disclose mounts.
func LoadFile(path string) (Config, error) {
	config, _, err := loadFileCanonicalBytes(path)
	return config, err
}

// LoadFileCanonicalBytes reads and validates one bounded, regular, non-symlink
// sealed artifact and returns a defensive copy of the exact bytes read from
// the securely-opened descriptor. Callers that verify a prebuild artifact's
// canonical representation should compare these bytes without reopening path.
func LoadFileCanonicalBytes(path string) (Config, []byte, error) {
	return loadFileCanonicalBytes(path)
}

func loadFileCanonicalBytes(path string) (Config, []byte, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, nil, errors.New("query config file path is empty")
	}
	file, err := openSecureQueryConfig(path)
	if err != nil {
		if errors.Is(err, errSecureQueryConfigUnsupported) {
			return Config{}, nil, err
		}
		return Config{}, nil, errors.New("query config file is unavailable")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		return Config{}, nil, errors.New("query config file must be a regular file")
	}
	if openedInfo.Mode().Perm()&0o022 != 0 {
		return Config{}, nil, errors.New("query config file must not be group or other writable")
	}
	if openedInfo.Size() <= 0 {
		return Config{}, nil, errors.New("query config file must not be empty")
	}
	if openedInfo.Size() > MaxFileBytes {
		return Config{}, nil, errors.New("query config file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return Config{}, nil, errors.New("query config file cannot be read")
	}
	if int64(len(data)) > MaxFileBytes {
		return Config{}, nil, errors.New("query config file is too large")
	}
	if len(data) == 0 {
		return Config{}, nil, errors.New("query config file must not be empty")
	}
	config, err := DecodeStrict(data)
	if err != nil {
		return Config{}, nil, err
	}
	if err := ValidateSealed(config); err != nil {
		return Config{}, nil, err
	}
	config, err = Normalize(config)
	if err != nil {
		return Config{}, nil, err
	}
	return config, append([]byte(nil), data...), nil
}

func ValidateSealed(config Config) error {
	if config.SchemaVersion == 1 {
		return ErrSchemaV1Unsupported
	}
	if config.ConfigDigest == "" {
		return errors.New("config digest is required")
	}
	normalized, err := Normalize(config)
	if err != nil {
		return err
	}
	if normalized.ConfigDigest != config.ConfigDigest {
		return errors.New("config digest mismatch")
	}
	return nil
}

func CanonicalJSON(config Config) ([]byte, error) {
	normalized, err := Normalize(config)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func Digest(config Config) (string, error) {
	normalized, err := Normalize(config)
	if err != nil {
		return "", err
	}
	return normalized.ConfigDigest, nil
}

func normalizeWithoutDigest(input Config) (Config, error) {
	if input.SchemaVersion == 1 {
		return Config{}, ErrSchemaV1Unsupported
	}
	if input.SchemaVersion < SchemaVersionMin || input.SchemaVersion > SchemaVersionMax {
		return Config{}, fmt.Errorf("unsupported schema_version %d", input.SchemaVersion)
	}
	if err := validateSafeID("config_revision", input.ConfigRevision); err != nil {
		return Config{}, err
	}
	if input.QueryServiceImplementation != QueryServiceImplementation {
		return Config{}, errors.New("unsupported query service implementation")
	}
	config := input
	config.ConfigDigest = ""
	config.Stages = input.Stages
	config.Profiles = append([]Profile(nil), input.Profiles...)
	config.ProjectBindings = append([]ProjectBinding(nil), input.ProjectBindings...)
	for i := range config.Profiles {
		config.Profiles[i].CriterionPolicy.RequiredWhenExplicit = append([]string(nil), input.Profiles[i].CriterionPolicy.RequiredWhenExplicit...)
		config.Profiles[i].CriterionPolicy.PreferredByDefault = append([]string(nil), input.Profiles[i].CriterionPolicy.PreferredByDefault...)
		config.Profiles[i].CriterionPolicy.GoalsToExpand = append([]string(nil), input.Profiles[i].CriterionPolicy.GoalsToExpand...)
	}
	if err := validateStages(config.Stages); err != nil {
		return Config{}, err
	}
	if len(config.Profiles) < 1 {
		return Config{}, errors.New("profiles must not be empty")
	}
	profiles := make(map[string]Profile, len(config.Profiles))
	for i := range config.Profiles {
		profile := &config.Profiles[i]
		validated, err := (queryquality.RetrievalProfile{ID: profile.ID, CriterionPolicy: profile.CriterionPolicy}).ValidatedCopy()
		if err != nil {
			return Config{}, fmt.Errorf("profile %q: %w", profile.ID, err)
		}
		if _, exists := profiles[validated.ID]; exists {
			return Config{}, fmt.Errorf("duplicate profile id %q", validated.ID)
		}
		digest, err := validated.Digest()
		if err != nil {
			return Config{}, fmt.Errorf("profile %q digest: %w", validated.ID, err)
		}
		if profile.ProfileDigest != digest {
			return Config{}, fmt.Errorf("profile %q digest mismatch", validated.ID)
		}
		profile.ID = validated.ID
		profile.CriterionPolicy = validated.CriterionPolicy
		profiles[profile.ID] = *profile
	}
	defaultProfile := queryquality.DefaultRetrievalProfile()
	defaultDigest, _ := defaultProfile.Digest()
	defaultProfileEntry, ok := profiles[defaultProfile.ID]
	if !ok || defaultProfileEntry.ProfileDigest != defaultDigest || !samePolicy(defaultProfileEntry.CriterionPolicy, defaultProfile.CriterionPolicy) {
		return Config{}, errors.New("immutable lifestyle default profile is required")
	}
	if config.Stages.QueryExpander.DefaultProfileID != defaultProfile.ID || config.Stages.QueryExpander.DefaultProfileDigest != defaultDigest {
		return Config{}, errors.New("query expander default profile reference mismatch")
	}
	if err := queryquality.ValidatePrompt(config.Stages.QueryExpander.DefaultPromptID, config.Stages.QueryExpander.DefaultPromptDigest); err != nil {
		return Config{}, fmt.Errorf("query expander default prompt: %w", err)
	}
	sort.Slice(config.Profiles, func(i, j int) bool { return config.Profiles[i].ID < config.Profiles[j].ID })
	config.ProjectBindings = append([]ProjectBinding(nil), config.ProjectBindings...)
	seenBindings := map[string]struct{}{}
	for i := range config.ProjectBindings {
		binding := &config.ProjectBindings[i]
		if err := validateBinding(*binding, profiles); err != nil {
			return Config{}, fmt.Errorf("project binding %d: %w", i+1, err)
		}
		key := bindingScopeKey(*binding)
		if _, exists := seenBindings[key]; exists {
			return Config{}, errors.New("duplicate project binding")
		}
		seenBindings[key] = struct{}{}
	}
	sort.Slice(config.ProjectBindings, func(i, j int) bool {
		return bindingKey(config.ProjectBindings[i]) < bindingKey(config.ProjectBindings[j])
	})
	return config, nil
}

func validateStages(stages Stages) error {
	expander := stages.QueryExpander
	if expander.Provider != ProviderDeepSeek || expander.Implementation != QueryExpanderImplementation || expander.Model != "deepseek-v4-flash" || expander.Reasoning != "none" || expander.Temperature != 0 {
		return errors.New("invalid query_expander provider/implementation/model/reasoning/temperature")
	}
	if expander.KeywordsPerAttempt < 1 || expander.KeywordsPerAttempt > 100 || expander.Attempts < 1 || expander.Attempts > 10 {
		return errors.New("invalid query_expander ranges")
	}
	matcher := stages.CandidateMatcher
	if matcher.Implementation != CandidateMatcherImplementation || matcher.EvidenceThreshold < 1 || matcher.EvidenceThreshold > 100 || matcher.RareKeywordMaxDocumentFrequency < 1 || matcher.RareKeywordMaxDocumentFrequency > 1000 {
		return errors.New("invalid candidate_matcher")
	}
	selector := stages.ResultSelector
	if selector.Implementation != ResultSelectorImplementation || selector.Limit < 1 || selector.Limit > 1000 || selector.ExplorationSlots < 0 || selector.ExplorationSlots > selector.Limit || selector.SeedPolicy != SeedPolicy {
		return errors.New("invalid result_selector")
	}
	synthesizer := stages.AnswerSynthesizer
	if synthesizer.Provider != ProviderDeepSeek || synthesizer.Implementation != AnswerSynthesizerImplementation || synthesizer.Model != "deepseek-v4-pro" || (synthesizer.Reasoning != "none" && synthesizer.Reasoning != "low" && synthesizer.Reasoning != "high" && synthesizer.Reasoning != "max") || synthesizer.Temperature != 0 || (synthesizer.NoEvidencePolicy != NoEvidencePolicy && synthesizer.NoEvidencePolicy != ModelPriorFallbackPolicy) {
		return errors.New("invalid answer_synthesizer provider/implementation/model/reasoning/temperature")
	}
	for name, value := range map[string]string{"default_profile_id": expander.DefaultProfileID, "default_prompt_id": expander.DefaultPromptID, "default_profile_digest": expander.DefaultProfileDigest, "default_prompt_digest": expander.DefaultPromptDigest} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

func validateBinding(binding ProjectBinding, profiles map[string]Profile) error {
	for name, value := range map[string]string{"project_id": binding.ProjectID, "generation_id": binding.GenerationID} {
		if err := validateSafeID(name, value); err != nil {
			return err
		}
	}
	if err := validateDigest("concepts_digest", binding.ConceptsDigest); err != nil {
		return err
	}
	profile, ok := profiles[binding.ProfileID]
	if !ok || profile.ProfileDigest != binding.ProfileDigest {
		return errors.New("binding profile reference mismatch")
	}
	if err := queryquality.ValidatePrompt(binding.PromptID, binding.PromptDigest); err != nil {
		return fmt.Errorf("binding prompt: %w", err)
	}
	if binding.Source != SourceLegacyCompatibility && binding.Source != SourceCorpusDerivedApproximation {
		return fmt.Errorf("unsupported binding source %q", binding.Source)
	}
	return nil
}

func validateSafeID(name, value string) error {
	if value == "" || len(value) > 128 || !safeID.MatchString(value) || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("%s is not a safe identifier", name)
	}
	return nil
}

func validateDigest(name, value string) error {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return fmt.Errorf("%s must be a full sha256 digest", name)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be lowercase hexadecimal sha256", name)
	}
	return nil
}

func samePolicy(a, b queryquality.CriterionPolicy) bool {
	return strings.Join(a.RequiredWhenExplicit, "\x00") == strings.Join(b.RequiredWhenExplicit, "\x00") && strings.Join(a.PreferredByDefault, "\x00") == strings.Join(b.PreferredByDefault, "\x00") && strings.Join(a.GoalsToExpand, "\x00") == strings.Join(b.GoalsToExpand, "\x00")
}

func bindingKey(binding ProjectBinding) string {
	return binding.ProjectID + "\x00" + binding.GenerationID + "\x00" + binding.ConceptsDigest + "\x00" + binding.ProfileID + "\x00" + binding.PromptID + "\x00" + binding.Source
}

func bindingScopeKey(binding ProjectBinding) string {
	return binding.ProjectID + "\x00" + binding.GenerationID + "\x00" + binding.ConceptsDigest
}

func canonicalWithoutDigest(config Config) ([]byte, error) {
	copy := config
	copy.ConfigDigest = ""
	return json.Marshal(struct {
		SchemaVersion              int              `json:"schema_version"`
		ConfigRevision             string           `json:"config_revision"`
		QueryServiceImplementation string           `json:"query_service_implementation"`
		Stages                     Stages           `json:"stages"`
		Profiles                   []Profile        `json:"profiles"`
		ProjectBindings            []ProjectBinding `json:"project_bindings"`
	}{copy.SchemaVersion, copy.ConfigRevision, copy.QueryServiceImplementation, copy.Stages, copy.Profiles, copy.ProjectBindings})
}

func strictObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("config must be a JSON object")
	}
	object := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid field name: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("field name must be a string")
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		object[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid config object")
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeConfig(object map[string]json.RawMessage) (Config, error) {
	if err := fields(object, map[string]bool{"schema_version": true, "config_revision": true, "config_digest": true, "query_service_implementation": true, "stages": true, "profiles": true, "project_bindings": true}); err != nil {
		return Config{}, err
	}
	var result Config
	var err error
	if result.SchemaVersion, err = intField(object, "schema_version"); err != nil {
		return Config{}, err
	}
	if result.SchemaVersion == 1 {
		return Config{}, ErrSchemaV1Unsupported
	}
	if result.ConfigRevision, err = stringField(object, "config_revision"); err != nil {
		return Config{}, err
	}
	if result.ConfigDigest, err = stringField(object, "config_digest"); err != nil {
		return Config{}, err
	}
	if result.QueryServiceImplementation, err = stringField(object, "query_service_implementation"); err != nil {
		return Config{}, err
	}
	if result.Stages, err = decodeStages(object["stages"]); err != nil {
		return Config{}, fmt.Errorf("stages: %w", err)
	}
	if result.Profiles, err = decodeProfiles(object["profiles"]); err != nil {
		return Config{}, fmt.Errorf("profiles: %w", err)
	}
	if result.ProjectBindings, err = decodeBindings(object["project_bindings"]); err != nil {
		return Config{}, fmt.Errorf("project_bindings: %w", err)
	}
	if err := ValidateSealed(result); err != nil {
		return Config{}, err
	}
	return result, nil
}

func decodeStages(data json.RawMessage) (Stages, error) {
	object, err := strictObject(data)
	if err != nil {
		return Stages{}, err
	}
	if err := fields(object, map[string]bool{"query_expander": true, "candidate_matcher": true, "result_selector": true, "answer_synthesizer": true}); err != nil {
		return Stages{}, err
	}
	var result Stages
	if result.QueryExpander, err = decodeQueryExpander(object["query_expander"]); err != nil {
		return Stages{}, fmt.Errorf("query_expander: %w", err)
	}
	if result.CandidateMatcher, err = decodeCandidateMatcher(object["candidate_matcher"]); err != nil {
		return Stages{}, fmt.Errorf("candidate_matcher: %w", err)
	}
	if result.ResultSelector, err = decodeResultSelector(object["result_selector"]); err != nil {
		return Stages{}, fmt.Errorf("result_selector: %w", err)
	}
	if result.AnswerSynthesizer, err = decodeAnswerSynthesizer(object["answer_synthesizer"]); err != nil {
		return Stages{}, fmt.Errorf("answer_synthesizer: %w", err)
	}
	return result, nil
}

func decodeQueryExpander(data json.RawMessage) (QueryExpanderStage, error) {
	o, err := strictObject(data)
	if err != nil {
		return QueryExpanderStage{}, err
	}
	if err := fields(o, map[string]bool{"provider": true, "implementation": true, "model": true, "reasoning": true, "temperature": true, "default_profile_id": true, "default_profile_digest": true, "default_prompt_id": true, "default_prompt_digest": true, "keywords_per_attempt": true, "attempts": true}); err != nil {
		return QueryExpanderStage{}, err
	}
	result := QueryExpanderStage{}
	if result.Provider, err = stringField(o, "provider"); err != nil {
		return result, err
	}
	if result.Implementation, err = stringField(o, "implementation"); err != nil {
		return result, err
	}
	if result.Model, err = stringField(o, "model"); err != nil {
		return result, err
	}
	if result.Reasoning, err = stringField(o, "reasoning"); err != nil {
		return result, err
	}
	if result.Temperature, err = floatField(o, "temperature"); err != nil {
		return result, err
	}
	if result.DefaultProfileID, err = stringField(o, "default_profile_id"); err != nil {
		return result, err
	}
	if result.DefaultProfileDigest, err = stringField(o, "default_profile_digest"); err != nil {
		return result, err
	}
	if result.DefaultPromptID, err = stringField(o, "default_prompt_id"); err != nil {
		return result, err
	}
	if result.DefaultPromptDigest, err = stringField(o, "default_prompt_digest"); err != nil {
		return result, err
	}
	if result.KeywordsPerAttempt, err = intField(o, "keywords_per_attempt"); err != nil {
		return result, err
	}
	if result.Attempts, err = intField(o, "attempts"); err != nil {
		return result, err
	}
	return result, nil
}

func decodeCandidateMatcher(data json.RawMessage) (CandidateMatcherStage, error) {
	o, err := strictObject(data)
	if err != nil {
		return CandidateMatcherStage{}, err
	}
	if err := fields(o, map[string]bool{"implementation": true, "evidence_threshold": true, "rare_keyword_max_document_frequency": true}); err != nil {
		return CandidateMatcherStage{}, err
	}
	result := CandidateMatcherStage{}
	if result.Implementation, err = stringField(o, "implementation"); err != nil {
		return result, err
	}
	if result.EvidenceThreshold, err = intField(o, "evidence_threshold"); err != nil {
		return result, err
	}
	if result.RareKeywordMaxDocumentFrequency, err = intField(o, "rare_keyword_max_document_frequency"); err != nil {
		return result, err
	}
	return result, nil
}
func decodeResultSelector(data json.RawMessage) (ResultSelectorStage, error) {
	o, err := strictObject(data)
	if err != nil {
		return ResultSelectorStage{}, err
	}
	if err := fields(o, map[string]bool{"implementation": true, "limit": true, "exploration_slots": true, "seed_policy": true}); err != nil {
		return ResultSelectorStage{}, err
	}
	result := ResultSelectorStage{}
	if result.Implementation, err = stringField(o, "implementation"); err != nil {
		return result, err
	}
	if result.Limit, err = intField(o, "limit"); err != nil {
		return result, err
	}
	if result.ExplorationSlots, err = intField(o, "exploration_slots"); err != nil {
		return result, err
	}
	if result.SeedPolicy, err = stringField(o, "seed_policy"); err != nil {
		return result, err
	}
	return result, nil
}
func decodeAnswerSynthesizer(data json.RawMessage) (AnswerSynthesizerStage, error) {
	o, err := strictObject(data)
	if err != nil {
		return AnswerSynthesizerStage{}, err
	}
	if err := fields(o, map[string]bool{"provider": true, "implementation": true, "model": true, "reasoning": true, "temperature": true, "no_evidence_policy": true}); err != nil {
		return AnswerSynthesizerStage{}, err
	}
	result := AnswerSynthesizerStage{}
	if result.Provider, err = stringField(o, "provider"); err != nil {
		return result, err
	}
	if result.Implementation, err = stringField(o, "implementation"); err != nil {
		return result, err
	}
	if result.Model, err = stringField(o, "model"); err != nil {
		return result, err
	}
	if result.Reasoning, err = stringField(o, "reasoning"); err != nil {
		return result, err
	}
	if result.Temperature, err = floatField(o, "temperature"); err != nil {
		return result, err
	}
	if result.NoEvidencePolicy, err = stringField(o, "no_evidence_policy"); err != nil {
		return result, err
	}
	return result, nil
}

func decodeProfiles(data json.RawMessage) ([]Profile, error) {
	raws, err := strictArray(data)
	if err != nil {
		return nil, err
	}
	result := make([]Profile, 0, len(raws))
	for _, raw := range raws {
		o, err := strictObject(raw)
		if err != nil {
			return nil, err
		}
		if err := fields(o, map[string]bool{"id": true, "criterion_policy": true, "profile_digest": true}); err != nil {
			return nil, err
		}
		p := Profile{}
		if p.ID, err = stringField(o, "id"); err != nil {
			return nil, err
		}
		if p.CriterionPolicy, err = decodePolicy(o["criterion_policy"]); err != nil {
			return nil, err
		}
		if p.ProfileDigest, err = stringField(o, "profile_digest"); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

func decodePolicy(data json.RawMessage) (queryquality.CriterionPolicy, error) {
	o, err := strictObject(data)
	if err != nil {
		return queryquality.CriterionPolicy{}, err
	}
	if err := fields(o, map[string]bool{"required_when_explicit": true, "preferred_by_default": true, "goals_to_expand": true}); err != nil {
		return queryquality.CriterionPolicy{}, err
	}
	result := queryquality.CriterionPolicy{}
	if result.RequiredWhenExplicit, err = stringArray(o["required_when_explicit"]); err != nil {
		return result, err
	}
	if result.PreferredByDefault, err = stringArray(o["preferred_by_default"]); err != nil {
		return result, err
	}
	if result.GoalsToExpand, err = stringArray(o["goals_to_expand"]); err != nil {
		return result, err
	}
	return result, nil
}
func decodeBindings(data json.RawMessage) ([]ProjectBinding, error) {
	raws, err := strictArray(data)
	if err != nil {
		return nil, err
	}
	result := make([]ProjectBinding, 0, len(raws))
	for _, raw := range raws {
		o, err := strictObject(raw)
		if err != nil {
			return nil, err
		}
		if err := fields(o, map[string]bool{"project_id": true, "generation_id": true, "concepts_digest": true, "profile_id": true, "profile_digest": true, "prompt_id": true, "prompt_digest": true, "source": true}); err != nil {
			return nil, err
		}
		b := ProjectBinding{}
		for key, target := range map[string]*string{"project_id": &b.ProjectID, "generation_id": &b.GenerationID, "concepts_digest": &b.ConceptsDigest, "profile_id": &b.ProfileID, "profile_digest": &b.ProfileDigest, "prompt_id": &b.PromptID, "prompt_digest": &b.PromptDigest, "source": &b.Source} {
			*target, err = stringField(o, key)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, b)
	}
	return result, nil
}

func fields(object map[string]json.RawMessage, allowed map[string]bool) error {
	for key := range object {
		if !allowed[key] {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	for key := range allowed {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("field %q is required", key)
		}
	}
	return nil
}
func stringField(object map[string]json.RawMessage, key string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", fmt.Errorf("field %q is required", key)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("field %q must not be null", key)
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("field %q must be a string", key)
	}
	return result, nil
}
func intField(object map[string]json.RawMessage, key string) (int, error) {
	raw, ok := object[key]
	if !ok {
		return 0, fmt.Errorf("field %q is required", key)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, fmt.Errorf("field %q must not be null", key)
	}
	var result int
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("field %q must be an integer", key)
	}
	return result, nil
}
func floatField(object map[string]json.RawMessage, key string) (float64, error) {
	raw, ok := object[key]
	if !ok {
		return 0, fmt.Errorf("field %q is required", key)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, fmt.Errorf("field %q must not be null", key)
	}
	var result float64
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("field %q must be a number", key)
	}
	return result, nil
}
func stringArray(data json.RawMessage) ([]string, error) {
	raws, err := strictArray(data)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(raws))
	for _, raw := range raws {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("array values must be strings")
		}
		result = append(result, value)
	}
	return result, nil
}
func strictArray(data []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("field must be an array")
	}
	result := []json.RawMessage{}
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		result = append(result, raw)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, errors.New("invalid array")
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	return result, nil
}
func ensureEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON")
	}
	return fmt.Errorf("trailing JSON: %w", err)
}
