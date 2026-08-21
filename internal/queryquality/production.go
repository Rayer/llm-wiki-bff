package queryquality

import (
	"context"
	"errors"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/query"
)

type QueryRetrievalServiceConfig struct {
	Cache                      *cache.Cache
	ChatProvider               ChatProvider
	Options                    Options
	RetrievalProfile           RetrievalProfile
	PromptID                   string
	AllowDeterministicFallback bool
	CandidateMatcher           CandidateMatcher
	ResultSelector             ResultSelector
}

func cloneOptions(options Options) Options {
	copy := options
	if options.Seed != nil {
		seed := *options.Seed
		copy.Seed = &seed
	}
	return copy
}

// NewQueryRetrievalService is the single production composition point for retrieval.
// CandidateMatcher and ResultSelector are test seams; nil selects production implementations.
func NewQueryRetrievalService(config QueryRetrievalServiceConfig) (*QueryRetrievalPipeline, error) {
	if config.Cache == nil {
		return nil, errors.New("query-retrieval cache is nil")
	}
	options, err := NormalizeOptions(cloneOptions(config.Options))
	if err != nil {
		return nil, err
	}
	profile, err := config.RetrievalProfile.ValidatedCopy()
	if err != nil {
		return nil, err
	}
	prompt, ok := LookupPrompt(config.PromptID)
	if !ok {
		return nil, errors.New("query-retrieval prompt is not built in")
	}
	expander, err := NewParallelMinimalStructuredPlanExpanderWithPrompt(config.ChatProvider, NewDeterministicExpander(), options, prompt.ID)
	if err != nil {
		return nil, err
	}
	matcher := config.CandidateMatcher
	if matcher == nil {
		matcher = NewLexicalMatcher(nil)
	}
	selector := config.ResultSelector
	if selector == nil {
		selector = NewResultSelector()
	}
	pipeline, err := newQueryRetrievalPipelineWithNormalizedOptions(config.Cache, expander, matcher, selector, options.SeedFor, options, profile)
	if err != nil {
		return nil, err
	}
	pipeline.allowDeterministicFallback = config.AllowDeterministicFallback
	pipeline.prompt = prompt
	return pipeline, nil
}

type ProductionExecutor struct {
	queryRetrievalPipeline *QueryRetrievalPipeline
	legacy                 query.Executor
	synthesizer            *query.Service
}

func NewProductionExecutor(conceptCache *cache.Cache, provider ChatProvider, legacy query.Executor, synthesizer *query.Service, options Options) (query.Executor, error) {
	if legacy == nil {
		return nil, errors.New("legacy query executor is nil")
	}
	queryRetrievalPipeline, err := NewQueryRetrievalService(QueryRetrievalServiceConfig{
		Cache: conceptCache, ChatProvider: provider, Options: options, RetrievalProfile: DefaultRetrievalProfile(),
		PromptID: StructuredPlanPromptID, AllowDeterministicFallback: true,
	})
	if err != nil {
		return nil, err
	}
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
