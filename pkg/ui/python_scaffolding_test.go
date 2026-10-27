// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestDetectProjectVerificationCmd_PythonScaffoldingDeadlock verifies that:
// 1. When a Python project is being scaffolded (e.g. requirements.txt or pyproject.toml exists),
//    DetectProjectVerificationCmd returns "python -m compileall -q ." so syntax is verified without
//    failing on missing tests (which returns exit code 5 from unittest discover).
// 2. As soon as a test file is created (test_*.py or tests/test_*.py), it switches to unittest or pytest.
func TestDetectProjectVerificationCmd_PythonScaffoldingDeadlock(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "py-test-app")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("failed to create projDir: %v", err)
	}

	// Step 1: Scaffolding with requirements.txt and app.py (no tests yet)
	_ = os.WriteFile(filepath.Join(projDir, "requirements.txt"), []byte("requests>=2.28.0\n"), 0644)
	_ = os.WriteFile(filepath.Join(projDir, "app.py"), []byte("print('hello world')\n"), 0644)

	cmd1 := DetectProjectVerificationCmd(projDir)
	if cmd1 != "python -m compileall -q ." {
		t.Errorf("expected scaffolding command 'python -m compileall -q .', got %q", cmd1)
	}

	// Step 2: Now add a test file (test_app.py)
	_ = os.WriteFile(filepath.Join(projDir, "test_app.py"), []byte("import unittest\nclass T(unittest.TestCase):\n    def test_ok(self): pass\n"), 0644)

	cmd2 := DetectProjectVerificationCmd(projDir)
	if !strings.Contains(cmd2, "unittest discover") {
		t.Errorf("expected test runner command after test_app.py added, got %q", cmd2)
	}

	// Step 3: Pytest project check
	_ = os.WriteFile(filepath.Join(projDir, "test_app.py"), []byte("import pytest\ndef test_ok(): pass\n"), 0644)
	cmd3 := DetectProjectVerificationCmd(projDir)
	if cmd3 != "python -m compileall -q . && pytest" {
		t.Errorf("expected pytest runner command, got %q", cmd3)
	}
}

// TestDetectRootExerciseFiles_And_InPlaceRemap verifies that when starter/test files exist at workspace root:
// 1. detectRootExerciseFiles discovers them.
// 2. Nested actions targeting projects/<name>/<rootfile> are remapped in-place to <rootfile>.
func TestDetectRootExerciseFiles_And_InPlaceRemap(t *testing.T) {
	tmpDir := t.TempDir()

	// Seed root exercise files (e.g. benchmark case like polyglot-go-react)
	rootGo := filepath.Join(tmpDir, "react.go")
	rootTestGo := filepath.Join(tmpDir, "react_test.go")
	_ = os.WriteFile(rootGo, []byte("package react\n\n// starter stub\n"), 0644)
	_ = os.WriteFile(rootTestGo, []byte("package react\n\nimport \"testing\"\n\nfunc TestReact(t *testing.T) {}\n"), 0644)

	found := detectRootExerciseFiles(tmpDir)
	if len(found) != 2 {
		t.Fatalf("expected 2 root exercise files, found %d: %v", len(found), found)
	}

	s := NewSession(nil, nil, tmpDir)
	s.SetActiveProject(".")

	if s.ActiveProject() != "." {
		t.Errorf("expected active project to be '.', got %q", s.ActiveProject())
	}

	// Verify HarmonizeProjectActions returns "." as canonicalDir when ActiveProject is "."
	canonicalDir, _ := HarmonizeProjectActions(s, nil)
	if canonicalDir != "." {
		t.Errorf("expected HarmonizeProjectActions to return '.', got %q", canonicalDir)
	}

	// Verify ScopePath with "." returns un-nested relative path
	scoped := ScopePath(".", "react.go")
	if scoped != "react.go" {
		t.Errorf("expected ScopePath('.', 'react.go') to be 'react.go', got %q", scoped)
	}
}

// TestCircuitBreaker_ZeroTestsExemption verifies that zero-test outputs do not increment
// identicalDiagCount or trip the circuit breaker during scaffolding turns.
func TestCircuitBreaker_ZeroTestsExemption(t *testing.T) {
	diagnostics := []string{
		"Ran 0 tests in 0.000s\nNO TESTS RAN",
		"collected 0 items\nno tests ran in 0.01s",
		"Auto-verification executed 0 tests ([no test files])",
	}

	for _, diag := range diagnostics {
		isZeroTestsDiag := zeroTestsExecuted(diag) ||
			strings.Contains(diag, "NO TESTS RAN") ||
			strings.Contains(diag, "Ran 0 tests") ||
			strings.Contains(diag, "executed 0 tests") ||
			strings.Contains(diag, "collected 0 items") ||
			strings.Contains(diag, "no tests ran")

		if !isZeroTestsDiag {
			t.Errorf("expected %q to be recognized as zero tests diagnostic", diag)
		}
	}
}

// TestCodePreservationGuard_PythonCleanRewrite verifies that valid Python files shrinking
// by >40% during a clean refactoring are accepted and NOT rejected by the Code Preservation Guard.
func TestCodePreservationGuard_PythonCleanRewrite(t *testing.T) {
	tmpDir := t.TempDir()
	appDir := filepath.Join(tmpDir, "projects", "pyapp")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	pyPath := filepath.Join(appDir, "utils.py")
	origPy := `# Legacy bloated utility file with many verbose comments and dead code
` + strings.Repeat("def unused_helper_func_%d():\n    # Legacy helper comment\n    pass\n\n", 20)
	if err := os.WriteFile(pyPath, []byte(origPy), 0644); err != nil {
		t.Fatal(err)
	}

	cleanPy := `def add(a: int, b: int) -> int:
    return a + b
`
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `Refactoring utils.py:
[ACTION:WRITE path="projects/pyapp/utils.py"]
` + "```python\n" + cleanPy + "\n```\n"

	session := NewSession(mockClient, nil, tmpDir)
	in := strings.NewReader("refactor utils.py\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	t.Logf("UI Output:\n%s", out.String())
	if strings.Contains(out.String(), "rejected incoming truncated write") {
		t.Errorf("expected clean Python write NOT to be rejected by Code Preservation Guard, got:\n%s", out.String())
	}
	savedPy, err := os.ReadFile(pyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(savedPy), "def add(a: int, b: int) -> int:") {
		t.Errorf("expected new clean Python code to be saved, got:\n%s", string(savedPy))
	}
}

