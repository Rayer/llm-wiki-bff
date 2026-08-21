package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func TestStageConfigFlagsRejectNonRetrievalAndMissingRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stage.json")
	if err := (experimentOptions{service: serviceProduction, stageConfigOutput: path, configRevision: "rev"}).validateStageConfigFlags(); err == nil {
		t.Fatal("accepted production stage config output")
	}
	if err := (experimentOptions{service: serviceQueryRetrieval, stageConfigOutput: path}).validateStageConfigFlags(); err == nil {
		t.Fatal("accepted stage config output without revision")
	}
}

func TestBuildStageConfigRejectsAmbiguousOrUnfrozenVariantBeforeProviderUse(t *testing.T) {
	variant := fixtureVariant{Profile: profileFixtureEntry{ID: "technical"}, Model: modelFixtureEntry{Provider: "other", Model: "deepseek-v4-flash"}, Prompt: promptFixtureEntry{ID: queryquality.DomainNeutralTechnicalPromptID}}
	options := experimentOptions{service: serviceQueryRetrieval, configRevision: "rev", projectID: "project", stageConfigOutput: filepath.Join(t.TempDir(), "stage.json")}
	if _, err := buildStageConfig(options, variant, preparedSnapshot{digest: strings.Repeat("a", 64), generationID: "generation"}, defaultQueryRetrievalOptions()); err == nil {
		t.Fatal("accepted unsupported provider")
	}
	if _, err := buildStageConfig(options, variant, preparedSnapshot{digest: strings.Repeat("a", 64)}, defaultQueryRetrievalOptions()); err == nil {
		t.Fatal("accepted missing frozen generation")
	}
}

func TestWriteStageConfigIsAtomicRegularJSONWithoutExperimentSecrets(t *testing.T) {
	profile := queryquality.DefaultRetrievalProfile()
	profileDigest, err := profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	prompt, ok := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	if !ok {
		t.Fatal("missing prompt")
	}
	config := queryconfig.Config{
		SchemaVersion: 1, ConfigRevision: "rev", QueryServiceImplementation: queryconfig.QueryServiceImplementation,
		Stages: queryconfig.Stages{
			QueryExpander:     queryconfig.QueryExpanderStage{Implementation: queryconfig.QueryExpanderImplementation, Model: "deepseek-v4-flash", Reasoning: "none", DefaultProfileID: profile.ID, DefaultProfileDigest: profileDigest, DefaultPromptID: prompt.ID, DefaultPromptDigest: prompt.TemplateDigest, KeywordsPerAttempt: 24, Attempts: 3},
			CandidateMatcher:  queryconfig.CandidateMatcherStage{Implementation: queryconfig.CandidateMatcherImplementation, EvidenceThreshold: 2, RareKeywordMaxDocumentFrequency: 1},
			ResultSelector:    queryconfig.ResultSelectorStage{Implementation: queryconfig.ResultSelectorImplementation, Limit: 10, ExplorationSlots: 1, SeedPolicy: queryconfig.SeedPolicy},
			AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{Implementation: queryconfig.AnswerSynthesizerImplementation, Model: "deepseek-v4-pro", Reasoning: "none", NoEvidencePolicy: queryconfig.NoEvidencePolicy},
		},
		Profiles:        []queryconfig.Profile{{ID: profile.ID, CriterionPolicy: profile.CriterionPolicy, ProfileDigest: profileDigest}},
		ProjectBindings: []queryconfig.ProjectBinding{{ProjectID: "project", GenerationID: "generation", ConceptsDigest: "sha256:" + strings.Repeat("a", 64), ProfileID: profile.ID, ProfileDigest: profileDigest, PromptID: prompt.ID, PromptDigest: prompt.TemplateDigest, Source: queryconfig.SourceCorpusDerivedApproximation}},
	}
	config, err = queryconfig.Seal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stage.json")
	if err := writeStageConfig(path, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded queryconfig.Config
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.ConfigDigest != config.ConfigDigest {
		t.Fatalf("artifact=%s err=%v", data, err)
	}
	for _, forbidden := range []string{"base_url", "api_key", "system_template", "user_template", "raw_query", "concepts.jsonl"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("artifact contains forbidden %q: %s", forbidden, data)
		}
	}
}
