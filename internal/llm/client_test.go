package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type countingRoundTripper struct{ calls atomic.Int32 }

func (r *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))), Header: make(http.Header)}, nil
}

type testCallRecorder struct {
	starts   atomic.Int32
	finishes atomic.Int32
	url      string
	outcomes []string
	finished []time.Time
}

func (r *testCallRecorder) StartCall(string, string, string) func(string) { return func(string) {} }
func (r *testCallRecorder) StartCallAt(_, _, _, rawURL string) func(string) {
	r.starts.Add(1)
	r.url = rawURL
	return func(outcome string) { r.finishes.Add(1); r.outcomes = append(r.outcomes, outcome) }
}
func (r *testCallRecorder) StartCallAtTimed(_, _, _, rawURL string) func(string, time.Time) {
	r.starts.Add(1)
	r.url = rawURL
	return func(outcome string, finished time.Time) {
		r.finishes.Add(1)
		r.outcomes = append(r.outcomes, outcome)
		r.finished = append(r.finished, finished)
	}
}

func TestChatPreCanceledContextMakesNoAttemptOrReceipt(t *testing.T) {
	transport := &countingRoundTripper{}
	client := NewClient("key")
	client.client = &http.Client{Transport: transport}
	recorder := &testCallRecorder{}
	ctx, cancel := context.WithCancel(WithCallRecorder(context.Background(), recorder))
	cancel()
	if _, err := client.Chat(ctx, "s", "u"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() error = %v, want cancellation", err)
	}
	if transport.calls.Load() != 0 || recorder.starts.Load() != 0 {
		t.Fatalf("attempts=%d receipts=%d, want zero", transport.calls.Load(), recorder.starts.Load())
	}
}

func TestNewClientRejectsInvalidReasoning(t *testing.T) {
	if client := NewClientWithOptions("key", ClientOptions{Reasoning: Reasoning("arbitrary")}); client != nil {
		t.Fatal("invalid reasoning constructed a client")
	}
}

func TestClientModelIdentityIsSanitizedAndUnavailableWhenNil(t *testing.T) {
	temperature := 0.0
	client := NewClientWithOptions("secret-key", ClientOptions{Model: "deepseek-v4-flash", Temperature: &temperature, Reasoning: ReasoningNone})
	identity, ok := client.ModelIdentity()
	if !ok || identity.Provider != "deepseek" || identity.Model != "deepseek-v4-flash" || identity.Reasoning != string(ReasoningNone) || identity.Temperature != 0 {
		t.Fatalf("identity=%+v ok=%v", identity, ok)
	}
	if _, ok := (*Client)(nil).ModelIdentity(); ok {
		t.Fatal("nil client claimed an identity")
	}
}

func TestChatReceiptUsesActualRequestURL(t *testing.T) {
	transport := &countingRoundTripper{}
	client := NewClient("key")
	client.baseURL = "http://127.0.0.1:4321/override?q=query"
	client.client = &http.Client{Transport: transport}
	recorder := &testCallRecorder{}
	if _, err := client.Chat(WithCallRecorder(context.Background(), recorder), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if recorder.url != "http://127.0.0.1:4321/override?q=query/chat/completions" {
		t.Fatalf("recorded URL = %q", recorder.url)
	}
	if recorder.finishes.Load() != 1 {
		t.Fatalf("finished receipts = %d, want one", recorder.finishes.Load())
	}
}

func TestFlashExpansionAndSynthesisClientOptionsAreIsolated(t *testing.T) {
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
	expansion := NewClientWithOptions("test-key", ClientOptions{Model: "deepseek-v4-flash", Temperature: &temperature})
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
	if gotExpansion.Model != "deepseek-v4-flash" || gotExpansion.Temperature == nil || *gotExpansion.Temperature != 0 {
		t.Fatalf("expansion request = %#v, want deepseek-v4-flash and temperature 0", gotExpansion)
	}
	if gotSynthesis.Model != "deepseek-chat" || gotSynthesis.Temperature != nil {
		t.Fatalf("synthesis request = %#v, want deepseek-chat without temperature", gotSynthesis)
	}
}

func TestChatSendsExplicitThinkingPolicy(t *testing.T) {
	var got struct {
		Model           string          `json:"model"`
		Temperature     *float64        `json:"temperature"`
		Thinking        json.RawMessage `json:"thinking"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	temperature := 0.0
	client := NewClientWithOptions("test-key", ClientOptions{Model: "deepseek-v4-flash", Temperature: &temperature, Reasoning: "none"})
	client.baseURL = server.URL
	if _, err := client.Chat(context.Background(), "system", "user"); err != nil {
		t.Fatal(err)
	}
	if got.Model != "deepseek-v4-flash" || got.Temperature == nil || *got.Temperature != 0 || string(got.Thinking) != `{"type":"disabled"}` || got.ReasoningEffort != "" {
		t.Fatalf("request = %#v, want flash/0/disabled/no effort", got)
	}
}

func TestChatSendsConfiguredSynthesisReasoning(t *testing.T) {
	for _, reasoning := range []Reasoning{ReasoningLow, ReasoningHigh, ReasoningMax} {
		t.Run(string(reasoning), func(t *testing.T) {
			var got struct {
				Thinking json.RawMessage `json:"thinking"`
				Effort   string          `json:"reasoning_effort"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer server.Close()
			client := NewClientWithOptions("key", ClientOptions{Model: "deepseek-v4-pro", Reasoning: reasoning})
			client.baseURL = server.URL
			if _, err := client.Chat(context.Background(), "s", "u"); err != nil {
				t.Fatal(err)
			}
			if string(got.Thinking) != `{"type":"enabled"}` || got.Effort != string(reasoning) {
				t.Fatalf("thinking=%s effort=%q", got.Thinking, got.Effort)
			}
		})
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

func TestChatActualAttemptsCloseExactlyOnceWithBoundedOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, mode, want string
	}{
		{name: "success", mode: "success", want: "success"},
		{name: "provider error", mode: "provider", want: "provider_error"},
		{name: "malformed JSON", mode: "decode", want: "decode_error"},
		{name: "cancellation", mode: "canceled", want: "canceled"},
		{name: "deadline timeout", mode: "timeout", want: "timeout"},
		{name: "response body cancellation", mode: "body-canceled", want: "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewClient("key")
			client.client = &http.Client{Transport: outcomeRoundTripper{mode: test.mode}}
			recorder := &testCallRecorder{}
			_, _ = client.Chat(WithCallRecorder(context.Background(), recorder), "s", "u")
			if recorder.starts.Load() != 1 || recorder.finishes.Load() != 1 || len(recorder.outcomes) != 1 || recorder.outcomes[0] != test.want {
				t.Fatalf("attempt receipt starts=%d finishes=%d outcomes=%v, want one %q", recorder.starts.Load(), recorder.finishes.Load(), recorder.outcomes, test.want)
			}
			if len(recorder.finished) != 1 || recorder.finished[0].IsZero() {
				t.Fatalf("network endpoint time = %v", recorder.finished)
			}
		})
	}
}

type outcomeRoundTripper struct{ mode string }

func (t outcomeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	switch t.mode {
	case "provider":
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	case "decode":
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("bad"))), Header: make(http.Header)}, nil
	case "canceled":
		return nil, context.Canceled
	case "timeout":
		return nil, context.DeadlineExceeded
	case "body-canceled":
		return &http.Response{StatusCode: http.StatusOK, Body: errorReadCloser{err: context.Canceled}, Header: make(http.Header)}, nil
	default:
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))), Header: make(http.Header)}, nil
	}
}

type errorReadCloser struct{ err error }

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r errorReadCloser) Close() error             { return nil }
