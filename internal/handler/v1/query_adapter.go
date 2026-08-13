package v1

import (
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/query"
)

func mapQueryResult(result query.Result) handler.QueryResponse {
	return handler.QueryResponse{
		Query:     result.Query,
		Mode:      result.Mode,
		Results:   result.Results,
		Expand:    result.Expand,
		AISynth:   result.AISynth,
		Citations: result.Citations,
	}
}
