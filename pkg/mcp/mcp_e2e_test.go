// SPDX-License-Identifier: Apache-2.0
// Package mcp_test provides comprehensive end-to-end and integration tests
// for the OmniCoder-ICX Model Context Protocol (MCP) server under the Config-and-Go doctrine.

package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// ----------------------------------------------------------------------------
// Test Client Harness
// ----------------------------------------------------------------------------

type testClient struct {
	t            *testing.T
	writer       io.WriteCloser
	reader       *bufio.Reader
	reqCounter   int
	isCLFramed   bool
}

func newTestClient(t *testing.T, w io.WriteCloser, r io.Reader) *testClient {
	return &testClient{
		t:      t,
		writer: w,
		reader: bufio.NewReader(r),
	}
}

func (c *testClient) sendRequest(method string, params any) mcp.JSONRPCResponse {
	c.t.Helper()
	c.reqCounter++
	id := c.reqCounter

	var rawParams json.RawMessage
	if params != nil {
		pBytes, err := json.Marshal(params)
		if err != nil {
			c.t.Fatalf("failed to marshal params: %v", err)
		}
		rawParams = pBytes
	}

	req := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		c.t.Fatalf("failed to marshal request: %v", err)
	}

	if c.isCLFramed {
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(reqBytes))
		if _, err := c.writer.Write([]byte(header)); err != nil {
			c.t.Fatalf("failed to write Content-Length header: %v", err)
		}
		if _, err := c.writer.Write(reqBytes); err != nil {
			c.t.Fatalf("failed to write request body: %v", err)
		}
	} else {
		reqBytes = append(reqBytes, '\n')
		if _, err := c.writer.Write(reqBytes); err != nil {
			c.t.Fatalf("failed to write request: %v", err)
		}
	}

	// Read response
	line, err := c.reader.ReadString('\n')
	if err != nil {
		c.t.Fatalf("failed to read response for method %s: %v", method, err)
	}

	// Handle optional Content-Length prefix if present
	line = strings.TrimSpace(line)
	if strings.HasPrefix(strings.ToLower(line), "content-length:") {
		// Read until empty line delimiter
		for {
			hdr, hErr := c.reader.ReadString('\n')
			if hErr != nil {
				c.t.Fatalf("failed reading header: %v", hErr)
			}
			if strings.TrimSpace(hdr) == "" {
				break
			}
		}
		bodyLine, bErr := c.reader.ReadString('\n')
		if bErr != nil && bErr != io.EOF {
			c.t.Fatalf("failed reading body after header: %v", bErr)
		}
		line = strings.TrimSpace(bodyLine)
	}

	var resp mcp.JSONRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		c.t.Fatalf("failed to unmarshal JSON-RPC response: %v, raw: %s", err, line)
	}
	return resp
}

func (c *testClient) sendNotification(method string, params any) {
	c.t.Helper()
	var rawParams json.RawMessage
	if params != nil {
		pBytes, err := json.Marshal(params)
		if err != nil {
			c.t.Fatalf("failed to marshal notification params: %v", err)
		}
		rawParams = pBytes
	}

	notif := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}

	notifBytes, err := json.Marshal(notif)
	if err != nil {
		c.t.Fatalf("failed to marshal notification: %v", err)
	}
	notifBytes = append(notifBytes, '\n')
	if _, err := c.writer.Write(notifBytes); err != nil {
		c.t.Fatalf("failed to write notification: %v", err)
	}
}

// setupE2EServer creates a fully wired MCP test environment with pipes and background server goroutine.
func setupE2EServer(t *testing.T, workspaceDir string) (*testClient, context.CancelFunc, <-chan error) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	server := mcp.NewServer(workspaceDir)
	det := mcp.AutoDetectEnvironment(workspaceDir)

	sCtx := &mcp.OmniCoderServerContext{
		Config:    det,
		Lattice:   icx.NewLocalLattice(),
		VMClient:  vm.NewHermeticLocalSandbox(workspaceDir),
		SandboxOn: true,
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

// ----------------------------------------------------------------------------
// 1. Client Handshake Test
// ----------------------------------------------------------------------------

func TestMCP_ClientHandshake(t *testing.T) {
	tmpDir := t.TempDir()
	client, cancel, errCh := setupE2EServer(t, tmpDir)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	// Step 1: Send "initialize" request with client info & capabilities
	initResp := client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]any{
			"name":    "antigravity-ide",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{
			"roots": map[string]any{
				"listChanged": true,
			},
		},
	})

	if initResp.Error != nil {
		t.Fatalf("initialize returned JSON-RPC error: %v", initResp.Error)
	}

	initBytes, _ := json.Marshal(initResp.Result)
	var initResult mcp.InitializeResult
	if err := json.Unmarshal(initBytes, &initResult); err != nil {
		t.Fatalf("failed to decode InitializeResult: %v", err)
	}

	if initResult.ServerInfo.Name != "omnicoder-mcp" {
		t.Errorf("expected server name omnicoder-mcp, got %q", initResult.ServerInfo.Name)
	}
	if initResult.ServerInfo.Version != "1.0.0" {
		t.Errorf("expected server version 1.0.0, got %q", initResult.ServerInfo.Version)
	}
	if initResult.ProtocolVersion != "2024-11-05" {
		t.Errorf("expected protocol version 2024-11-05, got %q", initResult.ProtocolVersion)
	}
	if initResult.Capabilities.Tools == nil {
		t.Fatalf("expected tools capability in initialize result")
	}

	// Step 2: Send "notifications/initialized"
	client.sendNotification("notifications/initialized", nil)
	time.Sleep(25 * time.Millisecond)

	// Step 3: Send "ping" to verify connection remains active and receptive
	pingResp := client.sendRequest("ping", nil)
	if pingResp.Error != nil {
		t.Fatalf("ping request failed: %v", pingResp.Error)
	}
}

// ----------------------------------------------------------------------------
// 2. Tools List & Schema Validation Test
// ----------------------------------------------------------------------------

func TestMCP_ToolsList_ValidJSONSchemas(t *testing.T) {
	tmpDir := t.TempDir()
	client, cancel, errCh := setupE2EServer(t, tmpDir)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	// Handshake
	_ = client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]any{
			"name":    "cline",
			"version": "3.2.0",
		},
	})
	client.sendNotification("notifications/initialized", nil)

	// Send "tools/list"
	listResp := client.sendRequest("tools/list", nil)
	if listResp.Error != nil {
		t.Fatalf("tools/list returned error: %v", listResp.Error)
	}

	listBytes, _ := json.Marshal(listResp.Result)
	var listResult mcp.ToolListResult
	if err := json.Unmarshal(listBytes, &listResult); err != nil {
		t.Fatalf("failed to parse tools/list result: %v", err)
	}

	if len(listResult.Tools) < 14 {
		t.Fatalf("expected at least 14 tools registered, got %d", len(listResult.Tools))
	}

	expectedCanonicalTools := []string{
		"omnicoder_status",
		"omnicoder_map",
		"omnicoder_search",
		"omnicoder_patch",
		"omnicoder_heal",
		"omnicoder_refactor",
		"omnicoder_run",
		"omnicoder_sandbox",
		"git_status",
		"git_diff",
		"git_commit",
		"read_file",
		"write_file",
		"list_directory",
	}

	toolMap := make(map[string]mcp.ToolDefinition)
	for _, tool := range listResult.Tools {
		toolMap[tool.Name] = tool
	}

	for _, name := range expectedCanonicalTools {
		tool, exists := toolMap[name]
		if !exists {
			t.Errorf("missing expected canonical tool %q", name)
			continue
		}

		t.Run(name, func(t *testing.T) {
			if strings.TrimSpace(tool.Description) == "" {
				t.Errorf("tool %s description is empty", name)
			}
			schema := tool.InputSchema
			if schema == nil {
				t.Fatalf("tool %s inputSchema is nil", name)
			}

			// Validate JSON Schema structure
			schemaType, ok := schema["type"].(string)
			if !ok || schemaType != "object" {
				t.Errorf("tool %s inputSchema type must be 'object', got %v", name, schema["type"])
			}

			props, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Errorf("tool %s inputSchema properties must be an object map", name)
			}

			// Verify required fields match properties
			if reqList, hasReq := schema["required"].([]any); hasReq {
				for _, r := range reqList {
					rStr, isStr := r.(string)
					if !isStr {
						t.Errorf("tool %s required entry is not string: %v", name, r)
						continue
					}
					if _, hasProp := props[rStr]; !hasProp {
						t.Errorf("tool %s requires property %q which is not declared in properties", name, rStr)
					}
				}
			}

			// Verify schema marshals to valid JSON
			jsonBytes, err := json.Marshal(schema)
			if err != nil {
				t.Errorf("tool %s schema failed JSON marshaling: %v", name, err)
			}
			var checkMap map[string]any
			if err := json.Unmarshal(jsonBytes, &checkMap); err != nil {
				t.Errorf("tool %s schema re-parsing failed: %v", name, err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 3. Core Tools/Call Integration Suite
// ----------------------------------------------------------------------------

func TestMCP_ToolsCall_CoreSuite(t *testing.T) {
	tmpDir := t.TempDir()

	// Seed workspace with a multi-function Go file
	sampleGo := filepath.Join(tmpDir, "calculator.go")
	initialCode := `package calc

import "fmt"

// CalculateSum adds two integers and formats the result.
func CalculateSum(a, b int) string {
	return fmt.Sprintf("Sum: %d", a+b)
}

// MultiplyInts multiplies two integers.
func MultiplyInts(a, b int) int {
	return a * b
}
`
	if err := os.WriteFile(sampleGo, []byte(initialCode), 0644); err != nil {
		t.Fatalf("failed to seed workspace: %v", err)
	}

	client, cancel, errCh := setupE2EServer(t, tmpDir)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-errCh
	}()

	// Handshake
	_ = client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
	})
	client.sendNotification("notifications/initialized", nil)

	// ---------------------------------------------------------
	// 3a. omnicoder_status
	// ---------------------------------------------------------
	t.Run("omnicoder_status", func(t *testing.T) {
		resp := client.sendRequest("tools/call", map[string]any{
			"name":      "omnicoder_status",
			"arguments": map[string]any{},
		})

		if resp.Error != nil {
			t.Fatalf("omnicoder_status returned error: %v", resp.Error)
		}

		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)

		if callRes.IsError {
			t.Fatalf("omnicoder_status returned isError=true")
		}
		if len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_status returned empty content")
		}

		text := callRes.Content[0].Text
		if !strings.Contains(text, "READY") {
			t.Errorf("status response missing 'READY': %s", text)
		}
		if !strings.Contains(text, "workspace_root") {
			t.Errorf("status response missing 'workspace_root': %s", text)
		}
		if !strings.Contains(text, "tools_count") {
			t.Errorf("status response missing 'tools_count': %s", text)
		}
	})

	// ---------------------------------------------------------
	// 3b. omnicoder_map
	// ---------------------------------------------------------
	t.Run("omnicoder_map", func(t *testing.T) {
		resp := client.sendRequest("tools/call", map[string]any{
			"name": "omnicoder_map",
			"arguments": map[string]any{
				"token_budget": 1024,
				"max_files":    50,
			},
		})

		if resp.Error != nil {
			t.Fatalf("omnicoder_map returned error: %v", resp.Error)
		}

		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)

		if callRes.IsError {
			t.Fatalf("omnicoder_map returned isError=true")
		}
		if len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_map returned empty content")
		}

		text := callRes.Content[0].Text
		if !strings.Contains(text, "calculator.go") {
			t.Errorf("expected omnicoder_map to outline calculator.go, got: %s", text)
		}
	})

	// ---------------------------------------------------------
	// 3c. omnicoder_search
	// ---------------------------------------------------------
	t.Run("omnicoder_search", func(t *testing.T) {
		resp := client.sendRequest("tools/call", map[string]any{
			"name": "omnicoder_search",
			"arguments": map[string]any{
				"query":       "CalculateSum",
				"max_results": 5,
			},
		})

		if resp.Error != nil {
			t.Fatalf("omnicoder_search returned error: %v", resp.Error)
		}

		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)

		if callRes.IsError {
			t.Fatalf("omnicoder_search returned isError=true")
		}
		if len(callRes.Content) == 0 {
			t.Fatalf("omnicoder_search returned empty content")
		}

		text := callRes.Content[0].Text
		if !strings.Contains(text, "CalculateSum") || !strings.Contains(text, "calculator.go") {
			t.Errorf("expected omnicoder_search to find CalculateSum in calculator.go, got: %s", text)
		}
	})

	// ---------------------------------------------------------
	// 3d. omnicoder_patch
	// ---------------------------------------------------------
	t.Run("omnicoder_patch_success", func(t *testing.T) {
		patchContent := `<<<<<<< SEARCH
func CalculateSum(a, b int) string {
	return fmt.Sprintf("Sum: %d", a+b)
}
=======
func CalculateSum(a, b int) string {
	return fmt.Sprintf("Total Result: %d", a+b)
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
			t.Fatalf("omnicoder_patch returned error: %v", resp.Error)
		}

		resBytes, _ := json.Marshal(resp.Result)
		var callRes mcp.CallToolResult
		_ = json.Unmarshal(resBytes, &callRes)

		if callRes.IsError {
			t.Fatalf("omnicoder_patch returned isError=true: %v", callRes.Content)
		}

		text := callRes.Content[0].Text
		if !strings.Contains(text, "PATCHED") {
			t.Errorf("expected patch status PATCHED, got: %s", text)
		}

		// Verify on-disk file content
		diskContent, err := os.ReadFile(sampleGo)
		if err != nil {
			t.Fatalf("failed to read patched file: %v", err)
		}
		if !strings.Contains(string(diskContent), `fmt.Sprintf("Total Result: %d", a+b)`) {
			t.Errorf("file did not contain patched code, got:\n%s", string(diskContent))
		}
		if strings.Contains(string(diskContent), `fmt.Sprintf("Sum: %d", a+b)`) {
			t.Errorf("file still contains old code after patch:\n%s", string(diskContent))
		}
	})

	t.Run("omnicoder_patch_invalid_block_rejected", func(t *testing.T) {
		badPatch := `<<<<<<< SEARCH
func NonExistentFunction() {
}
=======
func NonExistentFunction() {
	println("hello")
}
>>>>>>> REPLACE`

		resp := client.sendRequest("tools/call", map[string]any{
			"name": "omnicoder_patch",
			"arguments": map[string]any{
				"path":    "calculator.go",
				"content": badPatch,
			},
		})

		// Tool handler should return error cleanly either via JSON-RPC error or CallToolResult.IsError
		if resp.Error == nil {
			resBytes, _ := json.Marshal(resp.Result)
			var callRes mcp.CallToolResult
			_ = json.Unmarshal(resBytes, &callRes)
			if !callRes.IsError {
				t.Fatalf("expected patch rejection for non-existent search block, got success: %v", callRes)
			}
		}
	})
}

// ----------------------------------------------------------------------------
// 4. Offline Auto-Detection Fallback Test
// ----------------------------------------------------------------------------

func TestMCP_OfflineAutoDetectionFallback(t *testing.T) {
	// Clear cloud environment variables for isolated test
	origGemini := os.Getenv("GEMINI_API_KEY")
	origOpenAI := os.Getenv("OPENAI_API_KEY")
	origAnthropic := os.Getenv("ANTHROPIC_API_KEY")
	origOmni := os.Getenv("OMNICODER_API_KEY")
	defer func() {
		_ = os.Setenv("GEMINI_API_KEY", origGemini)
		_ = os.Setenv("OPENAI_API_KEY", origOpenAI)
		_ = os.Setenv("ANTHROPIC_API_KEY", origAnthropic)
		_ = os.Setenv("OMNICODER_API_KEY", origOmni)
	}()

	_ = os.Unsetenv("GEMINI_API_KEY")
	_ = os.Unsetenv("OPENAI_API_KEY")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Unsetenv("OMNICODER_API_KEY")

	tmpDir := t.TempDir()

	// Probing dead ports with 20ms timeout ensures instant fallback to Offline Mode
	det := mcp.AutoDetectEnvironmentWithOptions(mcp.AutoDetectOptions{
		BaseDir:      tmpDir,
		EnvFile:      filepath.Join(tmpDir, "nonexistent.env"),
		OllamaURL:    "http://127.0.0.1:59999",
		LMStudioURL:  "http://127.0.0.1:59998",
		VLLMURL:      "http://127.0.0.1:59997",
		LocalAIURL:   "http://127.0.0.1:59996",
		ProbeTimeout: 20 * time.Millisecond,
	})

	if !det.IsOffline {
		t.Fatalf("expected IsOffline to be true when no keys or daemons are reachable")
	}
	if det.Backend != mcp.BackendOffline && det.Backend != mcp.BackendOfflineLocal {
		t.Errorf("expected offline backend %q or %q, got %q", mcp.BackendOffline, mcp.BackendOfflineLocal, det.Backend)
	}
	if det.Model != "embedded-local-cache" {
		t.Errorf("expected embedded-local-cache model, got %q", det.Model)
	}
	if det.APIKey != "" {
		t.Errorf("expected empty APIKey in offline fallback, got %q", det.APIKey)
	}
	if det.BaseURL != "" {
		t.Errorf("expected empty BaseURL in offline fallback, got %q", det.BaseURL)
	}

	// Spin up server with this detected offline configuration
	clientR, clientW := io.Pipe()
	serverR, serverW := io.Pipe()

	server := mcp.NewServer(tmpDir)
	sCtx := &mcp.OmniCoderServerContext{
		Config:    det,
		Lattice:   icx.NewLocalLattice(),
		VMClient:  vm.NewHermeticLocalSandbox(tmpDir),
		SandboxOn: true,
	}
	mcp.RegisterOmniCoderTools(server, sCtx)

	ctx, cancel := context.WithCancel(context.Background())
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.Serve(ctx, clientR, serverW)
	}()

	client := newTestClient(t, clientW, serverR)
	defer func() {
		_ = client.writer.Close()
		cancel()
		<-serverErrCh
	}()

	// Handshake
	_ = client.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
	})
	client.sendNotification("notifications/initialized", nil)

	// Call omnicoder_status
	statusResp := client.sendRequest("tools/call", map[string]any{
		"name":      "omnicoder_status",
		"arguments": map[string]any{},
	})

	if statusResp.Error != nil {
		t.Fatalf("omnicoder_status failed in offline mode: %v", statusResp.Error)
	}

	var callRes mcp.CallToolResult
	rawBytes, _ := json.Marshal(statusResp.Result)
	_ = json.Unmarshal(rawBytes, &callRes)

	if callRes.IsError {
		t.Fatalf("omnicoder_status reported error in offline mode")
	}

	content := callRes.Content[0].Text
	if !strings.Contains(content, `"is_offline": true`) && !strings.Contains(content, `"is_offline":true`) {
		t.Errorf("expected omnicoder_status to report is_offline: true, got: %s", content)
	}
	if !strings.Contains(content, "offline-local-cache") {
		t.Errorf("expected omnicoder_status to report offline-local-cache backend, got: %s", content)
	}
}

// ----------------------------------------------------------------------------
// 5. Mock Cline & Antigravity IDE Integration Test
// ----------------------------------------------------------------------------

func TestMCP_MockClientIntegration_ClineAndAntigravity(t *testing.T) {
	scenarios := []struct {
		clientName    string
		clientVersion string
	}{
		{clientName: "cline", clientVersion: "3.2.0"},
		{clientName: "antigravity-ide", clientVersion: "1.0.0"},
	}

	for _, sc := range scenarios {
		t.Run(sc.clientName, func(t *testing.T) {
			tmpDir := t.TempDir()

			// Create a target file in workspace
			targetFile := filepath.Join(tmpDir, "service.go")
			_ = os.WriteFile(targetFile, []byte(`package service

func HandleRequest(id string) string {
	return "status: pending"
}
`), 0644)

			client, cancel, errCh := setupE2EServer(t, tmpDir)
			defer func() {
				_ = client.writer.Close()
				cancel()
				<-errCh
			}()

			// Step 1: Handshake
			initResp := client.sendRequest("initialize", map[string]any{
				"protocolVersion": "2024-11-05",
				"clientInfo": map[string]any{
					"name":    sc.clientName,
					"version": sc.clientVersion,
				},
			})
			if initResp.Error != nil {
				t.Fatalf("[%s] initialize failed: %v", sc.clientName, initResp.Error)
			}
			client.sendNotification("notifications/initialized", nil)

			// Step 2: Discovery (tools/list)
			listResp := client.sendRequest("tools/list", nil)
			if listResp.Error != nil {
				t.Fatalf("[%s] tools/list failed: %v", sc.clientName, listResp.Error)
			}

			// Step 3: IDE inspects environment
			statusResp := client.sendRequest("tools/call", map[string]any{
				"name":      "omnicoder_status",
				"arguments": map[string]any{},
			})
			if statusResp.Error != nil {
				t.Fatalf("[%s] omnicoder_status failed: %v", sc.clientName, statusResp.Error)
			}

			// Step 4: IDE searches for HandleRequest
			searchResp := client.sendRequest("tools/call", map[string]any{
				"name": "omnicoder_search",
				"arguments": map[string]any{
					"query": "HandleRequest",
				},
			})
			if searchResp.Error != nil {
				t.Fatalf("[%s] omnicoder_search failed: %v", sc.clientName, searchResp.Error)
			}

			// Step 5: IDE applies patch
			patchResp := client.sendRequest("tools/call", map[string]any{
				"name": "omnicoder_patch",
				"arguments": map[string]any{
					"path": "service.go",
					"content": `<<<<<<< SEARCH
func HandleRequest(id string) string {
	return "status: pending"
}
=======
func HandleRequest(id string) string {
	return "status: completed"
}
>>>>>>> REPLACE`,
				},
			})
			if patchResp.Error != nil {
				t.Fatalf("[%s] omnicoder_patch failed: %v", sc.clientName, patchResp.Error)
			}

			// Step 6: IDE verifies using read_file tool
			readResp := client.sendRequest("tools/call", map[string]any{
				"name": "read_file",
				"arguments": map[string]any{
					"path": "service.go",
				},
			})
			if readResp.Error != nil {
				t.Fatalf("[%s] read_file failed: %v", sc.clientName, readResp.Error)
			}

			readBytes, _ := json.Marshal(readResp.Result)
			var callRes mcp.CallToolResult
			_ = json.Unmarshal(readBytes, &callRes)
			if len(callRes.Content) == 0 || !strings.Contains(callRes.Content[0].Text, "status: completed") {
				t.Fatalf("[%s] read_file did not show patched code: %v", sc.clientName, callRes.Content)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 6. Direct Binary CLI Execution Test (bin/omnicoder.exe)
// ----------------------------------------------------------------------------

func TestOmniCoderCLI_BinaryExecution(t *testing.T) {
	// Find project root (containing go.mod)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	projectRoot := wd
	for i := 0; i < 5; i++ {
		if _, statErr := os.Stat(filepath.Join(projectRoot, "go.mod")); statErr == nil {
			break
		}
		projectRoot = filepath.Dir(projectRoot)
	}

	binName := "omnicoder"
	if runtime.GOOS == "windows" {
		binName = "omnicoder.exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)

	// Always compile a fresh binary to ensure active code parity
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/omnicoder")
	buildCmd.Dir = projectRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build %s: %v\nOutput:\n%s", binPath, err, string(out))
	}

	// 6a. Test "omnicoder version"
	verCmd := exec.Command(binPath, "version")
	verOut, err := verCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to execute 'omnicoder version': %v\nOutput: %s", err, string(verOut))
	}
	if !strings.Contains(string(verOut), "OmniCoder-ICX v1.0.0") {
		t.Errorf("unexpected version output: %s", string(verOut))
	}

	// 6b. Test "omnicoder help"
	helpCmd := exec.Command(binPath, "help")
	helpOut, err := helpCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to execute 'omnicoder help': %v\nOutput: %s", err, string(helpOut))
	}
	if !strings.Contains(string(helpOut), "mcp [options]") {
		t.Errorf("expected help output to document 'mcp' command: %s", string(helpOut))
	}

	// 6c. Test "omnicoder mcp" stdio communication
	mcpCmd := exec.Command(binPath, "mcp", "--quiet")
	mcpCmd.Dir = t.TempDir()

	stdin, err := mcpCmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to get stdin pipe: %v", err)
	}
	stdout, err := mcpCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to get stdout pipe: %v", err)
	}

	if err := mcpCmd.Start(); err != nil {
		t.Fatalf("failed to start 'omnicoder mcp': %v", err)
	}

	cliClient := newTestClient(t, stdin, stdout)

	// Handshake over real CLI process stdio pipe
	initResp := cliClient.sendRequest("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
	})
	if initResp.Error != nil {
		t.Fatalf("CLI stdio initialize failed: %v", initResp.Error)
	}

	cliClient.sendNotification("notifications/initialized", nil)

	// List tools over real CLI process stdio pipe
	listResp := cliClient.sendRequest("tools/list", nil)
	if listResp.Error != nil {
		t.Fatalf("CLI stdio tools/list failed: %v", listResp.Error)
	}

	listBytes, _ := json.Marshal(listResp.Result)
	var listRes mcp.ToolListResult
	_ = json.Unmarshal(listBytes, &listRes)

	if len(listRes.Tools) < 14 {
		t.Errorf("expected at least 14 tools from CLI process, got %d", len(listRes.Tools))
	}

	// Close stdin to trigger graceful server shutdown
	_ = stdin.Close()

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- mcpCmd.Wait()
	}()

	select {
	case waitErr := <-doneCh:
		if waitErr != nil {
			t.Logf("CLI exited with: %v (expected on stdin EOF)", waitErr)
		}
	case <-time.After(3 * time.Second):
		_ = mcpCmd.Process.Kill()
		t.Fatalf("CLI process did not terminate within timeout upon stdin EOF")
	}
}
