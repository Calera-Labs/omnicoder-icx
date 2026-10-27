package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPytestPluginCrash(t *testing.T) {
	diag := "PluggyTeardownRaisedWarning\nTraceback (most recent call last):\nImportError: cannot import name 'FixtureDef' from 'pytest'"
	if !isPytestPluginCrash("python -m pytest x", diag) {
		t.Error("plugin crash not detected")
	}
	if isPytestPluginCrash("python -m unittest", diag) || isPytestPluginCrash("python -m pytest", "AssertionError: 1 != 2") {
		t.Error("false positive")
	}
}

func TestWithEnvKeepsExisting(t *testing.T) {
	env := withEnv([]string{"A=1"}, "A", "2")
	if len(env) != 1 || env[0] != "A=1" {
		t.Errorf("existing key overwritten: %v", env)
	}
	if env = withEnv(env, "B", "2"); len(env) != 2 {
		t.Errorf("missing key not added: %v", env)
	}
}

func TestExtractTargetProject(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "my-service-app")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("failed to create test proj dir: %v", err)
	}

	// 1. Quoted absolute path
	prompt1 := `fix app at "` + projDir + `"`
	got1 := ExtractTargetProject(prompt1, tmpDir)
	if got1 != "projects/my-service-app" {
		t.Errorf("expected projects/my-service-app, got %q", got1)
	}

	// 2. Relative token
	prompt2 := `run tests in projects/my-service-app please`
	got2 := ExtractTargetProject(prompt2, tmpDir)
	if got2 != "projects/my-service-app" {
		t.Errorf("expected projects/my-service-app, got %q", got2)
	}

	// 3. Substring directory match
	prompt3 := `please investigate my-service-app failures`
	got3 := ExtractTargetProject(prompt3, tmpDir)
	if got3 != "projects/my-service-app" {
		t.Errorf("expected projects/my-service-app, got %q", got3)
	}

	// 4. No project mentioned
	prompt4 := `what is the weather today?`
	got4 := ExtractTargetProject(prompt4, tmpDir)
	if got4 != "" {
		t.Errorf("expected empty string, got %q", got4)
	}
}

func TestDiscoverUniversalEntries(t *testing.T) {
	tmpDir := t.TempDir()

	// Create various language entry points
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "server.py"), []byte("from http.server import HTTPServer\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "index.ts"), []byte("console.log('hi');\n"), 0644)

	entries := DiscoverUniversalEntries(tmpDir)
	if len(entries) < 3 {
		t.Errorf("expected at least 3 entries discovered, got %d", len(entries))
	}

	foundGo := false
	foundPy := false
	foundNode := false
	for _, e := range entries {
		if e.Path == "main.go" && e.Language == "go" {
			foundGo = true
		}
		if e.Path == "server.py" && e.Language == "python" {
			foundPy = true
		}
		if e.Path == "index.ts" && e.Language == "node" {
			foundNode = true
		}
	}

	if !foundGo || !foundPy || !foundNode {
		t.Errorf("missing language entries: go=%v py=%v node=%v", foundGo, foundPy, foundNode)
	}
}

func TestDetectDeclaredEntryPoint(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "main.py"), []byte("print('hello')\n"), 0644)
	readme := "# My App\nRun `python main.py` to start the service.\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte(readme), 0644)

	declared := DetectDeclaredEntryPoint(tmpDir)
	if declared != "main.py" {
		t.Errorf("expected main.py, got %q", declared)
	}
}

func TestBuildProjectGroundTruthBriefing(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "projects", "sample-app")
	_ = os.MkdirAll(projDir, 0755)
	_ = os.WriteFile(filepath.Join(projDir, "app.py"), []byte("print('ok')\n"), 0644)
	_ = os.WriteFile(filepath.Join(projDir, "schema.sql"), []byte("CREATE TABLE items (id INT);\n"), 0644)

	briefing := BuildProjectGroundTruthBriefing(tmpDir, "projects/sample-app")
	if !strings.Contains(briefing, "app.py") || !strings.Contains(briefing, "schema.sql") {
		t.Errorf("expected files in briefing, got: %s", briefing)
	}
	if !strings.Contains(briefing, "ACTIVE TARGET PROJECT INVENTORY") {
		t.Errorf("expected header in briefing, got: %s", briefing)
	}
}

func TestProbeUniversalStartup_CleanScript(t *testing.T) {
	tmpDir := t.TempDir()
	cleanPy := filepath.Join(tmpDir, "clean.py")
	_ = os.WriteFile(cleanPy, []byte("print('Universal startup success')\n"), 0644)

	entry := UniversalEntryInfo{
		Path:     "clean.py",
		Language: "python",
		IsDaemon: false,
	}

	ran, ok, diag := ProbeUniversalStartup(context.Background(), tmpDir, entry, nil)
	if !ran || !ok {
		t.Fatalf("expected clean script to pass, ran=%v ok=%v diag=%s", ran, ok, diag)
	}
}

func TestProbeUniversalStartup_CrashDetected(t *testing.T) {
	tmpDir := t.TempDir()
	crashPy := filepath.Join(tmpDir, "crash.py")
	_ = os.WriteFile(crashPy, []byte("raise KeyError('missing_database_config')\n"), 0644)

	entry := UniversalEntryInfo{
		Path:     "crash.py",
		Language: "python",
		IsDaemon: false,
	}

	ran, ok, diag := ProbeUniversalStartup(context.Background(), tmpDir, entry, nil)
	if !ran {
		t.Fatal("expected probe to run")
	}
	if ok {
		t.Fatal("expected probe to fail on unhandled exception")
	}
	if !strings.Contains(diag, "KeyError") {
		t.Fatalf("expected KeyError in diagnostic, got: %s", diag)
	}
}
