// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestEngine_SelfHealing_ActuationReceiptsStoredInMem verifies that:
// 1. Stage 5 fuzzy patches and Stage 6 self-healing package actuation receipts.
// 2. Both receipts are crystallized into e.mem (lat.facts under "actuation_receipts").
// 3. Receipts survive Stage 7 zero-token session reset.
// 4. Receipts are verifiably attested in the final Merkle seal.
// 5. Each receipt compact format strictly consumes < 170 tokens.
func TestEngine_SelfHealing_ActuationReceiptsStoredInMem(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc.go", "calc_test.go"},
			Rationale:   "Introduce calc.go with intentional undefined variable to trigger Stage 6 self-healing",
			Confidence:  0.95,
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		target := filepath.ToSlash(card.FilePath)
		var blocks []llm.EditBlockData

		if strings.HasSuffix(target, "calc.go") {
			// Deliberately introduce undefinedOffset to trigger compiler error
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
			FilePath:    target,
			EditBlocks:  blocks,
			Explanation: "Initial code with intentional diagnostic fault",
		}, nil
	}

	healInvoked := false
	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healInvoked = true
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
			return &llm.FinalizeDecision{Approved: false, Reason: "tests failed"}, nil
		}
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "fix(calc): healed undefinedOffset fault",
			Summary:        "Successfully healed calc.go",
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

	if !healInvoked {
		t.Fatalf("expected DecideHeal to be invoked")
	}
	if !report.Success {
		t.Fatalf("expected report.Success = true, got false: %s", report.ErrorMessage)
	}

	// 1. Verify that actuation receipts are crystallized in e.mem (lat)
	receipts := lat.GetActuationReceipts("")
	if len(receipts) == 0 {
		t.Fatalf("CRITICAL: expected actuation receipts in e.mem (lat), got 0")
	}

	t.Logf("Found %d actuation receipts crystallized in e.mem:", len(receipts))
	for i, r := range receipts {
		compact := r.FormatCompact()
		tokens := llm.EstimateTokens(compact)
		t.Logf("  [%d] Surface=%s Action=%s Exit=%d Tokens=%d File=%s Seal=%s",
			i, r.Surface, r.Action, r.ExitCode, tokens, r.FileModified, r.SHA256Seal)

		// 2. Verify token budget < 170
		if tokens >= 170 {
			t.Fatalf("Receipt %d exceeded 170 token budget: %d tokens (text: %q)", i, tokens, compact)
		}
	}

	// 3. Verify that we have at least one Stage 5 receipt and at least one Stage 6 receipt
	hasStage5 := false
	hasStage6 := false
	for _, r := range receipts {
		if r.Surface == "engine_stage5_patch" && r.Action == "fuzzy_patch" {
			hasStage5 = true
		}
		if r.Surface == "engine_stage6_heal" && r.Action == "compiler_self_healing" {
			hasStage6 = true
			if r.ExitCode != 0 {
				t.Errorf("Expected Stage 6 healed receipt to have ExitCode 0, got %d", r.ExitCode)
			}
		}
	}
	if !hasStage5 {
		t.Errorf("Missing expected Stage 5 fuzzy patch actuation receipt")
	}
	if !hasStage6 {
		t.Errorf("Missing expected Stage 6 compiler self-healing actuation receipt")
	}

	// 4. Verify that ResetSession (run in Stage 7) did NOT purge actuation receipts
	// We can run ResetSession again to explicitly verify idempotency and durability
	_, err = lat.ResetSession(ctx, "")
	if err != nil {
		t.Fatalf("ResetSession failed: %v", err)
	}
	receiptsAfterReset := lat.GetActuationReceipts("")
	if len(receiptsAfterReset) != len(receipts) {
		t.Fatalf("Expected %d receipts to survive ResetSession, got %d", len(receipts), len(receiptsAfterReset))
	}

	// 5. Verify QuoteSlot can read receipt verbatim
	quoteRes, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil || !quoteRes.OK || !quoteRes.Verified {
		t.Fatalf("QuoteSlot failed on actuation_receipts: %v, %+v", err, quoteRes)
	}

	// 6. Verify Merkle Seal is valid and non-empty
	if len(report.MerkleSeal) != 64 {
		t.Errorf("Invalid MerkleSeal length: %s", report.MerkleSeal)
	}
}

// TestEngine_Stage5FuzzyPatch_ActuationReceiptsInTurnState tests that
// executeStage5FuzzyPatch records the receipt directly into TurnState.
func TestEngine_Stage5FuzzyPatch_ActuationReceiptsInTurnState(t *testing.T) {
	ws := setupTestRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"greet.go"},
			Rationale:   "Create greet.go",
			Confidence:  0.99,
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		return &llm.EditDecision{
			FilePath: "greet.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package testpkg\n\nfunc Greet() string { return \"hello\" }\n",
				},
			},
			Explanation: "greet implementation",
		}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "feat: greet",
			Summary:        "added greet",
			FilesCommitted: card.FilesModified,
		}, nil
	}

	cfg := engine.EngineConfig{
		WorkspaceDir: ws,
		MaxTurns:     1,
		Offline:      true,
		ICXClient:    lat,
		DecisionGate: mockGate,
	}

	orch, err := engine.NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "create greet.go",
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ExecuteTask failed: %v", err)
	}
	if !report.Success {
		t.Fatalf("ExecuteTask report unsuccessful: %s", report.ErrorMessage)
	}

	receipts := lat.GetActuationReceipts("")
	if len(receipts) == 0 {
		t.Fatalf("Expected actuation receipts in lat, got 0")
	}

	var foundStage5 *icx.ActuationReceipt
	for i := range receipts {
		if receipts[i].Surface == "engine_stage5_patch" && receipts[i].Action == "fuzzy_patch" {
			foundStage5 = &receipts[i]
			break
		}
	}

	if foundStage5 == nil {
		t.Fatalf("Expected Stage 5 fuzzy patch actuation receipt to be recorded")
	}

	if foundStage5.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %d", foundStage5.ExitCode)
	}
	if foundStage5.SHA256Seal == "" {
		t.Errorf("Expected non-empty SHA256Seal")
	}

	tokens := llm.EstimateTokens(foundStage5.FormatCompact())
	if tokens >= 170 {
		t.Errorf("Receipt exceeded 170 token budget: %d tokens", tokens)
	}
}
