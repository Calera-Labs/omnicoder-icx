// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScaffoldProject_PythonStreamlit(t *testing.T) {
	tmpDir := t.TempDir()
	created, err := ScaffoldProject(tmpDir, ArchetypePythonStreamlit, "analytics-dashboard")
	if err != nil {
		t.Fatalf("ScaffoldProject failed: %v", err)
	}

	if _, ok := created["requirements.txt"]; !ok {
		t.Errorf("expected requirements.txt in created map")
	}
	if _, ok := created["tests/__init__.py"]; !ok {
		t.Errorf("expected tests/__init__.py in created map")
	}
	if _, ok := created["tests/conftest.py"]; !ok {
		t.Errorf("expected tests/conftest.py in created map")
	}

	reqData, err := os.ReadFile(filepath.Join(tmpDir, "requirements.txt"))
	if err != nil {
		t.Fatalf("failed to read requirements.txt: %v", err)
	}
	if len(reqData) == 0 {
		t.Errorf("requirements.txt should not be empty")
	}

	conftestData, err := os.ReadFile(filepath.Join(tmpDir, "tests", "conftest.py"))
	if err != nil {
		t.Fatalf("failed to read conftest.py: %v", err)
	}
	if len(conftestData) == 0 {
		t.Errorf("conftest.py should not be empty")
	}
}

func TestScaffoldProject_GoCLI(t *testing.T) {
	tmpDir := t.TempDir()
	created, err := ScaffoldProject(tmpDir, ArchetypeGoCLI, "my-tool")
	if err != nil {
		t.Fatalf("ScaffoldProject failed: %v", err)
	}

	if _, ok := created["go.mod"]; !ok {
		t.Errorf("expected go.mod in created map")
	}
	if _, ok := created["README.md"]; !ok {
		t.Errorf("expected README.md in created map")
	}
}
