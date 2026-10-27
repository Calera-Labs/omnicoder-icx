// SPDX-License-Identifier: Apache-2.0
package icx_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestActuationReceipts_ConcurrentStress30Plus stress-tests
// DispatchActuation, GetActuationReceipts, RecallScoped (ribbon format),
// and concurrent ResetSession across 36 concurrent goroutines.
func TestActuationReceipts_ConcurrentStress30Plus(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const (
		numDispatchers = 10
		numReaders     = 10
		numRecalls     = 10
		numResetters   = 6
		opsPerWorker   = 100
	)
	totalGoroutines := numDispatchers + numReaders + numRecalls + numResetters

	var wg sync.WaitGroup
	wg.Add(totalGoroutines)

	var dispatchedCount atomic.Int64
	var totalResets atomic.Int64

	// 1. Dispatcher goroutines
	for d := 0; d < numDispatchers; d++ {
		workerID := d
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				seal := canonical.SHA256Of(fmt.Sprintf("dispatcher-%d-op-%d", workerID, i))
				receipt := icx.ActuationReceipt{
					FileModified:      fmt.Sprintf("pkg/mod_%d/file_%d.go", workerID, i),
					ExitCode:          (workerID + i) % 3,
					DiagnosticMessage: fmt.Sprintf("Diagnostic fault from dispatcher %d iteration %d", workerID, i),
					RepairDiffSummary: fmt.Sprintf("Applied repair block for dispatcher %d iteration %d", workerID, i),
					SHA256Seal:        seal,
					TurnNumber:        i + 1,
					Surface:           "merkle_proof_test",
					Action:            "concurrent_dispatch",
				}

				res, err := lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
					Surface: receipt.Surface,
					Action:  receipt.Action,
					Payload: receipt.ToPayload(),
				})
				if err != nil {
					t.Errorf("DispatchActuation failed: %v", err)
					return
				}
				if !res.Success || res.Receipt != seal {
					t.Errorf("Unexpected dispatch response: %+v", res)
					return
				}
				dispatchedCount.Add(1)
			}
		}()
	}

	// 2. Continuous GetActuationReceipts readers
	for r := 0; r < numReaders; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				receipts := lat.GetActuationReceipts("")
				// Receipts must be valid and non-corrupted
				for idx := range receipts {
					if receipts[idx].SHA256Seal == "" {
						t.Errorf("Found receipt with empty SHA256Seal at index %d", idx)
						return
					}
				}
			}
		}()
	}

	// 3. Continuous RecallScoped workers (including ribbon formatting)
	for rc := 0; rc < numRecalls; rc++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				format := ""
				if i%2 == 0 {
					format = "ribbon"
				}
				recallRes, err := lat.RecallScoped(ctx, icx.RecallRequest{
					Query:  "dispatcher",
					Family: "actuation_receipts",
					Format: format,
					TopK:   10,
				})
				if err != nil {
					t.Errorf("RecallScoped failed: %v", err)
					return
				}
				if format == "ribbon" && recallRes.Count > 0 {
					if !strings.HasPrefix(recallRes.Ribbon, "<nav:ribbon:") {
						t.Errorf("Invalid ribbon format: %s", recallRes.Ribbon)
						return
					}
				}
			}
		}()
	}

	// 4. Concurrent ResetSession and Remember workers
	for rs := 0; rs < numResetters; rs++ {
		workerID := rs
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				// Remember a dialogue turn
				_, err := lat.Remember(ctx, icx.RememberRequest{
					Text:     fmt.Sprintf("Dialogue turn from resetter %d op %d", workerID, i),
					Family:   "dialogue_turn",
					SourceID: fmt.Sprintf("resetter_%d", workerID),
				})
				if err != nil {
					t.Errorf("Remember dialogue_turn failed: %v", err)
					return
				}

				// Trigger ResetSession
				if i%5 == 0 {
					resetRes, err := lat.ResetSession(ctx, "")
					if err != nil {
						t.Errorf("ResetSession failed: %v", err)
						return
					}
					if resetRes.TokensPreserved != 0 {
						t.Errorf("Expected 0 tokens preserved, got %d", resetRes.TokensPreserved)
						return
					}
					totalResets.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	t.Logf("Stress test finished: %d dispatches, %d resets across %d goroutines",
		dispatchedCount.Load(), totalResets.Load(), totalGoroutines)

	expectedDispatches := int64(numDispatchers * opsPerWorker)
	if dispatchedCount.Load() != expectedDispatches {
		t.Fatalf("Dispatched count mismatch: got %d, want %d", dispatchedCount.Load(), expectedDispatches)
	}

	// Final verification of receipts in lattice
	allReceipts := lat.GetActuationReceipts("")
	if int64(len(allReceipts)) != expectedDispatches {
		t.Fatalf("Lattice actuation receipts count mismatch: got %d, want %d", len(allReceipts), expectedDispatches)
	}

	// Run another ResetSession to verify total durability
	_, err := lat.ResetSession(ctx, "")
	if err != nil {
		t.Fatalf("Final ResetSession failed: %v", err)
	}

	afterResetReceipts := lat.GetActuationReceipts("")
	if int64(len(afterResetReceipts)) != expectedDispatches {
		t.Fatalf("Actuation receipts were lost after final ResetSession! got %d, want %d",
			len(afterResetReceipts), expectedDispatches)
	}
}

// TestActuationReceipts_SessionResetImmunity100Percent verifies that
// ResetSession preserves 100% of facts in the "actuation_receipts" family while
// correctly purging all "dialogue_turn" facts.
func TestActuationReceipts_SessionResetImmunity100Percent(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const numReceipts = 100
	const numDialogueTurns = 50

	receipts := make([]icx.ActuationReceipt, numReceipts)
	for i := 0; i < numReceipts; i++ {
		receipts[i] = icx.ActuationReceipt{
			FileModified:      fmt.Sprintf("internal/pkg_%03d/file_%03d.go", i, i),
			ExitCode:          i % 2,
			DiagnosticMessage: fmt.Sprintf("Diagnostic message for item %d", i),
			RepairDiffSummary: fmt.Sprintf("Repair diff summary for item %d", i),
			SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("immunity_receipt_%d", i)),
			TurnNumber:        i + 1,
			Surface:           "engine_stage5_patch",
			Action:            "fuzzy_patch",
		}

		res, err := lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
			Surface: receipts[i].Surface,
			Action:  receipts[i].Action,
			Payload: receipts[i].ToPayload(),
		})
		if err != nil {
			t.Fatalf("DispatchActuation %d failed: %v", i, err)
		}
		if res.Receipt != receipts[i].SHA256Seal {
			t.Fatalf("Seal mismatch on receipt %d: got %s, want %s", i, res.Receipt, receipts[i].SHA256Seal)
		}
	}

	// Insert dialogue turn facts
	for i := 0; i < numDialogueTurns; i++ {
		_, err := lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("User conversation prompt turn %d", i),
			Family:   "dialogue_turn",
			SourceID: fmt.Sprintf("chat_turn_%d", i),
		})
		if err != nil {
			t.Fatalf("Remember dialogue turn %d failed: %v", i, err)
		}
	}

	// Verify pre-reset counts
	preDialogue, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Family: "dialogue_turn",
		TopK:   10,
	})
	if err != nil {
		t.Fatalf("Pre-reset recall dialogue_turn failed: %v", err)
	}
	if preDialogue.Count == 0 {
		t.Fatalf("Expected dialogue_turn facts before reset")
	}

	preReceipts := lat.GetActuationReceipts("")
	if len(preReceipts) != numReceipts {
		t.Fatalf("Pre-reset actuation receipts count mismatch: got %d, want %d", len(preReceipts), numReceipts)
	}

	// Execute 10 consecutive ResetSession calls (idempotence & immunity test)
	for resetIter := 1; resetIter <= 10; resetIter++ {
		resetRes, err := lat.ResetSession(ctx, "")
		if err != nil {
			t.Fatalf("ResetSession iter %d failed: %v", resetIter, err)
		}
		if resetRes.TokensPreserved != 0 {
			t.Fatalf("ResetSession iter %d: expected 0 tokens preserved, got %d", resetIter, resetRes.TokensPreserved)
		}

		// Verify dialogue_turn is purged
		postDialogue, err := lat.RecallScoped(ctx, icx.RecallRequest{
			Family: "dialogue_turn",
			TopK:   10,
		})
		if err != nil {
			t.Fatalf("Post-reset recall dialogue_turn failed: %v", err)
		}
		if postDialogue.Count != 0 {
			t.Fatalf("ResetSession iter %d failed to purge dialogue_turn: got count %d", resetIter, postDialogue.Count)
		}

		// Verify 100% of actuation receipts survived
		survivedReceipts := lat.GetActuationReceipts("")
		if len(survivedReceipts) != numReceipts {
			t.Fatalf("ResetSession iter %d deleted actuation receipts: got %d, want %d",
				resetIter, len(survivedReceipts), numReceipts)
		}

		// Verify every single receipt matches exactly
		for i := 0; i < numReceipts; i++ {
			if survivedReceipts[i].FileModified != receipts[i].FileModified ||
				survivedReceipts[i].ExitCode != receipts[i].ExitCode ||
				survivedReceipts[i].SHA256Seal != receipts[i].SHA256Seal ||
				survivedReceipts[i].DiagnosticMessage != receipts[i].DiagnosticMessage ||
				survivedReceipts[i].RepairDiffSummary != receipts[i].RepairDiffSummary {
				t.Fatalf("ResetSession iter %d corrupted receipt %d: got %+v, want %+v",
					resetIter, i, survivedReceipts[i], receipts[i])
			}

			// Verify QuoteSlot on each slot
			quoteRes, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
				Family: "actuation_receipts",
				Index:  i + 1,
			})
			if err != nil || !quoteRes.OK || !quoteRes.Verified {
				t.Fatalf("QuoteSlot failed for slot %d after reset iter %d: err=%v, quoteRes=%+v",
					i+1, resetIter, err, quoteRes)
			}
			if quoteRes.Text != receipts[i].FormatCompact() {
				t.Fatalf("QuoteSlot text mismatch for slot %d:\ngot  %q\nwant %q",
					i+1, quoteRes.Text, receipts[i].FormatCompact())
			}
		}
	}
}

// TestActuationReceipts_AdversarialBudgetFloor verifies that
// under extreme string conditions, token estimates never hit or exceed 170.
func TestActuationReceipts_AdversarialBudgetFloor(t *testing.T) {
	// Construct extreme receipts
	adversarialCases := []struct {
		name    string
		receipt icx.ActuationReceipt
	}{
		{
			name: "Max repetition CJK + ASCII symbols",
			receipt: icx.ActuationReceipt{
				FileModified:      strings.Repeat("测/试/路/径/", 20),
				ExitCode:          -999,
				DiagnosticMessage: strings.Repeat("错误! 🚨 💥 🛑 ", 50),
				RepairDiffSummary: strings.Repeat("++--@@##$$%%^^&&**() ", 50),
				SHA256Seal:        strings.Repeat("f", 64),
			},
		},
		{
			name: "50,000 character pure space and newline text",
			receipt: icx.ActuationReceipt{
				FileModified:      "file.go",
				ExitCode:          1,
				DiagnosticMessage: strings.Repeat("   \n\t   \r\n   ", 2000),
				RepairDiffSummary: strings.Repeat("diff ", 5000),
				SHA256Seal:        "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			},
		},
		{
			name: "Single rune repeated 10,000 times without spaces",
			receipt: icx.ActuationReceipt{
				FileModified:      strings.Repeat("A", 10000),
				ExitCode:          0,
				DiagnosticMessage: strings.Repeat("Z", 10000),
				RepairDiffSummary: strings.Repeat("Q", 10000),
				SHA256Seal:        canonical.SHA256Of("single_rune_repeater"),
			},
		},
	}

	for _, tc := range adversarialCases {
		t.Run(tc.name, func(t *testing.T) {
			compact := tc.receipt.FormatCompact()
			tokens := llm.EstimateTokens(compact)
			if tokens >= 170 {
				t.Fatalf("[%s] Token budget violation: estimated %d tokens (text: %q)", tc.name, tokens, compact)
			}
			if !strings.HasPrefix(compact, "<actuation:receipt") || !strings.HasSuffix(compact, "/>") {
				t.Fatalf("[%s] Invalid syntax: %s", tc.name, compact)
			}
		})
	}
}
