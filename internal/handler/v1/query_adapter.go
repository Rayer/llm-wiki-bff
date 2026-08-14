package v1

import (
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func mapQueryResult(result query.Result) handler.QueryResponse {
	results := make([]search.Result, len(result.Results))
	for i, item := range result.Results {
		item.Snippet = ""
		results[i] = item
	}
	return handler.QueryResponse{
		Query:     result.Query,
		Mode:      result.Mode,
		Results:   results,
		Expand:    result.Expand,
		AISynth:   result.AISynth,
		Citations: result.Citations,
	}
}
