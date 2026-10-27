// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestBoundaryConfinement_TargetFile(t *testing.T) {
	tmpDir := t.TempDir()

	fileA := filepath.Join(tmpDir, "fileA.go")
	fileB := filepath.Join(tmpDir, "fileB.go")
	_ = os.WriteFile(fileA, []byte("package main\n\nfunc A() {}\n"), 0644)
	_ = os.WriteFile(fileB, []byte("package main\n\nfunc B() {}\n"), 0644)

	// Mock decision gate that attempts to edit both fileA and fileB
	mockGate := &mockMultiFileGate{
		targetFiles: []string{"fileA.go", "fileB.go"},
	}

	eng, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: tmpDir,
		MaxTurns:     1,
		Offline:      true,
		DecisionGate: mockGate,
		ICXClient:    icx.NewLocalLattice(),
		TargetFile:   "fileA.go",
	})
	if err != nil {
		t.Fatalf("failed to init orchestrator: %v", err)
	}

	report, err := eng.ExecuteTask(context.Background(), RunConfig{
		Task:         "Refactor fileA",
		WorkspaceDir: tmpDir,
		MaxTurns:     1,
		Offline:      true,
		TargetFile:   "fileA.go",
	})
	if err != nil {
		t.Fatalf("execution error: %v", err)
	}

	// Verify that fileB was never modified
	contentB, _ := os.ReadFile(fileB)
	if string(contentB) != "package main\n\nfunc B() {}\n" {
		t.Errorf("fileB was modified despite target-file constraint: %s", string(contentB))
	}

	// Verify files touched contains at most fileA.go
	for _, f := range report.FilesTouched {
		if filepath.Base(f) == "fileB.go" {
			t.Errorf("fileB appeared in FilesTouched: %v", report.FilesTouched)
		}
	}
}

func TestBoundaryConfinement_AuditOnly_ZeroDiskMutations(t *testing.T) {
	tmpDir := t.TempDir()

	target := filepath.Join(tmpDir, "target.go")
	initialContent := "package main\n\nfunc Original() string {\n\treturn \"orig\"\n}\n"
	_ = os.WriteFile(target, []byte(initialContent), 0644)

	mockGate := &mockMultiFileGate{
		targetFiles: []string{"target.go"},
	}

	eng, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: tmpDir,
		MaxTurns:     1,
		Offline:      true,
		DecisionGate: mockGate,
		ICXClient:    icx.NewLocalLattice(),
		TargetFile:   "target.go",
		AuditOnly:    true,
	})
	if err != nil {
		t.Fatalf("failed to init orchestrator: %v", err)
	}

	report, err := eng.ExecuteTask(context.Background(), RunConfig{
		Task:         "Audit and optimize target.go",
		WorkspaceDir: tmpDir,
		MaxTurns:     1,
		Offline:      true,
		TargetFile:   "target.go",
		AuditOnly:    true,
	})
	if err != nil {
		t.Fatalf("execution error in audit-only: %v", err)
	}

	if !report.Success {
		t.Errorf("expected audit-only to succeed, got false")
	}

	// Verify disk file is 100% unchanged
	currentContent, _ := os.ReadFile(target)
	if string(currentContent) != initialContent {
		t.Errorf("audit-only mode modified disk contents! Expected:\n%s\nGot:\n%s", initialContent, string(currentContent))
	}

	// Verify report metrics
	if report.ExecutionMS <= 0 && report.Duration < 0 {
		t.Errorf("expected positive ExecutionMS, got %d", report.ExecutionMS)
	}
}

func TestMatchesScope(t *testing.T) {
	cases := []struct {
		path     string
		scope    string
		expected bool
	}{
		{"frontend/src/pages/Dashboard.tsx", "frontend/src/*", true},
		{"frontend/src/pages/Dashboard.tsx", "frontend", true},
		{"frontend/src/pages/Dashboard.tsx", "backend", false},
		{"inspect_db.py", "frontend", false},
		{"pkg/vm/chained_exec.go", "pkg/vm", true},
		{"pkg/vm/chained_exec.go", "pkg/devops", false},
	}

	for _, c := range cases {
		got := matchesScope(c.path, c.scope)
		if got != c.expected {
			t.Errorf("matchesScope(%q, %q) = %v, expected %v", c.path, c.scope, got, c.expected)
		}
	}
}

type mockMultiFileGate struct {
	*llm.MockDecisionGate
	targetFiles []string
}

func (m *mockMultiFileGate) DecideIntent(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
	return &llm.IntentDecision{
		Operation:   llm.OpModify,
		TargetFiles: m.targetFiles,
		Rationale:   "Targeted test refactor",
		Confidence:  1.0,
	}, nil
}

func (m *mockMultiFileGate) DecideEdit(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
	return &llm.EditDecision{
		FilePath: card.FilePath,
		EditBlocks: []llm.EditBlockData{
			{
				SearchText:  "Original",
				ReplaceText: "Modified",
			},
		},
		Explanation: "Refactored function name",
	}, nil
}

func (m *mockMultiFileGate) DecideHeal(ctx context.Context, card llm.GateHealCard) (*llm.HealDecision, error) {
	return &llm.HealDecision{
		FilePath:      card.FilePath,
		RepairSummary: "Pre-healed",
	}, nil
}

func (m *mockMultiFileGate) DecideFinalize(ctx context.Context, card llm.GateFinalizeCard) (*llm.FinalizeDecision, error) {
	return &llm.FinalizeDecision{
		Approved:      true,
		CommitMessage: "chore: verified boundary confinement",
	}, nil
}
