// SPDX-License-Identifier: Apache-2.0
// Package orchestrator implements the isolated multi-process parallel agent swarm engine
// for OmniCoder-ICX.
//
// Master Swarm Orchestrator Engine.
// Ingests implementation plan Markdown, compiles acyclic wave DAGs, allocates isolated worktrees,
// supervises parallel worker subprocesses, performs Two-Phase Commit reconciliation, and renders
// real-time telemetry to the unified ANSI Terminal Matrix HUD.

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/ui"
)

// SwarmConfig controls swarm concurrency and execution parameters.
type SwarmConfig struct {
	PlanPath          string        `json:"plan_path"`
	WorkspaceRoot     string        `json:"workspace_root"`
	MaxWorkers        int           `json:"max_workers"`          // Default: min(NumCPU, 4)
	MaxTurnsPerWorker int           `json:"max_turns_per_worker"` // Default: 5
	TimeoutPerPhase   time.Duration `json:"timeout_per_phase"`    // Default: 10m
	Model             string        `json:"model"`
	BaseURL           string        `json:"base_url"`
	APIKey            string        `json:"api_key"`
	Offline           bool          `json:"offline"`
	Verbose           bool          `json:"verbose"`
	OutputWriter      io.Writer     `json:"-"`
	CustomWorkerRunner func(ctx context.Context, phase *engine.SwarmPhase, alloc *WorktreeAllocation) error `json:"-"`
}

// SwarmWaveReport captures execution and merge results for a single swarm wave.
type SwarmWaveReport struct {
	WaveIndex    int      `json:"wave_index"`
	Phases       []int    `json:"phases"`
	CleanMerge   bool     `json:"clean_merge"`
	FilesChanged []string `json:"files_changed"`
	Attestation  string   `json:"attestation,omitempty"`
}

// SwarmExecutionResult models the structured JSON summary emitted upon swarm completion.
type SwarmExecutionResult struct {
	Status        string            `json:"status"`
	PlanPath      string            `json:"plan_path"`
	WorkspaceRoot string            `json:"workspace_root"`
	WavesCount    int               `json:"waves_count"`
	PhasesCount   int               `json:"phases_count"`
	Waves         []SwarmWaveReport `json:"waves"`
	FilesTouched  []string          `json:"files_touched"`
	DurationMs    int64             `json:"duration_ms"`
	MerkleSeal    string            `json:"merkle_seal"`
}

// MasterOrchestrator drives the entire multi-process swarm execution end-to-end.
type MasterOrchestrator struct {
	cfg          SwarmConfig
	plan         *engine.PlanSpec
	dag          *engine.SwarmDAG
	worktreeMgr  *WorktreeManager
	committer    *TwoPhaseCommitter
	registry     *ContractRegistry
	hud          *ui.SwarmHUD
	workers      map[int]*SwarmWorker
	workerMu     sync.RWMutex
	events       chan SwarmEvent
	outputWriter io.Writer
	lastResult   *SwarmExecutionResult

	// CustomWorkerRunner allows tests or callers to intercept phase execution if needed
	CustomWorkerRunner func(ctx context.Context, phase *engine.SwarmPhase, alloc *WorktreeAllocation) error
}

// NewMasterOrchestrator initializes a new MasterOrchestrator from the given SwarmConfig.
func NewMasterOrchestrator(cfg SwarmConfig) (*MasterOrchestrator, error) {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = runtime.NumCPU()
		if cfg.MaxWorkers > 4 {
			cfg.MaxWorkers = 4
		}
		if cfg.MaxWorkers <= 0 {
			cfg.MaxWorkers = 1
		}
	}
	if cfg.MaxTurnsPerWorker <= 0 {
		cfg.MaxTurnsPerWorker = 5
	}
	if cfg.TimeoutPerPhase <= 0 {
		cfg.TimeoutPerPhase = 10 * time.Minute
	}
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = "."
	}
	absWorkspace, err := filepath.Abs(cfg.WorkspaceRoot)
	if err == nil {
		cfg.WorkspaceRoot = absWorkspace
	}

	planPath := cfg.PlanPath
	if planPath == "" {
		return nil, errors.New("plan path cannot be empty")
	}

	if !filepath.IsAbs(planPath) {
		candidate := filepath.Join(cfg.WorkspaceRoot, planPath)
		if _, statErr := os.Stat(candidate); statErr == nil {
			planPath = candidate
		}
	}

	data, err := os.ReadFile(planPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read plan: %w", err)
	}

	planSpec := engine.ParsePlanMarkdown(string(data))
	if planSpec == nil || len(planSpec.Phases) == 0 {
		return nil, fmt.Errorf("failed to parse valid phases from plan: %s", cfg.PlanPath)
	}

	dag, err := engine.CompileSwarmDAG(planSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to compile swarm DAG: %w", err)
	}

	wtMgr, err := NewWorktreeManager(cfg.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize worktree manager: %w", err)
	}

	baseBranch := "swarm/master-run"
	chkCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	chkCmd.Dir = cfg.WorkspaceRoot
	if out, err := chkCmd.Output(); err == nil {
		b := strings.TrimSpace(string(out))
		if b != "" && b != "HEAD" {
			baseBranch = b
		}
	}

	committer := NewTwoPhaseCommitter(cfg.WorkspaceRoot, baseBranch)
	registry := NewContractRegistry(nil, "swarm-contracts")

	outW := cfg.OutputWriter
	if outW == nil {
		outW = os.Stdout
	}
	hud := ui.NewSwarmHUD(outW)

	return &MasterOrchestrator{
		cfg:                cfg,
		plan:               planSpec,
		dag:                dag,
		worktreeMgr:        wtMgr,
		committer:          committer,
		registry:           registry,
		hud:                hud,
		workers:            make(map[int]*SwarmWorker),
		events:             make(chan SwarmEvent, 1000),
		outputWriter:       outW,
		CustomWorkerRunner: cfg.CustomWorkerRunner,
	}, nil
}

// Plan returns the parsed PlanSpec.
func (m *MasterOrchestrator) Plan() *engine.PlanSpec {
	return m.plan
}

// DAG returns the compiled SwarmDAG.
func (m *MasterOrchestrator) DAG() *engine.SwarmDAG {
	return m.dag
}

// Config returns the configuration used by the orchestrator.
func (m *MasterOrchestrator) Config() SwarmConfig {
	return m.cfg
}

// WorktreeManager returns the underlying WorktreeManager.
func (m *MasterOrchestrator) WorktreeManager() *WorktreeManager {
	return m.worktreeMgr
}

// TwoPhaseCommitter returns the underlying TwoPhaseCommitter.
func (m *MasterOrchestrator) TwoPhaseCommitter() *TwoPhaseCommitter {
	return m.committer
}

// ContractRegistry returns the underlying ContractRegistry.
func (m *MasterOrchestrator) ContractRegistry() *ContractRegistry {
	return m.registry
}

// HUD returns the underlying SwarmHUD.
func (m *MasterOrchestrator) HUD() *ui.SwarmHUD {
	return m.hud
}

// ExecuteProject drives the entire multi-process swarm execution end-to-end.
func (m *MasterOrchestrator) ExecuteProject(ctx context.Context) error {
	start := time.Now()
	outW := m.outputWriter
	if outW == nil {
		outW = os.Stdout
	}

	fmt.Fprintf(outW, "[Swarm] Initializing Swarm Forge for %s (%d Phases, %d Waves)\n",
		m.cfg.PlanPath, len(m.dag.Phases), len(m.dag.Waves))

	// Ensure cleanup of any leftover worktrees at end of execution
	defer func() {
		_ = m.worktreeMgr.PruneAll(context.Background())
	}()

	// Event logger goroutine to drain m.events and feed HUD
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		for ev := range m.events {
			m.hud.LogEvent(fmt.Sprintf("[Phase %d] %s: %s", ev.Phase, ev.State, ev.Message))
		}
	}()
	defer func() {
		close(m.events)
		<-eventDone
	}()

	var waveReports []SwarmWaveReport
	var leafHashes []string

	// Execute waves sequentially; execute phases within each wave concurrently
	for waveIdx, wave := range m.dag.Waves {
		fmt.Fprintf(outW, "\n>>> LAUNCHING EXECUTION WAVE %d (%d Parallel Phases) <<<\n", waveIdx, len(wave))

		var wavePhases []*engine.SwarmPhase
		for _, phaseNum := range wave {
			if p, ok := m.dag.Phases[phaseNum]; ok {
				wavePhases = append(wavePhases, p)
			}
		}

		// Boundary audit across wave phases
		if err := m.committer.AuditBoundaries(wavePhases); err != nil {
			return fmt.Errorf("wave %d boundary overlap violation: %w", waveIdx, err)
		}

		concurrency := m.cfg.MaxWorkers
		if concurrency <= 0 {
			concurrency = 4
		}
		sem := make(chan struct{}, concurrency)

		var wg sync.WaitGroup
		errChan := make(chan error, len(wavePhases))

		for _, phase := range wavePhases {
			wg.Add(1)
			sem <- struct{}{}

			go func(p *engine.SwarmPhase) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := m.executeWorkerPhase(ctx, p); err != nil {
					errChan <- fmt.Errorf("phase %d failed: %w", p.PhaseNumber, err)
				}
			}(phase)
		}

		wg.Wait()
		close(errChan)

		// Check for wave failures
		var errMsgs []string
		for e := range errChan {
			errMsgs = append(errMsgs, e.Error())
		}
		if len(errMsgs) > 0 {
			return fmt.Errorf("wave %d aborted due to failures: %s", waveIdx, strings.Join(errMsgs, "; "))
		}

		// Merge wave branches into base integration branch via 2PC
		hasGitBranches := true
		for _, p := range wavePhases {
			m.worktreeMgr.mu.RLock()
			alloc := m.worktreeMgr.allocations[p.PhaseNumber]
			m.worktreeMgr.mu.RUnlock()
			if alloc != nil && alloc.IsJunction {
				hasGitBranches = false
			}
		}

		waveReport := SwarmWaveReport{
			WaveIndex:  waveIdx,
			Phases:     wave,
			CleanMerge: true,
		}

		if hasGitBranches && len(wavePhases) > 0 {
			mergeRes, err := m.committer.MergeWave(ctx, wavePhases)
			if err != nil {
				return fmt.Errorf("wave %d merge failed: %w", waveIdx, err)
			}
			if m.cfg.Verbose && mergeRes != nil {
				fmt.Fprintf(outW, "[Swarm] Wave %d merged %d phases cleanly (base commit: %s)\n",
					waveIdx, len(mergeRes.MergedPhases), mergeRes.BaseCommit)
			}
			if mergeRes != nil {
				waveReport.CleanMerge = mergeRes.CleanMerge
				waveReport.FilesChanged = mergeRes.FilesChanged
				waveReport.Attestation = mergeRes.Attestation
				if mergeRes.Attestation != "" {
					leafHashes = append(leafHashes, mergeRes.Attestation)
				}
			}
		} else {
			var waveFiles []string
			for _, p := range wavePhases {
				waveFiles = append(waveFiles, p.TargetFiles...)
			}
			waveReport.FilesChanged = waveFiles
		}
		waveReports = append(waveReports, waveReport)

		// Prune worktrees for merged phases
		for _, p := range wavePhases {
			_ = m.worktreeMgr.Prune(ctx, p.PhaseNumber)
		}
	}

	var allFilesTouched []string
	filesSet := make(map[string]bool)
	for _, p := range m.dag.Phases {
		for _, tf := range p.TargetFiles {
			if !filesSet[tf] {
				filesSet[tf] = true
				allFilesTouched = append(allFilesTouched, tf)
			}
		}
	}
	sort.Strings(allFilesTouched)

	if len(leafHashes) == 0 {
		for _, p := range m.dag.Phases {
			h := canonical.SHA256Of(map[string]any{
				"phase":   p.PhaseNumber,
				"name":    p.Name,
				"targets": p.TargetFiles,
			})
			leafHashes = append(leafHashes, h)
		}
	}
	merkleSeal := canonical.MerkleRoot(leafHashes)

	m.lastResult = &SwarmExecutionResult{
		Status:        "SUCCESS",
		PlanPath:      m.cfg.PlanPath,
		WorkspaceRoot: m.cfg.WorkspaceRoot,
		WavesCount:    len(m.dag.Waves),
		PhasesCount:   len(m.dag.Phases),
		Waves:         waveReports,
		FilesTouched:  allFilesTouched,
		DurationMs:    time.Since(start).Milliseconds(),
		MerkleSeal:    merkleSeal,
	}

	fmt.Fprintf(outW, "\n================================================================================\n")
	fmt.Fprintf(outW, "  SWARM FORGE OUTCOME: 100%% SUCCESS\n")
	fmt.Fprintf(outW, "  Plan: %s (%d Phases Completed Across %d Waves)\n", m.cfg.PlanPath, len(m.dag.Phases), len(m.dag.Waves))
	fmt.Fprintf(outW, "================================================================================\n")
	return nil
}

// ExecutionResult returns the structured result of the last ExecuteProject run.
func (m *MasterOrchestrator) ExecutionResult() *SwarmExecutionResult {
	return m.lastResult
}

func (m *MasterOrchestrator) executeWorkerPhase(ctx context.Context, phase *engine.SwarmPhase) error {
	m.hud.UpdateWorker(ui.SwarmWorkerStatus{
		WorkerID:   phase.PhaseNumber,
		PhaseName:  phase.Name,
		Status:     "[SPAWNING]",
		MaxTurns:   m.cfg.MaxTurnsPerWorker,
		ActiveFile: strings.Join(phase.TargetFiles, ", "),
	})

	alloc, err := m.worktreeMgr.Allocate(ctx, phase.PhaseNumber, m.committer.BaseBranch())
	if err != nil {
		alloc, err = m.worktreeMgr.Allocate(ctx, phase.PhaseNumber, "HEAD")
		if err != nil {
			return fmt.Errorf("failed to allocate worktree for phase %d: %w", phase.PhaseNumber, err)
		}
	}

	m.hud.UpdateWorker(ui.SwarmWorkerStatus{
		WorkerID:   phase.PhaseNumber,
		PhaseName:  phase.Name,
		Status:     "[ACTIVE]",
		MaxTurns:   m.cfg.MaxTurnsPerWorker,
		ActiveFile: strings.Join(phase.TargetFiles, ", "),
	})

	if m.CustomWorkerRunner != nil {
		if err := m.CustomWorkerRunner(ctx, phase, alloc); err != nil {
			m.hud.UpdateWorker(ui.SwarmWorkerStatus{
				WorkerID:   phase.PhaseNumber,
				PhaseName:  phase.Name,
				Status:     "[FAILED]",
				Diagnostic: err.Error(),
			})
			return err
		}
	} else {
		workerCfg := WorkerConfig{
			ID:              phase.PhaseNumber,
			PhaseNumber:     phase.PhaseNumber,
			PhaseName:       phase.Name,
			WorktreeDir:     alloc.WorktreeDir,
			BranchName:      alloc.BranchName,
			ScopePrefix:     phase.ScopePrefix,
			TargetFiles:     phase.TargetFiles,
			VerificationCmd: phase.VerificationCmd,
			MaxTurns:        m.cfg.MaxTurnsPerWorker,
			Timeout:         m.cfg.TimeoutPerPhase,
			Model:           m.cfg.Model,
			BaseURL:         m.cfg.BaseURL,
			APIKey:          m.cfg.APIKey,
			Offline:         m.cfg.Offline,
		}

		worker, err := LaunchWorker(ctx, workerCfg, m.events)
		if err != nil {
			m.hud.UpdateWorker(ui.SwarmWorkerStatus{
				WorkerID:   phase.PhaseNumber,
				PhaseName:  phase.Name,
				Status:     "[FAILED]",
				Diagnostic: err.Error(),
			})
			return fmt.Errorf("failed to launch worker for phase %d: %w", phase.PhaseNumber, err)
		}

		m.workerMu.Lock()
		m.workers[phase.PhaseNumber] = worker
		m.workerMu.Unlock()

		m.hud.UpdateWorker(ui.SwarmWorkerStatus{
			WorkerID:   phase.PhaseNumber,
			PID:        worker.PID,
			PhaseName:  phase.Name,
			Status:     "[ACTIVE]",
			MaxTurns:   m.cfg.MaxTurnsPerWorker,
			ActiveFile: strings.Join(phase.TargetFiles, ", "),
		})

		if err := worker.Wait(); err != nil {
			diag := worker.GetDetailedDiagnostic()
			m.hud.UpdateWorker(ui.SwarmWorkerStatus{
				WorkerID:   phase.PhaseNumber,
				PID:        worker.PID,
				PhaseName:  phase.Name,
				Status:     "[FAILED]",
				Diagnostic: worker.GetLastError(),
			})
			return fmt.Errorf("worker phase %d failed: %w | %s", phase.PhaseNumber, err, diag)
		}
	}

	if !alloc.IsJunction {
		cmdAdd := exec.CommandContext(ctx, "git", "add", ".")
		cmdAdd.Dir = alloc.WorktreeDir
		_ = cmdAdd.Run()

		cmdCommit := exec.CommandContext(ctx, "git",
			"-c", "user.name=OmniCoder Swarm",
			"-c", "user.email=support@caleralabs.com",
			"commit", "--allow-empty", "-m", fmt.Sprintf("Phase %d: %s", phase.PhaseNumber, phase.Name))
		cmdCommit.Dir = alloc.WorktreeDir
		_ = cmdCommit.Run()
	} else {
		for _, tf := range phase.TargetFiles {
			src := filepath.Join(alloc.WorktreeDir, tf)
			dst := filepath.Join(m.cfg.WorkspaceRoot, tf)
			if data, rErr := os.ReadFile(src); rErr == nil {
				_ = os.MkdirAll(filepath.Dir(dst), 0755)
				_ = os.WriteFile(dst, data, 0644)
			}
		}
	}

	for _, tf := range phase.TargetFiles {
		if strings.HasSuffix(tf, ".go") {
			targetPath := filepath.Join(alloc.WorktreeDir, tf)
			if content, err := os.ReadFile(targetPath); err == nil && len(content) > 0 {
				_, _ = m.registry.ExtractAndPublish(ctx, targetPath, content)
			}
		}
	}

	m.hud.UpdateWorker(ui.SwarmWorkerStatus{
		WorkerID:   phase.PhaseNumber,
		PhaseName:  phase.Name,
		Status:     "[PASSED]",
		MaxTurns:   m.cfg.MaxTurnsPerWorker,
		ActiveFile: strings.Join(phase.TargetFiles, ", "),
	})

	return nil
}
