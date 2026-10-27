// SPDX-License-Identifier: Apache-2.0
// Package orchestrator implements the isolated multi-process parallel agent swarm engine
// for OmniCoder-ICX.
//
// Two-Phase Commit (2PC), Semantic Merging & Stage 6 Self-Healing.
// Reconciles wave branches, validates disjoint target file boundaries, merges clean
// git trees into the base integration branch, and triggers automated Stage 6 self-healing.

package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
)

// ErrBoundaryOverlapViolation is returned when two or more phases in the same wave touch common target files.
var ErrBoundaryOverlapViolation = errors.New("boundary overlap violation: phases in the same wave touch overlapping target files")

// BoundaryOverlapError provides diagnostic details on which file, symbol, and phases caused an overlap.
type BoundaryOverlapError struct {
	File   string `json:"file"`
	Symbol string `json:"symbol,omitempty"`
	PhaseA int    `json:"phase_a"`
	PhaseB int    `json:"phase_b"`
}

func (e *BoundaryOverlapError) Error() string {
	if e.Symbol != "" {
		return fmt.Sprintf("%v: symbol %q in file %q is claimed by both Phase %d and Phase %d", ErrBoundaryOverlapViolation, e.Symbol, e.File, e.PhaseA, e.PhaseB)
	}
	return fmt.Sprintf("%v: file %q is claimed by both Phase %d and Phase %d", ErrBoundaryOverlapViolation, e.File, e.PhaseA, e.PhaseB)
}

func (e *BoundaryOverlapError) Unwrap() error {
	return ErrBoundaryOverlapViolation
}

func (e *BoundaryOverlapError) Is(target error) bool {
	return target == ErrBoundaryOverlapViolation
}

// MergeResult captures the outcome of reconciling a wave of worker branches.
type MergeResult struct {
	WaveIndex    int      `json:"wave_index"`
	MergedPhases []int    `json:"merged_phases"`
	FilesChanged []string `json:"files_changed"`
	BaseCommit   string   `json:"base_commit"`
	CleanMerge   bool     `json:"clean_merge"`
	HealRequired bool     `json:"heal_required"`
	Attestation  string   `json:"attestation"`
}

// TwoPhaseCommitter coordinates wave reconciliation.
type TwoPhaseCommitter struct {
	workspaceRoot   string
	baseBranch      string
	CustomWorkerCmd *exec.Cmd
	CustomHealer    func(ctx context.Context, failedPhase *engine.SwarmPhase, compileErr string) error
	VerificationFn  func(ctx context.Context, cmd string) error
	AutoVerify      bool
}

// NewTwoPhaseCommitter creates a new TwoPhaseCommitter instance.
func NewTwoPhaseCommitter(workspaceRoot, baseBranch string) *TwoPhaseCommitter {
	if workspaceRoot == "" {
		workspaceRoot = "."
	}
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		absRoot = workspaceRoot
	}
	if baseBranch == "" {
		baseBranch = "swarm/master-run"
	}
	return &TwoPhaseCommitter{
		workspaceRoot: absRoot,
		baseBranch:    baseBranch,
	}
}

// WorkspaceRoot returns the absolute path to the workspace root.
func (tpc *TwoPhaseCommitter) WorkspaceRoot() string {
	return tpc.workspaceRoot
}

// BaseBranch returns the configured integration branch name.
func (tpc *TwoPhaseCommitter) BaseBranch() string {
	return tpc.baseBranch
}

// getPhaseSymbolsForFile resolves symbol list for the given file path from the phase's TargetSymbols.
func getPhaseSymbolsForFile(phase *engine.SwarmPhase, filePath string) []string {
	if phase == nil || len(phase.TargetSymbols) == 0 {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(strings.TrimSpace(filePath), "\\", "/")))
	cleanLower := strings.ToLower(clean)
	baseLower := strings.ToLower(filepath.Base(clean))

	for k, syms := range phase.TargetSymbols {
		kClean := strings.ToLower(filepath.ToSlash(filepath.Clean(strings.ReplaceAll(strings.TrimSpace(k), "\\", "/"))))
		kBase := strings.ToLower(filepath.Base(kClean))
		if kClean == cleanLower || kBase == baseLower || strings.HasSuffix(cleanLower, "/"+kClean) || strings.HasSuffix(kClean, "/"+cleanLower) {
			return syms
		}
	}
	return nil
}

// AuditBoundaries verifies that no two phases in the wave share conflicting TargetFiles or TargetSymbols:
// 1. If Phase A and Phase B share no target files, boundary audit passes.
// 2. If Phase A and Phase B share file F, but BOTH phases explicitly declare disjoint TargetSymbols
//    for F (TargetSymbols(A, F) ∩ TargetSymbols(B, F) = ∅), AST-level boundary audit passes!
// 3. If they share file F and either phase has not declared fine-grained symbols, or their symbol
//    declarations overlap, a BoundaryOverlapError is returned.
func (tpc *TwoPhaseCommitter) AuditBoundaries(wavePhases []*engine.SwarmPhase) error {
	if len(wavePhases) <= 1 {
		return nil
	}

	// Map: lowercase canonical filepath -> phaseID that owns it as a whole file
	fileOwners := make(map[string]int)
	// Map: lowercase canonical filepath -> symbol name (lowercase) -> phaseID that owns that symbol
	symbolOwners := make(map[string]map[string]int)

	for _, phase := range wavePhases {
		if phase == nil {
			continue
		}
		phaseID := phase.PhaseNumber
		seenInPhase := make(map[string]bool)

		for _, rawPath := range phase.TargetFiles {
			cleaned := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(strings.TrimSpace(rawPath), "\\", "/")))
			if cleaned == "" || cleaned == "." {
				continue
			}
			lookupKey := strings.ToLower(cleaned)
			if seenInPhase[lookupKey] {
				continue
			}
			seenInPhase[lookupKey] = true

			phaseSymbols := getPhaseSymbolsForFile(phase, cleaned)

			if len(phaseSymbols) == 0 {
				// Phase claims the whole file
				if ownerPhase, exists := fileOwners[lookupKey]; exists && ownerPhase != phaseID {
					return &BoundaryOverlapError{
						File:   cleaned,
						PhaseA: ownerPhase,
						PhaseB: phaseID,
					}
				}
				if symMap, exists := symbolOwners[lookupKey]; exists && len(symMap) > 0 {
					for sym, ownerPhase := range symMap {
						if ownerPhase != phaseID {
							return &BoundaryOverlapError{
								File:   cleaned,
								Symbol: sym,
								PhaseA: ownerPhase,
								PhaseB: phaseID,
							}
						}
					}
				}
				fileOwners[lookupKey] = phaseID
			} else {
				// Phase declares fine-grained AST symbols
				if ownerPhase, exists := fileOwners[lookupKey]; exists && ownerPhase != phaseID {
					return &BoundaryOverlapError{
						File:   cleaned,
						PhaseA: ownerPhase,
						PhaseB: phaseID,
					}
				}

				if symbolOwners[lookupKey] == nil {
					symbolOwners[lookupKey] = make(map[string]int)
				}
				for _, sym := range phaseSymbols {
					symKey := strings.ToLower(strings.TrimSpace(sym))
					if symKey == "" {
						continue
					}
					if prevOwner, exists := symbolOwners[lookupKey][symKey]; exists && prevOwner != phaseID {
						return &BoundaryOverlapError{
							File:   cleaned,
							Symbol: sym,
							PhaseA: prevOwner,
							PhaseB: phaseID,
						}
					}
					symbolOwners[lookupKey][symKey] = phaseID
				}
			}
		}
	}

	return nil
}

// MergeWave reconciles all worker branches belonging to the given wave into the base integration branch.
func (tpc *TwoPhaseCommitter) MergeWave(ctx context.Context, wavePhases []*engine.SwarmPhase) (*MergeResult, error) {
	// Phase 1 (Prepare): Boundary & Overlap Audit
	if err := tpc.AuditBoundaries(wavePhases); err != nil {
		return nil, err
	}

	waveIdx := 0
	if len(wavePhases) > 0 && wavePhases[0] != nil {
		waveIdx = wavePhases[0].Wave
	}

	result := &MergeResult{
		WaveIndex:    waveIdx,
		MergedPhases: make([]int, 0, len(wavePhases)),
		FilesChanged: make([]string, 0),
		CleanMerge:   true,
		HealRequired: false,
	}

	if len(wavePhases) == 0 {
		return result, nil
	}

	// Phase 2 (Commit): Fast-Forward / Three-Way Rebase
	// 1. Ensure working directory is on baseBranch
	curBranchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	curBranchCmd.Dir = tpc.workspaceRoot
	curBranchOut, _ := curBranchCmd.Output()
	curBranch := strings.TrimSpace(string(curBranchOut))

	if curBranch != tpc.baseBranch {
		chkCmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", tpc.baseBranch)
		chkCmd.Dir = tpc.workspaceRoot
		var coCmd *exec.Cmd
		if chkCmd.Run() == nil {
			coCmd = exec.CommandContext(ctx, "git", "checkout", tpc.baseBranch)
		} else {
			coCmd = exec.CommandContext(ctx, "git", "checkout", "-b", tpc.baseBranch)
		}
		coCmd.Dir = tpc.workspaceRoot
		if out, err := coCmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("failed to switch to base branch %q: %w: %s", tpc.baseBranch, err, string(out))
		}
	}

	// 2. Capture base commit hash before merging
	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = tpc.workspaceRoot
	if out, err := revCmd.Output(); err == nil {
		result.BaseCommit = strings.TrimSpace(string(out))
	}

	// 3. Sequentially merge each worker's branch
	for _, phase := range wavePhases {
		if phase == nil {
			continue
		}
		phaseID := phase.PhaseNumber

		branchCandidates := []string{
			fmt.Sprintf("swarm/worker-phase-%d", phaseID),
			fmt.Sprintf("swarm/phase-%d", phaseID),
			fmt.Sprintf("worker-phase-%d", phaseID),
			fmt.Sprintf("phase-%d", phaseID),
		}

		var targetBranch string
		for _, candidate := range branchCandidates {
			vCmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", candidate)
			vCmd.Dir = tpc.workspaceRoot
			if vCmd.Run() == nil {
				targetBranch = candidate
				break
			}
		}

		if targetBranch == "" {
			return nil, fmt.Errorf("cannot merge phase %d: branch not found (checked candidates %v)", phaseID, branchCandidates)
		}

		mergeMsg := fmt.Sprintf("Merge swarm worker phase %d", phaseID)
		maxRetries := 5
		backoff := 25 * time.Millisecond
		var mergeErr error
		var mergeOut []byte

		for attempt := 0; attempt < maxRetries; attempt++ {
			mCmd := exec.CommandContext(ctx, "git",
				"-c", "user.name=OmniCoder Swarm",
				"-c", "user.email=support@caleralabs.com",
				"merge", "--no-ff", "-m", mergeMsg, targetBranch)
			mCmd.Dir = tpc.workspaceRoot
			mergeOut, mergeErr = mCmd.CombinedOutput()
			if mergeErr == nil {
				break
			}
			errStr := string(mergeOut)
			if strings.Contains(errStr, "lock") || strings.Contains(errStr, "Unable to create") {
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			break
		}

		if mergeErr != nil {
			// Check if merge conflict can be resolved via AST symbol reconciliation
			if rErr := tpc.tryReconcileASTMerge(ctx, phase, targetBranch); rErr == nil {
				mergeErr = nil
			} else {
				// Abort merge to keep repository clean
				abortCmd := exec.CommandContext(ctx, "git", "merge", "--abort")
				abortCmd.Dir = tpc.workspaceRoot
				_ = abortCmd.Run()

				result.CleanMerge = false
				return result, fmt.Errorf("merge conflict or failure for phase %d on branch %s: %w: %s", phaseID, targetBranch, mergeErr, string(mergeOut))
			}
		}

		result.MergedPhases = append(result.MergedPhases, phaseID)
	}

	// 4. Collect changed files
	changedMap := make(map[string]bool)
	if result.BaseCommit != "" {
		diffCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", result.BaseCommit, "HEAD")
		diffCmd.Dir = tpc.workspaceRoot
		if out, err := diffCmd.Output(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				cleaned := filepath.Clean(filepath.ToSlash(strings.TrimSpace(line)))
				if cleaned != "" && cleaned != "." {
					changedMap[cleaned] = true
				}
			}
		}
	}

	if len(changedMap) == 0 {
		for _, p := range wavePhases {
			if p != nil {
				for _, f := range p.TargetFiles {
					cleaned := filepath.Clean(filepath.ToSlash(strings.TrimSpace(f)))
					if cleaned != "" {
						changedMap[cleaned] = true
					}
				}
			}
		}
	}

	for f := range changedMap {
		result.FilesChanged = append(result.FilesChanged, f)
	}
	sort.Strings(result.FilesChanged)

	// 5. Generate Merkle Attestation Seal
	headCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	headCmd.Dir = tpc.workspaceRoot
	newHead := ""
	if out, err := headCmd.Output(); err == nil {
		newHead = strings.TrimSpace(string(out))
	}

	attestPayload := map[string]any{
		"wave_index":    result.WaveIndex,
		"base_commit":   result.BaseCommit,
		"head_commit":   newHead,
		"merged_phases": result.MergedPhases,
		"files_changed": result.FilesChanged,
	}
	result.Attestation = fmt.Sprintf("attest_wave_%d_%s", result.WaveIndex, canonical.SHA256Of(attestPayload))

	// 6. Optional Integrated Verification Gate if AutoVerify is enabled
	if tpc.AutoVerify {
		var verifyCmd string
		for _, p := range wavePhases {
			if p != nil && p.VerificationCmd != "" {
				verifyCmd = p.VerificationCmd
				break
			}
		}
		if verifyCmd != "" {
			if vErr := tpc.RunIntegratedVerification(ctx, verifyCmd); vErr != nil {
				result.HealRequired = true
			}
		}
	}

	return result, nil
}

// RunIntegratedVerification executes the integrated verification command across the whole tree.
func (tpc *TwoPhaseCommitter) RunIntegratedVerification(ctx context.Context, cmd string) error {
	if tpc.VerificationFn != nil {
		return tpc.VerificationFn(ctx, cmd)
	}
	if strings.TrimSpace(cmd) == "" {
		return nil
	}

	fbReq := devops.CompilerFeedbackRequest{
		Command:      cmd,
		WorkspaceDir: tpc.workspaceRoot,
	}
	report, err := devops.RunCompilerFeedbackLoop(ctx, fbReq)
	if err != nil {
		return fmt.Errorf("verification execution error: %w", err)
	}
	if !report.Success {
		errMsg := strings.TrimSpace(report.CleanedStderr)
		if errMsg == "" {
			errMsg = strings.TrimSpace(report.RawStderr)
		}
		if errMsg == "" {
			errMsg = strings.TrimSpace(report.RawStdout)
		}
		if errMsg == "" {
			errMsg = fmt.Sprintf("command %q failed with exit code %d", cmd, report.ExitCode)
		}
		return fmt.Errorf("integrated verification failed (exit %d): %s", report.ExitCode, errMsg)
	}
	return nil
}

// BuildHealPromptCard formats the compiler error into a <diagnostic:heal> prompt card.
func BuildHealPromptCard(phase *engine.SwarmPhase, compileErr string) string {
	var sb strings.Builder
	sb.WriteString("<diagnostic:heal>\n")
	sb.WriteString(fmt.Sprintf("Phase: %d (%s)\n", phase.PhaseNumber, phase.Name))
	if phase.ScopePrefix != "" {
		sb.WriteString(fmt.Sprintf("Scope: %s\n", phase.ScopePrefix))
	}
	if len(phase.TargetFiles) > 0 {
		sb.WriteString("Target Files:\n")
		for _, f := range phase.TargetFiles {
			sb.WriteString(fmt.Sprintf("  - %s\n", f))
		}
	}
	sb.WriteString("Compiler Error:\n")
	sb.WriteString(compileErr)
	if !strings.HasSuffix(compileErr, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("Instructions:\n")
	sb.WriteString("Surgically resolve the compiler / test failure. Fix signature mismatches or syntax defects within a budget of 3 turns.\n")
	sb.WriteString("</diagnostic:heal>")
	return sb.String()
}

// TriggerSelfHealing handles Stage 6 Integration Self-Healing when integrated tests fail.
// It packages the compiler error into a <diagnostic:heal> prompt card, attempts deterministic
// feedback repairs, or launches a surgical single-process healing worker with a budget of 3 turns.
func (tpc *TwoPhaseCommitter) TriggerSelfHealing(ctx context.Context, failedPhase *engine.SwarmPhase, compileErr string) error {
	if failedPhase == nil {
		return errors.New("cannot trigger self healing on nil phase")
	}

	if tpc.CustomHealer != nil {
		return tpc.CustomHealer(ctx, failedPhase, compileErr)
	}

	// 1. Package the compiler error into a <diagnostic:heal> prompt card
	promptCard := BuildHealPromptCard(failedPhase, compileErr)

	// 2. Invoke the devops compiler feedback loop to parse diagnostics
	fbReq := devops.CompilerFeedbackRequest{
		Command:                 failedPhase.VerificationCmd,
		WorkspaceDir:            tpc.workspaceRoot,
		DeterministicMockStderr: compileErr,
	}
	report, err := devops.RunCompilerFeedbackLoop(ctx, fbReq)
	if err != nil {
		return fmt.Errorf("failed to run compiler feedback loop: %w", err)
	}

	// 3. Attempt deterministic pre-heal on parsed diagnostics
	healedAny := false
	if len(report.Diagnostics) > 0 {
		for _, diag := range report.Diagnostics {
			resolvedFile := diag.File
			if !filepath.IsAbs(resolvedFile) {
				fullCandidate := filepath.Join(tpc.workspaceRoot, resolvedFile)
				if _, statErr := os.Stat(fullCandidate); statErr != nil {
					for _, tf := range failedPhase.TargetFiles {
						if strings.HasSuffix(filepath.ToSlash(tf), filepath.ToSlash(resolvedFile)) ||
							filepath.Base(tf) == filepath.Base(resolvedFile) {
							resolvedFile = tf
							break
						}
					}
				}
			}

			if editBlock, _, ok := engine.AttemptDeterministicPreHeal(tpc.workspaceRoot, resolvedFile, diag); ok {
				applyRes, applyErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
					RepoPath: tpc.workspaceRoot,
					Blocks:   []devops.EditBlock{editBlock},
				})
				if applyErr == nil && applyRes != nil && applyRes.Applied {
					healedAny = true
				}
			}
		}
	}

	// If deterministic pre-heal applied modifications, re-verify
	if healedAny {
		if failedPhase.VerificationCmd != "" {
			if vErr := tpc.RunIntegratedVerification(ctx, failedPhase.VerificationCmd); vErr == nil {
				return nil
			}
		} else {
			return nil
		}
	}

	// 4. Launch surgical single-process healing worker targeting the offending package with 3-turn budget
	eventCh := make(chan SwarmEvent, 50)
	cfg := WorkerConfig{
		ID:              failedPhase.PhaseNumber,
		PhaseNumber:     failedPhase.PhaseNumber,
		PhaseName:       "Stage 6 Self-Healing: " + failedPhase.Name,
		WorktreeDir:     tpc.workspaceRoot,
		ScopePrefix:     failedPhase.ScopePrefix,
		TargetFiles:     failedPhase.TargetFiles,
		VerificationCmd: failedPhase.VerificationCmd,
		TaskPrompt:      promptCard,
		MaxTurns:        3,
		Offline:         true,
		CustomCmd:       tpc.CustomWorkerCmd,
	}

	worker, err := LaunchWorker(ctx, cfg, eventCh)
	if err != nil {
		return fmt.Errorf("failed to launch healing worker: %w", err)
	}

	if waitErr := worker.Wait(); waitErr != nil {
		return fmt.Errorf("healing worker failed: %w", waitErr)
	}

	// 5. Re-verify upon heal completion
	if failedPhase.VerificationCmd != "" {
		if vErr := tpc.RunIntegratedVerification(ctx, failedPhase.VerificationCmd); vErr != nil {
			return fmt.Errorf("re-verification failed after healing: %w", vErr)
		}
	}

	return nil
}

// tryReconcileASTMerge attempts to resolve git merge conflicts by AST splicing declared symbols.
func (tpc *TwoPhaseCommitter) tryReconcileASTMerge(ctx context.Context, phase *engine.SwarmPhase, targetBranch string) error {
	if phase == nil || len(phase.TargetSymbols) == 0 {
		return errors.New("no target symbols declared for AST merge reconciliation")
	}

	stCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	stCmd.Dir = tpc.workspaceRoot
	stOut, err := stCmd.Output()
	if err != nil {
		return err
	}

	lines := strings.Split(strings.TrimSpace(string(stOut)), "\n")
	var conflictedFiles []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "UU ") || strings.HasPrefix(l, "AA ") || strings.HasPrefix(l, "UD ") || strings.HasPrefix(l, "DU ") {
			f := strings.TrimSpace(l[3:])
			f = filepath.ToSlash(filepath.Clean(f))
			conflictedFiles = append(conflictedFiles, f)
		}
	}

	if len(conflictedFiles) == 0 {
		return errors.New("no conflicted files found in git status")
	}

	for _, cFile := range conflictedFiles {
		if !strings.HasSuffix(cFile, ".go") {
			return fmt.Errorf("conflicted file %s is not a Go file, cannot AST reconcile", cFile)
		}

		symbols := getPhaseSymbolsForFile(phase, cFile)
		if len(symbols) == 0 {
			return fmt.Errorf("no target symbols found for conflicted file %s", cFile)
		}

		baseShowCmd := exec.CommandContext(ctx, "git", "show", "HEAD:"+cFile)
		baseShowCmd.Dir = tpc.workspaceRoot
		baseBytes, err := baseShowCmd.Output()
		if err != nil {
			return fmt.Errorf("failed to read base content for %s: %w", cFile, err)
		}

		branchShowCmd := exec.CommandContext(ctx, "git", "show", targetBranch+":"+cFile)
		branchShowCmd.Dir = tpc.workspaceRoot
		branchBytes, err := branchShowCmd.Output()
		if err != nil {
			return fmt.Errorf("failed to read branch content for %s: %w", cFile, err)
		}

		reconciledBytes, err := ReconcileASTDeclarations(baseBytes, branchBytes, symbols)
		if err != nil {
			return fmt.Errorf("failed to AST reconcile %s: %w", cFile, err)
		}

		diskPath := filepath.Join(tpc.workspaceRoot, cFile)
		if err := os.WriteFile(diskPath, reconciledBytes, 0644); err != nil {
			return fmt.Errorf("failed to write reconciled file %s: %w", cFile, err)
		}

		addCmd := exec.CommandContext(ctx, "git", "add", cFile)
		addCmd.Dir = tpc.workspaceRoot
		if out, err := addCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to git add reconciled %s: %w: %s", cFile, err, string(out))
		}
	}

	cMsg := fmt.Sprintf("Merge swarm worker phase %d (AST Reconciled)", phase.PhaseNumber)
	commitCmd := exec.CommandContext(ctx, "git",
		"-c", "user.name=OmniCoder Swarm",
		"-c", "user.email=support@caleralabs.com",
		"commit", "-m", cMsg)
	commitCmd.Dir = tpc.workspaceRoot
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to commit AST reconciled merge: %w: %s", err, string(out))
	}

	return nil
}

// ReconcileASTDeclarations splices matching functions, types, and values from branchBytes into baseBytes.
func ReconcileASTDeclarations(baseBytes, branchBytes []byte, targetSymbols []string) ([]byte, error) {
	if len(targetSymbols) == 0 {
		return baseBytes, errors.New("no target symbols specified for AST reconciliation")
	}

	symSet := make(map[string]bool)
	for _, s := range targetSymbols {
		clean := strings.ToLower(strings.TrimSpace(s))
		if clean != "" {
			symSet[clean] = true
		}
	}

	fsetBranch := token.NewFileSet()
	branchAST, err := parser.ParseFile(fsetBranch, "branch.go", branchBytes, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse branch Go source: %w", err)
	}

	fsetBase := token.NewFileSet()
	baseAST, err := parser.ParseFile(fsetBase, "base.go", baseBytes, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base Go source: %w", err)
	}

	var extractedDecls []ast.Decl
	for _, decl := range branchAST.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil && symSet[strings.ToLower(d.Name.Name)] {
				extractedDecls = append(extractedDecls, d)
			}
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil && symSet[strings.ToLower(ts.Name.Name)] {
						extractedDecls = append(extractedDecls, d)
						break
					}
				}
			case token.CONST, token.VAR:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range vs.Names {
							if name != nil && symSet[strings.ToLower(name.Name)] {
								extractedDecls = append(extractedDecls, d)
								break
							}
						}
					}
				}
			}
		}
	}

	if len(extractedDecls) == 0 {
		return baseBytes, errors.New("no matching symbol declarations found in branch")
	}

	for _, incDecl := range extractedDecls {
		replaced := false
		switch id := incDecl.(type) {
		case *ast.FuncDecl:
			funcName := id.Name.Name
			for i, bd := range baseAST.Decls {
				if bfd, ok := bd.(*ast.FuncDecl); ok && bfd.Name != nil && bfd.Name.Name == funcName {
					baseAST.Decls[i] = id
					replaced = true
					break
				}
			}
		case *ast.GenDecl:
			if id.Tok == token.TYPE && len(id.Specs) > 0 {
				if ts, ok := id.Specs[0].(*ast.TypeSpec); ok && ts.Name != nil {
					typeName := ts.Name.Name
					for i, bd := range baseAST.Decls {
						if bgd, ok := bd.(*ast.GenDecl); ok && bgd.Tok == token.TYPE && len(bgd.Specs) > 0 {
							if bts, ok := bgd.Specs[0].(*ast.TypeSpec); ok && bts.Name != nil && bts.Name.Name == typeName {
								baseAST.Decls[i] = id
								replaced = true
								break
							}
						}
					}
				}
			}
		}

		if !replaced {
			baseAST.Decls = append(baseAST.Decls, incDecl)
		}
	}

	var buf bytes.Buffer
	if err := format.Node(&buf, fsetBase, baseAST); err != nil {
		return nil, fmt.Errorf("failed to format reconciled Go AST: %w", err)
	}

	return buf.Bytes(), nil
}

