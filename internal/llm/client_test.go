package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChatCancellationInterruptsHTTPCall(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()

	client := NewClient("fixture-key")
	client.baseURL = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Chat(ctx, "system", "user")
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat() error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Chat() waited for the fixed HTTP timeout after context cancellation")
	}
}
