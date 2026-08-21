package main

import (
	"bytes"
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
		SchemaVersion: 2, ConfigRevision: "rev", QueryServiceImplementation: queryconfig.QueryServiceImplementation,
		Stages: queryconfig.Stages{
			QueryExpander:     queryconfig.QueryExpanderStage{Provider: queryconfig.ProviderDeepSeek, Implementation: queryconfig.QueryExpanderImplementation, Model: "deepseek-v4-flash", Reasoning: "none", Temperature: 0, DefaultProfileID: profile.ID, DefaultProfileDigest: profileDigest, DefaultPromptID: prompt.ID, DefaultPromptDigest: prompt.TemplateDigest, KeywordsPerAttempt: 24, Attempts: 3},
			CandidateMatcher:  queryconfig.CandidateMatcherStage{Implementation: queryconfig.CandidateMatcherImplementation, EvidenceThreshold: 2, RareKeywordMaxDocumentFrequency: 1},
			ResultSelector:    queryconfig.ResultSelectorStage{Implementation: queryconfig.ResultSelectorImplementation, Limit: 10, ExplorationSlots: 1, SeedPolicy: queryconfig.SeedPolicy},
			AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{Provider: queryconfig.ProviderDeepSeek, Implementation: queryconfig.AnswerSynthesizerImplementation, Model: "deepseek-v4-pro", Reasoning: "none", Temperature: 0, NoEvidencePolicy: queryconfig.NoEvidencePolicy},
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
	canonical, err := queryconfig.CanonicalJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, canonical) || bytes.HasSuffix(data, []byte{'\n'}) {
		t.Fatalf("stage config bytes are not canonical and newline-free")
	}
	for _, forbidden := range []string{"base_url", "api_key", "system_template", "user_template", "raw_query", "concepts.jsonl"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("artifact contains forbidden %q: %s", forbidden, data)
		}
	}
}

func TestBuildStageConfigMatchesPromotedDEVArtifact(t *testing.T) {
	const (
		artifactPath   = "../../configs/query/dev/query-dev-2026-08-21.1.json"
		configDigest   = "sha256:a35955fe4a451c740e6252cae8087f114fbac6b4162245d3de7818c1ad37a5c6"
		profileDigest  = "sha256:264a3b8f6d9cb72886ab83784cfb81f06216d2df791717430420da0c6e168483"
		projectID      = "94071fede0c0"
		generationID   = "g_8548cab213bd54c6a536249135a7d7ee"
		conceptsDigest = "sha256:344e4ca50268ec88e1831d977b9461e5900573149f4206e5978630ec2ee8ae29"
	)
	prompt, ok := queryquality.LookupPrompt(queryquality.DomainNeutralTechnicalPromptID)
	if !ok {
		t.Fatal("missing technical prompt")
	}
	systemTemplate, userTemplate, ok := queryquality.LookupPromptTemplate(prompt.ID)
	if !ok {
		t.Fatal("missing technical prompt template")
	}
	variant := fixtureVariant{
		Profile: profileFixtureEntry{
			ID:                   "corpus-derived-tech-document-v1",
			RequiredWhenExplicit: []string{"explicit_exclusion"},
			PreferredByDefault:   []string{"entity", "technology", "component", "service", "process", "mechanism", "document_topic"},
			GoalsToExpand:        []string{"explanation", "interaction", "causal_relation", "discovery"},
		},
		Prompt: promptFixtureEntry{ID: prompt.ID, SystemTemplate: systemTemplate, UserTemplate: userTemplate, TemplateDigest: prompt.TemplateDigest},
		Model:  modelFixtureEntry{Provider: queryconfig.ProviderDeepSeek, Model: "deepseek-v4-flash", Temperature: float64Pointer(0), Reasoning: "none"},
	}
	options := experimentOptions{service: serviceQueryRetrieval, configRevision: "query-dev-2026-08-21.1", projectID: projectID, conceptsDigest: conceptsDigest}
	config, err := buildStageConfig(options, variant, preparedSnapshot{digest: strings.TrimPrefix(conceptsDigest, "sha256:"), generationID: generationID}, defaultQueryRetrievalOptions())
	if err != nil {
		t.Fatal(err)
	}
	if config.ConfigDigest != configDigest || config.Profiles[0].ProfileDigest != profileDigest {
		t.Fatalf("config digest/profile digest = %s/%s", config.ConfigDigest, config.Profiles[0].ProfileDigest)
	}
	canonical, err := queryconfig.CanonicalJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, artifact) {
		t.Fatalf("CLI-built canonical artifact differs from promoted artifact")
	}
}

func float64Pointer(value float64) *float64 { return &value }
