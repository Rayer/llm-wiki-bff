// Package queryruntime owns the immutable production runtime wiring for sealed
// query stage configurations.
package queryruntime

import (
	"context"
	"errors"

	"github.com/rayer/llm-wiki-bff/internal/cache"
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
}

func NewExecutor(config queryconfig.Config, conceptCache *cache.Cache, expansionProvider queryquality.ChatProvider, legacy query.Executor, synthesizer *query.Service) (*Executor, error) {
	resolver, err := queryconfig.NewResolver(config)
	if err != nil {
		return nil, err
	}
	services := make(map[string]query.Executor)
	compositionServices := make(map[string]query.Executor)
	for _, effective := range resolver.EffectiveConfigs() {
		if _, exists := services[effective.EffectiveConfigDigest]; exists {
			continue
		}
		service, err := queryquality.NewProductionExecutorWithQueryServiceConfig(
			conceptCache, expansionProvider, legacy, synthesizer,
			effective.Profile, effective.PromptID, effective.Options, effective.RuntimeConfigIdentity(),
		)
		if err != nil {
			return nil, err
		}
		services[effective.EffectiveConfigDigest] = service
		compositionServices[serviceKey(effective)] = service
	}
	return &Executor{resolver: resolver, services: services, compositionServices: compositionServices}, nil
}

func New(config queryconfig.Config, conceptCache *cache.Cache, expansionProvider queryquality.ChatProvider, legacy query.Executor, synthesizer *query.Service) (*Executor, error) {
	return NewExecutor(config, conceptCache, expansionProvider, legacy, synthesizer)
}

func (e *Executor) ServiceCount() int {
	if e == nil {
		return 0
	}
	return len(e.services)
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
	config.InputGenerationIdentity = queryconfig.GenerationIdentity{}
	// The production service is independent of the pinned corpus identity;
	// generation identity remains in the effective/runtime identity.
	return config.EffectiveConfigDigestWithoutGeneration()
}
