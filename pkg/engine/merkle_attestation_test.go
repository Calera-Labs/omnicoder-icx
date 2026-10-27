// SPDX-License-Identifier: Apache-2.0
package engine

import (
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
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func setupAttestationRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmdInit := exec.Command("git", "init")
	cmdInit.Dir = dir
	if out, err := cmdInit.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v, out: %s", err, out)
	}

	cmdUser := exec.Command("git", "config", "user.name", "AttestationTester")
	cmdUser.Dir = dir
	_ = cmdUser.Run()

	cmdEmail := exec.Command("git", "config", "user.email", "support@caleralabs.com")
	cmdEmail.Dir = dir
	_ = cmdEmail.Run()

	goMod := fmt.Sprintf("module example.com/attestation%d\n\ngo 1.22\n", time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("write go.mod failed: %v", err)
	}

	cmdAdd := exec.Command("git", "add", "go.mod")
	cmdAdd.Dir = dir
	_ = cmdAdd.Run()

	cmdCommit := exec.Command("git", "commit", "-m", "chore: initial commit")
	cmdCommit.Dir = dir
	_ = cmdCommit.Run()

	return dir
}

// verifyMerkleInclusion verifies that leaf at leafIdx in leafHashes is attested
// by root using the canonical balanced binary Merkle tree logic with odd duplication.
func verifyMerkleInclusion(leafHashes []string, leafIdx int, expectedRoot string) (bool, error) {
	if leafIdx < 0 || leafIdx >= len(leafHashes) {
		return false, fmt.Errorf("leafIdx %d out of bounds for %d leaves", leafIdx, len(leafHashes))
	}

	computedRoot := canonical.MerkleRoot(leafHashes)
	if computedRoot != expectedRoot {
		return false, fmt.Errorf("root mismatch: got %s, want %s", computedRoot, expectedRoot)
	}

	// Build layer-by-layer audit path
	layer := make([]string, len(leafHashes))
	copy(layer, leafHashes)
	currentIdx := leafIdx

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
		currentIdx = currentIdx / 2
	}

	return layer[0] == expectedRoot, nil
}

// TestMerkleTreeAttestation_SelfHealingTurn executes a complete autonomous
// engine turn with Stage 6 compiler self-healing and verifies that both Stage 5 and
// Stage 6 actuation receipt seals are verifiably attested in the final Merkle root.
func TestMerkleTreeAttestation_SelfHealingTurn(t *testing.T) {
	ws := setupAttestationRepo(t)
	ctx := context.Background()

	lat := icx.NewLocalLattice()
	mockGate := llm.NewMockDecisionGate()

	// Intent: create calc.go and calc_test.go
	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpCreate,
			TargetFiles: []string{"calc.go", "calc_test.go"},
			Rationale:   "Create calc.go with intentional diagnostic fault for self-healing",
			Confidence:  0.98,
		}, nil
	}

	// Stage 4 Edit: intentional syntax/type fault in calc.go
	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		target := filepath.ToSlash(card.FilePath)
		var blocks []llm.EditBlockData

		if strings.HasSuffix(target, "calc.go") {
			content := "package testpkg\n\nfunc Multiply(a, b int) int {\n\treturn a * b + undefinedConstant\n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		} else if strings.HasSuffix(target, "calc_test.go") {
			content := "package testpkg\n\nimport \"testing\"\n\nfunc TestMultiply(t *testing.T) {\n\tif Multiply(3, 4) != 12 {\n\t\tt.Fatalf(\"Multiply failed\")\n\t}\n}\n"
			blocks = append(blocks, llm.EditBlockData{
				SearchText:  "",
				ReplaceText: content,
			})
		}

		return &llm.EditDecision{
			FilePath:    target,
			EditBlocks:  blocks,
			Explanation: "Initial code with intentional undefinedConstant fault",
		}, nil
	}

	// Stage 6 Heal: repair the fault by removing undefinedConstant
	healAttemptsCount := 0
	mockGate.HealFunc = func(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
		healAttemptsCount++
		return &llm.HealDecision{
			FilePath: "calc.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "\treturn a * b + undefinedConstant",
					ReplaceText: "\treturn a * b",
				},
			},
			RepairSummary: "Replaced undefinedConstant with pure multiplication",
			RootCause:     "unresolved identifier undefinedConstant",
		}, nil
	}

	// Stage 8 Finalize: approve commit when tests pass
	mockGate.FinalizeFunc = func(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
		if card.TestExitCode != 0 {
			return &llm.FinalizeDecision{Approved: false, Reason: "tests failed"}, nil
		}
		return &llm.FinalizeDecision{
			Approved:       true,
			CommitMessage:  "feat(calc): implemented and healed Multiply",
			Summary:        "Healed undefinedConstant and verified tests",
			FilesCommitted: card.FilesModified,
			Reason:         "clean compilation and test pass",
		}, nil
	}

	cfg := EngineConfig{
		WorkspaceDir:    ws,
		MaxTurns:        3,
		MaxHealAttempts: 3,
		Offline:         true,
		TestCommand:     "go test -v ./...",
		ICXClient:       lat,
		DecisionGate:    mockGate,
	}

	orch, err := NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	state := NewEngineState("Implement Multiply and heal any compilation errors", cfg)
	turn := state.BeginTurn(state.Task, nil)

	err = orch.pipeline.ExecuteTurn(ctx, state, turn, cfg)
	if err != nil {
		t.Fatalf("ExecuteTurn failed: %v", err)
	}
	state.CompleteTurn(turn)
	report := state.BuildReport(turn.Success, "turn completed")

	if !turn.Success {
		t.Fatalf("ExecuteTurn turn unsuccessful: %s", turn.ErrorMessage)
	}
	if healAttemptsCount == 0 {
		t.Fatalf("Expected DecideHeal to be called at least once")
	}

	// 1. Retrieve actuation receipts from e.mem (which survived Stage 7 ResetSession)
	crystallizedReceipts := lat.GetActuationReceipts("")
	if len(crystallizedReceipts) < 2 {
		t.Fatalf("Expected at least 2 crystallized receipts in e.mem, got %d", len(crystallizedReceipts))
	}

	var stage5Receipt *icx.ActuationReceipt
	var stage6Receipt *icx.ActuationReceipt

	for i := range crystallizedReceipts {
		r := &crystallizedReceipts[i]
		if r.Surface == "engine_stage5_patch" && r.Action == "fuzzy_patch" {
			stage5Receipt = r
		}
		if r.Surface == "engine_stage6_heal" && r.Action == "compiler_self_healing" {
			stage6Receipt = r
		}
	}

	if stage5Receipt == nil {
		t.Fatalf("Stage 5 fuzzy patch actuation receipt not found in e.mem")
	}
	if stage6Receipt == nil {
		t.Fatalf("Stage 6 self-healing actuation receipt not found in e.mem")
	}

	t.Logf("Found Stage 5 receipt seal: %s (exit: %d)", stage5Receipt.SHA256Seal, stage5Receipt.ExitCode)
	t.Logf("Found Stage 6 receipt seal: %s (exit: %d)", stage6Receipt.SHA256Seal, stage6Receipt.ExitCode)
	t.Logf("Turn Merkle Seal:           %s", turn.TurnMerkleSeal)
	t.Logf("Final Report Merkle Seal:   %s", report.MerkleSeal)

	// 2. Inspect Merkle leaves
	leaves := state.MerkleLeaves
	if len(leaves) == 0 {
		t.Fatalf("state.MerkleLeaves is empty")
	}
	t.Logf("state has %d Merkle leaves: %v", len(leaves), leaves)

	// 3. Assert that stage5Receipt.SHA256Seal is present in MerkleLeaves
	stage5LeafIdx := -1
	for idx, leaf := range leaves {
		if leaf == stage5Receipt.SHA256Seal {
			stage5LeafIdx = idx
			break
		}
	}
	if stage5LeafIdx == -1 {
		t.Fatalf("CRITICAL: stage5Receipt.SHA256Seal %q NOT found in MerkleLeaves", stage5Receipt.SHA256Seal)
	}

	// 4. Assert that stage6Receipt.SHA256Seal is present in MerkleLeaves
	stage6LeafIdx := -1
	for idx, leaf := range leaves {
		if leaf == stage6Receipt.SHA256Seal {
			stage6LeafIdx = idx
			break
		}
	}
	if stage6LeafIdx == -1 {
		t.Fatalf("CRITICAL: stage6Receipt.SHA256Seal %q NOT found in MerkleLeaves", stage6Receipt.SHA256Seal)
	}

	// 5. Verify mathematical derivation of final Merkle root
	computedRoot := canonical.MerkleRoot(leaves)
	if computedRoot != report.MerkleSeal {
		t.Fatalf("MerkleRoot(leaves) mismatch: got %s, report has %s", computedRoot, report.MerkleSeal)
	}

	// 6. Cryptographic Inclusion Proof verification for Stage 5 receipt
	ok5, err5 := verifyMerkleInclusion(leaves, stage5LeafIdx, report.MerkleSeal)
	if err5 != nil || !ok5 {
		t.Fatalf("Merkle inclusion proof failed for Stage 5 receipt: %v", err5)
	}
	t.Logf("PASS: Stage 5 receipt seal [%d] verifiably attested in Merkle root", stage5LeafIdx)

	// 7. Cryptographic Inclusion Proof verification for Stage 6 receipt
	ok6, err6 := verifyMerkleInclusion(leaves, stage6LeafIdx, report.MerkleSeal)
	if err6 != nil || !ok6 {
		t.Fatalf("Merkle inclusion proof failed for Stage 6 receipt: %v", err6)
	}
	t.Logf("PASS: Stage 6 receipt seal [%d] verifiably attested in Merkle root", stage6LeafIdx)

	// 8. Negative Oracle / Tamper Challenge:
	// If an adversary alters even a single bit of the receipt seal,
	// the Merkle root MUST NOT match.
	tamperedLeaves := make([]string, len(leaves))
	copy(tamperedLeaves, leaves)
	// Flip the last byte of stage6 receipt seal
	originalSeal := tamperedLeaves[stage6LeafIdx]
	var tamperedSeal string
	if strings.HasSuffix(originalSeal, "0") {
		tamperedSeal = originalSeal[:len(originalSeal)-1] + "1"
	} else {
		tamperedSeal = originalSeal[:len(originalSeal)-1] + "0"
	}
	tamperedLeaves[stage6LeafIdx] = tamperedSeal

	tamperedRoot := canonical.MerkleRoot(tamperedLeaves)
	if tamperedRoot == report.MerkleSeal {
		t.Fatalf("CRITICAL SECURITY FAILURE: Tampering with Stage 6 receipt seal did NOT alter Merkle root!")
	}
	t.Logf("PASS: Tamper oracle confirmed: tampered root %s != valid root %s", tamperedRoot, report.MerkleSeal)

	// 9. Negative Oracle 2: Omission of Stage 6 receipt seal alters root
	omittedLeaves := append(leaves[:stage6LeafIdx], leaves[stage6LeafIdx+1:]...)
	omittedRoot := canonical.MerkleRoot(omittedLeaves)
	if omittedRoot == report.MerkleSeal {
		t.Fatalf("CRITICAL SECURITY FAILURE: Omitting Stage 6 receipt seal did NOT alter Merkle root!")
	}
	t.Logf("PASS: Omission oracle confirmed: omitted root %s != valid root %s", omittedRoot, report.MerkleSeal)
}

// TestMerkleTree_ReceiptSensitivity tests that canonical.MerkleRoot
// is sensitive to every receipt leaf across varying leaf set sizes (odd, even, 1 to 32).
func TestMerkleTree_ReceiptSensitivity(t *testing.T) {
	for numLeaves := 1; numLeaves <= 32; numLeaves++ {
		leaves := make([]string, numLeaves)
		for i := 0; i < numLeaves; i++ {
			receipt := icx.ActuationReceipt{
				FileModified: fmt.Sprintf("file_%d.go", i),
				ExitCode:     i % 2,
				SHA256Seal:   canonical.SHA256Of(fmt.Sprintf("receipt_%d", i)),
			}
			leaves[i] = receipt.SHA256Seal
		}

		root := canonical.MerkleRoot(leaves)
		if len(root) != 64 {
			t.Fatalf("[%d leaves] Merkle root must be 64 hex characters, got %d", numLeaves, len(root))
		}

		// Verify every single leaf contributes to the root
		for i := 0; i < numLeaves; i++ {
			tampered := make([]string, numLeaves)
			copy(tampered, leaves)
			tampered[i] = canonical.SHA256Of(fmt.Sprintf("tampered_%d", i))
			tamperedRoot := canonical.MerkleRoot(tampered)
			if tamperedRoot == root {
				t.Fatalf("[%d leaves] Tampering with leaf %d did not change root!", numLeaves, i)
			}
		}
	}
}
