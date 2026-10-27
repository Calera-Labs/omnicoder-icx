// SPDX-License-Identifier: Apache-2.0
// Package llm_test implements the comprehensive test suite for OmniCoder-ICX llm package:
// wire client httptest suite, token budget verification (<300 tokens), markdown JSON extraction
// resilience, mock decision gate lifecycle, and race condition hunting.

package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// 1. Wire Client Tests against httptest.Server
// ============================================================================

func TestClient_SuccessRoundtrip(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("expected /v1/chat/completions, got %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %s", r.Header.Get("Authorization"))
		}

		resp := llm.ChatCompletionResponse{
			ID:      "cmpl-test-123",
			Object:  "chat.completion",
			Created: 1726000000,
			Model:   "gpt-4o-mini",
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: `{"operation":"MODIFY","target_files":["pkg/llm/cards.go"]}`,
					},
					FinishReason: "stop",
				},
			},
			Usage: llm.Usage{
				PromptTokens:     120,
				CompletionTokens: 30,
				TotalTokens:      150,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: ts.URL,
		APIKey:  "test-key",
		Model:   "gpt-4o-mini",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected NewClient error: %v", err)
	}

	req := &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{
			{Role: "user", Content: "Plan the gate"},
		},
	}

	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected Complete error: %v", err)
	}

	if resp.ID != "cmpl-test-123" {
		t.Errorf("expected ID cmpl-test-123, got %s", resp.ID)
	}
	if resp.Content() != `{"operation":"MODIFY","target_files":["pkg/llm/cards.go"]}` {
		t.Errorf("unexpected content: %s", resp.Content())
	}
	if resp.FinishReason() != "stop" {
		t.Errorf("expected stop, got %s", resp.FinishReason())
	}
	if resp.Usage.TotalTokens != 150 {
		t.Errorf("expected 150 tokens, got %d", resp.Usage.TotalTokens)
	}
}

func TestClient_EndpointNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		baseURL  string
		expected string
	}{
		{"http://127.0.0.1:11434", "http://127.0.0.1:11434/v1/chat/completions"},
		{"http://127.0.0.1:11434/", "http://127.0.0.1:11434/v1/chat/completions"},
		{"http://127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1/chat/completions"},
		{"http://127.0.0.1:11434/v1/", "http://127.0.0.1:11434/v1/chat/completions"},
		{"http://127.0.0.1:11434/v1/chat/completions", "http://127.0.0.1:11434/v1/chat/completions"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"https://generativelanguage.googleapis.com/v1beta/openai/", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
	}

	for _, tc := range cases {
		c, err := llm.NewClient(llm.ClientConfig{BaseURL: tc.baseURL})
		if err != nil {
			t.Errorf("NewClient failed for %s: %v", tc.baseURL, err)
			continue
		}
		if c.Endpoint() != tc.expected {
			t.Errorf("for base %q: expected %q, got %q", tc.baseURL, tc.expected, c.Endpoint())
		}
	}
}

func TestClient_AuthHeaders(t *testing.T) {
	t.Parallel()

	t.Run("OmittedWhenEmpty", func(t *testing.T) {
		var receivedAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "{}"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL: ts.URL,
			APIKey:  "", // Local Ollama has no API key
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Complete error: %v", err)
		}

		if receivedAuth != "" {
			t.Errorf("expected empty Authorization header, got %q", receivedAuth)
		}
	})

	t.Run("PresentWhenSet", func(t *testing.T) {
		var receivedAuth string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "{}"}}},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL: ts.URL,
			APIKey:  "secret-token-xyz",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Complete error: %v", err)
		}

		if receivedAuth != "Bearer secret-token-xyz" {
			t.Errorf("expected 'Bearer secret-token-xyz', got %q", receivedAuth)
		}
	})
}

func TestClient_Retry_RateLimit_429(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := attempts.Add(1)
		if att == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
			return
		}
		if att == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
			Choices: []llm.Choice{{Message: llm.ChatMessage{Content: `{"status":"ok"}`}}},
		})
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		MaxRetries:       3,
		RetryInitialWait: 10 * time.Millisecond,
		RetryMaxWait:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "retry me"}},
	})
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if resp.Content() != `{"status":"ok"}` {
		t.Errorf("unexpected content: %s", resp.Content())
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 total requests, got %d", attempts.Load())
	}
}

func TestClient_RateLimit_Exhausted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	defer ts.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:          ts.URL,
		MaxRetries:       2,
		RetryInitialWait: 5 * time.Millisecond,
		RetryMaxWait:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "exhaust retries"}},
	})
	if err == nil {
		t.Fatalf("expected rate limit error, got nil")
	}
	if !errors.Is(err, llm.ErrRateLimit) {
		t.Errorf("expected ErrRateLimit, got: %v", err)
	}
	// Initial attempt + 2 retries = 3 total attempts
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestClient_FailFast_401_400_404(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		body       string
		targetErr  error
	}{
		{"Unauthorized_401", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, llm.ErrUnauthorized},
		{"Forbidden_403", http.StatusForbidden, `{"error":{"message":"model not permitted"}}`, llm.ErrForbidden},
		{"BadRequest_400", http.StatusBadRequest, `{"error":{"message":"malformed prompt"}}`, llm.ErrInvalidRequest},
		{"NotFound_404", http.StatusNotFound, `{"error":{"message":"model not found"}}`, llm.ErrNotFound},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			client, _ := llm.NewClient(llm.ClientConfig{
				BaseURL:    ts.URL,
				MaxRetries: 3,
			})

			_, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
				Messages: []llm.ChatMessage{{Role: "user", Content: "test"}},
			})
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, tc.targetErr) {
				t.Errorf("expected error wrapping %v, got %v", tc.targetErr, err)
			}
			if calls.Load() != 1 {
				t.Errorf("expected fail-fast with 1 request, got %d", calls.Load())
			}
		})
	}
}

func TestClient_ContextCancellation(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client, _ := llm.NewClient(llm.ClientConfig{BaseURL: ts.URL})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := client.Complete(ctx, &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "slow"}},
	})
	if err == nil {
		t.Fatalf("expected context error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected context cancellation, got %v", err)
	}
}

func TestClient_EmptyChoices(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
			Choices: []llm.Choice{},
		})
	}))
	defer ts.Close()

	client, _ := llm.NewClient(llm.ClientConfig{BaseURL: ts.URL})

	_, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "empty"}},
	})
	if !errors.Is(err, llm.ErrEmptyResponse) {
		t.Errorf("expected ErrEmptyResponse, got %v", err)
	}
}

func TestClient_RequestOverrides(t *testing.T) {
	t.Parallel()

	var receivedModel string
	var receivedTemp *float64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		receivedModel = req.Model
		receivedTemp = req.Temperature
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
			Choices: []llm.Choice{{Message: llm.ChatMessage{Content: "{}"}}},
		})
	}))
	defer ts.Close()

	clientDefaultTemp := 0.7
	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:     ts.URL,
		Model:       "default-model",
		Temperature: &clientDefaultTemp,
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	reqTemp := 0.1
	_, err = client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Model:       "override-model",
		Temperature: &reqTemp,
		Messages:    []llm.ChatMessage{{Role: "user", Content: "override"}},
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}

	if receivedModel != "override-model" {
		t.Errorf("expected override-model, got %s", receivedModel)
	}
	if receivedTemp == nil || *receivedTemp != 0.1 {
		t.Errorf("expected temp 0.1, got %v", receivedTemp)
	}
}

// ============================================================================
// 2. Token Budget Verification Tests (<300 Tokens)
// ============================================================================

func TestTokenBudgets_AllCards_Under300Tokens(t *testing.T) {
	t.Parallel()

	t.Run("GateIntent_TokenBudget", func(t *testing.T) {
		// Adversarial 10,000 char prompt + 20,000 char repo map
		card := llm.GateIntentCard{
			UserPrompt:     strings.Repeat("Implement concurrent LRU cache with TTL eviction. ", 200),
			RepoMapSummary: strings.Repeat("pkg/lru/lru.go: func New() *Cache\npkg/lru/lru_test.go: func TestLRU()\n", 300),
			FocusFiles:     []string{"pkg/lru/lru.go", "pkg/lru/lru_test.go"},
		}

		sys, user, err := card.FormatPrompt()
		if err != nil {
			t.Fatalf("unexpected formatting error: %v", err)
		}
		if err := llm.ValidateTokenBudget(sys, user, llm.MaxCardTokenBudget); err != nil {
			t.Fatalf("token budget exceeded: %v", err)
		}
		tokens := llm.EstimateTokens(sys + " " + user)
		if tokens >= 300 {
			t.Errorf("expected <300 tokens, got %d", tokens)
		}
	})

	t.Run("GateEdit_TokenBudget", func(t *testing.T) {
		// Adversarial 5,000 line context snippet
		var bigCode strings.Builder
		for i := 1; i <= 500; i++ {
			bigCode.WriteString(fmt.Sprintf("func Routine%d() { println(\"line %d\") }\n", i, i))
		}

		card := llm.GateEditCard{
			FilePath:        "pkg/lru/lru.go",
			Instruction:     "Add double-linked list node pool to avoid GC churn during eviction",
			ContextSnippet:  bigCode.String(),
			ASTNeighborhood: []string{"Node", "Cache", "Evict", "Get", "Put"},
		}

		sys, user, err := card.FormatPrompt()
		if err != nil {
			t.Fatalf("unexpected formatting error: %v", err)
		}
		if err := llm.ValidateTokenBudget(sys, user, llm.MaxCardTokenBudget); err != nil {
			t.Fatalf("token budget exceeded: %v", err)
		}
		tokens := llm.EstimateTokens(sys + " " + user)
		if tokens >= 300 {
			t.Errorf("expected <300 tokens, got %d", tokens)
		}
	})

	t.Run("GateHeal_TokenBudget", func(t *testing.T) {
		// Massive compiler error traceback
		var bigTrace strings.Builder
		for i := 1; i <= 200; i++ {
			bigTrace.WriteString(fmt.Sprintf("  %4d: errLine%d := CallUnknown()\n", i, i))
		}

		card := llm.GateHealCard{
			FilePath:      "pkg/lru/lru.go",
			ErrorLine:     42,
			ErrorMessage:  "undefined: Capacity in assignment c.cap = Capacity",
			Snippet5Lines: ">   42: c.cap = Capacity\n    43: c.mu.Unlock()\n",
			Command:       "go test -v -race ./pkg/lru",
			ErrorCode:     "compiler_error_12",
		}

		sys, user, err := card.FormatPrompt()
		if err != nil {
			t.Fatalf("unexpected formatting error: %v", err)
		}
		if err := llm.ValidateTokenBudget(sys, user, llm.MaxCardTokenBudget); err != nil {
			t.Fatalf("token budget exceeded: %v", err)
		}
		tokens := llm.EstimateTokens(sys + " " + user)
		if tokens >= 300 {
			t.Errorf("expected <300 tokens, got %d", tokens)
		}
	})

	t.Run("GateFinalize_TokenBudget", func(t *testing.T) {
		// Massive test failure summary
		card := llm.GateFinalizeCard{
			TaskDescription:   "Implement concurrent LRU cache with TTL eviction",
			FilesModified:     []string{"pkg/lru/lru.go", "pkg/lru/lru_test.go", "pkg/lru/ttl.go"},
			TestCommand:       "go test -v -race ./pkg/lru",
			TestExitCode:      0,
			TestOutputSummary: strings.Repeat("=== RUN TestConcurrentLRU\n--- PASS: TestConcurrentLRU (0.12s)\n", 100),
		}

		sys, user, err := card.FormatPrompt()
		if err != nil {
			t.Fatalf("unexpected formatting error: %v", err)
		}
		if err := llm.ValidateTokenBudget(sys, user, llm.MaxCardTokenBudget); err != nil {
			t.Fatalf("token budget exceeded: %v", err)
		}
		tokens := llm.EstimateTokens(sys + " " + user)
		if tokens >= 300 {
			t.Errorf("expected <300 tokens, got %d", tokens)
		}
	})
}

// ============================================================================
// 3. Resilience Tests: Extracting JSON from Messy Markdown
// ============================================================================

func TestJSONExtraction_Resilience(t *testing.T) {
	t.Parallel()

	type DummyPayload struct {
		Operation   string   `json:"operation"`
		TargetFiles []string `json:"target_files"`
		Code        string   `json:"code,omitempty"`
	}

	testCases := []struct {
		name        string
		rawText     string
		expectOp    string
		expectFiles int
		expectErr   bool
	}{
		{
			name:        "Clean_JSON",
			rawText:     `{"operation":"MODIFY","target_files":["pkg/lru/lru.go"]}`,
			expectOp:    "MODIFY",
			expectFiles: 1,
			expectErr:   false,
		},
		{
			name: "Markdown_Code_Fence_JSON",
			rawText: "```json\n" +
				`{"operation":"CREATE","target_files":["pkg/lru/lru.go","pkg/lru/lru_test.go"]}` +
				"\n```",
			expectOp:    "CREATE",
			expectFiles: 2,
			expectErr:   false,
		},
		{
			name: "Markdown_Code_Fence_NoTag",
			rawText: "```\n" +
				`{"operation":"REFACTOR","target_files":["pkg/devops/repomap.go"]}` +
				"\n```",
			expectOp:    "REFACTOR",
			expectFiles: 1,
			expectErr:   false,
		},
		{
			name: "Conversational_Leading_Text",
			rawText: "Sure! Here is the JSON decision for your task:\n\n" +
				`{"operation":"EXPLAIN","target_files":["README.md"]}` +
				"\n\nLet me know if you need anything else!",
			expectOp:    "EXPLAIN",
			expectFiles: 1,
			expectErr:   false,
		},
		{
			name: "DeepSeek_R1_Think_Block",
			rawText: "<think>\nThinking about files to edit...\nI choose lru.go\n</think>\n" +
				"```json\n" +
				`{"operation":"MODIFY","target_files":["pkg/lru/lru.go"]}` +
				"\n```",
			expectOp:    "MODIFY",
			expectFiles: 1,
			expectErr:   false,
		},
		{
			name: "Trailing_Commas_Sanitized",
			rawText: `{"operation":"MODIFY","target_files":["pkg/lru/lru.go",],}`,
			expectOp:    "MODIFY",
			expectFiles: 1,
			expectErr:   false,
		},
		{
			name: "Braces_Inside_Code_String",
			rawText: `{"operation":"MODIFY","target_files":["a.go"],"code":"func foo() { if true { return } }"}` ,
			expectOp:    "MODIFY",
			expectFiles: 1,
			expectErr:   false,
		},
		{
			name:      "Garbage_Non_JSON",
			rawText:   "I cannot fulfill this request because it violates safety guidelines.",
			expectErr: true,
		},
		{
			name:      "Empty_String",
			rawText:   "",
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var out DummyPayload
			err := llm.ExtractJSON(tc.rawText, &out)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected extract error: %v", err)
			}
			if out.Operation != tc.expectOp {
				t.Errorf("expected op %s, got %s", tc.expectOp, out.Operation)
			}
			if len(out.TargetFiles) != tc.expectFiles {
				t.Errorf("expected %d target files, got %d", tc.expectFiles, len(out.TargetFiles))
			}
		})
	}
}

func TestGateEdit_AiderBlockFallback(t *testing.T) {
	t.Parallel()

	mockClient := llm.NewMockLLMClient()
	// Model returns raw Aider search/replace block rather than JSON
	mockClient.CannedContent = `Here is the patch to apply:
<<<<<<< SEARCH
func Old() {
	println("old")
}
=======
func New() {
	println("new")
}
>>>>>>> REPLACE
`

	gate := llm.NewDecisionGate(mockClient, "mock-model")

	editDec, err := gate.DecideEdit(context.Background(), llm.GateEditCard{
		FilePath:    "pkg/lru/lru.go",
		Instruction: "rename Old to New",
	})
	if err != nil {
		t.Fatalf("unexpected DecideEdit error: %v", err)
	}

	if len(editDec.EditBlocks) != 1 {
		t.Fatalf("expected 1 edit block parsed from raw Aider block, got %d", len(editDec.EditBlocks))
	}
	if !strings.Contains(editDec.EditBlocks[0].SearchText, "func Old()") {
		t.Errorf("unexpected search text: %s", editDec.EditBlocks[0].SearchText)
	}
	if !strings.Contains(editDec.EditBlocks[0].ReplaceText, "func New()") {
		t.Errorf("unexpected replace text: %s", editDec.EditBlocks[0].ReplaceText)
	}
}

func TestGateFinalize_SafetyClamp(t *testing.T) {
	t.Parallel()

	mockClient := llm.NewMockLLMClient()
	// Model erroneously attempts to approve commit when tests failed
	mockClient.CannedContent = `{"approved": true, "commit_message": "feat: finish task"}`

	gate := llm.NewDecisionGate(mockClient, "mock-model")

	finalDec, err := gate.DecideFinalize(context.Background(), llm.GateFinalizeCard{
		TaskDescription: "Implement LRU",
		TestExitCode:    1, // FAILING TEST
		TestCommand:     "go test ./pkg/lru",
	})
	if err != nil {
		t.Fatalf("unexpected DecideFinalize error: %v", err)
	}

	// Invariant Clamp: Approved MUST be false when TestExitCode != 0
	if finalDec.Approved {
		t.Errorf("expected Approved to be clamped to false when TestExitCode != 0")
	}
	if !strings.Contains(finalDec.Reason, "failed with exit code 1") {
		t.Errorf("expected failure reason, got: %s", finalDec.Reason)
	}
}

// ============================================================================
// 4. Mock Decision Gate End-to-End Decision Flow & Failure Injection
// ============================================================================

func TestMockDecisionGate_HeuristicsAndE2E(t *testing.T) {
	t.Parallel()

	gate := llm.NewMockDecisionGate()
	ctx := context.Background()

	// Turn 1: DecideIntent
	intentCard := llm.GateIntentCard{
		UserPrompt: "Implement concurrent LRU cache in pkg/lru/lru.go",
	}
	intentDec, err := gate.DecideIntent(ctx, intentCard)
	if err != nil {
		t.Fatalf("DecideIntent error: %v", err)
	}
	if intentDec.Operation != llm.OpCreate && intentDec.Operation != llm.OpModify {
		t.Errorf("unexpected operation: %s", intentDec.Operation)
	}
	if len(intentDec.TargetFiles) == 0 || !strings.Contains(intentDec.TargetFiles[0], "lru") {
		t.Errorf("unexpected target files: %v", intentDec.TargetFiles)
	}

	// Turn 2: DecideEdit
	editCard := llm.GateEditCard{
		FilePath:       "pkg/lru/lru.go",
		Instruction:    "define Cache struct",
		ContextSnippet: "package lru\n\ntype Cache struct {}\n",
	}
	editDec, err := gate.DecideEdit(ctx, editCard)
	if err != nil {
		t.Fatalf("DecideEdit error: %v", err)
	}
	if len(editDec.EditBlocks) == 0 {
		t.Fatalf("expected edit blocks, got 0")
	}
	if editDec.FilePath != "pkg/lru/lru.go" {
		t.Errorf("expected pkg/lru/lru.go, got %s", editDec.FilePath)
	}

	// Turn 3: DecideHeal
	healCard := llm.GateHealCard{
		FilePath:      "pkg/lru/lru.go",
		ErrorLine:     3,
		ErrorMessage:  "syntax error: unexpected newline",
		Snippet5Lines: ">    3: type Cache struct {\n     4: }\n",
		Command:       "go test ./pkg/lru",
	}
	healDec, err := gate.DecideHeal(ctx, healCard)
	if err != nil {
		t.Fatalf("DecideHeal error: %v", err)
	}
	if len(healDec.EditBlocks) == 0 {
		t.Fatalf("expected surgical heal edit blocks, got 0")
	}
	if !strings.Contains(healDec.EditBlocks[0].SearchText, "type Cache struct") {
		t.Errorf("expected search block targeting line 3, got: %q", healDec.EditBlocks[0].SearchText)
	}

	// Turn 4: DecideFinalize (Success)
	finalCard := llm.GateFinalizeCard{
		TaskDescription:   "Implement concurrent LRU cache",
		FilesModified:     []string{"pkg/lru/lru.go"},
		TestCommand:       "go test ./pkg/lru",
		TestExitCode:      0,
		TestOutputSummary: "PASS\nok  pkg/lru  0.05s",
	}
	finalDec, err := gate.DecideFinalize(ctx, finalCard)
	if err != nil {
		t.Fatalf("DecideFinalize error: %v", err)
	}
	if !finalDec.Approved {
		t.Errorf("expected approved commit when exit code 0")
	}
	if !strings.HasPrefix(finalDec.CommitMessage, "feat(") {
		t.Errorf("expected conventional commit message, got: %s", finalDec.CommitMessage)
	}

	// Verify call counts and getters
	if gate.CallCount() != 4 {
		t.Errorf("expected 4 total gate calls, got %d", gate.CallCount())
	}
	if gate.LastIntent() == nil || gate.LastEdit() == nil || gate.LastHeal() == nil || gate.LastFinalize() == nil {
		t.Errorf("expected non-nil last call getters")
	}

	// Test Reset
	gate.Reset()
	if gate.CallCount() != 0 {
		t.Errorf("expected 0 calls after reset, got %d", gate.CallCount())
	}
}

func TestMockDecisionGate_FailureInjection(t *testing.T) {
	t.Parallel()

	gate := llm.NewMockDecisionGate()
	ctx := context.Background()

	// 1. Injected Intent Error
	gate.InjectIntentError = errors.New("custom intent failure")
	_, err := gate.DecideIntent(ctx, llm.GateIntentCard{UserPrompt: "test"})
	if err == nil || !strings.Contains(err.Error(), "custom intent failure") {
		t.Errorf("expected custom intent failure, got %v", err)
	}
	gate.InjectIntentError = nil

	// 2. Simulated Rejection in Finalize
	gate.SimulateRejection = true
	finalDec, err := gate.DecideFinalize(ctx, llm.GateFinalizeCard{
		TaskDescription: "task",
		TestExitCode:    0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if finalDec.Approved {
		t.Errorf("expected rejection when SimulateRejection is true")
	}
	gate.SimulateRejection = false

	// 3. Timeout injection and cancellation
	gate.InjectTimeout = 200 * time.Millisecond
	cancelCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()

	_, err = gate.DecideEdit(cancelCtx, llm.GateEditCard{FilePath: "a.go"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	gate.InjectTimeout = 0

	// 4. FailAttemptsRemaining countdown
	gate.FailAttemptsRemaining = 2
	_, err = gate.DecideHeal(ctx, llm.GateHealCard{FilePath: "a.go"})
	if err == nil {
		t.Errorf("expected failure on attempt 1")
	}
	_, err = gate.DecideHeal(ctx, llm.GateHealCard{FilePath: "a.go"})
	if err == nil {
		t.Errorf("expected failure on attempt 2")
	}
	healDec, err := gate.DecideHeal(ctx, llm.GateHealCard{FilePath: "a.go"})
	if err != nil {
		t.Errorf("expected success on attempt 3 after countdown, got %v", err)
	}
	if healDec == nil {
		t.Errorf("expected non-nil heal decision on attempt 3")
	}
}

func TestMockLLMClient_GateIntegration(t *testing.T) {
	t.Parallel()

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = "```json\n" +
		`{"operation":"REFACTOR","target_files":["pkg/canonical/canonical.go"],"rationale":"clean code"}` +
		"\n```"

	decisionGate := llm.NewDecisionGate(mockClient, "mock-model")

	dec, err := decisionGate.DecideIntent(context.Background(), llm.GateIntentCard{
		UserPrompt: "Refactor canonical pkg",
	})
	if err != nil {
		t.Fatalf("DecideIntent error: %v", err)
	}
	if dec.Operation != llm.OpRefactor {
		t.Errorf("expected REFACTOR, got %s", dec.Operation)
	}
	if len(dec.TargetFiles) != 1 || dec.TargetFiles[0] != "pkg/canonical/canonical.go" {
		t.Errorf("unexpected target files: %v", dec.TargetFiles)
	}

	if mockClient.LastRequest() == nil {
		t.Errorf("expected LastRequest() to be recorded")
	}
}

// ============================================================================
// 5. Concurrency Stress Tests under go test -v -race
// ============================================================================

func TestConcurrency_RaceFree(t *testing.T) {
	t.Parallel()

	t.Run("MockDecisionGate_ConcurrentAccess", func(t *testing.T) {
		gate := llm.NewMockDecisionGate()
		var wg sync.WaitGroup
		workers := 30
		iterations := 20

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()
				ctx := context.Background()

				for i := 0; i < iterations; i++ {
					switch (workerID + i) % 4 {
					case 0:
						_, _ = gate.DecideIntent(ctx, llm.GateIntentCard{
							UserPrompt: fmt.Sprintf("task from worker %d iteration %d", workerID, i),
						})
					case 1:
						_, _ = gate.DecideEdit(ctx, llm.GateEditCard{
							FilePath:    fmt.Sprintf("pkg/worker%d/file.go", workerID),
							Instruction: "edit instruction",
						})
					case 2:
						_, _ = gate.DecideHeal(ctx, llm.GateHealCard{
							FilePath:     fmt.Sprintf("pkg/worker%d/file.go", workerID),
							ErrorLine:    i + 1,
							ErrorMessage: "test error",
						})
					case 3:
						_, _ = gate.DecideFinalize(ctx, llm.GateFinalizeCard{
							TaskDescription: fmt.Sprintf("finalize worker %d", workerID),
							TestExitCode:    0,
						})
					}
				}
			}(w)
		}

		wg.Wait()

		expectedCalls := workers * iterations
		if gate.CallCount() != expectedCalls {
			t.Errorf("expected %d total calls, got %d", expectedCalls, gate.CallCount())
		}
	})

	t.Run("ChatCompletionClient_ConcurrentRequests", func(t *testing.T) {
		var counter atomic.Int64
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			idx := counter.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				ID: fmt.Sprintf("cmpl-%d", idx),
				Choices: []llm.Choice{
					{
						Message: llm.ChatMessage{
							Content: fmt.Sprintf(`{"status":"ok","id":%d}`, idx),
						},
					},
				},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL: ts.URL,
			Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		var wg sync.WaitGroup
		concurrency := 25

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
					Messages: []llm.ChatMessage{{Role: "user", Content: fmt.Sprintf("concurrency test %d", id)}},
				})
				if err != nil {
					t.Errorf("worker %d failed: %v", id, err)
					return
				}
				if resp == nil || resp.Content() == "" {
					t.Errorf("worker %d got empty response", id)
				}
			}(i)
		}

		wg.Wait()

		if counter.Load() != int64(concurrency) {
			t.Errorf("expected %d requests received, got %d", concurrency, counter.Load())
		}
	})
}

func TestGateHealCard_RoutesCommandClamping(t *testing.T) {
	snippet := `1021:     headers = ("Endpoint", "Methods", "Rule")
1022:     widths = (
1023:         max(len(rule.endpoint) for rule in rules),
1024:         max(len(methods) for methods in rule_methods),
1025:         max(len(rule.rule) for rule in rules),
1026:     )
1027:     widths = [max(len(h), w) for h, w in zip(headers, widths)]
1028:     row = "{{0:<{0}}}  {{1:<{1}}}  {{2:<{2}}}".format(*widths)
1030:     click.echo(row.format(*headers).strip())
1031:     click.echo(row.format(*("-" * width for width in widths)))
1033:     for rule, methods in zip(rules, rule_methods):
1034:         click.echo(row.format(rule.endpoint, methods, rule.rule).rstrip())`

	card := llm.GateHealCard{
		FilePath:      "src/flask/cli.py",
		ErrorLine:     1001,
		ErrorMessage:  `assert "Subdomain" in result.output`,
		Snippet5Lines: snippet,
		Command:       "python -m pytest tests/test_cli.py -k test_subdomain",
		IssueContext:  "Flask routes to return domain/sub-domains information",
	}

	sys, user, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}
	totalTokens := llm.EstimateTokens(sys + " " + user)
	t.Logf("User prompt (%d tokens):\n%s", totalTokens, user)
	if totalTokens >= llm.MaxCardTokenBudget {
		t.Fatalf("Total tokens %d >= %d budget", totalTokens, llm.MaxCardTokenBudget)
	}
	if !strings.Contains(user, "headers =") {
		t.Errorf("User prompt clamped out 'headers =':\n%s", user)
	}
	if !strings.Contains(user, "for rule, methods in zip") {
		t.Errorf("User prompt clamped out 'for rule, methods in zip':\n%s", user)
	}
	if !strings.Contains(user, "click.echo(row.format(rule.endpoint") {
		t.Errorf("User prompt clamped out 'click.echo(row.format(rule.endpoint':\n%s", user)
	}
}

// ============================================================================
// GateReproducerCard Tests
// ============================================================================

func TestGateReproducerCard_FormatPrompt_BudgetClamping(t *testing.T) {
	// 1. Normal prompt
	card := llm.GateReproducerCard{
		IssueText:        "TypeError: unsupported operand type(s) for +: 'int' and 'str'",
		CausalFiles:      []string{"src/calc.py"},
		Language:         "python",
		TracebackSnippet: `File "calc.py", line 10, in add: return a + b`,
	}

	sys, user, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}
	tokens := llm.EstimateTokens(sys + " " + user)
	if tokens >= llm.MaxCardTokenBudget {
		t.Errorf("expected tokens < %d, got %d", llm.MaxCardTokenBudget, tokens)
	}
	if !strings.Contains(sys, "GateReproducer") {
		t.Errorf("system prompt missing GateReproducer identifier")
	}
	if !strings.Contains(user, "calc.py") {
		t.Errorf("user prompt missing causal file: %s", user)
	}

	// 2. Adversarially large issue text (5,000 characters)
	hugeIssue := strings.Repeat("Long issue description line with error details and stack frames.\n", 100)
	hugeCard := llm.GateReproducerCard{
		IssueText:        hugeIssue,
		CausalFiles:      []string{"large/path/to/module.py"},
		Language:         "python",
		TracebackSnippet: "AssertionError: expected True but got False",
	}

	sysHuge, userHuge, err := hugeCard.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt with huge issue failed: %v", err)
	}
	hugeTokens := llm.EstimateTokens(sysHuge + " " + userHuge)
	if hugeTokens >= llm.MaxCardTokenBudget {
		t.Fatalf("SAFETY VIOLATION: huge issue card tokens %d >= %d budget", hugeTokens, llm.MaxCardTokenBudget)
	}
}

func TestLLMDecisionGate_DecideReproducer_JSON(t *testing.T) {
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `{
		"script_name": "reproduce_issue.py",
		"script_content": "import sys\nsys.exit(1)",
		"explanation": "minimal failure reproducer"
	}`

	gate := llm.NewDecisionGate(mockClient, "mock-model")
	decision, err := gate.DecideReproducer(context.Background(), llm.GateReproducerCard{
		IssueText: "Bug in module",
	})
	if err != nil {
		t.Fatalf("unexpected DecideReproducer error: %v", err)
	}

	if decision.ScriptName != "reproduce_issue.py" {
		t.Errorf("expected reproduce_issue.py, got %s", decision.ScriptName)
	}
	if !strings.Contains(decision.ScriptContent, "sys.exit(1)") {
		t.Errorf("unexpected script content: %s", decision.ScriptContent)
	}
}

func TestLLMDecisionGate_DecideReproducer_FencedFallback(t *testing.T) {
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = "```python\nimport sys\n# Raw script without JSON envelope\nsys.exit(1)\n```"

	gate := llm.NewDecisionGate(mockClient, "mock-model")
	decision, err := gate.DecideReproducer(context.Background(), llm.GateReproducerCard{
		IssueText: "Bug in module",
	})
	if err != nil {
		t.Fatalf("unexpected error on fenced fallback: %v", err)
	}

	if decision.ScriptName != "reproduce_issue.py" {
		t.Errorf("expected script name reproduce_issue.py, got %s", decision.ScriptName)
	}
	if !strings.Contains(decision.ScriptContent, "sys.exit(1)") {
		t.Errorf("expected script content to contain sys.exit(1), got %s", decision.ScriptContent)
	}
}

func TestMockDecisionGate_DecideReproducer(t *testing.T) {
	mockGate := llm.NewMockDecisionGate()

	// 1. Default heuristic
	card := llm.GateReproducerCard{
		IssueText: "Division by zero bug in compute",
	}
	decision, err := mockGate.DecideReproducer(context.Background(), card)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decision.ScriptName != "reproduce_issue.py" {
		t.Errorf("expected reproduce_issue.py, got %s", decision.ScriptName)
	}
	if len(mockGate.ReproducerCalls) != 1 {
		t.Errorf("expected 1 call recorded, got %d", len(mockGate.ReproducerCalls))
	}
	if mockGate.LastReproducer() == nil {
		t.Errorf("expected LastReproducer() not nil")
	}

	// 2. Canned decision
	mockGate.CannedReproducer = &llm.ReproducerDecision{
		ScriptName:    "custom_reproduce.py",
		ScriptContent: "import sys; sys.exit(1)",
	}
	cannedDec, err := mockGate.Reproducer(context.Background(), card)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cannedDec.ScriptName != "custom_reproduce.py" {
		t.Errorf("expected custom_reproduce.py, got %s", cannedDec.ScriptName)
	}

	// 3. Reset
	mockGate.Reset()
	if len(mockGate.ReproducerCalls) != 0 {
		t.Errorf("expected ReproducerCalls to be 0 after reset")
	}
	if mockGate.LastReproducer() != nil {
		t.Errorf("expected LastReproducer() to be nil after reset")
	}
}

func TestDecideHealSpeculative(t *testing.T) {
	t.Run("MockDecisionGate_SpeculativeK", func(t *testing.T) {
		mockGate := llm.NewMockDecisionGate()
		card := llm.GateHealCard{
			FilePath:     "calc.go",
			ErrorLine:    10,
			ErrorMessage: "undefined variable",
			Snippet5Lines: "> 10: x := y",
		}
		decisions, err := mockGate.DecideHealSpeculative(context.Background(), card, 3)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(decisions) != 3 {
			t.Fatalf("expected 3 decisions, got %d", len(decisions))
		}
		if decisions[0].FilePath != "calc.go" {
			t.Errorf("expected calc.go, got %s", decisions[0].FilePath)
		}
	})

	t.Run("LLMDecisionGate_SpeculativeWireN", func(t *testing.T) {
		var receivedReq llm.ChatCompletionRequest
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedReq)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatCompletionResponse{
				ID: "cmpl-speculative-1",
				Choices: []llm.Choice{
					{
						Index: 0,
						Message: llm.ChatMessage{
							Role:    "assistant",
							Content: `{"file_path": "foo.py", "edit_blocks": [{"search_text": "a", "replace_text": "b"}], "repair_summary": "cand 1"}`,
						},
					},
					{
						Index: 1,
						Message: llm.ChatMessage{
							Role:    "assistant",
							Content: `{"file_path": "foo.py", "edit_blocks": [{"search_text": "a", "replace_text": "c"}], "repair_summary": "cand 2"}`,
						},
					},
					{
						Index: 2,
						Message: llm.ChatMessage{
							Role:    "assistant",
							Content: `{"file_path": "foo.py", "edit_blocks": [{"search_text": "a", "replace_text": "d"}], "repair_summary": "cand 3"}`,
						},
					},
				},
			})
		}))
		defer ts.Close()

		client, err := llm.NewClient(llm.ClientConfig{
			BaseURL: ts.URL,
			APIKey:  "dummy",
			Model:   "test-model",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}

		gate := llm.NewDecisionGate(client, "test-model")
		card := llm.GateHealCard{
			FilePath:     "foo.py",
			ErrorLine:    5,
			ErrorMessage: "syntax error",
		}
		decisions, err := gate.DecideHealSpeculative(context.Background(), card, 3)
		if err != nil {
			t.Fatalf("DecideHealSpeculative error: %v", err)
		}
		if receivedReq.N != 3 {
			t.Errorf("expected receivedReq.N == 3, got %d", receivedReq.N)
		}
		if len(decisions) != 3 {
			t.Fatalf("expected 3 decisions, got %d", len(decisions))
		}
	})
}

