// SPDX-License-Identifier: Apache-2.0
package orchestrator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
)

// Test2PC_AuditDisjointBoundaries verifies that disjoint file sets pass and overlapping sets are rejected.
func Test2PC_AuditDisjointBoundaries(t *testing.T) {
	tpc := NewTwoPhaseCommitter(".", "swarm/master-run")

	// 1. Disjoint sets must pass cleanly
	p1 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 1,
			TargetFiles: []string{"pkg/a/a.go", "pkg/a/types.go"},
		},
		Wave: 1,
	}
	p2 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 2,
			TargetFiles: []string{"pkg/b/b.go", "pkg/b/service.go"},
		},
		Wave: 1,
	}
	p3 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 3,
			TargetFiles: []string{"pkg/c/c.go"},
		},
		Wave: 1,
	}

	if err := tpc.AuditBoundaries([]*engine.SwarmPhase{p1, p2, p3}); err != nil {
		t.Fatalf("expected disjoint file sets to pass audit, got: %v", err)
	}

	// 2. Overlapping file sets must be rejected with ErrBoundaryOverlapViolation
	pOverlap := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 4,
			TargetFiles: []string{"pkg/b/b.go", "pkg/common/util.go"},
		},
		Wave: 1,
	}

	err := tpc.AuditBoundaries([]*engine.SwarmPhase{p1, p2, pOverlap})
	if err == nil {
		t.Fatalf("expected overlap violation between Phase 2 and Phase 4 on pkg/b/b.go, but got nil")
	}

	if !errors.Is(err, ErrBoundaryOverlapViolation) {
		t.Fatalf("expected ErrBoundaryOverlapViolation, got: %v", err)
	}

	var boErr *BoundaryOverlapError
	if !errors.As(err, &boErr) {
		t.Fatalf("expected error to cast to *BoundaryOverlapError, got %T", err)
	}
	if boErr.PhaseA != 2 || boErr.PhaseB != 4 {
		t.Errorf("expected phases 2 and 4, got %d and %d", boErr.PhaseA, boErr.PhaseB)
	}
	if !strings.Contains(boErr.File, "pkg/b/b.go") {
		t.Errorf("expected overlapping file to be pkg/b/b.go, got: %s", boErr.File)
	}

	// 3. Path normalization (backslashes, relative paths, case insensitivity)
	pNormA := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 5,
			TargetFiles: []string{"pkg/shared/./config.go"},
		},
		Wave: 1,
	}
	pNormB := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 6,
			TargetFiles: []string{"pkg\\shared\\config.go"},
		},
		Wave: 1,
	}

	normErr := tpc.AuditBoundaries([]*engine.SwarmPhase{pNormA, pNormB})
	if normErr == nil || !errors.Is(normErr, ErrBoundaryOverlapViolation) {
		t.Fatalf("expected boundary overlap violation with normalized path matching, got: %v", normErr)
	}

	// 4. Single phase or empty slice should pass
	if err := tpc.AuditBoundaries([]*engine.SwarmPhase{p1}); err != nil {
		t.Errorf("single phase should pass audit: %v", err)
	}
	if err := tpc.AuditBoundaries(nil); err != nil {
		t.Errorf("nil phases should pass audit: %v", err)
	}
}

// Test2PC_CleanWaveMerge spawns two worktrees that write to pkg/a/a.go and pkg/b/b.go respectively,
// and merges both into base; asserts zero merge conflicts and valid Git tree.
func Test2PC_CleanWaveMerge(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Switch repo to swarm/master-run base integration branch
	coBaseCmd := exec.CommandContext(ctx, "git", "checkout", "-b", "swarm/master-run")
	coBaseCmd.Dir = repoDir
	if out, err := coBaseCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create base branch: %v: %s", err, string(out))
	}

	wm, err := NewWorktreeManager(repoDir)
	if err != nil {
		t.Fatalf("failed to create WorktreeManager: %v", err)
	}
	defer func() {
		_ = wm.PruneAll(context.Background())
	}()

	// 1. Allocate Worktree 1 for Phase 1
	alloc1, err := wm.Allocate(ctx, 1, "swarm/master-run")
	if err != nil {
		t.Fatalf("failed to allocate worktree 1: %v", err)
	}

	pkgADir := filepath.Join(alloc1.WorktreeDir, "pkg", "a")
	if err := os.MkdirAll(pkgADir, 0755); err != nil {
		t.Fatalf("failed to create pkg/a in worktree 1: %v", err)
	}
	fileA := filepath.Join(pkgADir, "a.go")
	contentA := "package a\n\nfunc A() string {\n\treturn \"Worker Phase 1 Output\"\n}\n"
	if err := os.WriteFile(fileA, []byte(contentA), 0644); err != nil {
		t.Fatalf("failed to write a.go: %v", err)
	}

	// Commit file inside worktree 1
	runGitIn := func(dir string, args ...string) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s failed: %v (%s)", args, dir, err, string(out))
		}
	}
	runGitIn(alloc1.WorktreeDir, "add", ".")
	runGitIn(alloc1.WorktreeDir, "commit", "-m", "Phase 1: pkg/a/a.go")

	// 2. Allocate Worktree 2 for Phase 2
	alloc2, err := wm.Allocate(ctx, 2, "swarm/master-run")
	if err != nil {
		t.Fatalf("failed to allocate worktree 2: %v", err)
	}

	pkgBDir := filepath.Join(alloc2.WorktreeDir, "pkg", "b")
	if err := os.MkdirAll(pkgBDir, 0755); err != nil {
		t.Fatalf("failed to create pkg/b in worktree 2: %v", err)
	}
	fileB := filepath.Join(pkgBDir, "b.go")
	contentB := "package b\n\nfunc B() string {\n\treturn \"Worker Phase 2 Output\"\n}\n"
	if err := os.WriteFile(fileB, []byte(contentB), 0644); err != nil {
		t.Fatalf("failed to write b.go: %v", err)
	}

	runGitIn(alloc2.WorktreeDir, "add", ".")
	runGitIn(alloc2.WorktreeDir, "commit", "-m", "Phase 2: pkg/b/b.go")

	// 3. Coordinate Two-Phase Commit reconciliation
	tpc := NewTwoPhaseCommitter(repoDir, "swarm/master-run")

	wavePhases := []*engine.SwarmPhase{
		{
			PlanPhase: engine.PlanPhase{
				PhaseNumber: 1,
				Name:        "Service A Scaffolding",
				TargetFiles: []string{"pkg/a/a.go"},
			},
			Wave: 1,
		},
		{
			PlanPhase: engine.PlanPhase{
				PhaseNumber: 2,
				Name:        "Service B Scaffolding",
				TargetFiles: []string{"pkg/b/b.go"},
			},
			Wave: 1,
		},
	}

	res, err := tpc.MergeWave(ctx, wavePhases)
	if err != nil {
		t.Fatalf("MergeWave failed unexpectedly: %v", err)
	}

	// 4. Validate MergeResult invariants
	if !res.CleanMerge {
		t.Errorf("expected CleanMerge == true")
	}
	if res.HealRequired {
		t.Errorf("expected HealRequired == false for clean wave merge")
	}
	if res.WaveIndex != 1 {
		t.Errorf("expected WaveIndex == 1, got %d", res.WaveIndex)
	}
	if len(res.MergedPhases) != 2 || res.MergedPhases[0] != 1 || res.MergedPhases[1] != 2 {
		t.Errorf("expected MergedPhases [1, 2], got %v", res.MergedPhases)
	}
	if !strings.HasPrefix(res.Attestation, "attest_wave_1_") {
		t.Errorf("expected attestation starting with attest_wave_1_, got %s", res.Attestation)
	}

	// 5. Verify physical presence of both merged files in root workspace
	mergedFileA := filepath.Join(repoDir, "pkg", "a", "a.go")
	mergedFileB := filepath.Join(repoDir, "pkg", "b", "b.go")

	dataA, err := os.ReadFile(mergedFileA)
	if err != nil {
		t.Fatalf("merged file pkg/a/a.go missing from workspace root: %v", err)
	}
	if !strings.Contains(string(dataA), "Worker Phase 1 Output") {
		t.Errorf("unexpected content in pkg/a/a.go: %s", string(dataA))
	}

	dataB, err := os.ReadFile(mergedFileB)
	if err != nil {
		t.Fatalf("merged file pkg/b/b.go missing from workspace root: %v", err)
	}
	if !strings.Contains(string(dataB), "Worker Phase 2 Output") {
		t.Errorf("unexpected content in pkg/b/b.go: %s", string(dataB))
	}

	// 6. Prune worktrees and verify Git status is 100% clean with zero merge conflict markers
	_ = wm.PruneAll(ctx)
	_ = os.RemoveAll(filepath.Join(repoDir, ".worktrees"))

	statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	statusCmd.Dir = repoDir
	statusOut, err := statusCmd.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if len(strings.TrimSpace(string(statusOut))) != 0 {
		t.Errorf("expected clean git status post-merge, got: %s", string(statusOut))
	}
}

// Test2PC_IntegrationSelfHealing injects an artificial compiler error post-merge
// and verifies that TriggerSelfHealing invokes the feedback loop and resolves the defect.
func Test2PC_IntegrationSelfHealing(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// 1. Initialize Go module and working package in test repo
	goModContent := "module testrepo\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	calcDir := filepath.Join(repoDir, "pkg", "calc")
	if err := os.MkdirAll(calcDir, 0755); err != nil {
		t.Fatalf("failed to create pkg/calc: %v", err)
	}

	calcCode := `package calc

func Add(a, b int) int {
	return a + b
}
`
	calcFile := filepath.Join(calcDir, "calc.go")
	if err := os.WriteFile(calcFile, []byte(calcCode), 0644); err != nil {
		t.Fatalf("failed to write calc.go: %v", err)
	}

	calcTestCode := `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2, 3) != 5")
	}
}
`
	calcTestFile := filepath.Join(calcDir, "calc_test.go")
	if err := os.WriteFile(calcTestFile, []byte(calcTestCode), 0644); err != nil {
		t.Fatalf("failed to write calc_test.go: %v", err)
	}

	// 2. Verify clean baseline execution
	tpc := NewTwoPhaseCommitter(repoDir, "main")
	if err := tpc.RunIntegratedVerification(ctx, "go test -v ./pkg/calc/..."); err != nil {
		t.Fatalf("baseline verification failed: %v", err)
	}

	// 3. Inject artificial compiler error post-merge (unused import: "math")
	brokenCalcCode := `package calc

import (
	"math"
)

func Add(a, b int) int {
	return a + b
}
`
	if err := os.WriteFile(calcFile, []byte(brokenCalcCode), 0644); err != nil {
		t.Fatalf("failed to inject broken code: %v", err)
	}

	// 4. Assert integrated verification correctly fails
	verifyErr := tpc.RunIntegratedVerification(ctx, "go test -v ./pkg/calc/...")
	if verifyErr == nil {
		t.Fatalf("expected integrated verification to fail on unused import defect")
	}

	// 5. Trigger automated Stage 6 Self-Healing
	failedPhase := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber:     1,
			Name:            "Calculator Module",
			TargetFiles:     []string{"pkg/calc/calc.go"},
			VerificationCmd: "go test -v ./pkg/calc/...",
		},
		ScopePrefix: "pkg/calc",
	}

	healErr := tpc.TriggerSelfHealing(ctx, failedPhase, verifyErr.Error())
	if healErr != nil {
		t.Fatalf("TriggerSelfHealing failed: %v", healErr)
	}

	// 6. Assert defect is resolved and re-verification succeeds
	if err := tpc.RunIntegratedVerification(ctx, "go test -v ./pkg/calc/..."); err != nil {
		t.Fatalf("expected package to pass verification after self-healing, got: %v", err)
	}

	repairedContent, err := os.ReadFile(calcFile)
	if err != nil {
		t.Fatalf("failed to read repaired calc.go: %v", err)
	}
	if strings.Contains(string(repairedContent), `"math"`) {
		t.Errorf("expected unused import 'math' to be purged by self-healing feedback loop, got:\n%s", string(repairedContent))
	}
}

// Test2PC_PromptCardFormatting verifies that BuildHealPromptCard generates compliant prompt blocks.
func Test2PC_PromptCardFormatting(t *testing.T) {
	phase := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 3,
			Name:        "Index Engine",
			TargetFiles: []string{"pkg/index/index.go", "pkg/index/types.go"},
		},
		ScopePrefix: "pkg/index",
	}

	prompt := BuildHealPromptCard(phase, "pkg/index/index.go:42: undefined: IndexEntry")
	if !strings.HasPrefix(prompt, "<diagnostic:heal>\n") {
		t.Errorf("missing <diagnostic:heal> opening tag")
	}
	if !strings.HasSuffix(prompt, "</diagnostic:heal>") {
		t.Errorf("missing </diagnostic:heal> closing tag")
	}
	if !strings.Contains(prompt, "Phase: 3 (Index Engine)") {
		t.Errorf("missing phase header in prompt card")
	}
	if !strings.Contains(prompt, "Scope: pkg/index") {
		t.Errorf("missing scope in prompt card")
	}
	if !strings.Contains(prompt, "pkg/index/index.go:42: undefined: IndexEntry") {
		t.Errorf("missing compiler error in prompt card")
	}
	if !strings.Contains(prompt, "budget of 3 turns") {
		t.Errorf("missing turn budget specification in prompt card")
	}
}

// Test2PC_OverlapHaltBeforeMerge asserts that MergeWave halts immediately on boundary overlap
// without performing any git merge or checkout operations.
func Test2PC_OverlapHaltBeforeMerge(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	ctx := context.Background()

	tpc := NewTwoPhaseCommitter(repoDir, "main")

	p1 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 1,
			TargetFiles: []string{"pkg/shared/config.go"},
		},
		Wave: 1,
	}
	p2 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 2,
			TargetFiles: []string{"pkg/shared/config.go"},
		},
		Wave: 1,
	}

	res, err := tpc.MergeWave(ctx, []*engine.SwarmPhase{p1, p2})
	if res != nil {
		t.Errorf("expected nil MergeResult on overlap halt, got %+v", res)
	}
	if !errors.Is(err, ErrBoundaryOverlapViolation) {
		t.Errorf("expected ErrBoundaryOverlapViolation, got %v", err)
	}
}

// Test2PC_ASTSymbolLevelBoundaryAudit verifies that two phases claiming the same file
// with disjoint symbols pass boundary audit, while overlapping symbols fail.
func Test2PC_ASTSymbolLevelBoundaryAudit(t *testing.T) {
	tpc := NewTwoPhaseCommitter(".", "main")

	// Case 1: Disjoint symbols on the same file -> PASS!
	p14 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 14,
			TargetFiles: []string{"internal/core/engine.go"},
			TargetSymbols: map[string][]string{
				"internal/core/engine.go": {"WorkerPool", "NewWorkerPool"},
			},
		},
		Wave: 1,
	}

	p15 := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 15,
			TargetFiles: []string{"internal/core/engine.go"},
			TargetSymbols: map[string][]string{
				"internal/core/engine.go": {"MetricsHook", "RegisterMetricsHook"},
			},
		},
		Wave: 1,
	}

	if err := tpc.AuditBoundaries([]*engine.SwarmPhase{p14, p15}); err != nil {
		t.Fatalf("expected disjoint symbols on same file to pass audit, got: %v", err)
	}

	// Case 2: Overlapping symbols on the same file -> FAIL!
	p15Overlap := &engine.SwarmPhase{
		PlanPhase: engine.PlanPhase{
			PhaseNumber: 15,
			TargetFiles: []string{"internal/core/engine.go"},
			TargetSymbols: map[string][]string{
				"internal/core/engine.go": {"WorkerPool", "RegisterMetricsHook"},
			},
		},
		Wave: 1,
	}

	err := tpc.AuditBoundaries([]*engine.SwarmPhase{p14, p15Overlap})
	if err == nil {
		t.Fatal("expected overlapping symbol 'WorkerPool' to fail audit, but got nil")
	}

	var boErr *BoundaryOverlapError
	if !errors.As(err, &boErr) {
		t.Fatalf("expected error to cast to *BoundaryOverlapError, got %T", err)
	}
	if boErr.Symbol != "WorkerPool" {
		t.Errorf("expected overlapping symbol 'WorkerPool', got %q", boErr.Symbol)
	}
	if boErr.PhaseA != 14 || boErr.PhaseB != 15 {
		t.Errorf("expected phases 14 and 15, got %d and %d", boErr.PhaseA, boErr.PhaseB)
	}
}

// Test2PC_ReconcileASTDeclarations verifies pure Go AST declaration splicing.
func Test2PC_ReconcileASTDeclarations(t *testing.T) {
	baseCode := `package core

// Existing WorkerPool
type WorkerPool struct {
	workers int
}

func NewWorkerPool(n int) *WorkerPool {
	return &WorkerPool{workers: n}
}
`

	branchCode := `package core

type MetricsHook struct {
	sampleRate float64
}

func NewMetricsHook() *MetricsHook {
	return &MetricsHook{sampleRate: 1.0}
}
`

	reconciled, err := ReconcileASTDeclarations(
		[]byte(baseCode),
		[]byte(branchCode),
		[]string{"MetricsHook", "NewMetricsHook"},
	)
	if err != nil {
		t.Fatalf("ReconcileASTDeclarations failed: %v", err)
	}

	reconciledStr := string(reconciled)
	if !strings.Contains(reconciledStr, "type WorkerPool struct") {
		t.Errorf("missing base WorkerPool in reconciled code")
	}
	if !strings.Contains(reconciledStr, "type MetricsHook struct") {
		t.Errorf("missing spliced MetricsHook in reconciled code")
	}
	if !strings.Contains(reconciledStr, "func NewMetricsHook()") {
		t.Errorf("missing spliced NewMetricsHook in reconciled code")
	}
}

