// SPDX-License-Identifier: Apache-2.0
// Package icx implements empirical verification for actuation receipts and session durability:
// 1. ActuationReceipt.FormatCompact() token budget strictly < 170 tokens across all pathological inputs.
// 2. QuoteSlot byte-exact cryptographic verification on crystallized actuation receipts.
// 3. Multi-turn ResetSession durability and tamper detection.
// 4. Concurrency stress testing under race detector.

package icx

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// PART 1: FormatCompact() Token Ceiling Under Pathological Inputs (< 170 tokens)
// ============================================================================

func TestFormatCompact_PathologicalTokenStress(t *testing.T) {
	// Build pathological test cases designed to maximize token count and trigger edge cases
	cases := []struct {
		name    string
		receipt *ActuationReceipt
	}{
		{
			name:    "Nil receipt pointer",
			receipt: nil,
		},
		{
			name:    "Completely empty receipt",
			receipt: &ActuationReceipt{},
		},
		{
			name: "100k character single unbroken word in diagnostic",
			receipt: &ActuationReceipt{
				FileModified:      "test.go",
				ExitCode:          1,
				DiagnosticMessage: strings.Repeat("A", 100000),
				RepairDiffSummary: "diff",
				SHA256Seal:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
		{
			name: "100k character word storm (50,000 single character words)",
			receipt: &ActuationReceipt{
				FileModified:      "pkg/a/b/c.go",
				ExitCode:          2,
				DiagnosticMessage: strings.Repeat("x y z 1 2 3 ", 10000),
				RepairDiffSummary: strings.Repeat("a b c d e f ", 10000),
				SHA256Seal:        canonical.SHA256Of("word_storm"),
			},
		},
		{
			name: "100k character diff summary with patch syntax",
			receipt: &ActuationReceipt{
				FileModified:      "pkg/engine/turn.go",
				ExitCode:          0,
				DiagnosticMessage: "Build successful",
				RepairDiffSummary: strings.Repeat("diff --git a/turn.go b/turn.go\n--- a/turn.go\n+++ b/turn.go\n@@ -10,5 +10,12 @@\n+func Added() {}\n-func Removed() {}\n", 1500),
				SHA256Seal:        canonical.SHA256Of("huge_diff"),
			},
		},
		{
			name: "Pathologically long file path (50,000 characters)",
			receipt: &ActuationReceipt{
				FileModified:      strings.Repeat("a/very/deep/and/pathological/directory/structure/", 1000) + "file.go",
				ExitCode:          0,
				DiagnosticMessage: "Fixed import",
				RepairDiffSummary: "Clean patch applied",
				SHA256Seal:        canonical.SHA256Of("long_path"),
			},
		},
		{
			name: "Binary control characters (all 0x00 to 0x1F and 0x7F to 0xFF)",
			receipt: &ActuationReceipt{
				FileModified:      string([]byte{0x00, 0x01, 0x02, 0x03, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x1b, 0x1f, 0x7f, 0x80, 0xff}),
				ExitCode:          -1,
				DiagnosticMessage: strings.Repeat(string([]byte{0x00, 0x01, 0x02, 0x1f, 0x7f, 0xff}), 500),
				RepairDiffSummary: strings.Repeat(string([]byte{0x00, 0x1b, '[', '3', '1', 'm', 0x00}), 500),
				SHA256Seal:        strings.Repeat(string([]byte{0x00}), 64),
			},
		},
		{
			name: "All 64 bytes of SHA256Seal as \\x00 (maximum %q escape expansion)",
			receipt: &ActuationReceipt{
				FileModified:      "main.go",
				ExitCode:          0,
				DiagnosticMessage: strings.Repeat("\x00", 200),
				RepairDiffSummary: strings.Repeat("\x00", 200),
				SHA256Seal:        strings.Repeat("\x00", 64),
			},
		},
		{
			name: "Extreme negative and positive ExitCodes and TurnNumbers",
			receipt: &ActuationReceipt{
				FileModified:      "pkg/calc/calc.go",
				ExitCode:          math.MinInt64,
				TurnNumber:        math.MaxInt64,
				DiagnosticMessage: "Diagnostic with extreme int64 exit code",
				RepairDiffSummary: "Summary with extreme int64 turn number",
				SHA256Seal:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
		},
		{
			name: "ExitCode MinInt32 and MaxInt32",
			receipt: &ActuationReceipt{
				FileModified:      "test.go",
				ExitCode:          math.MinInt32,
				DiagnosticMessage: "MinInt32 exit code",
				RepairDiffSummary: "Summary",
				SHA256Seal:        "abcdef",
			},
		},
		{
			name: "Special characters: quotes, backslashes, XML brackets, format verbs",
			receipt: &ActuationReceipt{
				FileModified:      `path/"with"/quotes/and\backslashes\file.go`,
				ExitCode:          1,
				DiagnosticMessage: `error: expected "%s", got "%d"; <tag attr="val"> &amp; 'single' \n \t \r %%`,
				RepairDiffSummary: `diff: <tag>\"escaped\"</tag> %q %v %#v \\ // /* comment */`,
				SHA256Seal:        `"quotes"\and/slashes"seal"`,
			},
		},
		{
			name: "Multilingual Unicode: CJK, Arabic, Hebrew, Russian, Devanagari",
			receipt: &ActuationReceipt{
				FileModified:      "src/日本語/العربية/עברית/русский/हिन्दी/файл.go",
				ExitCode:          0,
				DiagnosticMessage: strings.Repeat("错误: 找不到符号; خطأ في الترجمة; שגיאת קומפילציה; ошибка компиляции; संकलन त्रुटि; ", 30),
				RepairDiffSummary: strings.Repeat("修復: 新規関数追加; إصلاح الخلل; תיקון תקלה; исправлено; सुधार; ", 30),
				SHA256Seal:        canonical.SHA256Of("multilingual"),
			},
		},
		{
			name: "Emoji storms and multi-byte combining sequences",
			receipt: &ActuationReceipt{
				FileModified:      "src/🚀_service_🎉.go",
				ExitCode:          0,
				DiagnosticMessage: strings.Repeat("💥🔥⚡✨🚨🛠️🛡️🎯📦🧪 ", 50),
				RepairDiffSummary: strings.Repeat("👨‍👩‍👧‍👦 🏴󠁧󠁢󠁳󠁣󠁴󠁿 🏳️‍🌈 👁️‍🗨️ ", 50),
				SHA256Seal:        canonical.SHA256Of("emoji_storm"),
			},
		},
		{
			name: "Zalgo text (extreme combining diacritical marks)",
			receipt: &ActuationReceipt{
				FileModified:      "zalgo.go",
				ExitCode:          1,
				DiagnosticMessage: "e" + strings.Repeat("\u0300\u0301\u0302\u0303\u0304\u0305\u0306\u0307\u0308\u0309\u030a", 50),
				RepairDiffSummary: "Z" + strings.Repeat("\u0334\u0335\u0336\u0337\u0338\u0339\u033a\u033b\u033c\u033d\u033e", 50),
				SHA256Seal:        canonical.SHA256Of("zalgo"),
			},
		},
		{
			name: "All worst-case pathological combinations simultaneously",
			receipt: &ActuationReceipt{
				FileModified:      strings.Repeat("very_long_path_name/", 500) + strings.Repeat("\x00\xff", 50) + "file.go",
				ExitCode:          math.MinInt64,
				TurnNumber:        math.MaxInt64,
				DiagnosticMessage: strings.Repeat("a b c d e f \x00\x01\x1f\x7f\xff 🚨 Crash! 错误 ", 2000),
				RepairDiffSummary: strings.Repeat("1 2 3 4 5 6 + - @@ \x00\x01 ✨ Fixed! 修復 ", 2000),
				SHA256Seal:        strings.Repeat("\x00 a b c d \xff", 20),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compact := tc.receipt.FormatCompact()
			tokens := llm.EstimateTokens(compact)

			// MUST strictly adhere to < 170 tokens
			if tokens >= 170 {
				t.Fatalf("VIOLATION: FormatCompact exceeded 170 token budget!\nTokens: %d\nLength: %d bytes\nOutput: %s",
					tokens, len(compact), compact)
			}

			// Syntax validation
			if !strings.HasPrefix(compact, "<actuation:receipt") || !strings.HasSuffix(compact, "/>") {
				t.Fatalf("VIOLATION: FormatCompact invalid XML delimiter syntax: %q", compact)
			}

			// Confirm positive tokens for any valid string
			if tokens < 1 && tc.receipt != nil {
				t.Fatalf("VIOLATION: EstimateTokens returned < 1 token: %d", tokens)
			}

			t.Logf("[%s] -> %d chars, %d tokens | Compact: %.80s...", tc.name, len(compact), tokens, compact)
		})
	}
}

// TestFormatCompact_RandomFuzzingProperty executes 5,000 randomized property-based
// fuzz iterations across all possible combinations of bytes, runes, lengths, and integers.
func TestFormatCompact_RandomFuzzingProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// Helper to generate random string with diverse byte types
	randomString := func(maxLen int) string {
		n := rng.Intn(maxLen + 1)
		b := make([]byte, n)
		for i := 0; i < n; i++ {
			mode := rng.Intn(5)
			switch mode {
			case 0:
				b[i] = byte(rng.Intn(26) + 'a')
			case 1:
				b[i] = ' '
			case 2:
				b[i] = byte(rng.Intn(32)) // control char
			case 3:
				b[i] = byte(rng.Intn(128) + 128) // high byte
			case 4:
				b[i] = byte('"')
			}
		}
		return string(b)
	}

	maxTokensSeen := 0
	const iterations = 5000

	for i := 0; i < iterations; i++ {
		r := &ActuationReceipt{
			FileModified:      randomString(rng.Intn(1000)),
			ExitCode:          rng.Intn(2000000) - 1000000,
			DiagnosticMessage: randomString(rng.Intn(5000)),
			RepairDiffSummary: randomString(rng.Intn(5000)),
			SHA256Seal:        randomString(rng.Intn(200)),
			TurnNumber:        rng.Intn(10000),
		}

		compact := r.FormatCompact()
		tokens := llm.EstimateTokens(compact)
		if tokens > maxTokensSeen {
			maxTokensSeen = tokens
		}

		if tokens >= 170 {
			t.Fatalf("Fuzz iteration %d VIOLATION: tokens = %d >= 170!\nLength: %d\nCompact: %s",
				i, tokens, len(compact), compact)
		}

		if !strings.HasPrefix(compact, "<actuation:receipt") || !strings.HasSuffix(compact, "/>") {
			t.Fatalf("Fuzz iteration %d syntax failure: %s", i, compact)
		}
	}

	t.Logf("Property fuzzing completed 5,000 iterations: max tokens observed = %d (< 170)", maxTokensSeen)
}

// ============================================================================
// PART 2: Byte-Exact QuoteSlot Verification on Crystallized Actuation Receipts
// ============================================================================

// TestQuoteSlot_ByteExactVerification verifies that QuoteSlot deterministically
// verifies the byte-exact SHA-256 seal of crystallized actuation receipts.
func TestQuoteSlot_ByteExactVerification(t *testing.T) {
	ctx := context.Background()
	lat := NewLocalLattice()

	testReceipts := []*ActuationReceipt{
		{
			FileModified:      "pkg/engine/turn.go",
			ExitCode:          0,
			DiagnosticMessage: "Applied 2 fuzzy patches with 100% confidence",
			RepairDiffSummary: "Imported canonical and added Merkle leaf attestation",
			SHA256Seal:        canonical.SHA256Of("receipt_one"),
			TurnNumber:        1,
			Surface:           "engine_stage5_patch",
			Action:            "fuzzy_patch",
		},
		{
			FileModified:      "pkg/calc/calc.go",
			ExitCode:          0,
			DiagnosticMessage: "Healed 1 diagnostic fault in 1 attempt",
			RepairDiffSummary: "Replaced undefined symbol with 42",
			SHA256Seal:        canonical.SHA256Of("receipt_two"),
			TurnNumber:        2,
			Surface:           "engine_stage6_heal",
			Action:            "compiler_self_healing",
		},
		{
			FileModified:      "src/test/special_chars_\"quotes\"_\\backslashes.go",
			ExitCode:          1,
			DiagnosticMessage: "Error: <nil> pointer exception in \x00\x01 binary parser",
			RepairDiffSummary: "Added defensive bounds check for [3]byte header",
			SHA256Seal:        canonical.SHA256Of("receipt_three"),
			TurnNumber:        3,
			Surface:           "engine_stage5_patch",
			Action:            "fuzzy_patch",
		},
		{
			FileModified:      "src/日本語/🚀_service.go",
			ExitCode:          0,
			DiagnosticMessage: "修復: 新規関数追加 ✨ (Clean compile)",
			RepairDiffSummary: "Added exported Japanese telemetry function",
			SHA256Seal:        canonical.SHA256Of("receipt_four"),
			TurnNumber:        4,
			Surface:           "engine_stage6_heal",
			Action:            "compiler_self_healing",
		},
	}

	// Dispatch all receipts
	for i, r := range testReceipts {
		res, err := lat.DispatchActuation(ctx, DispatchActuationRequest{
			Surface: r.Surface,
			Action:  r.Action,
			Payload: r.ToPayload(),
		})
		if err != nil {
			t.Fatalf("DispatchActuation [%d] failed: %v", i, err)
		}
		if !res.Success || res.Status != "ACTUATED" {
			t.Fatalf("DispatchActuation [%d] unexpected response: %+v", i, res)
		}
	}

	// Verify QuoteSlot on every index (1-indexed)
	for i, r := range testReceipts {
		slotIdx := i + 1
		quoteRes, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
			Family: "actuation_receipts",
			Index:  slotIdx,
		})
		if err != nil {
			t.Fatalf("QuoteSlot index %d returned error: %v", slotIdx, err)
		}
		if !quoteRes.OK {
			t.Fatalf("QuoteSlot index %d expected OK=true, got false (error: %s)", slotIdx, quoteRes.Error)
		}
		if !quoteRes.Verified {
			t.Fatalf("CRITICAL: QuoteSlot index %d failed byte-exact cryptographic verification!", slotIdx)
		}

		expectedText := r.FormatCompact()
		if quoteRes.Text != expectedText {
			t.Fatalf("QuoteSlot index %d text mismatch:\ngot:  %q\nwant: %q", slotIdx, quoteRes.Text, expectedText)
		}

		expectedSHA := canonical.SHA256Bytes([]byte(expectedText))
		if quoteRes.SHA256 != expectedSHA {
			t.Fatalf("QuoteSlot index %d SHA256 mismatch:\ngot:  %q\nwant: %q", slotIdx, quoteRes.SHA256, expectedSHA)
		}

		// Recompute SHA-256 directly from quoteRes.Text
		recomputedSHA := canonical.SHA256Bytes([]byte(quoteRes.Text))
		if recomputedSHA != quoteRes.SHA256 {
			t.Fatalf("QuoteSlot index %d text tamper detected: recomputed SHA %s != fact SHA %s",
				slotIdx, recomputedSHA, quoteRes.SHA256)
		}

		t.Logf("QuoteSlot [%d] verified byte-exact: SHA256=%s Latency=%.2fus",
			slotIdx, quoteRes.SHA256[:16], quoteRes.LatencyUs)
	}
}

// TestQuoteSlot_TamperNegativeOracle explicitly verifies that QuoteSlot
// DETECTS tampering and returns Verified=false when a receipt fact is corrupted.
func TestQuoteSlot_TamperNegativeOracle(t *testing.T) {
	ctx := context.Background()
	lat := NewLocalLattice()

	receipt := &ActuationReceipt{
		FileModified:      "pkg/auth/token.go",
		ExitCode:          0,
		DiagnosticMessage: "Generated JWT signature",
		RepairDiffSummary: "Fixed HMAC SHA256 key padding",
		SHA256Seal:        canonical.SHA256Of("legit_receipt"),
		TurnNumber:        1,
	}

	_, err := lat.DispatchActuation(ctx, DispatchActuationRequest{
		Surface: "engine_stage5_patch",
		Action:  "fuzzy_patch",
		Payload: receipt.ToPayload(),
	})
	if err != nil {
		t.Fatalf("DispatchActuation failed: %v", err)
	}

	// 1. Initial QuoteSlot must verify cleanly
	resClean, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil || !resClean.Verified {
		t.Fatalf("Initial QuoteSlot expected Verified=true, got %v", resClean)
	}

	// 2. Tamper test: Corrupt fact text directly in lat.facts
	lat.mu.Lock()
	famKey := lat.defaultSpace + ":actuation_receipts"
	slots := lat.byFamily[famKey]
	factID := slots[0]
	origFact := lat.facts[factID]

	// Corrupt Text
	origFact.Text = origFact.Text + " [TAMPERED_INJECTED_BYTES]"
	lat.mu.Unlock()

	resTampered, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil {
		t.Fatalf("Tampered QuoteSlot error: %v", err)
	}
	if resTampered.Verified {
		t.Fatalf("CRITICAL SECURITY DEFECT: QuoteSlot reported Verified=true on tampered fact text!")
	}
	t.Logf("Tamper detection successfully confirmed: Verified=%v on tampered text", resTampered.Verified)

	// 3. Tamper test: Restore text but corrupt SHA256 seal
	lat.mu.Lock()
	origFact.Text = receipt.FormatCompact() // restored
	origFact.SHA256 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	lat.mu.Unlock()

	resBadSHA, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil {
		t.Fatalf("BadSHA QuoteSlot error: %v", err)
	}
	if resBadSHA.Verified {
		t.Fatalf("CRITICAL SECURITY DEFECT: QuoteSlot reported Verified=true on corrupted SHA256 seal!")
	}
	t.Logf("Tamper detection successfully confirmed: Verified=%v on corrupted SHA256 seal", resBadSHA.Verified)
}

// TestQuoteSlot_BoundaryConditions tests out of bounds, invalid families, and zero indices.
func TestQuoteSlot_BoundaryConditions(t *testing.T) {
	ctx := context.Background()
	lat := NewLocalLattice()

	// Empty lattice: QuoteSlot on non-existent family
	resEmpty, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
		Family: "actuation_receipts",
		Index:  1,
	})
	if err != nil {
		t.Fatalf("QuoteSlot on empty lattice returned error: %v", err)
	}
	if resEmpty.OK || resEmpty.Verified || resEmpty.Error != "SLOT_NOT_FOUND" {
		t.Fatalf("Expected SLOT_NOT_FOUND, got: %+v", resEmpty)
	}

	// Dispatch 1 receipt
	r := &ActuationReceipt{
		FileModified:      "test.go",
		ExitCode:          0,
		DiagnosticMessage: "OK",
		RepairDiffSummary: "OK",
		SHA256Seal:        canonical.SHA256Of("test"),
	}
	lat.DispatchActuation(ctx, DispatchActuationRequest{
		Surface: "stage5",
		Action:  "patch",
		Payload: r.ToPayload(),
	})

	// Test boundary indices:
	boundaryIndices := []struct {
		idx       int
		expectErr string
	}{
		{idx: 0, expectErr: "SLOT_NOT_FOUND"},
		{idx: -1, expectErr: "SLOT_NOT_FOUND"},
		{idx: 2, expectErr: "SLOT_NOT_FOUND"},
		{idx: 999999, expectErr: "SLOT_NOT_FOUND"},
		{idx: math.MinInt32, expectErr: "SLOT_NOT_FOUND"},
	}

	for _, b := range boundaryIndices {
		res, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
			Family: "actuation_receipts",
			Index:  b.idx,
		})
		if err != nil {
			t.Fatalf("QuoteSlot index %d returned unexpected error: %v", b.idx, err)
		}
		if res.OK || res.Verified || res.Error != b.expectErr {
			t.Fatalf("QuoteSlot index %d expected %s, got: %+v", b.idx, b.expectErr, res)
		}
	}
}

// ============================================================================
// PART 3: Multi-Turn ResetSession Durability & 4D Audit Node Persistence
// ============================================================================

func TestResetSession_DurabilityAndIsolation(t *testing.T) {
	ctx := context.Background()
	lat := NewLocalLattice()

	// 1. Add 5 dialogue_turn facts (which must be purged upon reset)
	for i := 1; i <= 5; i++ {
		_, err := lat.Remember(ctx, RememberRequest{
			Text:     fmt.Sprintf("User conversation prompt turn %d: please write code", i),
			Family:   "dialogue_turn",
			SourceID: fmt.Sprintf("turn_%d", i),
		})
		if err != nil {
			t.Fatalf("Remember turn %d failed: %v", i, err)
		}
	}

	// 2. Dispatch 5 actuation receipts across turns
	for i := 1; i <= 5; i++ {
		r := &ActuationReceipt{
			FileModified:      fmt.Sprintf("pkg/mod_%d/file.go", i),
			ExitCode:          0,
			DiagnosticMessage: fmt.Sprintf("Healed fault in turn %d", i),
			RepairDiffSummary: fmt.Sprintf("Applied patch for turn %d", i),
			SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("seal_turn_%d", i)),
			TurnNumber:        i,
			Surface:           "engine_stage6_heal",
			Action:            "compiler_self_healing",
		}
		res, err := lat.DispatchActuation(ctx, DispatchActuationRequest{
			Surface: r.Surface,
			Action:  r.Action,
			Payload: r.ToPayload(),
		})
		if err != nil || !res.Success {
			t.Fatalf("DispatchActuation turn %d failed: %v", i, err)
		}
	}

	// Verify before reset
	dialogueBefore, _ := lat.RecallScoped(ctx, RecallRequest{Family: "dialogue_turn"})
	if dialogueBefore.Count != 5 {
		t.Fatalf("Expected 5 dialogue facts before reset, got %d", dialogueBefore.Count)
	}
	receiptsBefore := lat.GetActuationReceipts("")
	if len(receiptsBefore) != 5 {
		t.Fatalf("Expected 5 receipts before reset, got %d", len(receiptsBefore))
	}

	// 3. Execute 5 consecutive ResetSession calls (idempotence test)
	for resetCycle := 1; resetCycle <= 5; resetCycle++ {
		resetRes, err := lat.ResetSession(ctx, "")
		if err != nil {
			t.Fatalf("ResetSession cycle %d failed: %v", resetCycle, err)
		}
		if resetRes.TokensPreserved != 0 {
			t.Errorf("Expected 0 tokens preserved, got %d", resetRes.TokensPreserved)
		}

		// Verify dialogue_turn is 0
		dialogueAfter, _ := lat.RecallScoped(ctx, RecallRequest{Family: "dialogue_turn"})
		if dialogueAfter.Count != 0 {
			t.Fatalf("Cycle %d: expected 0 dialogue turns after reset, got %d", resetCycle, dialogueAfter.Count)
		}

		// Verify all 5 receipts survive 100%
		receiptsAfter := lat.GetActuationReceipts("")
		if len(receiptsAfter) != 5 {
			t.Fatalf("Cycle %d: CRITICAL: receipts pruned by ResetSession! Expected 5, got %d",
				resetCycle, len(receiptsAfter))
		}

		// Verify QuoteSlot still verifies on all 5 slots
		for slot := 1; slot <= 5; slot++ {
			quoteRes, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
				Family: "actuation_receipts",
				Index:  slot,
			})
			if err != nil || !quoteRes.OK || !quoteRes.Verified {
				t.Fatalf("Cycle %d: QuoteSlot slot %d failed after reset: %v, %+v", resetCycle, slot, err, quoteRes)
			}
		}
	}

	t.Logf("ResetSession durability verified across 5 consecutive reset cycles (0 receipts lost, 100%% verified)")
}

// ============================================================================
// PART 4: Concurrency Stress Harness (Under Go Race Detector)
// ============================================================================

// TestConcurrentActuation_RaceFree tests 30 concurrent goroutines executing
// 1,500 operations (dispatches, reads, quote slot verifications, and session resets).
func TestConcurrentActuation_RaceFree(t *testing.T) {
	ctx := context.Background()
	lat := NewLocalLattice()

	const numWorkers = 30
	const opsPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		workerID := w
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for op := 0; op < opsPerWorker; op++ {
				action := rng.Intn(4)
				switch action {
				case 0:
					// Dispatch an actuation receipt
					r := &ActuationReceipt{
						FileModified:      fmt.Sprintf("pkg/w%d/file_%d.go", workerID, op),
						ExitCode:          rng.Intn(2),
						DiagnosticMessage: fmt.Sprintf("Diagnostic message w=%d op=%d", workerID, op),
						RepairDiffSummary: fmt.Sprintf("Diff summary w=%d op=%d", workerID, op),
						SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("worker_%d_op_%d", workerID, op)),
						TurnNumber:        op,
					}
					lat.DispatchActuation(ctx, DispatchActuationRequest{
						Surface: "engine_stage5_patch",
						Action:  "fuzzy_patch",
						Payload: r.ToPayload(),
					})

				case 1:
					// Read all receipts
					_ = lat.GetActuationReceipts("")

				case 2:
					// Quote slot verification on a random index
					idx := rng.Intn(20) + 1
					res, err := lat.QuoteSlot(ctx, QuoteSlotRequest{
						Family: "actuation_receipts",
						Index:  idx,
					})
					if err == nil && res.OK {
						if !res.Verified {
							t.Errorf("Concurrent QuoteSlot reported Verified=false for slot %d", idx)
						}
					}

				case 3:
					// Add a dialogue turn and occasionally reset session
					_, _ = lat.Remember(ctx, RememberRequest{
						Text:     "conversation turn",
						Family:   "dialogue_turn",
						SourceID: fmt.Sprintf("w%d", workerID),
					})
					if rng.Intn(10) == 0 {
						_, _ = lat.ResetSession(ctx, "")
					}
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("Concurrent stress harness completed %d operations across %d goroutines with zero data races",
		numWorkers*opsPerWorker, numWorkers)
}
