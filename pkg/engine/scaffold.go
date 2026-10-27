// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectArchetype represents a pre-configured template archetype.
type ProjectArchetype string

const (
	ArchetypePythonStreamlit ProjectArchetype = "python_streamlit"
	ArchetypePythonGeneric   ProjectArchetype = "python"
	ArchetypeGoCLI           ProjectArchetype = "go_cli"
	ArchetypeGoServer        ProjectArchetype = "go_server"
	ArchetypeNodeWeb         ProjectArchetype = "node_web"
)

// DetectArchetype determines the project archetype based on task or plan description.
func DetectArchetype(desc string) ProjectArchetype {
	lower := strings.ToLower(desc)
	if strings.Contains(lower, "streamlit") || strings.Contains(lower, "dashboard") && strings.Contains(lower, "python") {
		return ArchetypePythonStreamlit
	}
	if strings.Contains(lower, "python") || strings.Contains(lower, "pytest") || strings.Contains(lower, ".py") {
		return ArchetypePythonGeneric
	}
	if strings.Contains(lower, "three.js") || strings.Contains(lower, "react") || strings.Contains(lower, "vue") || strings.Contains(lower, "vite") {
		return ArchetypeNodeWeb
	}
	if strings.Contains(lower, "server") || strings.Contains(lower, "api") || strings.Contains(lower, "http") {
		return ArchetypeGoServer
	}
	return ArchetypeGoCLI
}

// ScaffoldProject emits foundational scaffolding files for the given project directory and archetype.
// Returns a map of relative file path to content written.
func ScaffoldProject(targetDir string, archetype ProjectArchetype, projectName string) (map[string]string, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create target directory %s: %w", targetDir, err)
	}

	cleanName := strings.ToLower(strings.ReplaceAll(projectName, "_", "-"))
	if cleanName == "" {
		cleanName = strings.ToLower(filepath.Base(targetDir))
	}
	titleName := strings.Title(strings.ReplaceAll(cleanName, "-", " "))

	created := make(map[string]string)

	switch archetype {
	case ArchetypePythonStreamlit, ArchetypePythonGeneric:
		// 1. requirements.txt
		reqPath := filepath.Join(targetDir, "requirements.txt")
		if _, err := os.Stat(reqPath); os.IsNotExist(err) {
			var reqContent string
			if archetype == ArchetypePythonStreamlit {
				reqContent = "streamlit>=1.30.0\npandas>=2.0.0\npytest>=7.0.0\n"
			} else {
				reqContent = "pytest>=7.0.0\nrequests>=2.31.0\npydantic>=2.0.0\n"
			}
			_ = os.WriteFile(reqPath, []byte(reqContent), 0644)
			created["requirements.txt"] = reqContent
		}

		// 2. tests directory with __init__.py and conftest.py
		testsDir := filepath.Join(targetDir, "tests")
		_ = os.MkdirAll(testsDir, 0755)

		initPath := filepath.Join(testsDir, "__init__.py")
		if _, err := os.Stat(initPath); os.IsNotExist(err) {
			_ = os.WriteFile(initPath, []byte(""), 0644)
			created["tests/__init__.py"] = ""
		}

		conftestPath := filepath.Join(testsDir, "conftest.py")
		if _, err := os.Stat(conftestPath); os.IsNotExist(err) {
			conftestContent := "import os\nimport sys\n\n# Ensure project root is always first on sys.path for test runners\nsys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), \"..\")))\n"
			_ = os.WriteFile(conftestPath, []byte(conftestContent), 0644)
			created["tests/conftest.py"] = conftestContent
		}

		// 3. README.md
		readmePath := filepath.Join(targetDir, "README.md")
		if _, err := os.Stat(readmePath); os.IsNotExist(err) {
			readmeContent := fmt.Sprintf("# %s\n\nAutonomous deliverable built with OmniCoder-ICX.\n\n## Quickstart\n\n```bash\npip install -r requirements.txt\npytest\n```\n", titleName)
			_ = os.WriteFile(readmePath, []byte(readmeContent), 0644)
			created["README.md"] = readmeContent
		}

	case ArchetypeGoCLI, ArchetypeGoServer:
		// 1. go.mod
		modPath := filepath.Join(targetDir, "go.mod")
		if _, err := os.Stat(modPath); os.IsNotExist(err) {
			modContent := fmt.Sprintf("module github.com/caleralabs/%s\n\ngo 1.23\n", cleanName)
			_ = os.WriteFile(modPath, []byte(modContent), 0644)
			created["go.mod"] = modContent
		}

		// 2. README.md
		readmePath := filepath.Join(targetDir, "README.md")
		if _, err := os.Stat(readmePath); os.IsNotExist(err) {
			readmeContent := fmt.Sprintf("# %s\n\nAutonomous deliverable built with OmniCoder-ICX.\n\n## Verification\n\n```bash\ngo test ./...\n```\n", titleName)
			_ = os.WriteFile(readmePath, []byte(readmeContent), 0644)
			created["README.md"] = readmeContent
		}

	case ArchetypeNodeWeb:
		// 1. package.json
		pkgPath := filepath.Join(targetDir, "package.json")
		if _, err := os.Stat(pkgPath); os.IsNotExist(err) {
			pkgContent := fmt.Sprintf("{\n  \"name\": \"%s\",\n  \"version\": \"1.0.0\",\n  \"description\": \"Autonomous deliverable built with OmniCoder-ICX\",\n  \"scripts\": {\n    \"test\": \"echo \\\"No tests specified\\\"\"\n  }\n}\n", cleanName)
			_ = os.WriteFile(pkgPath, []byte(pkgContent), 0644)
			created["package.json"] = pkgContent
		}

		// 2. README.md
		readmePath := filepath.Join(targetDir, "README.md")
		if _, err := os.Stat(readmePath); os.IsNotExist(err) {
			readmeContent := fmt.Sprintf("# %s\n\nAutonomous deliverable built with OmniCoder-ICX.\n", titleName)
			_ = os.WriteFile(readmePath, []byte(readmeContent), 0644)
			created["README.md"] = readmeContent
		}
	}

	return created, nil
}
