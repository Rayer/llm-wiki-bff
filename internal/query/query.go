// Package query contains the transport-independent wiki query application service.
package query

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// ErrCacheNotConfigured reports that the query service has no concept cache.
var ErrCacheNotConfigured = errors.New("concept cache is not configured")

// Request is the application input for one query.
type Request struct {
	Query string
	Mode  string
}

// Result is the application result for one query. Its fields are domain
// values; HTTP adapters are responsible for choosing a wire representation.
type Result struct {
	Query     string
	Mode      string
	Results   []search.Result
	Expand    *llm.ExpandResult
	AISynth   string
	Citations []search.Citation
}

// Executor is the narrow seam used by transport adapters.
type Executor interface {
	Execute(context.Context, cache.Reader, Request) (Result, error)
}

// Service runs query expansion, cache search, and optional citation synthesis.
type Service struct {
	cache    *cache.Cache
	expander *llm.QueryExpander
	llm      *llm.Client
}

// NewService creates a query application service.
func NewService(conceptCache *cache.Cache, expander *llm.QueryExpander, llmClient *llm.Client) *Service {
	return &Service{cache: conceptCache, expander: expander, llm: llmClient}
}

// Execute runs the production query pipeline.
func (s *Service) Execute(ctx context.Context, reader cache.Reader, request Request) (Result, error) {
	searchQuery := request.Query
	var expandResult *llm.ExpandResult

	if s.expander != nil {
		if result, err := s.expander.Expand(ctx, request.Query); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Result{}, err
			}
			log.Printf("[expander] query expansion failed: %v — falling back to raw query", err)
		} else if result != nil {
			expandResult = result
			searchQuery = strings.Join(result.Keywords, " ")
		}
	}

	if s.cache == nil {
		return Result{}, ErrCacheNotConfigured
	}

	results, err := s.cache.Search(ctx, reader, searchQuery, 10)
	if err != nil {
		return Result{}, err
	}
	log.Printf("Search query completed: terms=%d results=%d", len(strings.Fields(searchQuery)), len(results))

	response := Result{
		Query:   request.Query,
		Mode:    request.Mode,
		Results: results,
		Expand:  expandResult,
	}
	result, err := s.SynthesizeWithError(ctx, reader, request, response)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) Synthesize(ctx context.Context, reader cache.Reader, request Request, response Result) Result {
	result, err := s.SynthesizeWithError(ctx, reader, request, response)
	if err != nil {
		return response
	}
	return result
}

func (s *Service) SynthesizeWithError(ctx context.Context, reader cache.Reader, request Request, response Result) (Result, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return response, ctxErr
	}
	if s.llm == nil || len(response.Results) == 0 {
		return response, nil
	}

	authority, err := search.NewCitationAuthority(response.Results)
	if err != nil {
		log.Printf("citation capability issuance failed: %v", err)
		return response, nil
	}
	contexts, err := s.buildContexts(ctx, reader, response.Results[:min(10, len(response.Results))], authority)
	if err != nil {
		return response, err
	}
	if len(contexts) == 0 {
		return response, nil
	}

	answer, err := s.llm.Chat(ctx, buildSystemPrompt(request.Mode), buildUserPrompt(request.Query, contexts))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return response, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return response, err
		}
		log.Printf("LLM synthesis failed: %v", err)
		return response, nil
	}

	answer, citations, filtered := authority.Resolve(answer)
	response.AISynth = answer
	response.Citations = citations
	response.Results = filtered
	return response, nil
}

func (s *Service) buildContexts(ctx context.Context, reader cache.Reader, results []search.Result, authority *search.CitationAuthority) ([]string, error) {
	contexts := make([]string, 0, len(results))
	for rank, result := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, ok := s.cache.Entry(reader, result.Slug)
		if !ok {
			if _, err := s.cache.Build(ctx, reader); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
			} else {
				entry, ok = s.cache.Entry(reader, result.Slug)
			}
		}
		if !ok {
			continue
		}
		sourceContext := "Sources: none listed"
		if len(entry.Sources) > 0 {
			sourceContext = "Sources: [" + strings.Join(entry.Sources, ", ") + "]"
		}
		contexts = append(contexts, authority.AddContext(rank, result, sourceContext+"\n\n"+entry.Body))
	}
	return contexts, nil
}

func buildSystemPrompt(mode string) string {
	base := "CRITICAL: If the user asks about a specific location (city, district, area), ONLY include results relevant to that location. Ignore results from other locations even if they match on topic keywords." +
		"\n\nCITATION FORMAT RULES (mandatory):" +
		"\n- Each wiki block includes one server-issued internal reference in brackets. Use that exact reference in brackets when citing the block; the server will replace it with the canonical title." +
		"\n- Never invent, alter, or reuse a citation reference for a different wiki block" +
		"\n- EVERY factual claim from wiki content MUST have a bracketed citation: [Exact Source Name]" +
		"\n- Use the EXACT full title from the wiki content inside brackets" +
		"\n- Never use **bold** instead of brackets" +
		"\n- Never append source names as plain text without brackets" +
		"\n- Correct example: 「...適合親子放電。[中和員山公園遊逸之丘]」" +
		"\n- Wrong example: 「...適合親子放電。中和員山公園遊逸之丘」" +
		"\n- Each paragraph referencing a source MUST end with its bracketed citation. "
	if mode == "full" {
		return "You are a knowledgeable assistant with access to a personal wiki. Treat the wiki as supplementary reference material — NOT as a constraint." +
			"\n- If the wiki content is RELEVANT to the user's question (same location, topic, or category), use it and cite with [Source Name]." +
			"\n- If the wiki content is NOT relevant (wrong city, different topic, etc.), IGNORE it completely and answer from your own knowledge — exactly as if you were asked this question directly with no wiki." +
			"\n- NEVER say 'I cannot find this in the wiki' or apologize for missing information. Just answer the question." +
			"\n- When mixing wiki and general knowledge, make it seamless — don't call out which is which in the text." +
			"\n\nCITATION FORMAT RULES (mandatory):" +
			"\n- Each wiki block includes one server-issued internal reference in brackets. Use that exact reference in brackets when citing the block; the server will replace it with the canonical title." +
			"\n- Never invent, alter, or reuse a citation reference for a different wiki block" +
			"\n- EVERY factual claim from wiki content MUST have a bracketed citation: [Exact Source Name]" +
			"\n- Use the EXACT full title from the wiki content inside brackets" +
			"\n- Never use **bold** instead of brackets" +
			"\n- Correct example: 「...適合親子放電。[中和員山公園遊逸之丘]」" +
			"\n- Wrong example: 「...適合親子放電。中和員山公園遊逸之丘」"
	}
	return base + "You are a wiki Q&A assistant. Answer ONLY using the wiki content provided below. Do not use external knowledge. Cite every claim using [Source Name]."
}

func buildUserPrompt(query string, contexts []string) string {
	var builder strings.Builder
	builder.WriteString("User question: ")
	builder.WriteString(search.NeutralizeCitationReferences(query))
	builder.WriteString("\n\nWiki content:\n")
	for _, ctx := range contexts {
		builder.WriteString("\n---\n")
		builder.WriteString(ctx)
	}
	return builder.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
