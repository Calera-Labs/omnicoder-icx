// SPDX-License-Identifier: Apache-2.0
// Package ui contains adversarial and verification tests for multi-file generation.

package ui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestExtractContentForIntent_MultiFileFiveFiles verifies that a multi-file
// response containing 5 distinct files (Go Fiber web portal app with prose and headings)
// is correctly parsed into 5 distinct intents, and each intent extracts strictly its designated code block.
func TestExtractContentForIntent_MultiFileFiveFiles(t *testing.T) {
	multiFileResponse := `To build a high-performance Go web portal application, we will use the **Fiber** web framework combined with modern HTML5, CSS3 styling, and client-side JavaScript for interactive controls.

Below is the complete, production-grade implementation structured into a clean Go project layout:
1. ` + "`go.mod`" + ` - Go module definition.
2. ` + "`main.go`" + ` - Fiber web server.
3. ` + "`public/index.html`" + ` - The frontend.
4. ` + "`public/style.css`" + ` - CSS styling.
5. ` + "`public/script.js`" + ` - Controls.

---

### 1. Module Definition (` + "`go.mod`" + `)

[ACTION:WRITE path="apps/portal/go.mod"]
` + "```go\nmodule fiber-portal\n\ngo 1.21\n\nrequire github.com/gofiber/fiber/v2 v2.52.5\n```" + `

---

### 2. Fiber Server (` + "`main.go`" + `)

[ACTION:WRITE path="apps/portal/main.go"]
` + "```go\npackage main\n\nimport (\n\t\"log\"\n\t\"github.com/gofiber/fiber/v2\"\n)\n\nfunc main() {\n\tapp := fiber.New()\n\tlog.Fatal(app.Listen(\":3000\"))\n}\n```" + `

---

### 3. Frontend Markup (` + "`public/index.html`" + `)

[ACTION:WRITE path="apps/portal/public/index.html"]
` + "```html\n<!DOCTYPE html>\n<html>\n<head><title>Portal</title></head>\n<body><div id=\"portal\"></div></body>\n</html>\n```" + `

---

### 4. Styling & Keyframe Animations (` + "`public/style.css`" + `)

[ACTION:WRITE path="apps/portal/public/style.css"]
` + "```css\n:root { --bg-color: #0f172a; }\n.portal { width: 10px; height: 140px; }\n```" + `

---

### 5. Client-Side Interaction (` + "`public/script.js`" + `)

[ACTION:WRITE path="apps/portal/public/script.js"]
` + "```javascript\ndocument.addEventListener('DOMContentLoaded', () => {\n\tconsole.log('Portal started');\n});\n```" + `

---

### How to Run

1. Initialize and run:
   ` + "```bash\ngo run main.go\n```"

	intents := ParseAllActionIntents(multiFileResponse)
	if len(intents) != 5 {
		t.Fatalf("Expected 5 parsed action intents, got %d", len(intents))
	}

	expectedPaths := []string{
		"apps/portal/go.mod",
		"apps/portal/main.go",
		"apps/portal/public/index.html",
		"apps/portal/public/style.css",
		"apps/portal/public/script.js",
	}

	for i, expectedPath := range expectedPaths {
		if intents[i].Path() != expectedPath {
			t.Errorf("Intent %d path mismatch: expected %q, got %q", i, expectedPath, intents[i].Path())
		}
	}

	// 1. Verify go.mod content extraction
	goModContent := intents[0].ExtractContent(multiFileResponse)
	if !strings.HasPrefix(goModContent, "module fiber-portal") {
		t.Errorf("go.mod should start with 'module fiber-portal', got:\n%s", goModContent)
	}
	if strings.Contains(goModContent, "### 1. Module Definition") {
		t.Errorf("go.mod contains markdown heading:\n%s", goModContent)
	}
	if strings.Contains(goModContent, "package main") {
		t.Errorf("go.mod contains main.go code:\n%s", goModContent)
	}
	if strings.Contains(goModContent, "<!DOCTYPE html>") {
		t.Errorf("go.mod contains HTML:\n%s", goModContent)
	}

	// 2. Verify main.go content extraction
	mainGoContent := intents[1].ExtractContent(multiFileResponse)
	if !strings.HasPrefix(mainGoContent, "package main") {
		t.Errorf("main.go should start with 'package main', got:\n%s", mainGoContent)
	}
	if strings.Contains(mainGoContent, "module fiber-portal") {
		t.Errorf("main.go contains go.mod code:\n%s", mainGoContent)
	}
	if strings.Contains(mainGoContent, "### 2. Fiber Server") {
		t.Errorf("main.go contains markdown heading:\n%s", mainGoContent)
	}

	// 3. Verify public/index.html content extraction
	htmlContent := intents[2].ExtractContent(multiFileResponse)
	if !strings.HasPrefix(htmlContent, "<!DOCTYPE html>") {
		t.Errorf("index.html should start with '<!DOCTYPE html>', got:\n%s", htmlContent)
	}
	if strings.Contains(htmlContent, "package main") {
		t.Errorf("index.html contains Go code:\n%s", htmlContent)
	}

	// 4. Verify public/style.css content extraction
	cssContent := intents[3].ExtractContent(multiFileResponse)
	if !strings.HasPrefix(cssContent, ":root {") {
		t.Errorf("style.css should start with ':root {', got:\n%s", cssContent)
	}
	if strings.Contains(cssContent, "<!DOCTYPE html>") {
		t.Errorf("style.css contains HTML:\n%s", cssContent)
	}

	// 5. Verify public/script.js content extraction
	jsContent := intents[4].ExtractContent(multiFileResponse)
	if !strings.HasPrefix(jsContent, "document.addEventListener") {
		t.Errorf("script.js should start with 'document.addEventListener', got:\n%s", jsContent)
	}
	if strings.Contains(jsContent, ":root") {
		t.Errorf("script.js contains CSS:\n%s", jsContent)
	}
}

// TestExtractContentForIntent_TrailingActionTags verifies multi-file extraction
// when action tags are placed AFTER the code blocks.
func TestExtractContentForIntent_TrailingActionTags(t *testing.T) {
	trailingResponse := `Here are the configuration files:

` + "```yaml\nversion: '3.8'\nservices:\n  web:\n    image: nginx\n```\n" +
		`[ACTION:WRITE path="docker-compose.yml"]

` + "```json\n{\n  \"name\": \"app\",\n  \"version\": \"1.0.0\"\n}\n```\n" +
		`[ACTION:WRITE path="package.json"]`

	intents := ParseAllActionIntents(trailingResponse)
	if len(intents) != 2 {
		t.Fatalf("Expected 2 parsed action intents, got %d", len(intents))
	}

	composeContent := intents[0].ExtractContent(trailingResponse)
	if !strings.HasPrefix(composeContent, "version: '3.8'") {
		t.Errorf("docker-compose.yml content mismatch, got:\n%s", composeContent)
	}
	if strings.Contains(composeContent, "package.json") {
		t.Errorf("docker-compose.yml contains package.json text:\n%s", composeContent)
	}

	packageContent := intents[1].ExtractContent(trailingResponse)
	if !strings.HasPrefix(packageContent, "{\n  \"name\": \"app\"") {
		t.Errorf("package.json content mismatch, got:\n%s", packageContent)
	}
	if strings.Contains(packageContent, "services:") {
		t.Errorf("package.json contains yaml services:\n%s", packageContent)
	}
}

// TestRunConversationalUI_MultiFileE2E verifies that RunConversationalUI
// writes ALL 5 files to disk with non-empty content and valid SHA-256 seals.
func TestRunConversationalUI_MultiFileE2E(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-multifile-e2e-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	mockReply := `Here is your full website project:

[ACTION:WRITE path="web/go.mod"]
` + "```go\nmodule my-web-app\n\ngo 1.21\n```\n\n" +
		`[ACTION:WRITE path="web/main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```\n\n" +
		`[ACTION:WRITE path="web/public/index.html"]
` + "```html\n<h1>Hello Multi-File</h1>\n```\n\n" +
		`[ACTION:WRITE path="web/public/style.css"]
` + "```css\nh1 { color: red; }\n```\n\n" +
		`[ACTION:WRITE path="web/public/app.js"]
` + "```javascript\nconsole.log('Ready');\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	input := "build website\n/quit\n"
	in := strings.NewReader(input)
	var out bytes.Buffer

	ctx := context.Background()
	err = RunConversationalUI(ctx, session, in, &out)
	if err != nil {
		t.Fatalf("RunConversationalUI returned error: %v", err)
	}

	outStr := out.String()

	// Verify all 5 [Auto-Write Detected] and [File Created] badges appear in stdout
	expectedFiles := []string{
		"web/go.mod",
		"web/main.go",
		"web/public/index.html",
		"web/public/style.css",
		"web/public/app.js",
	}

	for _, relPath := range expectedFiles {
		if !strings.Contains(outStr, relPath) {
			t.Errorf("stdout missing expected path badge for %s", relPath)
		}

		fullPath := filepath.Join(tmpDir, filepath.FromSlash(relPath))
		data, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			t.Errorf("Expected file %s was not created on disk: %v", relPath, readErr)
			continue
		}
		if len(data) == 0 {
			t.Errorf("Created file %s is empty", relPath)
		}
	}

	// Verify go.mod does NOT contain html or javascript
	goModData, _ := os.ReadFile(filepath.Join(tmpDir, "web", "go.mod"))
	if strings.Contains(string(goModData), "<h1>") || strings.Contains(string(goModData), "console.log") {
		t.Errorf("web/go.mod was contaminated with HTML/JS:\n%s", string(goModData))
	}
}

// TestMultiFile_PathTraversalRejection verifies that if one file in a multi-file
// response attempts path traversal, it is rejected and does not write outside the workspace,
// while legitimate sibling files are successfully persisted.
func TestMultiFile_PathTraversalRejection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-traversal-matrix-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	parentDir := filepath.Dir(tmpDir)
	canaryPath := filepath.Join(parentDir, "escaped_canary.txt")
	_ = os.Remove(canaryPath)
	defer os.Remove(canaryPath)

	mockReply := `Here are the files:

[ACTION:WRITE path="safe/module.go"]
` + "```go\npackage safe\n```\n\n" +
		`[ACTION:WRITE path="../../escaped_canary.txt"]
` + "```text\nEXPLOIT_PAYLOAD\n```\n\n" +
		`[ACTION:WRITE path="safe/config.json"]
` + "```json\n{\"ok\": true}\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	input := "build with traversal\n/quit\n"
	in := strings.NewReader(input)
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	// 1. Canary file outside workspace must NOT exist
	if _, statErr := os.Stat(canaryPath); statErr == nil {
		t.Fatalf("SECURITY VIOLATION: Path traversal succeeded! Canary file was written at %s", canaryPath)
	}

	// 2. Legitimate files inside workspace must exist
	safeMod := filepath.Join(tmpDir, "safe", "module.go")
	if _, statErr := os.Stat(safeMod); statErr != nil {
		t.Errorf("Expected legitimate file %s to exist, err: %v", safeMod, statErr)
	}

	safeConf := filepath.Join(tmpDir, "safe", "config.json")
	if _, statErr := os.Stat(safeConf); statErr != nil {
		t.Errorf("Expected legitimate file %s to exist, err: %v", safeConf, statErr)
	}
}

// TestRootGoMod_CollisionProtection verifies that attempting to overwrite a root
// go.mod with a foreign module is rejected with the protection error, while legitimate updates
// or subfolder writes succeed.
func TestRootGoMod_CollisionProtection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-root-collision-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(rootModPath, []byte(hostModContent), 0644); err != nil {
		t.Fatalf("Failed to write initial root go.mod: %v", err)
	}

	session := NewSession(nil, nil, tmpDir)

	// 1. Attempting to overwrite root go.mod with a foreign module must be rejected
	foreignModContent := "module fiber-portal\n\ngo 1.21\n"
	_, _, err = session.WriteFile("go.mod", []byte(foreignModContent))
	if err == nil {
		t.Fatalf("Expected error when overwriting root go.mod with foreign module, got nil")
	}

	expectedErr := `ui: refusing to overwrite workspace root go.mod (module "github.com/caleralabs/omnicoder-icx") with foreign module "fiber-portal"; new applications must be placed in a subfolder (e.g. 'apps/<name>/go.mod')`
	if err.Error() != expectedErr {
		t.Errorf("Error mismatch:\nExpected: %q\nGot:      %q", expectedErr, err.Error())
	}

	// Verify root go.mod content on disk is completely untouched
	currentData, err := os.ReadFile(rootModPath)
	if err != nil {
		t.Fatalf("Failed to read root go.mod: %v", err)
	}
	if string(currentData) != hostModContent {
		t.Errorf("Root go.mod was corrupted or modified:\nGot:\n%s", string(currentData))
	}

	// Also verify with leading ./go.mod
	_, _, err = session.WriteFile("./go.mod", []byte(foreignModContent))
	if err == nil {
		t.Fatalf("Expected error when overwriting ./go.mod with foreign module, got nil")
	}
	if err.Error() != expectedErr {
		t.Errorf("Error mismatch for ./go.mod:\nExpected: %q\nGot:      %q", expectedErr, err.Error())
	}

	// 2. Legitimate update to root go.mod with the SAME module must succeed
	legitUpdate := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.22\n\nrequire github.com/stretchr/testify v1.9.0\n"
	writtenPath, sha, err := session.WriteFile("go.mod", []byte(legitUpdate))
	if err != nil {
		t.Fatalf("Expected legitimate update to root go.mod to succeed, got error: %v", err)
	}
	if writtenPath != "go.mod" {
		t.Errorf("Expected written path 'go.mod', got %q", writtenPath)
	}
	if sha == "" {
		t.Errorf("Expected non-empty SHA-256 seal")
	}

	updatedData, err := os.ReadFile(rootModPath)
	if err != nil {
		t.Fatalf("Failed to read updated root go.mod: %v", err)
	}
	if string(updatedData) != legitUpdate {
		t.Errorf("Root go.mod does not match updated content:\nGot:\n%s", string(updatedData))
	}

	// 3. Subfolder writes declaring foreign module must succeed cleanly
	subPath := "apps/fiber-portal/go.mod"
	writtenSub, shaSub, err := session.WriteFile(subPath, []byte(foreignModContent))
	if err != nil {
		t.Fatalf("Expected subfolder go.mod write to succeed, got error: %v", err)
	}
	if filepath.ToSlash(writtenSub) != subPath {
		t.Errorf("Expected subfolder written path %q, got %q", subPath, writtenSub)
	}
	if shaSub == "" {
		t.Errorf("Expected non-empty subfolder SHA-256 seal")
	}

	subData, err := os.ReadFile(filepath.Join(tmpDir, "apps", "fiber-portal", "go.mod"))
	if err != nil {
		t.Fatalf("Failed to read subfolder go.mod: %v", err)
	}
	if string(subData) != foreignModContent {
		t.Errorf("Subfolder go.mod content mismatch:\nGot:\n%s", string(subData))
	}

	// Ensure root go.mod remained untouched by subfolder write
	rootDataAfterSub, err := os.ReadFile(rootModPath)
	if err != nil {
		t.Fatalf("Failed to read root go.mod after sub write: %v", err)
	}
	if string(rootDataAfterSub) != legitUpdate {
		t.Errorf("Root go.mod was contaminated by subfolder write:\nGot:\n%s", string(rootDataAfterSub))
	}

	// 4. Critical infrastructure protection: attempting to write into .git must be rejected
	gitVectors := []string{
		".git/config",
		".GIT/config",
		".Git/HEAD",
		".gitmodules",
		".gitattributes",
		filepath.Join(tmpDir, ".gitmodules"),
		filepath.Join(tmpDir, ".gitattributes"),
		filepath.Join(tmpDir, ".git", "config"),
		filepath.Join(tmpDir, ".GIT", "config"),
	}
	for _, gv := range gitVectors {
		_, _, err = session.WriteFile(gv, []byte("[core]\n"))
		if err == nil {
			t.Fatalf("Expected error when attempting to write into %q, got nil", gv)
		}
		if !strings.Contains(err.Error(), "target path cannot be inside .git directory") {
			t.Errorf("Unexpected error message when writing into %q: %v", gv, err)
		}
	}

	// 5. Attempting to overwrite root go.mod with an empty or non-module payload must be rejected
	_, _, err = session.WriteFile("go.mod", []byte("// comments only, no module\n"))
	if err == nil {
		t.Fatalf("Expected error when attempting to overwrite root go.mod with non-module payload, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite workspace root go.mod") {
		t.Errorf("Unexpected error for non-module overwrite: %v", err)
	}
}

// TestAutoScope_RootProjectRelocation verifies that a multi-file response
// containing root go.mod, main.go, public/index.html with a foreign module is
// automatically redirected into apps/<module-name>/, leaving root go.mod untouched.
func TestAutoScope_RootProjectRelocation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-autoscope-relocate-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(rootModPath, []byte(hostModContent), 0644); err != nil {
		t.Fatalf("Failed to write initial root go.mod: %v", err)
	}

	mockReply := `I have designed and implemented your complete Fiber Portal web application.

[ACTION:WRITE path="go.mod"]
` + "```go\nmodule fiber-portal\n\ngo 1.21\n\nrequire github.com/gofiber/fiber/v2 v2.52.5\n```\n\n" +
		`[ACTION:WRITE path="main.go"]
` + "```go\npackage main\n\nimport (\n\t\"fmt\"\n\t\"github.com/gofiber/fiber/v2\"\n)\n\nfunc main() {\n\tapp := fiber.New()\n\tapp.Listen(\":3000\")\n}\n```\n\n" +
		`[ACTION:WRITE path="public/index.html"]
` + "```html\n<!DOCTYPE html>\n<html>\n<head><title>Fiber Portal</title></head>\n<body>\n  <div id=\"portal\"></div>\n</body>\n</html>\n```\n\n" +
		`[ACTION:WRITE path="public/style.css"]
` + "```css\n.portal { animation: fadein 1s infinite; }\n```\n\n" +
		`[ACTION:WRITE path="public/app.js"]
` + "```javascript\nconsole.log('Fiber portal initialized');\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	input := "build fiber portal app\n/quit\n"
	in := strings.NewReader(input)
	var out bytes.Buffer

	ctx := context.Background()
	err = RunConversationalUI(ctx, session, in, &out)
	if err != nil {
		t.Fatalf("RunConversationalUI returned unexpected error: %v", err)
	}

	outStr := out.String()

	// 1. Verify Auto-Scope badge in stdout
	expectedBadge := "[Auto-Scope Applied] Scoped new application to: apps/fiber-portal/"
	if !strings.Contains(outStr, expectedBadge) {
		t.Errorf("Output missing expected auto-scope badge %q:\nGot output:\n%s", expectedBadge, outStr)
	}

	// 2. Verify root go.mod is 100% UNTOUCHED
	rootData, err := os.ReadFile(rootModPath)
	if err != nil {
		t.Fatalf("Failed to read root go.mod: %v", err)
	}
	if string(rootData) != hostModContent {
		t.Errorf("CRITICAL INVARIANT VIOLATION: Host root go.mod was overwritten or modified!\nExpected:\n%s\nGot:\n%s", hostModContent, string(rootData))
	}

	// 3. Verify root main.go was NOT created
	if _, err := os.Stat(filepath.Join(tmpDir, "main.go")); !os.IsNotExist(err) {
		t.Errorf("Pollution detected: main.go exists at workspace root")
	}

	// 4. Verify root public/ directory was NOT created
	if _, err := os.Stat(filepath.Join(tmpDir, "public")); !os.IsNotExist(err) {
		t.Errorf("Pollution detected: public/ exists at workspace root")
	}

	// 5. Verify all 5 files were correctly relocated into apps/fiber-portal/
	expectedRelocated := []struct {
		relPath       string
		contentMarker string
	}{
		{"apps/fiber-portal/go.mod", "module fiber-portal"},
		{"apps/fiber-portal/main.go", "package main"},
		{"apps/fiber-portal/public/index.html", "<title>Fiber Portal</title>"},
		{"apps/fiber-portal/public/style.css", ".portal { animation: fadein 1s infinite; }"},
		{"apps/fiber-portal/public/app.js", "Fiber portal initialized"},
	}

	for _, ef := range expectedRelocated {
		fullPath := filepath.Join(tmpDir, filepath.FromSlash(ef.relPath))
		data, err := os.ReadFile(fullPath)
		if err != nil {
			t.Errorf("Expected relocated file %s was not found on disk: %v", ef.relPath, err)
			continue
		}
		if !strings.Contains(string(data), ef.contentMarker) {
			t.Errorf("Relocated file %s missing expected content %q; got:\n%s", ef.relPath, ef.contentMarker, string(data))
		}
	}
}

// TestAutoScope_NestedModulePath verifies that a module declaration with a URL path
// like github.com/organization/game-hub is properly sanitized to the base project folder.
func TestAutoScope_NestedModulePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	mockReply := `Multi-file nested module:

[ACTION:WRITE path="go.mod"]
` + "```go\nmodule github.com/organization/game-hub\n\ngo 1.21\n```\n\n" +
		`[ACTION:WRITE path="main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	err := RunConversationalUI(context.Background(), session, strings.NewReader("generate game-hub\n/quit\n"), &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	expectedBadge := "[Auto-Scope Applied] Scoped new application to: apps/game-hub/"
	if !strings.Contains(out.String(), expectedBadge) {
		t.Errorf("Expected badge %q in output, got:\n%s", expectedBadge, out.String())
	}

	// Verify file is placed in apps/game-hub/
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "game-hub", "go.mod")); err != nil {
		t.Errorf("Expected apps/game-hub/go.mod to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "game-hub", "main.go")); err != nil {
		t.Errorf("Expected apps/game-hub/main.go to exist: %v", err)
	}
}

// TestSystemPrompt_WorkspaceScopingInvariant verifies that DefaultSystemPrompt
// incorporates the Workspace Isolation & Subfolder Scoping invariant.
func TestSystemPrompt_WorkspaceScopingInvariant(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())
	prompt := session.SystemPrompt()

	clauses := []string{
		"Workspace Isolation & Subfolder Scoping",
		"NEVER write new project files to the workspace root directory",
		"apps/<project-name>/",
		"Root-level writes are exclusively permitted",
	}

	for _, c := range clauses {
		if !strings.Contains(prompt, c) {
			t.Errorf("DefaultSystemPrompt missing required scoping invariant clause: %q", c)
		}
	}
}

// TestExtractModuleName_Variations tests parsing of module names from various go.mod contents.
func TestExtractModuleName_Variations(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{"standard", "module fiber-portal\n\ngo 1.21\n", "fiber-portal"},
		{"github path", "module github.com/caleralabs/omnicoder-icx\n", "github.com/caleralabs/omnicoder-icx"},
		{"double quotes", `module "quoted-app"` + "\ngo 1.22", "quoted-app"},
		{"single quotes", "module 'single-app'\ngo 1.22", "single-app"},
		{"with trailing comment", "module foo/bar // the module\ngo 1.21", "foo/bar"},
		{"comment line before", "// Header\nmodule header-mod\ngo 1.20", "header-mod"},
		{"tabs indentation", "\tmodule\ttab-mod\n", "tab-mod"},
		{"empty content", "", ""},
		{"no module directive", "package main\n\nfunc main() {}", ""},
		{"variable module_name false positive", "module_name := \"my-app\"\n", ""},
		{"keyword prefix modules", "modules := []string{\"a\", \"b\"}\n", ""},
		{"multiline block comment with fake module", "/*\nLicensed under Apache 2.0\nmodule fake-name\n*/\nmodule real-app\n", "real-app"},
		{"inline block comment before module", "/* header */ module inline-app\n", "inline-app"},
		{"trailing block comment after module", "module trail-app /* comment */\n", "trail-app"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractModuleName([]byte(tc.input))
			if got != tc.expected {
				t.Errorf("ExtractModuleName(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		})
	}
}

// TestAutoScope_NestedModule_MajorVersionAndTraversal verifies that major version
// suffixes like /v2 and adversarial module names like .. are properly handled without escaping.
func TestAutoScope_NestedModule_MajorVersionAndTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	// 1. Module with /v2 should scope to apps/service-api/, NOT apps/v2/
	mockReplyV2 := `Multi-file v2 module:

[ACTION:WRITE path="go.mod"]
` + "```go\nmodule github.com/org/service-api/v2\n\ngo 1.21\n```\n\n" +
		`[ACTION:WRITE path="main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReplyV2
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	err := RunConversationalUI(context.Background(), session, strings.NewReader("generate v2\n/quit\n"), &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	expectedBadge := "[Auto-Scope Applied] Scoped new application to: apps/service-api/"
	if !strings.Contains(out.String(), expectedBadge) {
		t.Errorf("Expected badge %q in output, got:\n%s", expectedBadge, out.String())
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "service-api", "go.mod")); err != nil {
		t.Errorf("Expected apps/service-api/go.mod to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "v2")); !os.IsNotExist(err) {
		t.Errorf("apps/v2/ should not have been created")
	}
}

// TestAutoScope_SubfolderIntegrity_OutputFolder verifies that files inside an
// output/ subfolder of a generated project (e.g. output/bundle.js) are cleanly scoped into
// apps/<name>/output/bundle.js, with zero root output/ pollution.
func TestAutoScope_SubfolderIntegrity_OutputFolder(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	mockReply := `Project with build output:

[ACTION:WRITE path="go.mod"]
` + "```go\nmodule web-compiler\n\ngo 1.21\n```\n\n" +
		`[ACTION:WRITE path="main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```\n\n" +
		`[ACTION:WRITE path="output/bundle.js"]
` + "```javascript\nconsole.log('compiled');\n```\n\n" +
		`[ACTION:WRITE path="output/index.html"]
` + "```html\n<h1>App</h1>\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	err := RunConversationalUI(context.Background(), session, strings.NewReader("generate web-compiler\n/quit\n"), &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	// 1. Root output/ directory must NOT exist
	rootOutput := filepath.Join(tmpDir, "output")
	if _, err := os.Stat(rootOutput); !os.IsNotExist(err) {
		t.Errorf("CRITICAL VIOLATION: root directory 'output/' was created, polluting workspace root!")
	}

	// 2. Subfolder apps/web-compiler/output/bundle.js must exist
	scopedBundle := filepath.Join(tmpDir, "apps", "web-compiler", "output", "bundle.js")
	if _, err := os.Stat(scopedBundle); err != nil {
		t.Errorf("Expected scoped bundle %s to exist: %v", scopedBundle, err)
	}

	scopedHTML := filepath.Join(tmpDir, "apps", "web-compiler", "output", "index.html")
	if _, err := os.Stat(scopedHTML); err != nil {
		t.Errorf("Expected scoped HTML %s to exist: %v", scopedHTML, err)
	}
}

// TestSanitizeProjectName_ComprehensiveMatrix verifies project name sanitization across
// URLs, versions, adversarial traversals, and invalid characters.
func TestSanitizeProjectName_ComprehensiveMatrix(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"fiber-portal", "fiber-portal"},
		{"github.com/caleralabs/omnicoder-icx", "omnicoder-icx"},
		{"github.com/org/repo/v2", "repo"},
		{"github.com/org/repo/v10", "repo"},
		{"github.com/org/service.git", "service"},
		{"github.com/org/service.git/v2", "service"},
		{"CON", "app"},
		{"aux", "app"},
		{"nul", "app"},
		{"prn", "app"},
		{"com1", "app"},
		{"lpt1", "app"},
		{"..", "app"},
		{"../..", "app"},
		{".", "app"},
		{"", "app"},
		{"/root/path/", "path"},
		{"my-app:special*name?", "my-app-special-name"},
		{"v2", "app"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := SanitizeProjectName(tc.input)
			if got != tc.expected {
				t.Errorf("SanitizeProjectName(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		})
	}
}

// TestScopePath_ComprehensiveMatrix verifies path scoping and containment.
func TestScopePath_ComprehensiveMatrix(t *testing.T) {
	scopedDir := "apps/my-app"

	cases := []struct {
		name     string
		relPath  string
		expected string
	}{
		{"root go.mod", "go.mod", "apps/my-app/go.mod"},
		{"root main.go", "main.go", "apps/my-app/main.go"},
		{"nested public", "public/index.html", "apps/my-app/public/index.html"},
		{"nested output directory", "output/bundle.js", "apps/my-app/output/bundle.js"},
		{"nested projects directory", "projects/sub.go", "apps/my-app/projects/sub.go"},
		{"already scoped apps", "apps/my-app/main.go", "apps/my-app/main.go"},
		{"already scoped projects prefix", "projects/my-app/main.go", "projects/my-app/main.go"},
		{"already scoped output prefix", "output/my-app/main.go", "output/my-app/main.go"},
		{"path traversal breakout attempt", "../../main.go", "apps/my-app/main.go"},
		{"deep traversal breakout", "../../../secret.txt", "apps/my-app/secret.txt"},
		{"empty path", "", ""},
		{"dot directory", ".", ""},
		{"dot dot directory", "..", ""},
		{"windows backslashes", `public\sub\style.css`, "apps/my-app/public/sub/style.css"},
		{"drive letter path", `C:\main.go`, "apps/my-app/main.go"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScopePath(scopedDir, tc.relPath)
			if got != tc.expected {
				t.Errorf("ScopePath(%q, %q) = %q, expected %q", scopedDir, tc.relPath, got, tc.expected)
			}
		})
	}
}

// TestAutoScope_CommentOnlyGoMod_NoRootPollution verifies that a multi-file response
// containing a go.mod with comments only (no module declaration) and main.go, public/index.html
// is recognized as foreign, auto-scoped to apps/app/, and does NOT pollute the root directory.
func TestAutoScope_CommentOnlyGoMod_NoRootPollution(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	mockReply := `Project without explicit module declaration:

[ACTION:WRITE path="go.mod"]
` + "```go\n// Comments only\n// Go 1.21\n```\n\n" +
		`[ACTION:WRITE path="main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```\n\n" +
		`[ACTION:WRITE path="public/index.html"]
` + "```html\n<h1>Clean App</h1>\n```"

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	err := RunConversationalUI(context.Background(), session, strings.NewReader("generate app\n/quit\n"), &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	// 1. Verify Auto-Scope badge in output (defaults safely to apps/app/)
	expectedBadge := "[Auto-Scope Applied] Scoped new application to: apps/app/"
	if !strings.Contains(out.String(), expectedBadge) {
		t.Errorf("Expected auto-scope badge %q in output, got:\n%s", expectedBadge, out.String())
	}

	// 2. Verify root go.mod was NOT overwritten
	rootData, err := os.ReadFile(rootModPath)
	if err != nil {
		t.Fatalf("Failed to read root go.mod: %v", err)
	}
	if string(rootData) != hostModContent {
		t.Errorf("CRITICAL VIOLATION: root go.mod was modified!")
	}

	// 3. Verify root main.go was NOT created
	if _, err := os.Stat(filepath.Join(tmpDir, "main.go")); !os.IsNotExist(err) {
		t.Errorf("Pollution detected: main.go exists at workspace root")
	}

	// 4. Verify root public/ was NOT created
	if _, err := os.Stat(filepath.Join(tmpDir, "public")); !os.IsNotExist(err) {
		t.Errorf("Pollution detected: public/ exists at workspace root")
	}

	// 5. Verify files exist in apps/app/
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "app", "go.mod")); err != nil {
		t.Errorf("Expected apps/app/go.mod to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "app", "main.go")); err != nil {
		t.Errorf("Expected apps/app/main.go to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "app", "public", "index.html")); err != nil {
		t.Errorf("Expected apps/app/public/index.html to exist: %v", err)
	}
}

// TestAutoScope_AbsolutePathInTurn verifies that write actions declaring absolute
// paths within the workspace root are correctly scoped without deep path nesting.
func TestAutoScope_AbsolutePathInTurn(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	absGoMod := filepath.Join(tmpDir, "go.mod")
	absMainGo := filepath.Join(tmpDir, "main.go")

	mockReply := fmt.Sprintf(`Multi-file absolute paths:

[ACTION:WRITE path=%q]
`+"```go\nmodule abs-app\n\ngo 1.21\n```\n\n"+
		`[ACTION:WRITE path=%q]
`+"```go\npackage main\n\nfunc main() {}\n```", absGoMod, absMainGo)

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	err := RunConversationalUI(context.Background(), session, strings.NewReader("generate abs-app\n/quit\n"), &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	// Verify files are placed in apps/abs-app/go.mod and apps/abs-app/main.go
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "abs-app", "go.mod")); err != nil {
		t.Errorf("Expected apps/abs-app/go.mod to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "abs-app", "main.go")); err != nil {
		t.Errorf("Expected apps/abs-app/main.go to exist: %v", err)
	}

	// Verify root go.mod remained untouched
	rootData, _ := os.ReadFile(rootModPath)
	if string(rootData) != hostModContent {
		t.Errorf("Root go.mod was modified!")
	}
}

// TestIsWorkspaceRootFile_NonCwdWorkspace verifies isWorkspaceRootFile
// accurately detects root files when ws is a separate directory from os.Getwd().
func TestIsWorkspaceRootFile_NonCwdWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	if !isWorkspaceRootFile(tmpDir, "go.mod", "go.mod") {
		t.Errorf("isWorkspaceRootFile should return true for relative 'go.mod'")
	}
	if !isWorkspaceRootFile(tmpDir, "./go.mod", "go.mod") {
		t.Errorf("isWorkspaceRootFile should return true for './go.mod'")
	}
	if !isWorkspaceRootFile(tmpDir, filepath.Join(tmpDir, "go.mod"), "go.mod") {
		t.Errorf("isWorkspaceRootFile should return true for absolute path to root go.mod")
	}
	if isWorkspaceRootFile(tmpDir, "apps/go.mod", "go.mod") {
		t.Errorf("isWorkspaceRootFile should return false for subfolder 'apps/go.mod'")
	}
	if isWorkspaceRootFile(tmpDir, "main.go", "go.mod") {
		t.Errorf("isWorkspaceRootFile should return false for different filename 'main.go'")
	}
}

// TestExtractModuleName_ParenthesizedBlock verifies extraction of module names
// formatted in legal Go parenthesized module ( ... ) blocks.
func TestExtractModuleName_ParenthesizedBlock(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{"multiline block", "module (\n\tfiber-portal\n)\n", "fiber-portal"},
		{"multiline with comments", "module (\n\t// comment line\n\tfiber-portal // inline comment\n)\n", "fiber-portal"},
		{"single line parenthesized", "module ( fiber-portal )\n", "fiber-portal"},
		{"quoted parenthesized", "module (\n\t\"quoted-app\"\n)\n", "quoted-app"},
		{"single quoted parenthesized", "module (\n\t'single-app'\n)\n", "single-app"},
		{"prefixed block comment", "/* header */ module (\n\tcomment-prefixed-app\n)\n", "comment-prefixed-app"},
		{"empty parenthesized block", "module (\n)\n", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractModuleName([]byte(tc.input))
			if got != tc.expected {
				t.Errorf("ExtractModuleName(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		})
	}
}

// TestRootGoMod_EmptyHostModCollisionProtection verifies that a workspace root
// go.mod file containing only comments (empty extracted module) is actively protected against
// being overwritten by incoming foreign modules.
func TestRootGoMod_EmptyHostModCollisionProtection(t *testing.T) {
	tmpDir := t.TempDir()
	commentModContent := "// Workspace root go.mod with comments only\n// Go 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(rootModPath, []byte(commentModContent), 0644); err != nil {
		t.Fatalf("Failed to write initial comment-only root go.mod: %v", err)
	}

	session := NewSession(nil, nil, tmpDir)

	// Attempt to overwrite root go.mod with a foreign module must be rejected
	foreignModContent := "module rogue-app\n\ngo 1.21\n"
	_, _, err := session.WriteFile("go.mod", []byte(foreignModContent))
	if err == nil {
		t.Fatalf("Expected error when overwriting comment-only root go.mod with foreign module, got nil")
	}

	expectedErr := `ui: refusing to overwrite workspace root go.mod (module "") with foreign module "rogue-app"; new applications must be placed in a subfolder (e.g. 'apps/<name>/go.mod')`
	if err.Error() != expectedErr {
		t.Errorf("Error mismatch:\nExpected: %q\nGot:      %q", expectedErr, err.Error())
	}

	// Verify root go.mod on disk remains completely untouched
	currentData, err := os.ReadFile(rootModPath)
	if err != nil {
		t.Fatalf("Failed to read root go.mod: %v", err)
	}
	if string(currentData) != commentModContent {
		t.Errorf("Root go.mod was corrupted or modified:\nGot:\n%s", string(currentData))
	}
}

// TestRootGoMod_TrailingDotProtection verifies that attempts to overwrite
// root go.mod or git infrastructure using trailing dot/space vectors on Windows are blocked.
func TestRootGoMod_TrailingDotProtection(t *testing.T) {
	tmpDir := t.TempDir()
	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(rootModPath, []byte(hostModContent), 0644); err != nil {
		t.Fatalf("Failed to write initial root go.mod: %v", err)
	}

	session := NewSession(nil, nil, tmpDir)

	// 1. Attempt to overwrite root go.mod using "go.mod." must be rejected
	foreignModContent := "module evil-app\n\ngo 1.21\n"
	_, _, err := session.WriteFile("go.mod.", []byte(foreignModContent))
	if err == nil {
		t.Fatalf("Expected error when writing to 'go.mod.', got nil")
	}

	// Verify root go.mod is untouched
	data, _ := os.ReadFile(rootModPath)
	if string(data) != hostModContent {
		t.Errorf("Root go.mod was modified via trailing dot vector!")
	}

	// 2. Attempt to overwrite .gitmodules using ".gitmodules." must be rejected
	_, _, err = session.WriteFile(".gitmodules.", []byte("[submodule]\n"))
	if err == nil {
		t.Fatalf("Expected error when writing to '.gitmodules.', got nil")
	}
	if !strings.Contains(err.Error(), "target path cannot be inside .git directory or overwrite git infrastructure") {
		t.Errorf("Unexpected error message: %v", err)
	}
}

// TestAutoScope_TargetContextFromAbsolutePath verifies that if a turn has a comment-only
// go.mod and an absolute path inside the workspace targeting an apps/ subfolder, the project name
// is properly extracted from the absolute sibling action.
func TestAutoScope_TargetContextFromAbsolutePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	absSibling := filepath.Join(tmpDir, "apps", "quantum-portal", "main.go")

	mockReply := fmt.Sprintf(`Multi-file absolute context:

[ACTION:WRITE path="go.mod"]
`+"```go\n// comment only, no module directive\n```\n\n"+
		`[ACTION:WRITE path=%q]
`+"```go\npackage main\n\nfunc main() {}\n```", absSibling)

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	err := RunConversationalUI(context.Background(), session, strings.NewReader("generate quantum portal\n/quit\n"), &out)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	expectedBadge := "[Auto-Scope Applied] Scoped new application to: apps/quantum-portal/"
	if !strings.Contains(out.String(), expectedBadge) {
		t.Errorf("Expected badge %q in output, got:\n%s", expectedBadge, out.String())
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "quantum-portal", "go.mod")); err != nil {
		t.Errorf("Expected apps/quantum-portal/go.mod to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "apps", "quantum-portal", "main.go")); err != nil {
		t.Errorf("Expected apps/quantum-portal/main.go to exist: %v", err)
	}
}

// TestAutoScope_ExternalAbsolutePathBreakoutRejection verifies that an external
// absolute path declared in an auto-scoped multi-file turn is rejected rather than masked
// into apps/<project>/, and cannot write outside the workspace.
func TestAutoScope_ExternalAbsolutePathBreakoutRejection(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostModContent := "module github.com/caleralabs/omnicoder-icx\n\ngo 1.21\n"
	rootModPath := filepath.Join(tmpDir, "go.mod")
	_ = os.WriteFile(rootModPath, []byte(hostModContent), 0644)

	outsideDir := t.TempDir()
	canaryPath := filepath.Join(outsideDir, "outside_canary.txt")
	_ = os.Remove(canaryPath)

	mockReply := fmt.Sprintf(`Multi-file breakout attempt:

[ACTION:WRITE path="go.mod"]
`+"```go\nmodule escape-app\n\ngo 1.21\n```\n\n"+
		`[ACTION:WRITE path="main.go"]
`+"```go\npackage main\n\nfunc main() {}\n```\n\n"+
		`[ACTION:WRITE path=%q]
`+"```text\nEXPLOIT_PAYLOAD\n```", canaryPath)

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)

	var out bytes.Buffer
	_ = RunConversationalUI(context.Background(), session, strings.NewReader("generate escape\n/quit\n"), &out)

	// 1. Outside canary must NOT exist
	if _, statErr := os.Stat(canaryPath); statErr == nil {
		t.Fatalf("SECURITY VIOLATION: Canary file was written outside workspace at %s", canaryPath)
	}

	// 2. Legitimate files inside apps/escape-app/ must exist
	if _, statErr := os.Stat(filepath.Join(tmpDir, "apps", "escape-app", "go.mod")); statErr != nil {
		t.Errorf("Expected apps/escape-app/go.mod to exist: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(tmpDir, "apps", "escape-app", "main.go")); statErr != nil {
		t.Errorf("Expected apps/escape-app/main.go to exist: %v", statErr)
	}
}

// TestSanitizeProjectName_TrailingSlashesAndV2 verifies project name sanitization
// when trailing slashes accompany major version suffixes and git extensions.
func TestSanitizeProjectName_TrailingSlashesAndV2(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"github.com/org/repo/v2/", "repo"},
		{"github.com/org/repo///", "repo"},
		{"github.com/org/repo.git/v2/", "repo"},
		{"github.com/org/repo.git///", "repo"},
		{"github.com/org/service/v10///", "service"},
		{"/deep/nested/path/v3/", "path"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := SanitizeProjectName(tc.input)
			if got != tc.expected {
				t.Errorf("SanitizeProjectName(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		})
	}
}

// TestExtractContentForIntent_ActionTagLeakGuard verifies that raw action tags
// and CLI status headers are never extracted into file payloads.
func TestExtractContentForIntent_ActionTagLeakGuard(t *testing.T) {
	// Case 1: Unclosed code fence followed by [ACTION:BUILD] tag
	unclosedFenceResponse := `[ACTION:WRITE path="projects/my-service/go.mod"]
` + "```go\nmodule myservice\n\ngo 1.22\n" + `
[ACTION:BUILD task="go test ./..."]
`
	intents := ParseAllActionIntents(unclosedFenceResponse)
	if len(intents) < 2 {
		t.Fatalf("expected at least 2 intents, got %d", len(intents))
	}
	content := intents[0].ExtractContent(unclosedFenceResponse)
	if strings.Contains(content, "[ACTION:BUILD") {
		t.Errorf("content contains leaked [ACTION:BUILD]:\n%s", content)
	}
	if !strings.Contains(content, "module myservice") {
		t.Errorf("content missing expected Go module definition:\n%s", content)
	}

	// Case 2: Code block that contains ONLY an action tag
	tagInBlockResponse := `[ACTION:WRITE path="projects/my-service/pkg/canonical/canonical.go"]
` + "```bash\n[ACTION:BUILD task=\"go test ./...\"]\n```" + `
`
	intent := ParseActionIntent(tagInBlockResponse)
	if intent == nil {
		t.Fatal("expected parsed intent")
	}
	content2 := intent.ExtractContent(tagInBlockResponse)
	if content2 != "" {
		t.Errorf("expected empty content for malformed tag in code block, got: %q", content2)
	}

	// Case 3: Valid code with trailing action tag leak from unclosed fence
	trailingLeakResponse := `[ACTION:WRITE path="main.go"]
` + "```go\npackage main\n\nfunc main() {}\n" + `
[ACTION:BUILD task="go build"]
`
	intent3 := ParseActionIntent(trailingLeakResponse)
	if intent3 == nil {
		t.Fatal("expected parsed intent")
	}
	content3 := intent3.ExtractContent(trailingLeakResponse)
	if strings.Contains(content3, "[ACTION:BUILD") {
		t.Errorf("trailing action tag was not sanitized from content:\n%s", content3)
	}
	if !strings.Contains(content3, "func main()") {
		t.Errorf("expected code func main() missing:\n%s", content3)
	}

	// Case 4: Extracted text starting with CLI log headers
	if !isMalformedTagContent("[File Created] projects/my-service/main.go") {
		t.Error("expected [File Created] to be detected as malformed")
	}
	if !isMalformedTagContent("[Auto-Write Detected] Target File: foo.go") {
		t.Error("expected [Auto-Write Detected] to be detected as malformed")
	}
	if !isMalformedTagContent("[ACTION:BUILD task=\"go test\"]") {
		t.Error("expected [ACTION:BUILD] to be detected as malformed")
	}
}

// TestSelfHealing_CircuitBreaker verifies that repeated identical verification
// diagnostics trip the circuit breaker and halt the loop after consecutive identical errors.
func TestSelfHealing_CircuitBreaker(t *testing.T) {
	tmpDir := t.TempDir()

	// Mock LLM client that returns the same failing reply with no valid fix
	mockReply := `I will try to fix the test.
[ACTION:WRITE path="apps/fail/main.go"]
` + "```go\npackage main\ninvalid go code syntax [\n```" + `
[ACTION:BUILD task="go build -o test.exe apps/fail/main.go"]
`
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = mockReply
	session := NewSession(mockClient, nil, tmpDir)
	defer session.Close()

	var out bytes.Buffer
	ctx := context.Background()

	_ = executeConversationalTurn(ctx, session, "build application", &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Self-Healing Circuit Breaker]") {
		t.Errorf("expected circuit breaker to trip on repeated failure, output:\n%s", outStr)
	}
}


// TestMalformedTagGuard_NoFalsePositive verifies that valid Go code
// starting with "package" is NOT rejected by the Malformed Tag Guard even if the
// raw extracted content happens to contain [ACTION:WRITE ...] somewhere after
// the code (e.g., leaked from the next intent in the LLM response).
// Regression test for the bug where actionStartRegex.MatchString() matched
// ANYWHERE in the content instead of only at position 0.
func TestMalformedTagGuard_NoFalsePositive(t *testing.T) {
	// Case 1: Pure Go code should never trigger malformed tag guard
	pureGoCode := `package icx

import "math"

func Distance(a, b [5]int) float64 {
	var sum float64
	for i := 0; i < 5; i++ {
		diff := float64(a[i] - b[i])
		sum += diff * diff
	}
	return math.Sqrt(sum)
}
`
	if isMalformedTagContent(pureGoCode) {
		t.Error("pure Go code was falsely detected as malformed tag content")
	}

	// Case 2: Go code that mentions [ACTION in a comment should not be rejected
	codeWithComment := `package main

// This handler processes [ACTION:WRITE] requests from the client.
func handleWrite() {}
`
	if isMalformedTagContent(codeWithComment) {
		t.Error("Go code with [ACTION] in a comment was falsely detected as malformed")
	}

	// Case 3: Content that actually starts with [ACTION:WRITE should be detected
	actualMalformed := `[ACTION:WRITE path="foo.go"]
package main
func main() {}
`
	if !isMalformedTagContent(actualMalformed) {
		t.Error("content starting with [ACTION:WRITE] should be detected as malformed")
	}
}
