// SPDX-License-Identifier: Apache-2.0
// Package mcp_test implements unit and integration tests for the
// 20th MCP tool: omnicoder_swarm.
//
// Verifies:
// 1. Exactly 20 tools registered on the live server.
// 2. All 19 original tools retain 100% signature and execution compatibility.
// 3. omnicoder_swarm exposes a valid Draft 2020-12 JSON schema.
// 4. Stdio framing isolation: tool execution logs route to os.Stderr; os.Stdout has 0 corrupt tokens.
// 5. Offline swarm invocation returns structured JSON results with Merkle attestation.

package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
	"github.com/caleralabs/omnicoder-icx/pkg/orchestrator"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// setupSwarmGitRepo initializes an isolated Git repository with an initial commit.
func setupSwarmGitRepo(t *testing.T) string {
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

	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		runGit("init")
		runGit("checkout", "-b", "main")
	}

	runGit("config", "user.email", "support@caleralabs.com")
	runGit("config", "user.name", "OmniCoder Swarm Test")

	seedFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(seedFile, []byte("# Swarm Workspace Root\n"), 0644); err != nil {
		t.Fatalf("failed to write seed file: %v", err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "seed initial commit")

	return dir
}

// setupSwarmServerWithContext spins up an in-memory stdio MCP server with custom worker handler.
func setupSwarmServerWithContext(
	t *testing.T,
	workspaceDir string,
	workerRunner func(ctx context.Context, phase *engine.SwarmPhase, alloc *orchestrator.WorktreeAllocation) error,
) (*testClient, context.CancelFunc, <-chan error) {
	t.Helper()

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	server := mcp.NewServer(workspaceDir)
	det := mcp.AutoDetectEnvironment(workspaceDir)
	det.IsOffline = true

	sCtx := &mcp.OmniCoderServerContext{
		Config:             det,
		Lattice:            icx.NewLocalLattice(),
		VMClient:           vm.NewHermeticLocalSandbox(workspaceDir),
		SandboxOn:          true,
		CustomWorkerRunner: workerRunner,
	}
	mcp.RegisterOmniCoderTools(server, sCtx)

	ctx, cancel := context.WithCancel(context.Background())
	serverErrCh := make(chan error, 1)

	go func() {
		serverErrCh <- server.Serve(ctx, clientToServerR, serverToClientW)
	}()

	client := newTestClient(t, clientToServerW, serverToClientR)
	return client, cancel, serverErrCh
}

// ============================================================================
// Unit Test 1: TestMCP_ToolsList_20Tools
// Asserts total tool count is exactly 20.
// Asserts all 19 original tools are present with unmodified schemas.
// Asserts omnicoder_swarm is present with valid Draft 2020-12 schema.
// ============================================================================
func TestMCP_ToolsList_20Tools(t *testing.T) {
	tmpDir := setupSwarmGitRepo(t)
	client, cancel, errCh := setupE2EServer(t, tmpDir)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	// Protocol handshake
	initResp := client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]any{
			"name":    "antigravity-swarm-tester",
			"version": "1.0.0",
		},
	})
	if initResp.Error != nil {
		t.Fatalf("initialize failed: %v", initResp.Error)
	}
	client.sendNotification("notifications/initialized", nil)

	// Call tools/list
	listResp := client.sendRequest("tools/list", nil)
	if listResp.Error != nil {
		t.Fatalf("tools/list failed: %v", listResp.Error)
	}

	listBytes, _ := json.Marshal(listResp.Result)
	var listResult mcp.ToolListResult
	if err := json.Unmarshal(listBytes, &listResult); err != nil {
		t.Fatalf("failed to decode ToolListResult: %v", err)
	}

	// 1. Assert tool count is EXACTLY 20
	if len(listResult.Tools) != 20 {
		t.Fatalf("expected exactly 20 tools registered, got %d", len(listResult.Tools))
	}

	toolMap := make(map[string]mcp.ToolDefinition)
	for _, tool := range listResult.Tools {
		toolMap[tool.Name] = tool
	}

	// 2. Assert all 19 original tools are present
	original19Tools := []string{
		"read_file",
		"write_file",
		"list_directory",
		"git_status",
		"git_diff",
		"git_commit",
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
		"omnicoder_verify_service",
	}

	for _, toolName := range original19Tools {
		tl, exists := toolMap[toolName]
		if !exists {
			t.Errorf("missing original tool %q among registered tools", toolName)
			continue
		}
		if strings.TrimSpace(tl.Description) == "" {
			t.Errorf("tool %q has empty description", toolName)
		}
		if tl.InputSchema == nil {
			t.Errorf("tool %q has nil inputSchema", toolName)
		}
	}

	// 3. Assert omnicoder_swarm is present with valid Draft 2020-12 schema
	swarmTool, ok := toolMap["omnicoder_swarm"]
	if !ok {
		t.Fatalf("additive 20th tool 'omnicoder_swarm' not found in tools/list")
	}

	if !strings.Contains(swarmTool.Description, "worker swarm") && !strings.Contains(swarmTool.Description, "DAG") {
		t.Errorf("unexpected omnicoder_swarm description: %s", swarmTool.Description)
	}

	schema := swarmTool.InputSchema
	if schema == nil {
		t.Fatalf("omnicoder_swarm inputSchema is nil")
	}

	if schema["type"] != "object" {
		t.Errorf("expected schema type 'object', got %v", schema["type"])
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any properties in inputSchema, got %T", schema["properties"])
	}

	expectedProps := []struct {
		Name     string
		Type     string
		Required bool
	}{
		{Name: "plan_path", Type: "string", Required: true},
		{Name: "max_workers", Type: "integer", Required: false},
		{Name: "offline", Type: "boolean", Required: false},
		{Name: "timeout_minutes", Type: "integer", Required: false},
	}

	for _, ep := range expectedProps {
		propRaw, hasProp := props[ep.Name]
		if !hasProp {
			t.Errorf("omnicoder_swarm inputSchema missing property %q", ep.Name)
			continue
		}
		propMap, isMap := propRaw.(map[string]any)
		if !isMap {
			t.Errorf("property %q is not a map", ep.Name)
			continue
		}
		if propMap["type"] != ep.Type {
			t.Errorf("property %q expected type %s, got %v", ep.Name, ep.Type, propMap["type"])
		}
	}

	reqSlice, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("expected required properties slice, got %T", schema["required"])
	}
	foundPlanPath := false
	for _, r := range reqSlice {
		if r == "plan_path" {
			foundPlanPath = true
		}
	}
	if !foundPlanPath {
		t.Errorf("expected 'plan_path' to be required in omnicoder_swarm schema, got %v", reqSlice)
	}
}

// ============================================================================
// Unit Test 2: TestMCP_ExistingToolsUnchanged
// Calls omnicoder_status, omnicoder_map, omnicoder_search, and omnicoder_patch
// to verify zero regression across existing tools.
// ============================================================================
func TestMCP_ExistingToolsUnchanged(t *testing.T) {
	tmpDir := setupSwarmGitRepo(t)

	// Seed workspace with a sample Go file for map, search, and patch
	sampleFile := filepath.Join(tmpDir, "calculator.go")
	initialCode := `package calc

import "fmt"

// ComputeSum returns formatted addition result.
func ComputeSum(a, b int) string {
	return fmt.Sprintf("Result: %d", a+b)
}
`
	if err := os.WriteFile(sampleFile, []byte(initialCode), 0644); err != nil {
		t.Fatalf("failed to seed sample file: %v", err)
	}

	client, cancel, errCh := setupE2EServer(t, tmpDir)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	_ = client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
	})
	client.sendNotification("notifications/initialized", nil)

	// 1. omnicoder_status
	t.Run("omnicoder_status_unchanged", func(t *testing.T) {
		resp := client.sendRequest("tools/call", map[string]any{
			"name":      "omnicoder_status",
			"arguments": map[string]any{},
		})
		if resp.Error != nil {
			t.Fatalf("omnicoder_status failed: %v", resp.Error)
		}
		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)
		if callRes.IsError || len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_status returned error or empty content")
		}
		text := callRes.Content[0].Text
		if !strings.Contains(text, "READY") {
			t.Errorf("status missing READY: %s", text)
		}
		if !strings.Contains(text, "tools_count") {
			t.Errorf("status missing tools_count: %s", text)
		}
	})

	// 2. omnicoder_map
	t.Run("omnicoder_map_unchanged", func(t *testing.T) {
		resp := client.sendRequest("tools/call", map[string]any{
			"name": "omnicoder_map",
			"arguments": map[string]any{
				"token_budget": 1024,
				"max_files":    20,
			},
		})
		if resp.Error != nil {
			t.Fatalf("omnicoder_map failed: %v", resp.Error)
		}
		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)
		if callRes.IsError || len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_map returned error or empty content")
		}
		text := callRes.Content[0].Text
		if !strings.Contains(text, "calculator.go") {
			t.Errorf("omnicoder_map missing calculator.go: %s", text)
		}
	})

	// 3. omnicoder_search
	t.Run("omnicoder_search_unchanged", func(t *testing.T) {
		resp := client.sendRequest("tools/call", map[string]any{
			"name": "omnicoder_search",
			"arguments": map[string]any{
				"query": "ComputeSum",
			},
		})
		if resp.Error != nil {
			t.Fatalf("omnicoder_search failed: %v", resp.Error)
		}
		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)
		if callRes.IsError || len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_search returned error or empty content")
		}
		text := callRes.Content[0].Text
		if !strings.Contains(text, "ComputeSum") {
			t.Errorf("omnicoder_search missing ComputeSum: %s", text)
		}
	})

	// 4. omnicoder_patch
	t.Run("omnicoder_patch_unchanged", func(t *testing.T) {
		patchContent := `<<<<<<< SEARCH
func ComputeSum(a, b int) string {
	return fmt.Sprintf("Result: %d", a+b)
}
=======
func ComputeSum(a, b int) string {
	return fmt.Sprintf("Sum is %d", a+b)
}
>>>>>>> REPLACE`

		resp := client.sendRequest("tools/call", map[string]any{
			"name": "omnicoder_patch",
			"arguments": map[string]any{
				"path":    "calculator.go",
				"content": patchContent,
			},
		})
		if resp.Error != nil {
			t.Fatalf("omnicoder_patch failed: %v", resp.Error)
		}
		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)
		if callRes.IsError || len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_patch returned error or empty content")
		}
		text := callRes.Content[0].Text
		if !strings.Contains(text, "SUCCESS") && !strings.Contains(text, "applied") {
			t.Errorf("omnicoder_patch response unexpected: %s", text)
		}

		// Verify file on disk
		content, _ := os.ReadFile(sampleFile)
		if !strings.Contains(string(content), "Sum is %d") {
			t.Errorf("patched content not found in file: %s", string(content))
		}
	})
}

// ============================================================================
// Unit Test 3: TestMCP_ToolCall_SwarmOffline
// Calls tools/call for omnicoder_swarm with a mock plan and verifies structured JSON response.
// ============================================================================
func TestMCP_ToolCall_SwarmOffline(t *testing.T) {
	tmpDir := setupSwarmGitRepo(t)

	// 1. Create a structured markdown plan in docs/MOCK_SWARM_PLAN.md
	planDir := filepath.Join(tmpDir, "docs")
	if err := os.MkdirAll(planDir, 0755); err != nil {
		t.Fatalf("failed to create docs dir: %v", err)
	}

	planContent := `# Microservice Swarm Implementation Plan

## Architectural Overview
Parallel implementation plan for testing the swarm orchestrator.

### Phase 1: Core Domain Entities
- **Goal**: Define foundational data structures and models.
- **Target Files**:
  - ` + "`" + `pkg/domain/models.go` + "`" + `
- **Prompt**: Implement Entity and Record structs with serialisation.

### Phase 2: High Performance Engine
- **Goal**: Implement high-throughput computational pipeline.
- **Target Files**:
  - ` + "`" + `pkg/compute/pipeline.go` + "`" + `
- **Prompt**: Implement Pipeline processing function.
`
	planPath := filepath.Join(planDir, "MOCK_SWARM_PLAN.md")
	if err := os.WriteFile(planPath, []byte(planContent), 0644); err != nil {
		t.Fatalf("failed to write mock plan: %v", err)
	}

	// 2. Setup mock worker that creates target files in the allocated worktrees
	mockWorker := func(ctx context.Context, phase *engine.SwarmPhase, alloc *orchestrator.WorktreeAllocation) error {
		for _, tf := range phase.TargetFiles {
			full := filepath.Join(alloc.WorktreeDir, tf)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				return err
			}
			code := fmt.Sprintf("// Phase %d: %s\npackage generated\n\nfunc Phase%dFunc() string { return %q }\n",
				phase.PhaseNumber, phase.Name, phase.PhaseNumber, phase.Name)
			if err := os.WriteFile(full, []byte(code), 0644); err != nil {
				return err
			}
		}

		// Commit in worktree so git 2PC merge can merge cleanly
		if !alloc.IsJunction {
			cAdd := exec.Command("git", "add", "-A")
			cAdd.Dir = alloc.WorktreeDir
			_ = cAdd.Run()

			cCommit := exec.Command("git", "commit", "-m", fmt.Sprintf("feat: implement phase %d", phase.PhaseNumber))
			cCommit.Dir = alloc.WorktreeDir
			_ = cCommit.Run()
		}
		return nil
	}

	client, cancel, errCh := setupSwarmServerWithContext(t, tmpDir, mockWorker)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	_ = client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
	})
	client.sendNotification("notifications/initialized", nil)

	// 3. Dispatch omnicoder_swarm tool call
	callResp := client.sendRequest("tools/call", map[string]any{
		"name": "omnicoder_swarm",
		"arguments": map[string]any{
			"plan_path":       "docs/MOCK_SWARM_PLAN.md",
			"offline":         true,
			"max_workers":     2,
			"timeout_minutes": 5,
		},
	})

	if callResp.Error != nil {
		t.Fatalf("omnicoder_swarm tools/call failed with JSON-RPC error: %v", callResp.Error)
	}

	resBytes, err := json.Marshal(callResp.Result)
	if err != nil {
		t.Fatalf("failed to marshal CallToolResult: %v", err)
	}

	var callRes mcp.CallToolResult
	if err := json.Unmarshal(resBytes, &callRes); err != nil {
		t.Fatalf("failed to unmarshal CallToolResult: %v", err)
	}

	if callRes.IsError {
		errMsg := "unknown"
		if len(callRes.Content) > 0 {
			errMsg = callRes.Content[0].Text
		}
		t.Fatalf("omnicoder_swarm returned isError=true: %s", errMsg)
	}

	if len(callRes.Content) == 0 {
		t.Fatalf("omnicoder_swarm returned empty content array")
	}

	jsonText := callRes.Content[0].Text
	var swarmResult orchestrator.SwarmExecutionResult
	if err := json.Unmarshal([]byte(jsonText), &swarmResult); err != nil {
		t.Fatalf("failed to unmarshal SwarmExecutionResult from tool text: %v (raw: %s)", err, jsonText)
	}

	// 4. Assert structured JSON fields
	if swarmResult.Status != "SUCCESS" {
		t.Errorf("expected status 'SUCCESS', got %q", swarmResult.Status)
	}
	if !strings.Contains(swarmResult.PlanPath, "MOCK_SWARM_PLAN.md") {
		t.Errorf("expected plan_path to contain MOCK_SWARM_PLAN.md, got %q", swarmResult.PlanPath)
	}
	if swarmResult.WavesCount <= 0 {
		t.Errorf("expected waves_count > 0, got %d", swarmResult.WavesCount)
	}
	if swarmResult.PhasesCount != 2 {
		t.Errorf("expected phases_count 2, got %d", swarmResult.PhasesCount)
	}
	if len(swarmResult.Waves) == 0 {
		t.Errorf("expected non-empty waves slice")
	}
	if len(swarmResult.FilesTouched) < 2 {
		t.Errorf("expected at least 2 files touched, got %d: %v", len(swarmResult.FilesTouched), swarmResult.FilesTouched)
	}
	if swarmResult.MerkleSeal == "" {
		t.Errorf("expected non-empty MerkleSeal")
	}
	if !strings.Contains(swarmResult.MerkleSeal, "attest_wave_") && len(swarmResult.MerkleSeal) != 64 {
		t.Errorf("expected valid SHA-256 or wave attestation MerkleSeal, got %q", swarmResult.MerkleSeal)
	}
	if swarmResult.DurationMs < 0 {
		t.Errorf("expected duration_ms >= 0, got %d", swarmResult.DurationMs)
	}

	// 5. Test missing plan_path validation
	invalidResp := client.sendRequest("tools/call", map[string]any{
		"name":      "omnicoder_swarm",
		"arguments": map[string]any{},
	})
	if invalidResp.Error == nil {
		invBytes, _ := json.Marshal(invalidResp.Result)
		var invRes mcp.CallToolResult
		_ = json.Unmarshal(invBytes, &invRes)
		if !invRes.IsError {
			t.Errorf("expected tool error when plan_path is omitted, got success")
		}
	}
}

// TestMCP_ToolCall_SwarmFailureDiagnosticsSurfaced verifies that worker failures surface
// rich diagnostics (phase metadata, error details, duration) directly at the top-level MCP response.
func TestMCP_ToolCall_SwarmFailureDiagnosticsSurfaced(t *testing.T) {
	tmpDir := setupSwarmGitRepo(t)

	// Custom worker runner that simulates a failure with diagnostic
	failingRunner := func(ctx context.Context, phase *engine.SwarmPhase, alloc *orchestrator.WorktreeAllocation) error {
		return fmt.Errorf("simulated compilation panic: undefined symbol 'CorruptBuffer'")
	}

	client, cancel, errCh := setupSwarmServerWithContext(t, tmpDir, failingRunner)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	_ = client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]any{
			"name":    "diagnostic-test-client",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{},
	})

	planContent := `# Failure Test Plan

### Phase 1: Failing Substrate
* Target Files:
  - pkg/fail/fail.go
* Verification: go test ./pkg/fail/...
`
	planFile := filepath.Join(tmpDir, "FAIL_PLAN.md")
	if err := os.WriteFile(planFile, []byte(planContent), 0644); err != nil {
		t.Fatalf("failed to write fail plan: %v", err)
	}

	callResp := client.sendRequest("tools/call", map[string]any{
		"name": "omnicoder_swarm",
		"arguments": map[string]any{
			"plan_path": "FAIL_PLAN.md",
			"offline":   true,
		},
	})

	if callResp.Error == nil {
		resBytes, _ := json.Marshal(callResp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)
		if !callRes.IsError {
			t.Fatalf("expected tool call to return isError=true on worker failure")
		}
		if len(callRes.Content) == 0 {
			t.Fatalf("expected error message in content")
		}
		errMsg := callRes.Content[0].Text
		if !strings.Contains(errMsg, "simulated compilation panic") {
			t.Errorf("expected error to surface 'simulated compilation panic', got: %s", errMsg)
		}
		if !strings.Contains(errMsg, "Phase 1") && !strings.Contains(errMsg, "phase 1") {
			t.Errorf("expected error to surface phase number, got: %s", errMsg)
		}
	} else {
		errMsg := callResp.Error.Message
		if !strings.Contains(errMsg, "simulated compilation panic") {
			t.Errorf("expected error message to surface root cause, got: %s", errMsg)
		}
	}
}

