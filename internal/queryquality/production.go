package queryquality

import (
	"context"
	"errors"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/query"
)

type ProductionExecutor struct {
	queryRetrievalPipeline *QueryRetrievalPipeline
	legacy                 query.Executor
	synthesizer            *query.Service
}

func NewProductionExecutor(conceptCache *cache.Cache, provider ChatProvider, legacy query.Executor, synthesizer *query.Service, options Options) (query.Executor, error) {
	if conceptCache == nil {
		return nil, errors.New("query-retrieval cache is nil")
	}
	if legacy == nil {
		return nil, errors.New("legacy query executor is nil")
	}
	options, err := NormalizeOptions(options)
	if err != nil {
		return nil, err
	}
	expander, err := NewParallelMinimalStructuredPlanExpander(provider, NewDeterministicExpander(), options)
	if err != nil {
		return nil, err
	}
	queryRetrievalPipeline := NewQueryRetrievalPipelineWithOptions(expander, NewLexicalMatcher(nil), NewResultSelector(), options.SeedFor, options)
	queryRetrievalPipeline.cache = conceptCache
	queryRetrievalPipeline.allowDeterministicFallback = true
	return &ProductionExecutor{queryRetrievalPipeline: queryRetrievalPipeline, legacy: legacy, synthesizer: synthesizer}, nil
}

func (e *ProductionExecutor) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	receiptCtx, receipt := query.WithReceipt(ctx)
	defer query.FinishReceipt(receipt)
	result, err := e.queryRetrievalPipeline.Execute(receiptCtx, reader, request)
	if err != nil {
		var expansionErr *ExpansionError
		if errors.As(err, &expansionErr) && ctx.Err() == nil {
			return e.legacy.Execute(receiptCtx, reader, request)
		}
		return query.Result{}, err
	}
	if e.synthesizer != nil {
		var err error
		result, err = e.synthesizer.SynthesizeWithError(receiptCtx, reader, request, result)
		if err != nil {
			return query.Result{}, err
		}
	}
	return result, nil
}
