package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBinaryE2E_ConversationalSlashCommands tests the compiled omnicoder.exe
// binary end-to-end as a real OS subprocess, exercising:
// 1. Auto-Write detection from assistant turns
// 2. /save <path> with quotes and nested directories
// 3. Exact file contents on disk
// 4. Verification of the lowercase hex SHA-256 seal printed in terminal badges
func TestBinaryE2E_ConversationalSlashCommands(t *testing.T) {
	// Locate omnicoder binary in repository root or build into t.TempDir()
	binName := "omnicoder"
	if runtime.GOOS == "windows" {
		binName = "omnicoder.exe"
	}
	exePath, err := filepath.Abs(filepath.Join("..", "..", binName))
	if err != nil {
		t.Fatalf("Failed to resolve omnicoder binary path: %v", err)
	}
	if _, err := os.Stat(exePath); os.IsNotExist(err) {
		exePath = filepath.Join(t.TempDir(), binName)
		buildCmd := exec.Command("go", "build", "-o", exePath, "../../cmd/omnicoder")
		buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("Failed to build temporary omnicoder binary: %v\n%s", err, string(out))
		}
	}

	// Setup mock OpenAI/Gemini compatible completion server
	expectedContent := "package autowrite\n\nfunc Version() string { return \"1.0.0\" }\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}

		resp := map[string]interface{}{
			"id":      "chatcmpl-binary-e2e",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gemini-3.5-flash-lite",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "[ACTION:WRITE path=\"autowrite/pkg.go\"]\n````go\n" + expectedContent + "````",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     40,
				"completion_tokens": 25,
				"total_tokens":      65,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Isolated temporary workspace for this test run
	tmpWorkspace := t.TempDir()

	// Prepare subprocess commands
	stdinPayload := "Please generate the version package.\n" +
		"/save \"manual_save/custom_pkg.go\"\n" +
		"/exit\n"

	cmd := exec.Command(exePath, "chat",
		"--base-url", server.URL+"/v1",
		"--workspace", tmpWorkspace,
		"--api-key", "mock-e2e-key",
	)
	cmd.Stdin = strings.NewReader(stdinPayload)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err = cmd.Run()
	if err != nil {
		t.Fatalf("omnicoder.exe execution failed: %v\nStderr: %s\nStdout: %s", err, stderrBuf.String(), stdoutBuf.String())
	}

	stdoutText := stdoutBuf.String()

	// 1. Verify Auto-Write output in stdout
	if !strings.Contains(stdoutText, "[Auto-Write Detected]") {
		t.Fatalf("Expected [Auto-Write Detected] in stdout, got:\n%s", stdoutText)
	}
	if !strings.Contains(stdoutText, "Target File: autowrite/pkg.go") {
		t.Fatalf("Expected 'Target File: autowrite/pkg.go' in stdout, got:\n%s", stdoutText)
	}

	// 2. Verify auto-written file on disk
	autoWriteFile := filepath.Join(tmpWorkspace, "autowrite", "pkg.go")
	autoData, err := os.ReadFile(autoWriteFile)
	if err != nil {
		t.Fatalf("Auto-written file not found at %s: %v", autoWriteFile, err)
	}
	if string(autoData) != expectedContent {
		t.Fatalf("Auto-written file content mismatch.\nGot:\n%q\nExpected:\n%q", string(autoData), expectedContent)
	}

	// 3. Verify SHA-256 seal of auto-written file
	autoHash := sha256.Sum256(autoData)
	autoSeal := hex.EncodeToString(autoHash[:])
	if !strings.Contains(stdoutText, autoSeal) {
		t.Fatalf("stdout does not contain SHA-256 seal %s for auto-written file.\nStdout:\n%s", autoSeal, stdoutText)
	}

	// 4. Verify /save manually saved file on disk
	manualFile := filepath.Join(tmpWorkspace, "manual_save", "custom_pkg.go")
	manualData, err := os.ReadFile(manualFile)
	if err != nil {
		t.Fatalf("Manually saved file not found at %s: %v", manualFile, err)
	}
	if string(manualData) != expectedContent {
		t.Fatalf("Manually saved file content mismatch.\nGot:\n%q\nExpected:\n%q", string(manualData), expectedContent)
	}

	// 5. Verify SHA-256 seal of manually saved file
	manualHash := sha256.Sum256(manualData)
	manualSeal := hex.EncodeToString(manualHash[:])
	if !strings.Contains(stdoutText, manualSeal) {
		t.Fatalf("stdout does not contain SHA-256 seal %s for manually saved file.\nStdout:\n%s", manualSeal, stdoutText)
	}

	// 6. Verify [File Created] badges
	if strings.Count(stdoutText, "[File Created]") < 2 {
		t.Fatalf("Expected at least 2 [File Created] badges in stdout, got %d.\nStdout:\n%s",
			strings.Count(stdoutText, "[File Created]"), stdoutText)
	}
}
