package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func buildStageConfig(options experimentOptions, variant fixtureVariant, prepared preparedSnapshot, retrievalOptions queryRetrievalOptions) (queryconfig.Config, error) {
	if options.service != serviceQueryRetrieval {
		return queryconfig.Config{}, errors.New("stage config requires --service query-retrieval")
	}
	if strings.TrimSpace(options.configRevision) == "" {
		return queryconfig.Config{}, errors.New("--config-revision is required with --stage-config-output")
	}
	if len(variant.Profile.ID) == 0 {
		return queryconfig.Config{}, errors.New("selected profile is empty")
	}
	if variant.Model.Provider != "deepseek" || variant.Model.Model != "deepseek-v4-flash" {
		return queryconfig.Config{}, errors.New("stage config requires allowlisted deepseek-v4-flash provider model")
	}
	if variant.Model.Temperature == nil || *variant.Model.Temperature != 0 || variant.Model.Reasoning != "none" {
		return queryconfig.Config{}, errors.New("stage config requires reasoning none and temperature 0")
	}
	if err := queryquality.ValidatePromptTemplate(variant.Prompt.ID, variant.Prompt.SystemTemplate, variant.Prompt.UserTemplate); err != nil {
		return queryconfig.Config{}, fmt.Errorf("selected prompt: %w", err)
	}
	prompt, ok := queryquality.LookupPrompt(variant.Prompt.ID)
	if !ok {
		return queryconfig.Config{}, errors.New("selected prompt is not built in")
	}
	if variant.Prompt.TemplateDigest == "" || variant.Prompt.TemplateDigest != prompt.TemplateDigest {
		return queryconfig.Config{}, errors.New("selected prompt template digest mismatch")
	}
	profile, err := variant.Profile.retrievalProfile()
	if err != nil {
		return queryconfig.Config{}, fmt.Errorf("selected profile: %w", err)
	}
	if profile.ID == queryquality.DefaultRetrievalProfile().ID {
		defaultDigest, digestErr := queryquality.DefaultRetrievalProfile().Digest()
		profileDigest, profileErr := profile.Digest()
		if digestErr != nil || profileErr != nil || profileDigest != defaultDigest {
			return queryconfig.Config{}, errors.New("selected immutable lifestyle default profile mismatch")
		}
	}
	profileDigest, err := profile.Digest()
	if err != nil {
		return queryconfig.Config{}, err
	}
	defaultProfile := queryquality.DefaultRetrievalProfile()
	defaultProfileDigest, err := defaultProfile.Digest()
	if err != nil {
		return queryconfig.Config{}, err
	}
	lifestylePrompt, ok := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	if !ok {
		return queryconfig.Config{}, errors.New("lifestyle prompt is not built in")
	}
	conceptsDigest := "sha256:" + strings.TrimPrefix(prepared.digest, "sha256:")
	if options.conceptsDigest != "" {
		conceptsDigest = options.conceptsDigest
		if conceptsDigest != "sha256:"+strings.TrimPrefix(prepared.digest, "sha256:") {
			return queryconfig.Config{}, errors.New("concepts digest does not match frozen snapshot")
		}
	}
	if strings.TrimSpace(options.projectID) == "" || strings.TrimSpace(prepared.generationID) == "" || strings.TrimSpace(conceptsDigest) == "" {
		return queryconfig.Config{}, errors.New("stage config requires project id, generation id, and concepts digest")
	}
	if retrievalOptions, err = normalizeQueryRetrievalOptions(retrievalOptions); err != nil {
		return queryconfig.Config{}, err
	}
	config := queryconfig.Config{
		SchemaVersion:              queryconfig.SchemaVersion,
		ConfigRevision:             options.configRevision,
		QueryServiceImplementation: queryconfig.QueryServiceImplementation,
		Stages: queryconfig.Stages{
			QueryExpander: queryconfig.QueryExpanderStage{
				Provider: queryconfig.ProviderDeepSeek, Implementation: queryconfig.QueryExpanderImplementation, Model: "deepseek-v4-flash", Reasoning: "none", Temperature: 0,
				DefaultProfileID: defaultProfile.ID, DefaultProfileDigest: defaultProfileDigest,
				DefaultPromptID: queryquality.StructuredPlanPromptID, DefaultPromptDigest: lifestylePrompt.TemplateDigest,
				KeywordsPerAttempt: retrievalOptions.keywordsPerAttempt, Attempts: retrievalOptions.expansionAttempts,
			},
			CandidateMatcher:  queryconfig.CandidateMatcherStage{Implementation: queryconfig.CandidateMatcherImplementation, EvidenceThreshold: retrievalOptions.evidenceThreshold, RareKeywordMaxDocumentFrequency: retrievalOptions.rareDocumentFrequency},
			ResultSelector:    queryconfig.ResultSelectorStage{Implementation: queryconfig.ResultSelectorImplementation, Limit: retrievalOptions.selectionLimit, ExplorationSlots: retrievalOptions.explorationSlots, SeedPolicy: queryconfig.SeedPolicy},
			AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{Provider: queryconfig.ProviderDeepSeek, Implementation: queryconfig.AnswerSynthesizerImplementation, Model: "deepseek-v4-pro", Reasoning: "none", Temperature: 0, NoEvidencePolicy: queryconfig.NoEvidencePolicy},
		},
		Profiles:        []queryconfig.Profile{{ID: defaultProfile.ID, CriterionPolicy: defaultProfile.CriterionPolicy, ProfileDigest: defaultProfileDigest}},
		ProjectBindings: []queryconfig.ProjectBinding{{ProjectID: options.projectID, GenerationID: prepared.generationID, ConceptsDigest: conceptsDigest, ProfileID: profile.ID, ProfileDigest: profileDigest, PromptID: prompt.ID, PromptDigest: prompt.TemplateDigest, Source: queryconfig.SourceCorpusDerivedApproximation}},
	}
	if profile.ID != defaultProfile.ID {
		config.Profiles = append(config.Profiles, queryconfig.Profile{ID: profile.ID, CriterionPolicy: profile.CriterionPolicy, ProfileDigest: profileDigest})
	}
	return queryconfig.Seal(config)
}

func writeStageConfig(path string, config queryconfig.Config) error {
	if err := validateOutputPath(path); err != nil {
		return err
	}
	if err := queryconfig.ValidateSealed(config); err != nil {
		return err
	}
	data, err := queryconfig.CanonicalJSON(config)
	if err != nil {
		return err
	}
	clean := filepath.Clean(path)
	temp, err := os.CreateTemp(filepath.Dir(clean), "."+filepath.Base(clean)+".tmp-")
	if err != nil {
		return fmt.Errorf("create stage config temporary file: %w", err)
	}
	tempPath := temp.Name()
	remove := func() { _ = temp.Close(); _ = os.Remove(tempPath) }
	if _, err := temp.Write(data); err != nil {
		remove()
		return err
	}
	if err := temp.Sync(); err != nil {
		remove()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, clean); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}
