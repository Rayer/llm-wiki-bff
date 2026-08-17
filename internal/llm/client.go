package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type CallRecorder interface {
	StartCall(stage, model, reasoning string) func(outcome string)
}

type HostCallRecorder interface {
	StartHostCall(stage, scheme, host string) func(string)
}

type callURLRecorder interface {
	StartCallAt(stage, model, reasoning, rawURL string) func(string)
}

type timedCallRecorder interface {
	StartCallAtTimed(stage, model, reasoning, rawURL string) func(string, time.Time)
}

type recorderKey struct{}
type stageKey struct{}
type hostRecorderKey struct{}

func WithCallRecorder(ctx context.Context, recorder CallRecorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, recorder)
}

func WithHostCallRecorder(ctx context.Context, recorder HostCallRecorder) context.Context {
	return context.WithValue(ctx, hostRecorderKey{}, recorder)
}

func HostCallRecorderFromContext(ctx context.Context) HostCallRecorder {
	recorder, _ := ctx.Value(hostRecorderKey{}).(HostCallRecorder)
	return recorder
}

func WithCallStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(ctx, stageKey{}, stage)
}

func CallStage(ctx context.Context) string {
	stage, _ := ctx.Value(stageKey{}).(string)
	return stage
}

// Client calls the DeepSeek API (OpenAI-compatible endpoint).
type Client struct {
	apiKey      string
	baseURL     string
	client      *http.Client
	model       string
	temperature *float64
	reasoning   Reasoning
}

func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}
func (c *Client) Reasoning() Reasoning {
	if c == nil {
		return ""
	}
	return c.reasoning
}

const maxChatResponseBytes = 64 * 1024

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Temperature     *float64      `json:"temperature,omitempty"`
	Thinking        thinking      `json:"thinking"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type thinking struct {
	Type string `json:"type"`
}

type ClientOptions struct {
	Model       string
	Temperature *float64
	Reasoning   Reasoning
}

type Reasoning string

const (
	ReasoningNone Reasoning = "none"
	ReasoningLow  Reasoning = "low"
	ReasoningHigh Reasoning = "high"
	ReasoningMax  Reasoning = "max"
)

func (r Reasoning) Valid() bool {
	return r == ReasoningNone || r == ReasoningLow || r == ReasoningHigh || r == ReasoningMax
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// NewClient creates a DeepSeek API client. If apiKey is empty, returns nil.
func NewClient(apiKey string) *Client {
	return NewClientWithOptions(apiKey, ClientOptions{Model: "deepseek-chat"})
}

func NewClientWithOptions(apiKey string, options ClientOptions) *Client {
	if apiKey == "" {
		return nil
	}
	if options.Model == "" {
		options.Model = "deepseek-chat"
	}
	if options.Reasoning == "" {
		options.Reasoning = ReasoningNone
	}
	if !options.Reasoning.Valid() {
		return nil
	}
	return &Client{
		apiKey:      apiKey,
		baseURL:     "https://api.deepseek.com",
		client:      &http.Client{Timeout: 60 * time.Second},
		model:       options.Model,
		temperature: options.Temperature,
		reasoning:   options.Reasoning,
	}
}

// Chat sends a system + user message and returns the assistant's reply.
func (c *Client) Chat(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	var closeCall func(string)
	var closeCallAt func(string, time.Time)
	finish := func(outcome string) {
		if closeCallAt != nil {
			f := closeCallAt
			closeCallAt = nil
			f(outcome, time.Now())
			return
		}
		if closeCall != nil {
			f := closeCall
			closeCall = nil
			f(outcome)
		}
	}
	defer func() { finish("success") }()
	body := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMessage},
		},
		Temperature: c.temperature,
		Thinking: thinking{Type: func() string {
			if c.reasoning == ReasoningNone {
				return "disabled"
			}
			return "enabled"
		}()},
	}
	if c.reasoning != ReasoningNone {
		body.ReasoningEffort = string(c.reasoning)
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if recorder, ok := ctx.Value(recorderKey{}).(CallRecorder); ok {
		stage := CallStage(ctx)
		if timed, ok := recorder.(timedCallRecorder); ok {
			closeCallAt = timed.StartCallAtTimed(stage, c.model, string(c.reasoning), req.URL.String())
		} else if withURL, ok := recorder.(callURLRecorder); ok {
			closeCall = withURL.StartCallAt(stage, c.model, string(c.reasoning), req.URL.String())
		} else {
			closeCall = recorder.StartCall(stage, c.model, string(c.reasoning))
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		finish(callOutcome(ctx, err))
		return "", fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes+1))
	networkFinished := time.Now()
	if err != nil {
		if closeCallAt != nil {
			f := closeCallAt
			closeCallAt = nil
			f(callOutcome(ctx, err), networkFinished)
		} else {
			finish(callOutcome(ctx, err))
		}
		return "", fmt.Errorf("read response: %w", err)
	}
	if len(respData) > maxChatResponseBytes {
		if closeCallAt != nil {
			f := closeCallAt
			closeCallAt = nil
			f("decode_error", networkFinished)
		} else {
			finish("decode_error")
		}
		return "", fmt.Errorf("response exceeds %d-byte limit", maxChatResponseBytes)
	}

	if resp.StatusCode != 200 {
		if closeCallAt != nil {
			f := closeCallAt
			closeCallAt = nil
			f("provider_error", networkFinished)
		} else {
			finish("provider_error")
		}
		return "", fmt.Errorf("api error %d", resp.StatusCode)
	}

	var cr chatResponse
	if err := json.Unmarshal(respData, &cr); err != nil {
		if closeCallAt != nil {
			f := closeCallAt
			closeCallAt = nil
			f("decode_error", networkFinished)
		} else {
			finish("decode_error")
		}
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	if len(cr.Choices) == 0 {
		if closeCallAt != nil {
			f := closeCallAt
			closeCallAt = nil
			f("decode_error", networkFinished)
		} else {
			finish("decode_error")
		}
		return "", fmt.Errorf("no choices in response")
	}
	if closeCallAt != nil {
		f := closeCallAt
		closeCallAt = nil
		f("success", networkFinished)
	} else {
		finish("success")
	}

	return cr.Choices[0].Message.Content, nil
}

func callOutcome(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "provider_error"
}
