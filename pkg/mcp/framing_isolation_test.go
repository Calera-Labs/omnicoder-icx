// SPDX-License-Identifier: Apache-2.0
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

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// TestReadNextMessage_ComprehensiveFramingMatrix verifies readNextMessage against
// the full matrix of HTTP header ordering, CRLF vs LF, lowercase headers, leading newlines,
// BOM prefixes, and edge cases.
func TestReadNextMessage_ComprehensiveFramingMatrix(t *testing.T) {
	testCases := []struct {
		name          string
		input         string
		expectedBody  string
		expectedError string
	}{
		{
			name:         "Pure newline-delimited JSON (LF)",
			input:        "{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":1}\n",
			expectedBody: "{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":1}\n",
		},
		{
			name:         "Pure newline-delimited JSON (CRLF)",
			input:        "{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":2}\r\n",
			expectedBody: "{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":2}\r\n",
		},
		{
			name:         "Leading blank lines before NDJSON",
			input:        "\r\n\r\n\n{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":3}\n",
			expectedBody: "{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":3}\n",
		},
		{
			name:         "UTF-8 BOM prefix on NDJSON",
			input:        "\xef\xbb\xbf{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":4}\n",
			expectedBody: "{\"jsonrpc\":\"2.0\",\"method\":\"ping\",\"id\":4}\n",
		},
		{
			name:         "Content-Length standard CRLF framing",
			input:        fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":10}`), `{"jsonrpc":"2.0","method":"ping","id":10}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":10}`,
		},
		{
			name:         "Content-Length standard LF-only framing",
			input:        fmt.Sprintf("Content-Length: %d\n\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":11}`), `{"jsonrpc":"2.0","method":"ping","id":11}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":11}`,
		},
		{
			name:         "Content-Type BEFORE Content-Length",
			input:        fmt.Sprintf("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: %d\r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":12}`), `{"jsonrpc":"2.0","method":"ping","id":12}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":12}`,
		},
		{
			name:         "Content-Type AFTER Content-Length",
			input:        fmt.Sprintf("Content-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":13}`), `{"jsonrpc":"2.0","method":"ping","id":13}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":13}`,
		},
		{
			name: "Multiple arbitrary headers before Content-Length",
			input: fmt.Sprintf("User-Agent: test-client/1.0\r\n" +
				"X-MCP-Protocol: 2024-11-05\r\n" +
				"Content-Type: application/json\r\n" +
				"Content-Length: %d\r\n" +
				"\r\n" +
				"%s", len(`{"jsonrpc":"2.0","method":"ping","id":14}`), `{"jsonrpc":"2.0","method":"ping","id":14}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":14}`,
		},
		{
			name:         "Case-insensitive lowercase content-length header",
			input:        fmt.Sprintf("content-length: %d\r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":15}`), `{"jsonrpc":"2.0","method":"ping","id":15}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":15}`,
		},
		{
			name:         "Content-Length with extra surrounding whitespace",
			input:        fmt.Sprintf("Content-Length:   %d   \r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":16}`), `{"jsonrpc":"2.0","method":"ping","id":16}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":16}`,
		},
		{
			name:         "Leading newlines before Content-Length",
			input:        fmt.Sprintf("\r\n\n\r\nContent-Length: %d\r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":17}`), `{"jsonrpc":"2.0","method":"ping","id":17}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":17}`,
		},
		{
			name:         "UTF-8 BOM prefix before Content-Length",
			input:        fmt.Sprintf("\xef\xbb\xbfContent-Length: %d\r\n\r\n%s", len(`{"jsonrpc":"2.0","method":"ping","id":18}`), `{"jsonrpc":"2.0","method":"ping","id":18}`),
			expectedBody: `{"jsonrpc":"2.0","method":"ping","id":18}`,
		},
		{
			name:         "Content-Length with zero length",
			input:        "Content-Length: 0\r\n\r\n",
			expectedBody: "",
		},
		{
			name:          "Malformed Content-Length: non-numeric",
			input:         "Content-Length: invalid_num\r\n\r\nbody",
			expectedError: "invalid Content-Length value",
		},
		{
			name:          "Malformed Content-Length: negative",
			input:         "Content-Length: -50\r\n\r\nbody",
			expectedError: "exceeds maximum allowed buffer size",
		},
		{
			name:          "Header block missing Content-Length",
			input:         "Content-Type: application/json\r\nAuthorization: Bearer test\r\n\r\nbody",
			expectedError: "header block missing Content-Length",
		},
		{
			name:          "Oversized Content-Length exceeding MaxMessageSize",
			input:         fmt.Sprintf("Content-Length: %d\r\n\r\nbody", mcp.MaxMessageSize+1),
			expectedError: "exceeds maximum allowed buffer size",
		},
		{
			name: "Header line count exceeding MaxHeaderLines",
			input: func() string {
				var sb strings.Builder
				for i := 0; i < mcp.MaxHeaderLines+5; i++ {
					sb.WriteString(fmt.Sprintf("X-Header-%d: value\r\n", i))
				}
				sb.WriteString("Content-Length: 10\r\n\r\n0123456789")
				return sb.String()
			}(),
			expectedError: "exceeded maximum header line count",
		},
		{
			name:          "Unexpected EOF mid-body",
			input:         "Content-Length: 50\r\n\r\nshort",
			expectedError: "unexpected EOF",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tc.input))
			msgBytes, err := mcp.ReadNextMessageForTest(reader)

			if tc.expectedError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (msg: %q)", tc.expectedError, string(msgBytes))
				}
				if !strings.Contains(err.Error(), tc.expectedError) {
					t.Fatalf("expected error containing %q, got: %v", tc.expectedError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(msgBytes) != tc.expectedBody {
					t.Fatalf("body mismatch:\nexpected: %q\ngot:      %q", tc.expectedBody, string(msgBytes))
				}
			}
		})
	}
}

// TestReadNextMessage_SequentialStreamMix tests reading a continuous stream containing
// alternating Content-Length and NDJSON messages, both with and without trailing delimiters.
func TestReadNextMessage_SequentialStreamMix(t *testing.T) {
	var streamBuf bytes.Buffer

	// Message 1: Framed with Content-Type before Content-Length, no trailing newline
	msg1 := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	streamBuf.WriteString(fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(msg1), msg1))

	// Message 2: NDJSON with LF
	msg2 := "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}\n"
	streamBuf.WriteString(msg2)

	// Message 3: Framed with CRLF and trailing CRLF
	msg3 := `{"jsonrpc":"2.0","id":3,"method":"ping"}`
	streamBuf.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n%s\r\n", len(msg3), msg3))

	// Message 4: NDJSON with CRLF
	msg4 := "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"ping\"}\r\n"
	streamBuf.WriteString(msg4)

	// Message 5: Framed with lowercase header
	msg5 := `{"jsonrpc":"2.0","id":5,"method":"ping"}`
	streamBuf.WriteString(fmt.Sprintf("content-length: %d\r\n\r\n%s", len(msg5), msg5))

	reader := bufio.NewReader(&streamBuf)

	expectedMessages := []string{
		msg1,
		msg2,
		msg3,
		msg4,
		msg5,
	}

	for i, expected := range expectedMessages {
		got, err := mcp.ReadNextMessageForTest(reader)
		if err != nil {
			t.Fatalf("message %d: unexpected error: %v", i+1, err)
		}
		if string(got) != expected {
			t.Fatalf("message %d mismatch:\nexpected: %q\ngot:      %q", i+1, expected, string(got))
		}
	}

	// Next read should return EOF
	_, err := mcp.ReadNextMessageForTest(reader)
	if err != io.EOF {
		t.Fatalf("expected EOF at end of stream, got: %v", err)
	}
}

// TestServer_FramingOptionsAndAutoFraming verifies that Server supports Content-Length
// output framing, default newline-delimited output framing, and dynamic auto-framing.
func TestServer_FramingOptionsAndAutoFraming(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("Default framing is newline-delimited JSON", func(t *testing.T) {
		srv := mcp.NewServer(tmpDir)
		if srv.IsContentLengthFraming() {
			t.Errorf("expected default IsContentLengthFraming() to be false")
		}

		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = srv.Serve(ctx, inR, outW) }()

		// Send NDJSON ping
		_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n"))

		reader := bufio.NewReader(outR)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed reading response: %v", err)
		}

		if strings.HasPrefix(line, "Content-Length:") {
			t.Fatalf("did not expect Content-Length header, got: %s", line)
		}

		var resp mcp.JSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("failed unmarshaling NDJSON response: %v", err)
		}
		if resp.ID != float64(1) {
			t.Errorf("expected response ID 1, got %v", resp.ID)
		}
	})

	t.Run("WithContentLengthFraming(true) forces Content-Length output framing", func(t *testing.T) {
		srv := mcp.NewServer(tmpDir, mcp.WithContentLengthFraming(true))
		if !srv.IsContentLengthFraming() {
			t.Errorf("expected IsContentLengthFraming() to be true")
		}

		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = srv.Serve(ctx, inR, outW) }()

		// Send NDJSON ping
		_, _ = inW.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"ping\"}\n"))

		reader := bufio.NewReader(outR)
		headerLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed reading response header: %v", err)
		}

		if !strings.HasPrefix(headerLine, "Content-Length:") {
			t.Fatalf("expected Content-Length header, got: %s", headerLine)
		}

		parts := strings.SplitN(strings.TrimSpace(headerLine), ":", 2)
		length, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("invalid length: %v", err)
		}

		// Read empty line
		emptyLine, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(emptyLine) != "" {
			t.Fatalf("expected empty line after headers, got: %q, err: %v", emptyLine, err)
		}

		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatalf("failed reading body: %v", err)
		}

		var resp mcp.JSONRPCResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("failed unmarshaling framed response: %v (body: %s)", err, string(body))
		}
		if resp.ID != float64(2) {
			t.Errorf("expected response ID 2, got %v", resp.ID)
		}
	})

	t.Run("WithAutoFraming(true) mirrors client's Content-Length framing", func(t *testing.T) {
		srv := mcp.NewServer(tmpDir, mcp.WithAutoFraming(true))

		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() { _ = srv.Serve(ctx, inR, outW) }()

		// Send Content-Length framed ping
		req := "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"ping\"}"
		framedReq := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(req), req)
		_, _ = inW.Write([]byte(framedReq))

		reader := bufio.NewReader(outR)
		headerLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed reading response header: %v", err)
		}

		if !strings.HasPrefix(headerLine, "Content-Length:") {
			t.Fatalf("expected auto-framed Content-Length header, got: %s", headerLine)
		}

		parts := strings.SplitN(strings.TrimSpace(headerLine), ":", 2)
		length, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("invalid length: %v", err)
		}

		// Read empty line
		_, _ = reader.ReadString('\n')

		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatalf("failed reading body: %v", err)
		}

		var resp mcp.JSONRPCResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("failed unmarshaling framed response: %v", err)
		}
		if resp.ID != float64(3) {
			t.Errorf("expected response ID 3, got %v", resp.ID)
		}
	})
}

// TestStdoutIsolation_StrictZeroTextLeaks verifies that during MCP operations,
// zero unformatted text leaks into os.Stdout, and all emitted frames are 100% valid JSON-RPC.
func TestStdoutIsolation_StrictZeroTextLeaks(t *testing.T) {
	tmpDir := t.TempDir()

	// Seed workspace
	sampleFile := filepath.Join(tmpDir, "calculator.go")
	_ = os.WriteFile(sampleFile, []byte(`package main

func Add(a, b int) int {
	return a + b
}
`), 0644)

	det := mcp.AutoDetectEnvironment(tmpDir)
	sCtx := &mcp.OmniCoderServerContext{
		Config:    det,
		Lattice:   icx.NewLocalLattice(),
		VMClient:  vm.NewHermeticLocalSandbox(tmpDir),
		SandboxOn: true,
	}

	server := mcp.NewServer(tmpDir)
	mcp.RegisterOmniCoderTools(server, sCtx)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = server.Serve(ctx, inR, outW)
	}()

	clientReader := bufio.NewReader(outR)

	sendAndVerify := func(method string, id int, params any) mcp.JSONRPCResponse {
		reqMap := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
		}
		if params != nil {
			reqMap["params"] = params
		}
		data, _ := json.Marshal(reqMap)
		data = append(data, '\n')
		if _, err := inW.Write(data); err != nil {
			t.Fatalf("failed writing request: %v", err)
		}

		// Read line from output
		line, err := clientReader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed reading output line for method %s: %v", method, err)
		}

		// Critical Invariant: Output MUST be strictly valid JSON-RPC 2.0 object.
		// If ANY raw text, log banner, debug printf leaked to stdout, unmarshal will fail.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			t.Fatalf("stdout output contained unexpected empty line")
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			t.Fatalf("STDOUT CORRUPTION DETECTED! Leaked unformatted text: %q (err: %v)", line, err)
		}

		if raw["jsonrpc"] != "2.0" {
			t.Fatalf("missing or invalid 'jsonrpc' field in stdout frame: %q", line)
		}

		var resp mcp.JSONRPCResponse
		_ = json.Unmarshal([]byte(trimmed), &resp)
		return resp
	}

	// 1. Initialize
	initResp := sendAndVerify("initialize", 100, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
	})
	if initResp.Error != nil {
		t.Fatalf("initialize failed: %v", initResp.Error)
	}

	// 2. Initialized notification (no response expected)
	notifBytes, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	_, _ = inW.Write(append(notifBytes, '\n'))
	time.Sleep(10 * time.Millisecond)

	// 3. Ping
	pingResp := sendAndVerify("ping", 101, nil)
	if pingResp.Error != nil {
		t.Fatalf("ping failed: %v", pingResp.Error)
	}

	// 4. Tools list
	listResp := sendAndVerify("tools/list", 102, nil)
	if listResp.Error != nil {
		t.Fatalf("tools/list failed: %v", listResp.Error)
	}

	// 5. omnicoder_status
	statusResp := sendAndVerify("tools/call", 103, map[string]any{
		"name":      "omnicoder_status",
		"arguments": map[string]any{},
	})
	if statusResp.Error != nil {
		t.Fatalf("omnicoder_status failed: %v", statusResp.Error)
	}

	// 6. omnicoder_map
	mapResp := sendAndVerify("tools/call", 104, map[string]any{
		"name": "omnicoder_map",
		"arguments": map[string]any{
			"token_budget": 512,
		},
	})
	if mapResp.Error != nil {
		t.Fatalf("omnicoder_map failed: %v", mapResp.Error)
	}

	// 7. omnicoder_search
	searchResp := sendAndVerify("tools/call", 105, map[string]any{
		"name": "omnicoder_search",
		"arguments": map[string]any{
			"query": "Add",
		},
	})
	if searchResp.Error != nil {
		t.Fatalf("omnicoder_search failed: %v", searchResp.Error)
	}

	// Close streams and wait for server shutdown
	_ = inW.Close()
	cancel()
	wg.Wait()
}

// TestOmniCoderSubprocess_StdioIsolation launches omnicoder as a real operating system
// process via exec.Command("mcp") and rigorously verifies that 100% of startup banners,
// diagnostics, and logs go exclusively to stderr, while stdout contains exclusively valid JSON-RPC frames.
func TestOmniCoderSubprocess_StdioIsolation(t *testing.T) {
	tmpDir := t.TempDir()

	binPath := filepath.Join(tmpDir, "omnicoder_mcp_test.exe")
	buildCmd := exec.Command("go", "build", "-o", binPath, "../../cmd/omnicoder")
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build omnicoder binary: %v\nOutput: %s", err, string(buildOut))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "mcp", "--workspace", tmpDir)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to open stdin pipe: %v", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to open stdout pipe: %v", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start omnicoder process: %v", err)
	}

	stdoutReader := bufio.NewReader(stdoutPipe)

	// 1. Send initialize request
	initReq := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\"}}\n"
	if _, err := stdinPipe.Write([]byte(initReq)); err != nil {
		t.Fatalf("failed writing initialize to process: %v", err)
	}

	// 2. Read first response from stdout
	firstLine, err := stdoutReader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed reading stdout response: %v (stderr was: %s)", err, stderrBuf.String())
	}

	// Invariant: The VERY FIRST byte of stdout must belong to valid JSON-RPC.
	// It must NOT contain any banner text like "OmniCoder-ICX", "Workspace:", etc.
	trimmedFirst := strings.TrimSpace(firstLine)
	if strings.Contains(trimmedFirst, "OmniCoder-ICX") || strings.Contains(trimmedFirst, "Workspace:") {
		t.Fatalf("CRITICAL ISOLATION BREACH: Startup banner leaked into stdout! Line: %q", firstLine)
	}

	var initResp mcp.JSONRPCResponse
	if err := json.Unmarshal([]byte(trimmedFirst), &initResp); err != nil {
		t.Fatalf("stdout did not begin with valid JSON-RPC frame: %q (err: %v)", trimmedFirst, err)
	}
	if initResp.ID != float64(1) {
		t.Errorf("expected response ID 1, got %v", initResp.ID)
	}

	// 3. Send tools/call for omnicoder_status
	statusReq := "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"omnicoder_status\",\"arguments\":{}}}\n"
	if _, err := stdinPipe.Write([]byte(statusReq)); err != nil {
		t.Fatalf("failed writing status call: %v", err)
	}

	secondLine, err := stdoutReader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed reading second response: %v", err)
	}

	var statusResp mcp.JSONRPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(secondLine)), &statusResp); err != nil {
		t.Fatalf("second line was not valid JSON-RPC: %q (err: %v)", secondLine, err)
	}
	if statusResp.ID != float64(2) {
		t.Errorf("expected response ID 2, got %v", statusResp.ID)
	}

	// 4. Close stdin and wait for clean process exit
	_ = stdinPipe.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("process exited with error: %v (stderr: %s)", err, stderrBuf.String())
	}

	// 5. Verify stderr received the banner
	stderrContent := stderrBuf.String()
	if !strings.Contains(stderrContent, "OmniCoder-ICX MCP Server") {
		t.Errorf("expected stderr to contain 'OmniCoder-ICX MCP Server', got:\n%s", stderrContent)
	}
	if !strings.Contains(stderrContent, "Transport: stdio (JSON-RPC 2.0") {
		t.Errorf("expected stderr to contain 'Transport: stdio', got:\n%s", stderrContent)
	}
}
