package queryconfig_test

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

const (
	devArtifactPath                = "../../configs/query/dev/query-dev-2026-08-21.1.json"
	promotionConfigPathEnv         = "QUERY_STAGE_CONFIG_PROMOTION_PATH"
	approvedConfigDigest           = "sha256:a35955fe4a451c740e6252cae8087f114fbac6b4162245d3de7818c1ad37a5c6"
	approvedTechnicalProfileDigest = "sha256:264a3b8f6d9cb72886ab83784cfb81f06216d2df791717430420da0c6e168483"
	approvedTechnicalPromptDigest  = "sha256:a7ee7d783ad78eb591040f8acfbfc1902eeba34c4bc07045fe8ec1b8fe07604e"
	approvedLifestyleProfileDigest = "sha256:acb45fdc24658e69ea5971b4839bd0ebd38790bb9f195eef15b1c75fa4fbaef2"
	approvedLifestylePromptDigest  = "sha256:c32fc23377849702e143751430ea6d7d3a31f8361a4c850ec795e57b70866bd7"
)

var approvedTechnicalPolicy = queryquality.CriterionPolicy{
	RequiredWhenExplicit: []string{"explicit_exclusion"},
	PreferredByDefault:   []string{"entity", "technology", "component", "service", "process", "mechanism", "document_topic"},
	GoalsToExpand:        []string{"explanation", "interaction", "causal_relation", "discovery"},
}

func approvedStages() queryconfig.Stages {
	return queryconfig.Stages{
		QueryExpander: queryconfig.QueryExpanderStage{
			Provider:             "deepseek",
			Implementation:       "parallel-minimal-structured-plan-v1",
			Model:                "deepseek-v4-flash",
			Reasoning:            "none",
			Temperature:          0,
			DefaultProfileID:     "platform-owned-lifestyle-v1",
			DefaultProfileDigest: approvedLifestyleProfileDigest,
			DefaultPromptID:      "minimal-v1",
			DefaultPromptDigest:  approvedLifestylePromptDigest,
			KeywordsPerAttempt:   24,
			Attempts:             3,
		},
		CandidateMatcher: queryconfig.CandidateMatcherStage{
			Implementation:                  "lexical-evidence-v1",
			EvidenceThreshold:               2,
			RareKeywordMaxDocumentFrequency: 1,
		},
		ResultSelector: queryconfig.ResultSelectorStage{
			Implementation:   "evidence-selector-v1",
			Limit:            10,
			ExplorationSlots: 1,
			SeedPolicy:       "query-derived-or-explicit-v1",
		},
		AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{
			Provider:         "deepseek",
			Implementation:   "citation-answer-synthesis-v1",
			Model:            "deepseek-v4-pro",
			Reasoning:        "none",
			Temperature:      0,
			NoEvidencePolicy: "typed-insufficient-evidence-terminal-v1",
		},
	}
}

func approvedProfiles() []queryconfig.Profile {
	return []queryconfig.Profile{
		{ID: "corpus-derived-tech-document-v1", CriterionPolicy: approvedTechnicalPolicy, ProfileDigest: approvedTechnicalProfileDigest},
		{ID: "platform-owned-lifestyle-v1", CriterionPolicy: queryquality.CriterionPolicy{
			RequiredWhenExplicit: []string{"location", "explicit_exclusion"},
			PreferredByDefault:   []string{"venue_type", "activity", "audience", "setting"},
			GoalsToExpand:        []string{"suitability", "recommendation", "discovery"},
		}, ProfileDigest: approvedLifestyleProfileDigest},
	}
}

func approvedBinding() queryconfig.ProjectBinding {
	return queryconfig.ProjectBinding{
		ProjectID:      "94071fede0c0",
		GenerationID:   "g_8548cab213bd54c6a536249135a7d7ee",
		ConceptsDigest: "sha256:344e4ca50268ec88e1831d977b9461e5900573149f4206e5978630ec2ee8ae29",
		ProfileID:      "corpus-derived-tech-document-v1",
		ProfileDigest:  approvedTechnicalProfileDigest,
		PromptID:       "domain-neutral-technical-v1",
		PromptDigest:   approvedTechnicalPromptDigest,
		Source:         queryconfig.SourceCorpusDerivedApproximation,
	}
}

func loadStrictArtifact(t *testing.T, path string) (queryconfig.Config, []byte, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := queryconfig.DecodeStrict(raw)
	if err != nil {
		t.Fatalf("DecodeStrict(%q): %v", path, err)
	}
	loaded, err := queryconfig.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(%q): %v", path, err)
	}
	canonical, err := queryconfig.CanonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, loaded) {
		t.Fatalf("strict decode and LoadFile differ for %q", path)
	}
	if !bytes.Equal(bytes.TrimSuffix(raw, []byte("\n")), canonical) {
		t.Fatalf("%q is not the exact CanonicalJSON output", path)
	}
	return loaded, raw, canonical
}

func assertApprovedArtifact(t *testing.T, config queryconfig.Config, raw []byte) {
	t.Helper()
	if config.SchemaVersion != 2 || config.ConfigRevision != "query-dev-2026-08-21.1" || config.ConfigDigest != approvedConfigDigest || config.QueryServiceImplementation != "query-retrieval-pipeline-v2" {
		t.Fatalf("global identity=%+v", config)
	}
	if !reflect.DeepEqual(config.Stages, approvedStages()) {
		t.Fatalf("stages=%+v", config.Stages)
	}
	if !reflect.DeepEqual(config.Profiles, approvedProfiles()) {
		t.Fatalf("profiles=%+v", config.Profiles)
	}
	if !reflect.DeepEqual(config.ProjectBindings, []queryconfig.ProjectBinding{approvedBinding()}) {
		t.Fatalf("bindings=%+v", config.ProjectBindings)
	}
	for _, forbidden := range []string{"api_key", "base_url", "credential", "provider_response", "system_template", "user_template", "raw_query", "receipt", "fixture", "concepts.jsonl"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("artifact contains forbidden %q", forbidden)
		}
	}
}

func TestReviewedDevArtifactStrictLoadsAndRoundTrips(t *testing.T) {
	config, raw, _ := loadStrictArtifact(t, devArtifactPath)
	assertApprovedArtifact(t, config, raw)
}

func TestPromotedArtifactParityWhenPathSupplied(t *testing.T) {
	promotedPath := strings.TrimSpace(os.Getenv(promotionConfigPathEnv))
	if promotedPath == "" {
		t.Skip("promotion config path not supplied")
	}
	promoted, promotedRaw, promotedCanonical := loadStrictArtifact(t, promotedPath)
	assertApprovedArtifact(t, promoted, promotedRaw)
	repo, repoRaw, repoCanonical := loadStrictArtifact(t, devArtifactPath)
	assertApprovedArtifact(t, repo, repoRaw)
	if !reflect.DeepEqual(promoted, repo) {
		t.Fatalf("promoted config differs semantically from repo artifact")
	}
	if !bytes.Equal(promotedCanonical, repoCanonical) {
		t.Fatalf("promoted config differs canonically from repo artifact")
	}
}

func TestPriorPlaceholderPolicyCannotSatisfyApprovedArtifactWhenSelfSealed(t *testing.T) {
	config, _, _ := loadStrictArtifact(t, devArtifactPath)
	placeholder := queryquality.CriterionPolicy{
		RequiredWhenExplicit: []string{"topic"},
		PreferredByDefault:   []string{"system"},
		GoalsToExpand:        []string{"discovery"},
	}
	technicalIndex := -1
	for i, profile := range config.Profiles {
		if profile.ID == "corpus-derived-tech-document-v1" {
			technicalIndex = i
			config.Profiles[i].CriterionPolicy = placeholder
			profileDigest, err := (queryquality.RetrievalProfile{ID: profile.ID, CriterionPolicy: placeholder}).Digest()
			if err != nil {
				t.Fatal(err)
			}
			config.Profiles[i].ProfileDigest = profileDigest
			config.ProjectBindings[0].ProfileDigest = profileDigest
		}
	}
	if technicalIndex < 0 {
		t.Fatal("technical profile missing")
	}
	config.ConfigDigest = ""
	sealed, err := queryconfig.Seal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := queryconfig.ValidateSealed(sealed); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(sealed.Profiles[technicalIndex].CriterionPolicy, approvedTechnicalPolicy) || sealed.ConfigDigest == approvedConfigDigest {
		t.Fatal("self-sealed placeholder policy satisfied the approved artifact contract")
	}
	if sealed.Profiles[technicalIndex].ProfileDigest == approvedTechnicalProfileDigest {
		t.Fatal("placeholder policy retained the approved technical profile digest")
	}
	if _, err := queryconfig.CanonicalJSON(sealed); err != nil {
		t.Fatalf("canonical self-sealed placeholder: %v", err)
	}
}
