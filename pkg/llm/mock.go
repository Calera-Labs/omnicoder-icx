// SPDX-License-Identifier: Apache-2.0
// Package llm provides a deterministic, zero-network mock decision gate and wire client
// for testing OmniCoder-ICX without remote network calls or live model inference.

package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	filePathRegex  = regexp.MustCompile(`(?i)\b(?:[a-zA-Z0-9_\-\./\\]+/[a-zA-Z0-9_\-\./\\]+\.(?:go|ts|tsx|js|jsx|py|rs|cpp|hpp|html|css|json|yaml|yml|md|sql|sh)|[a-zA-Z0-9_\-]+\.(?:go|ts|tsx|js|jsx|py|rs|cpp|hpp|html|css|json|yaml|yml|md|sql|sh))\b`)
	errorLineRegex = regexp.MustCompile(`^>\s*(\d+):\s*(.*)$`)
)

// ============================================================================
// MockDecisionGate
// ============================================================================

// MockDecisionGate implements DecisionGate with configurable heuristics,
// programmable callbacks, failure injection, and thread-safe call recording.
type MockDecisionGate struct {
	mu sync.Mutex

	// Programmable callbacks (take precedence if set)
	IntentFunc     func(ctx context.Context, card GateIntentCard) (*IntentDecision, error)
	EditFunc       func(ctx context.Context, card GateEditCard) (*EditDecision, error)
	HealFunc       func(ctx context.Context, card GateHealCard) (*HealDecision, error)
	FinalizeFunc   func(ctx context.Context, card GateFinalizeCard) (*FinalizeDecision, error)
	ReproducerFunc func(ctx context.Context, card GateReproducerCard) (*ReproducerDecision, error)

	// Canned static responses (used if Func is nil and field is non-nil)
	CannedIntent     *IntentDecision
	CannedEdit       *EditDecision
	CannedHeal       *HealDecision
	CannedFinalize   *FinalizeDecision
	CannedReproducer *ReproducerDecision

	// Failure injection controls
	InjectError           error         // General error returned by all methods
	InjectIntentError     error         // Error returned by DecideIntent
	InjectEditError       error         // Error returned by DecideEdit
	InjectHealError       error         // Error returned by DecideHeal
	InjectFinalizeError   error         // Error returned by DecideFinalize
	InjectReproducerError error         // Error returned by DecideReproducer
	InjectTimeout         time.Duration // Delay injected before returning
	SimulateRejection     bool          // Forces Edit and Finalize to reject
	FailAttemptsRemaining int           // Fails N times with transient error, then succeeds

	// Thread-safe call history
	IntentCalls     []GateIntentCard
	EditCalls       []GateEditCard
	HealCalls       []GateHealCard
	FinalizeCalls   []GateFinalizeCard
	ReproducerCalls []GateReproducerCard
}

// Ensure MockDecisionGate implements DecisionGate.
var _ DecisionGate = (*MockDecisionGate)(nil)

// NewMockDecisionGate initializes an empty, ready-to-use MockDecisionGate.
func NewMockDecisionGate() *MockDecisionGate {
	return &MockDecisionGate{
		IntentCalls:     make([]GateIntentCard, 0),
		EditCalls:       make([]GateEditCard, 0),
		HealCalls:       make([]GateHealCard, 0),
		FinalizeCalls:   make([]GateFinalizeCard, 0),
		ReproducerCalls: make([]GateReproducerCard, 0),
	}
}

// Reset clears recorded calls and failure injection counters.
func (m *MockDecisionGate) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.IntentCalls = m.IntentCalls[:0]
	m.EditCalls = m.EditCalls[:0]
	m.HealCalls = m.HealCalls[:0]
	m.FinalizeCalls = m.FinalizeCalls[:0]
	m.ReproducerCalls = m.ReproducerCalls[:0]

	m.IntentFunc = nil
	m.EditFunc = nil
	m.HealFunc = nil
	m.FinalizeFunc = nil
	m.ReproducerFunc = nil

	m.CannedIntent = nil
	m.CannedEdit = nil
	m.CannedHeal = nil
	m.CannedFinalize = nil
	m.CannedReproducer = nil

	m.InjectError = nil
	m.InjectIntentError = nil
	m.InjectEditError = nil
	m.InjectHealError = nil
	m.InjectFinalizeError = nil
	m.InjectReproducerError = nil
	m.InjectTimeout = 0
	m.SimulateRejection = false
	m.FailAttemptsRemaining = 0
}

// CallCount returns the total number of gate decisions evaluated.
func (m *MockDecisionGate) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.IntentCalls) + len(m.EditCalls) + len(m.HealCalls) + len(m.FinalizeCalls) + len(m.ReproducerCalls)
}

// LastIntent returns the most recent GateIntentCard evaluated, or nil.
func (m *MockDecisionGate) LastIntent() *GateIntentCard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.IntentCalls) == 0 {
		return nil
	}
	card := m.IntentCalls[len(m.IntentCalls)-1]
	return &card
}

// LastEdit returns the most recent GateEditCard evaluated, or nil.
func (m *MockDecisionGate) LastEdit() *GateEditCard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.EditCalls) == 0 {
		return nil
	}
	card := m.EditCalls[len(m.EditCalls)-1]
	return &card
}

// LastHeal returns the most recent GateHealCard evaluated, or nil.
func (m *MockDecisionGate) LastHeal() *GateHealCard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.HealCalls) == 0 {
		return nil
	}
	card := m.HealCalls[len(m.HealCalls)-1]
	return &card
}

// LastFinalize returns the most recent GateFinalizeCard evaluated, or nil.
func (m *MockDecisionGate) LastFinalize() *GateFinalizeCard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.FinalizeCalls) == 0 {
		return nil
	}
	card := m.FinalizeCalls[len(m.FinalizeCalls)-1]
	return &card
}

// LastReproducer returns the most recent GateReproducerCard evaluated, or nil.
func (m *MockDecisionGate) LastReproducer() *GateReproducerCard {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ReproducerCalls) == 0 {
		return nil
	}
	card := m.ReproducerCalls[len(m.ReproducerCalls)-1]
	return &card
}

// handleTimeout sleeps for InjectTimeout, respecting context cancellation.
func (m *MockDecisionGate) handleTimeout(ctx context.Context) error {
	m.mu.Lock()
	delay := m.InjectTimeout
	m.mu.Unlock()

	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// checkFailAttempts checks and decrements FailAttemptsRemaining.
func (m *MockDecisionGate) checkFailAttempts() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.FailAttemptsRemaining > 0 {
		m.FailAttemptsRemaining--
		return errors.New("llm: simulated transient failure (countdown active)")
	}
	return nil
}

// DecideIntent evaluates repository state and prompt using programmable overrides or heuristics.
func (m *MockDecisionGate) DecideIntent(ctx context.Context, card GateIntentCard) (*IntentDecision, error) {
	if err := m.handleTimeout(ctx); err != nil {
		return nil, err
	}
	if err := m.checkFailAttempts(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.IntentCalls = append(m.IntentCalls, card)
	customFunc := m.IntentFunc
	canned := m.CannedIntent
	genErr := m.InjectError
	specificErr := m.InjectIntentError
	m.mu.Unlock()

	if genErr != nil {
		return nil, genErr
	}
	if specificErr != nil {
		return nil, specificErr
	}
	if customFunc != nil {
		return customFunc(ctx, card)
	}
	if canned != nil {
		res := *canned
		return &res, nil
	}

	// Rule-Based Heuristic Evaluation
	promptLower := strings.ToLower(card.UserPrompt)

	// 1. Determine Operation
	op := OpModify
	switch {
	case strings.Contains(promptLower, "refactor") || strings.Contains(promptLower, "rename") || strings.Contains(promptLower, "restructure"):
		op = OpRefactor
	case strings.Contains(promptLower, "create") || strings.Contains(promptLower, "new") || strings.Contains(promptLower, "add") || strings.Contains(promptLower, "implement") || strings.Contains(promptLower, "build"):
		op = OpCreate
	case strings.Contains(promptLower, "explain") || strings.Contains(promptLower, "why") || strings.Contains(promptLower, "describe") || strings.Contains(promptLower, "document"):
		op = OpExplain
	default:
		op = OpModify
	}

	// 2. Discover Target Files
	targetFiles := make([]string, 0)
	if len(card.FocusFiles) > 0 {
		for _, f := range card.FocusFiles {
			targetFiles = append(targetFiles, filepath.ToSlash(f))
		}
	} else {
		// Scan prompt for file paths
		matches := filePathRegex.FindAllString(card.UserPrompt, -1)
		for _, match := range matches {
			clean := filepath.ToSlash(strings.TrimSpace(match))
			lowerClean := strings.ToLower(clean)
			// Skip markdown plans, non-readme docs, URLs, web domains, and non-code assets
			isReadme := strings.EqualFold(filepath.Base(lowerClean), "readme.md")
			if (strings.HasSuffix(lowerClean, ".md") && !isReadme) || strings.Contains(clean, "://") || strings.HasPrefix(clean, "//") ||
				strings.Contains(lowerClean, ".edu") || strings.Contains(lowerClean, ".com") || strings.Contains(lowerClean, ".org") ||
				strings.Contains(lowerClean, ".io") || strings.Contains(lowerClean, ".gov") || strings.Contains(lowerClean, ".net") {
				continue
			}
			if !containsString(targetFiles, clean) {
				targetFiles = append(targetFiles, clean)
			}
			if len(targetFiles) >= 6 {
				break
			}
		}

		// Keyword mappings for common repository modules
		if len(targetFiles) == 0 {
			if strings.Contains(promptLower, "lru") || strings.Contains(promptLower, "cache") {
				targetFiles = []string{"pkg/lru/lru.go", "pkg/lru/lru_test.go"}
			} else if strings.Contains(promptLower, "repomap") {
				targetFiles = []string{"pkg/devops/repomap.go"}
			} else if strings.Contains(promptLower, "editblock") {
				targetFiles = []string{"pkg/devops/editblock.go"}
			} else if strings.Contains(promptLower, "feedback") {
				targetFiles = []string{"pkg/devops/feedback.go"}
			} else if strings.Contains(promptLower, "gitpatch") {
				targetFiles = []string{"pkg/devops/gitpatch.go"}
			} else if strings.Contains(promptLower, "client") || strings.Contains(promptLower, "gate") {
				targetFiles = []string{"pkg/llm/client.go", "pkg/llm/cards.go"}
			} else {
				// Scan repo map summary for files
				repoMatches := filePathRegex.FindAllString(card.RepoMapSummary, -1)
				for _, rm := range repoMatches {
					clean := filepath.ToSlash(strings.TrimSpace(rm))
					if strings.HasSuffix(strings.ToLower(clean), ".md") {
						continue
					}
					if !containsString(targetFiles, clean) {
						targetFiles = append(targetFiles, clean)
						if len(targetFiles) >= 2 {
							break
						}
					}
				}
				if len(targetFiles) == 0 {
					targetFiles = []string{"main.go"}
				}
			}
		}
	}

	return &IntentDecision{
		Operation:   op,
		TargetFiles: targetFiles,
		Rationale:   fmt.Sprintf("Mock gate resolved operation %s with %d target files from prompt keywords", op, len(targetFiles)),
		Confidence:  0.95,
	}, nil
}

// DecideEdit synthesizes search/replace edit blocks using programmable overrides or heuristics.
func (m *MockDecisionGate) DecideEdit(ctx context.Context, card GateEditCard) (*EditDecision, error) {
	if err := m.handleTimeout(ctx); err != nil {
		return nil, err
	}
	if err := m.checkFailAttempts(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.EditCalls = append(m.EditCalls, card)
	customFunc := m.EditFunc
	canned := m.CannedEdit
	genErr := m.InjectError
	specificErr := m.InjectEditError
	rejected := m.SimulateRejection
	m.mu.Unlock()

	if genErr != nil {
		return nil, genErr
	}
	if specificErr != nil {
		return nil, specificErr
	}
	if customFunc != nil {
		return customFunc(ctx, card)
	}
	if canned != nil {
		res := *canned
		return &res, nil
	}
	if rejected {
		return &EditDecision{
			FilePath:    card.FilePath,
			EditBlocks:  nil,
			Explanation: "mock gate edit rejected by policy simulation",
		}, nil
	}

	// Rule-Based Heuristic Evaluation
	targetPath := filepath.ToSlash(card.FilePath)
	editBlocks := make([]EditBlockData, 0)

	cleanInst := strings.TrimSpace(card.Instruction)
	if idx := strings.Index(cleanInst, "\n"); idx != -1 {
		cleanInst = strings.TrimSpace(cleanInst[:idx])
	}
	if len(cleanInst) > 120 {
		cleanInst = cleanInst[:120] + "..."
	}

	if strings.TrimSpace(card.ContextSnippet) == "" {
		// New file creation heuristic with format-aware boilerplate
		var content string
		lower := strings.ToLower(targetPath)
		switch {
		case strings.HasSuffix(lower, ".html"):
			content = fmt.Sprintf("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n  <meta charset=\"UTF-8\">\n  <title>%s</title>\n  <link rel=\"stylesheet\" href=\"style.css\">\n</head>\n<body>\n  <div id=\"app\">\n    <h1>%s</h1>\n    <canvas id=\"canvas\"></canvas>\n  </div>\n  <script src=\"app.js\"></script>\n</body>\n</html>\n", cleanInst, cleanInst)
		case strings.HasSuffix(lower, ".css"):
			content = fmt.Sprintf("/* %s */\nbody { margin: 0; background: #0f172a; color: #f8fafc; font-family: sans-serif; }\n#canvas { display: block; width: 100vw; height: 100vh; }\n", cleanInst)
		case strings.HasSuffix(lower, ".js"):
			content = fmt.Sprintf("// %s\nconsole.log('App initialized');\nexport function init() { return true; }\n", cleanInst)
		case strings.HasSuffix(lower, ".md"):
			content = fmt.Sprintf("# %s\n\n## Overview\nProduction grade architecture and operational specifications.\n\n## Verification\nRun automated test suite to verify 100%% compliance.\n", cleanInst)
		default:
			pkgName := inferPackageName(targetPath)
			content = fmt.Sprintf("// SPDX-License-Identifier: Apache-2.0\npackage %s\n\n// Implemented per instruction: %s\n", pkgName, cleanInst)
		}
		editBlocks = append(editBlocks, EditBlockData{
			SearchText:  "",
			ReplaceText: content,
		})
	} else {
		// Modify existing code snippet heuristic
		lines := strings.Split(card.ContextSnippet, "\n")
		searchLine := ""
		for _, l := range lines {
			trimmed := strings.TrimSpace(l)
			if trimmed != "" && !strings.HasPrefix(trimmed, "//") {
				searchLine = l
				break
			}
		}
		if searchLine == "" && len(lines) > 0 {
			searchLine = lines[0]
		}

		replaceLine := searchLine + "\n\t// Updated per: " + cleanInst
		editBlocks = append(editBlocks, EditBlockData{
			SearchText:  searchLine,
			ReplaceText: replaceLine,
		})
	}

	return &EditDecision{
		FilePath:    targetPath,
		EditBlocks:  editBlocks,
		Explanation: fmt.Sprintf("Mock gate generated %d edit block(s) for %s", len(editBlocks), targetPath),
	}, nil
}

// DecideHeal synthesizes surgical error repairs targeting diagnostic error lines.
func (m *MockDecisionGate) DecideHeal(ctx context.Context, card GateHealCard) (*HealDecision, error) {
	if err := m.handleTimeout(ctx); err != nil {
		return nil, err
	}
	if err := m.checkFailAttempts(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.HealCalls = append(m.HealCalls, card)
	customFunc := m.HealFunc
	canned := m.CannedHeal
	genErr := m.InjectError
	specificErr := m.InjectHealError
	m.mu.Unlock()

	if genErr != nil {
		return nil, genErr
	}
	if specificErr != nil {
		return nil, specificErr
	}
	if customFunc != nil {
		return customFunc(ctx, card)
	}
	if canned != nil {
		res := *canned
		return &res, nil
	}

	// Rule-Based Heuristic Evaluation
	targetPath := filepath.ToSlash(card.FilePath)
	editBlocks := make([]EditBlockData, 0)

	// Extract the offending line from Snippet5Lines (> line: content)
	offendingSearch := ""
	for _, line := range strings.Split(card.Snippet5Lines, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") {
			if matches := errorLineRegex.FindStringSubmatch(trimmed); len(matches) > 2 {
				offendingSearch = matches[2]
			} else {
				offendingSearch = strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			}
			break
		}
	}

	if offendingSearch == "" {
		offendingSearch = fmt.Sprintf("// error at line %d", card.ErrorLine)
	}

	// Synthesize surgical fix
	repairedReplace := offendingSearch + " // fixed: " + card.ErrorMessage
	if strings.Contains(strings.ToLower(card.ErrorMessage), "undefined") {
		repairedReplace = "// resolved definition: " + offendingSearch
	}

	editBlocks = append(editBlocks, EditBlockData{
		SearchText:  offendingSearch,
		ReplaceText: repairedReplace,
	})

	return &HealDecision{
		FilePath:      targetPath,
		EditBlocks:    editBlocks,
		RepairSummary: fmt.Sprintf("Mock gate surgical repair on %s line %d: %s", targetPath, card.ErrorLine, card.ErrorMessage),
		RootCause:     card.ErrorMessage,
	}, nil
}

// DecideHealSpeculative synthesizes up to k candidate repairs for speculative beam evaluation.
func (m *MockDecisionGate) DecideHealSpeculative(ctx context.Context, card GateHealCard, k int) ([]*HealDecision, error) {
	dec, err := m.DecideHeal(ctx, card)
	if err != nil {
		return nil, err
	}
	if k <= 1 {
		return []*HealDecision{dec}, nil
	}
	results := make([]*HealDecision, 0, k)
	results = append(results, dec)
	for i := 1; i < k; i++ {
		clone := *dec
		clone.RepairSummary = fmt.Sprintf("%s (beam candidate %d)", dec.RepairSummary, i+1)
		results = append(results, &clone)
	}
	return results, nil
}

// DecideFinalize evaluates test outputs and approves commit creation.
func (m *MockDecisionGate) DecideFinalize(ctx context.Context, card GateFinalizeCard) (*FinalizeDecision, error) {
	if err := m.handleTimeout(ctx); err != nil {
		return nil, err
	}
	if err := m.checkFailAttempts(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.FinalizeCalls = append(m.FinalizeCalls, card)
	customFunc := m.FinalizeFunc
	canned := m.CannedFinalize
	genErr := m.InjectError
	specificErr := m.InjectFinalizeError
	rejected := m.SimulateRejection
	m.mu.Unlock()

	if genErr != nil {
		return nil, genErr
	}
	if specificErr != nil {
		return nil, specificErr
	}
	if customFunc != nil {
		return customFunc(ctx, card)
	}
	if canned != nil {
		res := *canned
		return &res, nil
	}

	// Strict Safety Invariant: Tests with non-zero exit codes MUST NEVER be approved
	if card.TestExitCode != 0 || rejected {
		return &FinalizeDecision{
			Approved:       false,
			CommitMessage:  "",
			Summary:        fmt.Sprintf("Mock gate rejected commit: test exit code %d", card.TestExitCode),
			FilesCommitted: nil,
			Reason:         fmt.Sprintf("test command `%s` failed with exit code %d: %s", card.TestCommand, card.TestExitCode, card.TestOutputSummary),
		}, nil
	}

	// Success Heuristic: Format Conventional Commit
	scope := "core"
	if len(card.FilesModified) > 0 {
		first := filepath.ToSlash(card.FilesModified[0])
		parts := strings.Split(first, "/")
		if len(parts) >= 2 {
			scope = parts[1]
		}
	}

	desc := strings.TrimSpace(card.TaskDescription)
	if desc == "" {
		desc = "complete autonomous coding task"
	}
	commitMsg := fmt.Sprintf("feat(%s): %s", scope, desc)

	return &FinalizeDecision{
		Approved:       true,
		CommitMessage:  commitMsg,
		Summary:        fmt.Sprintf("Mock gate approved %d modified file(s) after successful verification", len(card.FilesModified)),
		FilesCommitted: card.FilesModified,
		Reason:         "all automated tests passed with exit code 0",
	}, nil
}

// Wrapper aliases matching DecisionGate interface signatures
func (m *MockDecisionGate) Intent(ctx context.Context, card GateIntentCard) (*GateIntentDecision, error) {
	return m.DecideIntent(ctx, card)
}

func (m *MockDecisionGate) Edit(ctx context.Context, card GateEditCard) (*GateEditDecision, error) {
	return m.DecideEdit(ctx, card)
}

func (m *MockDecisionGate) Heal(ctx context.Context, card GateHealCard) (*GateHealDecision, error) {
	return m.DecideHeal(ctx, card)
}

func (m *MockDecisionGate) Finalize(ctx context.Context, card GateFinalizeCard) (*GateFinalizeDecision, error) {
	return m.DecideFinalize(ctx, card)
}

// DecideReproducer evaluates issue description and synthesizes a mock reproduction script.
func (m *MockDecisionGate) DecideReproducer(ctx context.Context, card GateReproducerCard) (*ReproducerDecision, error) {
	if err := m.handleTimeout(ctx); err != nil {
		return nil, err
	}
	if err := m.checkFailAttempts(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.InjectError != nil {
		err := m.InjectError
		m.mu.Unlock()
		return nil, err
	}
	if m.InjectReproducerError != nil {
		err := m.InjectReproducerError
		m.mu.Unlock()
		return nil, err
	}

	m.ReproducerCalls = append(m.ReproducerCalls, card)

	if m.ReproducerFunc != nil {
		fn := m.ReproducerFunc
		m.mu.Unlock()
		return fn(ctx, card)
	}

	if m.CannedReproducer != nil {
		canned := *m.CannedReproducer
		m.mu.Unlock()
		return &canned, nil
	}
	m.mu.Unlock()

	// Default heuristic script
	script := `#!/usr/bin/env python3
import sys

def test_issue():
    try:
        # Minimal reproducer oracle generated by MockDecisionGate
        print("REPRODUCTION_PASS: Bug is resolved.")
        sys.exit(0)
    except Exception as e:
        print(f"REPRODUCTION_FAIL: {e}")
        sys.exit(1)

if __name__ == "__main__":
    test_issue()
`
	return &ReproducerDecision{
		ScriptName:    "reproduce_issue.py",
		ScriptContent: script,
		Explanation:   "Mock DecisionGate generated reproducer oracle",
		Language:      "python",
	}, nil
}

func (m *MockDecisionGate) Reproducer(ctx context.Context, card GateReproducerCard) (*GateReproducerDecision, error) {
	return m.DecideReproducer(ctx, card)
}


// ============================================================================
// Complementary Wire-Level Mock: MockLLMClient
// ============================================================================

// MockLLMClient implements LLMClient for testing LLMDecisionGate and raw extraction.
type MockLLMClient struct {
	mu sync.Mutex

	ConfigVal        ClientConfig
	CompleteFunc     func(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error)
	CannedResponse   *ChatCompletionResponse
	CannedContent    string
	InjectError      error
	InjectTimeout    time.Duration
	ReceivedRequests []ChatCompletionRequest
}

var _ LLMClient = (*MockLLMClient)(nil)

// NewMockLLMClient constructs a MockLLMClient.
func NewMockLLMClient() *MockLLMClient {
	return &MockLLMClient{
		ConfigVal: ClientConfig{
			Model: "mock-model",
		},
		ReceivedRequests: make([]ChatCompletionRequest, 0),
	}
}

func (c *MockLLMClient) Config() ClientConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ConfigVal
}

func (c *MockLLMClient) Complete(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	c.mu.Lock()
	if req != nil {
		c.ReceivedRequests = append(c.ReceivedRequests, *req)
	}
	customFunc := c.CompleteFunc
	cannedResp := c.CannedResponse
	cannedContent := c.CannedContent
	injErr := c.InjectError
	delay := c.InjectTimeout
	model := c.ConfigVal.Model
	c.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	if injErr != nil {
		return nil, injErr
	}
	if customFunc != nil {
		return customFunc(ctx, req)
	}
	if cannedResp != nil {
		res := *cannedResp
		return &res, nil
	}
	if cannedContent != "" {
		return &ChatCompletionResponse{
			ID:    "mock-cmpl-1",
			Model: model,
			Choices: []Choice{
				{
					Index:        0,
					FinishReason: "stop",
					Message: ChatMessage{
						Role:    "assistant",
						Content: cannedContent,
					},
				},
			},
			Usage: Usage{
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
			},
		}, nil
	}

	return &ChatCompletionResponse{
		ID:    "mock-cmpl-0",
		Model: model,
		Choices: []Choice{
			{
				Index:        0,
				FinishReason: "stop",
				Message: ChatMessage{
					Role:    "assistant",
					Content: "{}",
				},
			},
		},
	}, nil
}

// Reset clears calls and canned responses.
func (c *MockLLMClient) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ReceivedRequests = c.ReceivedRequests[:0]
	c.CompleteFunc = nil
	c.CannedResponse = nil
	c.CannedContent = ""
	c.InjectError = nil
	c.InjectTimeout = 0
}

// LastRequest returns the latest received request, or nil.
func (c *MockLLMClient) LastRequest() *ChatCompletionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ReceivedRequests) == 0 {
		return nil
	}
	req := c.ReceivedRequests[len(c.ReceivedRequests)-1]
	return &req
}

// Helper utilities
func containsString(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func inferPackageName(path string) string {
	base := filepath.Base(path)
	if base == "main.go" {
		return "main"
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "/" || dir == "" {
		return "main"
	}

	// If directory already contains .go files, match their package declaration
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
					lines := strings.Split(string(data), "\n")
					for _, l := range lines {
						trimmed := strings.TrimSpace(l)
						if strings.HasPrefix(trimmed, "package ") {
							fields := strings.Fields(trimmed)
							if len(fields) >= 2 {
								pkg := strings.TrimSpace(fields[1])
								if pkg != "" {
									return pkg
								}
							}
						}
					}
				}
			}
		}
	}

	pkg := filepath.Base(dir)
	pkg = strings.ReplaceAll(pkg, "-", "")
	pkg = strings.ReplaceAll(pkg, ".", "")
	pkg = strings.ReplaceAll(pkg, " ", "")
	if pkg == "" {
		return "main"
	}
	return pkg
}
