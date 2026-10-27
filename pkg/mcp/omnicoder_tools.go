// SPDX-License-Identifier: Apache-2.0
// Package mcp implements the Model Context Protocol (MCP) JSON-RPC 2.0 stdio server,
// tool registry, and sandboxed filesystem/git tools for OmniCoder-ICX.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/orchestrator"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// TaskRunnerFunc executes an autonomous multi-turn coding task.
type TaskRunnerFunc func(ctx context.Context, task string, maxTurns int, unbounded bool, sandbox bool) (map[string]any, error)

// ScopedRunOptions configures an autonomous coding run with boundary and mode constraints.
type ScopedRunOptions struct {
	Task       string `json:"task"`
	MaxTurns   int    `json:"max_turns,omitempty"`
	Unbounded  bool   `json:"unbounded,omitempty"`
	Sandbox    bool   `json:"sandbox,omitempty"`
	TargetFile string `json:"target_file,omitempty"`
	Scope      string `json:"scope,omitempty"`
	AuditOnly  bool   `json:"audit_only,omitempty"`
	JSON       bool   `json:"json,omitempty"`
}

// ScopedTaskRunnerFunc executes an autonomous multi-turn coding task with scope and boundary options.
type ScopedTaskRunnerFunc func(ctx context.Context, opts ScopedRunOptions) (map[string]any, error)

// HealRunnerFunc executes an iterative compiler/test self-healing loop.
type HealRunnerFunc func(ctx context.Context, command string, maxTurns int, issue string) (map[string]any, error)

// SwarmRunnerFunc executes a multi-process swarm execution.
type SwarmRunnerFunc func(ctx context.Context, cfg orchestrator.SwarmConfig) (any, error)

// OmniCoderServerContext provides shared state and dependencies for OmniCoder MCP tools.
type OmniCoderServerContext struct {
	Config             DetectedConfig
	Lattice            *icx.LocalMemoryCache
	VMClient           vm.Client
	SandboxOn          bool
	TaskRunner         TaskRunnerFunc
	ScopedTaskRunner   ScopedTaskRunnerFunc
	HealRunner         HealRunnerFunc
	SwarmRunner        SwarmRunnerFunc
	CustomWorkerRunner func(ctx context.Context, phase *engine.SwarmPhase, alloc *orchestrator.WorktreeAllocation) error
}

// PropertySchema defines a schema property for Draft 2020-12 MCP tool definitions.
type PropertySchema struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// ToolInputSchema defines the input schema for Draft 2020-12 MCP tool definitions.
type ToolInputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]PropertySchema `json:"properties"`
	Required   []string                  `json:"required,omitempty"`
}

// CanonicalToolEntry captures the canonical tool specification.
type CanonicalToolEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema ToolInputSchema `json:"inputSchema"`
}

// CanonicalOmniCoderTools defines the 20 authoritative MCP tools registered on the server.
// omnicoder_swarm is added additively at index 19 while preserving all 19 original tools.
var CanonicalOmniCoderTools = []CanonicalToolEntry{
	// 0. omnicoder_status
	{
		Name:        "omnicoder_status",
		Description: "Reports OmniCoder runtime environment, active model backend, MicroVM sandbox status, and workspace health.",
		InputSchema: ToolInputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
		},
	},
	// 1. omnicoder_map
	{
		Name:        "omnicoder_map",
		Description: "Synthesizes an AST PageRank tree outline of the workspace (<2.0s). RECOMMENDED FIRST STEP: Use this to rapidly understand workspace structure and top architectural symbols under 1,024 tokens without consuming context.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"token_budget": {Type: "integer", Description: "Token budget for the generated repo map (default: 1024)."},
				"query":        {Type: "string", Description: "Optional query to bias PageRank importance toward specific concepts or features."},
				"max_files":    {Type: "integer", Description: "Maximum number of files to scan (default: 200)."},
			},
		},
	},
	// 2. omnicoder_search
	{
		Name:        "omnicoder_search",
		Description: "Performs unified symbol-aware semantic + ripgrep regex search across the workspace. RECOMMENDED: Use to locate exact functions, structs, interfaces, and methods across files without reading entire files.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"query": {Type: "string", Description: "Symbol name, function/method identifier, or regex pattern to search for."},
				"path":  {Type: "string", Description: "Optional path or subdirectory to restrict search scope."},
			},
			Required: []string{"query"},
		},
	},
	// 3. omnicoder_patch
	{
		Name:        "omnicoder_patch",
		Description: "Applies surgical edits (Aider SEARCH/REPLACE blocks, 1-indexed start_line/end_line ranges, or multi-chunk atomic refactoring). BEST FOR: Applying fuzzy diffs or multi-point edits when exact line matching is challenging. NOTE: For standard direct replacements, native IDE edit tools are also fully supported.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"path":  {Type: "string", Description: "Relative path to the file to be patched."},
				"patch": {Type: "string", Description: "Unified diff or search/replace patch content."},
			},
			Required: []string{"path", "patch"},
		},
	},
	// 4. omnicoder_heal
	{
		Name:        "omnicoder_heal",
		Description: "Executes a compiler build or test command and extracts structured diagnostics (failing file, line number, and 5-line source snippets) with optional iterative self-healing.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"command":   {Type: "string", Description: "Build or test command to execute (e.g. 'go test ./...', 'npm test', 'cargo check')."},
				"max_turns": {Type: "integer", Description: "Maximum number of self-healing repair attempts (default: 5)."},
				"issue":     {Type: "string", Description: "Optional description of the issue or error symptom."},
			},
			Required: []string{"command"},
		},
	},
	// 5. omnicoder_refactor
	{
		Name:        "omnicoder_refactor",
		Description: "Performs AST blast-radius walk and coordinates multi-file atomic refactoring for a symbol.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"symbol":        {Type: "string", Description: "Symbol or function name to refactor."},
				"new_signature": {Type: "string", Description: "Optional new signature to apply."},
				"target_file":   {Type: "string", Description: "Optional file path to confine refactoring scope."},
				"scope":         {Type: "string", Description: "Optional directory or glob pattern to restrict blast radius."},
			},
			Required: []string{"symbol"},
		},
	},
	// 6. omnicoder_sandbox
	{
		Name:        "omnicoder_sandbox",
		Description: "Executes shell commands inside an isolated MicroVM sandbox with instant CoW rollback on non-zero exit codes. BEST FOR: Testing untrusted code, exploratory scripts, or destructive operations safely. NOTE: On failure, workspace changes are rolled back and not persisted on disk. For normal builds, migrations, and package installs where files must persist, use native terminal tools.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"command": {Type: "string", Description: "Shell command line to execute inside the sandbox."},
				"action":  {Type: "string", Description: "Optional sandbox action: 'exec' (default), 'checkpoint', 'rollback', 'status'."},
			},
			Required: []string{"command"},
		},
	},
	// 7. omnicoder_run
	{
		Name:        "omnicoder_run",
		Description: "Autonomous multi-turn batch coding runner using the 8-stage pipeline. BEST FOR: Background headless batch generation or automated benchmark sweeps. CAUTION: Interactive coding agents (Antigravity, Claude, Cline) should NOT invoke this recursively during active pair-programming sessions to avoid nested execution traps; instead, orchestrate tasks directly.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"task":        {Type: "string", Description: "High-level goal or task description."},
				"max_turns":   {Type: "integer", Description: "Maximum autonomous turn iterations (default: 10)."},
				"target_file": {Type: "string", Description: "Confine mutations strictly to this single file path."},
				"scope":       {Type: "string", Description: "Directory path or glob pattern defining the writable boundary."},
			},
			Required: []string{"task"},
		},
	},
	// 8. omnicoder_set_workspace
	{
		Name:        "omnicoder_set_workspace",
		Description: "Sets or switches the active workspace root directory across all tools and triggers AST cache pre-warming. Accepts 'path', 'workspace', or 'workspace_dir'.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"path": {Type: "string", Description: "Absolute or relative path to the new workspace directory."},
			},
			Required: []string{"path"},
		},
	},
	// 9. omnicoder_doctor
	{
		Name:        "omnicoder_doctor",
		Description: "Authority ground truth oracle for the workspace. Single-call diagnosis: inspects git status, verifies milestone artifacts, extracts declared version, and runs local compilation/test gates.",
		InputSchema: ToolInputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
		},
	},
	// 10. omnicoder_toggle_checklist
	{
		Name:        "omnicoder_toggle_checklist",
		Description: "Atomically toggles a Markdown checklist item ([ ] <-> [x]) in an implementation plan or task tracking document without re-reading or rewriting the entire file. HIGHLY RECOMMENDED for checking off completed milestone steps.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"file_path": {Type: "string", Description: "Path to Markdown file relative to workspace."},
				"item_text": {Type: "string", Description: "Exact text or substring of the checklist line to toggle."},
			},
			Required: []string{"file_path", "item_text"},
		},
	},
	// 11. omnicoder_update_section
	{
		Name:        "omnicoder_update_section",
		Description: "Atomically replaces the content under a Markdown header (## Section) in an implementation plan or documentation file. HIGHLY RECOMMENDED for updating progress sections without risk of document truncation.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"file_path":        {Type: "string", Description: "Path to Markdown file relative to workspace."},
				"heading":          {Type: "string", Description: "Markdown header line (e.g. '## Current Status' or '### Phase 2')."},
				"replacement_text": {Type: "string", Description: "New Markdown content to place beneath the header."},
			},
			Required: []string{"file_path", "heading", "replacement_text"},
		},
	},
	// 12. omnicoder_verify_service
	{
		Name:        "omnicoder_verify_service",
		Description: "Runs non-destructive HTTP endpoint verification against a local service or API server on an ephemeral port. BEST FOR: Verifying API contracts, health probes, and rate limits without manual background process scripts.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"entrypoint": {Type: "string", Description: "Optional command or script to launch the service in the background."},
				"port":       {Type: "integer", Description: "Port number to verify (default: 8099)."},
			},
		},
	},
	// 13. read_file
	{
		Name:        "read_file",
		Description: "Reads the complete contents of a file within the workspace boundary.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"path": {Type: "string", Description: "Path of the file to read relative to workspace."},
			},
			Required: []string{"path"},
		},
	},
	// 14. write_file
	{
		Name:        "write_file",
		Description: "Atomically writes content to a file within the workspace boundary.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"path":    {Type: "string", Description: "Path of the file to write relative to workspace."},
				"content": {Type: "string", Description: "Content to write to the file."},
			},
			Required: []string{"path", "content"},
		},
	},
	// 15. list_directory
	{
		Name:        "list_directory",
		Description: "Lists files and subdirectories within a workspace path.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"path": {Type: "string", Description: "Directory path relative to workspace (default: '.')."},
			},
		},
	},
	// 16. git_status
	{
		Name:        "git_status",
		Description: "Inspects git repository status, listing staged, modified, and untracked files.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"repo_path": {Type: "string", Description: "Optional repository path relative to workspace."},
			},
		},
	},
	// 17. git_diff
	{
		Name:        "git_diff",
		Description: "Returns unified git diff of working tree changes.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"staged": {Type: "boolean", Description: "If true, show staged changes."},
			},
		},
	},
	// 18. git_commit
	{
		Name:        "git_commit",
		Description: "Creates a git commit with staged or specified files.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"message": {Type: "string", Description: "Commit message."},
			},
			Required: []string{"message"},
		},
	},
	// 19. omnicoder_swarm (Additive, at index 19)
	{
		Name:        "omnicoder_swarm",
		Description: "Orchestrates a parallel multi-process worker swarm to execute a multi-phase implementation plan DAG in parallel with 2PC verification.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"plan_path": {
					Type:        "string",
					Description: "Path to markdown implementation plan (e.g. docs/IMPLEMENTATION_PLAN.md).",
				},
				"max_workers": {
					Type:        "integer",
					Description: "Maximum parallel child worker processes (default: 4).",
				},
				"offline": {
					Type:        "boolean",
					Description: "Run in offline mode using embedded local cache.",
				},
				"timeout_minutes": {
					Type:        "integer",
					Description: "Total swarm timeout budget in minutes (default: 30).",
				},
			},
			Required: []string{"plan_path"},
		},
	},
}

var (
	omnicoderContextsMu sync.RWMutex
	omnicoderContexts   = make(map[*Server]*OmniCoderServerContext)
)

func (s *Server) setOmniCoderContext(ctx *OmniCoderServerContext) {
	omnicoderContextsMu.Lock()
	defer omnicoderContextsMu.Unlock()
	omnicoderContexts[s] = ctx
}

func (s *Server) getOmniCoderContext() *OmniCoderServerContext {
	omnicoderContextsMu.RLock()
	defer omnicoderContextsMu.RUnlock()
	return omnicoderContexts[s]
}

// RegisterOmniCoderTools registers the complete OmniCoder engine suite on the MCP server.
func RegisterOmniCoderTools(s *Server, sCtx *OmniCoderServerContext) {
	if sCtx == nil {
		det := AutoDetectEnvironment(s.WorkspaceDir())
		sCtx = &OmniCoderServerContext{
			Config:    det,
			Lattice:   icx.NewLocalLattice(),
			VMClient:  vm.NewHermeticLocalSandbox(s.WorkspaceDir()),
			SandboxOn: true,
		}
	}
	s.setOmniCoderContext(sCtx)

	// Register workspace change hook to automatically update sCtx when workspace changes
	s.OnWorkspaceChanged(func(newDir string) {
		det := AutoDetectEnvironment(newDir)
		if sCtx.Config.IsOffline {
			det.Backend = BackendOfflineLocal
			det.Model = "embedded-local-cache"
			det.ActiveModel = ""
			det.IsOffline = true
			det.BaseURL = ""
			det.APIKey = ""
		}
		sCtx.Config = det
		if sCtx.SandboxOn {
			sCtx.VMClient = vm.NewHermeticLocalSandbox(newDir)
		}
	})

	// 1. omnicoder_status
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_status",
		Description: "Returns active workspace root, detected LLM backend, resident local model, and sandbox status.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace": map[string]any{
					"type":        "string",
					"description": "Optional workspace directory path to switch to or inspect.",
				},
			},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		if wsArg, ok := args["workspace"].(string); ok && strings.TrimSpace(wsArg) != "" {
			_ = s.SetWorkspace(wsArg)
		}

		currentWS := s.WorkspaceDir()
		if IsIDEOsOrSystemDir(currentWS) {
			if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
				_ = s.SetWorkspace(ideWS)
				currentWS = s.WorkspaceDir()
			}
		}

		defaultShell := "sh / bash"
		lineEnding := "LF"
		if runtime.GOOS == "windows" {
			defaultShell = "powershell.exe / cmd.exe"
			lineEnding = "CRLF"
		}

		return map[string]any{
			"status":           "READY",
			"workspace_root":   currentWS,
			"is_ide_directory": IsIDEOsOrSystemDir(currentWS),
			"host_os":          runtime.GOOS,
			"host_arch":        runtime.GOARCH,
			"default_shell":    defaultShell,
			"path_separator":   string(filepath.Separator),
			"line_ending":      lineEnding,
			"backend":          sCtx.Config.Backend,
			"model":            sCtx.Config.Model,
			"active_model":     sCtx.Config.ActiveModel,
			"is_offline":       sCtx.Config.IsOffline,
			"sandbox_ready":            sCtx.VMClient != nil,
			"sandbox_on":               sCtx.SandboxOn,
			"tools_count":              len(s.ListTools()),
			"typescript_ast_supported":  true,
			"windows_junction_sandbox":  true,
			"polyglot_ast_supported":    true,
			"ast_cache_entries":         devops.GetASTCache().Count(),
			"ast_cache_warmed":          devops.GetASTCache().Count() > 0,
			"environment":              sCtx.Config.MaskedCopy(),
			"timestamp":                time.Now().UTC().Format(time.RFC3339),
		}, nil
	})

	// 2. omnicoder_map
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_map",
		Description: "Synthesizes an AST PageRank tree outline of the workspace (<2.0s) for semantic orientation.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"token_budget": map[string]any{
					"type":        "integer",
					"description": "Token budget for the generated repo map (default: 1024).",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Optional query to bias PageRank importance toward specific concepts or features.",
				},
				"focus_files": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
					"description": "Optional list of files to anchor or prioritize in the tree.",
				},
				"max_files": map[string]any{
					"type":        "integer",
					"description": "Maximum number of files to scan (default: 200).",
				},
			},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		tokenBudget := 1024
		if tb, ok := args["token_budget"].(float64); ok && tb > 0 {
			tokenBudget = int(tb)
		}
		var focusFiles []string
		if ff, ok := args["focus_files"].([]any); ok {
			for _, item := range ff {
				if str, ok := item.(string); ok && str != "" {
					focusFiles = append(focusFiles, str)
				}
			}
		}
		query := ""
		if q, ok := args["query"].(string); ok {
			query = q
		}
		maxFiles := 200
		if mf, ok := args["max_files"].(float64); ok && mf > 0 {
			maxFiles = int(mf)
		}

		currentWS := s.ResolveEffectiveWorkspace(args, "")

		start := time.Now()
		res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
			WorkspaceDir: currentWS,
			TokenBudget:  tokenBudget,
			FocusFiles:   focusFiles,
			Query:        query,
			MaxFiles:     maxFiles,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to synthesize repo map: %w", err)
		}
		elapsed := time.Since(start)

		return map[string]any{
			"repo_map":      res.RepoMap,
			"files_scanned": res.FilesScanned,
			"symbols_found": res.SymbolsFound,
			"token_count":   res.TokenCount,
			"top_symbols":   res.TopSymbols,
			"duration_ms":   elapsed.Milliseconds(),
		}, nil
	})

	// 3. omnicoder_search
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_search",
		Description: "Fast workspace symbol search supporting exact function names, method receivers (e.g. 'func (s *Server) Routes'), struct types, constants, variables, and literal token fallback across the codebase.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query terms, symbol name, method receiver, or token phrase.",
				},
				"max_results": map[string]any{
					"type":        "integer",
					"description": "Maximum number of search results to return (default: 10).",
				},
				"include_tests": map[string]any{
					"type":        "boolean",
					"description": "Whether to include test files in results (default: false).",
				},
				"hint_files": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
					"description": "Optional list of files to prioritize or restrict search to.",
				},
				"compact": map[string]any{
					"type":        "boolean",
					"description": "When true, omits multi-line context blocks and returns concise snippets to reduce token consumption (default: true if max_results > 5).",
				},
			},
			"required": []string{"query"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("query cannot be empty")
		}
		maxResults := 10
		if mr, ok := args["max_results"].(float64); ok && mr > 0 {
			maxResults = int(mr)
		}
		compact := (maxResults > 5)
		if c, ok := args["compact"].(bool); ok {
			compact = c
		}
		includeTests := false
		if it, ok := args["include_tests"].(bool); ok {
			includeTests = it
		}
		var hintFiles []string
		if hf, ok := args["hint_files"].([]any); ok {
			for _, item := range hf {
				if str, ok := item.(string); ok && str != "" {
					hintFiles = append(hintFiles, str)
				}
			}
		}

		currentWS := s.WorkspaceDir()
		if wsArg, ok := args["workspace"].(string); ok && strings.TrimSpace(wsArg) != "" {
			if fi, err := os.Stat(wsArg); err == nil && fi.IsDir() && !IsIDEOsOrSystemDir(wsArg) {
				currentWS = wsArg
			}
		} else if IsIDEOsOrSystemDir(currentWS) {
			if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
				_ = s.SetWorkspace(ideWS)
				currentWS = s.WorkspaceDir()
			}
		}

		tokens := strings.Fields(query)
		results, err := devops.SearchCodebase(devops.CodeSearchOptions{
			WorkspaceDir: currentWS,
			RawQuery:     query,
			QueryTokens:  tokens,
			ExcludeTests: !includeTests,
			MaxResults:   maxResults,
			HintFiles:    hintFiles,
			Compact:      compact,
		})
		if err != nil {
			return nil, fmt.Errorf("code search failed: %w", err)
		}

		type SearchHit struct {
			File         string   `json:"file"`
			Path         string   `json:"path"`
			Line         int      `json:"line"`
			MatchKind    string   `json:"match_kind"`
			MatchedToken string   `json:"matched_token"`
			Score        int      `json:"score"`
			Snippet      string   `json:"snippet,omitempty"`
			Content      string   `json:"content,omitempty"`
			Context      []string `json:"context,omitempty"`
		}

		var hits []SearchHit
		for _, r := range results {
			hit := SearchHit{
				File:         r.File,
				Path:         r.File,
				Line:         r.Line,
				MatchKind:    string(r.MatchKind),
				MatchedToken: r.MatchedToken,
				Score:        r.Score,
			}
			if compact {
				trimmed := strings.TrimSpace(r.LineText)
				hit.Snippet = trimmed
				hit.Content = trimmed
			} else {
				hit.Context = r.ContextLines
				hit.Content = strings.Join(r.ContextLines, "\n")
			}
			hits = append(hits, hit)
		}

		return map[string]any{
			"query":         query,
			"compact":       compact,
			"count":         len(hits),
			"total_matches": len(hits),
			"results":       hits,
		}, nil
	})

	// 4. omnicoder_patch
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_patch",
		Description: "Applies surgical edits (Aider SEARCH/REPLACE blocks, 1-indexed start_line/end_line ranges, or multi-chunk atomic refactoring). CRITICAL: Keep SEARCH blocks SHORT (3-8 lines of unique surrounding context). NEVER put hundreds of lines into SEARCH blocks. For multi-point refactors, pass chunks: [{start, end, content}, ...].",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative file path within the workspace.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Aider search/replace blocks (<<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE), replacement lines (if using start_line/end_line), or full source.",
				},
				"chunks": map[string]any{
					"type": "array",
					"description": "Optional array of replacement chunks for multi-point atomic refactoring. Each chunk specifies start (or start_line), end (or end_line), and content. Chunks are applied atomically bottom-up to prevent line-drift.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"start": map[string]any{
								"type":        "integer",
								"description": "1-indexed start line (inclusive).",
							},
							"start_line": map[string]any{
								"type":        "integer",
								"description": "Alias for start.",
							},
							"end": map[string]any{
								"type":        "integer",
								"description": "1-indexed end line (inclusive). Defaults to start if omitted.",
							},
							"end_line": map[string]any{
								"type":        "integer",
								"description": "Alias for end.",
							},
							"content": map[string]any{
								"type":        "string",
								"description": "Replacement lines for this chunk.",
							},
						},
						"required": []string{"content"},
					},
				},
				"start_line": map[string]any{
					"type":        "integer",
					"description": "Optional 1-indexed start line number for range replacement. Replaces lines from start_line to end_line with content without needing SEARCH blocks.",
				},
				"end_line": map[string]any{
					"type":        "integer",
					"description": "Optional 1-indexed end line number for range replacement (inclusive). Defaults to start_line if omitted.",
				},
				"allow_shrinkage": map[string]any{
					"type":        "boolean",
					"description": "If true, bypasses Guard C file shrinkage checks (>40% drop), useful when trimming test cases or refactoring (default: false).",
				},
				"ast_node": map[string]any{
					"type":        "string",
					"description": "Optional AST symbol or declaration name (e.g. 'Button', 'useDashboard', 'ButtonProps', 'App') to replace directly, anchored to its AST declaration boundaries rather than fragile line numbers.",
				},
				"bypass_sandbox": map[string]any{
					"type":        "boolean",
					"description": "If true, bypasses microVM staging and applies directly to disk (default: false).",
				},
			},
			"required": []string{"path"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		relPath, _ := args["path"].(string)
		content, _ := args["content"].(string)
		astNode, _ := args["ast_node"].(string)
		startLine := getOptionalIntArg(args, "start_line", 0)
		endLine := getOptionalIntArg(args, "end_line", 0)
		allowShrinkage, _ := args["allow_shrinkage"].(bool)

		var parsedChunks []PatchChunk
		var synthesizedSearchBlocks []string
		if rawChunks, ok := args["chunks"].([]any); ok && len(rawChunks) > 0 {
			for idx, item := range rawChunks {
				if m, ok := item.(map[string]any); ok {
					sLine := getOptionalIntArg(m, "start", 0)
					if sLine <= 0 {
						sLine = getOptionalIntArg(m, "start_line", 0)
					}
					if sLine <= 0 {
						sLine = getOptionalIntArg(m, "line_start", 0)
					}
					eLine := getOptionalIntArg(m, "end", 0)
					if eLine <= 0 {
						eLine = getOptionalIntArg(m, "end_line", 0)
					}
					if eLine <= 0 {
						eLine = getOptionalIntArg(m, "line_end", 0)
					}
					chunkContent, _ := m["content"].(string)
					if chunkContent == "" {
						if rStr, hasR := firstStringArgPresent(m, "replace_block", "new_str", "new_string", "new_content", "new_text", "new", "replacement_content", "ReplacementContent", "replace", "replacement"); hasR {
							chunkContent = rStr
						}
					}
					if sStr, hasS := firstStringArgPresent(m, "search_block", "old_str", "old_string", "old_content", "old_text", "old", "target_content", "TargetContent", "search", "find"); hasS && sStr != "" {
						synthesizedSearchBlocks = append(synthesizedSearchBlocks, fmt.Sprintf("<<<<<<< SEARCH\n%s\n=======\n%s\n>>>>>>> REPLACE", sStr, chunkContent))
						continue
					}
					if sLine <= 0 {
						return nil, fmt.Errorf("chunk %d must specify a valid 1-indexed start line (or search_block/replace_block)", idx+1)
					}
					if eLine <= 0 {
						eLine = sLine
					}
					parsedChunks = append(parsedChunks, PatchChunk{
						Start:   sLine,
						End:     eLine,
						Content: chunkContent,
					})
				}
			}
		}
		if len(synthesizedSearchBlocks) > 0 && content == "" {
			content = strings.Join(synthesizedSearchBlocks, "\n")
		}

		if strings.TrimSpace(relPath) == "" || (content == "" && len(parsedChunks) == 0 && startLine <= 0) {
			return nil, fmt.Errorf("path and content (or chunks) are required")
		}

		currentWS := s.ResolveEffectiveWorkspace(args, relPath)

		targetFullPath, err := ResolveSecurePath(currentWS, relPath)
		if err != nil {
			return nil, fmt.Errorf("path security error: %w", err)
		}

		var existingStr string
		var existingSize int64
		if fi, statErr := os.Stat(targetFullPath); statErr == nil && !fi.IsDir() {
			existingSize = fi.Size()
			if raw, readErr := os.ReadFile(targetFullPath); readErr == nil {
				existingStr = string(raw)
			}
		}

		// Case A: Aider Edit Blocks or Unified Diff Hunks
		isAiderBlock := strings.Contains(content, "<<<<<") && strings.Contains(content, "=====")
		unifiedBlocks, isUnifiedDiff := convertUnifiedDiffToEditBlocks(content, relPath)
		if isAiderBlock || isUnifiedDiff {
			var blocks []devops.EditBlock
			if isAiderBlock {
				normalizedBlockContent := content
				if !strings.Contains(normalizedBlockContent, ">>>>>") {
					normalizedBlockContent = strings.TrimRight(normalizedBlockContent, "\r\n") + "\n>>>>>>> REPLACE\n"
				}
				var parseErr error
				blocks, parseErr = devops.ParseAiderEditBlocks(normalizedBlockContent, relPath)
				if parseErr != nil {
					return nil, fmt.Errorf("failed to parse Aider edit blocks: %w", parseErr)
				}
			} else {
				blocks = unifiedBlocks
			}

			// Pre-flight in-memory dry-run to evaluate post-patch state
			dryRes, dryErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
				RepoPath:   currentWS,
				Blocks:     blocks,
				FuzzyMatch: true,
				DryRun:     true,
			})
			if dryErr != nil || (dryRes != nil && dryRes.Status != "APPLIED") {
				return nil, fmt.Errorf("patch dry-run failed: %w", dryErr)
			}

			// Live Apply
			applyRes, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
				RepoPath:   currentWS,
				Blocks:     blocks,
				FuzzyMatch: true,
			})
			if err != nil || !applyRes.Applied {
				return nil, fmt.Errorf("patch application failed: %w", err)
			}

			return map[string]any{
				"status":         "PATCHED",
				"path":           relPath,
				"blocks_applied": applyRes.BlocksApplied,
				"cas_hash":       applyRes.CASHash,
				"patch_id":       applyRes.PatchID,
				"lines_added":    applyRes.LinesAdded,
				"lines_removed":  applyRes.LinesRemoved,
			}, nil
		}

		// Case B / Case C / Case Multi-Chunk
		finalContent := content
		isGo := strings.HasSuffix(strings.ToLower(relPath), ".go")
		chunksLinesAdded := 0
		chunksLinesRemoved := 0

		if len(parsedChunks) > 0 {
			// Case Multi-Chunk: Atomic multi-slice non-contiguous edits
			if existingSize == 0 && existingStr == "" {
				if _, statErr := os.Stat(targetFullPath); os.IsNotExist(statErr) && parsedChunks[0].Start > 1 {
					return nil, fmt.Errorf("cannot apply multi-chunk patch to non-existent file %s: specify full file content or create file first", relPath)
				}
			}
			patchedCode, added, removed, mErr := applyMultiChunkPatch(existingStr, parsedChunks)
			if mErr != nil {
				return nil, fmt.Errorf("multi-chunk patch error on %s: %w", relPath, mErr)
			}
			finalContent = patchedCode
			chunksLinesAdded = added
			chunksLinesRemoved = removed
		} else if strings.TrimSpace(astNode) != "" {
			// Case AST: AST Node-Level Anchored Replacement
			if existingSize == 0 && existingStr == "" {
				return nil, fmt.Errorf("cannot apply AST-anchored patch to non-existent file %s: specify full file content or create file first", relPath)
			}
			ext := filepath.Ext(relPath)
			patchedCode, ok, astErr := devops.ReplaceASTNode(existingStr, astNode, content, ext)
			if astErr != nil || !ok {
				return nil, fmt.Errorf("failed to apply AST-anchored patch for symbol %q on %s: %w", astNode, relPath, astErr)
			}
			finalContent = patchedCode
		} else if startLine > 0 {
			// Case B: Line Range Replacement (1-indexed start_line and optional end_line)
			if existingSize == 0 && existingStr == "" {
				if _, statErr := os.Stat(targetFullPath); os.IsNotExist(statErr) && startLine > 1 {
					return nil, fmt.Errorf("cannot apply line range patch to non-existent file %s: specify full file content or create file first", relPath)
				}
			}

			patchedCode, patchErr := applyLineRangePatch(existingStr, startLine, endLine, content)
			if patchErr != nil {
				return nil, fmt.Errorf("failed to apply line range patch to %s: %w", relPath, patchErr)
			}
			finalContent = patchedCode
		} else {
			// Case C: Full Content Write with Code Preservation Guards A, B, and C
			if existingSize > 0 {
				// Guard A: Tiny stub rejection (<80 bytes against substantive file >200 bytes)
				if !allowShrinkage && existingSize > 200 && isCodeStub(relPath, finalContent) {
					return nil, fmt.Errorf("code preservation guard: rejected stub rewrite (%d bytes) over substantive file (%d bytes)", len(finalContent), existingSize)
				}

				// Guard B: Go file function splicing (auto-splice single function updates)
				if isGo && isSingleFunctionUpdate(existingStr, finalContent) {
					if mergedCode, mergedOk := tryMergeGoFunction(existingStr, finalContent); mergedOk {
						finalContent = mergedCode
					}
				}

				// Guard C: File shrinkage (>40% drop) rejection
				isTestFile := strings.HasSuffix(strings.ToLower(relPath), "_test.go") || strings.Contains(strings.ToLower(relPath), "test")
				if !allowShrinkage && !isTestFile && existingSize > 300 && float64(len(finalContent)) < float64(existingSize)*0.6 {
					// If Go file, attempt merge fallback before outright rejection
					merged := false
					if isGo {
						if mergedCode, mergedOk := tryMergeGoFunction(existingStr, finalContent); mergedOk {
							finalContent = mergedCode
							merged = true
						}
					}
					if !merged {
						return nil, fmt.Errorf("code preservation guard: rejected shrinkage rewrite (>40%% size drop: %d -> %d bytes)", existingSize, len(finalContent))
					}
				}
			}
		}

		// Pre-flight Polyglot Syntax Check (Go, Python, JSON, JS/TS) with Deterministic Auto-Repair
		repairedContent, wasRepaired, preErr := devops.AutoRepairPreflightSyntax(currentWS, relPath, finalContent)
		if preErr != nil {
			return nil, fmt.Errorf("code preservation guard: pre-flight syntax check failed on %s: %w", relPath, preErr)
		}
		if wasRepaired {
			finalContent = repairedContent
		}

		// Preserve existing CRLF if existing file used CRLF
		if strings.Contains(existingStr, "\r\n") && !strings.Contains(finalContent, "\r\n") {
			finalContent = strings.ReplaceAll(finalContent, "\n", "\r\n")
		}

		// Live Atomic Workspace Persistence
		if err := os.MkdirAll(filepath.Dir(targetFullPath), 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory: %w", err)
		}
		if err := os.WriteFile(targetFullPath, []byte(finalContent), 0644); err != nil {
			return nil, fmt.Errorf("failed to write file: %w", err)
		}
		devops.GetASTCache().Invalidate(relPath)

		resp := map[string]any{
			"status":        "WRITTEN",
			"path":          relPath,
			"bytes_written": len(finalContent),
			"sha256":        canonical.SHA256Bytes([]byte(finalContent)),
		}
		if len(parsedChunks) > 0 {
			resp["status"] = "PATCHED"
			resp["mode"] = "MULTI_CHUNK"
			resp["patch_strategy"] = "multi_chunk_ast"
			resp["chunks_applied"] = len(parsedChunks)
			resp["lines_added"] = chunksLinesAdded
			resp["lines_removed"] = chunksLinesRemoved
		} else if strings.TrimSpace(astNode) != "" {
			resp["status"] = "PATCHED"
			resp["mode"] = "AST_ANCHORED"
			resp["ast_node"] = astNode
		} else if startLine > 0 {
			resp["status"] = "PATCHED"
			resp["mode"] = "LINE_RANGE"
			resp["start_line"] = startLine
			resp["end_line"] = endLine
		}
		return resp, nil
	})

	// 5. omnicoder_heal
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_heal",
		Description: "PRIMARY tool for build, test, and verification workflows. Runs any build or test command (e.g. 'go test ./...', 'pytest', 'npm test'), automatically extracts structured compiler diagnostics and test failures with 5-line source snippets, and executes iterative self-healing loops. Prefer this tool over manual shell test running or manual probe tests. Supports compound shell chaining (&&, ||, ;, cd <dir> && ...).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command to run and verify (e.g. 'go test ./...', 'pytest tests/test_api.py', 'cd frontend && npm test'). Supports compound shell chaining with &&, ||, ;, and directory navigation.",
				},
				"max_turns": map[string]any{
					"type":        "integer",
					"description": "Maximum self-healing attempts (default: 3).",
				},
				"issue": map[string]any{
					"type":        "string",
					"description": "Optional issue context or problem description.",
				},
			},
			"required": []string{"command"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		cmdToRun, _ := args["command"].(string)
		if strings.TrimSpace(cmdToRun) == "" {
			return nil, fmt.Errorf("command is required")
		}
		maxTurns := getOptionalIntArg(args, "max_turns", 1)
		if maxTurns <= 0 {
			maxTurns = 1
		}
		autoHeal, _ := args["auto_heal"].(bool)
		issue, _ := args["issue"].(string)

		backend := "Hermetic_Local"
		if sCtx.VMClient != nil {
			backend = "Calera_MicroVM"
		}

		currentWS := s.ResolveEffectiveWorkspace(args, "")

		report, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command:          cmdToRun,
			WorkspaceDir:     currentWS,
			TimeoutSeconds:   120,
			ExecutionBackend: backend,
			VMClient:         sCtx.VMClient,
		})
		if err != nil {
			return nil, fmt.Errorf("error running command: %w", err)
		}

		if report.Success && report.ExitCode == 0 {
			return map[string]any{
				"status":    "PASSED",
				"exit_code": 0,
				"command":   cmdToRun,
				"message":   fmt.Sprintf("Target command %q passed with exit code 0.", cmdToRun),
			}, nil
		}

		// Only delegate to autonomous iterative HealRunner when explicitly requested (max_turns > 1 or auto_heal: true)
		if sCtx.HealRunner != nil && (maxTurns > 1 || autoHeal) && !sCtx.Config.IsOffline {
			healRes, healErr := sCtx.HealRunner(ctx, cmdToRun, maxTurns, issue)
			if healErr == nil && healRes != nil {
				if healRes["status"] == "HEALED" || healRes["success"] == true {
					return healRes, nil
				}
				// Preserve extracted compiler/test diagnostics when autonomous heal loop did not resolve the failure
				healRes["status"] = "DIAGNOSED"
				healRes["command"] = cmdToRun
				healRes["exit_code"] = report.ExitCode
				healRes["diagnostics_count"] = len(report.Diagnostics)
				healRes["diagnostics"] = report.Diagnostics
				healRes["error_count"] = report.ErrorCount
				healRes["repair_prompt_payload"] = report.RepairPromptPayload
				healRes["cleaned_stderr"] = report.CleanedStderr
				return healRes, nil
			}
		}

		// If TaskRunner is available and maxTurns > 1, execute autonomous iterative healing
		if sCtx.TaskRunner != nil && maxTurns > 1 {
			healPrompt := fmt.Sprintf("Fix compiler/test errors for command: %s", cmdToRun)
			if issue != "" {
				healPrompt += fmt.Sprintf(". Issue details: %s", issue)
			}
			runRes, runErr := sCtx.TaskRunner(ctx, healPrompt, maxTurns, false, sCtx.SandboxOn)
			if runErr == nil {
				// Re-verify after healing
				verifyRep, vErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:          cmdToRun,
					WorkspaceDir:     currentWS,
					TimeoutSeconds:   60,
					ExecutionBackend: backend,
					VMClient:         sCtx.VMClient,
				})
				if vErr == nil && verifyRep != nil && verifyRep.Success && verifyRep.ExitCode == 0 {
					runRes["status"] = "HEALED"
					runRes["command"] = cmdToRun
					runRes["exit_code"] = 0
					return runRes, nil
				}
			}
		}

		// Return structured diagnostics
		return map[string]any{
			"status":                "DIAGNOSED",
			"command":               cmdToRun,
			"exit_code":             report.ExitCode,
			"diagnostics_count":     len(report.Diagnostics),
			"diagnostics":           report.Diagnostics,
			"error_count":           report.ErrorCount,
			"repair_prompt_payload": report.RepairPromptPayload,
			"cleaned_stderr":        report.CleanedStderr,
			"execution_engine":      report.ExecutionEngine,
			"attempts_used":         1,
			"max_turns":             maxTurns,
			"issue":                 issue,
		}, nil
	})

	// 6. omnicoder_refactor
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_refactor",
		Description: "Performs 5-hop AST blast radius walk and coordinated refactoring analysis.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"symbol": map[string]any{
					"type":        "string",
					"description": "Symbol, function, or type name to analyze.",
				},
				"new_signature": map[string]any{
					"type":        "string",
					"description": "Optional new signature to apply.",
				},
				"dry_run": map[string]any{
					"type":        "boolean",
					"description": "If true, only previews blast radius without changing code (default: true).",
				},
				"target_file": map[string]any{
					"type":        "string",
					"description": "Optional file path to confine refactoring scope.",
				},
				"scope": map[string]any{
					"type":        "string",
					"description": "Optional directory or glob pattern to restrict blast radius.",
				},
			},
			"required": []string{"symbol"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		symbol, _ := args["symbol"].(string)
		if strings.TrimSpace(symbol) == "" {
			return nil, fmt.Errorf("symbol is required")
		}
		newSig, _ := args["new_signature"].(string)
		targetFile, _ := args["target_file"].(string)
		scope, _ := args["scope"].(string)
		dryRun := true
		if dr, ok := args["dry_run"].(bool); ok {
			dryRun = dr
		}

		currentWS := s.WorkspaceDir()
		if IsIDEOsOrSystemDir(currentWS) {
			if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
				_ = s.SetWorkspace(ideWS)
				currentWS = s.WorkspaceDir()
			}
		}

		// Ingest polyglot AST files into lattice
		_ = filepath.WalkDir(currentWS, func(path string, d os.DirEntry, err error) error {
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
				rel, _ := filepath.Rel(currentWS, path)
				rel = filepath.ToSlash(rel)
				if raw, readErr := os.ReadFile(path); readErr == nil {
					_ = sCtx.Lattice.IngestPolyglotAST(rel, raw)
				}
			}
			return nil
		})

		startNode := symbol
		if qRes, qErr := sCtx.Lattice.QueryCodebase(ctx, icx.QueryCodebaseRequest{
			Query:     symbol,
			Dimension: "3D_VOLUMETRIC",
			TopK:      5,
		}); qErr == nil && len(qRes.Results) > 0 {
			startNode = qRes.Results[0].NodeID
			for _, m := range qRes.Results {
				if strings.HasSuffix(m.NodeID, "_"+symbol) || strings.HasPrefix(m.NodeID, "func_") || strings.HasPrefix(m.NodeID, "type_") {
					startNode = m.NodeID
					if strings.HasSuffix(m.NodeID, "_"+symbol) {
						break
					}
				}
			}
		}

		walkRes, err := sCtx.Lattice.MultihopWalk(ctx, icx.MultihopWalkRequest{
			StartNode: startNode,
			MaxHops:   5,
		})

		type BlastStep struct {
			Depth    int    `json:"depth"`
			Relation string `json:"relation"`
			Name     string `json:"name"`
			Type     string `json:"type"`
		}

		var steps []BlastStep
		ribbon := ""
		if err == nil && walkRes.OK {
			ribbon = walkRes.Ribbon
			for _, st := range walkRes.Steps {
				steps = append(steps, BlastStep{
					Depth:    st.Depth,
					Relation: st.Relation,
					Name:     st.Name,
					Type:     st.Type,
				})
			}
		}

		if targetFile != "" || scope != "" {
			var filteredSteps []BlastStep
			for _, st := range steps {
				matches := true
				if targetFile != "" && !strings.Contains(st.Name, targetFile) {
					matches = false
				}
				if scope != "" && !strings.Contains(st.Name, scope) {
					matches = false
				}
				if matches {
					filteredSteps = append(filteredSteps, st)
				}
			}
			steps = filteredSteps
		}

		if !dryRun && newSig != "" && sCtx.TaskRunner != nil {
			refactorTask := fmt.Sprintf("Refactor symbol `%s` to new signature `%s` across all callers in workspace", symbol, newSig)
			if targetFile != "" {
				refactorTask += fmt.Sprintf(" strictly confined to target file: %s", targetFile)
			}
			if scope != "" {
				refactorTask += fmt.Sprintf(" within scope: %s", scope)
			}
			runRes, runErr := sCtx.TaskRunner(ctx, refactorTask, 5, false, sCtx.SandboxOn)
			if runErr == nil {
				runRes["status"] = "REFACTORED"
				runRes["symbol"] = symbol
				runRes["new_signature"] = newSig
				runRes["blast_radius"] = steps
				runRes["affected_hops"] = len(steps)
				runRes["ribbon"] = ribbon
				runRes["dry_run"] = false
				if targetFile != "" {
					runRes["target_file"] = targetFile
				}
				if scope != "" {
					runRes["scope"] = scope
				}
				return runRes, nil
			}
		}

		res := map[string]any{
			"status":        "ANALYZED",
			"symbol":        symbol,
			"new_signature": newSig,
			"blast_radius":  steps,
			"affected_hops": len(steps),
			"ribbon":        ribbon,
			"dry_run":       dryRun,
		}
		if targetFile != "" {
			res["target_file"] = targetFile
		}
		if scope != "" {
			res["scope"] = scope
		}
		return res, nil
	})

	// 7. omnicoder_sandbox
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_sandbox",
		Description: "Executes command or script inside isolated microVM sandbox with instant Copy-on-Write rollback on non-zero exit codes. Commands execute directly inside workspace root (do NOT prepend 'cd <workspace>'). Ephemeral: on non-zero exit or failure, writes inside sandbox NEVER persist to the workspace. For normal builds, migrations, package installs, and test runs where files/artifacts must persist on disk, use native terminal tools. Supports compound shell chaining (&&, ||, ;, cd <dir> && ...), PowerShell script execution, and non-blocking async background execution.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command to execute inside sandbox. Already rooted at workspace directory (no need to cd to workspace). Supports compound shell chaining with &&, ||, ;, and relative directory navigation (e.g. 'cd frontend && npm test').",
				},
				"script": map[string]any{
					"type":        "string",
					"description": "Optional multi-line script content to execute in the sandbox. On Windows, executes as a PowerShell script (.ps1) by default, avoiding cmd.exe heredoc or quoting issues.",
				},
				"shell": map[string]any{
					"type":        "string",
					"description": "Optional shell interpreter: 'auto', 'powershell', 'cmd', 'sh', 'bash' (default: 'auto').",
				},
				"workdir": map[string]any{
					"type":        "string",
					"description": "Optional working directory relative to sandbox workspace.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Timeout in seconds (default: 30 for commands, 120 for tests/builds).",
				},
				"async": map[string]any{
					"type":        "boolean",
					"description": "If true, executes the command asynchronously in the background and returns an execution_id immediately without blocking stdio.",
				},
				"execution_id": map[string]any{
					"type":        "string",
					"description": "Optional execution ID from a previous async sandbox call to poll status or retrieve output.",
				},
				"action": map[string]any{
					"type":        "string",
					"description": "Action when execution_id is provided: 'poll' (check status & output), 'wait' (wait up to timeout for completion), or 'kill' (terminate process tree). Defaults to 'poll'.",
				},
			},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		execID, _ := args["execution_id"].(string)
		if strings.TrimSpace(execID) != "" {
			exec, ok := defaultAsyncSandboxManager.Get(strings.TrimSpace(execID))
			if !ok {
				return nil, fmt.Errorf("async execution ID %q not found or expired", execID)
			}
			action, _ := args["action"].(string)
			if action == "" {
				action = "poll"
			}
			switch strings.ToLower(action) {
			case "kill":
				if exec.cancel != nil {
					exec.cancel()
				}
				exec.mu.Lock()
				exec.Status = "KILLED"
				exec.mu.Unlock()
			case "wait":
				waitTimeout := 30
				if ts, ok := args["timeout_seconds"].(float64); ok && ts > 0 {
					waitTimeout = int(ts)
				}
				select {
				case <-exec.doneChan:
				case <-time.After(time.Duration(waitTimeout) * time.Second):
				}
			}

			exec.mu.RLock()
			defer exec.mu.RUnlock()
			resp := map[string]any{
				"status":         exec.Status,
				"execution_id":   exec.ID,
				"command":        exec.Command,
				"exit_code":      exec.ExitCode,
				"stdout":         exec.Stdout,
				"stderr":         exec.Stderr,
				"duration_ms":    exec.DurationMs,
				"rolled_back":    exec.RolledBack,
				"advisory":       "Advisory: Sandbox is ephemeral. All filesystem mutations inside the sandbox are isolated from the workspace and discarded on command exit. To persist code changes to your workspace, use 'omnicoder_patch' or 'write_file'.",
			}
			if exec.Error != "" {
				resp["error"] = exec.Error
			}
			if exec.RollbackReason != "" {
				resp["rollback_reason"] = exec.RollbackReason
			}
			if len(exec.Diagnostics) > 0 {
				resp["diagnostics"] = exec.Diagnostics
				resp["diagnostics_count"] = len(exec.Diagnostics)
			}
			return resp, nil
		}

		cmdStr, _ := args["command"].(string)
		scriptStr, _ := args["script"].(string)
		shellStr, _ := args["shell"].(string)
		if strings.TrimSpace(cmdStr) == "" && strings.TrimSpace(scriptStr) == "" {
			return nil, fmt.Errorf("command or script is required")
		}
		timeoutSec := 60
		lowerCmd := strings.ToLower(cmdStr)
		if strings.Contains(lowerCmd, "test") || strings.Contains(lowerCmd, "build") ||
			strings.Contains(lowerCmd, "npm") || strings.Contains(lowerCmd, "yarn") ||
			strings.Contains(lowerCmd, "pnpm") || strings.Contains(lowerCmd, "vitest") ||
			strings.Contains(lowerCmd, "jest") || strings.Contains(lowerCmd, "pytest") {
			timeoutSec = 120
		}
		if ts, ok := args["timeout_seconds"].(float64); ok && ts > 0 {
			timeoutSec = int(ts)
		}
		workdir, _ := args["workdir"].(string)
		isAsync, _ := args["async"].(bool)

		currentWS := s.WorkspaceDir()
		if IsIDEOsOrSystemDir(currentWS) {
			if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
				_ = s.SetWorkspace(ideWS)
				currentWS = s.WorkspaceDir()
			}
		}

		if isAsync {
			asyncID := fmt.Sprintf("sb_%x", time.Now().UnixNano())
			bgCtx, bgCancel := context.WithCancel(context.Background())
			asyncExec := &AsyncExecution{
				ID:        asyncID,
				Command:   cmdStr,
				Script:    scriptStr,
				Workdir:   workdir,
				Status:    "RUNNING",
				StartTime: time.Now().UTC(),
				cancel:    bgCancel,
				doneChan:  make(chan struct{}),
			}
			defaultAsyncSandboxManager.Register(asyncExec)

			go func() {
				defer close(asyncExec.doneChan)
				defer bgCancel()

				vmClient := sCtx.VMClient
				if vmClient == nil {
					vmClient = vm.NewHermeticLocalSandbox(currentWS)
				}
				sb, err := vmClient.CreateSandbox(bgCtx, vm.SandboxConfig{
					WorkspaceDir:   currentWS,
					TimeoutSeconds: timeoutSec,
				})
				if err != nil {
					asyncExec.mu.Lock()
					asyncExec.Status = "FAILED"
					asyncExec.Error = err.Error()
					asyncExec.EndTime = time.Now().UTC()
					asyncExec.DurationMs = time.Since(asyncExec.StartTime).Milliseconds()
					asyncExec.mu.Unlock()
					return
				}
				defer func() {
					_ = vmClient.DestroySandbox(context.Background(), sb.ID)
				}()

				snapID, _ := vmClient.Snapshot(bgCtx, sb.ID, "pre-exec")
				res, err := vmClient.RunCommand(bgCtx, vm.CommandRequest{
					SandboxID:      sb.ID,
					Command:        cmdStr,
					Script:         scriptStr,
					Shell:          shellStr,
					Workdir:        workdir,
					TimeoutSeconds: timeoutSec,
				})

				asyncExec.mu.Lock()
				defer asyncExec.mu.Unlock()
				asyncExec.EndTime = time.Now().UTC()
				asyncExec.DurationMs = time.Since(asyncExec.StartTime).Milliseconds()

				if err != nil {
					if snapID != "" {
						_ = vmClient.Rollback(context.Background(), sb.ID, snapID)
					}
					asyncExec.Status = "FAILED"
					asyncExec.Error = err.Error()
					return
				}

				asyncExec.ExitCode = res.ExitCode
				asyncExec.Stdout = res.Stdout
				asyncExec.Stderr = res.Stderr
				if res.ExitCode == 0 {
					asyncExec.Status = "COMPLETED"
				} else {
					asyncExec.Status = "FAILED"
					if snapID != "" {
						_ = vmClient.Rollback(context.Background(), sb.ID, snapID)
						asyncExec.RolledBack = true
						asyncExec.RollbackReason = fmt.Sprintf("command exited with non-zero status (exit code %d); sandbox state reverted", res.ExitCode)
					}
					cleanErr := devops.StripANSI(res.Stderr)
					cleanOut := devops.StripANSI(res.Stdout)
					diags := devops.ParseCompilerDiagnostics(cleanErr, "")
					if len(diags) == 0 {
						diags = devops.ParseCompilerDiagnostics(cleanOut, "")
					}
					if len(diags) > 0 {
						devops.EnrichDiagnosticsWithSource(currentWS, diags)
						asyncExec.Diagnostics = diags
					}
				}
			}()

			return map[string]any{
				"status":          "RUNNING",
				"execution_id":    asyncID,
				"command":         cmdStr,
				"async":           true,
				"timeout_seconds": timeoutSec,
				"message":         "Command started asynchronously inside microVM sandbox. Poll or wait on status using omnicoder_sandbox with execution_id.",
			}, nil
		}

		if sCtx.VMClient == nil {
			sCtx.VMClient = vm.NewHermeticLocalSandbox(currentWS)
		}

		sb, err := sCtx.VMClient.CreateSandbox(ctx, vm.SandboxConfig{
			WorkspaceDir:   currentWS,
			TimeoutSeconds: timeoutSec,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to provision sandbox: %w", err)
		}
		defer func() {
			_ = sCtx.VMClient.DestroySandbox(ctx, sb.ID)
		}()

		snapID, _ := sCtx.VMClient.Snapshot(ctx, sb.ID, "pre-exec")

		// Keepalive progress streaming ticker to prevent stdio pipe timeouts on Windows
		tickerStop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(1500 * time.Millisecond)
			defer ticker.Stop()
			startT := time.Now()
			for {
				select {
				case <-ticker.C:
					elapsed := int(time.Since(startT).Seconds())
					truncCmd := cmdStr
					if len(truncCmd) > 40 {
						truncCmd = truncCmd[:37] + "..."
					}
					s.ReportProgress(ctx, 0, 0, fmt.Sprintf("Sandbox executing: %s (elapsed %ds)...", truncCmd, elapsed))
				case <-tickerStop:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
		defer close(tickerStop)

		res, err := sCtx.VMClient.RunCommand(ctx, vm.CommandRequest{
			SandboxID:      sb.ID,
			Command:        cmdStr,
			Script:         scriptStr,
			Shell:          shellStr,
			Workdir:        workdir,
			TimeoutSeconds: timeoutSec,
		})
		if err != nil {
			if snapID != "" {
				_ = sCtx.VMClient.Rollback(ctx, sb.ID, snapID)
			}
			return nil, fmt.Errorf("sandbox command execution failed: %w", err)
		}

		rolledBack := false
		var rollbackReason string
		if res.ExitCode != 0 && snapID != "" {
			_ = sCtx.VMClient.Rollback(ctx, sb.ID, snapID)
			rolledBack = true
			rollbackReason = fmt.Sprintf("command exited with non-zero status (exit code %d); sandbox state reverted to snapshot", res.ExitCode)
		}

		// Structured test failure & compiler diagnostics extraction
		var diags []devops.CompilerDiagnostic
		if res.ExitCode != 0 {
			cleanErr := devops.StripANSI(res.Stderr)
			cleanOut := devops.StripANSI(res.Stdout)
			diags = devops.ParseCompilerDiagnostics(cleanErr, "")
			if len(diags) == 0 {
				diags = devops.ParseCompilerDiagnostics(cleanOut, "")
			}
			if len(diags) > 0 {
				devops.EnrichDiagnosticsWithSource(currentWS, diags)
			}
		}

		status := map[string]any{
			"is_ephemeral":           true,
			"persisted_to_workspace": false,
			"workspace_dir":          currentWS,
			"sandbox_dir":            sb.WorkspaceDir,
			"rolled_back":            rolledBack,
		}
		if rollbackReason != "" {
			status["rollback_reason"] = rollbackReason
		}

		resp := map[string]any{
			"exit_code":        res.ExitCode,
			"stdout":           res.Stdout,
			"stderr":           res.Stderr,
			"execution_engine": res.ExecutionEngine,
			"is_truncated":     res.IsTruncated,
			"rolled_back":      rolledBack,
			"vaulted_slot":     res.VaultedSlot,
			"sandbox_status":   status,
			"advisory":         "Advisory: Sandbox is ephemeral. All filesystem mutations inside the sandbox are isolated from the workspace and discarded on command exit. To persist code changes to your workspace, use 'omnicoder_patch' or 'write_file'.",
		}
		if len(diags) > 0 {
			resp["diagnostics_count"] = len(diags)
			resp["diagnostics"] = diags
		}

		return resp, nil
	})

	// 8. omnicoder_run
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_run",
		Description: "Autonomous multi-turn batch coding runner using the 8-stage pipeline. BEST FOR: Background headless batch generation or automated benchmark sweeps. CAUTION: Interactive coding agents (Antigravity, Claude, Cline) should NOT invoke this recursively during active pair-programming sessions to avoid nested execution traps; instead, orchestrate tasks directly.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "Natural language coding task or engineering objective.",
				},
				"max_turns": map[string]any{
					"type":        "integer",
					"description": "Maximum autonomous turns (default: 5).",
				},
				"unbounded": map[string]any{
					"type":        "boolean",
					"description": "If true, runs without turn limit until verified passing (default: false).",
				},
				"target_file": map[string]any{
					"type":        "string",
					"description": "Explicit boundary constraint: restricts all inspections, planning, and mutations strictly to this target file path.",
				},
				"scope": map[string]any{
					"type":        "string",
					"description": "Explicit scoping constraint: restricts actions to files matching this glob or directory prefix.",
				},
				"audit_only": map[string]any{
					"type":        "boolean",
					"description": "Read-only analysis mode: outputs structured diagnostic recommendations without executing speculative in-place patches on disk.",
				},
				"dry_run": map[string]any{
					"type":        "boolean",
					"description": "Alias for audit_only.",
				},
				"json": map[string]any{
					"type":        "boolean",
					"description": "Returns structured JSON diagnostics schema with status, target_symbols, diagnostics, patches_proposed, and execution_ms.",
				},
			},
			"required": []string{"task"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		task, _ := args["task"].(string)
		if strings.TrimSpace(task) == "" {
			return nil, fmt.Errorf("task is required")
		}
		maxTurns := 5
		if mt, ok := args["max_turns"].(float64); ok && mt > 0 {
			maxTurns = int(mt)
		}
		unbounded := false
		if unb, ok := args["unbounded"].(bool); ok {
			unbounded = unb
		}
		targetFile, _ := args["target_file"].(string)
		scope, _ := args["scope"].(string)
		auditOnly := false
		if ao, ok := args["audit_only"].(bool); ok && ao {
			auditOnly = true
		}
		if dr, ok := args["dry_run"].(bool); ok && dr {
			auditOnly = true
		}
		jsonMode := false
		if jm, ok := args["json"].(bool); ok && jm {
			jsonMode = true
		}

		currentWS := s.WorkspaceDir()
		if IsIDEOsOrSystemDir(currentWS) {
			if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
				_ = s.SetWorkspace(ideWS)
			}
		}

		if sCtx.ScopedTaskRunner != nil {
			return sCtx.ScopedTaskRunner(ctx, ScopedRunOptions{
				Task:       task,
				MaxTurns:   maxTurns,
				Unbounded:  unbounded,
				Sandbox:    sCtx.SandboxOn,
				TargetFile: targetFile,
				Scope:      scope,
				AuditOnly:  auditOnly,
				JSON:       jsonMode,
			})
		}

		if sCtx.TaskRunner != nil {
			return sCtx.TaskRunner(ctx, task, maxTurns, unbounded, sCtx.SandboxOn)
		}

		resp := map[string]any{
			"task":             task,
			"status":           "ACK",
			"target_symbols":   []string{},
			"diagnostics":      []any{},
			"patches_proposed": 0,
			"execution_ms":     0,
			"message":          "Direct tool primitives are active. For autonomous turn execution, launch via 'omnicoder run' or configure TaskRunner in server.",
		}
		if targetFile != "" {
			resp["target_file"] = targetFile
		}
		if scope != "" {
			resp["scope"] = scope
		}
		if auditOnly {
			resp["audit_only"] = true
		}
		return resp, nil
	})

	// 9. omnicoder_set_workspace
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_set_workspace",
		Description: "Sets or switches the active workspace root directory across all tools and triggers AST cache pre-warming. Accepts 'path', 'workspace', or 'workspace_dir'.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Absolute or relative path to the new workspace directory. (Aliases: workspace, workspace_dir).",
				},
				"workspace": map[string]any{
					"type":        "string",
					"description": "Optional alias for path.",
				},
				"workspace_dir": map[string]any{
					"type":        "string",
					"description": "Optional alias for path.",
				},
			},
			"required": []string{"path"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		pathStr, _ := args["path"].(string)
		if strings.TrimSpace(pathStr) == "" {
			if ws, ok := args["workspace"].(string); ok && strings.TrimSpace(ws) != "" {
				pathStr = ws
			} else if wsDir, ok := args["workspace_dir"].(string); ok && strings.TrimSpace(wsDir) != "" {
				pathStr = wsDir
			} else if dir, ok := args["dir"].(string); ok && strings.TrimSpace(dir) != "" {
				pathStr = dir
			}
		}
		if strings.TrimSpace(pathStr) == "" {
			return nil, fmt.Errorf("path is required")
		}
		if err := s.SetWorkspace(pathStr); err != nil {
			return nil, fmt.Errorf("failed to set workspace: %w", err)
		}
		newWS := s.WorkspaceDir()
		det := AutoDetectEnvironment(newWS)
		if sCtx.Config.IsOffline {
			det.Backend = BackendOfflineLocal
			det.Model = "embedded-local-cache"
			det.ActiveModel = ""
			det.IsOffline = true
			det.BaseURL = ""
			det.APIKey = ""
		}
		sCtx.Config = det
		if sCtx.SandboxOn {
			sCtx.VMClient = vm.NewHermeticLocalSandbox(newWS)
		}
		return map[string]any{
			"success":        true,
			"status":         "SWITCHED",
			"workspace_root": newWS,
			"backend":        det.Backend,
			"model":          det.Model,
			"has_git":        det.HasGit,
			"is_offline":     det.IsOffline,
			"timestamp":      time.Now().UTC().Format(time.RFC3339),
		}, nil
	})

	// 10. omnicoder_doctor
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_doctor",
		Description: "Authority ground truth oracle for the workspace. Single-call diagnosis: inspects git status (branch, head commit, clean/dirty, uncommitted/untracked files), verifies milestone artifacts without errors, extracts declared version, and runs local compilation/test gates. Generates a markdown evidence badge with rich diagnostics (test pass counts, bundle size, lint status).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"check_gates": map[string]any{
					"type":        "boolean",
					"description": "Whether to run build/vet/test gates (default: true).",
				},
				"check_artifacts": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
					"description": "List of relative file or folder paths to check for presence (e.g. ['Dockerfile', 'scripts/check.ps1']).",
				},
				"include_git": map[string]any{
					"type":        "boolean",
					"description": "Whether to inspect git branch, commit, and uncommitted status (default: true).",
				},
				"extract_version": map[string]any{
					"type":        "boolean",
					"description": "Whether to auto-detect project version from code/config (default: true).",
				},
				"gate_timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Timeout in seconds for gate execution (default: 45).",
				},
				"check_lint": map[string]any{
					"type":        "boolean",
					"description": "Whether to inspect linter and formatting status for badge (default: true).",
				},
				"bundle_paths": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
					"description": "Custom build or distribution paths to measure bundle size for badge (e.g. ['dist', 'build', 'out']).",
				},
			},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		currentWS := s.ResolveEffectiveWorkspace(args, "")

		checkGates := true
		if cg, ok := args["check_gates"].(bool); ok {
			checkGates = cg
		}
		includeGit := true
		if ig, ok := args["include_git"].(bool); ok {
			includeGit = ig
		}
		extractVersion := true
		if ev, ok := args["extract_version"].(bool); ok {
			extractVersion = ev
		}
		timeoutSec := 45
		if to, ok := args["gate_timeout_sec"].(float64); ok && to > 0 {
			timeoutSec = int(to)
		}
		checkLint := true
		if cl, ok := args["check_lint"].(bool); ok {
			checkLint = cl
		}
		var artifacts []string
		if arts, ok := args["check_artifacts"].([]any); ok {
			for _, a := range arts {
				if str, ok := a.(string); ok && str != "" {
					artifacts = append(artifacts, str)
				}
			}
		}
		var bundlePaths []string
		if bp, ok := args["bundle_paths"].([]any); ok {
			for _, p := range bp {
				if str, ok := p.(string); ok && str != "" {
					bundlePaths = append(bundlePaths, str)
				}
			}
		}

		opts := devops.DoctorOptions{
			WorkspaceDir:    currentWS,
			CheckGates:      checkGates,
			CheckArtifacts:  artifacts,
			IncludeGit:      includeGit,
			ExtractVersion:  extractVersion,
			GateTimeoutSecs: timeoutSec,
			CheckLint:       checkLint,
			BundlePaths:     bundlePaths,
		}

		return devops.RunDoctor(ctx, opts)
	})

	// 11. omnicoder_toggle_checklist
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_toggle_checklist",
		Description: "Atomically toggles markdown checklist items (e.g. in docs/paper.md, README.md, or TODO.md) between '- [ ]' and '- [x]' with optional verification notes. Preserves entire document layout, headers, and line endings without needing to read or edit line slices.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path to the markdown file to update (e.g. 'docs/paper.md').",
				},
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"pattern": map[string]any{
								"type":        "string",
								"description": "Substring or regex matching the checklist item line to toggle.",
							},
							"checked": map[string]any{
								"type":        "boolean",
								"description": "true to mark checked [x], false to mark unchecked [ ].",
							},
							"note": map[string]any{
								"type":        "string",
								"description": "Optional evidence, duration, or verification note to append to the item.",
							},
						},
						"required": []string{"pattern", "checked"},
					},
					"description": "List of checklist items to toggle.",
				},
			},
			"required": []string{"path", "items"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		pathStr, _ := args["path"].(string)
		if strings.TrimSpace(pathStr) == "" {
			return nil, fmt.Errorf("path is required")
		}

		itemsRaw, ok := args["items"].([]any)
		if !ok || len(itemsRaw) == 0 {
			return nil, fmt.Errorf("items array is required")
		}

		var toggles []devops.ChecklistToggle
		for _, raw := range itemsRaw {
			if m, ok := raw.(map[string]any); ok {
				pat, _ := m["pattern"].(string)
				chk, _ := m["checked"].(bool)
				note, _ := m["note"].(string)
				if pat != "" {
					toggles = append(toggles, devops.ChecklistToggle{
						Pattern: pat,
						Checked: chk,
						Note:    note,
					})
				}
			}
		}

		currentWS := s.ResolveEffectiveWorkspace(args, pathStr)
		return devops.ToggleChecklist(currentWS, pathStr, toggles)
	})

	// 12. omnicoder_update_section
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_update_section",
		Description: "Atomically replaces a specific Markdown section under a given heading without modifying or disturbing the rest of the document.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative path to the markdown file to update.",
				},
				"heading": map[string]any{
					"type":        "string",
					"description": "Heading text identifying the section to replace (e.g. '## Architecture' or '### Release Checklist').",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "New markdown body content for the section (omitting the heading line itself).",
				},
			},
			"required": []string{"path", "heading", "content"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		pathStr, _ := args["path"].(string)
		if strings.TrimSpace(pathStr) == "" {
			return nil, fmt.Errorf("path is required")
		}
		heading, _ := args["heading"].(string)
		if strings.TrimSpace(heading) == "" {
			return nil, fmt.Errorf("heading is required")
		}
		content, _ := args["content"].(string)

		currentWS := s.ResolveEffectiveWorkspace(args, pathStr)
		return devops.UpdateSection(currentWS, pathStr, heading, content)
	})

	// 13. omnicoder_verify_service
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_verify_service",
		Description: "Builds, launches, and probes a live HTTP service in a single call. Validates routes, security headers (CSP, nosniff, frame denial), payload size limits (413), rate limiting (429), and captures structured live evidence without ad-hoc shell scripts or process leaks.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"entrypoint": map[string]any{
					"type":        "string",
					"description": "Go package entrypoint (default: './cmd/server' or auto-detected).",
				},
				"port": map[string]any{
					"type":        "integer",
					"description": "Port to run server on (default: 8099).",
				},
				"endpoints": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
					"description": "List of routes to probe (default: ['/healthz', '/readyz', '/metrics', '/api/status', '/api/ledger']).",
				},
				"test_rate_limit": map[string]any{
					"type":        "boolean",
					"description": "Send burst requests to verify 429 Too Many Requests (default: true).",
				},
				"test_size_limit": map[string]any{
					"type":        "boolean",
					"description": "Send >1MB payload to verify 413 Payload Too Large (default: true).",
				},
				"test_security_headers": map[string]any{
					"type":        "boolean",
					"description": "Verify presence of CSP, nosniff, frame denial security headers (default: true).",
				},
			},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		currentWS := s.ResolveEffectiveWorkspace(args, "")

		entrypoint, _ := args["entrypoint"].(string)
		port := 8099
		if p, ok := args["port"].(float64); ok && p > 0 {
			port = int(p)
		}

		var endpoints []string
		if eps, ok := args["endpoints"].([]any); ok {
			for _, ep := range eps {
				if str, ok := ep.(string); ok && str != "" {
					endpoints = append(endpoints, str)
				}
			}
		}

		testRL := true
		if trl, ok := args["test_rate_limit"].(bool); ok {
			testRL = trl
		}
		testSL := true
		if tsl, ok := args["test_size_limit"].(bool); ok {
			testSL = tsl
		}
		testSec := true
		if tsec, ok := args["test_security_headers"].(bool); ok {
			testSec = tsec
		}

		opts := devops.ServiceVerifierOptions{
			WorkspaceDir:        currentWS,
			Entrypoint:          entrypoint,
			Port:                port,
			Endpoints:           endpoints,
			TestRateLimit:       testRL,
			TestSizeLimit:       testSL,
			TestSecurityHeaders: testSec,
		}

		return devops.RunServiceVerifier(ctx, opts)
	})

	// 14. omnicoder_swarm
	_ = s.RegisterTool(ToolDefinition{
		Name:        "omnicoder_swarm",
		Description: "Orchestrates a parallel multi-process worker swarm to execute a multi-phase implementation plan DAG in parallel with 2PC verification.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"plan_path": map[string]any{
					"type":        "string",
					"description": "Path to markdown implementation plan (e.g. docs/IMPLEMENTATION_PLAN.md).",
				},
				"max_workers": map[string]any{
					"type":        "integer",
					"description": "Maximum parallel child worker processes (default: 4).",
				},
				"offline": map[string]any{
					"type":        "boolean",
					"description": "Run in offline mode using embedded local cache.",
				},
				"timeout_minutes": map[string]any{
					"type":        "integer",
					"description": "Total swarm timeout budget in minutes (default: 30).",
				},
			},
			"required": []string{"plan_path"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return s.handleOmniCoderSwarm(ctx, args)
	})
}

// HandleOmniCoderToolCall dispatches tool calls with dedicated method routing.
func (s *Server) HandleOmniCoderToolCall(ctx context.Context, name string, params map[string]any) (any, error) {
	switch name {
	case "omnicoder_swarm":
		return s.handleOmniCoderSwarm(ctx, params)
	default:
		return s.Dispatch(ctx, name, params)
	}
}

// handleOmniCoderSwarm executes a parallel multi-process worker swarm against a markdown plan.
func (s *Server) handleOmniCoderSwarm(ctx context.Context, params map[string]any) (any, error) {
	planPath, _ := params["plan_path"].(string)
	planPath = strings.TrimSpace(planPath)
	if planPath == "" {
		return nil, fmt.Errorf("missing required parameter: plan_path")
	}

	currentWS := s.WorkspaceDir()
	if IsIDEOsOrSystemDir(currentWS) {
		if ideWS := DiscoverIDEActiveWorkspace(); ideWS != "" {
			_ = s.SetWorkspace(ideWS)
			currentWS = s.WorkspaceDir()
		}
	}

	maxWorkers := 4
	if mw, ok := params["max_workers"].(float64); ok && mw > 0 {
		maxWorkers = int(mw)
	} else if mwInt, ok := params["max_workers"].(int); ok && mwInt > 0 {
		maxWorkers = mwInt
	}

	offline := false
	if off, ok := params["offline"].(bool); ok {
		offline = off
	}

	timeoutMinutes := 30
	if tm, ok := params["timeout_minutes"].(float64); ok && tm > 0 {
		timeoutMinutes = int(tm)
	} else if tmInt, ok := params["timeout_minutes"].(int); ok && tmInt > 0 {
		timeoutMinutes = tmInt
	}

	det := AutoDetectEnvironment(currentWS)
	if offline {
		det.IsOffline = true
	}

	swarmCfg := orchestrator.SwarmConfig{
		PlanPath:          planPath,
		WorkspaceRoot:     currentWS,
		MaxWorkers:        maxWorkers,
		MaxTurnsPerWorker: 5,
		TimeoutPerPhase:   time.Duration(timeoutMinutes) * time.Minute,
		Model:             det.Model,
		BaseURL:           det.BaseURL,
		APIKey:            det.APIKey,
		Offline:           offline || det.IsOffline,
		Verbose:           false,
		OutputWriter:      os.Stderr, // Stdio framing isolation: os.Stdout is reserved for JSON-RPC 2.0 framing!
	}

	if sCtx := s.getOmniCoderContext(); sCtx != nil {
		if sCtx.SwarmRunner != nil {
			return sCtx.SwarmRunner(ctx, swarmCfg)
		}
		if sCtx.CustomWorkerRunner != nil {
			swarmCfg.CustomWorkerRunner = sCtx.CustomWorkerRunner
		}
	}

	master, err := orchestrator.NewMasterOrchestrator(swarmCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize swarm orchestrator: %w", err)
	}

	if err := master.ExecuteProject(ctx); err != nil {
		return nil, fmt.Errorf("swarm project execution failed: %w", err)
	}

	if res := master.ExecutionResult(); res != nil {
		return res, nil
	}

	dag := master.DAG()
	var waveBreakdown []any
	for idx, w := range dag.Waves {
		waveBreakdown = append(waveBreakdown, map[string]any{
			"wave_index": idx,
			"phases":     w,
		})
	}

	var filesTouched []string
	for _, p := range dag.Phases {
		filesTouched = append(filesTouched, p.TargetFiles...)
	}

	return map[string]any{
		"status":         "SUCCESS",
		"plan_path":      planPath,
		"workspace_root": currentWS,
		"waves_count":    len(dag.Waves),
		"phases_count":   len(dag.Phases),
		"waves":          waveBreakdown,
		"files_touched":  filesTouched,
		"duration_ms":    0,
		"merkle_seal":    canonical.SHA256Of(filesTouched),
	}, nil
}

// ----------------------------------------------------------------------------
// Code Preservation & Go AST Splicing Guards
// ----------------------------------------------------------------------------

func isCodeStub(targetPath, content string) bool {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) >= 80 {
		return false
	}
	ext := strings.ToLower(filepath.Ext(targetPath))
	base := strings.ToLower(filepath.Base(targetPath))

	if ext == ".json" && len(trimmed) >= 2 {
		var js any
		if err := json.Unmarshal([]byte(trimmed), &js); err == nil {
			return false
		}
	}
	if (ext == ".html" || ext == ".htm") && strings.Contains(strings.ToLower(trimmed), "</html>") {
		return false
	}
	if ext == ".py" && len(trimmed) > 30 && devops.ValidatePythonSource(trimmed, targetPath) == nil {
		return false
	}
	if base == "requirements.txt" {
		if valid, _ := devops.ValidateRequirementsTxt(trimmed); valid && len(trimmed) > 5 {
			return false
		}
	}
	return true
}

func isSingleFunctionUpdate(existingCode, incomingCode string) bool {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "", incomingCode, parser.PackageClauseOnly)
	if err != nil {
		return true
	}
	incFuncs := countGoFunctions(incomingCode)
	incTypes := countGoTypes(incomingCode)
	existFuncs := countGoFunctions(existingCode)
	existTypes := countGoTypes(existingCode)

	if incFuncs == 1 && incTypes == 0 && existFuncs >= 2 {
		return true
	}
	if incFuncs == 0 && incTypes == 1 && existTypes >= 2 {
		return true
	}
	return false
}

func tryMergeGoFunction(existingCode, incomingCode string) (string, bool) {
	fsetExist := token.NewFileSet()
	existAST, err := parser.ParseFile(fsetExist, "existing.go", existingCode, parser.ParseComments)
	if err != nil {
		return existingCode, false
	}

	fsetInc := token.NewFileSet()
	incAST, err := parser.ParseFile(fsetInc, "incoming.go", incomingCode, 0)
	prependedDummy := false
	dummyPrefix := "package dummy\n\n"
	if err != nil {
		incAST, err = parser.ParseFile(fsetInc, "incoming.go", dummyPrefix+incomingCode, 0)
		if err != nil {
			return existingCode, false
		}
		prependedDummy = true
	}

	if !prependedDummy {
		existFuncs := countGoFunctions(existingCode)
		existTypes := countGoTypes(existingCode)
		missingFuncs := detectMissingGoFunctions(existingCode, incomingCode)
		missingTypes := detectMissingGoTypes(existingCode, incomingCode)

		if (existFuncs == 0 || len(missingFuncs) == 0) &&
			(existTypes == 0 || len(missingTypes) == 0) {
			return existingCode, false
		}
	}

	type replacement struct {
		name        string
		recv        string
		isType      bool
		isValue     bool
		replaceText string
	}
	var incomingDecls []replacement

	for _, decl := range incAST.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			recvName := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvName = normalizeReceiver(formatReceiverType(d.Recv.List[0].Type))
			}
			startOffset := fsetInc.Position(d.Pos()).Offset
			endOffset := fsetInc.Position(d.End()).Offset
			if prependedDummy {
				startOffset -= len(dummyPrefix)
				endOffset -= len(dummyPrefix)
			}
			if startOffset >= 0 && endOffset <= len(incomingCode) && startOffset < endOffset {
				incomingDecls = append(incomingDecls, replacement{
					name:        d.Name.Name,
					recv:        recvName,
					isType:      false,
					isValue:     false,
					replaceText: incomingCode[startOffset:endOffset],
				})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil {
						startOffset := fsetInc.Position(d.Pos()).Offset
						endOffset := fsetInc.Position(d.End()).Offset
						if len(d.Specs) > 1 {
							startOffset = fsetInc.Position(s.Pos()).Offset
							endOffset = fsetInc.Position(s.End()).Offset
						}
						if prependedDummy {
							startOffset -= len(dummyPrefix)
							endOffset -= len(dummyPrefix)
						}
						if startOffset >= 0 && endOffset <= len(incomingCode) && startOffset < endOffset {
							incomingDecls = append(incomingDecls, replacement{
								name:        s.Name.Name,
								recv:        "",
								isType:      true,
								isValue:     false,
								replaceText: incomingCode[startOffset:endOffset],
							})
						}
					}
				case *ast.ValueSpec:
					for _, nameIdent := range s.Names {
						if nameIdent != nil {
							startOffset := fsetInc.Position(d.Pos()).Offset
							endOffset := fsetInc.Position(d.End()).Offset
							if len(d.Specs) > 1 || len(s.Names) > 1 {
								startOffset = fsetInc.Position(s.Pos()).Offset
								endOffset = fsetInc.Position(s.End()).Offset
							}
							if prependedDummy {
								startOffset -= len(dummyPrefix)
								endOffset -= len(dummyPrefix)
							}
							if startOffset >= 0 && endOffset <= len(incomingCode) && startOffset < endOffset {
								incomingDecls = append(incomingDecls, replacement{
									name:        nameIdent.Name,
									recv:        "",
									isType:      false,
									isValue:     true,
									replaceText: incomingCode[startOffset:endOffset],
								})
								if len(d.Specs) == 1 && len(s.Names) == 1 {
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if len(incomingDecls) == 0 {
		return existingCode, false
	}

	type spanReplacement struct {
		start       int
		end         int
		replaceText string
	}
	var spans []spanReplacement
	var toAppend []string

	for _, inc := range incomingDecls {
		matched := false
		if !inc.isType && !inc.isValue {
			for _, decl := range existAST.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil || fn.Name.Name != inc.name {
					continue
				}
				recvName := ""
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					recvName = normalizeReceiver(formatReceiverType(fn.Recv.List[0].Type))
				}
				if recvName == inc.recv {
					start := fsetExist.Position(fn.Pos()).Offset
					end := fsetExist.Position(fn.End()).Offset
					if start >= 0 && end <= len(existingCode) && start < end {
						spans = append(spans, spanReplacement{
							start:       start,
							end:         end,
							replaceText: inc.replaceText,
						})
						matched = true
						break
					}
				}
			}
		} else if inc.isType {
			for _, decl := range existAST.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gen.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil && ts.Name.Name == inc.name {
						start := fsetExist.Position(gen.Pos()).Offset
						end := fsetExist.Position(gen.End()).Offset
						if len(gen.Specs) > 1 {
							start = fsetExist.Position(ts.Pos()).Offset
							end = fsetExist.Position(ts.End()).Offset
						}
						if start >= 0 && end <= len(existingCode) && start < end {
							spans = append(spans, spanReplacement{
								start:       start,
								end:         end,
								replaceText: inc.replaceText,
							})
							matched = true
							break
						}
					}
				}
				if matched {
					break
				}
			}
		} else if inc.isValue {
			for _, decl := range existAST.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gen.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							if id != nil && id.Name == inc.name {
								start := fsetExist.Position(gen.Pos()).Offset
								end := fsetExist.Position(gen.End()).Offset
								if len(gen.Specs) > 1 || len(vs.Names) > 1 {
									start = fsetExist.Position(vs.Pos()).Offset
									end = fsetExist.Position(vs.End()).Offset
								}
								if start >= 0 && end <= len(existingCode) && start < end {
									spans = append(spans, spanReplacement{
										start:       start,
										end:         end,
										replaceText: inc.replaceText,
									})
									matched = true
									break
								}
							}
						}
					}
					if matched {
						break
					}
				}
				if matched {
					break
				}
			}
		}
		if !matched {
			alreadyAppended := false
			for _, existingApp := range toAppend {
				if existingApp == inc.replaceText {
					alreadyAppended = true
					break
				}
			}
			if !alreadyAppended {
				toAppend = append(toAppend, inc.replaceText)
			}
		}
	}

	if len(spans) == 0 && len(toAppend) == 0 {
		return existingCode, false
	}

	sort.Slice(spans, func(i, j int) bool {
		return spans[i].start > spans[j].start
	})

	merged := existingCode
	for _, s := range spans {
		merged = merged[:s.start] + s.replaceText + merged[s.end:]
	}

	for _, app := range toAppend {
		merged = strings.TrimRight(merged, "\r\n") + "\n\n" + strings.TrimSpace(app) + "\n"
	}

	if len(incAST.Imports) > 0 {
		for _, imp := range incAST.Imports {
			if imp.Path != nil && imp.Path.Value != "" {
				importPath := imp.Path.Value
				if !strings.Contains(merged, importPath) {
					if idx := strings.Index(merged, "import ("); idx != -1 {
						closeIdx := strings.Index(merged[idx:], ")")
						if closeIdx != -1 {
							insertPos := idx + closeIdx
							merged = merged[:insertPos] + "\t" + importPath + "\n" + merged[insertPos:]
						}
					} else {
						if pIdx := strings.Index(merged, "\n"); pIdx != -1 {
							merged = merged[:pIdx+1] + "\nimport " + importPath + "\n" + merged[pIdx+1:]
						}
					}
				}
			}
		}
	}

	testFset := token.NewFileSet()
	if _, err := parser.ParseFile(testFset, "merged.go", merged, 0); err != nil {
		return existingCode, false
	}

	return merged, true
}

func countGoFunctions(code string) int {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", code, 0)
	if err != nil {
		matches := regexp.MustCompile(`(?m)^func\s+`).FindAllString(code, -1)
		return len(matches)
	}
	count := 0
	for _, decl := range node.Decls {
		if _, ok := decl.(*ast.FuncDecl); ok {
			count++
		}
	}
	return count
}

func countGoTypes(code string) int {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", code, 0)
	if err != nil {
		node, _ = parser.ParseFile(fset, "", "package dummy\n\n"+code, 0)
	}
	if node == nil {
		return 0
	}
	count := 0
	for _, decl := range node.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if _, ok := spec.(*ast.TypeSpec); ok {
					count++
				}
			}
		}
	}
	return count
}

func detectMissingGoFunctions(existingCode, incomingCode string) []string {
	fsetExist := token.NewFileSet()
	existAST, err := parser.ParseFile(fsetExist, "exist.go", existingCode, 0)
	if err != nil {
		return nil
	}
	fsetInc := token.NewFileSet()
	incAST, err := parser.ParseFile(fsetInc, "inc.go", incomingCode, 0)
	if err != nil {
		incAST, _ = parser.ParseFile(fsetInc, "inc.go", "package dummy\n\n"+incomingCode, 0)
	}

	existingFuncs := make(map[string]bool)
	for _, decl := range existAST.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
			key := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				key = fmt.Sprintf("(%s).%s", formatReceiverType(fn.Recv.List[0].Type), fn.Name.Name)
			}
			existingFuncs[key] = true
		}
	}

	if incAST != nil {
		for _, decl := range incAST.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
				key := fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					key = fmt.Sprintf("(%s).%s", formatReceiverType(fn.Recv.List[0].Type), fn.Name.Name)
				}
				delete(existingFuncs, key)
			}
		}
	}

	var missing []string
	for k := range existingFuncs {
		missing = append(missing, k)
	}
	sort.Strings(missing)
	return missing
}

func detectMissingGoTypes(existingCode, incomingCode string) []string {
	fsetExist := token.NewFileSet()
	existAST, err := parser.ParseFile(fsetExist, "exist.go", existingCode, 0)
	if err != nil {
		return nil
	}
	fsetInc := token.NewFileSet()
	incAST, err := parser.ParseFile(fsetInc, "inc.go", incomingCode, 0)
	if err != nil {
		incAST, _ = parser.ParseFile(fsetInc, "inc.go", "package dummy\n\n"+incomingCode, 0)
	}

	existingTypes := make(map[string]bool)
	for _, decl := range existAST.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil {
					existingTypes[ts.Name.Name] = true
				}
			}
		}
	}

	if incAST != nil {
		for _, decl := range incAST.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
				for _, spec := range gen.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil {
						delete(existingTypes, ts.Name.Name)
					}
				}
			}
		}
	}

	var missing []string
	for k := range existingTypes {
		missing = append(missing, k)
	}
	sort.Strings(missing)
	return missing
}

func formatReceiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + formatReceiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return formatReceiverType(t.X)
	case *ast.IndexListExpr:
		return formatReceiverType(t.X)
	default:
		return ""
	}
}

func normalizeReceiver(recv string) string {
	return strings.TrimPrefix(recv, "*")
}

// convertUnifiedDiffToEditBlocks converts unified diff hunks (@@ -a,b +c,d @@) into Aider EditBlocks
// so they can be applied using 4-tier fuzzy matching even if line numbers drifted.
func convertUnifiedDiffToEditBlocks(diffText, defaultFile string) ([]devops.EditBlock, bool) {
	if !strings.Contains(diffText, "@@ ") {
		return nil, false
	}
	diffs, err := devops.ParseUnifiedDiff(diffText)
	if err != nil || len(diffs) == 0 {
		return nil, false
	}
	var blocks []devops.EditBlock
	for _, d := range diffs {
		target := defaultFile
		if target == "" {
			if d.NewPath != "" && d.NewPath != "/dev/null" {
				target = d.NewPath
			} else if d.OldPath != "" && d.OldPath != "/dev/null" {
				target = d.OldPath
			}
		}
		for _, h := range d.Hunks {
			var sLines, rLines []string
			for _, l := range h.Lines {
				if strings.HasPrefix(l, "-") {
					sLines = append(sLines, l[1:])
				} else if strings.HasPrefix(l, "+") {
					rLines = append(rLines, l[1:])
				} else if strings.HasPrefix(l, " ") {
					sLines = append(sLines, l[1:])
					rLines = append(rLines, l[1:])
				} else if l == "" {
					sLines = append(sLines, "")
					rLines = append(rLines, "")
				}
			}
			if len(sLines) > 0 || len(rLines) > 0 {
				blocks = append(blocks, devops.EditBlock{
					FilePath:    filepath.ToSlash(target),
					SearchText:  strings.Join(sLines, "\n"),
					ReplaceText: strings.Join(rLines, "\n"),
				})
			}
		}
	}
	return blocks, len(blocks) > 0
}

// applyLineRangePatch replaces 1-indexed lines [startLine, endLine] (inclusive) with newContent.
// If startLine == len(lines)+1, newContent is appended to the file.
func applyLineRangePatch(existingStr string, startLine, endLine int, newContent string) (string, error) {
	lineEnding := "\n"
	if strings.Contains(existingStr, "\r\n") {
		lineEnding = "\r\n"
	}

	if existingStr == "" {
		return newContent, nil
	}

	normalized := strings.ReplaceAll(existingStr, "\r\n", "\n")
	hadTrailingNewline := strings.HasSuffix(normalized, "\n")
	if hadTrailingNewline {
		normalized = strings.TrimSuffix(normalized, "\n")
	}

	lines := strings.Split(normalized, "\n")
	totalLines := len(lines)

	if startLine < 1 {
		startLine = 1
	}
	if startLine > totalLines+1 {
		return "", fmt.Errorf("start_line %d exceeds total file lines %d", startLine, totalLines)
	}

	if endLine <= 0 || endLine < startLine {
		endLine = startLine
	}
	if endLine > totalLines {
		endLine = totalLines
	}

	var replacementLines []string
	if len(newContent) > 0 {
		newContentNorm := strings.ReplaceAll(newContent, "\r\n", "\n")
		if strings.HasSuffix(newContentNorm, "\n") {
			newContentNorm = strings.TrimSuffix(newContentNorm, "\n")
		}
		replacementLines = strings.Split(newContentNorm, "\n")
	}

	prefix := lines[:startLine-1]
	var suffix []string
	if endLine < totalLines {
		suffix = lines[endLine:]
	}

	resultLines := make([]string, 0, len(prefix)+len(replacementLines)+len(suffix))
	resultLines = append(resultLines, prefix...)
	resultLines = append(resultLines, replacementLines...)
	resultLines = append(resultLines, suffix...)

	res := strings.Join(resultLines, lineEnding)
	if hadTrailingNewline && len(resultLines) > 0 {
		res += lineEnding
	}
	return res, nil
}

// PatchChunk models a single slice replacement in multi-chunk atomic refactoring.
type PatchChunk struct {
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Content string `json:"content"`
}

// applyMultiChunkPatch validates and applies multiple non-contiguous line-range edits
// atomically from highest line number to lowest (bottom-up), ensuring line index stability.
func applyMultiChunkPatch(existingStr string, chunks []PatchChunk) (string, int, int, error) {
	if len(chunks) == 0 {
		return existingStr, 0, 0, nil
	}

	normalized := strings.ReplaceAll(existingStr, "\r\n", "\n")
	totalLines := len(strings.Split(strings.TrimSuffix(normalized, "\n"), "\n"))
	if existingStr == "" {
		totalLines = 0
	}

	// 1. Validate chunks: start >= 1, end >= start
	validated := make([]PatchChunk, len(chunks))
	for i, ch := range chunks {
		s := ch.Start
		if s < 1 {
			s = 1
		}
		e := ch.End
		if e <= 0 || e < s {
			e = s
		}
		if s > totalLines+1 {
			return "", 0, 0, fmt.Errorf("chunk %d: start %d exceeds total file lines %d", i+1, s, totalLines)
		}
		validated[i] = PatchChunk{
			Start:   s,
			End:     e,
			Content: ch.Content,
		}
	}

	// 2. Overlap detection
	sortedAsc := make([]PatchChunk, len(validated))
	copy(sortedAsc, validated)
	sort.Slice(sortedAsc, func(i, j int) bool {
		if sortedAsc[i].Start == sortedAsc[j].Start {
			return sortedAsc[i].End < sortedAsc[j].End
		}
		return sortedAsc[i].Start < sortedAsc[j].Start
	})

	for i := 0; i < len(sortedAsc)-1; i++ {
		if sortedAsc[i].End >= sortedAsc[i+1].Start {
			return "", 0, 0, fmt.Errorf("overlapping chunk ranges detected: [%d..%d] overlaps with [%d..%d]",
				sortedAsc[i].Start, sortedAsc[i].End, sortedAsc[i+1].Start, sortedAsc[i+1].End)
		}
	}

	// 3. Sort bottom-up (Start descending) to preserve line indices
	sortedDesc := make([]PatchChunk, len(validated))
	copy(sortedDesc, validated)
	sort.Slice(sortedDesc, func(i, j int) bool {
		return sortedDesc[i].Start > sortedDesc[j].Start
	})

	// 4. Apply bottom-up
	current := existingStr
	for _, ch := range sortedDesc {
		newCur, err := applyLineRangePatch(current, ch.Start, ch.End, ch.Content)
		if err != nil {
			return "", 0, 0, fmt.Errorf("failed applying chunk [%d..%d]: %w", ch.Start, ch.End, err)
		}
		current = newCur
	}

	origLines := strings.Count(existingStr, "\n")
	newLines := strings.Count(current, "\n")
	delta := newLines - origLines
	linesAdded := 0
	linesRemoved := 0
	if delta > 0 {
		linesAdded = delta
	} else if delta < 0 {
		linesRemoved = -delta
	}

	return current, linesAdded, linesRemoved, nil
}

// AsyncExecution encapsulates background microVM command execution state.
type AsyncExecution struct {
	ID             string                      `json:"id"`
	Command        string                      `json:"command"`
	Script         string                      `json:"script,omitempty"`
	Workdir        string                      `json:"workdir,omitempty"`
	Status         string                      `json:"status"` // "RUNNING", "COMPLETED", "FAILED", "KILLED"
	StartTime      time.Time                   `json:"start_time"`
	EndTime        time.Time                   `json:"end_time,omitempty"`
	DurationMs     int64                       `json:"duration_ms"`
	ExitCode       int                         `json:"exit_code"`
	Stdout         string                      `json:"stdout"`
	Stderr         string                      `json:"stderr"`
	Error          string                      `json:"error,omitempty"`
	Diagnostics    []devops.CompilerDiagnostic `json:"diagnostics,omitempty"`
	RolledBack     bool                        `json:"rolled_back"`
	RollbackReason string                      `json:"rollback_reason,omitempty"`
	cancel         context.CancelFunc
	doneChan       chan struct{}
	mu             sync.RWMutex
}

type AsyncSandboxManager struct {
	mu         sync.RWMutex
	executions map[string]*AsyncExecution
}

var defaultAsyncSandboxManager = &AsyncSandboxManager{
	executions: make(map[string]*AsyncExecution),
}

func (m *AsyncSandboxManager) Register(exec *AsyncExecution) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executions[exec.ID] = exec
}

func (m *AsyncSandboxManager) Get(id string) (*AsyncExecution, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	exec, ok := m.executions[id]
	return exec, ok
}

