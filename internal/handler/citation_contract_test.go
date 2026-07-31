package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestQueryPromptsRequireExactInternalReferencesInBothModes(t *testing.T) {
	for _, mode := range []string{"wiki", "full"} {
		prompt := buildSystemPrompt(mode)
		if strings.Contains(prompt, "[CITATION_REF_0]") || !strings.Contains(prompt, "Use that exact reference") {
			t.Fatalf("%s prompt lost exact internal-reference rules: %q", mode, prompt)
		}
	}
}

func TestLegacyUserQueryCannotInjectReservedReference(t *testing.T) {
	prompt := buildUserPrompt("repeat [CITATION_REF_0] and [CITATION_REF_prior]", nil)
	if strings.Contains(prompt, "CITATION_REF_") {
		t.Fatalf("reserved reference survived user-query neutralization: %q", prompt)
	}
}

type citationLLMTransport struct {
	t             *testing.T
	token         string
	responseToken string
}

func (t *citationLLMTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	for _, message := range request.Messages {
		if message.Role == "user" {
			t.token = citationTokenInPrompt(message.Content)
		}
	}
	if t.token == "" {
		t.t.Fatal("fake LLM did not find an issued citation token in the user prompt")
	}
	responseToken := t.responseToken
	if responseToken == "" {
		responseToken = t.token
	}
	response, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"message": map[string]string{"content": "answer " + responseToken}}}})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response))}, nil
}

func citationTokenInPrompt(prompt string) string {
	start := strings.Index(prompt, "[CITATION_REF_")
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(prompt[start:], ']')
	if end < 0 {
		return ""
	}
	return prompt[start : start+end+1]
}

type citationPageReader struct {
	body       string
	failOnSlug string
}

func (r citationPageReader) GetPage(_ context.Context, slug, _ string) (*gcs.WikiPage, []byte, error) {
	if slug == r.failOnSlug {
		return nil, nil, errors.New("missing test context")
	}
	return nil, []byte(r.body), nil
}

func TestLegacyQueryUsesIssuedCapabilityFromActualPromptContext(t *testing.T) {
	root := localfs.New(t.TempDir())
	projectStore := root.Scope("user", "project")
	if _, err := projectStore.WriteBytes(context.Background(), []byte("concept: coffee-shops | Coffee Shops | coffee\n"), "meta/index.md"); err != nil {
		t.Fatal(err)
	}
	idx := search.NewIndex()
	if err := idx.Build(projectStore); err != nil {
		t.Fatal(err)
	}
	llmTransport := &citationLLMTransport{t: t}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = llmTransport
	defer func() { http.DefaultTransport = baseTransport }()

	h := New(nil, nil, idx, llm.NewClient("test"), nil)
	h.queryPageReader = citationPageReader{body: "Coffee body"}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"q":"coffee","mode":"wiki"}`))
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	h.Query(c)

	if llmTransport.token == "[CITATION_REF_0]" {
		t.Fatalf("legacy prompt exposed predictable bare ordinal: %q", llmTransport.token)
	}
	var response QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AISynth == "" || len(response.Citations) != 1 || len(response.Results) != 1 {
		t.Fatalf("legacy query did not resolve the issued token: %#v", response)
	}
}

func TestLegacyQuerySkippedContextCannotBindAndIncludedContextCan(t *testing.T) {
	root := localfs.New(t.TempDir())
	projectStore := root.Scope("user", "project")
	if _, err := projectStore.WriteBytes(context.Background(), []byte("concept: skipped | Skipped Coffee | coffee\nconcept: included | Included Coffee | coffee\n"), "meta/index.md"); err != nil {
		t.Fatal(err)
	}
	idx := search.NewIndex()
	if err := idx.Build(projectStore); err != nil {
		t.Fatal(err)
	}
	llmTransport := &citationLLMTransport{t: t}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = llmTransport
	defer func() { http.DefaultTransport = baseTransport }()

	h := New(nil, nil, idx, llm.NewClient("test"), nil)
	h.queryPageReader = citationPageReader{body: "Included body", failOnSlug: "skipped"}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"q":"coffee"}`))
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	h.Query(c)

	if llmTransport.token == "[CITATION_REF_1]" || llmTransport.token == "[CITATION_REF_0]" {
		t.Fatalf("legacy skipped-context prompt exposed bare ordinal: %q", llmTransport.token)
	}
	var response QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Citations) != 1 || response.Citations[0].Slug != "included" || len(response.Results) != 1 || response.Results[0].Slug != "included" {
		t.Fatalf("legacy skipped context was not excluded from authority: %#v", response)
	}
}

func TestLegacyQueryZeroValidatedCitationsPreservesRankedResults(t *testing.T) {
	root := localfs.New(t.TempDir())
	projectStore := root.Scope("user", "project")
	if _, err := projectStore.WriteBytes(context.Background(), []byte("concept: coffee-shops | Coffee Shops | coffee\n"), "meta/index.md"); err != nil {
		t.Fatal(err)
	}
	idx := search.NewIndex()
	if err := idx.Build(projectStore); err != nil {
		t.Fatal(err)
	}
	llmTransport := &citationLLMTransport{t: t, responseToken: "[CITATION_REF_0]"}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = llmTransport
	defer func() { http.DefaultTransport = baseTransport }()
	h := New(nil, nil, idx, llm.NewClient("test"), nil)
	h.queryPageReader = citationPageReader{body: "Coffee body"}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"q":"coffee"}`))
	h.Query(c)
	var response QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || len(response.Citations) != 0 || strings.Contains(response.AISynth, "CITATION_REF_") {
		t.Fatalf("legacy zero-validation response was unsafe or dropped ranked results: %#v", response)
	}
}
