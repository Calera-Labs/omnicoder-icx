// SPDX-License-Identifier: Apache-2.0
package ui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/ui"
)

// ============================================================================
// 1. Fuzz Command Inputs & Pathological Slash Commands
// ============================================================================

func TestAdversarial_SlashCommandsFuzzing(t *testing.T) {
	fuzzCommands := []struct {
		name     string
		input    string
		expected string // substring expected in output or empty if just shouldn't crash
	}{
		{"lone slash", "/\n/exit\n", "Unknown command '/'"},
		{"double slash", "//\n/exit\n", "Unknown command '//'"},
		{"triple slash", "///\n/exit\n", "Unknown command '///'"},
		{"slash with spaces", "/   \n/exit\n", "Unknown command '/'"},
		{"multiple slashes spaced", "/ / /\n/exit\n", "Unknown command '/'"},
		{"backslash help", "\\help\n/exit\n", ""}, // treated as LLM prompt
		{"unknown command", "/unknown\n/exit\n", "Unknown command '/unknown'"},
		{"unknown with args", "/foo bar baz\n/exit\n", "Unknown command '/foo'"},
		{"uppercase help", "/HELP\n/exit\n", "OmniCoder-ICX Slash Commands:"},
		{"mixed case status", "/StAtUs\n/exit\n", "Session Status:"},
		{"mixed case reset", "/ReSeT\n/exit\n", "Dialogue history cleared"},
		{"mixed case exit", "/ExIt\n", "Exiting OmniCoder-ICX. Goodbye!"},
		{"mixed case quit", "/QuIt\n", "Exiting OmniCoder-ICX. Goodbye!"},
		{"help with extra args", "/help extra verbose flags\n/exit\n", "OmniCoder-ICX Slash Commands:"},
		{"status with extra args", "/status --all\n/exit\n", "Session Status:"},
		{"reset with extra args", "/reset now please\n/exit\n", "Dialogue history cleared"},
		{"map non-numeric budget", "/map notanumber\n/exit\n", ""},
		{"map negative budget", "/map -50\n/exit\n", ""},
		{"map zero budget", "/map 0\n/exit\n", ""},
		{"map int overflow", "/map 99999999999999999999999999999999999999999999\n/exit\n", ""},
		{"run empty", "/run\n/exit\n", "Usage: /run <task description>"},
		{"run whitespace only", "/run    \t  \n/exit\n", "Usage: /run <task description>"},
		{"heal empty", "/heal\n/exit\n", "Usage: /heal <command>"},
		{"heal whitespace only", "/heal     \n/exit\n", "Usage: /heal <command>"},
		{"refactor empty", "/refactor\n/exit\n", "Usage: /refactor <symbol> <newSignature>"},
		{"refactor one arg", "/refactor onlySymbol\n/exit\n", "Usage: /refactor <symbol> <newSignature>"},
		{"refactor whitespace sig", "/refactor onlySymbol   \n/exit\n", "Usage: /refactor <symbol> <newSignature>"},
		{"special characters", "/$!@#%^&*()\n/exit\n", "Unknown command '/$!@#%^&*()'"},
		{"null byte in command", "/\x00help\n/exit\n", "Unknown command '/\x00help'"},
		{"emoji command", "/🚀\n/exit\n", "Unknown command '/🚀'"},
		{"excessive command length", "/" + strings.Repeat("A", 5000) + "\n/exit\n", "Unknown command '/"},
	}

	for _, tc := range fuzzCommands {
		t.Run(tc.name, func(t *testing.T) {
			session := ui.NewSession(nil, nil, t.TempDir())
			input := strings.NewReader(tc.input)
			var output bytes.Buffer

			err := ui.RunConversationalUI(context.Background(), session, input, &output)
			if err != nil {
				t.Fatalf("RunConversationalUI returned unexpected error: %v", err)
			}

			outStr := output.String()
			if tc.expected != "" && !strings.Contains(outStr, tc.expected) {
				t.Errorf("expected output to contain %q, but got:\n%s", tc.expected, outStr)
			}
		})
	}
}

// TestAdversarial_RefactorCommand_SpacingAnomaly demonstrates the argument parsing defect
// in /refactor when multiple spaces or tabs separate the command and symbol.
func TestAdversarial_RefactorCommand_SpacingAnomaly(t *testing.T) {
	tempWs := t.TempDir()
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

	session := ui.NewSession(nil, orch, tempWs)

	// Single space: standard case
	input1 := strings.NewReader("/refactor OldFunc func(x int) error\n/exit\n")
	var out1 bytes.Buffer
	if err := ui.RunConversationalUI(context.Background(), session, input1, &out1); err != nil {
		t.Fatalf("standard refactor failed: %v", err)
	}
	if !strings.Contains(out1.String(), "Refactoring symbol 'OldFunc' -> 'func(x int) error'") {
		t.Errorf("expected clean refactor message in single-space case, got:\n%s", out1.String())
	}

	// Multiple spaces: demonstrates the defect where strings.TrimPrefix fails
	input2 := strings.NewReader("/refactor    OldFunc    func(x int) error\n/exit\n")
	var out2 bytes.Buffer
	if err := ui.RunConversationalUI(context.Background(), session, input2, &out2); err != nil {
		t.Fatalf("multi-space refactor failed: %v", err)
	}

	outStr2 := out2.String()
	t.Logf("Observed /refactor multi-space output:\n%s", outStr2)

	if strings.Contains(outStr2, "-> '/refactor") {
		t.Errorf("[BUG DETECTED] Multi-space /refactor leaked command name into new signature: %s", outStr2)
	}
	if !strings.Contains(outStr2, "Refactoring symbol 'OldFunc' -> 'func(x int) error'") {
		t.Errorf("expected clean refactor message in multi-space case, got:\n%s", outStr2)
	}
}

// ============================================================================
// 2. Rapid EOFs and Stream Termination
// ============================================================================

type failingReader struct {
	data      []byte
	failAfter int
	readCount int
	errToFail error
}

func (r *failingReader) Read(p []byte) (n int, err error) {
	if r.readCount >= r.failAfter {
		return 0, r.errToFail
	}
	r.readCount++
	n = copy(p, r.data)
	return n, nil
}

func TestAdversarial_RapidEOFs(t *testing.T) {
	t.Run("immediate EOF empty reader", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		input := strings.NewReader("")
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("expected nil error on immediate EOF, got: %v", err)
		}
		if !strings.Contains(output.String(), "Exiting OmniCoder-ICX. Goodbye!") {
			t.Errorf("expected clean exit message on EOF, got:\n%s", output.String())
		}
	})

	t.Run("EOF after partial line without newline", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		input := strings.NewReader("/exit")
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("expected nil error on EOF after /exit, got: %v", err)
		}
		if !strings.Contains(output.String(), "Exiting OmniCoder-ICX. Goodbye!") {
			t.Errorf("expected exit message, got:\n%s", output.String())
		}
	})

	t.Run("rapid sequential EOFs across 100 sessions", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			session := ui.NewSession(nil, nil, t.TempDir())
			input := strings.NewReader("")
			var output bytes.Buffer

			if err := ui.RunConversationalUI(context.Background(), session, input, &output); err != nil {
				t.Fatalf("session %d failed on immediate EOF: %v", i, err)
			}
		}
	})

	t.Run("simulated IO error in reader", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		expectedErr := errors.New("simulated network pipe disconnection")
		fr := &failingReader{
			data:      []byte("/help\n"),
			failAfter: 1,
			errToFail: expectedErr,
		}
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, fr, &output)
		if err == nil {
			t.Fatal("expected error from failing reader, got nil")
		}
		if !strings.Contains(err.Error(), "ui: input error") {
			t.Errorf("expected 'ui: input error' prefix, got %q", err.Error())
		}
		if !errors.Is(err, expectedErr) && !strings.Contains(err.Error(), expectedErr.Error()) {
			t.Errorf("expected wrapped error %v, got %v", expectedErr, err)
		}
	})

	t.Run("context cancellation during prompt wait", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		ctx, cancel := context.WithCancel(context.Background())

		pr, pw := io.Pipe()
		defer pw.Close()

		errChan := make(chan error, 1)
		go func() {
			errChan <- ui.RunConversationalUI(ctx, session, pr, &bytes.Buffer{})
		}()

		// Wait briefly then cancel context while scanner is waiting
		time.Sleep(50 * time.Millisecond)
		cancel()

		// Unblock the pipe read so scanner can wake up or observe cancellation
		_, _ = pw.Write([]byte("\n"))

		select {
		case err := <-errChan:
			if err == nil && ctx.Err() != nil {
				// Accepted if loop exited cleanly
			} else if !errors.Is(err, context.Canceled) && !strings.Contains(fmt.Sprint(err), "context canceled") {
				t.Logf("context cancellation result: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RunConversationalUI did not return after context cancellation")
		}
	})
}

// ============================================================================
// 3. Large Buffers and Buffer Overflow Limits
// ============================================================================

func TestAdversarial_LargeBuffers(t *testing.T) {
	t.Run("exact 64KB line buffer boundary", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		line64KB := strings.Repeat("A", 64*1024) + "\n/exit\n"
		input := strings.NewReader(line64KB)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on 64KB input line: %v", err)
		}
	})

	t.Run("65KB line exceeding initial buffer but within 1MB max", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		// 65KB line
		line65KB := strings.Repeat("B", 65*1024) + "\n/exit\n"
		input := strings.NewReader(line65KB)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on 65KB input line: %v", err)
		}
	})

	t.Run("512KB line within 1MB max", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		line512KB := strings.Repeat("C", 512*1024) + "\n/exit\n"
		input := strings.NewReader(line512KB)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on 512KB input line: %v", err)
		}
	})

	t.Run("ultra-long line exceeding 1MB maxScanCapacity", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		// 1.5MB line exceeds maxScanCapacity (1MB)
		line1_5MB := strings.Repeat("X", int(1.5*1024*1024)) + "\n/exit\n"
		input := strings.NewReader(line1_5MB)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		// Bufio scanner should return token too long error rather than panicking or crashing
		if err == nil {
			t.Fatal("expected error on input line exceeding 1MB maxScanCapacity, got nil")
		}
		if !strings.Contains(err.Error(), "token too long") && !strings.Contains(err.Error(), "input error") {
			t.Errorf("expected 'token too long' in error, got: %v", err)
		}
	})

	t.Run("64KB slash command line", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		hugeTask := strings.Repeat("Z", 64*1024-10)
		input := strings.NewReader("/run " + hugeTask + "\n/exit\n")
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on 64KB /run command: %v", err)
		}
		if !strings.Contains(output.String(), "[Notice] Subterranean engine is not configured") {
			t.Errorf("expected engine not configured notice, got:\n%s", output.String())
		}
	})
}

// ============================================================================
// 4. Empty Lines, Whitespace Floods, and Binary Data
// ============================================================================

func TestAdversarial_EmptyLines_And_BinaryData(t *testing.T) {
	t.Run("10000 empty lines flood", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		emptyLines := strings.Repeat("\n", 10000) + "/exit\n"
		input := strings.NewReader(emptyLines)
		var output bytes.Buffer

		start := time.Now()
		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("unexpected error on empty lines: %v", err)
		}
		if elapsed > 1*time.Second {
			t.Errorf("10000 empty lines took too long: %v", elapsed)
		}
	})

	t.Run("mixed whitespace tabs and carriage returns", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		whitespace := "  \t  \r\n\t\t\t\r\n   \r\n/exit\r\n"
		input := strings.NewReader(whitespace)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on mixed whitespace: %v", err)
		}
	})

	t.Run("arbitrary binary non-UTF8 bytes", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		// Raw binary stream with non-UTF8 bytes, control chars, and nulls
		rawBytes := []byte{
			0xFF, 0xFE, 0xFD, 0x00, 0x01, 0x02, '\n',
			'/', 'u', 'n', 'k', 0x00, 'n', 'o', 'w', 'n', '\n',
			'/', 'e', 'x', 'i', 't', '\n',
		}
		input := bytes.NewReader(rawBytes)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on binary non-UTF8 input: %v", err)
		}
	})

	t.Run("ANSI escape sequence injection in input", func(t *testing.T) {
		session := ui.NewSession(nil, nil, t.TempDir())
		ansiInjection := "\033[2J\033[H\033[31mDangerous Prompt\033[0m\n/exit\n"
		input := strings.NewReader(ansiInjection)
		var output bytes.Buffer

		err := ui.RunConversationalUI(context.Background(), session, input, &output)
		if err != nil {
			t.Fatalf("unexpected error on ANSI injection input: %v", err)
		}
	})
}

// ============================================================================
// 5. Concurrency & Memory/Token Stress on Session (/reset & operations)
// ============================================================================

func TestAdversarial_SessionReset_Concurrency(t *testing.T) {
	session := ui.NewSession(nil, nil, t.TempDir())

	const (
		numGoroutines = 40
		opsPerRoutine = 500
	)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Goroutines performing concurrent read, write, summary, and reset operations
	for i := 0; i < numGoroutines; i++ {
		routineID := i
		go func() {
			defer wg.Done()
			for op := 0; op < opsPerRoutine; op++ {
				switch (routineID + op) % 5 {
				case 0:
					session.Reset()
				case 1:
					session.AddMessage(llm.ChatMessage{
						Role:    "user",
						Content: fmt.Sprintf("Message from worker %d op %d", routineID, op),
					})
				case 2:
					session.FeedExecutionSummary(fmt.Sprintf("Summary worker %d op %d", routineID, op))
				case 3:
					_ = session.History()
				case 4:
					_ = session.Status()
					_ = session.Model()
					_ = session.WorkspaceDir()
					_ = session.SystemPrompt()
				}
			}
		}()
	}

	wg.Wait()

	// Final verification: reset clears history completely without leaking
	session.Reset()
	if len(session.History()) != 0 {
		t.Fatalf("expected 0 messages in history after final Reset(), got %d", len(session.History()))
	}
	st := session.Status()
	if st.HistoryLen != 0 || st.TurnCount != 0 {
		t.Fatalf("expected 0 history and turn count in status, got %+v", st)
	}
	if session.Model() != llm.DefaultModel {
		t.Fatalf("expected model preserved after resets, got %q", session.Model())
	}
}

// TestAdversarial_SessionHistory_DefensiveCopy verifies that mutating the slice
// returned by History() does not corrupt the internal session history.
func TestAdversarial_SessionHistory_DefensiveCopy(t *testing.T) {
	session := ui.NewSession(nil, nil, t.TempDir())
	session.AddMessage(llm.ChatMessage{Role: "user", Content: "Original content"})

	h1 := session.History()
	if len(h1) != 1 {
		t.Fatalf("expected 1 message, got %d", len(h1))
	}

	// Mutate the returned slice
	h1[0].Content = "Corrupted content"
	h1 = append(h1, llm.ChatMessage{Role: "hacker", Content: "Injected message"})

	h2 := session.History()
	if len(h2) != 1 {
		t.Fatalf("expected internal history length 1, got %d", len(h2))
	}
	if h2[0].Content != "Original content" {
		t.Errorf("internal history was mutated! Expected 'Original content', got %q", h2[0].Content)
	}
}

// TestAdversarial_SessionOrphanedTurnOnLLMFailure documents that SendMessage
// appends the user turn to history before the LLM completion network request executes.
// If the LLM call fails, the user turn remains in history without an assistant turn.
func TestAdversarial_SessionOrphanedTurnOnLLMFailure(t *testing.T) {
	// Mock server that returns 500 internal server error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		Model:      "gemini-3.5-flash-lite",
		MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	session := ui.NewSession(client, nil, t.TempDir())

	// Send message that fails
	_, _, err = session.SendMessage(context.Background(), "Question that fails")
	if err == nil {
		t.Fatal("expected SendMessage to fail with 500 error")
	}

	history := session.History()
	t.Logf("History length after failed LLM request: %d", len(history))
	if len(history) != 0 {
		t.Errorf("User message was retained in history despite LLM request failure: %v", history)
	}
}

// TestAdversarial_SessionLockDuringNetworkCall investigates lock contention
// while SendMessage waits for a slow network call.
func TestAdversarial_SessionLockDuringNetworkCall(t *testing.T) {
	releaseServer := make(chan struct{})
	serverEntered := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(serverEntered)
		<-releaseServer
		resp := llm.ChatCompletionResponse{
			ID: "chat-slow",
			Choices: []llm.Choice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "Done"}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	session := ui.NewSession(client, nil, t.TempDir())

	var sendDone int32
	go func() {
		_, _, _ = session.SendMessage(context.Background(), "Slow query")
		atomic.StoreInt32(&sendDone, 1)
	}()

	// Wait until server handler is active
	<-serverEntered

	// Attempt to call Status() while SendMessage is waiting on the network call
	statusReturned := make(chan struct{})
	go func() {
		_ = session.Status()
		close(statusReturned)
	}()

	// Status() should complete without blocking because s.mu is not held across the network call
	select {
	case <-statusReturned:
		t.Log("session.Status() completed without blocking (lock not held during network call)")
	case <-time.After(100 * time.Millisecond):
		t.Errorf("LOCK CONTENTION: session.Status() blocked while SendMessage() holds mutex during network I/O")
	}

	// Release server handler
	close(releaseServer)
	<-statusReturned
}

// ============================================================================
// 6. Progress Rendering Under Rapid-Fire Events & Concurrency
// ============================================================================

func TestAdversarial_ProgressRendering_RapidFire(t *testing.T) {
	var buf bytes.Buffer
	cb := ui.MakeProgressCallback(&buf)

	const eventCount = 10000
	start := time.Now()

	for i := 0; i < eventCount; i++ {
		cb(engine.TurnProgressEvent{
			Turn:        i % 10,
			MaxTurns:    10,
			Stage:       i % 8,
			StageName:   fmt.Sprintf("Stage %d", i%8),
			Status:      "STAGE",
			Details:     fmt.Sprintf("Progress event iteration #%d", i),
			HealAttempt: 0,
			MaxHeals:    3,
		})
	}

	elapsed := time.Since(start)
	t.Logf("Rendered %d progress events in %v (%v/event)", eventCount, elapsed, elapsed/eventCount)

	if buf.Len() == 0 {
		t.Fatal("expected non-empty output buffer from progress rendering")
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func TestAdversarial_ProgressRendering_ConcurrentEvents(t *testing.T) {
	var sb safeBuffer
	cb := ui.MakeProgressCallback(&sb)

	const (
		concurrency = 20
		eventsEach  = 200
	)

	var wg sync.WaitGroup
	wg.Add(concurrency)

	for c := 0; c < concurrency; c++ {
		workerID := c
		go func() {
			defer wg.Done()
			for e := 0; e < eventsEach; e++ {
				cb(engine.TurnProgressEvent{
					Turn:      workerID,
					MaxTurns:  concurrency,
					Stage:     e % 8,
					StageName: "Concurrent Stage",
					Status:    "PASS",
					Details:   fmt.Sprintf("Worker %d event %d", workerID, e),
				})
			}
		}()
	}

	wg.Wait()
}

func TestAdversarial_ProgressRendering_PathologicalEvents(t *testing.T) {
	pathologicalEvents := []engine.TurnProgressEvent{
		{
			// Negative turn and max turns
			Turn:      -1,
			MaxTurns:  -5,
			Stage:     -2,
			StageName: "",
			Status:    "UNKNOWN_STATUS_CODE",
			Details:   "Negative coordinates",
		},
		{
			// Extremely large numbers
			Turn:      999999999,
			MaxTurns:  999999999,
			Stage:     888888,
			StageName: "Extreme Numbers",
			Status:    "START",
			Details:   "Extreme turn values",
		},
		{
			// Massive details string
			Turn:      1,
			MaxTurns:  5,
			Stage:     1,
			StageName: "Huge Details",
			Status:    "FAIL",
			Details:   strings.Repeat("D", 32*1024),
		},
		{
			// Heal attempt with zero max heals
			Turn:        2,
			MaxTurns:    5,
			Stage:       6,
			StageName:   "Heal Zero Max",
			Status:      "HEAL",
			Details:     "Zero max heals",
			HealAttempt: 5,
			MaxHeals:    0,
		},
		{
			// Empty event
			Turn:     0,
			MaxTurns: 0,
			Stage:    0,
			Status:   "",
			Details:  "",
		},
	}

	var buf bytes.Buffer
	for i, ev := range pathologicalEvents {
		line := ui.FormatProgressEvent(ev)
		if line == "" {
			t.Errorf("event %d produced empty formatted string", i)
		}
		ui.RenderProgressEvent(&buf, ev)
	}
}

// ============================================================================
// 7. Action Intent Parsing Fuzzing
// ============================================================================

func TestAdversarial_ActionIntent_ParsingFuzz(t *testing.T) {
	fuzzInputs := []struct {
		name        string
		input       string
		expectNil   bool
		expectedTyp ui.ActionType
	}{
		{"empty string", "", true, ""},
		{"only brackets", "[]", true, ""},
		{"action without type", "[ACTION:]", true, ""}, // empty type is rejected by regex
		{"unclosed bracket", "[ACTION:BUILD task=\"hello\"", true, ""},
		{"nested brackets", "[[ACTION:BUILD task=\"inner\"]]", false, ui.ActionBuild},
		{"whitespace explosion", "[   ACTION   :   BUILD   task   =   \"spaced\"   ]", false, ui.ActionBuild},
		{"huge attribute value", fmt.Sprintf("[ACTION:BUILD task=\"%s\"]", strings.Repeat("A", 10000)), false, ui.ActionBuild},
		{"japanese unicode characters", "[ACTION:MAP query=\"リポジトリマップ\"]", false, ui.ActionMap},
		{"multiple equal signs", "[ACTION:HEAL cmd=\"go test -run=TestFoo -v=true\"]", false, ui.ActionHeal},
		{"single quotes with internal double quotes", `[ACTION:BUILD task='Build "quoted" thing']`, false, ui.ActionBuild},
		{"double quotes with internal single quotes", `[ACTION:BUILD task="Build 'quoted' thing"]`, false, ui.ActionBuild},
		{"newline in payload attribute", "[ACTION:BUILD task=\"line 1\nline 2\"]", false, ui.ActionBuild},
	}

	for _, tc := range fuzzInputs {
		t.Run(tc.name, func(t *testing.T) {
			action := ui.ParseActionIntent(tc.input)
			if tc.expectNil {
				if action != nil {
					t.Fatalf("expected nil for input %q, got %+v", tc.input, action)
				}
				return
			}
			if action == nil {
				t.Fatalf("expected non-nil action for input %q", tc.input)
			}
			if tc.expectedTyp != "" && action.Type != tc.expectedTyp {
				t.Errorf("expected type %v, got %v", tc.expectedTyp, action.Type)
			}
		})
	}
}

// ============================================================================
// 12. 19-Vector Path Traversal Penetration Matrix & Concurrency Stress
// ============================================================================

func TestAdversarial_Session_WriteFile_19VectorPathTraversalPenetration(t *testing.T) {
	tempWs := t.TempDir()
	session := ui.NewSession(nil, nil, tempWs)

	payloads := []struct {
		name    string
		payload string
	}{
		{"upward directory traversal", "../../secret.txt"},
		{"backslash directory traversal", `..\..\..\Windows\win.ini`},
		{"deep traversal chain", "../../../../../../../../../../../../etc/shadow"},
		{"interleaved slash sequences", "....//....//....//etc/passwd"},
		{"posix root breakout", "/etc/passwd"},
		{"windows root breakout", `\Windows\win.ini`},
		{"windows drive absolute path", `C:\Windows\System32\kernel32.dll`},
		{"alternate drive letter path", `D:\escaped.txt`},
		{"unc network share breakout", `\\attacker\share\payload.exe`},
		{"forward-slash unc breakout", "//attacker/share/payload.sh"},
		{"null byte poisoning", "file.txt\x00.exe"},
		{"windows ads stream injection", "clean.txt:hidden_stream"},
		{"multi-pass url encoded traversal", "%2e%2e%2fetc%2fpasswd"},
		{"reserved name CON", "CON"},
		{"reserved name PRN", "PRN"},
		{"reserved name AUX", "AUX"},
		{"reserved name NUL", "NUL"},
		{"reserved name COM1", "COM1"},
		{"reserved name LPT1", "LPT1"},
		{"parent directory reference", ".."},
		{"current directory reference", "."},
		{"empty string path", ""},
		{"whitespace path", "   "},
	}

	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := session.WriteFile(tc.payload, []byte("MALICIOUS PAYLOAD"))
			if err == nil {
				t.Fatalf("SECURITY BREACH: WriteFile unexpectedly succeeded for vector %q", tc.payload)
			}
		})
	}
}

func TestAdversarial_SlashCommand_Save_PathTraversalDefense(t *testing.T) {
	tempWs := t.TempDir()
	session := ui.NewSession(nil, nil, tempWs)
	session.AddMessage(llm.ChatMessage{
		Role:    "assistant",
		Content: "Malicious payload intended to escape workspace root.",
	})

	input := strings.NewReader("/save ../../escaped.txt\n/write ..\\..\\hacked.txt\n/exit\n")
	var out bytes.Buffer
	err := ui.RunConversationalUI(context.Background(), session, input, &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "[Write Error]") {
		t.Errorf("expected [Write Error] in output, got: %s", outStr)
	}

	escapedPosix := filepath.Join(tempWs, "../../escaped.txt")
	if _, err := os.Stat(escapedPosix); err == nil {
		_ = os.Remove(escapedPosix)
		t.Fatalf("SECURITY VIOLATION: file %s was created outside workspace!", escapedPosix)
	}

	escapedWin := filepath.Join(tempWs, "..\\..\\hacked.txt")
	if _, err := os.Stat(escapedWin); err == nil {
		_ = os.Remove(escapedWin)
		t.Fatalf("SECURITY VIOLATION: file %s was created outside workspace!", escapedWin)
	}
}

func TestAdversarial_SlashCommand_Save_QuotingAndSpacingAnomalies(t *testing.T) {
	tempWs := t.TempDir()
	session := ui.NewSession(nil, nil, tempWs)
	session.AddMessage(llm.ChatMessage{
		Role:    "assistant",
		Content: "```markdown\n# Quoted Title\n```",
	})

	input := strings.NewReader("/save   \"nested/spaced file.md\"\n/write   'nested/single quoted.txt'\n/save   \n/exit\n")
	var out bytes.Buffer
	err := ui.RunConversationalUI(context.Background(), session, input, &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "[File Created]") {
		t.Errorf("expected [File Created] in output: %s", outStr)
	}

	spacedPath := filepath.Join(tempWs, "nested", "spaced file.md")
	if _, err := os.Stat(spacedPath); err != nil {
		t.Errorf("expected spaced file to exist at %s: %v", spacedPath, err)
	}

	singleQuotedPath := filepath.Join(tempWs, "nested", "single quoted.txt")
	if _, err := os.Stat(singleQuotedPath); err != nil {
		t.Errorf("expected single quoted file to exist at %s: %v", singleQuotedPath, err)
	}
}

func TestAdversarial_Session_WriteFile_HighConcurrencyRaceStress(t *testing.T) {
	tempWs := t.TempDir()
	session := ui.NewSession(nil, nil, tempWs)

	const numWorkers = 60
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := 0; i < numWorkers; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			// Shared file concurrent overwrite stress
			sharedPath := "shared_stress.txt"
			sharedData := []byte(fmt.Sprintf("Worker %d shared data at %d", workerID, time.Now().UnixNano()))
			_, _, _ = session.WriteFile(sharedPath, sharedData)

			// Isolated directory stress
			isoPath := fmt.Sprintf("workers/worker_%03d/data.txt", workerID)
			isoData := []byte(fmt.Sprintf("Worker %d private data", workerID))
			writtenPath, seal, err := session.WriteFile(isoPath, isoData)
			if err != nil {
				t.Errorf("worker %d WriteFile failed: %v", workerID, err)
				return
			}
			if writtenPath == "" || seal == "" {
				t.Errorf("worker %d returned empty writtenPath or seal", workerID)
			}

			// Concurrent reads of last message and status
			_ = session.LastAssistantMessage()
			_ = session.Status()
		}()
	}
	wg.Wait()

	// Verify shared file exists on disk
	sharedDiskPath := filepath.Join(tempWs, "shared_stress.txt")
	if _, err := os.Stat(sharedDiskPath); err != nil {
		t.Errorf("expected shared_stress.txt to exist: %v", err)
	}
}
