package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type modelFixtureEntry struct {
	ID          string   `json:"id"`
	Provider    string   `json:"provider"`
	BaseURL     string   `json:"base_url"`
	Model       string   `json:"model"`
	APIKey      string   `json:"-"`
	Temperature *float64 `json:"temperature,omitempty"`
	Reasoning   string   `json:"reasoning,omitempty"`
}

type profileFixtureEntry struct {
	ID                   string   `json:"id"`
	RequiredWhenExplicit []string `json:"required_when_explicit"`
	PreferredByDefault   []string `json:"preferred_by_default"`
	GoalsToExpand        []string `json:"goals_to_expand"`
}

type promptFixtureEntry struct {
	ID             string `json:"id"`
	SystemTemplate string `json:"system_template"`
	UserTemplate   string `json:"user_template"`
}

type fixtureVariant struct {
	Profile   profileFixtureEntry
	Prompt    promptFixtureEntry
	Model     modelFixtureEntry
	VariantID string
}

type renderedFixturePrompt struct {
	System string
	User   string
}

type fixtureUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type fixtureModelCall struct {
	Content     string
	RawResponse string
	Usage       fixtureUsage
	LatencyMS   int64
}

type publicModelFixture struct {
	ID          string   `json:"id"`
	Provider    string   `json:"provider"`
	BaseURL     string   `json:"base_url"`
	Model       string   `json:"model"`
	Temperature *float64 `json:"temperature,omitempty"`
	Reasoning   string   `json:"reasoning,omitempty"`
}

func marshalPublicJSON(model modelFixtureEntry) string {
	data, _ := json.Marshal(publicModelFixture{ID: model.ID, Provider: model.Provider, BaseURL: model.BaseURL, Model: model.Model, Temperature: model.Temperature, Reasoning: model.Reasoning})
	return string(data)
}

func renderFixturePrompt(prompt promptFixtureEntry, rawQuery string, policy CriterionPolicy) (renderedFixturePrompt, error) {
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return renderedFixturePrompt{}, err
	}
	replace := func(template string) (string, error) {
		result := strings.ReplaceAll(template, "{{raw_query}}", rawQuery)
		result = strings.ReplaceAll(result, "{{criterion_policy}}", string(policyJSON))
		if strings.Contains(result, "{{") || strings.Contains(result, "}}") {
			return "", errors.New("unsupported prompt placeholder")
		}
		return result, nil
	}
	system, err := replace(prompt.SystemTemplate)
	if err != nil {
		return renderedFixturePrompt{}, err
	}
	user, err := replace(prompt.UserTemplate)
	if err != nil {
		return renderedFixturePrompt{}, err
	}
	return renderedFixturePrompt{System: system, User: user}, nil
}

func callFixtureModel(ctx context.Context, model modelFixtureEntry, system, user string) (fixtureModelCall, error) {
	body := map[string]any{
		"model":    model.Model,
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	}
	if model.Temperature != nil {
		body["temperature"] = *model.Temperature
	}
	if model.Reasoning != "" {
		body["reasoning_effort"] = model.Reasoning
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fixtureModelCall{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(model.BaseURL, "/")+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return fixtureModelCall{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if model.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+model.APIKey)
	}
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	latency := time.Since(started).Milliseconds()
	if latency < 0 {
		latency = 0
	}
	if err != nil {
		return fixtureModelCall{LatencyMS: latency}, fmt.Errorf("model request failed: %w", err)
	}
	defer response.Body.Close()
	responseBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return fixtureModelCall{LatencyMS: latency}, fmt.Errorf("read model response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failed struct {
			Usage fixtureUsage `json:"usage"`
		}
		_ = json.Unmarshal(responseBytes, &failed)
		return fixtureModelCall{LatencyMS: latency, RawResponse: string(responseBytes), Usage: failed.Usage}, fmt.Errorf("model request returned HTTP %d", response.StatusCode)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage fixtureUsage `json:"usage"`
	}
	if err := json.Unmarshal(responseBytes, &decoded); err != nil {
		return fixtureModelCall{LatencyMS: latency, RawResponse: string(responseBytes)}, errors.New("decode model response")
	}
	if len(decoded.Choices) == 0 || decoded.Choices[0].Message.Content == "" {
		return fixtureModelCall{LatencyMS: latency, RawResponse: string(responseBytes), Usage: decoded.Usage}, errors.New("model response has no content")
	}
	return fixtureModelCall{Content: decoded.Choices[0].Message.Content, RawResponse: string(responseBytes), Usage: decoded.Usage, LatencyMS: latency}, nil
}

func readModelFixture(path string) ([]modelFixtureEntry, error) {
	var envelope struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := decodeFixtureFile(path, map[string]json.RawMessage{"models": nil}, &envelope); err != nil {
		return nil, fmt.Errorf("models fixture: %w", err)
	}
	if len(envelope.Models) == 0 {
		return nil, errors.New("models fixture must contain at least one model")
	}
	models := make([]modelFixtureEntry, 0, len(envelope.Models))
	seen := map[string]struct{}{}
	for i, raw := range envelope.Models {
		model, err := decodeModel(raw)
		if err != nil {
			return nil, fmt.Errorf("model %d: %w", i+1, err)
		}
		if _, ok := seen[model.ID]; ok {
			return nil, fmt.Errorf("duplicate model id %q", model.ID)
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	return models, nil
}

func readProfileFixture(path string) ([]profileFixtureEntry, error) {
	var envelope struct {
		Profiles []json.RawMessage `json:"profiles"`
	}
	if err := decodeFixtureFile(path, map[string]json.RawMessage{"profiles": nil}, &envelope); err != nil {
		return nil, fmt.Errorf("profiles fixture: %w", err)
	}
	if len(envelope.Profiles) == 0 {
		return nil, errors.New("profiles fixture must contain at least one profile")
	}
	profiles := make([]profileFixtureEntry, 0, len(envelope.Profiles))
	seen := map[string]struct{}{}
	for i, raw := range envelope.Profiles {
		profile, err := decodeProfile(raw)
		if err != nil {
			return nil, fmt.Errorf("profile %d: %w", i+1, err)
		}
		if _, ok := seen[profile.ID]; ok {
			return nil, fmt.Errorf("duplicate profile id %q", profile.ID)
		}
		seen[profile.ID] = struct{}{}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

func readPromptFixture(path string) ([]promptFixtureEntry, error) {
	var envelope struct {
		Prompts []json.RawMessage `json:"prompts"`
	}
	if err := decodeFixtureFile(path, map[string]json.RawMessage{"prompts": nil}, &envelope); err != nil {
		return nil, fmt.Errorf("prompts fixture: %w", err)
	}
	if len(envelope.Prompts) == 0 {
		return nil, errors.New("prompts fixture must contain at least one prompt")
	}
	prompts := make([]promptFixtureEntry, 0, len(envelope.Prompts))
	seen := map[string]struct{}{}
	for i, raw := range envelope.Prompts {
		prompt, err := decodePrompt(raw)
		if err != nil {
			return nil, fmt.Errorf("prompt %d: %w", i+1, err)
		}
		if _, ok := seen[prompt.ID]; ok {
			return nil, fmt.Errorf("duplicate prompt id %q", prompt.ID)
		}
		seen[prompt.ID] = struct{}{}
		prompts = append(prompts, prompt)
	}
	return prompts, nil
}

func decodeFixtureFile(path string, allowed map[string]json.RawMessage, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("fixture must be a regular file")
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	object, err := strictObject(data)
	if err != nil {
		return err
	}
	for key := range object {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	for key := range allowed {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("field %q is required", key)
		}
	}
	canonical, _ := json.Marshal(object)
	return json.Unmarshal(canonical, target)
}

func strictObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("fixture must be a JSON object")
	}
	object := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid field name: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("field name must be a string")
		}
		if _, ok := object[key]; ok {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		object[key] = raw
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid fixture object")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeModel(raw json.RawMessage) (modelFixtureEntry, error) {
	object, err := strictObject(raw)
	if err != nil {
		return modelFixtureEntry{}, err
	}
	for key := range object {
		switch key {
		case "id", "provider", "base_url", "model", "api_key", "temperature", "reasoning":
		default:
			return modelFixtureEntry{}, fmt.Errorf("unknown field %q", key)
		}
	}
	for _, key := range []string{"id", "provider", "base_url", "model", "api_key"} {
		if _, ok := object[key]; !ok {
			return modelFixtureEntry{}, fmt.Errorf("field %q is required", key)
		}
	}
	var model modelFixtureEntry
	if err := unmarshalString(object, "id", &model.ID); err != nil {
		return modelFixtureEntry{}, err
	}
	if err := unmarshalString(object, "provider", &model.Provider); err != nil {
		return modelFixtureEntry{}, err
	}
	if err := unmarshalString(object, "base_url", &model.BaseURL); err != nil {
		return modelFixtureEntry{}, err
	}
	if err := unmarshalString(object, "model", &model.Model); err != nil {
		return modelFixtureEntry{}, err
	}
	if err := unmarshalString(object, "api_key", &model.APIKey); err != nil {
		return modelFixtureEntry{}, err
	}
	if raw, ok := object["temperature"]; ok {
		if isJSONNull(raw) {
			return modelFixtureEntry{}, errors.New("temperature must not be null")
		}
		if err := json.Unmarshal(raw, &model.Temperature); err != nil {
			return modelFixtureEntry{}, fmt.Errorf("temperature: %w", err)
		}
	}
	if raw, ok := object["reasoning"]; ok {
		if isJSONNull(raw) {
			return modelFixtureEntry{}, errors.New("reasoning must not be null")
		}
		if err := json.Unmarshal(raw, &model.Reasoning); err != nil {
			return modelFixtureEntry{}, fmt.Errorf("reasoning: %w", err)
		}
	}
	if err := validateFixtureID(model.ID); err != nil {
		return modelFixtureEntry{}, fmt.Errorf("id: %w", err)
	}
	if strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" {
		return modelFixtureEntry{}, errors.New("provider and model must not be empty")
	}
	parsed, err := url.Parse(model.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return modelFixtureEntry{}, errors.New("base_url must be an http(s) URL")
	}
	model.BaseURL = strings.TrimRight(model.BaseURL, "/")
	return model, nil
}

func decodeProfile(raw json.RawMessage) (profileFixtureEntry, error) {
	object, err := strictObject(raw)
	if err != nil {
		return profileFixtureEntry{}, err
	}
	allowed := map[string]bool{"id": true, "required_when_explicit": true, "preferred_by_default": true, "goals_to_expand": true}
	for key := range object {
		if !allowed[key] {
			return profileFixtureEntry{}, fmt.Errorf("unknown field %q", key)
		}
	}
	var profile profileFixtureEntry
	if err := unmarshalString(object, "id", &profile.ID); err != nil {
		return profileFixtureEntry{}, err
	}
	for key, target := range map[string]*[]string{"required_when_explicit": &profile.RequiredWhenExplicit, "preferred_by_default": &profile.PreferredByDefault, "goals_to_expand": &profile.GoalsToExpand} {
		if raw, ok := object[key]; !ok {
			return profileFixtureEntry{}, fmt.Errorf("field %q is required", key)
		} else if isJSONNull(raw) {
			return profileFixtureEntry{}, fmt.Errorf("%s must not be null", key)
		} else if err := json.Unmarshal(raw, target); err != nil {
			return profileFixtureEntry{}, fmt.Errorf("%s: %w", key, err)
		}
		for _, value := range *target {
			if strings.TrimSpace(value) == "" {
				return profileFixtureEntry{}, fmt.Errorf("%s must not contain empty values", key)
			}
		}
	}
	if err := validateFixtureID(profile.ID); err != nil {
		return profileFixtureEntry{}, fmt.Errorf("id: %w", err)
	}
	return profile, nil
}

func decodePrompt(raw json.RawMessage) (promptFixtureEntry, error) {
	object, err := strictObject(raw)
	if err != nil {
		return promptFixtureEntry{}, err
	}
	allowed := map[string]bool{"id": true, "system_template": true, "user_template": true}
	for key := range object {
		if !allowed[key] {
			return promptFixtureEntry{}, fmt.Errorf("unknown field %q", key)
		}
	}
	var prompt promptFixtureEntry
	for key, target := range map[string]*string{"id": &prompt.ID, "system_template": &prompt.SystemTemplate, "user_template": &prompt.UserTemplate} {
		if err := unmarshalString(object, key, target); err != nil {
			return promptFixtureEntry{}, err
		}
	}
	if err := validateFixtureID(prompt.ID); err != nil {
		return promptFixtureEntry{}, fmt.Errorf("id: %w", err)
	}
	if strings.TrimSpace(prompt.SystemTemplate) == "" || strings.TrimSpace(prompt.UserTemplate) == "" {
		return promptFixtureEntry{}, errors.New("prompt templates must not be empty")
	}
	return prompt, nil
}

func unmarshalString(object map[string]json.RawMessage, key string, target *string) error {
	raw, ok := object[key]
	if !ok {
		return fmt.Errorf("field %q is required", key)
	}
	if isJSONNull(raw) {
		return fmt.Errorf("field %q must not be null", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validateFixtureID(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return errors.New("must contain only letters, digits, '.', '_' or '-'")
		}
	}
	return nil
}

func selectFixtureMatrix(models []modelFixtureEntry, profiles []profileFixtureEntry, prompts []promptFixtureEntry, modelIDs, profileIDs, promptIDs string) ([]fixtureVariant, error) {
	selectedModels, err := selectFixtureIDs(models, modelIDs, func(value modelFixtureEntry) string { return value.ID })
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	selectedProfiles, err := selectFixtureIDs(profiles, profileIDs, func(value profileFixtureEntry) string { return value.ID })
	if err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	selectedPrompts, err := selectFixtureIDs(prompts, promptIDs, func(value promptFixtureEntry) string { return value.ID })
	if err != nil {
		return nil, fmt.Errorf("prompts: %w", err)
	}
	variants := make([]fixtureVariant, 0, len(selectedModels)*len(selectedProfiles)*len(selectedPrompts))
	for _, profile := range selectedProfiles {
		for _, prompt := range selectedPrompts {
			for _, model := range selectedModels {
				variants = append(variants, fixtureVariant{Profile: profile, Prompt: prompt, Model: model, VariantID: "p=" + profile.ID + "__pr=" + prompt.ID + "__m=" + model.ID})
			}
		}
	}
	return variants, nil
}

func selectFixtureIDs[T any](values []T, selector string, id func(T) string) ([]T, error) {
	byID := make(map[string]T, len(values))
	for _, value := range values {
		byID[id(value)] = value
	}
	parts := []string{}
	if strings.TrimSpace(selector) == "" {
		for _, value := range values {
			parts = append(parts, id(value))
		}
	} else {
		for _, value := range strings.Split(selector, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, errors.New("selector contains an empty id")
			}
			parts = append(parts, value)
		}
	}
	seen := map[string]struct{}{}
	result := make([]T, 0, len(parts))
	for _, part := range parts {
		if _, ok := seen[part]; ok {
			return nil, fmt.Errorf("duplicate id %q", part)
		}
		value, ok := byID[part]
		if !ok {
			return nil, fmt.Errorf("unknown id %q", part)
		}
		seen[part] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}
