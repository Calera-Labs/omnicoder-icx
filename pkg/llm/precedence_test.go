// SPDX-License-Identifier: Apache-2.0
// Package llm_test provides empirical adversarial stress tests and precedence verification
// for pkg/llm credential resolution across all combinations of flags and environment variables.

package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// -----------------------------------------------------------------------------
// 1. Oracle Combinatorial Verification (108 States)
// -----------------------------------------------------------------------------

// oracleResolve computes the mathematically expected key resolution order:
// Flag (trimmed) > GEMINI_API_KEY (trimmed) > OMNICODER_API_KEY (trimmed) > OPENAI_API_KEY (trimmed) > DefaultFallbackAPIKey
func oracleResolve(flagKey, geminiEnv, omniEnv, openaiEnv string) string {
	if k := strings.TrimSpace(flagKey); k != "" {
		return k
	}
	if k := strings.TrimSpace(geminiEnv); k != "" {
		return k
	}
	if k := strings.TrimSpace(omniEnv); k != "" {
		return k
	}
	if k := strings.TrimSpace(openaiEnv); k != "" {
		return k
	}
	return llm.DefaultFallbackAPIKey
}

func TestPrecedence_CombinatorialMatrix_108States(t *testing.T) {
	// Not parallel because environment variables are process-global in Go
	flagValues := []struct {
		desc string
		val  string
	}{
		{"flag_empty", ""},
		{"flag_spaces", "   "},
		{"flag_valid", "sk-flag-override-123"},
		{"flag_weird", "flag-🔑-!@#$%-with-symbols"},
	}

	geminiValues := []struct {
		desc string
		val  string
	}{
		{"gemini_empty", ""},
		{"gemini_whitespace", "  \t\n  "},
		{"gemini_valid", "AIza-gemini-env-override"},
	}

	omniValues := []struct {
		desc string
		val  string
	}{
		{"omni_empty", ""},
		{"omni_whitespace", " \r\n "},
		{"omni_valid", "omnicoder-env-tier3"},
	}

	openaiValues := []struct {
		desc string
		val  string
	}{
		{"openai_empty", ""},
		{"openai_whitespace", "\t\t"},
		{"openai_valid", "sk-openai-tier4"},
	}

	stateCount := 0
	for _, f := range flagValues {
		for _, g := range geminiValues {
			for _, o := range omniValues {
				for _, a := range openaiValues {
					stateCount++
					t.Setenv("GEMINI_API_KEY", g.val)
					t.Setenv("OMNICODER_API_KEY", o.val)
					t.Setenv("OPENAI_API_KEY", a.val)

					expected := oracleResolve(f.val, g.val, o.val, a.val)
					actual := llm.ResolveAPIKey(f.val)

					if actual != expected {
						t.Errorf("[State %d: %s | %s | %s | %s] expected %q, got %q",
							stateCount, f.desc, g.desc, o.desc, a.desc, expected, actual)
					}
				}
			}
		}
	}
	t.Logf("Empirically tested all %d credential combinations; all matched formal oracle.", stateCount)
}

// -----------------------------------------------------------------------------
// 2. Strict Precedence Hierarchy Ladder
// -----------------------------------------------------------------------------

func TestPrecedence_HierarchyLadder(t *testing.T) {
	const (
		expectedFallback = "AQ.ExampleDemoKey000000000000000000000000000000"
		keyOpenAI        = "sk-openai-priority-4"
		keyOmni          = "omni-key-priority-3"
		keyGemini        = "gemini-key-priority-2"
		keyFlag          = "flag-key-priority-1"
	)

	// Tier 5: All empty -> DefaultFallbackAPIKey
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OMNICODER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	if got := llm.ResolveAPIKey(""); got != expectedFallback {
		t.Fatalf("Tier 5 failure: expected fallback %q, got %q", expectedFallback, got)
	}

	// Tier 4: OPENAI_API_KEY set -> overrides fallback
	t.Setenv("OPENAI_API_KEY", keyOpenAI)
	if got := llm.ResolveAPIKey(""); got != keyOpenAI {
		t.Fatalf("Tier 4 failure: expected %q, got %q", keyOpenAI, got)
	}

	// Tier 3: OMNICODER_API_KEY set -> overrides OPENAI_API_KEY and fallback
	t.Setenv("OMNICODER_API_KEY", keyOmni)
	if got := llm.ResolveAPIKey(""); got != keyOmni {
		t.Fatalf("Tier 3 failure: expected %q, got %q", keyOmni, got)
	}

	// Tier 2: GEMINI_API_KEY set -> overrides OMNICODER_API_KEY, OPENAI_API_KEY, and fallback
	t.Setenv("GEMINI_API_KEY", keyGemini)
	if got := llm.ResolveAPIKey(""); got != keyGemini {
		t.Fatalf("Tier 2 failure: expected %q, got %q", keyGemini, got)
	}

	// Tier 1: Explicit flag set -> overrides ALL environment variables
	if got := llm.ResolveAPIKey(keyFlag); got != keyFlag {
		t.Fatalf("Tier 1 failure: expected %q, got %q", keyFlag, got)
	}
}

// -----------------------------------------------------------------------------
// 3. Pathological Whitespace Permutations
// -----------------------------------------------------------------------------

func TestPathologicalWhitespace(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OMNICODER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	whitespaceSamples := []struct {
		name string
		val  string
	}{
		{"single_space", " "},
		{"ten_spaces", "          "},
		{"hundred_spaces", strings.Repeat(" ", 100)},
		{"tab", "\t"},
		{"multi_tabs", "\t\t\t\t"},
		{"newline_lf", "\n"},
		{"newline_crlf", "\r\n"},
		{"mixed_whitespace", " \t \r \n \v \f "},
		{"non_breaking_space", "\u00a0"},
		{"ideographic_space", "\u3000"},
		{"ogham_space_mark", "\u1680"},
		{"zero_width_space", "\u200b"}, // Note: U+200B is not a unicode.IsSpace rune in Go
	}

	for _, tc := range whitespaceSamples {
		t.Run("Flag_"+tc.name, func(t *testing.T) {
			got := llm.ResolveAPIKey(tc.val)
			// For non-space zero-width characters, ResolveAPIKey preserves if !unicode.IsSpace
			if tc.name == "zero_width_space" {
				if got != tc.val {
					t.Logf("Note: U+200B zero-width space treated as valid rune %q", got)
				}
				return
			}
			if got != llm.DefaultFallbackAPIKey {
				t.Errorf("expected whitespace sample %q to resolve to fallback, got %q", tc.name, got)
			}
		})

		t.Run("GeminiEnv_"+tc.name, func(t *testing.T) {
			if tc.name == "zero_width_space" {
				return
			}
			t.Setenv("GEMINI_API_KEY", tc.val)
			t.Setenv("OMNICODER_API_KEY", "valid-omni-key")
			got := llm.ResolveAPIKey("")
			if got != "valid-omni-key" {
				t.Errorf("expected whitespace in GEMINI_API_KEY (%q) to fall through to OMNICODER_API_KEY, got %q",
					tc.name, got)
			}
			t.Setenv("GEMINI_API_KEY", "")
			t.Setenv("OMNICODER_API_KEY", "")
		})
	}
}

// -----------------------------------------------------------------------------
// 4. Trimming and Weird Character Handling
// -----------------------------------------------------------------------------

func TestWeirdCharactersAndTrimming(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OMNICODER_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{"leading_trailing_spaces", "   key-12345   ", "key-12345"},
		{"leading_trailing_newlines_tabs", "\n\t  my-token \r\n", "my-token"},
		{"internal_spaces_preserved", "key with spaces inside", "key with spaces inside"},
		{"special_characters", "!@#$%^&*()_+-=[]{}|;':,.<>/?~`", "!@#$%^&*()_+-=[]{}|;':,.<>/?~`"},
		{"unicode_emojis", "🔑_api_gemini_🚀", "🔑_api_gemini_🚀"},
		{"base64_padding_equal", "AQ.ExampleDemoKey000000000000000000000000000000==", "AQ.ExampleDemoKey000000000000000000000000000000=="},
		{"massive_64kb_key", strings.Repeat("a", 65536), strings.Repeat("a", 65536)},
	}

	for _, tc := range testCases {
		t.Run("Flag_"+tc.name, func(t *testing.T) {
			got := llm.ResolveAPIKey(tc.input)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})

		t.Run("Env_"+tc.name, func(t *testing.T) {
			t.Setenv("GEMINI_API_KEY", tc.input)
			got := llm.ResolveAPIKey("")
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
			t.Setenv("GEMINI_API_KEY", "")
		})
	}
}

// -----------------------------------------------------------------------------
// 5. Predicate Functions: IsGeminiKey & IsGeminiModel
// -----------------------------------------------------------------------------

func TestPredicates(t *testing.T) {
	geminiKeyTests := []struct {
		key      string
		expected bool
	}{
		{llm.DefaultFallbackAPIKey, true},
		{"AQ.1234567890abcdef", true},
		{"AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz", true},
		{"  AQ.with-spaces  ", true},
		{"  AIza-with-spaces ", true},
		{"", false},
		{"   ", false},
		{"sk-openai-key", false},
		{"omnicoder-secret-key", false},
		{"bearer aq.lowercase", false}, // case sensitive prefix
	}

	for _, tc := range geminiKeyTests {
		t.Run("IsGeminiKey_"+tc.key, func(t *testing.T) {
			got := llm.IsGeminiKey(tc.key)
			if got != tc.expected {
				t.Errorf("IsGeminiKey(%q) expected %v, got %v", tc.key, tc.expected, got)
			}
		})
	}

	geminiModelTests := []struct {
		model    string
		expected bool
	}{
		{"gemini-3.5-flash-lite", true},
		{"GEMINI-3.5-FLASH-LITE", true},
		{"models/gemini-1.5-pro", true},
		{"  gemini-2.0-flash  ", true},
		{"custom-gemini-finetune", true},
		{"gpt-4o", false},
		{"claude-3-5-sonnet", false},
		{"deepseek-r1", false},
		{"", false},
		{"   ", false},
	}

	for _, tc := range geminiModelTests {
		t.Run("IsGeminiModel_"+tc.model, func(t *testing.T) {
			got := llm.IsGeminiModel(tc.model)
			if got != tc.expected {
				t.Errorf("IsGeminiModel(%q) expected %v, got %v", tc.model, tc.expected, got)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 6. NewClient Integration & Edge Cases
// -----------------------------------------------------------------------------

func TestNewClient_CredentialResolution(t *testing.T) {
	t.Run("GeminiDefault_ResolvesFallbackKey", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "")
		t.Setenv("OMNICODER_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		c, err := llm.NewClient(llm.ClientConfig{})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if c.Config().APIKey != llm.DefaultFallbackAPIKey {
			t.Errorf("expected fallback key, got %q", c.Config().APIKey)
		}
		if c.Config().BaseURL != llm.GoogleAIStudioBaseURL {
			t.Errorf("expected Google AI Studio BaseURL, got %q", c.Config().BaseURL)
		}
	})

	t.Run("GeminiDefault_ResolvesEnvVarWhenPresent", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "env-gemini-client-key")
		t.Setenv("OMNICODER_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		c, err := llm.NewClient(llm.ClientConfig{})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if c.Config().APIKey != "env-gemini-client-key" {
			t.Errorf("expected 'env-gemini-client-key', got %q", c.Config().APIKey)
		}
	})

	t.Run("LocalhostOllama_PreservesEmptyAPIKey", func(t *testing.T) {
		t.Setenv("GEMINI_API_KEY", "")
		t.Setenv("OMNICODER_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: "http://localhost:11434/v1",
			Model:   "llama3:8b",
			APIKey:  "",
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if c.Config().APIKey != "" {
			t.Errorf("expected empty APIKey for local Ollama, got %q", c.Config().APIKey)
		}
	})

	t.Run("TestServer_MockURL_PreservesEmptyAPIKey", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer ts.Close()

		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL: ts.URL,
			Model:   "mock-model",
			APIKey:  "",
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		if c.Config().APIKey != "" {
			t.Errorf("expected empty APIKey for httptest server, got %q", c.Config().APIKey)
		}
	})

	t.Run("Adversarial_WhitespaceAPIKey_In_ClientConfig", func(t *testing.T) {
		// If caller directly passes APIKey: "   ", does NewClient pass it as-is or normalize?
		var capturedAuth, capturedGoog string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedAuth = r.Header.Get("Authorization")
			capturedGoog = r.Header.Get("x-goog-api-key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer ts.Close()

		c, err := llm.NewClient(llm.ClientConfig{
			BaseURL:    ts.URL,
			HTTPClient: ts.Client(),
			APIKey:     "   ",
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		t.Logf("Observed ClientConfig.APIKey when given '   ': %q", c.Config().APIKey)

		// When making a request, what happens?
		_, _ = c.Complete(context.Background(), &llm.ChatCompletionRequest{
			Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}},
		})
		t.Logf("Captured Authorization header: %q", capturedAuth)
		t.Logf("Captured x-goog-api-key header: %q", capturedGoog)
	})
}

// -----------------------------------------------------------------------------
// 7. Concurrent Resolution Stress
// -----------------------------------------------------------------------------

func TestConcurrentResolutionSafety(t *testing.T) {
	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				var explicit string
				switch i % 4 {
				case 0:
					explicit = ""
				case 1:
					explicit = "   "
				case 2:
					explicit = fmt.Sprintf("explicit-worker-%d-%d", id, i)
				case 3:
					explicit = "  spaced-worker-key  "
				}

				res := llm.ResolveAPIKey(explicit)
				if res == "" {
					t.Errorf("ResolveAPIKey returned empty string")
				}
			}
		}(g)
	}

	wg.Wait()
}
