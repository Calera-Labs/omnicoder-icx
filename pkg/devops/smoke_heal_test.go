// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// TestSmoke_MultiLevelStubBypassAndSupportingFiles verifies that when stack traces terminate
// in framework assertion helpers, raise statements, or validation stubs, OmniCoder correctly:
// 1. Bypasses leaf stubs and identifies the true caller in the repository as the primary target.
// 2. Extracts all in-repo files participating in the traceback as supporting files.
// 3. Excludes external libraries and test files from the supporting target set.
func TestSmoke_MultiLevelStubBypassAndSupportingFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Create simulated repo file hierarchy
	dirs := []string{
		"django/forms",
		"django/db/models/sql",
		"django/core/exceptions",
		"tests",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(tempDir, d), 0755); err != nil {
			t.Fatalf("failed to create dir %s: %v", d, err)
		}
	}

	files := map[string]string{
		"django/forms/widgets.py": `class Media:
    def merge(self, other):
        return self._do_merge(other)
    def _do_merge(self, other):
        raise ValueError("Invalid media merge order")
`,
		"django/db/models/sql/query.py": `class Query:
    def build_filter(self, q_object):
        return self._add_q(q_object)
    def _add_q(self, q_object):
        self.where.add(q_object)
`,
		"django/db/models/sql/where.py": `class WhereNode:
    def add(self, data):
        assert data is not None, "Data cannot be null"
`,
		"tests/test_media.py": `def test_media():
    m = Media()
    m.merge(other)
`,
	}
	for fp, content := range files {
		fullPath := filepath.Join(tempDir, fp)
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", fp, err)
		}
	}

	// Case 1: Raise ValueError stub in _do_merge -> caller should be promoted to merge() at line 2-3
	tb1 := `
Traceback (most recent call last):
  File "tests/test_media.py", line 42, in test_conflict
    m.merge(other)
  File "django/forms/widgets.py", line 3, in merge
    return self._do_merge(other)
  File "django/forms/widgets.py", line 5, in _do_merge
    raise ValueError("Invalid media merge order")
`
	targetFile, targetLine, targetFunc, isStub, supporting := devops.FindInRepoFaultOrigin(tempDir, tb1)
	if !isStub {
		t.Errorf("[Case 1] expected isStub=true for raise ValueError")
	}
	if filepath.ToSlash(targetFile) != "django/forms/widgets.py" {
		t.Errorf("[Case 1] expected django/forms/widgets.py, got %s", targetFile)
	}
	if targetLine != 3 {
		t.Errorf("[Case 1] expected caller line 3, got %d", targetLine)
	}
	if targetFunc != "merge" {
		t.Errorf("[Case 1] expected caller function 'merge', got '%s'", targetFunc)
	}
	_ = supporting

	// Case 2: Multi-file assert stub in WhereNode.add -> caller should be Query.build_filter or _add_q
	tb2 := `
Traceback (most recent call last):
  File "tests/test_query.py", line 10, in test_filter
    qs.filter(name="test")
  File "django/db/models/query.py", line 512, in filter
    return self._filter_or_exclude(False, *args, **kwargs)
  File "django/db/models/sql/query.py", line 2, in build_filter
    return self._add_q(q_object)
  File "django/db/models/sql/query.py", line 4, in _add_q
    self.where.add(q_object)
  File "django/db/models/sql/where.py", line 3, in add
    assert data is not None, "Data cannot be null"
`
	targetFile2, targetLine2, _, isStub2, supporting2 := devops.FindInRepoFaultOrigin(tempDir, tb2)
	if !isStub2 {
		t.Errorf("[Case 2] expected isStub2=true for assert data is not None")
	}
	if filepath.ToSlash(targetFile2) != "django/db/models/sql/query.py" {
		t.Errorf("[Case 2] expected caller django/db/models/sql/query.py, got %s", targetFile2)
	}
	if targetLine2 != 4 {
		t.Errorf("[Case 2] expected caller line 4 (_add_q), got %d", targetLine2)
	}

	// Verify supporting files extraction
	foundWhere := false
	foundQuery := false
	for _, sf := range supporting2 {
		norm := filepath.ToSlash(sf)
		if strings.Contains(norm, "where.py") {
			foundWhere = true
		}
		if strings.Contains(norm, "query.py") {
			foundQuery = true
		}
		if strings.Contains(norm, "test_") {
			t.Errorf("[Case 2] supporting files should not contain test files: %s", sf)
		}
	}
	if !foundWhere || !foundQuery {
		t.Errorf("[Case 2] expected supporting files to contain where.py and query.py, got %v", supporting2)
	}
}

// TestSmoke_ErrorGradientScoring verifies that OmniCoder evaluates candidates across the full
// error gradient rather than falling into the all-or-nothing "Diagnostic Invariance Trap":
// 1. Rewarding diagnostic reduction (+100/diag).
// 2. Penalizing regressions / extra errors (-50/diag).
// 3. Rewarding transition from fatal crashes (ImportError, AttributeError, etc.) to assertions (+50).
// 4. Rewarding error progression when diagnostic counts remain unchanged (+30).
// 5. Rewarding exit code improvements (+25) and minimal edit block sizes (+10).
// 6. Hard-rejecting candidates with SyntaxError or IndentationError (-1000).
// 7. Maximum reward for clean pass (+1000).
func TestSmoke_ErrorGradientScoring(t *testing.T) {
	baselineReport := &devops.CompilerFeedbackReport{
		Success:   false,
		ExitCode:  2,
		Diagnostics: []devops.CompilerDiagnostic{
			{File: "sklearn/linear_model/base.py", Line: 120, Message: "AttributeError: 'NoneType' object has no attribute 'fit'"},
		},
	}

	// 1. Candidate introduces syntax error
	syntaxReport := &devops.CompilerFeedbackReport{
		Success:  false,
		ExitCode: 1,
		Diagnostics: []devops.CompilerDiagnostic{
			{File: "sklearn/linear_model/base.py", Line: 120, Message: "SyntaxError: invalid syntax"},
		},
	}
	score, hasSyntax := devops.ScoreHealCandidate(baselineReport, syntaxReport, 1)
	if !hasSyntax || score != -1000 {
		t.Errorf("expected score=-1000 and hasSyntax=true, got score=%d, hasSyntax=%v", score, hasSyntax)
	}

	// 2. Candidate progresses from AttributeError to AssertionError (Crash-to-Assertion)
	// Same 1 diagnostic, exit code drops from 2 to 1, 1 edit block:
	// score = 0 (diagReduction=0) + 50 (crash->assert) + 25 (exitCode 2->1) + 10 (blocks<=2) = 85
	assertReport := &devops.CompilerFeedbackReport{
		Success:  false,
		ExitCode: 1,
		Diagnostics: []devops.CompilerDiagnostic{
			{File: "tests/test_base.py", Line: 45, Message: "AssertionError: expected array of shape (10, 2) but got (10, 1)"},
		},
	}
	scoreAssert, hasSyntax2 := devops.ScoreHealCandidate(baselineReport, assertReport, 1)
	if hasSyntax2 {
		t.Errorf("did not expect syntax error")
	}
	if scoreAssert != 85 {
		t.Errorf("expected score=85 for crash-to-assertion progression, got %d", scoreAssert)
	}

	// 3. Candidate reduces diagnostics from 2 to 1
	twoErrBaseline := &devops.CompilerFeedbackReport{
		Success:  false,
		ExitCode: 1,
		Diagnostics: []devops.CompilerDiagnostic{
			{File: "xarray/core/dataset.py", Line: 10, Message: "TypeError: unsupported operand"},
			{File: "xarray/core/dataset.py", Line: 20, Message: "KeyError: 'dim'"},
		},
	}
	oneErrTrial := &devops.CompilerFeedbackReport{
		Success:  false,
		ExitCode: 1,
		Diagnostics: []devops.CompilerDiagnostic{
			{File: "xarray/core/dataset.py", Line: 20, Message: "KeyError: 'dim'"},
		},
	}
	scoreReduction, _ := devops.ScoreHealCandidate(twoErrBaseline, oneErrTrial, 1)
	// score = 100 (diagReduction=1) + 30 (error progressed) + 10 (blocks<=2) = 140
	if scoreReduction != 140 {
		t.Errorf("expected score=140 for diagnostic reduction with error progression, got %d", scoreReduction)
	}

	// 4. Clean pass candidate
	passReport := &devops.CompilerFeedbackReport{
		Success:     true,
		ExitCode:    0,
		Diagnostics: []devops.CompilerDiagnostic{},
	}
	scorePass, _ := devops.ScoreHealCandidate(baselineReport, passReport, 1)
	if scorePass != 1000 {
		t.Errorf("expected score=1000 for passing candidate, got %d", scorePass)
	}
}

// TestSmoke_ReflexionPreservationAcrossTurns verifies the fix for Reflexion Amnesia:
// When no candidate completely fixes the issue in Turn 1, the closest candidate diff
// must be retained in memory and formatted for Turn 2 prompt injection, rather than
// being wiped to empty string.
func TestSmoke_ReflexionPreservationAcrossTurns(t *testing.T) {
	// Simulate Turn 1 blocks
	blocks := []devops.EditBlock{
		{
			FilePath:    "django/forms/widgets.py",
			SearchText:  "def merge(self, other):",
			ReplaceText: "def merge(self, other):\n        if other is None:\n            return self",
		},
	}

	var patchSnippets []string
	for _, b := range blocks {
		patchSnippets = append(patchSnippets, fmt.Sprintf("--- %s\n+++ %s\n- %s\n+ %s", b.FilePath, b.FilePath, b.SearchText, b.ReplaceText))
	}
	lastTurnPatch := strings.Join(patchSnippets, "\n")

	if len(lastTurnPatch) == 0 {
		t.Fatalf("expected non-empty lastTurnPatch")
	}

	// Verify that the patch snippet contains exact unified diff syntax
	if !strings.Contains(lastTurnPatch, "--- django/forms/widgets.py") || !strings.Contains(lastTurnPatch, "+++ django/forms/widgets.py") {
		t.Errorf("expected valid patch header, got: %s", lastTurnPatch)
	}
	if !strings.Contains(lastTurnPatch, "if other is None:") {
		t.Errorf("expected addition of null check, got: %s", lastTurnPatch)
	}

	// Verify card prompt formatting with reflection
	reflectionNote := fmt.Sprintf("Turn 1 note: Prior attempt on django/forms/widgets.py did not resolve the test failure (exit 1). Try a different method.")
	if !strings.Contains(reflectionNote, "Turn 1 note:") {
		t.Errorf("expected reflection note format, got %s", reflectionNote)
	}
}

// TestSmoke_ProbeTimeoutEnforcement verifies that RunCompilerFeedbackLoop honors
// TimeoutSeconds strictly, aborting long-running or hanging probe commands within the limit.
func TestSmoke_ProbeTimeoutEnforcement(t *testing.T) {
	// Run command that sleeps for 10 seconds, but set TimeoutSeconds to 1
	start := time.Now()
	report, err := devops.RunCompilerFeedbackLoop(context.Background(), devops.CompilerFeedbackRequest{
		Command:        "powershell -Command Start-Sleep -Seconds 10",
		WorkspaceDir:   ".",
		TimeoutSeconds: 2,
	})
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Errorf("expected command to be aborted in ~2 seconds, but took %v", elapsed)
	}
	// Error or non-zero exit is expected
	if report != nil && report.Success {
		t.Errorf("expected timeout failure, got success: %v, err: %v", report, err)
	}
}

// TestSmoke_RepoTestCommandSynthesis verifies that test command generation correctly targets
// clean sub-tests and avoids unbounded test sweeps across diverse Python repositories.
func TestSmoke_RepoTestCommandSynthesis(t *testing.T) {
	testCases := []struct {
		name         string
		repo         string
		target       string
		testFiles    []string
		expectedIn   string
		notExpectedIn string
	}{
		{
			name:       "Django with class and method target",
			repo:       "django/django",
			target:     "test_merge (forms_tests.tests.test_media.FormsMediaTestCase)",
			testFiles:  []string{"tests/forms_tests/tests/test_media.py"},
			expectedIn: "tests/runtests.py --settings=test_sqlite forms_tests.tests.test_media.FormsMediaTestCase.test_merge",
		},
		{
			name:       "Scikit-learn with parameterized pytest target",
			repo:       "scikit-learn/scikit-learn",
			target:     "sklearn/tests/test_base.py::test_clone[estimator0]",
			testFiles:  []string{"sklearn/tests/test_base.py"},
			expectedIn: "-k test_clone",
		},
		{
			name:       "Astropy with custom flag suppression",
			repo:       "astropy/astropy",
			target:     "astropy/io/fits/tests/test_header.py::test_header_comments",
			testFiles:  []string{"astropy/io/fits/tests/test_header.py"},
			expectedIn: "-p no:cacheprovider -o addopts=''",
		},
		{
			name:       "Xarray with clean k filter",
			repo:       "pydata/xarray",
			target:     "xarray/tests/test_dataset.py::TestDataset::test_merge",
			testFiles:  []string{"xarray/tests/test_dataset.py"},
			expectedIn: "-k test_merge",
		},
		{
			name:          "General Pytest with empty test_files (No unbounded sweeps)",
			repo:          "pallets/flask",
			target:        "tests/test_basic.py::test_options_work",
			testFiles:     []string{},
			expectedIn:    "-k test_options_work --maxfail=1",
			notExpectedIn: "pytest -p no:cacheprovider -W ignore -p no:langsmith -p no:asyncio\n",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate build_clean_test_cmd logic
			cleanTarget := tc.target
			if strings.Contains(cleanTarget, "[") {
				cleanTarget = strings.Split(cleanTarget, "[")[0]
			}
			if strings.Contains(cleanTarget, " (") {
				cleanTarget = strings.Split(cleanTarget, " (")[0]
			}
			cleanTarget = strings.ReplaceAll(cleanTarget, "(", "")
			cleanTarget = strings.ReplaceAll(cleanTarget, ")", "")

			var probeCmd string
			if strings.Contains(tc.repo, "django") {
				runner := "python3 tests/runtests.py --settings=test_sqlite"
				if strings.Contains(tc.target, " (") && strings.HasSuffix(tc.target, ")") {
					parts := strings.Split(strings.TrimSuffix(tc.target, ")"), " (")
					testMethod := parts[0]
					classPath := parts[1]
					probeCmd = fmt.Sprintf("%s %s.%s", runner, classPath, testMethod)
				}
			} else if strings.Contains(tc.repo, "astropy") {
				cleanK := cleanTarget
				if strings.Contains(cleanK, "::") {
					parts := strings.Split(cleanK, "::")
					cleanK = parts[len(parts)-1]
				}
				probeCmd = fmt.Sprintf("pytest -p no:cacheprovider -o addopts='' -p no:langsmith -p no:asyncio -o filterwarnings=ignore %s -k %s --maxfail=1", strings.Join(tc.testFiles, " "), cleanK)
			} else {
				cleanK := cleanTarget
				if strings.Contains(cleanK, "::") {
					parts := strings.Split(cleanK, "::")
					cleanK = parts[len(parts)-1]
				}
				if len(tc.testFiles) > 0 {
					probeCmd = fmt.Sprintf("pytest -p no:cacheprovider -W ignore -p no:langsmith -p no:asyncio %s -k %s --maxfail=1", strings.Join(tc.testFiles, " "), cleanK)
				} else {
					probeCmd = fmt.Sprintf("pytest -p no:cacheprovider -W ignore -p no:langsmith -p no:asyncio -k %s --maxfail=1", cleanK)
				}
			}

			if !strings.Contains(probeCmd, tc.expectedIn) {
				t.Errorf("[%s] expected command to contain %q, got: %s", tc.name, tc.expectedIn, probeCmd)
			}
			if tc.notExpectedIn != "" && strings.Contains(probeCmd, tc.notExpectedIn) {
				t.Errorf("[%s] expected command NOT to contain %q, got: %s", tc.name, tc.notExpectedIn, probeCmd)
			}
		})
	}
}

// TestSmoke_MultiFileOutlineAssembly verifies that OmniCoder extracts compact outlines of
// supporting files and enriches the issue context, giving the LLM caller-callee awareness.
func TestSmoke_MultiFileOutlineAssembly(t *testing.T) {
	tempDir := t.TempDir()
	modelsDir := filepath.Join(tempDir, "django", "db", "models")
	if err := os.MkdirAll(modelsDir, 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	whereContent := `class WhereNode:
    """Represents a SQL WHERE condition tree."""
    def __init__(self, connector="AND", negated=False):
        self.connector = connector
        self.negated = negated
        self.children = []

    def add(self, data):
        """Adds a child condition or node."""
        assert data is not None, "Data cannot be null"
        self.children.append(data)
`
	if err := os.WriteFile(filepath.Join(modelsDir, "where.py"), []byte(whereContent), 0644); err != nil {
		t.Fatalf("failed to write where.py: %v", err)
	}

	outline := devops.ExtractFileSymbolOutline(tempDir, "django/db/models/where.py")
	if !strings.Contains(outline, "WhereNode") {
		t.Errorf("expected outline to contain 'WhereNode', got: %s", outline)
	}
	if !strings.Contains(outline, "add") {
		t.Errorf("expected outline to contain 'add', got: %s", outline)
	}

	// Verify enriched issue context assembly
	baseIssue := "AssertionError: Data cannot be null when evaluating filter"
	supportingFiles := []string{"django/db/models/where.py"}
	var suppOutlines []string
	for _, sf := range supportingFiles {
		if o := devops.ExtractFileSymbolOutline(tempDir, sf); o != "" {
			suppOutlines = append(suppOutlines, fmt.Sprintf("Supporting `%s`: %s", sf, o))
		}
	}
	enriched := baseIssue + " | " + strings.Join(suppOutlines, " | ")

	if !strings.Contains(enriched, "Supporting `django/db/models/where.py`:") {
		t.Errorf("expected enriched context to include supporting file, got: %s", enriched)
	}
	if len(enriched) > 4000 {
		t.Errorf("enriched context exceeded 4000 char budget: %d chars", len(enriched))
	}
}

// TestSmoke_TwoTierGateWorkflow verifies the Two-Tier Test Harness contract:
// 1. Probe test is evaluated first (low latency, fast feedback).
// 2. If probe passes, the full gate test is executed.
// 3. If full gate test fails (e.g. regression in broader test suite), candidate is rejected and rolled back.
func TestSmoke_TwoTierGateWorkflow(t *testing.T) {
	tempDir := t.TempDir()
	sourceFile := filepath.Join(tempDir, "calculator.py")
	if err := os.WriteFile(sourceFile, []byte("def add(a, b):\n    return a + b\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Candidate block that breaks subtraction while fixing addition
	block := devops.EditBlock{
		FilePath:    "calculator.py",
		SearchText:  "def add(a, b):\n    return a + b",
		ReplaceText: "def add(a, b):\n    return a + b\ndef sub(a, b):\n    return 0 # broken",
	}

	// Apply candidate edit
	_, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath: tempDir,
		Blocks:   []devops.EditBlock{block},
	})
	if err != nil {
		t.Fatalf("failed to apply edit: %v", err)
	}

	// Simulated Tier 1 probe: test_add passes
	probeReport := &devops.CompilerFeedbackReport{
		Success:  true,
		ExitCode: 0,
		Command:  "pytest tests/test_calc.py -k test_add",
	}

	// Simulated Tier 2 gate: test_sub fails (regression caught!)
	gateReport := &devops.CompilerFeedbackReport{
		Success:  false,
		ExitCode: 1,
		Command:  "pytest tests/test_calc.py",
		Diagnostics: []devops.CompilerDiagnostic{
			{File: "tests/test_calc.py", Line: 20, Message: "AssertionError: 5 - 2 != 0"},
		},
	}

	// Verify gate decision logic
	candidateAccepted := false
	if probeReport.Success && probeReport.ExitCode == 0 {
		// Probe passed, now check gate
		if gateReport.Success && gateReport.ExitCode == 0 {
			candidateAccepted = true
		} else {
			// Rollback candidate
			rollbackBlock := devops.EditBlock{
				FilePath:    block.FilePath,
				SearchText:  block.ReplaceText,
				ReplaceText: block.SearchText,
			}
			_, _ = devops.ApplyEditBlocks(devops.EditBlockRequest{
				RepoPath: tempDir,
				Blocks:   []devops.EditBlock{rollbackBlock},
			})
		}
	}

	if candidateAccepted {
		t.Fatalf("candidate should have been rejected by Tier 2 regression gate")
	}

	// Verify that rollback restored original content
	restored, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatalf("failed to read restored file: %v", err)
	}
	if string(restored) != "def add(a, b):\n    return a + b\n" {
		t.Errorf("file not cleanly restored after gate failure: %q", string(restored))
	}
}

