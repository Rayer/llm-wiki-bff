package queryconfig_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
)

const (
	lwc281ArtifactPath = "../../configs/query/dev/query-dev-2026-08-21.2.json"
	lwc281ConfigDigest = "sha256:75e4f76de991b496c503b42fd893d34408ddae726fe99003365a5c89b8e46642"
)

func TestLWC281TechnicalBindingsAreSealedAndFailClosed(t *testing.T) {
	config, raw, _ := loadStrictArtifact(t, lwc281ArtifactPath)
	if config.ConfigRevision != "query-dev-2026-08-21.2" {
		t.Fatalf("revision = %q", config.ConfigRevision)
	}
	if config.ConfigDigest != lwc281ConfigDigest {
		t.Fatalf("config digest = %q", config.ConfigDigest)
	}
	if len(config.ProjectBindings) != 3 {
		t.Fatalf("binding count = %d", len(config.ProjectBindings))
	}
	for _, forbidden := range []string{"api_key", "credential", "provider_response", "raw_query", "receipt", "fixture"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("artifact contains forbidden %q", forbidden)
		}
	}

	resolver, err := queryconfig.NewResolver(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []queryconfig.GenerationIdentity{
		{ProjectID: "94071fede0c0", GenerationID: "g_8548cab213bd54c6a536249135a7d7ee", ConceptsDigest: "sha256:344e4ca50268ec88e1831d977b9461e5900573149f4206e5978630ec2ee8ae29"},
		{ProjectID: "a75fc933e2c7", GenerationID: "g_070384ada24da3b2918fc68cb7112e77", ConceptsDigest: "sha256:c4f9dc0aa745cc9fc510d7627ac64d3211b329a0eabb56dce0a600d7034fc349"},
		{ProjectID: "a75fc933e2c7", GenerationID: "g_e993a45f6d3f939e49170aa758cb5863", ConceptsDigest: "sha256:8c8c803ffe8cae83a1019efccd89371a6716cf8f64a682ced2c709baa8e737eb"},
	} {
		effective, err := resolver.Resolve(identity)
		if err != nil {
			t.Fatalf("resolve %+v: %v", identity, err)
		}
		if effective.Profile.ID != "corpus-derived-tech-document-v1" || effective.PromptID != "domain-neutral-technical-v1" || !effective.ExactBinding {
			t.Fatalf("effective %+v = %+v", identity, effective)
		}
	}
	for _, identity := range []queryconfig.GenerationIdentity{
		{ProjectID: "a75fc933e2c7", GenerationID: "unknown", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)},
		{ProjectID: "a75fc933e2c7", GenerationID: "g_070384ada24da3b2918fc68cb7112e77", ConceptsDigest: "sha256:" + strings.Repeat("b", 64)},
	} {
		if _, err := resolver.Resolve(identity); !errors.Is(err, queryconfig.ErrBindingMismatch) {
			t.Fatalf("resolve %+v error = %v, want binding mismatch", identity, err)
		}
	}
	legacy, err := resolver.Resolve(queryconfig.GenerationIdentity{ProjectID: "3892e6e3bb16", GenerationID: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ExactBinding || legacy.BindingSource != queryconfig.SourceLegacyCompatibility || legacy.Profile.ID != "platform-owned-lifestyle-v1" {
		t.Fatalf("legacy effective = %+v", legacy)
	}
}
