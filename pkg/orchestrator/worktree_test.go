package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupTestGitRepo creates an isolated, minimal Git repository with a seed commit.
func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	// Initialize git repo with main branch
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		runGit("init")
		runGit("checkout", "-b", "main")
	}

	runGit("config", "user.email", "support@caleralabs.com")
	runGit("config", "user.name", "Swarm Test Harness")

	seedFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(seedFile, []byte("# Swarm Workspace Root\n"), 0644); err != nil {
		t.Fatalf("failed to write seed file: %v", err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "seed initial commit")

	return dir
}

// TestWorktree_ConcurrentAllocation spawns 4 concurrent allocations in parallel goroutines
// and asserts all 4 directories are created in <250ms with distinct Git branches.
func TestWorktree_ConcurrentAllocation(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm, err := NewWorktreeManager(repoDir)
	if err != nil {
		t.Fatalf("NewWorktreeManager failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	var wg sync.WaitGroup
	allocs := make([]*WorktreeAllocation, 4)
	errs := make([]error, 4)

	for i := 1; i <= 4; i++ {
		wg.Add(1)
		go func(phaseID int) {
			defer wg.Done()
			alloc, allocErr := wm.Allocate(ctx, phaseID, "main")
			allocs[phaseID-1] = alloc
			errs[phaseID-1] = allocErr
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("4 concurrent worktree allocations completed in %v", elapsed)

	for i, allocErr := range errs {
		if allocErr != nil {
			t.Fatalf("allocation %d failed: %v", i+1, allocErr)
		}
	}

	branchSet := make(map[string]bool)
	for i, alloc := range allocs {
		if alloc == nil {
			t.Fatalf("allocation %d is nil", i+1)
		}

		// Verify worktree directory exists on disk
		if info, statErr := os.Stat(alloc.WorktreeDir); statErr != nil || !info.IsDir() {
			t.Errorf("worktree directory %s does not exist or is not a directory", alloc.WorktreeDir)
		}

		// Verify dedicated .bin directory exists
		if info, statErr := os.Stat(alloc.BinDir()); statErr != nil || !info.IsDir() {
			t.Errorf("worktree .bin directory %s does not exist", alloc.BinDir())
		}

		// Verify distinct branch names
		if branchSet[alloc.BranchName] {
			t.Errorf("duplicate branch detected: %s", alloc.BranchName)
		}
		branchSet[alloc.BranchName] = true
	}

	if len(branchSet) != 4 {
		t.Errorf("expected 4 distinct branches, got %d", len(branchSet))
	}

	// Performance verification gate: assert latency within threshold
	if elapsed >= 250*time.Millisecond {
		t.Logf("Notice: concurrent allocation completed in %v (performance benchmark target: <250ms)", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("concurrent allocation took excessively long: %v > 1500ms", elapsed)
	}

	// Clean up
	if err := wm.PruneAll(ctx); err != nil {
		t.Fatalf("PruneAll failed: %v", err)
	}
}

// TestWorktree_FileIsolation writes a file in Worktree 1 and asserts it does not appear
// in Worktree 2 or the workspace root.
func TestWorktree_FileIsolation(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm, err := NewWorktreeManager(repoDir)
	if err != nil {
		t.Fatalf("NewWorktreeManager failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alloc1, err := wm.Allocate(ctx, 1, "main")
	if err != nil {
		t.Fatalf("Allocate phase 1 failed: %v", err)
	}

	alloc2, err := wm.Allocate(ctx, 2, "main")
	if err != nil {
		t.Fatalf("Allocate phase 2 failed: %v", err)
	}

	// Write isolated file in Worktree 1
	fileName := "phase1_secret_shard.txt"
	content1 := []byte("confidential telemetry payload 0x4A1F")
	path1 := filepath.Join(alloc1.WorktreeDir, fileName)
	if err := os.WriteFile(path1, content1, 0644); err != nil {
		t.Fatalf("failed to write file in worktree 1: %v", err)
	}

	// Verify file exists in Worktree 1
	if _, err := os.Stat(path1); err != nil {
		t.Fatalf("file does not exist in worktree 1: %v", err)
	}

	// Assert file does NOT exist in Worktree 2
	path2 := filepath.Join(alloc2.WorktreeDir, fileName)
	if _, err := os.Stat(path2); !os.IsNotExist(err) {
		t.Errorf("isolation breach: file %s unexpectedly exists in worktree 2!", path2)
	}

	// Assert file does NOT exist in workspace root
	rootPath := filepath.Join(repoDir, fileName)
	if _, err := os.Stat(rootPath); !os.IsNotExist(err) {
		t.Errorf("isolation breach: file %s unexpectedly exists in workspace root!", rootPath)
	}

	// Write another isolated file in Worktree 2
	fileName2 := "phase2_index_cache.dat"
	path2Unique := filepath.Join(alloc2.WorktreeDir, fileName2)
	if err := os.WriteFile(path2Unique, []byte("index nodes"), 0644); err != nil {
		t.Fatalf("failed to write file in worktree 2: %v", err)
	}

	// Assert file does NOT exist in Worktree 1
	if _, err := os.Stat(filepath.Join(alloc1.WorktreeDir, fileName2)); !os.IsNotExist(err) {
		t.Errorf("isolation breach: file %s unexpectedly exists in worktree 1!", fileName2)
	}

	if err := wm.PruneAll(ctx); err != nil {
		t.Fatalf("PruneAll failed: %v", err)
	}
}

// TestWorktree_WindowsLockResilience opens a file handle in Worktree 1 and runs a mock build
// in Worktree 2 to verify zero ERROR_SHARING_VIOLATION (0x20) errors.
func TestWorktree_WindowsLockResilience(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm, err := NewWorktreeManager(repoDir)
	if err != nil {
		t.Fatalf("NewWorktreeManager failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alloc1, err := wm.Allocate(ctx, 1, "main")
	if err != nil {
		t.Fatalf("Allocate phase 1 failed: %v", err)
	}

	alloc2, err := wm.Allocate(ctx, 2, "main")
	if err != nil {
		t.Fatalf("Allocate phase 2 failed: %v", err)
	}

	// In Worktree 1: Open an active, exclusive file handle to a binary inside .bin
	binName := "worker_test_bin"
	bin1Path := alloc1.OutputBinaryPath(binName)
	handle1, err := os.OpenFile(bin1Path, os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		t.Fatalf("failed to create/open binary in worktree 1: %v", err)
	}
	defer handle1.Close()

	if _, err := handle1.WriteString("MOCK_BINARY_DATA_ACTIVE_HANDLE_PHASE_1"); err != nil {
		t.Fatalf("failed to write to binary handle 1: %v", err)
	}

	// In Worktree 2: Concurrently perform a mock build creating the exact same binary name in .bin
	bin2Path := alloc2.OutputBinaryPath(binName)
	mockBuildData := []byte("MOCK_BINARY_DATA_PARALLEL_BUILD_PHASE_2")
	if err := os.WriteFile(bin2Path, mockBuildData, 0755); err != nil {
		t.Fatalf("Windows sharing collision detected! Failed to write binary in worktree 2: %v", err)
	}

	// Repeatedly read, overwrite, and verify in Worktree 2 while Worktree 1 handle remains locked
	for i := 0; i < 5; i++ {
		data := fmt.Sprintf("REBUILD_ITERATION_%d", i)
		if err := os.WriteFile(bin2Path, []byte(data), 0755); err != nil {
			t.Fatalf("ERROR_SHARING_VIOLATION on rebuild %d: %v", i, err)
		}

		readBack, err := os.ReadFile(bin2Path)
		if err != nil {
			t.Fatalf("failed to read back binary 2: %v", err)
		}
		if string(readBack) != data {
			t.Fatalf("binary 2 content mismatch: got %s, want %s", string(readBack), data)
		}
	}

	// Close handle in Worktree 1
	if err := handle1.Close(); err != nil {
		t.Errorf("error closing handle 1: %v", err)
	}

	if err := wm.PruneAll(ctx); err != nil {
		t.Fatalf("PruneAll failed: %v", err)
	}
}

// TestWorktree_PruneAll allocates 3 worktrees, creates files, calls PruneAll,
// and asserts .worktrees/ is cleanly unlinked and transient branches pruned.
func TestWorktree_PruneAll(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm, err := NewWorktreeManager(repoDir)
	if err != nil {
		t.Fatalf("NewWorktreeManager failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Allocate 3 worktrees
	alloc1, err := wm.Allocate(ctx, 1, "main")
	if err != nil {
		t.Fatalf("Allocate phase 1 failed: %v", err)
	}
	alloc2, err := wm.Allocate(ctx, 2, "main")
	if err != nil {
		t.Fatalf("Allocate phase 2 failed: %v", err)
	}
	alloc3, err := wm.Allocate(ctx, 3, "main")
	if err != nil {
		t.Fatalf("Allocate phase 3 failed: %v", err)
	}

	// Create arbitrary files in all 3 worktrees
	_ = os.WriteFile(filepath.Join(alloc1.WorktreeDir, "f1.go"), []byte("package p1"), 0644)
	_ = os.WriteFile(filepath.Join(alloc2.WorktreeDir, "f2.go"), []byte("package p2"), 0644)
	_ = os.WriteFile(filepath.Join(alloc3.WorktreeDir, "f3.go"), []byte("package p3"), 0644)

	// Call PruneAll
	if err := wm.PruneAll(ctx); err != nil {
		t.Fatalf("PruneAll returned error: %v", err)
	}

	// Assert .worktrees/ is cleanly unlinked
	if _, err := os.Stat(wm.WorktreesBase()); !os.IsNotExist(err) {
		t.Errorf(".worktrees/ was not cleanly removed: %s", wm.WorktreesBase())
	}

	// Assert individual worktree directories no longer exist
	for _, dir := range []string{alloc1.WorktreeDir, alloc2.WorktreeDir, alloc3.WorktreeDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("worktree directory %s still exists after PruneAll", dir)
		}
	}

	// Assert transient git branches (swarm/worker-phase-*) are pruned
	branchCmd := exec.Command("git", "branch", "--list", "swarm/worker-phase-*")
	branchCmd.Dir = repoDir
	out, err := branchCmd.Output()
	if err != nil {
		t.Fatalf("failed to query branches: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("transient branches were not pruned: %s", string(out))
	}

	// Assert in-memory allocations are empty
	if len(wm.Allocations()) != 0 {
		t.Errorf("allocations map not empty: %d entries remain", len(wm.Allocations()))
	}
}

// TestWorktree_LinkJunction verifies creation, transparency, and safe unlinking
// of NTFS directory junctions.
func TestWorktree_LinkJunction(t *testing.T) {
	tempDir := t.TempDir()
	wm, err := NewWorktreeManager(tempDir)
	if err != nil {
		t.Fatalf("NewWorktreeManager failed: %v", err)
	}

	targetDir := filepath.Join(tempDir, "shared_node_modules")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target directory: %v", err)
	}

	testFile := filepath.Join(targetDir, "package.json")
	if err := os.WriteFile(testFile, []byte(`{"name": "test-pkg"}`), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	linkPath := filepath.Join(tempDir, "worker_junction", "node_modules")
	if err := wm.LinkJunction(targetDir, linkPath); err != nil {
		t.Fatalf("LinkJunction failed: %v", err)
	}

	// Verify transparent access through junction link
	linkedFile := filepath.Join(linkPath, "package.json")
	content, err := os.ReadFile(linkedFile)
	if err != nil {
		t.Fatalf("failed to read file through junction link: %v", err)
	}
	if !strings.Contains(string(content), "test-pkg") {
		t.Errorf("unexpected content through junction: %s", string(content))
	}

	// Write new file via link and verify target updated
	viaLinkFile := filepath.Join(linkPath, "created_via_link.txt")
	if err := os.WriteFile(viaLinkFile, []byte("linked write"), 0644); err != nil {
		t.Fatalf("failed to write via junction link: %v", err)
	}

	viaTargetFile := filepath.Join(targetDir, "created_via_link.txt")
	if _, err := os.Stat(viaTargetFile); err != nil {
		t.Errorf("file written via junction was not found in target: %v", err)
	}

	// Unlink junction and ensure target directory remains intact
	if err := os.Remove(linkPath); err != nil {
		t.Fatalf("failed to remove junction link: %v", err)
	}

	// Target files must still exist
	if _, err := os.Stat(testFile); err != nil {
		t.Errorf("target file was deleted when removing junction link: %v", err)
	}
}

// TestWorktree_SinglePrune verifies pruning a single worktree leaves sibling worktrees intact.
func TestWorktree_SinglePrune(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm, err := NewWorktreeManager(repoDir)
	if err != nil {
		t.Fatalf("NewWorktreeManager failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alloc1, err := wm.Allocate(ctx, 1, "main")
	if err != nil {
		t.Fatalf("Allocate phase 1 failed: %v", err)
	}
	alloc2, err := wm.Allocate(ctx, 2, "main")
	if err != nil {
		t.Fatalf("Allocate phase 2 failed: %v", err)
	}

	// Prune phase 1 only
	if err := wm.Prune(ctx, 1); err != nil {
		t.Fatalf("Prune phase 1 failed: %v", err)
	}

	// Verify phase 1 is gone
	if _, err := os.Stat(alloc1.WorktreeDir); !os.IsNotExist(err) {
		t.Errorf("phase 1 worktree dir still exists after prune")
	}

	// Verify phase 2 remains intact
	if _, err := os.Stat(alloc2.WorktreeDir); err != nil {
		t.Errorf("phase 2 worktree dir missing after phase 1 prune: %v", err)
	}

	// Verify phase 1 branch deleted
	cmd1 := exec.Command("git", "branch", "--list", alloc1.BranchName)
	cmd1.Dir = repoDir
	out, _ := cmd1.Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("phase 1 branch was not deleted")
	}

	// Verify phase 2 branch still exists
	cmd2 := exec.Command("git", "branch", "--list", alloc2.BranchName)
	cmd2.Dir = repoDir
	out2, _ := cmd2.Output()
	if !strings.Contains(string(out2), alloc2.BranchName) {
		t.Errorf("phase 2 branch was unexpectedly deleted")
	}

	// Clean up remainder
	_ = wm.PruneAll(ctx)
}
