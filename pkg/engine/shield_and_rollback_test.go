// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

func TestHostPackageShield(t *testing.T) {
	// 1. Verify host package identification
	hostPaths := []string{
		"pkg/canonical/merkle.go",
		"pkg/icx/lattice.go",
		"pkg/engine/turn.go",
		"pkg/devops/editblock.go",
		"pkg/llm/cards.go",
		"pkg/mcp/server.go",
		"pkg/ui/cli.go",
		"cmd/omnicoder/main.go",
		"./pkg/icx/types.go",
		"/pkg/canonical/canonical.go",
	}

	for _, p := range hostPaths {
		if !isHostPackagePath(p) {
			t.Errorf("expected isHostPackagePath(%q) == true, got false", p)
		}
	}

	nonHostPaths := []string{
		"projects/my-service/main.go",
		"projects/web-dashboard/app.js",
		"src/index.ts",
		"cmd/myapp/main.go",
		"internal/service/worker.go",
	}

	for _, p := range nonHostPaths {
		if isHostPackagePath(p) {
			t.Errorf("expected isHostPackagePath(%q) == false, got true", p)
		}
	}

	// 2. Verify isHostPackageTask
	if isHostPackageTask("Build a telemetry dashboard in projects/my-service") {
		t.Errorf("user project task should not be classified as host package task")
	}

	if !isHostPackageTask("Refactor pkg/engine/turn.go to add rollback support") {
		t.Errorf("task mentioning pkg/engine should be classified as host package task")
	}

	if !isHostPackageTask("Fix OmniCoder CLI argument parsing") {
		t.Errorf("task mentioning omnicoder should be classified as host package task")
	}
}

func TestAtomicRollback_OnTurnRejection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-rollback-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create an existing clean file
	existingFile := filepath.Join(tmpDir, "calculator.go")
	originalContent := []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	if err := os.WriteFile(existingFile, originalContent, 0644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	pipeline := &TurnPipeline{}
	turn := &TurnState{
		TurnNumber:       1,
		PreTurnSnapshots: make(map[string][]byte),
	}

	// Simulate PreTurnSnapshots:
	// 1. Existing file snapshot
	turn.PreTurnSnapshots["calculator.go"] = originalContent
	// 2. Brand new file that didn't exist before turn
	turn.PreTurnSnapshots["corrupted_new.go"] = nil

	// Corrupt existing file (e.g. malformed edit block applied)
	corruptedContent := append(originalContent, []byte("syntax error: non-declaration statement outside function body\n")...)
	_ = os.WriteFile(existingFile, corruptedContent, 0644)

	// Create new unverified file
	newFile := filepath.Join(tmpDir, "corrupted_new.go")
	_ = os.WriteFile(newFile, []byte("broken code"), 0644)

	// Execute rollback
	reverted := pipeline.rollbackTurnSnapshots(tmpDir, turn)
	if reverted != 2 {
		t.Fatalf("expected 2 files reverted, got %d", reverted)
	}

	// Verify existing file was restored byte-exact
	restored, err := os.ReadFile(existingFile)
	if err != nil {
		t.Fatalf("failed to read restored file: %v", err)
	}
	if string(restored) != string(originalContent) {
		t.Errorf("restored content mismatch:\nGot:  %q\nWant: %q", string(restored), string(originalContent))
	}

	// Verify new file was deleted
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Errorf("expected newly created file %q to be deleted on rollback, but it still exists", newFile)
	}
}

func TestStage5_NoToxicAppendFallback(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-stage5-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cleanFile := filepath.Join(tmpDir, "service.go")
	initialContent := "package service\n\ntype Service struct{}\n"
	if err := os.WriteFile(cleanFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("write service.go: %v", err)
	}

	// Setup pipeline and state
	lat := icx.NewLocalLattice()
	eng, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: tmpDir,
		ICXClient:    lat,
		Offline:      true,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	state := NewEngineState("Build a feature", EngineConfig{WorkspaceDir: tmpDir})
	turn := state.BeginTurn("Build a feature", []string{"service.go"})

	// Provide an edit block with SEARCH TEXT that does NOT exist in service.go
	turn.Edits = []*llm.EditDecision{
		{
			FilePath: "service.go",
			EditBlocks: []llm.EditBlockData{
				{
					SearchText:  "nonExistentMethod() { ... }",
					ReplaceText: "func BrokenAppendedFunction() { return }",
				},
			},
		},
	}

	// Execute Stage 5
	err = eng.pipeline.executeStage5FuzzyPatch(context.Background(), state, turn, EngineConfig{WorkspaceDir: tmpDir})
	if err != nil {
		t.Fatalf("executeStage5FuzzyPatch failed: %v", err)
	}

	// Read service.go — it MUST NOT have the broken code appended to EOF!
	afterContent, err := os.ReadFile(cleanFile)
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}

	if string(afterContent) != initialContent {
		t.Errorf("service.go was corrupted by blind append!\nGot:\n%s\nWant:\n%s", string(afterContent), initialContent)
	}
}

func TestPhaseBoundaryTransition_OnlyTargetsMissingFiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-phase-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	scope := "projects/test-app"
	plan := &PlanSpec{
		ProjectScope: scope,
		ModuleName:   "testapp",
		Phases: []PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Phase 1 Core",
				TargetFiles: []string{
					scope + "/go.mod",
					scope + "/pkg/core/core.go",
				},
			},
			{
				PhaseNumber: 2,
				Name:        "Phase 2 Service",
				TargetFiles: []string{
					scope + "/pkg/service/service.go",
					scope + "/pkg/service/service_test.go",
				},
			},
		},
	}

	// 1. Initially nothing exists. Active phase is Phase 1
	active := plan.DetermineActivePhase(tmpDir)
	if active == nil || active.PhaseNumber != 1 {
		t.Fatalf("expected Phase 1 active, got %+v", active)
	}

	// 2. Create Phase 1 files
	for _, f := range plan.Phases[0].TargetFiles {
		absPath := filepath.Join(tmpDir, filepath.FromSlash(f))
		_ = os.MkdirAll(filepath.Dir(absPath), 0755)
		_ = os.WriteFile(absPath, []byte("package core\n"), 0644)
	}

	// Now Phase 1 is complete. DetermineActivePhase should advance to Phase 2
	active = plan.DetermineActivePhase(tmpDir)
	if active == nil || active.PhaseNumber != 2 {
		t.Fatalf("expected Phase 2 active, got %+v", active)
	}

	// 3. Create one file of Phase 2 (service.go), leaving service_test.go missing
	svcPath := filepath.Join(tmpDir, filepath.FromSlash(plan.Phases[1].TargetFiles[0]))
	_ = os.MkdirAll(filepath.Dir(svcPath), 0755)
	_ = os.WriteFile(svcPath, []byte("package service\n"), 0644)

	// DetermineActivePhase is still Phase 2 because service_test.go is missing
	active = plan.DetermineActivePhase(tmpDir)
	if active == nil || active.PhaseNumber != 2 {
		t.Fatalf("expected Phase 2 active, got %+v", active)
	}

	// 4. Create remaining file of Phase 2
	testPath := filepath.Join(tmpDir, filepath.FromSlash(plan.Phases[1].TargetFiles[1]))
	_ = os.WriteFile(testPath, []byte("package service\n"), 0644)

	// Now all files exist. DetermineActivePhase should return nil
	active = plan.DetermineActivePhase(tmpDir)
	if active != nil {
		t.Fatalf("expected nil active phase when all files exist, got %+v", active)
	}
	if !plan.IsComplete(tmpDir) {
		t.Fatalf("expected plan.IsComplete() == true")
	}
}

func TestMicroVMRollback_OnTurnRejection(t *testing.T) {
	tmpDir := t.TempDir()
	originalFile := filepath.Join(tmpDir, "service.go")
	originalContent := "package service\n\nfunc Serve() string { return \"ok\" }\n"
	_ = os.WriteFile(originalFile, []byte(originalContent), 0644)

	vmClient := vm.NewHermeticLocalSandbox(tmpDir)
	sb, err := vmClient.CreateSandbox(context.Background(), vm.SandboxConfig{WorkspaceDir: tmpDir})
	if err != nil {
		t.Fatalf("CreateSandbox error: %v", err)
	}

	chkID, err := vmClient.Snapshot(context.Background(), sb.ID, "pre_turn")
	if err != nil {
		t.Fatalf("Snapshot error: %v", err)
	}

	// Corrupt file in sandbox
	_ = vmClient.WriteFile(context.Background(), sb.ID, "service.go", "corrupted syntax error")
	_ = vmClient.WriteFile(context.Background(), sb.ID, "bad_artifact.txt", "debris")

	// Trigger rollback
	if err := vmClient.Rollback(context.Background(), sb.ID, chkID); err != nil {
		t.Fatalf("Rollback error: %v", err)
	}

	restored, err := vmClient.ReadFile(context.Background(), sb.ID, "service.go")
	if err != nil || restored != originalContent {
		t.Fatalf("expected restored content %q, got %q (err=%v)", originalContent, restored, err)
	}

	_, err = vmClient.ReadFile(context.Background(), sb.ID, "bad_artifact.txt")
	if err == nil {
		t.Fatalf("expected bad_artifact.txt to be wiped by rollback")
	}
}


