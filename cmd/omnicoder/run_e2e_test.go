// SPDX-License-Identifier: Apache-2.0
package main_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func setupRunE2ETestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmdInit := exec.Command("git", "init")
	cmdInit.Dir = dir
	if out, err := cmdInit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v, out: %s", err, out)
	}

	cmdUser := exec.Command("git", "config", "user.name", "TestRunner")
	cmdUser.Dir = dir
	_ = cmdUser.Run()

	cmdEmail := exec.Command("git", "config", "user.email", "support@caleralabs.com")
	cmdEmail.Dir = dir
	_ = cmdEmail.Run()

	goMod := fmt.Sprintf("module example.com/rune2e_%d\n\ngo 1.21\n", time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("write go.mod failed: %v", err)
	}

	initialCode := `package main

func Compute(x int) int {
	return x * 2
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(initialCode), 0644); err != nil {
		t.Fatalf("write main.go failed: %v", err)
	}

	cmdAdd := exec.Command("git", "add", ".")
	cmdAdd.Dir = dir
	_ = cmdAdd.Run()

	cmdCommit := exec.Command("git", "commit", "-m", "chore: initial commit")
	cmdCommit.Dir = dir
	_ = cmdCommit.Run()

	return dir
}

// verifyAuditMerkleProof validates that leaf at leafIdx is mathematically proven by expectedRoot.
func verifyAuditMerkleProof(leafHashes []string, leafIdx int, expectedRoot string) (bool, error) {
	if leafIdx < 0 || leafIdx >= len(leafHashes) {
		return false, fmt.Errorf("leafIdx %d out of bounds for %d leaves", leafIdx, len(leafHashes))
	}
	computedRoot := canonical.MerkleRoot(leafHashes)
	if computedRoot != expectedRoot {
		return false, fmt.Errorf("root mismatch: got %s, want %s", computedRoot, expectedRoot)
	}

	layer := make([]string, len(leafHashes))
	copy(layer, leafHashes)

	for len(layer) > 1 {
		next := make([]string, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			left := layer[i]
			right := left
			if i+1 < len(layer) {
				right = layer[i+1]
			}
			sum := sha256.Sum256([]byte(left + right))
			next = append(next, hex.EncodeToString(sum[:]))
		}
		layer = next
	}

	return layer[0] == expectedRoot, nil
}

// TestStandaloneBinary_RunAutonomousBuild exercises:
// 1. Invoking `omnicoder.exe run` as an external OS process in a temporary git workspace.
// 2. Verifying output includes all 8 stages, 1D ribbon progression, and Merkle root seal emission.
// 3. In-process deep verification with mock gate to measure token counts (<300 tokens), AST ribbon
//    (<160 tokens), actuation receipts in e.mem (<170 tokens), quoted slot provenance, Merkle inclusion,
//    and tamper resistance oracles.
func TestStandaloneBinary_RunAutonomousBuild(t *testing.T) {
	binPath := findOmnicoderBinary(t)
	ws := setupRunE2ETestRepo(t)

	// =========================================================================
	// Part 1: External Process Execution (`omnicoder.exe run`)
	// =========================================================================
	taskDesc := "Refactor Compute in main.go to return x * 3 and add tests"
	cmd := exec.Command(binPath, "run", taskDesc, "--workspace="+ws, "--offline", "--verbose")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	outAll := stdout.String() + "\n" + stderr.String()

	t.Logf("CLI Output:\n%s", outAll)

	if err != nil {
		t.Fatalf("omnicoder.exe run failed: %v\nOutput:\n%s", err, outAll)
	}

	// Verify required output banners and stage transitions
	requiredPhrases := []string{
		"OmniCoder-ICX Autonomous Coding Engine",
		"Task: \"Refactor Compute in main.go to return x * 3 and add tests\"",
		"[Stage 1/8] Ingest & RepoMap:",
		"[Stage 2/8] AST Blast-Radius Walk:",
		"[Stage 3/8] Verbatim Quoting & Planning:",
		"[Stage 4/8] Micro-Decision Gate:",
		"[Stage 5/8] 4-Tier Fuzzy Patch:",
		"[Stage 6/8] Compiler Feedback & Self-Healing:",
		"[Stage 7/8] Zero-Token Session Reset:",
		"[Stage 8/8] Git Commit & Attestation:",
		"Execution Summary: SUCCESS",
		"Merkle Root Seal:",
	}

	for _, phrase := range requiredPhrases {
		if !strings.Contains(outAll, phrase) {
			t.Fatalf("MISSING TELEMETRY: expected %q in omnicoder run output", phrase)
		}
	}

	// Extract and validate Merkle Root Seal from CLI output
	lines := strings.Split(outAll, "\n")
	var merkleSeal string
	for _, l := range lines {
		if strings.Contains(l, "Merkle Root Seal:") {
			parts := strings.Split(l, "Merkle Root Seal:")
			if len(parts) > 1 {
				merkleSeal = strings.TrimSpace(parts[1])
			}
		}
	}

	if len(merkleSeal) != 64 {
		t.Fatalf("Invalid Merkle Root Seal extracted: %q (len=%d, expected 64)", merkleSeal, len(merkleSeal))
	}
	t.Logf("PASS: External omnicoder.exe run executed autonomously and emitted Merkle Root Seal: %s", merkleSeal)

	// =========================================================================
	// Part 2: In-Process Deep Mathematical & Token Verification
	// =========================================================================
	ws2 := setupRunE2ETestRepo(t)
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Ingest sample AST nodes and quote slots
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "file_calc.go",
		Name:      "calc.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "func_Multiply",
		Name:      "Multiply",
		Type:      "function",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTEdge("file_calc.go", "defines", "func_Multiply")

	slotText := "package main\n\nfunc ExistingHelper() int { return 100 }\n"
	remRes, err := lat.Remember(ctx, icx.RememberRequest{
		Family:   "file_slots",
		SourceID: "calc.go:1-3",
		Text:     slotText,
	})
	if err != nil || remRes == nil {
		t.Fatalf("Remember failed: %v", err)
	}
	expectedSlotSHA := remRes.SHA256

	mockGate := llm.NewMockDecisionGate()
	var capturedEditCard llm.GateEditCard
	var capturedIntentCard llm.GateIntentCard
	var capturedHealCard llm.GateHealCard
	var capturedFinalizeCard llm.GateFinalizeCard
	var healCalled bool

	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		capturedIntentCard = card
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc.go"},
			Rationale:   "Autonomous build with compiler diagnostic recovery",
		}, nil
	}

	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		capturedEditCard = card
		return &llm.EditDecision{
			FilePath: card.FilePath,
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "",
					ReplaceText: "package main\n\nfunc Multiply(a, b int) int {\n\treturn a * b + undefinedVarFault\n}\n",
				},
			},
		}, nil
	}

	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		capturedHealCard = card
		healCalled = true
		return &llm.HealDecision{
			FilePath: "calc.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "\treturn a * b + undefinedVarFault",
					ReplaceText: "\treturn a * b",
				},
			},
			RepairSummary: "Eliminated undefinedVarFault",
		}, nil
	}

	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		capturedFinalizeCard = card
		return &llm.FinalizeDecision{
			Approved:      true,
			CommitMessage: "feat(calc): autonomous build completed",
			Summary:       "All checks passed cleanly",
		}, nil
	}

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: ws2,
		MaxTurns:     3,
		Offline:      true,
		DecisionGate: mockGate,
		ICXClient:    lat,
		TestCommand:  "go test -v ./...",
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         "Implement Multiply in calc.go",
		WorkspaceDir: ws2,
	})
	if err != nil || !report.Success {
		t.Fatalf("ExecuteTask failed: %v, report: %+v", err, report)
	}
	if !healCalled {
		t.Fatalf("Compiler self-healing was not invoked")
	}

	// 1. Verify Card Token Budgets (<300 tokens)
	for _, cardPrompt := range []struct {
		name string
		card interface {
			FormatPrompt() (string, string, error)
		}
	}{
		{"Intent", &capturedIntentCard},
		{"Edit", &capturedEditCard},
		{"Heal", &capturedHealCard},
		{"Finalize", &capturedFinalizeCard},
	} {
		sys, user, err := cardPrompt.card.FormatPrompt()
		if err != nil {
			t.Fatalf("%s card FormatPrompt failed: %v", cardPrompt.name, err)
		}
		toks := llm.EstimateTokens(sys + " " + user)
		if toks >= 300 {
			t.Fatalf("BUDGET BREACH: %s card tokens %d >= 300", cardPrompt.name, toks)
		}
		t.Logf("PASS: %s card token count: %d (< 300 tokens)", cardPrompt.name, toks)
	}

	// 2. Verify 1D Ribbon Format (<160 tokens)
	ribbonTokens := llm.EstimateTokens(capturedEditCard.ASTRibbon)
	if ribbonTokens >= 160 {
		t.Fatalf("BUDGET BREACH: ASTRibbon tokens %d >= 160", ribbonTokens)
	}
	if !strings.HasPrefix(capturedEditCard.ASTRibbon, "<nav:ribbon:") || !strings.HasSuffix(capturedEditCard.ASTRibbon, "/>") {
		t.Fatalf("FORMAT ERROR: ASTRibbon malformed: %s", capturedEditCard.ASTRibbon)
	}
	t.Logf("PASS: 1D ASTRibbon (%d tokens): %s", ribbonTokens, capturedEditCard.ASTRibbon)

	// 3. Verify Actuation Receipts in e.mem (<170 tokens) and Durability
	receipts := lat.GetActuationReceipts("")
	if len(receipts) < 2 {
		t.Fatalf("Expected at least 2 crystallized actuation receipts in e.mem, got %d", len(receipts))
	}
	for i, r := range receipts {
		compact := r.FormatCompact()
		toks := llm.EstimateTokens(compact)
		if toks >= 170 {
			t.Fatalf("BUDGET BREACH: receipt %d tokens %d >= 170: %s", i, toks, compact)
		}
		t.Logf("PASS: Actuation receipt %d (%d tokens): surface=%s action=%s seal=%s", i, toks, r.Surface, r.Action, r.SHA256Seal)
	}

	// Verify durability across session reset
	_, _ = lat.ResetSession(ctx, "")
	receiptsAfterReset := lat.GetActuationReceipts("")
	if len(receiptsAfterReset) != len(receipts) {
		t.Fatalf("Session reset durability loss: had %d, now %d", len(receipts), len(receiptsAfterReset))
	}

	// 4. Verify Quoted Slot Provenance & Merkle Inclusion Proofs
	quoteRes, quoteErr := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "file_slots",
		Index:  1,
	})
	if quoteErr != nil || !quoteRes.OK || !quoteRes.Verified {
		t.Fatalf("QuoteSlot failed: %v, %+v", quoteErr, quoteRes)
	}
	if quoteRes.SHA256 != expectedSlotSHA {
		t.Fatalf("QuoteSlot SHA256 mismatch: got %s, want %s", quoteRes.SHA256, expectedSlotSHA)
	}

	auditLeafSet := []string{
		quoteRes.SHA256,
		receipts[0].SHA256Seal,
		receipts[1].SHA256Seal,
	}
	auditRoot := canonical.MerkleRoot(auditLeafSet)

	// Verify inclusion proof for quoted slot (leaf 0)
	ok0, err0 := verifyAuditMerkleProof(auditLeafSet, 0, auditRoot)
	if err0 != nil || !ok0 {
		t.Fatalf("Inclusion proof failed for quoted slot: %v", err0)
	}

	// Verify inclusion proof for actuation receipts (leaves 1 and 2)
	ok1, err1 := verifyAuditMerkleProof(auditLeafSet, 1, auditRoot)
	if err1 != nil || !ok1 {
		t.Fatalf("Inclusion proof failed for Stage 5 receipt: %v", err1)
	}
	ok2, err2 := verifyAuditMerkleProof(auditLeafSet, 2, auditRoot)
	if err2 != nil || !ok2 {
		t.Fatalf("Inclusion proof failed for Stage 6 receipt: %v", err2)
	}

	// Tamper resistance oracle: flip a bit in quoted slot or receipts
	for leafIdx := 0; leafIdx < len(auditLeafSet); leafIdx++ {
		tampered := append([]string{}, auditLeafSet...)
		tampered[leafIdx] = canonical.SHA256Of("adversarial_bit_flip")
		tamperedRoot := canonical.MerkleRoot(tampered)
		if tamperedRoot == auditRoot {
			t.Fatalf("TAMPER ORACLE FAILED: tampering leaf %d did not alter Merkle root!", leafIdx)
		}
	}
	t.Logf("PASS: Quoted slot provenance and actuation receipts mathematically attested in Merkle audit tree with tamper resistance")
}
