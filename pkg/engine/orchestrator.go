// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/storage"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// EngineConfig holds configuration and dependency injection for the master orchestrator.
type EngineConfig struct {
	WorkspaceDir     string                        `json:"workspace_dir"`
	MaxTurns         int                           `json:"max_turns"`
	Unbounded        bool                          `json:"unbounded,omitempty"`
	LatticeSpaceID   string                        `json:"lattice_space_id,omitempty"`
	MaxHealAttempts  int                           `json:"max_heal_attempts"`
	Offline          bool                          `json:"offline"`
	APIKey           string                        `json:"api_key,omitempty"`
	BaseURL          string                        `json:"base_url,omitempty"`
	Model            string                        `json:"model,omitempty"`
	TestCommand      string                        `json:"test_command,omitempty"`
	BuildCommand     string                        `json:"build_command,omitempty"`
	LLMClient        llm.LLMClient                 `json:"-"`
	DecisionGate     llm.DecisionGate              `json:"-"`
	ICXClient        icx.ICXClient                 `json:"-"`
	MCPServer        any                           `json:"-"`
	VMClient         vm.Client                     `json:"-"`
	UseSandbox       bool                          `json:"use_sandbox,omitempty"`
	Timeout          time.Duration                 `json:"timeout"`
	Verbose          bool                          `json:"verbose"`
	Vault            *storage.Vault                `json:"-"`
	TargetFile       string                        `json:"target_file,omitempty"`
	Scope            string                        `json:"scope,omitempty"`
	AuditOnly        bool                          `json:"audit_only,omitempty"`
	JSON             bool                          `json:"json,omitempty"`
	ProgressCallback func(event TurnProgressEvent) `json:"-"`
	OnEvent          func(event TurnEvent)         `json:"-"`
}

// RunConfig configures an individual task run.
type RunConfig struct {
	Task            string   `json:"task"`
	WorkspaceDir    string   `json:"workspace_dir,omitempty"`
	MaxTurns        int      `json:"max_turns,omitempty"`
	Unbounded       bool     `json:"unbounded,omitempty"`
	LatticeSpaceID  string   `json:"lattice_space_id,omitempty"`
	MaxHealAttempts int      `json:"max_heal_attempts,omitempty"`
	Offline         bool     `json:"offline,omitempty"`
	FocusFiles      []string `json:"focus_files,omitempty"`
	TestCommand     string   `json:"test_command,omitempty"`
	BuildCommand    string   `json:"build_command,omitempty"`
	UseSandbox      bool     `json:"use_sandbox,omitempty"`
	TargetFile      string   `json:"target_file,omitempty"`
	Scope           string   `json:"scope,omitempty"`
	AuditOnly       bool     `json:"audit_only,omitempty"`
	JSON            bool     `json:"json,omitempty"`
}

// RunReport captures the overall outcome of an autonomous software engineering session.
type RunReport struct {
	Success         bool          `json:"success"`
	Task            string        `json:"task"`
	TurnsCount      int           `json:"turns_count"`
	HealCount       int           `json:"heal_count"`
	FilesTouched    []string      `json:"files_touched"`
	Commits         []string      `json:"commits"`
	MerkleSeal      string        `json:"merkle_seal"`
	FinalOutput     string        `json:"final_output"`
	Duration        time.Duration `json:"duration"`
	ErrorMessage    string        `json:"error_message,omitempty"`
	TargetSymbols   []string      `json:"target_symbols,omitempty"`
	Diagnostics     any           `json:"diagnostics,omitempty"`
	PatchesProposed int           `json:"patches_proposed,omitempty"`
	ExecutionMS     int64         `json:"execution_ms,omitempty"`
}

// TurnEvent captures telemetry emitted during turn pipeline execution.
type TurnEvent struct {
	TurnNumber int       `json:"turn_number"`
	Stage      int       `json:"stage"`
	StageName  string    `json:"stage_name"`
	Message    string    `json:"message"`
	Timestamp  time.Time `json:"timestamp"`
}

// TurnProgressEvent models real-time progress callbacks for CLI and telemetry.
type TurnProgressEvent struct {
	Turn        int    `json:"turn"`
	MaxTurns    int    `json:"max_turns"`
	Stage       int    `json:"stage"`
	StageName   string `json:"stage_name"`
	Status      string `json:"status"` // "START", "STAGE", "PASS", "HEAL", "FAIL"
	Details     string `json:"details"`
	HealAttempt int    `json:"heal_attempt,omitempty"`
	MaxHeals    int    `json:"max_heals,omitempty"`
}

// Orchestrator coordinates DevOps primitives, 4D memory, Decision Gate, and MCP.
type Orchestrator struct {
	cfg          EngineConfig
	workspaceDir string
	icxClient    icx.ICXClient
	gate         llm.DecisionGate
	mcpServer    any
	pipeline     *TurnPipeline
	mu           sync.Mutex
}

// NewOrchestrator creates and initializes an Orchestrator with automatic offline fallback.
func NewOrchestrator(cfg EngineConfig) (*Orchestrator, error) {
	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir = "."
	}
	absWs, err := filepath.Abs(cfg.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("invalid workspace directory: %w", err)
	}
	cfg.WorkspaceDir = absWs

	if cfg.MaxTurns <= 0 && !cfg.Unbounded {
		cfg.MaxTurns = 10
	}
	if cfg.MaxHealAttempts <= 0 {
		cfg.MaxHealAttempts = 6
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}

	// 1. ICX Client Resolution (Automatic Offline Fallback)
	var icxClient icx.ICXClient = cfg.ICXClient
	if icxClient == nil {
		if cfg.Offline || cfg.APIKey == "" {
			icxClient = icx.NewLocalLattice()
		} else {
			c, err := icx.NewClient(icx.ClientConfig{
				BaseURL: cfg.BaseURL,
				APIKey:  cfg.APIKey,
				Offline: false,
			})
			if err != nil {
				icxClient = icx.NewLocalLattice()
			} else {
				icxClient = c
			}
		}
	}

	// 2. Decision Gate Resolution (Automatic Offline Fallback)
	var gate llm.DecisionGate = cfg.DecisionGate
	if gate == nil {
		if cfg.Offline || (cfg.APIKey == "" && cfg.LLMClient == nil) {
			gate = llm.NewMockDecisionGate()
		} else {
			llmClient := cfg.LLMClient
			if llmClient == nil {
				var err error
				llmClient, err = llm.NewClient(llm.ClientConfig{
					BaseURL: cfg.BaseURL,
					APIKey:  cfg.APIKey,
					Model:   cfg.Model,
					Timeout: cfg.Timeout,
				})
				if err != nil {
					gate = llm.NewMockDecisionGate()
				}
			}
			if gate == nil && llmClient != nil {
				gate = llm.NewDecisionGate(llmClient, cfg.Model)
			}
		}
	}

	// 3. MCP Server Resolution
	mcpServer := cfg.MCPServer

	orch := &Orchestrator{
		cfg:          cfg,
		workspaceDir: cfg.WorkspaceDir,
		icxClient:    icxClient,
		gate:         gate,
		mcpServer:    mcpServer,
	}
	orch.pipeline = &TurnPipeline{orch: orch}
	return orch, nil
}

// ExecuteTask runs the multi-turn autonomous software engineering loop.
func (o *Orchestrator) ExecuteTask(ctx context.Context, runCfg RunConfig) (*RunReport, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	start := time.Now()

	// Clone config to allow per-run overrides
	runEngineCfg := o.cfg
	if runCfg.WorkspaceDir != "" {
		if absWs, err := filepath.Abs(runCfg.WorkspaceDir); err == nil {
			runEngineCfg.WorkspaceDir = absWs
		}
	}
	if runCfg.MaxTurns > 0 {
		runEngineCfg.MaxTurns = runCfg.MaxTurns
	}
	if runCfg.MaxHealAttempts > 0 {
		runEngineCfg.MaxHealAttempts = runCfg.MaxHealAttempts
	}
	if runCfg.TestCommand != "" {
		runEngineCfg.TestCommand = runCfg.TestCommand
	}
	if runCfg.BuildCommand != "" {
		runEngineCfg.BuildCommand = runCfg.BuildCommand
	}
	if runCfg.Unbounded {
		runEngineCfg.Unbounded = true
	}
	if runCfg.LatticeSpaceID != "" {
		runEngineCfg.LatticeSpaceID = runCfg.LatticeSpaceID
	}
	if runCfg.TargetFile != "" {
		runEngineCfg.TargetFile = runCfg.TargetFile
	}
	if runCfg.Scope != "" {
		runEngineCfg.Scope = runCfg.Scope
	}
	if runCfg.AuditOnly {
		runEngineCfg.AuditOnly = true
	}
	if runCfg.JSON {
		runEngineCfg.JSON = true
	}

	if runEngineCfg.Vault == nil && runEngineCfg.WorkspaceDir != "" {
		if v, err := storage.OpenVault(runEngineCfg.WorkspaceDir); err == nil {
			runEngineCfg.Vault = v
		}
	}

	state := NewEngineState(runCfg.Task, runEngineCfg)
	defer state.Close()
	focusFiles := runCfg.FocusFiles

	// Fast pre-flight check: If a structured plan is already 100% complete and passing verification, succeed immediately!
	plan := DetectPlanSpec(runCfg.Task, runEngineCfg.WorkspaceDir)
	if plan != nil && plan.IsComplete(runEngineCfg.WorkspaceDir) {
		projDir := filepath.Join(runEngineCfg.WorkspaceDir, filepath.FromSlash(plan.ProjectScope))
		if _, err := os.Stat(filepath.Join(projDir, "go.mod")); err == nil {
			report, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
				Command:      "go test ./...",
				WorkspaceDir: projDir,
			})
			if err == nil && report != nil && report.Success && report.ExitCode == 0 {
				state.Status = "COMPLETED"
				finalReport := state.BuildReport(true, fmt.Sprintf("All %d phases of %s plan verified complete (all files exist and tests pass)", len(plan.Phases), plan.ModuleName))
				finalReport.Duration = time.Since(start)
				return finalReport, nil
			}
		}
	}

	isUnbounded := runEngineCfg.Unbounded || runEngineCfg.MaxTurns <= 0
	for isUnbounded || state.CurrentTurn < state.MaxTurns {
		select {
		case <-ctx.Done():
			report := state.BuildReport(false, "execution cancelled: "+ctx.Err().Error())
			report.Duration = time.Since(start)
			return report, ctx.Err()
		default:
		}

		turn := state.BeginTurn(runCfg.Task, focusFiles)
		err := o.pipeline.ExecuteTurn(ctx, state, turn, runEngineCfg)
		state.CompleteTurn(turn)

		if err != nil {
			report := state.BuildReport(false, fmt.Sprintf("turn %d failed: %v", turn.TurnNumber, err))
			report.Duration = time.Since(start)
			return report, err
		}

		// Update focusFiles for subsequent turns based on target files touched
		if len(turn.TargetFiles) > 0 {
			focusFiles = turn.TargetFiles
		}

		// If finalized and approved, check if multi-phase plan is completely finished
		if turn.Finalize != nil && turn.Finalize.Approved {
			plan := DetectPlanSpec(runCfg.Task, runEngineCfg.WorkspaceDir)
			if plan != nil && !plan.IsComplete(runEngineCfg.WorkspaceDir) {
				continue
			}
			break
		}

		// If no files touched and turn completed cleanly, stop unless multi-phase plan in progress
		if len(turn.TargetFiles) == 0 && turn.Success {
			plan := DetectPlanSpec(runCfg.Task, runEngineCfg.WorkspaceDir)
			if plan != nil && !plan.IsComplete(runEngineCfg.WorkspaceDir) {
				continue
			}
			break
		}
	}

	finalSuccess := false
	if len(state.Turns) > 0 {
		lastTurn := state.Turns[len(state.Turns)-1]
		finalSuccess = lastTurn.Success
	}

	finalMsg := "task completed successfully"
	if !finalSuccess {
		if len(state.Turns) > 0 && state.Turns[len(state.Turns)-1].ErrorMessage != "" {
			finalMsg = state.Turns[len(state.Turns)-1].ErrorMessage
		} else {
			finalMsg = "task execution halted without approval"
		}
	}

	report := state.BuildReport(finalSuccess, finalMsg)
	report.Duration = time.Since(start)
	return report, nil
}

// Run executes a task string using default configuration.
func (o *Orchestrator) Run(ctx context.Context, task string) (*RunReport, error) {
	return o.ExecuteTask(ctx, RunConfig{Task: task})
}

// Map synthesizes and returns the PageRank repository map bounded to tokenBudget (<1024 tokens).
func (o *Orchestrator) Map(ctx context.Context, tokenBudget int) (*devops.RepoMapResult, error) {
	return o.MapWithQuery(ctx, tokenBudget, "")
}

// MapWithQuery synthesizes and returns the PageRank repository map prioritizing query-matching files.
func (o *Orchestrator) MapWithQuery(ctx context.Context, tokenBudget int, query string) (*devops.RepoMapResult, error) {
	if tokenBudget <= 0 {
		tokenBudget = 1024
	}
	return devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: o.workspaceDir,
		TokenBudget:  tokenBudget,
		Query:        query,
	})
}

// Heal executes target command, intercepts errors, and loops self-healing until exit code 0.
func (o *Orchestrator) Heal(ctx context.Context, command string) (*RunReport, error) {
	return o.ExecuteTask(ctx, RunConfig{
		Task:        "Fix compiler errors for command: " + command,
		TestCommand: command,
	})
}

// Refactor coordinates AST blast-radius walk and signature refactoring.
func (o *Orchestrator) Refactor(ctx context.Context, symbol string, newSig string) (*RunReport, error) {
	taskPrompt := fmt.Sprintf("Refactor symbol `%s` to new signature `%s` across all callers", symbol, newSig)
	return o.ExecuteTask(ctx, RunConfig{
		Task: taskPrompt,
	})
}

// Close releases any underlying client resources.
func (o *Orchestrator) Close() error {
	if o.icxClient != nil {
		return o.icxClient.Close()
	}
	return nil
}

// ICX returns the connected ICXClient instance.
func (o *Orchestrator) ICX() icx.ICXClient {
	if o == nil {
		return nil
	}
	return o.icxClient
}

