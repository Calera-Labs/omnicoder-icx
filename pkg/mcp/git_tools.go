// SPDX-License-Identifier: Apache-2.0
// Package mcp implements the Model Context Protocol (MCP) JSON-RPC 2.0 stdio server,
// tool registry, and sandboxed filesystem/git tools for OmniCoder-ICX.

package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitStatusResult reports structured git status.
type GitStatusResult struct {
	Branch       string   `json:"branch"`
	Clean        bool     `json:"clean"`
	Staged       []string `json:"staged"`
	Modified     []string `json:"modified"`
	Untracked    []string `json:"untracked"`
	TotalChanges int      `json:"total_changes"`
}

// GitDiffResult reports unified diff string and metadata.
type GitDiffResult struct {
	Diff      string `json:"diff"`
	Staged    bool   `json:"staged"`
	Path      string `json:"path,omitempty"`
	Empty     bool   `json:"empty"`
	LineCount int    `json:"line_count"`
}

// GitCommitResult reports commit hash and summary.
type GitCommitResult struct {
	CommitHash string   `json:"commit_hash"`
	Message    string   `json:"message"`
	Files      []string `json:"files,omitempty"`
	Summary    string   `json:"summary"`
	Branch     string   `json:"branch"`
}

// Tool Schemas (Draft 2020-12 compatible)
var GitStatusSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"repo_path": map[string]any{
			"type":        "string",
			"description": "Optional repository path relative to workspace (default: workspace root).",
		},
	},
}

var GitDiffSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"staged": map[string]any{
			"type":        "boolean",
			"description": "Whether to view staged diff (--cached) instead of unstaged diff (default: false).",
		},
		"path": map[string]any{
			"type":        "string",
			"description": "Optional file path to limit the diff.",
		},
		"repo_path": map[string]any{
			"type":        "string",
			"description": "Optional repository path relative to workspace (default: workspace root).",
		},
	},
}

var GitCommitSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Commit message describing the changes.",
		},
		"files": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
			"description": "Optional list of specific files to stage and commit. If omitted or empty, stages all changes (git add -A).",
		},
		"repo_path": map[string]any{
			"type":        "string",
			"description": "Optional repository path relative to workspace (default: workspace root).",
		},
	},
	"required": []string{"message"},
}

// RegisterGitTools registers git_status, git_diff, and git_commit on the server.
func RegisterGitTools(s *Server, workspaceDir string) {
	if workspaceDir != "" && s.WorkspaceDir() == "" {
		_ = s.SetWorkspace(workspaceDir)
	}

	defStatus, _ := NewGitStatusTool(s.WorkspaceDir())
	_ = s.RegisterTool(defStatus, func(ctx context.Context, args map[string]any) (any, error) {
		rp, _ := args["repo_path"].(string)
		ws := s.ResolveEffectiveWorkspace(args, rp)
		return HandleGitStatus(ctx, ws, args)
	})

	defDiff, _ := NewGitDiffTool(s.WorkspaceDir())
	_ = s.RegisterTool(defDiff, func(ctx context.Context, args map[string]any) (any, error) {
		p, _ := args["path"].(string)
		if p == "" {
			p, _ = args["repo_path"].(string)
		}
		ws := s.ResolveEffectiveWorkspace(args, p)
		return HandleGitDiff(ctx, ws, args)
	})

	defCommit, _ := NewGitCommitTool(s.WorkspaceDir())
	_ = s.RegisterTool(defCommit, func(ctx context.Context, args map[string]any) (any, error) {
		rp, _ := args["repo_path"].(string)
		ws := s.ResolveEffectiveWorkspace(args, rp)
		return HandleGitCommit(ctx, ws, args)
	})
}

// NewGitStatusTool returns the definition and handler for git_status.
func NewGitStatusTool(workspaceDir string) (ToolDefinition, ToolHandler) {
	return ToolDefinition{
		Name:        "git_status",
		Description: "Inspects git repository status reporting branch, staged files, modified files, untracked files, and clean status.",
		InputSchema: GitStatusSchema,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return HandleGitStatus(ctx, workspaceDir, args)
	}
}

// NewGitDiffTool returns the definition and handler for git_diff.
func NewGitDiffTool(workspaceDir string) (ToolDefinition, ToolHandler) {
	return ToolDefinition{
		Name:        "git_diff",
		Description: "Generates unified diff of staged or unstaged changes, optionally scoped to a file.",
		InputSchema: GitDiffSchema,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return HandleGitDiff(ctx, workspaceDir, args)
	}
}

// NewGitCommitTool returns the definition and handler for git_commit.
func NewGitCommitTool(workspaceDir string) (ToolDefinition, ToolHandler) {
	return ToolDefinition{
		Name:        "git_commit",
		Description: "Stages specified files (or all changes if empty) and creates a git commit.",
		InputSchema: GitCommitSchema,
	}, func(ctx context.Context, args map[string]any) (any, error) {
		return HandleGitCommit(ctx, workspaceDir, args)
	}
}

// HandleGitStatus queries working tree status via git status --porcelain=v1 -b.
func HandleGitStatus(ctx context.Context, workspaceDir string, args map[string]any) (*GitStatusResult, error) {
	repoDir, err := resolveRepoDirectory(workspaceDir, getOptionalStringArg(args, "repo_path", ""))
	if err != nil {
		return nil, err
	}

	stdout, _, err := runGit(ctx, repoDir, "status", "--porcelain=v1", "-b")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(stdout, "\n")
	branch := "unknown"
	staged := make([]string, 0)
	modified := make([]string, 0)
	untracked := make([]string, 0)

	for i, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		if i == 0 && strings.HasPrefix(line, "## ") {
			branch = parseBranchHeader(line[3:])
			continue
		}

		if len(line) < 3 {
			continue
		}

		code := line[0:2]
		filePath := strings.TrimSpace(line[3:])
		if strings.Contains(filePath, " -> ") {
			parts := strings.Split(filePath, " -> ")
			filePath = parts[len(parts)-1]
		}
		cleanPath := filepath.ToSlash(filePath)
		if strings.HasPrefix(cleanPath, ".omnicoder/") || cleanPath == ".omnicoder" {
			continue
		}

		x := code[0]
		y := code[1]

		if x == '?' && y == '?' {
			untracked = append(untracked, cleanPath)
		} else {
			if x != ' ' && x != '?' {
				staged = append(staged, cleanPath)
			}
			if y != ' ' && y != '?' {
				modified = append(modified, cleanPath)
			}
		}
	}

	clean := len(staged) == 0 && len(modified) == 0 && len(untracked) == 0
	total := len(staged) + len(modified) + len(untracked)

	return &GitStatusResult{
		Branch:       branch,
		Clean:        clean,
		Staged:       staged,
		Modified:     modified,
		Untracked:    untracked,
		TotalChanges: total,
	}, nil
}

// HandleGitDiff runs git diff or git diff --cached.
func HandleGitDiff(ctx context.Context, workspaceDir string, args map[string]any) (*GitDiffResult, error) {
	repoDir, err := resolveRepoDirectory(workspaceDir, getOptionalStringArg(args, "repo_path", ""))
	if err != nil {
		return nil, err
	}

	staged := getOptionalBoolArg(args, "staged", false)
	pathFilter := getOptionalStringArg(args, "path", "")

	cmdArgs := []string{"diff"}
	if staged {
		cmdArgs = append(cmdArgs, "--cached")
	}
	if pathFilter != "" {
		cmdArgs = append(cmdArgs, "--", pathFilter)
	}

	stdout, _, err := runGit(ctx, repoDir, cmdArgs...)
	if err != nil {
		return nil, err
	}

	diffStr := stdout
	empty := strings.TrimSpace(diffStr) == ""
	lineCount := 0
	if !empty {
		lineCount = len(strings.Split(strings.TrimRight(diffStr, "\r\n"), "\n"))
	}

	return &GitDiffResult{
		Diff:      diffStr,
		Staged:    staged,
		Path:      pathFilter,
		Empty:     empty,
		LineCount: lineCount,
	}, nil
}

// HandleGitCommit stages files and creates a git commit.
func HandleGitCommit(ctx context.Context, workspaceDir string, args map[string]any) (*GitCommitResult, error) {
	repoDir, err := resolveRepoDirectory(workspaceDir, getOptionalStringArg(args, "repo_path", ""))
	if err != nil {
		return nil, err
	}

	message, err := getStringArg(args, "message", true)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("commit message cannot be empty")
	}

	files, err := extractStringSlice(args["files"])
	if err != nil {
		return nil, fmt.Errorf("invalid files argument: %w", err)
	}

	// 1. Stage files
	if len(files) > 0 {
		addArgs := append([]string{"add", "--"}, files...)
		if _, _, err := runGit(ctx, repoDir, addArgs...); err != nil {
			return nil, fmt.Errorf("git add failed: %w", err)
		}
	} else {
		if _, _, err := runGit(ctx, repoDir, "add", "-A"); err != nil {
			return nil, fmt.Errorf("git add -A failed: %w", err)
		}
	}

	// 2. Commit
	commitOut, commitErr, err := runGit(ctx, repoDir, "commit", "-m", message)
	if err != nil {
		combined := strings.ToLower(commitOut + " " + commitErr)
		if strings.Contains(combined, "nothing to commit") || strings.Contains(combined, "working tree clean") || strings.Contains(combined, "nothing added to commit") {
			return nil, ErrNothingToCommit
		}
		return nil, fmt.Errorf("git commit failed: %w", err)
	}

	// 3. Inspect committed SHA
	shaOut, _, err := runGit(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve commit hash: %w", err)
	}
	commitHash := strings.TrimSpace(shaOut)

	// 4. Inspect branch
	branchOut, _, _ := runGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	branch := strings.TrimSpace(branchOut)

	summary := strings.TrimSpace(strings.Split(message, "\n")[0])

	return &GitCommitResult{
		CommitHash: commitHash,
		Message:    message,
		Files:      files,
		Summary:    summary,
		Branch:     branch,
	}, nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", "", ErrGitNotFound
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	outStr := stdout.String()
	errStr := stderr.String()

	if err != nil {
		combined := strings.ToLower(outStr + " " + errStr)
		if strings.Contains(combined, "not a git repository") {
			return outStr, errStr, ErrNotGitRepo
		}
		return outStr, errStr, fmt.Errorf("%w: %s", ErrGitExecution, strings.TrimSpace(errStr))
	}

	return outStr, errStr, nil
}

func resolveRepoDirectory(workspaceDir, repoPath string) (string, error) {
	if repoPath == "" || repoPath == "." {
		return ResolveSecurePath(workspaceDir, ".")
	}
	return ResolveSecurePath(workspaceDir, repoPath)
}

func parseBranchHeader(header string) string {
	if strings.HasPrefix(header, "Initial commit on ") {
		return strings.TrimPrefix(header, "Initial commit on ")
	}
	if strings.HasPrefix(header, "No commits yet on ") {
		return strings.TrimPrefix(header, "No commits yet on ")
	}
	if strings.HasPrefix(header, "HEAD (no branch)") {
		return "HEAD (detached)"
	}

	parts := strings.Split(header, "...")
	fields := strings.Fields(parts[0])
	if len(fields) > 0 {
		return fields[0]
	}
	return parts[0]
}
