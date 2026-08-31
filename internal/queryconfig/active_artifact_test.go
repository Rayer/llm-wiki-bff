package queryconfig_test

import (
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
)

func TestActiveArtifactUsesDefaultProfileAcrossGenerationRevisions(t *testing.T) {
	config, err := queryconfig.LoadFile("../../configs/query/dev/query-dev-2026-08-31.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ProjectBindings) != 0 {
		t.Fatalf("active artifact has %d project bindings, want none", len(config.ProjectBindings))
	}
	resolver, err := queryconfig.NewResolver(config)
	if err != nil {
		t.Fatal(err)
	}

	for _, identity := range []queryconfig.GenerationIdentity{
		{ProjectID: "project-a", GenerationID: "g_changed_1", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)},
		{ProjectID: "project-a", GenerationID: "g_changed_2", ConceptsDigest: "sha256:" + strings.Repeat("b", 64)},
	} {
		effective, err := resolver.Resolve(identity)
		if err != nil {
			t.Fatalf("identity=%+v: %v", identity, err)
		}
		if effective.Profile.ID != "platform-owned-lifestyle-v1" || effective.PromptID != "minimal-v1" || effective.ExactBinding || effective.BindingSource != queryconfig.SourceLegacyCompatibility {
			t.Fatalf("identity=%+v effective=%+v", identity, effective)
		}
		runtime := effective.RuntimeConfigIdentity()
		if runtime.GenerationID != identity.GenerationID || runtime.ConceptsDigest != identity.ConceptsDigest {
			t.Fatalf("identity=%+v runtime=%+v", identity, runtime)
		}
	}
}
