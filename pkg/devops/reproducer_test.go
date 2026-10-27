// SPDX-License-Identifier: Apache-2.0
package devops_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestSynthesizeReproducer_FencedPythonCode(t *testing.T) {
	tempDir := t.TempDir()

	issueText := `
Fix calculation bug in revenue projection.
When calling calculate(), it should return 100, but raises AssertionError.

` + "```python\n" + `assert 1 + 1 == 3, "Math error: 1 + 1 != 3"` + "\n```"

	scriptPath, err := devops.SynthesizeReproducer(issueText, tempDir)
	if err != nil {
		t.Fatalf("unexpected error from SynthesizeReproducer: %v", err)
	}

	if filepath.Base(scriptPath) != "reproduce_issue.py" {
		t.Errorf("expected script name reproduce_issue.py, got %s", filepath.Base(scriptPath))
	}

	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("failed to read synthesized script: %v", err)
	}
	sContent := string(content)

	if !strings.Contains(sContent, "assert 1 + 1 == 3") {
		t.Errorf("script content missing target assertion snippet: %s", sContent)
	}
	if !strings.Contains(sContent, "test_issue()") {
		t.Errorf("script content missing test_issue wrapper: %s", sContent)
	}
	if !strings.Contains(sContent, "sys.exit(1)") || !strings.Contains(sContent, "sys.exit(0)") {
		t.Errorf("script content missing protocol exit codes: %s", sContent)
	}

	// Execution check: 1 + 1 == 3 fails with AssertionError -> exit code 1
	fails := devops.VerifyReproducerFails(scriptPath)
	if !fails {
		t.Errorf("expected VerifyReproducerFails to return true for failing assertion, got false")
	}

	passes := devops.VerifyReproducerPasses(scriptPath)
	if passes {
		t.Errorf("expected VerifyReproducerPasses to return false for failing assertion, got true")
	}
}

func TestSynthesizeReproducer_Doctest(t *testing.T) {
	tempDir := t.TempDir()

	issueText := `
The parser fails on zero division:
>>> val = 42
>>> div = 0
>>> result = val / div
`

	scriptPath, err := devops.SynthesizeReproducer(issueText, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("failed to read script: %v", err)
	}
	sContent := string(content)

	if !strings.Contains(sContent, "val = 42") || !strings.Contains(sContent, "val / div") {
		t.Errorf("script content missing doctest statements: %s", sContent)
	}

	// Zero division raises ZeroDivisionError -> exit code 1
	if !devops.VerifyReproducerFails(scriptPath) {
		t.Errorf("expected VerifyReproducerFails to return true for zero division doctest")
	}
}

func TestSynthesizeReproducer_Traceback(t *testing.T) {
	tempDir := t.TempDir()

	issueText := `
Exception observed in pipeline:
Traceback (most recent call last):
  File "math_util.py", line 12, in compute_ratio
ValueError: invalid input
`

	scriptPath, err := devops.SynthesizeReproducer(issueText, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("failed to read script: %v", err)
	}
	sContent := string(content)

	if !strings.Contains(sContent, "math_util") {
		t.Errorf("script content missing extracted module reference: %s", sContent)
	}
}

func TestSynthesizeReproducer_EmptyIssue(t *testing.T) {
	tempDir := t.TempDir()

	_, err := devops.SynthesizeReproducer("", tempDir)
	if err == nil {
		t.Fatal("expected error for empty issue text, got nil")
	}
	if !errorsIs(err, devops.ErrEmptyIssueText) {
		t.Errorf("expected ErrEmptyIssueText, got %v", err)
	}
}

func TestVerifyReproducer_FullLifecycle(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Create buggy target module in repo
	moduleFile := filepath.Join(tempDir, "calculator.py")
	buggyCode := `
def add_positive(a, b):
    # BUG: subtracting instead of adding
    return a - b
`
	if err := os.WriteFile(moduleFile, []byte(buggyCode), 0644); err != nil {
		t.Fatalf("failed to create target module: %v", err)
	}

	// 2. Synthesize reproducer oracle from issue
	issueText := `
Bug: add_positive(2, 3) returns -1 instead of 5.

` + "```python\n" + `from calculator import add_positive
res = add_positive(2, 3)
assert res == 5, f"Expected 5, got {res}"
` + "```"

	scriptPath, err := devops.SynthesizeReproducer(issueText, tempDir)
	if err != nil {
		t.Fatalf("failed to synthesize reproducer: %v", err)
	}

	// 3. Pre-Repair Verification Gate:
	// MUST fail with exit code 1 (clean reproduction)
	preFails := devops.VerifyReproducerFails(scriptPath)
	if !preFails {
		t.Fatalf("PRE-REPAIR GATE FAILED: reproducer did not fail with exit code 1 on buggy code!")
	}
	prePasses := devops.VerifyReproducerPasses(scriptPath)
	if prePasses {
		t.Fatalf("PRE-REPAIR GATE FAILED: reproducer incorrectly passed on buggy code!")
	}

	// 4. Perform code repair on target module
	fixedCode := `
def add_positive(a, b):
    # FIXED: correctly adding
    return a + b
`
	if err := os.WriteFile(moduleFile, []byte(fixedCode), 0644); err != nil {
		t.Fatalf("failed to update module: %v", err)
	}

	// 5. Post-Repair Verification Gate:
	// MUST pass with exit code 0
	postFails := devops.VerifyReproducerFails(scriptPath)
	if postFails {
		t.Fatalf("POST-REPAIR GATE FAILED: reproducer still fails after bug was fixed!")
	}
	postPasses := devops.VerifyReproducerPasses(scriptPath)
	if !postPasses {
		t.Fatalf("POST-REPAIR GATE FAILED: reproducer did not pass with exit code 0 after fix!")
	}
}

func TestVerifyReproducer_SyntaxErrorRejection(t *testing.T) {
	tempDir := t.TempDir()

	// Script with syntax error (invalid Python)
	scriptPath := filepath.Join(tempDir, "reproduce_issue.py")
	brokenScript := `
import sys
def invalid_syntax(((:
    print("invalid")
`
	if err := os.WriteFile(scriptPath, []byte(brokenScript), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	// Although Python exits with 1 on syntax error, VerifyReproducerFails
	// MUST reject it (return false).
	if devops.VerifyReproducerFails(scriptPath) {
		t.Errorf("INVARIANT VIOLATION: VerifyReproducerFails must return false when script has syntax error!")
	}
	if devops.VerifyReproducerPasses(scriptPath) {
		t.Errorf("VerifyReproducerPasses must return false for broken syntax script")
	}
}

func TestVerifyReproducer_Exit0_RejectedInPreRepair(t *testing.T) {
	tempDir := t.TempDir()

	// Script that immediately passes (bug not present or test incorrect)
	scriptPath := filepath.Join(tempDir, "reproduce_issue.py")
	passingScript := `
import sys
print("All good")
sys.exit(0)
`
	if err := os.WriteFile(scriptPath, []byte(passingScript), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	// Pre-repair gate requires exit code 1; exit 0 MUST be rejected (return false)
	if devops.VerifyReproducerFails(scriptPath) {
		t.Errorf("INVARIANT VIOLATION: VerifyReproducerFails must return false when script exits with 0!")
	}
	// Post-repair gate requires exit 0
	if !devops.VerifyReproducerPasses(scriptPath) {
		t.Errorf("VerifyReproducerPasses must return true when script exits with 0")
	}
}

func TestVerifyReproducer_NonExistentScript(t *testing.T) {
	fakePath := filepath.Join(t.TempDir(), "nonexistent_reproducer.py")

	if devops.VerifyReproducerFails(fakePath) {
		t.Errorf("VerifyReproducerFails should return false for nonexistent file")
	}
	if devops.VerifyReproducerPasses(fakePath) {
		t.Errorf("VerifyReproducerPasses should return false for nonexistent file")
	}
}

func TestVerifyReproducer_TimeoutEnforcement(t *testing.T) {
	tempDir := t.TempDir()

	scriptPath := filepath.Join(tempDir, "reproduce_issue.py")
	hangScript := `
import time
import sys
time.sleep(10)
sys.exit(1)
`
	if err := os.WriteFile(scriptPath, []byte(hangScript), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	ctx := context.Background()
	res, err := devops.RunReproducerScript(ctx, scriptPath, 300*time.Millisecond)
	if err == nil && !res.TimedOut {
		t.Errorf("expected script to time out within 300ms")
	}
}

func TestCustomSynthesizerHook(t *testing.T) {
	tempDir := t.TempDir()

	devops.SetCustomSynthesizer(func(issueText string, repoDir string) (string, error) {
		target := filepath.Join(repoDir, "custom_reproduce.py")
		content := "#!/usr/bin/env python3\nimport sys\nsys.exit(1)\n"
		if err := os.WriteFile(target, []byte(content), 0755); err != nil {
			return "", err
		}
		return target, nil
	})
	defer devops.SetCustomSynthesizer(nil)

	scriptPath, err := devops.SynthesizeReproducer("some issue", tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filepath.Base(scriptPath) != "custom_reproduce.py" {
		t.Errorf("expected custom_reproduce.py, got %s", filepath.Base(scriptPath))
	}
}

func TestSynthesizeReproducer_GoLanguage(t *testing.T) {
	tempDir := t.TempDir()

	// Create go.mod
	goMod := "module example.com/testpkg\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	issueText := "Package testpkg has issue in TestFoo"
	scriptPath, err := devops.SynthesizeReproducer(issueText, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filepath.Base(scriptPath) != "reproduce_issue_test.go" {
		t.Errorf("expected reproduce_issue_test.go, got %s", filepath.Base(scriptPath))
	}
}

func errorsIs(err, target error) bool {
	if err == nil || target == nil {
		return err == target
	}
	return err.Error() == target.Error() || strings.Contains(err.Error(), target.Error())
}
