// SPDX-License-Identifier: Apache-2.0
package vm_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

func TestParseCommandChain(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []vm.CommandSegment
	}{
		{
			name:  "Single command",
			input: "go test ./...",
			expected: []vm.CommandSegment{
				{Raw: "go test ./...", Operator: vm.OpNone},
			},
		},
		{
			name:  "Two commands with AND",
			input: "go build ./... && go test ./...",
			expected: []vm.CommandSegment{
				{Raw: "go build ./...", Operator: vm.OpNone},
				{Raw: "go test ./...", Operator: vm.OpAnd},
			},
		},
		{
			name:  "Two commands with OR",
			input: "test -f file || touch file",
			expected: []vm.CommandSegment{
				{Raw: "test -f file", Operator: vm.OpNone},
				{Raw: "touch file", Operator: vm.OpOr},
			},
		},
		{
			name:  "Two commands with Semicolon",
			input: "mkdir sub ; cd sub",
			expected: []vm.CommandSegment{
				{Raw: "mkdir sub", Operator: vm.OpNone},
				{Raw: "cd sub", Operator: vm.OpSeq},
			},
		},
		{
			name:  "Quotes containing operators should not split",
			input: `echo "hello && world" && echo done`,
			expected: []vm.CommandSegment{
				{Raw: `echo "hello && world"`, Operator: vm.OpNone},
				{Raw: "echo done", Operator: vm.OpAnd},
			},
		},
		{
			name:  "Single quotes containing operators should not split",
			input: `git commit -m 'feat: add feature; fix test' && git push`,
			expected: []vm.CommandSegment{
				{Raw: `git commit -m 'feat: add feature; fix test'`, Operator: vm.OpNone},
				{Raw: "git push", Operator: vm.OpAnd},
			},
		},
		{
			name:  "Mixed compound operators",
			input: "cmd1 && cmd2 || cmd3 ; cmd4",
			expected: []vm.CommandSegment{
				{Raw: "cmd1", Operator: vm.OpNone},
				{Raw: "cmd2", Operator: vm.OpAnd},
				{Raw: "cmd3", Operator: vm.OpOr},
				{Raw: "cmd4", Operator: vm.OpSeq},
			},
		},
		{
			name:  "Trailing semicolon and spaces",
			input: "echo test ;  ",
			expected: []vm.CommandSegment{
				{Raw: "echo test", Operator: vm.OpNone},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := vm.ParseCommandChain(tt.input)
			if len(actual) != len(tt.expected) {
				t.Fatalf("expected %d segments, got %d: %+v", len(tt.expected), len(actual), actual)
			}
			for i := range actual {
				if actual[i].Raw != tt.expected[i].Raw || actual[i].Operator != tt.expected[i].Operator {
					t.Errorf("segment[%d] mismatch: expected %+v, got %+v", i, tt.expected[i], actual[i])
				}
			}
		})
	}
}

func TestExecuteChainedCommand_SequentialAnd(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	cmd := "echo first_step && echo second_step"
	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "first_step") || !strings.Contains(res.Stdout, "second_step") {
		t.Errorf("expected both steps in stdout, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_AndShortCircuitOnFailure(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	var failCmd string
	if runtime.GOOS == "windows" {
		failCmd = "cmd.exe /c exit 7 && echo should_never_run"
	} else {
		failCmd = "sh -c 'exit 7' && echo should_never_run"
	}

	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, failCmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", res.ExitCode)
	}
	if strings.Contains(res.Stdout, "should_never_run") {
		t.Errorf("expected second command to be skipped, but stdout contained it: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_OrFallback(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	var fallbackCmd string
	if runtime.GOOS == "windows" {
		fallbackCmd = "cmd.exe /c exit 1 || echo fallback_succeeded"
	} else {
		fallbackCmd = "sh -c 'exit 1' || echo fallback_succeeded"
	}

	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, fallbackCmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0 from fallback, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "fallback_succeeded") {
		t.Errorf("expected fallback stdout, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_OrSkippedOnSuccess(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	cmd := "echo primary_ok || echo should_never_run"
	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
	if strings.Contains(res.Stdout, "should_never_run") {
		t.Errorf("expected fallback to be skipped, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_SemicolonAlwaysExecutes(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	cmd := "echo step_one ; echo step_two"
	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "step_one") || !strings.Contains(res.Stdout, "step_two") {
		t.Errorf("expected both steps in stdout, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_CdDirectoryNavigation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "subproject")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subproject: %v", err)
	}
	targetFile := filepath.Join(subDir, "marker.txt")
	if err := os.WriteFile(targetFile, []byte("in_subproject"), 0644); err != nil {
		t.Fatalf("failed to write marker.txt: %v", err)
	}

	var readCmd string
	if runtime.GOOS == "windows" {
		readCmd = "cd subproject && type marker.txt"
	} else {
		readCmd = "cd subproject && cat marker.txt"
	}

	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
		SandboxRoot:    tmpDir,
	}, readCmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "in_subproject") {
		t.Errorf("expected marker content in stdout, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_CdNonExistentDirectoryFails(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	cmd := "cd nonexistent_directory_123 && echo should_not_run"
	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
		SandboxRoot:    tmpDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected non-zero exit code when cd fails")
	}
	if strings.Contains(res.Stdout, "should_not_run") {
		t.Errorf("expected second command to be skipped after failed cd, got: %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "nonexistent_directory_123") {
		t.Errorf("expected stderr to mention missing directory, got: %q", res.Stderr)
	}
}

func TestExecuteChainedCommand_CdSandboxContainment(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "sandbox_root")
	_ = os.MkdirAll(subDir, 0755)

	cmd := "cd ../../../ && echo escaped"
	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: subDir,
		SandboxRoot:    subDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected failure when cd attempts to escape sandbox boundary")
	}
	if !strings.Contains(res.Stderr, "escapes sandbox boundary") {
		t.Errorf("expected sandbox containment error message in stderr, got: %q", res.Stderr)
	}
}

func TestExecuteChainedCommand_HostWorkspaceRemap(t *testing.T) {
	ctx := context.Background()
	sandboxDir := t.TempDir()
	hostWsDir := filepath.Join(t.TempDir(), "MyHostWorkspace")
	_ = os.MkdirAll(hostWsDir, 0755)

	// Create subfolder inside sandboxDir matching relative structure
	subInsideSandbox := filepath.Join(sandboxDir, "subdir")
	_ = os.MkdirAll(subInsideSandbox, 0755)

	// Test 1: cd to hostWsDir directly -> should remap to sandboxDir
	cmd1 := fmt.Sprintf("cd %q && echo root_ok", hostWsDir)
	res1, err1 := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: sandboxDir,
		SandboxRoot:    sandboxDir,
		HostWorkspace:  hostWsDir,
	}, cmd1)
	if err1 != nil {
		t.Fatalf("unexpected error: %v", err1)
	}
	if res1.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res1.ExitCode, res1.Stderr)
	}
	if !strings.Contains(res1.Stdout, "root_ok") {
		t.Errorf("expected root_ok in stdout, got: %s", res1.Stdout)
	}

	// Test 2: cd to hostWsDir/subdir -> should remap to sandboxDir/subdir
	targetSub := filepath.Join(hostWsDir, "subdir")
	cmd2 := fmt.Sprintf("cd %q && echo sub_ok", targetSub)
	res2, err2 := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: sandboxDir,
		SandboxRoot:    sandboxDir,
		HostWorkspace:  hostWsDir,
	}, cmd2)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if res2.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res2.ExitCode, res2.Stderr)
	}
	if !strings.Contains(res2.Stdout, "sub_ok") {
		t.Errorf("expected sub_ok in stdout, got: %s", res2.Stdout)
	}
}

func TestExecuteChainedCommand_ExportAndSetEnvironment(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	var envCmd string
	if runtime.GOOS == "windows" {
		envCmd = "export MY_CUSTOM_VAR=OmniVal && set OTHER_VAR=SecondVal && echo %MY_CUSTOM_VAR% %OTHER_VAR%"
	} else {
		envCmd = "export MY_CUSTOM_VAR=OmniVal && export OTHER_VAR=SecondVal && echo $MY_CUSTOM_VAR $OTHER_VAR"
	}

	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, envCmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "OmniVal") || !strings.Contains(res.Stdout, "SecondVal") {
		t.Errorf("expected environment variables in stdout, got: %q", res.Stdout)
	}
}

func TestHermeticLocalSandbox_ChainedRunCommand(t *testing.T) {
	ctx := context.Background()
	tempBase := t.TempDir()
	client := vm.NewHermeticLocalSandbox(tempBase)

	sb, err := client.CreateSandbox(ctx, vm.SandboxConfig{
		MemoryMB:       128,
		VCPUs:          1,
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}
	defer client.DestroySandbox(ctx, sb.ID)

	// Write a file in a subfolder inside the sandbox
	if err := client.WriteFile(ctx, sb.ID, "pkg/core/hello.txt", "ChainedExecutionInMicroVM"); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var chainedCmd string
	if runtime.GOOS == "windows" {
		chainedCmd = "cd pkg/core && type hello.txt"
	} else {
		chainedCmd = "cd pkg/core && cat hello.txt"
	}

	res, err := client.RunCommand(ctx, vm.CommandRequest{
		SandboxID: sb.ID,
		Command:   chainedCmd,
	})
	if err != nil {
		t.Fatalf("RunCommand failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "ChainedExecutionInMicroVM") {
		t.Errorf("expected file contents in chained RunCommand stdout, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_PowerShellCmdletPropagation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows PowerShell test on non-windows")
	}
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Write-Output and (Get-Content ...) are classic PowerShell expressions
	cmd := "Write-Output 'step_one' ; (Get-Item .).Name"
	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "step_one") {
		t.Errorf("expected 'step_one' in stdout, got: %q", res.Stdout)
	}
}

func TestHermeticLocalSandbox_DependencyJunctionLinking(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()

	// Create fake dependency directory (node_modules)
	nodeModulesDir := filepath.Join(sourceDir, "node_modules", "test-pkg")
	if err := os.MkdirAll(nodeModulesDir, 0755); err != nil {
		t.Fatalf("failed to create fake node_modules: %v", err)
	}
	indexFile := filepath.Join(nodeModulesDir, "index.js")
	if err := os.WriteFile(indexFile, []byte("module.exports = 'CALERA_TEST_PKG';"), 0644); err != nil {
		t.Fatalf("failed to write index.js: %v", err)
	}

	// Create fake source code
	srcFile := filepath.Join(sourceDir, "app.js")
	if err := os.WriteFile(srcFile, []byte("console.log('app');"), 0644); err != nil {
		t.Fatalf("failed to write app.js: %v", err)
	}

	client := vm.NewHermeticLocalSandbox(sourceDir)
	sb, err := client.CreateSandbox(ctx, vm.SandboxConfig{
		WorkspaceDir:   sourceDir,
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}
	defer func() {
		_ = client.DestroySandbox(ctx, sb.ID)
	}()

	// Verify node_modules is accessible inside sandbox
	pkgContent, err := client.ReadFile(ctx, sb.ID, "node_modules/test-pkg/index.js")
	if err != nil {
		t.Fatalf("ReadFile from linked node_modules failed: %v", err)
	}
	if !strings.Contains(pkgContent, "CALERA_TEST_PKG") {
		t.Errorf("expected linked package content, got %q", pkgContent)
	}

	// Verify Snapshot takes < 50ms and skips walking inside node_modules
	startSnap := time.Now()
	chkID, err := client.Snapshot(ctx, sb.ID, "pre-test")
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	if time.Since(startSnap) > 50*time.Millisecond {
		t.Errorf("Snapshot took too long (%v), likely recursed into dependencies", time.Since(startSnap))
	}

	// Mutate app.js and rollback
	_ = client.WriteFile(ctx, sb.ID, "app.js", "corrupted")
	if err := client.Rollback(ctx, sb.ID, chkID); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// Verify app.js is restored and node_modules remains intact
	restoredApp, _ := client.ReadFile(ctx, sb.ID, "app.js")
	if restoredApp != "console.log('app');" {
		t.Errorf("expected app.js to be restored, got %q", restoredApp)
	}
	pkgContentAfter, err := client.ReadFile(ctx, sb.ID, "node_modules/test-pkg/index.js")
	if err != nil || !strings.Contains(pkgContentAfter, "CALERA_TEST_PKG") {
		t.Errorf("node_modules damaged after rollback: %v", err)
	}
}

func TestExecuteChainedCommand_NonInteractiveCIEnv(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "cmd.exe /c echo CI=%CI% CI_ENV=%CONTINUOUS_INTEGRATION%"
	} else {
		cmd = "echo CI=$CI CI_ENV=$CONTINUOUS_INTEGRATION"
	}

	res, err := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "CI=true") {
		t.Errorf("expected CI=true in stdout, got: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "CI_ENV=true") {
		t.Errorf("expected CI_ENV=true in stdout, got: %q", res.Stdout)
	}
}

func TestExecuteChainedCommand_TimeoutProcessTreeCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	tmpDir := t.TempDir()
	var longCmd string
	if runtime.GOOS == "windows" {
		longCmd = "cmd.exe /c ping -n 15 127.0.0.1"
	} else {
		longCmd = "sleep 10"
	}

	start := time.Now()
	res, _ := vm.ExecuteChainedCommand(ctx, vm.ChainedExecConfig{
		InitialWorkdir: tmpDir,
	}, longCmd)
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Errorf("timeout took too long (%v), process tree was not cleaned up promptly", elapsed)
	}
	if res != nil && res.ExitCode == 0 {
		t.Errorf("expected non-zero exit code on timeout cancellation, got: %d", res.ExitCode)
	}
}

