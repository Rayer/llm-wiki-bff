package queryconfig_test

import (
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
)

func TestLWC280NewPolicyIsImmutableAndReachesRuntimeIdentity(t *testing.T) {
	config, err := queryconfig.LoadFile("../../configs/query/dev/query-dev-2026-08-31.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if config.Stages.AnswerSynthesizer.NoEvidencePolicy != queryconfig.ModelPriorFallbackPolicy {
		t.Fatalf("policy=%q", config.Stages.AnswerSynthesizer.NoEvidencePolicy)
	}
	resolver, err := queryconfig.NewResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	identity := resolver.EffectiveConfigs()[0].RuntimeConfigIdentity()
	if identity.NoEvidencePolicy != queryconfig.ModelPriorFallbackPolicy {
		t.Fatalf("runtime policy=%q", identity.NoEvidencePolicy)
	}
}
