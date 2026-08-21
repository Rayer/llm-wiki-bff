package queryconfig_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
)

func TestReviewedDevArtifactStrictLoadsAndRoundTrips(t *testing.T) {
	path := "../../configs/query/dev/query-dev-2026-08-21.1.json"
	config, err := queryconfig.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != 2 || config.ConfigRevision != "query-dev-2026-08-21.1" || config.ConfigDigest != "sha256:46511d0cacf5a33b7c81cac771fbdcc9b7f5a14aaf4f1a5fcb39c0b28e550694" {
		t.Fatalf("global identity=%+v", config)
	}
	if config.Stages.QueryExpander.Model != "deepseek-v4-flash" || config.Stages.QueryExpander.Reasoning != "none" || config.Stages.QueryExpander.Temperature != 0 || config.Stages.QueryExpander.Attempts != 3 || config.Stages.QueryExpander.KeywordsPerAttempt != 24 {
		t.Fatalf("expansion stage=%+v", config.Stages.QueryExpander)
	}
	if config.Stages.CandidateMatcher.EvidenceThreshold != 2 || config.Stages.CandidateMatcher.RareKeywordMaxDocumentFrequency != 1 || config.Stages.ResultSelector.Limit != 10 || config.Stages.ResultSelector.ExplorationSlots != 1 {
		t.Fatalf("retrieval stages=%+v/%+v", config.Stages.CandidateMatcher, config.Stages.ResultSelector)
	}
	if config.Stages.AnswerSynthesizer.Model != "deepseek-v4-pro" || config.Stages.AnswerSynthesizer.Reasoning != "none" || config.Stages.AnswerSynthesizer.Temperature != 0 {
		t.Fatalf("synthesis stage=%+v", config.Stages.AnswerSynthesizer)
	}
	if len(config.Profiles) != 2 || len(config.ProjectBindings) != 1 {
		t.Fatalf("profiles=%d bindings=%d", len(config.Profiles), len(config.ProjectBindings))
	}
	binding := config.ProjectBindings[0]
	if binding.ProjectID != "94071fede0c0" || binding.GenerationID != "g_8548cab213bd54c6a536249135a7d7ee" || binding.ConceptsDigest != "sha256:344e4ca50268ec88e1831d977b9461e5900573149f4206e5978630ec2ee8ae29" || binding.PromptID != "domain-neutral-technical-v1" || binding.Source != queryconfig.SourceCorpusDerivedApproximation {
		t.Fatalf("binding=%+v", binding)
	}
	roundTrip, err := queryconfig.CanonicalJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSuffix(original, []byte("\n")), roundTrip) {
		t.Fatalf("artifact is not canonical round-trip")
	}
	if strings.Contains(string(original), "api_key") || strings.Contains(string(original), "prompt text") || strings.Contains(string(original), "{{raw_query}}") {
		t.Fatal("artifact contains forbidden secret or prompt content")
	}
}
