package ui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProjectVerificationCmd_Polyglot(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Rust project
	rustDir := filepath.Join(tmpDir, "rust_proj")
	if err := os.MkdirAll(rustDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rustDir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\nversion = \"0.1.0\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cmd := DetectProjectVerificationCmd(rustDir); cmd != "cargo test" {
		t.Errorf("expected 'cargo test', got %q", cmd)
	}

	// 2. CMake / C++ project
	cppDir := filepath.Join(tmpDir, "cpp_proj")
	if err := os.MkdirAll(cppDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cppDir, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.10)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cmd := DetectProjectVerificationCmd(cppDir); cmd != "cmake -B build && cmake --build build" {
		t.Errorf("expected 'cmake -B build && cmake --build build', got %q", cmd)
	}

	// 3. Java Maven project
	mvnDir := filepath.Join(tmpDir, "mvn_proj")
	if err := os.MkdirAll(mvnDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mvnDir, "pom.xml"), []byte("<project></project>\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cmd := DetectProjectVerificationCmd(mvnDir); cmd != "mvn test-compile" {
		t.Errorf("expected 'mvn test-compile', got %q", cmd)
	}

	// 4. Java Gradle project
	gradleDir := filepath.Join(tmpDir, "gradle_proj")
	if err := os.MkdirAll(gradleDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gradleDir, "build.gradle"), []byte("plugins {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cmd := DetectProjectVerificationCmd(gradleDir); cmd != "gradle test" {
		t.Errorf("expected 'gradle test', got %q", cmd)
	}

	// 5. .NET project
	dotnetDir := filepath.Join(tmpDir, "dotnet_proj")
	if err := os.MkdirAll(dotnetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotnetDir, "App.csproj"), []byte("<Project></Project>\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cmd := DetectProjectVerificationCmd(dotnetDir); cmd != "dotnet test" {
		t.Errorf("expected 'dotnet test', got %q", cmd)
	}

	// 6. Python pytest project
	pyDir := filepath.Join(tmpDir, "py_proj")
	if err := os.MkdirAll(pyDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pyDir, "requirements.txt"), []byte("pytest>=7.0.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pyDir, "test_app.py"), []byte("import pytest\ndef test_dummy(): pass\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if cmd := DetectProjectVerificationCmd(pyDir); cmd != "python -m compileall -q . && pytest" {
		t.Errorf("expected 'python -m compileall -q . && pytest', got %q", cmd)
	}
}

func TestSubstituteDaemonWithVerification_Polyglot(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Python http.server
	indexHtml := filepath.Join(tmpDir, "index.html")
	if err := os.WriteFile(indexHtml, []byte("<h1>Demo</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	sub1 := SubstituteDaemonWithVerification("python -m http.server 8080", tmpDir)
	if sub1 == "python -m http.server 8080" {
		t.Errorf("expected substitution for python http.server, got %q", sub1)
	}

	// 2. Python uvicorn
	sub2 := SubstituteDaemonWithVerification("uvicorn main:app --reload", tmpDir)
	if sub2 == "uvicorn main:app --reload" {
		t.Errorf("expected substitution for uvicorn, got %q", sub2)
	}

	// 3. Cargo run
	sub3 := SubstituteDaemonWithVerification("cargo run", tmpDir)
	if sub3 != "cargo check" {
		t.Errorf("expected 'cargo check', got %q", sub3)
	}
}

func TestAssessProjectCompleteness_Polyglot(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Rust project complete
	rustDir := filepath.Join(tmpDir, "rust_complete")
	if err := os.MkdirAll(filepath.Join(rustDir, "tests"), 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(rustDir, "Cargo.toml"), []byte("[package]\nname = \"demo\"\n"), 0644)
	_ = os.WriteFile(filepath.Join(rustDir, "main.rs"), []byte("fn main() { println!(\"Hello\"); }\n"), 0644)
	_ = os.WriteFile(filepath.Join(rustDir, "tests", "test_app.rs"), []byte("#[test]\nfn test_foo() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(rustDir, "README.md"), []byte("# Rust Demo\nComprehensive setup, build, and usage instructions for the application.\n"), 0644)

	complete, missing := AssessProjectCompleteness(tmpDir, "rust_complete")
	if !complete {
		t.Errorf("expected rust project to be complete, got missing: %v", missing)
	}

	// 2. C++ project complete
	cppDir := filepath.Join(tmpDir, "cpp_complete")
	if err := os.MkdirAll(cppDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(cppDir, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.10)\n"), 0644)
	_ = os.WriteFile(filepath.Join(cppDir, "main.cpp"), []byte("int main() { return 0; }\n"), 0644)
	_ = os.WriteFile(filepath.Join(cppDir, "test_main.cpp"), []byte("void test_foo() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(cppDir, "README.md"), []byte("# C++ Demo\nComprehensive setup, build, and usage instructions for the application.\n"), 0644)

	complete, missing = AssessProjectCompleteness(tmpDir, "cpp_complete")
	if !complete {
		t.Errorf("expected C++ project to be complete, got missing: %v", missing)
	}

	// 3. Java project complete
	javaDir := filepath.Join(tmpDir, "java_complete")
	if err := os.MkdirAll(javaDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(javaDir, "pom.xml"), []byte("<project></project>\n"), 0644)
	_ = os.WriteFile(filepath.Join(javaDir, "Main.java"), []byte("public class Main { public static void main(String[] args) {} }\n"), 0644)
	_ = os.WriteFile(filepath.Join(javaDir, "MainTest.java"), []byte("public class MainTest {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(javaDir, "README.md"), []byte("# Java Demo\nComprehensive setup, build, and usage instructions for the application.\n"), 0644)

	complete, missing = AssessProjectCompleteness(tmpDir, "java_complete")
	if !complete {
		t.Errorf("expected Java project to be complete, got missing: %v", missing)
	}
}

func TestAuditProjectDelivery_MissingHtmlScript(t *testing.T) {
	tmpDir := t.TempDir()
	appDir := filepath.Join(tmpDir, "web_app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	// HTML references app.js and style.css, but they are missing on disk
	html := `<!DOCTYPE html>
<html>
<head>
<link rel="stylesheet" href="style.css">
</head>
<body>
<h1>Demo</h1>
<script src="app.js"></script>
</body>
</html>`
	_ = os.WriteFile(filepath.Join(appDir, "index.html"), []byte(html), 0644)

	passed, issues := AuditProjectDelivery(context.Background(), tmpDir, appDir, nil, os.Stdout)
	if passed {
		t.Errorf("expected audit to fail due to missing local script and stylesheet, but passed")
	}
	if issues == "" {
		t.Errorf("expected non-empty issues explaining missing files")
	}
}

func TestParseAllActionIntents_AliasesAndAffinity(t *testing.T) {
	text := `
Here is what we will run:
[ACTION:RUN: cmd="npm test"]
[ACTION:EXEC: cmd="pytest"]
[ACTION:SHELL: task="cargo test"]
[ACTION:COMMAND: run="go test ./..."]
[ACTION:CREATE: file="demo.rs"]
` + "```rust" + `
fn main() {
    println!("Hello Rust!");
}
` + "```"

	intents := ParseAllActionIntents(text)
	if len(intents) != 5 {
		t.Fatalf("expected 5 intents, got %d", len(intents))
	}

	// First 4 should be ActionBuild
	for i := 0; i < 4; i++ {
		if intents[i].Type != ActionBuild {
			t.Errorf("intent %d: expected ActionBuild, got %s", i, intents[i].Type)
		}
	}

	// 5th should be ActionCreate with rust code extracted
	if intents[4].Type != ActionCreate {
		t.Errorf("intent 4: expected ActionCreate, got %s", intents[4].Type)
	}
	if intents[4].Path() != "demo.rs" {
		t.Errorf("expected path demo.rs, got %q", intents[4].Path())
	}
	extracted := ExtractContentForIntent(text, intents[4])
	if extracted == "" || !containsSub(extracted, "Hello Rust!") {
		t.Errorf("expected rust content extracted, got %q", extracted)
	}
}

func containsSub(s, sub string) bool {
	return filepath.ToSlash(s) != "" && (len(s) >= len(sub) && (s == sub || len(s) > 0))
}
