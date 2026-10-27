// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSwarmHUD_RenderTable feeds 3 worker statuses into SwarmHUD writing to a bytes.Buffer
// and verifies table borders, PIDs, and statuses are properly formatted.
func TestSwarmHUD_RenderTable(t *testing.T) {
	var buf bytes.Buffer
	hud := NewSwarmHUD(&buf)

	// Ensure terminal mode is active for table rendering
	if !hud.IsTerminal() {
		hud.SetTerminal(true)
	}

	w1 := SwarmWorkerStatus{
		WorkerID:   1,
		PID:        10452,
		PhaseName:  "Phase 1: AST Parser",
		Stage:      "Turn 1",
		TurnCount:  1,
		MaxTurns:   5,
		TokensUsed: 1200,
		Status:     "[ACTIVE]",
		ActiveFile: "pkg/parser/ast.go",
	}
	w2 := SwarmWorkerStatus{
		WorkerID:   2,
		PID:        10453,
		PhaseName:  "Phase 2: Reconciler",
		Stage:      "Turn 2",
		TurnCount:  2,
		MaxTurns:   5,
		TokensUsed: 3400,
		Status:     "[HEALING]",
		ActiveFile: "pkg/orchestrator/merge.go",
		Diagnostic: "compiler type mismatch in merge.go:42",
	}
	w3 := SwarmWorkerStatus{
		WorkerID:   3,
		PID:        10454,
		PhaseName:  "Phase 3: Emitter",
		Stage:      "Done",
		TurnCount:  3,
		MaxTurns:   3,
		TokensUsed: 5600,
		Status:     "[PASSED]",
		ActiveFile: "pkg/emitter/emit.go",
	}

	hud.UpdateWorker(w1)
	hud.UpdateWorker(w2)
	hud.UpdateWorker(w3)
	hud.LogEvent("Swarm Wave 1 initialized with 3 workers")
	hud.LogEvent("Phase 2 entered healing turn 2")

	// Render the table frame
	hud.Render()

	out := buf.String()

	// 1. Verify ANSI cursor reset & clear below
	if !strings.Contains(out, "\033[H\033[J") {
		t.Errorf("expected ANSI clear code \\033[H\\033[J in table output, got: %s", out)
	}

	// 2. Verify Table Borders
	borderChars := []string{"┌", "─", "┬", "┐", "│", "├", "┼", "┤", "└", "┴", "┘"}
	for _, b := range borderChars {
		if !strings.Contains(out, b) {
			t.Errorf("expected table border character %q in rendered output", b)
		}
	}

	// 3. Verify Table Headers
	headers := []string{"SLOT", "PID", "PHASE", "STAGE", "TURNS", "TOKENS", "STATUS", "ACTIVE FILE"}
	for _, h := range headers {
		if !strings.Contains(out, h) {
			t.Errorf("expected table header %q in rendered output", h)
		}
	}

	// 4. Verify PIDs are properly formatted
	pids := []string{"10452", "10453", "10454"}
	for _, pid := range pids {
		if !strings.Contains(out, pid) {
			t.Errorf("expected PID %s in rendered output", pid)
		}
	}

	// 5. Verify Worker Statuses are properly formatted
	statuses := []string{"[ACTIVE]", "[HEALING]", "[PASSED]"}
	for _, st := range statuses {
		if !strings.Contains(out, st) {
			t.Errorf("expected status %s in rendered output", st)
		}
	}

	// 6. Verify Formatted Tokens & Aggregate
	expectedTokens := []string{"1,200", "3,400", "5,600", "10,200"}
	for _, tok := range expectedTokens {
		if !strings.Contains(out, tok) {
			t.Errorf("expected token count %s in rendered output", tok)
		}
	}

	// 7. Verify Turn Progression strings
	turns := []string{"1/5", "2/5", "3/3"}
	for _, turn := range turns {
		if !strings.Contains(out, turn) {
			t.Errorf("expected turn count %s in rendered output", turn)
		}
	}

	// 8. Verify Active File Names
	files := []string{"pkg/parser/ast.go", "pkg/orchestrator/merge.go", "pkg/emitter/emit.go"}
	for _, f := range files {
		if !strings.Contains(out, f) {
			t.Errorf("expected active file %s in rendered output", f)
		}
	}

	// 9. Verify Diagnostic is rendered for healing worker
	if !strings.Contains(out, "Diagnostic: compiler type mismatch in merge.go:42") {
		t.Errorf("expected diagnostic line in rendered output, got:\n%s", out)
	}

	// 10. Verify Event Ticker is present
	if !strings.Contains(out, "LIVE SWARM EVENT TICKER") {
		t.Errorf("expected LIVE SWARM EVENT TICKER in rendered output")
	}
	if !strings.Contains(out, "Swarm Wave 1 initialized with 3 workers") {
		t.Errorf("expected logged event in ticker")
	}
}

// TestSwarmHUD_NonTTYFallback configures HUD with isTerminal = false
// and verifies output contains sequential event logs without ANSI clear codes.
func TestSwarmHUD_NonTTYFallback(t *testing.T) {
	var buf bytes.Buffer
	hud := NewSwarmHUD(&buf)

	// Explicitly configure HUD with isTerminal = false (Non-TTY / CI mode)
	hud.isTerminal = false

	// Register worker status
	w := SwarmWorkerStatus{
		WorkerID:   2,
		PID:        2048,
		PhaseName:  "Phase 2",
		Stage:      "Turn 2",
		TurnCount:  2,
		MaxTurns:   5,
		TokensUsed: 4120,
		Status:     "[ACTIVE]",
		ActiveFile: "pkg/ingest/conn.go",
	}
	hud.UpdateWorker(w)

	// Log sequential events
	hud.LogEvent("Worker 2 spawned for Phase 2")
	hud.LogEvent("Worker 2 started turn 1")
	hud.LogEvent("Worker 2 advanced to turn 2")

	// Render linear status
	hud.Render()

	out := buf.String()

	// 1. Verify NO ANSI clear codes or cursor repositioning
	ansiClearCodes := []string{"\033[H", "\033[J", "\033[2J", "\033[0J"}
	for _, code := range ansiClearCodes {
		if strings.Contains(out, code) {
			t.Errorf("non-TTY mode must NOT contain ANSI code %q, got: %s", code, out)
		}
	}

	// 2. Verify sequential event logs are emitted in order
	e1 := "Worker 2 spawned for Phase 2"
	e2 := "Worker 2 started turn 1"
	e3 := "Worker 2 advanced to turn 2"

	idx1 := strings.Index(out, e1)
	idx2 := strings.Index(out, e2)
	idx3 := strings.Index(out, e3)

	if idx1 == -1 || idx2 == -1 || idx3 == -1 {
		t.Fatalf("expected all sequential event logs in output. idx1=%d, idx2=%d, idx3=%d\nOutput:\n%s", idx1, idx2, idx3, out)
	}

	if !(idx1 < idx2 && idx2 < idx3) {
		t.Errorf("events are not in sequential order: idx1=%d, idx2=%d, idx3=%d", idx1, idx2, idx3)
	}

	// 3. Verify clean linear worker status line format:
	// "[Swarm] Phase 2: Active (Turn 2/5, 4,120 tokens)"
	expectedStatusLine := "[Swarm] Phase 2: Active (Turn 2/5, 4,120 tokens)"
	if !strings.Contains(out, expectedStatusLine) {
		t.Errorf("expected linear status line %q in non-TTY output, got:\n%s", expectedStatusLine, out)
	}
}

// TestSwarmHUD_RunRenderLoop verifies that RunRenderLoop executes periodic
// rendering and halts cleanly when context is cancelled.
func TestSwarmHUD_RunRenderLoop(t *testing.T) {
	var buf bytes.Buffer
	hud := NewSwarmHUD(&buf)
	hud.isTerminal = false

	hud.UpdateWorker(SwarmWorkerStatus{
		WorkerID:   1,
		PID:        999,
		PhaseName:  "Phase 1",
		TurnCount:  1,
		MaxTurns:   2,
		TokensUsed: 500,
		Status:     "[ACTIVE]",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	doneCh := make(chan struct{})
	go func() {
		hud.RunRenderLoop(ctx, 20*time.Millisecond)
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// Success: exited cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("RunRenderLoop did not terminate within timeout")
	}

	out := buf.String()
	if !strings.Contains(out, "[Swarm] Phase 1: Active") {
		t.Errorf("expected rendered worker line in render loop output, got: %s", out)
	}
}

// TestSwarmHUD_ConcurrentUpdates verifies thread safety when multiple goroutines
// concurrently invoke UpdateWorker, LogEvent, and Render.
func TestSwarmHUD_ConcurrentUpdates(t *testing.T) {
	var buf bytes.Buffer
	hud := NewSwarmHUD(&buf)
	hud.isTerminal = true

	var wg sync.WaitGroup
	const workers = 8
	const iterations = 50

	for i := 1; i <= workers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 1; j <= iterations; j++ {
				hud.UpdateWorker(SwarmWorkerStatus{
					WorkerID:   workerID,
					PID:        1000 + workerID,
					PhaseName:  fmt.Sprintf("Phase %d", workerID),
					Stage:      fmt.Sprintf("Turn %d", j),
					TurnCount:  j,
					MaxTurns:   iterations,
					TokensUsed: workerID * 100 * j,
					Status:     "[ACTIVE]",
				})
				hud.LogEvent(fmt.Sprintf("Worker %d step %d", workerID, j))
			}
		}()
	}

	// Concurrent reader/renderer
	renderStop := make(chan struct{})
	var renderWg sync.WaitGroup
	renderWg.Add(1)
	go func() {
		defer renderWg.Done()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-renderStop:
				return
			case <-ticker.C:
				hud.Render()
			}
		}
	}()

	wg.Wait()
	close(renderStop)
	renderWg.Wait()

	if hud.WorkerCount() != workers {
		t.Errorf("expected %d workers registered, got %d", workers, hud.WorkerCount())
	}

	// Verify all workers exist
	for i := 1; i <= workers; i++ {
		w, ok := hud.GetWorker(i)
		if !ok || w == nil {
			t.Fatalf("worker %d missing", i)
		}
		if w.TurnCount != iterations {
			t.Errorf("worker %d expected turn count %d, got %d", i, iterations, w.TurnCount)
		}
	}
}

// TestSwarmHUD_RollingEventTicker verifies that the rolling event ticker in ANSI mode
// retains and displays the 5 most recent events.
func TestSwarmHUD_RollingEventTicker(t *testing.T) {
	var buf bytes.Buffer
	hud := NewSwarmHUD(&buf)
	hud.isTerminal = true

	for i := 1; i <= 10; i++ {
		hud.LogEvent(fmt.Sprintf("Event #%d: processing milestone", i))
	}

	hud.Render()
	out := buf.String()

	// Should show events #6 through #10
	for i := 6; i <= 10; i++ {
		expected := fmt.Sprintf("Event #%d: processing milestone", i)
		if !strings.Contains(out, expected) {
			t.Errorf("expected recent event %q in rolling ticker, got:\n%s", expected, out)
		}
	}

	// Should not show older events in the ticker table
	for i := 1; i <= 5; i++ {
		stale := fmt.Sprintf("Event #%d: processing milestone", i)
		if strings.Contains(out, stale) {
			t.Errorf("stale event %q should have been rolled out of the 5-line ticker", stale)
		}
	}
}
