package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExpansionAndSynthesisClientOptionsAreIsolated(t *testing.T) {
	type request struct {
		Model       string   `json:"model"`
		Temperature *float64 `json:"temperature"`
	}
	requests := make(chan request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got request
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests <- got
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	temperature := 0.0
	expansion := NewClientWithOptions("test-key", ClientOptions{Model: "deepseek-v4-pro", Temperature: &temperature})
	expansion.baseURL = server.URL
	synthesis := NewClient("test-key")
	synthesis.baseURL = server.URL
	if _, err := expansion.Chat(context.Background(), "system", "expand"); err != nil {
		t.Fatal(err)
	}
	if _, err := synthesis.Chat(context.Background(), "system", "synthesize"); err != nil {
		t.Fatal(err)
	}

	gotExpansion := <-requests
	gotSynthesis := <-requests
	if gotExpansion.Model != "deepseek-v4-pro" || gotExpansion.Temperature == nil || *gotExpansion.Temperature != 0 {
		t.Fatalf("expansion request = %#v, want deepseek-v4-pro and temperature 0", gotExpansion)
	}
	if gotSynthesis.Model != "deepseek-chat" || gotSynthesis.Temperature != nil {
		t.Fatalf("synthesis request = %#v, want deepseek-chat without temperature", gotSynthesis)
	}
}

func TestChatErrorDoesNotExposeResponseBodyOrAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("raw-model-response-and-secret-key"))
	}))
	defer server.Close()
	client := NewClient("secret-key")
	client.baseURL = server.URL
	_, err := client.Chat(context.Background(), "system", "user")
	if err == nil || contains(err.Error(), "raw-model-response") || contains(err.Error(), "secret-key") {
		t.Fatalf("Chat() error = %v, leaked response or credential", err)
	}
}

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
