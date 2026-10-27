// SPDX-License-Identifier: Apache-2.0
// Package mcp_test contains adversarial stress tests and protocol compliance challenges
// for the OmniCoder-ICX Model Context Protocol (MCP) server.

package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

// ----------------------------------------------------------------------------
// Dedicated Robust Pipe Harness with Single Reader (Prevents Goroutine Leaks)
// ----------------------------------------------------------------------------

type RobustHarness struct {
	t          *testing.T
	Server     *mcp.Server
	ClientOut  *io.PipeWriter
	ServerDone chan error
	RespCh     chan *mcp.JSONRPCResponse
	RawLineCh  chan []byte
	ErrCh      chan error
	Cancel     context.CancelFunc
	writeMu    sync.Mutex
}

func NewRobustHarness(t *testing.T, workDir string) *RobustHarness {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	srv := mcp.NewServer(workDir)

	h := &RobustHarness{
		t:          t,
		Server:     srv,
		ClientOut:  clientOut,
		ServerDone: make(chan error, 1),
		RespCh:     make(chan *mcp.JSONRPCResponse, 100),
		RawLineCh:  make(chan []byte, 100),
		ErrCh:      make(chan error, 10),
		Cancel:     cancel,
	}

	go func() {
		h.ServerDone <- srv.Serve(ctx, serverIn, serverOut)
	}()

	// Single reader loop
	go func() {
		reader := bufio.NewReader(clientIn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				h.ErrCh <- err
				return
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			h.RawLineCh <- trimmed

			var resp mcp.JSONRPCResponse
			if err := json.Unmarshal(trimmed, &resp); err == nil {
				h.RespCh <- &resp
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = clientOut.Close()
		_ = serverOut.Close()
		_ = serverIn.Close()
		_ = clientIn.Close()
	})

	return h
}

func (h *RobustHarness) SendRaw(data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, err := h.ClientOut.Write(append(data, '\n'))
	return err
}

func (h *RobustHarness) SendFramed(body string) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	_, err := h.ClientOut.Write([]byte(msg))
	return err
}

func (h *RobustHarness) ReadResponse(timeout time.Duration) (*mcp.JSONRPCResponse, error) {
	select {
	case resp := <-h.RespCh:
		return resp, nil
	case err := <-h.ErrCh:
		return nil, fmt.Errorf("stream read error: %w", err)
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for response after %v", timeout)
	}
}

func (h *RobustHarness) Call(id any, method string, params any) (*mcp.JSONRPCResponse, error) {
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
	return h.ReadResponse(3 * time.Second)
}

// ----------------------------------------------------------------------------
// Domain 1: Framing Stress Testing
// ----------------------------------------------------------------------------

// TestAdversarial_MixedFramingStreams tests alternating between Content-Length
// framed messages and raw newline-delimited JSON over the exact same pipe.
func TestAdversarial_MixedFramingStreams(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	const iterations = 50

	for i := 0; i < iterations; i++ {
		reqID := 1000 + i
		if i%2 == 0 {
			// Content-Length framed
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, reqID)
			if err := h.SendFramed(body); err != nil {
				t.Fatalf("iteration %d: SendFramed failed: %v", i, err)
			}
		} else {
			// Newline-delimited JSON
			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, reqID)
			if err := h.SendRaw([]byte(raw)); err != nil {
				t.Fatalf("iteration %d: SendRaw failed: %v", i, err)
			}
		}

		resp, err := h.ReadResponse(2 * time.Second)
		if err != nil {
			t.Fatalf("iteration %d (id=%d): ReadResponse failed: %v", i, reqID, err)
		}
		if resp == nil {
			t.Fatalf("iteration %d (id=%d): nil response", i, reqID)
		}
		if resp.Error != nil {
			t.Fatalf("iteration %d (id=%d): unexpected error: %v", i, reqID, resp.Error)
		}

		// Verify ID matches
		respID, ok := resp.ID.(float64)
		if !ok || int(respID) != reqID {
			t.Fatalf("iteration %d: expected response ID %d, got %v", i, reqID, resp.ID)
		}
	}
}

// TestAdversarial_MalformedFraming_NegativeContentLength verifies server handles negative Content-Length.
func TestAdversarial_MalformedFraming_NegativeContentLength(t *testing.T) {
	workDir := t.TempDir()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	defer clientIn.Close()
	defer serverOut.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mcp.NewServer(workDir)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Serve(ctx, serverIn, serverOut)
	}()

	// Send negative Content-Length
	_, err := clientOut.Write([]byte("Content-Length: -5\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	select {
	case srvErr := <-serverDone:
		if srvErr == nil {
			t.Errorf("expected server to terminate with error on negative Content-Length, got nil")
		} else if strings.Contains(srvErr.Error(), "exceeds maximum allowed buffer size") {
			t.Logf("server terminated with expected error: %v", srvErr)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("server hung and did not reject negative Content-Length within timeout")
	}
}

// TestAdversarial_MalformedFraming_ContentLengthExceeding64MB verifies server rejects lengths > 64MB.
func TestAdversarial_MalformedFraming_ContentLengthExceeding64MB(t *testing.T) {
	workDir := t.TempDir()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	defer clientIn.Close()
	defer serverOut.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mcp.NewServer(workDir)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Serve(ctx, serverIn, serverOut)
	}()

	// Send 65MB length header
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n", 65*1024*1024)
	_, err := clientOut.Write([]byte(msg))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	select {
	case srvErr := <-serverDone:
		if srvErr == nil {
			t.Errorf("expected server error on Content-Length > 64MB")
		} else {
			t.Logf("server correctly terminated with: %v", srvErr)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("server did not reject >64MB Content-Length in time")
	}
}

// TestAdversarial_MalformedFraming_TruncatedHeaders verifies server rejects >32 header lines.
func TestAdversarial_MalformedFraming_TruncatedHeaders(t *testing.T) {
	workDir := t.TempDir()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	defer clientIn.Close()
	defer serverOut.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := mcp.NewServer(workDir)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Serve(ctx, serverIn, serverOut)
	}()

	// Send Content-Length followed by 35 extra headers without empty line
	var buf bytes.Buffer
	buf.WriteString("Content-Length: 42\r\n")
	for i := 0; i < 35; i++ {
		buf.WriteString(fmt.Sprintf("X-Extra-Header-%d: value%d\r\n", i, i))
	}
	buf.WriteString("\r\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}")

	_, err := clientOut.Write(buf.Bytes())
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	select {
	case srvErr := <-serverDone:
		if srvErr == nil {
			t.Errorf("expected server to reject >32 header lines")
		} else if strings.Contains(srvErr.Error(), "exceeded maximum header line count") {
			t.Logf("server correctly rejected excess header lines: %v", srvErr)
		}
	case <-time.After(1 * time.Second):
		t.Errorf("server did not reject >32 header lines in time")
	}
}

// TestAdversarial_MalformedFraming_MissingCRLFSeparators tests behavior with \n instead of \r\n,
// and missing blank line separators.
func TestAdversarial_MalformedFraming_MissingCRLFSeparators(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	// 1. Headers separated with pure \n (Unix style) instead of \r\n (RFC style)
	// Server should tolerate pure \n as TrimSpace handles it.
	unixFramed := "Content-Length: 42\n\n{\"jsonrpc\":\"2.0\",\"id\":555,\"method\":\"ping\"}"
	if _, err := h.ClientOut.Write([]byte(unixFramed)); err != nil {
		t.Fatalf("write unixFramed failed: %v", err)
	}
	resp, err := h.ReadResponse(1 * time.Second)
	if err != nil {
		t.Fatalf("failed reading response for pure LF framing: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error for pure LF framing: %v", resp.Error)
	}
	if resp.ID == nil || int(resp.ID.(float64)) != 555 {
		t.Fatalf("expected ID 555, got %v", resp.ID)
	}
}

// TestAdversarial_Framing_PartialWritesAndChunkedReads tests writing requests
// in tiny 1-3 byte fragments to test buffering robustness.
func TestAdversarial_Framing_PartialWritesAndChunkedReads(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	body := `{"jsonrpc":"2.0","id":777,"method":"ping"}`
	framed := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)

	// Write framed request in 2-byte chunks with slight pause
	h.writeMu.Lock()
	for i := 0; i < len(framed); i += 2 {
		end := i + 2
		if end > len(framed) {
			end = len(framed)
		}
		if _, err := h.ClientOut.Write([]byte(framed[i:end])); err != nil {
			h.writeMu.Unlock()
			t.Fatalf("failed partial write at offset %d: %v", i, err)
		}
		time.Sleep(1 * time.Millisecond)
	}
	h.writeMu.Unlock()

	resp, err := h.ReadResponse(2 * time.Second)
	if err != nil {
		t.Fatalf("failed reading response after partial writes: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.ID == nil || int(resp.ID.(float64)) != 777 {
		t.Fatalf("expected ID 777, got %v", resp.ID)
	}
}

// ----------------------------------------------------------------------------
// Domain 2: Protocol Compliance Stress
// ----------------------------------------------------------------------------

// TestAdversarial_Protocol_NotificationSilence floods the server with 240 notifications
// across varied formats and asserts that EXACTLY 0 bytes are emitted in response.
func TestAdversarial_Protocol_NotificationSilence(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	notificationPayloads := []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":1}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","method":"custom/telemetry","params":{"event":"ping"}}`,
		`{"jsonrpc":"2.0","method":"notifications/message","params":{"text":"hello"}}`,
		`{"jsonrpc":"2.0","method":"tools/call"}`, // No ID member -> notification
	}

	// Flood 240 notifications
	for i := 0; i < 40; i++ {
		for _, payload := range notificationPayloads {
			if err := h.SendRaw([]byte(payload)); err != nil {
				t.Fatalf("failed sending notification: %v", err)
			}
		}
	}

	// Verify silence: no response or bytes emitted for 200ms
	select {
	case line := <-h.RawLineCh:
		t.Fatalf("VIOLATION: server emitted bytes for notification: %s", string(line))
	case resp := <-h.RespCh:
		t.Fatalf("VIOLATION: server emitted JSON-RPC response for notification: %+v", resp)
	case <-time.After(200 * time.Millisecond):
		// Expected: complete silence!
	}

	// Verify server remains alive and responsive after flood
	aliveResp, err := h.Call(8888, "ping", nil)
	if err != nil {
		t.Fatalf("server stalled or died after notification flood: %v", err)
	}
	if aliveResp.Error != nil {
		t.Fatalf("unexpected error on post-flood ping: %v", aliveResp.Error)
	}
	if aliveResp.ID == nil || int(aliveResp.ID.(float64)) != 8888 {
		t.Fatalf("expected ping response ID 8888, got %v", aliveResp.ID)
	}
}

// TestAdversarial_Protocol_MalformedJSONPayloads tests malformed JSON inputs.
func TestAdversarial_Protocol_MalformedJSONPayloads(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	malformedCases := []string{
		`{bad-json-not-valid`,
		`{"jsonrpc": "2.0", "method": "ping", "id": 1,}`, // Trailing comma
		`{"jsonrpc": "2.0", "method": `,                  // Truncated
		`NOT_EVEN_CLOSE_TO_JSON`,
		`{"id": 10, "method": "ping", "params": {unclosed`,
	}

	for i, badJSON := range malformedCases {
		if err := h.SendRaw([]byte(badJSON)); err != nil {
			t.Fatalf("case %d: send failed: %v", i, err)
		}

		resp, err := h.ReadResponse(1 * time.Second)
		if err != nil {
			t.Fatalf("case %d: failed reading response: %v", i, err)
		}
		if resp.Error == nil {
			t.Fatalf("case %d: expected ParseError, got nil error", i)
		}
		if resp.Error.Code != mcp.ErrCodeParseError {
			t.Errorf("case %d: expected code %d (ParseError), got %d", i, mcp.ErrCodeParseError, resp.Error.Code)
		}
		if resp.ID != nil {
			t.Errorf("case %d: expected ID null on ParseError, got %v", i, resp.ID)
		}
	}
}

// TestAdversarial_Protocol_InvalidVersionStrings tests invalid jsonrpc versions.
func TestAdversarial_Protocol_InvalidVersionStrings(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	invalidVersions := []string{"1.0", "3.0", "2.1", "foo"}

	for i, ver := range invalidVersions {
		reqID := 3000 + i
		raw := fmt.Sprintf(`{"jsonrpc": "%s", "id": %d, "method": "ping"}`, ver, reqID)
		if err := h.SendRaw([]byte(raw)); err != nil {
			t.Fatalf("ver %s: send failed: %v", ver, err)
		}

		resp, err := h.ReadResponse(1 * time.Second)
		if err != nil {
			t.Fatalf("ver %s: read failed: %v", ver, err)
		}
		if resp.Error == nil {
			t.Fatalf("ver %s: expected error, got nil", ver)
		}
		if resp.Error.Code != mcp.ErrCodeInvalidRequest {
			t.Errorf("ver %s: expected code %d (InvalidRequest), got %d", ver, mcp.ErrCodeInvalidRequest, resp.Error.Code)
		}
		if resp.ID == nil || int(resp.ID.(float64)) != reqID {
			t.Errorf("ver %s: expected response ID %d, got %v", ver, reqID, resp.ID)
		}
	}
}

// TestAdversarial_Protocol_MissingVersionString tests requests completely omitting the "jsonrpc" field.
func TestAdversarial_Protocol_MissingVersionString(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	// In JSON-RPC 2.0 Section 4: "jsonrpc: A String specifying the version of the JSON-RPC protocol. MUST be exactly '2.0'."
	// A request without "jsonrpc" is an invalid request.
	raw := `{"id": 3500, "method": "ping"}`
	if err := h.SendRaw([]byte(raw)); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	resp, err := h.ReadResponse(1 * time.Second)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	t.Logf("missing jsonrpc response: %+v (error: %v)", resp, resp.Error)
	if resp.Error == nil || resp.Error.Code != mcp.ErrCodeInvalidRequest {
		t.Errorf("server accepted request missing 'jsonrpc' field or returned non-InvalidRequest code: %+v", resp.Error)
	}
}

// TestAdversarial_Protocol_BatchRequests tests batch request behavior against JSON-RPC 2.0.
func TestAdversarial_Protocol_BatchRequests(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	batchPayloads := []struct {
		name    string
		payload string
	}{
		{"empty_batch", `[]`},
		{"single_item_batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`},
		{"multi_item_batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`},
	}

	for _, tc := range batchPayloads {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.SendRaw([]byte(tc.payload)); err != nil {
				t.Fatalf("send failed: %v", err)
			}
			resp, err := h.ReadResponse(1 * time.Second)
			if err != nil {
				t.Fatalf("read failed: %v", err)
			}
			t.Logf("[%s] received response: %+v (error code: %v)", tc.name, resp, resp.Error)

			if resp.Error == nil {
				t.Fatalf("expected error on batch request, got success")
			}
			// JSON-RPC 2.0 Section 6 asserts InvalidRequest (-32600)
			if resp.Error.Code != mcp.ErrCodeInvalidRequest {
				t.Errorf("batch request yielded error code %d (%s), expected %d (InvalidRequest)",
					resp.Error.Code, resp.Error.Message, mcp.ErrCodeInvalidRequest)
			}
		})
	}
}

// TestAdversarial_Protocol_UnknownToolsAndInvalidArguments tests error responses.
func TestAdversarial_Protocol_UnknownToolsAndInvalidArguments(t *testing.T) {
	workDir := t.TempDir()
	h := NewRobustHarness(t, workDir)

	// 1. Unknown tool -> MethodNotFound (-32601)
	resp, err := h.Call(4001, "tools/call", map[string]any{
		"name":      "non_existent_tool_xyz",
		"arguments": map[string]any{},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != mcp.ErrCodeMethodNotFound {
		t.Errorf("expected MethodNotFound (-32601), got: %+v", resp.Error)
	}

	// 2. Unknown method entirely -> MethodNotFound (-32601)
	resp2, err := h.Call(4002, "non_existent_rpc_method", nil)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if resp2.Error == nil || resp2.Error.Code != mcp.ErrCodeMethodNotFound {
		t.Errorf("expected MethodNotFound (-32601), got: %+v", resp2.Error)
	}

	// 3. Invalid arguments (schema violation) -> CallToolResult with isError: true
	resp3, err := h.Call(4003, "tools/call", map[string]any{
		"name": "write_file",
		"arguments": map[string]any{
			"path": "test.txt",
			// missing required "content"
		},
	})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if resp3.Error != nil {
		t.Fatalf("unexpected RPC error for schema violation: %v", resp3.Error)
	}
	resBytes, _ := json.Marshal(resp3.Result)
	var toolRes mcp.CallToolResult
	if err := json.Unmarshal(resBytes, &toolRes); err != nil {
		t.Fatalf("unmarshal tool result failed: %v", err)
	}
	if !toolRes.IsError {
		t.Errorf("expected toolRes.IsError == true on missing content")
	}

	// 4. Malformed call tool params -> InvalidParams (-32602)
	// Passing a non-object params member
	raw := `{"jsonrpc":"2.0","id":4004,"method":"tools/call","params":"invalid-string-params"}`
	if err := h.SendRaw([]byte(raw)); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	resp4, err := h.ReadResponse(1 * time.Second)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if resp4.Error == nil || resp4.Error.Code != mcp.ErrCodeInvalidParams {
		t.Errorf("expected InvalidParams (-32602), got: %+v", resp4.Error)
	}
}

// ----------------------------------------------------------------------------
// Domain 3: High-Concurrency Pipe Stress (50+ goroutines)
// ----------------------------------------------------------------------------

// MultiplexedPipeHarness manages concurrent access to a single pipe pair
// with response demultiplexing by JSON-RPC ID.
type MultiplexedPipeHarness struct {
	writeMu   sync.Mutex
	clientOut *io.PipeWriter

	pendingMu sync.Mutex
	pending   map[int]chan *mcp.JSONRPCResponse

	stopCh chan struct{}
}

func NewMultiplexedPipeHarness(srv *mcp.Server, ctx context.Context) (*MultiplexedPipeHarness, func()) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	h := &MultiplexedPipeHarness{
		clientOut: clientOut,
		pending:   make(map[int]chan *mcp.JSONRPCResponse),
		stopCh:    make(chan struct{}),
	}

	go func() {
		_ = srv.Serve(ctx, serverIn, serverOut)
	}()

	// Single reader demultiplexer loop
	go func() {
		reader := bufio.NewReader(clientIn)
		for {
			select {
			case <-h.stopCh:
				return
			default:
			}

			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}

			var resp mcp.JSONRPCResponse
			if err := json.Unmarshal(trimmed, &resp); err != nil {
				continue
			}

			if resp.ID != nil {
				var idInt int
				switch v := resp.ID.(type) {
				case float64:
					idInt = int(v)
				case int:
					idInt = v
				case string:
					idInt, _ = strconv.Atoi(v)
				}

				h.pendingMu.Lock()
				ch, ok := h.pending[idInt]
				if ok {
					delete(h.pending, idInt)
					ch <- &resp
				}
				h.pendingMu.Unlock()
			}
		}
	}()

	cleanup := func() {
		close(h.stopCh)
		clientOut.Close()
		serverOut.Close()
		serverIn.Close()
		clientIn.Close()
	}

	return h, cleanup
}

func (h *MultiplexedPipeHarness) Call(id int, method string, params any) (*mcp.JSONRPCResponse, error) {
	ch := make(chan *mcp.JSONRPCResponse, 1)

	h.pendingMu.Lock()
	h.pending[id] = ch
	h.pendingMu.Unlock()

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

	h.writeMu.Lock()
	_, err = h.clientOut.Write(append(data, '\n'))
	h.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(5 * time.Second):
		h.pendingMu.Lock()
		delete(h.pending, id)
		h.pendingMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for response ID %d", id)
	}
}

// TestAdversarial_ConcurrencyStress_55GoroutinesOverPipes runs 55 concurrent goroutines
// issuing simultaneous tools/call and tools/list requests over an in-memory pipe.
func TestAdversarial_ConcurrencyStress_55GoroutinesOverPipes(t *testing.T) {
	workDir := t.TempDir()
	srv := mcp.NewServer(workDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, cleanup := NewMultiplexedPipeHarness(srv, ctx)
	defer cleanup()

	const numGoroutines = 55
	const opsPerGoroutine = 10

	var globalID atomic.Int64
	globalID.Store(10000)

	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines*opsPerGoroutine)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for op := 0; op < opsPerGoroutine; op++ {
				id := int(globalID.Add(1))
				if op%2 == 0 {
					// tools/list call
					resp, err := h.Call(id, "tools/list", nil)
					if err != nil {
						errCh <- fmt.Errorf("worker %d op %d tools/list failed: %w", workerID, op, err)
						return
					}
					if resp.Error != nil {
						errCh <- fmt.Errorf("worker %d op %d tools/list unexpected error: %v", workerID, op, resp.Error)
						return
					}
					if resp.Result == nil {
						errCh <- fmt.Errorf("worker %d op %d tools/list nil result", workerID, op)
						return
					}
					// Verify result is a valid tool list
					resBytes, err := json.Marshal(resp.Result)
					if err != nil {
						errCh <- fmt.Errorf("worker %d marshal tools/list failed: %w", workerID, err)
						return
					}
					var listRes mcp.ToolListResult
					if err := json.Unmarshal(resBytes, &listRes); err != nil {
						errCh <- fmt.Errorf("worker %d unmarshal tools/list failed: %w", workerID, err)
						return
					}
					if len(listRes.Tools) == 0 {
						errCh <- fmt.Errorf("worker %d empty tools list", workerID)
						return
					}
				} else {
					// tools/call: ping or read_file
					resp, err := h.Call(id, "ping", nil)
					if err != nil {
						errCh <- fmt.Errorf("worker %d op %d ping failed: %w", workerID, op, err)
						return
					}
					if resp.Error != nil {
						errCh <- fmt.Errorf("worker %d op %d ping unexpected error: %v", workerID, op, resp.Error)
						return
					}
					if resp.ID == nil {
						errCh <- fmt.Errorf("worker %d op %d nil response ID", workerID, op)
						return
					}
					respID, ok := resp.ID.(float64)
					if !ok || int(respID) != id {
						errCh <- fmt.Errorf("worker %d op %d ID mismatch: expected %d, got %v", workerID, op, id, resp.ID)
						return
					}
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	var errors []error
	for err := range errCh {
		errors = append(errors, err)
	}
	if len(errors) > 0 {
		t.Fatalf("encountered %d concurrency errors across 55 goroutines over pipes, first: %v", len(errors), errors[0])
	}
}
