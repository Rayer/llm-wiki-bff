package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client calls the DeepSeek API (OpenAI-compatible endpoint).
type Client struct {
	apiKey      string
	baseURL     string
	client      *http.Client
	model       string
	temperature *float64
}

const maxChatResponseBytes = 64 * 1024

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type ClientOptions struct {
	Model       string
	Temperature *float64
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
	return &Client{
		apiKey:      apiKey,
		baseURL:     "https://api.deepseek.com",
		client:      &http.Client{Timeout: 60 * time.Second},
		model:       options.Model,
		temperature: options.Temperature,
	}
}

// Chat sends a system + user message and returns the assistant's reply.
func (c *Client) Chat(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	body := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMessage},
		},
		Temperature: c.temperature,
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

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, maxChatResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if len(respData) > maxChatResponseBytes {
		return "", fmt.Errorf("response exceeds %d-byte limit", maxChatResponseBytes)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("api error %d", resp.StatusCode)
	}

	var cr chatResponse
	if err := json.Unmarshal(respData, &cr); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return cr.Choices[0].Message.Content, nil
}
