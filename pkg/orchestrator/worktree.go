// Package orchestrator implements the isolated multi-process parallel agent swarm engine
// for OmniCoder-ICX.
package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// WorktreeAllocation represents an active, isolated workspace shard.
type WorktreeAllocation struct {
	PhaseID     int       `json:"phase_id"`
	BranchName  string    `json:"branch_name"`
	WorktreeDir string    `json:"worktree_dir"`
	BaseCommit  string    `json:"base_commit"`
	CreatedAt   time.Time `json:"created_at"`
	IsJunction  bool      `json:"is_junction"`
	mu          sync.Mutex
}

// WorktreeManager coordinates the lifecycle of transient worker directories.
type WorktreeManager struct {
	workspaceRoot string
	worktreesBase string
	allocations   map[int]*WorktreeAllocation
	mu            sync.RWMutex
	cleanOnce     sync.Once
}

// NewWorktreeManager initializes a WorktreeManager targeting the specified workspaceRoot.
func NewWorktreeManager(workspaceRoot string) (*WorktreeManager, error) {
	if workspaceRoot == "" {
		workspaceRoot = "."
	}
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace root: %w", err)
	}

	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("workspace root does not exist: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory: %s", absRoot)
	}

	worktreesBase := filepath.Join(absRoot, ".worktrees")
	if err := os.MkdirAll(worktreesBase, 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktrees base directory %s: %w", worktreesBase, err)
	}

	return &WorktreeManager{
		workspaceRoot: absRoot,
		worktreesBase: worktreesBase,
		allocations:   make(map[int]*WorktreeAllocation),
	}, nil
}

// Allocate provisions a transient, hermetically isolated worktree for the given phase.
func (wm *WorktreeManager) Allocate(ctx context.Context, phaseID int, baseBranch string) (*WorktreeAllocation, error) {
	if baseBranch == "" {
		baseBranch = "HEAD"
	}

	branchName := fmt.Sprintf("swarm/worker-phase-%d", phaseID)
	worktreeDir := filepath.Join(wm.worktreesBase, fmt.Sprintf("wt_phase_%d", phaseID))

	// Resolve base commit hash
	baseCommit := baseBranch
	revCmd := exec.CommandContext(ctx, "git", "rev-parse", baseBranch)
	revCmd.Dir = wm.workspaceRoot
	if out, err := revCmd.Output(); err == nil {
		baseCommit = strings.TrimSpace(string(out))
	}

	// Clean up stale worktree or branch if exists
	if _, err := os.Stat(worktreeDir); err == nil {
		rmCmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", worktreeDir)
		rmCmd.Dir = wm.workspaceRoot
		_ = rmCmd.Run()
		_ = os.RemoveAll(worktreeDir)
	}

	// Execute git worktree add with retry for transient lock contention
	var isJunction bool
	var gitErr error
	maxRetries := 5
	backoff := 20 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-B", branchName, worktreeDir, baseCommit)
		cmd.Dir = wm.workspaceRoot
		var out []byte
		out, gitErr = cmd.CombinedOutput()
		if gitErr == nil {
			break
		}

		errStr := string(out)
		// Check for common Git lock contention
		if strings.Contains(errStr, "lock") || strings.Contains(errStr, "Unable to create") {
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		// Non-lock error (or final attempt)
		break
	}

	if gitErr != nil {
		// Windows NTFS Junction Fallback when Git worktree fails or is unsupported
		if err := os.MkdirAll(worktreeDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create worktree directory: %w (git error: %v)", err, gitErr)
		}
		isJunction = true
	}

	// Ensure dedicated compiler output directory exists: .worktrees/wt_phase_<id>/.bin
	binDir := filepath.Join(worktreeDir, ".bin")
	_ = os.MkdirAll(binDir, 0755)

	alloc := &WorktreeAllocation{
		PhaseID:     phaseID,
		BranchName:  branchName,
		WorktreeDir: worktreeDir,
		BaseCommit:  baseCommit,
		CreatedAt:   time.Now(),
		IsJunction:  isJunction,
	}

	wm.mu.Lock()
	wm.allocations[phaseID] = alloc
	wm.mu.Unlock()

	return alloc, nil
}

// Prune unlinks and deletes the isolated worktree and transient git branch for a specific phase.
func (wm *WorktreeManager) Prune(ctx context.Context, phaseID int) error {
	wm.mu.Lock()
	alloc := wm.allocations[phaseID]
	delete(wm.allocations, phaseID)
	wm.mu.Unlock()

	worktreeDir := filepath.Join(wm.worktreesBase, fmt.Sprintf("wt_phase_%d", phaseID))
	branchName := fmt.Sprintf("swarm/worker-phase-%d", phaseID)
	isJunction := false

	if alloc != nil {
		worktreeDir = alloc.WorktreeDir
		branchName = alloc.BranchName
		isJunction = alloc.IsJunction
	}

	if !isJunction {
		// 1. Remove git worktree
		cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", worktreeDir)
		cmd.Dir = wm.workspaceRoot
		_ = cmd.Run()

		// 2. Prune git worktree administrative records
		pruneCmd := exec.CommandContext(ctx, "git", "worktree", "prune")
		pruneCmd.Dir = wm.workspaceRoot
		_ = pruneCmd.Run()

		// 3. Delete transient branch
		delBranchCmd := exec.CommandContext(ctx, "git", "branch", "-D", branchName)
		delBranchCmd.Dir = wm.workspaceRoot
		_ = delBranchCmd.Run()
	}

	// 4. Clean directory on disk if remaining
	if _, err := os.Stat(worktreeDir); err == nil {
		_ = os.RemoveAll(worktreeDir)
	}

	return nil
}

// PruneAll tears down all active worktrees, unlinks junctions, and deletes all transient branches.
func (wm *WorktreeManager) PruneAll(ctx context.Context) error {
	wm.mu.Lock()
	phaseIDs := make([]int, 0, len(wm.allocations))
	for id := range wm.allocations {
		phaseIDs = append(phaseIDs, id)
	}
	wm.mu.Unlock()

	var errs []string
	for _, id := range phaseIDs {
		if err := wm.Prune(ctx, id); err != nil {
			errs = append(errs, fmt.Sprintf("phase %d: %v", id, err))
		}
	}

	// Also discover and prune any lingering wt_phase_* directories in worktreesBase
	entries, err := os.ReadDir(wm.worktreesBase)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "wt_phase_") {
				p := filepath.Join(wm.worktreesBase, entry.Name())
				cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", p)
				cmd.Dir = wm.workspaceRoot
				_ = cmd.Run()
				_ = os.RemoveAll(p)
			}
		}
	}

	// Final git worktree prune
	pruneCmd := exec.CommandContext(ctx, "git", "worktree", "prune")
	pruneCmd.Dir = wm.workspaceRoot
	_ = pruneCmd.Run()

	// Clean transient branches matching swarm/worker-phase-*
	branchListCmd := exec.CommandContext(ctx, "git", "branch", "--list", "swarm/worker-phase-*")
	branchListCmd.Dir = wm.workspaceRoot
	if out, err := branchListCmd.Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			branch := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
			if branch != "" {
				delCmd := exec.CommandContext(ctx, "git", "branch", "-D", branch)
				delCmd.Dir = wm.workspaceRoot
				_ = delCmd.Run()
			}
		}
	}

	// Clean unlinking of .worktrees directory
	_ = os.RemoveAll(wm.worktreesBase)

	if len(errs) > 0 {
		return fmt.Errorf("prune errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// LinkJunction instantiates an NTFS Directory Junction on Windows (mklink /J)
// or standard directory symlink on Unix platforms.
func (wm *WorktreeManager) LinkJunction(targetDir, linkPath string) error {
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("failed to resolve target directory: %w", err)
	}
	absLink, err := filepath.Abs(linkPath)
	if err != nil {
		return fmt.Errorf("failed to resolve link path: %w", err)
	}

	info, err := os.Stat(absTarget)
	if err != nil {
		return fmt.Errorf("junction target does not exist: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("junction target is not a directory: %s", absTarget)
	}

	if err := os.MkdirAll(filepath.Dir(absLink), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory for junction: %w", err)
	}

	// Remove existing link if present
	_ = os.Remove(absLink)

	if runtime.GOOS == "windows" {
		// Use mklink /J for NTFS directory junction without requiring administrator privileges
		cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", absLink, absTarget)
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			// Fallback to os.Symlink if mklink returns error
			symErr := os.Symlink(absTarget, absLink)
			if symErr == nil {
				return nil
			}
			return fmt.Errorf("failed to create NTFS junction via mklink (%s) or symlink: %w", string(out), symErr)
		}
	}

	return os.Symlink(absTarget, absLink)
}

// RegisterCleanupOnInterrupt registers an OS signal listener to ensure PruneAll is called on Ctrl+C / SIGTERM.
func (wm *WorktreeManager) RegisterCleanupOnInterrupt() {
	wm.cleanOnce.Do(func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = wm.PruneAll(ctx)
		}()
	})
}

// WorkspaceRoot returns the manager's target workspace root.
func (wm *WorktreeManager) WorkspaceRoot() string {
	return wm.workspaceRoot
}

// WorktreesBase returns the base directory where worker shards are placed (.worktrees).
func (wm *WorktreeManager) WorktreesBase() string {
	return wm.worktreesBase
}

// GetAllocation returns an active allocation by phase ID.
func (wm *WorktreeManager) GetAllocation(phaseID int) (*WorktreeAllocation, bool) {
	wm.mu.RLock()
	defer wm.mu.RUnlock()
	alloc, ok := wm.allocations[phaseID]
	return alloc, ok
}

// Allocations returns a copy of all active allocations.
func (wm *WorktreeManager) Allocations() map[int]*WorktreeAllocation {
	wm.mu.RLock()
	defer wm.mu.RUnlock()
	copyMap := make(map[int]*WorktreeAllocation, len(wm.allocations))
	for k, v := range wm.allocations {
		copyMap[k] = v
	}
	return copyMap
}

// BinDir returns the dedicated compiler output directory for this worktree shard.
func (wa *WorktreeAllocation) BinDir() string {
	return filepath.Join(wa.WorktreeDir, ".bin")
}

// OutputBinaryPath returns an isolated executable path within .bin.
func (wa *WorktreeAllocation) OutputBinaryPath(binaryName string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(binaryName, ".exe") {
		binaryName += ".exe"
	}
	return filepath.Join(wa.WorktreeDir, ".bin", binaryName)
}
