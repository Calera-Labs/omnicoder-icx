// SPDX-License-Identifier: Apache-2.0
// Package llm_test implements empirical adversarial stress testing for LLM decision gates:
// 1. Adversarial Token Budget Fuzzing:
//    - Pass massive inputs (50,000+ chars, giant repo maps, 1,000-line context snippets)
//      into GateIntent, GateEdit, GateHeal, and GateFinalize.
//    - Verify EstimateTokens(sysPrompt + " " + userPrompt) < 300 under any circumstance.
// 2. Fuzz ExtractJSON with Corrupt Inputs:
//    - DeepSeek <think> blocks with reasoning, nested braces, leading/trailing preambles.
//    - Markdown code fences with leading/trailing text, unclosed tags, multiple fences.
//    - Trailing commas in objects and arrays.
//    - Unescaped quotes, truncated JSON mid-stream (verify NO panics).
//    - Pure Aider blocks (verify ExtractJSON error and DecisionGate resilient fallback).
//    - Random binary and syntactic fuzzing (zero panics).
// 3. GateFinalize Safety Clamp:
//    - Force LLM/Mock responses with Approved=true under exit codes 1, 2, 127, -1.
//    - Verify Approved is strictly clamped to false with non-empty Reason.
// 4. Concurrency Stress:
//    - Test MockDecisionGate with 60+ concurrent callers under go test -v -race.

package llm_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// PART 1: Adversarial Token Budget Fuzzing
// ============================================================================

func TestTokenBudget_GateIntent_MassiveInputs(t *testing.T) {
	t.Parallel()

	// 1. Massive 50,000+ char UserPrompt + 50,000+ char RepoMapSummary
	giantPrompt := strings.Repeat("Refactor concurrent LRU cache with TTL eviction and memory limits. ", 1000) // ~67,000 chars
	giantRepoMap := strings.Repeat("pkg/lru/lru.go: func NewCache(cap int) *Cache\npkg/lru/ttl.go: func Expire()\n", 800) // ~62,000 chars

	manyFocusFiles := make([]string, 500)
	for i := 0; i < 500; i++ {
		manyFocusFiles[i] = fmt.Sprintf("pkg/submodule%d/component%d.go", i, i)
	}

	testCases := []struct {
		name string
		card llm.GateIntentCard
	}{
		{
			name: "Massive_Prompt_And_RepoMap",
			card: llm.GateIntentCard{
				UserPrompt:     giantPrompt,
				RepoMapSummary: giantRepoMap,
				FocusFiles:     []string{"pkg/lru/lru.go", "pkg/lru/ttl.go"},
			},
		},
		{
			name: "500_Focus_Files",
			card: llm.GateIntentCard{
				UserPrompt:     "Short user task",
				RepoMapSummary: "Short repo map",
				FocusFiles:     manyFocusFiles,
			},
		},
		{
			name: "Pathological_Unbroken_Single_Word_50K",
			card: llm.GateIntentCard{
				UserPrompt:     strings.Repeat("X", 50000),
				RepoMapSummary: strings.Repeat("Y", 50000),
			},
		},
		{
			name: "Pathological_Spaces_And_Newlines_50K",
			card: llm.GateIntentCard{
				UserPrompt:     strings.Repeat(" \n\t  ", 10000),
				RepoMapSummary: strings.Repeat(" \r\n ", 10000),
			},
		},
		{
			name: "Massive_Multibyte_Unicode_CJK_Emoji",
			card: llm.GateIntentCard{
				UserPrompt:     strings.Repeat("🚀并发LRU缓存构建与单元测试验证✨", 2000), // ~70,000 bytes
				RepoMapSummary: strings.Repeat("文件/结构体/方法定义映射表：Cache.Get()", 2000),
			},
		},
		{
			name: "Empty_Inputs",
			card: llm.GateIntentCard{
				UserPrompt:     "",
				RepoMapSummary: "",
				FocusFiles:     nil,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sysPrompt, userPrompt, err := tc.card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt returned error: %v", err)
			}

			combined := sysPrompt + " " + userPrompt
			tokens := llm.EstimateTokens(combined)

			if tokens >= llm.MaxCardTokenBudget {
				t.Fatalf("VIOLATION: GateIntent tokens %d >= max budget %d (sys: %d, user: %d)",
					tokens, llm.MaxCardTokenBudget, llm.EstimateTokens(sysPrompt), llm.EstimateTokens(userPrompt))
			}
			if err := llm.ValidateTokenBudget(sysPrompt, userPrompt, llm.MaxCardTokenBudget); err != nil {
				t.Fatalf("VIOLATION: ValidateTokenBudget failed: %v", err)
			}
		})
	}
}

func TestTokenBudget_GateEdit_MassiveInputs(t *testing.T) {
	t.Parallel()

	// 1,000-line context snippet (approx 50,000+ chars)
	var snippetBuilder strings.Builder
	for i := 1; i <= 1000; i++ {
		snippetBuilder.WriteString(fmt.Sprintf("func ProcessWorkerStreamLine%d(ctx context.Context, item *Item) (bool, error) {\n\treturn item.Validate(ctx)\n}\n", i))
	}
	giantSnippet := snippetBuilder.String()
	giantInstruction := strings.Repeat("Optimize lock contention using sync.Pool and RWMutex atomic fast-path. ", 800)
	giantFilePath := strings.Repeat("very/deep/nested/path/to/enterprise/module/", 100) + "cache_impl.go"

	manyAST := make([]string, 500)
	for i := 0; i < 500; i++ {
		manyAST[i] = fmt.Sprintf("ASTSymbolNode_Worker%d", i)
	}

	testCases := []struct {
		name string
		card llm.GateEditCard
	}{
		{
			name: "1000Line_ContextSnippet_And_GiantInstruction",
			card: llm.GateEditCard{
				FilePath:        "pkg/lru/lru.go",
				Instruction:     giantInstruction,
				ContextSnippet:  giantSnippet,
				ASTNeighborhood: []string{"Cache", "Node", "Evict"},
			},
		},
		{
			name: "500_AST_Neighborhood_Nodes",
			card: llm.GateEditCard{
				FilePath:        "pkg/lru/lru.go",
				Instruction:     "Add eviction hook",
				ContextSnippet:  "package lru\n\ntype Cache struct{}\n",
				ASTNeighborhood: manyAST,
			},
		},
		{
			name: "Massive_FilePath_50K",
			card: llm.GateEditCard{
				FilePath:        giantFilePath,
				Instruction:     "Fix signature",
				ContextSnippet:  "func Run() {}",
				ASTNeighborhood: nil,
			},
		},
		{
			name: "Unbroken_Pathological_Snippet_50K",
			card: llm.GateEditCard{
				FilePath:       "pkg/lru/lru.go",
				Instruction:    strings.Repeat("A", 10000),
				ContextSnippet: strings.Repeat("B", 50000),
			},
		},
		{
			name: "Empty_Edit_Card",
			card: llm.GateEditCard{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sysPrompt, userPrompt, err := tc.card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt returned error: %v", err)
			}

			combined := sysPrompt + " " + userPrompt
			tokens := llm.EstimateTokens(combined)

			if tokens >= llm.MaxCardTokenBudget {
				t.Fatalf("VIOLATION: GateEdit tokens %d >= max budget %d (sys: %d, user: %d)",
					tokens, llm.MaxCardTokenBudget, llm.EstimateTokens(sysPrompt), llm.EstimateTokens(userPrompt))
			}
			if err := llm.ValidateTokenBudget(sysPrompt, userPrompt, llm.MaxCardTokenBudget); err != nil {
				t.Fatalf("VIOLATION: ValidateTokenBudget failed: %v", err)
			}
		})
	}
}

func TestTokenBudget_GateHeal_MassiveInputs(t *testing.T) {
	t.Parallel()

	// 1,000-line error snippet & massive compiler diagnostics
	var traceBuilder strings.Builder
	for i := 1; i <= 1000; i++ {
		if i == 500 {
			traceBuilder.WriteString(fmt.Sprintf("> %4d: c.cap = Capacity // undefined identifier\n", i))
		} else {
			traceBuilder.WriteString(fmt.Sprintf("  %4d: println(\"debugging line %d\")\n", i, i))
		}
	}
	giantTrace := traceBuilder.String()
	giantErrorMsg := strings.Repeat("compiler diagnostic: undefined symbol Capacity at declaration site; ", 800)
	giantCommand := "go test -v -race -run TestCacheEvictionStress " + strings.Repeat("--flag=arg ", 1000)

	testCases := []struct {
		name string
		card llm.GateHealCard
	}{
		{
			name: "1000Line_Compiler_Trace_And_GiantError",
			card: llm.GateHealCard{
				FilePath:      "pkg/lru/lru.go",
				ErrorLine:     500,
				ErrorMessage:  giantErrorMsg,
				Snippet5Lines: giantTrace,
				Command:       giantCommand,
				ErrorCode:     "compiler_err_undefined_var",
			},
		},
		{
			name: "Pathological_Unbroken_Error_50K",
			card: llm.GateHealCard{
				FilePath:      "pkg/lru/lru.go",
				ErrorLine:     42,
				ErrorMessage:  strings.Repeat("ERR", 20000),
				Snippet5Lines: strings.Repeat("CODE", 15000),
				Command:       "go test",
			},
		},
		{
			name: "Empty_Heal_Card",
			card: llm.GateHealCard{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sysPrompt, userPrompt, err := tc.card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt returned error: %v", err)
			}

			combined := sysPrompt + " " + userPrompt
			tokens := llm.EstimateTokens(combined)

			if tokens >= llm.MaxCardTokenBudget {
				t.Fatalf("VIOLATION: GateHeal tokens %d >= max budget %d (sys: %d, user: %d)",
					tokens, llm.MaxCardTokenBudget, llm.EstimateTokens(sysPrompt), llm.EstimateTokens(userPrompt))
			}
			if err := llm.ValidateTokenBudget(sysPrompt, userPrompt, llm.MaxCardTokenBudget); err != nil {
				t.Fatalf("VIOLATION: ValidateTokenBudget failed: %v", err)
			}
		})
	}
}

func TestTokenBudget_GateFinalize_MassiveInputs(t *testing.T) {
	t.Parallel()

	// Massive 50,000+ char test output summary
	giantTestOutput := strings.Repeat("=== RUN TestConcurrentCache\n=== PAUSE TestConcurrentCache\n=== CONT TestConcurrentCache\n--- PASS: TestConcurrentCache (0.01s)\n", 500)
	giantTaskDesc := strings.Repeat("Build and verify complete LRU cache with TTL eviction and memory limits. ", 700)

	manyFiles := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		manyFiles[i] = fmt.Sprintf("pkg/module%d/sub%d/file%d.go", i, i, i)
	}

	testCases := []struct {
		name string
		card llm.GateFinalizeCard
	}{
		{
			name: "500_Pass_Outputs_And_GiantTask",
			card: llm.GateFinalizeCard{
				TaskDescription:   giantTaskDesc,
				FilesModified:     []string{"pkg/lru/lru.go", "pkg/lru/lru_test.go"},
				TestCommand:       "go test -v -race ./pkg/lru",
				TestExitCode:      0,
				TestOutputSummary: giantTestOutput,
			},
		},
		{
			name: "1000_Modified_Files",
			card: llm.GateFinalizeCard{
				TaskDescription:   "Batch refactoring across workspace",
				FilesModified:     manyFiles,
				TestCommand:       "go test ./pkg/...",
				TestExitCode:      0,
				TestOutputSummary: "PASS",
			},
		},
		{
			name: "Pathological_Unbroken_TestOutput_50K",
			card: llm.GateFinalizeCard{
				TaskDescription:   strings.Repeat("TASK", 10000),
				FilesModified:     []string{"main.go"},
				TestCommand:       strings.Repeat("CMD", 5000),
				TestExitCode:      1,
				TestOutputSummary: strings.Repeat("STDOUT", 10000),
			},
		},
		{
			name: "Empty_Finalize_Card",
			card: llm.GateFinalizeCard{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			sysPrompt, userPrompt, err := tc.card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt returned error: %v", err)
			}

			combined := sysPrompt + " " + userPrompt
			tokens := llm.EstimateTokens(combined)

			if tokens >= llm.MaxCardTokenBudget {
				t.Fatalf("VIOLATION: GateFinalize tokens %d >= max budget %d (sys: %d, user: %d)",
					tokens, llm.MaxCardTokenBudget, llm.EstimateTokens(sysPrompt), llm.EstimateTokens(userPrompt))
			}
			if err := llm.ValidateTokenBudget(sysPrompt, userPrompt, llm.MaxCardTokenBudget); err != nil {
				t.Fatalf("VIOLATION: ValidateTokenBudget failed: %v", err)
			}
		})
	}
}

// ============================================================================
// PART 2: Fuzz ExtractJSON with Corrupt & Malformed Inputs
// ============================================================================

func TestExtractJSON_DeepSeekThinkBlocks(t *testing.T) {
	t.Parallel()

	type IntentPayload struct {
		Operation   string   `json:"operation"`
		TargetFiles []string `json:"target_files"`
		Rationale   string   `json:"rationale"`
	}

	testCases := []struct {
		name        string
		input       string
		expectOp    string
		expectFiles []string
	}{
		{
			name: "Standard_Think_Block_With_Fenced_JSON",
			input: "<think>\nLet me analyze the repo.\nI need to create the cache.\n</think>\n" +
				"```json\n" +
				`{"operation":"CREATE","target_files":["pkg/lru/lru.go"],"rationale":"need cache"}` +
				"\n```",
			expectOp:    "CREATE",
			expectFiles: []string{"pkg/lru/lru.go"},
		},
		{
			name: "Think_Block_With_Nested_JSON_Inside_Thinking",
			input: "<think>\nMaybe I should do {\"operation\":\"INVALID\"}? No, that is wrong.\n</think>\n" +
				`{"operation":"MODIFY","target_files":["pkg/lru/lru.go"],"rationale":"correct"}` ,
			expectOp:    "MODIFY",
			expectFiles: []string{"pkg/lru/lru.go"},
		},
		{
			name: "Think_Block_With_Leading_Whitespace_And_Newlines",
			input: "\n\n   <think>\nThinking step 1... step 2...\n</think>\n\n" +
				"```\n" +
				`{"operation":"REFACTOR","target_files":["pkg/devops/repomap.go"],"rationale":"clean"}` +
				"\n```",
			expectOp:    "REFACTOR",
			expectFiles: []string{"pkg/devops/repomap.go"},
		},
		{
			name: "Empty_Think_Block",
			input: "<think></think>" +
				`{"operation":"EXPLAIN","target_files":["README.md"],"rationale":"document"}`,
			expectOp:    "EXPLAIN",
			expectFiles: []string{"README.md"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var out IntentPayload
			err := llm.ExtractJSON(tc.input, &out)
			if err != nil {
				t.Fatalf("ExtractJSON failed: %v", err)
			}
			if out.Operation != tc.expectOp {
				t.Errorf("expected op %s, got %s", tc.expectOp, out.Operation)
			}
			if len(out.TargetFiles) != len(tc.expectFiles) || out.TargetFiles[0] != tc.expectFiles[0] {
				t.Errorf("expected files %v, got %v", tc.expectFiles, out.TargetFiles)
			}
		})
	}
}

func TestExtractJSON_MarkdownCodeFences(t *testing.T) {
	t.Parallel()

	type DecisionPayload struct {
		Operation   string   `json:"operation"`
		TargetFiles []string `json:"target_files"`
	}

	testCases := []struct {
		name        string
		input       string
		expectOp    string
		expectFiles int
	}{
		{
			name: "Fences_With_Leading_Conversational_Preambles",
			input: "Hello developer! I have analyzed your request.\nHere is the resulting plan:\n" +
				"```json\n" +
				`{"operation":"CREATE","target_files":["pkg/lru/lru.go","pkg/lru/lru_test.go"]}` +
				"\n```\nLet me know if you need modifications.",
			expectOp:    "CREATE",
			expectFiles: 2,
		},
		{
			name: "Untagged_Fences_With_Extra_Whitespace",
			input: "  ```  \n" +
				`{"operation":"MODIFY","target_files":["pkg/lru/lru.go"]}` +
				"\n  ```  ",
			expectOp:    "MODIFY",
			expectFiles: 1,
		},
		{
			name: "Embedded_Code_Braces_In_String_Values",
			input: "```json\n" +
				`{"operation":"MODIFY","target_files":["pkg/lru/lru.go"],"code_sample":"func() { if x { return } }"}` +
				"\n```",
			expectOp:    "MODIFY",
			expectFiles: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var out DecisionPayload
			err := llm.ExtractJSON(tc.input, &out)
			if err != nil {
				t.Fatalf("ExtractJSON failed: %v", err)
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

func TestExtractJSON_TrailingCommas(t *testing.T) {
	t.Parallel()

	type EditPayload struct {
		FilePath   string              `json:"file_path"`
		EditBlocks []llm.EditBlockData `json:"edit_blocks"`
	}

	// Multiple nested trailing commas in objects and arrays
	input := `{
		"file_path": "pkg/lru/lru.go",
		"edit_blocks": [
			{
				"search_text": "type Cache struct {}",
				"replace_text": "type Cache struct { mu sync.RWMutex }",
			},
		],
	}`

	var out EditPayload
	err := llm.ExtractJSON(input, &out)
	if err != nil {
		t.Fatalf("ExtractJSON failed on trailing commas: %v", err)
	}

	if out.FilePath != "pkg/lru/lru.go" {
		t.Errorf("expected file_path pkg/lru/lru.go, got %s", out.FilePath)
	}
	if len(out.EditBlocks) != 1 {
		t.Fatalf("expected 1 edit block, got %d", len(out.EditBlocks))
	}
	if !strings.Contains(out.EditBlocks[0].ReplaceText, "sync.RWMutex") {
		t.Errorf("unexpected replace text: %s", out.EditBlocks[0].ReplaceText)
	}
}

func TestExtractJSON_CorruptAndTruncated_ZeroPanics(t *testing.T) {
	t.Parallel()

	corruptInputs := []string{
		// Unescaped quotes inside string
		`{"operation":"MODIFY", "rationale": "He said "fix it" now"}`,
		// Truncated mid-string
		`{"operation":"MODIFY", "target_files": ["pkg/lru/lru.go"`,
		// Truncated mid-object
		`{"operation":"MODIFY", "target_files":`,
		// Truncated at opening
		`{"operation":`,
		`{`,
		// Empty and whitespace
		``,
		`   `,
		"\n\t\r\n",
		// Pure non-JSON text
		"I am an LLM and I refuse to output JSON because of my guidelines.",
		// Random unmatched braces
		`}{`,
		`{{{{{`,
		`}}}}}`,
		// Deeply nested empty braces (100 levels)
		strings.Repeat("{", 100) + strings.Repeat("}", 100),
		// Unclosed strings with escaped slashes
		`{"test": "\\\\\\\\\"`,
		// Pure null bytes and control codes
		"\x00\x01\x02\x03\x04\x05",
	}

	for idx, corrupt := range corruptInputs {
		t.Run(fmt.Sprintf("Corrupt_%d", idx), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on corrupt input %d (%q): %v", idx, corrupt, r)
				}
			}()

			var out map[string]any
			err := llm.ExtractJSON(corrupt, &out)
			if err == nil {
				// If it somehow unmarshaled without error (e.g. 100 nested braces), that's fine as long as no panic
				t.Logf("corrupt input %d unmarshaled successfully", idx)
			}
		})
	}
}

func TestExtractJSON_PureAiderBlocks_AndGateFallback(t *testing.T) {
	t.Parallel()

	rawAiderBlock := `Here is the search/replace block to apply:
<<<<<<< SEARCH
func (c *Cache) Get(key string) (any, bool) {
	return nil, false
}
=======
func (c *Cache) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.items[key]
	return val, ok
}
>>>>>>> REPLACE
`

	// 1. Direct ExtractJSON on pure Aider block: MUST return error (NOT valid JSON) without panic
	var dummy map[string]any
	err := llm.ExtractJSON(rawAiderBlock, &dummy)
	if err == nil {
		t.Fatalf("expected ExtractJSON to return error on pure Aider block, got nil")
	}

	// 2. DecisionGate DecideEdit MUST gracefully fall back to parsing raw Aider block
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = rawAiderBlock

	gate := llm.NewDecisionGate(mockClient, "mock-model")
	editDec, err := gate.DecideEdit(context.Background(), llm.GateEditCard{
		FilePath:    "pkg/lru/lru.go",
		Instruction: "implement thread-safe Get",
	})
	if err != nil {
		t.Fatalf("DecideEdit failed to parse Aider fallback: %v", err)
	}

	if len(editDec.EditBlocks) != 1 {
		t.Fatalf("expected 1 edit block from Aider fallback, got %d", len(editDec.EditBlocks))
	}
	if !strings.Contains(editDec.EditBlocks[0].ReplaceText, "c.mu.RLock()") {
		t.Errorf("unexpected replace text in fallback: %s", editDec.EditBlocks[0].ReplaceText)
	}

	// 3. DecisionGate DecideHeal MUST also gracefully fall back to parsing raw Aider block
	healDec, err := gate.DecideHeal(context.Background(), llm.GateHealCard{
		FilePath:      "pkg/lru/lru.go",
		ErrorLine:     10,
		ErrorMessage:  "data race detected",
		Snippet5Lines: "> 10: return c.items[key]\n",
		Command:       "go test -race",
	})
	if err != nil {
		t.Fatalf("DecideHeal failed to parse Aider fallback: %v", err)
	}
	if len(healDec.EditBlocks) != 1 {
		t.Fatalf("expected 1 heal edit block from Aider fallback, got %d", len(healDec.EditBlocks))
	}
}

// ============================================================================
// PART 3: GateFinalize Safety Clamp Under Failing Exit Codes
// ============================================================================

func TestSpeculativeStress_GateFinalize_SafetyClamp(t *testing.T) {
	t.Parallel()

	failingExitCodes := []int{1, 2, 127, -1, 255}

	for _, exitCode := range failingExitCodes {
		t.Run(fmt.Sprintf("LLMDecisionGate_ExitCode_%d", exitCode), func(t *testing.T) {
			mockClient := llm.NewMockLLMClient()
			// Adversarial LLM maliciously attempts to approve commit despite test failure
			mockClient.CannedContent = fmt.Sprintf(`{
				"approved": true,
				"commit_message": "feat(core): maliciously approved commit with exit code %d",
				"summary": "all tests passed according to model hallucinations"
			}`, exitCode)

			gate := llm.NewDecisionGate(mockClient, "mock-model")

			card := llm.GateFinalizeCard{
				TaskDescription:   "Implement LRU cache",
				FilesModified:     []string{"pkg/lru/lru.go"},
				TestCommand:       "go test -v -race ./pkg/lru",
				TestExitCode:      exitCode,
				TestOutputSummary: fmt.Sprintf("FAIL: TestLRU failed with exit status %d", exitCode),
			}

			dec, err := gate.DecideFinalize(context.Background(), card)
			if err != nil {
				t.Fatalf("unexpected DecideFinalize error: %v", err)
			}

			// STRICT INVARIANT SAFETY CLAMP
			if dec.Approved {
				t.Fatalf("SAFETY VIOLATION: GateFinalize approved commit when TestExitCode was %d!", exitCode)
			}
			if dec.Reason == "" {
				t.Errorf("expected non-empty rejection reason when exit code is %d", exitCode)
			}
			if !strings.Contains(dec.Reason, fmt.Sprintf("%d", exitCode)) {
				t.Errorf("expected reason to cite exit code %d, got: %s", exitCode, dec.Reason)
			}

			// Also verify interface wrapper Finalize()
			wrapperDec, err := gate.Finalize(context.Background(), card)
			if err != nil {
				t.Fatalf("Finalize wrapper error: %v", err)
			}
			if wrapperDec.Approved {
				t.Fatalf("SAFETY VIOLATION: Finalize wrapper approved commit when TestExitCode was %d!", exitCode)
			}
		})

		t.Run(fmt.Sprintf("MockDecisionGate_ExitCode_%d", exitCode), func(t *testing.T) {
			mockGate := llm.NewMockDecisionGate()

			card := llm.GateFinalizeCard{
				TaskDescription:   "Implement LRU cache",
				FilesModified:     []string{"pkg/lru/lru.go"},
				TestCommand:       "go test ./pkg/lru",
				TestExitCode:      exitCode,
				TestOutputSummary: fmt.Sprintf("exit status %d", exitCode),
			}

			dec, err := mockGate.DecideFinalize(context.Background(), card)
			if err != nil {
				t.Fatalf("unexpected DecideFinalize error: %v", err)
			}

			// STRICT INVARIANT SAFETY CLAMP
			if dec.Approved {
				t.Fatalf("SAFETY VIOLATION: MockDecisionGate approved commit when TestExitCode was %d!", exitCode)
			}
			if dec.CommitMessage != "" {
				t.Errorf("expected empty commit message on rejection, got %q", dec.CommitMessage)
			}
			if dec.FilesCommitted != nil {
				t.Errorf("expected nil FilesCommitted on rejection, got %v", dec.FilesCommitted)
			}
		})
	}

	t.Run("ExitCode_0_Approved_When_Valid", func(t *testing.T) {
		mockClient := llm.NewMockLLMClient()
		mockClient.CannedContent = `{"approved": true, "commit_message": "feat(lru): add concurrent cache", "summary": "tests green"}`

		gate := llm.NewDecisionGate(mockClient, "mock-model")
		dec, err := gate.DecideFinalize(context.Background(), llm.GateFinalizeCard{
			TaskDescription:   "Add concurrent cache",
			FilesModified:     []string{"pkg/lru/lru.go"},
			TestCommand:       "go test ./pkg/lru",
			TestExitCode:      0,
			TestOutputSummary: "PASS",
		})
		if err != nil {
			t.Fatalf("unexpected DecideFinalize error: %v", err)
		}
		if !dec.Approved {
			t.Errorf("expected Approved=true when TestExitCode=0")
		}
		if dec.CommitMessage != "feat(lru): add concurrent cache" {
			t.Errorf("unexpected commit message: %s", dec.CommitMessage)
		}
	})
}

// ============================================================================
// PART 4: Concurrency Stress Testing (60+ Concurrent Callers under -race)
// ============================================================================

func TestMockDecisionGate_HighConcurrencyStress(t *testing.T) {
	t.Parallel()

	gate := llm.NewMockDecisionGate()
	ctx := context.Background()

	numWorkers := 60
	iterationsPerWorker := 40
	var totalSuccessfulCalls atomic.Int64

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for workerID := 0; workerID < numWorkers; workerID++ {
		go func(id int) {
			defer wg.Done()

			for it := 0; it < iterationsPerWorker; it++ {
				op := (id + it) % 4
				switch op {
				case 0:
					dec, err := gate.DecideIntent(ctx, llm.GateIntentCard{
						UserPrompt: fmt.Sprintf("Worker %d task iteration %d: refactor pkg/lru/lru.go", id, it),
					})
					if err == nil && dec != nil {
						totalSuccessfulCalls.Add(1)
					}
				case 1:
					dec, err := gate.DecideEdit(ctx, llm.GateEditCard{
						FilePath:       fmt.Sprintf("pkg/worker%d/file_%d.go", id, it),
						Instruction:    "add atomic counter",
						ContextSnippet: "type Worker struct {}\n",
					})
					if err == nil && dec != nil {
						totalSuccessfulCalls.Add(1)
					}
				case 2:
					dec, err := gate.DecideHeal(ctx, llm.GateHealCard{
						FilePath:      fmt.Sprintf("pkg/worker%d/file_%d.go", id, it),
						ErrorLine:     it + 1,
						ErrorMessage:  "undefined reference",
						Snippet5Lines: fmt.Sprintf("> %d: x := undefinedSymbol\n", it+1),
						Command:       "go build",
					})
					if err == nil && dec != nil {
						totalSuccessfulCalls.Add(1)
					}
				case 3:
					dec, err := gate.DecideFinalize(ctx, llm.GateFinalizeCard{
						TaskDescription:   fmt.Sprintf("Finalize worker %d iteration %d", id, it),
						FilesModified:     []string{fmt.Sprintf("pkg/worker%d/file_%d.go", id, it)},
						TestCommand:       "go test",
						TestExitCode:      0,
						TestOutputSummary: "PASS",
					})
					if err == nil && dec != nil {
						totalSuccessfulCalls.Add(1)
					}
				}

				// Interspersed state reading under concurrent mutation
				if it%10 == 0 {
					_ = gate.CallCount()
					_ = gate.LastIntent()
					_ = gate.LastEdit()
					_ = gate.LastHeal()
					_ = gate.LastFinalize()
				}
			}
		}(workerID)
	}

	// Concurrent observer goroutine querying call statistics
	stopObserver := make(chan struct{})
	var observerWg sync.WaitGroup
	observerWg.Add(1)
	go func() {
		defer observerWg.Done()
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopObserver:
				return
			case <-ticker.C:
				_ = gate.CallCount()
				_ = gate.LastIntent()
				_ = gate.LastFinalize()
			}
		}
	}()

	wg.Wait()
	close(stopObserver)
	observerWg.Wait()

	expectedCalls := numWorkers * iterationsPerWorker
	if gate.CallCount() != expectedCalls {
		t.Fatalf("CallCount mismatch: expected %d, got %d", expectedCalls, gate.CallCount())
	}
	if totalSuccessfulCalls.Load() != int64(expectedCalls) {
		t.Fatalf("Successful calls mismatch: expected %d, got %d", expectedCalls, totalSuccessfulCalls.Load())
	}

	// Verify Reset under concurrency
	gate.Reset()
	if gate.CallCount() != 0 {
		t.Fatalf("expected 0 calls after Reset, got %d", gate.CallCount())
	}
}

// ============================================================================
// PART 5: Randomized Boundary Fuzzer
// ============================================================================

func TestFuzz_EstimateTokens_And_ValidateTokenBudget(t *testing.T) {
	t.Parallel()

	// Deterministic pseudo-random fuzzing over 200 random inputs
	for i := 0; i < 200; i++ {
		strLenBig, _ := rand.Int(rand.Reader, big.NewInt(1000))
		strLen := int(strLenBig.Int64())

		var sb strings.Builder
		for j := 0; j < strLen; j++ {
			b, _ := rand.Int(rand.Reader, big.NewInt(128))
			ch := byte(b.Int64())
			if ch == 0 {
				ch = ' '
			}
			sb.WriteByte(ch)
		}
		raw := sb.String()

		tok := llm.EstimateTokens(raw)
		if strLen == 0 && tok != 0 {
			t.Fatalf("expected 0 tokens for empty string, got %d", tok)
		}
		if strLen > 0 && tok < 1 {
			t.Fatalf("expected at least 1 token for non-empty string, got %d", tok)
		}

		// Verify ValidateTokenBudget behaves consistently
		err := llm.ValidateTokenBudget("sys", raw, 100)
		sysAndUserTok := llm.EstimateTokens("sys " + raw)
		if sysAndUserTok >= 100 && err == nil {
			t.Fatalf("expected ValidateTokenBudget error when tokens %d >= 100", sysAndUserTok)
		}
		if sysAndUserTok < 100 && err != nil {
			t.Fatalf("expected ValidateTokenBudget nil when tokens %d < 100, got: %v", sysAndUserTok, err)
		}
	}
}
