package v1

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

type queryProviderTransport struct {
	started  chan struct{}
	canceled chan struct{}
}

func (t queryProviderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	close(t.started)
	<-req.Context().Done()
	close(t.canceled)
	return nil, req.Context().Err()
}

type cachedContextCancellationReader struct {
	started chan struct{}
}

func (r *cachedContextCancellationReader) Prefix() string {
	return "users/u/projects/p"
}

func (r *cachedContextCancellationReader) ListConcepts(ctx context.Context, _ bool) ([]gcs.WikiPage, error) {
	close(r.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *cachedContextCancellationReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, context.Canceled
}

func TestQueryPassesCanceledRequestToExpander(t *testing.T) {
	gin.SetMode(gin.TestMode)
	started := make(chan struct{})
	providerCanceled := make(chan struct{})
	baseTransport := http.DefaultTransport
	http.DefaultTransport = queryProviderTransport{started: started, canceled: providerCanceled}
	defer func() { http.DefaultTransport = baseTransport }()

	expander, err := llm.NewExpander(llm.NewClient("test"), "")
	if err != nil {
		t.Fatal(err)
	}
	h := New(nil, nil, search.NewIndex(), nil, nil, expander)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewBufferString(`{"q":"coffee shops"}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(response)
	ginContext.Request = req

	done := make(chan struct{})
	go func() {
		h.Query(ginContext)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expander provider did not receive request")
	}
	cancel()
	select {
	case <-providerCanceled:
	case <-time.After(time.Second):
		t.Fatal("canceled handler context did not reach expander provider")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after expander cancellation")
	}
}

func TestQueryCancellationPropagatesToCachedContextRebuild(t *testing.T) {
	reader := &cachedContextCancellationReader{started: make(chan struct{})}
	conceptCache := cache.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	completed := make(chan struct{})
	go func() {
		authority, _ := search.NewCitationAuthority()
		_ = cachedContexts(ctx, conceptCache, reader, []search.Result{{Slug: "coffee-shops", Title: "Coffee Shops"}}, authority)
		close(completed)
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("cachedContexts did not start ListConcepts")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("cachedContexts did not return after cancellation")
	}
}
