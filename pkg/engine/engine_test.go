// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// Test Fixtures & Helpers
// ============================================================================

var testRepoSeq atomic.Int64

// setupTestRepo initializes an isolated Git repository in t.TempDir() with
// test author metadata and a valid root go.mod so `go test` can execute.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	seq := testRepoSeq.Add(1)

	// 1. Initialize Git repository
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.name", "OmniCoder Tester")
	runCmd(t, dir, "git", "config", "user.email", "support@caleralabs.com")

	// 2. Write valid root go.mod with unique module name to prevent Go build cache collision
	goModContent := fmt.Sprintf("module example.com/testpkg%d\n\ngo 1.22\n", seq)
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644)
	if err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	// 3. Initial commit to set main/master branch
	runCmd(t, dir, "git", "add", "go.mod")
	runCmd(t, dir, "git", "commit", "-m", "chore: initial commit")

	return dir
}

// runCmd executes an external command in dir, failing the test on error.
func runCmd(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command %s %v failed in %s: %v\nOutput:\n%s", name, args, dir, err, string(out))
	}
	return string(out)
}

// assertValidMerkleSeal verifies that the seal is a 64-character hex SHA-256 string.
func assertValidMerkleSeal(t *testing.T, seal string) {
	t.Helper()
	if len(seal) != 64 {
		t.Errorf("expected 64-char hex Merkle seal, got len %d: %q", len(seal), seal)
	}
	if _, err := hex.DecodeString(seal); err != nil {
		t.Errorf("Merkle seal is not valid hex: %v", err)
	}
}

// findRepoRoot resolves the root directory of the omnicoder repository.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("failed to determine repo root: %v", err)
	}
	return abs
}

// ============================================================================
// Suite 1: End-to-End Autonomous Loop Testing
// ============================================================================

// TestEngine_EndToEnd_CreateNewPackage verifies the complete 8-stage turn pipeline
// running offline with LocalLattice and MockDecisionGate to create a new Go package.
func TestEngine_EndToEnd_CreateNewPackage(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	// 1. Configure in-memory LocalLattice and MockDecisionGate
	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	// Program MockGate for the creation task
	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc.go", "calc_test.go"},
			Rationale:   "Create basic arithmetic package with Add function and unit tests",
			Confidence:  0.99,
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		target := filepath.ToSlash(card.FilePath)
		var blocks []llm.EditBlockData

		if strings.HasSuffix(target, "calc.go") {
			content := "package testpkg\n\n// Add returns the sum of two integers.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		} else if strings.HasSuffix(target, "calc_test.go") {
			content := "package testpkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"expected 5, got %d\", Add(2, 3))\n\t}\n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		}

		return &llm.EditDecision{
			FilePath:    target,
			EditBlocks:  blocks,
			Explanation: fmt.Sprintf("Generated implementation for %s", target),
		}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		if card.TestExitCode != 0 {
			return &llm.FinalizeDecision{Approved: false, Reason: "tests failed"}, nil
		}
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "feat(calc): implement Add function and unit tests",
			Summary:        "Successfully added calc.go and calc_test.go",
			FilesCommitted: card.FilesModified,
			Reason:         "all automated tests passed cleanly",
		}, nil
	}

	// 2. Initialize Engine
	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        3,
		MaxHealAttempts: 3,
		Offline:         true,
		TestCommand:     "go test -v ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}
	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}

	// 3. Execute Autonomous Task
	task := "Create a calculator package with an Add function and unit tests"
	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         task,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}

	// 4. Validate RunReport
	if !report.Success {
		t.Fatalf("expected report.Success = true, got false (err: %s)", report.ErrorMessage)
	}
	if report.TurnsCount != 1 {
		t.Errorf("expected 1 turn, got %d", report.TurnsCount)
	}
	if report.HealCount != 0 {
		t.Errorf("expected 0 heals for clean code, got %d", report.HealCount)
	}
	assertValidMerkleSeal(t, report.MerkleSeal)

	// 5. Verify Files on Disk
	calcPath := filepath.Join(ws, "calc.go")
	testPath := filepath.Join(ws, "calc_test.go")
	if _, err := os.Stat(calcPath); os.IsNotExist(err) {
		t.Fatalf("expected calc.go to exist on disk")
	}
	if _, err := os.Stat(testPath); os.IsNotExist(err) {
		t.Fatalf("expected calc_test.go to exist on disk")
	}

	calcContent, _ := os.ReadFile(calcPath)
	if !strings.Contains(string(calcContent), "func Add(a, b int) int") {
		t.Errorf("calc.go does not contain Add function signature: %s", string(calcContent))
	}

	// 6. Verify AST Symbols via RepoMap
	rmRes, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: ws,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed: %v", err)
	}
	if rmRes.SymbolsFound < 2 {
		t.Errorf("expected at least 2 AST symbols, found %d", rmRes.SymbolsFound)
	}
	foundAdd := false
	foundTestAdd := false
	for _, sym := range rmRes.TopSymbols {
		if sym.Name == "Add" {
			foundAdd = true
		}
		if sym.Name == "TestAdd" {
			foundTestAdd = true
		}
	}
	if !foundAdd || !foundTestAdd {
		t.Errorf("RepoMap missing expected symbols: foundAdd=%v, foundTestAdd=%v", foundAdd, foundTestAdd)
	}

	// 7. Verify Git Commit
	logOut := runCmd(t, ws, "git", "log", "-1", "--pretty=%B")
	if !strings.Contains(logOut, "feat(calc): implement Add function and unit tests") {
		t.Errorf("git commit message mismatch: %s", logOut)
	}

	// 8. Verify Clean Working Tree
	statusOut := strings.TrimSpace(runCmd(t, ws, "git", "status", "--porcelain"))
	if statusOut != "" {
		t.Errorf("expected clean git working tree, got:\n%s", statusOut)
	}
}

// ============================================================================
// Suite 2: Self-Healing Verification
// ============================================================================

// TestEngine_SelfHealing_UndefinedSymbol verifies that the engine intercepts a
// synthetic undefined variable error, captures the 5-line diagnostic window,
// calls DecideHeal to apply a surgical fix, and finishes with Success: true.
func TestEngine_SelfHealing_UndefinedSymbol(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	// Intent: Target calc.go and calc_test.go
	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc.go", "calc_test.go"},
			Rationale:   "Create calculator with initial deliberate fault",
			Confidence:  0.95,
		}, nil
	}

	// EditFunc: Injects deliberate undefined symbol `undefinedOffset`
	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		target := filepath.ToSlash(card.FilePath)
		var blocks []llm.EditBlockData

		if strings.HasSuffix(target, "calc.go") {
			// Buggy code: undefined variable `undefinedOffset`
			content := "package testpkg\n\nfunc Add(a, b int) int {\n\treturn a + b + undefinedOffset\n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		} else if strings.HasSuffix(target, "calc_test.go") {
			content := "package testpkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"fail\")\n\t}\n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		}

		return &llm.EditDecision{
			FilePath:   target,
			EditBlocks: blocks,
		}, nil
	}

	// HealFunc: Intercepts compiler feedback and applies surgical fix
	healInvoked := false
	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healInvoked = true

		// 1. Verify card received accurate 5-line snippet and diagnostics
		if card.ErrorLine != 4 {
			t.Errorf("expected ErrorLine = 4, got %d", card.ErrorLine)
		}
		if !strings.Contains(card.ErrorMessage, "undefined") {
			t.Errorf("expected ErrorMessage to mention 'undefined', got: %s", card.ErrorMessage)
		}
		if !strings.Contains(card.Snippet5Lines, "undefinedOffset") {
			t.Errorf("expected Snippet5Lines to contain 'undefinedOffset', got:\n%s", card.Snippet5Lines)
		}
		if !strings.Contains(card.Snippet5Lines, ">") {
			t.Errorf("expected Snippet5Lines to contain '>' line marker, got:\n%s", card.Snippet5Lines)
		}

		// 2. Synthesize surgical replacement
		return &llm.HealDecision{
			FilePath: "calc.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "\treturn a + b + undefinedOffset",
					ReplaceText: "\treturn a + b",
				},
			},
			RepairSummary: "Removed undefined variable reference undefinedOffset",
			RootCause:     "undefined identifier in return statement",
		}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		if card.TestExitCode != 0 {
			return &llm.FinalizeDecision{Approved: false, Reason: "tests still failing"}, nil
		}
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "fix(calc): repair undefined variable and pass tests",
			Summary:        "Healed undefinedOffset fault",
			FilesCommitted: card.FilesModified,
			Reason:         "clean compilation verified",
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        3,
		MaxHealAttempts: 3,
		Offline:         true,
		TestCommand:     "go test -v ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}
	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Implement Add and heal any compilation errors",
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}

	// Assertions
	if !healInvoked {
		t.Fatalf("expected DecideHeal to be invoked, but it was not")
	}
	if !report.Success {
		t.Fatalf("expected report.Success = true after self-healing, got false: %s", report.ErrorMessage)
	}
	if report.HealCount != 1 {
		t.Errorf("expected HealCount = 1, got %d", report.HealCount)
	}

	// Verify repaired file on disk
	content, _ := os.ReadFile(filepath.Join(ws, "calc.go"))
	if strings.Contains(string(content), "undefinedOffset") {
		t.Errorf("calc.go still contains undefinedOffset after healing: %s", string(content))
	}
	if !strings.Contains(string(content), "return a + b") {
		t.Errorf("calc.go missing healed return statement: %s", string(content))
	}
}

// TestEngine_SelfHealing_MaxAttemptsExhausted verifies that if surgical repairs fail
// repeatedly, the engine halts when MaxHealAttempts is reached, reporting failure
// without committing broken code.
func TestEngine_SelfHealing_MaxAttemptsExhausted(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"bad.go"},
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "bad.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nfunc Broken() {\n\tsyntax error here\n}\n",
				},
			},
		}, nil
	}

	// Ineffective heal: keeps syntax error broken across all attempts
	healAttempts := 0
	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healAttempts++
		return &llm.HealDecision{
			FilePath: "bad.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: fmt.Sprintf("package testpkg\n\nfunc Broken%d() {\n\tsyntax error here\n}\n", healAttempts),
				},
			},
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        1,
		MaxHealAttempts: 3,
		Offline:         true,
		TestCommand:     "go test ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}
	orch, _ := engine.NewOrchestrator(cfg)

	report, _ := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Fail repeatedly",
		WorkspaceDir: ws,
	})

	if report.Success {
		t.Errorf("expected report.Success = false when heal attempts exhausted")
	}
	if report.HealCount != 3 {
		t.Errorf("expected report.HealCount = 3, got %d", report.HealCount)
	}
	if healAttempts != 3 {
		t.Errorf("expected 3 heal attempts, got %d", healAttempts)
	}
	if len(report.Commits) > 0 {
		t.Errorf("expected 0 commits created for unhealed broken code, got %d", len(report.Commits))
	}
}

// ============================================================================
// Suite 3: Standalone CLI Verification Tests
// ============================================================================

// TestCLI_Map_TimingAndOutput tests executing `omnicoder map` in a temporary
// workspace, validating personalized PageRank AST output within 2.0s.
func TestCLI_Map_TimingAndOutput(t *testing.T) {
	ws := t.TempDir()

	// Populate workspace with multiple Go files defining types, structs, and calls
	files := map[string]string{
		"model.go":   "package main\n\ntype User struct {\n\tID int\n\tName string\n}\n",
		"store.go":   "package main\n\ntype Store interface {\n\tGetUser(id int) (*User, error)\n}\n",
		"service.go": "package main\n\ntype Service struct {\n\tStore Store\n}\n\nfunc (s *Service) Find(id int) (*User, error) {\n\treturn s.Store.GetUser(id)\n}\n",
		"main.go":    "package main\n\nfunc main() {\n\tsvc := &Service{}\n\t_, _ = svc.Find(1)\n}\n",
	}
	for name, content := range files {
		err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0644)
		if err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	// 1. Build omnicoder.exe in temp directory
	repoRoot := findRepoRoot(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "omnicoder.exe")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/omnicoder")
	buildCmd.Dir = repoRoot
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build omnicoder binary: %v\nOutput:\n%s", err, string(out))
	}

	// 2. Execute `omnicoder map` and measure wall-clock duration
	start := time.Now()
	mapCmd := exec.Command(binPath, "map", "--workspace", ws)
	mapOut, err := mapCmd.CombinedOutput()
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("`omnicoder map` failed: %v\nOutput:\n%s", err, string(mapOut))
	}

	// Invariant: Completed strictly within 2.0s
	if elapsed >= 2000*time.Millisecond {
		t.Errorf("`omnicoder map` took %v, exceeded 2.0s timing invariant", elapsed)
	}

	outStr := string(mapOut)
	if !strings.Contains(outStr, "User") || !strings.Contains(outStr, "Service") {
		t.Errorf("`omnicoder map` output missing expected AST symbols:\n%s", outStr)
	}
}

// TestCLI_Run_Offline tests executing `omnicoder run "..."` with `--offline` flag.
func TestCLI_Run_Offline(t *testing.T) {
	ws := setupTestRepo(t)
	repoRoot := findRepoRoot(t)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "omnicoder.exe")

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/omnicoder")
	buildCmd.Dir = repoRoot
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, string(out))
	}

	// Run omnicoder with --offline
	runCmd := exec.Command(binPath, "run", "Verify offline execution", "--workspace", ws, "--offline", "--max-turns", "1")
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`omnicoder run --offline` failed: %v\nOutput:\n%s", err, string(runOut))
	}

	outStr := string(runOut)
	if !strings.Contains(outStr, "SUCCESS") && !strings.Contains(outStr, "completed") && !strings.Contains(outStr, "Merkle") {
		t.Logf("CLI Output:\n%s", outStr)
	}
}

// ============================================================================
// Suite 4: Concurrency & Race Condition Safety
// ============================================================================

// TestEngine_Concurrent_Orchestrator_Runs spawns 8 concurrent orchestrator
// tasks, verifying zero data race warnings under `go test -race`.
func TestEngine_Concurrent_Orchestrator_Runs(t *testing.T) {
	const concurrency = 8
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ws := setupTestRepo(t)
			ctx := context.Background()

			lat := icx.NewLocalLattice()
			gate := llm.NewMockDecisionGate()

			gate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
				return &llm.IntentDecision{
					Operation:   llm.OpCreate,
					TargetFiles: []string{fmt.Sprintf("worker_%d.go", id)},
				}, nil
			}

			gate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
				return &llm.EditDecision{
					FilePath: card.FilePath,
					EditBlocks: []llm.EditBlockData{
						{
							SearchText:  "",
							ReplaceText: fmt.Sprintf("package testpkg\n\nvar WorkerID%d = %d\n", id, id),
						},
					},
				}, nil
			}

			gate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
				return &llm.FinalizeDecision{
					Approved:      true,
					CommitMessage: fmt.Sprintf("feat(worker): add worker %d", id),
				}, nil
			}

			cfg := engine.EngineConfig{
				WorkspaceDir: ws,
				MaxTurns:     1,
				Offline:      true,
				ICXClient:    lat,
				DecisionGate: gate,
			}
			orch, err := engine.NewOrchestrator(cfg)
			if err != nil {
				errCh <- fmt.Errorf("worker %d orchestrator init: %w", id, err)
				return
			}

			report, err := orch.ExecuteTask(ctx, engine.RunConfig{
				Task:         fmt.Sprintf("Task for worker %d", id),
				WorkspaceDir: ws,
			})
			if err != nil {
				errCh <- fmt.Errorf("worker %d execute task: %w", id, err)
				return
			}
			if !report.Success {
				errCh <- fmt.Errorf("worker %d report failed: %s", id, report.ErrorMessage)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent run error: %v", err)
	}
}

// TestEngine_Context_Cancellation verifies that canceling the turn context
// terminates execution immediately without hanging or leaking goroutines.
func TestEngine_Context_Cancellation(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())

	lat := icx.NewLocalLattice()
	gate := llm.NewMockDecisionGate()

	// Gate that cancels context mid-call
	gate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		cancel() // Trigger cancellation during Stage 4
		return nil, context.Canceled
	}

	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		Offline:      true,
		ICXClient:    lat,
		DecisionGate: gate,
	}
	orch, _ := engine.NewOrchestrator(cfg)

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Will be canceled",
		WorkspaceDir: ws,
	})

	if err == nil && (report != nil && report.Success) {
		t.Errorf("expected execution to fail on cancellation")
	}
}

// TestEngine_State_ConcurrentAccess validates that EngineState methods
// are completely race-free under high-concurrency read/write hammer testing.
func TestEngine_State_ConcurrentAccess(t *testing.T) {
	cfg := engine.EngineConfig{
		WorkspaceDir:    "C:/workspace",
		MaxTurns:        5,
		MaxHealAttempts: 3,
	}
	state := engine.NewEngineState("Concurrent test task", cfg)

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if id%2 == 0 {
					state.RecordTouch(fmt.Sprintf("file_%d_%d.go", id, j))
					state.RecordCommit(fmt.Sprintf("commit_%d_%d", id, j))
					state.AddMerkleLeaf(canonical.SHA256Bytes([]byte(fmt.Sprintf("leaf_%d_%d", id, j))))
				} else {
					_ = state.TouchedList()
					_ = state.ComputeMerkleSeal()
					_ = state.BuildReport(true, "status ok")
				}
			}
		}(i)
	}

	wg.Wait()

	rep := state.BuildReport(true, "done")
	if len(rep.FilesTouched) == 0 {
		t.Errorf("expected touched files, got 0")
	}
	assertValidMerkleSeal(t, rep.MerkleSeal)
}

// TestEngine_Orchestrator_PublicMethods tests Close, Map, Heal, and Refactor.
func TestEngine_Orchestrator_PublicMethods(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		Offline:      true,
	}
	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	// Map
	mapRes, err := orch.Map(ctx, 512)
	if err != nil {
		t.Fatalf("orch.Map failed: %v", err)
	}
	if mapRes.RepoMap == "" {
		t.Errorf("expected non-empty repo map")
	}

	// MapWithQuery
	mapQueryRes, err := orch.MapWithQuery(ctx, 512, "main")
	if err != nil {
		t.Fatalf("orch.MapWithQuery failed: %v", err)
	}
	if mapQueryRes.RepoMap == "" {
		t.Errorf("expected non-empty repo map for MapWithQuery")
	}

	// Heal (clean command)
	healRep, err := orch.Heal(ctx, "go version")
	if err != nil {
		t.Fatalf("orch.Heal failed: %v", err)
	}
	if !healRep.Success {
		t.Errorf("expected clean command to succeed: %s", healRep.ErrorMessage)
	}

	// Refactor
	rfRep, err := orch.Refactor(ctx, "DummySymbol", "func DummySymbol() int")
	if err != nil {
		t.Fatalf("orch.Refactor failed: %v", err)
	}
	_ = rfRep
}

func TestTurnPipeline_SubfolderScopingAndHostPackageProtection(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	// Create projects/telemetry-service directory
	serviceDir := filepath.Join(ws, "projects", "telemetry-service")
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		t.Fatalf("failed to create projects/telemetry-service: %v", err)
	}

	// Mock gate that mimics an LLM returning un-scoped paths
	mockGate := llm.NewMockDecisionGate()
	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation: llm.OpCreate,
			TargetFiles: []string{
				"cmd/telemetry-service/main.go",
				"pkg/engine/core.go",
				"pkg/memory/substrate.go",
			},
			Rationale:  "test un-scoped paths from LLM",
			Confidence: 0.99,
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		Offline:      true,
		DecisionGate: mockGate,
	}
	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	task := "Build projects/telemetry-service engine from specification"
	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         task,
		WorkspaceDir: ws,
		TestCommand:  "go version", // lightweight test command for unit test
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}
	if !report.Success {
		t.Fatalf("expected report.Success = true, got false (err: %s)", report.ErrorMessage)
	}

	// Verify files are strictly inside projects/telemetry-service/
	expectedFiles := []string{
		filepath.Join(serviceDir, "cmd", "main.go"),
		filepath.Join(serviceDir, "pkg", "engine", "core.go"),
		filepath.Join(serviceDir, "pkg", "memory", "substrate.go"),
	}
	for _, ef := range expectedFiles {
		if _, err := os.Stat(ef); os.IsNotExist(err) {
			t.Errorf("expected scoped file %s to exist on disk", ef)
		}
	}

	// Verify host paths are NOT touched
	hostEngineCore := filepath.Join(ws, "pkg", "engine", "core.go")
	if _, err := os.Stat(hostEngineCore); !os.IsNotExist(err) {
		t.Errorf("host package was violated: %s exists!", hostEngineCore)
	}
	hostCmdService := filepath.Join(ws, "cmd", "telemetry-service")
	if _, err := os.Stat(hostCmdService); !os.IsNotExist(err) {
		t.Errorf("host root cmd was violated: %s exists!", hostCmdService)
	}
}

func TestEngine_DeterministicPreHeal_UnusedImport(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc.go"},
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "calc.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nimport \"fmt\"\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
				},
			},
		}, nil
	}

	_ = os.WriteFile(filepath.Join(ws, "calc_test.go"), []byte("package testpkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 { t.Fail() }\n}\n"), 0644)

	healGateInvoked := false
	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healGateInvoked = true
		return &llm.HealDecision{}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "fix(calc): resolve unused import",
			FilesCommitted: card.FilesModified,
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        2,
		MaxHealAttempts: 3,
		Offline:         true,
		TestCommand:     "go test -v ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}
	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Implement Add with clean imports",
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}

	if !report.Success {
		t.Fatalf("expected report.Success = true, got false: %s", report.ErrorMessage)
	}
	if healGateInvoked {
		t.Errorf("expected deterministic pre-heal to fix unused import without invoking DecideHeal gate, but DecideHeal was called")
	}

	content, _ := os.ReadFile(filepath.Join(ws, "calc.go"))
	if strings.Contains(string(content), `"fmt"`) {
		t.Errorf("calc.go still contains unused import 'fmt': %s", string(content))
	}
	if !strings.Contains(string(content), "return a + b") {
		t.Errorf("calc.go lost substantive logic: %s", string(content))
	}
}

func TestEngine_DeterministicPreHeal_CaseTypo(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc_test.go"},
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "calc_test.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif addNumber(1, 2) != 3 { t.Fail() }\n}\n",
				},
			},
		}, nil
	}

	_ = os.WriteFile(filepath.Join(ws, "calc.go"), []byte("package testpkg\n\nfunc AddNumber(a, b int) int {\n\treturn a + b\n}\n"), 0644)

	healGateInvoked := false
	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healGateInvoked = true
		return &llm.HealDecision{}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "fix(calc): resolve case typo",
			FilesCommitted: card.FilesModified,
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        2,
		MaxHealAttempts: 3,
		Offline:         true,
		TestCommand:     "go test -v ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}
	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Test AddNumber function",
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}

	if !report.Success {
		t.Fatalf("expected report.Success = true, got false: %s", report.ErrorMessage)
	}
	if healGateInvoked {
		t.Errorf("expected deterministic pre-heal to fix case typo without invoking DecideHeal gate, but DecideHeal was called")
	}

	content, _ := os.ReadFile(filepath.Join(ws, "calc_test.go"))
	if !strings.Contains(string(content), "AddNumber(1, 2)") {
		t.Errorf("calc_test.go does not contain corrected AddNumber: %s", string(content))
	}
}

func TestAttemptDeterministicPreHeal_InvalidEmbed(t *testing.T) {
	ws := t.TempDir()
	serverDir := filepath.Join(ws, "pkg", "server")
	if err := os.MkdirAll(serverDir, 0755); err != nil {
		t.Fatal(err)
	}

	src := `package server

import "embed"

//go:embed ../../web/static
var staticFiles embed.FS
`
	filePath := filepath.Join(serverDir, "server.go")
	if err := os.WriteFile(filePath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	diag := devops.CompilerDiagnostic{
		File:    "pkg/server/server.go",
		Line:    5,
		Message: "pattern ../../web/static: invalid pattern syntax",
	}

	block, summary, ok := engine.AttemptDeterministicPreHeal(ws, "pkg/server/server.go", diag)
	if !ok {
		t.Fatalf("expected AttemptDeterministicPreHeal to succeed on invalid pattern syntax")
	}
	if !strings.Contains(summary, "invalid //go:embed parent path") {
		t.Errorf("unexpected summary: %s", summary)
	}
	if !strings.Contains(block.ReplaceText, "(embed removed") {
		t.Errorf("unexpected replace text: %s", block.ReplaceText)
	}
}

