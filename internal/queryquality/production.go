package queryquality

import (
	"context"
	"errors"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/query"
)

type ProductionExecutor struct {
	retriever   *Service
	legacy      query.Executor
	synthesizer *query.Service
}

func NewProductionExecutor(conceptCache *cache.Cache, provider ChatProvider, legacy query.Executor, synthesizer *query.Service, options Options) (query.Executor, error) {
	if conceptCache == nil {
		return nil, errors.New("three-host cache is nil")
	}
	if legacy == nil {
		return nil, errors.New("legacy query executor is nil")
	}
	options, err := NormalizeOptions(options)
	if err != nil {
		return nil, err
	}
	retriever := NewServiceWithOptions(NewMinimalStructuredPlanExpander(provider, nil), NewLexicalMatcher(nil), NewSelector(), options.SeedFor, options)
	retriever.cache = conceptCache
	return &ProductionExecutor{retriever: retriever, legacy: legacy, synthesizer: synthesizer}, nil
}

func (e *ProductionExecutor) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	result, err := e.retriever.Execute(ctx, reader, request)
	if err != nil {
		var expansionErr *ExpansionError
		if errors.As(err, &expansionErr) && ctx.Err() == nil {
			return e.legacy.Execute(ctx, reader, request)
		}
		return query.Result{}, err
	}
	if e.synthesizer != nil {
		var err error
		result, err = e.synthesizer.SynthesizeWithError(ctx, reader, request, result)
		if err != nil {
			return query.Result{}, err
		}
	}
	return result, nil
}
