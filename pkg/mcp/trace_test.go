// SPDX-License-Identifier: Apache-2.0
package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

func TestSanitizeArguments(t *testing.T) {
	longStr := strings.Repeat("A", 500)
	raw := map[string]any{
		"path":       "src/main.go",
		"password":   "super-secret-123",
		"api_key":    "AQ.ExampleDemoKey000000000000000000000000000000",
		"sk_token":   "sk-ant-api03-abcdefg",
		"big_buffer": longStr,
		"nested": map[string]any{
			"auth_token": "bearer-token-val",
			"code":       "func Hello() {}",
		},
		"slice": []any{
			"normal",
			"AQ.another-secret-token",
		},
	}

	sanitized := mcp.SanitizeArguments(raw)

	if sanitized["path"] != "src/main.go" {
		t.Errorf("expected path to be preserved, got %v", sanitized["path"])
	}
	if sanitized["password"] != "[REDACTED]" {
		t.Errorf("expected password to be redacted, got %v", sanitized["password"])
	}
	if sanitized["api_key"] != "[REDACTED]" {
		t.Errorf("expected api_key to be redacted, got %v", sanitized["api_key"])
	}
	if sanitized["sk_token"] != "[REDACTED]" {
		t.Errorf("expected sk_token to be redacted, got %v", sanitized["sk_token"])
	}

	bigVal, ok := sanitized["big_buffer"].(string)
	if !ok || !strings.Contains(bigVal, "[truncated 500 bytes total]") {
		t.Errorf("expected big_buffer to be truncated, got %v", sanitized["big_buffer"])
	}

	nested, ok := sanitized["nested"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested map, got %T", sanitized["nested"])
	}
	if nested["auth_token"] != "[REDACTED]" {
		t.Errorf("expected nested auth_token to be redacted, got %v", nested["auth_token"])
	}
	if nested["code"] != "func Hello() {}" {
		t.Errorf("expected nested code to be preserved, got %v", nested["code"])
	}

	slice, ok := sanitized["slice"].([]any)
	if !ok || len(slice) != 2 {
		t.Fatalf("expected slice of length 2, got %v", sanitized["slice"])
	}
	if slice[0] != "normal" {
		t.Errorf("expected slice[0] to be normal, got %v", slice[0])
	}
	if slice[1] != "[REDACTED]" {
		t.Errorf("expected slice[1] to be redacted, got %v", slice[1])
	}
}

func TestDefaultTracer_FileLogging(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "logs", "mcp_trace.jsonl")

	tracer := mcp.NewDefaultTracer(mcp.TracerConfig{
		FilePath: logPath,
		Verbose:  false,
		Level:    mcp.LogLevelInfo,
	})

	evt1 := mcp.TraceEvent{
		Timestamp:  time.Now().UTC(),
		Event:      "tool_call",
		RequestID:  1,
		Method:     "tools/call",
		Tool:       "read_file",
		Arguments:  map[string]any{"path": "foo.go"},
		DurationMS: 12,
		Status:     "OK",
		Workspace:  tmpDir,
	}

	evt2 := mcp.TraceEvent{
		Timestamp:  time.Now().UTC(),
		Event:      "tool_call",
		RequestID:  2,
		Method:     "tools/call",
		Tool:       "write_file",
		Arguments:  map[string]any{"path": "bar.go", "password": "leak"},
		DurationMS: 45,
		Status:     "ERROR",
		Error:      "disk full",
		Workspace:  tmpDir,
	}

	tracer.LogEvent(evt1)
	tracer.LogEvent(evt2)
	_ = tracer.Close()

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed reading trace log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines in trace log, got %d", len(lines))
	}

	var parsed1, parsed2 mcp.TraceEvent
	if err := json.Unmarshal([]byte(lines[0]), &parsed1); err != nil {
		t.Fatalf("failed unmarshaling line 1: %v", err)
	}
	if parsed1.Tool != "read_file" || parsed1.Status != "OK" || parsed1.DurationMS != 12 {
		t.Errorf("unexpected parsed event 1: %+v", parsed1)
	}

	if err := json.Unmarshal([]byte(lines[1]), &parsed2); err != nil {
		t.Fatalf("failed unmarshaling line 2: %v", err)
	}
	if parsed2.Tool != "write_file" || parsed2.Status != "ERROR" || parsed2.Error != "disk full" {
		t.Errorf("unexpected parsed event 2: %+v", parsed2)
	}
	if parsed2.Arguments["password"] != "[REDACTED]" {
		t.Errorf("expected password to be redacted in log file: %+v", parsed2.Arguments)
	}
}

func TestDefaultTracer_VerboseStderr(t *testing.T) {
	var stderrBuf bytes.Buffer
	tracer := mcp.NewDefaultTracer(mcp.TracerConfig{
		Verbose: true,
		Stderr:  &stderrBuf,
	})

	tracer.LogEvent(mcp.TraceEvent{
		Event:      "tool_call",
		Tool:       "omnicoder_patch",
		Arguments:  map[string]any{"path": "cmd/main.go"},
		DurationMS: 25,
		Status:     "OK",
	})

	tracer.LogEvent(mcp.TraceEvent{
		Event:      "tool_call",
		Tool:       "omnicoder_heal",
		Arguments:  map[string]any{"command": "go test ./..."},
		DurationMS: 120,
		Status:     "ERROR",
		Error:      "compilation failed",
	})

	out := stderrBuf.String()
	if !strings.Contains(out, "[OmniCoder-MCP] [tool:omnicoder_patch] status=OK duration=25ms path=\"cmd/main.go\"") {
		t.Errorf("stderr missing expected patch log line: %s", out)
	}
	if !strings.Contains(out, "[OmniCoder-MCP] [tool:omnicoder_heal] status=ERROR duration=120ms err=\"compilation failed\" cmd=\"go test ./...\"") {
		t.Errorf("stderr missing expected heal log line: %s", out)
	}
}

func TestDefaultTracer_Concurrency(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "concurrent_trace.jsonl")

	tracer := mcp.NewDefaultTracer(mcp.TracerConfig{
		FilePath: logPath,
	})

	var wg sync.WaitGroup
	const workers = 20
	const eventsPerWorker = 30

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < eventsPerWorker; j++ {
				tracer.LogEvent(mcp.TraceEvent{
					Event:      "tool_call",
					RequestID:  workerID*1000 + j,
					Tool:       "concurrency_test",
					Arguments:  map[string]any{"worker": workerID, "seq": j},
					DurationMS: int64(j),
					Status:     "OK",
				})
			}
		}(i)
	}

	wg.Wait()
	_ = tracer.Close()

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read concurrent trace log: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	expectedCount := workers * eventsPerWorker
	if len(lines) != expectedCount {
		t.Fatalf("expected %d lines in concurrent trace, got %d", expectedCount, len(lines))
	}

	// Verify each line is valid JSON
	for idx, line := range lines {
		var evt mcp.TraceEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("line %d is corrupted JSON: %v", idx, err)
		}
	}
}

// memoryTracer records events in memory for test verification
type memoryTracer struct {
	mu     sync.Mutex
	events []mcp.TraceEvent
	level  mcp.LogLevel
}

func (m *memoryTracer) LogEvent(e mcp.TraceEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}

func (m *memoryTracer) SetLevel(l mcp.LogLevel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.level = l
}

func (m *memoryTracer) GetLevel() mcp.LogLevel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.level
}

func (m *memoryTracer) FilePath() string {
	return ""
}

func (m *memoryTracer) Close() error {
	return nil
}

func (m *memoryTracer) Events() []mcp.TraceEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]mcp.TraceEvent, len(m.events))
	copy(res, m.events)
	return res
}

func TestServer_TraceIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	mem := &memoryTracer{level: mcp.LogLevelInfo}

	srv := mcp.NewServer(tmpDir, mcp.WithTracer(mem))

	// Register a simple test tool
	_ = srv.RegisterTool(mcp.ToolDefinition{
		Name:        "test_echo",
		Description: "echo tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
			"required": []any{"message"},
		},
	}, func(ctx context.Context, args map[string]any) (any, error) {
		msg, _ := args["message"].(string)
		return "Echo: " + msg, nil
	})

	inBuf := bytes.NewBuffer(nil)
	outBuf := bytes.NewBuffer(nil)

	// 1. Successful tool call
	req1 := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_echo",
			"arguments": map[string]any{"message": "hello world"},
		},
	}
	d1, _ := json.Marshal(req1)
	inBuf.Write(append(d1, '\n'))

	// 2. Schema validation error
	req2 := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "test_echo",
			"arguments": map[string]any{}, // missing required "message"
		},
	}
	d2, _ := json.Marshal(req2)
	inBuf.Write(append(d2, '\n'))

	// 3. Tool not found error
	req3 := map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "non_existent_tool",
			"arguments": map[string]any{},
		},
	}
	d3, _ := json.Marshal(req3)
	inBuf.Write(append(d3, '\n'))

	// 4. logging/setLevel
	req4 := map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "logging/setLevel",
		"params": map[string]any{
			"level": "debug",
		},
	}
	d4, _ := json.Marshal(req4)
	inBuf.Write(append(d4, '\n'))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := srv.Serve(ctx, inBuf, outBuf)
	if err != nil {
		t.Fatalf("Serve error: %v", err)
	}

	events := mem.Events()
	if len(events) < 3 {
		t.Fatalf("expected at least 3 trace events, got %d", len(events))
	}

	eventMap := make(map[any]mcp.TraceEvent)
	for _, evt := range events {
		eventMap[evt.RequestID] = evt
	}

	// Verify Event 1 (RequestID 1): OK
	evt1, ok1 := eventMap[float64(1)]
	if !ok1 {
		evt1, ok1 = eventMap[1]
	}
	if !ok1 {
		t.Fatalf("missing trace event for request ID 1: %+v", events)
	}
	if evt1.Tool != "test_echo" || evt1.Status != "OK" {
		t.Errorf("expected Request 1 to be test_echo OK, got %+v", evt1)
	}
	if evt1.Arguments["message"] != "hello world" {
		t.Errorf("expected message argument to be hello world, got %+v", evt1.Arguments)
	}

	// Verify Event 2 (RequestID 2): VALIDATION_ERROR
	evt2, ok2 := eventMap[float64(2)]
	if !ok2 {
		evt2, ok2 = eventMap[2]
	}
	if !ok2 {
		t.Fatalf("missing trace event for request ID 2: %+v", events)
	}
	if evt2.Tool != "test_echo" || evt2.Status != "VALIDATION_ERROR" {
		t.Errorf("expected Request 2 to be VALIDATION_ERROR, got %+v", evt2)
	}

	// Verify Event 3 (RequestID 3): ERROR (Tool not found)
	evt3, ok3 := eventMap[float64(3)]
	if !ok3 {
		evt3, ok3 = eventMap[3]
	}
	if !ok3 {
		t.Fatalf("missing trace event for request ID 3: %+v", events)
	}
	if evt3.Tool != "non_existent_tool" || evt3.Status != "ERROR" {
		t.Errorf("expected Request 3 to be ERROR, got %+v", evt3)
	}

	// Verify logging/setLevel updated level
	if mem.GetLevel() != mcp.LogLevelDebug {
		t.Errorf("expected level to be updated to debug, got %v", mem.GetLevel())
	}
}

func TestServer_SendLogMessage(t *testing.T) {
	srv := mcp.NewServer(t.TempDir())
	outBuf := bytes.NewBuffer(nil)
	inBuf := bytes.NewBuffer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ctx, inBuf, outBuf)
	}()

	// Small pause for serve loop initialization
	time.Sleep(50 * time.Millisecond)

	srv.SendLogMessage(mcp.LogLevelWarning, "omnicoder", "Low memory warning", map[string]any{"free_mb": 128})
	cancel()
	wg.Wait()

	line := strings.TrimSpace(outBuf.String())
	if line == "" {
		t.Fatalf("expected notification on stdout, got empty string")
	}

	var notif map[string]any
	if err := json.Unmarshal([]byte(line), &notif); err != nil {
		t.Fatalf("failed to unmarshal notification: %v", err)
	}

	if notif["method"] != "notifications/message" {
		t.Errorf("expected method 'notifications/message', got %v", notif["method"])
	}

	params, _ := notif["params"].(map[string]any)
	if params["level"] != "warning" || params["logger"] != "omnicoder" || params["msg"] != "Low memory warning" {
		t.Errorf("unexpected notification params: %+v", params)
	}
}

func TestServer_StdioIsolationWithTracing(t *testing.T) {
	// Rigorously test that even with verbose stderr logging and file tracing enabled,
	// stdout receives ONLY 100% valid JSON-RPC frames with zero leaked debug text.
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "isolation_trace.jsonl")
	var stderrBuf bytes.Buffer

	tracer := mcp.NewDefaultTracer(mcp.TracerConfig{
		FilePath: logPath,
		Verbose:  true,
		Stderr:   &stderrBuf,
	})

	srv := mcp.NewServer(tmpDir, mcp.WithTracer(tracer))

	inBuf := bytes.NewBuffer(nil)
	outBuf := bytes.NewBuffer(nil)

	// Send an initialize request
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      "init-1",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "testClient", "version": "1.0"},
		},
	}
	d, _ := json.Marshal(initReq)
	inBuf.Write(append(d, '\n'))

	// Send a tools/list request
	listReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      "list-1",
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	dList, _ := json.Marshal(listReq)
	inBuf.Write(append(dList, '\n'))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = srv.Serve(ctx, inBuf, outBuf)

	// Verify all stdout output is strict JSON-RPC
	scanner := bufio.NewScanner(outBuf)
	lineCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("STDOUT CORRUPTED by logging! Raw text leaked: %q (err: %v)", line, err)
		}
		if parsed["jsonrpc"] != "2.0" {
			t.Fatalf("invalid jsonrpc in line: %q", line)
		}
		lineCount++
	}

	if lineCount != 2 {
		t.Errorf("expected exactly 2 valid JSON-RPC frames on stdout, got %d", lineCount)
	}

	// Verify stderr got human-readable logs
	stderrOut := stderrBuf.String()
	t.Logf("Captured stderr diagnostic stream:\n%s", stderrOut)
}
