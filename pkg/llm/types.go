// SPDX-License-Identifier: Apache-2.0
// Package llm provides a universal OpenAI-compatible wire client and decision gate
// for local (Ollama, LM Studio, vLLM) and cloud (Gemini, OpenAI, Anthropic proxies) LLMs.

package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Default client configuration constants.
const (
	GoogleAIStudioBaseURL   = "https://generativelanguage.googleapis.com/v1beta/openai/"
	DefaultBaseURL          = "http://localhost:11434/v1"
	DefaultModel            = "gemini-3.5-flash-lite"
	DefaultFallbackAPIKey   = "AQ.ExampleDemoKey000000000000000000000000000000"
	DefaultTimeout          = 60 * time.Second
	DefaultMaxRetries       = 3
	DefaultRetryInitialWait = 500 * time.Millisecond
	DefaultRetryMaxWait     = 8 * time.Second
	DefaultTemperature      = 0.2

	// Gemini 3.5+ Generation Models (Official Google AI Studio / Gemini API IDs)
	ModelGemini38Flash     = "gemini-3.8-flash"      // Latest GA flagship workhorse for coding agents and reasoning
	ModelGemini38Live      = "gemini-3.8-live"       // Real-time dialogue and voice/text interaction
	ModelGemini37Flash     = "gemini-3.7-flash"      // Multi-step agentic execution & coding
	ModelGemini36Flash     = "gemini-3.6-flash"      // Speed and intelligence balance
	ModelGemini35Flash     = "gemini-3.5-flash"      // Flagship 3.5 coding & action model
	ModelGemini35FlashLite = "gemini-3.5-flash-lite" // High-throughput, cost-effective baseline

	LatestFlagshipModel = ModelGemini38Flash

	// Constants for 1D Geodesic Ribbon Mode.
	DomainRibbon = 61
	FormatRibbon = "ribbon"
)

// IsGeminiModel reports whether the given model name indicates a Google Gemini model.
func IsGeminiModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "gemini")
}

// IsGeminiKey reports whether the given API key matches standard Gemini / Google AI Studio patterns.
func IsGeminiKey(apiKey string) bool {
	k := strings.TrimSpace(apiKey)
	if k == "" {
		return false
	}
	return strings.HasPrefix(k, "AQ.") || strings.HasPrefix(k, "AIza") || k == DefaultFallbackAPIKey
}


// Standard typed sentinel errors for classification.
var (
	ErrUnauthorized   = errors.New("llm: unauthorized (HTTP 401)")
	ErrForbidden      = errors.New("llm: forbidden (HTTP 403)")
	ErrNotFound       = errors.New("llm: endpoint or model not found (HTTP 404)")
	ErrRateLimit      = errors.New("llm: rate limit exceeded (HTTP 429)")
	ErrServerError    = errors.New("llm: server error (HTTP 5xx)")
	ErrInvalidRequest = errors.New("llm: invalid request (HTTP 400)")
	ErrEmptyResponse  = errors.New("llm: empty choices in completion response")
)

// ClientConfig holds configuration for the universal LLM wire client.
type ClientConfig struct {
	BaseURL          string        `json:"base_url"`
	APIKey           string        `json:"api_key,omitempty"`
	Model            string        `json:"model"`
	Timeout          time.Duration `json:"timeout"`
	MaxRetries       int           `json:"max_retries"`
	RetryInitialWait time.Duration `json:"retry_initial_wait"`
	RetryMaxWait     time.Duration `json:"retry_max_wait"`
	Temperature      *float64      `json:"temperature,omitempty"`
	TopP             *float64      `json:"top_p,omitempty"`
	HTTPClient       *http.Client  `json:"-"`
}

// LLMClient abstracts the OpenAI-compatible chat completions interface.
type LLMClient interface {
	Complete(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error)
	Config() ClientConfig
}

// ChatMessage represents a single message in the chat completions conversation.
type ChatMessage struct {
	Role             string     `json:"role"`                        // "system", "user", "assistant", "tool"
	Content          string     `json:"content"`                     // Message text body
	ReasoningContent string     `json:"reasoning_content,omitempty"` // Thinking/reasoning output from reasoning models
	Name             string     `json:"name,omitempty"`              // Optional participant name
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`        // Optional tool invocations
	ToolCallID       string     `json:"tool_call_id,omitempty"`      // ID linking tool responses
}

// ToolCall represents a model-generated function/tool invocation.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function FunctionCallDesc `json:"function"`
}

// FunctionCallDesc describes the target function name and serialized JSON arguments.
type FunctionCallDesc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ResponseFormat specifies the desired output structure (e.g. JSON object or schema).
type ResponseFormat struct {
	Type       string            `json:"type"` // "text", "json_object", "json_schema"
	JSONSchema *JSONSchemaConfig `json:"json_schema,omitempty"`
}

// JSONSchemaConfig specifies structured output validation schema.
type JSONSchemaConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Strict      bool   `json:"strict,omitempty"`
	Schema      any    `json:"schema"`
}

// ChatCompletionRequest encapsulates the payload sent to /v1/chat/completions.
type ChatCompletionRequest struct {
	Model          string          `json:"model,omitempty"`
	Messages       []ChatMessage   `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat  *ResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Seed            *int            `json:"seed,omitempty"`
	N               int             `json:"n,omitempty"`
}

// Validate checks request invariants.
func (r *ChatCompletionRequest) Validate() error {
	if len(r.Messages) == 0 {
		return errors.New("llm: messages cannot be empty")
	}
	if r.Temperature != nil && (*r.Temperature < 0.0 || *r.Temperature > 2.0) {
		return fmt.Errorf("llm: temperature %f out of range [0.0, 2.0]", *r.Temperature)
	}
	if r.TopP != nil && (*r.TopP < 0.0 || *r.TopP > 1.0) {
		return fmt.Errorf("llm: top_p %f out of range [0.0, 1.0]", *r.TopP)
	}
	return nil
}

// Choice represents a single generated chat completion choice.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"` // "stop", "length", "tool_calls", etc.
}

// Usage captures token accounting metrics.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionResponse models the standard OpenAI chat completions response.
type ChatCompletionResponse struct {
	ID                string    `json:"id"`
	Object            string    `json:"object"`
	Created           int64     `json:"created"`
	Model             string    `json:"model"`
	SystemFingerprint string    `json:"system_fingerprint,omitempty"`
	Choices           []Choice  `json:"choices"`
	Usage             Usage     `json:"usage"`
	Error             *APIError `json:"error,omitempty"`
}

// Content returns the text content of the first choice, or reasoning content if content is empty.
func (r *ChatCompletionResponse) Content() string {
	if r == nil || len(r.Choices) == 0 {
		return ""
	}
	if strings.TrimSpace(r.Choices[0].Message.Content) != "" {
		return r.Choices[0].Message.Content
	}
	return r.Choices[0].Message.ReasoningContent
}

// FinishReason returns the finish reason of the first choice, or empty string if none.
func (r *ChatCompletionResponse) FinishReason() string {
	if r == nil || len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].FinishReason
}

// APIError represents an upstream OpenAI-compatible error object.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    any    `json:"code,omitempty"`
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Type != "" {
		return fmt.Sprintf("%s: %s", e.Type, e.Message)
	}
	return e.Message
}

// StatusError encapsulates HTTP error responses with status codes and raw bodies.
type StatusError struct {
	StatusCode int
	Status     string
	APIError   *APIError
	RawBody    []byte
	Err        error
}

func (e *StatusError) Error() string {
	if e.APIError != nil && e.APIError.Message != "" {
		return fmt.Sprintf("llm: HTTP %d (%s): %s", e.StatusCode, e.Status, e.APIError.Message)
	}
	if len(e.RawBody) > 0 {
		return fmt.Sprintf("llm: HTTP %d (%s): %s", e.StatusCode, e.Status, string(e.RawBody))
	}
	return fmt.Sprintf("llm: HTTP %d (%s)", e.StatusCode, e.Status)
}

func (e *StatusError) Unwrap() error {
	return e.Err
}
