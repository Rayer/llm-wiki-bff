package queryconfig_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
)

func TestResolverExactBindingAndEffectiveDigestFactors(t *testing.T) {
	resolver, err := queryconfig.NewResolver(mustSeal(t))
	if err != nil {
		t.Fatal(err)
	}
	effective, err := resolver.Resolve(queryconfig.GenerationIdentity{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if effective.Profile.ID != "technical-v1" || effective.PromptID != "domain-neutral-technical-v1" || !effective.ExactBinding || effective.BindingSource != queryconfig.SourceCorpusDerivedApproximation {
		t.Fatalf("effective=%+v", effective)
	}
	if effective.Options.SelectionLimit != 10 || effective.Options.KeywordsPerAttempt != 24 || effective.Options.ExpansionAttempts != 3 {
		t.Fatalf("options=%+v", effective.Options)
	}
	base := effective.EffectiveConfigDigest
	for name, mutate := range map[string]func(*queryconfig.Config){
		"revision": func(c *queryconfig.Config) { c.ConfigRevision = "operator-2026-08-21" },
		"profile": func(c *queryconfig.Config) {
			c.ProjectBindings[0].ProfileID = c.Profiles[0].ID
			c.ProjectBindings[0].ProfileDigest = c.Profiles[0].ProfileDigest
		},
		"prompt": func(c *queryconfig.Config) {
			c.ProjectBindings[0].PromptID = c.Stages.QueryExpander.DefaultPromptID
			c.ProjectBindings[0].PromptDigest = c.Stages.QueryExpander.DefaultPromptDigest
		},
		"options":    func(c *queryconfig.Config) { c.Stages.ResultSelector.Limit = 9 },
		"generation": func(c *queryconfig.Config) { c.ProjectBindings[0].GenerationID = "generation-8" },
	} {
		t.Run(name, func(t *testing.T) {
			config := mustSeal(t)
			mutate(&config)
			config.ConfigDigest = ""
			sealed, err := queryconfig.Seal(config)
			if err != nil {
				t.Fatal(err)
			}
			other, err := queryconfig.NewResolver(sealed)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := other.Resolve(queryconfig.GenerationIdentity{ProjectID: "project-a", GenerationID: sealed.ProjectBindings[0].GenerationID, ConceptsDigest: sealed.ProjectBindings[0].ConceptsDigest})
			if err != nil {
				t.Fatal(err)
			}
			if resolved.EffectiveConfigDigest == base {
				t.Fatalf("independent %s factor did not alter effective digest", name)
			}
		})
	}
}

func TestResolverDefaultAndBindingMismatchFailClosed(t *testing.T) {
	resolver, err := queryconfig.NewResolver(mustSeal(t))
	if err != nil {
		t.Fatal(err)
	}
	defaultConfig, err := resolver.Resolve(queryconfig.GenerationIdentity{ProjectID: "unlisted", GenerationID: "legacy", ConceptsDigest: ""})
	if err != nil || defaultConfig.Profile.ID != "platform-owned-lifestyle-v1" || defaultConfig.BindingSource != queryconfig.SourceLegacyCompatibility || defaultConfig.ExactBinding {
		t.Fatalf("default=%+v err=%v", defaultConfig, err)
	}
	for _, identity := range []queryconfig.GenerationIdentity{
		{ProjectID: "project-a", GenerationID: "wrong-generation", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)},
		{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("b", 64)},
		{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: ""},
	} {
		if _, err := resolver.Resolve(identity); !errors.Is(err, queryconfig.ErrBindingMismatch) {
			t.Fatalf("identity=%+v err=%v, want ErrBindingMismatch", identity, err)
		}
	}
}

func TestResolverDefensivelyCopiesEffectiveConfig(t *testing.T) {
	resolver, err := queryconfig.NewResolver(mustSeal(t))
	if err != nil {
		t.Fatal(err)
	}
	effective, err := resolver.Resolve(queryconfig.GenerationIdentity{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	effective.Profile.CriterionPolicy.RequiredWhenExplicit[0] = "mutated"
	again, err := resolver.Resolve(queryconfig.GenerationIdentity{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)})
	if err != nil || again.Profile.CriterionPolicy.RequiredWhenExplicit[0] == "mutated" {
		t.Fatalf("resolver was mutated: %+v err=%v", again, err)
	}
}

func TestResolverSupportsMultipleImmutableGenerationsPerProject(t *testing.T) {
	config := mustSeal(t)
	second := config.ProjectBindings[0]
	second.GenerationID = "generation-8"
	second.ConceptsDigest = "sha256:" + strings.Repeat("b", 64)
	third := second
	third.GenerationID = "generation-9"
	third.ConceptsDigest = "sha256:" + strings.Repeat("c", 64)
	defaultProfile := config.Profiles[0]
	third.ProfileID = defaultProfile.ID
	third.ProfileDigest = defaultProfile.ProfileDigest
	third.PromptID = config.Stages.QueryExpander.DefaultPromptID
	third.PromptDigest = config.Stages.QueryExpander.DefaultPromptDigest
	config.ProjectBindings = append(config.ProjectBindings, second, third)
	config.ConfigDigest = ""
	sealed, err := queryconfig.Seal(config)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := queryconfig.NewResolver(sealed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		generation, digest, profile, prompt string
	}{
		{"generation-7", "sha256:" + strings.Repeat("a", 64), "technical-v1", "domain-neutral-technical-v1"},
		{"generation-8", "sha256:" + strings.Repeat("b", 64), "technical-v1", "domain-neutral-technical-v1"},
		{"generation-9", "sha256:" + strings.Repeat("c", 64), defaultProfile.ID, config.Stages.QueryExpander.DefaultPromptID},
	} {
		got, err := resolver.Resolve(queryconfig.GenerationIdentity{ProjectID: "project-a", GenerationID: want.generation, ConceptsDigest: want.digest})
		if err != nil || got.Profile.ID != want.profile || got.PromptID != want.prompt || !got.ExactBinding {
			t.Fatalf("generation=%s got=%+v err=%v", want.generation, got, err)
		}
	}
	for _, identity := range []queryconfig.GenerationIdentity{
		{ProjectID: "project-a", GenerationID: "unknown", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)},
		{ProjectID: "project-a", GenerationID: "generation-8", ConceptsDigest: "sha256:" + strings.Repeat("d", 64)},
	} {
		if _, err := resolver.Resolve(identity); !errors.Is(err, queryconfig.ErrBindingMismatch) {
			t.Fatalf("identity=%+v err=%v, want ErrBindingMismatch", identity, err)
		}
	}

	reversed := sealed
	reversed.ProjectBindings = append([]queryconfig.ProjectBinding(nil), sealed.ProjectBindings...)
	for left, right := 0, len(reversed.ProjectBindings)-1; left < right; left, right = left+1, right-1 {
		reversed.ProjectBindings[left], reversed.ProjectBindings[right] = reversed.ProjectBindings[right], reversed.ProjectBindings[left]
	}
	reversed.ConfigDigest = ""
	reversed, err = queryconfig.Seal(reversed)
	if err != nil || reversed.ConfigDigest != sealed.ConfigDigest {
		t.Fatalf("binding order changed config digest: got=%q want=%q err=%v", reversed.ConfigDigest, sealed.ConfigDigest, err)
	}
}
