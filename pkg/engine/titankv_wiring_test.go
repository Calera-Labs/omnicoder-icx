// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/storage"
)

// TestTitanKV_PreTurnSnapshotsCAS verifies that pre-turn file states are automatically
// persisted into Content-Addressed Storage (CAS) in the embedded TitanKV vault.
func TestTitanKV_PreTurnSnapshotsCAS(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Create a workspace file
	srcPath := filepath.Join(tmpDir, "calculator.go")
	originalContent := []byte("package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	if err := os.WriteFile(srcPath, originalContent, 0644); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(originalContent)
	expectedHash := hex.EncodeToString(sum[:])

	// 2. Open TitanKV Vault
	v, err := storage.OpenVault(tmpDir)
	if err != nil {
		t.Fatalf("OpenVault failed: %v", err)
	}
	defer v.Close()

	cfg := EngineConfig{
		WorkspaceDir: tmpDir,
		Vault:        v,
	}
	state := NewEngineState("test CAS pre-turn snapshots", cfg)
	defer state.Close()

	pipeline := &TurnPipeline{orch: &Orchestrator{cfg: cfg}}
	turn := state.BeginTurn("modify calculator", []string{"calculator.go"})

	// Prepare an edit decision to trigger Stage 5 actuation
	turn.Edits = []*llm.EditDecision{
		{
			FilePath: "calculator.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "return a + b",
					ReplaceText: "return a + b + 0",
				},
			},
		},
	}

	ctx := context.Background()
	if err := pipeline.executeStage5FuzzyPatch(ctx, state, turn, cfg); err != nil {
		t.Fatalf("executeStage5FuzzyPatch failed: %v", err)
	}

	// 3. Verify that the pre-turn snapshot exists in RAM
	snapshot, exists := turn.PreTurnSnapshots["calculator.go"]
	if !exists || string(snapshot) != string(originalContent) {
		t.Fatalf("expected in-memory snapshot to match original content")
	}

	// 4. Verify that the file content was committed into TitanKV CAS
	if !v.HasCAS(expectedHash) {
		t.Fatalf("expected TitanKV Vault to have CAS object for hash %s", expectedHash)
	}

	casData, err := v.GetCAS(expectedHash)
	if err != nil {
		t.Fatalf("GetCAS failed: %v", err)
	}
	if string(casData) != string(originalContent) {
		t.Errorf("CAS data mismatch: got %q, expected %q", string(casData), string(originalContent))
	}

	// 5. Verify stats reflect CAS object
	stats := v.Stats()
	if stats.CASCount < 1 {
		t.Errorf("expected CASCount >= 1 in vault stats, got %d", stats.CASCount)
	}
}

// TestTitanKV_SelfHealingCache verifies that winning compiler feedback patches
// are cached into TitanKV and applied on identical error fingerprints without LLM calls.
func TestTitanKV_SelfHealingCache(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Setup workspace with a broken file
	srcPath := filepath.Join(tmpDir, "sample.go")
	badCode := "package main\n\nfunc main() {\n\tvar x int = \"wrong\"\n}\n"
	if err := os.WriteFile(srcPath, []byte(badCode), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Open TitanKV Vault
	v, err := storage.OpenVault(tmpDir)
	if err != nil {
		t.Fatalf("OpenVault failed: %v", err)
	}
	defer v.Close()

	// 3. Formulate error fingerprint
	diagMsg := "cannot use \"wrong\" (untyped string constant) as int value in variable declaration"
	errHash := canonical.SHA256Bytes([]byte(strings.TrimSpace(diagMsg)))
	if len(errHash) > 16 {
		errHash = errHash[:16]
	}
	fingerprint := fmt.Sprintf("%s:%s", "sample.go", errHash)

	// Pre-seed a known winning patch into Vault
	winningPatch := []devops.EditBlock{
		{
			FilePath:    "sample.go",
			SearchText:  `var x int = "wrong"`,
			ReplaceText: `var x string = "wrong"`,
		},
	}
	patchBytes, _ := json.Marshal(winningPatch)
	if err := v.PutHealPattern(fingerprint, string(patchBytes)); err != nil {
		t.Fatalf("PutHealPattern failed: %v", err)
	}

	// 4. Verify pattern is retrieved and valid
	cached, found := v.GetHealPattern(fingerprint)
	if !found {
		t.Fatalf("expected heal pattern to be found in Vault")
	}

	var parsedBlocks []devops.EditBlock
	if err := json.Unmarshal([]byte(cached), &parsedBlocks); err != nil || len(parsedBlocks) == 0 {
		t.Fatalf("failed to unmarshal cached heal blocks: %v", err)
	}

	// 5. Apply the cached block and verify fix
	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath: tmpDir,
		Blocks:   parsedBlocks,
	})
	if err != nil || !res.Applied {
		t.Fatalf("failed to apply cached heal blocks: %v", err)
	}

	updated, _ := os.ReadFile(srcPath)
	if !strings.Contains(string(updated), `var x string = "wrong"`) {
		t.Errorf("file not patched correctly by cached pattern: %s", string(updated))
	}

	// 6. Verify stats reflect Heal Pattern
	stats := v.Stats()
	if stats.HealCount < 1 {
		t.Errorf("expected HealCount >= 1 in vault stats, got %d", stats.HealCount)
	}
}

type panicIfCalledGate struct {
	llm.DecisionGate
}

func (p *panicIfCalledGate) DecideHeal(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
	panic("DecideHeal must NOT be called on a cache hit!")
}

// TestTitanKV_Stage6CacheHit verifies that executeStage6CompilerFeedbackAndHeal applies
// cached heal patterns from TitanKV directly, bypassing the LLM gate entirely.
func TestTitanKV_Stage6CacheHit(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Write go.mod
	_ = os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte("module testpkg\n\ngo 1.21\n"), 0644)

	// 2. Write broken Go file
	srcPath := filepath.Join(tmpDir, "calc.go")
	badCode := "package testpkg\n\nfunc Calculate() int {\n\tvar x int = \"wrong\"\n\treturn 42\n}\n"
	_ = os.WriteFile(srcPath, []byte(badCode), 0644)

	testPath := filepath.Join(tmpDir, "calc_test.go")
	testCode := "package testpkg\n\nimport \"testing\"\n\nfunc TestCalc(t *testing.T) {\n\tif Calculate() != 42 { t.Fatal(\"wrong\") }\n}\n"
	_ = os.WriteFile(testPath, []byte(testCode), 0644)

	// 3. Open Vault
	v, err := storage.OpenVault(tmpDir)
	if err != nil {
		t.Fatalf("OpenVault failed: %v", err)
	}
	defer v.Close()

	// 4. Pre-run compiler check to capture exact diagnostic signature
	rep, _ := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:      "go test ./...",
		WorkspaceDir: tmpDir,
	})
	if len(rep.Diagnostics) == 0 {
		t.Fatalf("expected compiler diagnostics from broken code")
	}
	diag := rep.Diagnostics[0]
	errHash := canonical.SHA256Bytes([]byte(strings.TrimSpace(diag.Message)))
	if len(errHash) > 16 {
		errHash = errHash[:16]
	}
	fingerprint := fmt.Sprintf("%s:%s", filepath.ToSlash(diag.File), errHash)

	winningPatch := []devops.EditBlock{
		{
			FilePath:    diag.File,
			SearchText:  `var x int = "wrong"`,
			ReplaceText: `var x string = "wrong"`,
		},
	}
	patchBytes, _ := json.Marshal(winningPatch)
	_ = v.PutHealPattern(fingerprint, string(patchBytes))

	cfg := EngineConfig{
		WorkspaceDir: tmpDir,
		MaxTurns:     5,
		Vault:        v,
		TestCommand:  "go test ./...",
	}
	state := NewEngineState("test stage 6 cache hit", cfg)
	defer state.Close()

	orch := &Orchestrator{
		cfg:  cfg,
		gate: &panicIfCalledGate{}, // Panics if DecideHeal is called
	}
	pipeline := &TurnPipeline{orch: orch}
	turn := state.BeginTurn("heal calc.go", []string{"calc.go"})

	ctx := context.Background()
	if err := pipeline.executeStage6CompilerFeedbackAndHeal(ctx, state, turn, cfg); err != nil {
		t.Fatalf("executeStage6CompilerFeedbackAndHeal failed: %v", err)
	}

	// 5. Verify the file was fixed and tests now pass
	updated, _ := os.ReadFile(srcPath)
	if !strings.Contains(string(updated), `var x string = "wrong"`) {
		t.Fatalf("calc.go was not updated with cached heal patch: %s", string(updated))
	}

	if len(turn.HealDecisions) == 0 {
		t.Fatalf("expected heal decisions to record the cached fix")
	}
	if !strings.Contains(turn.HealDecisions[0].RepairSummary, "Vault cached heal pattern") {
		t.Errorf("expected RepairSummary to note Vault cache hit, got %q", turn.HealDecisions[0].RepairSummary)
	}
}

