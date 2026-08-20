package queryconfig_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func validConfig(t *testing.T) queryconfig.Config {
	t.Helper()
	defaultProfile := queryquality.DefaultRetrievalProfile()
	defaultDigest, err := defaultProfile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	selected := queryquality.RetrievalProfile{
		ID: "technical-v1",
		CriterionPolicy: queryquality.CriterionPolicy{
			RequiredWhenExplicit: []string{"component"},
			PreferredByDefault:   []string{"technology"},
			GoalsToExpand:        []string{"explanation"},
		},
	}
	selectedDigest, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	lifestylePrompt, ok := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	if !ok {
		t.Fatal("missing lifestyle prompt")
	}
	technicalPrompt, ok := queryquality.LookupPrompt("domain-neutral-technical-v1")
	if !ok {
		t.Fatal("missing technical prompt")
	}
	return queryconfig.Config{
		SchemaVersion:  1,
		ConfigRevision: "operator-2026-08-20",
		Stages: queryconfig.Stages{
			QueryExpander: queryconfig.QueryExpanderStage{
				Implementation:       queryconfig.QueryExpanderImplementation,
				Model:                "deepseek-v4-flash",
				Reasoning:            "none",
				Temperature:          0,
				DefaultProfileID:     defaultProfile.ID,
				DefaultProfileDigest: defaultDigest,
				DefaultPromptID:      queryquality.StructuredPlanPromptID,
				DefaultPromptDigest:  lifestylePrompt.TemplateDigest,
				KeywordsPerAttempt:   24,
				Attempts:             3,
			},
			CandidateMatcher: queryconfig.CandidateMatcherStage{
				Implementation:                  queryconfig.CandidateMatcherImplementation,
				EvidenceThreshold:               2,
				RareKeywordMaxDocumentFrequency: 1,
			},
			ResultSelector: queryconfig.ResultSelectorStage{
				Implementation:   queryconfig.ResultSelectorImplementation,
				Limit:            10,
				ExplorationSlots: 1,
				SeedPolicy:       queryconfig.SeedPolicy,
			},
			AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{
				Implementation:   queryconfig.AnswerSynthesizerImplementation,
				Model:            "deepseek-v4-pro",
				Reasoning:        "none",
				NoEvidencePolicy: queryconfig.NoEvidencePolicy,
			},
		},
		Profiles: []queryconfig.Profile{
			{ID: defaultProfile.ID, CriterionPolicy: defaultProfile.CriterionPolicy, ProfileDigest: defaultDigest},
			{ID: selected.ID, CriterionPolicy: selected.CriterionPolicy, ProfileDigest: selectedDigest},
		},
		ProjectBindings: []queryconfig.ProjectBinding{{
			ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64),
			ProfileID: selected.ID, ProfileDigest: selectedDigest, PromptID: technicalPrompt.ID, PromptDigest: technicalPrompt.TemplateDigest,
			Source: queryconfig.SourceCorpusDerivedApproximation,
		}},
	}
}

func TestSealDecodeStrictAndValidateSealed(t *testing.T) {
	sealed, err := queryconfig.Seal(validConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed.ConfigDigest, "sha256:") || len(sealed.ConfigDigest) != len("sha256:")+64 {
		t.Fatalf("digest = %q", sealed.ConfigDigest)
	}
	data, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := queryconfig.DecodeStrict(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := queryconfig.ValidateSealed(decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, mustJSON(decoded)) {
		t.Fatalf("sealed JSON is not stable:\n%s\n%s", data, mustJSON(decoded))
	}
}

func TestStrictDecodeRejectsSchemaBoundariesUnknownFieldsAndTrailingJSON(t *testing.T) {
	base := string(mustJSON(mustSeal(t)))
	for _, test := range []struct {
		name string
		data string
	}{
		{"below minimum", strings.Replace(base, `"schema_version":1`, `"schema_version":0`, 1)},
		{"above maximum", strings.Replace(base, `"schema_version":1`, `"schema_version":2`, 1)},
		{"nested unknown", strings.Replace(base, `"implementation":"parallel-minimal-structured-plan-v1"`, `"extra":true,"implementation":"parallel-minimal-structured-plan-v1"`, 1)},
		{"trailing JSON", base + " {}"},
		{"secret-like field", strings.Replace(base, `"config_revision"`, `"api_key":"secret","config_revision"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := queryconfig.DecodeStrict([]byte(test.data)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestSealRejectsReferencesRangesAndUnsafeIDs(t *testing.T) {
	base := validConfig(t)
	tests := []struct {
		name   string
		mutate func(*queryconfig.Config)
	}{
		{"duplicate profiles", func(c *queryconfig.Config) { c.Profiles = append(c.Profiles, c.Profiles[1]) }},
		{"profile digest mismatch", func(c *queryconfig.Config) { c.Profiles[1].ProfileDigest = "sha256:" + strings.Repeat("b", 64) }},
		{"binding profile mismatch", func(c *queryconfig.Config) { c.ProjectBindings[0].ProfileDigest = c.Profiles[0].ProfileDigest }},
		{"prompt digest mismatch", func(c *queryconfig.Config) { c.ProjectBindings[0].PromptDigest = "sha256:" + strings.Repeat("b", 64) }},
		{"invalid range", func(c *queryconfig.Config) { c.Stages.ResultSelector.Limit = 0 }},
		{"unsafe revision", func(c *queryconfig.Config) { c.ConfigRevision = "../secret" }},
		{"unsupported implementation", func(c *queryconfig.Config) { c.Stages.ResultSelector.Implementation = "random" }},
		{"unsupported reasoning", func(c *queryconfig.Config) { c.Stages.AnswerSynthesizer.Reasoning = "medium" }},
		{"unsupported source", func(c *queryconfig.Config) { c.ProjectBindings[0].Source = "user" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			if _, err := queryconfig.Seal(config); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestNormalizeIsDeterministicAndSemanticChangesAlterDigest(t *testing.T) {
	left := mustSeal(t)
	right := validConfig(t)
	right.Profiles = []queryconfig.Profile{right.Profiles[1], right.Profiles[0]}
	right.ProjectBindings = append([]queryconfig.ProjectBinding(nil), right.ProjectBindings...)
	sealed, err := queryconfig.Seal(right)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ConfigDigest != left.ConfigDigest {
		t.Fatalf("catalog order changed digest: %q != %q", sealed.ConfigDigest, left.ConfigDigest)
	}
	changed := validConfig(t)
	changed.Profiles[1].CriterionPolicy.RequiredWhenExplicit[0] = "service"
	changedProfile := queryquality.RetrievalProfile{ID: changed.Profiles[1].ID, CriterionPolicy: changed.Profiles[1].CriterionPolicy}
	changed.Profiles[1].ProfileDigest, err = changedProfile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	changed.ProjectBindings[0].ProfileDigest = changed.Profiles[1].ProfileDigest
	changedSealed, err := queryconfig.Seal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedSealed.ConfigDigest == left.ConfigDigest || changedSealed.Profiles[1].ProfileDigest == left.Profiles[1].ProfileDigest {
		t.Fatal("semantic change did not alter digests")
	}
	ordered := validConfig(t)
	ordered.Profiles[1].CriterionPolicy.RequiredWhenExplicit = []string{"service", "component"}
	orderedProfile := queryquality.RetrievalProfile{ID: ordered.Profiles[1].ID, CriterionPolicy: ordered.Profiles[1].CriterionPolicy}
	ordered.Profiles[1].ProfileDigest, err = orderedProfile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ordered.ProjectBindings[0].ProfileDigest = ordered.Profiles[1].ProfileDigest
	orderedSealed, err := queryconfig.Seal(ordered)
	if err != nil {
		t.Fatal(err)
	}
	if orderedSealed.Profiles[1].ProfileDigest == changedSealed.Profiles[1].ProfileDigest {
		t.Fatal("criterion order did not alter profile digest")
	}
}

func TestSealInputAndOutputAreMutationIsolated(t *testing.T) {
	input := validConfig(t)
	sealed, err := queryconfig.Seal(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Profiles[1].CriterionPolicy.RequiredWhenExplicit[0] = "mutated"
	if err := queryconfig.ValidateSealed(sealed); err != nil {
		t.Fatalf("sealed config aliases input: %v", err)
	}
	sealed.Profiles[1].CriterionPolicy.RequiredWhenExplicit[0] = "output-mutated"
	if sealed.Profiles[1].CriterionPolicy.RequiredWhenExplicit[0] == input.Profiles[1].CriterionPolicy.RequiredWhenExplicit[0] {
		t.Fatal("output aliases input")
	}
}

func mustSeal(t *testing.T) queryconfig.Config {
	t.Helper()
	sealed, err := queryconfig.Seal(validConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
