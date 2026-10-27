// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestUnboundedExecution_RelentlessPast10Turns verifies that OmniCoder in Unbounded mode
// does not halt at the legacy 10-turn limit, maintaining full memory crystallization
// and persisting turn milestones and checkpoints across 15+ turns until approved.
func TestUnboundedExecution_RelentlessPast10Turns(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	var turnCount atomic.Int64
	targetTurn := int64(15)

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		current := turnCount.Load() + 1
		// Verify Trajectory Ribbon is included in intent card after turn 1
		if current > 1 && card.TrajectoryRibbon == "" {
			t.Errorf("turn %d: expected TrajectoryRibbon in GateIntentCard", current)
		}
		return &llm.IntentDecision{
			Operation:   llm.OpModify,
			TargetFiles: []string{"step.go"},
			Rationale:   fmt.Sprintf("Executing step %d of multi-turn workflow", current),
			Confidence:  0.95,
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		current := turnCount.Load() + 1
		content := fmt.Sprintf("package testpkg\n\n// StepValue records milestone progress.\nconst CurrentStep = %d\n", current)
		return &llm.EditDecision{
			FilePath: "step.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: content,
				},
			},
			Explanation: fmt.Sprintf("Updated step.go for step %d", current),
		}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		current := turnCount.Add(1)
		if current < targetTurn {
			return &llm.FinalizeDecision{
				Approved: false,
				Reason:   fmt.Sprintf("Turn %d completed; further autonomous steps required", current),
			}, nil
		}
		return &llm.FinalizeDecision{
			Approved:      true,
			CommitMessage: fmt.Sprintf("feat: completed all %d autonomous steps", targetTurn),
		}, nil
	}

	spaceID := "space_unbounded_test"

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir:   ws,
		MaxTurns:       0, // Zero signals unbounded
		Unbounded:      true,
		LatticeSpaceID: spaceID,
		ICXClient:      lat,
		DecisionGate:   mockGate,
		TestCommand:    "go test ./...",
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	// Initial step.go so git repo is clean
	_ = os.WriteFile(filepath.Join(ws, "step.go"), []byte("package testpkg\n\nconst CurrentStep = 0\n"), 0644)
	runCmd(t, ws, "git", "add", "step.go")
	runCmd(t, ws, "git", "commit", "-m", "chore: init step.go")

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:           "Implement and verify 15-step autonomous workflow",
		WorkspaceDir:   ws,
		Unbounded:      true,
		LatticeSpaceID: spaceID,
		TestCommand:    "go test ./...",
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}

	if !report.Success {
		t.Fatalf("expected report.Success to be true, got false (output: %s)", report.FinalOutput)
	}

	if report.TurnsCount != int(targetTurn) {
		t.Fatalf("expected TurnsCount to be %d, got %d (must run beyond turn 10 without halting)", targetTurn, report.TurnsCount)
	}

	// Verify exact O(1) recall for step 1, step 10, and step 15
	for _, checkTurn := range []int{1, 10, 15} {
		res, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
			SpaceID: spaceID,
			Family:  "turn_milestones",
			Index:   checkTurn,
		})
		if err != nil {
			t.Fatalf("QuoteSlot for turn %d failed: %v", checkTurn, err)
		}
		if !res.OK || !res.Verified {
			t.Fatalf("QuoteSlot for turn %d not verified: %+v", checkTurn, res)
		}
		expectedSubstr := fmt.Sprintf("turn=%d", checkTurn)
		if !strings.Contains(res.Text, expectedSubstr) {
			t.Errorf("expected %q in quote slot text, got: %s", expectedSubstr, res.Text)
		}
	}

	// Verify checkpoint was written at turn 10
	varRes, err := lat.VarGet(ctx, icx.VarGetRequest{
		Key:     "checkpoint.turn_10",
		SpaceID: spaceID,
	})
	if err != nil {
		t.Fatalf("VarGet for checkpoint.turn_10 failed: %v", err)
	}
	if !varRes.OK || varRes.Value == nil {
		t.Fatalf("expected checkpoint.turn_10 to exist in ICX swarm variables: %+v", varRes)
	}
}

// TestBoundedExecution_HaltsAtMaxTurns verifies that when Unbounded is false,
// the engine strictly halts at MaxTurns if gate approval is not granted.
func TestBoundedExecution_HaltsAtMaxTurns(t *testing.T) {
	ws := setupTestRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpModify,
			TargetFiles: []string{"step.go"},
			Rationale:   "Attempting bounded step",
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "step.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nconst Bounded = true\n",
				},
			},
		}, nil
	}

	// Always reject
	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		return &llm.FinalizeDecision{
			Approved: false,
			Reason:   "rejecting for test",
		}, nil
	}

	_ = os.WriteFile(filepath.Join(ws, "step.go"), []byte("package testpkg\n\nconst Bounded = false\n"), 0644)
	runCmd(t, ws, "git", "add", "step.go")
	runCmd(t, ws, "git", "commit", "-m", "chore: init step.go")

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: ws,
		MaxTurns:     4,
		Unbounded:    false,
		ICXClient:    lat,
		DecisionGate: mockGate,
		TestCommand:  "go test ./...",
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Bounded task that should halt at 4 turns",
		WorkspaceDir: ws,
		MaxTurns:     4,
		Unbounded:    false,
		TestCommand:  "go test ./...",
	})
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	if report.Success {
		t.Fatalf("expected report.Success to be false due to rejection")
	}

	if report.TurnsCount != 4 {
		t.Fatalf("expected bounded turns to stop at 4, got %d", report.TurnsCount)
	}
}
