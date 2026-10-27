// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

var (
	builtTestBinOnce sync.Once
	builtTestBinPath string
	builtTestBinErr  error
)

func getOmnicoderBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../omnicoder.exe",
		"../../omnicoder",
		"omnicoder.exe",
		"omnicoder",
		filepath.Join(os.Getenv("OMNICODER_WORKSPACE"), "omnicoder.exe"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		abs, err := filepath.Abs(c)
		if err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs
			}
		}
	}
	builtTestBinOnce.Do(func() {
		binName := "omnicoder"
		if runtime.GOOS == "windows" {
			binName = "omnicoder.exe"
		}
		tmpDir, err := os.MkdirTemp("", "omnicoder-test-bin-*")
		if err != nil {
			builtTestBinErr = err
			return
		}
		outPath := filepath.Join(tmpDir, binName)
		cmd := exec.Command("go", "build", "-o", outPath, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtTestBinErr = os.ErrNotExist
			_ = out
			return
		}
		builtTestBinPath = outPath
	})
	if builtTestBinErr == nil && builtTestBinPath != "" {
		return builtTestBinPath
	}
	t.Fatalf("omnicoder binary not found in candidates: %v", candidates)
	return ""
}

func TestIsInteractiveTerminal(t *testing.T) {
	// 1. Nil file must return false
	if isInteractiveTerminal(nil) {
		t.Errorf("expected isInteractiveTerminal(nil) to be false")
	}

	// 2. Regular temporary file must return false (not a character device)
	tmpFile, err := os.CreateTemp(t.TempDir(), "terminal_test_*.tmp")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer tmpFile.Close()

	if isInteractiveTerminal(tmpFile) {
		t.Errorf("expected isInteractiveTerminal(regularFile) to be false")
	}

	// 3. Pipe endpoints must return false (pipes are os.ModeNamedPipe, not os.ModeCharDevice)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	if isInteractiveTerminal(r) {
		t.Errorf("expected isInteractiveTerminal(pipeReader) to be false")
	}
	if isInteractiveTerminal(w) {
		t.Errorf("expected isInteractiveTerminal(pipeWriter) to be false")
	}

	// 4. os.DevNull must return false (null character device, not interactive console)
	devNull, err := os.Open(os.DevNull)
	if err == nil {
		defer devNull.Close()
		if isInteractiveTerminal(devNull) {
			t.Errorf("expected isInteractiveTerminal(os.DevNull) to be false")
		}
	}
}

func TestZeroArgs_NonInteractive_PrintsUsage(t *testing.T) {
	binPath := getOmnicoderBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Redirect stdin with a pipe (strings.NewReader) to ensure non-interactive execution
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		t.Fatalf("zero-argument execution failed: %v\nStderr: %s", err, stderr.String())
	}

	combined := stderr.String() + "\n" + stdout.String()
	if !strings.Contains(combined, "OmniCoder-ICX — All-in-One Autonomous Coding Engine") {
		t.Errorf("expected root banner in output, got:\n%s", combined)
	}
	if !strings.Contains(combined, "chat [options]") {
		t.Errorf("expected 'chat [options]' command in usage, got:\n%s", combined)
	}
	if !strings.Contains(combined, "gemini-3.5-flash-lite") {
		t.Errorf("expected default model 'gemini-3.5-flash-lite' in usage, got:\n%s", combined)
	}
}

func TestChatSubcommand_CLI_Invocation(t *testing.T) {
	binPath := getOmnicoderBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "chat", "--offline", "--workspace="+t.TempDir())
	cmd.Stdin = strings.NewReader("/exit\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		t.Fatalf("omnicoder chat --offline failed: %v\nStderr: %s", err, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "OMNICODER-ICX") && !strings.Contains(outStr, "OmniCoder-ICX") {
		t.Errorf("expected banner in chat output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "omnicoder>") {
		t.Errorf("expected prompt 'omnicoder>' in chat output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "Goodbye") {
		t.Errorf("expected goodbye message on exit, got:\n%s", outStr)
	}
}

func TestChatSubcommand_InProcess_SlashCommands(t *testing.T) {
	tempWs := t.TempDir()
	input := strings.NewReader("/status\n/reset\n/help\n/exit\n")
	var output bytes.Buffer

	cfg := defaultGlobalConfig()
	cfg.Workspace = tempWs
	cfg.Offline = true

	err := executeChatWithIO(context.Background(), &cfg, []string{"--offline", "--workspace=" + tempWs}, input, &output)
	if err != nil {
		t.Fatalf("executeChatWithIO returned unexpected error: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "OMNICODER-ICX") && !strings.Contains(outStr, "OmniCoder-ICX") {
		t.Errorf("output missing welcome banner:\n%s", outStr)
	}
	if !strings.Contains(outStr, "OmniCoder-ICX Slash Commands:") {
		t.Errorf("output missing slash commands listing:\n%s", outStr)
	}
	if !strings.Contains(outStr, "Dialogue history cleared") {
		t.Errorf("output missing reset confirmation:\n%s", outStr)
	}
	if !strings.Contains(outStr, "Session Status:") {
		t.Errorf("output missing /status information:\n%s", outStr)
	}
}

func TestChatSubcommand_DefaultModel_And_KeyResolution(t *testing.T) {
	tempWs := t.TempDir()
	input := strings.NewReader("/exit\n")
	var output bytes.Buffer

	// Provide empty parent config to test default model and fallback key resolution
	parentCfg := defaultGlobalConfig()
	parentCfg.Workspace = tempWs

	err := executeChatWithIO(context.Background(), &parentCfg, []string{"--workspace=" + tempWs}, input, &output)
	if err != nil {
		t.Fatalf("executeChatWithIO failed: %v", err)
	}

	outStr := output.String()
	// Should show gemini-3.5-flash-lite in status badges
	if !strings.Contains(outStr, "gemini-3.5-flash-lite") {
		t.Errorf("expected default model gemini-3.5-flash-lite in status badge, got:\n%s", outStr)
	}
}

func TestChatSubcommand_FlagHandling(t *testing.T) {
	tempWs := t.TempDir()

	// 1. Unknown flag should fail gracefully
	input := strings.NewReader("/exit\n")
	var output bytes.Buffer
	err := executeChatWithIO(context.Background(), nil, []string{"--nonexistent-flag-abc"}, input, &output)
	if err == nil {
		t.Errorf("expected error for nonexistent flag, got nil")
	}

	// 2. Help flag should return nil
	var helpOut bytes.Buffer
	err = executeChatWithIO(context.Background(), nil, []string{"--help"}, strings.NewReader(""), &helpOut)
	if err != nil {
		t.Errorf("expected --help to return nil, got %v", err)
	}

	// 3. Custom model and workspace flags
	var customOut bytes.Buffer
	cfg := defaultGlobalConfig()
	err = executeChatWithIO(context.Background(), &cfg, []string{
		"--workspace=" + tempWs,
		"--model=gemini-custom-model",
		"--api-key=test-key-12345",
		"--offline",
	}, strings.NewReader("/exit\n"), &customOut)
	if err != nil {
		t.Fatalf("unexpected error with custom flags: %v", err)
	}
}

func TestExistingSubcommands_Preserved(t *testing.T) {
	binPath := getOmnicoderBinary(t)

	// 1. Version command
	cmdVersion := exec.Command(binPath, "version")
	outVersion, err := cmdVersion.CombinedOutput()
	if err != nil {
		t.Fatalf("version command failed: %v\nOutput: %s", err, string(outVersion))
	}
	if !strings.Contains(string(outVersion), "OmniCoder-ICX v1.0.0") {
		t.Errorf("unexpected version output: %s", string(outVersion))
	}

	// 2. Help command
	cmdHelp := exec.Command(binPath, "help")
	var helpStderr bytes.Buffer
	cmdHelp.Stderr = &helpStderr
	if err := cmdHelp.Run(); err != nil {
		t.Fatalf("help command failed: %v", err)
	}
	if !strings.Contains(helpStderr.String(), "Commands:") || !strings.Contains(helpStderr.String(), "Global Flags:") {
		t.Errorf("unexpected help output:\n%s", helpStderr.String())
	}

	// 3. Run command without arguments should exit 1
	cmdRun := exec.Command(binPath, "run")
	var runStderr bytes.Buffer
	cmdRun.Stderr = &runStderr
	err = cmdRun.Run()
	if err == nil {
		t.Fatalf("expected omnicoder run without args to fail with code 1")
	}
	if !strings.Contains(runStderr.String(), "task description required") {
		t.Errorf("expected missing task description error, got:\n%s", runStderr.String())
	}

	// 4. Heal command without arguments should exit 1
	cmdHeal := exec.Command(binPath, "heal")
	if err := cmdHeal.Run(); err == nil {
		t.Fatalf("expected omnicoder heal without args to fail with code 1")
	}

	// 5. Refactor command without arguments should exit 1
	cmdRefactor := exec.Command(binPath, "refactor")
	if err := cmdRefactor.Run(); err == nil {
		t.Fatalf("expected omnicoder refactor without args to fail with code 1")
	}
}

func TestDefaultModel_Constant(t *testing.T) {
	if DefaultModel != "gemini-3.5-flash-lite" {
		t.Errorf("expected DefaultModel to be 'gemini-3.5-flash-lite', got %q", DefaultModel)
	}
	if DefaultModel != llm.DefaultModel {
		t.Errorf("cmd/omnicoder DefaultModel (%q) must match pkg/llm.DefaultModel (%q)", DefaultModel, llm.DefaultModel)
	}
}

func TestSubcommands_HelpFlag_ExitZero(t *testing.T) {
	binPath := getOmnicoderBinary(t)
	subcommands := []string{"run", "map", "heal", "refactor", "repl", "chat", "version", "help", "mcp"}

	for _, subcmd := range subcommands {
		for _, flag := range []string{"--help", "-h"} {
			cmd := exec.Command(binPath, subcmd, flag)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err != nil {
				t.Errorf("omnicoder %s %s failed with error: %v\nStderr: %s\nStdout: %s",
					subcmd, flag, err, stderr.String(), stdout.String())
			}
		}
	}
}

func TestMCPSubcommand_CLI_Invocation(t *testing.T) {
	binPath := getOmnicoderBinary(t)
	tmpDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "mcp", "--workspace="+tmpDir)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to create stdin pipe: %v", err)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start omnicoder mcp: %v", err)
	}

	// Helper to send request
	sendReq := func(reqMap map[string]any) {
		data, err := json.Marshal(reqMap)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		data = append(data, '\n')
		if _, err := stdinPipe.Write(data); err != nil {
			t.Fatalf("write error: %v", err)
		}
	}

	// 1. Send initialize
	sendReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo": map[string]any{
				"name":    "antigravity-test",
				"version": "1.0.0",
			},
		},
	})

	// 2. Send initialized notification
	sendReq(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	// 3. Send tools/list
	sendReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})

	// 4. Send omnicoder_status call
	sendReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "omnicoder_status",
			"arguments": map[string]any{},
		},
	})

	// Close stdin to signal EOF and trigger clean server shutdown
	_ = stdinPipe.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("omnicoder mcp failed: %v\nStderr:\n%s\nStdout:\n%s", err, stderrBuf.String(), stdoutBuf.String())
	}

	stderrStr := stderrBuf.String()
	stdoutStr := stdoutBuf.String()

	// Verify stderr isolation: banner and diagnostics must be on stderr exclusively
	if !strings.Contains(stderrStr, "OmniCoder-ICX MCP Server") {
		t.Errorf("expected MCP banner in stderr, got:\n%s", stderrStr)
	}
	if !strings.Contains(stderrStr, "Calera MicroVM with CoW Rollback (ENABLED)") {
		t.Errorf("expected enabled sandbox in stderr, got:\n%s", stderrStr)
	}
	if !strings.Contains(stderrStr, "Transport: stdio (JSON-RPC 2.0") {
		t.Errorf("expected transport in stderr, got:\n%s", stderrStr)
	}

	// Verify stdout contains ONLY valid JSON-RPC responses (line by line)
	scanner := bufio.NewScanner(strings.NewReader(stdoutStr))
	responses := make(map[any]map[string]any)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("stdout contained non-JSON-RPC line: %q (err: %v)", line, err)
		}
		if id, ok := parsed["id"]; ok {
			responses[id] = parsed
		}
	}

	// Verify response 1 (initialize)
	respInit, ok := responses[float64(1)]
	if !ok {
		t.Fatalf("missing response for initialize request; stdout was:\n%s", stdoutStr)
	}
	initResult, _ := respInit["result"].(map[string]any)
	if initResult["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got %v", initResult["protocolVersion"])
	}

	// Verify response 2 (tools/list)
	respTools, ok := responses[float64(2)]
	if !ok {
		t.Fatalf("missing response for tools/list request; stdout was:\n%s", stdoutStr)
	}
	toolsResult, _ := respTools["result"].(map[string]any)
	toolsList, _ := toolsResult["tools"].([]any)

	toolNames := make(map[string]bool)
	for _, tl := range toolsList {
		if tm, ok := tl.(map[string]any); ok {
			if n, ok := tm["name"].(string); ok {
				toolNames[n] = true
			}
		}
	}

	expectedTools := []string{
		// Filesystem tools
		"read_file", "write_file", "list_directory",
		// Git tools
		"git_status", "git_diff", "git_commit",
		// Omnicoder tools
		"omnicoder_status", "omnicoder_map", "omnicoder_search",
		"omnicoder_patch", "omnicoder_heal", "omnicoder_refactor",
		"omnicoder_sandbox", "omnicoder_run",
	}

	for _, exp := range expectedTools {
		if !toolNames[exp] {
			t.Errorf("expected tool %q in tools/list, but it was missing. Got: %v", exp, toolNames)
		}
	}

	// Verify response 3 (omnicoder_status tool call)
	respStatus, ok := responses[float64(3)]
	if !ok {
		t.Fatalf("missing response for omnicoder_status call; stdout was:\n%s", stdoutStr)
	}
	statusResult, _ := respStatus["result"].(map[string]any)
	contentArr, _ := statusResult["content"].([]any)
	if len(contentArr) == 0 {
		t.Fatalf("expected content in omnicoder_status result, got: %v", statusResult)
	}
	firstContent, _ := contentArr[0].(map[string]any)
	textStr, _ := firstContent["text"].(string)

	var statusPayload map[string]any
	if err := json.Unmarshal([]byte(textStr), &statusPayload); err != nil {
		t.Fatalf("failed to parse omnicoder_status text JSON: %v", err)
	}

	if statusPayload["status"] != "READY" {
		t.Errorf("expected status 'READY', got %v", statusPayload["status"])
	}
	if statusPayload["sandbox_on"] != true {
		t.Errorf("expected sandbox_on to be true by default, got %v", statusPayload["sandbox_on"])
	}
	if statusPayload["sandbox_ready"] != true {
		t.Errorf("expected sandbox_ready to be true, got %v", statusPayload["sandbox_ready"])
	}
}

func TestMCPSubcommand_SandboxFlag(t *testing.T) {
	binPath := getOmnicoderBinary(t)
	tmpDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "mcp", "--sandbox=false", "--workspace="+tmpDir)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to create stdin pipe: %v", err)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start omnicoder mcp: %v", err)
	}

	// 1. Send initialize
	reqData, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
		},
	})
	reqData = append(reqData, '\n')
	_, _ = stdinPipe.Write(reqData)

	// 2. Call omnicoder_status
	statusReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "omnicoder_status",
			"arguments": map[string]any{},
		},
	})
	statusReq = append(statusReq, '\n')
	_, _ = stdinPipe.Write(statusReq)

	_ = stdinPipe.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd wait failed: %v", err)
	}

	stderrStr := stderrBuf.String()
	if !strings.Contains(stderrStr, "Calera MicroVM with CoW Rollback (DISABLED)") {
		t.Errorf("expected DISABLED sandbox in stderr, got:\n%s", stderrStr)
	}

	// Parse response 2
	var foundStatus bool
	scanner := bufio.NewScanner(strings.NewReader(stdoutBuf.String()))
	for scanner.Scan() {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &parsed); err == nil {
			if parsed["id"] == float64(2) {
				foundStatus = true
				res, _ := parsed["result"].(map[string]any)
				content, _ := res["content"].([]any)
				first, _ := content[0].(map[string]any)
				text, _ := first["text"].(string)
				var payload map[string]any
				_ = json.Unmarshal([]byte(text), &payload)
				if payload["sandbox_on"] != false {
					t.Errorf("expected sandbox_on to be false with --sandbox=false, got %v", payload["sandbox_on"])
				}
			}
		}
	}

	if !foundStatus {
		t.Fatalf("omnicoder_status response not found in stdout:\n%s", stdoutBuf.String())
	}
}

// ----------------------------------------------------------------------------
// CLI Subcommand & Intent Parsing Tests
// ----------------------------------------------------------------------------

func TestCLI_SwarmSubcommandHelp(t *testing.T) {
	binPath := getOmnicoderBinary(t)

	for _, flag := range []string{"--help", "-h"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, binPath, "swarm", flag)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		cancel()
		if err != nil {
			t.Fatalf("omnicoder swarm %s failed with error: %v\nStderr: %s\nStdout: %s",
				flag, err, stderr.String(), stdout.String())
		}

		output := stderr.String() + "\n" + stdout.String()
		if !strings.Contains(output, "Usage of swarm:") && !strings.Contains(output, "swarm [options]") {
			t.Errorf("expected usage documentation in output for %s, got:\n%s", flag, output)
		}
		if !strings.Contains(output, "-max-workers") {
			t.Errorf("expected -max-workers flag in swarm usage, got:\n%s", output)
		}
	}
}

const testMockPlanMarkdown = `# Mock Implementation Plan

## Phase 1: Core Engine
- Target: ` + "`projects/mock-service/go.mod`" + `
- Target: ` + "`projects/mock-service/pkg/core/core.go`" + `

## Phase 2: API Layer
- Target: ` + "`projects/mock-service/pkg/api/api.go`" + `
`

func writeTempMockPlan(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mock_plan.md")
	if err := os.WriteFile(p, []byte(testMockPlanMarkdown), 0644); err != nil {
		t.Fatalf("failed to write temp mock plan: %v", err)
	}
	return p
}

func TestCLI_SwarmExecution_Offline(t *testing.T) {
	binPath := getOmnicoderBinary(t)
	planPath := writeTempMockPlan(t)

	tmpWorkspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "swarm", "--offline", "--workspace", tmpWorkspace, planPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	combined := stdout.String() + "\n" + stderr.String()
	if err != nil {
		t.Fatalf("omnicoder swarm --offline %s failed: %v\nOutput:\n%s", planPath, err, combined)
	}

	if !strings.Contains(combined, "[Swarm] Initializing Swarm Forge") {
		t.Errorf("expected swarm initialization in output, got:\n%s", combined)
	}
	if !strings.Contains(combined, "LAUNCHING EXECUTION WAVE") {
		t.Errorf("expected wave execution in output, got:\n%s", combined)
	}
	if !strings.Contains(combined, "SWARM FORGE OUTCOME: 100% SUCCESS") {
		t.Errorf("expected success banner in output, got:\n%s", combined)
	}
}

func TestCLI_ExistingSubcommandsUnaltered(t *testing.T) {
	binPath := getOmnicoderBinary(t)

	subcommands := []string{"run", "mcp", "chat"}
	for _, subcmd := range subcommands {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, binPath, subcmd, "--help")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		cancel()
		if err != nil {
			t.Errorf("omnicoder %s --help failed: %v\nStderr: %s\nStdout: %s", subcmd, err, stderr.String(), stdout.String())
		}

		output := stderr.String() + "\n" + stdout.String()
		expectedUsage := "Usage of " + subcmd + ":"
		if !strings.Contains(output, expectedUsage) {
			t.Errorf("expected %q in output for %s --help, got:\n%s", expectedUsage, subcmd, output)
		}

		// Ensure -max-workers is NOT present in existing subcommands
		if strings.Contains(output, "-max-workers") {
			t.Errorf("subcommand %s --help must NOT contain -max-workers, but it was found in:\n%s", subcmd, output)
		}
	}
}

func TestCLI_ConversationalIntent_SwarmTrigger(t *testing.T) {
	binPath := getOmnicoderBinary(t)
	planPath := writeTempMockPlan(t)

	tmpWorkspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "--offline", "--workspace", tmpWorkspace, "Complete this project "+planPath)
	cmd.Env = append(os.Environ(), "OMNICODER_OFFLINE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	combined := stdout.String() + "\n" + stderr.String()
	if err != nil {
		t.Fatalf("conversational intent trigger failed: %v\nOutput:\n%s", err, combined)
	}

	if !strings.Contains(combined, "[Swarm] Initializing Swarm Forge") {
		t.Errorf("expected swarm initialization via intent trigger, got:\n%s", combined)
	}
	if !strings.Contains(combined, "SWARM FORGE OUTCOME: 100% SUCCESS") {
		t.Errorf("expected success banner via intent trigger, got:\n%s", combined)
	}
}

