// Package queryruntime owns the immutable production runtime wiring for sealed
// query stage configurations.
package queryruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryconfig"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)

var ErrIdentityProviderRequired = errors.New("query runtime identity provider required")
var ErrIdentityUnavailable = errors.New("query runtime identity unavailable")

type Executor struct {
	resolver            *queryconfig.Resolver
	services            map[string]query.Executor
	compositionServices map[string]query.Executor
	readback            query.RuntimeConfigReadback
}

func NewExecutor(config queryconfig.Config, conceptCache *cache.Cache, expansionProvider queryquality.ChatProvider, legacy query.Executor, synthesizer query.Synthesizer) (*Executor, error) {
	return newExecutor(config, conceptCache, expansionProvider, legacy, synthesizer, queryquality.NewStrictProductionExecutorWithQueryServiceConfig)
}

type productionExecutorFactory func(*cache.Cache, queryquality.ChatProvider, query.Executor, query.Synthesizer, queryquality.RetrievalProfile, string, queryquality.Options, query.RuntimeConfigIdentity) (query.Executor, error)

func newExecutor(config queryconfig.Config, conceptCache *cache.Cache, expansionProvider queryquality.ChatProvider, legacy query.Executor, synthesizer query.Synthesizer, factory productionExecutorFactory) (*Executor, error) {
	resolver, err := queryconfig.NewResolver(config)
	if err != nil {
		return nil, err
	}
	effectiveConfigs := resolver.EffectiveConfigs()
	for _, effective := range effectiveConfigs {
		if err := validateIdentities(effective, expansionProvider, synthesizer); err != nil {
			return nil, err
		}
	}
	services := make(map[string]query.Executor)
	compositionServices := make(map[string]query.Executor)
	for _, effective := range effectiveConfigs {
		composition := serviceKey(effective)
		service, exists := compositionServices[composition]
		if !exists {
			service, err = factory(
				conceptCache, expansionProvider, legacy, synthesizer,
				effective.Profile, effective.PromptID, effective.Options, effective.RuntimeConfigIdentity(),
			)
			if err != nil {
				return nil, err
			}
			compositionServices[composition] = service
		}
		services[effective.EffectiveConfigDigest] = service
	}
	defaultConfig := effectiveConfigs[0]
	return &Executor{
		resolver: resolver, services: services, compositionServices: compositionServices,
		readback: query.RuntimeConfigReadback{
			SchemaVersion: defaultConfig.SchemaVersion, ConfigRevision: defaultConfig.ConfigRevision, ConfigDigest: defaultConfig.ConfigDigest,
			QueryServiceImplementation: defaultConfig.QueryServiceImplementation,
			DefaultProfileID:           defaultConfig.Profile.ID, DefaultProfileDigest: defaultConfig.ProfileDigest,
			DefaultPromptID: defaultConfig.PromptID, DefaultPromptDigest: defaultConfig.PromptDigest,
			ExpansionProvider: defaultConfig.ExpansionProvider, ExpansionModel: defaultConfig.ExpansionModel,
			ExpansionReasoning: defaultConfig.ExpansionReasoning, ExpansionTemperature: defaultConfig.ExpansionTemperature,
			SynthesisProvider: defaultConfig.SynthesisProvider, SynthesisModel: defaultConfig.SynthesisModel,
			SynthesisReasoning: defaultConfig.SynthesisReasoning, SynthesisTemperature: defaultConfig.SynthesisTemperature,
			NoEvidencePolicy: defaultConfig.NoEvidencePolicy,
			Options: query.RuntimeQueryOptions{
				SelectionLimit: defaultConfig.Options.SelectionLimit, ExplorationSlots: defaultConfig.Options.ExplorationSlots,
				EvidenceThreshold: defaultConfig.Options.EvidenceThreshold, KeywordsPerAttempt: defaultConfig.Options.KeywordsPerAttempt,
				ExpansionAttempts: defaultConfig.Options.ExpansionAttempts, RareDocumentFrequency: defaultConfig.Options.RareDocumentFrequency,
			},
			BindingCount: len(config.ProjectBindings), DistinctServiceCompositionCount: len(compositionServices),
		},
	}, nil
}

func validateIdentities(effective queryconfig.EffectiveConfig, expansionProvider queryquality.ChatProvider, synthesizer query.Synthesizer) error {
	provider, ok := expansionProvider.(llm.ModelIdentityProvider)
	if !ok || provider == nil {
		return fmt.Errorf("%w: expansion provider identity unavailable", ErrIdentityUnavailable)
	}
	expansion, ok := provider.ModelIdentity()
	if !ok {
		return fmt.Errorf("%w: expansion provider identity unavailable", ErrIdentityUnavailable)
	}
	if expansion.Provider != effective.ExpansionProvider || expansion.Model != effective.ExpansionModel || expansion.Reasoning != effective.ExpansionReasoning || expansion.Temperature != effective.ExpansionTemperature {
		return fmt.Errorf("%w: expansion provider identity mismatch", ErrIdentityUnavailable)
	}
	if synthesizer == nil {
		return fmt.Errorf("%w: synthesizer identity unavailable", ErrIdentityUnavailable)
	}
	synthesis, ok := synthesizer.ModelIdentity()
	if !ok {
		return fmt.Errorf("%w: synthesizer identity unavailable", ErrIdentityUnavailable)
	}
	if synthesis.Provider != effective.SynthesisProvider || synthesis.Model != effective.SynthesisModel || synthesis.Reasoning != effective.SynthesisReasoning || synthesis.Temperature != effective.SynthesisTemperature {
		return fmt.Errorf("%w: synthesizer identity mismatch", ErrIdentityUnavailable)
	}
	return nil
}

func New(config queryconfig.Config, conceptCache *cache.Cache, expansionProvider queryquality.ChatProvider, legacy query.Executor, synthesizer query.Synthesizer) (*Executor, error) {
	return NewExecutor(config, conceptCache, expansionProvider, legacy, synthesizer)
}

func (e *Executor) ServiceCount() int {
	if e == nil {
		return 0
	}
	return len(e.services)
}

func (e *Executor) Readback() query.RuntimeConfigReadback {
	if e == nil {
		return query.RuntimeConfigReadback{}
	}
	return e.readback
}

func (e *Executor) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	if e == nil || e.resolver == nil {
		return query.Result{}, ErrIdentityUnavailable
	}
	identityReader, ok := reader.(storage.QueryGenerationIdentityProvider)
	if !ok || identityReader == nil {
		return query.Result{}, ErrIdentityProviderRequired
	}
	storageIdentity, err := identityReader.QueryGenerationIdentity(ctx)
	if err != nil {
		return query.Result{}, ErrIdentityUnavailable
	}
	identity := queryconfig.GenerationIdentity{ProjectID: storageIdentity.ProjectID, GenerationID: storageIdentity.GenerationID, ConceptsDigest: storageIdentity.ConceptsDigest}
	effective, err := e.resolver.Resolve(identity)
	if err != nil {
		return query.Result{}, err
	}
	service, ok := e.services[effective.EffectiveConfigDigest]
	if !ok {
		service, ok = e.compositionServices[serviceKey(effective)]
	}
	if !ok {
		return query.Result{}, ErrIdentityUnavailable
	}
	result, err := service.Execute(query.WithRuntimeConfigIdentity(ctx, effective.RuntimeConfigIdentity()), reader, request)
	if err != nil {
		return query.Result{}, err
	}
	runtimeIdentity := effective.RuntimeConfigIdentity()
	result.RuntimeConfigIdentity = query.CloneRuntimeConfigIdentity(&runtimeIdentity)
	return result, nil
}

func serviceKey(config queryconfig.EffectiveConfig) string {
	// The production service is independent of pinned corpus and binding provenance.
	return config.EffectiveConfigDigestWithoutGeneration()
}
