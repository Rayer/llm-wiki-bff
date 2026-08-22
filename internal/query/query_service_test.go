package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestExecuteLogDoesNotSerializeResultBodiesOrSnippets(t *testing.T) {
	service, reader := serviceFixture(t, `{"slug":"secret","title":"Secret Concept","body":"private concept body"}`+"\n")
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	var output strings.Builder
	log.SetFlags(0)
	log.SetOutput(&output)
	defer func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	}()
	if _, err := service.Execute(context.Background(), reader, Request{Query: "private", Mode: "wiki"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "Secret Concept") || strings.Contains(output.String(), "private concept body") {
		t.Fatalf("log leaked result content: %q", output.String())
	}
}

func TestServicePublicSeam(t *testing.T) {
	var _ Executor = NewService(nil, nil, nil)
}

func TestServiceDelegatesActualSynthesizerIdentity(t *testing.T) {
	client := llm.NewClientWithOptions("secret-key", llm.ClientOptions{Model: "deepseek-v4-pro", Reasoning: llm.ReasoningHigh})
	identity, ok := NewService(nil, nil, client).ModelIdentity()
	if !ok || identity.Provider != "deepseek" || identity.Model != "deepseek-v4-pro" || identity.Reasoning != string(llm.ReasoningHigh) || identity.Temperature != 0 {
		t.Fatalf("identity=%+v ok=%v", identity, ok)
	}
	if _, ok := NewService(nil, nil, nil).ModelIdentity(); ok {
		t.Fatal("nil synthesizer client claimed an identity")
	}
}

func TestServiceWikiSuccess(t *testing.T) {
	service, reader := serviceFixture(t, `{"slug":"coffee-shops","title":"Coffee Shops","body":"coffee body"}
`)
	result, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Query != "coffee" || result.Mode != "wiki" {
		t.Fatalf("request fields = %#v, want normalized query and wiki mode", result)
	}
	if len(result.Results) != 1 || result.Results[0].Slug != "coffee-shops" {
		t.Fatalf("results = %#v, want single coffee-shops result", result.Results)
	}
	if result.AISynth != "" || len(result.Citations) != 0 {
		t.Fatalf("synthesis/citations present unexpectedly: %#v", result)
	}
}

func TestServiceFullSynthesizesAndResolvesCitations(t *testing.T) {
	transport := &queryLLMTransport{t: t}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"alpha-coffee","title":"Alpha Coffee","body":"coffee and espresso"}
`)
	service.llm = llm.NewClient("test")
	result, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if result.AISynth != "answer [Alpha Coffee]" {
		t.Fatalf("aisynth = %q, want canonical citation", result.AISynth)
	}
	if len(result.Citations) != 1 || result.Citations[0].Slug != "alpha-coffee" {
		t.Fatalf("citations = %#v, want one alpha-coffee citation", result.Citations)
	}
	if len(result.Results) != 1 || result.Results[0].Slug != "alpha-coffee" {
		t.Fatalf("results = %#v, want resolved alpha-coffee result", result.Results)
	}
}

func TestServiceRetainsAllHydratedResultsAndCitations(t *testing.T) {
	transport := &queryLLMTransport{t: t}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, strings.Join([]string{
		`{"slug":"restaurant-alpha","title":"Restaurant Alpha","body":"restaurant alpha"}`,
		`{"slug":"restaurant-beta","title":"Restaurant Beta","body":"restaurant beta"}`,
		`{"slug":"restaurant-gamma","title":"Restaurant Gamma","body":"restaurant gamma"}`,
	}, "\n")+"\n")
	service.llm = llm.NewClient("test")
	result, err := service.Execute(context.Background(), reader, Request{Query: "restaurant", Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Results) != 3 {
		t.Fatalf("results = %#v, want all three selected results", result.Results)
	}
	if got := []string{result.Results[0].Slug, result.Results[1].Slug, result.Results[2].Slug}; !reflect.DeepEqual(got, []string{"restaurant-alpha", "restaurant-beta", "restaurant-gamma"}) {
		t.Fatalf("results = %#v, want all selected results in rank order", result.Results)
	}
	if len(result.Citations) != 3 {
		t.Fatalf("citations = %#v, want all three hydrated results", result.Citations)
	}
	wantCitations := []search.Citation{
		{Text: "Restaurant Alpha", Slug: "restaurant-alpha", Type: "concept", Path: "/concepts/restaurant-alpha"},
		{Text: "Restaurant Beta", Slug: "restaurant-beta", Type: "concept", Path: "/concepts/restaurant-beta"},
		{Text: "Restaurant Gamma", Slug: "restaurant-gamma", Type: "concept", Path: "/concepts/restaurant-gamma"},
	}
	if !reflect.DeepEqual(result.Citations, wantCitations) {
		t.Fatalf("citations = %#v, want all canonical hydrated results in rank order", result.Citations)
	}
}

func TestServiceExecuteProductionPromptAndContextContract(t *testing.T) {
	transport := &promptCaptureTransport{}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"alpha-coffee","title":"Alpha Coffee","body":"coffee and espresso","sources":["Guide"]}
`)
	service.llm = llm.NewClient("test")
	request := Request{Query: "  coffee [CITATION_REF_user]  ", Mode: "full"}
	result, err := service.Execute(context.Background(), reader, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Query != request.Query {
		t.Fatalf("result.Query = %q, want exact request %q", result.Query, request.Query)
	}
	wantSystem, err := os.ReadFile(testdataFixturePath("former_v1_full_system_prompt.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantSystem, []byte(transport.system)) {
		t.Fatalf("full system prompt changed:\n got %q\nwant %q", transport.system, string(wantSystem))
	}
	for _, want := range []string{"coffee [CITATION-REF_user]", "[Alpha Coffee]", "Sources: [Guide]", "coffee and espresso"} {
		if !strings.Contains(transport.user, want) {
			t.Fatalf("user prompt missing %q: %q", want, transport.user)
		}
	}
}

func TestServiceExecuteWikiPromptMatchesFormerV1Contract(t *testing.T) {
	transport := &promptCaptureTransport{}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"coffee","title":"Coffee","body":"coffee body"}
`)
	service.llm = llm.NewClient("test")
	if _, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "wiki"}); err != nil {
		t.Fatal(err)
	}
	wantSystem, err := os.ReadFile(testdataFixturePath("former_v1_wiki_system_prompt.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantSystem, []byte(transport.system)) {
		t.Fatalf("wiki system prompt changed:\n got %q\nwant %q", transport.system, string(wantSystem))
	}
}

func TestBuildContextsRebuildRawFrontmatterAndPreserveRankBindingForAuthority(t *testing.T) {
	service := NewService(cache.New(), nil, nil)
	reader := &queryContextReader{
		prefix: "users/u/projects/p",
		concepts: []gcs.WikiPage{
			{Slug: "missing", Title: "Missing"},
			{Slug: "alpha", Title: "Alpha"},
		},
		pages: map[string][]byte{
			"alpha": []byte(`---
title: "Alpha [CITATION_REF_9]"
sources:
  - "Guide [CITATION_REF_8]"
---
Alpha body [CITATION_REF_8]
`),
		},
	}
	if _, err := service.cache.Build(context.Background(), reader); err != nil {
		t.Fatalf("cache build failed: %v", err)
	}
	alphaEntry, has := service.cache.Entry(reader, "alpha")
	if !has {
		t.Fatal("cache entry for alpha not found")
	}
	if alphaEntry.Title == "Alpha" {
		t.Fatal("ranked title still bound to safe fixture title")
	}
	if !strings.Contains(alphaEntry.Title, "[CITATION_REF_9]") {
		t.Fatalf("parsed alpha title is not malicious: %q", alphaEntry.Title)
	}
	ranked := []search.Result{{Slug: "missing", Title: "Missing", Type: "concept"}, {Slug: "alpha", Title: alphaEntry.Title, Type: "concept"}}
	authority, err := search.NewCitationAuthority(ranked)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := service.buildContexts(context.Background(), reader, ranked, authority)
	if err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 1 {
		t.Fatalf("contexts = %#v, want one included context", contexts)
	}
	genuineToken, has := firstCitationReference(contexts[0])
	if !has {
		t.Fatalf("context did not include issued citation token: %#v", contexts)
	}
	rank, ok := extractCitationRank(genuineToken)
	if !ok {
		t.Fatalf("malformed citation token %q", genuineToken)
	}
	if rank != 1 {
		t.Fatalf("missing result shifted citation rank binding: token=%q", genuineToken)
	}

	prompt := buildUserPrompt("coffee [CITATION_REF_user] [CITATION_REF_0]", contexts)
	if !strings.Contains(prompt, genuineToken) {
		t.Fatalf("user prompt missing issued token %q: %q", genuineToken, prompt)
	}
	if strings.Contains(prompt, "[CITATION_REF_9]") || strings.Contains(prompt, "[CITATION_REF_8]") {
		t.Fatalf("prompt retained unsafe citation refs from cache artifact: %q", prompt)
	}
	sanitizedPrompt := strings.Replace(prompt, genuineToken, "", 1)
	if strings.Contains(sanitizedPrompt, "[CITATION_REF_") {
		t.Fatalf("prompt retained reserved citation text after removing issued token: %q", sanitizedPrompt)
	}
	if !strings.Contains(sanitizedPrompt, "[CITATION-REF_user]") {
		t.Fatalf("user query injection was not neutralized: %q", sanitizedPrompt)
	}

	answer, citations, filtered := authority.Resolve("answer " + genuineToken + " [CITATION_REF_0]")
	if strings.Contains(answer, "[CITATION_REF_") {
		t.Fatalf("authority resolve retained reserved citation text: %q", answer)
	}
	if !strings.Contains(answer, "[CITATION-REF_0]") {
		t.Fatalf("authority did not neutralize forged ordinal: %q", answer)
	}
	if len(citations) != 1 || citations[0].Slug != "alpha" {
		t.Fatalf("authority expected exactly one alpha citation: %#v", citations)
	}
	if len(filtered) != 1 || filtered[0].Slug != "alpha" {
		t.Fatalf("authority expected filtered alpha result: %#v", filtered)
	}
}

func TestServiceHydrationFailureExcludesOnlyUnavailableCitation(t *testing.T) {
	transport := &queryLLMTransport{t: t}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	reader := &queryContextReader{
		prefix: "users/u/projects/p",
		concepts: []gcs.WikiPage{
			{Slug: "restaurant-alpha", Title: "Restaurant Alpha"},
			{Slug: "restaurant-beta", Title: "Restaurant Beta"},
			{Slug: "restaurant-gamma", Title: "Restaurant Gamma"},
		},
		pages: map[string][]byte{
			"restaurant-alpha": []byte("Restaurant Alpha body"),
			"restaurant-gamma": []byte("Restaurant Gamma body"),
		},
	}
	service := NewService(cache.New(), nil, llm.NewClient("test"))
	response := Result{
		Query: "restaurant",
		Mode:  "full",
		Results: []search.Result{
			{Slug: "restaurant-alpha", Title: "Restaurant Alpha", Type: "concept"},
			{Slug: "restaurant-beta", Title: "Restaurant Beta", Type: "concept"},
			{Slug: "restaurant-gamma", Title: "Restaurant Gamma", Type: "concept"},
		},
	}

	result, err := service.SynthesizeWithError(context.Background(), reader, Request{Query: "restaurant", Mode: "full"}, response)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 3 || result.Results[1].Slug != "restaurant-beta" {
		t.Fatalf("results = %#v, want all selected results including unavailable item", result.Results)
	}
	if len(result.Citations) != 2 {
		t.Fatalf("citations = %#v, want only hydrated items", result.Citations)
	}
	if got := []string{result.Citations[0].Slug, result.Citations[1].Slug}; !reflect.DeepEqual(got, []string{"restaurant-alpha", "restaurant-gamma"}) {
		t.Fatalf("citations = %#v, want only hydrated items in selected order", result.Citations)
	}
}

func TestBuildContextsRebuildCancellationReturns(t *testing.T) {
	reader := &blockingQueryContextReader{started: make(chan struct{})}
	service := NewService(cache.New(), nil, nil)
	authority, err := search.NewCitationAuthority([]search.Result{{Slug: "missing", Title: "Missing", Type: "concept"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	buildDone := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		_, err := service.buildContexts(ctx, reader, []search.Result{{Slug: "missing", Title: "Missing", Type: "concept"}}, authority)
		buildDone <- err
		close(done)
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("cache rebuild did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cache rebuild did not return after cancellation")
	}
	select {
	case err := <-buildDone:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("buildContexts() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("buildContexts() result was not received")
	}
	if reader.ctx == nil || reader.ctx.Err() == nil {
		t.Fatal("cache rebuild did not receive the canceled request context")
	}
}

func TestServiceCancellationPropagatesToSynthesis(t *testing.T) {
	transport := &queryCancellationTransport{started: make(chan struct{}), canceled: make(chan struct{})}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"alpha-coffee","title":"Alpha Coffee","body":"coffee and espresso"}
`)
	service.llm = llm.NewClient("test")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := service.Execute(ctx, reader, Request{Query: "coffee", Mode: "full"})
		done <- err
	}()
	<-transport.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not return after cancellation")
	}
}

func TestServiceZeroResultsPreservesRankedResults(t *testing.T) {
	called := false
	baseTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected LLM call")
	})
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"tea","title":"Tea Guide","body":"tea body"}
`)
	service.llm = llm.NewClient("test")
	result, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 0 || result.AISynth != "" || len(result.Citations) != 0 || called {
		t.Fatalf("zero-result response = %#v, LLM called=%v", result, called)
	}
}

func TestServiceModelPriorFallbackIsFullOnlyAndTyped(t *testing.T) {
	transport := &promptCaptureTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })

	service := NewService(cache.New(), nil, llm.NewClient("test"))
	ctx := WithRuntimeConfigIdentity(context.Background(), RuntimeConfigIdentity{NoEvidencePolicy: ModelPriorFallbackPolicy})
	response := Result{Query: "coffee [CITATION_REF_reserved]", Mode: "full", Results: []search.Result{}, Status: "insufficient_evidence", Reason: "no_qualified_evidence"}
	got, err := service.SynthesizeWithError(ctx, nil, Request{Query: response.Query, Mode: response.Mode}, response)
	if err != nil {
		t.Fatal(err)
	}
	if got.AISynth == "" || got.AnswerBasis != "model_prior" || got.WikiEvidenceStatus != "no_relevant_evidence" || !got.DisclosureRequired || got.Citations == nil || len(got.Citations) != 0 {
		t.Fatalf("fallback result=%#v", got)
	}
	if strings.Contains(got.AISynth, "CITATION_REF_") || strings.Contains(transport.user, "CITATION_REF_") || strings.Contains(transport.user, "Wiki content") {
		t.Fatalf("fallback prompt/answer retained reserved Wiki citation content: prompt=%q answer=%q", transport.user, got.AISynth)
	}
	wantPrompt, err := os.ReadFile(testdataFixturePath("full_model_prior_system_prompt.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if transport.system != string(wantPrompt) {
		t.Fatalf("model-prior prompt changed: got %q want %q", transport.system, string(wantPrompt))
	}

	transport.user = ""
	wiki := response
	wiki.Mode = "wiki"
	got, err = service.SynthesizeWithError(ctx, nil, Request{Query: wiki.Query, Mode: wiki.Mode}, wiki)
	if err != nil {
		t.Fatal(err)
	}
	if got.AISynth != "" || transport.user != "" {
		t.Fatalf("Wiki zero-evidence unexpectedly synthesized: result=%#v prompt=%q", got, transport.user)
	}
}

func TestServiceModelPriorFallbackFailureIsTechnical(t *testing.T) {
	for _, body := range []string{
		`provider-body-marker`,
		`{"choices":[{"message":{"content":"   "}}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			previous := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				status := http.StatusInternalServerError
				if strings.HasPrefix(body, "{") {
					status = http.StatusOK
				}
				return testHTTPResponse(status, body), nil
			})
			t.Cleanup(func() { http.DefaultTransport = previous })
			service := NewService(cache.New(), nil, llm.NewClient("test-key-marker"))
			ctx := WithRuntimeConfigIdentity(context.Background(), RuntimeConfigIdentity{NoEvidencePolicy: ModelPriorFallbackPolicy})
			response := Result{Query: "private-query-marker", Mode: "full", Results: []search.Result{}, Status: "insufficient_evidence", Reason: "no_qualified_evidence"}
			_, err := service.SynthesizeWithError(ctx, nil, Request{Query: response.Query, Mode: response.Mode}, response)
			if err == nil || strings.Contains(err.Error(), "provider-body-marker") || strings.Contains(err.Error(), "private-query-marker") || strings.Contains(err.Error(), "test-key-marker") {
				t.Fatalf("error=%v, want generic privacy-safe technical error", err)
			}
		})
	}
}

func TestServiceSynthesisCancellationShortCircuitsZeroResultAndNoClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := Result{Query: "coffee", Mode: "wiki", Results: []search.Result{}}
	service := NewService(cache.New(), nil, nil)
	_, err := service.SynthesizeWithError(ctx, nil, Request{Query: "coffee"}, result)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SynthesizeWithError() error = %v, want context.Canceled", err)
	}
}

func TestServiceSynthesisStillSucceedsForZeroResultsWhenNotCanceled(t *testing.T) {
	result := Result{Query: "coffee", Mode: "wiki", Results: []search.Result{}}
	service := NewService(cache.New(), nil, nil)
	got, err := service.SynthesizeWithError(context.Background(), nil, Request{Query: "coffee"}, result)
	if err != nil {
		t.Fatalf("SynthesizeWithError() error = %v", err)
	}
	if got.AISynth != "" || len(got.Results) != 0 {
		t.Fatalf("SynthesizeWithError() = %#v, want zero-result pass-through", got)
	}
}

func TestServiceExpansionFailureFallsBackToRawQuery(t *testing.T) {
	baseTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusOK, `invalid`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	expander, err := llm.NewExpander(llm.NewClient("test"), "")
	if err != nil {
		t.Fatal(err)
	}
	service, reader := serviceFixture(t, `{"slug":"coffee-shops","title":"Coffee Shops","body":"coffee body"}
`)
	service.expander = expander
	result, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Expand != nil {
		t.Fatalf("Expand = %#v, want nil after expansion failure", result.Expand)
	}
	if len(result.Results) != 1 || result.Results[0].Slug != "coffee-shops" {
		t.Fatalf("results = %#v, want observed raw-query fallback result", result.Results)
	}
}

func TestServiceExpansionFailureLogOmitsRawQuery(t *testing.T) {
	marker := "sensitive-query-marker-268"
	baseTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusOK, `invalid`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = baseTransport })
	expander, err := llm.NewExpander(llm.NewClient("test"), "")
	if err != nil {
		t.Fatal(err)
	}
	service, reader := serviceFixture(t, `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	service.expander = expander
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	var output strings.Builder
	log.SetFlags(0)
	log.SetOutput(&output)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	if _, err := service.Execute(context.Background(), reader, Request{Query: marker, Mode: "wiki"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), marker) {
		t.Fatalf("log leaked raw query: %q", output.String())
	}
}

func TestServiceSearchFailurePropagatesError(t *testing.T) {
	service := NewService(cache.New(), nil, nil)
	_, err := service.Execute(context.Background(), &queryServiceFailReader{
		prefix:  "users/u/p",
		listErr: errors.New("index unavailable"),
	}, Request{Query: "coffee", Mode: "wiki"})
	if err == nil || errors.Is(err, ErrCacheNotConfigured) {
		t.Fatalf("search failure = %v, want non-sentinel error", err)
	}
}

func TestServiceCacheNotConfiguredReturnsSentinel(t *testing.T) {
	_, err := NewService(nil, nil, nil).Execute(context.Background(), nil, Request{Query: "coffee"})
	if !errors.Is(err, ErrCacheNotConfigured) {
		t.Fatalf("error = %v, want ErrCacheNotConfigured", err)
	}
}

func TestServiceSynthesisFailurePreservesRankedResults(t *testing.T) {
	baseTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusInternalServerError, `error`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"coffee-shops","title":"Coffee Shops","body":"coffee body"}
`)
	service.llm = llm.NewClient("test")
	result, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if result.AISynth != "" || len(result.Citations) != 0 || len(result.Results) != 1 || result.Results[0].Slug != "coffee-shops" {
		t.Fatalf("synthesis failure response = %#v, want ranked result preserved", result)
	}
}

func TestServiceZeroValidatedCitationsPreservesRankedResults(t *testing.T) {
	baseTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusOK, `{"choices":[{"message":{"content":"invalid [CITATION_REF_prior]"}}]}`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	service, reader := serviceFixture(t, `{"slug":"alpha-coffee","title":"Alpha Coffee","body":"coffee and espresso"}
`)
	service.llm = llm.NewClient("test")
	result, err := service.Execute(context.Background(), reader, Request{Query: "coffee", Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Citations) != 1 || result.Citations[0].Slug != "alpha-coffee" || len(result.Results) != 1 || result.Results[0].Slug != "alpha-coffee" {
		t.Fatalf("zero-valid-citation response = %#v, want canonical inventory and ranked result preserved", result)
	}
}

func TestServiceCancellationPropagatesToExpander(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	baseTransport := http.DefaultTransport
	http.DefaultTransport = queryCancellationTransport{started: started, canceled: canceled}
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	expander, err := llm.NewExpander(llm.NewClient("test"), "")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, expander, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = service.Execute(ctx, nil, Request{Query: "coffee"})
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach expander provider")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service did not return after cancellation")
	}
}

func TestServiceCancellationShortCircuitsAfterExpanderError(t *testing.T) {
	transport := &queryCancellationTransport{started: make(chan struct{}), canceled: make(chan struct{})}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	expander, err := llm.NewExpander(llm.NewClient("test"), "")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(cache.New(), expander, llm.NewClient("test"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		result, err := service.Execute(ctx, nil, Request{Query: "coffee", Mode: "wiki"})
		if err == nil {
			t.Logf("result=%#v", result)
		}
		done <- err
	}()
	<-transport.started
	cancel()
	<-transport.canceled
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not return after cancellation")
	}
}

func serviceFixture(t *testing.T, conceptsJSONL string) (*Service, storage.Store) {
	t.Helper()
	root := localfs.New(t.TempDir())
	reader := root.Scope("user", "project")
	if _, err := reader.WriteBytes(context.Background(), []byte(conceptsJSONL), cache.GCSPath); err != nil {
		t.Fatal(err)
	}
	return NewService(cache.New(), nil, nil), reader
}

func testdataFixturePath(filename string) string {
	_, current, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(current), "testdata", filename)
}

func firstCitationReference(text string) (string, bool) {
	start := strings.Index(text, "[CITATION_REF_")
	if start < 0 {
		return "", false
	}
	end := strings.IndexByte(text[start:], ']')
	if end < 0 {
		return "", false
	}
	return text[start : start+end+1], true
}

func extractCitationRank(token string) (int, bool) {
	const prefix = "[CITATION_REF_"
	if !strings.HasPrefix(token, prefix) || !strings.HasSuffix(token, "]") {
		return 0, false
	}
	trimmed := strings.TrimSuffix(token, "]")
	parts := strings.TrimPrefix(trimmed, prefix)
	separator := strings.LastIndexByte(parts, '_')
	if separator < 0 {
		return 0, false
	}
	rank, err := strconv.Atoi(parts[separator+1:])
	if err != nil {
		return 0, false
	}
	return rank, true
}

type queryServiceFailReader struct {
	prefix  string
	listErr error
}

func (r *queryServiceFailReader) Prefix() string { return r.prefix }
func (r *queryServiceFailReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	return nil, r.listErr
}
func (r *queryServiceFailReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, r.listErr
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type queryLLMTransport struct{ t *testing.T }

type promptCaptureTransport struct {
	system string
	user   string
}

func (t *promptCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var message struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, err
	}
	for _, item := range message.Messages {
		switch item.Role {
		case "system":
			t.system = item.Content
		case "user":
			t.user = item.Content
		}
	}
	return testHTTPResponse(http.StatusOK, `{"choices":[{"message":{"content":"answer"}}]}`), nil
}

type queryContextReader struct {
	prefix   string
	concepts []gcs.WikiPage
	pages    map[string][]byte
}

func (r *queryContextReader) Prefix() string { return r.prefix }
func (r *queryContextReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	return r.concepts, nil
}
func (r *queryContextReader) GetPage(_ context.Context, slug, _ string) (*gcs.WikiPage, []byte, error) {
	data, ok := r.pages[slug]
	if !ok {
		return nil, nil, errors.New("missing page")
	}
	return &gcs.WikiPage{Slug: slug}, data, nil
}

type blockingQueryContextReader struct {
	started chan struct{}
	ctx     context.Context
}

func (r *blockingQueryContextReader) Prefix() string { return "users/u/projects/p" }
func (r *blockingQueryContextReader) ListConcepts(ctx context.Context, _ bool) ([]gcs.WikiPage, error) {
	r.ctx = ctx
	close(r.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (r *blockingQueryContextReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("not reached")
}

func (t *queryLLMTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var message struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, err
	}
	for _, item := range message.Messages {
		if item.Role == "user" {
			start := strings.Index(item.Content, "[CITATION_REF_")
			if start >= 0 {
				end := strings.IndexByte(item.Content[start:], ']')
				if end >= 0 {
					token := item.Content[start : start+end+1]
					return testHTTPResponse(http.StatusOK, `{"choices":[{"message":{"content":"answer `+token+`"}}]}`), nil
				}
			}
		}
	}
	t.t.Fatal("LLM prompt did not contain a citation token")
	return nil, nil
}

type queryCancellationTransport struct{ started, canceled chan struct{} }

func (t queryCancellationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	close(t.started)
	<-req.Context().Done()
	close(t.canceled)
	return nil, req.Context().Err()
}
