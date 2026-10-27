// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestSession_Initialization(t *testing.T) {
	tempDir := t.TempDir()
	session := NewSession(nil, nil, tempDir)

	if session == nil {
		t.Fatal("expected non-nil session")
	}

	if session.Model() != llm.DefaultModel {
		t.Errorf("expected default model %q, got %q", llm.DefaultModel, session.Model())
	}

	absTempDir, _ := filepath.Abs(tempDir)
	if session.WorkspaceDir() != absTempDir {
		t.Errorf("expected workspace %q, got %q", absTempDir, session.WorkspaceDir())
	}

	if !strings.Contains(session.SystemPrompt(), "Conversational Architect & Planner") {
		t.Error("expected system prompt to calibrate Conversational Architect & Planner")
	}

	if !strings.Contains(session.SystemPrompt(), "[ACTION:BUILD") {
		t.Error("expected system prompt to reference [ACTION:BUILD tag")
	}

	if len(session.History()) != 0 {
		t.Errorf("expected empty initial history, got %d messages", len(session.History()))
	}

	st := session.Status()
	if st.TurnCount != 0 {
		t.Errorf("expected 0 turns, got %d", st.TurnCount)
	}
	if st.HasEngine {
		t.Error("expected HasEngine=false when engine is nil")
	}
}

func TestActionIntent_Parsing(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		expectFound     bool
		expectedType    ActionType
		expectedPayload string
		expectedTask    string
		expectedQuery   string
		expectedCmd     string
	}{
		{
			name:            "standard build double quotes",
			input:           `[ACTION:BUILD task="Implement user auth"]`,
			expectFound:     true,
			expectedType:    ActionBuild,
			expectedPayload: "Implement user auth",
			expectedTask:    "Implement user auth",
		},
		{
			name:            "standard build single quotes",
			input:           `[ACTION:BUILD task='Refactor database schema']`,
			expectFound:     true,
			expectedType:    ActionBuild,
			expectedPayload: "Refactor database schema",
			expectedTask:    "Refactor database schema",
		},
		{
			name:            "build with multiple attributes",
			input:           `[ACTION:BUILD task="Fix bug" priority="high" scope="pkg/ui"]`,
			expectFound:     true,
			expectedType:    ActionBuild,
			expectedPayload: "Fix bug",
			expectedTask:    "Fix bug",
		},
		{
			name:            "standard map query",
			input:           `[ACTION:MAP query="pkg/engine/orchestrator.go"]`,
			expectFound:     true,
			expectedType:    ActionMap,
			expectedPayload: "pkg/engine/orchestrator.go",
			expectedQuery:   "pkg/engine/orchestrator.go",
		},
		{
			name:            "map path attribute",
			input:           `[ACTION:MAP path="pkg/devops"]`,
			expectFound:     true,
			expectedType:    ActionMap,
			expectedPayload: "pkg/devops",
			expectedQuery:   "pkg/devops",
		},
		{
			name:            "map unkeyed raw value",
			input:           `[ACTION:MAP internal/compiler]`,
			expectFound:     true,
			expectedType:    ActionMap,
			expectedPayload: "internal/compiler",
			expectedQuery:   "internal/compiler",
		},
		{
			name:            "heal command",
			input:           `[ACTION:HEAL cmd="go test ./..."]`,
			expectFound:     true,
			expectedType:    ActionHeal,
			expectedPayload: "go test ./...",
			expectedCmd:     "go test ./...",
		},
		{
			name:            "heal command attribute variation",
			input:           `[ACTION:HEAL command="go build -v ./cmd/omnicoder"]`,
			expectFound:     true,
			expectedType:    ActionHeal,
			expectedPayload: "go build -v ./cmd/omnicoder",
			expectedCmd:     "go build -v ./cmd/omnicoder",
		},
		{
			name:            "whitespace and spacing tolerance",
			input:           `[   ACTION:BUILD   task="Spaced out task"   ]`,
			expectFound:     true,
			expectedType:    ActionBuild,
			expectedPayload: "Spaced out task",
			expectedTask:    "Spaced out task",
		},
		{
			name:            "case insensitivity",
			input:           `[action:build task="Lowercase tag"]`,
			expectFound:     true,
			expectedType:    ActionBuild,
			expectedPayload: "Lowercase tag",
			expectedTask:    "Lowercase tag",
		},
		{
			name: "embedded in multi-line markdown response",
			input: "I have analyzed the repository structure.\n\n" +
				"Here is the proposed architectural plan:\n" +
				"1. Add session.go to pkg/ui\n" +
				"2. Add cli.go for ANSI loop\n\n" +
				"[ACTION:BUILD task=\"Implement Conversational UI in pkg/ui\"]\n\n" +
				"Let me know if you would like me to execute this.",
			expectFound:     true,
			expectedType:    ActionBuild,
			expectedPayload: "Implement Conversational UI in pkg/ui",
			expectedTask:    "Implement Conversational UI in pkg/ui",
		},
		{
			name:        "regular text without action tag",
			input:       "Hello! How can I assist you with OmniCoder-ICX architecture today?",
			expectFound: false,
		},
		{
			name:        "malformed bracket tag",
			input:       "[NOTE: This is not an action tag]",
			expectFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if !tc.expectFound {
				if action != nil {
					t.Fatalf("expected nil action for input %q, got %+v", tc.input, action)
				}
				return
			}

			if action == nil {
				t.Fatalf("expected action intent for input %q, got nil", tc.input)
			}

			if action.Type != tc.expectedType {
				t.Errorf("expected type %v, got %v", tc.expectedType, action.Type)
			}

			if action.Payload != tc.expectedPayload {
				t.Errorf("expected payload %q, got %q", tc.expectedPayload, action.Payload)
			}

			if tc.expectedTask != "" && action.Task() != tc.expectedTask {
				t.Errorf("expected task %q, got %q", tc.expectedTask, action.Task())
			}

			if tc.expectedQuery != "" && action.Query() != tc.expectedQuery {
				t.Errorf("expected query %q, got %q", tc.expectedQuery, action.Query())
			}

			if tc.expectedCmd != "" && action.Cmd() != tc.expectedCmd {
				t.Errorf("expected cmd %q, got %q", tc.expectedCmd, action.Cmd())
			}

			if action.String() == "" || action.String() == "<nil>" {
				t.Error("expected non-empty String() representation")
			}
		})
	}
}

func TestStripActionTags(t *testing.T) {
	input := "Hello!\n[ACTION:BUILD task=\"Build system\"]\nHave a nice day!"
	cleaned := StripActionTags(input)
	if strings.Contains(cleaned, "[ACTION:") {
		t.Errorf("expected action tag stripped, got %q", cleaned)
	}
	if !strings.Contains(cleaned, "Hello!") || !strings.Contains(cleaned, "Have a nice day!") {
		t.Errorf("expected remaining text preserved, got %q", cleaned)
	}
}

func TestParseAllActionIntents(t *testing.T) {
	input := "First plan:\n[ACTION:MAP query=\"pkg/ui\"]\nSecond plan:\n[ACTION:BUILD task=\"Implement UI\"]\n"
	intents := ParseAllActionIntents(input)
	if len(intents) != 2 {
		t.Fatalf("expected 2 action intents, got %d", len(intents))
	}
	if intents[0].Type != ActionMap || intents[0].Query() != "pkg/ui" {
		t.Errorf("unexpected first intent: %+v", intents[0])
	}
	if intents[1].Type != ActionBuild || intents[1].Task() != "Implement UI" {
		t.Errorf("unexpected second intent: %+v", intents[1])
	}
}

func TestSession_SendMessage_WithMockServer(t *testing.T) {
	// Create mock HTTP server simulating OpenAI-compatible chat completions endpoint
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "invalid method", http.StatusMethodNotAllowed)
			return
		}

		var req llm.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Ensure system prompt was prepended
		if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
			http.Error(w, "missing system message", http.StatusBadRequest)
			return
		}

		// Generate assistant reply containing an action tag
		resp := llm.ChatCompletionResponse{
			ID:      "chatcmpl-mock-test",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role: "assistant",
						Content: "I have formulated the plan.\n\n" +
							"[ACTION:BUILD task=\"Add unit test coverage\"]\n\n" +
							"Let's begin execution.",
					},
					FinishReason: "stop",
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:    mockServer.URL,
		APIKey:     "test-key",
		Model:      "gemini-3.5-flash-lite",
		MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, t.TempDir())

	// Test empty prompt error
	_, _, err = session.SendMessage(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty prompt")
	}

	// Submit valid prompt
	reply, action, err := session.SendMessage(context.Background(), "How do we test pkg/ui?")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	if !strings.Contains(reply, "formulated the plan") {
		t.Errorf("unexpected reply: %q", reply)
	}

	if action == nil {
		t.Fatal("expected action intent parsed from reply, got nil")
	}

	if action.Type != ActionBuild {
		t.Errorf("expected ActionBuild, got %v", action.Type)
	}

	if action.Task() != "Add unit test coverage" {
		t.Errorf("expected task 'Add unit test coverage', got %q", action.Task())
	}

	// Verify history contains user prompt + assistant reply
	history := session.History()
	if len(history) != 2 {
		t.Fatalf("expected 2 messages in history, got %d", len(history))
	}
	if history[0].Role != "user" || history[0].Content != "How do we test pkg/ui?" {
		t.Errorf("unexpected user message: %+v", history[0])
	}
	if history[1].Role != "assistant" || !strings.Contains(history[1].Content, "Add unit test coverage") {
		t.Errorf("unexpected assistant message: %+v", history[1])
	}

	// Check status
	st := session.Status()
	if st.TurnCount != 1 {
		t.Errorf("expected 1 turn, got %d", st.TurnCount)
	}
	if st.HistoryLen != 2 {
		t.Errorf("expected history length 2, got %d", st.HistoryLen)
	}
}

func TestSession_HistoryRetentionAndReset(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())

	session.AddMessage(llm.ChatMessage{Role: "user", Content: "Hello"})
	session.AddMessage(llm.ChatMessage{Role: "assistant", Content: "Greetings! How can I help?"})

	if len(session.History()) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(session.History()))
	}

	// Feed execution summary
	session.FeedExecutionSummary("Task: Refactor UI, Commits: [a1b2c3d], Merkle: 0xfeedface")
	if len(session.History()) != 3 {
		t.Fatalf("expected 3 messages after feeding summary, got %d", len(session.History()))
	}
	lastMsg := session.History()[2]
	if !strings.Contains(lastMsg.Content, "0xfeedface") {
		t.Errorf("expected summary in last message, got %q", lastMsg.Content)
	}

	// Reset dialogue tokens
	session.Reset()
	if len(session.History()) != 0 {
		t.Errorf("expected 0 messages after Reset(), got %d", len(session.History()))
	}

	// Verify workspace and model configuration are preserved
	if session.Model() != llm.DefaultModel {
		t.Errorf("expected model %q preserved, got %q", llm.DefaultModel, session.Model())
	}
	if session.WorkspaceDir() == "" {
		t.Error("expected workspace directory preserved after reset")
	}
}

func TestCLI_SlashCommands(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())

	input := strings.NewReader("/help\n/status\n/reset\n/exit\n")
	var output bytes.Buffer

	err := RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI returned unexpected error: %v", err)
	}

	outStr := output.String()

	// Verify welcome banner
	if !strings.Contains(outStr, "OMNICODER-ICX") {
		t.Error("expected banner title in output")
	}
	if !strings.Contains(outStr, "gemini-3.5-flash-lite") {
		t.Error("expected model badge in banner")
	}

	// Verify /help guide
	if !strings.Contains(outStr, "/help") || !strings.Contains(outStr, "/run") || !strings.Contains(outStr, "/map") {
		t.Error("expected slash command guide in /help output")
	}

	// Verify /status output
	if !strings.Contains(outStr, "Session Status:") || !strings.Contains(outStr, "Turns:") {
		t.Error("expected status metrics in /status output")
	}

	// Verify /reset confirmation
	if !strings.Contains(outStr, "Dialogue history cleared") {
		t.Error("expected reset confirmation in /reset output")
	}

	// Verify /exit message
	if !strings.Contains(outStr, "Exiting OmniCoder-ICX. Goodbye!") {
		t.Error("expected exit message in output")
	}
}

func TestCLI_SlashCommands_MissingArgs_And_Unknown(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())

	input := strings.NewReader("/run\n/heal\n/refactor\n/unknowncmd\n/quit\n")
	var output bytes.Buffer

	err := RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "Usage: /run <task description>") {
		t.Error("expected /run usage warning")
	}
	if !strings.Contains(outStr, "Usage: /heal <command>") {
		t.Error("expected /heal usage warning")
	}
	if !strings.Contains(outStr, "Usage: /refactor <symbol> <newSignature>") {
		t.Error("expected /refactor usage warning")
	}
	if !strings.Contains(outStr, "Unknown command '/unknowncmd'") {
		t.Error("expected unknown command notification")
	}
}

func TestCLI_SlashCommands_WithEngine(t *testing.T) {
	tempWs := t.TempDir()

	// Create an offline orchestrator with MockDecisionGate
	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tempWs,
		Offline:      true,
		MaxTurns:     1,
		DecisionGate: llm.NewMockDecisionGate(),
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(nil, orch, tempWs)

	input := strings.NewReader("/map 500\n/run Verify engine connectivity\n/heal go test ./...\n/refactor OldFunc func(x int)\n/exit\n")
	var output bytes.Buffer

	err = RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	outStr := output.String()

	// Check repository map output
	if !strings.Contains(outStr, "Repository Map Synthesized") {
		t.Error("expected repository map synthesis output")
	}

	// Check autonomous execution output
	if !strings.Contains(outStr, "AUTONOMOUS EXECUTION: Verify engine connectivity") {
		t.Error("expected autonomous execution header")
	}
	if !strings.Contains(outStr, "Execution Status:") {
		t.Error("expected execution summary in output")
	}

	// Check heal and refactor command execution
	if !strings.Contains(outStr, "Initiating compiler self-healing") {
		t.Error("expected heal initiation banner in output")
	}
	if !strings.Contains(outStr, "Refactoring symbol 'OldFunc'") {
		t.Error("expected refactoring banner in output")
	}

	// Verify session context was updated with summaries
	history := session.History()
	if len(history) < 4 {
		t.Fatalf("expected at least 4 summary messages fed to session, got %d", len(history))
	}
}

func TestCLI_ConversationalTurn_WithActionHandoff(t *testing.T) {
	tempWs := t.TempDir()

	// Setup mock LLM server that returns a build action tag
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := llm.ChatCompletionResponse{
			ID:      "chatcmpl-build-handoff",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role: "assistant",
						Content: "I recommend building the module.\n\n" +
							"[ACTION:BUILD task=\"Implement test module\"]\n\n" +
							"Executing plan...",
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tempWs,
		Offline:      true,
		MaxTurns:     1,
		DecisionGate: llm.NewMockDecisionGate(),
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(client, orch, tempWs)

	input := strings.NewReader("Please build the test module\n/exit\n")
	var output bytes.Buffer

	err = RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := output.String()

	// Verify LLM response was rendered
	if !strings.Contains(outStr, "I recommend building the module") {
		t.Error("expected assistant response in output")
	}

	// Verify action intent was detected and build ran in conversational loop
	if !strings.Contains(outStr, "[Build]") {
		t.Error("expected [Build] banner for inline conversational build")
	}
	if !strings.Contains(outStr, "Implement test module") {
		t.Error("expected build task description in output")
	}

	// Verify build was executed inline (not delegated to mechanical engine)
	if strings.Contains(outStr, "AUTONOMOUS EXECUTION") {
		t.Error("build should NOT delegate to mechanical engine; should stay conversational")
	}
}

func TestProgressRendering_And_Formatting(t *testing.T) {
	events := []engine.TurnProgressEvent{
		{
			Turn:      1,
			MaxTurns:  10,
			Stage:     1,
			StageName: "Ingest & RepoMap",
			Status:    "START",
			Details:   "Starting workspace ingestion",
		},
		{
			Turn:      1,
			MaxTurns:  10,
			Stage:     3,
			StageName: "Quote & Plan",
			Status:    "STAGE",
			Details:   "Synthesizing plan with QuoteSlot index 1",
		},
		{
			Turn:      1,
			MaxTurns:  10,
			Stage:     4,
			StageName: "Decision Gate",
			Status:    "PASS",
			Details:   "Intent and edit cards verified",
		},
		{
			Turn:        1,
			MaxTurns:    10,
			Stage:       6,
			StageName:   "Compiler Feedback",
			Status:      "HEAL",
			Details:     "Self-healing compile error",
			HealAttempt: 1,
			MaxHeals:    3,
		},
		{
			Turn:      1,
			MaxTurns:  10,
			Stage:     5,
			StageName: "Patch",
			Status:    "FAIL",
			Details:   "Patch application rejected",
		},
	}

	var buf bytes.Buffer
	cb := MakeProgressCallback(&buf)

	for _, ev := range events {
		cb(ev)
	}

	out := buf.String()
	for _, ev := range events {
		if !strings.Contains(out, ev.Details) {
			t.Errorf("expected details %q in progress output", ev.Details)
		}
		if !strings.Contains(out, ev.StageName) {
			t.Errorf("expected stage name %q in progress output", ev.StageName)
		}
	}
}

func TestFormatReportSummary(t *testing.T) {
	report := &engine.RunReport{
		Success:      true,
		Task:         "Implement feature X",
		TurnsCount:   2,
		HealCount:    1,
		FilesTouched: []string{"pkg/ui/session.go", "pkg/ui/cli.go"},
		Commits:      []string{"feat(ui): add session"},
		MerkleSeal:   "0x123456789abcdef",
		Duration:     1250 * time.Millisecond,
	}

	summary := FormatReportSummary(report)
	if !strings.Contains(summary, "SUCCESS") {
		t.Error("expected SUCCESS status in summary")
	}
	if !strings.Contains(summary, "Implement feature X") {
		t.Error("expected task in summary")
	}
	if !strings.Contains(summary, "0x123456789abcdef") {
		t.Error("expected Merkle seal in summary")
	}
	if !strings.Contains(summary, "pkg/ui/session.go") {
		t.Error("expected touched files in summary")
	}
}

func TestFormatRepoMapSummary(t *testing.T) {
	res := &devops.RepoMapResult{
		FilesScanned: 15,
		SymbolsFound: 42,
		TokenCount:   1200,
		RepoMap:      "pkg/ui/session.go (12 symbols)\npkg/ui/cli.go (18 symbols)",
	}

	summary := FormatRepoMapSummary(res)
	if !strings.Contains(summary, "15") || !strings.Contains(summary, "1200") {
		t.Error("expected files and tokens in repo map summary")
	}
	if !strings.Contains(summary, "pkg/ui/session.go") {
		t.Error("expected formatted tree in repo map summary")
	}
}

func TestTerminalTheme_NoColor(t *testing.T) {
	theme := &TerminalTheme{ColorEnabled: false}

	if theme.Bold("test") != "test" {
		t.Errorf("expected plain text when color disabled, got %q", theme.Bold("test"))
	}
	if theme.Cyan("test") != "test" {
		t.Errorf("expected plain text when color disabled, got %q", theme.Cyan("test"))
	}
	if theme.Badge("Model", "test") != "[Model: test]" {
		t.Errorf("expected plain badge when color disabled, got %q", theme.Badge("Model", "test"))
	}
}

func TestCLI_ConversationalTurn_WithMapAndHealActions(t *testing.T) {
	tempWs := t.TempDir()

	turn := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var content string
		if turn == 1 {
			content = "Let's explore the repo.\n\n[ACTION:MAP query=\"pkg/ui\"]\n\nCheck the map."
		} else {
			content = "Fixing compilation.\n\n[ACTION:HEAL cmd=\"go test ./...\"]\n\nDone."
		}
		resp := llm.ChatCompletionResponse{
			ID:      "chatcmpl-actions",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: content,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tempWs,
		Offline:      true,
		MaxTurns:     1,
		DecisionGate: llm.NewMockDecisionGate(),
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(client, orch, tempWs)

	input := strings.NewReader("Explore repo\nFix tests\n/exit\n")
	var output bytes.Buffer

	err = RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "Map Query: pkg/ui") {
		t.Error("expected Map Query: pkg/ui in output")
	}
	if !strings.Contains(outStr, "Heal Command: go test ./...") {
		t.Error("expected Heal Command: go test ./... in output")
	}
}

func TestSession_Accessors(t *testing.T) {
	tempWs := t.TempDir()
	session := NewSession(nil, nil, tempWs)

	if session.Engine() != nil {
		t.Error("expected nil engine initially")
	}

	session.SetEngine(nil)

	cb := func(event engine.TurnProgressEvent) {}
	session.SetProgressCallback(cb)
	if session.ProgressCallback() == nil {
		t.Error("expected non-nil ProgressCallback after setting")
	}

	if session.LLMClient() != nil {
		t.Error("expected nil LLMClient initially")
	}

	if err := RunConversationalUI(context.Background(), nil, nil, nil); err == nil {
		t.Error("expected error when running with nil session")
	}
}

func TestActionIntent_Parsing_WriteAndCreate(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		expectedType ActionType
		expectedPath string
	}{
		{"standard write double quotes", `[ACTION:WRITE path="docs/paper.md"]`, ActionWrite, "docs/paper.md"},
		{"standard write single quotes", `[ACTION:WRITE path='src/lib.rs']`, ActionWrite, "src/lib.rs"},
		{"standard create double quotes", `[ACTION:CREATE path="cmd/main.go"]`, ActionCreate, "cmd/main.go"},
		{"unkeyed raw path", `[ACTION:WRITE docs/README.md]`, ActionWrite, "docs/README.md"},
		{"unkeyed create raw path", `[ACTION:CREATE src/config.json]`, ActionCreate, "src/config.json"},
		{"extra attributes", `[ACTION:WRITE path="test.txt" mode="overwrite"]`, ActionWrite, "test.txt"},
		{"case insensitivity", `[action:write path="foo.txt"]`, ActionWrite, "foo.txt"},
		{"spacing tolerance", `[   ACTION:WRITE   path="spaced.txt"   ]`, ActionWrite, "spaced.txt"},
		{"fallback file attribute", `[ACTION:WRITE file="file_attr.txt"]`, ActionWrite, "file_attr.txt"},
		{"fallback target attribute", `[ACTION:CREATE target="target_attr.txt"]`, ActionCreate, "target_attr.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if action == nil {
				t.Fatalf("expected non-nil action for %q", tc.input)
			}
			if action.Type != tc.expectedType {
				t.Errorf("expected type %q, got %q", tc.expectedType, action.Type)
			}
			if action.Path() != tc.expectedPath {
				t.Errorf("expected path %q, got %q", tc.expectedPath, action.Path())
			}
			if !action.IsWrite() {
				t.Errorf("expected IsWrite() to be true for %v", action.Type)
			}
		})
	}

	// Nil safety checks
	var nilIntent *ActionIntent
	if nilIntent.Path() != "" {
		t.Errorf("expected empty string for nilIntent.Path(), got %q", nilIntent.Path())
	}
	if nilIntent.IsWrite() {
		t.Error("expected false for nilIntent.IsWrite()")
	}
	if nilIntent.ExtractContent("hello") != "hello\n" {
		t.Errorf("expected 'hello\\n' for nilIntent.ExtractContent, got %q", nilIntent.ExtractContent("hello"))
	}
}

func TestActionIntent_ExtractContent(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite, Attributes: map[string]string{"path": "test.md"}}

	t.Run("markdown fence unwrapping", func(t *testing.T) {
		input := "Here is the file:\n```markdown\n# Research Paper\nContent goes here.\n```\n[ACTION:WRITE path=\"test.md\"]"
		extracted := intent.ExtractContent(input)
		expected := "# Research Paper\nContent goes here.\n"
		if extracted != expected {
			t.Errorf("expected %q, got %q", expected, extracted)
		}
	})

	t.Run("nested 4-backtick code fence", func(t *testing.T) {
		input := "````markdown\n# Title\n```go\nfunc main() {}\n```\n````\n[ACTION:WRITE path=\"doc.md\"]"
		extracted := intent.ExtractContent(input)
		if !strings.Contains(extracted, "```go\nfunc main() {}\n```") {
			t.Errorf("nested code block was corrupted: %q", extracted)
		}
		if !strings.Contains(extracted, "# Title") {
			t.Errorf("title missing: %q", extracted)
		}
	})

	t.Run("raw text without code fences", func(t *testing.T) {
		input := "Simple raw content.\n[ACTION:WRITE path=\"plain.txt\"]"
		extracted := intent.ExtractContent(input)
		if strings.Contains(extracted, "[ACTION:WRITE") {
			t.Errorf("action tag was not stripped: %q", extracted)
		}
		if !strings.Contains(extracted, "Simple raw content.") {
			t.Errorf("missing content: %q", extracted)
		}
	})

	t.Run("markdown with code blocks but no outer fence", func(t *testing.T) {
		input := "# Research Paper\n\nHere is an example:\n```go\nfunc run() {}\n```\n## Section 2\nMore details."
		extracted := ExtractContent(input)
		if !strings.Contains(extracted, "# Research Paper") {
			t.Error("expected markdown header to be preserved")
		}
		if !strings.Contains(extracted, "func run() {}") {
			t.Error("expected inner code to be preserved")
		}
		if !strings.Contains(extracted, "## Section 2") {
			t.Error("expected section 2 to be preserved")
		}
	})

	t.Run("empty string handling", func(t *testing.T) {
		if ExtractContent("") != "" {
			t.Errorf("expected empty string, got %q", ExtractContent(""))
		}
	})
}

func TestSession_WriteFile_Execution(t *testing.T) {
	tempWs := t.TempDir()
	session := NewSession(nil, nil, tempWs)

	t.Run("write simple file", func(t *testing.T) {
		data := []byte("Hello, OmniCoder!")
		writtenPath, seal, err := session.WriteFile("hello.txt", data)
		if err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		if writtenPath != "hello.txt" {
			t.Errorf("expected relPath 'hello.txt', got %q", writtenPath)
		}
		expectedSeal := canonical.SHA256Bytes(data)
		if seal != expectedSeal {
			t.Errorf("expected seal %q, got %q", expectedSeal, seal)
		}

		fullPath := filepath.Join(tempWs, "hello.txt")
		diskData, err := os.ReadFile(fullPath)
		if err != nil {
			t.Fatalf("failed to read written file: %v", err)
		}
		if string(diskData) != string(data) {
			t.Errorf("expected disk data %q, got %q", string(data), string(diskData))
		}
	})

	t.Run("auto create parent directories", func(t *testing.T) {
		data := []byte("nested deliverable content")
		writtenPath, _, err := session.WriteFile("docs/deep/sub/paper.md", data)
		if err != nil {
			t.Fatalf("WriteFile nested failed: %v", err)
		}
		if writtenPath != "docs/deep/sub/paper.md" {
			t.Errorf("expected 'docs/deep/sub/paper.md', got %q", writtenPath)
		}

		fullPath := filepath.Join(tempWs, "docs", "deep", "sub", "paper.md")
		if _, err := os.Stat(fullPath); err != nil {
			t.Errorf("expected file to exist at %s: %v", fullPath, err)
		}
	})

	t.Run("overwrite existing file", func(t *testing.T) {
		initial := []byte("initial version")
		_, _, err := session.WriteFile("versioned.txt", initial)
		if err != nil {
			t.Fatalf("initial write failed: %v", err)
		}

		updated := []byte("updated version")
		_, seal2, err := session.WriteFile("versioned.txt", updated)
		if err != nil {
			t.Fatalf("overwrite failed: %v", err)
		}
		if seal2 != canonical.SHA256Bytes(updated) {
			t.Errorf("seal mismatch on overwrite")
		}

		diskData, _ := os.ReadFile(filepath.Join(tempWs, "versioned.txt"))
		if string(diskData) != "updated version" {
			t.Errorf("expected updated version, got %q", string(diskData))
		}
	})

	t.Run("empty data write", func(t *testing.T) {
		writtenPath, seal, err := session.WriteFile("empty.txt", []byte{})
		if err != nil {
			t.Fatalf("empty write failed: %v", err)
		}
		if writtenPath != "empty.txt" {
			t.Errorf("expected 'empty.txt', got %q", writtenPath)
		}
		if seal != canonical.SHA256Bytes([]byte{}) {
			t.Errorf("expected empty seal %q, got %q", canonical.SHA256Bytes([]byte{}), seal)
		}
	})

	t.Run("empty relPath error", func(t *testing.T) {
		_, _, err := session.WriteFile("   ", []byte("data"))
		if err == nil {
			t.Error("expected error for empty relPath")
		}
	})
}

func TestSession_LastAssistantMessage(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())

	if session.LastAssistantMessage() != "" {
		t.Error("expected empty string on fresh session")
	}

	session.AddMessage(llm.ChatMessage{Role: "user", Content: "Hello"})
	if session.LastAssistantMessage() != "" {
		t.Error("expected empty string when no assistant message exists")
	}

	session.AddMessage(llm.ChatMessage{Role: "assistant", Content: "First answer"})
	session.AddMessage(llm.ChatMessage{Role: "user", Content: "Followup"})
	if session.LastAssistantMessage() != "First answer" {
		t.Errorf("expected 'First answer', got %q", session.LastAssistantMessage())
	}

	session.AddMessage(llm.ChatMessage{Role: "assistant", Content: "Second answer"})
	session.FeedExecutionSummary("Build succeeded")
	// FeedExecutionSummary appends a "user" role message, so the last assistant message is still "Second answer"
	if session.LastAssistantMessage() != "Second answer" {
		t.Errorf("expected 'Second answer', got %q", session.LastAssistantMessage())
	}
}

func TestSession_SystemPrompt_Recalibration(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())
	prompt := session.SystemPrompt()

	requiredSubstrings := []string{
		"Conversational Architect & Planner",
		"[ACTION:BUILD",
		"[ACTION:WRITE path=\"<relative-path>\"]",
		"Direct Document & File Generation",
		"Anti-Map Invariant on Generative Requests",
		"Never emit placeholder stubs",
	}

	for _, sub := range requiredSubstrings {
		if !strings.Contains(prompt, sub) {
			t.Errorf("DefaultSystemPrompt missing required clause %q", sub)
		}
	}
}

func TestCLI_SlashCommand_SaveAndWrite(t *testing.T) {
	tempWs := t.TempDir()
	session := NewSession(nil, nil, tempWs)

	t.Run("save with no assistant message", func(t *testing.T) {
		input := strings.NewReader("/save doc.md\n/exit\n")
		var out bytes.Buffer
		err := RunConversationalUI(context.Background(), session, input, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed: %v", err)
		}
		if !strings.Contains(out.String(), "No previous assistant response") {
			t.Errorf("expected notice about no assistant response, got: %s", out.String())
		}
	})

	t.Run("save with no path argument", func(t *testing.T) {
		input := strings.NewReader("/save\n/exit\n")
		var out bytes.Buffer
		err := RunConversationalUI(context.Background(), session, input, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed: %v", err)
		}
		if !strings.Contains(out.String(), "Usage: /save <path>") {
			t.Errorf("expected usage info, got: %s", out.String())
		}
	})

	t.Run("save valid assistant message", func(t *testing.T) {
		session.AddMessage(llm.ChatMessage{
			Role:    "assistant",
			Content: "```markdown\n# Generated Research Paper\nBody text.\n```\n[ACTION:WRITE path=\"docs/paper.md\"]",
		})

		input := strings.NewReader("/save docs/saved_paper.md\n/exit\n")
		var out bytes.Buffer
		err := RunConversationalUI(context.Background(), session, input, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed: %v", err)
		}

		if !strings.Contains(out.String(), "[File Created]") {
			t.Errorf("expected [File Created] badge in output: %s", out.String())
		}

		savedFile := filepath.Join(tempWs, "docs", "saved_paper.md")
		content, err := os.ReadFile(savedFile)
		if err != nil {
			t.Fatalf("failed to read saved file: %v", err)
		}
		if !strings.Contains(string(content), "# Generated Research Paper") {
			t.Errorf("expected file content to contain '# Generated Research Paper', got %q", string(content))
		}
	})

	t.Run("write alias command", func(t *testing.T) {
		session.AddMessage(llm.ChatMessage{
			Role:    "assistant",
			Content: "package main\n\nfunc main() {}\n",
		})

		input := strings.NewReader("/write main.go\n/exit\n")
		var out bytes.Buffer
		err := RunConversationalUI(context.Background(), session, input, &out)
		if err != nil {
			t.Fatalf("RunConversationalUI failed: %v", err)
		}

		if !strings.Contains(out.String(), "[File Created]") {
			t.Errorf("expected [File Created] badge: %s", out.String())
		}

		writtenFile := filepath.Join(tempWs, "main.go")
		content, err := os.ReadFile(writtenFile)
		if err != nil {
			t.Fatalf("failed to read written file: %v", err)
		}
		if !strings.Contains(string(content), "package main") {
			t.Errorf("expected file content to contain 'package main', got %q", string(content))
		}
	})

	t.Run("help contains save command", func(t *testing.T) {
		var out bytes.Buffer
		PrintHelp(&out)
		if !strings.Contains(out.String(), "/save <path>") {
			t.Errorf("expected /save <path> in PrintHelp output: %s", out.String())
		}
	})
}

func TestCLI_ConversationalTurn_WithActionWrite(t *testing.T) {
	tempWs := t.TempDir()

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "Here is the implementation:\n```go\npackage calc\n\nfunc Add(a, b int) int { return a + b }\n```\n[ACTION:WRITE path=\"pkg/calc/add.go\"]"
		resp := llm.ChatCompletionResponse{
			ID:      "chatcmpl-write",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: content,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, tempWs)

	input := strings.NewReader("Write an add function\n/exit\n")
	var output bytes.Buffer

	err = RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "[Auto-Write Detected]") {
		t.Errorf("expected [Auto-Write Detected] in output: %s", outStr)
	}
	if !strings.Contains(outStr, "[File Created]") {
		t.Errorf("expected [File Created] in output: %s", outStr)
	}
	if !strings.Contains(outStr, "Size") || !strings.Contains(outStr, "54 bytes") {
		t.Errorf("expected Size badge in output: %s", outStr)
	}
	if !strings.Contains(outStr, "SHA-256") {
		t.Errorf("expected SHA-256 badge in output: %s", outStr)
	}

	// Verify file was created on disk
	createdFile := filepath.Join(tempWs, "pkg", "calc", "add.go")
	content, err := os.ReadFile(createdFile)
	if err != nil {
		t.Fatalf("failed to read created file on disk: %v", err)
	}
	if !strings.Contains(string(content), "func Add(a, b int) int") {
		t.Errorf("expected created file to contain 'func Add(a, b int) int', got %q", string(content))
	}
}

func TestCLI_DirectBuild_WithPlanFile(t *testing.T) {
	tempWs := t.TempDir()
	planPath := filepath.Join(tempWs, "TEST_PLAN.md")
	_ = os.WriteFile(planPath, []byte("# Master Test Plan\nPhase 1: Implement pkg/calc/add.go"), 0644)

	mockGate := llm.NewMockDecisionGate()
	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tempWs,
		Offline:      true,
		DecisionGate: mockGate,
		MaxTurns:     1,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}

	session := NewSession(nil, orch, tempWs)
	input := strings.NewReader(fmt.Sprintf("build %q\n/exit\n", planPath))
	var output bytes.Buffer

	err = RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "[Plan Detected]") {
		t.Errorf("expected [Plan Detected] in output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "AUTONOMOUS EXECUTION") {
		t.Errorf("expected AUTONOMOUS EXECUTION in output, got:\n%s", outStr)
	}
}

func TestResolvePlanFile_PrefersMasterPlanInDocsOverStub(t *testing.T) {
	tempWs := t.TempDir()
	parentDir := filepath.Dir(tempWs)
	docsDir := filepath.Join(parentDir, "docs")
	_ = os.MkdirAll(docsDir, 0755)

	// Create a tiny stub in the workspace
	workspaceStub := filepath.Join(tempWs, "TEST_SPEC.md")
	_ = os.WriteFile(workspaceStub, []byte("stub"), 0644)

	// Create a comprehensive master plan in parent docs/
	docsMaster := filepath.Join(docsDir, "TEST_SPEC.md")
	largeContent := strings.Repeat("# Master Architectural Specification\nDetailed system requirements.\n", 100)
	_ = os.WriteFile(docsMaster, []byte(largeContent), 0644)

	// When resolving from the workspace stub path
	resolvedPath, resolvedContent := ResolvePlanFile(workspaceStub, tempWs)
	if resolvedPath != docsMaster {
		t.Errorf("expected master plan in docs/ to be selected over stub, got %q", resolvedPath)
	}
	if len(resolvedContent) <= len("stub") {
		t.Errorf("expected large master content to be loaded, got %d bytes", len(resolvedContent))
	}

	// When resolving with just the filename
	resolvedPath2, _ := ResolvePlanFile("TEST_SPEC.md", tempWs)
	if resolvedPath2 != docsMaster {
		t.Errorf("expected master plan in docs/ to be selected by filename, got %q", resolvedPath2)
	}
}

func testUISamplePlan() *engine.PlanSpec {
	scope := "projects/telemetry-service"
	return &engine.PlanSpec{
		ProjectScope: scope,
		ModuleName:   "telemetryservice",
		Phases: []engine.PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Core Substrate",
				TargetFiles: []string{
					scope + "/go.mod",
					scope + "/pkg/canonical/canonical.go",
				},
				VerificationCmd: "go test -v ./...",
			},
			{
				PhaseNumber: 2,
				Name:        "Ingestion Collectors",
				TargetFiles: []string{
					scope + "/pkg/ingest/http.go",
					scope + "/pkg/ingest/grpc.go",
				},
				VerificationCmd: "go test -v ./...",
			},
		},
	}
}

func TestCLI_DirectBuild_CompletedPlan_FastSuccess(t *testing.T) {
	tempWs := t.TempDir()
	planPath := filepath.Join(tempWs, "TELEMETRY_IMPLEMENTATION_PLAN.md")
	planContent := "# Telemetry Service Implementation Plan\n" +
		"Target Output Artifact: `projects/telemetry-service/`\n\n" +
		"## Phase 1: Core Substrate\n" +
		"- Implement `projects/telemetry-service/go.mod` and `projects/telemetry-service/pkg/canonical/canonical.go`\n\n" +
		"## Phase 2: Ingestion Collectors\n" +
		"- Implement `projects/telemetry-service/pkg/ingest/http.go` and `projects/telemetry-service/pkg/ingest/grpc.go`\n"
	_ = os.WriteFile(planPath, []byte(planContent), 0644)

	// Create fake project scope with passing go test
	projScope := filepath.Join(tempWs, "projects", "telemetry-service")
	_ = os.MkdirAll(projScope, 0755)
	_ = os.WriteFile(filepath.Join(projScope, "go.mod"), []byte("module telemetryservice\n\ngo 1.22\n"), 0644)

	// Create all files of the plan so plan.IsComplete() is true
	plan := testUISamplePlan()
	for _, phase := range plan.Phases {
		for _, f := range phase.TargetFiles {
			abs := filepath.Join(tempWs, filepath.FromSlash(f))
			_ = os.MkdirAll(filepath.Dir(abs), 0755)
			if filepath.Base(f) == "go.mod" {
				_ = os.WriteFile(abs, []byte("module telemetryservice\n\ngo 1.22\n"), 0644)
			} else if strings.HasSuffix(f, ".go") {
				pkgName := filepath.Base(filepath.Dir(abs))
				_ = os.WriteFile(abs, []byte("package "+pkgName+"\n"), 0644)
			} else {
				_ = os.WriteFile(abs, []byte("/* dummy */\n"), 0644)
			}
		}
	}

	// Create passing test file
	_ = os.WriteFile(filepath.Join(projScope, "dummy_test.go"), []byte("package test\nimport \"testing\"\nfunc TestDummy(t *testing.T) {}\n"), 0644)

	mockGate := llm.NewMockDecisionGate()
	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tempWs,
		Offline:      true,
		DecisionGate: mockGate,
		MaxTurns:     10,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}

	session := NewSession(nil, orch, tempWs)
	input := strings.NewReader(fmt.Sprintf("build %q\n/exit\n", planPath))
	var output bytes.Buffer

	start := time.Now()
	err = RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}
	elapsed := time.Since(start)

	outStr := output.String()
	if !strings.Contains(outStr, "[Plan Detected]") {
		t.Errorf("expected [Plan Detected] in output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "SUCCESS") {
		t.Errorf("expected SUCCESS in output, got:\n%s", outStr)
	}
	if elapsed > 10*time.Second {
		t.Errorf("expected fast pre-flight completion (<10s), took %v", elapsed)
	}
}

func TestLooksLikeShellCommand(t *testing.T) {
	validCommands := []string{
		"go test ./...",
		"go build -v ./cmd/app",
		"npm test",
		"npm run build",
		"yarn test",
		"pnpm build",
		"cargo test --release",
		"make all",
		"pytest tests/",
		"python -m unittest",
		"python3 main.py",
		"node dist/index.js",
		"./build.sh",
		".\\build.bat",
		"../scripts/run.sh",
		"cd projects/calc && go test ./...",
		"go build -o server.exe .",
		"docker compose up -d",
		"dotnet test",
	}

	for _, cmd := range validCommands {
		if !looksLikeShellCommand(cmd) {
			t.Errorf("expected looksLikeShellCommand(%q) == true, got false", cmd)
		}
	}

	invalidCommands := []string{
		"",
		"   ",
		"Verify complete project",
		"Verify all tests pass",
		"Check implementation",
		"Ensure all unit tests pass.",
		"Run the test suite",
		"Test the calculator module",
		"Please verify everything works",
		"Build and verify the full application",
		"Compile and test all components",
	}

	for _, cmd := range invalidCommands {
		if looksLikeShellCommand(cmd) {
			t.Errorf("expected looksLikeShellCommand(%q) == false, got true", cmd)
		}
	}
}

func TestDetectPlaceholderTags(t *testing.T) {
	placeholderSamples := []struct {
		name    string
		content string
	}{
		{"xml placeholder", "func Add(a, b int) int {\n\t<placeholder>\n}"},
		{"xml placeholder with index", "func Sub(a, b int) int {\n\t<placeholder_1>\n}"},
		{"insert code tag", "// implementation\n<INSERT_CODE_HERE>"},
		{"fill in tag", "func main() {\n\t<FILL_IN>\n}"},
		{"bracket todo", "type User struct {\n\t[TODO]\n}"},
		{"bracket placeholder", "func Validate() {\n\t[PLACEHOLDER]\n}"},
		{"bracket insert code", "package test\n[INSERT_CODE]"},
		{"ellipsis existing code", "package calc\n\n// ... existing code ...\nfunc New() {}"},
		{"ellipsis remaining code", "/* ... remaining code ... */"},
	}

	for _, tc := range placeholderSamples {
		t.Run(tc.name, func(t *testing.T) {
			detected, tag := detectPlaceholderTags(tc.content)
			if !detected {
				t.Errorf("expected placeholder detected in %q, got false", tc.content)
			}
			if tag == "" {
				t.Errorf("expected non-empty tag, got empty")
			}
		})
	}

	cleanSamples := []string{
		"package calc\n\nfunc Add(a, b int) int { return a + b }\n",
		"<!DOCTYPE html><html><head><title>App</title></head><body><h1>Hello</h1></body></html>",
		"# Master Plan\n\nPhase 1: Implement core logic.\nPhase 2: Add UI.",
		"// TODO: consider caching in the future when traffic scales\nfunc Fast() {}",
	}

	for i, clean := range cleanSamples {
		t.Run(fmt.Sprintf("clean_%d", i), func(t *testing.T) {
			detected, tag := detectPlaceholderTags(clean)
			if detected {
				t.Errorf("unexpected placeholder detection in clean content %q (tag=%q)", clean, tag)
			}
		})
	}
}

func TestSession_WriteFile_PlaceholderDefense(t *testing.T) {
	tempWs := t.TempDir()
	session := NewSession(nil, nil, tempWs)

	// Clean file write succeeds
	cleanContent := []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	_, _, err := session.WriteFile("calc.go", cleanContent)
	if err != nil {
		t.Fatalf("expected clean WriteFile to succeed, got %v", err)
	}

	// Placeholder file write is rejected
	placeholderContent := []byte("package calc\n\nfunc Add(a, b int) int {\n\t<placeholder>\n}\n")
	_, _, err = session.WriteFile("bad.go", placeholderContent)
	if err == nil {
		t.Fatalf("expected WriteFile to reject content with <placeholder>, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to write file containing placeholder tag") {
		t.Errorf("expected placeholder rejection error, got %v", err)
	}
}

func TestCLI_ConversationalTurn_LooksLikeShellCommandGuard(t *testing.T) {
	tempWs := t.TempDir()

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role: "assistant",
						Content: "Let's verify.\n\n" +
							"[ACTION:BUILD task=\"Verify complete project\"]\n\n" +
							"Waiting for build result.",
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, tempWs)
	input := strings.NewReader("Please verify the project\n/exit\n")
	var output bytes.Buffer

	_ = RunConversationalUI(context.Background(), session, input, &output)
	outStr := output.String()

	if !strings.Contains(outStr, "[Build Guard Rejected]") {
		t.Errorf("expected [Build Guard Rejected] banner, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "must be an executable shell command") {
		t.Errorf("expected shell command mandate message in output, got:\n%s", outStr)
	}
}

func TestCLI_ConversationalTurn_LoopSelfContinuation_PlanCompletion(t *testing.T) {
	tempWs := t.TempDir()

	planPath := filepath.Join(tempWs, "FIXTURE_PLAN.md")
	planBody := "## Phase 1: Fixture\n\n- `projects/fixture/main.go`\n"
	if err := os.WriteFile(planPath, []byte(planBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempWs, "go.mod"), []byte("module example.com/fixture\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempWs, "projects", "fixture", "main.go")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	stepCount := 0

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stepCount++
		replyContent := ""
		if stepCount == 1 {
			replyContent = "I am reviewing the project status."
		} else {
			replyContent = "Everything looks clean and all unit tests pass. DONE"
		}

		resp := llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: replyContent,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, tempWs)
	input := strings.NewReader("Execute \"" + planPath + "\"\n/exit\n")
	var output bytes.Buffer

	_ = RunConversationalUI(context.Background(), session, input, &output)
	outStr := output.String()

	if !strings.Contains(outStr, "[Plan Completed]") {
		t.Errorf("expected [Plan Completed] banner, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[Auto-Next Step: Final Verification]") {
		t.Errorf("expected [Auto-Next Step: Final Verification] step, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[Task Complete]") {
		t.Errorf("expected [Task Complete] termination banner, got:\n%s", outStr)
	}
	if stepCount < 2 {
		t.Errorf("expected at least 2 steps (initial + final verification), got %d", stepCount)
	}
}

func TestCLI_ConversationalTurn_FinalVerification_WithBuildAction(t *testing.T) {
	tempWs := t.TempDir()

	planPath := filepath.Join(tempWs, "FIXTURE_PLAN.md")
	planBody := "## Phase 1: Fixture\n\n- `projects/fixture/main.go`\n"
	if err := os.WriteFile(planPath, []byte(planBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempWs, "go.mod"), []byte("module example.com/fixture\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempWs, "projects", "fixture", "main.go")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(target), "go.mod"), []byte("module example.com/fixture\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	stepCount := 0

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stepCount++
		replyContent := "I am running final verification tests.\n\n[ACTION:BUILD task=\"go build ./projects/fixture/...\"]\nAll deliverables verified. DONE"

		resp := llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: replyContent,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, tempWs)
	input := strings.NewReader("Execute \"" + planPath + "\"\n/exit\n")
	var output bytes.Buffer

	_ = RunConversationalUI(context.Background(), session, input, &output)
	outStr := output.String()

	if !strings.Contains(outStr, "[Plan Completed]") {
		t.Errorf("expected [Plan Completed] banner, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[Task Complete]") {
		t.Errorf("expected [Task Complete] termination banner, got:\n%s", outStr)
	}
	// Verify that the shell command was NOT echoed back as an auto-next step prompt
	if strings.Contains(outStr, "[Auto-Next Step] go build") {
		t.Errorf("detected dangerous infinite Case C echo loop of build command in output:\n%s", outStr)
	}
}


func TestCLI_ConversationalTurn_PlaceholderGuard(t *testing.T) {
	tempWs := t.TempDir()
	stepCount := 0

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stepCount++
		replyContent := ""
		if stepCount == 1 {
			// First turn: LLM emits code with <placeholder>
			replyContent = "Here is the implementation:\n```go\npackage calc\n\nfunc Add(a, b int) int {\n\t<placeholder>\n}\n```\n[ACTION:WRITE path=\"projects/calc/calc.go\"]"
		} else {
			// Second turn: LLM corrects it with full implementation
			replyContent = "Here is the corrected implementation:\n```go\npackage calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n```\n[ACTION:WRITE path=\"projects/calc/calc.go\"]\nAll done."
		}

		resp := llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: replyContent,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, tempWs)
	input := strings.NewReader("Implement calculator\n/exit\n")
	var output bytes.Buffer

	_ = RunConversationalUI(context.Background(), session, input, &output)
	outStr := output.String()

	// 1. Verify Placeholder Guard triggered
	if !strings.Contains(outStr, "[Placeholder Guard]") {
		t.Errorf("expected [Placeholder Guard] in output, got:\n%s", outStr)
	}
	// 2. Verify self-healing step occurred
	if !strings.Contains(outStr, "[Auto-Next Step: Self-Healing]") {
		t.Errorf("expected [Auto-Next Step: Self-Healing] in output, got:\n%s", outStr)
	}
	// 3. Verify final file was written on disk with real code
	targetFile := filepath.Join(tempWs, "projects", "calc", "calc.go")
	data, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("expected target file to exist after self-healing, got %v", err)
	}
	if strings.Contains(string(data), "<placeholder>") {
		t.Errorf("file still contains <placeholder>: %s", string(data))
	}
	if !strings.Contains(string(data), "return a + b") {
		t.Errorf("file missing final corrected code: %s", string(data))
	}
}

func TestReadPrompt_SingleLine(t *testing.T) {
	input := "build projects/demo/plan.md\n"
	reader := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "test> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "build projects/demo/plan.md" {
		t.Errorf("expected 'build projects/demo/plan.md', got %q", prompt)
	}
}

func TestReadPrompt_MultiLinePaste(t *testing.T) {
	paste := "\"\"\"\nbuild projects/demo/plan.md\nLine 2 of prompt\nLine 3 of prompt\n\"\"\"\n"
	reader := bufio.NewReader(strings.NewReader(paste))
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "test> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "build projects/demo/plan.md\nLine 2 of prompt\nLine 3 of prompt"
	if prompt != expected {
		t.Errorf("expected multi-line paste:\n%q\ngot:\n%q", expected, prompt)
	}
}

func TestReadPrompt_BlockQuotes(t *testing.T) {
	input := "\"\"\"\nfirst line of prompt\nsecond line of prompt\n\"\"\"\n"
	reader := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "test> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "first line of prompt\nsecond line of prompt"
	if prompt != expected {
		t.Errorf("expected block quote prompt:\n%q\ngot:\n%q", expected, prompt)
	}
}

func TestReadPrompt_LineContinuation(t *testing.T) {
	input := "go test \\\n-v ./... \\\n-race\n"
	reader := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "test> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "go test -v ./... -race"
	if prompt != expected {
		t.Errorf("expected joined prompt %q, got %q", expected, prompt)
	}
}

func TestBuildSelfHealingPrompt_WithHistory(t *testing.T) {
	history := []TurnAttemptRecord{
		{
			AttemptNumber:  1,
			Command:        "go test ./pkg/canonical/...",
			ActionsSummary: []string{"WRITE projects/telemetry-service/pkg/canonical/canonical.go"},
			Diagnostic:     "undefined: ComputeMerkleRoot",
		},
		{
			AttemptNumber:  2,
			Command:        "go test ./pkg/canonical/...",
			ActionsSummary: []string{"PATCH projects/telemetry-service/pkg/canonical/canonical.go"},
			Diagnostic:     "cannot use ComputeMerkleRoot (value of type func()) as type string",
		},
	}

	prompt := buildSelfHealingPrompt(".", "", 3, "syntax error in canonical.go:42", history...)

	if !strings.Contains(prompt, "PREVIOUS FAILED ATTEMPTS IN THIS HEALING CYCLE") {
		t.Errorf("expected previous failed attempts section in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Attempt #1") || !strings.Contains(prompt, "undefined: ComputeMerkleRoot") {
		t.Errorf("expected Attempt #1 details in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Attempt #2") || !strings.Contains(prompt, "cannot use ComputeMerkleRoot") {
		t.Errorf("expected Attempt #2 details in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "CRITICAL: Inspect the progression of failures above") {
		t.Errorf("expected critical guidance in prompt:\n%s", prompt)
	}
}

func TestBuildSelfHealingPrompt_ConcurrencyDeadlock(t *testing.T) {
	deadlockDiag := "panic: test timed out after 10s\ngoroutine 20 [sync.Mutex.Lock]:\nworker.go:88\n"
	prompt := buildSelfHealingPrompt(".", "", 1, deadlockDiag)

	if !strings.Contains(prompt, "CONCURRENCY DEADLOCK & TIMEOUT REPAIR PRESCRIPTION") {
		t.Errorf("expected concurrency deadlock prescription header in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "LOCKED HELPER PATTERN") {
		t.Errorf("expected locked helper pattern in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "RPC REPLY POPULATION") {
		t.Errorf("expected RPC reply population guidance in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "CONCURRENCY DEADLOCK / TIMEOUT BRIEFING") {
		t.Errorf("expected ICX partner briefing in prompt:\n%s", prompt)
	}
}

func TestBuildPhaseProgressionPrompt(t *testing.T) {
	spec := testUISamplePlan()
	if spec == nil || len(spec.Phases) < 2 {
		t.Fatal("expected sample plan to have multiple phases")
	}

	phase2 := &spec.Phases[1]
	missing := []string{phase2.TargetFiles[0], phase2.TargetFiles[1]}

	prompt := buildPhaseProgressionPrompt(".", spec, phase2, false, missing)

	if !strings.Contains(prompt, "PLAN PROGRESSION: PHASE 2 / 2") {
		t.Errorf("expected Phase 2 progression header, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Module: telemetryservice") {
		t.Errorf("expected module declaration, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "REUSE EXISTING PACKAGES") {
		t.Errorf("expected package reuse rule, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "ZERO STUBS") {
		t.Errorf("expected zero stubs rule, got:\n%s", prompt)
	}
}

func TestCleanGoModContent(t *testing.T) {
	t.Run("strips closing action tag and leaked prose", func(t *testing.T) {
		raw := `module telemetryservice

go 1.21

[/ACTION:WRITE]

Now let's run the verification build to verify our scaffolding:
`
		cleaned := cleanGoModContent(raw)
		expected := "module telemetryservice\n\ngo 1.21\n"
		if cleaned != expected {
			t.Errorf("expected %q, got %q", expected, cleaned)
		}
	})

	t.Run("strips conflict markers and draft collision", func(t *testing.T) {
		raw := `module telemetryservice

go 1.21
<<<<
=======
>>>>>>>
`
		cleaned := cleanGoModContent(raw)
		expected := "module telemetryservice\n\ngo 1.21\n"
		if cleaned != expected {
			t.Errorf("expected %q, got %q", expected, cleaned)
		}
	})

	t.Run("preserves require block and comments", func(t *testing.T) {
		raw := `// module declaration
module telemetryservice

go 1.21

require (
	github.com/gorilla/websocket v1.5.3
)
`
		cleaned := cleanGoModContent(raw)
		if !strings.Contains(cleaned, "github.com/gorilla/websocket v1.5.3") {
			t.Errorf("expected require block to be preserved, got %q", cleaned)
		}
	})

	t.Run("skips leading conversational prose before module directive", func(t *testing.T) {
		raw := `OmniCoder:
We are ready to initiate Phase 1 of the Telemetry Service project.
Let's begin by writing projects/telemetry-service/go.mod.

module telemetryservice

go 1.21
`
		cleaned := cleanGoModContent(raw)
		expected := "module telemetryservice\n\ngo 1.21\n"
		if cleaned != expected {
			t.Errorf("expected %q, got %q", expected, cleaned)
		}
	})
}

func TestDetectPlaceholderTags_ConflictMarkers(t *testing.T) {
	collision := `module telemetryservice

go 1.21
<<<<
=======
>>>>>>>
`
	detected, tag := detectPlaceholderTags(collision)
	if !detected {
		t.Errorf("expected collision markers to be detected as placeholder/invalid content")
	}
	if tag == "" {
		t.Errorf("expected tag to be non-empty")
	}
}

func TestExtractContentForIntent_GoModSanitization(t *testing.T) {
	reply := `We now create go.mod for the project.

[ACTION:WRITE path="projects/telemetry-service/go.mod"]
module telemetryservice

go 1.21

[/ACTION:WRITE]

Now let's build the project using:
[ACTION:BUILD task="go test ./..."]
`
	intents := ParseAllActionIntents(reply)
	if len(intents) == 0 {
		t.Fatal("expected to parse action intents")
	}
	extracted := intents[0].ExtractContent(reply)
	if strings.Contains(extracted, "[/ACTION") || strings.Contains(extracted, "Now let's build") {
		t.Errorf("extracted go.mod contained leaked tags or prose: %q", extracted)
	}
	if !strings.Contains(extracted, "module telemetryservice") || !strings.Contains(extracted, "go 1.21") {
		t.Errorf("extracted go.mod missing core directives: %q", extracted)
	}
}

func TestExtractContentForIntent_GoModSingleIntentNoFences(t *testing.T) {
	reply := `OmniCoder:
We are ready to initiate Phase 1 of the Telemetry Service project: **Project Scaffolding & Core Substrate**.

Let's begin by writing ` + "`projects/telemetry-service/go.mod`" + `.

[ACTION:WRITE path="projects/telemetry-service/go.mod"]
module telemetryservice

go 1.21
`
	intents := ParseAllActionIntents(reply)
	if len(intents) != 1 {
		t.Fatalf("expected exactly 1 intent, got %d", len(intents))
	}
	extracted := intents[0].ExtractContent(reply)
	expected := "module telemetryservice\n\ngo 1.21\n"
	if extracted != expected {
		t.Errorf("expected %q, got %q", expected, extracted)
	}
}

func TestSession_ActivePlanPersistence(t *testing.T) {
	session := NewSession(nil, nil, ".")
	spec := testUISamplePlan()
	session.SetActivePlan(spec, "docs/TELEMETRY_IMPLEMENTATION_PLAN.md")

	gotSpec, path := session.ActivePlan()
	if gotSpec == nil || path != "docs/TELEMETRY_IMPLEMENTATION_PLAN.md" {
		t.Fatalf("expected active plan to be retained, got %v, %q", gotSpec, path)
	}

	session.Reset()
	clearedSpec, clearedPath := session.ActivePlan()
	if clearedSpec != nil || clearedPath != "" {
		t.Fatalf("expected active plan to be cleared on Reset, got %v, %q", clearedSpec, clearedPath)
	}
}

func TestResolveVerifyDir_MultiModuleResolution(t *testing.T) {
	tempWs := t.TempDir()
	modA := filepath.Join(tempWs, "projects", "proj-a")
	modB := filepath.Join(tempWs, "projects", "proj-b")
	_ = os.MkdirAll(filepath.Join(modA, "pkg", "service"), 0755)
	_ = os.MkdirAll(filepath.Join(modB, "pkg", "client"), 0755)
	_ = os.WriteFile(filepath.Join(tempWs, "go.mod"), []byte("module host\ngo 1.21\n"), 0644)
	_ = os.WriteFile(filepath.Join(modA, "go.mod"), []byte("module proja\ngo 1.21\n"), 0644)
	_ = os.WriteFile(filepath.Join(modB, "go.mod"), []byte("module projb\ngo 1.21\n"), 0644)

	// Command targeting package in modA
	dirA := ResolveVerifyDir(tempWs, tempWs, "go test ./pkg/service/...")
	if dirA != modA {
		t.Errorf("expected ResolveVerifyDir to route to modA (%s), got %s", modA, dirA)
	}

	// Command targeting package in modB
	dirB := ResolveVerifyDir(tempWs, tempWs, "go test ./pkg/client/...")
	if dirB != modB {
		t.Errorf("expected ResolveVerifyDir to route to modB (%s), got %s", modB, dirB)
	}
}

func TestSession_SetModel_And_Gemini35Plus(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())
	if session.Model() != llm.DefaultModel {
		t.Errorf("expected initial model %q, got %q", llm.DefaultModel, session.Model())
	}

	session.SetModel(llm.ModelGemini38Flash)
	if session.Model() != "gemini-3.8-flash" {
		t.Errorf("expected updated model 'gemini-3.8-flash', got %q", session.Model())
	}

	status := session.Status()
	if status.Model != "gemini-3.8-flash" {
		t.Errorf("expected status model 'gemini-3.8-flash', got %q", status.Model)
	}
}

func TestDetectProjectFromPrompt(t *testing.T) {
	tmpDir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(tmpDir, "projects", "task-tracker"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, "apps", "billing-service"), 0755)

	// 1. Matches "task-tracker"
	p1 := DetectProjectFromPrompt("how does task-tracker look? should we add improvements?", tmpDir)
	if p1 != "projects/task-tracker" {
		t.Errorf("expected 'projects/task-tracker', got %q", p1)
	}

	// 2. Matches "billing-service"
	p2 := DetectProjectFromPrompt("Review the billing-service implementation", tmpDir)
	if p2 != "apps/billing-service" {
		t.Errorf("expected 'apps/billing-service', got %q", p2)
	}

	// 3. Unrelated prompt
	p3 := DetectProjectFromPrompt("hello how are you doing today?", tmpDir)
	if p3 != "" {
		t.Errorf("expected empty string for unrelated prompt, got %q", p3)
	}
}

func TestSession_DynamicProjectAnchoring(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "task-tracker")
	_ = os.MkdirAll(projDir, 0755)
	_ = os.WriteFile(filepath.Join(projDir, "server.go"), []byte("package main\n"), 0644)

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = "I see task-tracker is a task management service."

	session := NewSession(mockClient, nil, tmpDir)

	if session.ActiveProject() != "" {
		t.Fatalf("expected empty initial ActiveProject, got %q", session.ActiveProject())
	}

	ctx := context.Background()
	_, _, err := session.SendMessageAll(ctx, "how does task-tracker look? should we add improvements?")
	if err != nil {
		t.Fatalf("SendMessageAll failed: %v", err)
	}

	if session.ActiveProject() != "projects/task-tracker" {
		t.Errorf("expected ActiveProject to be 'projects/task-tracker', got %q", session.ActiveProject())
	}

	// Verify request sent to LLM contains epistemic grounding with the file
	reqs := mockClient.ReceivedRequests
	if len(reqs) == 0 {
		t.Fatal("expected at least 1 request to LLM")
	}
	systemMsg := reqs[0].Messages[0].Content
	if !strings.Contains(systemMsg, "Active Project Anchor: projects/task-tracker") {
		t.Errorf("expected system message to contain active project anchor, got:\n%s", systemMsg)
	}
	if !strings.Contains(systemMsg, "projects/task-tracker/server.go") {
		t.Errorf("expected system message to list projects/task-tracker/server.go, got:\n%s", systemMsg)
	}
}

func TestExecuteConversationalTurn_ActionMapContinuation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	projDir := filepath.Join(tmpDir, "projects", "task-tracker")
	_ = os.MkdirAll(projDir, 0755)
	_ = os.WriteFile(filepath.Join(projDir, "server.go"), []byte("package main\ntype App struct{}\n"), 0644)

	callCount := 0
	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		callCount++
		if callCount == 1 {
			// Step 1: Model emits exploratory map action
			return &llm.ChatCompletionResponse{
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `[ACTION:MAP query="task-tracker"]`}},
				},
			}, nil
		}
		// Step 2: Model receives map observation and returns answer with DONE
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: `TaskTracker has App struct. Improvements: add timers.`}},
			},
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(mockClient, orch, tmpDir)
	defer session.Close()

	var out bytes.Buffer
	ctx := context.Background()
	err = executeConversationalTurn(ctx, session, "how does task-tracker look?", &out)
	if err != nil {
		t.Fatalf("executeConversationalTurn failed: %v", err)
	}

	outStr := out.String()

	// Verify that step 1 ran the map query
	if !strings.Contains(outStr, "[Auto-Engine Detected]") || !strings.Contains(outStr, "Map Query: task-tracker") {
		t.Errorf("expected Auto-Engine map query detection, got:\n%s", outStr)
	}

	// Verify that auto-continuation occurred (did not halt after step 1)
	if callCount != 2 {
		t.Errorf("expected exactly 2 LLM completion calls (continuation after map), got %d", callCount)
	}

	// Verify that step 2's answer was printed
	if !strings.Contains(outStr, "TaskTracker has App struct. Improvements: add timers.") {
		t.Errorf("expected step 2 answer in output, got:\n%s", outStr)
	}
}

func TestExtractMissingTool(t *testing.T) {
	cases := []struct {
		diag     string
		expected string
	}{
		{
			diag:     "'bundler' is not recognized as an internal or external command, operable program or batch file.",
			expected: "bundler",
		},
		{
			diag:     "The term 'npm' is not recognized as the name of a cmdlet, function, script file, or operable program.",
			expected: "npm",
		},
		{
			diag:     "bash: cargo: command not found",
			expected: "cargo",
		},
		{
			diag:     "bash: line 1: rustc: command not found",
			expected: "rustc",
		},
		{
			diag:     "zsh: command not found: pytest",
			expected: "pytest",
		},
		{
			diag:     `exec: "gcc": executable file not found in %PATH%`,
			expected: "gcc",
		},
		{
			diag:     "command not found: docker",
			expected: "docker",
		},
		{
			diag:     "C:\\path\\to\\custom-tool.exe is not recognized as an internal or external command",
			expected: "custom-tool",
		},
		{
			diag:     "Everything compiled cleanly with exit code 0",
			expected: "",
		},
	}

	for _, c := range cases {
		got := ExtractMissingTool(c.diag)
		if got != c.expected {
			t.Errorf("ExtractMissingTool(%q) = %q, want %q", c.diag, got, c.expected)
		}
	}
}

func TestDetectUserErrorReport(t *testing.T) {
	// Error report with missing tool
	report1 := "'bundler' is not recognized as an internal or external command"
	isReport, tool, summary := DetectUserErrorReport(report1)
	if !isReport || tool != "bundler" || summary == "" {
		t.Errorf("expected isReport=true, tool=bundler; got %v, %q, %q", isReport, tool, summary)
	}

	// Error report without specific missing tool (e.g. compiler error)
	report2 := "main.go:12:3: undefined: fooBar\nexit status 1"
	isReport, tool, summary = DetectUserErrorReport(report2)
	if !isReport || tool != "" || summary == "" {
		t.Errorf("expected isReport=true, tool=''; got %v, %q, %q", isReport, tool, summary)
	}

	// Benign user prompt
	prompt := "Please write a calculator app in Go."
	isReport, _, _ = DetectUserErrorReport(prompt)
	if isReport {
		t.Errorf("expected isReport=false for benign prompt, got true")
	}
}

func TestDetectProjectVerificationCmd(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Go project with test
	goProj := filepath.Join(tmpDir, "go_with_tests")
	_ = os.MkdirAll(goProj, 0755)
	_ = os.WriteFile(filepath.Join(goProj, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(goProj, "app_test.go"), []byte("package main\nimport \"testing\"\nfunc TestX(t *testing.T){}\n"), 0644)

	cmd := DetectProjectVerificationCmd(goProj)
	if cmd != "go test ./..." {
		t.Errorf("expected 'go test ./...', got %q", cmd)
	}

	// 2. Go project without test
	goProjNoTest := filepath.Join(tmpDir, "go_no_tests")
	_ = os.MkdirAll(goProjNoTest, 0755)
	_ = os.WriteFile(filepath.Join(goProjNoTest, "go.mod"), []byte("module example.com/notest\n\ngo 1.22\n"), 0644)

	cmd = DetectProjectVerificationCmd(goProjNoTest)
	if cmd != "go build ." {
		t.Errorf("expected 'go build .', got %q", cmd)
	}

	// 3. Node project with test script
	nodeProj := filepath.Join(tmpDir, "node_proj")
	_ = os.MkdirAll(nodeProj, 0755)
	_ = os.WriteFile(filepath.Join(nodeProj, "package.json"), []byte(`{"name":"test-node","scripts":{"test":"jest"}}`), 0644)

	cmd = DetectProjectVerificationCmd(nodeProj)
	if cmd != "npm test" {
		t.Errorf("expected 'npm test', got %q", cmd)
	}
}

func TestIsDaemonCommandAndSubstitution(t *testing.T) {
	daemons := []string{"go run .", "npm run dev", "next dev", "python -m http.server"}
	for _, d := range daemons {
		if !IsDaemonCommand(d) {
			t.Errorf("expected IsDaemonCommand(%q) = true", d)
		}
		sub := SubstituteDaemonWithVerification(d, "")
		if sub == d {
			t.Errorf("expected SubstituteDaemonWithVerification(%q) to replace daemon command, got same", d)
		}
	}

	nonDaemons := []string{"go test ./...", "go build .", "npm test", "npm run build", "cargo test", "pytest"}
	for _, nd := range nonDaemons {
		if IsDaemonCommand(nd) {
			t.Errorf("expected IsDaemonCommand(%q) = false", nd)
		}
	}
}

func TestAccountabilityGuardTriggersOnCheerleading(t *testing.T) {
	tmpDir := t.TempDir()
	calls := 0

	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		calls++
		if calls == 1 {
			// First call: model cheerleads without any action tags
			return &llm.ChatCompletionResponse{
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: "I see what happened! You just need to install the bundler or run the app."}},
				},
			}, nil
		}
		// Second call: accountability guard insisted on concrete actions
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "[ACTION:WRITE path=\"build.bat\"]\n@echo off\ngo build .\n[/ACTION]\nDONE"}},
			},
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(mockClient, orch, tmpDir)
	defer session.Close()
	var out bytes.Buffer
	ctx := context.Background()

	err = executeConversationalTurn(ctx, session, "'bundler' is not recognized as an internal or external command", &out)
	if err != nil {
		t.Fatalf("unexpected turn error: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "[Accountability Guard]") {
		t.Errorf("expected [Accountability Guard] in output, got:\n%s", outStr)
	}
	if calls != 2 {
		t.Errorf("expected 2 completion calls due to accountability retry, got %d", calls)
	}

	// Verify build.bat was actually written
	batPath := filepath.Join(tmpDir, "build.bat")
	if data, err := os.ReadFile(batPath); err != nil || !strings.Contains(string(data), "go build") {
		t.Errorf("expected build.bat to be written by auto-repair, got err=%v, content=%s", err, string(data))
	}
}

func TestSlashCommandFallbackForTerminalErrors(t *testing.T) {
	tmpDir := t.TempDir()
	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "Resolved the bash error.\nDONE"}},
			},
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(mockClient, orch, tmpDir)

	// Simulate user inputting a pasted terminal error starting with /
	in := strings.NewReader("/bin/bash: bundler: command not found\n/exit\n")
	var out bytes.Buffer
	ctx := context.Background()

	err = RunConversationalUI(ctx, session, in, &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := out.String()
	if strings.Contains(outStr, "Unknown command '/bin/bash:'") {
		t.Errorf("terminal error starting with / was incorrectly treated as unknown command:\n%s", outStr)
	}
	if !strings.Contains(outStr, "OmniCoder:") {
		t.Errorf("expected OmniCoder to process the terminal error as a prompt, got:\n%s", outStr)
	}
}

func TestAssessProjectCompleteness_IncompleteDirectoryDetected(t *testing.T) {
	tmpDir := t.TempDir()
	appDir := filepath.Join(tmpDir, "incomplete-app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Only write main.go
	if err := os.WriteFile(filepath.Join(appDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	complete, missing := AssessProjectCompleteness(tmpDir, "incomplete-app")
	if complete {
		t.Errorf("expected project to be incomplete, got complete == true")
	}
	if len(missing) == 0 {
		t.Fatalf("expected missing items, got 0")
	}

	missingStr := strings.Join(missing, ", ")
	if !strings.Contains(missingStr, "go.mod") {
		t.Errorf("expected missing go.mod, got: %v", missing)
	}
	if !strings.Contains(missingStr, "Test Suite") {
		t.Errorf("expected missing Test Suite, got: %v", missing)
	}
	if !strings.Contains(missingStr, "README.md") {
		t.Errorf("expected missing README.md, got: %v", missing)
	}
}

func TestAssessProjectCompleteness_CompletedProjects(t *testing.T) {
	ws := t.TempDir()
	projRel := "projects/sample-service"
	absPath := filepath.Join(ws, filepath.FromSlash(projRel))
	if err := os.MkdirAll(absPath, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(absPath, "go.mod"), []byte("module sampleservice\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(absPath, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc Run() string { return \"ok\" }\nfunc main() {\n\tfmt.Println(Run())\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(absPath, "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {\n\tif Run() != \"ok\" { t.Fatal(\"expected ok\") }\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(absPath, "README.md"), []byte("# Sample Service\n\nComprehensive architecture, installation, and CLI usage guide for Sample Service.\n"), 0644)

	complete, missing := AssessProjectCompleteness(ws, projRel)
	if !complete {
		t.Errorf("project %s expected complete, but missing: %v", projRel, missing)
	}
}

func TestIsZeroTestsPass(t *testing.T) {
	if !isZeroTestsPass("go test ./...", "?   \tgithub.com/foo\t[no test files]\n", "") {
		t.Errorf("expected isZeroTestsPass == true for [no test files]")
	}
	if isZeroTestsPass("go test ./...", "PASS\nok  \tgithub.com/foo\t0.12s\n", "") {
		t.Errorf("expected isZeroTestsPass == false when tests PASS")
	}
	if isZeroTestsPass("go build ./...", "build success", "") {
		t.Errorf("expected isZeroTestsPass == false for non-test commands")
	}
}

func TestExecuteConversationalTurn_PhaseIncompleteGuardAndScaffolding(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Set up an active plan with 1 phase requiring projects/mock-app/go.mod and projects/mock-app/main.go
	plan := &engine.PlanSpec{
		ModuleName:   "mockapp",
		ProjectScope: "projects/mock-app",
		Phases: []engine.PlanPhase{
			{
				PhaseNumber:     1,
				Name:            "Scaffolding",
				TargetFiles:     []string{"projects/mock-app/go.mod", "projects/mock-app/main.go"},
				VerificationCmd: "go version",
			},
		},
	}

	callCount := 0
	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		callCount++
		if callCount == 1 {
			// Step 1: LLM writes main.go, but skips go.mod
			return &llm.ChatCompletionResponse{
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: "[ACTION:WRITE path=\"projects/mock-app/main.go\"]\n```go\npackage main\nfunc main() {}\n```"}},
				},
			}, nil
		}
		if callCount == 2 {
			// Step 2: LLM prematurely runs build command while go.mod is still missing
			return &llm.ChatCompletionResponse{
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: "All done! [ACTION:BUILD task=\"go version\"]"}},
				},
			}, nil
		}
		if callCount == 3 {
			// Step 3: LLM repeats build command without writing go.mod -> triggers autonomous scaffolding
			return &llm.ChatCompletionResponse{
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: "[ACTION:BUILD task=\"go version\"]"}},
				},
			}, nil
		}
		// Step 4: After scaffolding, LLM verifies and completes
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "DONE"}},
			},
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(mockClient, orch, tmpDir)
	defer session.Close()
	session.SetActivePlan(plan, "")

	var out bytes.Buffer
	ctx := context.Background()
	err = executeConversationalTurn(ctx, session, "build mock app", &out)
	if err != nil {
		t.Fatalf("executeConversationalTurn failed: %v", err)
	}

	outStr := out.String()

	// 1. Verify Phase Incomplete Guard intercepted premature verification
	if !strings.Contains(outStr, "[Phase Incomplete Guard]") {
		t.Errorf("expected [Phase Incomplete Guard] in output, got:\n%s", outStr)
	}

	// 2. Verify Autonomous Scaffolding created the missing go.mod
	if !strings.Contains(outStr, "[Autonomous Scaffolding]") {
		t.Errorf("expected [Autonomous Scaffolding] in output, got:\n%s", outStr)
	}

	// 3. Verify that the synthesized go.mod exists on disk
	modPath := filepath.Join(tmpDir, "projects", "mock-app", "go.mod")
	data, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatalf("expected go.mod to be created on disk, got error: %v", err)
	}
	if !strings.Contains(string(data), "module mockapp") {
		t.Errorf("expected go.mod to contain 'module mockapp', got: %s", string(data))
	}

	// 4. Verify that it did NOT trigger a [Stall] exit
	if strings.Contains(outStr, "[Stall]") {
		t.Errorf("unexpected [Stall] in output:\n%s", outStr)
	}
}

func TestExecuteConversationalTurn_EmptyContentGuardFeedsDiagnostics(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	callCount := 0
	var receivedPromptStep2 string
	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		callCount++
		if callCount == 1 {
			// Step 1: Model emits WRITE tag with empty content (nothing after tag)
			return &llm.ChatCompletionResponse{
				Choices: []llm.Choice{
					{Message: llm.ChatMessage{Role: "assistant", Content: "[ACTION:WRITE path=\"projects/app/main.go\"]\n\n"}},
				},
			}, nil
		}
		// Step 2: Model receives diagnostic about empty payload and fixes it
		if callCount == 2 && len(req.Messages) > 0 {
			receivedPromptStep2 = req.Messages[len(req.Messages)-1].Content
		}
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "[ACTION:WRITE path=\"projects/app/main.go\"]\n```go\npackage main\nfunc main() {}\n```\nDONE"}},
			},
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(mockClient, orch, tmpDir)
	defer session.Close()

	var out bytes.Buffer
	ctx := context.Background()
	err = executeConversationalTurn(ctx, session, "create app", &out)
	if err != nil {
		t.Fatalf("executeConversationalTurn failed: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "[Empty Content Guard]") {
		t.Errorf("expected [Empty Content Guard] in output, got:\n%s", outStr)
	}

	// Verify that diagnostic was fed back to LLM in step 2
	if !strings.Contains(receivedPromptStep2, "extracted file payload was empty") {
		t.Errorf("expected empty payload diagnostic in step 2 prompt, got:\n%s", receivedPromptStep2)
	}

	// Verify file was created in step 2
	mainPath := filepath.Join(tmpDir, "projects", "app", "main.go")
	if _, err := os.Stat(mainPath); os.IsNotExist(err) {
		t.Fatalf("expected main.go to be created on disk after self-healing, got error: %v", err)
	}
}

func TestAnalyzeICXSubterraneanFeedback_ShellTokenizerTrap(t *testing.T) {
	ws := t.TempDir()
	targetDir := filepath.Join(ws, "projects", "test-app")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(targetDir, "index.html"), []byte("<html></html>"), 0644)

	cmd := `python -c \"import os; print('hi')\"`
	diag := `File "<string>", line 1\n  \"import\n   ^\nSyntaxError: unexpected character after line continuation character`

	analysis := analyzeICXSubterraneanFeedback(ws, targetDir, cmd, diag)
	if !strings.Contains(analysis.ConversationalSpeech, "look closely at that shell invocation") {
		t.Errorf("expected shell invocation advice in conversational speech, got: %q", analysis.ConversationalSpeech)
	}
	if !strings.Contains(analysis.PromptBriefing, "SHELL ESCAPING / TOKENIZER TRAP") {
		t.Errorf("expected SHELL ESCAPING / TOKENIZER TRAP in briefing, got: %q", analysis.PromptBriefing)
	}
	if !strings.Contains(analysis.PromptBriefing, "0D-4D LATTICE AUDIT") {
		t.Errorf("expected 0D-4D LATTICE AUDIT in briefing, got: %q", analysis.PromptBriefing)
	}
}

func TestAnalyzeICXSubterraneanFeedback_GoScopingAndEmbed(t *testing.T) {
	ws := t.TempDir()
	diagScoping := `main.go:12:6: RunServer redeclared in this block`
	analysisScoping := analyzeICXSubterraneanFeedback(ws, "", "go test ./...", diagScoping)
	if !strings.Contains(analysisScoping.ConversationalSpeech, "watch out for Go package scoping") {
		t.Errorf("expected scoping speech, got: %q", analysisScoping.ConversationalSpeech)
	}

	diagEmbed := `server.go:8:12: pattern ../../web/static: invalid pattern syntax`
	analysisEmbed := analyzeICXSubterraneanFeedback(ws, "", "go build", diagEmbed)
	if !strings.Contains(analysisEmbed.ConversationalSpeech, "//go:embed directive cannot step outside") {
		t.Errorf("expected embed speech, got: %q", analysisEmbed.ConversationalSpeech)
	}
}

func TestSelfHealingPrompt_IncludesICXBriefing(t *testing.T) {
	ws := t.TempDir()
	targetDir := filepath.Join(ws, "projects", "order-service")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(targetDir, "README.md"), []byte("# OrderService\n\nA web order catalog application."), 0644)
	_ = os.WriteFile(filepath.Join(targetDir, "index.html"), []byte("<!DOCTYPE html><html><body><h1>OrderService</h1></body></html>"), 0644)

	diag := `SyntaxError: unexpected character after line continuation character`
	history := TurnAttemptRecord{
		AttemptNumber: 1,
		Command:       `python -c \"import os\"`,
		Diagnostic:    diag,
	}

	prompt := buildSelfHealingPrompt(ws, targetDir, 1, diag, history)
	if !strings.Contains(prompt, "=== ICX SUBTERRANEAN PARTNER BRIEFING ===") {
		t.Errorf("expected ICX partner briefing header in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Architect, my friend — ICX here") {
		t.Errorf("expected friendly greeting from ICX in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "SHELL ESCAPING / TOKENIZER TRAP") {
		t.Errorf("expected shell escaping trap in prompt, got:\n%s", prompt)
	}
}

func TestExtractUnavailableTool_IgnoresQuotedTokens(t *testing.T) {
	s := NewSession(nil, nil, t.TempDir())
	// Artificially record linter as unavailable
	s.mu.Lock()
	s.unavailableTools["linter"] = true
	s.mu.Unlock()

	// 1. Python code importing linter inside quotes should NOT be blocked
	if blocked := s.ExtractUnavailableToolFromCommand(`python -c "import linter"`); blocked != "" {
		t.Errorf("expected quoted 'linter' inside python -c to NOT be blocked, got: %q", blocked)
	}

	// 2. Path referencing a script should NOT be blocked
	if blocked := s.ExtractUnavailableToolFromCommand(`python projects/my-app/test_linter.py`); blocked != "" {
		t.Errorf("expected argument path to NOT be blocked, got: %q", blocked)
	}

	// 3. Direct invocation of the missing tool SHOULD be blocked
	if blocked := s.ExtractUnavailableToolFromCommand(`linter --version`); blocked != "linter" {
		t.Errorf("expected direct tool invocation 'linter' to be blocked, got: %q", blocked)
	}

	// 4. Chained invocation with && SHOULD be blocked
	if blocked := s.ExtractUnavailableToolFromCommand(`echo hello && linter run`); blocked != "linter" {
		t.Errorf("expected chained 'linter' to be blocked, got: %q", blocked)
	}
}

func TestDetectUserErrorReport_FileDoesNotExist(t *testing.T) {
	reports := []string{
		"Error: Invalid value: File does not exist: app.py",
		"cannot find the path specified",
		"can't open file 'app.py': [Errno 2] No such file or directory",
		"fatal: pathspec 'app.py' did not match any files",
	}

	for _, rep := range reports {
		isReport, _, _ := DetectUserErrorReport(rep)
		if !isReport {
			t.Errorf("expected %q to be recognized as user error report", rep)
		}
	}
}

func TestBuildTask_UnescapesQuotes(t *testing.T) {
	tag := `task="python -c \"import sys; print('hello')\""`
	attrs := parseKeyedAttributes(tag)
	raw := attrs["task"]
	// Verify raw preserves payload for AST parity
	if !strings.Contains(raw, `\"`) {
		t.Errorf("expected raw attribute to retain payload, got %q", raw)
	}
	// Verify unescaped execution string
	unescaped := strings.ReplaceAll(raw, `\"`, `"`)
	expected := `python -c "import sys; print('hello')"`
	if unescaped != expected {
		t.Errorf("expected unescaped task %q, got %q", expected, unescaped)
	}
}

func TestSession_QuarantineAndRestore(t *testing.T) {
	tmpDir := t.TempDir()
	s := NewSession(nil, nil, tmpDir)

	// 1. Quarantine a rejected payload
	qp := s.QuarantinePayload("projects/dashboard/app.py", "import streamlit as st\nst.title('Dashboard')", "infra error")
	if qp == nil || qp.Attempts != 1 {
		t.Fatalf("expected quarantined payload with attempts=1, got: %v", qp)
	}

	// 2. Retrieve by full path and basename
	if q, found := s.GetQuarantinedPayload("projects/dashboard/app.py"); !found || q.Content != qp.Content {
		t.Errorf("failed to retrieve quarantined payload by path")
	}
	if q, found := s.GetQuarantinedPayload("app.py"); !found || q.Content != qp.Content {
		t.Errorf("failed to retrieve quarantined payload by basename")
	}

	// 3. Second rejection increments attempts
	qp2 := s.QuarantinePayload("projects/dashboard/app.py", "import streamlit as st\nst.title('Dashboard')", "infra error")
	if qp2.Attempts != 2 {
		t.Errorf("expected attempts=2 on resubmit, got %d", qp2.Attempts)
	}

	// 4. Remove clears quarantine
	s.RemoveQuarantined("projects/dashboard/app.py")
	if _, found := s.GetQuarantinedPayload("app.py"); found {
		t.Errorf("expected quarantine to be cleared after remove")
	}
}

func TestAssessProjectCompleteness_StubEntryPointRejected(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "sample-app")
	_ = os.MkdirAll(projDir, 0755)

	_ = os.WriteFile(filepath.Join(projDir, "requirements.txt"), []byte("streamlit>=1.30.0\n"), 0644)
	_ = os.WriteFile(filepath.Join(projDir, "test_app.py"), []byte("import unittest\nclass T(unittest.TestCase):\n  def test_ok(self): pass\n"), 0644)
	_ = os.WriteFile(filepath.Join(projDir, "README.md"), []byte("# Sample App\nRun `streamlit run app.py` to start.\n"), 0644)

	// A stub main.py that merely prints how to run app.py
	stubMain := "def main():\n    print('To run this app, run: streamlit run app.py')\nif __name__ == '__main__':\n    main()\n"
	_ = os.WriteFile(filepath.Join(projDir, "main.py"), []byte(stubMain), 0644)

	// Since README declared app.py and main.py is a stub, completeness MUST fail
	isComp, missing := AssessProjectCompleteness(tmpDir, projDir)
	if isComp {
		t.Fatalf("expected project with stub main.py and missing app.py to be INCOMPLETE, got complete")
	}

	foundEntryMissing := false
	for _, m := range missing {
		if strings.Contains(m, "Core Entry Point") && strings.Contains(m, "app.py") {
			foundEntryMissing = true
			break
		}
	}
	if !foundEntryMissing {
		t.Errorf("expected missing pillars to specifically require app.py, got: %v", missing)
	}

	// Now write real substantive app.py
	realApp := "import streamlit as st\nimport os\n\nst.set_page_config(page_title='Sample App')\nst.title('Sample')\nst.sidebar.header('Controls')\nx = st.slider('Pick a number', 0, 100)\nst.write(f'Selected: {x}')\n"
	_ = os.WriteFile(filepath.Join(projDir, "app.py"), []byte(realApp), 0644)

	isCompNow, missingNow := AssessProjectCompleteness(tmpDir, projDir)
	if !isCompNow {
		t.Fatalf("expected project with real app.py to be COMPLETE, got missing: %v", missingNow)
	}
}

func TestRecordUnavailableTool_LocalProjectFilesShielded(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "my-app")
	_ = os.MkdirAll(projDir, 0755)
	_ = os.WriteFile(filepath.Join(projDir, "worker.py"), []byte("def process(): pass\n"), 0644)

	s := NewSession(nil, nil, tmpDir)
	s.SetActiveProject("projects/my-app")

	// Attempting to record "worker" when worker.py exists in project MUST be rejected
	s.RecordUnavailableTool("worker")
	if s.IsToolUnavailable("worker") {
		t.Errorf("expected local module 'worker' to NOT be recorded as an unavailable tool")
	}
}

func TestSlashPlanCommand_ParsingAndHelp(t *testing.T) {
	var out bytes.Buffer
	PrintHelp(&out)
	helpStr := out.String()
	if !strings.Contains(helpStr, "/plan") {
		t.Fatalf("expected /plan to be listed in PrintHelp, got: %s", helpStr)
	}

	session := NewSession(nil, nil, t.TempDir())
	ctx := context.Background()

	// Verify executing /help via RunConversationalUI with offline session
	input := "/help\n/plan\n/exit\n"
	in := bufio.NewReader(strings.NewReader(input))
	var loopOut bytes.Buffer
	err := RunConversationalUI(ctx, session, in, &loopOut)
	if err != nil {
		t.Fatalf("unexpected RunConversationalUI error: %v", err)
	}
	outText := loopOut.String()
	if !strings.Contains(outText, "OmniCoder-ICX Slash Commands:") {
		t.Errorf("expected slash command guide in output, got: %s", outText)
	}
	if !strings.Contains(outText, "Usage: /plan <task description>") {
		t.Errorf("expected Usage: /plan <task description> in output, got: %s", outText)
	}
}

func TestAssessProjectCompleteness_GoLibrary(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Complete Go Library with tests
	libDir := filepath.Join(tmpDir, "projects", "mylib")
	_ = os.MkdirAll(libDir, 0755)
	_ = os.WriteFile(filepath.Join(libDir, "go.mod"), []byte("module mylib\n\ngo 1.21\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "mylib.go"), []byte("package mylib\n\nfunc Exported() int { return 42 }\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "mylib_test.go"), []byte("package mylib\n\nimport \"testing\"\n\nfunc TestExported(t *testing.T) {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "README.md"), []byte("# MyLib\nComprehensive documentation and usage instructions for MyLib storage engine.\n"), 0644)

	isComp, missing := AssessProjectCompleteness(tmpDir, "projects/mylib")
	if !isComp || len(missing) > 0 {
		t.Fatalf("expected Go library with tests to be complete, got isComp=%v, missing=%v", isComp, missing)
	}

	// 2. Go Library missing tests (should ask for tests or cmd, not root main.go)
	libNoTestsDir := filepath.Join(tmpDir, "projects", "notests")
	_ = os.MkdirAll(libNoTestsDir, 0755)
	_ = os.WriteFile(filepath.Join(libNoTestsDir, "go.mod"), []byte("module notests\n\ngo 1.21\n"), 0644)
	_ = os.WriteFile(filepath.Join(libNoTestsDir, "notests.go"), []byte("package notests\n\nfunc Foo() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(libNoTestsDir, "README.md"), []byte("# NoTests\nComprehensive documentation.\n"), 0644)

	isComp2, missing2 := AssessProjectCompleteness(tmpDir, "projects/notests")
	if isComp2 {
		t.Fatalf("expected Go library without tests to be incomplete")
	}
	missingStr := strings.Join(missing2, "; ")
	if strings.Contains(missingStr, "Core Entry Point (main.go)") {
		t.Errorf("Go library should NOT demand root main.go, got: %s", missingStr)
	}
	if !strings.Contains(missingStr, "Library Test Suite") {
		t.Errorf("expected missing message to mention Library Test Suite, got: %s", missingStr)
	}
}

func TestAssessProjectCompleteness_GoLibraryWithCmd(t *testing.T) {
	ws := t.TempDir()
	projPath := "projects/kvstore"
	kvDir := filepath.Join(ws, "projects", "kvstore")
	cmdDir := filepath.Join(kvDir, "cmd", "kvstore")
	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(kvDir, "go.mod"), []byte("module kvstore\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(kvDir, "store.go"), []byte("package kvstore\n\ntype Store struct{}\nfunc NewStore() *Store { return &Store{} }\n"), 0644)
	_ = os.WriteFile(filepath.Join(kvDir, "store_test.go"), []byte("package kvstore\n\nimport \"testing\"\n\nfunc TestStore(t *testing.T) {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(cmdDir, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"ok\") }\n"), 0644)
	_ = os.WriteFile(filepath.Join(kvDir, "README.md"), []byte("# KVStore\n\nComprehensive documentation and usage instructions for the embedded key-value storage library.\n"), 0644)

	isComp, missing := AssessProjectCompleteness(ws, projPath)
	if !isComp {
		t.Errorf("expected %s to be assessed as complete, but missing: %v", projPath, missing)
	}
}

func TestExecuteConversationalTurn_GoEntrypointAutoRelocation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Pre-create Go library
	libDir := filepath.Join(tmpDir, "projects", "mylib")
	_ = os.MkdirAll(libDir, 0755)
	_ = os.WriteFile(filepath.Join(libDir, "go.mod"), []byte("module mylib\n\ngo 1.21\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "mylib.go"), []byte("package mylib\n\nfunc Exported() int { return 42 }\n"), 0644)

	mockClient := llm.NewMockLLMClient()
	mockClient.CompleteFunc = func(ctx context.Context, req *llm.ChatCompletionRequest) (*llm.ChatCompletionResponse, error) {
		return &llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{
					Role:    "assistant",
					Content: "[ACTION:WRITE path=\"projects/mylib/main.go\"]\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"demo\")\n}\n[/ACTION:WRITE]\nDONE",
				}},
			},
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := NewSession(mockClient, orch, tmpDir)
	defer session.Close()

	var out bytes.Buffer
	ctx := context.Background()
	err = executeConversationalTurn(ctx, session, "create entry point for mylib", &out)
	if err != nil {
		t.Fatalf("executeConversationalTurn failed: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "[Auto-Relocate Go Entrypoint]") {
		t.Errorf("expected Auto-Relocate Go Entrypoint log in output, got:\n%s", outStr)
	}

	// Verify root main.go does NOT exist (no collision with package mylib)
	if _, err := os.Stat(filepath.Join(libDir, "main.go")); err == nil {
		t.Errorf("projects/mylib/main.go should not exist; should have been relocated")
	}

	// Verify cmd/mylib/main.go DOES exist
	relocatedPath := filepath.Join(libDir, "cmd", "mylib", "main.go")
	if _, err := os.Stat(relocatedPath); os.IsNotExist(err) {
		t.Errorf("expected relocated entry point at %s, but file does not exist", relocatedPath)
	}

	// Verify no quarantined deliverables
	if q := session.AllQuarantined(); len(q) > 0 {
		t.Errorf("expected 0 quarantined deliverables, got %d: %v", len(q), q)
	}
}

func TestQuarantineReconciliation_CompleteProject(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Pre-create complete Go library
	libDir := filepath.Join(tmpDir, "projects", "mylib")
	_ = os.MkdirAll(libDir, 0755)
	_ = os.WriteFile(filepath.Join(libDir, "go.mod"), []byte("module mylib\n\ngo 1.21\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "mylib.go"), []byte("package mylib\n\nfunc Exported() int { return 42 }\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "mylib_test.go"), []byte("package mylib\n\nimport \"testing\"\n\nfunc TestExported(t *testing.T) {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(libDir, "README.md"), []byte("# MyLib: High Performance Storage Engine\nComprehensive setup, architecture, and usage instructions for MyLib.\n"), 0644)

	session := NewSession(nil, nil, tmpDir)
	session.SetActiveProject("projects/mylib")

	// Quarantine an obsolete draft/attempt
	session.QuarantinePayload("projects/mylib/obsolete_draft.go", "package bad\nfunc x()", "syntax error")
	if len(session.AllQuarantined()) != 1 {
		t.Fatalf("expected 1 quarantined payload before reconciliation")
	}

	// Verify project is complete
	isComp, missing := AssessProjectCompleteness(tmpDir, "projects/mylib")
	if !isComp || len(missing) > 0 {
		t.Fatalf("expected project to be complete, got isComp=%v, missing=%v", isComp, missing)
	}

	// Simulate build command
	explicitBuildCmd := "go test ./..."
	buildDir := libDir
	session.RecordBuild(explicitBuildCmd, 0, "SUCCESS")

	// Run reconciliation logic as in cli.go line 2388
	targetCheckDir := buildDir
	if isComp, missing := AssessProjectCompleteness(tmpDir, targetCheckDir); isComp && len(missing) == 0 {
		for _, qp := range session.AllQuarantined() {
			absQ := filepath.Join(tmpDir, filepath.FromSlash(qp.Path))
			if strings.HasPrefix(filepath.Clean(absQ), filepath.Clean(targetCheckDir)) {
				session.RemoveQuarantined(qp.Path)
			}
		}
	}

	if len(session.AllQuarantined()) != 0 {
		t.Errorf("expected quarantine to be reconciled and cleared for complete project, but found: %v", session.AllQuarantined())
	}
}



