package queryruntime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

type strictIdentityProvider struct {
	identity  llm.ModelIdentity
	chatCalls atomic.Int32
}

type noIdentityProvider struct{}

func (noIdentityProvider) Chat(context.Context, string, string) (string, error) { return "", nil }

func (p *strictIdentityProvider) Chat(context.Context, string, string) (string, error) {
	p.chatCalls.Add(1)
	return "", errors.New("unexpected constructor chat")
}

func (p *strictIdentityProvider) ModelIdentity() (llm.ModelIdentity, bool) {
	return p.identity, true
}

type strictExecutor struct{}

func (strictExecutor) Execute(context.Context, cache.Reader, query.Request) (query.Result, error) {
	return query.Result{}, nil
}

type strictSynthesizer struct{ identity llm.ModelIdentity }

func (s strictSynthesizer) ModelIdentity() (llm.ModelIdentity, bool) { return s.identity, true }
func (strictSynthesizer) SynthesizeWithError(_ context.Context, _ cache.Reader, _ query.Request, response query.Result) (query.Result, error) {
	return response, nil
}

func TestNewExecutorDeduplicatesGenerationOnlyBindingsBeforeFactory(t *testing.T) {
	config := strictConfig(t, 2)
	provider := &strictIdentityProvider{identity: expansionIdentity()}
	synthesizer := expectedSynthesizer()
	var constructions atomic.Int32
	factory := func(_ *cache.Cache, _ queryquality.ChatProvider, _ query.Executor, _ query.Synthesizer, _ queryquality.RetrievalProfile, _ string, _ queryquality.Options, _ query.RuntimeConfigIdentity) (query.Executor, error) {
		constructions.Add(1)
		return strictExecutor{}, nil
	}
	runtime, err := newExecutor(config, cache.New(), provider, strictExecutor{}, synthesizer, factory)
	if err != nil {
		t.Fatal(err)
	}
	if constructions.Load() != 1 || runtime.ServiceCount() != 3 {
		t.Fatalf("constructions=%d services=%d, want one construction and three generation routes", constructions.Load(), runtime.ServiceCount())
	}
}

func TestNewExecutorBuildsDistinctCompositionsOnceEach(t *testing.T) {
	config := strictConfig(t, 2)
	technical := queryquality.RetrievalProfile{ID: "technical-v1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"component"}}}
	technicalDigest, err := technical.Digest()
	if err != nil {
		t.Fatal(err)
	}
	prompt, ok := queryquality.LookupPrompt(queryquality.DomainNeutralTechnicalPromptID)
	if !ok {
		t.Fatal("missing technical prompt")
	}
	config.Profiles = append(config.Profiles, queryconfig.Profile{ID: technical.ID, CriterionPolicy: technical.CriterionPolicy, ProfileDigest: technicalDigest})
	config.ProjectBindings[1].ProfileID = technical.ID
	config.ProjectBindings[1].ProfileDigest = technicalDigest
	config.ProjectBindings[1].PromptID = prompt.ID
	config.ProjectBindings[1].PromptDigest = prompt.TemplateDigest
	config.ConfigDigest = ""
	config, err = queryconfig.Seal(config)
	if err != nil {
		t.Fatal(err)
	}
	var constructions atomic.Int32
	runtime, err := newExecutor(config, cache.New(), &strictIdentityProvider{identity: expansionIdentity()}, strictExecutor{}, expectedSynthesizer(), func(_ *cache.Cache, _ queryquality.ChatProvider, _ query.Executor, _ query.Synthesizer, _ queryquality.RetrievalProfile, _ string, _ queryquality.Options, _ query.RuntimeConfigIdentity) (query.Executor, error) {
		constructions.Add(1)
		return strictExecutor{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if constructions.Load() != 2 || runtime.ServiceCount() != 3 {
		t.Fatalf("constructions=%d services=%d, want two compositions and three generation routes", constructions.Load(), runtime.ServiceCount())
	}
}

func TestNewExecutorRejectsEveryIdentityDimensionBeforeFactory(t *testing.T) {
	base := expansionIdentity()
	tests := []struct {
		name   string
		mutate func(*llm.ModelIdentity)
	}{
		{"provider", func(identity *llm.ModelIdentity) { identity.Provider = "other" }},
		{"model", func(identity *llm.ModelIdentity) { identity.Model = "other-model" }},
		{"reasoning", func(identity *llm.ModelIdentity) { identity.Reasoning = "high" }},
		{"temperature", func(identity *llm.ModelIdentity) { identity.Temperature = 0.5 }},
	}
	for _, test := range tests {
		t.Run("expansion_"+test.name, func(t *testing.T) {
			identity := base
			test.mutate(&identity)
			provider := &strictIdentityProvider{identity: identity}
			var constructions atomic.Int32
			_, err := newExecutor(strictConfig(t, 1), cache.New(), provider, strictExecutor{}, expectedSynthesizer(), func(_ *cache.Cache, _ queryquality.ChatProvider, _ query.Executor, _ query.Synthesizer, _ queryquality.RetrievalProfile, _ string, _ queryquality.Options, _ query.RuntimeConfigIdentity) (query.Executor, error) {
				constructions.Add(1)
				return strictExecutor{}, nil
			})
			if !errors.Is(err, ErrIdentityUnavailable) || constructions.Load() != 0 {
				t.Fatalf("err=%v constructions=%d, want identity error and zero construction", err, constructions.Load())
			}
		})
	}

	synthesisBase := llm.ModelIdentity{Provider: queryconfig.ProviderDeepSeek, Model: "deepseek-v4-pro", Reasoning: "none", Temperature: 0}
	for _, test := range tests {
		t.Run("synthesis_"+test.name, func(t *testing.T) {
			identity := synthesisBase
			test.mutate(&identity)
			var constructions atomic.Int32
			_, err := newExecutor(strictConfig(t, 1), cache.New(), &strictIdentityProvider{identity: base}, strictExecutor{}, strictSynthesizer{identity: identity}, func(_ *cache.Cache, _ queryquality.ChatProvider, _ query.Executor, _ query.Synthesizer, _ queryquality.RetrievalProfile, _ string, _ queryquality.Options, _ query.RuntimeConfigIdentity) (query.Executor, error) {
				constructions.Add(1)
				return strictExecutor{}, nil
			})
			if !errors.Is(err, ErrIdentityUnavailable) || constructions.Load() != 0 {
				t.Fatalf("err=%v constructions=%d, want identity error and zero construction", err, constructions.Load())
			}
		})
	}
}

func TestNewExecutorRejectsMissingIdentitiesBeforeFactory(t *testing.T) {
	var constructions atomic.Int32
	factory := func(_ *cache.Cache, _ queryquality.ChatProvider, _ query.Executor, _ query.Synthesizer, _ queryquality.RetrievalProfile, _ string, _ queryquality.Options, _ query.RuntimeConfigIdentity) (query.Executor, error) {
		constructions.Add(1)
		return strictExecutor{}, nil
	}
	if _, err := newExecutor(strictConfig(t, 1), cache.New(), noIdentityProvider{}, strictExecutor{}, expectedSynthesizer(), factory); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("missing expansion identity err=%v", err)
	}
	if _, err := newExecutor(strictConfig(t, 1), cache.New(), &strictIdentityProvider{identity: expansionIdentity()}, strictExecutor{}, nil, factory); !errors.Is(err, ErrIdentityUnavailable) {
		t.Fatalf("missing synthesis identity err=%v", err)
	}
	if constructions.Load() != 0 {
		t.Fatalf("factory constructions=%d, want zero", constructions.Load())
	}
}

func expansionIdentity() llm.ModelIdentity {
	return llm.ModelIdentity{Provider: queryconfig.ProviderDeepSeek, Model: "deepseek-v4-flash", Reasoning: "none", Temperature: 0}
}

func expectedSynthesizer() *query.Service {
	return query.NewService(cache.New(), nil, llm.NewClientWithOptions("test", llm.ClientOptions{Model: "deepseek-v4-pro", Reasoning: llm.ReasoningNone}))
}

func strictConfig(t *testing.T, bindings int) queryconfig.Config {
	t.Helper()
	profile := queryquality.DefaultRetrievalProfile()
	profileDigest, err := profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	prompt, ok := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	if !ok {
		t.Fatal("missing prompt")
	}
	projectBindings := make([]queryconfig.ProjectBinding, 0, bindings)
	for i := 0; i < bindings; i++ {
		projectBindings = append(projectBindings, queryconfig.ProjectBinding{ProjectID: "project", GenerationID: "generation-" + string(rune('1'+i)), ConceptsDigest: "sha256:" + strings.Repeat(string(rune('a'+i)), 64), ProfileID: profile.ID, ProfileDigest: profileDigest, PromptID: prompt.ID, PromptDigest: prompt.TemplateDigest, Source: queryconfig.SourceCorpusDerivedApproximation})
	}
	sealed, err := queryconfig.Seal(queryconfig.Config{SchemaVersion: queryconfig.SchemaVersion, ConfigRevision: "rev", QueryServiceImplementation: queryconfig.QueryServiceImplementation,
		Stages:   queryconfig.Stages{QueryExpander: queryconfig.QueryExpanderStage{Provider: queryconfig.ProviderDeepSeek, Implementation: queryconfig.QueryExpanderImplementation, Model: "deepseek-v4-flash", Reasoning: "none", Temperature: 0, DefaultProfileID: profile.ID, DefaultProfileDigest: profileDigest, DefaultPromptID: prompt.ID, DefaultPromptDigest: prompt.TemplateDigest, KeywordsPerAttempt: 24, Attempts: 3}, CandidateMatcher: queryconfig.CandidateMatcherStage{Implementation: queryconfig.CandidateMatcherImplementation, EvidenceThreshold: 2, RareKeywordMaxDocumentFrequency: 1}, ResultSelector: queryconfig.ResultSelectorStage{Implementation: queryconfig.ResultSelectorImplementation, Limit: 10, ExplorationSlots: 1, SeedPolicy: queryconfig.SeedPolicy}, AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{Provider: queryconfig.ProviderDeepSeek, Implementation: queryconfig.AnswerSynthesizerImplementation, Model: "deepseek-v4-pro", Reasoning: "none", Temperature: 0, NoEvidencePolicy: queryconfig.NoEvidencePolicy}},
		Profiles: []queryconfig.Profile{{ID: profile.ID, CriterionPolicy: profile.CriterionPolicy, ProfileDigest: profileDigest}}, ProjectBindings: projectBindings})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
