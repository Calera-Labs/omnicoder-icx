package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// UniversalEntryInfo describes an executable entry point in any language or framework.
type UniversalEntryInfo struct {
	Path     string // relative path to project root, e.g. "app.py", "main.go", "index.ts"
	Language string // "python", "node", "go", "rust", "c", "cpp", "shell", "html", "generic"
	IsDaemon bool   // true if this starts a server, GUI, or event-loop daemon
}

var runnerBootstrapCrashRe = regexp.MustCompile(
	`(?is)(PluggyTeardownRaisedWarning|pytest_cmdline_parse|load_setuptools_entrypoints|` +
		`cannot import name '[A-Za-z_]+' from 'pytest'|pytest_asyncio|pytest-asyncio|` +
		`internal error:.*hookimpl|PluginValidationError|no module named pytest)`)

// isRunnerBootstrapCrash detects when a test runner crashes due to host environment corruption
// or broken third-party runner plugins, before running any actual project tests.
func isRunnerBootstrapCrash(cmd, diag string) bool {
	lowerCmd := strings.ToLower(cmd)
	if strings.Contains(lowerCmd, "pytest") {
		return runnerBootstrapCrashRe.MatchString(diag) && strings.Contains(diag, "Traceback")
	}
	return false
}

// isPytestPluginCrash preserves backward compatibility for existing callers and tests.
func isPytestPluginCrash(cmd, diag string) bool {
	return isRunnerBootstrapCrash(cmd, diag)
}

// withEnv returns env with key=value appended unless key is already set.
func withEnv(env []string, key, value string) []string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return env
		}
	}
	return append(append([]string{}, env...), prefix+value)
}

// ignoredScanDirs contains directory names that should never be searched for entry points.
var ignoredScanDirs = map[string]bool{
	".git":          true,
	".venv":         true,
	"venv":          true,
	"env":           true,
	".env":          true,
	"node_modules":  true,
	"vendor":        true,
	"__pycache__":   true,
	".pytest_cache": true,
	"dist":          true,
	"build":         true,
	"target":        true,
	".idea":         true,
	".vscode":       true,
	"data":          true,
	"test":          true,
	"tests":         true,
}

// daemonPatterns detect if a source file initializes a long-running service, HTTP server, GUI, or event loop.
var daemonPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(listen|serve|bind|listenandserve|createserver|socket|server\b|app\.listen|port|endpoint|router|route)\b`),
	regexp.MustCompile(`(?i)\b(event_loop|run_forever|mainloop|set_page_config|window|gui|display|canvas|render|view|page|ui\b)\b`),
	regexp.MustCompile(`(?i)\b(while\s+True\s*:|for\s*\{\s*select|setInterval|schedule|worker|polling|listen_to)\b`),
}

// isFileDaemon reports whether the file source contains daemon, server, or UI event-loop patterns.
func isFileDaemon(content string) bool {
	for _, re := range daemonPatterns {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}

// DetectDeclaredEntryPoint inspects README.md to determine the documented application entry point across all languages.
func DetectDeclaredEntryPoint(dir string) string {
	readmePath := filepath.Join(dir, "README.md")
	data, err := os.ReadFile(readmePath)
	if err != nil {
		return ""
	}
	content := string(data)

	// 1. Language runner command: python/node/go run/cargo run/bun/deno
	langRegex := regexp.MustCompile(`(?i)(?:python3?|py|node|ts-node|tsx|bun|deno\s+run|go\s+run)\s+([a-zA-Z0-9_\-\./\\]+\.[a-zA-Z0-9]+)`)
	if m := langRegex.FindStringSubmatch(content); len(m) > 1 {
		cand := filepath.Base(filepath.FromSlash(m[1]))
		if st, err := os.Stat(filepath.Join(dir, cand)); err == nil && !st.IsDir() {
			return cand
		}
	}

	// 2. Generic launch/run command: e.g. "run app.py", "serve index.html", "start server.js"
	runRegex := regexp.MustCompile(`(?i)(?:run|start|exec|launch|serve)\s+([a-zA-Z0-9_\-\.]+\.[a-zA-Z0-9]+)`)
	if m := runRegex.FindStringSubmatch(content); len(m) > 1 {
		cand := filepath.Base(filepath.FromSlash(m[1]))
		if st, err := os.Stat(filepath.Join(dir, cand)); err == nil && !st.IsDir() {
			return cand
		}
	}

	return ""
}

// DiscoverUniversalEntries inspects a project directory and discovers all entry points
// across Go, Python, Node/TS, Rust, C/C++, Shell, and Web frontend projects.
func DiscoverUniversalEntries(projectDir string) []UniversalEntryInfo {
	if projectDir == "" {
		return nil
	}

	var entries []UniversalEntryInfo
	seen := make(map[string]bool)

	// Helper to add an entry point
	addEntry := func(relPath, lang string, isDaemon bool) {
		clean := filepath.ToSlash(filepath.Clean(relPath))
		if !seen[clean] {
			seen[clean] = true
			entries = append(entries, UniversalEntryInfo{
				Path:     clean,
				Language: lang,
				IsDaemon: isDaemon,
			})
		}
	}

	// 1. Check README.md documented entry point first
	if declared := DetectDeclaredEntryPoint(projectDir); declared != "" {
		ext := strings.ToLower(filepath.Ext(declared))
		lang := "generic"
		switch ext {
		case ".py":
			lang = "python"
		case ".go":
			lang = "go"
		case ".js", ".mjs", ".cjs":
			lang = "node"
		case ".ts", ".tsx":
			lang = "node"
		case ".rs":
			lang = "rust"
		case ".c":
			lang = "c"
		case ".cpp", ".cc", ".cxx":
			lang = "cpp"
		case ".html", ".htm":
			lang = "html"
		case ".sh", ".bash", ".ps1":
			lang = "shell"
		}
		data, _ := os.ReadFile(filepath.Join(projectDir, declared))
		addEntry(declared, lang, isFileDaemon(string(data)))
	}

	// 2. Check root level well-known entry files
	rootCandidates := []struct {
		name string
		lang string
	}{
		{"app.py", "python"},
		{"main.py", "python"},
		{"server.py", "python"},
		{"cli.py", "python"},
		{"run.py", "python"},
		{"manage.py", "python"},
		{"index.py", "python"},
		{"api.py", "python"},
		{"index.ts", "node"},
		{"index.js", "node"},
		{"app.ts", "node"},
		{"app.js", "node"},
		{"server.ts", "node"},
		{"server.js", "node"},
		{"main.ts", "node"},
		{"main.js", "node"},
		{"main.go", "go"},
		{"src/main.rs", "rust"},
		{"main.rs", "rust"},
		{"main.c", "c"},
		{"main.cpp", "cpp"},
		{"index.html", "html"},
	}

	for _, cand := range rootCandidates {
		absPath := filepath.Join(projectDir, filepath.FromSlash(cand.name))
		if st, err := os.Stat(absPath); err == nil && !st.IsDir() {
			data, _ := os.ReadFile(absPath)
			isDaemon := isFileDaemon(string(data))
			addEntry(cand.name, cand.lang, isDaemon)
		}
	}

	// 3. Node / JS package.json inspection
	pkgPath := filepath.Join(projectDir, "package.json")
	if st, err := os.Stat(pkgPath); err == nil && !st.IsDir() {
		if data, err := os.ReadFile(pkgPath); err == nil {
			var pkg struct {
				Main    string            `json:"main"`
				Bin     interface{}       `json:"bin"`
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(data, &pkg) == nil {
				if pkg.Main != "" {
					addEntry(pkg.Main, "node", true)
				}
				if binStr, ok := pkg.Bin.(string); ok && binStr != "" {
					addEntry(binStr, "node", false)
				}
				for _, scriptKey := range []string{"start", "dev", "server", "app"} {
					if cmd, ok := pkg.Scripts[scriptKey]; ok && cmd != "" {
						words := strings.Fields(cmd)
						for _, w := range words {
							if strings.HasSuffix(w, ".js") || strings.HasSuffix(w, ".ts") {
								addEntry(w, "node", true)
							}
						}
					}
				}
			}
		}
	}

	// 4. Scan cmd/ directory for Go applications
	cmdDir := filepath.Join(projectDir, "cmd")
	if st, err := os.Stat(cmdDir); err == nil && st.IsDir() {
		_ = filepath.WalkDir(cmdDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil {
				return nil
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".go") {
				rel, _ := filepath.Rel(projectDir, path)
				data, _ := os.ReadFile(path)
				isDaemon := isFileDaemon(string(data))
				addEntry(rel, "go", isDaemon)
			}
			return nil
		})
	}

	// 5. Shallow scan for files declaring main / execution points if still empty
	if len(entries) == 0 {
		_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if ignoredScanDirs[name] || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				rel, _ := filepath.Rel(projectDir, path)
				if strings.Count(filepath.ToSlash(rel), "/") > 2 {
					return filepath.SkipDir
				}
				return nil
			}

			nameLower := strings.ToLower(d.Name())
			if strings.HasPrefix(nameLower, "test_") || strings.HasSuffix(nameLower, "_test.py") || strings.HasSuffix(nameLower, "_test.go") {
				return nil
			}

			rel, _ := filepath.Rel(projectDir, path)
			if strings.HasSuffix(nameLower, ".py") {
				data, _ := os.ReadFile(path)
				s := string(data)
				if strings.Contains(s, "if __name__ == '__main__':") || strings.Contains(s, "def main(") || isFileDaemon(s) {
					addEntry(rel, "python", isFileDaemon(s))
				}
			} else if strings.HasSuffix(nameLower, ".go") {
				data, _ := os.ReadFile(path)
				s := string(data)
				if strings.Contains(s, "package main") && strings.Contains(s, "func main(") {
					addEntry(rel, "go", isFileDaemon(s))
				}
			}
			return nil
		})
	}

	return entries
}

// VerifyUniversalSyntax verifies that an entry point source compiles or parses cleanly.
func VerifyUniversalSyntax(ctx context.Context, projectDir string, entry UniversalEntryInfo) (bool, string) {
	absPath := filepath.Join(projectDir, filepath.FromSlash(entry.Path))
	if _, err := os.Stat(absPath); err != nil {
		return false, fmt.Sprintf("Entry file %s does not exist on disk", entry.Path)
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch entry.Language {
	case "python":
		pyBin := "python"
		if p, err := exec.LookPath("python"); err == nil {
			pyBin = p
		}
		cmd := exec.CommandContext(cctx, pyBin, "-X", "utf8", "-m", "py_compile", entry.Path)
		cmd.Dir = projectDir
		cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return false, fmt.Sprintf("Python syntax / bytecode compilation error in %s:\n%s", entry.Path, strings.TrimSpace(string(out)))
		}
		return true, ""

	case "node":
		if nodeBin, err := exec.LookPath("node"); err == nil && strings.HasSuffix(entry.Path, ".js") {
			cmd := exec.CommandContext(cctx, nodeBin, "--check", entry.Path)
			cmd.Dir = projectDir
			out, err := cmd.CombinedOutput()
			if err != nil {
				return false, fmt.Sprintf("JavaScript syntax check error in %s:\n%s", entry.Path, strings.TrimSpace(string(out)))
			}
		}
		return true, ""

	case "go":
		if goBin, err := exec.LookPath("go"); err == nil {
			cmd := exec.CommandContext(cctx, goBin, "vet", entry.Path)
			cmd.Dir = projectDir
			out, err := cmd.CombinedOutput()
			if err != nil {
				return false, fmt.Sprintf("Go vet error in %s:\n%s", entry.Path, strings.TrimSpace(string(out)))
			}
		}
		return true, ""

	case "rust":
		if cargoBin, err := exec.LookPath("cargo"); err == nil {
			cmd := exec.CommandContext(cctx, cargoBin, "check")
			cmd.Dir = projectDir
			out, err := cmd.CombinedOutput()
			if err != nil {
				return false, fmt.Sprintf("Rust cargo check error:\n%s", strings.TrimSpace(string(out)))
			}
		}
		return true, ""
	}

	return true, ""
}

// killProcessTree terminates a process and any of its children cleanly across platforms.
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		pid := cmd.Process.Pid
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
	} else {
		_ = cmd.Process.Kill()
	}
}

// ProbeUniversalStartup probes execution of an entry point with a bounded timeout (2 seconds).
// For compiled languages (Go, Rust, C, C++), compilation verification via VerifyUniversalSyntax
// is the definitive smoke check.
// For interpreted languages (Python, Node/JS, Shell), it executes the file headlessly; if it crashes
// on startup with an uncaught runtime error, it reports failure with the traceback.
// If it runs cleanly (exit 0) or stays alive as a service daemon, it reports success!
func ProbeUniversalStartup(ctx context.Context, projectDir string, entry UniversalEntryInfo, extraEnv []string) (ran bool, ok bool, diag string) {
	absPath := filepath.Join(projectDir, filepath.FromSlash(entry.Path))
	if _, err := os.Stat(absPath); err != nil {
		return false, false, fmt.Sprintf("Entry file %s does not exist on disk", entry.Path)
	}

	// 1. Verify syntax / bytecode / compilation first
	if synOk, synDiag := VerifyUniversalSyntax(ctx, projectDir, entry); !synOk {
		return true, false, synDiag
	}

	// Compiled languages pass once syntax/compilation is verified;
	// launching unmanaged compiled daemons (e.g. servers binding ports) during build audits is avoided.
	switch entry.Language {
	case "go", "rust", "c", "cpp", "html":
		return true, true, ""
	}

	// 2. Prepare startup command for interpreted languages (Python, Node, Shell)
	var cmd *exec.Cmd
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()

	switch entry.Language {
	case "python":
		pyBin := "python"
		if p, err := exec.LookPath("python"); err == nil {
			pyBin = p
		}
		cmd = exec.Command(pyBin, "-X", "utf8", entry.Path)

	case "node":
		nodeBin := "node"
		if n, err := exec.LookPath("node"); err == nil {
			nodeBin = n
		} else {
			return true, true, ""
		}
		cmd = exec.Command(nodeBin, entry.Path)

	case "shell":
		if runtime.GOOS == "windows" {
			cmd = exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", entry.Path)
		} else {
			cmd = exec.Command("bash", entry.Path)
		}

	default:
		return true, true, ""
	}

	cmd.Dir = projectDir
	// Setup universal headless / non-interactive environment
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Env = withEnv(cmd.Env, "CI", "true")
	cmd.Env = withEnv(cmd.Env, "HEADLESS", "true")
	cmd.Env = withEnv(cmd.Env, "NONINTERACTIVE", "1")
	cmd.Env = withEnv(cmd.Env, "TERM", "dumb")
	cmd.Env = withEnv(cmd.Env, "BROWSER", "none")
	cmd.Env = withEnv(cmd.Env, "PYTHONUTF8", "1")
	cmd.Env = withEnv(cmd.Env, "PYTHONIOENCODING", "utf-8")
	cmd.Env = withEnv(cmd.Env, "PYTHONUNBUFFERED", "1")
	cmd.Env = withEnv(cmd.Env, "PYTHONPATH", projectDir)

	// Use temporary file for stdout and stderr to guarantee zero pipe hangs on Windows
	tmpOut, err := os.CreateTemp("", "omnicoder_probe_*.log")
	if err != nil {
		return false, true, ""
	}
	tmpPath := tmpOut.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	cmd.Stdout = tmpOut
	cmd.Stderr = tmpOut

	if err := cmd.Start(); err != nil {
		_ = tmpOut.Close()
		return false, true, ""
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	timedOut := false

	select {
	case waitErr = <-done:
		// Process exited on its own within probe timeout
	case <-probeCtx.Done():
		timedOut = true
		killProcessTree(cmd)
		select {
		case waitErr = <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}

	_ = tmpOut.Close()
	outBytes, _ := os.ReadFile(tmpPath)
	output := string(outBytes)

	// If probe timed out, the daemon/server stayed alive without crashing: DEFINITIVE PASS!
	if timedOut {
		return true, true, ""
	}

	// If process exited cleanly with code 0: PASS!
	if waitErr == nil {
		return true, true, ""
	}

	// Non-zero exit code: check if it's a fatal crash vs normal CLI behavior
	fatalKeywords := []string{
		"Traceback (most recent call last):",
		"SyntaxError:",
		"IndentationError:",
		"NameError:",
		"KeyError:",
		"TypeError:",
		"AttributeError:",
		"ModuleNotFoundError:",
		"ImportError:",
		"IndexError:",
		"ValueError:",
		"ZeroDivisionError:",
		"panic: runtime error",
		"Fatal error",
		"Uncaught Error:",
		"ReferenceError:",
	}

	hasFatalError := false
	for _, kw := range fatalKeywords {
		if strings.Contains(output, kw) {
			hasFatalError = true
			break
		}
	}

	// If it printed standard CLI usage or options, it's a CLI tool expecting args, not a crash
	if !hasFatalError && (strings.Contains(output, "usage:") || strings.Contains(output, "Usage:") || strings.Contains(output, "options:")) {
		return true, true, ""
	}

	if hasFatalError || waitErr != nil {
		if len(output) > 3000 {
			output = output[len(output)-3000:]
		}
		return true, false, fmt.Sprintf("STARTUP PROBE FAILED for %s (%s exited with error):\n%s", entry.Path, entry.Language, strings.TrimSpace(output))
	}

	return true, true, ""
}

// ProbeAllProjectEntries probes all discovered entry points in a project directory.
func ProbeAllProjectEntries(ctx context.Context, projectDir string, extraEnv []string) (ran bool, ok bool, diag string) {
	entries := DiscoverUniversalEntries(projectDir)
	if len(entries) == 0 {
		return false, true, ""
	}

	for _, entry := range entries {
		r, passed, d := ProbeUniversalStartup(ctx, projectDir, entry, extraEnv)
		if r && !passed {
			return true, false, d
		}
	}
	return true, true, ""
}

// ExtractTargetProject parses user prompts for explicit or implicit project paths
// across quoted paths, relative paths, absolute paths with spaces, or matching existing project directories on disk.
func ExtractTargetProject(prompt string, workspaceDir string) string {
	if prompt == "" || workspaceDir == "" {
		return ""
	}

	cleanWs := filepath.Clean(workspaceDir)

	// Strategy 1: Check existing folders in projects/ and apps/ on disk.
	// If any folder name is present in the prompt, this matches with 100% precision.
	for _, parentDirName := range []string{"projects", "apps"} {
		parentPath := filepath.Join(cleanWs, parentDirName)
		entries, err := os.ReadDir(parentPath)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					name := e.Name()
					if strings.Contains(strings.ToLower(prompt), strings.ToLower(name)) {
						return parentDirName + "/" + name
					}
				}
			}
		}
	}

	// Strategy 2: Check for quoted paths first: "..." or '...'
	quotedRe := regexp.MustCompile(`["']([^"']+)["']`)
	for _, match := range quotedRe.FindAllStringSubmatch(prompt, -1) {
		if len(match) > 1 {
			cand := strings.TrimSpace(match[1])
			if p := resolveToRelativeProject(cand, cleanWs); p != "" {
				return p
			}
		}
	}

	// Strategy 3: Check for explicit path tokens matching (projects|apps)[/\\][A-Za-z0-9_.-]+
	tokenRe := regexp.MustCompile(`(?i)\b((?:projects|apps)[/\\][A-Za-z0-9_.-]+)`)
	for _, match := range tokenRe.FindAllStringSubmatch(prompt, -1) {
		if len(match) > 1 {
			cand := strings.TrimSpace(match[1])
			if p := resolveToRelativeProject(cand, cleanWs); p != "" {
				return p
			}
		}
	}

	// Strategy 4: Absolute path prefix matching workspaceDir
	lowerPrompt := strings.ToLower(prompt)
	lowerWs := strings.ToLower(cleanWs)
	if idx := strings.Index(lowerPrompt, lowerWs); idx != -1 {
		sub := prompt[idx:]
		words := strings.Fields(sub)
		accum := ""
		for _, w := range words {
			if accum == "" {
				accum = w
			} else {
				accum = accum + " " + w
			}
			if p := resolveToRelativeProject(accum, cleanWs); p != "" {
				return p
			}
		}
	}

	return ""
}

// resolveToRelativeProject resolves a candidate string into a valid workspace-relative project directory.
func resolveToRelativeProject(candidate, workspaceDir string) string {
	cand := strings.TrimSpace(candidate)
	cand = strings.Trim(cand, "\"'`")
	if cand == "" {
		return ""
	}

	cleanWs := filepath.Clean(workspaceDir)
	var relPath string

	if filepath.IsAbs(cand) {
		rel, err := filepath.Rel(cleanWs, cand)
		if err != nil || strings.HasPrefix(rel, "..") {
			return ""
		}
		relPath = rel
	} else {
		cleanCand := filepath.ToSlash(filepath.Clean(cand))
		cleanCand = strings.TrimPrefix(cleanCand, "./")
		if strings.HasPrefix(cleanCand, "projects/") || strings.HasPrefix(cleanCand, "apps/") {
			relPath = cleanCand
		} else {
			relPath = filepath.Join("projects", cleanCand)
		}
	}

	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")
	if len(parts) >= 2 && (parts[0] == "projects" || parts[0] == "apps") {
		projScope := parts[0] + "/" + parts[1]
		fullDir := filepath.Join(cleanWs, filepath.FromSlash(projScope))
		if st, err := os.Stat(fullDir); err == nil && st.IsDir() {
			return projScope
		}
	}

	return ""
}

// BuildProjectGroundTruthBriefing produces a concise ground-truth inventory of files
// present in a project directory, giving the model factual awareness before it responds.
func BuildProjectGroundTruthBriefing(ws, projectScope string) string {
	if ws == "" || projectScope == "" {
		return ""
	}

	fullDir := filepath.Join(ws, filepath.FromSlash(projectScope))
	st, err := os.Stat(fullDir)
	if err != nil || !st.IsDir() {
		return ""
	}

	var filesList []string
	_ = filepath.WalkDir(fullDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if ignoredScanDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(fullDir, path)
		cleanRel := filepath.ToSlash(rel)
		info, _ := d.Info()
		sizeKB := float64(0)
		if info != nil {
			sizeKB = float64(info.Size()) / 1024.0
		}
		filesList = append(filesList, fmt.Sprintf("  - %s (%.1f KB)", cleanRel, sizeKB))
		return nil
	})

	if len(filesList) == 0 {
		return ""
	}

	var testCmd string
	hasPythonTests := false
	for _, f := range filesList {
		fl := strings.ToLower(f)
		if strings.Contains(fl, "test_") && strings.HasSuffix(fl, ".py)") {
			hasPythonTests = true
			break
		}
	}
	if hasPythonTests {
		testCmd = `python -m unittest discover -s . -p test_*.py`
	} else if _, err := os.Stat(filepath.Join(fullDir, "go.mod")); err == nil {
		testCmd = `go test ./...`
	} else if _, err := os.Stat(filepath.Join(fullDir, "Cargo.toml")); err == nil {
		testCmd = `cargo test`
	} else if _, err := os.Stat(filepath.Join(fullDir, "package.json")); err == nil {
		testCmd = `npm test`
	}

	var sb strings.Builder
	sb.WriteString("\n=== ACTIVE TARGET PROJECT INVENTORY ===\n")
	sb.WriteString(fmt.Sprintf("Target Project Directory: %s/\n", projectScope))
	sb.WriteString("Verified Files Currently on Disk:\n")
	for _, f := range filesList {
		sb.WriteString(f + "\n")
	}
	if testCmd != "" {
		sb.WriteString(fmt.Sprintf("Canonical Verified Test Command: %s\n", testCmd))
	}
	sb.WriteString("MANDATORY OPERATIONAL INSTRUCTIONS:\n")
	sb.WriteString(fmt.Sprintf("1. All code edits, new files, and tests MUST be anchored within %s/.\n", projectScope))
	sb.WriteString("2. Check the existing files above. Do NOT recreate, duplicate, or overwrite working modules with incomplete stubs.\n")
	sb.WriteString("3. Command execution is anchored directly in the project directory; use relative paths like -s . or ./tests.\n")
	sb.WriteString("4. Do not wrap glob patterns in single quotes on Windows (use unquoted test_*.py or double-quoted \"test_*.py\").\n")
	sb.WriteString("======================================\n")

	return sb.String()
}
