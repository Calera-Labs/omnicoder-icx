// SPDX-License-Identifier: Apache-2.0
package mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

func TestOmniCoderTools_RegistrationAndStatus(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)

	mcp.RegisterOmniCoderTools(srv, nil)

	tools := srv.ListTools()
	toolMap := make(map[string]bool)
	for _, tl := range tools {
		toolMap[tl.Name] = true
	}

	expectedTools := []string{
		"omnicoder_status",
		"omnicoder_map",
		"omnicoder_search",
		"omnicoder_patch",
		"omnicoder_heal",
		"omnicoder_refactor",
		"omnicoder_sandbox",
		"omnicoder_run",
		"omnicoder_set_workspace",
		"omnicoder_doctor",
		"omnicoder_toggle_checklist",
		"omnicoder_update_section",
		"read_file",
		"write_file",
		"list_directory",
		"git_status",
		"git_diff",
		"git_commit",
	}

	for _, name := range expectedTools {
		if !toolMap[name] {
			t.Errorf("expected tool %q to be registered on server", name)
		}
	}

	// Test omnicoder_status
	ctx := context.Background()
	res, err := srv.Dispatch(ctx, "omnicoder_status", map[string]any{})
	if err != nil {
		t.Fatalf("omnicoder_status failed: %v", err)
	}

	statusMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any result, got %T", res)
	}

	if statusMap["status"] != "READY" {
		t.Errorf("expected status READY, got %v", statusMap["status"])
	}
	if statusMap["workspace_root"] != tmpDir {
		t.Errorf("expected workspace_root %s, got %v", tmpDir, statusMap["workspace_root"])
	}
	if statusMap["tools_count"].(int) < 14 {
		t.Errorf("expected at least 14 tools registered, got %v", statusMap["tools_count"])
	}
	if statusMap["host_os"] != runtime.GOOS {
		t.Errorf("expected host_os %s, got %v", runtime.GOOS, statusMap["host_os"])
	}
	if statusMap["default_shell"] == "" {
		t.Errorf("expected non-empty default_shell")
	}
	if statusMap["line_ending"] == "" {
		t.Errorf("expected non-empty line_ending")
	}
	if statusMap["typescript_ast_supported"] != true {
		t.Errorf("expected typescript_ast_supported to be true, got %v", statusMap["typescript_ast_supported"])
	}
	if statusMap["windows_junction_sandbox"] != true {
		t.Errorf("expected windows_junction_sandbox to be true, got %v", statusMap["windows_junction_sandbox"])
	}
	if statusMap["polyglot_ast_supported"] != true {
		t.Errorf("expected polyglot_ast_supported to be true, got %v", statusMap["polyglot_ast_supported"])
	}
}

func TestOmniCoderTools_MapAndSearch(t *testing.T) {
	tmpDir := t.TempDir()

	// Create sample code files
	mainGo := filepath.Join(tmpDir, "main.go")
	content := `package main

import "fmt"

func ProcessOrder(orderID string) error {
	fmt.Println("Processing order", orderID)
	return nil
}

func main() {
	_ = ProcessOrder("12345")
}
`
	if err := os.WriteFile(mainGo, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Test omnicoder_map
	mapRes, err := srv.Dispatch(ctx, "omnicoder_map", map[string]any{
		"token_budget": float64(512),
	})
	if err != nil {
		t.Fatalf("omnicoder_map failed: %v", err)
	}
	mMap := mapRes.(map[string]any)
	if mMap["files_scanned"].(int) < 1 {
		t.Errorf("expected at least 1 file scanned, got %v", mMap["files_scanned"])
	}

	// Test omnicoder_search
	searchRes, err := srv.Dispatch(ctx, "omnicoder_search", map[string]any{
		"query": "ProcessOrder",
	})
	if err != nil {
		t.Fatalf("omnicoder_search failed: %v", err)
	}
	sMap := searchRes.(map[string]any)
	if sMap["count"].(int) < 1 {
		t.Errorf("expected at least 1 hit for ProcessOrder, got %d", sMap["count"])
	}
}

func TestOmniCoderTools_PatchPreservation(t *testing.T) {
	tmpDir := t.TempDir()

	calcGo := filepath.Join(tmpDir, "calc.go")
	initialContent := `package calc

import "math"

type Calculator struct {
	base float64
}

func (c *Calculator) Add(a, b float64) float64 {
	return a + b
}

func (c *Calculator) Sqrt(x float64) float64 {
	return math.Sqrt(x)
}
`
	if err := os.WriteFile(calcGo, []byte(initialContent), 0644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// 1. Surgical Aider search/replace block
	patchContent := `<<<<<<< SEARCH
func (c *Calculator) Add(a, b float64) float64 {
	return a + b
}
=======
func (c *Calculator) Add(a, b float64) float64 {
	// Added zero-check
	return a + b + c.base
}
>>>>>>> REPLACE`

	res, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "calc.go",
		"content": patchContent,
	})
	if err != nil {
		t.Fatalf("omnicoder_patch failed: %v", err)
	}
	resMap := res.(map[string]any)
	if resMap["status"] != "PATCHED" {
		t.Errorf("expected status PATCHED, got %v", resMap["status"])
	}

	// Verify content was updated surgically
	updatedBytes, err := os.ReadFile(calcGo)
	if err != nil {
		t.Fatal(err)
	}
	updatedStr := string(updatedBytes)
	if !strings.Contains(updatedStr, "Added zero-check") {
		t.Errorf("expected patched file to contain new comment")
	}
	if !strings.Contains(updatedStr, "func (c *Calculator) Sqrt") {
		t.Errorf("expected existing Sqrt method to be preserved")
	}

	// 2. Reject tiny stub replacement (<80 bytes)
	stubContent := `package calc
// Stub
`
	_, stubErr := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "calc.go",
		"content": stubContent,
	})
	if stubErr == nil {
		t.Fatalf("expected stub replacement to be rejected by Code Preservation Guard")
	}
	if !strings.Contains(stubErr.Error(), "code preservation guard") {
		t.Errorf("expected error message to mention code preservation guard, got: %v", stubErr)
	}
}

func TestOmniCoderTools_Patch_ShrinkageGuardC(t *testing.T) {
	tmpDir := t.TempDir()

	serviceGo := filepath.Join(tmpDir, "service.go")
	// Substantive > 300 bytes
	content := `package svc

import "fmt"

type Service struct {
	Name string
}

func (s *Service) Start() error {
	fmt.Println("Starting service")
	return nil
}

func (s *Service) Stop() error {
	fmt.Println("Stopping service")
	return nil
}

func (s *Service) Restart() error {
	_ = s.Stop()
	return s.Start()
}
`
	if err := os.WriteFile(serviceGo, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Severe shrinkage rewrite without the existing functions
	shrunkContent := `package svc

type Service struct {
	Name string
}
`
	_, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "service.go",
		"content": shrunkContent,
	})
	// Should either splice declarations or reject shrinkage
	if err == nil {
		// If spliced, ensure existing methods were not deleted
		raw, _ := os.ReadFile(serviceGo)
		if !strings.Contains(string(raw), "func (s *Service) Restart") {
			t.Errorf("expected Restart to be preserved if rewrite was accepted")
		}
	}
}

func TestOmniCoderTools_Patch_SingleFunctionUpdateGuardB(t *testing.T) {
	tmpDir := t.TempDir()

	mathGo := filepath.Join(tmpDir, "math.go")
	initialCode := `package mathutils

func Multiply(a, b int) int {
	return a * b
}

func Divide(a, b int) int {
	if b == 0 {
		return 0
	}
	return a / b
}
`
	if err := os.WriteFile(mathGo, []byte(initialCode), 0644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Single function update for Multiply without repeating Divide
	singleUpdate := `package mathutils

func Multiply(a, b int) int {
	// Added fast-path
	if a == 0 || b == 0 {
		return 0
	}
	return a * b
}
`
	res, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "math.go",
		"content": singleUpdate,
	})
	if err != nil {
		t.Fatalf("omnicoder_patch single function update failed: %v", err)
	}

	resMap := res.(map[string]any)
	if resMap["status"] != "WRITTEN" {
		t.Errorf("expected status WRITTEN, got %v", resMap["status"])
	}

	updated, err := os.ReadFile(mathGo)
	if err != nil {
		t.Fatal(err)
	}
	updatedStr := string(updated)
	if !strings.Contains(updatedStr, "Added fast-path") {
		t.Errorf("expected new Multiply logic to be present")
	}
	if !strings.Contains(updatedStr, "func Divide") {
		t.Errorf("expected Guard B to preserve Divide function")
	}
}

func TestOmniCoderTools_Heal_Passing(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	cmd := "go version"
	res, err := srv.Dispatch(ctx, "omnicoder_heal", map[string]any{
		"command": cmd,
	})
	if err != nil {
		t.Fatalf("omnicoder_heal failed: %v", err)
	}

	hMap := res.(map[string]any)
	if hMap["status"] != "PASSED" {
		t.Errorf("expected status PASSED, got %v", hMap["status"])
	}
	if hMap["exit_code"].(int) != 0 {
		t.Errorf("expected exit_code 0, got %v", hMap["exit_code"])
	}
}

func TestOmniCoderTools_Heal_FailingDiagnostics(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "cmd.exe /c exit 1"
	} else {
		cmd = "false"
	}

	res, err := srv.Dispatch(ctx, "omnicoder_heal", map[string]any{
		"command": cmd,
	})
	if err != nil {
		t.Fatalf("omnicoder_heal failed: %v", err)
	}

	hMap := res.(map[string]any)
	if hMap["status"] != "DIAGNOSED" {
		t.Errorf("expected status DIAGNOSED, got %v", hMap["status"])
	}
	if hMap["exit_code"].(int) == 0 {
		t.Errorf("expected non-zero exit_code, got %v", hMap["exit_code"])
	}
}

func TestOmniCoderTools_Heal_IterativeLoop(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)

	called := false
	cfg := mcp.AutoDetectEnvironment(tmpDir)
	cfg.IsOffline = false
	sCtx := &mcp.OmniCoderServerContext{
		Config:    cfg,
		SandboxOn: true,
		HealRunner: func(ctx context.Context, command string, maxTurns int, issue string) (map[string]any, error) {
			called = true
			return map[string]any{
				"status":    "HEALED",
				"command":   command,
				"max_turns": maxTurns,
			}, nil
		},
	}
	mcp.RegisterOmniCoderTools(srv, sCtx)
	ctx := context.Background()

	var failingCmd string
	if runtime.GOOS == "windows" {
		failingCmd = "cmd.exe /c exit 2"
	} else {
		failingCmd = "sh -c 'exit 2'"
	}

	res, err := srv.Dispatch(ctx, "omnicoder_heal", map[string]any{
		"command":   failingCmd,
		"max_turns": float64(3),
	})
	if err != nil {
		t.Fatalf("omnicoder_heal with HealRunner failed: %v", err)
	}

	if !called {
		t.Errorf("expected HealRunner to be invoked on command failure")
	}

	hMap := res.(map[string]any)
	if hMap["status"] != "HEALED" {
		t.Errorf("expected status HEALED, got %v", hMap["status"])
	}
}

func TestOmniCoderTools_RefactorDryRun(t *testing.T) {
	tmpDir := t.TempDir()

	file1 := filepath.Join(tmpDir, "models.go")
	_ = os.WriteFile(file1, []byte(`package app

type User struct {
	ID string
}
`), 0644)

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	res, err := srv.Dispatch(ctx, "omnicoder_refactor", map[string]any{
		"symbol":  "User",
		"dry_run": true,
	})
	if err != nil {
		t.Fatalf("omnicoder_refactor failed: %v", err)
	}

	refMap := res.(map[string]any)
	if refMap["symbol"] != "User" {
		t.Errorf("expected symbol User, got %v", refMap["symbol"])
	}
	if refMap["dry_run"] != true {
		t.Errorf("expected dry_run true, got %v", refMap["dry_run"])
	}
}

func TestOmniCoderTools_RefactorTypeScriptTSX(t *testing.T) {
	tmpDir := t.TempDir()

	compFile := filepath.Join(tmpDir, "Button.tsx")
	_ = os.WriteFile(compFile, []byte(`
export interface ButtonProps {
    label: string;
}

export const Button: React.FC<ButtonProps> = ({ label }) => {
    return <button>{label}</button>;
};
`), 0644)

	appFile := filepath.Join(tmpDir, "App.tsx")
	_ = os.WriteFile(appFile, []byte(`
import { Button } from './Button';

export function App() {
    return <Button label="Submit" />;
}
`), 0644)

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	res, err := srv.Dispatch(ctx, "omnicoder_refactor", map[string]any{
		"symbol":  "Button",
		"dry_run": true,
	})
	if err != nil {
		t.Fatalf("omnicoder_refactor on Button failed: %v", err)
	}

	refMap := res.(map[string]any)
	if refMap["symbol"] != "Button" {
		t.Errorf("expected symbol Button, got %v", refMap["symbol"])
	}
	if refMap["dry_run"] != true {
		t.Errorf("expected dry_run true, got %v", refMap["dry_run"])
	}
	blastSteps, ok := refMap["blast_radius"].([]any)
	if !ok && refMap["blast_radius"] == nil {
		t.Errorf("expected non-nil blast_radius")
	} else if ok && len(blastSteps) == 0 {
		t.Errorf("expected at least 1 step in blast radius for Button")
	}
}

func TestOmniCoderTools_Sandbox_ExecutionAndRollback(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// 1. Successful sandbox command
	var echoCmd string
	if runtime.GOOS == "windows" {
		echoCmd = "cmd.exe /c echo MicroVM_OK"
	} else {
		echoCmd = "echo MicroVM_OK"
	}

	res, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"command": echoCmd,
	})
	if err != nil {
		t.Fatalf("omnicoder_sandbox failed: %v", err)
	}
	sbMap := res.(map[string]any)
	if sbMap["exit_code"].(int) != 0 {
		t.Errorf("expected exit_code 0, got %v", sbMap["exit_code"])
	}
	stdout := sbMap["stdout"].(string)
	if !strings.Contains(stdout, "MicroVM_OK") {
		t.Errorf("expected stdout to contain MicroVM_OK, got %q", stdout)
	}
	if sbMap["rolled_back"].(bool) != false {
		t.Errorf("expected rolled_back to be false on success")
	}
	sbStatus, ok := sbMap["sandbox_status"].(map[string]any)
	if !ok {
		t.Fatalf("expected sandbox_status map, got %T", sbMap["sandbox_status"])
	}
	if sbStatus["is_ephemeral"] != true || sbStatus["persisted_to_workspace"] != false {
		t.Errorf("expected ephemeral=true and persisted_to_workspace=false, got %v", sbStatus)
	}

	// 2. Failing sandbox command triggers automatic rollback
	var failCmd string
	if runtime.GOOS == "windows" {
		failCmd = "cmd.exe /c exit 42"
	} else {
		failCmd = "sh -c 'exit 42'"
	}

	resFail, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"command": failCmd,
	})
	if err != nil {
		t.Fatalf("omnicoder_sandbox failing command returned err: %v", err)
	}
	failMap := resFail.(map[string]any)
	if failMap["exit_code"].(int) != 42 {
		t.Errorf("expected exit_code 42, got %v", failMap["exit_code"])
	}
	if failMap["rolled_back"].(bool) != true {
		t.Errorf("expected rolled_back to be true on non-zero exit code")
	}
	failStatus, ok := failMap["sandbox_status"].(map[string]any)
	if !ok {
		t.Fatalf("expected sandbox_status map on failure, got %T", failMap["sandbox_status"])
	}
	if failStatus["rolled_back"] != true {
		t.Errorf("expected status rolled_back=true on failure")
	}
	if reason, ok := failStatus["rollback_reason"].(string); !ok || !strings.Contains(reason, "exit code 42") {
		t.Errorf("expected rollback_reason to mention exit code 42, got %v", failStatus["rollback_reason"])
	}

	// 3. Chained command execution with &&
	resChain, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"command": "echo step_alpha && echo step_beta",
	})
	if err != nil {
		t.Fatalf("omnicoder_sandbox chained command returned err: %v", err)
	}
	chainMap := resChain.(map[string]any)
	if chainMap["exit_code"].(int) != 0 {
		t.Errorf("expected exit_code 0, got %v", chainMap["exit_code"])
	}
	chainStdout := chainMap["stdout"].(string)
	if !strings.Contains(chainStdout, "step_alpha") || !strings.Contains(chainStdout, "step_beta") {
		t.Errorf("expected chained output to contain step_alpha and step_beta, got: %q", chainStdout)
	}

	// 4. Script execution
	scriptPayload := "echo Script_Execution_OK"
	resScript, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"script": scriptPayload,
	})
	if err != nil {
		t.Fatalf("omnicoder_sandbox script execution failed: %v", err)
	}
	scriptMap := resScript.(map[string]any)
	if scriptMap["exit_code"].(int) != 0 {
		t.Errorf("expected exit_code 0 for script, got %v", scriptMap["exit_code"])
	}
	if !strings.Contains(scriptMap["stdout"].(string), "Script_Execution_OK") {
		t.Errorf("expected script output to contain Script_Execution_OK, got %q", scriptMap["stdout"])
	}
}

func TestOmniCoderTools_Run_AutonomousAndACK(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)

	// A. Default without TaskRunner -> ACK
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	res, err := srv.Dispatch(ctx, "omnicoder_run", map[string]any{
		"task": "Build concurrent worker pool",
	})
	if err != nil {
		t.Fatalf("omnicoder_run failed: %v", err)
	}
	rMap := res.(map[string]any)
	if rMap["status"] != "ACK" {
		t.Errorf("expected status ACK, got %v", rMap["status"])
	}

	// B. With TaskRunner configured -> executes autonomous run
	runnerInvoked := false
	srv2 := mcp.NewServer(tmpDir)
	sCtx := &mcp.OmniCoderServerContext{
		Config:    mcp.AutoDetectEnvironment(tmpDir),
		SandboxOn: true,
		TaskRunner: func(ctx context.Context, task string, maxTurns int, unbounded bool, sandbox bool) (map[string]any, error) {
			runnerInvoked = true
			return map[string]any{
				"success":       true,
				"task":          task,
				"turns_count":   1,
				"files_touched": 2,
			}, nil
		},
	}
	mcp.RegisterOmniCoderTools(srv2, sCtx)

	res2, err := srv2.Dispatch(ctx, "omnicoder_run", map[string]any{
		"task":      "Build concurrent worker pool",
		"max_turns": float64(3),
	})
	if err != nil {
		t.Fatalf("omnicoder_run with TaskRunner failed: %v", err)
	}
	if !runnerInvoked {
		t.Errorf("expected TaskRunner to be invoked")
	}
	rMap2 := res2.(map[string]any)
	if rMap2["success"] != true {
		t.Errorf("expected success true, got %v", rMap2["success"])
	}

	// C. With ScopedTaskRunner configured -> receives explicit scope, boundary, and audit_only options
	scopedInvoked := false
	var capturedOpts mcp.ScopedRunOptions
	srv3 := mcp.NewServer(tmpDir)
	sCtx3 := &mcp.OmniCoderServerContext{
		Config:    mcp.AutoDetectEnvironment(tmpDir),
		SandboxOn: true,
		ScopedTaskRunner: func(ctx context.Context, opts mcp.ScopedRunOptions) (map[string]any, error) {
			scopedInvoked = true
			capturedOpts = opts
			return map[string]any{
				"status":           "success",
				"target_symbols":   []string{"Dashboard"},
				"diagnostics":      []any{map[string]any{"rule": "ScopeRule", "file": opts.TargetFile}},
				"patches_proposed": 2,
				"execution_ms":     150,
			}, nil
		},
	}
	mcp.RegisterOmniCoderTools(srv3, sCtx3)

	res3, err := srv3.Dispatch(ctx, "omnicoder_run", map[string]any{
		"task":        "Refactor Dashboard.tsx",
		"target_file": "frontend/src/pages/Dashboard.tsx",
		"scope":       "frontend/src/*",
		"audit_only":  true,
		"json":        true,
	})
	if err != nil {
		t.Fatalf("omnicoder_run with ScopedTaskRunner failed: %v", err)
	}
	if !scopedInvoked {
		t.Fatalf("expected ScopedTaskRunner to be invoked")
	}
	if capturedOpts.TargetFile != "frontend/src/pages/Dashboard.tsx" {
		t.Errorf("expected TargetFile %q, got %q", "frontend/src/pages/Dashboard.tsx", capturedOpts.TargetFile)
	}
	if capturedOpts.Scope != "frontend/src/*" {
		t.Errorf("expected Scope %q, got %q", "frontend/src/*", capturedOpts.Scope)
	}
	if !capturedOpts.AuditOnly {
		t.Errorf("expected AuditOnly to be true")
	}
	if !capturedOpts.JSON {
		t.Errorf("expected JSON to be true")
	}
	rMap3 := res3.(map[string]any)
	if rMap3["status"] != "success" {
		t.Errorf("expected status 'success', got %v", rMap3["status"])
	}
	if rMap3["patches_proposed"] != 2 {
		t.Errorf("expected patches_proposed 2, got %v", rMap3["patches_proposed"])
	}
}

func TestAutoDetectEnvironment(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a dummy go.mod in a subfolder
	projectDir := filepath.Join(tmpDir, "myproject")
	_ = os.MkdirAll(projectDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module test\n"), 0644)

	subDir := filepath.Join(projectDir, "pkg", "sub")
	_ = os.MkdirAll(subDir, 0755)

	// Call AutoDetectEnvironment starting from subDir
	cfg := mcp.AutoDetectEnvironment(subDir)

	// Should resolve up to projectDir where go.mod was found
	if cfg.WorkspaceDir != projectDir {
		t.Errorf("expected resolved workspace %s, got %s", projectDir, cfg.WorkspaceDir)
	}

	// Should have determined an engine (cloud, local, or offline mechanical)
	if cfg.Backend == "" {
		t.Errorf("expected detected backend to be non-empty")
	}

	masked := cfg.MaskedCopy()
	if masked.WorkspaceDir != cfg.WorkspaceDir {
		t.Errorf("masked copy workspace mismatch")
	}
}

func TestOmniCoderTools_SetWorkspace(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	srv := mcp.NewServer(dirA)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Initial status should point to dirA
	resStatus, err := srv.Dispatch(ctx, "omnicoder_status", map[string]any{})
	if err != nil {
		t.Fatalf("omnicoder_status failed: %v", err)
	}
	sMap := resStatus.(map[string]any)
	if sMap["workspace_root"] != dirA {
		t.Errorf("expected workspace_root %s, got %v", dirA, sMap["workspace_root"])
	}

	// Switch workspace to dirB via omnicoder_set_workspace
	resSet, err := srv.Dispatch(ctx, "omnicoder_set_workspace", map[string]any{
		"path": dirB,
	})
	if err != nil {
		t.Fatalf("omnicoder_set_workspace failed: %v", err)
	}
	setMap := resSet.(map[string]any)
	if setMap["success"] != true {
		t.Errorf("expected success true, got %v", setMap["success"])
	}
	if setMap["workspace_root"] != dirB {
		t.Errorf("expected new workspace_root %s, got %v", dirB, setMap["workspace_root"])
	}

	// Verify server and omnicoder_status reflect dirB
	if srv.WorkspaceDir() != dirB {
		t.Errorf("expected srv.WorkspaceDir() %s, got %s", dirB, srv.WorkspaceDir())
	}
	resStatus2, err := srv.Dispatch(ctx, "omnicoder_status", map[string]any{})
	if err != nil {
		t.Fatalf("omnicoder_status failed: %v", err)
	}
	sMap2 := resStatus2.(map[string]any)
	if sMap2["workspace_root"] != dirB {
		t.Errorf("expected updated workspace_root %s, got %v", dirB, sMap2["workspace_root"])
	}

	// Switch workspace to dirA via omnicoder_set_workspace using workspace_dir alias
	resSetAlias, err := srv.Dispatch(ctx, "omnicoder_set_workspace", map[string]any{
		"workspace_dir": dirA,
	})
	if err != nil {
		t.Fatalf("omnicoder_set_workspace with workspace_dir failed: %v", err)
	}
	setMapAlias := resSetAlias.(map[string]any)
	if setMapAlias["workspace_root"] != dirA {
		t.Errorf("expected workspace_root %s, got %v", dirA, setMapAlias["workspace_root"])
	}

	// Switch workspace to dirB via omnicoder_set_workspace using workspace alias
	resSetAlias2, err := srv.Dispatch(ctx, "omnicoder_set_workspace", map[string]any{
		"workspace": dirB,
	})
	if err != nil {
		t.Fatalf("omnicoder_set_workspace with workspace alias failed: %v", err)
	}
	setMapAlias2 := resSetAlias2.(map[string]any)
	if setMapAlias2["workspace_root"] != dirB {
		t.Errorf("expected workspace_root %s, got %v", dirB, setMapAlias2["workspace_root"])
	}

	// Invalid directory should fail gracefully
	_, err = srv.Dispatch(ctx, "omnicoder_set_workspace", map[string]any{
		"path": filepath.Join(t.TempDir(), "nonexistent_sub_folder_12345"),
	})
	if err == nil {
		t.Errorf("expected error when setting workspace to nonexistent dir")
	}

	// Empty path should fail
	_, err = srv.Dispatch(ctx, "omnicoder_set_workspace", map[string]any{
		"path": "",
	})
	if err == nil {
		t.Errorf("expected error when setting workspace to empty path")
	}
}

func TestOmniCoderTools_Patch_LineRange(t *testing.T) {
	tmpDir := t.TempDir()
	mathGo := filepath.Join(tmpDir, "math.go")
	initialCode := `package mathutils

func Add(a, b int) int {
	return a + b
}

func Subtract(a, b int) int {
	return a - b
}
`
	if err := os.WriteFile(mathGo, []byte(initialCode), 0644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Replace lines 7 to 9 (Subtract function) with Multiply
	replacement := `func Multiply(a, b int) int {
	return a * b
}`
	res, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":       "math.go",
		"start_line": 7,
		"end_line":   9,
		"content":    replacement,
	})
	if err != nil {
		t.Fatalf("omnicoder_patch with line range failed: %v", err)
	}

	resMap := res.(map[string]any)
	if resMap["status"] != "PATCHED" || resMap["mode"] != "LINE_RANGE" {
		t.Errorf("expected status PATCHED with mode LINE_RANGE, got %v", resMap)
	}

	updated, err := os.ReadFile(mathGo)
	if err != nil {
		t.Fatal(err)
	}
	updatedStr := string(updated)
	if !strings.Contains(updatedStr, "func Multiply") {
		t.Errorf("expected math.go to contain Multiply after line range patch: %s", updatedStr)
	}

	// Test patch alias instead of content
	resAlias, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":       "math.go",
		"start_line": 7,
		"end_line":   9,
		"patch":      "func Divide(a, b int) int {\n\treturn a / b\n}",
	})
	if err != nil {
		t.Fatalf("omnicoder_patch with 'patch' alias failed: %v", err)
	}
	resAliasMap := resAlias.(map[string]any)
	if resAliasMap["status"] != "PATCHED" {
		t.Errorf("expected status PATCHED with alias, got %v", resAliasMap)
	}
	if strings.Contains(updatedStr, "func Subtract") {
		t.Errorf("expected Subtract to be replaced in math.go: %s", updatedStr)
	}
	if !strings.Contains(updatedStr, "func Add") {
		t.Errorf("expected Add to be preserved in math.go: %s", updatedStr)
	}
}

func TestOmniCoderTools_Patch_AllowShrinkageAndTestFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testGo := filepath.Join(tmpDir, "svc_test.go")
	// Substantive > 300 bytes
	content := `package svc_test

import "testing"

func TestOne(t *testing.T) {
	t.Log("test one running")
}

func TestTwo(t *testing.T) {
	t.Log("test two running")
}

func TestThree(t *testing.T) {
	t.Log("test three running")
}

func TestFour(t *testing.T) {
	t.Log("test four running")
}
`
	if err := os.WriteFile(testGo, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Severe shrinkage rewrite on a test file should NOT be rejected by Guard C
	shrunkTest := `package svc_test

import "testing"

func TestOnly(t *testing.T) {
	t.Log("single test")
}
`
	res, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "svc_test.go",
		"content": shrunkTest,
	})
	if err != nil {
		t.Fatalf("expected test file shrinkage to succeed, got error: %v", err)
	}
	resMap := res.(map[string]any)
	if resMap["status"] != "WRITTEN" {
		t.Errorf("expected WRITTEN, got %v", resMap["status"])
	}

	// Now verify on a non-test file with allow_shrinkage: true
	nonTestGo := filepath.Join(tmpDir, "logic.go")
	if err := os.WriteFile(nonTestGo, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	shrunkLogic := `package logic

func Run() int {
	return 42
}
`
	res2, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":            "logic.go",
		"content":         shrunkLogic,
		"allow_shrinkage": true,
	})
	if err != nil {
		t.Fatalf("expected allow_shrinkage: true to bypass Guard C, got: %v", err)
	}
	resMap2 := res2.(map[string]any)
	if resMap2["status"] != "WRITTEN" {
		t.Errorf("expected WRITTEN, got %v", resMap2["status"])
	}
}

func TestOmniCoderTools_Patch_AutoRepair(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// 1. Python file with stray leading markdown fence
	pyWithFence := "```python\nimport math\n\ndef calc(x):\n    return math.sqrt(x)\n"
	res, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "calc.py",
		"content": pyWithFence,
	})
	if err != nil {
		t.Fatalf("expected auto-repair on leading fence to succeed, got: %v", err)
	}
	resMap := res.(map[string]any)
	if resMap["status"] != "WRITTEN" {
		t.Errorf("expected WRITTEN, got %v", resMap["status"])
	}

	saved, err := os.ReadFile(filepath.Join(tmpDir, "calc.py"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "```") {
		t.Errorf("expected leading fence to be stripped by AutoRepairPreflightSyntax: %s", string(saved))
	}

	// 2. Go file with trailing stray >>>> marker
	goWithMarker := "package main\n\nfunc Hello() string {\n    return \"world\"\n}\n>>>>\n"
	res2, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":    "hello.go",
		"content": goWithMarker,
	})
	if err != nil {
		t.Fatalf("expected auto-repair on trailing marker to succeed, got: %v", err)
	}
	resMap2 := res2.(map[string]any)
	if resMap2["status"] != "WRITTEN" {
		t.Errorf("expected WRITTEN, got %v", resMap2["status"])
	}

	savedGo, err := os.ReadFile(filepath.Join(tmpDir, "hello.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(savedGo), ">>>>") {
		t.Errorf("expected trailing >>>> to be stripped by AutoRepairPreflightSyntax: %s", string(savedGo))
	}
}

func TestOmniCoderTools_Sandbox_AdvisoryAndStatus(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Execute a command that fails (exit 1)
	cmd := "exit 1"
	if runtime.GOOS == "windows" {
		cmd = "powershell -Command exit 1"
	}
	res, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"command": cmd,
	})
	if err != nil {
		t.Fatalf("sandbox execution itself should not return error on non-zero exit: %v", err)
	}

	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", res)
	}

	if resMap["exit_code"] != 1 {
		t.Errorf("expected exit_code 1, got %v", resMap["exit_code"])
	}
	if resMap["rolled_back"] != true {
		t.Errorf("expected rolled_back true, got %v", resMap["rolled_back"])
	}

	advisory, _ := resMap["advisory"].(string)
	if !strings.Contains(advisory, "Advisory: Sandbox is ephemeral") {
		t.Errorf("expected advisory note, got %q", advisory)
	}

	sbStatus, ok := resMap["sandbox_status"].(map[string]any)
	if !ok {
		t.Fatalf("expected sandbox_status map, got %T", resMap["sandbox_status"])
	}
	if sbStatus["is_ephemeral"] != true {
		t.Errorf("expected is_ephemeral true, got %v", sbStatus["is_ephemeral"])
	}
	if sbStatus["persisted_to_workspace"] != false {
		t.Errorf("expected persisted_to_workspace false, got %v", sbStatus["persisted_to_workspace"])
	}
	if sbStatus["rolled_back"] != true {
		t.Errorf("expected rolled_back true, got %v", sbStatus["rolled_back"])
	}
	reason, _ := sbStatus["rollback_reason"].(string)
	if !strings.Contains(reason, "exit code 1") {
		t.Errorf("expected rollback_reason mentioning exit code 1, got %q", reason)
	}
}

func TestOmniCoderTools_DoctorAndMarkdown(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)

	ctx := context.Background()

	// 1. Create a markdown document with checklists and sections
	docPath := filepath.Join(tmpDir, "docs")
	if err := os.MkdirAll(docPath, 0755); err != nil {
		t.Fatal(err)
	}
	paperFile := filepath.Join(docPath, "spec.md")
	initialContent := `# Project Specification

### Release Checklist
- [ ] Observability middleware
- [ ] Security headers
- [ ] Multi-stage Dockerfile

### Release Details
Initial section content.
`
	if err := os.WriteFile(paperFile, []byte(initialContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Test omnicoder_toggle_checklist
	toggleRes, err := srv.Dispatch(ctx, "omnicoder_toggle_checklist", map[string]any{
		"path": "docs/spec.md",
		"items": []any{
			map[string]any{
				"pattern": "Observability",
				"checked": true,
				"note":    "verified green",
			},
			map[string]any{
				"pattern": "Security headers",
				"checked": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("omnicoder_toggle_checklist failed: %v", err)
	}
	tResMap, ok := toggleRes.(*devops.ChecklistResult)
	if !ok {
		// or map if serialized
		t.Logf("toggle result: %+v", toggleRes)
	} else if tResMap.MatchedCount != 2 {
		t.Errorf("expected 2 matches, got %d", tResMap.MatchedCount)
	}

	// Verify file content after toggle
	docBytes, _ := os.ReadFile(paperFile)
	docStr := string(docBytes)
	if !strings.Contains(docStr, "- [x] Observability middleware (verified green)") {
		t.Errorf("expected Observability to be checked with note, got:\n%s", docStr)
	}
	if !strings.Contains(docStr, "- [x] Security headers") {
		t.Errorf("expected Security headers to be checked, got:\n%s", docStr)
	}

	// 3. Test omnicoder_update_section
	_, err = srv.Dispatch(ctx, "omnicoder_update_section", map[string]any{
		"path":    "docs/spec.md",
		"heading": "### Release Details",
		"content": "Replaced release details with verified production evidence.",
	})
	if err != nil {
		t.Fatalf("omnicoder_update_section failed: %v", err)
	}
	docBytes, _ = os.ReadFile(paperFile)
	docStr = string(docBytes)
	if !strings.Contains(docStr, "Replaced release details with verified production evidence.") {
		t.Errorf("expected section update, got:\n%s", docStr)
	}

	// 4. Test omnicoder_doctor
	docRes, err := srv.Dispatch(ctx, "omnicoder_doctor", map[string]any{
		"check_gates":     false,
		"check_artifacts": []any{"docs/spec.md", "Dockerfile"},
		"include_git":     false,
		"extract_version": false,
	})
	if err != nil {
		t.Fatalf("omnicoder_doctor failed: %v", err)
	}
	t.Logf("doctor result: %+v", docRes)
}

func TestOmniCoderTools_MultiChunkPatch(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Create a sample multi-section file
	sampleCode := strings.Join([]string{
		"import { oldImport } from './old';", // line 1
		"// Header comment",                 // line 2
		"const UNTOUCHED = true;",           // line 3
		"function MyComponent() {",          // line 4
		"  const oldState = 1;",             // line 5
		"  // middle body",                  // line 6
		"  const oldHandler = () => {};",    // line 7
		"  return null;",                    // line 8
		"}",                                 // line 9
		"export default MyComponent;",       // line 10
	}, "\n") + "\n"

	filePath := filepath.Join(tmpDir, "Component.tsx")
	if err := os.WriteFile(filePath, []byte(sampleCode), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Apply multi-chunk patch:
	// Chunk 1: Line 1 (imports)
	// Chunk 2: Line 5 (state)
	// Chunk 3: Line 7-8 (handler and return)
	chunks := []any{
		map[string]any{
			"start":   1,
			"end":     1,
			"content": "import { newImport } from './new';",
		},
		map[string]any{
			"start":   5,
			"end":     5,
			"content": "  const [newState, setNewState] = useState(2);",
		},
		map[string]any{
			"start":   7,
			"end":     8,
			"content": "  const newHandler = () => alert(newState);\n  return <div>{newState}</div>;",
		},
	}

	res, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":   "Component.tsx",
		"chunks": chunks,
	})
	if err != nil {
		t.Fatalf("omnicoder_patch multi-chunk failed: %v", err)
	}

	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any result, got %T", res)
	}
	if resMap["patch_strategy"] != "multi_chunk_ast" {
		t.Errorf("expected patch_strategy 'multi_chunk_ast', got %v", resMap["patch_strategy"])
	}
	if resMap["chunks_applied"] != 3 {
		t.Errorf("expected chunks_applied 3, got %v", resMap["chunks_applied"])
	}

	// Verify resulting file on disk
	patchedBytes, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	patchedStr := string(patchedBytes)

	if !strings.Contains(patchedStr, "import { newImport } from './new';") {
		t.Errorf("expected new import at top, got:\n%s", patchedStr)
	}
	if !strings.Contains(patchedStr, "const [newState, setNewState] = useState(2);") {
		t.Errorf("expected new state in body, got:\n%s", patchedStr)
	}
	if !strings.Contains(patchedStr, "return <div>{newState}</div>;") {
		t.Errorf("expected new return jsx, got:\n%s", patchedStr)
	}
	if !strings.Contains(patchedStr, "const UNTOUCHED = true;") {
		t.Errorf("expected untouched line 3 to remain intact, got:\n%s", patchedStr)
	}

	// Test Overlap Detection:
	overlappingChunks := []any{
		map[string]any{"start": 2, "end": 4, "content": "chunk A"},
		map[string]any{"start": 3, "end": 5, "content": "chunk B"},
	}
	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":   "Component.tsx",
		"chunks": overlappingChunks,
	})
	if err == nil {
		t.Errorf("expected error for overlapping patch chunks, but got nil")
	} else if !strings.Contains(err.Error(), "overlapping") {
		t.Errorf("expected error message to mention overlapping, got: %v", err)
	}
}

func TestOmniCoderTools_AsyncSandbox(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	var echoCmd string
	if runtime.GOOS == "windows" {
		echoCmd = "cmd.exe /c echo ASYNC_STREAM_SUCCESS"
	} else {
		echoCmd = "echo ASYNC_STREAM_SUCCESS"
	}

	// Step 1: Launch command with async: true
	res, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"command": echoCmd,
		"async":   true,
	})
	if err != nil {
		t.Fatalf("async omnicoder_sandbox launch failed: %v", err)
	}

	resMap, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any result, got %T", res)
	}
	if resMap["async"] != true {
		t.Errorf("expected async: true, got %v", resMap["async"])
	}
	execID, ok := resMap["execution_id"].(string)
	if !ok || execID == "" {
		t.Fatalf("expected valid execution_id, got %v", resMap["execution_id"])
	}

	// Step 2: Poll or wait for completion using action: "wait"
	waitRes, err := srv.Dispatch(ctx, "omnicoder_sandbox", map[string]any{
		"execution_id":    execID,
		"action":          "wait",
		"wait_timeout_ms": 5000,
	})
	if err != nil {
		t.Fatalf("async omnicoder_sandbox wait failed: %v", err)
	}

	waitMap, ok := waitRes.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any result from wait, got %T", waitRes)
	}
	if strings.ToUpper(waitMap["status"].(string)) != "COMPLETED" {
		t.Errorf("expected status 'COMPLETED', got %v", waitMap["status"])
	}
	if waitMap["exit_code"].(int) != 0 {
		t.Errorf("expected exit_code 0, got %v", waitMap["exit_code"])
	}
	stdout, _ := waitMap["stdout"].(string)
	if !strings.Contains(stdout, "ASYNC_STREAM_SUCCESS") {
		t.Errorf("expected stdout to contain ASYNC_STREAM_SUCCESS, got %q", stdout)
	}
}

func TestOmniCoderTools_Doctor_RichBadge(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// Create a dummy dist directory with a mock bundle file
	distDir := filepath.Join(tmpDir, "dist")
	_ = os.MkdirAll(distDir, 0755)
	_ = os.WriteFile(filepath.Join(distDir, "bundle.js"), []byte("console.log('test-bundle-size');"), 0644)

	res, err := srv.Dispatch(ctx, "omnicoder_doctor", map[string]any{
		"check_gates":     false,
		"include_git":     false,
		"extract_version": false,
		"check_lint":      true,
		"bundle_paths":    []any{"dist"},
	})
	if err != nil {
		t.Fatalf("omnicoder_doctor failed: %v", err)
	}

	resDoc, ok := res.(*devops.DoctorResult)
	if !ok {
		t.Fatalf("expected *devops.DoctorResult, got %T", res)
	}
	if resDoc.BundleSize == "" {
		t.Errorf("expected non-empty BundleSize")
	}
	if resDoc.LintStatus != "Zero Lint Errors" {
		t.Errorf("expected 'Zero Lint Errors', got %q", resDoc.LintStatus)
	}
	if !strings.Contains(resDoc.BadgeSummary, "Bundle:") {
		t.Errorf("expected BadgeSummary to contain Bundle:, got %q", resDoc.BadgeSummary)
	}
}

func TestOmniCoderTools_DownstreamAgentErgonomicsAndAliases(t *testing.T) {
	tmpDir := t.TempDir()
	srv := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(srv, nil)
	ctx := context.Background()

	// 1. Create new file via omnicoder_patch with start_line: 1 on non-existent file
	_, err := srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"filepath":   "sample.py",
		"start_line": 1,
		"content":    "def alpha():\n    return 1\n\ndef beta():\n    return 2\n",
	})
	if err != nil {
		t.Fatalf("expected omnicoder_patch with start_line=1 on new file to succeed, got: %v", err)
	}

	// 2. Patch using search_block / replace_block aliases
	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"file_path":     "sample.py",
		"search_block":  "def alpha():\n    return 1",
		"replace_block": "def alpha():\n    return 10",
	})
	if err != nil {
		t.Fatalf("expected omnicoder_patch with search_block/replace_block to succeed, got: %v", err)
	}

	// 3. Patch using Unified Diff via patch_content alias
	unifiedDiff := "@@ -4,2 +4,2 @@\n def beta():\n-    return 2\n+    return 20\n"
	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":          "sample.py",
		"patch_content": unifiedDiff,
	})
	if err != nil {
		t.Fatalf("expected omnicoder_patch with unified diff to succeed, got: %v", err)
	}

	// 4. Read line slice using line_start / line_end aliases
	readRes, err := srv.Dispatch(ctx, "read_file", map[string]any{
		"file_path":  "sample.py",
		"line_start": 1,
		"line_end":   2,
	})
	if err != nil {
		t.Fatalf("read_file with line_start/line_end failed: %v", err)
	}
	rf := readRes.(*mcp.ReadFileResult)
	if rf.LineCount != 2 || !strings.Contains(rf.Content, "return 10") {
		t.Errorf("expected 2 lines with 'return 10', got LineCount=%d Content=%q", rf.LineCount, rf.Content)
	}

	// 5. Flat checklist toggle & section update aliases (flat parameter compatibility)
	mdPath := filepath.Join(tmpDir, "TODO.md")
	_ = os.WriteFile(mdPath, []byte("# Plan\n- [ ] Task Alpha\n\n## Status\nPending\n"), 0644)
	_, err = srv.Dispatch(ctx, "omnicoder_toggle_checklist", map[string]any{
		"file_path": "TODO.md",
		"item_text": "Task Alpha",
		"checked":   true,
	})
	if err != nil {
		t.Fatalf("flat omnicoder_toggle_checklist failed: %v", err)
	}
	_, err = srv.Dispatch(ctx, "omnicoder_update_section", map[string]any{
		"file_path":        "TODO.md",
		"heading":          "## Status",
		"replacement_text": "Completed",
	})
	if err != nil {
		t.Fatalf("omnicoder_update_section with replacement_text alias failed: %v", err)
	}
	mdBytes, _ := os.ReadFile(mdPath)
	if !strings.Contains(string(mdBytes), "- [x] Task Alpha") || !strings.Contains(string(mdBytes), "Completed") {
		t.Errorf("unexpected TODO.md content:\n%s", string(mdBytes))
	}

	// 6. Patch using old_content / new_content aliases (including alongside drifted start_line/end_line)
	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":        "sample.py",
		"old_content": "def alpha():\n    return 10",
		"new_content": "def alpha():\n    return 100",
		"start_line":  1,
		"end_line":    1,
	})
	if err != nil {
		t.Fatalf("expected omnicoder_patch with old_content/new_content to succeed, got: %v", err)
	}

	// 7. Patch using old / new aliases
	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path": "sample.py",
		"old":  "def beta():\n    return 20",
		"new":  "def beta():\n    return 200",
	})
	if err != nil {
		t.Fatalf("expected omnicoder_patch with old/new to succeed, got: %v", err)
	}

	// 8. Patch using edits: [{old_text, new_text}] alias
	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path": "sample.py",
		"edits": []any{
			map[string]any{
				"old_text": "return 100",
				"new_text": "return 1000",
			},
			map[string]any{
				"old_text": "return 200",
				"new_text": "return 2000",
			},
		},
	})
	if err != nil {
		t.Fatalf("expected omnicoder_patch with edits[{old_text, new_text}] to succeed, got: %v", err)
	}
	pyBytes, _ := os.ReadFile(filepath.Join(tmpDir, "sample.py"))
	if !strings.Contains(string(pyBytes), "return 1000") || !strings.Contains(string(pyBytes), "return 2000") {
		t.Errorf("unexpected sample.py after edits alias:\n%s", string(pyBytes))
	}
}



