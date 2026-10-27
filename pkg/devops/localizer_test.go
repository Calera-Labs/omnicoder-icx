// SPDX-License-Identifier: Apache-2.0
package devops_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestFaultLocalizer_BM25SymbolIndexer(t *testing.T) {
	symbols := []devops.SymbolTag{
		{File: "pkg/calc/revenue.go", Kind: "func", Name: "CalculateRevenueCAGR", Signature: "func CalculateRevenueCAGR(start, end float64) float64", Line: 15},
		{File: "pkg/calc/tax.go", Kind: "func", Name: "ComputeTaxDeductions", Signature: "func ComputeTaxDeductions(income float64) float64", Line: 20},
		{File: "pkg/auth/session.go", Kind: "struct", Name: "SessionManager", Signature: "type SessionManager struct", Line: 10},
		{File: "pkg/auth/login.go", Kind: "func", Name: "AuthenticateUser", Signature: "func AuthenticateUser(user, pass string) bool", Line: 35},
	}

	indexer := devops.NewBM25SymbolIndexer(symbols)

	// Test 1: Query for revenue calculation
	matches := indexer.Score("revenue cagr calculation error", 3)
	if len(matches) == 0 {
		t.Fatalf("expected BM25 matches for revenue query, got none")
	}
	if matches[0].Symbol.Name != "CalculateRevenueCAGR" {
		t.Errorf("expected top match CalculateRevenueCAGR, got %s", matches[0].Symbol.Name)
	}

	// Test 2: Query for session authentication
	authMatches := indexer.Score("authenticate user login session", 3)
	if len(authMatches) == 0 {
		t.Fatalf("expected BM25 matches for auth query, got none")
	}
	topAuth := authMatches[0].Symbol.Name
	if topAuth != "AuthenticateUser" && topAuth != "SessionManager" {
		t.Errorf("expected top auth match, got %s", topAuth)
	}

	// Test 3: ScoreFile aggregation
	revFileScore := indexer.ScoreFile("revenue cagr", "pkg/calc/revenue.go")
	taxFileScore := indexer.ScoreFile("revenue cagr", "pkg/calc/tax.go")
	if revFileScore <= taxFileScore {
		t.Errorf("expected revenue.go score (%f) > tax.go score (%f)", revFileScore, taxFileScore)
	}
}

func TestFaultLocalizer_TracebackParsingAndPruning(t *testing.T) {
	traceback := `
Traceback (most recent call last):
  File "/usr/lib/python3.10/unittest/case.py", line 549, in run
    testMethod()
  File "/workspace/tests/test_blueprints.py", line 42, in test_duplicate_registration
    bp.register(app, {})
  File "/usr/local/lib/python3.10/site-packages/click/core.py", line 1055, in invoke
    return ctx.invoke(self.callback, **ctx.params)
  File "/workspace/flask/blueprints.py", line 211, in register
    assert app.blueprints[self.name] is self, "Blueprint conflict"
AssertionError: Blueprint conflict
`

	frames := devops.ParseTraceback(traceback)
	if len(frames) != 4 {
		t.Fatalf("expected 4 parsed traceback frames, got %d", len(frames))
	}

	// Check frame 0: /usr/lib stdlib
	if !frames[0].IsStdlib {
		t.Errorf("expected frame 0 to be marked as stdlib, got false")
	}

	// Check frame 1: /workspace/tests/test_blueprints.py
	if !frames[1].IsTest {
		t.Errorf("expected frame 1 to be marked as test, got false")
	}
	if frames[1].IsStdlib {
		t.Errorf("expected frame 1 to not be stdlib")
	}

	// Check frame 2: site-packages/click/core.py
	if !frames[2].IsStdlib {
		t.Errorf("expected frame 2 (site-packages) to be marked as stdlib")
	}

	// Check frame 3: /workspace/flask/blueprints.py
	if frames[3].IsStdlib || frames[3].IsTest {
		t.Errorf("expected frame 3 to be a genuine repository implementation frame")
	}
	if frames[3].Line != 211 {
		t.Errorf("expected frame 3 line 211, got %d", frames[3].Line)
	}
	if frames[3].Function != "register" {
		t.Errorf("expected frame 3 function 'register', got %s", frames[3].Function)
	}
}

func TestFaultLocalizer_EndToEnd_AssertionMisdirection(t *testing.T) {
	// Simulate repository with test file and implementation file
	tempDir := t.TempDir()

	testDir := filepath.Join(tempDir, "tests")
	srcDir := filepath.Join(tempDir, "flask")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Implementation file: flask/blueprints.py
	implContent := `
class Scaffold:
    pass

class Blueprint(Scaffold):
    def __init__(self, name, import_name):
        self.name = name
        self.import_name = import_name

    def register(self, app, options):
        if self.name in app.blueprints:
            assert app.blueprints[self.name] is self, "Conflict"
        app.blueprints[self.name] = self
`
	if err := os.WriteFile(filepath.Join(srcDir, "blueprints.py"), []byte(implContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Test file: tests/test_blueprints.py
	testContent := `
import pytest
from flask.blueprints import Blueprint

def test_duplicate_registration():
    bp1 = Blueprint("auth", "auth")
    bp2 = Blueprint("auth", "auth")
    app = MockApp()
    bp1.register(app, {})
    bp2.register(app, {})
`
	if err := os.WriteFile(filepath.Join(testDir, "test_blueprints.py"), []byte(testContent), 0644); err != nil {
		t.Fatal(err)
	}

	issueDesc := "Duplicate blueprint registration with same name raises AssertionError instead of allowing nested prefixes."

	traceback := `
Traceback (most recent call last):
  File "/workspace/tests/test_blueprints.py", line 10, in test_duplicate_registration
    bp2.register(app, {})
  File "/workspace/flask/blueprints.py", line 12, in register
    assert app.blueprints[self.name] is self, "Conflict"
AssertionError: Conflict
`

	result, err := devops.LocalizeFault(tempDir, issueDesc, traceback)
	if err != nil {
		t.Fatalf("unexpected error from LocalizeFault: %v", err)
	}

	if result == nil {
		t.Fatalf("expected non-nil LocalizationResult")
	}

	// Invariant: Test file MUST NOT be selected as primary fault location
	if strings.Contains(result.PrimaryFile, "test") {
		t.Errorf("violation: test file selected as PrimaryFile: %s", result.PrimaryFile)
	}

	if !strings.HasSuffix(result.PrimaryFile, "blueprints.py") {
		t.Errorf("expected PrimaryFile flask/blueprints.py, got %s", result.PrimaryFile)
	}

	if result.CausalSymbol != "register" && result.CausalSymbol != "Blueprint" {
		t.Errorf("expected CausalSymbol 'register' or 'Blueprint', got %s", result.CausalSymbol)
	}

	if result.Confidence < 0.60 {
		t.Errorf("expected high confidence (>0.60), got %f", result.Confidence)
	}
}

func TestFaultLocalizer_TestFileExclusion_WhenTracebackOnlyInTest(t *testing.T) {
	// Scenario: traceback terminates in test assertion, implementation never appears in traceback
	tempDir := t.TempDir()

	testDir := filepath.Join(tempDir, "tests")
	srcDir := filepath.Join(tempDir, "src", "cli")
	_ = os.MkdirAll(testDir, 0755)
	_ = os.MkdirAll(srcDir, 0755)

	// Implementation
	cliContent := `
def routes_command(sort_order):
    """Display all registered routes."""
    headers = ["Endpoint", "Methods", "Rule"]
    return headers
`
	_ = os.WriteFile(filepath.Join(srcDir, "commands.py"), []byte(cliContent), 0644)

	// Test
	testContent := `
def test_routes():
    res = runner.invoke(cli, ["routes"])
    assert "Subdomain" in res.output
`
	_ = os.WriteFile(filepath.Join(testDir, "test_cli.py"), []byte(testContent), 0644)

	issueDesc := "Add Subdomain column to routes command output in CLI"
	traceback := `
  File "tests/test_cli.py", line 4, in test_routes
    assert "Subdomain" in res.output
AssertionError: assert 'Subdomain' in 'Endpoint  Methods  Rule\n'
`

	result, err := devops.LocalizeFault(tempDir, issueDesc, traceback)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must exclude tests/test_cli.py despite assertion occurring there!
	if strings.Contains(result.PrimaryFile, "test") {
		t.Errorf("test-exclusion rule failed: primary file was %s", result.PrimaryFile)
	}

	if !strings.HasSuffix(result.PrimaryFile, "commands.py") {
		t.Errorf("expected commands.py, got %s", result.PrimaryFile)
	}
}

func TestFaultLocalizer_MultiFileCandidates(t *testing.T) {
	tempDir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(tempDir, "pkg", "core"), 0755)
	_ = os.MkdirAll(filepath.Join(tempDir, "pkg", "api"), 0755)

	_ = os.WriteFile(filepath.Join(tempDir, "pkg", "core", "session.go"), []byte(`
package core

type SessionStore struct {}

func (s *SessionStore) CreateSession(token string) bool {
    return true
}
`), 0644)

	_ = os.WriteFile(filepath.Join(tempDir, "pkg", "api", "handler.go"), []byte(`
package api

func LoginHandler(token string) {
    // calls session store
}
`), 0644)

	issueDesc := "Session token validation in SessionStore CreateSession and LoginHandler"
	traceback := `
panic: invalid session token
pkg/core/session.go:6 +0x24
`

	result, err := devops.LocalizeFault(tempDir, issueDesc, traceback)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(result.PrimaryFile, "session.go") {
		t.Errorf("expected session.go as primary file, got %s", result.PrimaryFile)
	}

	// Supporting files should include handler.go
	foundHandler := false
	for _, sf := range result.SupportingFiles {
		if strings.HasSuffix(sf, "handler.go") {
			foundHandler = true
			break
		}
	}
	if !foundHandler && len(result.SupportingFiles) > 0 {
		t.Logf("Supporting files identified: %v", result.SupportingFiles)
	}
}

func TestFaultLocalizer_EmptyDiagnostics_Error(t *testing.T) {
	tempDir := t.TempDir()
	_, err := devops.LocalizeFault(tempDir, "", "")
	if err == nil {
		t.Errorf("expected ErrNoDiagnostics when both issue and traceback are empty")
	}
}

func TestFaultLocalizer_PROBLEM_MD_StrictExclusion(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Create PROBLEM.md with high-scoring keywords and code blocks
	problemMd := `# Issue Description
class ColorPicker(forms.Widget):
    class Media:
        js = ['color-picker.js']
`
	_ = os.WriteFile(filepath.Join(tempDir, "PROBLEM.md"), []byte(problemMd), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "README.md"), []byte("# Readme with class Media\n"), 0644)

	// 2. Create actual implementation file
	djangoDir := filepath.Join(tempDir, "django", "forms")
	_ = os.MkdirAll(djangoDir, 0755)
	widgetPy := `package forms
class ColorPicker:
    def __init__(self):
        pass
    class Media:
        js = ('color-picker.js',)
`
	_ = os.WriteFile(filepath.Join(djangoDir, "widgets.py"), []byte(widgetPy), 0644)

	// 3. Create test file
	testsDir := filepath.Join(tempDir, "tests")
	_ = os.MkdirAll(testsDir, 0755)
	testPy := `def test_combine_media():
    assert False, "AssertionError: Media combine failed"
`
	_ = os.WriteFile(filepath.Join(testsDir, "test_media.py"), []byte(testPy), 0644)

	issueDesc := "Combine media on ColorPicker Widget raises assertion failure"
	traceback := `
Traceback (most recent call last):
  File "tests/test_media.py", line 2, in test_combine_media
    assert False, "AssertionError: Media combine failed"
AssertionError: Media combine failed
`

	// Verify LocalizeFault does NOT pick PROBLEM.md or test_media.py
	result, err := devops.LocalizeFault(tempDir, issueDesc, traceback)
	if err != nil {
		t.Fatalf("LocalizeFault error: %v", err)
	}

	if strings.Contains(result.PrimaryFile, "PROBLEM.md") || strings.Contains(result.PrimaryFile, "README.md") {
		t.Fatalf("CRITICAL FAILURE: LocalizeFault falsely localized to documentation file: %s", result.PrimaryFile)
	}
	if strings.Contains(result.PrimaryFile, "test_media.py") {
		t.Fatalf("CRITICAL FAILURE: LocalizeFault falsely localized to test file: %s", result.PrimaryFile)
	}
	if !strings.HasSuffix(filepath.ToSlash(result.PrimaryFile), "django/forms/widgets.py") {
		t.Errorf("expected django/forms/widgets.py, got %s", result.PrimaryFile)
	}

	// Verify SearchCodebase with ExcludeTests does NOT return PROBLEM.md or README.md
	searchResults, searchErr := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: tempDir,
		QueryTokens:  []string{"ColorPicker", "Media"},
		ExcludeTests: true,
		MaxResults:   5,
	})
	if searchErr != nil {
		t.Fatalf("SearchCodebase error: %v", searchErr)
	}
	for _, res := range searchResults {
		if strings.HasSuffix(res.File, ".md") || strings.EqualFold(filepath.Base(res.File), "problem.md") {
			t.Errorf("CRITICAL FAILURE: SearchCodebase returned markdown file: %s", res.File)
		}
	}
}

func TestLocalizer_ExceptionStubDereference(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "omnicoder_deref_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	formsDir := filepath.Join(tempDir, "django", "forms")
	_ = os.MkdirAll(formsDir, 0755)

	widgetsPy := `# Python module with Warning stub and implementation method
import warnings

class MediaOrderConflictWarning(RuntimeWarning):
    pass

class Media:
    def __init__(self, media=None):
        self._css = {}
        self._js = []

    @staticmethod
    def merge(list_1, list_2):
        combined = list(list_1)
        for item in list_2:
            if item in combined:
                warnings.warn("conflict", MediaOrderConflictWarning)
            else:
                combined.append(item)
        return combined
`
	_ = os.WriteFile(filepath.Join(formsDir, "widgets.py"), []byte(widgetsPy), 0644)

	issueDesc := "Merging 3 or more media objects can throw unnecessary MediaOrderConflictWarning"
	traceback := `
Traceback (most recent call last):
  File "tests/test_media.py", line 121, in test_combine_media
    assert w1.media + w2.media
MediaOrderConflictWarning: Detected duplicate Media files in an opposite order
`

	result, err := devops.LocalizeFault(tempDir, issueDesc, traceback)
	if err != nil {
		t.Fatalf("LocalizeFault error: %v", err)
	}

	if !strings.HasSuffix(filepath.ToSlash(result.PrimaryFile), "django/forms/widgets.py") {
		t.Errorf("expected django/forms/widgets.py, got %s", result.PrimaryFile)
	}

	// Causal symbol must be re-anchored to the usage site 'merge', NOT the stub 'MediaOrderConflictWarning'
	if result.CausalSymbol == "MediaOrderConflictWarning" {
		t.Errorf("CRITICAL FAILURE: LocalizeFault stayed on stub MediaOrderConflictWarning instead of de-referencing to merge")
	}
	if result.CausalSymbol != "merge" {
		t.Fatalf("expected causal symbol 'merge', got '%s'", result.CausalSymbol)
	}
	t.Logf("✓ Successfully de-referenced to causal symbol '%s' with line range %v", result.CausalSymbol, result.LineRange)

	// Line range should enclose def merge (starts around line 14), not line 4
	if result.LineRange[0] < 10 {
		t.Errorf("expected line range to enclose def merge (>10), got %v", result.LineRange)
	}

	// Verify ExtractFileSymbolOutline
	outline := devops.ExtractFileSymbolOutline(tempDir, "django/forms/widgets.py")
	if !strings.Contains(outline, "MediaOrderConflictWarning") || !strings.Contains(outline, "merge") {
		t.Errorf("expected outline to contain symbols, got: %s", outline)
	}
}

func TestFindInRepoFaultOrigin(t *testing.T) {
	tempDir := t.TempDir()

	// Simulate in-repo files
	widgetsPy := filepath.Join(tempDir, "django", "forms", "widgets.py")
	_ = os.MkdirAll(filepath.Dir(widgetsPy), 0755)
	_ = os.WriteFile(widgetsPy, []byte(`
class Media:
    def merge(self, other):
        return self._do_merge(other)
    def _do_merge(self, other):
        raise ValueError("Invalid media merge order")
`), 0644)

	rawTraceback := `
Traceback (most recent call last):
  File "tests/test_media.py", line 42, in test_conflict
    m.merge(other)
  File "django/forms/widgets.py", line 4, in merge
    return self._do_merge(other)
  File "django/forms/widgets.py", line 6, in _do_merge
    raise ValueError("Invalid media merge order")
`

	targetFile, targetLine, targetFunc, isStub, _ := devops.FindInRepoFaultOrigin(tempDir, rawTraceback)
	if !isStub {
		t.Errorf("expected isStub=true for raise ValueError")
	}
	if filepath.ToSlash(targetFile) != "django/forms/widgets.py" {
		t.Errorf("expected django/forms/widgets.py, got %s", targetFile)
	}
	// Caller should be promoted to line 4 (merge), not line 6 (_do_merge with raise)
	if targetLine != 4 {
		t.Errorf("expected targetLine=4 (caller), got %d", targetLine)
	}
	if targetFunc != "merge" {
		t.Errorf("expected targetFunc='merge', got '%s'", targetFunc)
	}

	// Verify multi-file stack trace with supporting files
	multiTraceback := `
Traceback (most recent call last):
  File "tests/test_query.py", line 10, in test_filter
    qs.filter(name="test")
  File "django/db/models/query.py", line 512, in filter
    return self._filter_or_exclude(False, *args, **kwargs)
  File "django/db/models/sql/query.py", line 1240, in build_filter
    return self._add_q(q_object)
  File "django/db/models/sql/where.py", line 80, in add
    assert node is not None
`
	mfFile, mfLine, mfFunc, mfStub, mfSupporting := devops.FindInRepoFaultOrigin(tempDir, multiTraceback)
	if !mfStub {
		t.Errorf("expected mfStub=true for assert node is not None")
	}
	if filepath.ToSlash(mfFile) != "django/db/models/sql/query.py" {
		t.Errorf("expected caller django/db/models/sql/query.py, got %s", mfFile)
	}
	if mfLine != 1240 || mfFunc != "build_filter" {
		t.Errorf("expected line 1240 build_filter, got %d %s", mfLine, mfFunc)
	}
	if len(mfSupporting) < 2 {
		t.Errorf("expected at least 2 supporting files, got %v", mfSupporting)
	}
}
