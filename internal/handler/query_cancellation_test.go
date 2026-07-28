package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
	h := New(nil, nil, search.NewIndex(), nil, expander)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewBufferString(`{"q":"coffee shops"}`)).WithContext(ctx)
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
