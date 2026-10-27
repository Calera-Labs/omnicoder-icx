// SPDX-License-Identifier: Apache-2.0
package icx_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestActuationReceipt_TokenBudgetUnder170 validates that ActuationReceipt.FormatCompact()
// strictly adheres to the < 170 token budget across standard, pathological, and adversarial inputs.
func TestActuationReceipt_TokenBudgetUnder170(t *testing.T) {
	cases := []struct {
		name    string
		receipt icx.ActuationReceipt
	}{
		{
			name: "Standard successful patch",
			receipt: icx.ActuationReceipt{
				FileModified:      "pkg/engine/turn.go",
				ExitCode:          0,
				DiagnosticMessage: "Applied 2 edit blocks (+15 -3)",
				RepairDiffSummary: "Fixed undefined symbol and imported canonical",
				SHA256Seal:        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				TurnNumber:        1,
				Surface:           "engine_stage5_patch",
				Action:            "fuzzy_patch",
			},
		},
		{
			name: "Standard compiler heal",
			receipt: icx.ActuationReceipt{
				FileModified:      "pkg/calc/calc.go",
				ExitCode:          0,
				DiagnosticMessage: "Healed 1 diagnostic faults in 1 attempts",
				RepairDiffSummary: "calc.go: replaced undefined Symbol with 42",
				SHA256Seal:        "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
				TurnNumber:        2,
				Surface:           "engine_stage6_heal",
				Action:            "compiler_self_healing",
			},
		},
		{
			name: "Pathological 10k character diagnostic message",
			receipt: icx.ActuationReceipt{
				FileModified:      "a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/very_long_file_name_exceeding_budget.go",
				ExitCode:          1,
				DiagnosticMessage: strings.Repeat("FATAL_ERROR: type mismatch in line 42; unexpected token; ", 300),
				RepairDiffSummary: strings.Repeat("diff --git a/file b/file\n--- a/file\n+++ b/file\n@@ -1,5 +1,5 @@\n", 50),
				SHA256Seal:        strings.Repeat("deadbeef", 8),
				TurnNumber:        999,
				Surface:           "engine_stage6_heal",
				Action:            "compiler_self_healing",
			},
		},
		{
			name: "Adversarial single-character word storm (max word density)",
			receipt: icx.ActuationReceipt{
				FileModified:      strings.Repeat("f ", 50),
				ExitCode:          -1,
				DiagnosticMessage: strings.Repeat("a b c d e f g h i j k l m n o p q r s t u v w x y z ", 20),
				RepairDiffSummary: strings.Repeat("1 2 3 4 5 6 7 8 9 0 ! @ # $ % ^ & * ( ) _ + - = ", 20),
				SHA256Seal:        canonical.SHA256Of("adversarial"),
				TurnNumber:        42,
			},
		},
		{
			name: "Unicode CJK and Emoji rich diagnostics",
			receipt: icx.ActuationReceipt{
				FileModified:      "src/日本語/🚀_service_测试.go",
				ExitCode:          2,
				DiagnosticMessage: strings.Repeat("错误: 找不到符号 🚨 Crash! ", 50),
				RepairDiffSummary: strings.Repeat("修復: 新規関数追加 ✨ ", 50),
				SHA256Seal:        canonical.SHA256Of("emoji_cjk"),
				TurnNumber:        5,
			},
		},
		{
			name: "Empty / zero-value receipt",
			receipt: icx.ActuationReceipt{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compact := tc.receipt.FormatCompact()
			tokens := llm.EstimateTokens(compact)
			t.Logf("[%s] Length: %d chars, Estimated tokens: %d | Output: %s",
				tc.name, len(compact), tokens, compact)

			if tokens >= 170 {
				t.Fatalf("ActuationReceipt.FormatCompact() exceeded 170 token budget: got %d tokens (chars: %d, text: %q)",
					tokens, len(compact), compact)
			}

			// Must adhere to syntax <actuation:receipt ... />
			if !strings.HasPrefix(compact, "<actuation:receipt") || !strings.HasSuffix(compact, "/>") {
				t.Fatalf("FormatCompact syntax error: %q", compact)
			}
		})
	}
}

// TestActuationReceipt_PayloadRoundTrip verifies serialization fidelity.
func TestActuationReceipt_PayloadRoundTrip(t *testing.T) {
	orig := &icx.ActuationReceipt{
		FileModified:      "pkg/calc/calc.go",
		ExitCode:          0,
		DiagnosticMessage: "Clean compilation verified",
		RepairDiffSummary: "diff summary of changes",
		SHA256Seal:        "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		TurnNumber:        3,
		Surface:           "engine_stage6_heal",
		Action:            "compiler_self_healing",
		Timestamp:         1234567890,
	}

	payload := orig.ToPayload()
	if payload["file_modified"] != orig.FileModified {
		t.Fatalf("Payload file_modified mismatch: got %v want %v", payload["file_modified"], orig.FileModified)
	}

	reconstructed, err := icx.ActuationReceiptFromPayload(payload)
	if err != nil {
		t.Fatalf("ActuationReceiptFromPayload error: %v", err)
	}

	if reconstructed.FileModified != orig.FileModified ||
		reconstructed.ExitCode != orig.ExitCode ||
		reconstructed.DiagnosticMessage != orig.DiagnosticMessage ||
		reconstructed.RepairDiffSummary != orig.RepairDiffSummary ||
		reconstructed.SHA256Seal != orig.SHA256Seal ||
		reconstructed.TurnNumber != orig.TurnNumber ||
		reconstructed.Surface != orig.Surface ||
		reconstructed.Action != orig.Action {
		t.Fatalf("Roundtrip mismatch: got %+v, want %+v", reconstructed, orig)
	}
}

// TestDispatchActuation_CrystallizationAndSessionPersistence verifies:
// 1. DispatchActuation crystallizes facts under family "actuation_receipts".
// 2. 4D memory contains the actuation node.
// 3. lat.GetActuationReceipts retrieves crystallized receipts.
// 4. ResetSession DOES NOT prune actuation_receipts facts.
// 5. QuoteSlot succeeds on actuation_receipts slot indices.
func TestDispatchActuation_CrystallizationAndSessionPersistence(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	receipt1 := icx.ActuationReceipt{
		FileModified:      "pkg/engine/turn.go",
		ExitCode:          0,
		DiagnosticMessage: "Applied 1 edit block (+5 -1)",
		RepairDiffSummary: "added stage 5 receipt recording",
		SHA256Seal:        canonical.SHA256Of("receipt1"),
		TurnNumber:        1,
		Surface:           "engine_stage5_patch",
		Action:            "fuzzy_patch",
	}

	receipt2 := icx.ActuationReceipt{
		FileModified:      "pkg/calc/calc.go",
		ExitCode:          0,
		DiagnosticMessage: "Clean compilation verified",
		RepairDiffSummary: "calc.go: fixed undefined symbol",
		SHA256Seal:        canonical.SHA256Of("receipt2"),
		TurnNumber:        1,
		Surface:           "engine_stage6_heal",
		Action:            "compiler_self_healing",
	}

	// 1. Dispatch receipt 1
	res1, err := lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
		Surface: receipt1.Surface,
		Action:  receipt1.Action,
		Payload: receipt1.ToPayload(),
	})
	if err != nil {
		t.Fatalf("DispatchActuation 1 failed: %v", err)
	}
	if !res1.Success || res1.Status != "ACTUATED" {
		t.Fatalf("DispatchActuation 1 unexpected response: %+v", res1)
	}
	if res1.Receipt != receipt1.SHA256Seal {
		t.Fatalf("DispatchActuation 1 receipt seal mismatch: got %q, want %q", res1.Receipt, receipt1.SHA256Seal)
	}

	// 2. Dispatch receipt 2
	res2, err := lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
		Surface: receipt2.Surface,
		Action:  receipt2.Action,
		Payload: receipt2.ToPayload(),
	})
	if err != nil {
		t.Fatalf("DispatchActuation 2 failed: %v", err)
	}
	if !res2.Success || res2.Receipt != receipt2.SHA256Seal {
		t.Fatalf("DispatchActuation 2 unexpected response: %+v", res2)
	}

	// 3. Verify GetActuationReceipts returns both
	allReceipts := lat.GetActuationReceipts("")
	if len(allReceipts) != 2 {
		t.Fatalf("GetActuationReceipts expected 2 receipts, got %d", len(allReceipts))
	}
	if allReceipts[0].FileModified != receipt1.FileModified {
		t.Errorf("Receipt 0 mismatch: got %v", allReceipts[0])
	}
	if allReceipts[1].FileModified != receipt2.FileModified {
		t.Errorf("Receipt 1 mismatch: got %v", allReceipts[1])
	}

	// 4. Verify RecallScoped discovers actuation_receipts family
	recallRes, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Family: "actuation_receipts",
		TopK:   10,
	})
	if err != nil {
		t.Fatalf("RecallScoped failed: %v", err)
	}
	if recallRes.Count != 2 {
		t.Fatalf("RecallScoped expected count 2, got %d", recallRes.Count)
	}

	// 5. Verify QuoteSlot returns byte-exact compact representation
	quoteRes, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil {
		t.Fatalf("QuoteSlot failed: %v", err)
	}
	if !quoteRes.OK || !quoteRes.Verified {
		t.Fatalf("QuoteSlot unverified: %+v", quoteRes)
	}
	if quoteRes.Text != receipt1.FormatCompact() {
		t.Fatalf("QuoteSlot text mismatch:\ngot  %q\nwant %q", quoteRes.Text, receipt1.FormatCompact())
	}

	// 6. Also add a dialogue_turn fact to prove ResetSession purges dialogue_turn while keeping actuation_receipts
	_, err = lat.Remember(ctx, icx.RememberRequest{
		Text:     "User prompt: build me a website",
		Family:   "dialogue_turn",
		SourceID: "chat_turn_1",
	})
	if err != nil {
		t.Fatalf("Remember dialogue_turn failed: %v", err)
	}

	// Reset Session (Stage 7 zero-token reset)
	resetRes, err := lat.ResetSession(ctx, "")
	if err != nil {
		t.Fatalf("ResetSession failed: %v", err)
	}
	if resetRes.TokensPreserved != 0 {
		t.Errorf("Expected 0 tokens preserved, got %d", resetRes.TokensPreserved)
	}

	// 7. Verify dialogue_turn is pruned
	dialogueRecall, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Family: "dialogue_turn",
		TopK:   10,
	})
	if err != nil {
		t.Fatalf("RecallScoped dialogue_turn failed: %v", err)
	}
	if dialogueRecall.Count != 0 {
		t.Fatalf("Expected dialogue_turn to be pruned, got %d matches", dialogueRecall.Count)
	}

	// 8. Verify actuation_receipts SURVIVED 100%
	survivedReceipts := lat.GetActuationReceipts("")
	if len(survivedReceipts) != 2 {
		t.Fatalf("CRITICAL: ResetSession deleted actuation receipts! Expected 2, got %d", len(survivedReceipts))
	}
	if survivedReceipts[0].SHA256Seal != receipt1.SHA256Seal || survivedReceipts[1].SHA256Seal != receipt2.SHA256Seal {
		t.Fatalf("Actuation receipts corrupted after ResetSession")
	}

	// Verify QuoteSlot still succeeds after reset
	quoteAfterReset, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil || !quoteAfterReset.Verified {
		t.Fatalf("QuoteSlot after ResetSession failed: %v, %+v", err, quoteAfterReset)
	}
}

// TestCallTool_DispatchActuation verifies MCP tool routing for icx_dispatch_actuation.
func TestCallTool_DispatchActuation(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	receipt := icx.ActuationReceipt{
		FileModified:      "main.go",
		ExitCode:          0,
		DiagnosticMessage: "Compiled cleanly",
		RepairDiffSummary: "no repairs necessary",
		SHA256Seal:        canonical.SHA256Of("tool_test"),
		TurnNumber:        1,
	}

	args := map[string]any{
		"surface": "tool_surface",
		"action":  "test_action",
		"payload": receipt.ToPayload(),
	}

	res, err := lat.CallTool(ctx, "icx_dispatch_actuation", args)
	if err != nil {
		t.Fatalf("CallTool icx_dispatch_actuation failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned error result: %+v", res)
	}

	receipts := lat.GetActuationReceipts("")
	if len(receipts) != 1 {
		t.Fatalf("Expected 1 receipt via CallTool, got %d", len(receipts))
	}
	if receipts[0].FileModified != "main.go" {
		t.Fatalf("Receipt file modified mismatch: %s", receipts[0].FileModified)
	}
}

// TestConcurrentDispatchActuation_RaceFree tests high-concurrency access to DispatchActuation
// and GetActuationReceipts to guarantee zero data races.
func TestConcurrentDispatchActuation_RaceFree(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const concurrency = 20
	const opsPerWorker = 30

	var wg sync.WaitGroup
	wg.Add(concurrency)

	for worker := 0; worker < concurrency; worker++ {
		w := worker
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				r := icx.ActuationReceipt{
					FileModified:      fmt.Sprintf("file_%d_%d.go", w, i),
					ExitCode:          i % 2,
					DiagnosticMessage: fmt.Sprintf("Diagnostic from worker %d op %d", w, i),
					RepairDiffSummary: fmt.Sprintf("Diff summary %d", i),
					SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("%d-%d", w, i)),
					TurnNumber:        i,
					Surface:           "worker_surface",
					Action:            "concurrent_test",
				}
				_, err := lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
					Surface: r.Surface,
					Action:  r.Action,
					Payload: r.ToPayload(),
				})
				if err != nil {
					t.Errorf("Worker %d op %d failed: %v", w, i, err)
				}

				// Concurrent read
				_ = lat.GetActuationReceipts("")

				// Occasional reset on alternating workers
				if w%5 == 0 && i%10 == 0 {
					_, _ = lat.ResetSession(ctx, "")
				}
			}
		}()
	}

	wg.Wait()

	total := len(lat.GetActuationReceipts(""))
	expected := concurrency * opsPerWorker
	if total != expected {
		t.Fatalf("Expected %d receipts, got %d", expected, total)
	}
}
