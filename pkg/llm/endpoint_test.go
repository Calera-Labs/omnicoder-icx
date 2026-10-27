// SPDX-License-Identifier: Apache-2.0
// Package llm_test contains empirical adversarial stress tests for
// pkg/llm endpoint normalization, NewClient permutations, URL variations,
// and credential isolation.

package llm_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// 1. Trailing Slashes & URL Variation Matrix
// ============================================================================

func TestEndpointNormalization_AdversarialURLs(t *testing.T) {
	t.Parallel()

	expectedGemini := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

	cases := []struct {
		name     string
		cfg      llm.ClientConfig
		expected string
		checkFn  func(t *testing.T, endpoint string)
	}{
		// 1. Trailing Slash Stress (Google AI Studio)
		{
			name:     "GoogleAIStudio_BareHost",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_SingleTrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_FiveTrailingSlashes",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/////"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1beta_NoSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1beta_MultipleTrailingSlashes",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta///"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1_Path",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1/"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1beta_openai_NoSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1beta_openai_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_v1beta_openai_ManySlashes",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai///////"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_FullChatCompletions_NoSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
			expected: expectedGemini,
		},
		{
			name:     "GoogleAIStudio_FullChatCompletions_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions/"},
			expected: expectedGemini,
		},

		// 2. Local & Proxy URLs with Trailing Slashes
		{
			name:     "LocalOllama_HostOnly_NoSlash",
			cfg:      llm.ClientConfig{BaseURL: "http://localhost:11434", Model: "qwen2.5"},
			expected: "http://localhost:11434/v1/chat/completions",
		},
		{
			name:     "LocalOllama_HostOnly_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "http://localhost:11434/", Model: "qwen2.5"},
			expected: "http://localhost:11434/v1/chat/completions",
		},
		{
			name:     "LocalOllama_HostOnly_MultiSlash",
			cfg:      llm.ClientConfig{BaseURL: "http://localhost:11434////", Model: "qwen2.5"},
			expected: "http://localhost:11434/v1/chat/completions",
		},
		{
			name:     "LocalOllama_v1_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "http://localhost:11434/v1/", Model: "qwen2.5"},
			expected: "http://localhost:11434/v1/chat/completions",
		},
		{
			name:     "LocalOllama_v1_MultiSlash",
			cfg:      llm.ClientConfig{BaseURL: "http://localhost:11434/v1////", Model: "qwen2.5"},
			expected: "http://localhost:11434/v1/chat/completions",
		},
		{
			name:     "LocalOllama_FullEndpoint_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "http://localhost:11434/v1/chat/completions/", Model: "qwen2.5"},
			expected: "http://localhost:11434/v1/chat/completions",
		},

		// 3. Custom Path Prefixes & Proxies
		{
			name:     "CustomProxy_Prefix_v1",
			cfg:      llm.ClientConfig{BaseURL: "https://proxy.example.com/api/v1", Model: "custom"},
			expected: "https://proxy.example.com/api/v1/chat/completions",
		},
		{
			name:     "CustomProxy_Prefix_v1_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://proxy.example.com/api/v1/", Model: "custom"},
			expected: "https://proxy.example.com/api/v1/chat/completions",
		},
		{
			name:     "CustomProxy_Prefix_openai",
			cfg:      llm.ClientConfig{BaseURL: "https://gateway.internal.net/gateway/openai", Model: "custom"},
			expected: "https://gateway.internal.net/gateway/openai/chat/completions",
		},
		{
			name:     "CustomProxy_Prefix_openai_TrailingSlash",
			cfg:      llm.ClientConfig{BaseURL: "https://gateway.internal.net/gateway/openai/", Model: "custom"},
			expected: "https://gateway.internal.net/gateway/openai/chat/completions",
		},
		{
			name:     "CustomProxy_DeepPath_NoStandardSuffix",
			cfg:      llm.ClientConfig{BaseURL: "https://ai.corp.io/teams/eng/llm-router", Model: "custom"},
			expected: "https://ai.corp.io/teams/eng/llm-router/v1/chat/completions",
		},

		// 4. Ports & Protocols
		{
			name:     "CustomPort_NonStandard",
			cfg:      llm.ClientConfig{BaseURL: "http://10.0.0.5:8088/v1", Model: "llama"},
			expected: "http://10.0.0.5:8088/v1/chat/completions",
		},
		{
			name:     "GoogleAIStudio_ExplicitPort443",
			cfg:      llm.ClientConfig{BaseURL: "https://generativelanguage.googleapis.com:443/v1beta/openai"},
			expected: "https://generativelanguage.googleapis.com:443/v1beta/openai/chat/completions",
		},

		// 5. Whitespace Handling
		{
			name:     "BaseURL_SurroundingWhitespaceAndTabs",
			cfg:      llm.ClientConfig{BaseURL: " \t https://generativelanguage.googleapis.com/v1beta/openai \n\t "},
			expected: expectedGemini,
		},
		{
			name:     "BaseURL_EmptyString_DefaultsToGemini",
			cfg:      llm.ClientConfig{BaseURL: ""},
			expected: expectedGemini,
		},
		{
			name:     "BaseURL_WhitespaceOnly_DefaultsToGemini",
			cfg:      llm.ClientConfig{BaseURL: "   \t\n  "},
			expected: expectedGemini,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := llm.NewClient(tc.cfg)
			if err != nil {
				t.Fatalf("unexpected NewClient error: %v", err)
			}
			actual := c.Endpoint()
			if tc.expected != "" && actual != tc.expected {
				t.Errorf("endpoint mismatch:\n  got:      %s\n  expected: %s", actual, tc.expected)
			}
			if tc.checkFn != nil {
				tc.checkFn(t, actual)
			}

			// Invariant: Normalized endpoint must parse cleanly as a valid absolute URI
			parsed, err := url.Parse(actual)
			if err != nil {
				t.Errorf("normalized endpoint failed url.Parse: %v", err)
			}
			if parsed.Scheme == "" {
				t.Errorf("normalized endpoint missing URI scheme: %s", actual)
			}
			if parsed.Host == "" {
				t.Errorf("normalized endpoint missing URI host: %s", actual)
			}
			if strings.HasSuffix(actual, "/") {
				t.Errorf("normalized endpoint should never end with a trailing slash: %s", actual)
			}
			if strings.Contains(actual, "//chat") {
				t.Errorf("normalized endpoint contains redundant double slashes: %s", actual)
			}
		})
	}
}

// ============================================================================
// 2. NewClient Model and API Key Precedence Matrix
// ============================================================================

func TestNewClient_ModelAndKeyCombinations(t *testing.T) {
	t.Parallel()

	expectedGemini := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"

	t.Run("Default_ZeroConfig", func(t *testing.T) {
		c, err := llm.NewClient(llm.ClientConfig{})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		if c.Endpoint() != expectedGemini {
			t.Errorf("expected endpoint %s, got %s", expectedGemini, c.Endpoint())
		}
		if c.Config().Model != "gemini-3.5-flash-lite" {
			t.Errorf("expected model %q, got %q", "gemini-3.5-flash-lite", c.Config().Model)
		}
		if c.Config().APIKey != llm.DefaultFallbackAPIKey {
			t.Errorf("expected default fallback API key, got %q", c.Config().APIKey)
		}
	})

	t.Run("GeminiModel_With_DefaultBaseURL_AutoNormalizes", func(t *testing.T) {
		// When user provides DefaultBaseURL (http://localhost:11434/v1) but Gemini model,
		// it auto-normalizes to Google AI Studio
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: llm.DefaultBaseURL,
			Model:   "gemini-2.5-flash",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		if c.Endpoint() != expectedGemini {
			t.Errorf("expected endpoint %s, got %s", expectedGemini, c.Endpoint())
		}
		if c.Config().BaseURL != llm.GoogleAIStudioBaseURL {
			t.Errorf("expected BaseURL %s, got %s", llm.GoogleAIStudioBaseURL, c.Config().BaseURL)
		}
	})

	t.Run("NonGeminiModel_With_EmptyBaseURL_DefaultsToOllama", func(t *testing.T) {
		// Non-Gemini model with empty BaseURL and no Gemini key defaults to local Ollama
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "",
			Model:   "gpt-4o",
			APIKey:  "sk-openai-test-key",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		expected := "http://localhost:11434/v1/chat/completions"
		if c.Endpoint() != expected {
			t.Errorf("expected endpoint %s, got %s", expected, c.Endpoint())
		}
		if c.Config().BaseURL != llm.DefaultBaseURL {
			t.Errorf("expected BaseURL %s, got %s", llm.DefaultBaseURL, c.Config().BaseURL)
		}
	})

	t.Run("GeminiKey_ForcesNormalization_EvenWithNonGeminiModelName", func(t *testing.T) {
		// If an AQ. key is passed with empty BaseURL and an arbitrary model name,
		// IsGeminiKey detects it and normalizes to Google AI Studio
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "",
			Model:   "custom-proxy-model",
			APIKey:  "AQ.TestGeminiKeyABC123",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		if c.Endpoint() != expectedGemini {
			t.Errorf("expected endpoint %s, got %s", expectedGemini, c.Endpoint())
		}
	})

	t.Run("AIza_Key_ForcesNormalization_WithEmptyBaseURL", func(t *testing.T) {
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "",
			Model:   "custom-model",
			APIKey:  "AIzaSyDemoGoogleKey456",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		if c.Endpoint() != expectedGemini {
			t.Errorf("expected endpoint %s, got %s", expectedGemini, c.Endpoint())
		}
	})

	t.Run("Explicit_LocalProxy_Preserved_For_NonDefaultURL", func(t *testing.T) {
		// If user explicitly specifies a local proxy (e.g. 127.0.0.1 instead of DefaultBaseURL),
		// even with a Gemini model, their custom BaseURL is respected
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "http://127.0.0.1:8000/v1",
			Model:   "gemini-3.5-flash-lite",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		expected := "http://127.0.0.1:8000/v1/chat/completions"
		if c.Endpoint() != expected {
			t.Errorf("expected custom proxy %s, got %s", expected, c.Endpoint())
		}
	})
}

// ============================================================================
// 3. Credential Isolation & Non-Leakage Invariants
// ============================================================================

func TestCredentialIsolation_NoFallbackLeakToExternal(t *testing.T) {
	t.Parallel()

	// CRITICAL INVARIANT: The hardcoded Gemini fallback API key must NEVER be sent
	// to non-Google servers (e.g., local Ollama, third-party proxies, api.openai.com)
	// when the user has not specified an API key.

	externalHosts := []struct {
		name    string
		baseURL string
		model   string
	}{
		{"Localhost_Ollama", "http://localhost:11434/v1", "llama3.2"},
		{"Local_IP_Proxy", "http://127.0.0.1:8000", "mistral"},
		{"OpenAI_Official", "https://api.openai.com/v1", "gpt-4o"},
		{"ThirdParty_Proxy", "https://api.groq.com/openai/v1", "llama-3-70b"},
	}

	for _, tc := range externalHosts {
		t.Run(tc.name, func(t *testing.T) {
			c, err := llm.NewClient(llm.ClientConfig{
				BaseURL: tc.baseURL,
				Model:   tc.model,
				APIKey:  "", // No key provided
			})
			if err != nil {
				t.Fatalf("NewClient error: %v", err)
			}

			// Invariant: APIKey must remain empty, never populated with DefaultFallbackAPIKey
			if c.Config().APIKey != "" {
				t.Fatalf("SECURITY VIOLATION: fallback API key %q was leaked to non-Google endpoint %s!",
					c.Config().APIKey, tc.baseURL)
			}
		})
	}
}

// ============================================================================
// 4. Live HTTP Roundtrip & Header Verification with Normalized Endpoints
// ============================================================================

func TestNormalizedEndpoints_LiveHTTPRoundtrip(t *testing.T) {
	t.Parallel()

	// Verify that normalized endpoints are valid for net/http.Do without malformed request errors
	var interceptedPath string
	var interceptedHost string
	var interceptedAuth string
	var interceptedGoogKey string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		interceptedPath = r.URL.Path
		interceptedHost = r.Host
		interceptedAuth = r.Header.Get("Authorization")
		interceptedGoogKey = r.Header.Get("x-goog-api-key")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "cmpl-endpoint-live",
			"choices": [{"index":0,"message":{"role":"assistant","content":"live-ok"},"finish_reason":"stop"}],
			"usage": {"total_tokens": 10}
		}`))
	}))
	defer ts.Close()

	// Test 1: ts.URL with trailing slash
	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: ts.URL + "////",
		APIKey:  "endpoint-test-key",
	})
	if err != nil {
		t.Fatalf("NewClient error: %v", err)
	}

	resp, err := client.Complete(context.Background(), &llm.ChatCompletionRequest{
		Messages: []llm.ChatMessage{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("Complete failed on normalized endpoint: %v", err)
	}
	if resp.Content() != "live-ok" {
		t.Errorf("unexpected content: %s", resp.Content())
	}
	if interceptedPath != "/v1/chat/completions" {
		t.Errorf("expected path /v1/chat/completions, got %s", interceptedPath)
	}
	if interceptedAuth != "Bearer endpoint-test-key" {
		t.Errorf("expected Bearer endpoint-test-key, got %s", interceptedAuth)
	}
	_ = interceptedHost
	_ = interceptedGoogKey
}

// ============================================================================
// 5. Adversarial Input Fuzzing: Zero Panics
// ============================================================================

func TestEndpointNormalization_FuzzInputs_ZeroPanics(t *testing.T) {
	t.Parallel()

	fuzzInputs := []string{
		// Extremely long URL
		"https://generativelanguage.googleapis.com/" + strings.Repeat("subpath/", 500),
		// Only slashes
		"////////////////////",
		// Unicode control characters and emojis
		"https://generativelanguage.googleapis.com/v1beta/openai/🚀/🔥",
		// Tab and newline injections
		"https://generativelanguage.googleapis.com\r\n/v1beta/openai",
		// Null bytes
		"https://generativelanguage.googleapis.com\x00/v1beta",
		// Colon variations
		":///::://",
		// IP with high port
		"http://127.0.0.1:65535/v1",
		// IPv6 literals
		"http://[::1]:11434/v1",
		// Windows backslashes
		"https:\\\\generativelanguage.googleapis.com\\v1beta\\openai",
	}

	for idx, input := range fuzzInputs {
		t.Run(fmt.Sprintf("Fuzz_%d", idx), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on fuzz input %d (%q): %v", idx, input, r)
				}
			}()

			c, err := llm.NewClient(llm.ClientConfig{
				BaseURL: input,
				Model:   "gemini-3.5-flash-lite",
			})
			if err != nil {
				// Errors during NewClient are acceptable as long as it does not panic
				return
			}
			// Endpoint should be non-empty and callable without panic
			ep := c.Endpoint()
			if ep == "" {
				t.Errorf("endpoint was unexpectedly empty for input: %q", input)
			}
		})
	}

	// Pseudo-random byte fuzzing (50 iterations)
	for i := 0; i < 50; i++ {
		b := make([]byte, 64)
		_, _ = rand.Read(b)
		randomStr := string(b)

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("PANIC on random byte string iteration %d: %v", i, r)
			}
		}()

		_, _ = llm.NewClient(llm.ClientConfig{
			BaseURL: randomStr,
			Model:   randomStr,
		})
	}
}

// ============================================================================
// 6. Adversarial Query Parameters & Fragment Edge Case Examination
// ============================================================================

func TestEndpointNormalization_QueryAndFragmentAnalysis(t *testing.T) {
	t.Parallel()

	// Documenting behavior with query parameters in BaseURL:
	// e.g. Azure OpenAI endpoints or proxies with query parameters.
	queryURLs := []struct {
		name    string
		baseURL string
	}{
		{"QueryParam_alt_json", "https://generativelanguage.googleapis.com/v1beta/openai?alt=json"},
		{"QueryParam_apiVersion", "https://api.openai.com/v1?api-version=2024-02-01"},
		{"Fragment_Hash", "https://api.openai.com/v1#hash"},
	}

	for _, tc := range queryURLs {
		t.Run(tc.name, func(t *testing.T) {
			c, err := llm.NewClient(llm.ClientConfig{
				BaseURL: tc.baseURL,
			})
			if err != nil {
				t.Fatalf("NewClient error: %v", err)
			}
			t.Logf("[%s] Input: %q -> Endpoint: %q", tc.name, tc.baseURL, c.Endpoint())
		})
	}
}

// ============================================================================
// 7. Case Sensitivity & Scheme Omission Observations
// ============================================================================

func TestEndpointNormalization_EdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("SchemeOmission_GenerativelanguageWithoutScheme", func(t *testing.T) {
		// Test: generativelanguage.googleapis.com without https://
		c1, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "generativelanguage.googleapis.com",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		// Because it doesn't contain /v1beta/openai or /openai, NewClient resets it to GoogleAIStudioBaseURL:
		expected := "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
		if c1.Endpoint() != expected {
			t.Errorf("expected %s, got %s", expected, c1.Endpoint())
		}

		// BUT what if they pass generativelanguage.googleapis.com/v1beta/openai (missing https://)?
		c2, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "generativelanguage.googleapis.com/v1beta/openai",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		t.Logf("Observed endpoint when scheme omitted with subpath: %q", c2.Endpoint())
	})

	t.Run("DefaultBaseURL_WithTrailingSlash_Preservation", func(t *testing.T) {
		// When BaseURL is "http://localhost:11434/v1/" (with trailing slash) and Model is Gemini:
		// Does NewClient treat it as DefaultBaseURL or an explicit custom host?
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "http://localhost:11434/v1/",
			Model:   "gemini-3.5-flash-lite",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		t.Logf("Observed endpoint for 'http://localhost:11434/v1/': %q", c.Endpoint())
	})

	t.Run("Uppercase_Host_CaseSensitivity", func(t *testing.T) {
		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "https://GENERATIVELANGUAGE.GOOGLEAPIS.COM/v1beta/openai",
		})
		if err != nil {
			t.Fatalf("NewClient error: %v", err)
		}
		t.Logf("Observed endpoint for uppercase host: %q", c.Endpoint())
	})
}

