package queryruntime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/queryruntime"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)

type runtimeReader struct {
	mu       sync.Mutex
	identity storage.QueryGenerationIdentity
	reads    int
}

func (r *runtimeReader) Prefix() string { return "users/test/projects/" + r.identity.ProjectID }
func (r *runtimeReader) ReadFile(context.Context, string) ([]byte, error) {
	r.mu.Lock()
	r.reads++
	r.mu.Unlock()
	return []byte(`{"slug":"deploy","title":"Deploy","body":"deploy docs"}` + "\n"), nil
}
func (r *runtimeReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) { return nil, nil }
func (r *runtimeReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("unexpected page read")
}
func (r *runtimeReader) QueryGenerationIdentity(context.Context) (storage.QueryGenerationIdentity, error) {
	return r.identity, nil
}

type countingProvider struct {
	mu           sync.Mutex
	calls        int
	system, user string
}

func (p *countingProvider) Chat(_ context.Context, system, user string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.system, p.user = system, user
	return `{"raw_query":"deploy docs","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"deploy","terms":["deploy"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`, nil
}

type countingLegacy struct {
	mu    sync.Mutex
	calls int
}

func (e *countingLegacy) Execute(_ context.Context, _ cache.Reader, _ query.Request) (query.Result, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return query.Result{Status: "legacy"}, nil
}

func TestRuntimePrebuildsAndRoutesExactAndDefault(t *testing.T) {
	config := runtimeConfig(t)
	provider := &countingProvider{}
	legacy := &countingLegacy{}
	runtime, err := queryruntime.NewExecutor(config, cache.New(), provider, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ServiceCount() != 2 {
		t.Fatalf("service count=%d, want 2", runtime.ServiceCount())
	}
	exactReader := &runtimeReader{identity: storage.QueryGenerationIdentity{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)}}
	result, err := runtime.Execute(context.Background(), exactReader, query.Request{Query: "deploy docs", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RuntimeConfigIdentity == nil || !result.RuntimeConfigIdentity.ExactBinding || result.RuntimeConfigIdentity.ProfileID != "technical-v1" {
		t.Fatalf("identity=%+v", result.RuntimeConfigIdentity)
	}
	if provider.calls != 3 || legacy.calls != 0 {
		t.Fatalf("provider=%d legacy=%d", provider.calls, legacy.calls)
	}
	defaultReader := &runtimeReader{identity: storage.QueryGenerationIdentity{ProjectID: "unlisted", GenerationID: "legacy"}}
	result, err = runtime.Execute(context.Background(), defaultReader, query.Request{Query: "deploy docs", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RuntimeConfigIdentity == nil || result.RuntimeConfigIdentity.ExactBinding || result.RuntimeConfigIdentity.BindingSource != queryconfig.SourceLegacyCompatibility {
		t.Fatalf("default identity=%+v", result.RuntimeConfigIdentity)
	}
	if result.RuntimeConfigIdentity.GenerationID != "legacy" {
		t.Fatalf("default generation=%q", result.RuntimeConfigIdentity.GenerationID)
	}
}

func TestRuntimeMismatchAndUnknownReaderHaveZeroPipelineEffects(t *testing.T) {
	runtime, err := queryruntime.NewExecutor(runtimeConfig(t), cache.New(), &countingProvider{}, &countingLegacy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &runtimeReader{identity: storage.QueryGenerationIdentity{ProjectID: "project-a", GenerationID: "wrong-generation", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)}}
	if _, err := runtime.Execute(context.Background(), reader, query.Request{Query: "private-query"}); !errors.Is(err, queryconfig.ErrBindingMismatch) {
		t.Fatalf("err=%v", err)
	}
	reader.mu.Lock()
	reads := reader.reads
	reader.mu.Unlock()
	if reads != 0 {
		t.Fatalf("cache reads=%d, want 0", reads)
	}
	if _, err := runtime.Execute(context.Background(), &unknownReader{}, query.Request{Query: "private-query"}); !errors.Is(err, queryruntime.ErrIdentityProviderRequired) {
		t.Fatalf("unknown reader err=%v", err)
	}
}

type unknownReader struct{}

func (*unknownReader) Prefix() string                                             { return "unknown" }
func (*unknownReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) { return nil, nil }
func (*unknownReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("unexpected")
}

func TestRuntimeTechnicalPromptMatchesSharedProductionRenderer(t *testing.T) {
	config := runtimeConfig(t)
	provider := &countingProvider{}
	runtime, err := queryruntime.NewExecutor(config, cache.New(), provider, &countingLegacy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &runtimeReader{identity: storage.QueryGenerationIdentity{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64)}}
	if _, err := runtime.Execute(context.Background(), reader, query.Request{Query: "deploy docs", Mode: "wiki"}); err != nil {
		t.Fatal(err)
	}
	profile := queryquality.RetrievalProfile{ID: "technical-v1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"component"}, PreferredByDefault: []string{"technology"}, GoalsToExpand: []string{"explanation"}}}
	want, err := queryquality.RenderPrompt(queryquality.DomainNeutralTechnicalPromptID, "deploy docs", profile.CriterionPolicy, 24)
	if err != nil {
		t.Fatal(err)
	}
	if provider.system != want.System || provider.user != want.User {
		t.Fatalf("prompt mismatch system=%q user=%q", provider.system, provider.user)
	}
}

func runtimeConfig(t *testing.T) queryconfig.Config {
	t.Helper()
	base := queryquality.DefaultRetrievalProfile()
	defaultDigest, _ := base.Digest()
	technical := queryquality.RetrievalProfile{ID: "technical-v1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"component"}, PreferredByDefault: []string{"technology"}, GoalsToExpand: []string{"explanation"}}}
	technicalDigest, _ := technical.Digest()
	lifestylePrompt, _ := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	technicalPrompt, _ := queryquality.LookupPrompt(queryquality.DomainNeutralTechnicalPromptID)
	sealed, err := queryconfig.Seal(queryconfig.Config{SchemaVersion: 1, ConfigRevision: "rev", QueryServiceImplementation: queryconfig.QueryServiceImplementation,
		Stages:   queryconfig.Stages{QueryExpander: queryconfig.QueryExpanderStage{Implementation: queryconfig.QueryExpanderImplementation, Model: "deepseek-v4-flash", Reasoning: "none", Temperature: 0, DefaultProfileID: base.ID, DefaultProfileDigest: defaultDigest, DefaultPromptID: lifestylePrompt.ID, DefaultPromptDigest: lifestylePrompt.TemplateDigest, KeywordsPerAttempt: 24, Attempts: 3}, CandidateMatcher: queryconfig.CandidateMatcherStage{Implementation: queryconfig.CandidateMatcherImplementation, EvidenceThreshold: 2, RareKeywordMaxDocumentFrequency: 1}, ResultSelector: queryconfig.ResultSelectorStage{Implementation: queryconfig.ResultSelectorImplementation, Limit: 10, ExplorationSlots: 1, SeedPolicy: queryconfig.SeedPolicy}, AnswerSynthesizer: queryconfig.AnswerSynthesizerStage{Implementation: queryconfig.AnswerSynthesizerImplementation, Model: "deepseek-v4-pro", Reasoning: "none", NoEvidencePolicy: queryconfig.NoEvidencePolicy}},
		Profiles: []queryconfig.Profile{{ID: base.ID, CriterionPolicy: base.CriterionPolicy, ProfileDigest: defaultDigest}, {ID: technical.ID, CriterionPolicy: technical.CriterionPolicy, ProfileDigest: technicalDigest}}, ProjectBindings: []queryconfig.ProjectBinding{{ProjectID: "project-a", GenerationID: "generation-7", ConceptsDigest: "sha256:" + strings.Repeat("a", 64), ProfileID: technical.ID, ProfileDigest: technicalDigest, PromptID: technicalPrompt.ID, PromptDigest: technicalPrompt.TemplateDigest, Source: queryconfig.SourceCorpusDerivedApproximation}}})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
