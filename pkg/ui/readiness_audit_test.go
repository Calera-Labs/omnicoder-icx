// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
)

// TestExtractContent_UnclosedFenceAndEndMarker verifies that when a model outputs
// code without a matching closing fence or with trailing `>>>>`, ExtractContent isolates
// the clean code without trailing artifacts.
func TestExtractContent_UnclosedFenceAndEndMarker(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite}

	input := "Here is the implementation:\n[ACTION:WRITE path=\"extractor.py\"]\n```python\nimport re\n\ndef extract_names(text):\n    return re.findall(r'\\b[A-Z][a-z]+\\b', text)\n>>>>\n"
	extracted := intent.ExtractContent(input)

	if strings.Contains(extracted, ">>>>") {
		t.Errorf("ExtractContent should strip trailing >>>> end marker, got:\n%s", extracted)
	}
	if strings.Contains(extracted, "Here is the implementation") {
		t.Errorf("ExtractContent should not leak conversational preamble into extracted code, got:\n%s", extracted)
	}
	if !strings.Contains(extracted, "def extract_names(text):") {
		t.Errorf("ExtractContent failed to extract code block, got:\n%s", extracted)
	}
}

// TestAutoRepairPreflightSyntax_BoundarySlip verifies deterministic auto-repair
// when an LLM outputs a boundary marker or fence slip.
func TestAutoRepairPreflightSyntax_BoundarySlip(t *testing.T) {
	codeWithSlip := "```python\ndef calculate(a, b):\n    return a + b\n>>>>"
	repaired, didRepair, err := devops.AutoRepairPreflightSyntax(".", "math_util.py", codeWithSlip)
	if err != nil {
		t.Fatalf("AutoRepairPreflightSyntax failed: %v", err)
	}
	if !didRepair {
		t.Errorf("expected AutoRepairPreflightSyntax to repair boundary slip")
	}
	if strings.Contains(repaired, ">>>>") {
		t.Errorf("expected repaired code to omit >>>> marker, got:\n%s", repaired)
	}
	if strings.HasPrefix(strings.TrimSpace(repaired), "```") {
		t.Errorf("expected repaired code to omit leading fence, got:\n%s", repaired)
	}
}

// TestZeroTestsDetection verifies that Python 0-test runs are detected as non-passing.
func TestZeroTestsDetection(t *testing.T) {
	pyStdout := "----------------------------------------------------------------------\nRan 0 tests in 0.000s\n\nOK\n"
	if !isZeroTestsPass("python -m unittest discover", pyStdout, "") {
		t.Errorf("expected isZeroTestsPass to return true for 'Ran 0 tests in 0.000s - OK'")
	}

	pytestStdout := "============================= no tests ran in 0.01s =============================\n"
	if !isZeroTestsPass("pytest", pytestStdout, "") {
		t.Errorf("expected isZeroTestsPass to return true for 'no tests ran'")
	}

	realPassStdout := "----------------------------------------------------------------------\nRan 3 tests in 0.002s\n\nOK\n"
	if isZeroTestsPass("python -m unittest discover", realPassStdout, "") {
		t.Errorf("expected isZeroTestsPass to return false for real test pass")
	}
}

// TestValidateRequirementsTxt verifies strict rejection of markdown prose in requirements.txt.
func TestValidateRequirementsTxt(t *testing.T) {
	clean := "streamlit>=1.30.0\nnetworkx>=3.0\npytest>=7.0.0\n# comment line\n"
	valid, err := devops.ValidateRequirementsTxt(clean)
	if !valid || err != nil {
		t.Fatalf("expected clean requirements to pass validation: %v", err)
	}

	prose := "Here are the packages:\n- streamlit\n- pytest\nRun pip install to install them."
	valid, err = devops.ValidateRequirementsTxt(prose)
	if valid || err == nil {
		t.Fatalf("expected prose in requirements.txt to be rejected")
	}
}

// TestPythonPreHealing_InitAndModuleRecognition verifies __init__.py generation
// and suppression of hallucinated pip install on local module names.
func TestPythonPreHealing_InitAndModuleRecognition(t *testing.T) {
	tmpDir := t.TempDir()

	// Create tests/ directory without __init__.py
	testsDir := filepath.Join(tmpDir, "tests")
	_ = os.MkdirAll(testsDir, 0755)
	_ = os.WriteFile(filepath.Join(testsDir, "test_database.py"), []byte("def test_dummy(): pass\n"), 0644)

	// Run pre-healing
	healed := fixPythonModuleWithDiag(tmpDir, "ModuleNotFoundError: No module named 'database'")
	if !healed {
		t.Errorf("expected fixPythonModuleWithDiag to report healed action")
	}

	// Verify __init__.py was auto-created in tests/
	initPath := filepath.Join(testsDir, "__init__.py")
	if _, err := os.Stat(initPath); os.IsNotExist(err) {
		t.Errorf("expected tests/__init__.py to be auto-created")
	}

	// Verify database.py stub was created locally instead of running pip install database
	dbPath := filepath.Join(tmpDir, "database.py")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("expected local database.py stub to be created")
	}

	// Verify requirements.txt does NOT contain 'database'
	reqPath := filepath.Join(tmpDir, "requirements.txt")
	if data, err := os.ReadFile(reqPath); err == nil {
		if strings.Contains(string(data), "database") {
			t.Errorf("requirements.txt should NOT have 'database' appended: %s", string(data))
		}
	}
}

// TestScaffoldAndReadiness verifies end-to-end scaffolding and consumer readiness audit.
func TestScaffoldAndReadiness(t *testing.T) {
	tmpDir := t.TempDir()

	// Scaffold python project
	_, err := engine.ScaffoldProject(tmpDir, engine.ArchetypePythonStreamlit, "analytics-dashboard")
	if err != nil {
		t.Fatalf("ScaffoldProject failed: %v", err)
	}

	// Add basic app.py and passing test
	_ = os.WriteFile(filepath.Join(tmpDir, "app.py"), []byte("def main(): pass\nif __name__ == '__main__': main()\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "tests", "test_app.py"), []byte("def test_ok(): assert True\n"), 0644)

	ready, issues := engine.AuditConsumerReadiness(tmpDir, tmpDir)
	if !ready {
		t.Fatalf("expected consumer readiness to pass, got issues: %v", issues)
	}
}
