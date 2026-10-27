// SPDX-License-Identifier: Apache-2.0
// Package llm_test tests Google AI Studio integration, Gemini base URL normalization,
// API key resolution precedence, and wire header injection.

package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGemini_ConstantsAndHelpers(t *testing.T) {
	t.Parallel()

	if llm.DefaultModel != "gemini-3.5-flash-lite" {
		t.Errorf("expected DefaultModel to be 'gemini-3.5-flash-lite', got %q", llm.DefaultModel)
	}

	if llm.GoogleAIStudioBaseURL != "https://generativelanguage.googleapis.com/v1beta/openai/" {
		t.Errorf("unexpected GoogleAIStudioBaseURL: %q", llm.GoogleAIStudioBaseURL)
	}

	if llm.DefaultFallbackAPIKey != "AQ.ExampleDemoKey000000000000000000000000000000" {
		t.Errorf("unexpected DefaultFallbackAPIKey: %q", llm.DefaultFallbackAPIKey)
	}

	modelCases := []struct {
		model    string
		isGemini bool
	}{
		{"gemini-3.8-flash", true},
		{"gemini-3.8-live", true},
		{"gemini-3.7-flash", true},
		{"gemini-3.6-flash", true},
		{"gemini-3.5-flash", true},
		{"gemini-3.5-flash-lite", true},
		{"gemini-2.5-flash", true},
		{"google/gemini-pro", true},
		{"models/gemini-1.5-pro", true},
		{"GEMINI-3.5-FLASH-LITE", true},
		{"GEMINI-3.8-FLASH", true},
		{"gpt-4o-mini", false},
		{"claude-3-5-sonnet", false},
		{"qwen2.5-coder:7b", false},
		{"", false},
	}

	for _, tc := range modelCases {
		res := llm.IsGeminiModel(tc.model)
		if res != tc.isGemini {
			t.Errorf("IsGeminiModel(%q) = %v; expected %v", tc.model, res, tc.isGemini)
		}
	}

	keyCases := []struct {
		key      string
		isGemini bool
	}{
		{llm.DefaultFallbackAPIKey, true},
		{"AQ.testkey123456", true},
		{"AIzaSyTestKeyABCDEF", true},
		{"sk-proj-openai-key", false},
		{"bearer-token", false},
		{"", false},
	}

	for _, tc := range keyCases {
		res := llm.IsGeminiKey(tc.key)
		if res != tc.isGemini {
			t.Errorf("IsGeminiKey(%q) = %v; expected %v", tc.key, res, tc.isGemini)
		}
	}
}

func TestGemini_BaseURLNormalization_And_DefaultClient(t *testing.T) {
	t.Parallel()

	// Default initialization with zero config
	client, err := llm.NewClient(llm.ClientConfig{})
	if err != nil {
		t.Fatalf("unexpected NewClient error: %v", err)
	}

	cfg := client.Config()
	if cfg.Model != "gemini-3.5-flash-lite" {
		t.Errorf("expected default model 'gemini-3.5-flash-lite', got %q", cfg.Model)
	}
	if cfg.BaseURL != llm.GoogleAIStudioBaseURL {
		t.Errorf("expected BaseURL to normalize to %q, got %q", llm.GoogleAIStudioBaseURL, cfg.BaseURL)
	}
	expectedEndpoint := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	if client.Endpoint() != expectedEndpoint {
		t.Errorf("expected endpoint %q, got %q", expectedEndpoint, client.Endpoint())
	}
	if cfg.APIKey != llm.DefaultFallbackAPIKey {
		t.Errorf("expected default fallback API key %q, got %q", llm.DefaultFallbackAPIKey, cfg.APIKey)
	}

	// Initialization with Gemini model specified explicitly
	clientWithModel, err := llm.NewClient(llm.ClientConfig{
		Model: "gemini-2.5-flash",
	})
	if err != nil {
		t.Fatalf("unexpected NewClient error: %v", err)
	}
	if clientWithModel.Endpoint() != expectedEndpoint {
		t.Errorf("expected endpoint %q, got %q", expectedEndpoint, clientWithModel.Endpoint())
	}

	// Initialization with Gemini API key specified explicitly
	clientWithKey, err := llm.NewClient(llm.ClientConfig{
		APIKey: "AQ.custom-gemini-key",
	})
	if err != nil {
		t.Fatalf("unexpected NewClient error: %v", err)
	}
	if clientWithKey.Endpoint() != expectedEndpoint {
		t.Errorf("expected endpoint %q, got %q", expectedEndpoint, clientWithKey.Endpoint())
	}
	if clientWithKey.Config().APIKey != "AQ.custom-gemini-key" {
		t.Errorf("expected custom API key to be preserved, got %q", clientWithKey.Config().APIKey)
	}
}

func TestGemini_EndpointNormalization_Variants(t *testing.T) {
	t.Parallel()

	expectedGemini := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

	cases := []struct {
		name     string
		baseURL  string
		expected string
	}{
		{
			name:     "Google AI Studio Host Only",
			baseURL:  "https://generativelanguage.googleapis.com",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio Host Trailing Slash",
			baseURL:  "https://generativelanguage.googleapis.com/",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio v1beta",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio v1beta Trailing Slash",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio v1beta openai",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/openai",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio v1beta openai Trailing Slash",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/openai/",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio v1beta openai chat completions",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			expected: expectedGemini,
		},
		{
			name:     "Google AI Studio v1 Path",
			baseURL:  "https://generativelanguage.googleapis.com/v1",
			expected: expectedGemini,
		},
		{
			name:     "Empty BaseURL",
			baseURL:  "",
			expected: expectedGemini,
		},
		{
			name:     "Local Ollama Host",
			baseURL:  "http://127.0.0.1:11434",
			expected: "http://127.0.0.1:11434/v1/chat/completions",
		},
		{
			name:     "Local Ollama v1",
			baseURL:  "http://127.0.0.1:11434/v1",
			expected: "http://127.0.0.1:11434/v1/chat/completions",
		},
		{
			name:     "OpenAI Official API",
			baseURL:  "https://api.openai.com/v1",
			expected: "https://api.openai.com/v1/chat/completions",
		},
	}

	for _, tc := range cases {
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: tc.baseURL,
			Model:   "gemini-3.5-flash-lite",
		})
		if err != nil {
			t.Errorf("[%s] NewClient failed: %v", tc.name, err)
			continue
		}
		if c.Endpoint() != tc.expected {
			t.Errorf("[%s] for base %q: expected %q, got %q", tc.name, tc.baseURL, tc.expected, c.Endpoint())
		}
	}
}

func TestResolveAPIKey_Precedence(t *testing.T) {
	// Not parallel because modifying env vars affects process
	fallback := llm.DefaultFallbackAPIKey

	// Case 1: No env vars, no explicit key -> fallback
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OMNICODER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	if key := llm.ResolveAPIKey(""); key != fallback {
		t.Errorf("expected fallback key %q, got %q", fallback, key)
	}

	// Case 2: OPENAI_API_KEY set -> OPENAI_API_KEY
	t.Setenv("OPENAI_API_KEY", "sk-openai-lowest-priority")
	if key := llm.ResolveAPIKey(""); key != "sk-openai-lowest-priority" {
		t.Errorf("expected 'sk-openai-lowest-priority', got %q", key)
	}

	// Case 3: OMNICODER_API_KEY overrides OPENAI_API_KEY
	t.Setenv("OMNICODER_API_KEY", "omnicoder-key-mid-priority")
	if key := llm.ResolveAPIKey(""); key != "omnicoder-key-mid-priority" {
		t.Errorf("expected 'omnicoder-key-mid-priority', got %q", key)
	}

	// Case 4: GEMINI_API_KEY overrides OMNICODER_API_KEY
	t.Setenv("GEMINI_API_KEY", "gemini-key-highest-env")
	if key := llm.ResolveAPIKey(""); key != "gemini-key-highest-env" {
		t.Errorf("expected 'gemini-key-highest-env', got %q", key)
	}

	// Case 5: Explicit key overrides all env vars
	if key := llm.ResolveAPIKey("explicit-flag-key"); key != "explicit-flag-key" {
		t.Errorf("expected 'explicit-flag-key', got %q", key)
	}

	// Case 6: Whitespace explicit key falls through to GEMINI_API_KEY
	if key := llm.ResolveAPIKey("   "); key != "gemini-key-highest-env" {
		t.Errorf("expected whitespace to fall through to env var, got %q", key)
	}
}

func TestGemini_Headers(t *testing.T) {
	t.Parallel()

	t.Run("GoogleAIStudioHeaders_IncludeBothBearerAndGoogKey", func(t *testing.T) {
		var interceptedAuth string
		var interceptedGoogKey string

		customClient := &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				interceptedAuth = r.Header.Get("Authorization")
				interceptedGoogKey = r.Header.Get("x-goog-api-key")
				body := `{"id":"test-gemini","choices":[{"message":{"content":"{\"result\":\"ok\"}"}}]}`
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}),
		}

		client, err := llm.NewClient(llm.ClientConfig{
			HTTPClient: customClient,
			APIKey:     "test-gemini-key",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "hello"}},
		})
		if err != nil {
			t.Fatalf("Complete error: %v", err)
		}
		if resp.Content() != "{\"result\":\"ok\"}" {
			t.Errorf("unexpected content: %s", resp.Content())
		}

		if interceptedAuth != "Bearer test-gemini-key" {
			t.Errorf("expected 'Bearer test-gemini-key', got %q", interceptedAuth)
		}
		if interceptedGoogKey != "test-gemini-key" {
			t.Errorf("expected 'test-gemini-key' for x-goog-api-key, got %q", interceptedGoogKey)
		}
	})

	t.Run("LocalOllamaServer_PreservesEmptyAuthHeader", func(t *testing.T) {
		var receivedAuth string
		var receivedGoogKey string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuth = r.Header.Get("Authorization")
			receivedGoogKey = r.Header.Get("x-goog-api-key")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "{}"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL: ts.URL,
			APIKey:  "", // No key provided for local server
			Model:   "gemini-3.5-flash-lite",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
		})
		if err != nil {
			t.Fatalf("Complete error: %v", err)
		}

		if receivedAuth != "" {
			t.Errorf("expected empty Authorization header on local server, got %q", receivedAuth)
		}
		if receivedGoogKey != "" {
			t.Errorf("expected empty x-goog-api-key header on local server, got %q", receivedGoogKey)
		}
	})
}

func TestGemini_ResolveModel(t *testing.T) {
	// Case 1: Explicit model takes highest priority
	if m := llm.ResolveModel("gemini-3.8-flash"); m != "gemini-3.8-flash" {
		t.Errorf("expected explicit 'gemini-3.8-flash', got %q", m)
	}

	// Case 2: GEMINI_MODEL env var
	t.Setenv("GEMINI_MODEL", "gemini-3.8-live")
	t.Setenv("OMNICODER_MODEL", "gemini-3.7-flash")
	t.Setenv("OPENAI_MODEL", "gemini-3.5-flash")
	if m := llm.ResolveModel(""); m != "gemini-3.8-live" {
		t.Errorf("expected GEMINI_MODEL override 'gemini-3.8-live', got %q", m)
	}

	// Case 3: OMNICODER_MODEL env var
	t.Setenv("GEMINI_MODEL", "")
	if m := llm.ResolveModel(""); m != "gemini-3.7-flash" {
		t.Errorf("expected OMNICODER_MODEL override 'gemini-3.7-flash', got %q", m)
	}

	// Case 4: OPENAI_MODEL env var
	t.Setenv("OMNICODER_MODEL", "")
	if m := llm.ResolveModel(""); m != "gemini-3.5-flash" {
		t.Errorf("expected OPENAI_MODEL override 'gemini-3.5-flash', got %q", m)
	}

	// Case 5: Default fallback
	t.Setenv("OPENAI_MODEL", "")
	if m := llm.ResolveModel(""); m != llm.DefaultModel {
		t.Errorf("expected DefaultModel %q, got %q", llm.DefaultModel, m)
	}
}

func TestGemini_ReasoningEffortSanitized(t *testing.T) {
	var capturedBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
			ID: "test-resp",
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: `{"ok":true}`}},
			},
		})
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: ts.URL,
		Model:   "gemini-3.8-flash",
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Case 1: ReasoningEffort is "none" -> must be stripped
	_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Model:           "gemini-3.8-flash",
		Messages:        []llm.ChatMessage{{Role: "user", Content: "ping"}},
		ReasoningEffort: "none",
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if _, exists := capturedBody["reasoning_effort"]; exists {
		t.Errorf("expected 'reasoning_effort' to be stripped from JSON payload, found: %v", capturedBody["reasoning_effort"])
	}

	// Case 2: ReasoningEffort set on Gemini model -> must be stripped
	_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Model:           "gemini-3.5-flash-lite",
		Messages:        []llm.ChatMessage{{Role: "user", Content: "ping"}},
		ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if _, exists := capturedBody["reasoning_effort"]; exists {
		t.Errorf("expected 'reasoning_effort' to be stripped for Gemini model, found: %v", capturedBody["reasoning_effort"])
	}
}

