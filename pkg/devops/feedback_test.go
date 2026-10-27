// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// Tier 1 — Feature Coverage: StripANSI
func TestStripANSI(t *testing.T) {
	colored := "\x1b[31mError:\x1b[0m undefined variable \x1b[1;32m'config'\x1b[0m in \x1b[4mmain.go\x1b[0m"
	expected := "Error: undefined variable 'config' in main.go"
	res := devops.StripANSI(colored)
	if res != expected {
		t.Errorf("StripANSI mismatch:\nGot:      %q\nExpected: %q", res, expected)
	}

	clean := "Already clean text"
	if devops.StripANSI(clean) != clean {
		t.Errorf("StripANSI mutated clean text")
	}
}

// Tier 1 — Feature Coverage: Go Compiler Diagnostics
func TestParseCompilerDiagnostics_Go(t *testing.T) {
	sampleStderr := `
# github.com/example/server
pkg/server/router.go:42:15: undefined: UserHandler
pkg/server/router.go:55:2: cannot use cfg (variable of type Config) as string value in argument to InitDB
main.go:10: syntax error: unexpected newline
`
	diags := devops.ParseCompilerDiagnostics(sampleStderr, "go")
	if len(diags) != 3 {
		t.Fatalf("expected 3 diagnostics, got %d", len(diags))
	}

	if diags[0].File != "pkg/server/router.go" || diags[0].Line != 42 || diags[0].Column != 15 {
		t.Errorf("diagnostic 0 mismatch: %+v", diags[0])
	}
	if !strings.Contains(diags[0].Message, "undefined: UserHandler") {
		t.Errorf("diagnostic 0 message mismatch: %s", diags[0].Message)
	}

	if diags[1].Line != 55 || !strings.Contains(diags[1].Message, "cannot use cfg") {
		t.Errorf("diagnostic 1 mismatch: %+v", diags[1])
	}

	if diags[2].File != "main.go" || diags[2].Line != 10 || diags[2].Column != 0 {
		t.Errorf("diagnostic 2 mismatch: %+v", diags[2])
	}
}

func TestParseCompilerDiagnostics_GoPanic(t *testing.T) {
	samplePanic := `--- FAIL: TestEmptyListHasSizeZero (0.00s)
panic: Please implement the New function [recovered]
	panic: Please implement the New function

goroutine 19 [running]:
testing.tRunner.func1.2({0xe16880, 0xe79950})
	C:/Program Files/Go/src/testing/testing.go:1632 +0x225
testing.tRunner.func1()
	C:/Program Files/Go/src/testing/testing.go:1635 +0x359
panic({0xe16880?, 0xe79950?})
	C:/Program Files/Go/src/runtime/panic.go:785 +0x132
linkedlist.New(...)
	/workspace/simple_linked_list.go:6
linkedlist.TestEmptyListHasSizeZero(0xc000118680?)
	/workspace/simple_linked_list_test.go:11 +0x25
testing.tRunner(0xc000118680, 0xe4f9a0)
	C:/Program Files/Go/src/testing/testing.go:1690 +0xcb
exit status 2
FAIL	linkedlist	0.110s
`
	diags := devops.ParseCompilerDiagnostics(samplePanic, "go")
	if len(diags) == 0 {
		t.Fatalf("expected at least 1 diagnostic, got 0")
	}
	if diags[0].Line != 6 {
		t.Errorf("expected line 6, got %d", diags[0].Line)
	}
	if !strings.HasSuffix(diags[0].File, "simple_linked_list.go") {
		t.Errorf("expected file ending with simple_linked_list.go, got %s", diags[0].File)
	}
	if !strings.Contains(diags[0].Message, "Please implement the New function") {
		t.Errorf("expected panic message in diagnostic, got %s", diags[0].Message)
	}
}

// Tier 1 — Feature Coverage: TypeScript Diagnostics
func TestParseCompilerDiagnostics_TypeScript(t *testing.T) {
	sampleStderr := `
src/auth/service.ts(18,25): error TS2304: Cannot find name 'JwtSecret'.
src/auth/service.ts(30,10): warning TS6133: 'unusedToken' is declared but its value is never read.
src/components/button.tsx:42:5 - error TS1005: ';' expected.
`
	diags := devops.ParseCompilerDiagnostics(sampleStderr, "ts")
	if len(diags) != 3 {
		t.Fatalf("expected 3 diagnostics, got %d", len(diags))
	}

	if diags[0].File != "src/auth/service.ts" || diags[0].Line != 18 || diags[0].Column != 25 {
		t.Errorf("diagnostic 0 mismatch: %+v", diags[0])
	}
	if diags[0].ErrorCode != "TS2304" || diags[0].Severity != "ERROR" {
		t.Errorf("diagnostic 0 code/severity mismatch: %+v", diags[0])
	}

	if diags[1].ErrorCode != "TS6133" || diags[1].Severity != "WARNING" {
		t.Errorf("diagnostic 1 mismatch: %+v", diags[1])
	}

	if diags[2].File != "src/components/button.tsx" || diags[2].Line != 42 || diags[2].Column != 5 {
		t.Errorf("diagnostic 2 mismatch: %+v", diags[2])
	}
}

// Tier 1 — Feature Coverage: Rust Diagnostics
func TestParseCompilerDiagnostics_Rust(t *testing.T) {
	sampleStderr := `
error[E0425]: cannot find value 'unresolved_var' in this scope
  --> src/main.rs:14:5
warning[W0001]: unused variable 'x'
  --> src/lib.rs:22:9
`
	diags := devops.ParseCompilerDiagnostics(sampleStderr, "rust")
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(diags))
	}

	if diags[0].File != "src/main.rs" || diags[0].Line != 14 || diags[0].Column != 5 {
		t.Errorf("diagnostic 0 mismatch: %+v", diags[0])
	}
	if diags[0].ErrorCode != "E0425" || !strings.Contains(diags[0].Message, "cannot find value") {
		t.Errorf("diagnostic 0 message/code mismatch: %+v", diags[0])
	}

	if diags[1].File != "src/lib.rs" || diags[1].Line != 22 || diags[1].Severity != "WARNING" {
		t.Errorf("diagnostic 1 mismatch: %+v", diags[1])
	}
}

// Tier 1 — Feature Coverage: Python Diagnostics
func TestParseCompilerDiagnostics_Python(t *testing.T) {
	sampleStderr := `
app/models.py:24: error: Incompatible types in assignment
app/calc.py:10: warning: unused variable 'result'
  File "app/service.py", line 42, in run_handler
app/test_suite.py:15: AssertionError: expected 200 got 500
`
	diags := devops.ParseCompilerDiagnostics(sampleStderr, "python")
	if len(diags) != 4 {
		t.Fatalf("expected 4 diagnostics, got %d", len(diags))
	}

	if diags[0].File != "app/models.py" || diags[0].Line != 24 || diags[0].Severity != "ERROR" {
		t.Errorf("diagnostic 0 mismatch: %+v", diags[0])
	}
	if diags[1].File != "app/calc.py" || diags[1].Severity != "WARNING" {
		t.Errorf("diagnostic 1 mismatch: %+v", diags[1])
	}
	if diags[2].File != "app/service.py" || diags[2].Line != 42 || !strings.Contains(diags[2].Message, "run_handler") {
		t.Errorf("diagnostic 2 mismatch: %+v", diags[2])
	}
	if diags[3].File != "app/test_suite.py" || diags[3].ErrorCode != "AssertionError" {
		t.Errorf("diagnostic 3 mismatch: %+v", diags[3])
	}
}

// Tier 1 — Feature Coverage: C/C++ Diagnostics
func TestParseCompilerDiagnostics_CCpp(t *testing.T) {
	sampleStderr := `
src/main.cpp:25:10: error: 'vector' was not declared in this scope
include/util.h:12:5: warning: implicit conversion loses integer precision
src/engine.cpp(45,2): error C2065: 'Mesh': undeclared identifier
`
	diags := devops.ParseCompilerDiagnostics(sampleStderr, "c")
	if len(diags) != 3 {
		t.Fatalf("expected 3 diagnostics, got %d", len(diags))
	}

	if diags[0].File != "src/main.cpp" || diags[0].Line != 25 || diags[0].Column != 10 {
		t.Errorf("diagnostic 0 mismatch: %+v", diags[0])
	}
	if diags[1].File != "include/util.h" || diags[1].Severity != "WARNING" {
		t.Errorf("diagnostic 1 mismatch: %+v", diags[1])
	}
	if diags[2].File != "src/engine.cpp" || diags[2].Line != 45 || diags[2].ErrorCode != "C2065" {
		t.Errorf("diagnostic 2 mismatch: %+v", diags[2])
	}
}

// Tier 1 — Feature Coverage: 5-Line Source Window Context
func TestEnrichDiagnostics_5LineWindow(t *testing.T) {
	tmpDir := t.TempDir()
	sourceFile := filepath.Join(tmpDir, "calc.go")
	lines := []string{
		"package calc",     // Line 1
		"",                 // Line 2
		"func Multiply() {",// Line 3
		"    a := 10",      // Line 4
		"    b := 20",      // Line 5
		"    return a * b", // Line 6 (error here)
		"}",                // Line 7
		"",                 // Line 8
		"func Divide() {}", // Line 9
		"// EOF",           // Line 10
	}
	_ = os.WriteFile(sourceFile, []byte(strings.Join(lines, "\n")), 0644)

	mockStderr := "calc.go:6:5: too many return values"
	rep, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:                   "go build ./...",
		WorkspaceDir:              tmpDir,
		DeterministicMockStderr:   mockStderr,
		DeterministicMockExitCode: 1,
	})
	if err != nil {
		t.Fatalf("RunCompilerFeedbackLoop failed: %v", err)
	}

	if len(rep.Diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(rep.Diagnostics))
	}
	diag := rep.Diagnostics[0]
	if diag.OffendingLine != "return a * b" {
		t.Errorf("offending line mismatch: %s", diag.OffendingLine)
	}

	// Window should be [6-2, 6+2] = lines 4, 5, 6, 7, 8 (exactly 5 lines)
	if len(diag.ContextLines) != 5 {
		t.Fatalf("expected exactly 5 context lines, got %d", len(diag.ContextLines))
	}

	if !strings.HasPrefix(diag.ContextLines[2], "> ") {
		t.Errorf("error line 6 should have marker '> ', got: %s", diag.ContextLines[2])
	}
	if !strings.HasPrefix(diag.ContextLines[0], "  ") {
		t.Errorf("context line 4 should have marker '  ', got: %s", diag.ContextLines[0])
	}
}

// Tier 1 — Feature Coverage: Token Budget Clamping (<300 tokens)
func TestSynthesizeRepairPayload_TokenClamping(t *testing.T) {
	diags := []devops.CompilerDiagnostic{
		{
			File:         "pkg/heavy/service.go",
			Line:         100,
			Severity:     "ERROR",
			Message:      "massive type error with multiple parameter mismatches and conflicting interfaces",
			ContextLines: []string{"    98: a", "    99: b", ">  100: c", "   101: d", "   102: e"},
		},
	}
	// Add 20 extra diagnostics to simulate a massive failure cascade
	for i := 1; i <= 20; i++ {
		diags = append(diags, devops.CompilerDiagnostic{
			File:     "pkg/heavy/service.go",
			Line:     100 + i,
			Severity: "ERROR",
			Message:  "cascading syntax error",
		})
	}

	payload := devops.SynthesizeRepairPayload("go test ./...", 1, diags, "")
	tokenCount := devops.EstimateTokens(payload)

	if tokenCount >= 300 {
		t.Errorf("repair payload exceeded 300 tokens: estimated %d tokens, len=%d chars", tokenCount, len(payload))
	}
	if !strings.Contains(payload, "omitted") {
		t.Errorf("payload should indicate omitted diagnostics")
	}
}

// Tier 2 — Boundary & Corner Cases: File Bounds & Non-existent Files
func TestEnrichDiagnostics_Boundaries(t *testing.T) {
	tmpDir := t.TempDir()
	sourceFile := filepath.Join(tmpDir, "short.go")
	_ = os.WriteFile(sourceFile, []byte("package short\nfunc A() {}\n"), 0644) // 2 lines

	rep, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:                   "go test",
		WorkspaceDir:              tmpDir,
		DeterministicMockStderr:   "short.go:1:1: syntax error\nnon_existent.go:50:1: cannot find",
		DeterministicMockExitCode: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rep.Diagnostics) != 2 {
		t.Errorf("expected 2 diagnostics, got %d", len(rep.Diagnostics))
	}
	// short.go line 1 should have 2 context lines (lines 1 and 2)
	if len(rep.Diagnostics[0].ContextLines) != 2 {
		t.Errorf("expected 2 context lines for short file, got %d", len(rep.Diagnostics[0].ContextLines))
	}
}

// Concurrency safety
func TestFeedback_ConcurrencyRace(t *testing.T) {
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			stderr := "main.go:10:2: undefined: Foo\nservice.go:20:1: syntax error"
			diags := devops.ParseCompilerDiagnostics(stderr, "go")
			if len(diags) != 2 {
				t.Errorf("goroutine %d expected 2 diags, got %d", idx, len(diags))
			}
			payload := devops.SynthesizeRepairPayload("go build", 1, diags, "")
			if devops.EstimateTokens(payload) >= 300 {
				t.Errorf("goroutine %d payload exceeded token budget", idx)
			}
		}(i)
	}

	wg.Wait()
}

// TestRunCompilerFeedbackLoop_ChainedCdCommand verifies that chained cd commands with && resolve directories and execute.
func TestRunCompilerFeedbackLoop_ChainedCdCommand(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "subproject")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subproject dir: %v", err)
	}

	rep, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:      "cd subproject && echo test_ok",
		WorkspaceDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error running feedback loop with cd: %v", err)
	}
	if !rep.Success || rep.ExitCode != 0 {
		t.Fatalf("expected command to succeed, got exit code %d, stderr: %s", rep.ExitCode, rep.RawStderr)
	}
	if !strings.Contains(strings.ToLower(rep.RawStdout), "test_ok") {
		t.Errorf("expected stdout to contain 'test_ok', got: %q", rep.RawStdout)
	}
}

// TestRunCompilerFeedbackLoop_ChainedExecutionCompound verifies compound execution with && and environment variables.
func TestRunCompilerFeedbackLoop_ChainedExecutionCompound(t *testing.T) {
	tmpDir := t.TempDir()

	rep, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:      "echo part_alpha && echo part_beta",
		WorkspaceDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rep.Success || rep.ExitCode != 0 {
		t.Fatalf("expected success, got exit code %d, stderr: %s", rep.ExitCode, rep.RawStderr)
	}
	if !strings.Contains(rep.RawStdout, "part_alpha") || !strings.Contains(rep.RawStdout, "part_beta") {
		t.Errorf("expected stdout to contain both parts, got: %q", rep.RawStdout)
	}
}

// TestRunCompilerFeedbackLoop_CFlagAndScopeNormalization verifies that -C flags and redundant scope paths are normalized.
func TestRunCompilerFeedbackLoop_CFlagAndScopeNormalization(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "my-app")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("failed to create projDir: %v", err)
	}

	// 1. Test -C from parent workspace dir
	rep, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:      "echo ok_c_flag -C projects/my-app",
		WorkspaceDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("feedback loop error: %v", err)
	}
	if !rep.Success || rep.ExitCode != 0 {
		t.Fatalf("expected success, got exit code %d, stderr: %s", rep.ExitCode, rep.RawStderr)
	}

	// 2. Test redundant -C when WorkspaceDir is already inside projects/my-app
	rep2, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:      "echo ok_redundant_c -C projects/my-app",
		WorkspaceDir: projDir,
	})
	if err != nil {
		t.Fatalf("feedback loop error: %v", err)
	}
	if !rep2.Success || rep2.ExitCode != 0 {
		t.Fatalf("expected success, got exit code %d, stderr: %s", rep2.ExitCode, rep2.RawStderr)
	}
}

func TestRunCompilerFeedbackLoop_CaleraMicroVM(t *testing.T) {
	tempWs := t.TempDir()
	sbClient := vm.NewHermeticLocalSandbox(tempWs)

	rep, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:          "echo microvm_execution_verified",
		WorkspaceDir:     tempWs,
		ExecutionBackend: "Calera_MicroVM",
		VMClient:         sbClient,
	})
	if err != nil {
		t.Fatalf("feedback loop microvm error: %v", err)
	}
	if !rep.Success || rep.ExitCode != 0 {
		t.Fatalf("expected microvm success, got exit code %d, stderr: %s", rep.ExitCode, rep.RawStderr)
	}
	if rep.ExecutionEngine != "Calera_MicroVM_Hermetic" {
		t.Errorf("expected execution engine 'Calera_MicroVM_Hermetic', got %q", rep.ExecutionEngine)
	}
	if !strings.Contains(rep.RawStdout, "microvm_execution_verified") {
		t.Errorf("expected stdout to contain 'microvm_execution_verified', got: %q", rep.RawStdout)
	}
}

func TestInjectGoTestTimeout(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		timeoutSec int
		expected   string
	}{
		{
			name:       "Standard go test",
			cmd:        "go test ./...",
			timeoutSec: 30,
			expected:   "go test -timeout=25s ./...",
		},
		{
			name:       "Chained cd and go test with flags",
			cmd:        "cd projects/widget-service && go test -v -count=1 ./...",
			timeoutSec: 30,
			expected:   "cd projects/widget-service && go test -timeout=25s -v -count=1 ./...",
		},
		{
			name:       "Existing timeout flag preserved",
			cmd:        "go test -timeout=10s -v ./...",
			timeoutSec: 60,
			expected:   "go test -timeout=10s -v ./...",
		},
		{
			name:       "Existing double-dash timeout flag preserved",
			cmd:        "go test --timeout 15s ./...",
			timeoutSec: 60,
			expected:   "go test --timeout 15s ./...",
		},
		{
			name:       "Clamped to maximum 45s for long outer timeout",
			cmd:        "go test ./...",
			timeoutSec: 120,
			expected:   "go test -timeout=45s ./...",
		},
		{
			name:       "Clamped to minimum 5s for short outer timeout",
			cmd:        "go test ./...",
			timeoutSec: 8,
			expected:   "go test -timeout=5s ./...",
		},
		{
			name:       "Non-test command unchanged",
			cmd:        "go build -o app ./cmd/app",
			timeoutSec: 30,
			expected:   "go build -o app ./cmd/app",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := devops.InjectGoTestTimeout(tc.cmd, tc.timeoutSec)
			if res != tc.expected {
				t.Errorf("InjectGoTestTimeout mismatch:\nGot:      %q\nExpected: %q", res, tc.expected)
			}
		})
	}
}

func TestParseCompilerDiagnostics_GoDeadlock(t *testing.T) {
	sampleDeadlockOutput := `panic: test timed out after 10s
running tests:
	TestWorkerPoolDispatch (10.00s)

goroutine 1 [chan receive]:
testing.(*T).Run(0xc00010c1e0, {0x8d5c40, 0x1f}, 0x8e8268)
	C:/Go/src/testing/testing.go:1750 +0x3bf
testing.tRunner(0xc00010c0e0, 0x8e8258)
	C:/Go/src/testing/testing.go:1689 +0xfb
main.main()
	_testmain.go:57 +0x1aa

goroutine 20 [sync.Mutex.Lock]:
sync.runtime_SemacquireMutex(0xc000108008, 0x0, 0x1)
	C:/Go/src/runtime/sema.go:77 +0x25
sync.(*Mutex).Lock(0xc000108008)
	C:/Go/src/sync/mutex.go:90 +0x45
github.com/example/workerservice/pkg/worker.(*Worker).resetTimeout(0xc000108000)
	/workspace/pkg/worker/worker.go:88 +0x65
github.com/example/workerservice/pkg/worker.(*Worker).Dispatch(0xc000108000)
	/workspace/pkg/worker/worker.go:145 +0x120
exit status 2
FAIL	github.com/example/workerservice/pkg/worker	10.150s
`

	diags := devops.ParseCompilerDiagnostics(sampleDeadlockOutput, "go")
	if len(diags) == 0 {
		t.Fatalf("expected at least 1 diagnostic, got 0")
	}

	foundLine88 := false
	for _, d := range diags {
		if strings.HasSuffix(d.File, "worker.go") && d.Line == 88 {
			foundLine88 = true
			if !strings.Contains(d.Message, "sync.Mutex.Lock") {
				t.Errorf("expected diagnostic message to mention sync.Mutex.Lock, got: %s", d.Message)
			}
			if !strings.Contains(d.Message, "TestWorkerPoolDispatch") {
				t.Errorf("expected diagnostic message to identify TestWorkerPoolDispatch, got: %s", d.Message)
			}
		}
	}

	if !foundLine88 {
		t.Errorf("expected diagnostic pointing to raft.go line 88 where mutex lock blocked, got: %+v", diags)
	}
}

func TestParseCompilerDiagnostics_JestAndVitest(t *testing.T) {
	sampleJestOutput := `
FAIL src/components/Button.test.tsx
  ● Button component › renders with primary variant

    expect(received).toBe(expected) // Object.is equality

    Expected: "btn-primary"
    Received: "btn-default"

      40 |   it('renders with primary variant', () => {
      41: const { container } = render(<Button variant="primary" />);
    > 42 |     expect(container.firstChild).toHaveClass('btn-primary');
         |                                  ^
      43 |   });

      at Object.<anonymous> (src/components/Button.test.tsx:42:34)

Test Suites: 1 failed, 1 total
Tests:       1 failed, 1 total
`

	diags := devops.ParseCompilerDiagnostics(sampleJestOutput, "typescript")
	if len(diags) == 0 {
		t.Fatalf("expected at least 1 diagnostic from Jest output, got 0")
	}

	foundButton := false
	for _, d := range diags {
		if strings.HasSuffix(d.File, "Button.test.tsx") && d.Line == 42 {
			foundButton = true
			if !strings.Contains(d.Message, "Button component") {
				t.Errorf("expected diagnostic message to mention test name, got: %s", d.Message)
			}
		}
	}
	if !foundButton {
		t.Errorf("expected diagnostic pointing to Button.test.tsx:42, got: %+v", diags)
	}

	sampleVitestOutput := `
 ✕ tests/api/user.test.ts > User API > returns 401 on missing token
   Error: expected 401 but received 500
    at tests/api/user.test.ts:18:7
`
	vitestDiags := devops.ParseCompilerDiagnostics(sampleVitestOutput, "typescript")
	if len(vitestDiags) == 0 {
		t.Fatalf("expected at least 1 diagnostic from Vitest output, got 0")
	}
	if !strings.HasSuffix(vitestDiags[0].File, "user.test.ts") || vitestDiags[0].Line != 18 {
		t.Errorf("expected Vitest diagnostic on user.test.ts:18, got: %+v", vitestDiags[0])
	}
}
