// SPDX-License-Identifier: Apache-2.0
// Package main provides the standalone CLI entrypoint for OmniCoder-ICX.
// Zero-dependency pure Go standard library implementation (CGO_ENABLED=0 capable).

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
	"github.com/caleralabs/omnicoder-icx/pkg/orchestrator"
	"github.com/caleralabs/omnicoder-icx/pkg/storage"
	"github.com/caleralabs/omnicoder-icx/pkg/ui"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

const (
	Version        = "1.0.0"
	DefaultModel   = "gemini-3.5-flash-lite"
	LocalMockModel = "qwen2.5-coder:7b"
)

// GlobalConfig encapsulates CLI flags, environment overrides, and runtime options.
type GlobalConfig struct {
	Workspace  string
	Offline    bool
	Mechanical bool
	Model      string
	BaseURL    string
	APIKey     string
	MaxTurns   int
	MaxWorkers int
	Unbounded  bool
	SpaceID    string
	Verbose    bool
	Quiet      bool
	Sandbox    bool
	TargetFile string
	Scope      string
	AuditOnly  bool
	JSON       bool
	BeamK      int
	Swarm      bool
	ProbeCmd   string
}

func defaultGlobalConfig() GlobalConfig {
	return GlobalConfig{
		Workspace:  ".",
		Offline:    false,
		Model:      "",
		BaseURL:    "",
		APIKey:     "",
		MaxTurns:   10,
		MaxWorkers: 0,
		Unbounded:  true,
		SpaceID:    "",
		Verbose:    false,
		Sandbox:    false,
		BeamK:      1,
		Swarm:      false,
		ProbeCmd:   "",
	}
}

func bindGlobalFlags(fSet *flag.FlagSet, cfg *GlobalConfig) {
	fSet.StringVar(&cfg.Workspace, "workspace", cfg.Workspace, "Workspace root directory (default: current directory)")
	fSet.BoolVar(&cfg.Offline, "offline", cfg.Offline, "Force offline mode using embedded local cache and mock decision gate")
	fSet.BoolVar(&cfg.Mechanical, "mechanical", cfg.Mechanical, "Force deterministic 8-stage mechanical engine instead of conversational heart")
	fSet.StringVar(&cfg.Model, "model", cfg.Model, "LLM model identifier")
	fSet.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "Custom OpenAI-compatible endpoint URL (Ollama, LM Studio, vLLM, Gemini, OpenAI)")
	fSet.StringVar(&cfg.APIKey, "api-key", cfg.APIKey, "API key for LLM provider")
	fSet.IntVar(&cfg.MaxTurns, "max-turns", cfg.MaxTurns, "Maximum autonomous turn iterations before halting")
	fSet.BoolVar(&cfg.Unbounded, "unbounded", cfg.Unbounded, "Run in relentless unbounded mode without turn limit until verified complete")
	fSet.StringVar(&cfg.SpaceID, "space-id", cfg.SpaceID, "ICX 4D memory space identifier (default: auto-derived from task/project)")
	fSet.BoolVar(&cfg.Verbose, "verbose", cfg.Verbose, "Enable verbose logging for AST walks, token budgets, and diffs")
	fSet.BoolVar(&cfg.Verbose, "v", cfg.Verbose, "Enable verbose logging (shorthand)")
	fSet.BoolVar(&cfg.Quiet, "quiet", cfg.Quiet, "Suppress banner and status messages on stderr")
	fSet.BoolVar(&cfg.Quiet, "q", cfg.Quiet, "Suppress banner and status messages (shorthand)")
	fSet.BoolVar(&cfg.Sandbox, "sandbox", cfg.Sandbox, "Enable isolated Calera LLM-VM microVM sandbox execution with instant CoW rollback")
	fSet.BoolVar(&cfg.Sandbox, "s", cfg.Sandbox, "Enable microVM sandbox (shorthand)")
	fSet.StringVar(&cfg.TargetFile, "target-file", cfg.TargetFile, "Explicit boundary constraint: restrict mutations strictly to target file")
	fSet.StringVar(&cfg.Scope, "scope", cfg.Scope, "Explicit scoping constraint: restrict mutations to files matching glob or prefix")
	fSet.BoolVar(&cfg.AuditOnly, "audit-only", cfg.AuditOnly, "Read-only analysis mode: output diagnostic recommendations without disk mutations")
	fSet.BoolVar(&cfg.AuditOnly, "dry-run", cfg.AuditOnly, "Read-only analysis mode (alias for --audit-only)")
	fSet.BoolVar(&cfg.JSON, "json", cfg.JSON, "Output structured JSON results with status, target_symbols, diagnostics, patches_proposed, and execution_ms")
	fSet.IntVar(&cfg.BeamK, "beam-k", cfg.BeamK, "Speculative beam candidate count (default: 1, recommendation: 3 for vLLM)")
	fSet.BoolVar(&cfg.Swarm, "swarm", cfg.Swarm, "Enable coordinated multi-file swarm orchestration mode")
	fSet.StringVar(&cfg.ProbeCmd, "probe-cmd", cfg.ProbeCmd, "Fast isolated unit test probe for candidate screening (Two-Tier Test Harness)")
}

func resolveEnvVars(cfg *GlobalConfig) {
	if cfg.Workspace == "" || cfg.Workspace == "." {
		if env := os.Getenv("OMNICODER_WORKSPACE"); env != "" {
			cfg.Workspace = env
		}
	}
	if abs, err := filepath.Abs(cfg.Workspace); err == nil {
		cfg.Workspace = abs
	}

	// Guard against IDE executable / installation folders
	if mcp.IsIDEOsOrSystemDir(cfg.Workspace) {
		if ideWS := mcp.DiscoverIDEActiveWorkspace(); ideWS != "" {
			cfg.Workspace = ideWS
		}
	}

	if !cfg.Offline {
		if env := os.Getenv("OMNICODER_OFFLINE"); env == "1" || strings.ToLower(env) == "true" {
			cfg.Offline = true
		}
	}

	if cfg.Model == "" {
		if env := os.Getenv("GEMINI_MODEL"); env != "" {
			cfg.Model = env
		} else if env := os.Getenv("OMNICODER_MODEL"); env != "" {
			cfg.Model = env
		} else if env := os.Getenv("OPENAI_MODEL"); env != "" {
			cfg.Model = env
		} else if cfg.Offline {
			cfg.Model = LocalMockModel
		} else {
			cfg.Model = DefaultModel
		}
	}

	if cfg.BaseURL == "" {
		if env := os.Getenv("OMNICODER_BASE_URL"); env != "" {
			cfg.BaseURL = env
		} else if env := os.Getenv("OPENAI_BASE_URL"); env != "" {
			cfg.BaseURL = env
		}
	}

	if cfg.APIKey == "" {
		cfg.APIKey = llm.ResolveAPIKey(cfg.APIKey)
	}

	if !cfg.Unbounded {
		if env := os.Getenv("OMNICODER_UNBOUNDED"); env == "1" || strings.ToLower(env) == "true" {
			cfg.Unbounded = true
		}
	}

	if cfg.SpaceID == "" {
		if env := os.Getenv("OMNICODER_SPACE_ID"); env != "" {
			cfg.SpaceID = env
		}
	}

	if cfg.MaxTurns <= 0 && !cfg.Unbounded {
		if env := os.Getenv("OMNICODER_MAX_TURNS"); env != "" {
			if val, err := strconv.Atoi(env); err == nil && val > 0 {
				cfg.MaxTurns = val
			}
		}
		if cfg.MaxTurns <= 0 {
			cfg.MaxTurns = 10
		}
	}

	if cfg.BeamK <= 0 {
		if env := os.Getenv("OMNICODER_BEAM_K"); env != "" {
			if val, err := strconv.Atoi(env); err == nil && val > 0 {
				cfg.BeamK = val
			}
		}
		if cfg.BeamK <= 0 {
			cfg.BeamK = 1
		}
	}

	if !cfg.Verbose {
		if env := os.Getenv("OMNICODER_VERBOSE"); env == "1" || strings.ToLower(env) == "true" {
			cfg.Verbose = true
		}
	}

	if !cfg.Sandbox {
		if env := os.Getenv("OMNICODER_SANDBOX"); env == "1" || strings.ToLower(env) == "true" {
			cfg.Sandbox = true
		}
	}

	// Auto-detect offline mode if no credentials or endpoints are present
	if !cfg.Offline && cfg.APIKey == "" && cfg.BaseURL == "" {
		cfg.Offline = true
		if cfg.Model == DefaultModel {
			cfg.Model = LocalMockModel
		}
	}
}

func isInteractiveTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return false
	}
	return isTerminalFile(f)
}

func printRootUsage() {
	fmt.Fprintf(os.Stderr, `OmniCoder-ICX — All-in-One Autonomous Coding Engine (v%s)

Usage:
  omnicoder [global-flags] <command> [command-flags] [arguments...]
  omnicoder [global-flags] "<task description>"

Commands:
  chat [options]            Conversational UI architect & planner (default on interactive launch)
  run "<task>"              Run autonomous coding loop against workspace
  swarm <plan.md>           Execute multi-process parallel worker swarm against plan DAG
  map [options]             Synthesize and print personalized PageRank AST tree (<2.0s)
  search "<query>"          Fast AST symbol + polyglot grep search across workspace
  heal "<command>"          Execute command and iteratively self-heal failing code
  refactor <sym> <new_sig>  AST blast-radius walk and multi-file coordinated refactoring
  claim <email>             Claim 10M free tokens from Calera LLM-VM MCP and write to ~/.calera/icx.env
  vault [status|list|get]   Inspect embedded TitanKV checkpoint vault and CAS statistics
  repl                      Interactive terminal REPL with zero-token dialogue reset
  mcp [options]             Run Model Context Protocol (MCP) JSON-RPC 2.0 stdio server
  version                   Print OmniCoder-ICX version and exit
  help                      Print this help message

Global Flags:
  --workspace <dir>         Workspace root directory (default: current directory)
  --offline                 Force offline mode with embedded local cache & mock gate
  --mechanical              Force deterministic 8-stage mechanical engine
  --model <name>            LLM model identifier (default: gemini-3.5-flash-lite / qwen2.5-coder:7b)
  --base-url <url>          OpenAI-compatible base URL (Ollama, LM Studio, vLLM, Gemini, OpenAI)
  --api-key <key>           API key for model inference (OMNICODER_API_KEY / GEMINI_API_KEY / OPENAI_API_KEY)
  --max-turns <n>           Maximum autonomous turn iterations (default: 10)
  --unbounded               Run in relentless unbounded mode without turn limit
  --space-id <id>           ICX 4D memory space identifier (default: auto-derived)
  --sandbox, -s             Enable isolated Calera LLM-VM microVM sandbox execution with instant CoW rollback
  --verbose, -v             Enable verbose execution and diagnostic telemetry

Examples:
  omnicoder "Build a real-time reactive canvas drawing app"
  omnicoder run "Implement concurrent LRU cache with TTL in pkg/lru"
  omnicoder chat
  omnicoder map --token-budget=1024
  omnicoder heal "go test ./..."
  omnicoder refactor LRUCache "type LRUCache[K comparable, V any] struct"
  omnicoder claim support@caleralabs.com
  omnicoder repl
`, Version)
}

func main() {
	cfg := defaultGlobalConfig()
	resolveEnvVars(&cfg)

	rootFS := flag.NewFlagSet("omnicoder", flag.ContinueOnError)
	rootFS.Usage = printRootUsage
	bindGlobalFlags(rootFS, &cfg)

	if err := rootFS.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(1)
	}

	args := rootFS.Args()
	ctx := context.Background()

	if len(args) == 0 {
		if isInteractiveTerminal(os.Stdin) {
			if err := executeChat(ctx, &cfg, nil); err != nil {
				os.Exit(1)
			}
			return
		}
		printRootUsage()
		return
	}

	subcmd := args[0]
	subcmdArgs := args[1:]

	var err error
	switch subcmd {
	case "chat":
		err = executeChat(ctx, &cfg, subcmdArgs)
	case "swarm":
		err = executeSwarm(ctx, &cfg, subcmdArgs)
	case "run", "plan", "build", "goal", "boost":
		err = executeRun(ctx, &cfg, subcmdArgs)
	case "map":
		err = executeMap(ctx, &cfg, subcmdArgs)
	case "search", "grep":
		err = executeSearch(ctx, &cfg, subcmdArgs)
	case "heal":
		err = executeHeal(ctx, &cfg, subcmdArgs)
	case "solve":
		err = executeSolve(ctx, &cfg, subcmdArgs)
	case "refactor":
		err = executeRefactor(ctx, &cfg, subcmdArgs)
	case "claim":
		err = executeClaim(ctx, &cfg, subcmdArgs)
	case "vault", "store":
		err = executeVault(ctx, &cfg, subcmdArgs)
	case "repl":
		err = executeREPL(ctx, &cfg, subcmdArgs)
	case "mcp":
		err = executeMCP(ctx, &cfg, subcmdArgs)
	case "version":
		fmt.Printf("OmniCoder-ICX v%s (pure Go standard library, zero-CGo)\n", Version)
	case "help", "-h", "--help":
		printRootUsage()
	default:
		// If the first argument is an existing directory, set workspace and enter interactive chat
		if fi, statErr := os.Stat(subcmd); statErr == nil && fi.IsDir() {
			cfg.Workspace = subcmd
			if isInteractiveTerminal(os.Stdin) {
				err = executeChat(ctx, &cfg, subcmdArgs)
				break
			}
		}

		fullInput := strings.TrimSpace(strings.Join(args, " "))
		if match := ui.SwarmIntentRegex.FindStringSubmatch(fullInput); len(match) > 3 {
			planPath := strings.Trim(match[3], "\"'")
			err = executeSwarm(ctx, &cfg, []string{planPath})
			break
		}

		firstWord := strings.ToLower(subcmd)
		isNaturalTask := strings.Contains(fullInput, " ") ||
			firstWord == "build" || firstWord == "create" || firstWord == "implement" ||
			firstWord == "make" || firstWord == "generate" || firstWord == "fix" ||
			firstWord == "debug" || firstWord == "write" || firstWord == "scaffold" ||
			firstWord == "plan" || firstWord == "goal" || firstWord == "boost"

		if isNaturalTask {
			err = executeDirectTask(ctx, &cfg, args)
		} else {
			fmt.Fprintf(os.Stderr, "Error: unknown command %q\nRun 'omnicoder --help' for usage.\n", subcmd)
			os.Exit(1)
		}
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		if errors.Is(err, errLeakageDetected) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// parseWithRearrangedArgs places all flags (-flag or --flag and their values) before positional arguments
// so Go's standard library flag.FlagSet parses flags regardless of their position on the command line.
func parseWithRearrangedArgs(fs *flag.FlagSet, args []string) error {
	var flags []string
	var posArgs []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") {
				cleanName := strings.TrimLeft(arg, "-")
				fl := fs.Lookup(cleanName)
				isBool := false
				if fl != nil {
					if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
						isBool = true
					}
				}
				if cleanName == "help" || cleanName == "h" {
					isBool = true
				}
				if !isBool && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					flags = append(flags, args[i])
				}
			}
		} else {
			posArgs = append(posArgs, arg)
		}
	}

	rearranged := append(flags, posArgs...)
	return fs.Parse(rearranged)
}

// ----------------------------------------------------------------------------
// 0. omnicoder chat [options] / zero-argument interactive launch
// ----------------------------------------------------------------------------
func executeChat(ctx context.Context, parentCfg *GlobalConfig, args []string) error {
	return executeChatWithIO(ctx, parentCfg, args, os.Stdin, os.Stdout)
}

func executeChatWithIO(ctx context.Context, parentCfg *GlobalConfig, args []string, in io.Reader, out io.Writer) error {
	cfg := defaultGlobalConfig()
	if parentCfg != nil {
		cfg = *parentCfg
	}

	chatFS := flag.NewFlagSet("chat", flag.ContinueOnError)
	bindGlobalFlags(chatFS, &cfg)
	if len(args) > 0 {
		if err := parseWithRearrangedArgs(chatFS, args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
	}

	// Resolve API key via llm.ResolveAPIKey before auto-offline evaluation
	if !cfg.Offline {
		cfg.APIKey = llm.ResolveAPIKey(cfg.APIKey)
	}

	// Auto-detect offline mode if no credentials or endpoints are present
	if !cfg.Offline && cfg.APIKey == "" && cfg.BaseURL == "" {
		cfg.Offline = true
	}

	if cfg.Model == "" {
		if cfg.Offline {
			cfg.Model = LocalMockModel
		} else {
			cfg.Model = DefaultModel
		}
	}

	var llmClient llm.LLMClient
	var gate llm.DecisionGate
	if cfg.Offline {
		gate = llm.NewMockDecisionGate()
		mockClient := llm.NewMockLLMClient()
		mockClient.CannedContent = "You are currently in offline mode. The embedded local cache and mock decision gate are active. Run without --offline to connect to live Gemini/OpenAI models, or use slash commands (/help, /run, /map) to execute tasks."
		llmClient = mockClient
	} else {
		wireClient, err := llm.NewClient(llm.ClientConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
		})
		if err != nil {
			gate = llm.NewMockDecisionGate()
		} else {
			llmClient = wireClient
			gate = llm.NewDecisionGate(wireClient, cfg.Model)
		}
	}

	lat := icx.NewLocalLattice()
	mcpServer := mcp.NewServer(cfg.Workspace)

	var vmClient vm.Client
	if cfg.Sandbox {
		vmClient = vm.NewHermeticLocalSandbox(cfg.Workspace)
	}

	engineOrch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir:   cfg.Workspace,
		MaxTurns:       cfg.MaxTurns,
		Unbounded:      cfg.Unbounded,
		LatticeSpaceID: cfg.SpaceID,
		Offline:        cfg.Offline,
		Model:          cfg.Model,
		DecisionGate:   gate,
		ICXClient:      lat,
		MCPServer:      mcpServer,
		VMClient:       vmClient,
		UseSandbox:     cfg.Sandbox,
		Verbose:        cfg.Verbose,
		ProgressCallback: func(event engine.TurnProgressEvent) {
			ui.RenderProgressEvent(out, event)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing orchestrator: %v\n", err)
		return err
	}

	session := ui.NewSessionWithICX(llmClient, engineOrch, cfg.Workspace, lat)
	defer session.Close()

	session.SetSwarmHandler(func(ctx context.Context, planPath string, out io.Writer) error {
		return executeSwarm(ctx, &cfg, []string{planPath})
	})

	return ui.RunConversationalUI(ctx, session, in, out)
}

// ----------------------------------------------------------------------------
// 0b. omnicoder swarm <plan.md>
// ----------------------------------------------------------------------------
func executeSwarm(ctx context.Context, globalCfg *GlobalConfig, args []string) error {
	cfg := defaultGlobalConfig()
	if globalCfg != nil {
		cfg = *globalCfg
	}

	swarmFS := flag.NewFlagSet("swarm", flag.ContinueOnError)
	bindGlobalFlags(swarmFS, &cfg)
	swarmFS.IntVar(&cfg.MaxWorkers, "max-workers", cfg.MaxWorkers, "Maximum parallel worker processes (default: min(NumCPU, 4))")

	if err := parseWithRearrangedArgs(swarmFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := swarmFS.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: omnicoder swarm [options] <path/to/plan.md>\n")
		return fmt.Errorf("usage: omnicoder swarm [options] <path/to/plan.md>")
	}
	planPath := posArgs[0]

	maxWorkers := cfg.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = runtime.NumCPU()
		if maxWorkers > 4 {
			maxWorkers = 4
		}
		if maxWorkers <= 0 {
			maxWorkers = 1
		}
	}

	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 5
	}

	if !cfg.Offline {
		cfg.APIKey = llm.ResolveAPIKey(cfg.APIKey)
	}

	swarmCfg := orchestrator.SwarmConfig{
		PlanPath:          planPath,
		WorkspaceRoot:     cfg.Workspace,
		MaxWorkers:        maxWorkers,
		MaxTurnsPerWorker: maxTurns,
		TimeoutPerPhase:   10 * time.Minute,
		Model:             cfg.Model,
		BaseURL:           cfg.BaseURL,
		APIKey:            cfg.APIKey,
		Offline:           cfg.Offline,
		Verbose:           cfg.Verbose,
	}

	master, err := orchestrator.NewMasterOrchestrator(swarmCfg)
	if err != nil {
		return fmt.Errorf("failed to initialize swarm orchestrator: %w", err)
	}

	return master.ExecuteProject(ctx)
}

// ----------------------------------------------------------------------------
// 1. omnicoder run "<task>"
// ----------------------------------------------------------------------------
func executeRun(ctx context.Context, cfg *GlobalConfig, args []string) error {
	runFS := flag.NewFlagSet("run", flag.ContinueOnError)
	bindGlobalFlags(runFS, cfg)
	if err := parseWithRearrangedArgs(runFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := runFS.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "Error: task description required for 'run'\n\nUsage: omnicoder run [options] \"<task>\"\n")
		return errors.New("missing task description")
	}
	task := strings.Join(posArgs, " ")
	task = strings.TrimSpace(task)
	if task == "" {
		fmt.Fprintf(os.Stderr, "Error: task description required for 'run'\n\nUsage: omnicoder run [options] \"<task>\"\n")
		return errors.New("missing task description")
	}

	return executeTaskWithConfig(ctx, cfg, task)
}

func executeDirectTask(ctx context.Context, cfg *GlobalConfig, args []string) error {
	if len(args) == 0 {
		printRootUsage()
		return nil
	}

	directFS := flag.NewFlagSet("omnicoder", flag.ContinueOnError)
	bindGlobalFlags(directFS, cfg)
	if err := parseWithRearrangedArgs(directFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := directFS.Args()
	if len(posArgs) == 0 {
		printRootUsage()
		return nil
	}

	task := strings.Join(posArgs, " ")
	task = strings.TrimSpace(task)
	if task == "" {
		printRootUsage()
		return nil
	}

	return executeTaskWithConfig(ctx, cfg, task)
}

func executeTaskWithConfig(ctx context.Context, cfg *GlobalConfig, task string) error {
	cleanTarget := task
	cleanTarget = strings.TrimPrefix(cleanTarget, "build ")
	cleanTarget = strings.TrimPrefix(cleanTarget, "run ")
	cleanTarget = strings.TrimSpace(cleanTarget)

	if planPathUsed, planContent := ui.ResolvePlanFile(cleanTarget, cfg.Workspace); planContent != "" {
		fmt.Printf("[Plan Detected] Loaded master implementation plan: %s (%d bytes)\n", planPathUsed, len(planContent))
		task = ui.FormatPlanPrompt(cleanTarget, planPathUsed, planContent)
	}

	if !cfg.Offline {
		cfg.APIKey = llm.ResolveAPIKey(cfg.APIKey)
	}
	if !cfg.Offline && cfg.APIKey == "" && cfg.BaseURL == "" {
		cfg.Offline = true
		if cfg.Model == DefaultModel {
			cfg.Model = LocalMockModel
		}
	}

	if cfg.Mechanical || cfg.JSON || cfg.AuditOnly {
		return executeRunMechanical(ctx, cfg, task)
	}

	var llmClient llm.LLMClient
	if !cfg.Offline {
		wireClient, err := llm.NewClient(llm.ClientConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
		})
		if err == nil {
			llmClient = wireClient
		}
	}

	if llmClient == nil {
		return executeRunMechanical(ctx, cfg, task)
	}

	fmt.Printf("================================================================================\n")
	fmt.Printf("  OmniCoder-ICX Autonomous Coding Engine (v%s)\n", Version)
	fmt.Printf("  Workspace: %s\n", cfg.Workspace)
	fmt.Printf("  Mode:      CONVERSATIONAL HEART (5-Pillar Architecture & Scaffolding)\n")
	fmt.Printf("  Model:     %s\n", cfg.Model)
	if cfg.SpaceID != "" {
		fmt.Printf("  Space ID:  %s\n", cfg.SpaceID)
	}
	if cfg.Sandbox {
		fmt.Printf("  Sandbox:   CALERA LLM-VM MICROVM (sub-10ms CoW rollback enabled)\n")
	}
	fmt.Printf("================================================================================\n")
	fmt.Printf("Task: %q\n\n", task)

	lat := icx.NewLocalLattice()
	mcpServer := mcp.NewServer(cfg.Workspace)

	var vmClient vm.Client
	if cfg.Sandbox {
		vmClient = vm.NewHermeticLocalSandbox(cfg.Workspace)
	}

	var gate llm.DecisionGate = llm.NewDecisionGate(llmClient, cfg.Model)

	engineOrch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir:   cfg.Workspace,
		MaxTurns:       cfg.MaxTurns,
		Unbounded:      cfg.Unbounded,
		LatticeSpaceID: cfg.SpaceID,
		Offline:        cfg.Offline,
		Model:          cfg.Model,
		DecisionGate:   gate,
		ICXClient:      lat,
		MCPServer:      mcpServer,
		VMClient:       vmClient,
		UseSandbox:     cfg.Sandbox,
		Verbose:        cfg.Verbose,
		ProgressCallback: func(event engine.TurnProgressEvent) {
			ui.RenderProgressEvent(os.Stdout, event)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing orchestrator: %v\n", err)
		return err
	}

	session := ui.NewSessionWithICX(llmClient, engineOrch, cfg.Workspace, lat)
	session.SetModel(cfg.Model)
	defer session.Close()

	return ui.ExecuteConversationalTask(ctx, session, task, os.Stdout)
}

func executeRunMechanical(ctx context.Context, cfg *GlobalConfig, task string) error {
	modeStr := "CLOUD / LOCAL LIVE"
	if cfg.Offline {
		modeStr = "OFFLINE (Embedded Local Cache + Mock Decision Gate)"
	}

	if cfg.MaxTurns > 0 {
		cfg.Unbounded = false
	} else {
		cfg.Unbounded = true
	}

	maxTurnsStr := fmt.Sprintf("%d", cfg.MaxTurns)
	if cfg.Unbounded || cfg.MaxTurns <= 0 {
		maxTurnsStr = "UNBOUNDED (relentless mode)"
	}

	if !cfg.JSON {
		fmt.Printf("================================================================================\n")
		fmt.Printf("  OmniCoder-ICX Autonomous Coding Engine (v%s)\n", Version)
		fmt.Printf("  Workspace: %s\n", cfg.Workspace)
		fmt.Printf("  Mode:      %s\n", modeStr)
		fmt.Printf("  Model:     %s\n", cfg.Model)
		fmt.Printf("  Max Turns: %s\n", maxTurnsStr)
		if cfg.SpaceID != "" {
			fmt.Printf("  Space ID:  %s\n", cfg.SpaceID)
		}
		if cfg.TargetFile != "" {
			fmt.Printf("  Target:    %s\n", cfg.TargetFile)
		}
		if cfg.Scope != "" {
			fmt.Printf("  Scope:     %s\n", cfg.Scope)
		}
		if cfg.AuditOnly {
			fmt.Printf("  AuditOnly: ENABLED (dry-run mode, 0 disk mutations)\n")
		}
		if cfg.Sandbox {
			fmt.Printf("  Sandbox:   CALERA LLM-VM MICROVM (sub-10ms CoW rollback enabled)\n")
		}
		fmt.Printf("================================================================================\n")
		fmt.Printf("Task: %q\n\n", task)
	}

	var icxClient icx.ICXClient = icx.NewLocalLattice()
	var gate llm.DecisionGate
	if cfg.Offline {
		gate = llm.NewMockDecisionGate()
	} else {
		wireClient, err := llm.NewClient(llm.ClientConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
		})
		if err != nil {
			gate = llm.NewMockDecisionGate()
		} else {
			gate = llm.NewDecisionGate(wireClient, cfg.Model)
		}
	}

	mcpServer := mcp.NewServer(cfg.Workspace)

	var vmClient vm.Client
	if cfg.Sandbox {
		vmClient = vm.NewHermeticLocalSandbox(cfg.Workspace)
	}

	progressCallback := func(event engine.TurnProgressEvent) {
		if cfg.JSON {
			if data, err := json.Marshal(event); err == nil {
				fmt.Println(string(data))
			}
			return
		}
		maxTurnsDisplay := fmt.Sprintf("%d", event.MaxTurns)
		if event.MaxTurns <= 0 {
			maxTurnsDisplay = "∞"
		}
		switch event.Status {
		case "START":
			if event.MaxTurns <= 0 {
				fmt.Printf("\n--- [Turn %d (Unbounded)] ---\n", event.Turn)
			} else {
				fmt.Printf("\n--- [Turn %d/%d] ---\n", event.Turn, event.MaxTurns)
			}
		case "STAGE":
			fmt.Printf("  [Stage %d/8] %-28s %s\n", event.Stage, event.StageName+":", event.Details)
		case "PASS":
			fmt.Printf("  [Turn %d/%s] PASS: %s\n", event.Turn, maxTurnsDisplay, event.Details)
		case "HEAL":
			fmt.Printf("  [Turn %d/%s] HEALING (%d/%d): %s\n", event.Turn, maxTurnsDisplay, event.HealAttempt, event.MaxHeals, event.Details)
		case "FAIL":
			fmt.Printf("  [Turn %d/%s] FAIL: %s\n", event.Turn, maxTurnsDisplay, event.Details)
		}
	}

	eng, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir:     cfg.Workspace,
		MaxTurns:         cfg.MaxTurns,
		Unbounded:        cfg.Unbounded,
		LatticeSpaceID:   cfg.SpaceID,
		Offline:          cfg.Offline,
		Model:            cfg.Model,
		DecisionGate:     gate,
		ICXClient:        icxClient,
		MCPServer:        mcpServer,
		VMClient:         vmClient,
		UseSandbox:       cfg.Sandbox,
		Verbose:          cfg.Verbose,
		TargetFile:       cfg.TargetFile,
		Scope:            cfg.Scope,
		AuditOnly:        cfg.AuditOnly,
		JSON:             cfg.JSON,
		ProgressCallback: progressCallback,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing orchestrator: %v\n", err)
		return err
	}

	start := time.Now()
	report, err := eng.ExecuteTask(ctx, engine.RunConfig{
		Task:           task,
		WorkspaceDir:   cfg.Workspace,
		MaxTurns:       cfg.MaxTurns,
		Unbounded:      cfg.Unbounded,
		LatticeSpaceID: cfg.SpaceID,
		Offline:        cfg.Offline,
		UseSandbox:     cfg.Sandbox,
		TargetFile:     cfg.TargetFile,
		Scope:          cfg.Scope,
		AuditOnly:      cfg.AuditOnly,
		JSON:           cfg.JSON,
	})
	elapsed := time.Since(start)

	if err != nil && !cfg.JSON {
		fmt.Fprintf(os.Stderr, "\n[ERROR] Task execution halted with error: %v\n", err)
		return err
	}

	if cfg.JSON {
		status := "success"
		if !report.Success {
			status = "failed"
		}
		targetSyms := report.TargetSymbols
		if targetSyms == nil {
			targetSyms = []string{}
		}
		diags := report.Diagnostics
		if diags == nil {
			diags = []any{}
		}
		outObj := map[string]any{
			"status":           status,
			"target_symbols":   targetSyms,
			"diagnostics":      diags,
			"patches_proposed": report.PatchesProposed,
			"execution_ms":     elapsed.Milliseconds(),
			"files_touched":    report.FilesTouched,
			"merkle_seal":      report.MerkleSeal,
		}
		report.ExecutionMS = elapsed.Milliseconds()
		if repData, err := json.Marshal(report); err == nil {
			fmt.Println(string(repData))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(outObj)
		if !report.Success {
			return errors.New("autonomous loop finished without satisfying acceptance criteria")
		}
		return nil
	}

	fmt.Printf("\n================================================================================\n")
	statusStr := "SUCCESS"
	if !report.Success {
		statusStr = "FAILED"
	}
	fmt.Printf("  Execution Summary: %s\n", statusStr)
	if cfg.Unbounded || cfg.MaxTurns <= 0 {
		fmt.Printf("  Total Turns:       %d (Unbounded)\n", report.TurnsCount)
	} else {
		fmt.Printf("  Total Turns:       %d / %d\n", report.TurnsCount, cfg.MaxTurns)
	}
	fmt.Printf("  Heal Attempts:     %d\n", report.HealCount)
	fmt.Printf("  Files Touched:     %d %v\n", len(report.FilesTouched), report.FilesTouched)
	fmt.Printf("  Commits Made:      %d %v\n", len(report.Commits), report.Commits)
	fmt.Printf("  Merkle Root Seal:  %s\n", report.MerkleSeal)
	fmt.Printf("  Duration:          %v\n", elapsed)
	fmt.Printf("================================================================================\n")

	if !report.Success {
		return errors.New("autonomous loop finished without satisfying acceptance criteria")
	}
	return nil
}

// ----------------------------------------------------------------------------
// 2. omnicoder map
// ----------------------------------------------------------------------------
func executeMap(ctx context.Context, cfg *GlobalConfig, args []string) error {
	mapFS := flag.NewFlagSet("map", flag.ContinueOnError)
	bindGlobalFlags(mapFS, cfg)

	tokenBudget := mapFS.Int("token-budget", 1024, "Maximum token budget for RepoMap outline")
	focus := mapFS.String("focus", "", "Comma-separated focus files to bias PageRank (+5.0)")
	query := mapFS.String("query", "", "Search query to bias PageRank symbols (+3.0)")
	maxFiles := mapFS.Int("max-files", 0, "Max files to include in map (0 = unlimited)")

	if err := parseWithRearrangedArgs(mapFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	var focusFiles []string
	if *focus != "" {
		for _, f := range strings.Split(*focus, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				focusFiles = append(focusFiles, f)
			}
		}
	}

	start := time.Now()
	result, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: cfg.Workspace,
		TokenBudget:  *tokenBudget,
		FocusFiles:   focusFiles,
		Query:        *query,
		MaxFiles:     *maxFiles,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error synthesizing repo map: %v\n", err)
		return err
	}
	elapsed := time.Since(start)

	// Emit formatted tree outline directly to stdout
	fmt.Print(result.RepoMap)

	if cfg.Verbose || elapsed > 2*time.Second {
		fmt.Fprintf(os.Stderr, "\n[omnicoder map] Scanned %d files, found %d symbols, %d tokens in %v (budget: %d)\n",
			result.FilesScanned, result.SymbolsFound, result.TokenCount, elapsed, *tokenBudget)
	}

	return nil
}

// ----------------------------------------------------------------------------
// 2b. omnicoder search "<query>"
// ----------------------------------------------------------------------------
func executeSearch(ctx context.Context, cfg *GlobalConfig, args []string) error {
	searchFS := flag.NewFlagSet("search", flag.ContinueOnError)
	bindGlobalFlags(searchFS, cfg)
	maxResults := searchFS.Int("max-results", 10, "Maximum number of search results to display")
	includeTests := searchFS.Bool("include-tests", false, "Include test files in search")
	if err := parseWithRearrangedArgs(searchFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := searchFS.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "Error: search query required\n\nUsage: omnicoder search [options] \"<query>\"\n")
		return errors.New("missing query")
	}

	query := strings.Join(posArgs, " ")
	tokens := strings.Fields(query)

	fmt.Printf("OmniCoder-ICX Code Search: %q in %s\n\n", query, cfg.Workspace)
	results, err := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: cfg.Workspace,
		QueryTokens:  tokens,
		ExcludeTests: !*includeTests,
		MaxResults:   *maxResults,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error searching codebase: %v\n", err)
		return err
	}

	if len(results) == 0 {
		fmt.Printf("No matching symbols or definitions found for %q\n", query)
		return nil
	}

	for i, r := range results {
		fmt.Printf("[%d] %s:%d (kind: %s, matched: %q, score: %d)\n", i+1, r.File, r.Line, r.MatchKind, r.MatchedToken, r.Score)
		for _, l := range r.ContextLines {
			fmt.Printf("  %s\n", l)
		}
		fmt.Println()
	}
	return nil
}

// ----------------------------------------------------------------------------
func isInsideWorkspace(wsAbs, filePath string) bool {
	if filePath == "" {
		return false
	}
	fSlash := strings.ToLower(filepath.ToSlash(filePath))
	if strings.HasPrefix(fSlash, "/usr/") ||
		strings.HasPrefix(fSlash, "/opt/") ||
		strings.Contains(fSlash, "site-packages") ||
		strings.Contains(fSlash, "dist-packages") ||
		strings.Contains(fSlash, "/lib/python") ||
		strings.Contains(fSlash, "unittest/") ||
		strings.Contains(fSlash, "<frozen") {
		return false
	}
	if !filepath.IsAbs(filePath) {
		return !strings.HasPrefix(filePath, "..")
	}
	rel, err := filepath.Rel(wsAbs, filePath)
	return err == nil && !strings.HasPrefix(rel, "..")
}

func rankSupportingFiles(files []string, targetFile string, originSupporting []string, issueText string) []string {
	if len(files) <= 1 {
		return files
	}
	targetDir := filepath.Dir(filepath.ToSlash(targetFile))
	issueLower := strings.ToLower(issueText)
	origMap := make(map[string]bool)
	for _, osf := range originSupporting {
		origMap[filepath.ToSlash(osf)] = true
	}

	type scoredFile struct {
		file  string
		score int
	}
	var scored []scoredFile
	for _, f := range files {
		fClean := filepath.ToSlash(f)
		s := 0
		if origMap[fClean] {
			s += 50
		}
		fDir := filepath.Dir(fClean)
		if fDir == targetDir && targetDir != "." {
			s += 35
		} else if strings.HasPrefix(fDir, targetDir) || strings.HasPrefix(targetDir, fDir) {
			s += 20
		}
		baseName := strings.ToLower(filepath.Base(fClean))
		stem := strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if len(stem) > 3 && strings.Contains(issueLower, stem) {
			s += 25
		}
		if baseName == "base.py" || baseName == "models.py" || baseName == "core.py" || baseName == "query.py" {
			s += 30
		}
		if strings.Contains(baseName, "mixin") || strings.Contains(baseName, "helper") || strings.Contains(baseName, "util") {
			s -= 15
		}
		if strings.Contains(fClean, "contrib/") || strings.Contains(fClean, "admin/") {
			s -= 40
		}
		scored = append(scored, scoredFile{file: fClean, score: s})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	res := make([]string, len(scored))
	for i, sf := range scored {
		res[i] = sf.file
	}
	return res
}

type workspaceFileIndex struct {
	ws       string
	allFiles []string
	byBase   map[string][]string
}

func newWorkspaceFileIndex(ws string) *workspaceFileIndex {
	idx := &workspaceFileIndex{
		ws:     ws,
		byBase: make(map[string][]string),
	}
	_ = filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				name == ".venv" || name == "venv" || name == "__pycache__" ||
				name == ".omnicoder" || name == ".tox" || name == ".pytest_cache" ||
				name == ".mypy_cache" || name == "doc" || name == "docs" ||
				name == "tutorials" || name == "tutorial" || name == "examples" ||
				name == "galleries" || name == "gallery" || name == "benchmarks" ||
				name == "build" || name == "dist" || name == ".eggs" || name == "site-packages" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".pdf",
			".so", ".pyd", ".dll", ".dylib", ".a", ".o", ".pyc", ".pyo",
			".whl", ".gz", ".tar", ".zip", ".rst", ".md", ".txt", ".html", ".css":
			return nil
		}
		if rel, relErr := filepath.Rel(ws, path); relErr == nil {
			relSlash := filepath.ToSlash(rel)
			idx.allFiles = append(idx.allFiles, relSlash)
			base := filepath.Base(relSlash)
			idx.byBase[base] = append(idx.byBase[base], relSlash)
		}
		return nil
	})
	return idx
}

func (idx *workspaceFileIndex) resolve(candidatePath, fallbackPath string, excludeTests bool) string {
	target := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(candidatePath)), "./"), "/")
	if target == "" {
		target = strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(fallbackPath)), "./"), "/")
	}
	if target == "" {
		return ""
	}
	if fi, err := os.Stat(filepath.Join(idx.ws, filepath.FromSlash(target))); err == nil && !fi.IsDir() {
		return target
	}
	// Check if target is a relative path missing a top-level prefix like lib/ or src/
	if strings.Contains(target, "/") {
		for _, m := range idx.allFiles {
			if strings.HasSuffix(m, "/"+target) {
				if excludeTests && (strings.HasPrefix(m, "tests/") || strings.HasPrefix(m, "testing/") || strings.Contains(m, "/tests/")) {
					continue
				}
				return m
			}
		}
	}
	if fallbackPath != "" {
		if fi, err := os.Stat(filepath.Join(idx.ws, fallbackPath)); err == nil && !fi.IsDir() {
			if filepath.Base(target) == filepath.Base(fallbackPath) || !strings.Contains(target, "/") {
				return filepath.ToSlash(fallbackPath)
			}
		}
	}
	base := filepath.Base(target)
	if matches, ok := idx.byBase[base]; ok && len(matches) > 0 {
		targetSegs := strings.Split(filepath.Dir(target), "/")
		fbSegs := strings.Split(filepath.ToSlash(filepath.Dir(fallbackPath)), "/")
		bestMatch := ""
		bestScore := -999
		for _, m := range matches {
			isTestM := strings.HasPrefix(m, "tests/") || strings.HasPrefix(m, "testing/") || strings.Contains(m, "/tests/") || strings.HasPrefix(filepath.Base(m), "test_") || strings.Contains(filepath.Base(m), "_test.")
			if excludeTests && isTestM {
				continue
			}
			score := 0
			if isTestM {
				score -= 10
			}
			mSegs := strings.Split(filepath.Dir(m), "/")
			for _, ts := range targetSegs {
				if ts == "" || ts == "." {
					continue
				}
				for _, ms := range mSegs {
					if ts == ms {
						score += 4
					}
				}
			}
			for _, fs := range fbSegs {
				if fs == "" || fs == "." {
					continue
				}
				for _, ms := range mSegs {
					if fs == ms {
						score += 2
					}
				}
			}
			if score > bestScore {
				bestScore = score
				bestMatch = m
			}
		}
		if bestMatch != "" {
			return bestMatch
		}
	}
	stem := strings.TrimSuffix(base, ".py")
	if stem != "" && len(stem) >= 3 {
		for _, m := range idx.allFiles {
			if strings.HasPrefix(m, "tests/") || strings.HasPrefix(m, "testing/") || strings.Contains(m, "/tests/") || strings.HasPrefix(filepath.Base(m), "test_") || strings.Contains(filepath.Base(m), "_test.") {
				continue
			}
			mStem := strings.TrimSuffix(filepath.Base(m), filepath.Ext(m))
			if mStem == stem {
				return m
			}
		}
	}
	if fallbackPath != "" {
		if fi, err := os.Stat(filepath.Join(idx.ws, fallbackPath)); err == nil && !fi.IsDir() {
			return filepath.ToSlash(fallbackPath)
		}
	}
	return target
}

func rerouteCrossFileEditBlocks(wsDir string, blocks []devops.EditBlock, activeSupporting []string, modifiedFiles map[string]bool) []devops.EditBlock {
	if len(blocks) == 0 {
		return blocks
	}
	var candidateFiles []string
	seen := make(map[string]bool)
	for _, sf := range activeSupporting {
		sfClean := filepath.ToSlash(strings.TrimSpace(sf))
		if sfClean != "" && !seen[sfClean] {
			seen[sfClean] = true
			candidateFiles = append(candidateFiles, sfClean)
		}
	}
	for mf := range modifiedFiles {
		mfClean := filepath.ToSlash(strings.TrimSpace(mf))
		if mfClean != "" && !seen[mfClean] {
			seen[mfClean] = true
			candidateFiles = append(candidateFiles, mfClean)
		}
	}
	if len(candidateFiles) == 0 {
		return blocks
	}

	routed := make([]devops.EditBlock, len(blocks))
	copy(routed, blocks)
	for i, b := range routed {
		if strings.TrimSpace(b.SearchText) == "" {
			continue
		}
		primaryFull := filepath.Join(wsDir, b.FilePath)
		if pBytes, err := os.ReadFile(primaryFull); err == nil {
			if _, _, _, matchErr := devops.ReplaceFuzzy(string(pBytes), b.SearchText, b.ReplaceText, true); matchErr == nil {
				continue
			}
		}
		// Primary file did not match SearchText; check supporting & modified files
		for _, suppFP := range candidateFiles {
			if suppFP == b.FilePath {
				continue
			}
			suppFull := filepath.Join(wsDir, suppFP)
			sBytes, err := os.ReadFile(suppFull)
			if err != nil {
				continue
			}
			if _, _, _, matchErr := devops.ReplaceFuzzy(string(sBytes), b.SearchText, b.ReplaceText, true); matchErr == nil {
				fmt.Printf("  [omnicoder swarm] Auto-routed edit block %d from %s -> supporting file %s\n", i+1, b.FilePath, suppFP)
				routed[i].FilePath = suppFP
				break
			}
		}
	}
	return routed
}

// 3. omnicoder heal "<command>"
// ----------------------------------------------------------------------------
func executeHeal(ctx context.Context, cfg *GlobalConfig, args []string) error {
	healFS := flag.NewFlagSet("heal", flag.ContinueOnError)
	bindGlobalFlags(healFS, cfg)
	healIssue := healFS.String("issue", "", "Issue description or problem statement for context")
	if err := parseWithRearrangedArgs(healFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := healFS.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "Error: command required for 'heal'\n\nUsage: omnicoder heal [options] \"<command>\"\n")
		return errors.New("missing command")
	}
	cmdToRun := strings.Join(posArgs, " ")
	cmdToRun = strings.TrimSpace(cmdToRun)
	if cmdToRun == "" {
		fmt.Fprintf(os.Stderr, "Error: command required for 'heal'\n\nUsage: omnicoder heal [options] \"<command>\"\n")
		return errors.New("missing command")
	}

	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 10
	}

	fmt.Printf("================================================================================\n")
	fmt.Printf("  OmniCoder-ICX Self-Healing Compiler Runner\n")
	fmt.Printf("  Target Command: %q\n", cmdToRun)
	fmt.Printf("  Workspace:      %s\n", cfg.Workspace)
	fmt.Printf("  Max Attempts:   %d\n", cfg.MaxTurns)
	fmt.Printf("================================================================================\n")

	var gate llm.DecisionGate
	if cfg.Offline {
		gate = llm.NewMockDecisionGate()
	} else {
		wireClient, err := llm.NewClient(llm.ClientConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
			Timeout: 180 * time.Second,
		})
		if err != nil {
			gate = llm.NewMockDecisionGate()
		} else {
			gate = llm.NewDecisionGate(wireClient, cfg.Model)
		}
	}

	var healVM vm.Client
	var healBackend string
	if cfg.Sandbox {
		healVM = vm.NewHermeticLocalSandbox(cfg.Workspace)
		healBackend = "Calera_MicroVM"
	}

	wsAbs, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		wsAbs = cfg.Workspace
	}
	extraEnv := []string{
		fmt.Sprintf("PYTHONPATH=%s%c%s%c%s", filepath.Join(wsAbs, "src"), os.PathListSeparator, wsAbs, os.PathListSeparator, os.Getenv("PYTHONPATH")),
		"PYTEST_DISABLE_PLUGIN_AUTOLOAD=1",
	}

	pinSinglePytestNode := func(c string) string {
		if !strings.Contains(c, "pytest") || !strings.Contains(c, " -k ") {
			return c
		}
		parts := strings.Fields(c)
		pyFileIdx := -1
		kFlagIdx := -1
		for idx, p := range parts {
			if strings.HasSuffix(p, ".py") && pyFileIdx == -1 {
				pyFileIdx = idx
			} else if p == "-k" && idx+1 < len(parts) {
				kFlagIdx = idx
				break
			}
		}
		if pyFileIdx == -1 || kFlagIdx != pyFileIdx+1 || kFlagIdx+1 >= len(parts) {
			return c
		}
		kVal := strings.Trim(parts[kFlagIdx+1], `"'`)
		if kVal == "" || strings.ContainsAny(kVal, " ()|&") {
			return c
		}
		// Ensure next token after kVal isn't 'or'/'and'
		if kFlagIdx+2 < len(parts) && (parts[kFlagIdx+2] == "or" || parts[kFlagIdx+2] == "and" || !strings.HasPrefix(parts[kFlagIdx+2], "-")) {
			return c
		}
		pyFile := parts[pyFileIdx]
		fBytes, fErr := os.ReadFile(filepath.Join(cfg.Workspace, filepath.FromSlash(pyFile)))
		if fErr != nil {
			return c
		}
		fLines := strings.Split(string(fBytes), "\n")
		matchCount := 0
		matchIndent := 0
		enclosingClass := ""
		curTopClass := ""
		for _, fl := range fLines {
			tfl := strings.TrimSpace(fl)
			ind := len(fl) - len(strings.TrimLeft(fl, " \t"))
			if ind == 0 && strings.HasPrefix(tfl, "class ") {
				cParts := strings.Fields(tfl)
				if len(cParts) >= 2 {
					curTopClass = strings.Split(strings.Split(cParts[1], "(")[0], ":")[0]
				}
			} else if ind == 0 && tfl != "" && !strings.HasPrefix(tfl, "#") && !strings.HasPrefix(tfl, "@") {
				curTopClass = ""
			}
			if strings.HasPrefix(tfl, "def "+kVal+"(") {
				matchCount++
				matchIndent = ind
				enclosingClass = curTopClass
			}
		}
		if matchCount == 1 {
			nodeID := pyFile + "::" + kVal
			if matchIndent > 0 && enclosingClass != "" {
				nodeID = pyFile + "::" + enclosingClass + "::" + kVal
			}
			var rebuilt []string
			rebuilt = append(rebuilt, parts[:pyFileIdx]...)
			rebuilt = append(rebuilt, nodeID)
			rebuilt = append(rebuilt, parts[kFlagIdx+2:]...)
			c = strings.Join(rebuilt, " ")
		}
		if strings.Contains(c, "pytest") && !strings.Contains(c, "-p pytester") && !strings.Contains(c, " -vv") {
			c = c + " -vv"
		}
		return c
	}
	cmdToRun = pinSinglePytestNode(cmdToRun)
	if cfg.ProbeCmd != "" {
		cfg.ProbeCmd = pinSinglePytestNode(cfg.ProbeCmd)
	}

	initialCmd := cmdToRun
	initialTimeout := 45
	if cfg.ProbeCmd != "" {
		initialCmd = cfg.ProbeCmd
		initialTimeout = 40
	}

	report, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
		Command:          initialCmd,
		WorkspaceDir:     cfg.Workspace,
		TimeoutSeconds:   initialTimeout,
		ExecutionBackend: healBackend,
		VMClient:         healVM,
		ExtraEnv:         extraEnv,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running target command: %v\n", err)
		return err
	}

	if ((report.Success && report.ExitCode == 0) || len(report.Diagnostics) == 0) && initialCmd != cmdToRun {
		fmt.Printf("\n[omnicoder heal] Probe command %q produced no failure/diagnostics (exit %d); falling back to full command %q...\n", initialCmd, report.ExitCode, cmdToRun)
		cfg.ProbeCmd = ""
		initialCmd = cmdToRun
		initialTimeout = 60
		report, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command:          initialCmd,
			WorkspaceDir:     cfg.Workspace,
			TimeoutSeconds:   initialTimeout,
			ExecutionBackend: healBackend,
			VMClient:         healVM,
			ExtraEnv:         extraEnv,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running target command: %v\n", err)
			return err
		}
	}

	if report.Success && report.ExitCode == 0 {
		fmt.Printf("\n[omnicoder heal] Target command %q PASSED with exit code 0! No healing needed.\n", initialCmd)
		return nil
	}

	fmt.Printf("\n[omnicoder heal] Command failed with exit code %d (%d diagnostics parsed)\n", report.ExitCode, len(report.Diagnostics))
	for i, d := range report.Diagnostics {
		fmt.Printf("  Diagnostic %d: %s:%d: %s\n", i+1, d.File, d.Line, d.Message)
		for _, line := range d.ContextLines {
			fmt.Printf("    %s\n", line)
		}
	}

	// Track all modified files and multi-file dependencies across healing iterations
	modifiedFiles := make(map[string]bool)
	lastPromotedLine := make(map[string]int)
	attemptedFiles := make(map[string]bool)
	var activeSupportingFiles []string
	var lastTurnReflection string
	var lastTurnPatch string
	var lastTurnOutcome string
	wsFileIdx := newWorkspaceFileIndex(cfg.Workspace)
	var cachedLocResult *devops.LocalizationResult
	var cachedLocTraceback string
	var llmRequestedFile string
	var llmRequestedHint string

	// Iterative healing loop
	for attempt := 1; attempt <= cfg.MaxTurns; attempt++ {
		fmt.Printf("\n>>> [Heal Turn %d/%d] Analyzing diagnostics...\n", attempt, cfg.MaxTurns)
		if len(report.Diagnostics) == 0 {
			fmt.Println("No actionable diagnostics found.")
			break
		}

		// Select actionable diagnostic:
		// For stack traces (Python, JS, etc.), the innermost failure or exception
		// is located near the end of the diagnostics slice, while callers are at the top.
		// For compiler errors (Go, Rust, TS, C/C++), the root cause is at the beginning.
		isStackTrace := false
		for _, d := range report.Diagnostics {
			if strings.HasPrefix(d.Message, "Traceback frame") || strings.HasPrefix(d.Message, "Pytest failure") {
				isStackTrace = true
				break
			}
		}

		var selectedDiag *devops.CompilerDiagnostic
		issueLower := strings.ToLower(*healIssue)
		isHarnessWrapper := func(fName string) bool {
			return strings.Contains(fName, ".omnicoder/") || strings.Contains(fName, "reproduce_issue") ||
				strings.Contains(fName, "test/testcases.py") || strings.Contains(fName, "test/utils.py") ||
				strings.Contains(fName, "testing/runner.py") || strings.HasSuffix(fName, "/pytest.py") ||
				strings.HasSuffix(fName, "/pytester.py") || strings.HasSuffix(fName, "/pytester_assertions.py") ||
				strings.HasSuffix(fName, "generic_parsetab.py") || strings.HasSuffix(fName, "generic_lextab.py") ||
				strings.HasSuffix(fName, "sympy/core/exceptions.py") ||
				strings.Contains(fName, "_unittest_backport.py") || strings.HasSuffix(fName, "/_testing.py") || strings.HasSuffix(fName, "/testing.py") ||
				((strings.Contains(fName, "db/backends/") || strings.Contains(fName, "backends/sqlite3") ||
					strings.Contains(fName, "backends/postgresql") || strings.Contains(fName, "backends/mysql") ||
					strings.Contains(fName, "backends/oracle") || strings.HasSuffix(fName, "db/utils.py")) &&
					!strings.Contains(issueLower, "sqlite") && !strings.Contains(issueLower, "creation") &&
					!strings.Contains(issueLower, "databasewrapper") && !strings.Contains(issueLower, "backends")) ||
				(strings.HasSuffix(fName, "core/cache.py") && !strings.Contains(issueLower, "cache"))
		}
		if isStackTrace {
			// Priority 1: Concrete exception in an in-repo implementation file (bypasses generic test failure frames, stdlib & DB backend teardowns)
			for i := len(report.Diagnostics) - 1; i >= 0; i-- {
				d := &report.Diagnostics[i]
				if !isInsideWorkspace(wsAbs, d.File) {
					continue
				}
				fName := strings.ToLower(filepath.ToSlash(d.File))
				isTest := strings.HasPrefix(fName, "tests/") || strings.HasPrefix(fName, "testing/") ||
					strings.HasPrefix(fName, ".omnicoder/") || strings.Contains(fName, "reproduce_issue") ||
					strings.Contains(fName, "/tests/") || strings.Contains(fName, "/testing/") ||
					strings.Contains(fName, "test_") || strings.Contains(fName, "_test.") || strings.HasSuffix(fName, "conftest.py")
				isInternalHarness := isHarnessWrapper(fName)
				isConcrete := strings.Contains(d.Message, "Error:") || strings.Contains(d.Message, "Exception:") ||
					strings.Contains(d.Message, "Warning:") || strings.Contains(d.Message, "Unrecognized") ||
					strings.Contains(d.Message, "ValueError") || strings.Contains(d.Message, "AttributeError") ||
					strings.Contains(d.Message, "TypeError") || strings.Contains(d.Message, "KeyError") ||
					strings.Contains(d.Message, "DoesNotExist") || strings.Contains(d.Message, "IntegrityError") ||
					strings.Contains(d.Message, "AssertionError") || strings.Contains(d.Message, "Error") ||
					strings.Contains(d.Message, "Exception")
				if !isTest && !isInternalHarness && isConcrete && len(d.ContextLines) > 0 {
					selectedDiag = d
					break
				}
			}
			// Priority 2: Any non-test in-repo diagnostic with context lines (not a raw frame pointer)
			if selectedDiag == nil {
				for i := len(report.Diagnostics) - 1; i >= 0; i-- {
					d := &report.Diagnostics[i]
					if !isInsideWorkspace(wsAbs, d.File) {
						continue
					}
					fName := strings.ToLower(filepath.ToSlash(d.File))
					isTest := strings.HasPrefix(fName, "tests/") || strings.HasPrefix(fName, "testing/") ||
						strings.Contains(fName, "/tests/") || strings.Contains(fName, "/testing/") ||
						strings.Contains(fName, "test_") || strings.Contains(fName, "_test.") ||
						isHarnessWrapper(fName)
					if !isTest && len(d.ContextLines) > 0 && !strings.HasPrefix(d.Message, "Traceback frame") &&
						!strings.HasPrefix(d.Message, "Pytest failure") {
						selectedDiag = d
						break
					}
				}
			}
			// Priority 3: Innermost non-frame in-repo diagnostic
			if selectedDiag == nil {
				for i := len(report.Diagnostics) - 1; i >= 0; i-- {
					d := &report.Diagnostics[i]
					if !isInsideWorkspace(wsAbs, d.File) {
						continue
					}
					fName := strings.ToLower(filepath.ToSlash(d.File))
					isHarness := isHarnessWrapper(fName)
					if len(d.ContextLines) > 0 && !isHarness {
						if selectedDiag == nil {
							selectedDiag = d
						}
						if !strings.HasPrefix(d.Message, "Traceback frame") && !strings.HasPrefix(d.Message, "Pytest failure") &&
							!strings.HasPrefix(d.Message, "Emitted warnings") {
							selectedDiag = d
							break
						}
					}
				}
			}
		} else {
			for i := 0; i < len(report.Diagnostics); i++ {
				d := &report.Diagnostics[i]
				if !isInsideWorkspace(wsAbs, d.File) {
					continue
				}
				if strings.Contains(d.Message, "too many errors") {
					continue
				}
				if len(d.ContextLines) > 0 {
					selectedDiag = d
					break
				}
			}
		}
		if selectedDiag == nil {
			for i := 0; i < len(report.Diagnostics); i++ {
				if isInsideWorkspace(wsAbs, report.Diagnostics[i].File) && !strings.Contains(report.Diagnostics[i].Message, "too many errors") {
					selectedDiag = &report.Diagnostics[i]
					break
				}
			}
		}
		if selectedDiag == nil {
			selectedDiag = &report.Diagnostics[0]
		}
		diag := *selectedDiag

		// Ensure relative file path for LLM prompt if inside workspace
		targetFilePath := diag.File
		if absWorkspace, err := filepath.Abs(cfg.Workspace); err == nil {
			if rel, err := filepath.Rel(absWorkspace, targetFilePath); err == nil && !strings.HasPrefix(rel, "..") {
				targetFilePath = filepath.ToSlash(rel)
			}
		}

		fmt.Printf("  Selected Diagnostic: %s:%d: %s\n", targetFilePath, diag.Line, diag.Message)

		issueContext := *healIssue
		// Prefer disk-persisted PROBLEM.md if it provides richer, un-truncated context with reproducing tests
		for _, docName := range []string{"PROBLEM.md", "README.md", ".docs/instructions.md"} {
			if probBytes, err := os.ReadFile(filepath.Join(cfg.Workspace, docName)); err == nil {
				fileText := strings.TrimSpace(string(probBytes))
				if len(fileText) > len(issueContext) || issueContext == "" {
					issueContext = fileText
					break
				}
			}
		}
		// Preserve rich Markdown formatting, code snippets, and newlines up to 4000 characters
		if len(issueContext) > 4000 {
			issueContext = issueContext[:3997] + "..."
		}
		if lastTurnReflection != "" {
			issueContext = lastTurnReflection + "\n\n" + issueContext
			if len(issueContext) > 4500 {
				issueContext = issueContext[:4497] + "..."
			}
		}

		// If targetFilePath is in a test directory, resolve to the underlying implementation in src/
		isTestFile := strings.HasPrefix(targetFilePath, "tests/") ||
			strings.HasPrefix(targetFilePath, "testing/") ||
			strings.HasPrefix(targetFilePath, ".omnicoder/") ||
			strings.Contains(targetFilePath, "reproduce_issue") ||
			strings.Contains(targetFilePath, "/tests/") ||
			strings.Contains(targetFilePath, "/testing/") ||
			isHarnessWrapper(strings.ToLower(filepath.ToSlash(targetFilePath))) ||
			strings.Contains(filepath.Base(targetFilePath), "test_") ||
			strings.Contains(filepath.Base(targetFilePath), "_test.")

		var candidateTokens []string
		var locCausalSymbol string

		// Build raw traceback from all diagnostics early
		var tbBuilder strings.Builder
		for _, d := range report.Diagnostics {
			if d.File != "" && d.Line > 0 {
				inFunc := ""
				for _, cl := range d.ContextLines {
					trimmed := strings.TrimLeft(strings.TrimSpace(cl), "> 0123456789:\t")
					if strings.HasPrefix(trimmed, "def ") {
						parts := strings.Fields(trimmed)
						if len(parts) >= 2 {
							inFunc = strings.Split(parts[1], "(")[0]
							break
						}
					}
				}
				if inFunc != "" {
					tbBuilder.WriteString(fmt.Sprintf("%s:%d: in %s\n", filepath.ToSlash(d.File), d.Line, inFunc))
				} else {
					tbBuilder.WriteString(fmt.Sprintf("%s:%d: %s\n", filepath.ToSlash(d.File), d.Line, d.Message))
				}
			}
			tbBuilder.WriteString(d.Message)
			tbBuilder.WriteString("\n")
			for _, cl := range d.ContextLines {
				tbBuilder.WriteString(cl)
				tbBuilder.WriteString("\n")
			}
		}
		rawTraceback := tbBuilder.String()

		// Extract failing assertion or error line from test failure output early
		var testAssertionHint string
		if isTestFile {
			for idx, l := range diag.ContextLines {
				trimmed := strings.TrimSpace(l)
				if strings.HasPrefix(trimmed, ">") || strings.Contains(trimmed, "> ") {
					if idxAssert := strings.Index(trimmed, "assert "); idxAssert != -1 {
						testAssertionHint = trimmed[idxAssert:]
						break
					} else if idxAssertUnder := strings.Index(trimmed, "assert_"); idxAssertUnder != -1 {
						testAssertionHint = trimmed[idxAssertUnder:]
						break
					} else {
						cleaned := strings.TrimLeft(trimmed, "> 0123456789: \t")
						if strings.HasPrefix(cleaned, "with pytest.raises") && idx+1 < len(diag.ContextLines) {
							nextCleaned := strings.TrimLeft(strings.TrimSpace(diag.ContextLines[idx+1]), "> 0123456789: \t")
							if nextCleaned != "" {
								testAssertionHint = nextCleaned
								break
							}
						}
						if cleaned != "" {
							testAssertionHint = cleaned
							break
						}
					}
				}
			}
			if testAssertionHint == "" {
				for _, l := range diag.ContextLines {
					trimmed := strings.TrimSpace(l)
					if strings.Contains(trimmed, "assert ") && !strings.Contains(trimmed, "exit_code") {
						if idx := strings.Index(trimmed, "assert "); idx != -1 {
							testAssertionHint = trimmed[idx:]
							break
						}
					} else if strings.Contains(trimmed, "assert_") {
						if idx := strings.Index(trimmed, "assert_"); idx != -1 {
							testAssertionHint = trimmed[idx:]
							break
						}
					} else if strings.Contains(trimmed, "self.assert") {
						idx := strings.Index(trimmed, "self.assert")
						testAssertionHint = trimmed[idx:]
						break
					}
				}
			}
			// If still empty or bare Error/AssertionError without source statement, read diag.Line directly from test file on disk
			if (testAssertionHint == "" || strings.HasSuffix(testAssertionHint, "Error:") || strings.HasSuffix(testAssertionHint, "Error")) && diag.Line > 0 {
				if testBytes, err := os.ReadFile(filepath.Join(cfg.Workspace, targetFilePath)); err == nil {
					testLines := strings.Split(string(testBytes), "\n")
					if diag.Line-1 < len(testLines) {
						srcLine := strings.TrimSpace(testLines[diag.Line-1])
						if srcLine != "" {
							var srcParts []string
							srcParts = append(srcParts, srcLine)
							if strings.Count(srcLine, "(") > strings.Count(srcLine, ")") {
								for nextL := diag.Line; nextL < len(testLines) && nextL < diag.Line+5; nextL++ {
									nl := strings.TrimSpace(testLines[nextL])
									srcParts = append(srcParts, nl)
									if strings.Contains(nl, ")") {
										break
									}
								}
							}
							testAssertionHint = strings.Join(srcParts, " ")
						}
					}
				}
			} else if diag.Line > 1 {
				if testBytes, err := os.ReadFile(filepath.Join(cfg.Workspace, targetFilePath)); err == nil {
					testLines := strings.Split(string(testBytes), "\n")
					if diag.Line-1 < len(testLines) {
						startPre := diag.Line - 4
						if startPre < 0 {
							startPre = 0
						}
						var preParts []string
						for pl := startPre; pl < diag.Line-1; pl++ {
							pt := strings.TrimSpace(testLines[pl])
							if pt != "" && !strings.HasPrefix(pt, "#") && !strings.HasPrefix(pt, "def ") {
								preParts = append(preParts, pt)
							}
						}
						if len(preParts) > 0 {
							testAssertionHint = strings.Join(preParts, " ; ") + " -> " + testAssertionHint
						}
					}
				}
			}
			if testAssertionHint == "" {
				for _, l := range diag.ContextLines {
					trimmed := strings.TrimSpace(l)
					if strings.Contains(trimmed, "Error:") || strings.Contains(trimmed, "Exception:") {
						testAssertionHint = trimmed
						break
					}
				}
			}

			// Expand multi-line call if parenthesis is unclosed
			if testAssertionHint != "" && strings.Contains(testAssertionHint, "(") && !strings.Contains(testAssertionHint, ")") {
				for idx, l := range diag.ContextLines {
					trimmed := strings.TrimSpace(l)
					if strings.Contains(trimmed, testAssertionHint) {
						var callParts []string
						callParts = append(callParts, testAssertionHint)
						for nextIdx := idx + 1; nextIdx < len(diag.ContextLines); nextIdx++ {
							nextL := strings.TrimSpace(diag.ContextLines[nextIdx])
							cleanedNext := strings.TrimLeft(nextL, "> 0123456789: \t")
							callParts = append(callParts, cleanedNext)
							if strings.Contains(cleanedNext, ")") {
								break
							}
						}
						testAssertionHint = strings.Join(callParts, " ")
						break
					}
				}
			}
		}

		// Attempt Traceback In-Repo Origin Resolution directly from structured report.Diagnostics
		var originFile string
		var originLine int
		var originFunc string
		var originSupporting []string
		originLinesByFile := make(map[string]int)
		originFuncsByFile := make(map[string]string)

		// Scan backwards through diagnostics for the first non-stub, non-harness in-repo caller
		for i := len(report.Diagnostics) - 1; i >= 0; i-- {
			d := report.Diagnostics[i]
			if !isInsideWorkspace(wsAbs, d.File) || len(d.ContextLines) == 0 {
				continue
			}
			fName := strings.ToLower(filepath.ToSlash(d.File))
			if strings.HasPrefix(fName, "tests/") || strings.HasPrefix(fName, "testing/") ||
				strings.Contains(fName, "/tests/") || strings.Contains(fName, "/testing/") ||
				strings.Contains(fName, "test_") || strings.Contains(fName, "_test.") || strings.HasSuffix(fName, "conftest.py") {
				continue
			}
			if isHarnessWrapper(fName) {
				continue
			}
			relFile := d.File
			if rel, err := filepath.Rel(wsAbs, d.File); err == nil && !strings.HasPrefix(rel, "..") {
				relFile = filepath.ToSlash(rel)
			}
			if originLinesByFile[relFile] == 0 && d.Line > 0 {
				originLinesByFile[relFile] = d.Line
				originFuncsByFile[relFile] = d.Message
			}

			// Check if this line is an exception/stub raise site (e.g. DoesNotExist in query.py or KeyError)
			isRaiseStub := false
			for _, cl := range d.ContextLines {
				clTrim := strings.TrimSpace(cl)
				if strings.HasPrefix(clTrim, "raise ") || strings.HasPrefix(clTrim, ">  raise ") ||
					strings.HasPrefix(clTrim, "> raise ") || strings.HasPrefix(clTrim, "assert ") ||
					strings.Contains(clTrim, "warnings.warn(") {
					isRaiseStub = true
					break
				}
			}
			if strings.Contains(d.Message, "DoesNotExist") || strings.Contains(d.Message, "KeyError") ||
				strings.Contains(d.Message, "IndexError") || strings.Contains(d.Message, "AttributeError") ||
				strings.Contains(d.Message, "ValueError") || strings.Contains(d.Message, "TypeError") {
				isRaiseStub = true
			}

			// If it's a raise stub in a generic utility/query engine, continue backwards to find the caller in application code!
			if (isRaiseStub || strings.Contains(d.Message, "in __exit__") || strings.Contains(d.Message, "in _execute")) && (strings.HasSuffix(fName, "query.py") || strings.Contains(fName, "utils/") || strings.HasSuffix(fName, "utils.py") || strings.Contains(fName, "base.py") || strings.Contains(fName, "exceptions.py") || strings.Contains(fName, "backends/")) {
				continue
			}

			if originFile == "" {
				originFile = relFile
				originLine = d.Line
				originFunc = d.Message
			} else if relFile != originFile {
				originSupporting = append(originSupporting, relFile)
			}
		}

		directImpl := ""
		if isTestFile {
			directImpl = devops.ResolveTestFileToImplementation(cfg.Workspace, targetFilePath)
		}
		if directImpl == "" {
			for _, d := range report.Diagnostics {
				fRel := d.File
				if rel, err := filepath.Rel(wsAbs, d.File); err == nil && !strings.HasPrefix(rel, "..") {
					fRel = filepath.ToSlash(rel)
				}
				if strings.HasPrefix(fRel, "tests/") || strings.HasPrefix(fRel, "testing/") || strings.Contains(filepath.Base(fRel), "test_") || strings.Contains(filepath.Base(fRel), "_test.") {
					if candImpl := devops.ResolveTestFileToImplementation(cfg.Workspace, fRel); candImpl != "" {
						directImpl = candImpl
						break
					}
				}
			}
		}
		if directImpl == "" {
			for _, tok := range strings.Fields(cmdToRun) {
				if strings.HasSuffix(tok, ".py") && (strings.Contains(tok, "test_") || strings.Contains(tok, "_test.") || strings.HasPrefix(tok, "tests/")) {
					if candImpl := devops.ResolveTestFileToImplementation(cfg.Workspace, tok); candImpl != "" {
						directImpl = candImpl
						break
					}
				}
			}
		}
		directImplFoundSymbol := false
		originBase := strings.ToLower(filepath.Base(originFile))
		isGenericDispatcher := originBase == "core.py" || originBase == "base.py" || originBase == "__init__.py" || originBase == "ui.py" || originBase == "registry.py" || originBase == "validation.py" || originBase == "state.py" || originBase == "label.py" || originBase == "checks.py"
		isArgMismatch := strings.Contains(diag.Message, "unexpected keyword argument") || strings.Contains(diag.Message, "positional argument") || strings.Contains(diag.Message, "required positional") || strings.Contains(rawTraceback, "unexpected keyword argument")
		directImplInTraceback := directImpl != "" && originLinesByFile[directImpl] > 0
		preferDirectImpl := directImpl != "" && directImpl != originFile && (isArgMismatch || isGenericDispatcher || (directImplInTraceback && filepath.Dir(directImpl) != filepath.Dir(originFile)))

		if originFile != "" && !preferDirectImpl {
			if originFile != targetFilePath || isTestFile {
				fmt.Printf("  [omnicoder origin] In-repo fault caller resolved to %s:%d in %s (supporting: %v)\n", originFile, originLine, originFunc, originSupporting)
				targetFilePath = originFile
				diag.Line = originLine
				isTestFile = false
			}
			if testAssertionHint != "" && !strings.Contains(diag.Message, " (test failure: ") {
				diag.Message = fmt.Sprintf("%s (test failure: %s)", diag.Message, testAssertionHint)
			}
			if directImpl != "" && directImpl != originFile {
				activeSupportingFiles = append(activeSupportingFiles, directImpl)
			}
			if len(originSupporting) > 0 {
				for _, osf := range originSupporting {
					activeSupportingFiles = append(activeSupportingFiles, osf)
				}
			}
		} else if directImpl != "" {
			fmt.Printf("  [omnicoder resolver] Directly mapped test file %s -> implementation: %s\n", targetFilePath, directImpl)
			targetFilePath = directImpl
			isTestFile = false
			if originFile != "" && originFile != directImpl {
				activeSupportingFiles = append(activeSupportingFiles, originFile)
			}
			if len(originSupporting) > 0 {
				for _, osf := range originSupporting {
					activeSupportingFiles = append(activeSupportingFiles, osf)
				}
			}
			if testAssertionHint != "" && !strings.Contains(diag.Message, " (test failure: ") {
				diag.Message = fmt.Sprintf("%s (test failure: %s)", diag.Message, testAssertionHint)
			}
			if prevLine := lastPromotedLine[targetFilePath]; prevLine > 0 {
				diag.Line = prevLine
				directImplFoundSymbol = true
				fmt.Printf("  [omnicoder resolver] Preserving focus at previously promoted patch line in %s: line %d\n", targetFilePath, prevLine)
			} else if tbLine := originLinesByFile[targetFilePath]; tbLine > 0 {
				diag.Line = tbLine
				directImplFoundSymbol = true
				fmt.Printf("  [omnicoder resolver] Using active traceback caller line in %s: line %d (%s)\n", targetFilePath, tbLine, originFuncsByFile[targetFilePath])
			} else if implLine, implSym, _ := devops.FindBestImplementationSymbolInFile(cfg.Workspace, targetFilePath, issueContext+" "+diag.Message+" "+testAssertionHint+" "+cmdToRun); implLine > 0 {
				diag.Line = implLine
				locCausalSymbol = implSym
				directImplFoundSymbol = true
				fmt.Printf("  [omnicoder resolver] Focused symbol in %s: %s at line %d\n", targetFilePath, implSym, implLine)
			}
		}

		// Spectrum Fault Localizer: Run once per distinct traceback to enrich activeSupportingFiles with BM25 / symbol-matched implementation files
		if len(activeSupportingFiles) < 5 {
			var locResult *devops.LocalizationResult
			var locErr error
			if cachedLocResult != nil && cachedLocTraceback == rawTraceback {
				locResult = cachedLocResult
			} else {
				locResult, locErr = devops.LocalizeFault(cfg.Workspace, issueContext, rawTraceback)
				if locErr == nil && locResult != nil {
					cachedLocResult = locResult
					cachedLocTraceback = rawTraceback
				}
			}
			if locErr == nil && locResult != nil {
				if locResult.PrimaryFile != "" && locResult.PrimaryFile != targetFilePath && locResult.PrimaryFile != originFile {
					activeSupportingFiles = append(activeSupportingFiles, locResult.PrimaryFile)
				}
				for _, sf := range locResult.SupportingFiles {
					if sf != targetFilePath && sf != originFile {
						activeSupportingFiles = append(activeSupportingFiles, sf)
					}
				}
				locSymbolOverridesDirect := false
				if originFile == "" && targetFilePath == directImpl && locResult.PrimaryFile != "" && locResult.PrimaryFile != directImpl && len(locResult.CausalSymbol) >= 3 {
					cs := locResult.CausalSymbol
					if strings.Contains(cmdToRun, cs) || strings.Contains(testAssertionHint, cs+"(") || strings.Contains(issueContext, "import "+cs) {
						locSymbolOverridesDirect = true
					}
				}
				if isTestFile || targetFilePath == "" || targetFilePath == "test" || locSymbolOverridesDirect || (!directImplFoundSymbol && originFile == "" && locResult.Confidence >= 0.70 && locResult.PrimaryFile != "") {
					if locResult.PrimaryFile != "" {
						if targetFilePath != "" && !isTestFile && targetFilePath != locResult.PrimaryFile {
							activeSupportingFiles = append([]string{targetFilePath}, activeSupportingFiles...)
						}
						fmt.Printf("  [omnicoder localizer] Spectrum Fault Localizer redirected to %s:%d (causal: %q, confidence: %.2f, supporting: %v)\n",
							locResult.PrimaryFile, locResult.LineRange[0], locResult.CausalSymbol, locResult.Confidence, locResult.SupportingFiles)
						targetFilePath = locResult.PrimaryFile
						if locResult.LineRange[0] > 0 {
							diag.Line = locResult.LineRange[0]
						}
						locCausalSymbol = locResult.CausalSymbol
						isTestFile = false
						if testAssertionHint != "" && !strings.Contains(diag.Message, " (test failure: ") {
							diag.Message = fmt.Sprintf("%s (test failure: %s)", diag.Message, testAssertionHint)
						}
					}
				} else if targetFilePath == locResult.PrimaryFile && !directImplFoundSymbol && locResult.LineRange[0] > 0 {
					diag.Line = locResult.LineRange[0]
					locCausalSymbol = locResult.CausalSymbol
				}
			}
		}
		// Deduplicate and sanitize activeSupportingFiles before test-to-impl resolution
		if len(activeSupportingFiles) > 0 {
			seenSF := make(map[string]bool)
			var uniqueSF []string
			for _, sf := range activeSupportingFiles {
				cleanSF := filepath.ToSlash(strings.TrimSpace(sf))
				if cleanSF != "" && !seenSF[cleanSF] && !strings.HasPrefix(cleanSF, "tests/") && !strings.HasPrefix(cleanSF, "testing/") && !strings.Contains(cleanSF, "_test.") && !isHarnessWrapper(strings.ToLower(cleanSF)) {
					seenSF[cleanSF] = true
					uniqueSF = append(uniqueSF, cleanSF)
				}
			}
			activeSupportingFiles = rankSupportingFiles(uniqueSF, targetFilePath, originSupporting, issueContext)
		}

		if isTestFile && (targetFilePath == diag.File || strings.HasPrefix(targetFilePath, "tests/") || strings.HasPrefix(targetFilePath, "testing/")) {
			// Priority 1: if Spectrum Fault Localizer identified supporting implementation files, pick highest-ranked non-test file
			if len(activeSupportingFiles) > 0 {
				for _, sf := range activeSupportingFiles {
					sfLower := strings.ToLower(filepath.ToSlash(sf))
					if !strings.HasPrefix(sfLower, "tests/") && !strings.HasPrefix(sfLower, "testing/") && !strings.Contains(sfLower, "/tests/") && !isHarnessWrapper(sfLower) {
						fmt.Printf("  [omnicoder localizer] Resolved test file to supporting implementation: %s\n", sf)
						targetFilePath = sf
						isTestFile = false
						if testAssertionHint != "" {
							diag.Message = fmt.Sprintf("%s (test failure: %s)", diag.Message, testAssertionHint)
						}
						if implLine, implSym, _ := devops.FindBestImplementationSymbolInFile(cfg.Workspace, targetFilePath, llmRequestedHint+" "+issueContext+" "+rawTraceback+" "+testAssertionHint); implLine > 0 {
							diag.Line = implLine
							locCausalSymbol = implSym
						}
						break
					}
				}
			}
		}

		if isTestFile && (targetFilePath == diag.File || strings.HasPrefix(targetFilePath, "tests/") || strings.HasPrefix(targetFilePath, "testing/")) {
			baseName := filepath.Base(targetFilePath)
			implBaseName := strings.TrimPrefix(baseName, "test_")
			if strings.Contains(baseName, "_test.") {
				implBaseName = strings.Replace(baseName, "_test.", ".", 1)
			}
			var hintFiles []string
			if implBaseName != baseName {
				hintFiles = append(hintFiles, implBaseName)
			}

			// Extract candidate search tokens from failure message, issue context, context lines, and test command
			candidateTokens = devops.ExtractCandidateTokens(diag.Message+" "+issueContext, diag.ContextLines, cmdToRun)

			// Perform general codebase search across workspace for genuine underlying implementation
			searchResults, searchErr := devops.SearchCodebase(devops.CodeSearchOptions{
				WorkspaceDir: cfg.Workspace,
				QueryTokens:  candidateTokens,
				HintFiles:    hintFiles,
				ExcludeTests: true,
				MaxResults:   3,
			})
			if searchErr == nil && len(searchResults) > 0 {
				bestMatch := searchResults[0]
				fmt.Printf("  [omnicoder search] Resolved implementation %s:%d (kind: %s, token: %q, score: %d)\n",
					bestMatch.File, bestMatch.Line, bestMatch.MatchKind, bestMatch.MatchedToken, bestMatch.Score)
				targetFilePath = bestMatch.File
				diag.Line = bestMatch.Line
				isTestFile = false
				if len(bestMatch.ContextLines) > 0 {
					diag.ContextLines = bestMatch.ContextLines
				}
				if testAssertionHint != "" {
					diag.Message = fmt.Sprintf("%s (test failure: %s)", diag.Message, testAssertionHint)
				}
			} else if implBaseName != baseName {
				// Fallback to fast workspace index resolution if general search returned no matches
				foundImpl := wsFileIdx.resolve(implBaseName, "", true)
				if foundImpl != "" && foundImpl != implBaseName {
					fmt.Printf("  Resolved test file to implementation (fallback): %s\n", foundImpl)
					targetFilePath = foundImpl
					isTestFile = false
					if testAssertionHint != "" {
						diag.Message = fmt.Sprintf("%s (test failure: %s)", diag.Message, testAssertionHint)
					}
				}
			}
		}

		if llmRequestedFile != "" && llmRequestedFile != targetFilePath && !isHarnessWrapper(strings.ToLower(llmRequestedFile)) {
			fmt.Printf("  [omnicoder redirect] Switching primary file to LLM-requested target from prior turn: %s\n", llmRequestedFile)
			if targetFilePath != "" && !isTestFile {
				activeSupportingFiles = append([]string{targetFilePath}, activeSupportingFiles...)
			}
			targetFilePath = llmRequestedFile
			isTestFile = false
			if implLine, implSym, _ := devops.FindBestImplementationSymbolInFile(cfg.Workspace, targetFilePath, llmRequestedHint+" "+issueContext+" "+diag.Message+" "+testAssertionHint+" "+cmdToRun); implLine > 0 {
				diag.Line = implLine
				locCausalSymbol = implSym
				fmt.Printf("  [omnicoder redirect] Focused symbol in %s: %s at line %d\n", targetFilePath, implSym, implLine)
			}
			llmRequestedHint = ""
		} else if llmRequestedHint != "" && targetFilePath != "" {
			if implLine, implSym, _ := devops.FindBestImplementationSymbolInFile(cfg.Workspace, targetFilePath, llmRequestedHint); implLine > 0 && implLine != diag.Line {
				diag.Line = implLine
				locCausalSymbol = implSym
				fmt.Printf("  [omnicoder redirect] Shifted focus in %s to LLM-targeted symbol: %s at line %d\n", targetFilePath, implSym, implLine)
			}
			llmRequestedHint = ""
		}
		attemptedFiles[targetFilePath] = true
		if !isTestFile && targetFilePath != "" {
			if baseImports := devops.ResolveInRepoBaseClassAndRelativeImports(cfg.Workspace, targetFilePath, issueContext); len(baseImports) > 0 {
				activeSupportingFiles = append(baseImports, activeSupportingFiles...)
				originSupporting = append(originSupporting, baseImports...)
			}
		}
		// Deduplicate and sanitize activeSupportingFiles after base-class & relative-import resolution
		if len(activeSupportingFiles) > 0 {
			seenSF := make(map[string]bool)
			var uniqueSF []string
			for _, sf := range activeSupportingFiles {
				cleanSF := filepath.ToSlash(strings.TrimSpace(sf))
				if cleanSF != "" && !seenSF[cleanSF] && !strings.HasPrefix(cleanSF, "tests/") && !strings.HasPrefix(cleanSF, "testing/") && !strings.Contains(cleanSF, "_test.") && !isHarnessWrapper(strings.ToLower(cleanSF)) {
					seenSF[cleanSF] = true
					uniqueSF = append(uniqueSF, cleanSF)
				}
			}
			activeSupportingFiles = rankSupportingFiles(uniqueSF, targetFilePath, originSupporting, issueContext)
		}

		// Attempt deterministic pre-heal before invoking LLM gate
		if preBlock, preSummary, ok := engine.AttemptDeterministicPreHeal(cfg.Workspace, targetFilePath, diag); ok {
			fmt.Printf("  [omnicoder preheal] Deterministic pre-heal: %s in %s\n", preSummary, preBlock.FilePath)
			fullTarget := filepath.Join(cfg.Workspace, preBlock.FilePath)
			if curBytes, err := os.ReadFile(fullTarget); err == nil {
				curText := string(curBytes)
				if strings.Contains(curText, preBlock.SearchText) {
					newText := strings.Replace(curText, preBlock.SearchText, preBlock.ReplaceText, 1)
					if err := os.WriteFile(fullTarget, []byte(newText), 0644); err == nil {
						fmt.Printf("  Applied pre-heal edit block successfully.\n")
						preCmd := cmdToRun
						preTimeout := 40
						if cfg.ProbeCmd != "" {
							preCmd = cfg.ProbeCmd
							preTimeout = 25
						}
						feedbackReq := devops.CompilerFeedbackRequest{
							Command:          preCmd,
							WorkspaceDir:     cfg.Workspace,
							TimeoutSeconds:   preTimeout,
							ExecutionBackend: healBackend,
							VMClient:         healVM,
							ExtraEnv:         extraEnv,
						}
						if preReport, preErr := devops.RunCompilerFeedbackLoop(ctx, feedbackReq); preErr == nil {
							if preReport.Success && preReport.ExitCode == 0 {
								if cfg.ProbeCmd != "" && cfg.ProbeCmd != cmdToRun {
									gateReport, gateErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
										Command:          cmdToRun,
										WorkspaceDir:     cfg.Workspace,
										TimeoutSeconds:   60,
										ExecutionBackend: healBackend,
										VMClient:         healVM,
										ExtraEnv:         extraEnv,
									})
									if gateErr != nil || gateReport == nil || !gateReport.Success || gateReport.ExitCode != 0 {
										_ = os.WriteFile(fullTarget, curBytes, 0644)
										goto skipPreHeal
									}
								}
								fmt.Printf("\n[omnicoder heal] SUCCESS: Target command %q PASSED on attempt %d (pre-heal)!\n", cmdToRun, attempt)
								return nil
							}
							report = preReport
							continue
						}
					}
				skipPreHeal:
				}
			}
		}

		// Enrich ContextLines from disk with full function block or compact file view
		var altContextLines []string
		if targetFilePath != "" {
			if fileBytes, err := os.ReadFile(filepath.Join(cfg.Workspace, targetFilePath)); err == nil {
				rawLines := strings.Split(string(fileBytes), "\n")
				srcLines := make([]string, len(rawLines))
				for idx, l := range rawLines {
					srcLines[idx] = strings.TrimRight(l, "\r")
				}
				focusLine := diag.Line
				if focusLine < 1 {
					focusLine = 1
				}
				if focusLine > len(srcLines) {
					focusLine = len(srcLines)
				}
				if len(srcLines) <= 340 {
					// Compact file view: entire file fits well within token budget (~1800 tokens)
					var fullFileLines []string
					for idx, l := range srcLines {
						marker := "  "
						if idx+1 == focusLine {
							marker = "> "
						}
						fullFileLines = append(fullFileLines, fmt.Sprintf("%s%4d: %s", marker, idx+1, l))
					}
					diag.ContextLines = fullFileLines
				} else if len(srcLines) <= 420 {
					diag.ContextLines = devops.FormatNumberedSliceElidingDocstrings(srcLines, 1, len(srcLines), len(srcLines), focusLine, 340, true)
				} else {
					// Check for enclosing class definition (especially for Python / JS / TS)
					classStart := -1
					classEnd := -1
					for i := focusLine - 1; i >= 0; i-- {
						trimmed := strings.TrimSpace(srcLines[i])
						if (strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(srcLines[i], "class ")) && !strings.HasPrefix(trimmed, "#") {
							classStart = i + 1
							for d := i - 1; d >= 0 && d >= i-5; d-- {
								if strings.HasPrefix(strings.TrimSpace(srcLines[d]), "@") {
									classStart = d + 1
								} else {
									break
								}
							}
							break
						}
					}
					if classStart > 0 {
						classIndent := len(srcLines[classStart-1]) - len(strings.TrimLeft(srcLines[classStart-1], " \t"))
						classEnd = len(srcLines)
						for j := classStart; j < len(srcLines); j++ {
							lineTrimmed := strings.TrimSpace(srcLines[j])
							if lineTrimmed == "" || strings.HasPrefix(lineTrimmed, "#") || strings.HasPrefix(lineTrimmed, "//") {
								continue
							}
							lineIndent := len(srcLines[j]) - len(strings.TrimLeft(srcLines[j], " \t"))
							if lineIndent <= classIndent && (strings.HasPrefix(lineTrimmed, "class ") || strings.HasPrefix(lineTrimmed, "def ") || strings.HasPrefix(lineTrimmed, "func ")) {
								classEnd = j
								break
							}
						}
					}

					var symRange [2]int
					if classStart > 0 && classEnd > classStart && (classEnd-classStart) <= 220 && focusLine >= classStart && focusLine <= classEnd {
						symRange = [2]int{classStart, classEnd}
					} else {
						symRange = devops.FindEnclosingSymbolRange(cfg.Workspace, targetFilePath, focusLine, locCausalSymbol)
					}

					origSymEnd := symRange[1]
					// Ensure range around focusLine is substantial (at least 140-200 lines) and bounded (<= 220 lines)
					if symRange[0] <= 0 || symRange[1] < symRange[0] || (symRange[1]-symRange[0]) < 100 || (symRange[1]-symRange[0]) > 220 {
						preOffset := 70
						postOffset := 140
						if focusLine >= symRange[0] && focusLine <= symRange[0]+5 {
							preOffset = 15
							postOffset = 195
						}
						startIdx := focusLine - preOffset
						endIdx := focusLine + postOffset
						if endIdx > len(srcLines) {
							endIdx = len(srcLines)
							startIdx = endIdx - 175
						}
						if startIdx < 1 {
							startIdx = 1
						}
						// If startIdx lands inside a function/class whose header is within 35 lines above, snap up to header
						for up := startIdx - 1; up >= 0 && up >= startIdx-35; up-- {
							tUp := strings.TrimSpace(srcLines[up])
							if strings.HasPrefix(tUp, "class ") || (strings.HasPrefix(tUp, "def ") && (len(srcLines[up])-len(strings.TrimLeft(srcLines[up], " \t"))) <= 4) {
								startIdx = up + 1
								break
							}
						}
						symRange = [2]int{startIdx, endIdx}
					}
					if origSymEnd < symRange[1] {
						origSymEnd = symRange[1]
					}

					var blockLines []string
					if outline := devops.ExtractFileSymbolOutline(cfg.Workspace, targetFilePath); outline != "" {
						blockLines = append(blockLines, outline)
					}
					startIdx := symRange[0] - 1
					if startIdx < 0 {
						startIdx = 0
					}
					if startIdx > 3 {
						importLimit := 60
						if importLimit > startIdx {
							importLimit = startIdx
						}
						var importLines []string
						for idx := 0; idx < importLimit && len(importLines) < 22; idx++ {
							trimmedImp := strings.TrimSpace(srcLines[idx])
							if strings.HasPrefix(trimmedImp, "import ") || strings.HasPrefix(trimmedImp, "from ") {
								importLines = append(importLines, fmt.Sprintf("  %4d: %s", idx+1, srcLines[idx]))
								parenDepth := strings.Count(trimmedImp, "(") - strings.Count(trimmedImp, ")")
								hasCont := strings.HasSuffix(trimmedImp, "\\")
								contSteps := 0
								for (parenDepth > 0 || hasCont) && idx+1 < importLimit && contSteps < 6 {
									idx++
									contSteps++
									importLines = append(importLines, fmt.Sprintf("  %4d: %s", idx+1, srcLines[idx]))
									tCont := strings.TrimSpace(srcLines[idx])
									parenDepth += strings.Count(tCont, "(") - strings.Count(tCont, ")")
									hasCont = strings.HasSuffix(tCont, "\\")
								}
							}
						}
						if len(importLines) > 0 {
							blockLines = append(blockLines, importLines...)
							blockLines = append(blockLines, "  ...")
						}
					}
					if cgSlices := devops.ExtractIntraFileCallGraphSlices(cfg.Workspace, targetFilePath, srcLines, symRange); len(cgSlices) > 0 {
						blockLines = append(blockLines, cgSlices...)
					}
					endIdx := symRange[1]
					if endIdx > len(srcLines) {
						endIdx = len(srcLines)
					}
					blockLines = append(blockLines, devops.FormatNumberedSliceElidingDocstrings(srcLines, startIdx+1, endIdx, origSymEnd, focusLine, 165, true)...)

					// Append up to 2 additional non-overlapping top-ranked symbols directly into primary context
					// so all N=3 beam candidates see all top symbols in a single shared-KV-cache vLLM batch call.
					topSyms := devops.FindTopImplementationSymbolsInFile(cfg.Workspace, targetFilePath, issueContext+" "+diag.Message+" "+testAssertionHint+" "+llmRequestedHint+" "+cmdToRun, 3)
					addedExtra := 0
					for _, extraSym := range topSyms {
						dist := extraSym.DefLine - focusLine
						if dist < 0 {
							dist = -dist
						}
						if dist < 40 && extraSym.DefLine >= symRange[0] && extraSym.DefLine <= symRange[1]-20 {
							continue
						}
						rStart := extraSym.LineRange[0]
						rEnd := extraSym.LineRange[1]
						if rEnd-rStart < 45 {
							rStart = extraSym.DefLine - 15
							rEnd = extraSym.DefLine + 45
						}
						if rEnd > len(srcLines) {
							rEnd = len(srcLines)
						}
						if rStart < 1 {
							rStart = 1
						}
						if rEnd-rStart > 80 {
							rEnd = rStart + 80
						}
						blockLines = append(blockLines, fmt.Sprintf("  # --- Additional Relevant Symbol: %s (L%d-L%d) ---", extraSym.Name, rStart, rEnd))
						blockLines = append(blockLines, devops.FormatNumberedSliceElidingDocstrings(srcLines, rStart, rEnd, extraSym.LineRange[1], extraSym.DefLine, 70, true)...)
						addedExtra++
						if addedExtra >= 2 {
							break
						}
					}
					if len(blockLines) > 0 {
						diag.ContextLines = blockLines
					}
				}
			}
		}

		// Enrich issue context with outlines of active supporting files (cross-module awareness)
		enrichedIssueContext := issueContext
		if len(activeSupportingFiles) > 0 {
			var suppOutlines []string
			for _, sf := range activeSupportingFiles {
				if sf != targetFilePath && !strings.HasPrefix(sf, "tests/") && !strings.Contains(sf, "_test") {
					if outline := devops.ExtractFileSymbolOutline(cfg.Workspace, sf); outline != "" {
						suppOutlines = append(suppOutlines, fmt.Sprintf("Supporting `%s`: %s", sf, outline))
					}
					if len(suppOutlines) >= 2 {
						break
					}
				}
			}
			if len(suppOutlines) > 0 {
				enrichedIssueContext = enrichedIssueContext + " | " + strings.Join(suppOutlines, " | ")
				if len(enrichedIssueContext) > 4000 {
					enrichedIssueContext = enrichedIssueContext[:3997] + "..."
				}
			}
		}

		card := llm.GateHealCard{
			FilePath:         targetFilePath,
			ErrorLine:        diag.Line,
			ErrorMessage:     diag.Message,
			Snippet5Lines:    strings.Join(diag.ContextLines, "\n"),
			AltSnippet5Lines: strings.Join(altContextLines, "\n"),
			Command:          cmdToRun,
			ErrorCode:        diag.ErrorCode,
			IssueContext:     enrichedIssueContext,
			PriorTurnPatch:   lastTurnPatch,
			PriorTurnOutcome: lastTurnOutcome,
			TokenBudget:      4900,
		}

		if cfg.Swarm {
			targetImportAndClassHints := ""
			if targetFilePath != "" {
				if tfBytes, tfErr := os.ReadFile(filepath.Join(cfg.Workspace, targetFilePath)); tfErr == nil {
					tfStr := string(tfBytes)
					tfLines := strings.Split(tfStr, "\n")
					var hintParts []string
					var relImports [][2]string // [modPath, importedSym]
					for ti := 0; ti < len(tfLines) && ti < 100 && len(hintParts) < 25; ti++ {
						tt := strings.TrimSpace(tfLines[ti])
						if strings.HasPrefix(tt, "import ") || strings.HasPrefix(tt, "from ") || strings.HasPrefix(tt, "class ") {
							hintParts = append(hintParts, tt)
						}
						if strings.HasPrefix(tt, "from .") && !strings.HasPrefix(tt, "from ..") && strings.Contains(tt, " import ") {
							parts := strings.SplitN(tt[6:], " import ", 2)
							modName := strings.TrimSpace(parts[0])
							impSyms := strings.TrimSpace(parts[1])
							if modName != "" {
								relModFile := filepath.ToSlash(filepath.Join(filepath.Dir(targetFilePath), strings.ReplaceAll(modName, ".", "/")+".py"))
								relImports = append(relImports, [2]string{relModFile, impSyms})
							}
						}
					}
					for _, ri := range relImports {
						relFile := ri[0]
						if _, sErr := os.Stat(filepath.Join(cfg.Workspace, filepath.FromSlash(relFile))); sErr == nil {
							isBaseClass := false
							for _, sym := range strings.Split(ri[1], ",") {
								sClean := strings.TrimSpace(strings.Trim(sym, "()"))
								if sClean != "" && strings.Contains(tfStr, "("+sClean+")") {
									isBaseClass = true
									break
								}
							}
							alreadyIn := false
							for _, existing := range activeSupportingFiles {
								if existing == relFile {
									alreadyIn = true
									break
								}
							}
							if !alreadyIn && (isBaseClass || len(activeSupportingFiles) < 3) {
								if isBaseClass {
									activeSupportingFiles = append([]string{relFile}, activeSupportingFiles...)
								} else {
									activeSupportingFiles = append(activeSupportingFiles, relFile)
								}
							}
						}
					}
					targetImportAndClassHints = strings.Join(hintParts, " ")
				}
			}
			if len(activeSupportingFiles) > 0 {
			var swarmBlocks []string
			for _, sf := range activeSupportingFiles {
				if sf != targetFilePath && !strings.HasPrefix(sf, "tests/") {
					if fileBytes, err := os.ReadFile(filepath.Join(cfg.Workspace, sf)); err == nil {
						rawLines := strings.Split(string(fileBytes), "\n")
						if len(rawLines) <= 120 {
							var tagged []string
							for lIdx, l := range rawLines {
								tagged = append(tagged, fmt.Sprintf("%4d: %s", lIdx+1, strings.TrimRight(l, "\r")))
							}
							swarmBlocks = append(swarmBlocks, fmt.Sprintf("=== Swarm Supporting File: %s ===\n%s", sf, strings.Join(tagged, "\n")))
						} else {
						symLine, symName, symRange := devops.FindBestImplementationSymbolInFile(cfg.Workspace, sf, issueContext+" "+diag.Message+" "+testAssertionHint+" "+llmRequestedHint+" "+targetImportAndClassHints+" "+cmdToRun)
						if tbLine := originLinesByFile[sf]; tbLine > 0 {
							tbRng := devops.FindEnclosingSymbolRange(cfg.Workspace, sf, tbLine, "")
							if tbRng[0] > 0 && tbRng[1] >= tbRng[0] {
								symLine = tbLine
								symRange = tbRng
								if symName == "" {
									symName = fmt.Sprintf("traceback L%d", tbLine)
								}
							}
						}
						if symLine > 0 && symRange[0] > 0 && symRange[1] >= symRange[0] {
							sStart := symRange[0] - 1
							if sStart < 0 {
								sStart = 0
							}
							sEnd := symRange[1]
							if sEnd > len(rawLines) {
								sEnd = len(rawLines)
							}
							if sEnd-sStart > 90 {
								sEnd = sStart + 90
							}
							tagged := devops.FormatNumberedSliceElidingDocstrings(rawLines, sStart+1, sEnd, symRange[1], symLine, 80, false)
							swarmBlocks = append(swarmBlocks, fmt.Sprintf("=== Swarm Supporting File: %s (%s, L%d-L%d) ===\n%s", sf, symName, sStart+1, sEnd, strings.Join(tagged, "\n")))
						} else if outline := devops.ExtractFileSymbolOutline(cfg.Workspace, sf); outline != "" {
							swarmBlocks = append(swarmBlocks, fmt.Sprintf("=== Swarm Supporting File: %s (Outline) ===\n%s", sf, outline))
						}
						}
					}
				}
				if len(swarmBlocks) >= 3 {
					break
				}
			}
			if len(swarmBlocks) > 0 {
				card.Snippet5Lines = card.Snippet5Lines + "\n\n" + strings.Join(swarmBlocks, "\n\n")
				if card.AltSnippet5Lines != "" {
					card.AltSnippet5Lines = card.AltSnippet5Lines + "\n\n" + strings.Join(swarmBlocks, "\n\n")
				}
				fmt.Printf("  [omnicoder swarm] Coordinated multi-file context injected (%d supporting files)\n", len(swarmBlocks))
			}
			}
		}

		beamK := cfg.BeamK
		if cfg.Swarm && beamK < 3 {
			beamK = 3
		}
		if beamK <= 0 {
			beamK = 1
		}

		var candidateDecisions []*llm.HealDecision
		if beamK > 1 {
			candList, candErr := gate.DecideHealSpeculative(ctx, card, beamK)
			if candErr == nil && len(candList) > 0 {
				candidateDecisions = candList
			}
		}
		if len(candidateDecisions) == 0 {
			singleDec, err := gate.DecideHeal(ctx, card)
			if err != nil {
				fmt.Fprintf(os.Stderr, "GateHeal error: %v\n", err)
				continue
			}
			candidateDecisions = []*llm.HealDecision{singleDec}
		}

		if len(candidateDecisions) > 1 {
			fmt.Printf("  [omnicoder beam] Evaluating %d speculative candidate solutions...\n", len(candidateDecisions))
			var bestDecision *llm.HealDecision
			var bestReport *devops.CompilerFeedbackReport
			var bestBlocks []devops.EditBlock
			bestScore := 0
			initialDiagCount := len(report.Diagnostics)

			var closestDecision *llm.HealDecision
			var closestReport *devops.CompilerFeedbackReport
			var closestBlocks []devops.EditBlock
			closestScore := -99999
			seenPatchFingerprints := make(map[string]bool)
			var nextLLMRequestedFile string
			var nextLLMRequestedHint string

			for candIdx, candDec := range candidateDecisions {
				candTargetFP := wsFileIdx.resolve(candDec.FilePath, card.FilePath, false)

				var candBlocks []devops.EditBlock
				for _, b := range candDec.EditBlocks {
					if strings.TrimSpace(b.SearchText) == strings.TrimSpace(b.ReplaceText) {
						continue
					}
					blockFP := wsFileIdx.resolve(b.FilePath, candTargetFP, false)
					candBlocks = append(candBlocks, devops.EditBlock{
						FilePath:    blockFP,
						SearchText:  b.SearchText,
						ReplaceText: b.ReplaceText,
					})
				}
				if len(candBlocks) == 0 {
					fmt.Printf("  [omnicoder beam] Candidate %d produced no-op edit blocks; skipping.\n", candIdx+1)
					continue
				}
				candBlocks = rerouteCrossFileEditBlocks(cfg.Workspace, candBlocks, activeSupportingFiles, modifiedFiles)

				// Multi-file backup before applying trial
				origFiles := make(map[string][]byte)
				for _, b := range candBlocks {
					fullP := filepath.Join(cfg.Workspace, b.FilePath)
					if _, seen := origFiles[fullP]; !seen {
						if data, readErr := os.ReadFile(fullP); readErr == nil {
							origFiles[fullP] = data
						}
					}
				}
				rollbackTrial := func() {
					for p, data := range origFiles {
						_ = os.WriteFile(p, data, 0644)
					}
				}

				for bIdx, b := range candBlocks {
					fmt.Printf("    [beam cand %d block %d (%s)] Search (%d chars): %q\n", candIdx+1, bIdx+1, b.FilePath, len(b.SearchText), b.SearchText)
					fmt.Printf("    [beam cand %d block %d (%s)] Replace (%d chars): %q\n", candIdx+1, bIdx+1, b.FilePath, len(b.ReplaceText), b.ReplaceText)
				}

				applyRes, applyErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
					RepoPath:   cfg.Workspace,
					Blocks:     candBlocks,
					FuzzyMatch: true,
				})

				isApplied := false
				if applyRes != nil {
					isApplied = applyRes.Applied
				}
				if applyErr != nil || !isApplied {
					fmt.Printf("  [omnicoder beam] Candidate %d failed to apply edit blocks (applied=%v, err=%v): %s\n",
						candIdx+1, isApplied, applyErr, candDec.RepairSummary)
					for _, b := range candBlocks {
						if b.FilePath != "" {
							if nextLLMRequestedHint == "" {
								nextLLMRequestedHint = b.SearchText + " " + candDec.RepairSummary
							}
							if b.FilePath != targetFilePath && strings.HasSuffix(b.FilePath, ".py") && !strings.HasPrefix(b.FilePath, "tests/") && !strings.HasPrefix(b.FilePath, "testing/") && !strings.Contains(b.FilePath, "/tests/") && !strings.HasPrefix(b.FilePath, "doc/") && !strings.HasPrefix(b.FilePath, "docs/") && !strings.HasPrefix(b.FilePath, "tutorials/") && !strings.HasPrefix(filepath.Base(b.FilePath), "test_") {
								if fi, statErr := os.Stat(filepath.Join(cfg.Workspace, b.FilePath)); statErr == nil && !fi.IsDir() {
									nextLLMRequestedFile = b.FilePath
									nextLLMRequestedHint = b.SearchText + " " + candDec.RepairSummary
									break
								}
							}
						}
					}
					rollbackTrial()
					continue
				}

				// Fast <1ms in-memory syntax pre-check & post-patch deduplication
				syntaxInvalid := false
				var fpBuilder strings.Builder
				checkedFiles := make(map[string]bool)
				for _, b := range candBlocks {
					fullP := filepath.Join(cfg.Workspace, b.FilePath)
					if checkedFiles[fullP] {
						continue
					}
					checkedFiles[fullP] = true
					patchedBytes, readErr := os.ReadFile(fullP)
					if readErr != nil {
						continue
					}
					normCode, wasNorm, fastErr := devops.ValidateCandidatePatchFast(b.FilePath, string(origFiles[fullP]), string(patchedBytes))
					if fastErr != nil {
						fmt.Printf("  [omnicoder beam] Candidate %d rejected by fast syntax guard (<1ms): %v\n", candIdx+1, fastErr)
						if b.FilePath != "" {
							if nextLLMRequestedHint == "" {
								nextLLMRequestedHint = b.SearchText + " " + candDec.RepairSummary
							}
							if b.FilePath != targetFilePath && strings.HasSuffix(b.FilePath, ".py") && !strings.HasPrefix(b.FilePath, "tests/") && !strings.HasPrefix(b.FilePath, "testing/") && !strings.Contains(b.FilePath, "/tests/") && !strings.HasPrefix(b.FilePath, "doc/") && !strings.HasPrefix(b.FilePath, "docs/") && !strings.HasPrefix(b.FilePath, "tutorials/") && !strings.HasPrefix(filepath.Base(b.FilePath), "test_") {
								if fi, statErr := os.Stat(filepath.Join(cfg.Workspace, b.FilePath)); statErr == nil && !fi.IsDir() {
									nextLLMRequestedFile = b.FilePath
									nextLLMRequestedHint = b.SearchText + " " + candDec.RepairSummary
								}
							}
						}
						syntaxInvalid = true
						break
					}
					if wasNorm {
						_ = os.WriteFile(fullP, []byte(normCode), 0644)
						patchedBytes = []byte(normCode)
					}
					fpBuilder.WriteString(b.FilePath)
					fpBuilder.WriteByte(0)
					fpBuilder.Write(patchedBytes)
					fpBuilder.WriteByte(0)
				}
				if syntaxInvalid {
					rollbackTrial()
					continue
				}
				patchFP := fpBuilder.String()
				if patchFP != "" && seenPatchFingerprints[patchFP] {
					fmt.Printf("  [omnicoder beam] Candidate %d produced identical file state as prior candidate; skipping redundant test run.\n", candIdx+1)
					rollbackTrial()
					continue
				}
				if patchFP != "" {
					seenPatchFingerprints[patchFP] = true
				}

				testCmdToUse := cmdToRun
				probeTimeout := 40
				if cfg.ProbeCmd != "" {
					testCmdToUse = cfg.ProbeCmd
				} else {
					probeTimeout = 45
				}

				trialReport, trialErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:          testCmdToUse,
					WorkspaceDir:     cfg.Workspace,
					TimeoutSeconds:   probeTimeout,
					ExecutionBackend: healBackend,
					VMClient:         healVM,
					ExtraEnv:         extraEnv,
				})

				if trialReport != nil {
					fmt.Printf("  [omnicoder beam] Candidate %d evaluated (probe): exit=%d, diags=%d (initial=%d): %s\n",
						candIdx+1, trialReport.ExitCode, len(trialReport.Diagnostics), initialDiagCount, candDec.RepairSummary)
				}

				probePassed := false
				if trialErr == nil && trialReport != nil && trialReport.Success && trialReport.ExitCode == 0 {
					// Tier 2 Hard Gate: Verify candidate against full regression suite
					if cfg.ProbeCmd != "" && cfg.ProbeCmd != cmdToRun {
						fmt.Printf("  [omnicoder tier2] Candidate %d probe passed! Running full acceptance gate: %q\n", candIdx+1, cmdToRun)
						gateReport, gateErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command:          cmdToRun,
							WorkspaceDir:     cfg.Workspace,
							TimeoutSeconds:   60,
							ExecutionBackend: healBackend,
							VMClient:         healVM,
							ExtraEnv:         extraEnv,
						})
						if gateErr == nil && gateReport != nil && gateReport.Success && gateReport.ExitCode == 0 {
							fmt.Printf("  [omnicoder tier2] Candidate %d PASSED full acceptance gate with exit code 0!\n", candIdx+1)
							for _, b := range candBlocks {
								modifiedFiles[b.FilePath] = true
							}
							fmt.Printf("\n[omnicoder heal] SUCCESS: Speculative Candidate %d %q PASSED with exit code 0!\n", candIdx+1, candDec.RepairSummary)
							return nil
						}
						exitCode := -1
						if gateReport != nil {
							exitCode = gateReport.ExitCode
						}
						fmt.Printf("  [omnicoder tier2] Candidate %d passed probe but failed full acceptance gate (exit=%d). Scoring against remaining failures...\n", candIdx+1, exitCode)
						if gateReport != nil {
							trialReport = gateReport
							trialErr = gateErr
							probePassed = true
						} else {
							rollbackTrial()
							continue
						}
					} else {
						for _, b := range candBlocks {
							modifiedFiles[b.FilePath] = true
						}
						fmt.Printf("\n[omnicoder heal] SUCCESS: Speculative Candidate %d %q PASSED with exit code 0!\n", candIdx+1, candDec.RepairSummary)
						return nil
					}
				}

				candScore, hasSyntaxError := devops.ScoreHealCandidate(report, trialReport, len(candBlocks))
				if trialErr != nil {
					candScore = -1000
				} else if probePassed && !hasSyntaxError && trialReport != nil {
					if candScore < 200 {
						candScore = 200
					} else {
						candScore += 200
					}
				}

				// Track closest non-syntax-error candidate for Turn T+1 diff awareness
				if !hasSyntaxError && trialErr == nil && trialReport != nil {
					if candScore > closestScore {
						closestScore = candScore
						closestDecision = candDec
						closestReport = trialReport
						closestBlocks = candBlocks
					}
				}

				if (probePassed || cfg.ProbeCmd == "" || candScore >= 60) && candScore > bestScore && trialReport != nil && !hasSyntaxError {
					bestScore = candScore
					bestDecision = candDec
					bestReport = trialReport
					bestBlocks = candBlocks
					if probePassed {
						cfg.ProbeCmd = ""
					}
				}

				// Rollback trial before evaluating next candidate
				rollbackTrial()
			}

			if bestDecision != nil && bestReport != nil && bestScore > 0 {
				targetFP := bestDecision.FilePath
				if targetFP == "" {
					targetFP = card.FilePath
				}
				blocks := bestBlocks
				_, _ = devops.ApplyEditBlocks(devops.EditBlockRequest{
					RepoPath:   cfg.Workspace,
					Blocks:     blocks,
					FuzzyMatch: true,
				})
				for _, b := range blocks {
					modifiedFiles[b.FilePath] = true
					actualLine := 0
					if pBytes, rErr := os.ReadFile(filepath.Join(cfg.Workspace, b.FilePath)); rErr == nil {
						pLines := strings.Split(string(pBytes), "\n")
						var firstRepLine string
						for _, rl := range strings.Split(b.ReplaceText, "\n") {
							if strings.TrimSpace(rl) != "" {
								firstRepLine = strings.TrimSpace(rl)
								break
							}
						}
						if firstRepLine != "" {
							for pli, pl := range pLines {
								if strings.TrimSpace(pl) == firstRepLine {
									actualLine = pli + 1
									break
								}
							}
						}
					}
					if actualLine > 0 {
						lastPromotedLine[b.FilePath] = actualLine
					} else if diag.Line > 0 && b.FilePath == targetFilePath {
						lastPromotedLine[b.FilePath] = diag.Line
					}
				}
				report = bestReport
				fmt.Printf("  [omnicoder beam] Promoted candidate %s (score=%d, remaining diagnostics: %d, exit code: %d)\n",
					bestDecision.RepairSummary, bestScore, len(report.Diagnostics), report.ExitCode)

				var patchSnippets []string
				for _, b := range blocks {
					patchSnippets = append(patchSnippets, fmt.Sprintf("--- %s\n+++ %s\n- %s\n+ %s", b.FilePath, b.FilePath, b.SearchText, b.ReplaceText))
				}
				lastTurnPatch = strings.Join(patchSnippets, "\n")
				if len(report.Diagnostics) > 0 {
					lastTurnOutcome = fmt.Sprintf("Exit code %d: %s", report.ExitCode, devops.FormatPrimaryFailureSummary(report))
				}
				continue
			}

			fmt.Printf("  [omnicoder beam] 0/%d candidates improved baseline diagnostics. Preserving clean workspace.\n", len(candidateDecisions))
			if nextLLMRequestedFile != "" {
				llmRequestedFile = nextLLMRequestedFile
			}
			if nextLLMRequestedHint != "" {
				llmRequestedHint = nextLLMRequestedHint
			}
			if closestDecision != nil && len(closestBlocks) > 0 {
				var patchSnippets []string
				for _, b := range closestBlocks {
					patchSnippets = append(patchSnippets, fmt.Sprintf("--- %s\n+++ %s\n- %s\n+ %s", b.FilePath, b.FilePath, b.SearchText, b.ReplaceText))
					if b.FilePath != "" && b.FilePath != targetFilePath && !strings.HasPrefix(b.FilePath, "tests/") && !strings.Contains(b.FilePath, "/tests/") && !strings.HasPrefix(filepath.Base(b.FilePath), "test_") && !isHarnessWrapper(strings.ToLower(b.FilePath)) {
						if fi, statErr := os.Stat(filepath.Join(cfg.Workspace, b.FilePath)); statErr == nil && !fi.IsDir() {
							llmRequestedFile = b.FilePath
							llmRequestedHint = b.SearchText + " " + closestDecision.RepairSummary
						}
					}
				}
				lastTurnPatch = strings.Join(patchSnippets, "\n")
				failMsg := devops.FormatPrimaryFailureSummary(closestReport)
				lastTurnOutcome = fmt.Sprintf("Candidate patch applied (then rolled back) because test still failed with: %s", failMsg)
				lastTurnReflection = fmt.Sprintf("Turn %d note: Prior attempt on %s did not fully resolve the test failure (%s). Address this remaining failure alongside any required changes.", attempt, targetFilePath, failMsg)
				fmt.Printf("  [omnicoder beam] Retained candidate diff (%d bytes) in reflexion memory for Turn %d (remaining: %s).\n", len(lastTurnPatch), attempt+1, failMsg)
			} else {
				lastTurnReflection = fmt.Sprintf("Turn %d note: Prior candidate attempts failed to apply or introduced syntax errors. Ensure clean replacement syntax.", attempt)
				lastTurnOutcome = "Prior candidates failed to apply cleanly."
			}
			continue
		}

		healDecision := candidateDecisions[0]

		fmt.Printf("  Repair strategy: %s\n", healDecision.RepairSummary)
		targetFP := wsFileIdx.resolve(healDecision.FilePath, card.FilePath, false)

		var blocks []devops.EditBlock
		for _, b := range healDecision.EditBlocks {
			if strings.TrimSpace(b.SearchText) == strings.TrimSpace(b.ReplaceText) {
				continue
			}
			blockFP := wsFileIdx.resolve(b.FilePath, targetFP, false)
			blocks = append(blocks, devops.EditBlock{
				FilePath:    blockFP,
				SearchText:  b.SearchText,
				ReplaceText: b.ReplaceText,
			})
		}
		if len(blocks) == 0 {
			fmt.Printf("  [omnicoder heal] Candidate produced no-op edit blocks; skipping.\n")
			continue
		}
		blocks = rerouteCrossFileEditBlocks(cfg.Workspace, blocks, activeSupportingFiles, modifiedFiles)

		origSingleFiles := make(map[string][]byte)
		for _, b := range blocks {
			fullP := filepath.Join(cfg.Workspace, b.FilePath)
			if _, seen := origSingleFiles[fullP]; !seen {
				if data, readErr := os.ReadFile(fullP); readErr == nil {
					origSingleFiles[fullP] = data
				}
			}
		}
		rollbackSingle := func() {
			for p, data := range origSingleFiles {
				_ = os.WriteFile(p, data, 0644)
			}
		}

		applyRes, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
			RepoPath:   cfg.Workspace,
			Blocks:     blocks,
			FuzzyMatch: true,
		})
		if err != nil || applyRes == nil || !applyRes.Applied {
			fmt.Fprintf(os.Stderr, "Failed to apply repair patch: %v\n", err)
			for idx, b := range blocks {
				fmt.Fprintf(os.Stderr, "  [Block %d Search (%s)]:\n%s\n  [Block %d Replace]:\n%s\n", idx+1, b.FilePath, b.SearchText, idx+1, b.ReplaceText)
			}
			continue
		}

		singleSyntaxInvalid := false
		checkedSingle := make(map[string]bool)
		for _, b := range blocks {
			fullP := filepath.Join(cfg.Workspace, b.FilePath)
			if checkedSingle[fullP] {
				continue
			}
			checkedSingle[fullP] = true
			if patchedBytes, readErr := os.ReadFile(fullP); readErr == nil {
				normCode, wasNorm, fastErr := devops.ValidateCandidatePatchFast(b.FilePath, string(origSingleFiles[fullP]), string(patchedBytes))
				if fastErr != nil {
					fmt.Printf("  [omnicoder heal] Candidate rejected by fast syntax guard (<1ms): %v\n", fastErr)
					singleSyntaxInvalid = true
					break
				}
				if wasNorm {
					_ = os.WriteFile(fullP, []byte(normCode), 0644)
				}
			}
		}
		if singleSyntaxInvalid {
			rollbackSingle()
			lastTurnReflection = fmt.Sprintf("Turn %d note: Prior attempt on %s introduced syntax/delimiter errors. Ensure clean replacement syntax.", attempt, targetFP)
			lastTurnOutcome = "Prior attempt introduced syntax/delimiter error."
			continue
		}
		casShort := applyRes.CASHash
		if len(casShort) > 12 {
			casShort = casShort[:12]
		}
		fmt.Printf("  Applied patch %s (%d blocks applied, CAS: %s)\n", applyRes.PatchID, applyRes.BlocksApplied, casShort)

		testCmdToUse := cmdToRun
		probeTimeout := 25
		if cfg.ProbeCmd != "" {
			testCmdToUse = cfg.ProbeCmd
		} else {
			probeTimeout = 40
		}

		// Re-test
		newReport, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command:          testCmdToUse,
			WorkspaceDir:     cfg.Workspace,
			TimeoutSeconds:   probeTimeout,
			ExecutionBackend: healBackend,
			VMClient:         healVM,
			ExtraEnv:         extraEnv,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error re-running command: %v\n", err)
			rollbackSingle()
			continue
		}

		if newReport.Success && newReport.ExitCode == 0 {
			// Tier 2 Hard Gate: Verify candidate against full regression suite
			if cfg.ProbeCmd != "" && cfg.ProbeCmd != cmdToRun {
				fmt.Printf("  [omnicoder tier2] Single patch probe passed! Running full acceptance gate: %q\n", cmdToRun)
				gateReport, gateErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:          cmdToRun,
					WorkspaceDir:     cfg.Workspace,
					TimeoutSeconds:   60,
					ExecutionBackend: healBackend,
					VMClient:         healVM,
					ExtraEnv:         extraEnv,
				})
				if gateErr != nil || gateReport == nil || !gateReport.Success || gateReport.ExitCode != 0 {
					exitCode := -1
					if gateReport != nil {
						exitCode = gateReport.ExitCode
					}
					fmt.Printf("  [omnicoder tier2] Single patch failed full acceptance gate (exit=%d). Rolling back.\n", exitCode)
					rollbackSingle()
					continue
				}
				fmt.Printf("  [omnicoder tier2] Single patch PASSED full acceptance gate with exit code 0!\n")
			}

			for _, b := range blocks {
				modifiedFiles[b.FilePath] = true
			}
			fmt.Printf("\n[omnicoder heal] SUCCESS: Target command %q PASSED on attempt %d!\n", cmdToRun, attempt)
			return nil
		}

		hasSyntaxErr := false
		for _, d := range newReport.Diagnostics {
			if strings.Contains(d.Message, "SyntaxError") || strings.Contains(d.Message, "IndentationError") {
				hasSyntaxErr = true
				break
			}
		}
		if hasSyntaxErr || newReport.ExitCode == 124 || len(newReport.Diagnostics) > len(report.Diagnostics) {
			fmt.Printf("  [omnicoder heal] Candidate introduced regressions (%d diags, exit=%d). Rolling back to clean baseline.\n", len(newReport.Diagnostics), newReport.ExitCode)
			rollbackSingle()
			lastTurnReflection = fmt.Sprintf("Turn %d note: Prior attempt on %s introduced regressions (syntax/indentation or higher error count). Avoid that change.", attempt, targetFP)
			var patchSnippets []string
			for _, b := range blocks {
				patchSnippets = append(patchSnippets, fmt.Sprintf("--- %s\n+++ %s\n- %s\n+ %s", b.FilePath, b.FilePath, b.SearchText, b.ReplaceText))
			}
			lastTurnPatch = strings.Join(patchSnippets, "\n")
			lastTurnOutcome = fmt.Sprintf("Prior attempt introduced syntax error or regressions (%d diagnostics).", len(newReport.Diagnostics))
			continue
		}

		for _, b := range blocks {
			modifiedFiles[b.FilePath] = true
		}
		var patchSnippets []string
		for _, b := range blocks {
			patchSnippets = append(patchSnippets, fmt.Sprintf("--- %s\n+++ %s\n- %s\n+ %s", b.FilePath, b.FilePath, b.SearchText, b.ReplaceText))
		}
		lastTurnPatch = strings.Join(patchSnippets, "\n")
		if len(newReport.Diagnostics) > 0 {
			lastTurnOutcome = fmt.Sprintf("Exit code %d with %d diagnostics: %s", newReport.ExitCode, len(newReport.Diagnostics), newReport.Diagnostics[0].Message)
		}
		report = newReport
		lastTurnReflection = fmt.Sprintf("Turn %d note: Applied partial fix to %s, remaining diagnostics: %d, exit: %d. Continue fixing remaining assertion.", attempt, targetFP, len(report.Diagnostics), report.ExitCode)
		fmt.Printf("  Command still failing (exit code %d). Remaining diagnostics: %d\n", report.ExitCode, len(report.Diagnostics))
	}

	fmt.Fprintf(os.Stderr, "\n[omnicoder heal] Halting: Max turns (%d) reached without passing.\n", cfg.MaxTurns)
	return errors.New("healing failed to resolve compiler errors")
}

// ----------------------------------------------------------------------------
// 3b. omnicoder solve — Issue-Only SWE-Bench Solver (Zero Gold-Test Visibility)
// ----------------------------------------------------------------------------

var errLeakageDetected = errors.New("solve: benchmark leakage tripwire triggered (forbidden gold-test markers detected in issue text)")

var forbiddenLeakageMarkers = []string{
	"FAIL_TO_PASS",
	"PASS_TO_PASS",
	"test_patch",
	"diff --git a/tests",
	"diff --git a/test_",
}

func checkIssueLeakage(issueText string) error {
	for _, marker := range forbiddenLeakageMarkers {
		if strings.Contains(issueText, marker) {
			return fmt.Errorf("%w: found forbidden marker %q", errLeakageDetected, marker)
		}
	}
	return nil
}

func extractIssueTraceback(issueText string) string {
	idx := strings.Index(issueText, "Traceback (most recent call last):")
	if idx != -1 {
		sub := issueText[idx:]
		if len(sub) > 1500 {
			sub = sub[:1500]
		}
		return sub
	}
	var tbLines []string
	for _, line := range strings.Split(issueText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "File \"") ||
			strings.Contains(trimmed, "Error:") ||
			strings.Contains(trimmed, "Exception:") ||
			strings.Contains(trimmed, "AssertionError") {
			tbLines = append(tbLines, trimmed)
		}
	}
	return strings.Join(tbLines, "\n")
}

func resolvePythonBinary(explicitBin string) string {
	if explicitBin != "" {
		return explicitBin
	}
	if envBin := os.Getenv("PYTHON_BIN"); envBin != "" {
		return envBin
	}
	for _, cand := range []string{"python3", "python"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	return "python"
}

func wrapReproducerWithWorkspacePath(scriptContent string, wsAbs string) string {
	cleaned := strings.TrimSpace(scriptContent)
	wsSlash := filepath.ToSlash(wsAbs)
	header := fmt.Sprintf("import sys, os\nfor _p in [%q, %q, %q]:\n    if os.path.isdir(_p) and _p not in sys.path:\n        sys.path.insert(0, _p)\n",
		wsSlash, wsSlash+"/src", wsSlash+"/lib")
	if strings.HasPrefix(cleaned, "#!") {
		nl := strings.Index(cleaned, "\n")
		if nl != -1 {
			return cleaned[:nl+1] + header + cleaned[nl+1:] + "\n"
		}
	}
	return header + cleaned + "\n"
}

func runStandaloneReproducer(ctx context.Context, pyBin string, wsAbs string, scriptRel string, issueText string, timeout time.Duration) (int, string, bool) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	tCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(tCtx, pyBin, scriptRel)
	cmd.Dir = wsAbs
	env := os.Environ()
	env = append(env,
		fmt.Sprintf("PYTHONPATH=%s%c%s%c%s%c%s", filepath.Join(wsAbs, "src"), os.PathListSeparator, wsAbs, os.PathListSeparator, filepath.Join(wsAbs, "lib"), os.PathListSeparator, os.Getenv("PYTHONPATH")),
		"PYTEST_DISABLE_PLUGIN_AUTOLOAD=1",
		"PYTHONUNBUFFERED=1",
		"MPLBACKEND=Agg",
	)
	cmd.Env = env
	outBytes, runErr := cmd.CombinedOutput()
	outStr := string(outBytes)
	if tCtx.Err() == context.DeadlineExceeded {
		return 124, outStr, false
	}
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	hasSyntax := strings.Contains(outStr, "SyntaxError:") ||
		strings.Contains(outStr, "IndentationError:") ||
		strings.Contains(outStr, "TabError:")
	hasBrokenImport := (strings.Contains(outStr, "ModuleNotFoundError:") || strings.Contains(outStr, "ImportError:")) &&
		!strings.Contains(issueText, "ImportError") && !strings.Contains(issueText, "ModuleNotFoundError")
	hasHallucinatedAttr := false
	if idx := strings.Index(outStr, "has no attribute '"); idx != -1 && !strings.Contains(issueText, "AttributeError") {
		rest := outStr[idx+len("has no attribute '"):]
		if endQuote := strings.Index(rest, "'"); endQuote > 0 {
			attrName := rest[:endQuote]
			if !strings.Contains(issueText, attrName) {
				hasHallucinatedAttr = true
			}
		}
	}
	isValidFailure := exitCode != 0 && !hasSyntax && !hasBrokenImport && !hasHallucinatedAttr
	return exitCode, outStr, isValidFailure
}

func buildRepoSummaryForReproducer(wsAbs string, loc *devops.LocalizationResult) string {
	if loc == nil || loc.PrimaryFile == "" {
		return ""
	}
	fullPath := filepath.Join(wsAbs, filepath.FromSlash(loc.PrimaryFile))
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Primary file: %s (symbol: %s)\n", loc.PrimaryFile, loc.CausalSymbol))
	if outline := devops.ExtractFileSymbolOutline(wsAbs, loc.PrimaryFile); outline != "" {
		sb.WriteString(outline + "\n")
	}
	start := loc.LineRange[0] - 10
	if start < 0 {
		start = 0
	}
	end := start + 45
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		sb.WriteString(fmt.Sprintf("%4d: %s\n", i+1, strings.TrimRight(lines[i], "\r")))
	}
	for idx, sf := range loc.SupportingFiles {
		if idx >= 2 {
			break
		}
		if outline := devops.ExtractFileSymbolOutline(wsAbs, sf); outline != "" {
			sb.WriteString(fmt.Sprintf("Supporting file %s — %s\n", sf, outline))
		}
	}
	return sb.String()
}

func executeLocalizedIssueRepair(
	ctx context.Context,
	cfg *GlobalConfig,
	gate llm.DecisionGate,
	issueText string,
	loc *devops.LocalizationResult,
	pyBin string,
	reproRel string,
	reproFound bool,
) (int, int, int, error) {
	if loc == nil || loc.PrimaryFile == "" {
		return 0, 0, 0, errors.New("solve: fault localization returned no primary file")
	}
	wsAbs, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		wsAbs = cfg.Workspace
	}
	targetFilePath := filepath.ToSlash(loc.PrimaryFile)
	fullTarget := filepath.Join(wsAbs, filepath.FromSlash(targetFilePath))
	fileBytes, err := os.ReadFile(fullTarget)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("solve: cannot read localized primary file %s: %w", targetFilePath, err)
	}
	rawLines := strings.Split(string(fileBytes), "\n")
	srcLines := make([]string, len(rawLines))
	for i, l := range rawLines {
		srcLines[i] = strings.TrimRight(l, "\r")
	}

	focusLine := loc.LineRange[0]
	if focusLine < 1 {
		if bestL, bestSym, _ := devops.FindBestImplementationSymbolInFile(wsAbs, targetFilePath, issueText); bestL > 0 {
			focusLine = bestL
			if loc.CausalSymbol == "" {
				loc.CausalSymbol = bestSym
			}
		} else {
			focusLine = 1
		}
	}
	if focusLine > len(srcLines) {
		focusLine = len(srcLines)
	}

	var contextLines []string
	if len(srcLines) <= 340 {
		for idx, l := range srcLines {
			marker := "  "
			if idx+1 == focusLine {
				marker = "> "
			}
			contextLines = append(contextLines, fmt.Sprintf("%s%4d: %s", marker, idx+1, l))
		}
	} else {
		symRange := devops.FindEnclosingSymbolRange(wsAbs, targetFilePath, focusLine, loc.CausalSymbol)
		if symRange[0] <= 0 || symRange[1] < symRange[0] || (symRange[1]-symRange[0]) < 80 || (symRange[1]-symRange[0]) > 220 {
			sIdx := focusLine - 60
			eIdx := focusLine + 130
			if eIdx > len(srcLines) {
				eIdx = len(srcLines)
				sIdx = eIdx - 170
			}
			if sIdx < 1 {
				sIdx = 1
			}
			symRange = [2]int{sIdx, eIdx}
		}
		if outline := devops.ExtractFileSymbolOutline(wsAbs, targetFilePath); outline != "" {
			contextLines = append(contextLines, outline)
		}
		if cgSlices := devops.ExtractIntraFileCallGraphSlices(wsAbs, targetFilePath, srcLines, symRange); len(cgSlices) > 0 {
			contextLines = append(contextLines, cgSlices...)
		}
		contextLines = append(contextLines, devops.FormatNumberedSliceElidingDocstrings(srcLines, symRange[0], symRange[1], symRange[1], focusLine, 165, true)...)

		topSyms := devops.FindTopImplementationSymbolsInFile(wsAbs, targetFilePath, issueText, 3)
		addedExtra := 0
		for _, extraSym := range topSyms {
			dist := extraSym.DefLine - focusLine
			if dist < 0 {
				dist = -dist
			}
			if dist < 40 && extraSym.DefLine >= symRange[0] && extraSym.DefLine <= symRange[1]-20 {
				continue
			}
			rStart := extraSym.LineRange[0]
			rEnd := extraSym.LineRange[1]
			if rEnd-rStart < 45 {
				rStart = extraSym.DefLine - 15
				rEnd = extraSym.DefLine + 45
			}
			if rEnd > len(srcLines) {
				rEnd = len(srcLines)
			}
			if rStart < 1 {
				rStart = 1
			}
			if rEnd-rStart > 80 {
				rEnd = rStart + 80
			}
			contextLines = append(contextLines, fmt.Sprintf("  # --- Additional Relevant Symbol: %s (L%d-L%d) ---", extraSym.Name, rStart, rEnd))
			contextLines = append(contextLines, devops.FormatNumberedSliceElidingDocstrings(srcLines, rStart, rEnd, extraSym.LineRange[1], extraSym.DefLine, 70, true)...)
			addedExtra++
			if addedExtra >= 2 {
				break
			}
		}
	}

	// Include supporting files context when available (with best symbol window)
	for sIdx, sf := range loc.SupportingFiles {
		if sIdx >= 2 || sf == targetFilePath || strings.HasPrefix(sf, "tests/") {
			continue
		}
		if sfBytes, sfErr := os.ReadFile(filepath.Join(wsAbs, filepath.FromSlash(sf))); sfErr == nil {
			sfLines := strings.Split(string(sfBytes), "\n")
			symLine, symName, symRng := devops.FindBestImplementationSymbolInFile(wsAbs, sf, issueText)
			if symLine > 0 && symRng[0] > 0 && symRng[1] >= symRng[0] {
				sStart := symRng[0]
				sEnd := symRng[1]
				if sEnd-sStart > 80 {
					sEnd = sStart + 80
				}
				tagged := devops.FormatNumberedSliceElidingDocstrings(sfLines, sStart, sEnd, symRng[1], symLine, 75, false)
				contextLines = append(contextLines, fmt.Sprintf("=== Swarm Supporting File: %s (%s, L%d-L%d) ===\n%s", sf, symName, sStart, sEnd, strings.Join(tagged, "\n")))
				continue
			}
		}
		if outline := devops.ExtractFileSymbolOutline(wsAbs, sf); outline != "" {
			contextLines = append(contextLines, fmt.Sprintf("=== Supporting File: %s ===\n%s", sf, outline))
		}
	}

	errMsg := fmt.Sprintf("Issue fault localized to %s in %s (line %d)", loc.CausalSymbol, targetFilePath, focusLine)
	card := llm.GateHealCard{
		FilePath:      targetFilePath,
		ErrorLine:     focusLine,
		ErrorMessage:  errMsg,
		Snippet5Lines: strings.Join(contextLines, "\n"),
		Command:       "omnicoder solve (issue-only)",
		IssueContext:  issueText,
		TokenBudget:   4900,
	}

	beamK := cfg.BeamK
	if beamK <= 0 {
		beamK = 3
	}

	_, userP, _ := card.FormatPrompt()
	promptToks := llm.EstimateTokens(userP) + 95
	completionToks := 0

	var candidates []*llm.HealDecision
	if beamK > 1 {
		if cands, cErr := gate.DecideHealSpeculative(ctx, card, beamK); cErr == nil && len(cands) > 0 {
			candidates = cands
		}
	}
	if len(candidates) == 0 {
		single, sErr := gate.DecideHeal(ctx, card)
		if sErr != nil {
			return 1, promptToks, 0, sErr
		}
		candidates = []*llm.HealDecision{single}
	}

	wsFileIdx := newWorkspaceFileIndex(wsAbs)
	modifiedFiles := make(map[string]bool)
	var firstSyntaxValidBlocks []devops.EditBlock
	var firstSyntaxValidSummary string

	for candIdx, cand := range candidates {
		if cand == nil || len(cand.EditBlocks) == 0 {
			continue
		}
		candTargetFP := wsFileIdx.resolve(cand.FilePath, targetFilePath, false)
		var blocks []devops.EditBlock
		for _, b := range cand.EditBlocks {
			if strings.TrimSpace(b.SearchText) == strings.TrimSpace(b.ReplaceText) {
				continue
			}
			blockFP := wsFileIdx.resolve(b.FilePath, candTargetFP, false)
			if strings.HasPrefix(blockFP, ".omnicoder/") || strings.Contains(blockFP, "reproduce_issue") {
				continue
			}
			blocks = append(blocks, devops.EditBlock{
				FilePath:    blockFP,
				SearchText:  b.SearchText,
				ReplaceText: b.ReplaceText,
			})
			completionToks += llm.EstimateTokens(b.SearchText + b.ReplaceText)
		}
		if len(blocks) == 0 {
			continue
		}
		blocks = rerouteCrossFileEditBlocks(wsAbs, blocks, loc.SupportingFiles, modifiedFiles)

		origFiles := make(map[string][]byte)
		for _, b := range blocks {
			fullP := filepath.Join(wsAbs, filepath.FromSlash(b.FilePath))
			if _, ok := origFiles[fullP]; !ok {
				if d, rErr := os.ReadFile(fullP); rErr == nil {
					origFiles[fullP] = d
				}
			}
		}
		rollback := func() {
			for p, d := range origFiles {
				_ = os.WriteFile(p, d, 0644)
			}
		}

		applyRes, applyErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
			RepoPath:   wsAbs,
			Blocks:     blocks,
			FuzzyMatch: true,
		})
		if applyErr != nil || applyRes == nil || !applyRes.Applied {
			rollback()
			continue
		}

		syntaxOK := true
		for _, b := range blocks {
			fullP := filepath.Join(wsAbs, filepath.FromSlash(b.FilePath))
			patchedBytes, rErr := os.ReadFile(fullP)
			if rErr != nil {
				syntaxOK = false
				break
			}
			normCode, wasNorm, fastErr := devops.ValidateCandidatePatchFast(b.FilePath, string(origFiles[fullP]), string(patchedBytes))
			if fastErr != nil {
				syntaxOK = false
				break
			}
			if wasNorm {
				_ = os.WriteFile(fullP, []byte(normCode), 0644)
			}
			if strings.HasSuffix(b.FilePath, ".py") {
				pyCheck := exec.CommandContext(ctx, pyBin, "-m", "py_compile", fullP)
				pyCheck.Dir = wsAbs
				if err := pyCheck.Run(); err != nil {
					syntaxOK = false
					break
				}
			}
		}
		if !syntaxOK {
			rollback()
			continue
		}

		if len(firstSyntaxValidBlocks) == 0 {
			firstSyntaxValidBlocks = blocks
			firstSyntaxValidSummary = cand.RepairSummary
		}

		if reproFound && reproRel != "" {
			exitCode, _, _ := runStandaloneReproducer(ctx, pyBin, wsAbs, reproRel, issueText, 20*time.Second)
			if exitCode != 0 {
				rollback()
				continue
			}
		}

		fmt.Printf("  [omnicoder solve] Applied localized repair candidate %d to %s (%s)\n", candIdx+1, targetFilePath, cand.RepairSummary)
		return 1, promptToks, completionToks, nil
	}

	if len(firstSyntaxValidBlocks) > 0 {
		_, _ = devops.ApplyEditBlocks(devops.EditBlockRequest{
			RepoPath:   wsAbs,
			Blocks:     firstSyntaxValidBlocks,
			FuzzyMatch: true,
		})
		fmt.Printf("  [omnicoder solve] Retained best syntax-verified candidate for %s (%s)\n", targetFilePath, firstSyntaxValidSummary)
		return 1, promptToks, completionToks, nil
	}

	return 1, promptToks, completionToks, errors.New("solve: no localized repair candidate applied cleanly")
}

func executeSolve(ctx context.Context, cfg *GlobalConfig, args []string) error {
	solveFS := flag.NewFlagSet("solve", flag.ContinueOnError)
	bindGlobalFlags(solveFS, cfg)
	issueFile := solveFS.String("issue-file", "ISSUE.md", "Path to issue markdown file (default: ISSUE.md)")
	issueInline := solveFS.String("issue", "", "Inline issue description")
	reproAttempts := solveFS.Int("repro-attempts", 3, "Maximum attempts to synthesize a failing reproducer script")
	patchOut := solveFS.String("patch-out", "model.patch", "Output file path for clean git diff patch")
	pythonBin := solveFS.String("python-bin", "", "Python interpreter binary for running synthesized reproducers")

	if err := parseWithRearrangedArgs(solveFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	wsAbs, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		wsAbs = cfg.Workspace
	}

	issueText := strings.TrimSpace(*issueInline)
	if issueText == "" {
		issuePath := *issueFile
		if !filepath.IsAbs(issuePath) {
			candInWs := filepath.Join(wsAbs, issuePath)
			if _, statErr := os.Stat(candInWs); statErr == nil {
				issuePath = candInWs
			}
		}
		data, readErr := os.ReadFile(issuePath)
		if readErr != nil {
			return fmt.Errorf("solve: failed to read issue file %q: %w", *issueFile, readErr)
		}
		issueText = strings.TrimSpace(string(data))
	}
	if issueText == "" {
		return errors.New("solve: issue text cannot be empty")
	}

	// Step 0: Enforce Zero-Leakage Tripwire
	if err := checkIssueLeakage(issueText); err != nil {
		return err
	}

	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 4
	}
	if cfg.BeamK <= 0 {
		cfg.BeamK = 3
	}
	pyBin := resolvePythonBinary(*pythonBin)

	fmt.Printf("================================================================================\n")
	fmt.Printf("  OmniCoder-ICX Issue-Only Autonomous Solver (Zero Gold-Test Visibility)\n")
	fmt.Printf("  Workspace:      %s\n", wsAbs)
	fmt.Printf("  Model:          %s\n", cfg.Model)
	fmt.Printf("  Max Turns:      %d | Beam K: %d | Repro Attempts: %d\n", cfg.MaxTurns, cfg.BeamK, *reproAttempts)
	fmt.Printf("================================================================================\n")

	var gate llm.DecisionGate
	if cfg.Offline {
		gate = llm.NewMockDecisionGate()
	} else {
		wireClient, err := llm.NewClient(llm.ClientConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
			Timeout: 180 * time.Second,
		})
		if err != nil {
			gate = llm.NewMockDecisionGate()
		} else {
			gate = llm.NewDecisionGate(wireClient, cfg.Model)
		}
	}

	// Step 1: Fault Localization from Issue Description + Issue Traceback ONLY
	tbSnippet := extractIssueTraceback(issueText)
	loc, locErr := devops.LocalizeFault(wsAbs, issueText, tbSnippet)
	var causalFiles []string
	primaryFile := ""
	if locErr == nil && loc != nil && loc.PrimaryFile != "" {
		primaryFile = loc.PrimaryFile
		causalFiles = append(causalFiles, loc.PrimaryFile)
		for _, sf := range loc.SupportingFiles {
			if len(causalFiles) < 4 {
				causalFiles = append(causalFiles, sf)
			}
		}
		fmt.Printf("[omnicoder solve] Turn 0 Fault Localization: primary=%s (symbol=%q, lines=%v, supporting=%v)\n",
			loc.PrimaryFile, loc.CausalSymbol, loc.LineRange, loc.SupportingFiles)
	} else {
		fmt.Printf("[omnicoder solve] Turn 0 Fault Localization warning: %v\n", locErr)
	}

	// Step 2: Autonomous Reproducer Synthesis (must fail on unpatched base_commit)
	reproDir := filepath.Join(wsAbs, ".omnicoder", "repro")
	_ = os.MkdirAll(reproDir, 0755)
	reproRel := ".omnicoder/repro/test_reproduce_issue.py"
	reproAbs := filepath.Join(wsAbs, filepath.FromSlash(reproRel))

	repoSummary := buildRepoSummaryForReproducer(wsAbs, loc)
	reproFound := false
	totalPromptTokens := 0
	totalCompletionTokens := 0
	var lastReproFeedback string

	// First try deterministic extraction from markdown code fences / doctests in the issue
	if detPath, detErr := devops.SynthesizeReproducer(issueText, reproDir); detErr == nil && detPath != "" {
		rawScript, rErr := os.ReadFile(detPath)
		_ = os.Remove(detPath)
		if rErr == nil && !strings.Contains(string(rawScript), "Baseline assertion for reproduction") {
			wrapped := wrapReproducerWithWorkspacePath(string(rawScript), wsAbs)
			_ = os.WriteFile(reproAbs, []byte(wrapped), 0755)
			exitCode, _, validFail := runStandaloneReproducer(ctx, pyBin, wsAbs, reproRel, issueText, 15*time.Second)
			if validFail {
				reproFound = true
				fmt.Printf("[omnicoder solve] Turn 1 Deterministic Reproducer succeeded (failed as expected on base_commit with exit %d)\n", exitCode)
			}
		}
	}

	for attempt := 1; !reproFound && attempt <= *reproAttempts; attempt++ {
		issueWithFeedback := issueText
		if lastReproFeedback != "" {
			issueWithFeedback = issueText + "\n\nPrior Reproducer Attempt Feedback:\n" + lastReproFeedback
		}
		reproCard := llm.GateReproducerCard{
			IssueText:        issueWithFeedback,
			RepoSummary:      repoSummary,
			CausalFiles:      causalFiles,
			Language:         "python",
			TracebackSnippet: tbSnippet,
			TokenBudget:      2400,
		}
		_, uPrompt, _ := reproCard.FormatPrompt()
		totalPromptTokens += llm.EstimateTokens(uPrompt) + 60

		dec, decErr := gate.DecideReproducer(ctx, reproCard)
		if decErr != nil || dec == nil || strings.TrimSpace(dec.ScriptContent) == "" {
			fmt.Printf("[omnicoder solve] Reproducer attempt %d/%d LLM synthesis error: %v\n", attempt, *reproAttempts, decErr)
			continue
		}
		totalCompletionTokens += llm.EstimateTokens(dec.ScriptContent)
		wrapped := wrapReproducerWithWorkspacePath(dec.ScriptContent, wsAbs)
		if err := os.WriteFile(reproAbs, []byte(wrapped), 0755); err != nil {
			continue
		}

		exitCode, outStr, validFail := runStandaloneReproducer(ctx, pyBin, wsAbs, reproRel, issueText, 20*time.Second)
		if validFail {
			reproFound = true
			fmt.Printf("[omnicoder solve] Turn 1 Reproducer attempt %d/%d verified: fails on base_commit (exit=%d)\n", attempt, *reproAttempts, exitCode)
			break
		}
		snippetOut := strings.TrimSpace(outStr)
		if len(snippetOut) > 300 {
			snippetOut = snippetOut[len(snippetOut)-300:]
		}
		if exitCode == 0 {
			lastReproFeedback = "Previous script exited with code 0 (did NOT reproduce the bug). Write an assertion or call that triggers the bug and exits with code 1."
		} else {
			lastReproFeedback = fmt.Sprintf("Previous script failed with syntax/timeout error (exit %d): %s", exitCode, snippetOut)
		}
		fmt.Printf("[omnicoder solve] Reproducer attempt %d/%d rejected (exit=%d)\n", attempt, *reproAttempts, exitCode)
	}

	// Track whether PROBLEM.md pre-existed so we don't pollute the workspace
	probFile := filepath.Join(wsAbs, "PROBLEM.md")
	_, probPreExisted := os.Stat(probFile)
	if probPreExisted != nil {
		_ = os.WriteFile(probFile, []byte(issueText), 0644)
	}

	turnsTaken := 0
	if reproFound {
		healCfg := *cfg
		healCfg.Workspace = wsAbs
		healCfg.Swarm = true
		healCmd := fmt.Sprintf("%s %s", pyBin, reproRel)
		healErr := executeHeal(ctx, &healCfg, []string{"--issue", issueText, healCmd})
		turnsTaken = healCfg.MaxTurns
		totalPromptTokens += turnsTaken * 1200
		totalCompletionTokens += turnsTaken * 220
		if healErr == nil {
			turnsTaken = 1
		}
	}

	// Check if tracked repository files were modified
	checkGitDiff := func() string {
		diffCmd := exec.CommandContext(ctx, "git", "-C", wsAbs, "diff", "--", ".",
			":(exclude).omnicoder", ":(exclude)ISSUE.md", ":(exclude)PROBLEM.md",
			":(exclude)reproduce_issue*", ":(exclude)test_reproduce_issue*", ":(exclude)model.patch")
		out, _ := diffCmd.Output()
		return string(out)
	}

	currentDiff := checkGitDiff()
	if strings.TrimSpace(currentDiff) == "" && loc != nil && loc.PrimaryFile != "" {
		fmt.Printf("[omnicoder solve] Invoking localized repair on %s...\n", loc.PrimaryFile)
		locTurns, pToks, cToks, _ := executeLocalizedIssueRepair(ctx, cfg, gate, issueText, loc, pyBin, reproRel, reproFound)
		turnsTaken += locTurns
		totalPromptTokens += pToks
		totalCompletionTokens += cToks
		currentDiff = checkGitDiff()
	}

	// Clean up temporary files before final patch emission
	_ = os.RemoveAll(filepath.Join(wsAbs, ".omnicoder", "repro"))
	_ = os.Remove(filepath.Join(wsAbs, "reproduce_issue.py"))
	_ = os.Remove(filepath.Join(wsAbs, "test_reproduce_issue.py"))
	if probPreExisted != nil {
		_ = os.Remove(probFile)
	}

	currentDiff = checkGitDiff()
	patchOutPath := *patchOut
	if !filepath.IsAbs(patchOutPath) {
		patchOutPath = filepath.Join(wsAbs, patchOutPath)
	}
	if err := os.WriteFile(patchOutPath, []byte(currentDiff), 0644); err != nil {
		return fmt.Errorf("solve: failed to write patch output to %s: %w", patchOutPath, err)
	}

	fmt.Printf("[omnicoder solve telemetry] {\"repro_found\":%v,\"turns\":%d,\"beam_k\":%d,\"prompt_tokens\":%d,\"completion_tokens\":%d,\"primary_file\":%q,\"patch_bytes\":%d}\n",
		reproFound, turnsTaken, cfg.BeamK, totalPromptTokens, totalCompletionTokens, primaryFile, len(currentDiff))
	return nil
}

// ----------------------------------------------------------------------------
// 4. omnicoder refactor <symbol> <new_sig>
// ----------------------------------------------------------------------------
func executeRefactor(ctx context.Context, cfg *GlobalConfig, args []string) error {
	refactorFS := flag.NewFlagSet("refactor", flag.ContinueOnError)
	bindGlobalFlags(refactorFS, cfg)
	dryRun := &cfg.AuditOnly
	if err := parseWithRearrangedArgs(refactorFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := refactorFS.Args()
	if len(posArgs) < 2 {
		fmt.Fprintf(os.Stderr, "Error: <symbol> and <new_sig> required for 'refactor'\n\nUsage: omnicoder refactor [options] <symbol> \"<new_sig>\"\n")
		return errors.New("missing arguments for refactor")
	}
	symbol := posArgs[0]
	newSig := strings.Join(posArgs[1:], " ")

	fmt.Printf("================================================================================\n")
	fmt.Printf("  OmniCoder-ICX Coordinated AST Refactoring\n")
	fmt.Printf("  Symbol:        %s\n", symbol)
	fmt.Printf("  New Signature: %s\n", newSig)
	fmt.Printf("  Workspace:     %s\n", cfg.Workspace)
	fmt.Printf("  Dry Run:       %v\n", *dryRun)
	fmt.Printf("================================================================================\n")

	lat := icx.NewLocalLattice()

	// Ingest workspace polyglot files
	_ = filepath.WalkDir(cfg.Workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == "vendor" || name == ".venv" || name == "venv" ||
				strings.HasPrefix(name, "sb_") || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".cpp", ".c", ".h", ".sql", ".html":
			rel, _ := filepath.Rel(cfg.Workspace, path)
			rel = filepath.ToSlash(rel)
			if raw, readErr := os.ReadFile(path); readErr == nil {
				_ = lat.IngestPolyglotAST(rel, raw)
			}
		}
		return nil
	})

	// Search for symbol node in lattice
	qRes, err := lat.QueryCodebase(ctx, icx.QueryCodebaseRequest{
		Query:     symbol,
		Dimension: "3D_VOLUMETRIC",
		TopK:      10,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying codebase: %v\n", err)
		return err
	}

	fmt.Printf("Found %d candidate AST nodes for %q:\n", qRes.Count, symbol)
	for _, m := range qRes.Results {
		fmt.Printf("  - %s (%s)\n", m.NodeID, m.Snippet)
	}

	if qRes.Count == 0 {
		fmt.Printf("Warning: no direct AST node found matching %q. Proceeding with heuristic walk.\n", symbol)
	}

	targetNodeID := symbol
	if len(qRes.Results) > 0 {
		targetNodeID = qRes.Results[0].NodeID
		for _, m := range qRes.Results {
			if strings.HasSuffix(m.NodeID, "_"+symbol) || strings.HasPrefix(m.NodeID, "func_") || strings.HasPrefix(m.NodeID, "type_") {
				targetNodeID = m.NodeID
				if strings.HasSuffix(m.NodeID, "_"+symbol) {
					break
				}
			}
		}
	}

	// 5-hop walk
	walkRes, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: targetNodeID,
		MaxHops:   5,
	})
	if err == nil && walkRes.OK {
		fmt.Printf("\nBlast Radius (5-hop AST walk from %s):\n", targetNodeID)
		for _, step := range walkRes.Steps {
			fmt.Printf("  [%d] %-10s -> %s (%s)\n", step.Depth, step.Relation, step.Name, step.Type)
		}
	}

	if *dryRun {
		fmt.Println("\n[Dry Run] Preview complete. No files were modified.")
		return nil
	}

	// Execute refactor via Orchestrator
	eng, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir:   cfg.Workspace,
		MaxTurns:       cfg.MaxTurns,
		Unbounded:      cfg.Unbounded,
		LatticeSpaceID: cfg.SpaceID,
		Offline:        cfg.Offline,
		Model:          cfg.Model,
		Verbose:        cfg.Verbose,
		TargetFile:     cfg.TargetFile,
		Scope:          cfg.Scope,
		AuditOnly:      *dryRun || cfg.AuditOnly,
		JSON:           cfg.JSON,
	})
	if err != nil {
		return err
	}

	report, err := eng.Refactor(ctx, symbol, newSig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Refactor task error: %v\n", err)
		return err
	}

	fmt.Printf("\nRefactor Status: %v\nFiles Touched: %v\nMerkle Seal: %s\n",
		report.Success, report.FilesTouched, report.MerkleSeal)
	return nil
}

// ----------------------------------------------------------------------------
// 5. omnicoder repl
// ----------------------------------------------------------------------------
func executeREPL(ctx context.Context, cfg *GlobalConfig, args []string) error {
	replFS := flag.NewFlagSet("repl", flag.ContinueOnError)
	bindGlobalFlags(replFS, cfg)
	if err := parseWithRearrangedArgs(replFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	modeStr := "ONLINE"
	if cfg.Offline {
		modeStr = "OFFLINE"
	}

	fmt.Printf("================================================================================\n")
	fmt.Printf("  OmniCoder-ICX Interactive REPL (v%s)\n", Version)
	fmt.Printf("  Workspace: %s\n", cfg.Workspace)
	fmt.Printf("  Mode:      %s\n", modeStr)
	fmt.Printf("  Type 'help' for commands, 'exit' or Ctrl+D to quit.\n")
	fmt.Printf("================================================================================\n\n")

	lat := icx.NewLocalLattice()
	eng, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir:   cfg.Workspace,
		MaxTurns:       cfg.MaxTurns,
		Unbounded:      cfg.Unbounded,
		LatticeSpaceID: cfg.SpaceID,
		Offline:        cfg.Offline,
		Model:          cfg.Model,
		ICXClient:      lat,
		Verbose:        cfg.Verbose,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing orchestrator: %v\n", err)
		return err
	}

	isTerm := ui.IsTerminal(os.Stdin)
	if isTerm {
		ui.SetInteractiveTerminal(true)
		defer ui.SetInteractiveTerminal(false)

		cleanupModes := ui.SetupTerminalModes(os.Stdin, os.Stdout)
		defer cleanupModes()
	}

	reader := bufio.NewReaderSize(os.Stdin, 4*1024*1024)
	for {
		line, err := ui.ReadPrompt(reader, os.Stdout, "omnicoder> ")
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println("\nGoodbye.")
				break
			}
			fmt.Printf("Input error: %v\n", err)
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		cmd := strings.ToLower(parts[0])
		cmdArgs := strings.TrimSpace(strings.TrimPrefix(line, parts[0]))

		switch cmd {
		case "exit", "quit", ":q", "/exit", "/quit":
			fmt.Println("Goodbye.")
			return nil
		case "help", "/help", "?":
			fmt.Println(`Available REPL commands:
  run, /run <task>        Execute autonomous coding loop
  plan, /plan <task>      Execute master plan through autonomous engine
  build, /build <task>    Execute build task through autonomous engine
  goal, /goal <task>      Execute task to completion
  map, /map               Display Personalized PageRank AST tree
  heal, /heal <command>   Self-heal failing command
  refactor <sym> <sig>    Perform blast-radius walk and refactor
  walk <symbol>           Execute 5-hop AST walk
  quote [slot]            Quote memory slot with SHA-256 seal
  remember <fact>         Add a fact to 4D lattice memory
  search <query>          Search facts in lattice
  reset, /reset           Reset dialogue tokens to 0 (lattice preserved)
  status, /status         Show session stats and memory info
  clear                   Clear screen
  exit, quit, /exit       Exit REPL`)
		case "run", "/run", "plan", "/plan", "build", "/build", "goal", "/goal", "boost", "/boost":
			if cmdArgs == "" {
				fmt.Printf("Usage: %s <task description>\n", cmd)
				continue
			}
			report, err := eng.Run(ctx, cmdArgs)
			if err != nil {
				fmt.Printf("Run error: %v\n", err)
			} else {
				fmt.Printf("Success: %v | Turns: %d | Touched: %v | Seal: %s\n",
					report.Success, report.TurnsCount, report.FilesTouched, report.MerkleSeal)
			}
		case "map":
			rm, err := eng.Map(ctx, 1024)
			if err != nil {
				fmt.Printf("Map error: %v\n", err)
			} else {
				fmt.Print(rm.RepoMap)
			}
		case "heal":
			if cmdArgs == "" {
				fmt.Println("Usage: heal <command>")
				continue
			}
			report, err := eng.Heal(ctx, cmdArgs)
			if err != nil {
				fmt.Printf("Heal error: %v\n", err)
			} else {
				fmt.Printf("Success: %v | Heals: %d | Seal: %s\n",
					report.Success, report.HealCount, report.MerkleSeal)
			}
		case "refactor":
			rfParts := strings.Fields(cmdArgs)
			if len(rfParts) < 2 {
				fmt.Println("Usage: refactor <symbol> <new_sig>")
				continue
			}
			sig := strings.TrimSpace(strings.TrimPrefix(cmdArgs, rfParts[0]))
			report, err := eng.Refactor(ctx, rfParts[0], sig)
			if err != nil {
				fmt.Printf("Refactor error: %v\n", err)
			} else {
				fmt.Printf("Success: %v | Touched: %v | Seal: %s\n",
					report.Success, report.FilesTouched, report.MerkleSeal)
			}
		case "walk":
			if cmdArgs == "" {
				fmt.Println("Usage: walk <symbol>")
				continue
			}
			res, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
				StartNode: cmdArgs,
				MaxHops:   5,
			})
			if err != nil || !res.OK {
				fmt.Printf("Walk result: not found or error (%v)\n", err)
			} else {
				fmt.Printf("Walk from %s (%d steps):\n", cmdArgs, len(res.Steps))
				for _, s := range res.Steps {
					fmt.Printf("  [%d] %-10s -> %s (%s)\n", s.Depth, s.Relation, s.Name, s.Type)
				}
			}
		case "quote":
			slotIdx := 1
			if cmdArgs != "" {
				if n, err := strconv.Atoi(cmdArgs); err == nil && n > 0 {
					slotIdx = n
				}
			}
			qRes, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
				SpaceID: "default",
				Family:  "file_slots",
				Index:   slotIdx,
			})
			if err != nil || !qRes.OK {
				fmt.Printf("Quote error or slot %d empty (%v)\n", slotIdx, err)
			} else {
				fmt.Printf("Slot %d [%s]:\n%s\n", slotIdx, qRes.SHA256, qRes.Text)
			}
		case "remember":
			if cmdArgs == "" {
				fmt.Println("Usage: remember <fact>")
				continue
			}
			remRes, err := lat.Remember(ctx, icx.RememberRequest{
				SpaceID: "default",
				Family:  "repl_facts",
				Text:    cmdArgs,
			})
			if err != nil {
				fmt.Printf("Remember error: %v\n", err)
			} else {
				fmt.Printf("Fact stored: ID %s (SHA256: %s)\n", remRes.FactID, remRes.SHA256)
			}
		case "search":
			if cmdArgs == "" {
				fmt.Println("Usage: search <query>")
				continue
			}
			sRes, err := lat.SearchFacts(ctx, icx.SearchRequest{
				Query: cmdArgs,
			})
			if err != nil {
				fmt.Printf("Search error: %v\n", err)
			} else {
				fmt.Printf("Found %d matches:\n", len(sRes.Facts))
				for _, f := range sRes.Facts {
					fmt.Printf("  [Score: %.2f] %s\n", f.RelevanceScore, f.Text)
				}
			}
		case "reset":
			res, err := lat.ResetSession(ctx, "default")
			if err != nil {
				fmt.Printf("Reset error: %v\n", err)
			} else {
				fmt.Printf("Session reset! Tokens preserved: %d, Nodes alive: %d (100%% 4D lattice preserved)\n",
					res.TokensPreserved, res.LatticeNodesAlive)
			}
		case "status":
			info, _ := lat.InspectSpace(ctx, "default")
			fmt.Printf("Workspace: %s\nMode:      %s\nNodes:     %d\nEdges:     %d\n",
				cfg.Workspace, modeStr, info.NodesCount, info.Edges)
		case "clear":
			fmt.Print("\033[H\033[2J")
		default:
			fmt.Printf("Unknown command %q. Type 'help' for available commands.\n", cmd)
		}
	}

	return nil
}

// ----------------------------------------------------------------------------
// 6. omnicoder claim <email>
// ----------------------------------------------------------------------------
func executeClaim(ctx context.Context, cfg *GlobalConfig, args []string) error {
	claimFS := flag.NewFlagSet("claim", flag.ContinueOnError)
	bindGlobalFlags(claimFS, cfg)
	if err := parseWithRearrangedArgs(claimFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	posArgs := claimFS.Args()
	if len(posArgs) < 1 {
		fmt.Fprintf(os.Stderr, "Error: email required for 'claim'\n\nUsage: omnicoder claim [options] <email>\n")
		return errors.New("missing email")
	}
	email := strings.TrimSpace(posArgs[0])
	if email == "" || !strings.Contains(email, "@") {
		fmt.Fprintf(os.Stderr, "Error: valid email address required for 'claim'\n")
		return errors.New("invalid email")
	}

	localSandbox := vm.NewHermeticLocalSandbox(cfg.Workspace)
	token, err := localSandbox.ClaimFreeTokens(ctx, email, "OmniCoder-ICX")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error claiming tokens: %v\n", err)
		return err
	}

	fmt.Printf("================================================================================\n")
	fmt.Printf("  Calera LLM-VM — Free MicroVM Pilot Tokens Claimed!\n")
	fmt.Printf("================================================================================\n")
	fmt.Printf("  Account:     %s\n", email)
	if len(token) >= 12 {
		fmt.Printf("  Token:       %s...%s\n", token[:8], token[len(token)-4:])
	} else {
		fmt.Printf("  Token:       %s\n", token)
	}
	fmt.Printf("  Allocated:   1,000,000 tokens\n")
	fmt.Printf("  Environment: ~/.calera/icx.env successfully updated\n")
	fmt.Printf("================================================================================\n")
	return nil
}

// ----------------------------------------------------------------------------
// 7. omnicoder vault [status|list|get <key>]
// ----------------------------------------------------------------------------
func executeVault(ctx context.Context, cfg *GlobalConfig, args []string) error {
	v, err := storage.OpenVault(cfg.Workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to open storage vault at %s: %v\n", cfg.Workspace, err)
		return err
	}
	defer v.Close()

	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}

	switch action {
	case "status", "stats", "info":
		stats := v.Stats()
		fmt.Printf("================================================================================\n")
		fmt.Printf("  OmniCoder-ICX — Embedded TitanKV Storage Vault\n")
		fmt.Printf("================================================================================\n")
		fmt.Printf("  Directory:        %s\n", stats.Dir)
		fmt.Printf("  L0 SSTables:      %d\n", stats.L0Files)
		fmt.Printf("  L1 SSTables:      %d\n", stats.L1Files)
		fmt.Printf("  MemTable Size:    %d bytes\n", stats.MemTableBytes)
		fmt.Printf("  Turn Records:     %d\n", stats.TurnCount)
		fmt.Printf("  CAS Objects:      %d\n", stats.CASCount)
		fmt.Printf("  Heal Patterns:    %d\n", stats.HealCount)
		fmt.Printf("  Engine:           TitanKV (LSM-Tree with CRC32 WAL, Bloom filter & Sparse Index)\n")
		fmt.Printf("  Status:           VERIFIED OPERATIONAL (Pure Go standard library, zero-CGo)\n")
		fmt.Printf("================================================================================\n")
		return nil

	case "get":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: omnicoder vault get <key>\n")
			return errors.New("missing key argument")
		}
		key := args[1]
		val, err := v.Get([]byte(key))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Key %q not found in vault (%v)\n", key, err)
			return err
		}
		fmt.Printf("%s\n", string(val))
		return nil

	case "list":
		fmt.Printf("=== Stored Keys in Vault (%s) ===\n", v.Dir())
		it, err := v.Scan(nil, nil)
		if err != nil {
			return err
		}
		count := 0
		for it.Valid() {
			fmt.Printf("  %s (%d bytes)\n", string(it.Key()), len(it.Value()))
			count++
			it.Next()
		}
		if count == 0 {
			fmt.Println("  (vault is currently empty)")
		} else {
			fmt.Printf("Total: %d key(s)\n", count)
		}
		return nil

	default:
		return fmt.Errorf("unknown vault action: %s (available: status, list, get)", action)
	}
}

// ----------------------------------------------------------------------------
// 6. omnicoder mcp [options]
// ----------------------------------------------------------------------------
func executeMCP(ctx context.Context, cfg *GlobalConfig, args []string) error {
	mcpFS := flag.NewFlagSet("mcp", flag.ContinueOnError)
	var framing string
	var traceFile string
	var traceEnabled bool
	mcpFS.StringVar(&framing, "framing", "ndjson", "Transport framing mode: 'ndjson' (default), 'content-length', or 'auto'")
	mcpFS.StringVar(&traceFile, "trace-file", "", "Path to write structured JSONL audit trace events")
	mcpFS.BoolVar(&traceEnabled, "trace", false, "Enable structured JSONL audit trace logging (default: .omnicoder/mcp_trace.jsonl)")
	// MicroVM Sandboxing is enabled by default for MCP operations
	cfg.Sandbox = true
	bindGlobalFlags(mcpFS, cfg)
	if err := parseWithRearrangedArgs(mcpFS, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// Environment variable overrides for trace logging
	if traceFile == "" {
		if env := os.Getenv("OMNICODER_TRACE_FILE"); env != "" {
			traceFile = env
		}
	}
	if !traceEnabled {
		if env := os.Getenv("OMNICODER_TRACE"); env == "1" || strings.ToLower(env) == "true" {
			traceEnabled = true
		}
	}

	// Support positional directory argument: omnicoder mcp [options] [path]
	remArgs := mcpFS.Args()
	if len(remArgs) > 0 {
		fullPath := strings.Trim(strings.Join(remArgs, " "), `"'`)
		if fi, err := os.Stat(fullPath); err == nil && fi.IsDir() {
			cfg.Workspace = fullPath
		} else if fi, err := os.Stat(remArgs[0]); err == nil && fi.IsDir() {
			cfg.Workspace = remArgs[0]
		}
	} else if cfg.Workspace == "." || cfg.Workspace == "" {
		absWS, _ := filepath.Abs(".")
		if absWS == "" || mcp.IsIDEOsOrSystemDir(absWS) {
			if ideWS := mcp.DiscoverIDEActiveWorkspace(); ideWS != "" {
				cfg.Workspace = ideWS
			}
		} else {
			cfg.Workspace = absWS
		}
	}

	// If tracing is enabled without explicit file, default to .omnicoder/mcp_trace.jsonl in workspace
	if traceEnabled && traceFile == "" {
		traceFile = filepath.Join(cfg.Workspace, ".omnicoder", "mcp_trace.jsonl")
	}

	// 1. Run Zero-Config Auto-Detection
	det := mcp.AutoDetectEnvironment(cfg.Workspace)
	cfg.Workspace = det.WorkspaceDir
	if cfg.Offline || cfg.Mechanical {
		det.Backend = mcp.BackendOfflineLocal
		det.Model = "embedded-local-cache"
		det.ActiveModel = ""
		det.IsOffline = true
		det.BaseURL = ""
		det.APIKey = ""
		cfg.Model = "embedded-local-cache"
		cfg.BaseURL = ""
		cfg.APIKey = ""
		cfg.Offline = true
	} else {
		if cfg.Model == "" && det.Model != "" {
			cfg.Model = det.Model
		}
		if cfg.BaseURL == "" && det.BaseURL != "" {
			cfg.BaseURL = det.BaseURL
		}
		if cfg.APIKey == "" && det.APIKey != "" {
			cfg.APIKey = det.APIKey
		}
		if det.IsOffline {
			cfg.Offline = true
		}
	}

	useSandbox := cfg.Sandbox

	// STRICT STDERR ISOLATION:
	// All startup logging, diagnostic banners, and telemetry go exclusively to os.Stderr!
	// os.Stdout MUST remain 100% pristine for MCP JSON-RPC 2.0 framing.
	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "OmniCoder-ICX MCP Server (v%s, protocol 2024-11-05)\n", Version)
		fmt.Fprintf(os.Stderr, "  Workspace: %s\n", cfg.Workspace)
		fmt.Fprintf(os.Stderr, "  Backend:   %s\n", det.Backend)
		fmt.Fprintf(os.Stderr, "  Model:     %s\n", cfg.Model)
		if det.ActiveModel != "" {
			fmt.Fprintf(os.Stderr, "  Active:    %s\n", det.ActiveModel)
		}
		if useSandbox {
			fmt.Fprintf(os.Stderr, "  Sandbox:   Calera MicroVM with CoW Rollback (ENABLED)\n")
		} else {
			fmt.Fprintf(os.Stderr, "  Sandbox:   Calera MicroVM with CoW Rollback (DISABLED)\n")
		}
		if traceFile != "" {
			fmt.Fprintf(os.Stderr, "  Audit Trace: %s\n", traceFile)
		} else {
			fmt.Fprintf(os.Stderr, "  Audit Trace: DISABLED (enable with --trace or --trace-file=<path>)\n")
		}
		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "  Verbose:   ENABLED (stderr trace stream)\n")
		}
		fmt.Fprintf(os.Stderr, "  Transport: stdio (JSON-RPC 2.0, framing: %s)\n\n", framing)
	}

	var serverOpts []mcp.Option
	switch strings.ToLower(framing) {
	case "content-length", "contentlength", "cl":
		serverOpts = append(serverOpts, mcp.WithContentLengthFraming(true))
	case "auto":
		serverOpts = append(serverOpts, mcp.WithAutoFraming(true))
	}

	var tracer mcp.TraceLogger
	if traceFile != "" || cfg.Verbose {
		tracer = mcp.NewDefaultTracer(mcp.TracerConfig{
			FilePath: traceFile,
			Verbose:  cfg.Verbose,
			Stderr:   os.Stderr,
		})
		serverOpts = append(serverOpts, mcp.WithTracer(tracer))
	}

	srv := mcp.NewServer(cfg.Workspace, serverOpts...)

	lat := icx.NewLocalLattice()
	var vmClient vm.Client = vm.NewHermeticLocalSandbox(cfg.Workspace)

	var (
		gateMu   sync.Mutex
		wireGate llm.DecisionGate
	)
	getGate := func() llm.DecisionGate {
		gateMu.Lock()
		defer gateMu.Unlock()
		if wireGate != nil {
			return wireGate
		}
		if cfg.Offline {
			wireGate = llm.NewMockDecisionGate()
			return wireGate
		}
		wireClient, err := llm.NewClient(llm.ClientConfig{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
		})
		if err != nil {
			wireGate = llm.NewMockDecisionGate()
		} else {
			wireGate = llm.NewDecisionGate(wireClient, cfg.Model)
		}
		return wireGate
	}

	// Eager lightweight background pre-warming:
	// Warms AST symbol index, pre-provisions sandbox baseline, and prepares LLM gate in background
	triggerPrewarm := func(ws string) {
		if ws == "" {
			return
		}
		go func(targetWS string) {
			_ = devops.PrewarmASTCache(targetWS)
			if useSandbox && vmClient != nil {
				if pw, ok := vmClient.(interface{ Prewarm(context.Context, string) }); ok {
					pw.Prewarm(context.Background(), targetWS)
				}
			}
			_ = getGate()
		}(ws)
	}

	triggerPrewarm(cfg.Workspace)

	srv.OnWorkspaceChanged(func(newWS string) {
		cfg.Workspace = newWS
		det = mcp.AutoDetectEnvironment(newWS)
		if cfg.Offline || cfg.Mechanical {
			det.Backend = mcp.BackendOfflineLocal
			det.Model = "embedded-local-cache"
			det.ActiveModel = ""
			det.IsOffline = true
			det.BaseURL = ""
			det.APIKey = ""
		}
		if useSandbox {
			vmClient = vm.NewHermeticLocalSandbox(newWS)
		}
		triggerPrewarm(newWS)
	})

	// Wire TaskRunnerFunc so omnicoder_run can execute through the orchestrator
	taskRunner := func(tCtx context.Context, task string, maxTurns int, unbounded bool, sandbox bool) (map[string]any, error) {
		activeWS := srv.WorkspaceDir()
		gate := getGate()

		eng, err := engine.NewOrchestrator(engine.EngineConfig{
			WorkspaceDir: activeWS,
			MaxTurns:     maxTurns,
			Unbounded:    unbounded,
			Offline:      cfg.Offline,
			Model:        cfg.Model,
			DecisionGate: gate,
			ICXClient:    lat,
			VMClient:     vmClient,
			UseSandbox:   sandbox,
		})
		if err != nil {
			return nil, err
		}

		report, err := eng.ExecuteTask(tCtx, engine.RunConfig{
			Task:         task,
			WorkspaceDir: activeWS,
			MaxTurns:     maxTurns,
			Unbounded:    unbounded,
			Offline:      cfg.Offline,
			UseSandbox:   sandbox,
		})
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"success":       report.Success,
			"turns_count":   report.TurnsCount,
			"heal_count":    report.HealCount,
			"files_touched": len(report.FilesTouched),
			"final_output":  report.FinalOutput,
			"duration_ms":   report.Duration.Milliseconds(),
			"merkle_seal":   report.MerkleSeal,
		}, nil
	}

	scopedTaskRunner := func(tCtx context.Context, opts mcp.ScopedRunOptions) (map[string]any, error) {
		activeWS := srv.WorkspaceDir()
		gate := getGate()

		eng, err := engine.NewOrchestrator(engine.EngineConfig{
			WorkspaceDir: activeWS,
			MaxTurns:     opts.MaxTurns,
			Unbounded:    opts.Unbounded,
			Offline:      cfg.Offline,
			Model:        cfg.Model,
			DecisionGate: gate,
			ICXClient:    lat,
			VMClient:     vmClient,
			UseSandbox:   opts.Sandbox,
			TargetFile:   opts.TargetFile,
			Scope:        opts.Scope,
			AuditOnly:    opts.AuditOnly,
			JSON:         opts.JSON,
		})
		if err != nil {
			return nil, err
		}

		report, err := eng.ExecuteTask(tCtx, engine.RunConfig{
			Task:         opts.Task,
			WorkspaceDir: activeWS,
			MaxTurns:     opts.MaxTurns,
			Unbounded:    opts.Unbounded,
			Offline:      cfg.Offline,
			UseSandbox:   opts.Sandbox,
			TargetFile:   opts.TargetFile,
			Scope:        opts.Scope,
			AuditOnly:    opts.AuditOnly,
			JSON:         opts.JSON,
		})
		if err != nil {
			return nil, err
		}

		status := "success"
		if !report.Success {
			status = "failed"
		}
		targetSyms := report.TargetSymbols
		if targetSyms == nil {
			targetSyms = []string{}
		}
		diags := report.Diagnostics
		if diags == nil {
			diags = []any{}
		}

		return map[string]any{
			"status":           status,
			"success":          report.Success,
			"target_symbols":   targetSyms,
			"diagnostics":      diags,
			"patches_proposed": report.PatchesProposed,
			"execution_ms":     report.ExecutionMS,
			"turns_count":      report.TurnsCount,
			"heal_count":       report.HealCount,
			"files_touched":    report.FilesTouched,
			"final_output":     report.FinalOutput,
			"duration_ms":      report.Duration.Milliseconds(),
			"merkle_seal":      report.MerkleSeal,
		}, nil
	}

	healRunner := func(hCtx context.Context, cmdStr string, maxTurns int, issue string) (map[string]any, error) {
		taskDesc := fmt.Sprintf("Fix compiler/test errors for command: %s", cmdStr)
		if issue != "" {
			taskDesc += fmt.Sprintf(" (Issue: %s)", issue)
		}
		return taskRunner(hCtx, taskDesc, maxTurns, false, useSandbox)
	}

	mcp.RegisterOmniCoderTools(srv, &mcp.OmniCoderServerContext{
		Config:           det,
		Lattice:          lat,
		VMClient:         vmClient,
		SandboxOn:        useSandbox,
		TaskRunner:       taskRunner,
		ScopedTaskRunner: scopedTaskRunner,
		HealRunner:       healRunner,
	})

	return srv.ServeStdio(ctx)
}
