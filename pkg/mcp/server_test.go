// SPDX-License-Identifier: Apache-2.0
// Package mcp_test implements comprehensive unit, security, and concurrency tests
// for the OmniCoder-ICX Model Context Protocol (MCP) server.

package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

// ----------------------------------------------------------------------------
// Test Harness: In-Memory Bidirectional Pipe
// ----------------------------------------------------------------------------

type PipeHarness struct {
	t          *testing.T
	Server     *mcp.Server
	ClientIn   *bufio.Reader
	ClientOut  *io.PipeWriter
	ServerIn   *io.PipeReader
	ServerOut  *io.PipeWriter
	ServerDone chan error
	Ctx        context.Context
	Cancel     context.CancelFunc
	WorkDir    string
	writeMu    sync.Mutex
	respCh     chan *mcp.JSONRPCResponse
	errCh      chan error
}

func NewPipeHarness(t *testing.T, workDir string) *PipeHarness {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	srv := mcp.NewServer(workDir)

	h := &PipeHarness{
		t:          t,
		Server:     srv,
		ClientIn:   bufio.NewReader(clientIn),
		ClientOut:  clientOut,
		ServerIn:   serverIn,
		ServerOut:  serverOut,
		ServerDone: make(chan error, 1),
		Ctx:        ctx,
		Cancel:     cancel,
		WorkDir:    workDir,
		respCh:     make(chan *mcp.JSONRPCResponse, 100),
		errCh:      make(chan error, 10),
	}

	go func() {
		h.ServerDone <- srv.Serve(ctx, serverIn, serverOut)
	}()

	// Single persistent reader loop to avoid goroutine leaks and data races
	go func() {
		for {
			line, err := h.ClientIn.ReadBytes('\n')
			if err != nil {
				h.errCh <- err
				return
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}

			// Content-Length header framing
			if bytes.HasPrefix(bytes.ToLower(trimmed), []byte("content-length:")) {
				parts := bytes.SplitN(trimmed, []byte(":"), 2)
				lengthStr := strings.TrimSpace(string(parts[1]))
				contentLen, err := strconv.Atoi(lengthStr)
				if err != nil {
					h.errCh <- fmt.Errorf("invalid Content-Length: %w", err)
					return
				}
				for {
					headerLine, err := h.ClientIn.ReadBytes('\n')
					if err != nil {
						h.errCh <- err
						return
					}
					if len(bytes.TrimSpace(headerLine)) == 0 {
						break
					}
				}
				body := make([]byte, contentLen)
				if _, err := io.ReadFull(h.ClientIn, body); err != nil {
					h.errCh <- fmt.Errorf("failed reading framed body: %w", err)
					return
				}
				var resp mcp.JSONRPCResponse
				if err := json.Unmarshal(body, &resp); err != nil {
					h.errCh <- fmt.Errorf("unmarshal framed response failed: %w (body: %s)", err, string(body))
					continue
				}
				h.respCh <- &resp
				continue
			}

			// Newline-delimited JSON
			var resp mcp.JSONRPCResponse
			if err := json.Unmarshal(trimmed, &resp); err != nil {
				h.errCh <- fmt.Errorf("unmarshal response failed: %w (raw: %s)", err, string(trimmed))
				continue
			}
			h.respCh <- &resp
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = clientOut.Close()
		_ = serverOut.Close()
		_ = serverIn.Close()
		_ = clientIn.Close()
		select {
		case <-h.ServerDone:
		case <-time.After(1 * time.Second):
		}
	})

	return h
}

func (h *PipeHarness) SendRaw(data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err := h.ClientOut.Write(append(data, '\n'))
	return err
}

func (h *PipeHarness) SendFramed(body string) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	_, err := h.ClientOut.Write([]byte(msg))
	return err
}

func (h *PipeHarness) ReadResponseWithTimeout(timeout time.Duration) (*mcp.JSONRPCResponse, error) {
	select {
	case resp := <-h.respCh:
		return resp, nil
	case err := <-h.errCh:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out waiting for response after %v", timeout)
	}
}

func (h *PipeHarness) ReadResponse() (*mcp.JSONRPCResponse, error) {
	return h.ReadResponseWithTimeout(5 * time.Second)
}

func (h *PipeHarness) Call(id any, method string, params any) (*mcp.JSONRPCResponse, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}
	req := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := h.SendRaw(data); err != nil {
		return nil, err
	}
	return h.ReadResponse()
}

// ----------------------------------------------------------------------------
// Tier 1: Protocol & Lifecycle Tests
// ----------------------------------------------------------------------------

func TestMCPServer_InitializeHandshake(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	resp, err := h.Call(1, "initialize", mcp.InitializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo: mcp.ClientInfo{
			Name:    "test-client",
			Version: "1.0.0",
		},
	})
	if err != nil {
		t.Fatalf("initialize call failed: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	var initResult mcp.InitializeResult
	if err := json.Unmarshal(resultBytes, &initResult); err != nil {
		t.Fatalf("failed unmarshaling InitializeResult: %v", err)
	}

	if initResult.ProtocolVersion != "2024-11-05" {
		t.Errorf("expected protocolVersion '2024-11-05', got '%s'", initResult.ProtocolVersion)
	}
	if initResult.ServerInfo.Name != mcp.DefaultServerName {
		t.Errorf("expected server name '%s', got '%s'", mcp.DefaultServerName, initResult.ServerInfo.Name)
	}
	if initResult.Capabilities.Tools == nil || !initResult.Capabilities.Tools.ListChanged {
		t.Errorf("expected tools capability with ListChanged=true")
	}
}

func TestMCPServer_NotificationsInitialized(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	// Send notification (id is omitted / nil)
	notification := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	data, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("failed marshaling notification: %v", err)
	}
	if err := h.SendRaw(data); err != nil {
		t.Fatalf("failed sending notification: %v", err)
	}

	// Verify server does NOT reply (Section 4.1 compliant)
	resp, err := h.ReadResponseWithTimeout(150 * time.Millisecond)
	if err == nil && resp != nil {
		t.Fatalf("server MUST NOT reply to notifications, but received: %+v", resp)
	}
}

func TestMCPServer_InitializeWithWorkspaceFolders(t *testing.T) {
	initialDir := t.TempDir()
	targetProjectDir := t.TempDir()

	h := NewPipeHarness(t, initialDir)

	// Initialize with workspaceFolders
	targetURI := "file://" + filepath.ToSlash(targetProjectDir)
	resp, err := h.Call(10, "initialize", mcp.InitializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo: mcp.ClientInfo{
			Name:    "cline-agent",
			Version: "3.2.0",
		},
		WorkspaceFolders: []mcp.WorkspaceFolder{
			{
				URI:  targetURI,
				Name: "target-project",
			},
		},
	})
	if err != nil {
		t.Fatalf("initialize call failed: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected initialize error: %v", resp.Error)
	}

	// Server workspace should now be targetProjectDir
	if h.Server.WorkspaceDir() != targetProjectDir {
		t.Errorf("expected server workspace to adopt workspaceFolders %s, got %s", targetProjectDir, h.Server.WorkspaceDir())
	}
}

func TestMCPServer_RootsList(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	resp, err := h.Call(20, "roots/list", nil)
	if err != nil {
		t.Fatalf("roots/list call failed: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("roots/list error: %v", resp.Error)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	var rootsRes mcp.RootsListResult
	if err := json.Unmarshal(resultBytes, &rootsRes); err != nil {
		t.Fatalf("failed unmarshaling roots/list result: %v", err)
	}

	if len(rootsRes.Roots) == 0 {
		t.Fatalf("expected at least 1 root, got 0")
	}
	if rootsRes.Roots[0].Name != filepath.Base(workDir) {
		t.Errorf("expected root name %s, got %s", filepath.Base(workDir), rootsRes.Roots[0].Name)
	}
}

func TestMCPServer_DidChangeWorkspaceFolders(t *testing.T) {
	initialDir := t.TempDir()
	newProjectDir := t.TempDir()
	h := NewPipeHarness(t, initialDir)

	newURI := "file://" + filepath.ToSlash(newProjectDir)
	notifParams := map[string]any{
		"event": map[string]any{
			"added": []map[string]any{
				{
					"uri":  newURI,
					"name": "new-project",
				},
			},
		},
	}
	paramBytes, _ := json.Marshal(notifParams)
	notif := mcp.JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  "workspace/didChangeWorkspaceFolders",
		Params:  paramBytes,
	}
	data, _ := json.Marshal(notif)
	if err := h.SendRaw(data); err != nil {
		t.Fatalf("failed to send notification: %v", err)
	}

	// Give a few ms for goroutine to process notification
	time.Sleep(50 * time.Millisecond)

	if h.Server.WorkspaceDir() != newProjectDir {
		t.Errorf("expected server workspace to update to %s, got %s", newProjectDir, h.Server.WorkspaceDir())
	}
}

func TestMCPServer_Ping(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	resp, err := h.Call(2, "ping", nil)
	if err != nil {
		t.Fatalf("ping call failed: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected ping error: %v", resp.Error)
	}
}

func TestMCPServer_ToolsList(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	resp, err := h.Call(3, "tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list call failed: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected tools/list error: %v", resp.Error)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	var listResult mcp.ToolListResult
	if err := json.Unmarshal(resultBytes, &listResult); err != nil {
		t.Fatalf("failed unmarshaling ToolListResult: %v", err)
	}

	expectedTools := map[string]bool{
		"read_file":      false,
		"write_file":     false,
		"list_directory": false,
		"git_status":     false,
		"git_diff":       false,
		"git_commit":     false,
	}

	for _, tool := range listResult.Tools {
		if _, ok := expectedTools[tool.Name]; ok {
			expectedTools[tool.Name] = true
		}
		if tool.Description == "" {
			t.Errorf("tool '%s' missing description", tool.Name)
		}
		if tool.InputSchema == nil || tool.InputSchema["type"] != "object" {
			t.Errorf("tool '%s' missing valid Draft 2020-12 inputSchema", tool.Name)
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected tool '%s' was not registered", name)
		}
	}
}

// ----------------------------------------------------------------------------
// Tier 2: Error Taxonomy & Dual Framing Tests
// ----------------------------------------------------------------------------

func TestJSONRPC_ParseError(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	// Send malformed JSON
	if err := h.SendRaw([]byte("{invalid-json-payload")); err != nil {
		t.Fatalf("failed sending malformed json: %v", err)
	}

	resp, err := h.ReadResponse()
	if err != nil {
		t.Fatalf("failed reading parse error response: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error object in response")
	}
	if resp.Error.Code != mcp.ErrCodeParseError {
		t.Errorf("expected error code %d, got %d", mcp.ErrCodeParseError, resp.Error.Code)
	}
	if resp.ID != nil {
		t.Errorf("expected null ID on parse error, got %v", resp.ID)
	}
}

func TestJSONRPC_InvalidRequest(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	// Send invalid jsonrpc version
	raw := `{"jsonrpc": "1.0", "id": 10, "method": "ping"}`
	if err := h.SendRaw([]byte(raw)); err != nil {
		t.Fatalf("failed sending invalid request: %v", err)
	}

	resp, err := h.ReadResponse()
	if err != nil {
		t.Fatalf("failed reading response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != mcp.ErrCodeInvalidRequest {
		t.Errorf("expected error code %d, got %+v", mcp.ErrCodeInvalidRequest, resp.Error)
	}
}

func TestJSONRPC_MethodNotFound(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	resp, err := h.Call(11, "nonexistent_method", nil)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != mcp.ErrCodeMethodNotFound {
		t.Errorf("expected error code %d, got %+v", mcp.ErrCodeMethodNotFound, resp.Error)
	}
}

func TestJSONRPC_SchemaValidationFailure(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	// read_file requires 'path'
	resp, err := h.Call(12, "tools/call", map[string]any{
		"name":      "read_file",
		"arguments": map[string]any{}, // missing required "path"
	})
	if err != nil {
		t.Fatalf("tools/call failed: %v", err)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	var callResult mcp.CallToolResult
	if err := json.Unmarshal(resultBytes, &callResult); err != nil {
		t.Fatalf("failed unmarshaling CallToolResult: %v", err)
	}

	if !callResult.IsError {
		t.Errorf("expected IsError=true on schema validation violation")
	}
	if len(callResult.Content) == 0 || !strings.Contains(callResult.Content[0].Text, "required") {
		t.Errorf("expected schema error mentioning required property, got: %+v", callResult)
	}
}

func TestFraming_ContentLengthAndNewline(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	// 1. Content-Length framed message
	framedMsg := `{"jsonrpc":"2.0","id":20,"method":"ping"}`
	if err := h.SendFramed(framedMsg); err != nil {
		t.Fatalf("SendFramed failed: %v", err)
	}
	resp1, err := h.ReadResponse()
	if err != nil {
		t.Fatalf("ReadResponse failed for framed message: %v", err)
	}
	if resp1.Error != nil {
		t.Errorf("unexpected error on framed ping: %v", resp1.Error)
	}

	// 2. Newline-delimited JSON message
	ndjsonMsg := `{"jsonrpc":"2.0","id":21,"method":"ping"}`
	if err := h.SendRaw([]byte(ndjsonMsg)); err != nil {
		t.Fatalf("SendRaw failed: %v", err)
	}
	resp2, err := h.ReadResponse()
	if err != nil {
		t.Fatalf("ReadResponse failed for NDJSON message: %v", err)
	}
	if resp2.Error != nil {
		t.Errorf("unexpected error on ndjson ping: %v", resp2.Error)
	}
}

// ----------------------------------------------------------------------------
// Tier 3: Filesystem Tools Functional & Security Tests
// ----------------------------------------------------------------------------

func TestFSTools_ReadFile(t *testing.T) {
	workDir := t.TempDir()
	testFile := filepath.Join(workDir, "sample.txt")
	content := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		t.Fatalf("failed writing test file: %v", err)
	}

	ctx := context.Background()

	// 1. Read entire file
	res, err := mcp.HandleReadFile(ctx, workDir, map[string]any{
		"path": "sample.txt",
	})
	if err != nil {
		t.Fatalf("HandleReadFile failed: %v", err)
	}
	if res.LineCount != 6 || res.TotalLines != 6 {
		t.Errorf("expected 6 lines, got %d", res.LineCount)
	}

	// 2. Read with offset and limit
	res2, err := mcp.HandleReadFile(ctx, workDir, map[string]any{
		"path":   "sample.txt",
		"offset": 2,
		"limit":  2,
	})
	if err != nil {
		t.Fatalf("HandleReadFile with offset/limit failed: %v", err)
	}
	expectedSub := "line 2\nline 3"
	if res2.Content != expectedSub {
		t.Errorf("expected '%s', got '%s'", expectedSub, res2.Content)
	}
	if res2.LineCount != 2 {
		t.Errorf("expected LineCount 2, got %d", res2.LineCount)
	}

	// 3. Offset beyond total lines
	res3, err := mcp.HandleReadFile(ctx, workDir, map[string]any{
		"path":   "sample.txt",
		"offset": 100,
	})
	if err != nil {
		t.Fatalf("HandleReadFile offset beyond EOF failed: %v", err)
	}
	if res3.Content != "" || res3.LineCount != 0 {
		t.Errorf("expected empty content when offset > totalLines")
	}

	// 4. Non-existent file
	_, err = mcp.HandleReadFile(ctx, workDir, map[string]any{
		"path": "does_not_exist.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Errorf("expected ErrFileNotFound, got: %v", err)
	}

	// 5. Read directory as file
	subDir := filepath.Join(workDir, "subdir")
	_ = os.MkdirAll(subDir, 0o755)
	_, err = mcp.HandleReadFile(ctx, workDir, map[string]any{
		"path": "subdir",
	})
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("expected directory error, got: %v", err)
	}
}

func TestFSTools_WriteFile(t *testing.T) {
	workDir := t.TempDir()
	ctx := context.Background()

	// 1. Write new file with auto parent directory creation
	relPath := "deep/nested/dir/test.txt"
	content := "hello world from omnicoder-icx mcp"
	res, err := mcp.HandleWriteFile(ctx, workDir, map[string]any{
		"path":      relPath,
		"content":   content,
		"overwrite": false,
	})
	if err != nil {
		t.Fatalf("HandleWriteFile failed: %v", err)
	}
	if res.BytesWritten != len(content) {
		t.Errorf("expected %d bytes written, got %d", len(content), res.BytesWritten)
	}
	expectedHash := canonical.SHA256Bytes([]byte(content))
	if res.SHA256 != expectedHash {
		t.Errorf("expected SHA256 %s, got %s", expectedHash, res.SHA256)
	}

	// Verify content on disk
	diskData, err := os.ReadFile(filepath.Join(workDir, relPath))
	if err != nil || string(diskData) != content {
		t.Errorf("disk content mismatch: %v", err)
	}

	// 2. Overwrite=false on existing file should fail
	_, err = mcp.HandleWriteFile(ctx, workDir, map[string]any{
		"path":      relPath,
		"content":   "new content",
		"overwrite": false,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected ErrFileExists, got: %v", err)
	}

	// 3. Overwrite=true should succeed
	newContent := "updated content atomically"
	resOverwrite, err := mcp.HandleWriteFile(ctx, workDir, map[string]any{
		"path":      relPath,
		"content":   newContent,
		"overwrite": true,
	})
	if err != nil {
		t.Fatalf("HandleWriteFile with overwrite=true failed: %v", err)
	}
	if resOverwrite.SHA256 != canonical.SHA256Bytes([]byte(newContent)) {
		t.Errorf("hash mismatch on overwrite")
	}
	diskUpdated, _ := os.ReadFile(filepath.Join(workDir, relPath))
	if string(diskUpdated) != newContent {
		t.Errorf("disk content not updated after overwrite")
	}
}

func TestFSTools_ListDirectory(t *testing.T) {
	workDir := t.TempDir()
	ctx := context.Background()

	// Create test structure:
	// a.txt, b.txt
	// sub/c.txt
	// .git/config (should be pruned)
	// node_modules/pkg.json (should be pruned)
	// vendor/v.go (should be pruned)
	// .agents/plan.md (should be pruned)
	_ = os.WriteFile(filepath.Join(workDir, "b.txt"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(workDir, "a.txt"), []byte("a"), 0o644)
	_ = os.MkdirAll(filepath.Join(workDir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(workDir, "sub", "c.txt"), []byte("c"), 0o644)

	_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(workDir, ".git", "config"), []byte("git"), 0o644)
	_ = os.MkdirAll(filepath.Join(workDir, "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(workDir, "node_modules", "package.json"), []byte("{}"), 0o644)
	_ = os.MkdirAll(filepath.Join(workDir, "vendor"), 0o755)
	_ = os.WriteFile(filepath.Join(workDir, "vendor", "mod.go"), []byte("go"), 0o644)
	_ = os.MkdirAll(filepath.Join(workDir, ".agents"), 0o755)
	_ = os.WriteFile(filepath.Join(workDir, ".agents", "plan.md"), []byte("# Plan"), 0o644)

	// 1. Flat listing
	entries, err := mcp.HandleListDirectory(ctx, workDir, map[string]any{
		"path":      ".",
		"recursive": false,
	})
	if err != nil {
		t.Fatalf("HandleListDirectory failed: %v", err)
	}

	for _, e := range entries {
		if e.Name == ".git" || e.Name == "node_modules" || e.Name == "vendor" || e.Name == ".agents" {
			t.Errorf("noisy directory '%s' was not pruned", e.Name)
		}
	}

	// 2. Recursive listing
	recEntries, err := mcp.HandleListDirectory(ctx, workDir, map[string]any{
		"path":      ".",
		"recursive": true,
		"max_depth": 3,
	})
	if err != nil {
		t.Fatalf("recursive HandleListDirectory failed: %v", err)
	}

	foundSubC := false
	for _, e := range recEntries {
		if e.Path == "sub/c.txt" {
			foundSubC = true
		}
		if strings.HasPrefix(e.Path, ".git") || strings.HasPrefix(e.Path, "node_modules") {
			t.Errorf("recursive noisy entry discovered: %s", e.Path)
		}
	}
	if !foundSubC {
		t.Errorf("expected 'sub/c.txt' in recursive listing")
	}

	// Verify deterministic sorting
	for i := 1; i < len(recEntries); i++ {
		if recEntries[i].Path < recEntries[i-1].Path {
			t.Errorf("entries are not sorted: %s before %s", recEntries[i-1].Path, recEntries[i].Path)
		}
	}
}

// ----------------------------------------------------------------------------
// Tier 4: Security & Path Traversal Verification
// ----------------------------------------------------------------------------

func TestSecurity_ResolveSecurePath_AttackTable(t *testing.T) {
	workDir := t.TempDir()

	attackPayloads := []struct {
		name string
		path string
	}{
		{"parent directory traversal", "../secret.txt"},
		{"multi-level parent traversal", "../../etc/passwd"},
		{"windows backslash traversal", `..\..\windows\win.ini`},
		{"mixed slash traversal", `..\/..//../etc/shadow`},
		{"linux absolute root", "/etc/passwd"},
		{"windows drive absolute", `C:\Windows\System32\cmd.exe`},
		{"unc double backslash", `\\attacker\share\payload`},
		{"unc double forward slash", `//attacker/share/payload`},
		{"poison null byte inline", "safe.txt\x00evil.txt"},
		{"poison null byte prefix", "\x00/../../etc/passwd"},
		{"url encoded dot dot slash", `%2e%2e%2fetc%2fpasswd`},
		{"url encoded mixed", `%2e%2e/secret.txt`},
		{"alternate data stream colon", "file.txt:evil.exe"},
		{"alternate data stream data", "test.txt::$DATA"},
		{"windows reserved device CON", "CON"},
		{"windows reserved device NUL", "NUL"},
		{"windows reserved device AUX with ext", "AUX.txt"},
		{"windows reserved device COM1", "COM1"},
		{"windows reserved device LPT1", "LPT1.dat"},
	}

	for _, tc := range attackPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mcp.ResolveSecurePath(workDir, tc.path)
			if err == nil {
				t.Errorf("SECURITY VULNERABILITY: attack payload '%s' (%s) was NOT rejected!", tc.name, tc.path)
			}
		})
	}
}

func TestSecurity_ToolsCall_RejectsTraversal(t *testing.T) {
	workDir := t.TempDir()
	h := NewPipeHarness(t, workDir)

	// Call read_file with path traversal over JSON-RPC
	resp, err := h.Call(30, "tools/call", map[string]any{
		"name": "read_file",
		"arguments": map[string]any{
			"path": "../../../outside.txt",
		},
	})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	resultBytes, _ := json.Marshal(resp.Result)
	var callResult mcp.CallToolResult
	if err := json.Unmarshal(resultBytes, &callResult); err != nil {
		t.Fatalf("failed unmarshaling CallToolResult: %v", err)
	}

	if !callResult.IsError {
		t.Errorf("expected IsError=true on path traversal attempt")
	}
}

// ----------------------------------------------------------------------------
// Tier 5: Git Tools Subprocess Tests
// ----------------------------------------------------------------------------

func TestGitTools_Workflow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available on host, skipping git tests")
	}

	repoDir := t.TempDir()
	if realDir, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = realDir
	}

	// Initialize isolated git repo
	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "checkout", "-B", "main")
	runGitCmd(t, repoDir, "config", "user.name", "OmniCoder Tester")
	runGitCmd(t, repoDir, "config", "user.email", "support@caleralabs.com")
	runGitCmd(t, repoDir, "config", "commit.gpgsign", "false")

	// Create initial commit
	readme := filepath.Join(repoDir, "README.md")
	_ = os.WriteFile(readme, []byte("# Baseline\n"), 0o644)
	runGitCmd(t, repoDir, "add", "README.md")
	runGitCmd(t, repoDir, "commit", "-m", "chore: baseline commit")

	ctx := context.Background()

	// 1. git_status on clean repo
	statusRes, err := mcp.HandleGitStatus(ctx, repoDir, map[string]any{})
	if err != nil {
		t.Fatalf("HandleGitStatus failed: %v", err)
	}
	if !statusRes.Clean {
		t.Errorf("expected clean working tree, got dirty: %+v", statusRes)
	}
	if statusRes.Branch != "main" {
		t.Errorf("expected branch 'main', got '%s'", statusRes.Branch)
	}

	// 2. Modify file and check git_diff
	_ = os.WriteFile(readme, []byte("# Baseline\nAdded modification\n"), 0o644)

	diffRes, err := mcp.HandleGitDiff(ctx, repoDir, map[string]any{"staged": false})
	if err != nil {
		t.Fatalf("HandleGitDiff failed: %v", err)
	}
	if diffRes.Empty || !strings.Contains(diffRes.Diff, "Added modification") {
		t.Errorf("expected diff to contain 'Added modification', got: %s", diffRes.Diff)
	}

	// 3. git_status reflects modified file
	statusDirty, err := mcp.HandleGitStatus(ctx, repoDir, map[string]any{})
	if err != nil {
		t.Fatalf("HandleGitStatus dirty failed: %v", err)
	}
	if statusDirty.Clean || len(statusDirty.Modified) == 0 {
		t.Errorf("expected dirty status with modified files, got: %+v", statusDirty)
	}

	// 4. git_commit commits modification
	commitRes, err := mcp.HandleGitCommit(ctx, repoDir, map[string]any{
		"message": "feat: updated readme",
		"files":   []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("HandleGitCommit failed: %v", err)
	}
	if len(commitRes.CommitHash) < 7 {
		t.Errorf("invalid commit hash: %s", commitRes.CommitHash)
	}
	if commitRes.Summary != "feat: updated readme" {
		t.Errorf("expected summary 'feat: updated readme', got '%s'", commitRes.Summary)
	}

	// 5. Commit again on clean repo should return ErrNothingToCommit
	_, err = mcp.HandleGitCommit(ctx, repoDir, map[string]any{
		"message": "redundant commit",
	})
	if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("expected ErrNothingToCommit, got: %v", err)
	}
}

func TestGitTools_NonGitDirectoryFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available on host, skipping git tests")
	}

	nonGitDir := t.TempDir()
	ctx := context.Background()

	_, err := mcp.HandleGitStatus(ctx, nonGitDir, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected ErrNotGitRepo, got: %v", err)
	}
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v (output: %s)", strings.Join(args, " "), err, string(out))
	}
}

// ----------------------------------------------------------------------------
// Tier 6: Concurrency Stress & Race Testing
// ----------------------------------------------------------------------------

func TestServer_ConcurrencyStress_30Goroutines(t *testing.T) {
	workDir := t.TempDir()
	srv := mcp.NewServer(workDir)

	// Seed 10 baseline files
	for i := 0; i < 10; i++ {
		p := filepath.Join(workDir, fmt.Sprintf("baseline_%d.txt", i))
		_ = os.WriteFile(p, []byte(fmt.Sprintf("initial content %d\n", i)), 0o644)
	}

	const numWorkers = 30
	const opsPerWorker = 20

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers*opsPerWorker)
	ctx := context.Background()

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for op := 0; op < opsPerWorker; op++ {
				action := (workerID + op) % 5
				switch action {
				case 0:
					// Concurrent read of baseline files
					fileIdx := (workerID + op) % 10
					res, err := srv.Dispatch(ctx, "read_file", map[string]any{
						"path": fmt.Sprintf("baseline_%d.txt", fileIdx),
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d read_file failed: %w", workerID, err)
						return
					}
					if res == nil {
						errCh <- fmt.Errorf("worker %d read_file nil result", workerID)
						return
					}

				case 1:
					// Concurrent write to per-worker distinct files
					fileName := fmt.Sprintf("worker_%d_op_%d.txt", workerID, op)
					content := fmt.Sprintf("worker %d op %d timestamp %d", workerID, op, time.Now().UnixNano())
					res, err := srv.Dispatch(ctx, "write_file", map[string]any{
						"path":      fileName,
						"content":   content,
						"overwrite": true,
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d write_file failed: %w", workerID, err)
						return
					}
					if res == nil {
						errCh <- fmt.Errorf("worker %d write_file nil result", workerID)
						return
					}

				case 2:
					// Concurrent directory listing
					res, err := srv.Dispatch(ctx, "list_directory", map[string]any{
						"path": ".",
					})
					if err != nil {
						errCh <- fmt.Errorf("worker %d list_directory failed: %w", workerID, err)
						return
					}
					if res == nil {
						errCh <- fmt.Errorf("worker %d list_directory nil result", workerID)
						return
					}

				case 3:
					// Concurrent tool definition inspection
					defs := srv.GetToolDefinitions()
					if len(defs) == 0 {
						errCh <- fmt.Errorf("worker %d empty tool definitions", workerID)
						return
					}

				case 4:
					// Concurrent path traversal probe (must return error, must not crash)
					res, err := srv.Dispatch(ctx, "read_file", map[string]any{
						"path": "../../../outside.txt",
					})
					if err == nil && res != nil {
						errCh <- fmt.Errorf("worker %d traversal was not rejected!", workerID)
						return
					}
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		t.Fatalf("encountered %d concurrency errors, first: %v", len(errs), errs[0])
	}
}

func TestExtractWorkspaceFromHandshake(t *testing.T) {
	testCases := []struct {
		name     string
		jsonRaw  string
		expected string
	}{
		{
			name:     "rootUri file scheme",
			jsonRaw:  `{"protocolVersion":"2024-11-05","rootUri":"file:///C:/Projects/WebApp"}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
		{
			name:     "workspaceFolders object list",
			jsonRaw:  `{"workspaceFolders":[{"uri":"file:///C:/Projects/WebApp","name":"WebApp"}]}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
		{
			name:     "workspaceFolders string list",
			jsonRaw:  `{"workspaceFolders":["file:///C:/Projects/WebApp"]}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
		{
			name:     "snake_case workspace_folders",
			jsonRaw:  `{"workspace_folders":[{"uri":"file:///C:/Projects/WebApp"}]}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
		{
			name:     "roots object list",
			jsonRaw:  `{"roots":[{"uri":"file:///C:/Projects/WebApp"}]}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
		{
			name:     "initializationOptions workspace",
			jsonRaw:  `{"initializationOptions":{"workspace":"C:\\Projects\\WebApp"}}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
		{
			name:     "rootPath plain string",
			jsonRaw:  `{"rootPath":"C:\\Projects\\WebApp"}`,
			expected: filepath.FromSlash("C:/Projects/WebApp"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcp.ExtractWorkspaceFromHandshake([]byte(tc.jsonRaw))
			if filepath.Clean(got) != filepath.Clean(tc.expected) {
				t.Errorf("ExtractWorkspaceFromHandshake(%s) = %q, expected %q", tc.name, got, tc.expected)
			}
		})
	}
}

func TestServer_InitializeAutoWorkspaceSync(t *testing.T) {
	tmpDir := t.TempDir()
	targetProject := filepath.Join(tmpDir, "MyActiveProject")
	_ = os.MkdirAll(targetProject, 0755)

	h := NewPipeHarness(t, tmpDir)

	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"workspaceFolders": []map[string]any{
			{"uri": "file:///" + filepath.ToSlash(targetProject), "name": "MyActiveProject"},
		},
	}

	resp, err := h.Call(1, "initialize", initParams)
	if err != nil {
		t.Fatalf("initialize call failed: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %v", resp.Error)
	}

	if filepath.Clean(h.Server.WorkspaceDir()) != filepath.Clean(targetProject) {
		t.Errorf("expected server workspace to auto-anchor to %s, got %s", targetProject, h.Server.WorkspaceDir())
	}
}


