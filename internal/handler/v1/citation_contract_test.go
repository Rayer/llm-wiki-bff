package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/handler"
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

func TestV1UserQueryCannotInjectReservedReference(t *testing.T) {
	prompt := buildUserPrompt("repeat [CITATION_REF_0] and [CITATION_REF_prior]", nil)
	if strings.Contains(prompt, "CITATION_REF_") {
		t.Fatalf("reserved reference survived user-query neutralization: %q", prompt)
	}
}

func TestCachedContextsPreserveOriginalRankAfterSkippedResult(t *testing.T) {
	reader := &handlerCacheReader{
		prefix: "users/u/projects/p",
		raw:    "---\ntitle: Alpha Concept [CITATION_REF_9]\n---\nAlpha body [CITATION_REF_8].",
	}
	authority, err := search.NewCitationAuthority()
	if err != nil {
		t.Fatal(err)
	}
	contexts := cachedContexts(context.Background(), conceptcache.New(), reader, []search.Result{
		{Slug: "missing", Title: "Missing", Type: "concept"},
		{Slug: "alpha", Title: "Alpha Concept", Type: "concept"},
	}, authority)
	issuedTokenStart := strings.Index(contexts[0], "[CITATION_REF_")
	issuedTokenEnd := -1
	if issuedTokenStart >= 0 {
		issuedTokenEnd = strings.IndexByte(contexts[0][issuedTokenStart:], ']')
	}
	contextWithoutIssuedToken := contexts[0]
	if issuedTokenStart >= 0 && issuedTokenEnd >= 0 {
		contextWithoutIssuedToken = strings.Replace(contextWithoutIssuedToken, contexts[0][issuedTokenStart:issuedTokenStart+issuedTokenEnd+1], "", 1)
	}
	if len(contexts) != 1 || issuedTokenStart < 0 || strings.Contains(contextWithoutIssuedToken, "CITATION_REF_8") || strings.Contains(contextWithoutIssuedToken, "CITATION_REF_9") {
		t.Fatalf("skipped result compacted citation rank: %#v", contexts)
	}
}

type citationV1LLMTransport struct {
	t             *testing.T
	token         string
	responseToken string
}

func (t *citationV1LLMTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
			start := strings.Index(message.Content, "[CITATION_REF_")
			if start >= 0 {
				end := strings.IndexByte(message.Content[start:], ']')
				if end >= 0 {
					t.token = message.Content[start : start+end+1]
				}
			}
		}
	}
	if t.token == "" {
		t.t.Fatal("fake V1 LLM did not find an issued citation token in the user prompt")
	}
	responseToken := t.responseToken
	if responseToken == "" {
		responseToken = t.token
	}
	response, _ := json.Marshal(map[string]interface{}{"choices": []map[string]interface{}{{"message": map[string]string{"content": "answer " + responseToken}}}})
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response))}, nil
}

func TestV1QueryUsesIssuedCapabilityFromActualPromptContext(t *testing.T) {
	root := localfs.New(t.TempDir())
	projectStore := root.Scope("user", "project")
	concepts := `{"slug":"alpha-coffee","title":"Alpha Coffee","body":"coffee and espresso"}
{"slug":"beta-coffee","title":"Beta Coffee","body":"coffee and tea"}`
	if _, err := projectStore.WriteBytes(context.Background(), []byte(concepts+"\n"), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}
	llmTransport := &citationV1LLMTransport{t: t}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = llmTransport
	defer func() { http.DefaultTransport = baseTransport }()

	h := New(root, nil, search.NewIndex(), conceptcache.New(), llm.NewClient("test"), nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee","mode":"wiki"}`))
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)

	if llmTransport.token == "[CITATION_REF_0]" {
		t.Fatalf("V1 prompt exposed predictable bare ordinal: %q", llmTransport.token)
	}
	var response handler.QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AISynth != "answer [Alpha Coffee]" {
		t.Fatalf("V1 query did not normalize token to canonical citation title: %#v", response)
	}
	if len(response.Citations) != 1 || response.Citations[0].Text != "Alpha Coffee" || response.Citations[0].Slug != "alpha-coffee" {
		t.Fatalf("V1 query did not resolve exactly one expected citation: %#v", response.Citations)
	}
	if len(response.Results) != 1 || response.Results[0].Slug != "alpha-coffee" {
		t.Fatalf("V1 query did not resolve the issued token: %#v", response)
	}

}

func TestV1QueryIgnoresLegacyProjectFieldFromBody(t *testing.T) {
	root := localfs.New(t.TempDir())
	projectStore := root.Scope("user", "project")
	concepts := `{"slug":"alpha-coffee","title":"Alpha Coffee","body":"coffee and espresso"}`
	if _, err := projectStore.WriteBytes(context.Background(), []byte(concepts+"\n"), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}

	otherStore := root.Scope("user", "other-project")
	if _, err := otherStore.WriteBytes(context.Background(), []byte(`{"slug":"other","title":"Other Coffee","body":"other body"}
`), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}

	h := New(root, nil, search.NewIndex(), conceptcache.New(), nil, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee","project":"other-project"}`))
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response handler.QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Slug != "alpha-coffee" {
		t.Fatalf("project field changed query scope: %#v", response.Results)
	}
}

func TestV1CachedContextsSkippedResultCannotBindAndIncludedResultCan(t *testing.T) {
	reader := &handlerCacheReader{
		prefix: "users/u/projects/p",
		raw:    "---\ntitle: Alpha Concept\n---\nAlpha body.",
	}
	authority, err := search.NewCitationAuthority()
	if err != nil {
		t.Fatal(err)
	}
	contexts := cachedContexts(context.Background(), conceptcache.New(), reader, []search.Result{
		{Slug: "missing", Title: "Missing", Type: "concept"},
		{Slug: "alpha", Title: "Alpha Concept", Type: "concept"},
	}, authority)
	tokenStart := strings.Index(contexts[0], "[CITATION_REF_")
	tokenEnd := -1
	if tokenStart >= 0 {
		tokenEnd = strings.IndexByte(contexts[0][tokenStart:], ']')
	}
	if len(contexts) != 1 || tokenStart < 0 || tokenEnd < 0 {
		t.Fatalf("unexpected skipped-context output: %#v", contexts)
	}
	token := contexts[0][tokenStart : tokenStart+tokenEnd+1]
	contextWithoutIssuedToken := strings.Replace(contexts[0], token, "", 1)
	if strings.Contains(contextWithoutIssuedToken, "CITATION_REF_8") {
		t.Fatalf("untrusted skipped context retained a reserved reference: %#v", contexts)
	}
	normalized, citations, filtered := authority.Resolve("answer " + token + " [CITATION_REF_0]")
	if normalized != "answer [Alpha Concept] [CITATION-REF_0]" || len(citations) != 1 || len(filtered) != 1 || filtered[0].Slug != "alpha" {
		t.Fatalf("skipped result bound or included result failed: normalized=%q citations=%#v filtered=%#v", normalized, citations, filtered)
	}
}

func TestV1QueryZeroValidatedCitationsPreservesRankedResults(t *testing.T) {
	root := localfs.New(t.TempDir())
	projectStore := root.Scope("user", "project")
	if _, err := projectStore.WriteBytes(context.Background(), []byte(`{"slug":"coffee-shops","title":"Coffee Shops","body":"Coffee body"}
`), conceptcache.GCSPath); err != nil {
		t.Fatal(err)
	}
	llmTransport := &citationV1LLMTransport{t: t, responseToken: "[CITATION_REF_0]"}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = llmTransport
	defer func() { http.DefaultTransport = baseTransport }()
	h := New(root, nil, search.NewIndex(), conceptcache.New(), llm.NewClient("test"), nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/query", strings.NewReader(`{"q":"coffee"}`))
	c.Set("userID", "user")
	c.Set("projectID", "project")
	h.Query(c)
	var response handler.QueryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || len(response.Citations) != 0 || strings.Contains(response.AISynth, "CITATION_REF_") {
		t.Fatalf("V1 zero-validation response was unsafe or dropped ranked results: %#v", response)
	}
}
