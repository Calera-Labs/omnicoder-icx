// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSolveLeakageTripwire(t *testing.T) {
	markers := []string{
		"Here is the issue.\nFAIL_TO_PASS: ['tests/test_foo.py::test_bar']",
		"Problem description with PASS_TO_PASS leaked",
		"Contains test_patch content",
		"diff --git a/tests/test_core.py b/tests/test_core.py",
	}

	for _, leakedText := range markers {
		tmpDir := t.TempDir()
		issuePath := filepath.Join(tmpDir, "ISSUE.md")
		if err := os.WriteFile(issuePath, []byte(leakedText), 0644); err != nil {
			t.Fatalf("failed to write ISSUE.md: %v", err)
		}

		cfg := defaultGlobalConfig()
		cfg.Workspace = tmpDir
		cfg.Offline = true

		err := executeSolve(context.Background(), &cfg, []string{"--issue-file", issuePath})
		if err == nil {
			t.Fatalf("expected executeSolve to fail on leaked text %q, got nil", leakedText)
		}
		if !errors.Is(err, errLeakageDetected) {
			t.Fatalf("expected errLeakageDetected, got: %v", err)
		}
	}
}

func initTempGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	runGit("init", "-q")
	runGit("config", "user.email", "support@caleralabs.com")
	runGit("config", "user.name", "OmniCoderTest")
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "initial commit")
	return dir
}

func TestSolveReproducerAndHealEndToEnd(t *testing.T) {
	buggyCode := `def safe_divide(a, b):
    if b == 0:
        raise ZeroDivisionError("cannot divide by zero")
    return a / b
`
	repoDir := initTempGitRepo(t, map[string]string{
		"calc/__init__.py": "",
		"calc/divider.py":  buggyCode,
	})

	issueMD := `# Bug in calc.divider.safe_divide
When b is 0, safe_divide(a, 0) raises ZeroDivisionError instead of returning 0.0.

Reproduction:
` + "```python\nfrom calc.divider import safe_divide\nassert safe_divide(10, 0) == 0.0\n```\n"

	issuePath := filepath.Join(repoDir, "ISSUE.md")
	if err := os.WriteFile(issuePath, []byte(issueMD), 0644); err != nil {
		t.Fatalf("write ISSUE.md: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respPayload := map[string]any{
			"id":      "chatcmpl-test-solve-1",
			"object":  "chat.completion",
			"created": 1700000000,
			"model":   "mock-gemma4",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role": "assistant",
						"content": `{"root_cause":"safe_divide raises ZeroDivisionError instead of returning 0.0","file_path":"calc/divider.py","edit_blocks":[{"file_path":"calc/divider.py","search_text":"    if b == 0:\n        raise ZeroDivisionError(\"cannot divide by zero\")","replace_text":"    if b == 0:\n        return 0.0"}],"repair_summary":"Return 0.0 when divisor is zero"}`,
					},
					"finish_reason": "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respPayload)
	}))
	defer server.Close()

	cfg := defaultGlobalConfig()
	cfg.Workspace = repoDir
	cfg.BaseURL = server.URL
	cfg.Model = "mock-gemma4"
	cfg.MaxTurns = 2
	cfg.BeamK = 1

	patchOut := filepath.Join(repoDir, "model.patch")
	err := executeSolve(context.Background(), &cfg, []string{
		"--workspace", repoDir,
		"--issue-file", issuePath,
		"--repro-attempts", "1",
		"--patch-out", patchOut,
	})
	if err != nil {
		t.Fatalf("executeSolve failed: %v", err)
	}

	patchBytes, err := os.ReadFile(patchOut)
	if err != nil {
		t.Fatalf("failed to read model.patch: %v", err)
	}
	patchStr := string(patchBytes)
	if !strings.Contains(patchStr, "diff --git a/calc/divider.py b/calc/divider.py") {
		t.Fatalf("expected model.patch to modify calc/divider.py, got:\n%s", patchStr)
	}
	if !strings.Contains(patchStr, "+        return 0.0") {
		t.Fatalf("expected model.patch to include '+        return 0.0', got:\n%s", patchStr)
	}
	if strings.Contains(patchStr, "reproduce_issue") || strings.Contains(patchStr, ".omnicoder") || strings.Contains(patchStr, "ISSUE.md") {
		t.Fatalf("model.patch must not contain scratch/reproducer files, got:\n%s", patchStr)
	}
}

func TestSolveFallbackWhenNoReproducerFails(t *testing.T) {
	buggyCode := `def format_greeting(name):
    return "Hello," + name
`
	repoDir := initTempGitRepo(t, map[string]string{
		"pkg/__init__.py":  "",
		"pkg/formatter.py": buggyCode,
	})

	issueMD := `In pkg/formatter.py, format_greeting(name) is missing a space after the comma.`
	issuePath := filepath.Join(repoDir, "ISSUE.md")
	if err := os.WriteFile(issuePath, []byte(issueMD), 0644); err != nil {
		t.Fatalf("write ISSUE.md: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		rawBytes, _ := json.Marshal(reqBody)
		rawStr := string(rawBytes)

		var content string
		if strings.Contains(rawStr, "GateReproducer") {
			// Return a reproducer that exits 0 (fails the pre-repair oracle gate)
			content = `{"script_name":"reproduce_issue.py","script_content":"import sys\nsys.exit(0)\n","explanation":"noop"}`
		} else {
			content = `{"root_cause":"Missing space after comma in format_greeting","file_path":"pkg/formatter.py","edit_blocks":[{"file_path":"pkg/formatter.py","search_text":"def format_greeting(name):\n    return \"Hello,\" + name","replace_text":"def format_greeting(name):\n    return \"Hello, \" + name"}],"repair_summary":"Add space after comma"}`
		}

		respPayload := map[string]any{
			"id":      "chatcmpl-test-solve-2",
			"object":  "chat.completion",
			"created": 1700000000,
			"model":   "mock-gemma4",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": content,
					},
					"finish_reason": "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respPayload)
	}))
	defer server.Close()

	cfg := defaultGlobalConfig()
	cfg.Workspace = repoDir
	cfg.BaseURL = server.URL
	cfg.Model = "mock-gemma4"
	cfg.MaxTurns = 1
	cfg.BeamK = 1

	patchOut := filepath.Join(repoDir, "model.patch")
	err := executeSolve(context.Background(), &cfg, []string{
		"--workspace", repoDir,
		"--issue-file", issuePath,
		"--repro-attempts", "1",
		"--patch-out", patchOut,
	})
	if err != nil {
		t.Fatalf("executeSolve fallback failed: %v", err)
	}

	patchBytes, err := os.ReadFile(patchOut)
	if err != nil {
		t.Fatalf("failed to read model.patch: %v", err)
	}
	patchStr := string(patchBytes)
	if !strings.Contains(patchStr, "diff --git a/pkg/formatter.py b/pkg/formatter.py") {
		t.Fatalf("expected fallback repair to patch pkg/formatter.py, got:\n%s", patchStr)
	}
	if !strings.Contains(patchStr, `+    return "Hello, " + name`) {
		t.Fatalf("expected fallback repair diff to include fixed greeting, got:\n%s", patchStr)
	}
}
