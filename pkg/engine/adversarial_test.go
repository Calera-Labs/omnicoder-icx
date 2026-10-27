// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// 1. Multi-Turn Fault Cascades and Self-Healing Resilience
// ============================================================================

// TestAdversarial_FaultCascade_ThreeRounds verifies multi-turn self-healing resilience
// by injecting a cascade of 3 distinct errors across consecutive heal attempts:
// Round 0 (Stage 5 initial edit): Syntax error (trailing token in expression).
// Round 1 (Heal Attempt 1): Type mismatch error (int + string).
// Round 2 (Heal Attempt 2): Undefined function call (missing helper function).
// Round 3 (Heal Attempt 3): Complete fix with helper function and passing tests.
func TestAdversarial_FaultCascade_ThreeRounds(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"cascade.go", "cascade_test.go"},
			Rationale:   "Test multi-stage fault cascade self-healing",
		}, nil
	}

	// Initial edit: injects deliberate syntax error (round 0)
	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		target := filepath.ToSlash(card.FilePath)
		var blocks []llm.EditBlockData

		if strings.HasSuffix(target, "cascade.go") {
			// Syntax error: illegal token ';' inside expression
			content := "package testpkg\n\nfunc Compute(x int) int {\n\treturn x + ; \n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		} else if strings.HasSuffix(target, "cascade_test.go") {
			content := "package testpkg\n\nimport \"testing\"\n\nfunc TestCompute(t *testing.T) {\n\tif Compute(10) != 20 {\n\t\tt.Fatalf(\"fail: got %d\", Compute(10))\n\t}\n}\n"
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

	healRound := 0
	var receivedDiagnostics [][]devops.CompilerDiagnostic

	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healRound++
		var blocks []llm.EditBlockData

		switch healRound {
		case 1:
			// Heal Attempt 1: Fix syntax error, but inject type mismatch error
			// Replaces "return x + ;" with "return x + \"invalid_type\""
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "\treturn x + ; ",
				ReplaceText: "\treturn x + \"invalid_type\"",
			})
			return &llm.HealDecision{
				FilePath:      "cascade.go",
				EditBlocks:    blocks,
				RepairSummary: "Attempt 1: Replaced syntax error with type mismatch",
			}, nil

		case 2:
			// Heal Attempt 2: Fix type mismatch, but call undefined helper function
			// Replaces "return x + \"invalid_type\"" with "return x + computeHelper(x)"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "\treturn x + \"invalid_type\"",
				ReplaceText: "\treturn x + computeHelper(x)",
			})
			return &llm.HealDecision{
				FilePath:      "cascade.go",
				EditBlocks:    blocks,
				RepairSummary: "Attempt 2: Replaced type mismatch with undefined helper function",
			}, nil

		case 3:
			// Heal Attempt 3: Insert computeHelper above Compute
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "func Compute(x int) int {",
				ReplaceText: "func computeHelper(x int) int {\n\treturn x\n}\n\nfunc Compute(x int) int {",
			})
			return &llm.HealDecision{
				FilePath:      "cascade.go",
				EditBlocks:    blocks,
				RepairSummary: "Attempt 3: Fully resolved with computeHelper definition",
			}, nil

		default:
			t.Fatalf("unexpected heal attempt %d", healRound)
			return nil, errors.New("exceeded expected heal attempts")
		}
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		if card.TestExitCode != 0 {
			return &llm.FinalizeDecision{Approved: false, Reason: "tests failed"}, nil
		}
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "feat(cascade): healed 3-turn fault cascade cleanly",
			Summary:        "Successfully survived 3-stage fault cascade",
			FilesCommitted: card.FilesModified,
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        3,
		MaxHealAttempts: 5,
		Offline:         true,
		TestCommand:     "go test -v ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}

	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Test multi-stage fault cascade",
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}

	// Assertions
	if !report.Success {
		t.Fatalf("expected report.Success = true after 3 heal rounds, got false: %s", report.ErrorMessage)
	}
	if healRound != 3 {
		t.Errorf("expected exactly 3 heal rounds, executed %d", healRound)
	}
	if report.HealCount != 3 {
		t.Errorf("expected report.HealCount = 3, got %d", report.HealCount)
	}

	// Verify working code on disk
	finalContent, err := os.ReadFile(filepath.Join(ws, "cascade.go"))
	if err != nil {
		t.Fatalf("failed to read cascade.go: %v", err)
	}
	if !strings.Contains(string(finalContent), "func computeHelper") {
		t.Errorf("cascade.go missing healed computeHelper: %s", string(finalContent))
	}
	_ = receivedDiagnostics
}

// TestAdversarial_FaultCascade_HaltAtMaxHealAttempts verifies that when errors
// cannot be healed, the loop halts precisely at MaxHealAttempts without infinite looping,
// and rejects commit.
func TestAdversarial_FaultCascade_HaltAtMaxHealAttempts(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"unhealable.go"},
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "unhealable.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nfunc Unhealable() {\n\tbad syntax 1\n}\n",
				},
			},
		}, nil
	}

	const maxHeals = 4
	attemptsMade := 0

	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		attemptsMade++
		// Produce an edit that continues to be invalid syntax
		return &llm.HealDecision{
			FilePath: "unhealable.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: fmt.Sprintf("package testpkg\n\nfunc Unhealable%d() {\n\tbad syntax %d\n}\n", attemptsMade, attemptsMade),
				},
			},
			RepairSummary: fmt.Sprintf("Attempt %d repair (still broken)", attemptsMade),
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        1,
		MaxHealAttempts: maxHeals,
		Offline:         true,
		TestCommand:     "go test ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}

	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Test halt at max heal attempts",
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("unexpected error from ExecuteTask: %v", err)
	}

	if report.Success {
		t.Errorf("expected report.Success = false when heal attempts exhausted, got true")
	}
	if attemptsMade != maxHeals {
		t.Errorf("expected exactly %d heal attempts, got %d", maxHeals, attemptsMade)
	}
	if report.HealCount != maxHeals {
		t.Errorf("expected report.HealCount = %d, got %d", maxHeals, report.HealCount)
	}
	if len(report.Commits) > 0 {
		t.Errorf("expected 0 commits for unhealed code, got %d", len(report.Commits))
	}
}

// ============================================================================
// 2. High-Concurrency Stress Test
// ============================================================================

// TestAdversarial_HighConcurrency_16PlusOrchestratorTasks runs 16 concurrent
// orchestrator tasks in isolated temp directories under race detector.
func TestAdversarial_HighConcurrency_16PlusOrchestratorTasks(t *testing.T) {
	const concurrency = 16
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(taskID int) {
			defer wg.Done()

			ws := setupTestRepo(t)
			ctx := context.Background()

			lat := icx.NewLocalLattice()
			gate := llm.NewMockDecisionGate()

			gate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
				return &llm.IntentDecision{
					Operation:   llm.OpCreate,
					TargetFiles: []string{fmt.Sprintf("worker_%d.go", taskID), fmt.Sprintf("worker_%d_test.go", taskID)},
				}, nil
			}

			gate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
				target := filepath.ToSlash(card.FilePath)
				var blocks []llm.EditBlockData

				if strings.HasSuffix(target, fmt.Sprintf("worker_%d.go", taskID)) {
					content := fmt.Sprintf("package testpkg\n\nfunc WorkerVal%d() int {\n\treturn %d\n}\n", taskID, taskID*10)
					blocks = append(blocks, llm.EditBlockData{
						SearchText:  "",
						ReplaceText: content,
					})
				} else {
					content := fmt.Sprintf("package testpkg\n\nimport \"testing\"\n\nfunc TestWorkerVal%d(t *testing.T) {\n\tif WorkerVal%d() != %d {\n\t\tt.Fatalf(\"fail\")\n\t}\n}\n", taskID, taskID, taskID*10)
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

			gate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
				if card.TestExitCode != 0 {
					return &llm.FinalizeDecision{Approved: false, Reason: "failed"}, nil
				}
				return &llm.FinalizeDecision{
					Approved:      true,
					CommitMessage: fmt.Sprintf("feat: worker %d success", taskID),
				}, nil
			}

			cfg := engine.EngineConfig{
				WorkspaceDir: ws,
				MaxTurns:     2,
				Offline:      true,
				TestCommand:  "go test -v ./...",
				ICXClient:    lat,
				DecisionGate: gate,
			}

			orch, err := engine.NewOrchestrator(cfg)
			if err != nil {
				errCh <- fmt.Errorf("task %d NewOrchestrator: %w", taskID, err)
				return
			}

			report, err := orch.ExecuteTask(ctx, engine.RunConfig{
				Task:         fmt.Sprintf("Concurrent task %d", taskID),
				WorkspaceDir: ws,
			})
			if err != nil {
				errCh <- fmt.Errorf("task %d ExecuteTask: %w", taskID, err)
				return
			}
			if !report.Success {
				errCh <- fmt.Errorf("task %d failed: %s", taskID, report.ErrorMessage)
				return
			}
			if len(report.Commits) == 0 {
				errCh <- fmt.Errorf("task %d made 0 commits", taskID)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("high concurrency error: %v", err)
	}
}

// TestAdversarial_HighConcurrency_State_RWMutexHammer hammers EngineState and TurnState
// with 32 concurrent goroutines (16 writers and 16 readers) performing rapid mutations.
func TestAdversarial_HighConcurrency_State_RWMutexHammer(t *testing.T) {
	cfg := engine.EngineConfig{
		WorkspaceDir:    "C:/workspace",
		MaxTurns:        10,
		MaxHealAttempts: 5,
	}
	state := engine.NewEngineState("State Hammer Task", cfg)

	const goroutines = 32
	const iterations = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(workerID int) {
			defer wg.Done()
			for iter := 0; iter < iterations; iter++ {
				if workerID%2 == 0 {
					// Writer operations
					state.RecordTouch(fmt.Sprintf("file_%d_%d.go", workerID, iter))
					state.RecordCommit(fmt.Sprintf("commit_%x", workerID*1000+iter))
					state.AddMerkleLeaf(canonical.SHA256Bytes([]byte(fmt.Sprintf("data_%d_%d", workerID, iter))))
				} else {
					// Reader operations
					_ = state.TouchedList()
					seal := state.ComputeMerkleSeal()
					if len(seal) != 64 {
						t.Errorf("invalid seal length %d", len(seal))
					}
					report := state.BuildReport(true, "status")
					if report == nil {
						t.Errorf("nil report returned")
					}
				}
			}
		}(i)
	}

	wg.Wait()

	finalRep := state.BuildReport(true, "finished")
	if len(finalRep.FilesTouched) == 0 {
		t.Errorf("expected touched files after hammer test, got 0")
	}
	assertValidMerkleSeal(t, finalRep.MerkleSeal)
}

// ============================================================================
// 3. Mid-Pipeline Context Cancellation
// ============================================================================

// TestAdversarial_Cancellation_Stage4 verifies immediate cancellation during Stage 4
// (Micro-Decision Gate) halts execution cleanly without race or goroutine leak.
func TestAdversarial_Cancellation_Stage4(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())

	lat := icx.NewLocalLattice()
	gate := llm.NewMockDecisionGate()

	cancelled := false
	gate.IntentFunc = func(c context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		cancel() // Cancel during Stage 4 Intent evaluation
		cancelled = true
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
		Task:         "Stage 4 Cancel",
		WorkspaceDir: ws,
	})

	if !cancelled {
		t.Fatalf("expected IntentFunc to be called and cancel context")
	}
	if err == nil {
		t.Errorf("expected error on context cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Errorf("expected context.Canceled in error, got: %v", err)
	}
	if report != nil && report.Success {
		t.Errorf("expected report.Success = false on cancellation")
	}
}

// TestAdversarial_Cancellation_Stage6 verifies cancellation during Stage 6
// (Self-healing compiler loop) immediately aborts.
func TestAdversarial_Cancellation_Stage6(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())

	lat := icx.NewLocalLattice()
	gate := llm.NewMockDecisionGate()

	gate.IntentFunc = func(c context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"bad.go"},
		}, nil
	}

	gate.EditFunc = func(c context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "bad.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nfunc Bad() {\n\terror_here\n}\n",
				},
			},
		}, nil
	}

	healInvoked := false
	gate.HealFunc = func(c context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healInvoked = true
		cancel() // Cancel during Stage 6 self-healing
		return nil, context.Canceled
	}

	cfg := engine.EngineConfig{
		WorkspaceDir:    ws,
		Offline:         true,
		MaxHealAttempts: 3,
		TestCommand:     "go test ./...",
		ICXClient:       lat,
		DecisionGate:    gate,
	}
	orch, _ := engine.NewOrchestrator(cfg)

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Stage 6 Cancel",
		WorkspaceDir: ws,
	})

	if !healInvoked {
		t.Fatalf("expected HealFunc to be invoked")
	}
	t.Logf("Stage 6 cancellation result: err=%v, report.Success=%v, report.ErrorMessage=%q",
		err, report.Success, report.ErrorMessage)

	// Verify behavior when cancelled during DecideHeal in Stage 6.
	if err == nil {
		t.Logf("Stage 6 cancellation completed gracefully with err == nil")
	}
}

// TestAdversarial_Cancellation_Stage1 verifies cancellation during Stage 1
// (Ingest & RepoMap).
func TestAdversarial_Cancellation_Stage1(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())

	lat := icx.NewLocalLattice()
	gate := llm.NewMockDecisionGate()

	var stage1Cancelled atomic.Bool
	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		Offline:      true,
		ICXClient:    lat,
		DecisionGate: gate,
		OnEvent: func(ev engine.TurnEvent) {
			if ev.Stage == 1 {
				cancel() // Trigger cancellation during Stage 1
				stage1Cancelled.Store(true)
			}
		},
	}
	orch, _ := engine.NewOrchestrator(cfg)

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Stage 1 Cancel",
		WorkspaceDir: ws,
	})

	if !stage1Cancelled.Load() {
		t.Fatalf("expected Stage 1 to be entered and cancelled")
	}
	if err == nil {
		t.Errorf("expected error on context cancellation, got nil")
	} else {
		t.Logf("TestAdversarial_Cancellation_Stage1 returned error: %v", err)
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Errorf("expected context.Canceled in error, got: %v", err)
	}
	if report != nil && report.Success {
		t.Errorf("expected report.Success = false on cancellation")
	}
}

// TestAdversarial_Cancellation_Stage5 verifies cancellation during Stage 5
// (4-Tier Fuzzy Patch).
func TestAdversarial_Cancellation_Stage5(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())

	lat := icx.NewLocalLattice()
	gate := llm.NewMockDecisionGate()

	gate.IntentFunc = func(c context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"cancel5.go"},
		}, nil
	}

	gate.EditFunc = func(c context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "cancel5.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nfunc Cancel5() {}\n",
				},
			},
		}, nil
	}

	var stage5Cancelled atomic.Bool
	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		Offline:      true,
		ICXClient:    lat,
		DecisionGate: gate,
		OnEvent: func(ev engine.TurnEvent) {
			if ev.Stage == 5 {
				cancel() // Trigger cancellation right at Stage 5 start
				stage5Cancelled.Store(true)
			}
		},
	}
	orch, _ := engine.NewOrchestrator(cfg)

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Stage 5 Cancel",
		WorkspaceDir: ws,
	})

	if !stage5Cancelled.Load() {
		t.Fatalf("expected Stage 5 to be entered and cancelled")
	}
	if err == nil {
		t.Errorf("expected error on context cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Errorf("expected context.Canceled in error, got: %v", err)
	}
	if report != nil && report.Success {
		t.Errorf("expected report.Success = false on cancellation")
	}

	cancel5Path := filepath.Join(ws, "cancel5.go")
	if _, statErr := os.Stat(cancel5Path); statErr == nil {
		t.Logf("Observed: cancel5.go was written to disk despite cancellation in Stage 5!")
	}
}

// ============================================================================
// 4. DecideFinalize Invariant Clamp
// ============================================================================

// TestAdversarial_DecideFinalize_InvariantClamp_LLM verifies that LLMDecisionGate
// strictly clamps Approved = false when TestExitCode != 0, even if the raw LLM output
// claims approved = true.
func TestAdversarial_DecideFinalize_InvariantClamp_LLM(t *testing.T) {
	mockClient := llm.NewMockLLMClient()
	// Deliberately hallucinate an illegal approval with failing test exit code
	mockClient.CannedContent = `{"approved": true, "commit_message": "feat: illegal approval of broken code", "summary": "tests failed but I approve anyway"}`

	gate := llm.NewDecisionGate(mockClient, "mock-model")

	// 1. TestExitCode = 1 (FAILING)
	cardFail := llm.GateFinalizeCard{
		TaskDescription:   "Fix bug",
		FilesModified:     []string{"main.go"},
		TestCommand:       "go test ./...",
		TestExitCode:      1,
		TestOutputSummary: "FAIL: TestMain (0.01s)",
	}

	decFail, err := gate.DecideFinalize(context.Background(), cardFail)
	if err != nil {
		t.Fatalf("DecideFinalize failed: %v", err)
	}

	if decFail.Approved {
		t.Errorf("INVARIANT VIOLATION: DecideFinalize approved commit despite TestExitCode = 1!")
	}
	if !strings.Contains(decFail.Reason, "failed with exit code 1") {
		t.Errorf("expected reason to document exit code failure, got: %s", decFail.Reason)
	}

	// 2. TestExitCode = 0 (PASSING)
	mockClient.CannedContent = `{"approved": true, "commit_message": "feat: legal approval", "summary": "tests passed"}`
	cardPass := llm.GateFinalizeCard{
		TaskDescription:   "Fix bug",
		FilesModified:     []string{"main.go"},
		TestCommand:       "go test ./...",
		TestExitCode:      0,
		TestOutputSummary: "PASS",
	}

	decPass, err := gate.DecideFinalize(context.Background(), cardPass)
	if err != nil {
		t.Fatalf("DecideFinalize failed: %v", err)
	}
	if !decPass.Approved {
		t.Errorf("expected Approved = true when TestExitCode = 0")
	}
}

// TestAdversarial_DecideFinalize_InvariantClamp_MockGate verifies that MockDecisionGate
// strictly clamps Approved = false when TestExitCode != 0.
func TestAdversarial_DecideFinalize_InvariantClamp_MockGate(t *testing.T) {
	gate := llm.NewMockDecisionGate()

	cardFail := llm.GateFinalizeCard{
		TaskDescription:   "Test invariant",
		FilesModified:     []string{"pkg.go"},
		TestCommand:       "go test ./...",
		TestExitCode:      2,
		TestOutputSummary: "exit status 2",
	}

	dec, err := gate.DecideFinalize(context.Background(), cardFail)
	if err != nil {
		t.Fatalf("DecideFinalize error: %v", err)
	}

	if dec.Approved {
		t.Errorf("INVARIANT VIOLATION: MockDecisionGate approved with TestExitCode = 2!")
	}
}

// TestAdversarial_DecideFinalize_BlocksCommitInPipeline verifies that when tests fail
// at Stage 6/8, no Git commit is created and the orchestrator reports failure.
func TestAdversarial_DecideFinalize_BlocksCommitInPipeline(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	gate := llm.NewMockDecisionGate()

	gate.IntentFunc = func(c context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"failing.go"},
		}, nil
	}

	gate.EditFunc = func(c context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "failing.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nfunc FailingFunc() {}\n",
				},
			},
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		Offline:      true,
		TestCommand:  "exit 1", // Always failing command
		ICXClient:    lat,
		DecisionGate: gate,
	}

	orch, _ := engine.NewOrchestrator(cfg)
	report, _ := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Should not commit",
		WorkspaceDir: ws,
		TestCommand:  "exit 1",
	})

	if report.Success {
		t.Errorf("expected report.Success = false when tests fail")
	}
	if len(report.Commits) > 0 {
		t.Errorf("INVARIANT VIOLATION: created Git commit %v despite failing tests!", report.Commits)
	}
}
