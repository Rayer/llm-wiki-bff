package queryconfig_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"golang.org/x/sys/unix"
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
		SchemaVersion:              2,
		ConfigRevision:             "operator-2026-08-20",
		QueryServiceImplementation: queryconfig.QueryServiceImplementation,
		Stages: queryconfig.Stages{
			QueryExpander: queryconfig.QueryExpanderStage{
				Provider:             queryconfig.ProviderDeepSeek,
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
				Provider:         queryconfig.ProviderDeepSeek,
				Implementation:   queryconfig.AnswerSynthesizerImplementation,
				Model:            "deepseek-v4-pro",
				Reasoning:        "none",
				Temperature:      0,
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

func TestLoadFileRejectsUnsafeFilesAndBadDocuments(t *testing.T) {
	dir := t.TempDir()
	valid, err := json.Marshal(mustSeal(t))
	if err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(validPath, valid, 0o444); err != nil {
		t.Fatal(err)
	}
	loaded, err := queryconfig.LoadFile(validPath)
	if err != nil || loaded.ConfigDigest == "" {
		t.Fatalf("valid load=%+v err=%v", loaded, err)
	}
	for _, test := range []struct {
		name string
		path string
		data []byte
	}{
		{"missing", filepath.Join(dir, "missing.json"), nil},
		{"directory", dir, nil},
		{"oversized", filepath.Join(dir, "large.json"), bytes.Repeat([]byte("x"), int(queryconfig.MaxFileBytes)+1)},
		{"duplicate", filepath.Join(dir, "duplicate.json"), append(valid[:len(valid)-1], []byte(`,"schema_version":2}`)...)},
		{"trailing", filepath.Join(dir, "trailing.json"), append(valid, []byte(` {}`)...)},
	} {
		if test.data != nil {
			if err := os.WriteFile(test.path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(test.name, func(t *testing.T) {
			if _, err := queryconfig.LoadFile(test.path); err == nil {
				t.Fatal("accepted invalid file")
			}
		})
	}
	symlink := filepath.Join(dir, "link.json")
	if err := os.Symlink(validPath, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := queryconfig.LoadFile(symlink); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestLoadFileModeBoundaryAndSpecialFileRejection(t *testing.T) {
	dir := t.TempDir()
	data := mustJSON(mustSeal(t))
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o644, 0o600, 0o400} {
		t.Run(fmt.Sprintf("mode-%04o", mode), func(t *testing.T) {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := queryconfig.LoadFile(path); err != nil {
				t.Fatalf("LoadFile(%04o) error = %v", mode, err)
			}
		})
	}
	for _, mode := range []os.FileMode{0o666, 0o602} {
		t.Run(fmt.Sprintf("writable-%04o", mode), func(t *testing.T) {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := queryconfig.LoadFile(path); err == nil {
				t.Fatalf("LoadFile(%04o) accepted writable file", mode)
			}
		})
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := queryconfig.LoadFile(empty); err == nil {
		t.Fatal("LoadFile accepted empty file")
	}

	fifo := filepath.Join(dir, "config.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := queryconfig.LoadFile(fifo); err == nil {
		t.Fatal("LoadFile accepted FIFO")
	}
}

func TestLoadFileCanonicalBytesRemainBoundToOpenedArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	movedPath := filepath.Join(dir, "moved.json")
	want := mustJSON(mustSeal(t))
	if err := os.WriteFile(path, want, 0o444); err != nil {
		t.Fatal(err)
	}
	loaded, raw, err := queryconfig.LoadFileCanonicalBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigRevision != "operator-2026-08-20" || !bytes.Equal(raw, want) {
		t.Fatal("canonical bytes did not match the opened artifact")
	}
	if err := os.Rename(path, movedPath); err != nil {
		t.Fatal(err)
	}
	attacker := validConfig(t)
	attacker.ConfigRevision = "attacker-revision"
	attackerBytes, err := json.Marshal(mustSealConfig(t, attacker))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, attackerBytes, 0o444); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) || loaded.ConfigRevision == "attacker-revision" {
		t.Fatal("returned config or bytes changed after pathname replacement")
	}
}

func TestLoadFileSymlinkSwapNeverLoadsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	safe, err := json.Marshal(mustSeal(t))
	if err != nil {
		t.Fatal(err)
	}
	attackerConfig := validConfig(t)
	attackerConfig.ConfigRevision = "attacker-revision"
	attacker, err := queryconfig.Seal(attackerConfig)
	if err != nil {
		t.Fatal(err)
	}
	attackerData, err := json.Marshal(attacker)
	if err != nil {
		t.Fatal(err)
	}
	safePath := filepath.Join(dir, "safe.json")
	attackerPath := filepath.Join(dir, "attacker.json")
	candidatePath := filepath.Join(dir, "candidate.json")
	tempPath := filepath.Join(dir, "swap.tmp")
	for path, data := range map[string][]byte{safePath: safe, attackerPath: attackerData, candidatePath: safe} {
		if err := os.WriteFile(path, data, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Link(safePath, tempPath)
			_ = os.Rename(tempPath, candidatePath)
			_ = os.Symlink(attackerPath, tempPath)
			_ = os.Rename(tempPath, candidatePath)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	for i := 0; i < 1000; i++ {
		loaded, err := queryconfig.LoadFile(candidatePath)
		if err == nil && loaded.ConfigRevision == "attacker-revision" {
			t.Fatal("LoadFile followed a symlink during pathname swap")
		}
	}
}

func TestStrictDecodeRejectsSchemaBoundariesUnknownFieldsAndTrailingJSON(t *testing.T) {
	base := string(mustJSON(mustSeal(t)))
	for _, test := range []struct {
		name string
		data string
	}{
		{"v1 unsupported", strings.Replace(base, `"schema_version":2`, `"schema_version":1`, 1)},
		{"below minimum", strings.Replace(base, `"schema_version":2`, `"schema_version":0`, 1)},
		{"above maximum", strings.Replace(base, `"schema_version":2`, `"schema_version":3`, 1)},
		{"nested unknown", strings.Replace(base, `"implementation":"parallel-minimal-structured-plan-v1"`, `"extra":true,"implementation":"parallel-minimal-structured-plan-v1"`, 1)},
		{"missing expansion provider", strings.Replace(base, `"provider":"deepseek","implementation":"parallel-minimal-structured-plan-v1",`, `"implementation":"parallel-minimal-structured-plan-v1",`, 1)},
		{"missing synthesis temperature", strings.Replace(base, `"temperature":0,"no_evidence_policy"`, `"no_evidence_policy"`, 1)},
		{"trailing JSON", base + " {}"},
		{"secret-like field", strings.Replace(base, `"config_revision"`, `"api_key":"secret","config_revision"`, 1)},
		{"missing query service revision", strings.Replace(base, `"query_service_implementation":"`+queryconfig.QueryServiceImplementation+`",`, "", 1)},
		{"unknown query service revision", strings.Replace(base, queryconfig.QueryServiceImplementation, "query-retrieval-pipeline-v1", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := queryconfig.DecodeStrict([]byte(test.data)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	if _, err := queryconfig.DecodeStrict([]byte(strings.Replace(base, `"schema_version":2`, `"schema_version":1`, 1))); !errors.Is(err, queryconfig.ErrSchemaV1Unsupported) {
		t.Fatalf("v1 error=%v, want ErrSchemaV1Unsupported", err)
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

func TestSealRejectsDuplicateBindingResolutionScope(t *testing.T) {
	config := validConfig(t)
	binding := config.ProjectBindings[0]
	binding.Source = queryconfig.SourceLegacyCompatibility
	config.ProjectBindings = append(config.ProjectBindings, binding)
	if _, err := queryconfig.Seal(config); err == nil {
		t.Fatal("accepted duplicate binding resolution scope")
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
	return mustSealConfig(t, validConfig(t))
}

func mustSealConfig(t *testing.T, config queryconfig.Config) queryconfig.Config {
	t.Helper()
	sealed, err := queryconfig.Seal(config)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
