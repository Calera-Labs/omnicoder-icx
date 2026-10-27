// SPDX-License-Identifier: Apache-2.0
// Package ui provides a zero-dependency, pure Go terminal conversational interface
// for OmniCoder-ICX.

package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ANSI color escape codes for terminal formatting.
const (
	ansiReset        = "\033[0m"
	ansiBold         = "\033[1m"
	ansiDim          = "\033[2m"
	ansiCyan         = "\033[36m"
	ansiBrightCyan   = "\033[96m"
	ansiGreen        = "\033[32m"
	ansiBrightGreen  = "\033[92m"
	ansiYellow       = "\033[33m"
	ansiBrightYellow = "\033[93m"
	ansiRed          = "\033[31m"
	ansiBrightRed    = "\033[91m"
	ansiMagenta      = "\033[35m"
	ansiBrightMag    = "\033[95m"
	ansiGray         = "\033[90m"
)

// SwarmIntentRegex detects conversational swarm execution directives like "Complete this project xyz.md".
var SwarmIntentRegex = regexp.MustCompile(`(?i)^(complete\s+(this\s+)?project|swarm)\s+["']?(.+?\.md)["']?(\s*)$`)

// TerminalTheme provides color styling with graceful degradation for non-ANSI environments.
type TerminalTheme struct {
	ColorEnabled bool
}

// NewTerminalTheme detects whether color is supported or disabled via NO_COLOR / TERM.
func NewTerminalTheme() *TerminalTheme {
	noColor := os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	return &TerminalTheme{ColorEnabled: !noColor}
}

// colorize applies the specified escape codes if color is enabled.
func (t *TerminalTheme) colorize(code, s string) string {
	if !t.ColorEnabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (t *TerminalTheme) Bold(s string) string        { return t.colorize(ansiBold, s) }
func (t *TerminalTheme) Dim(s string) string         { return t.colorize(ansiDim, s) }
func (t *TerminalTheme) Cyan(s string) string        { return t.colorize(ansiCyan, s) }
func (t *TerminalTheme) BrightCyan(s string) string  { return t.colorize(ansiBrightCyan, s) }
func (t *TerminalTheme) Green(s string) string       { return t.colorize(ansiGreen, s) }
func (t *TerminalTheme) BrightGreen(s string) string { return t.colorize(ansiBrightGreen, s) }
func (t *TerminalTheme) Yellow(s string) string      { return t.colorize(ansiYellow, s) }
func (t *TerminalTheme) BrightYellow(s string) string {
	return t.colorize(ansiBrightYellow, s)
}
func (t *TerminalTheme) Red(s string) string           { return t.colorize(ansiRed, s) }
func (t *TerminalTheme) BrightRed(s string) string     { return t.colorize(ansiBrightRed, s) }
func (t *TerminalTheme) Magenta(s string) string       { return t.colorize(ansiMagenta, s) }
func (t *TerminalTheme) BrightMagenta(s string) string { return t.colorize(ansiBrightMag, s) }
func (t *TerminalTheme) Gray(s string) string          { return t.colorize(ansiGray, s) }

// Badge formats a status badge like [Model: gemini-3.5-flash-lite].
func (t *TerminalTheme) Badge(key, val string) string {
	if !t.ColorEnabled {
		return fmt.Sprintf("[%s: %s]", key, val)
	}
	return fmt.Sprintf("%s[%s: %s%s%s]%s",
		ansiGray,
		t.Cyan(key),
		ansiBrightGreen,
		val,
		ansiGray,
		ansiReset)
}

// PrintBanner renders the welcome banner with OmniCoder-ICX title and badges.
func PrintBanner(out io.Writer, session *Session) {
	theme := NewTerminalTheme()
	model := session.Model()
	if model == "" {
		model = llm.DefaultModel
	}
	ws := session.WorkspaceDir()
	if ws == "" {
		ws = "."
	}

	fmt.Fprintln(out, theme.Cyan("================================================================================"))
	fmt.Fprintf(out, "  %s  ::  %s\n",
		theme.Bold(theme.BrightCyan("OMNICODER-ICX")),
		theme.Gray("Autonomous Subterranean Software Engineering Engine"))
	fmt.Fprintln(out, theme.Cyan("================================================================================"))
	fmt.Fprintf(out, "  %s  %s  %s\n",
		theme.Badge("Model", model),
		theme.Badge("Workspace", ws),
		theme.Badge("Subterranean", "8-Stage Engine"))
	fmt.Fprintf(out, "  Type %s for slash commands, or enter a prompt to begin planning.\n", theme.Yellow("/help"))
	fmt.Fprintln(out, theme.Cyan("--------------------------------------------------------------------------------"))
}

// PrintHelp prints the slash command reference guide.
func PrintHelp(out io.Writer) {
	theme := NewTerminalTheme()
	fmt.Fprintln(out, theme.Bold(theme.BrightCyan("\nOmniCoder-ICX Slash Commands:")))
	fmt.Fprintf(out, "  %s                   - %s\n", theme.Yellow("/help"), "Display this command guide")
	fmt.Fprintf(out, "  %s            - %s\n", theme.Yellow("/map [budget]"), "Synthesize PageRank AST repository map (default: 4000 tokens)")
	fmt.Fprintf(out, "  %s - %s\n", theme.Yellow("/plan, /run, /build, /goal, /boost <task>"), "Execute autonomous engineering task through 8-stage engine")
	fmt.Fprintf(out, "  %s                - %s\n", theme.Yellow("/heal <cmd>"), "Run compiler feedback loop and self-heal command failures")
	fmt.Fprintf(out, "  %s - %s\n", theme.Yellow("/refactor <sym> <sig>"), "Refactor symbol signature across workspace callers")
	fmt.Fprintf(out, "  %s              - %s\n", theme.Yellow("/save <path>"), "Save the last assistant turn to disk (also /write)")
	fmt.Fprintf(out, "  %s                  - %s\n", theme.Yellow("/paste"), "Ingest and execute large items from system clipboard or terminal")
	fmt.Fprintf(out, "  %s              - %s\n", theme.Yellow("/multiline"), "Toggle multi-line input mode on/off (also /ml)")
	fmt.Fprintf(out, "  %s               - %s\n", theme.Yellow("/model [name]"), "Inspect or switch active LLM (e.g. gemini-3.8-flash, gemini-3.7-flash)")
	fmt.Fprintf(out, "  %s                 - %s\n", theme.Yellow("/status"), "Display current session state and token metrics")
	fmt.Fprintf(out, "  %s                 - %s\n", theme.Yellow("/memory"), "Inspect ICX 4D volumetric memory surfaces (0D-4D)")
	fmt.Fprintf(out, "  %s                  - %s\n", theme.Yellow("/reset"), "Reset conversation history (workspace memory preserved)")
	fmt.Fprintf(out, "  %s            - %s\n", theme.Yellow("/exit, /quit"), "Exit conversational prompt loop")
	fmt.Fprintln(out)
}

// PrintStatus prints active session metadata and connectivity metrics.
func PrintStatus(out io.Writer, session *Session) {
	theme := NewTerminalTheme()
	st := session.Status()

	fmt.Fprintln(out, theme.Bold(theme.BrightCyan("\nSession Status:")))
	fmt.Fprintf(out, "  Model:         %s\n", theme.Green(st.Model))
	fmt.Fprintf(out, "  Workspace:     %s\n", theme.Cyan(st.WorkspaceDir))
	if st.ActiveProject != "" {
		fmt.Fprintf(out, "  Active Project:%s\n", theme.BrightGreen(" "+st.ActiveProject))
	}
	fmt.Fprintf(out, "  Turns:         %d dialogue turns\n", st.TurnCount)
	fmt.Fprintf(out, "  History:       %d messages in context\n", st.HistoryLen)
	if st.LatticeNodes > 0 {
		fmt.Fprintf(out, "  Lattice Nodes: %s\n", theme.Cyan(fmt.Sprintf("%d indexed nodes (0D-4D)", st.LatticeNodes)))
	}
	engineStatus := theme.Red("Disconnected")
	if st.HasEngine {
		engineStatus = theme.Green("Connected & Ready (8-Stage)")
	}
	fmt.Fprintf(out, "  Subterranean:  %s\n\n", engineStatus)
}

// FormatProgressEvent formats an 8-stage turn progress event for display.
func FormatProgressEvent(event engine.TurnProgressEvent) string {
	theme := NewTerminalTheme()
	var statusColored string
	switch strings.ToUpper(event.Status) {
	case "START":
		statusColored = theme.Bold(theme.Magenta("START"))
	case "STAGE":
		statusColored = theme.Cyan("STAGE")
	case "PASS":
		statusColored = theme.Green("PASS")
	case "HEAL":
		healInfo := ""
		if event.HealAttempt > 0 {
			healInfo = fmt.Sprintf(" (%d/%d)", event.HealAttempt, event.MaxHeals)
		}
		statusColored = theme.Yellow("HEAL" + healInfo)
	case "FAIL":
		statusColored = theme.Red("FAIL")
	case "ROLLBACK":
		statusColored = theme.Yellow("ROLLBACK")
	default:
		statusColored = event.Status
	}

	stageDesc := event.StageName
	if stageDesc == "" && event.Stage > 0 {
		stageDesc = fmt.Sprintf("Stage %d", event.Stage)
	}

	if event.Turn > 0 {
		if event.MaxTurns > 0 {
			return fmt.Sprintf("  [%s %d/%d] [%s] %s: %s",
				theme.Bold("Turn"), event.Turn, event.MaxTurns,
				theme.Bold(stageDesc), statusColored, event.Details)
		}
		return fmt.Sprintf("  [%s %d] [%s] %s: %s",
			theme.Bold("Turn"), event.Turn,
			theme.Bold(stageDesc), statusColored, event.Details)
	}
	return fmt.Sprintf("  [%s] %s: %s", theme.Bold(stageDesc), statusColored, event.Details)
}

// RenderProgressEvent writes a formatted progress line to the specified writer.
func RenderProgressEvent(out io.Writer, event engine.TurnProgressEvent) {
	line := FormatProgressEvent(event)
	fmt.Fprintln(out, line)
}

// MakeProgressCallback constructs a progress callback bound to an output writer.
func MakeProgressCallback(out io.Writer) func(event engine.TurnProgressEvent) {
	return func(event engine.TurnProgressEvent) {
		RenderProgressEvent(out, event)
	}
}

// FormatReportSummary converts a RunReport into a formatted multi-line summary string.
func FormatReportSummary(report *engine.RunReport) string {
	if report == nil {
		return "  Execution Report: (nil)\n"
	}
	theme := NewTerminalTheme()
	statusStr := theme.Green("SUCCESS")
	if !report.Success {
		statusStr = theme.Red("FAILED")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("  Execution Status: %s\n", statusStr))
	sb.WriteString(fmt.Sprintf("  Task:             %s\n", report.Task))
	sb.WriteString(fmt.Sprintf("  Duration:         %s\n", report.Duration.Round(time.Millisecond)))
	sb.WriteString(fmt.Sprintf("  Turns Executed:   %d\n", report.TurnsCount))
	if report.HealCount > 0 {
		sb.WriteString(fmt.Sprintf("  Heal Cycles:      %d\n", report.HealCount))
	}
	if len(report.FilesTouched) > 0 {
		sb.WriteString(fmt.Sprintf("  Files Touched:    %s\n", strings.Join(report.FilesTouched, ", ")))
	} else {
		sb.WriteString("  Files Touched:    (none)\n")
	}
	if len(report.Commits) > 0 {
		sb.WriteString(fmt.Sprintf("  Commits:          %s\n", strings.Join(report.Commits, ", ")))
	}
	if report.MerkleSeal != "" {
		sb.WriteString(fmt.Sprintf("  Merkle Seal:      %s\n", report.MerkleSeal))
	}
	if report.ErrorMessage != "" {
		sb.WriteString(fmt.Sprintf("  Error Message:    %s\n", report.ErrorMessage))
	}
	return sb.String()
}

// FormatRepoMapSummary formats a RepoMapResult into a human-readable summary string.
func FormatRepoMapSummary(res *devops.RepoMapResult) string {
	if res == nil {
		return "Repository map: (empty)"
	}
	var sb strings.Builder
	sb.WriteString("Repository Map Synthesized:\n")
	sb.WriteString(fmt.Sprintf("  Files Scanned: %d\n", res.FilesScanned))
	sb.WriteString(fmt.Sprintf("  Symbols Found: %d\n", res.SymbolsFound))
	sb.WriteString(fmt.Sprintf("  Tokens:        %d\n", res.TokenCount))
	if res.RepoMap != "" {
		sb.WriteString("--------------------------------------------------------------------------------\n")
		sb.WriteString(res.RepoMap)
		sb.WriteString("\n--------------------------------------------------------------------------------\n")
	}
	return sb.String()
}

// ResolvePlanFile searches for an implementation plan file across canonical workspace and documentation paths.
func ResolvePlanFile(target string, workspaceDir string) (string, string) {
	return engine.ResolvePlanPath(target, workspaceDir)
}

// FormatPlanPrompt formats the prompt injected into the LLM context when an implementation plan is detected,
// preserving any extra user instructions while scoping the initial turn to Phase 1.
func FormatPlanPrompt(cleanTarget, planPathUsed, planContent string, ws ...string) string {
	workspaceDir := "."
	if len(ws) > 0 && ws[0] != "" {
		workspaceDir = ws[0]
	}

	extra := cleanTarget
	extra = strings.ReplaceAll(extra, planPathUsed, "")
	extra = strings.ReplaceAll(extra, filepath.Base(planPathUsed), "")
	extra = strings.Trim(extra, `"'`+"`"+`:;`)
	extra = strings.TrimSpace(extra)

	planSpec := engine.ParsePlanMarkdown(planContent)
	if planSpec != nil && len(planSpec.Phases) > 0 {
		activePhase := planSpec.DetermineActivePhase(workspaceDir)
		if activePhase == nil {
			activePhase = &planSpec.Phases[0]
		}
		var sb strings.Builder
		sb.WriteString("=== AUTONOMOUS SOFTWARE ENGINEERING: MASTER PLAN ACTIVE ===\n")
		sb.WriteString(fmt.Sprintf("Master Plan: %s (%d bytes, %d Phases)\n", planPathUsed, len(planContent), len(planSpec.Phases)))
		if planSpec.ProjectScope != "" {
			sb.WriteString(fmt.Sprintf("Project Scope: %s (Module: %s)\n", planSpec.ProjectScope, planSpec.ModuleName))
		}
		if extra != "" && !strings.EqualFold(extra, "build") && !strings.EqualFold(extra, "run") {
			sb.WriteString(fmt.Sprintf("User Instruction: %s\n\n", extra))
		}

		sb.WriteString("=== STRICT PHASE ISOLATION MANDATE ===\n")
		sb.WriteString(fmt.Sprintf("You are beginning Milestone: Phase %d: %s.\n", activePhase.PhaseNumber, activePhase.Name))
		sb.WriteString("DO NOT attempt to generate the entire multi-phase project in one turn! Doing so produces shallow stubs and causes test failures.\n")
		sb.WriteString(fmt.Sprintf("Focus EXCLUSIVELY on implementing the target files for Phase %d with full production-grade completeness.\n\n", activePhase.PhaseNumber))
		sb.WriteString(planSpec.GetPhasePrompt(activePhase.PhaseNumber, workspaceDir))
		return sb.String()
	}

	if extra != "" && !strings.EqualFold(extra, "build") && !strings.EqualFold(extra, "run") {
		return fmt.Sprintf("User Instruction: %s\n\nBuild and implement all components according to master implementation plan at %s:\n\n%s", extra, planPathUsed, planContent)
	}
	return fmt.Sprintf("Build and implement all components according to master implementation plan at %s:\n\n%s", planPathUsed, planContent)
}

// checkImplementationDepth inspects newly written file content to ensure it meets
// production-grade substance requirements and is not a toy placeholder or stub.
func checkImplementationDepth(targetPath, content string, planSpec *engine.PlanSpec) (bool, string) {
	if os.Getenv("OMNICODER_DISABLE_DEPTH_GUARD") == "1" {
		return false, ""
	}

	cleanPath := filepath.ToSlash(strings.TrimSpace(targetPath))
	base := filepath.Base(cleanPath)
	ext := strings.ToLower(filepath.Ext(cleanPath))

	// Exempt files that can legitimately be small
	if base == "go.mod" || base == "go.sum" || base == "embed.go" || base == "doc.go" || base == "version.go" ||
		base == "api.go" || base == "routes.go" || base == "handlers.go" || base == "errors.go" ||
		base == "types.go" || base == "constants.go" || base == "interfaces.go" || base == "options.go" ||
		strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") {
		return false, ""
	}

	lines := strings.Split(content, "\n")
	nonEmptyLines := 0
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" && !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "/*") && !strings.HasPrefix(trimmed, "*") {
			nonEmptyLines++
		}
	}

	// Dynamic depth thresholds based on plan scale:
	// For production master plans (>2KB markdown), require full production depth.
	// For short test fixtures or direct interactive CLI tasks, allow compact files.
	if planSpec == nil || len(planSpec.RawPlanContent) < 2000 {
		return false, ""
	}

	minGoLines := 25
	minJSLines := 30
	minPyLines := 20

	// 1. Go files in pkg/ or internal/
	if ext == ".go" && (strings.Contains(cleanPath, "/pkg/") || strings.Contains(cleanPath, "/internal/")) {
		if hasSubstantiveGoLogic(content, nonEmptyLines) {
			return false, ""
		}
		if !strings.HasSuffix(base, "_test.go") && nonEmptyLines < minGoLines {
			return true, fmt.Sprintf("Go package file %s has only %d non-empty lines of code (minimum %d lines required for a production package)", base, nonEmptyLines, minGoLines)
		}
	}

	// 2. JavaScript files in web/
	if ext == ".js" && strings.Contains(cleanPath, "/web/") {
		if nonEmptyLines < minJSLines {
			return true, fmt.Sprintf("JavaScript module %s has only %d non-empty lines of code (minimum %d lines required for a complete client controller)", base, nonEmptyLines, minJSLines)
		}
	}

	// 3. Python files
	if ext == ".py" {
		if base == "__init__.py" || base == "config.py" || base == "types.py" || base == "server_runner.py" {
			return false, ""
		}
		if hasSubstantivePythonLogic(content, nonEmptyLines) {
			return false, ""
		}
		if !strings.HasPrefix(base, "test_") && !strings.HasSuffix(base, "_test.py") && nonEmptyLines < minPyLines {
			return true, fmt.Sprintf("Python package file %s has only %d non-empty lines of code (minimum %d lines required for a production module)", base, nonEmptyLines, minPyLines)
		}
	}

	return false, ""
}

// hasSubstantivePythonLogic returns true if a Python file contains valid functions/classes and no placeholder stubs.
func hasSubstantivePythonLogic(content string, nonEmptyLines int) bool {
	if nonEmptyLines < 15 {
		return false
	}
	lower := strings.ToLower(content)
	if strings.Contains(lower, "pass  # todo") || strings.Contains(lower, "raise notimplementederror") {
		return false
	}
	return strings.Contains(content, "def ") || strings.Contains(content, "class ")
}

// hasSubstantiveGoLogic returns true if a Go file contains valid, substantive logic
// (has package, imports, functions/methods or structs, and >= 15 non-empty lines with no stub markers).
func hasSubstantiveGoLogic(content string, nonEmptyLines int) bool {
	if nonEmptyLines < 15 {
		return false
	}
	lower := strings.ToLower(content)
	if strings.Contains(lower, "todo") || strings.Contains(lower, "unimplemented") || strings.Contains(lower, "panic(\"not implemented\")") {
		return false
	}
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", content, 0)
	if err != nil {
		return false
	}
	for _, decl := range node.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && len(fn.Body.List) > 0 {
			return true
		}
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE && len(gen.Specs) > 0 {
			return true
		}
	}
	return false
}

// executeTask triggers an autonomous engineering task via engineOrch.ExecuteTask,
// formats the execution report, and feeds the summary back into conversational context.
func executeTask(ctx context.Context, session *Session, task string, out io.Writer) error {
	theme := NewTerminalTheme()
	orch := session.Engine()
	if orch == nil {
		fmt.Fprintln(out, theme.Yellow("[Notice] Subterranean engine is not configured; task execution skipped."))
		return nil
	}

	fmt.Fprintln(out, theme.Cyan("\n================================================================================"))
	fmt.Fprintf(out, "  %s\n", theme.Bold(fmt.Sprintf("AUTONOMOUS EXECUTION: %s", task)))
	fmt.Fprintln(out, theme.Cyan("================================================================================"))

	start := time.Now()
	report, err := orch.ExecuteTask(ctx, engine.RunConfig{
		Task:         task,
		WorkspaceDir: session.WorkspaceDir(),
	})
	elapsed := time.Since(start)
	if report != nil && report.Duration == 0 {
		report.Duration = elapsed
	}

	fmt.Fprintln(out, theme.Cyan("--------------------------------------------------------------------------------"))
	if report != nil {
		summaryText := FormatReportSummary(report)
		fmt.Fprint(out, summaryText)
		session.FeedExecutionSummary(summaryText)
	}
	if err != nil {
		fmt.Fprintf(out, "  %s: %v\n", theme.Red("Execution Error"), err)
		return err
	}
	fmt.Fprintln(out, theme.Cyan("================================================================================\n"))
	return nil
}

var doneWordRegex = regexp.MustCompile(`(?i)(?:\bDONE\b|\bALL\s+PHASES\s+(?:ARE\s+)?(?:COMPLETE|VERIFIED|FINISHED)\b|\bTASK\s+COMPLETE\b|\bPROJECT\s+COMPLETE\b)`)

// isDoneResponse returns true if the assistant's reply explicitly indicates task completion with the word "DONE" or completion statement.
func isDoneResponse(reply string) bool {
	return doneWordRegex.MatchString(reply)
}

// looksLikeShellCommand verifies that a command string looks like an executable shell invocation
// rather than descriptive English prose (e.g., "Verify complete project" vs "go test ./...").
func looksLikeShellCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return false
	}

	lower := strings.ToLower(trimmed)
	prosePrefixes := []string{
		"verify ", "check ", "ensure ", "test the ", "run the ", "please ",
		"validate ", "inspect ", "make sure ", "confirm ", "build the ",
		"build and ", "compile and ", "test and ", "review ", "execute the ",
	}
	for _, prefix := range prosePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}

	// A trailing "." is the current package (`go build .`), not a sentence.
	fieldsEarly := strings.Fields(trimmed)
	lastIsPkg := len(fieldsEarly) > 0 && (fieldsEarly[len(fieldsEarly)-1] == "." || fieldsEarly[len(fieldsEarly)-1] == "./...")
	if strings.HasSuffix(trimmed, ".") && !strings.HasSuffix(trimmed, "/...") && !strings.HasSuffix(trimmed, "./.") && !lastIsPkg {
		return false
	}

	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false
	}
	firstToken := strings.ToLower(fields[0])

	// Known command binaries and development tools
	knownBinaries := map[string]bool{
		"go": true, "npm": true, "npx": true, "yarn": true, "pnpm": true,
		"cargo": true, "make": true, "pytest": true, "python": true, "python3": true,
		"node": true, "deno": true, "bun": true, "bash": true, "sh": true,
		"powershell": true, "pwsh": true, "cmd": true, "git": true, "docker": true,
		"docker-compose": true, "mvn": true, "gradle": true, "dotnet": true,
		"tsc": true, "rustc": true, "gcc": true, "g++": true, "clang": true,
		"clang++": true, "cmake": true, "ctest": true, "nmake": true,
		"pip": true, "pip3": true, "py": true, "rustup": true,
		"cargo-clippy": true, "echo": true, "cat": true, "ls": true,
		"dir": true, "rm": true, "del": true, "cp": true, "mv": true,
		"mkdir": true, "touch": true, "curl": true, "wget": true, "tar": true,
		"zip": true, "unzip": true, "find": true, "grep": true, "sed": true,
		"awk": true, "cd": true, "set": true, "export": true,
	}

	if knownBinaries[firstToken] {
		return true
	}

	// Starts with path/script invocation: ./, ../, / or .\, ..\, \ or Windows drive letter C:\
	if strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, ".\\") ||
		strings.HasPrefix(trimmed, "../") || strings.HasPrefix(trimmed, "..\\") ||
		strings.HasPrefix(trimmed, "/") || (len(trimmed) >= 3 && trimmed[1] == ':' && (trimmed[2] == '\\' || trimmed[2] == '/')) {
		return true
	}

	firstBase := filepath.Base(firstToken)
	for _, ext := range []string{".exe", ".bat", ".cmd", ".sh", ".ps1", ".py", ".js", ".bin"} {
		if strings.HasSuffix(firstBase, ext) {
			return true
		}
	}

	// Contains shell chaining / redirection: &&, ||, |, ;, >, <
	if strings.Contains(trimmed, "&&") || strings.Contains(trimmed, "||") ||
		strings.Contains(trimmed, "|") || strings.Contains(trimmed, ";") ||
		strings.Contains(trimmed, ">") {
		return true
	}

	// Check if any arguments are CLI flags (e.g. -v, --flag)
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			return true
		}
	}

	// If it has multiple words with no flags or known binary, and starts with capital letter, it's descriptive prose
	if len(fields) > 2 && unicode.IsUpper(rune(trimmed[0])) {
		return false
	}

	return false
}

var (
	// Windows cmd: 'foo' is not recognized as an internal or external command, operable program or batch file.
	winCmdMissingToolRegex = regexp.MustCompile(`(?i)(?:'([^']+)'|(\S+))\s+is not recognized as an internal or external command`)
	// Windows PowerShell: The term 'foo' is not recognized as the name of a cmdlet...
	winPsMissingToolRegex = regexp.MustCompile(`(?i)The term\s+'([^']+)'\s+is not recognized as the name of a cmdlet`)
	// Linux/macOS bash/zsh/sh: bash: foo: command not found or bash: line 1: foo: command not found
	unixBashMissingToolRegex = regexp.MustCompile(`(?i)(?:bash|sh|zsh|dash)(?:\.exe)?:\s*(?:line\s+\d+:\s*)?([^:\s]+):\s*(?:command\s+)?not\s+found`)
	unixZshMissingToolRegex  = regexp.MustCompile(`(?i)zsh:\s*command\s+not\s+found:\s*(\S+)`)
	// Go exec: exec: "foo": executable file not found
	goExecMissingToolRegex  = regexp.MustCompile(`(?i)exec:\s*"([^"]+)":\s*executable file not found`)
	generalCmdNotFoundRegex = regexp.MustCompile(`(?i)(?:^|\s)command\s+not\s+found:\s*(\S+)`)
)

// ExtractMissingTool dynamically identifies missing tool names from CLI/OS error output without hardcoding tool names.
func ExtractMissingTool(diag string) string {
	if diag == "" {
		return ""
	}
	clean := func(tool string) string {
		t := strings.TrimSpace(tool)
		t = strings.Trim(t, "\"'`")
		t = strings.ReplaceAll(t, "\\", "/")
		t = filepath.Base(filepath.FromSlash(t))
		t = strings.TrimSuffix(strings.ToLower(t), ".exe")
		t = strings.TrimSuffix(t, ".bat")
		t = strings.TrimSuffix(t, ".cmd")
		t = strings.TrimSuffix(t, ".sh")
		t = strings.TrimSuffix(t, ".ps1")
		return strings.TrimSpace(t)
	}

	if m := winCmdMissingToolRegex.FindStringSubmatch(diag); len(m) > 1 {
		if m[1] != "" {
			return clean(m[1])
		}
		if len(m) > 2 && m[2] != "" {
			return clean(m[2])
		}
	}
	if m := winPsMissingToolRegex.FindStringSubmatch(diag); len(m) > 1 {
		return clean(m[1])
	}
	if m := unixBashMissingToolRegex.FindStringSubmatch(diag); len(m) > 1 {
		return clean(m[1])
	}
	if m := unixZshMissingToolRegex.FindStringSubmatch(diag); len(m) > 1 {
		return clean(m[1])
	}
	if m := goExecMissingToolRegex.FindStringSubmatch(diag); len(m) > 1 {
		return clean(m[1])
	}
	if m := generalCmdNotFoundRegex.FindStringSubmatch(diag); len(m) > 1 {
		return clean(m[1])
	}
	return ""
}

// DetectUserErrorReport inspects a user prompt to determine if it is reporting a CLI, compiler, or runtime error.
// Returns (isErrorReport, missingTool, summary).
func DetectUserErrorReport(prompt string) (bool, string, string) {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return false, "", ""
	}

	lower := strings.ToLower(trimmed)
	missingTool := ExtractMissingTool(trimmed)

	errorMarkers := []string{
		"is not recognized as an internal or external command",
		"is not recognized as the name of a cmdlet",
		"command not found",
		"executable file not found",
		"cannot find module",
		"modulenotfounderror",
		"no such file or directory",
		"no such file",
		"file does not exist",
		"does not exist",
		"cannot find the path",
		"can't open file",
		"file not found",
		"invalid value:",
		"errno 2",
		"build failed",
		"fatal:",
		"exit status",
		"exit code 1",
		"compilation error",
		"traceback (most recent call last)",
		"exception in thread",
		"panic: runtime error",
		"undefined:",
		"cannot use",
		"syntaxerror:",
		"referenceerror:",
		"typeerror:",
		"requires cgo",
	}

	isReport := false
	if missingTool != "" {
		isReport = true
	} else {
		for _, marker := range errorMarkers {
			if strings.Contains(lower, marker) {
				isReport = true
				break
			}
		}
	}

	if !isReport {
		return false, "", ""
	}

	lines := strings.Split(trimmed, "\n")
	var summaryLines []string
	for _, l := range lines {
		tl := strings.TrimSpace(l)
		if tl == "" {
			continue
		}
		summaryLines = append(summaryLines, tl)
		if len(summaryLines) >= 3 {
			break
		}
	}
	summary := strings.Join(summaryLines, " | ")
	return true, missingTool, summary
}

// DetectProjectVerificationCmd inspects targetDir for language/ecosystem markers and returns an appropriate verification command.
func DetectProjectVerificationCmd(targetDir string) string {
	if targetDir == "" {
		return ""
	}

	// 1. Go project
	goModPath := filepath.Join(targetDir, "go.mod")
	if st, err := os.Stat(goModPath); err == nil && !st.IsDir() {
		hasTests := false
		hasGoFiles := false
		hasRootGoFiles := false
		if entries, err := os.ReadDir(targetDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".go") {
					hasRootGoFiles = true
					break
				}
			}
		}
		_ = filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			if info.IsDir() && path != targetDir {
				name := info.Name()
				if name == "vendor" || name == ".git" || name == "node_modules" {
					return filepath.SkipDir
				}
			}
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".go") {
				hasGoFiles = true
				if strings.HasSuffix(info.Name(), "_test.go") {
					hasTests = true
					return filepath.SkipAll
				}
			}
			return nil
		})
		if hasTests {
			return "go test ./..."
		}
		if hasGoFiles && !hasRootGoFiles {
			return "go build ./..."
		}
		return "go build ."
	}

	// 2. Node / JS / TS project
	pkgJSONPath := filepath.Join(targetDir, "package.json")
	if st, err := os.Stat(pkgJSONPath); err == nil && !st.IsDir() {
		if data, err := os.ReadFile(pkgJSONPath); err == nil {
			var pkg struct {
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(data, &pkg) == nil && pkg.Scripts != nil {
				if testCmd, ok := pkg.Scripts["test"]; ok && testCmd != "" && !strings.Contains(testCmd, "no test specified") {
					return "npm test"
				}
				if _, ok := pkg.Scripts["build"]; ok {
					return "npm run build"
				}
			}
		}
		if _, err := os.Stat(filepath.Join(targetDir, "index.js")); err == nil {
			return "node --check index.js"
		}
		return "npm test"
	}

	// 3. Rust project
	cargoPath := filepath.Join(targetDir, "Cargo.toml")
	if st, err := os.Stat(cargoPath); err == nil && !st.IsDir() {
		return "cargo test"
	}

	// 4. CMake / C / C++ project
	cmakePath := filepath.Join(targetDir, "CMakeLists.txt")
	if st, err := os.Stat(cmakePath); err == nil && !st.IsDir() {
		return "cmake -B build && cmake --build build"
	}

	// 5. Java Maven / Gradle project
	pomPath := filepath.Join(targetDir, "pom.xml")
	if st, err := os.Stat(pomPath); err == nil && !st.IsDir() {
		return "mvn test-compile"
	}
	gradlePath := filepath.Join(targetDir, "build.gradle")
	gradleKtsPath := filepath.Join(targetDir, "build.gradle.kts")
	if _, err := os.Stat(gradlePath); err == nil {
		return "gradle test"
	}
	if _, err := os.Stat(gradleKtsPath); err == nil {
		return "gradle test"
	}

	// 6. .NET / C# project
	var hasDotnet bool
	_ = filepath.Walk(targetDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() {
			low := strings.ToLower(fi.Name())
			if strings.HasSuffix(low, ".csproj") || strings.HasSuffix(low, ".sln") {
				hasDotnet = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if hasDotnet {
		return "dotnet test"
	}

	// 7. Python project
	pyProject := filepath.Join(targetDir, "pyproject.toml")
	setupPy := filepath.Join(targetDir, "setup.py")
	reqTxt := filepath.Join(targetDir, "requirements.txt")
	hasPy := false
	if _, err := os.Stat(pyProject); err == nil {
		hasPy = true
	} else if _, err := os.Stat(setupPy); err == nil {
		hasPy = true
	} else if _, err := os.Stat(reqTxt); err == nil {
		hasPy = true
	} else {
		_ = filepath.Walk(targetDir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi != nil {
				if fi.IsDir() {
					name := fi.Name()
					if name == ".venv" || name == "venv" || name == ".git" || name == "__pycache__" || name == "node_modules" {
						return filepath.SkipDir
					}
				} else if strings.HasSuffix(strings.ToLower(fi.Name()), ".py") {
					hasPy = true
					return filepath.SkipAll
				}
			}
			return nil
		})
	}
	if hasPy {
		usesPytest := false
		hasTestFiles := false
		hasSubstantiveTestsDir := false
		_ = filepath.Walk(targetDir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi != nil {
				if fi.IsDir() {
					name := fi.Name()
					if name == ".venv" || name == "venv" || name == ".git" || name == "__pycache__" || name == "node_modules" {
						return filepath.SkipDir
					}
				} else if strings.HasSuffix(strings.ToLower(fi.Name()), ".py") {
					lowName := strings.ToLower(fi.Name())
					rel, _ := filepath.Rel(targetDir, p)
					relSlash := filepath.ToSlash(rel)
					isTest := strings.HasPrefix(lowName, "test_") || strings.HasSuffix(lowName, "_test.py") ||
						((strings.Contains(relSlash, "/tests/") || strings.HasPrefix(relSlash, "tests/")) && lowName != "__init__.py")
					if isTest {
						hasTestFiles = true
						if strings.Contains(relSlash, "/tests/") || strings.HasPrefix(relSlash, "tests/") {
							hasSubstantiveTestsDir = true
						}
					}
					if data, rErr := os.ReadFile(p); rErr == nil {
						if strings.Contains(string(data), "import pytest") || strings.Contains(string(data), "from pytest") {
							usesPytest = true
						}
					}
				}
			}
			return nil
		})
		if !hasTestFiles {
			return "python -m compileall -q ."
		}
		if usesPytest {
			return "python -m compileall -q . && pytest"
		}
		if hasSubstantiveTestsDir {
			return "python -m compileall -q . && python -m unittest discover -s tests -p \"test_*.py\""
		}
		return "python -m compileall -q . && python -m unittest discover -s . -p \"*test*.py\""
	}

	// 8. Makefile
	makePath := filepath.Join(targetDir, "Makefile")
	if _, err := os.Stat(makePath); err == nil {
		return "make test"
	}

	return ""
}

// IsDaemonCommand identifies long-running dev servers, watchers, or interactive processes
// that do not terminate on their own and would block automated verification loops.
func IsDaemonCommand(cmd string) bool {
	norm := strings.ToLower(strings.TrimSpace(cmd))
	daemonPrefixes := []string{
		"go run ",
		"npm start",
		"npm run dev",
		"npm run serve",
		"yarn start",
		"yarn dev",
		"pnpm dev",
		"pnpm start",
		"vite",
		"next dev",
		"python app.py",
		"python main.py",
		"python server.py",
		"python3 app.py",
		"python3 main.py",
		"python3 server.py",
		"python -m http.server",
		"python3 -m http.server",
		"http-server",
		"live-server",
		"uvicorn ",
		"gunicorn ",
		"fastapi dev",
		"flask run",
		"nodemon ",
		"cargo run",
	}
	if norm == "go run ." || norm == "go run main.go" || norm == "npm start" || norm == "npm run dev" {
		return true
	}
	for _, p := range daemonPrefixes {
		if strings.HasPrefix(norm, p) {
			return true
		}
	}
	return false
}

// SubstituteDaemonWithVerification replaces a non-terminating daemon command with a deterministic
// compile/test verification command for that project.
func SubstituteDaemonWithVerification(cmd, targetDir string) string {
	norm := strings.ToLower(strings.TrimSpace(cmd))
	if strings.HasPrefix(norm, "go run") {
		if targetDir != "" {
			if auto := DetectProjectVerificationCmd(targetDir); auto != "" {
				return auto
			}
		}
		return "go build ."
	}
	if strings.HasPrefix(norm, "npm start") || strings.HasPrefix(norm, "npm run dev") || strings.HasPrefix(norm, "npm run serve") ||
		strings.HasPrefix(norm, "yarn start") || strings.HasPrefix(norm, "yarn dev") || strings.HasPrefix(norm, "vite") || strings.HasPrefix(norm, "next dev") {
		if targetDir != "" {
			if auto := DetectProjectVerificationCmd(targetDir); auto != "" {
				return auto
			}
		}
		return "npm test"
	}
	if strings.HasPrefix(norm, "python -m http.server") || strings.HasPrefix(norm, "python3 -m http.server") || strings.HasPrefix(norm, "http-server") {
		if targetDir != "" {
			if _, err := os.Stat(filepath.Join(targetDir, "index.html")); err == nil {
				return "python -c \"import os; assert os.path.exists('index.html')\""
			}
		}
		return "echo Static server verified"
	}
	if strings.HasPrefix(norm, "uvicorn ") || strings.HasPrefix(norm, "gunicorn ") || strings.HasPrefix(norm, "fastapi dev") || strings.HasPrefix(norm, "flask run") {
		if targetDir != "" {
			if auto := DetectProjectVerificationCmd(targetDir); auto != "" {
				return auto
			}
		}
		return "python -m compileall -q ."
	}
	if strings.HasPrefix(norm, "python ") || strings.HasPrefix(norm, "python3 ") {
		parts := strings.Fields(cmd)
		if len(parts) >= 2 && strings.HasSuffix(parts[1], ".py") {
			return fmt.Sprintf("python -m py_compile %s", parts[1])
		}
		return "python -m unittest"
	}
	if strings.HasPrefix(norm, "cargo run") {
		return "cargo check"
	}
	return cmd
}

// ExecuteConversationalTask coordinates autonomous conversational LLM planning, multi-file code writing,
// scoped project verification, and automated turn-by-turn conversational loops.
func ExecuteConversationalTask(ctx context.Context, session *Session, prompt string, out io.Writer) error {
	if session != nil {
		defer session.Close()
	}
	return executeConversationalTurn(ctx, session, prompt, out)
}

// ExecuteTask triggers an autonomous engineering task via engineOrch.ExecuteTask,
// formats the execution report, and feeds the summary back into conversational context.
func ExecuteTask(ctx context.Context, session *Session, task string, out io.Writer) error {
	if session != nil {
		defer session.Close()
	}
	return executeTask(ctx, session, task, out)
}

// detectRootExerciseFiles checks if the workspace root contains existing code or test files
// (e.g. standalone benchmark exercises, coding challenges, single-package repositories)
// rather than being an empty root where a new subproject should be scaffolded.
func detectRootExerciseFiles(ws string) []string {
	entries, err := os.ReadDir(ws)
	if err != nil {
		return nil
	}
	var rootFiles []string
	codeExts := map[string]bool{
		".go": true, ".py": true, ".ts": true, ".js": true,
		".rs": true, ".c": true, ".cpp": true, ".java": true,
	}
	for _, e := range entries {
		if !e.IsDir() {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if codeExts[ext] {
				rootFiles = append(rootFiles, e.Name())
			}
		}
	}
	return rootFiles
}

// executeConversationalTurn coordinates autonomous conversational LLM planning, multi-file code writing,
// scoped project verification, and automated turn-by-turn conversational loops.
func executeConversationalTurn(ctx context.Context, session *Session, prompt string, out io.Writer) error {
	theme := NewTerminalTheme()
	if session == nil {
		return errors.New("ui: session cannot be nil")
	}

	ws := session.WorkspaceDir()
	if ws == "" {
		ws = "."
	}

	currentPrompt := prompt
	maxSteps := 25
	finalVerificationSent := false

	// Universal Target Project Extraction & Ground Truth Briefing
	rootExerciseFiles := detectRootExerciseFiles(ws)
	if targetProj := ExtractTargetProject(prompt, ws); targetProj != "" {
		session.SetActiveProject(targetProj)
		fmt.Fprintf(out, "%s %s\n\n",
			theme.Bold(theme.BrightCyan("[Target Project Discovered]")),
			theme.Cyan(fmt.Sprintf("Scoped session context to %s/", targetProj)))
		if briefing := BuildProjectGroundTruthBriefing(ws, targetProj); briefing != "" {
			currentPrompt = briefing + "\n\n" + currentPrompt
		}
	} else if activeProj := session.ActiveProject(); activeProj != "" && activeProj != "." {
		if briefing := BuildProjectGroundTruthBriefing(ws, activeProj); briefing != "" {
			currentPrompt = briefing + "\n\n" + currentPrompt
		}
	} else if len(rootExerciseFiles) > 0 {
		session.SetActiveProject(".")
		fmt.Fprintf(out, "%s %s\n\n",
			theme.Bold(theme.BrightCyan("[Root Exercise Detected]")),
			theme.Cyan(fmt.Sprintf("Existing starter/test files found at workspace root (%s). Enforcing in-place root completion.", strings.Join(rootExerciseFiles, ", "))))
		fileList := strings.Join(rootExerciseFiles, ", ")
		primaryFile := rootExerciseFiles[0]
		for _, f := range rootExerciseFiles {
			if !strings.Contains(f, "test") {
				primaryFile = f
				break
			}
		}
		directive := fmt.Sprintf("=== IN-PLACE ROOT EXERCISE DIRECTIVE ===\n"+
			"Existing starter and/or test files are already present directly in the workspace root directory:\n"+
			"Files: %s\n"+
			"CRITICAL INSTRUCTION: You MUST complete and edit these files IN-PLACE at the workspace root (e.g. [ACTION:PATCH path=\"%s\"] or [ACTION:WRITE path=\"%s\"]).\n"+
			"DO NOT create a nested 'projects/' or 'apps/' subfolder. The test runner executes directly against the workspace root.\n\n",
			fileList, primaryFile, primaryFile)
		currentPrompt = directive + currentPrompt
	}

	isErrorReport, reportedMissingTool, errSummary := DetectUserErrorReport(prompt)
	if isErrorReport {
		if reportedMissingTool != "" {
			session.RecordUnavailableTool(reportedMissingTool)
			session.RecordBuild(reportedMissingTool, 127, prompt)
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightYellow("[Missing Host Tool Detected]")),
				theme.Yellow(fmt.Sprintf("Registered unavailable host tool %q. OmniCoder will adapt to available host tools.", reportedMissingTool)))
		}

		directive := fmt.Sprintf("\n\n=== INCIDENT RESOLUTION DIRECTIVE ===\n"+
			"The user is reporting a blocking build, execution, or runtime failure:\n%s\n"+
			"MANDATORY REQUIREMENTS:\n"+
			"1. Do NOT reply with mere apologies, cheerleading commentary, or instructions telling the user to run uninstalled tools.\n"+
			"2. You MUST take immediate, concrete action: emit [ACTION:WRITE path=\"...\"], [ACTION:PATCH path=\"...\"], and/or [ACTION:BUILD task=\"...\"] tags.\n"+
			"3. If a tool (%s) is missing or unavailable on this host, do NOT call it or require it. Rewrite the build scripts, configuration, or code to use standard tools installed on the host.\n", errSummary, reportedMissingTool)

		// Missing-File Resolver: if the user prompt mentions a file currently quarantined, auto-restore it
		var restoredFiles []string
		for _, qp := range session.AllQuarantined() {
			base := filepath.Base(qp.Path)
			if strings.Contains(strings.ToLower(prompt), strings.ToLower(base)) {
				targetAbs := filepath.Join(ws, filepath.FromSlash(qp.Path))
				_ = os.MkdirAll(filepath.Dir(targetAbs), 0755)
				if err := os.WriteFile(targetAbs, []byte(qp.Content), 0644); err == nil {
					session.RemoveQuarantined(qp.Path)
					restoredFiles = append(restoredFiles, qp.Path)
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightGreen("[Quarantine Auto-Restore]")),
						theme.Green(fmt.Sprintf("Restored missing deliverable %s from quarantine to disk.", qp.Path)))
				}
			}
		}
		if len(restoredFiles) > 0 {
			directive += fmt.Sprintf("4. The engine has automatically restored %s from quarantine to disk. Run verification to confirm.\n", strings.Join(restoredFiles, ", "))
		}
		directive += "====================================="
		currentPrompt = currentPrompt + directive
	}
	var buildSess *BuildSession
	activePlanSpec := engine.DetectPlanSpec(prompt, ws)
	if activePlanSpec == nil {
		if spec, _ := session.ActivePlan(); spec != nil {
			// If session has an active project, ensure the spec matches it
			if session.ActiveProject() != "" && spec.ProjectScope != "" {
				activeProjBase := stripVariantSuffix(filepath.Base(session.ActiveProject()))
				specProjBase := stripVariantSuffix(filepath.Base(spec.ProjectScope))
				if strings.EqualFold(activeProjBase, specProjBase) {
					activePlanSpec = spec
				} else {
					session.SetActivePlan(nil, "")
				}
			} else {
				activePlanSpec = spec
			}
		}
	} else {
		session.SetActivePlan(activePlanSpec, "")
	}
	// If activePlanSpec targets a project different from session.ActiveProject(), clear it
	if activePlanSpec != nil && activePlanSpec.ProjectScope != "" && session.ActiveProject() != "" {
		activeProjBase := stripVariantSuffix(filepath.Base(session.ActiveProject()))
		specProjBase := stripVariantSuffix(filepath.Base(activePlanSpec.ProjectScope))
		if !strings.EqualFold(activeProjBase, specProjBase) {
			activePlanSpec = nil
			session.SetActivePlan(nil, "")
		}
	}
	if session.ICX() != nil {
		buildSess = NewBuildSession(session.ICX(), ws)
		// Isolate each project in its own ICX memory space (docs: Memory Spaces & Multi-Tenancy).
		project := session.ActiveProject()
		if (project == "" || project == ".") && activePlanSpec != nil {
			project = activePlanSpec.ProjectScope
		}
		buildSess.SpaceID = spaceIDForProject(project)
		_ = buildSess.Open(ctx, prompt, activePlanSpec)
		// Give the architect relevant remembered files on turn one instead of only from step two.
		if rc := buildSess.PromptRecall(ctx, prompt); rc != "" && !strings.Contains(currentPrompt, icxRecallHeader) {
			currentPrompt = currentPrompt + "\n\n" + rc
		}
		session.SetBuildSession(buildSess)
	}
	if activePlanSpec != nil {
		if activePhase := activePlanSpec.DetermineActivePhase(ws); activePhase != nil {
			complete, missing := activePlanSpec.IsPhaseFilesPresent(ws, activePhase)
			if !complete {
				if !strings.Contains(currentPrompt, "=== STRICT PHASE ISOLATION MANDATE ===") && !strings.Contains(currentPrompt, "=== PLAN PROGRESSION:") {
					currentPrompt = currentPrompt + "\n\n" + buildPhaseProgressionPrompt(ws, activePlanSpec, activePhase, complete, missing)
				}
			}
		}
	}
	consecutiveHeals := 0
	stagnantHeals := 0
	lastErrorCount := 0
	lastTestDiag := ""
	identicalDiagCount := 0
	overallSuccess := true
	turnSnapshots := make(map[string][]byte)
	lastGap := ""
	stalls := 0
	lastWrittenFiles := make(map[string]bool)
	lastVerifyDir := ""
	lastRunCmd := ""
	var lastExtraEnv []string
	var failedAttempts []TurnAttemptRecord
	mapTurnCount := 0
	readTurnCount := 0
	explicitBuildRan := false // a BUILD probe/command has run during this task (runtime evidence)
	probeFeedback := ""       // output of the latest passing BUILD, pending delivery to the architect
	probeFeedbackTurns := 0
	stalledIntentRetries := 0                      // replies that announced a step without an executable tag
	readPathsSeen := make(map[string]bool)         // files already returned to the architect this task
	rejectedEdits := make(map[string]rejectedEdit) // guard-rejected edits, keyed by target path
	rejectedEditPrompts := 0
	var evFlags evidenceFlags // kinds of fabricated/inflated data found in files the architect read
	symptomMismatch := false  // a read-only probe contradicts the count the user reported
	symptomMismatchMsg := ""
	dataDoneGatePrompts := 0
	var dataGateUnmet []string
	dataGateSatisfied := false // a final data-bug answer has passed the completion gate
	// appendEvidenceWarnings adds whole-file engine warnings (fabricated data, hidden errors) to a READ result.
	appendEvidenceWarnings := func(rb *strings.Builder, ra *ActionIntent) {
		w, fl := evidenceWarningsForRead(ws, session.ActiveProject(), ra)
		if w == "" {
			return
		}
		rb.WriteString(w)
		evFlags = evFlags.merge(fl)
		fmt.Fprintf(out, "%s %s\n", theme.Bold(theme.BrightRed("[Evidence Warning]")), theme.Yellow(strings.TrimSpace(w)))
	}
	unplannedCompletionPasses := 0

	isHostTask := session.IsHostTask() || engine.IsHostPackageTask(prompt)
	session.SetHostTask(isHostTask)

	defer func() {
		if !overallSuccess && len(turnSnapshots) > 0 {
			planSpec := activePlanSpec
			if planSpec == nil {
				planSpec = engine.DetectPlanSpec(prompt, ws)
			}
			if planSpec == nil {
				planSpec = engine.DetectPlanSpec(currentPrompt, ws)
			}
			// For structured multi-phase plans (planSpec != nil), preserve newly created files
			// on disk so subsequent turns and self-healing can iteratively converge without
			// losing generated phase code. Only rollback if planSpec == nil.
			if planSpec != nil {
				return
			}
			// For brand new project scaffolds (all touched files are new under projects/ or apps/),
			// preserve them so the user/agent can iteratively converge without nuking the scaffold.
			allNewScaffold := true
			hasScaffoldFiles := false
			for relPath, snapshot := range turnSnapshots {
				cleanRP := filepath.ToSlash(relPath)
				if strings.HasPrefix(cleanRP, "projects/") || strings.HasPrefix(cleanRP, "apps/") {
					hasScaffoldFiles = true
					if snapshot != nil {
						allNewScaffold = false
						break
					}
				} else {
					allNewScaffold = false
					break
				}
			}
			if hasScaffoldFiles && allNewScaffold {
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.Yellow("[Scaffold Preserved]")),
					theme.Yellow("Preserved newly scaffolded project files on disk for inspection and iterative healing."))
				return
			}
			reverted := 0
			for relPath, snapshot := range turnSnapshots {
				fullPath := filepath.Join(ws, filepath.FromSlash(relPath))
				if snapshot == nil {
					if err := os.Remove(fullPath); err == nil {
						reverted++
						if bs := session.BuildSession(); bs != nil {
							_ = bs.PurgeSource(context.Background(), "project:"+filepath.ToSlash(relPath))
						}
					}
				} else {
					if err := os.WriteFile(fullPath, snapshot, 0644); err == nil {
						reverted++
						if bs := session.BuildSession(); bs != nil {
							_, _ = bs.CrystallizeFile(context.Background(), filepath.ToSlash(relPath), string(snapshot), "")
						}
					}
				}
			}
			if reverted > 0 {
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightMagenta("[Rollback Guard]")),
					theme.Magenta(fmt.Sprintf("Restored %d file(s) to clean pre-turn snapshot after failed healing loop.", reverted)))
			}
		}
	}()

	for step := 1; step <= maxSteps; step++ {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, theme.Yellow("\nTurn cancelled by context."))
			return ctx.Err()
		default:
		}

		if buildSess != nil && step > 1 {
			card, refused := buildSess.Card(ctx, "", "", 0)
			if refused {
				fmt.Fprintf(out, "%s %s\n\n", theme.Bold(theme.BrightRed("[Register Miss]")), theme.Red(card))
				break
			}
			if card != "" && !strings.Contains(currentPrompt, card) {
				currentPrompt = currentPrompt + "\n\n" + card
			}
		}

		fmt.Fprintln(out, theme.Dim("Architect thinking..."))
		reply, actions, err := session.SendMessageAll(ctx, currentPrompt)
		if err != nil {
			fmt.Fprintf(out, "%s %v\n\n", theme.Red("[Error]"), err)
			return err
		}

		fmt.Fprintf(out, "\n%s\n%s\n\n", theme.Bold(theme.Green("OmniCoder:")), reply)

		// Anti-Cheerleading Accountability Guard:
		// If user reported an error or incident and the model provided conversational prose with ZERO action tags,
		// re-prompt the model to force concrete repair action tags instead of leaving the user stranded.
		if isErrorReport && len(actions) == 0 && !isDoneResponse(reply) && step == 1 {
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightYellow("[Accountability Guard]")),
				theme.Yellow("No executable actions were emitted in response to the reported error. Demanding concrete repair actions from architect..."))
			retryPrompt := fmt.Sprintf("CRITICAL ACCOUNTABILITY DIRECTIVE: Your previous response provided explanation without any concrete action tags.\n"+
				"The reported error is: %s.\n"+
				"You MUST emit concrete [ACTION:WRITE], [ACTION:PATCH], and/or [ACTION:BUILD] tags NOW to fix the files and build configuration directly. Do not ask the user to fix it.", errSummary)
			if reportedMissingTool != "" {
				retryPrompt += fmt.Sprintf(" Tool %q is NOT installed on this host; switch to native or available tools.", reportedMissingTool)
			}
			reply2, actions2, err2 := session.SendMessageAll(ctx, retryPrompt)
			if err2 == nil && len(actions2) > 0 {
				reply = reply2
				actions = actions2
				fmt.Fprintf(out, "\n%s\n%s\n\n", theme.Bold(theme.Green("OmniCoder (Auto-Repair):")), reply)
			}
		}

		var currentActionsSummary []string
		for _, a := range actions {
			switch a.Type {
			case ActionWrite:
				currentActionsSummary = append(currentActionsSummary, fmt.Sprintf("WRITE %s", a.Path()))
			case ActionPatch:
				currentActionsSummary = append(currentActionsSummary, fmt.Sprintf("PATCH %s", a.Path()))
			case ActionBuild:
				currentActionsSummary = append(currentActionsSummary, fmt.Sprintf("BUILD %s", a.Task()))
			}
		}

		// Completion gate: If final verification prompt was sent, only break when LLM says DONE with zero ACTION tags.
		if finalVerificationSent && len(actions) == 0 && isDoneResponse(reply) {
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightGreen("[Task Complete]")),
				theme.Green("Architect verified complete with zero pending actions."))
			break
		}

		// 1. Filter and deduplicate file modification actions (writes, creates, surgical patches, and edits).
		// If multiple actions target the same file in a single turn, keep the last one (model self-correction/draft progression),
		// EXCEPT consecutive PATCH/EDIT hunks: those are independent edits to different parts of the file and must all apply.
		var fileActions []*ActionIntent
		seenFileAction := make(map[string]int)
		for _, action := range actions {
			if action.IsFileModification() {
				normPath := strings.ToLower(filepath.ToSlash(filepath.Clean(strings.Trim(strings.TrimSpace(action.Path()), "\"'`"))))
				if idx, exists := seenFileAction[normPath]; exists && !(action.IsPatch() && fileActions[idx].IsPatch()) {
					fileActions[idx] = action
				} else {
					seenFileAction[normPath] = len(fileActions)
					fileActions = append(fileActions, action)
				}
			}
		}

		// Anti-Verification-Theater Gate: If user reported an error and assistant only ran tests without modifying files
		if isErrorReport && len(fileActions) == 0 && len(actions) > 0 {
			allBuilds := true
			for _, a := range actions {
				if a.Type != ActionBuild {
					allBuilds = false
					break
				}
			}
			if allBuilds {
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightYellow("[Verification Theater Blocked]")),
					theme.Yellow("Assistant repeated build commands without modifying files after user reported an error. Forcing remediation."))
				currentPrompt = fmt.Sprintf("REMEDIATION DIRECTIVE:\nYou ran build/test commands without writing or repairing any files, even though the user reported an error:\n%s\n\nYou MUST emit [ACTION:WRITE path=\"...\"] or [ACTION:PATCH path=\"...\"] to fix the underlying error or create missing files before verifying.", errSummary)
				isErrorReport = false
				continue
			}
		}

		// 2. Project Harmonizer & Scoping
		var remappedPaths map[*ActionIntent]string
		var canonicalDir string
		if len(fileActions) > 0 {
			readTurnCount = 0
			canonicalDir, remappedPaths = HarmonizeProjectActions(session, fileActions)
			if canonicalDir != "" {
				remappedAny := false
				for wa, target := range remappedPaths {
					orig := strings.Trim(strings.TrimSpace(wa.Path()), "\"'`")
					if target != orig && target != filepath.ToSlash(filepath.Clean(orig)) {
						remappedAny = true
						break
					}
				}
				if remappedAny {
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightMagenta("[Project Harmonizer]")),
						theme.Magenta(fmt.Sprintf("Coalescing variant project paths into canonical root: %s/", canonicalDir)))
				}
			}
		}

		if activePlanSpec == nil {
			if spec, _ := session.ActivePlan(); spec != nil {
				if canonicalDir == "" || spec.ProjectScope == "" || strings.EqualFold(stripVariantSuffix(filepath.Base(canonicalDir)), stripVariantSuffix(filepath.Base(spec.ProjectScope))) {
					activePlanSpec = spec
				}
			} else if spec := engine.DetectPlanSpec(currentPrompt, ws); spec != nil {
				if canonicalDir == "" || spec.ProjectScope == "" || strings.EqualFold(stripVariantSuffix(filepath.Base(canonicalDir)), stripVariantSuffix(filepath.Base(spec.ProjectScope))) {
					activePlanSpec = spec
				}
			} else if canonicalDir != "" {
				if spec := engine.DetectPlanSpec(canonicalDir, ws); spec != nil {
					activePlanSpec = spec
				}
			}
			if activePlanSpec != nil {
				session.SetActivePlan(activePlanSpec, "")
			}
		} else if canonicalDir != "" && activePlanSpec.ProjectScope != "" {
			canonBase := stripVariantSuffix(filepath.Base(canonicalDir))
			planBase := stripVariantSuffix(filepath.Base(activePlanSpec.ProjectScope))
			if !strings.EqualFold(canonBase, planBase) {
				activePlanSpec = nil
				session.SetActivePlan(nil, "")
			}
		}

		scopedDir := ""
		if canonicalDir != "" && canonicalDir != "." {
			scopedDir = canonicalDir
		} else if activePlanSpec != nil && activePlanSpec.ProjectScope != "" && activePlanSpec.ProjectScope != "." {
			scopedDir = activePlanSpec.ProjectScope
		}
		if len(fileActions) > 1 && (session == nil || session.ActiveProject() != ".") && len(rootExerciseFiles) == 0 {
			for _, wa := range fileActions {
				p := strings.TrimSpace(wa.Path())
				p = strings.Trim(p, "\"'`")
				cleanP := filepath.ToSlash(filepath.Clean(p))
				cleanP = strings.TrimPrefix(cleanP, "./")
				cleanP = strings.TrimPrefix(cleanP, "/")
				cleanPTrimmed := strings.TrimRight(cleanP, ". ")
				if strings.EqualFold(cleanPTrimmed, "go.mod") || isWorkspaceRootFile(ws, p, "go.mod") {
					modContent := wa.ExtractContent(reply)
					foreignMod := ExtractModuleName([]byte(modContent))
					hostModPath := filepath.Join(ws, "go.mod")
					isForeign := false
					if hostData, err := os.ReadFile(hostModPath); err == nil {
						hostMod := ExtractModuleName(hostData)
						if hostMod != foreignMod {
							isForeign = true
						}
					} else {
						isForeign = true
					}

					if isForeign {
						projName := SanitizeProjectName(foreignMod)
						if projName == "app" {
							for _, otherAction := range fileActions {
								otherP := strings.TrimSpace(otherAction.Path())
								otherP = strings.Trim(otherP, "\"'`")
								if filepath.IsAbs(otherP) {
									if rel, err := filepath.Rel(ws, otherP); err == nil && !strings.HasPrefix(rel, "..") {
										otherP = rel
									}
								}
								cleanOtherP := filepath.ToSlash(filepath.Clean(otherP))
								cleanOtherP = strings.TrimPrefix(cleanOtherP, "./")
								cleanOtherP = strings.TrimPrefix(cleanOtherP, "/")
								for _, prefix := range []string{"apps/", "projects/", "output/"} {
									if strings.HasPrefix(cleanOtherP, prefix) {
										sub := strings.TrimPrefix(cleanOtherP, prefix)
										parts := strings.Split(sub, "/")
										if len(parts) > 0 && parts[0] != "" {
											if candidate := SanitizeProjectName(parts[0]); candidate != "app" {
												projName = candidate
												break
											}
										}
									}
								}
								if projName != "app" {
									break
								}
							}
						}
						if scopedDir == "" || !strings.Contains(scopedDir, projName) {
							scopedDir = fmt.Sprintf("apps/%s", projName)
							fmt.Fprintf(out, "%s %s\n",
								theme.Bold(theme.BrightYellow("[Auto-Scope Applied]")),
								theme.Yellow(fmt.Sprintf("Scoped new application to: %s/", scopedDir)))
						}
						break
					}
				}
			}
		}

		// 3. Write or Patch files to disk with Duplicate Extraction, Code Preservation, Splicing, and Placeholder guards
		testPassed := true
		skipCommit := false
		var testDiagSummary string
		var lastTargetDir string
		writtenPayloadsInTurn := make(map[string]string)
		writtenFilesInTurn := make(map[string]bool)
		for _, action := range fileActions {
			targetPath := strings.TrimSpace(action.Path())
			targetPath = strings.Trim(targetPath, "\"'`")
			if remappedPaths != nil {
				if rPath, ok := remappedPaths[action]; ok && rPath != "" {
					targetPath = rPath
				}
			}
			if targetPath == "" {
				continue
			}
			isExternalAbs := false
			if filepath.IsAbs(targetPath) {
				if rel, err := filepath.Rel(ws, targetPath); err == nil && !strings.HasPrefix(rel, "..") {
					targetPath = rel
				} else {
					isExternalAbs = true
				}
			}
			if scopedDir != "" && !isExternalAbs {
				targetPath = ScopePath(scopedDir, targetPath)
			}

			// In-Place Root Exercise Remap:
			// If workspace contains root exercise files (or session is active in "."),
			// and targetPath targets a nested projects/<subfolder>/<filename> or apps/<subfolder>/<filename>,
			// check if <filename> exists directly at the root of ws. If so, remap to the root file in-place.
			if (session != nil && session.ActiveProject() == ".") || len(rootExerciseFiles) > 0 {
				cleanTarget := filepath.ToSlash(filepath.Clean(targetPath))
				for _, prefix := range []string{"projects/", "apps/", "output/"} {
					if strings.HasPrefix(cleanTarget, prefix) {
						parts := strings.Split(strings.TrimPrefix(cleanTarget, prefix), "/")
						if len(parts) >= 2 {
							baseFile := parts[len(parts)-1]
							if _, statErr := os.Stat(filepath.Join(ws, baseFile)); statErr == nil {
								targetPath = baseFile
								fmt.Fprintf(out, "%s %s\n",
									theme.Bold(theme.BrightYellow("[In-Place Root Remap]")),
									theme.Yellow(fmt.Sprintf("Remapped %s -> %s (modifying workspace root exercise in-place)", cleanTarget, targetPath)))
								break
							}
						}
					}
				}
			}
			if targetPath == "" {
				continue
			}

			// Host Package Shield Guard:
			// If targetPath points into OmniCoder's own core packages (pkg/*, cmd/*) and
			// the overarching task is not explicitly an OmniCoder host package task, protect host packages.
			if engine.IsHostPackagePath(targetPath) && !isHostTask {
				if scopedDir != "" {
					targetPath = ScopePath(scopedDir, targetPath)
				} else if activePlanSpec != nil && activePlanSpec.ProjectScope != "" {
					targetPath = ScopePath(activePlanSpec.ProjectScope, targetPath)
				}
				if engine.IsHostPackagePath(targetPath) {
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightRed("[Host Package Shield]")),
						theme.Yellow(fmt.Sprintf("Shielded host engine package %q from mutation during non-host task", targetPath)))
					testPassed = false
					overallSuccess = false
					testDiagSummary = fmt.Sprintf("Action on %q was rejected by Host Package Shield: host engine packages (pkg/*, cmd/*) are protected from mutation during application tasks. Place all application code in projects/<name>/ or apps/<name>/.", targetPath)
					continue
				}
			}

			// Pre-Turn Snapshot Guard: capture pre-modification bytes for clean rollback on failure
			fullCheckPath := filepath.Join(ws, filepath.FromSlash(targetPath))
			if _, snapshotted := turnSnapshots[targetPath]; !snapshotted {
				if existingData, readErr := os.ReadFile(fullCheckPath); readErr == nil {
					turnSnapshots[targetPath] = existingData
				} else {
					turnSnapshots[targetPath] = nil // New file created in turn
				}
			}

			content := action.ExtractContent(reply)
			trimmedContent := strings.TrimSpace(content)

			// Empty Content Guard:
			// If extracted content is empty, refuse to process and feed diagnostic to self-healing loop.
			if trimmedContent == "" {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.Yellow("[Empty Content Guard]")),
					theme.Yellow(fmt.Sprintf("Skipping empty payload for %s", targetPath)))
				testPassed = false
				overallSuccess = false
				testDiagSummary = fmt.Sprintf("Action on %q was rejected because the extracted file payload was empty. Provide the complete file content within standard markdown code fences (```) or immediately following the [ACTION:WRITE path=%q] tag.", targetPath, targetPath)
				continue
			}

			// Malformed Action Tag Guard:
			// If content starts with an action tag or system log tag, reject and prompt LLM to fix.
			if strings.HasPrefix(trimmedContent, "[ACTION:") ||
				(strings.HasPrefix(trimmedContent, "[") && func() bool {
					loc := actionStartRegex.FindStringIndex(trimmedContent)
					return loc != nil && loc[0] == 0
				}()) ||
				strings.HasPrefix(trimmedContent, "[File Created]") ||
				strings.HasPrefix(trimmedContent, "[Build]") ||
				strings.HasPrefix(trimmedContent, "[Auto-Write") ||
				strings.HasPrefix(trimmedContent, "[Auto-Engine") ||
				strings.HasPrefix(trimmedContent, "[Auto-Next") ||
				strings.HasPrefix(trimmedContent, "[Verification") {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightRed("[Malformed Tag Guard]")),
					theme.Yellow(fmt.Sprintf("Rejected action on %s: extracted payload contains raw action/system tag instead of code.", targetPath)))
				testPassed = false
				testDiagSummary = fmt.Sprintf("Action on %q was rejected because the extracted payload contained raw action tags instead of valid source code.", targetPath)
				continue
			}

			// Fabricated Data Guard:
			// Reject edits that invent records or hardcode harvest counts instead of fixing the real pipeline.
			if fabricated, why := detectFabricatedData(targetPath, content); fabricated {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightRed("[Fabricated Data Guard]")),
					theme.Yellow(fmt.Sprintf("Rejected edit to %s: appears to invent or hardcode data (%q).", targetPath, why)))
				testPassed = false
				overallSuccess = false
				testDiagSummary = fmt.Sprintf("Edit to %q was rejected: it appears to fabricate records or hardcode counts (%q). Never invent data to make numbers match. Find the real root cause (compare the actual schema/row counts with the code that writes them), then fix the pipeline so real data flows, e.g. migrate the schema or repair the failing insert.", targetPath, why)
				rejectedEdits[targetPath] = rejectedEdit{Reason: "fabricated data: " + why, Hash: fileHash(ws, targetPath)}
				continue
			}

			// Unsafe Patch Guard: swallowed exceptions and triggering bulk seeders hide real failures.
			if unsafe, why := detectUnsafePatch(targetPath, content); unsafe {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightRed("[Unsafe Patch Guard]")),
					theme.Yellow(fmt.Sprintf("Rejected edit to %s: %s", targetPath, why)))
				testPassed = false
				overallSuccess = false
				testDiagSummary = fmt.Sprintf("Edit to %q was rejected: %s Fix the real failure instead.", targetPath, why)
				rejectedEdits[targetPath] = rejectedEdit{Reason: why, Hash: fileHash(ws, targetPath)}
				continue
			}

			// Runtime Probe Gate: data-bug fixes to data-layer files require real runtime evidence first.
			if !explicitBuildRan && needsRuntimeProbe(prompt, targetPath) {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightRed("[Runtime Probe Gate]")),
					theme.Yellow(fmt.Sprintf("Deferred edit to %s: no runtime evidence gathered yet.", targetPath)))
				testPassed = false
				overallSuccess = false
				testDiagSummary = fmt.Sprintf("Edit to %q was deferred: this is a data bug and you have not inspected the real runtime state. First emit a read-only [ACTION:BUILD task=\"python -c ...\"] that prints the actual table schema (PRAGMA table_info), row counts, and any ledger/counter rows, and capture the real error from the failing insert or scraper. State the root cause from that output, then re-issue the edit.", targetPath)
				continue
			}

			// Surgical Patch Detection:
			// If action is explicitly PATCH/EDIT or content contains Aider search/replace blocks,
			// execute non-destructive surgical patch.
			hasAiderMarkers := strings.Contains(content, "<<<<<<< SEARCH") && strings.Contains(content, "=======") && strings.Contains(content, ">>>>>>> REPLACE")
			if !action.IsPatch() && hasAiderMarkers {
				emptyAider := regexp.MustCompile(`(?s)<<<<<<<\s*SEARCH\s*=======\s*>>>>>>>\s*REPLACE`)
				content = strings.TrimSpace(emptyAider.ReplaceAllString(content, ""))
				trimmedContent = strings.TrimSpace(content)
				hasAiderMarkers = strings.Contains(content, "<<<<<<< SEARCH") && strings.Contains(content, "=======") && strings.Contains(content, ">>>>>>> REPLACE")
			}
			isAiderPatch := action.IsPatch() || hasAiderMarkers
			if isAiderPatch {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightMagenta("[Surgical Patch Detected]")),
					theme.Magenta(fmt.Sprintf("Target File: %s", targetPath)))
				fullPath := filepath.Join(ws, filepath.FromSlash(targetPath))
				prePatchBytes, _ := os.ReadFile(fullPath)
				if err := executePatch(session, targetPath, content, out); err != nil {
					testPassed = false
					overallSuccess = false
					testDiagSummary = fmt.Sprintf("Surgical patch on %q failed: %v", targetPath, err)
				} else {
					if data, err := os.ReadFile(fullPath); err == nil {
						normData := devops.NormalizePolyglotSource(targetPath, string(data))
						if normData != string(data) {
							_ = os.WriteFile(fullPath, []byte(normData), 0644)
							data = []byte(normData)
						}
						if preErr := devops.ValidatePolyglotPreflight(ws, targetPath, string(data)); preErr != nil {
							if repaired, didRepair, repErr := devops.AutoRepairPreflightSyntax(ws, targetPath, string(data)); didRepair && repErr == nil {
								_ = os.WriteFile(fullPath, []byte(repaired), 0644)
								writtenFilesInTurn[targetPath] = true
								fmt.Fprintf(out, "%s %s\n",
									theme.Bold(theme.BrightGreen("[Pre-flight Auto-Trim Repaired]")),
									theme.Green(fmt.Sprintf("Auto-repaired boundary syntax slip in patched %s.", targetPath)))
							} else {
								guardName := "[Polyglot Pre-flight Syntax Guard]"
								if strings.HasSuffix(strings.ToLower(targetPath), ".go") {
									guardName = "[Go Pre-flight Syntax Guard]"
								}
								fmt.Fprintf(out, "%s %s\n",
									theme.Bold(theme.BrightRed(guardName)),
									theme.Yellow(fmt.Sprintf("Patched file %s has invalid syntax: %v; reverting to pre-patch state.", targetPath, preErr)))
								if len(prePatchBytes) > 0 {
									_ = os.WriteFile(fullPath, prePatchBytes, 0644)
								} else if prior, ok := turnSnapshots[targetPath]; ok && prior != nil {
									_ = os.WriteFile(fullPath, prior, 0644)
								}
								testPassed = false
								overallSuccess = false
								testDiagSummary = fmt.Sprintf("Surgical patch on %q produced invalid syntax: %v. The file was reverted to its pre-patch state. Please provide an exact search/replace block matching existing lines or write the full file.", targetPath, preErr)
							}
						} else {
							writtenFilesInTurn[targetPath] = true
						}
					} else {
						writtenFilesInTurn[targetPath] = true
					}
				}
				cleanTP := filepath.ToSlash(targetPath)
				parts := strings.Split(cleanTP, "/")
				if len(parts) >= 2 && (parts[0] == "projects" || parts[0] == "apps") {
					lastTargetDir = filepath.Join(ws, parts[0], parts[1])
				}
				continue
			}

			// Placeholder Tag Detection Guard:
			// If content contains literal placeholder tags or truncation comments, refuse write and prompt LLM to fix.
			if hasPlaceholder, tag := detectPlaceholderTags(content); hasPlaceholder {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightRed("[Placeholder Guard]")),
					theme.Yellow(fmt.Sprintf("Rejected write to %s: detected placeholder tag %q. Full implementation required.", targetPath, tag)))
				testPassed = false
				testDiagSummary = fmt.Sprintf("File write for %q was rejected because it contains unresolved placeholder tag %q. You must write the complete, functional code without any placeholder tags or truncation comments.", targetPath, tag)
				continue
			}

			// Implementation Depth & Anti-Stub Guard:
			if isTooShallow, reason := checkImplementationDepth(targetPath, trimmedContent, activePlanSpec); isTooShallow {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightRed("[Implementation Depth Guard]")),
					theme.Yellow(fmt.Sprintf("Rejected shallow write to %s: %s", targetPath, reason)))
				testPassed = false
				overallSuccess = false
				testDiagSummary = fmt.Sprintf("File write for %q was rejected because it is an incomplete/shallow stub (%s). Implement the full production-grade logic with complete algorithms, structs, error handling, and zero stubs as required by the master plan.", targetPath, reason)
				continue
			}

			// Duplicate Turn Extraction Collision Guard:
			// If two distinct target files in the same turn have the exact same non-empty content (>100 bytes),
			// this indicates an extraction collision or hallucinated duplicate. Protect existing files from clobbering.
			contentSha := canonical.SHA256Bytes([]byte(content))
			if priorPath, exists := writtenPayloadsInTurn[contentSha]; exists && priorPath != targetPath && len(content) > 100 {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.Red("[Extraction Collision Guard]")),
					theme.Yellow(fmt.Sprintf("Skipping duplicate payload for %s (identical to %s in same turn)", targetPath, priorPath)))
				continue
			}

			// Non-Destructive Code Preservation & Function Splicing Guard:
			fullCheckPath = filepath.Join(ws, filepath.FromSlash(targetPath))
			if fi, err := os.Stat(fullCheckPath); err == nil && !fi.IsDir() && fi.Size() > 200 {
				existingData, readErr := os.ReadFile(fullCheckPath)
				if readErr == nil && len(existingData) > 200 {
					existingStr := string(existingData)

					// Corrupted Existing File Override:
					// If the existing file on disk fails syntax check or contains markdown prose (e.g. broken requirements.txt),
					// never protect the broken file from being replaced by incoming valid content.
					existingIsCorrupted := false
					if strings.EqualFold(filepath.Base(targetPath), "requirements.txt") {
						if _, err := devops.ValidateRequirementsTxt(existingStr); err != nil {
							existingIsCorrupted = true
						}
					} else if preErr := devops.ValidatePolyglotPreflight(ws, targetPath, existingStr); preErr != nil {
						existingIsCorrupted = true
					}

					if !existingIsCorrupted {
						// Guard A: Tiny stub (<80 bytes) against substantive file
						isStub := len(trimmedContent) < 80
						if isStub && strings.HasSuffix(strings.ToLower(targetPath), ".json") && json.Valid([]byte(trimmedContent)) && len(trimmedContent) > 2 {
							isStub = false
						}
						if isStub && strings.HasSuffix(strings.ToLower(targetPath), ".html") && strings.Contains(strings.ToLower(trimmedContent), "</html>") {
							isStub = false
						}
						if isStub && strings.HasSuffix(strings.ToLower(targetPath), ".py") && devops.ValidatePythonSource(trimmedContent) == nil && len(trimmedContent) > 30 {
							isStub = false
						}
						if isStub && strings.EqualFold(filepath.Base(targetPath), "requirements.txt") {
							if validReq, _ := devops.ValidateRequirementsTxt(trimmedContent); validReq && len(trimmedContent) > 5 {
								isStub = false
							}
						}
						if isStub {
							fmt.Fprintf(out, "%s %s\n",
								theme.Bold(theme.Yellow("[Code Preservation Guard]")),
								theme.Yellow(fmt.Sprintf("Preserved existing %s (%d bytes); rejected incoming stub replacement (%d bytes)", targetPath, fi.Size(), len(content))))
							continue
						}

						// Guard B: Go file function splicing (auto-splice single function updates)
						if strings.HasSuffix(strings.ToLower(targetPath), ".go") && isSingleFunctionUpdate(existingStr, content) {
							mergedCode, mergedOk := tryMergeGoFunction(existingStr, content)
							if mergedOk {
								mergedCode = devops.NormalizeGoSource(mergedCode)
								// Pre-flight check merged code
								if preErr := devops.ValidateGoPreflight(ws, targetPath, mergedCode); preErr != nil {
									fmt.Fprintf(out, "%s %s\n",
										theme.Bold(theme.BrightRed("[Go Pre-flight Syntax Guard]")),
										theme.Yellow(fmt.Sprintf("Merged function in %s failed syntax check: %v", targetPath, preErr)))
									testPassed = false
									overallSuccess = false
									testDiagSummary = fmt.Sprintf("Pre-flight Go syntax check failed on spliced %q: %v", targetPath, preErr)
									continue
								}
								fmt.Fprintf(out, "%s %s\n",
									theme.Bold(theme.BrightGreen("[Go Function Splicer]")),
									theme.Green(fmt.Sprintf("Non-destructively spliced updated function(s) into %s; preserved remaining implementation.", targetPath)))
								_ = executeWrite(session, targetPath, mergedCode, out)
								writtenPayloadsInTurn[canonical.SHA256Bytes([]byte(mergedCode))] = targetPath
								writtenFilesInTurn[targetPath] = true
								cleanTP := filepath.ToSlash(targetPath)
								parts := strings.Split(cleanTP, "/")
								if len(parts) >= 2 && (parts[0] == "projects" || parts[0] == "apps") {
									lastTargetDir = filepath.Join(ws, parts[0], parts[1])
								}
								continue
							}
						}

						// Guard C: File shrinkage (>40% drop) and missing functions/types check
						forceWrite := action.Attributes != nil && strings.EqualFold(action.Attributes["force"], "true")
						if !forceWrite && fi.Size() > 300 && float64(len(content)) < float64(fi.Size())*0.6 {
							isGo := strings.HasSuffix(strings.ToLower(targetPath), ".go")
							missingFuncs := []string{}
							missingTypes := []string{}
							if isGo {
								missingFuncs = detectMissingGoFunctions(existingStr, content)
								missingTypes = detectMissingGoTypes(existingStr, content)
							}

							// If missing functions or types detected in Go file, attempt Go function splicing fallback
							// unless the missing symbols were legitimately refactored into companion package files
							if isGo && (len(missingFuncs) > 0 || len(missingTypes) > 0) {
								if !areMissingSymbolsDeclaredInPackage(ws, targetPath, missingFuncs, missingTypes) {
									if mergedCode, mergedOk := tryMergeGoFunction(existingStr, content); mergedOk {
										mergedCode = devops.NormalizeGoSource(mergedCode)
										if preErr := devops.ValidateGoPreflight(ws, targetPath, mergedCode); preErr == nil {
											fmt.Fprintf(out, "%s %s\n",
												theme.Bold(theme.BrightGreen("[Go Function Splicer]")),
												theme.Green(fmt.Sprintf("Non-destructively spliced incoming partial write into %s; preserved remaining %d functions, %d types.", targetPath, len(missingFuncs), len(missingTypes))))
											_ = executeWrite(session, targetPath, mergedCode, out)
											writtenPayloadsInTurn[canonical.SHA256Bytes([]byte(mergedCode))] = targetPath
											writtenFilesInTurn[targetPath] = true
											cleanTP := filepath.ToSlash(targetPath)
											parts := strings.Split(cleanTP, "/")
											if len(parts) >= 2 && (parts[0] == "projects" || parts[0] == "apps") {
												lastTargetDir = filepath.Join(ws, parts[0], parts[1])
											}
											continue
										}
									}
								}
							}

							// Check if incoming content is a valid complete rewrite
							isCleanRewrite := false
							if isGo && (len(missingFuncs) == 0 && len(missingTypes) == 0 || areMissingSymbolsDeclaredInPackage(ws, targetPath, missingFuncs, missingTypes)) && devops.ValidateGoSource(content) == nil {
								isCleanRewrite = true
							} else if !isGo {
								ext := strings.ToLower(filepath.Ext(targetPath))
								trimmed := strings.TrimSpace(content)
								switch ext {
								case ".json":
									isCleanRewrite = json.Valid([]byte(trimmed)) && len(trimmed) > 2
								case ".html", ".htm":
									lower := strings.ToLower(trimmed)
									isCleanRewrite = strings.Contains(lower, "</html>") && (strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<html"))
								case ".css":
									openB := strings.Count(trimmed, "{")
									closeB := strings.Count(trimmed, "}")
									isCleanRewrite = openB > 0 && openB == closeB
								case ".py":
									isCleanRewrite = devops.ValidatePythonSource(content) == nil && len(trimmed) > 30
								case ".md", ".txt", ".yaml", ".yml", ".toml":
									if strings.EqualFold(filepath.Base(targetPath), "requirements.txt") {
										if validReq, _ := devops.ValidateRequirementsTxt(trimmed); validReq && len(trimmed) > 5 {
											isCleanRewrite = true
										}
									} else {
										hasPlaceholders := strings.Contains(content, "...") || strings.Contains(content, "TODO") || strings.Contains(content, "REST OF CODE")
										isCleanRewrite = !hasPlaceholders && len(trimmed) > 80
									}
								}
							}

							if isCleanRewrite {
								// All existing functions/types preserved or complete valid document; allow rewrite
							} else {
								var reason string
								if isGo && (len(missingFuncs) > 0 || len(missingTypes) > 0) {
									var missingItems []string
									if len(missingFuncs) > 0 {
										missingItems = append(missingItems, "functions: "+strings.Join(missingFuncs, ", "))
									}
									if len(missingTypes) > 0 {
										missingItems = append(missingItems, "types: "+strings.Join(missingTypes, ", "))
									}
									reason = fmt.Sprintf("rejected incoming truncated write (%d -> %d bytes, -%d%%); would delete %s",
										fi.Size(), len(content), int((1.0-float64(len(content))/float64(fi.Size()))*100), strings.Join(missingItems, "; "))
								} else {
									reason = fmt.Sprintf("rejected incoming truncated write (%d -> %d bytes, -%d%% drop); file shrinkage exceeds 40%% threshold",
										fi.Size(), len(content), int((1.0-float64(len(content))/float64(fi.Size()))*100))
								}

								fmt.Fprintf(out, "%s %s\n",
									theme.Bold(theme.BrightRed("[Code Preservation Guard]")),
									theme.Yellow(fmt.Sprintf("Preserved existing %s (%d bytes); %s", targetPath, fi.Size(), reason)))
								testPassed = false
								testDiagSummary = fmt.Sprintf("File write for %q was rejected by Code Preservation Guard: %s. Use [ACTION:PATCH path=%q] with search/replace blocks to modify specific lines, or provide the complete file content.", targetPath, reason, targetPath)
								continue
							}
						}
					}
				}
			}

			// Go Entrypoint Auto-Relocation Guard:
			// If the architect writes "main.go" with "package main" inside a directory whose existing
			// non-test files belong to a library package (e.g. package titankv), writing "main.go" there
			// will cause a Go compiler package declaration mismatch error.
			// Automatically relocate the entry point to "cmd/<pkg>/main.go" to preserve both the library
			// and the runnable executable.
			if strings.HasSuffix(strings.ToLower(targetPath), ".go") && filepath.Base(targetPath) == "main.go" {
				if incomingPkg, pkgErr := devops.GetGoPackageName(content); pkgErr == nil && incomingPkg == "main" {
					fullTargetDir := filepath.Dir(filepath.Join(ws, filepath.FromSlash(targetPath)))
					if entries, err := os.ReadDir(fullTargetDir); err == nil {
						var existingLibPkg string
						for _, e := range entries {
							if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") && !strings.EqualFold(e.Name(), "main.go") {
								if d, rErr := os.ReadFile(filepath.Join(fullTargetDir, e.Name())); rErr == nil {
									if ep, epErr := devops.GetGoPackageName(string(d)); epErr == nil && ep != "main" && ep != "" {
										existingLibPkg = ep
										break
									}
								}
							}
						}
						if existingLibPkg != "" {
							origTarget := targetPath
							targetDirRel := filepath.Dir(targetPath)
							targetPath = filepath.ToSlash(filepath.Join(targetDirRel, "cmd", existingLibPkg, "main.go"))
							fmt.Fprintf(out, "%s %s\n",
								theme.Bold(theme.BrightYellow("[Auto-Relocate Go Entrypoint]")),
								theme.Yellow(fmt.Sprintf("Relocating %q -> %q to prevent package declaration collision with library %q.", origTarget, targetPath, existingLibPkg)))
							session.RemoveQuarantined(origTarget)
						}
					}
				}
			}

			// Pre-flight Polyglot Language Check Guard (full file write)
			content = devops.NormalizePolyglotSource(targetPath, content)
			if preErr := devops.ValidatePolyglotPreflight(ws, targetPath, content); preErr != nil {
				// Deterministic Pre-Flight Auto-Repair: try trimming boundary lines (stray fence/header or >>>>/DONE)
				if repaired, didRepair, repErr := devops.AutoRepairPreflightSyntax(ws, targetPath, content); didRepair && repErr == nil {
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightGreen("[Pre-flight Auto-Trim Repaired]")),
						theme.Green(fmt.Sprintf("Auto-repaired boundary syntax slip in %s.", targetPath)))
					content = repaired
					session.RemoveQuarantined(targetPath)
				} else {
					qp := session.QuarantinePayload(targetPath, content, preErr.Error())
					if qp != nil && qp.Attempts >= 2 {
						fmt.Fprintf(out, "%s %s\n",
							theme.Bold(theme.BrightYellow("[Guard Override: Deterministic Payload Preserved]")),
							theme.Yellow(fmt.Sprintf("Writing %s after %d identical attempts to prevent work loss (validation: %v)", targetPath, qp.Attempts, preErr)))
						session.RemoveQuarantined(targetPath)
					} else {
						guardName := "[Polyglot Pre-flight Syntax Guard]"
						if strings.HasSuffix(strings.ToLower(targetPath), ".go") {
							guardName = "[Go Pre-flight Syntax Guard]"
						}
						fmt.Fprintf(out, "%s %s\n",
							theme.Bold(theme.BrightRed(guardName)),
							theme.Yellow(fmt.Sprintf("Rejected write to %s: %v", targetPath, preErr)))
						testPassed = false
						overallSuccess = false
						testDiagSummary = fmt.Sprintf("File write for %q was rejected by %s: %v. You must write complete, syntactically valid code without syntax errors, unclosed string literals, or platform slips.", targetPath, strings.Trim(guardName, "[]"), preErr)
						continue
					}
				}
			} else {
				session.RemoveQuarantined(targetPath)
			}

			fmt.Fprintf(out, "%s %s\n",
				theme.Bold(theme.BrightGreen("[Auto-Write Detected]")),
				theme.Green(fmt.Sprintf("Target File: %s", targetPath)))
			_ = executeWrite(session, targetPath, content, out)
			writtenPayloadsInTurn[contentSha] = targetPath
			writtenFilesInTurn[targetPath] = true

			cleanTP := filepath.ToSlash(targetPath)
			parts := strings.Split(cleanTP, "/")
			if len(parts) >= 2 && (parts[0] == "projects" || parts[0] == "apps") {
				lastTargetDir = filepath.Join(ws, parts[0], parts[1])
			}
		}

		// 4. Build & Verify — stay in conversational loop (visible reasoning)
		var explicitBuildCmd string
		executedBuildsInTurn := make(map[string]bool)

		for _, action := range actions {
			if action.Type == ActionBuild && action.Task() != "" {
				explicitBuildCmd = action.Task()
				if strings.Contains(explicitBuildCmd, `\"`) {
					explicitBuildCmd = strings.ReplaceAll(explicitBuildCmd, `\"`, `"`)
				}
				if strings.Contains(explicitBuildCmd, `\'`) {
					explicitBuildCmd = strings.ReplaceAll(explicitBuildCmd, `\'`, `'`)
				}
				normBuildCmd := strings.TrimSpace(explicitBuildCmd)
				if executedBuildsInTurn[normBuildCmd] {
					continue
				}
				executedBuildsInTurn[normBuildCmd] = true
				fmt.Fprintf(out, "\n%s %s\n",
					theme.Bold(theme.BrightCyan("[Build]")),
					theme.Cyan(fmt.Sprintf("Running: %s", explicitBuildCmd)))
				if !looksLikeShellCommand(explicitBuildCmd) {
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.Yellow("[Build Guard Rejected]")),
						theme.Yellow(fmt.Sprintf("Ignoring non-shell command %q: BUILD task must be an executable shell command (e.g. 'go test ./...'), not descriptive prose.", explicitBuildCmd)))
					explicitBuildCmd = ""
					continue
				}

				// Read-Only Probe Guard: on data bugs, never let a "probe" seed, init, sync or write the DB.
				if mutating, why := detectMutatingProbe(prompt, explicitBuildCmd); mutating {
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightRed("[Read-Only Probe Guard]")),
						theme.Red(fmt.Sprintf("Refused to run: probe calls %q, which writes persisted data during a data-bug investigation.", why)))
					testPassed = false
					testDiagSummary = mutatingProbeDiag(explicitBuildCmd, why)
					session.RecordBuild(explicitBuildCmd, 126, testDiagSummary)
					explicitBuildCmd = ""
					continue
				}

				// Tool Availability Guard: check against known missing tools
				if unavail := session.ExtractUnavailableToolFromCommand(explicitBuildCmd); unavail != "" {
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightRed("[Tool Guard Rejected]")),
						theme.Red(fmt.Sprintf("Cannot run %q: tool %q is not installed on this host environment.", explicitBuildCmd, unavail)))
					testPassed = false
					overallSuccess = false
					testDiagSummary = fmt.Sprintf("Command %q failed: tool %q is not installed on this host. Use native standard library runtimes or available host tools.", explicitBuildCmd, unavail)
					session.RecordBuild(explicitBuildCmd, 127, testDiagSummary)
					continue
				}

				// Pre-check executable binary via exec.LookPath
				cmdFields := strings.Fields(explicitBuildCmd)
				if len(cmdFields) > 0 {
					binName := strings.ToLower(cmdFields[0])
					binBase := filepath.Base(filepath.FromSlash(binName))
					binBase = strings.TrimSuffix(binBase, ".exe")
					binBase = strings.TrimSuffix(binBase, ".bat")
					binBase = strings.TrimSuffix(binBase, ".cmd")
					binBase = strings.TrimSuffix(binBase, ".sh")
					builtins := map[string]bool{
						"cd": true, "echo": true, "set": true, "export": true,
						"dir": true, "cls": true, "exit": true, "rem": true,
						"type": true, "copy": true, "del": true, "mkdir": true,
					}
					if !builtins[binBase] && !strings.Contains(cmdFields[0], "=") && !strings.HasPrefix(binName, "./") && !strings.HasPrefix(binName, ".\\") {
						if _, lookErr := exec.LookPath(cmdFields[0]); lookErr != nil {
							session.RecordUnavailableTool(binBase)
							fmt.Fprintf(out, "  %s  %s\n\n",
								theme.Bold(theme.BrightRed("[Tool Guard Rejected]")),
								theme.Red(fmt.Sprintf("Executable %q was not found in PATH on this host.", cmdFields[0])))
							testPassed = false
							overallSuccess = false
							testDiagSummary = fmt.Sprintf("Command %q failed: executable %q was not found in PATH on this host.", explicitBuildCmd, cmdFields[0])
							session.RecordBuild(explicitBuildCmd, 127, testDiagSummary)
							continue
						}
					}
				}

				// Daemon Guard: substitute non-terminating daemon command with static verification
				if IsDaemonCommand(explicitBuildCmd) {
					subCmd := SubstituteDaemonWithVerification(explicitBuildCmd, lastTargetDir)
					fmt.Fprintf(out, "  %s  %s\n",
						theme.Bold(theme.BrightCyan("[Daemon Substitution]")),
						theme.Cyan(fmt.Sprintf("Replaced non-terminating daemon %q with verification %q", explicitBuildCmd, subCmd)))
					explicitBuildCmd = subCmd
				}

				buildDir := ws
				if lastTargetDir != "" {
					buildDir = lastTargetDir
				} else if canonicalDir != "" {
					buildDir = filepath.Join(ws, filepath.FromSlash(canonicalDir))
				} else if activeProj := session.ActiveProject(); activeProj != "" {
					buildDir = filepath.Join(ws, filepath.FromSlash(activeProj))
				}
				buildDir = ResolveVerifyDir(ws, buildDir, explicitBuildCmd)
				if activePlanSpec != nil && activePlanSpec.ProjectScope != "" && !strings.Contains(explicitBuildCmd, "cd ") {
					targetDirToCheck := canonicalDir
					if targetDirToCheck == "" {
						targetDirToCheck = lastTargetDir
					}
					if targetDirToCheck == "" || strings.EqualFold(stripVariantSuffix(filepath.Base(targetDirToCheck)), stripVariantSuffix(filepath.Base(activePlanSpec.ProjectScope))) {
						scoped := filepath.Join(ws, filepath.FromSlash(activePlanSpec.ProjectScope))
						_ = os.MkdirAll(scoped, 0755)
						buildDir = scoped
					}
				}
				lastVerifyDir = buildDir
				readTurnCount = 0
				runCmd, extraEnv := SplitEnvPrefix(explicitBuildCmd)
				runCmd = normalizeBuildCommand(runCmd, buildDir, ws)
				if runtime.GOOS == "windows" {
					if _, err := exec.LookPath("gcc"); err == nil && os.Getenv("CGO_ENABLED") != "0" {
						hasCgoEnv := false
						for _, e := range extraEnv {
							if strings.HasPrefix(e, "CGO_ENABLED=") {
								hasCgoEnv = true
								break
							}
						}
						if !hasCgoEnv {
							extraEnv = append(extraEnv, "CGO_ENABLED=1")
						}
					}
				}
				if strings.Contains(strings.ToLower(runCmd), "python") || strings.Contains(strings.ToLower(runCmd), "pytest") || strings.Contains(strings.ToLower(runCmd), "unittest") {
					hasPyPath := false
					for _, e := range extraEnv {
						if strings.HasPrefix(e, "PYTHONPATH=") {
							hasPyPath = true
							break
						}
					}
					if !hasPyPath {
						pyPath := buildDir
						if cur := os.Getenv("PYTHONPATH"); cur != "" {
							pyPath = buildDir + string(os.PathListSeparator) + cur
						}
						extraEnv = append(extraEnv, "PYTHONPATH="+pyPath)
					}
					extraEnv = withEnv(extraEnv, "PYTEST_DISABLE_PLUGIN_AUTOLOAD", "1")
				}
				runCmd = stripRaceIfCgoUnavailable(runCmd)
				lastRunCmd = runCmd
				lastExtraEnv = extraEnv
				buildReport, buildErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:        runCmd,
					WorkspaceDir:   buildDir,
					TimeoutSeconds: 120,
					ExtraEnv:       extraEnv,
				})
				if (buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0)) &&
					buildReport != nil && strings.Contains(buildReport.CleanedStderr+buildReport.RawStderr, "requires cgo") && strings.Contains(explicitBuildCmd, "-race") {
					rerun := strings.ReplaceAll(runCmd, "-race", "")
					fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.Yellow("[Race Skipped]")), theme.Yellow("cgo is unavailable; rerunning without -race"))
					runCmd = rerun
					lastRunCmd = rerun
					explicitBuildCmd = rerun
					buildReport, buildErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
						Command: rerun, WorkspaceDir: buildDir, TimeoutSeconds: 120, ExtraEnv: extraEnv,
					})
				}
				if (buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0)) &&
					strings.Contains(runCmd, "go build .") &&
					buildReport != nil && strings.Contains(buildReport.CleanedStderr+buildReport.RawStderr+buildReport.RawStdout, "no Go files in") {
					fallbackCmd := strings.Replace(runCmd, "go build .", "go build ./...", 1)
					fbReport, fbErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
						Command:        fallbackCmd,
						WorkspaceDir:   buildDir,
						TimeoutSeconds: 120,
						ExtraEnv:       extraEnv,
					})
					if fbErr == nil && fbReport != nil && (fbReport.Success || fbReport.ExitCode == 0) {
						buildReport = fbReport
						buildErr = nil
						runCmd = fallbackCmd
						lastRunCmd = fallbackCmd
					}
				}
				if buildReport != nil && (buildErr != nil || (!buildReport.Success && buildReport.ExitCode != 0)) &&
					isPytestPluginCrash(runCmd, buildReport.CleanedStderr+buildReport.RawStderr+buildReport.RawStdout) {
					fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Env Auto-Recover]")),
						theme.Cyan("pytest crashed while loading a third-party plugin (environment fault, not project code); rerunning with plugin autoload disabled."))
					extraEnv = withEnv(extraEnv, "PYTEST_DISABLE_PLUGIN_AUTOLOAD", "1")
					lastExtraEnv = extraEnv
					buildReport, buildErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
						Command: runCmd, WorkspaceDir: buildDir, TimeoutSeconds: 180, ExtraEnv: extraEnv,
					})
				}
				if (buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0)) &&
					buildReport != nil && isModuleDependencyDiag(buildReport.CleanedStderr+buildReport.RawStderr) {
					if fixGoModule(buildDir) {
						fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Module Auto-Tidy]")), theme.Cyan("Resolved missing go.sum / module dependencies; rerunning build."))
						buildReport, buildErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command: runCmd, WorkspaceDir: buildDir, TimeoutSeconds: 120, ExtraEnv: extraEnv,
						})
					}
				}
				if (buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0)) &&
					buildReport != nil && isPythonDependencyDiag(buildReport.CleanedStderr+buildReport.RawStderr+buildReport.RawStdout) {
					diagText := buildReport.CleanedStderr + buildReport.RawStderr + buildReport.RawStdout
					if fixPythonModuleWithDiag(buildDir, diagText) || fixPythonModule(buildDir) {
						fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Python Auto-Install]")), theme.Cyan("Resolved missing Python requirements/dependencies; rerunning build."))
						buildReport, buildErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command: runCmd, WorkspaceDir: buildDir, TimeoutSeconds: 180, ExtraEnv: extraEnv,
						})
					}
				}
				if (buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0)) &&
					buildReport != nil && isNodeDependencyDiag(buildReport.CleanedStderr+buildReport.RawStderr+buildReport.RawStdout) {
					diagText := buildReport.CleanedStderr + buildReport.RawStderr + buildReport.RawStdout
					if fixNodeModuleWithDiag(buildDir, diagText) || fixNodeModule(buildDir) {
						fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Node Auto-Install]")), theme.Cyan("Resolved missing node_modules / dependencies; rerunning build."))
						buildReport, buildErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command: runCmd, WorkspaceDir: buildDir, TimeoutSeconds: 180, ExtraEnv: extraEnv,
						})
					}
				}
				if (buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0)) &&
					buildReport != nil && isRustDependencyDiag(buildReport.CleanedStderr+buildReport.RawStderr+buildReport.RawStdout) {
					if fixRustModule(buildDir) {
						fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Rust Auto-Check]")), theme.Cyan("Resolved Rust package dependencies; rerunning build."))
						buildReport, buildErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command: runCmd, WorkspaceDir: buildDir, TimeoutSeconds: 180, ExtraEnv: extraEnv,
						})
					}
				}
				if buildErr == nil && buildReport != nil && isTestRunnerCmd(explicitBuildCmd) &&
					zeroTestsExecuted(buildReport.RawStdout+"\n"+buildReport.RawStderr+"\n"+buildReport.CleanedStderr) {
					buildReport.Success = false
					if buildReport.ExitCode == 0 {
						buildReport.ExitCode = 5
					}
					buildReport.CleanedStderr = "ZERO TESTS EXECUTED: the command exited 0 but no tests ran (discovery/import problem). This is NOT a pass. " +
						"Check the test path, that test files are named test_*.py, and that imports resolve.\n" + buildReport.CleanedStderr
					if tf := listTestFiles(buildDir, 5); len(tf) > 0 {
						buildReport.CleanedStderr = "Test files that exist on disk: " + strings.Join(tf, ", ") + ". Do NOT retry the same discovery command; run one of these files directly (e.g. python <file>) or fix the missing __init__.py/import path.\n" + buildReport.CleanedStderr
					} else {
						buildReport.CleanedStderr = "No test files exist under the build directory. Do NOT retry discovery; verify the change another way (e.g. run the real code path and inspect actual state).\n" + buildReport.CleanedStderr
					}
				}
				if buildErr == nil && buildReport != nil && (buildReport.Success || buildReport.ExitCode == 0) {
					if ran, ok, smokeDiag := ProbeAllProjectEntries(ctx, buildDir, extraEnv); ran {
						if ok {
							fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightGreen("[Universal Launch Probe OK]")), theme.Green("application entries verified and started cleanly with no exceptions"))
						} else {
							buildReport.Success = false
							if buildReport.ExitCode == 0 {
								buildReport.ExitCode = 1
							}
							buildReport.CleanedStderr = smokeDiag + "\n" + buildReport.CleanedStderr
						}
					}
				}
				if buildErr != nil || (buildReport != nil && !buildReport.Success && buildReport.ExitCode != 0) {
					testPassed = false
					if buildReport != nil {
						failedDiag := buildReport.CleanedStderr
						if failedDiag == "" {
							failedDiag = buildReport.RepairPromptPayload
						}
						if failedDiag == "" {
							failedDiag = buildReport.RawStderr
						}
						if failedDiag == "" {
							failedDiag = buildReport.RawStdout
						}
						// Accumulate diagnostics from ALL failing builds in this turn
						if testDiagSummary == "" {
							testDiagSummary = failedDiag
						} else if failedDiag != "" && failedDiag != testDiagSummary {
							testDiagSummary = testDiagSummary + "\n\n" + failedDiag
						}
					} else {
						if testDiagSummary == "" {
							testDiagSummary = buildErr.Error()
						}
					}
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightRed("[Build Failed]")),
						theme.Red(fmt.Sprintf("%s failed:\n%s", explicitBuildCmd, testDiagSummary)))
					diagAnalysis := analyzeICXSubterraneanFeedback(ws, lastTargetDir, explicitBuildCmd, testDiagSummary)
					if diagAnalysis.ConversationalSpeech != "" {
						fmt.Fprintf(out, "  %s\n  \"%s\"\n\n",
							theme.Bold(theme.BrightCyan("ICX (Subterranean Engine):")),
							theme.Cyan(diagAnalysis.ConversationalSpeech))
					}
					exitCode := 1
					if buildReport != nil {
						exitCode = buildReport.ExitCode
					}
					session.RecordBuild(explicitBuildCmd, exitCode, testDiagSummary)
					explicitBuildRan = true
					if bs := session.BuildSession(); bs != nil {
						_ = bs.NoteDiagnostic(ctx, testDiagSummary, "build.error")
						bs.BumpAttempts(ctx, "architect")
					}
					if missing := ExtractMissingTool(testDiagSummary); missing != "" {
						session.RecordUnavailableTool(missing)
					}
				} else {
					// Only mark test as passed if no prior build in this turn has failed.
					// This prevents a secondary `go build main.go` from masking a failed `go test ./...`.
					if testPassed {
						// testPassed was already true (no prior failure) — keep it true
					}
					// Note: do NOT set testPassed = true if a prior build already set it to false
					explicitBuildRan = true
					fmt.Fprintf(out, "  %s  %s\n",
						theme.Bold(theme.BrightGreen("[Build Passed]")),
						theme.Green(fmt.Sprintf("%s completed successfully", explicitBuildCmd)))
					if buildReport != nil {
						rawOut := strings.TrimSpace(buildReport.RawStdout)
						rawErr := strings.TrimSpace(buildReport.RawStderr)
						combinedOut := rawOut
						if rawErr != "" {
							if combinedOut != "" {
								combinedOut = combinedOut + "\n" + rawErr
							} else {
								combinedOut = rawErr
							}
						}
						lines := strings.Split(combinedOut, "\n")
						var cleanLines []string
						for _, l := range lines {
							if strings.Contains(l, "UserWarning: [W") {
								continue
							}
							cleanLines = append(cleanLines, l)
						}
						displayOut := strings.TrimSpace(strings.Join(cleanLines, "\n"))
						if displayOut != "" {
							if len(displayOut) > 1500 {
								displayOut = displayOut[:1500] + "\n... (output truncated)"
							}
							if isDataBugInvestigation(prompt) && !isTestInvocation(explicitBuildCmd) {
								probeFeedback = fmt.Sprintf("Command: %s\nExit: 0\nOutput:\n%s", explicitBuildCmd, displayOut)
								if rep, obs, mm := detectSymptomMismatch(prompt, explicitBuildCmd, displayOut); mm {
									symptomMismatch = true
									symptomMismatchMsg = symptomMismatchNote(rep, obs)
									probeFeedback += symptomMismatchMsg
									fmt.Fprintf(out, "  %s %s\n",
										theme.Bold(theme.BrightYellow("[Symptom Mismatch]")),
										theme.Yellow(fmt.Sprintf("User reports %d, probe shows %d: the reported symptom is NOT reproduced by this probe.", rep, obs)))
								}
							}
							fmt.Fprintf(out, "  %s\n```\n%s\n```\n\n",

								theme.Bold(theme.BrightCyan("[Output]")),
								theme.Cyan(displayOut))
						} else {
							fmt.Fprintln(out)
						}
					} else {
						fmt.Fprintln(out)
					}
					// Quarantine Reconciler: if project is verified complete on disk, clear obsolete quarantined files
					qList := session.AllQuarantined()
					if len(qList) > 0 {
						targetCheckDir := buildDir
						if targetCheckDir == "" || targetCheckDir == ws {
							targetCheckDir = lastTargetDir
						}
						if targetCheckDir == "" && canonicalDir != "" {
							targetCheckDir = filepath.Join(ws, filepath.FromSlash(canonicalDir))
						}
						if targetCheckDir != "" && targetCheckDir != ws {
							if isComp, missing := AssessProjectCompleteness(ws, targetCheckDir); isComp && len(missing) == 0 {
								for _, qp := range qList {
									absQ := filepath.Join(ws, filepath.FromSlash(qp.Path))
									if strings.HasPrefix(filepath.Clean(absQ), filepath.Clean(targetCheckDir)) {
										session.RemoveQuarantined(qp.Path)
									}
								}
								qList = session.AllQuarantined()
							}
						}
					}
					if len(qList) > 0 {
						var qNames []string
						for _, qp := range qList {
							qNames = append(qNames, filepath.Base(qp.Path))
						}
						fmt.Fprintf(out, "  %s\n  \"%s\"\n\n",
							theme.Bold(theme.BrightYellow("ICX (Subterranean Engine):")),
							theme.Yellow(fmt.Sprintf("Command passed, but %d deliverable(s) are quarantined on disk (%s). Project is NOT yet clean.", len(qNames), strings.Join(qNames, ", "))))
					} else {
						fmt.Fprintf(out, "  %s\n  \"%s\"\n\n",
							theme.Bold(theme.BrightCyan("ICX (Subterranean Engine):")),
							theme.Cyan("Lattice synchronized and verified clean. Nice work, Architect!"))
					}
					session.RecordBuild(explicitBuildCmd, 0, "SUCCESS")
					if bs := session.BuildSession(); bs != nil {
						_ = bs.Receipt(ctx, buildDir, 0, "verified clean")
					}
					skipCommit = IsSideCommand(explicitBuildCmd)
				}
			} else if action.Type == ActionMap && action.Query() != "" {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightYellow("[Auto-Engine Detected]")),
					theme.Yellow(fmt.Sprintf("Map Query: %s", action.Query())))
				_, _ = executeMap(ctx, session, 4000, action.Query(), out)
			} else if action.Type == ActionHeal && action.Cmd() != "" {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightYellow("[Auto-Engine Detected]")),
					theme.Yellow(fmt.Sprintf("Heal Command: %s", action.Cmd())))
				_ = executeHeal(ctx, session, action.Cmd(), out)
			}
		}
		// 5. Scoped verification — verify the phase that was touched in this turn
		planSpec := activePlanSpec
		if planSpec == nil {
			if spec := engine.DetectPlanSpec(prompt, ws); spec != nil {
				if canonicalDir == "" || spec.ProjectScope == "" || strings.EqualFold(stripVariantSuffix(filepath.Base(canonicalDir)), stripVariantSuffix(filepath.Base(spec.ProjectScope))) {
					planSpec = spec
				}
			}
		}
		if planSpec == nil && canonicalDir != "" {
			planSpec = engine.DetectPlanSpec(canonicalDir, ws)
		}
		if planSpec != nil && canonicalDir != "" && planSpec.ProjectScope != "" {
			canonBase := stripVariantSuffix(filepath.Base(canonicalDir))
			planBase := stripVariantSuffix(filepath.Base(planSpec.ProjectScope))
			if !strings.EqualFold(canonBase, planBase) {
				planSpec = nil
				activePlanSpec = nil
				session.SetActivePlan(nil, "")
			}
		}
		if planSpec != nil {
			activePlanSpec = planSpec
		}
		if planSpec != nil && explicitBuildCmd == "" && len(fileActions) > 0 {
			// Find which phase was touched by the files written in this turn
			var touchedPhase *engine.PlanPhase
			for _, action := range fileActions {
				targetP := strings.TrimSpace(action.Path())
				targetP = strings.Trim(targetP, "\"'`")
				if remappedPaths != nil {
					if rPath, ok := remappedPaths[action]; ok && rPath != "" {
						targetP = rPath
					}
				}
				if ph := planSpec.FindPhaseForFile(targetP); ph != nil {
					if touchedPhase == nil || ph.PhaseNumber > touchedPhase.PhaseNumber {
						touchedPhase = ph
					}
				}
			}

			if touchedPhase != nil {
				complete, missing := planSpec.IsPhaseFilesPresent(ws, touchedPhase)
				if !complete {
					// Phase is partially written; do not run tests yet, allow next turn to supply missing files.
					// Only clear testPassed if no explicit build in this turn already failed.
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightYellow("[Phase In Progress]")),
						theme.Yellow(fmt.Sprintf("Phase %d (%s) partially written (%d/%d files present, %d missing). Continuing generation...",
							touchedPhase.PhaseNumber, touchedPhase.Name, len(touchedPhase.TargetFiles)-len(missing), len(touchedPhase.TargetFiles), len(missing))))
					if testPassed {
						// testPassed was true (no prior build failures) — keep it true to allow next turn
					}
				} else if touchedPhase.VerificationCmd == "" {
					// Asset/web phase without Go test command
					// Verify that all target assets are present on disk and meet depth standards
					shallowAssets := 0
					var shallowMsg string
					var missingAssets []string
					for _, tf := range touchedPhase.TargetFiles {
						if !planSpec.FileExists(ws, tf) {
							missingAssets = append(missingAssets, tf)
							continue
						}
						scopedP := tf
						if planSpec.ProjectScope != "" && !strings.HasPrefix(tf, planSpec.ProjectScope+"/") {
							scopedP = filepath.Join(planSpec.ProjectScope, tf)
						}
						diskPath := filepath.Join(ws, filepath.FromSlash(scopedP))
						if data, err := os.ReadFile(diskPath); err == nil {
							if isShallow, reason := checkImplementationDepth(tf, string(data), planSpec); isShallow {
								shallowAssets++
								shallowMsg = reason
								break
							}
						}
					}
					if len(missingAssets) > 0 {
						testPassed = false
						overallSuccess = false
						testDiagSummary = fmt.Sprintf("Phase %d asset verification failed: target file(s) %v are missing from disk. Provide full implementations for all phase target files.", touchedPhase.PhaseNumber, missingAssets)
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightRed("[Asset Verification Failed]")),
							theme.Red(testDiagSummary))
					} else if shallowAssets > 0 {
						testPassed = false
						overallSuccess = false
						testDiagSummary = fmt.Sprintf("Phase %d asset verification failed: %s. Provide full, production-quality implementation.", touchedPhase.PhaseNumber, shallowMsg)
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightRed("[Asset Verification Failed]")),
							theme.Red(testDiagSummary))
					} else {
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightGreen("[Verification Passed]")),
							theme.Green(fmt.Sprintf("Phase %d (%s) verified (all %d target assets present on disk with complete depth)",
								touchedPhase.PhaseNumber, touchedPhase.Name, len(touchedPhase.TargetFiles))))
						if testPassed {
							// testPassed was true (no prior build failures) — keep it true
						}
					}
				} else {
					// All phase files present on disk; run phase-specific verification
					projDir := filepath.Join(ws, filepath.FromSlash(planSpec.ProjectScope))
					verifyCmd := stripRaceIfCgoUnavailable(touchedPhase.VerificationCmd)
					lastVerifyDir = projDir
					lastRunCmd = verifyCmd
					var verifyEnv []string
					if runtime.GOOS == "windows" {
						if _, err := exec.LookPath("gcc"); err == nil && os.Getenv("CGO_ENABLED") != "0" {
							verifyEnv = append(verifyEnv, "CGO_ENABLED=1")
						}
					}
					verifyEnv = withEnv(verifyEnv, "PYTEST_DISABLE_PLUGIN_AUTOLOAD", "1")
					lastExtraEnv = verifyEnv
					testReport, testErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
						Command:        verifyCmd,
						WorkspaceDir:   projDir,
						TimeoutSeconds: 60,
						ExtraEnv:       verifyEnv,
					})
					if (testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0)) &&
						testReport != nil && strings.Contains(testReport.CleanedStderr+testReport.RawStderr, "requires cgo") && strings.Contains(verifyCmd, "-race") {
						rerun := strings.ReplaceAll(verifyCmd, "-race", "")
						fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.Yellow("[Race Skipped]")), theme.Yellow("cgo is unavailable; rerunning without -race"))
						verifyCmd = rerun
						lastRunCmd = rerun
						testReport, testErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command:        rerun,
							WorkspaceDir:   projDir,
							TimeoutSeconds: 60,
							ExtraEnv:       verifyEnv,
						})
					}
					if (testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0)) &&
						strings.Contains(verifyCmd, "go build .") &&
						testReport != nil && strings.Contains(testReport.CleanedStderr+testReport.RawStderr+testReport.RawStdout, "no Go files in") {
						fallbackCmd := strings.Replace(verifyCmd, "go build .", "go build ./...", 1)
						fbReport, fbErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command:        fallbackCmd,
							WorkspaceDir:   projDir,
							TimeoutSeconds: 60,
							ExtraEnv:       verifyEnv,
						})
						if fbErr == nil && fbReport != nil && (fbReport.Success || fbReport.ExitCode == 0) {
							testReport = fbReport
							testErr = nil
							verifyCmd = fallbackCmd
							lastRunCmd = fallbackCmd
						}
					}
					if (testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0)) &&
						testReport != nil && isModuleDependencyDiag(testReport.CleanedStderr+testReport.RawStderr) {
						if fixGoModule(projDir) {
							fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Module Auto-Tidy]")), theme.Cyan("Resolved missing go.sum / module dependencies; rerunning phase verification."))
							testReport, testErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
								Command:        verifyCmd,
								WorkspaceDir:   projDir,
								TimeoutSeconds: 60,
								ExtraEnv:       verifyEnv,
							})
						}
					}
					if (testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0)) &&
						testReport != nil && isPythonDependencyDiag(testReport.CleanedStderr+testReport.RawStderr+testReport.RawStdout) {
						diagText := testReport.CleanedStderr + testReport.RawStderr + testReport.RawStdout
						if fixPythonModuleWithDiag(projDir, diagText) || fixPythonModule(projDir) {
							fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Python Auto-Install]")), theme.Cyan("Resolved missing Python requirements/dependencies; rerunning phase verification."))
							testReport, testErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
								Command:        verifyCmd,
								WorkspaceDir:   projDir,
								TimeoutSeconds: 180,
								ExtraEnv:       verifyEnv,
							})
						}
					}
					if (testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0)) &&
						testReport != nil && isNodeDependencyDiag(testReport.CleanedStderr+testReport.RawStderr+testReport.RawStdout) {
						diagText := testReport.CleanedStderr + testReport.RawStderr + testReport.RawStdout
						if fixNodeModuleWithDiag(projDir, diagText) || fixNodeModule(projDir) {
							fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Node Auto-Install]")), theme.Cyan("Resolved missing node_modules / dependencies; rerunning phase verification."))
							testReport, testErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
								Command:        verifyCmd,
								WorkspaceDir:   projDir,
								TimeoutSeconds: 180,
								ExtraEnv:       verifyEnv,
							})
						}
					}
					if (testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0)) &&
						testReport != nil && isRustDependencyDiag(testReport.CleanedStderr+testReport.RawStderr+testReport.RawStdout) {
						if fixRustModule(projDir) {
							fmt.Fprintf(out, "  %s  %s\n\n", theme.Bold(theme.BrightCyan("[Rust Auto-Check]")), theme.Cyan("Resolved Rust package dependencies; rerunning phase verification."))
							testReport, testErr = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
								Command:        verifyCmd,
								WorkspaceDir:   projDir,
								TimeoutSeconds: 180,
								ExtraEnv:       verifyEnv,
							})
						}
					}
					if testErr != nil || (testReport != nil && !testReport.Success && testReport.ExitCode != 0) {
						testPassed = false
						if testReport != nil {
							testDiagSummary = testReport.CleanedStderr
							if testDiagSummary == "" {
								testDiagSummary = testReport.RepairPromptPayload
							}
							if testDiagSummary == "" {
								testDiagSummary = testReport.RawStderr
							}
							if testDiagSummary == "" {
								testDiagSummary = testReport.RawStdout
							}
						} else {
							testDiagSummary = testErr.Error()
						}
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightRed("[Verification Failed]")),
							theme.Red(fmt.Sprintf("Phase %d (%s) verification %q failed:\n%s",
								touchedPhase.PhaseNumber, touchedPhase.Name, verifyCmd, testDiagSummary)))
					} else if testReport != nil && isZeroTestsPass(verifyCmd, testReport.RawStdout, testReport.RawStderr) {
						testPassed = false
						overallSuccess = false
						testDiagSummary = fmt.Sprintf("Phase %d (%s) verification %q executed 0 tests ([no test files]). A complete project requires unit tests (*_test.go).",
							touchedPhase.PhaseNumber, touchedPhase.Name, verifyCmd)
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightRed("[Zero Tests Detected]")),
							theme.Red(testDiagSummary))
					} else {
						testPassed = true
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightGreen("[Verification Passed]")),
							theme.Green(fmt.Sprintf("Phase %d (%s): %s passed with exit code 0",
								touchedPhase.PhaseNumber, touchedPhase.Name, verifyCmd)))
					}
				}
			}
		}

		// 5b. Unplanned project auto-verification — dynamically detect and run verification for any app
		if planSpec == nil && explicitBuildCmd == "" && len(fileActions) > 0 {
			targetDir := lastTargetDir
			if targetDir == "" && canonicalDir != "" {
				targetDir = filepath.Join(ws, filepath.FromSlash(canonicalDir))
			}
			if targetDir == "" && session.ActiveProject() != "" {
				targetDir = filepath.Join(ws, filepath.FromSlash(session.ActiveProject()))
			}
			if targetDir == "" {
				targetDir = ws
			}

			autoCmd := DetectProjectVerificationCmd(targetDir)
			if autoCmd != "" {
				fmt.Fprintf(out, "\n%s %s\n",
					theme.Bold(theme.BrightCyan("[Auto-Verification]")),
					theme.Cyan(fmt.Sprintf("Running project verification: %s (in %s)", autoCmd, targetDir)))
				runCmd := stripRaceIfCgoUnavailable(autoCmd)
				lastVerifyDir = targetDir
				lastRunCmd = runCmd
				var autoEnv []string
				if runtime.GOOS == "windows" {
					if _, err := exec.LookPath("gcc"); err == nil && os.Getenv("CGO_ENABLED") != "0" {
						autoEnv = append(autoEnv, "CGO_ENABLED=1")
					}
				}
				autoEnv = append(autoEnv, "PYTHONPATH="+targetDir)
				autoEnv = withEnv(autoEnv, "PYTEST_DISABLE_PLUGIN_AUTOLOAD", "1")
				lastExtraEnv = autoEnv
				autoReport, autoErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:        runCmd,
					WorkspaceDir:   targetDir,
					TimeoutSeconds: 90,
					ExtraEnv:       autoEnv,
				})
				if (autoErr != nil || (autoReport != nil && !autoReport.Success && autoReport.ExitCode != 0)) &&
					strings.Contains(runCmd, "go build .") &&
					autoReport != nil && strings.Contains(autoReport.CleanedStderr+autoReport.RawStderr+autoReport.RawStdout, "no Go files in") {
					fallbackCmd := strings.Replace(runCmd, "go build .", "go build ./...", 1)
					fbReport, fbErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
						Command:        fallbackCmd,
						WorkspaceDir:   targetDir,
						TimeoutSeconds: 90,
						ExtraEnv:       autoEnv,
					})
					if fbErr == nil && fbReport != nil && (fbReport.Success || fbReport.ExitCode == 0) {
						autoReport = fbReport
						autoErr = nil
						runCmd = fallbackCmd
						lastRunCmd = fallbackCmd
					}
				}
				// Zero-test fallback: discovery collected nothing but test files exist -> run them directly,
				// and accept the result only if the output proves tests actually executed.
				if autoReport != nil && zeroTestsExecuted(autoReport.RawStdout+"\n"+autoReport.RawStderr+"\n"+autoReport.CleanedStderr) {
					fbEnv := withEnv(autoEnv, "PYTEST_DISABLE_PLUGIN_AUTOLOAD", "1")
					for _, direct := range zeroTestFallbackCmds(targetDir) {
						fmt.Fprintf(out, "  %s  %s\n",
							theme.Bold(theme.BrightCyan("[Zero-Test Fallback]")),
							theme.Cyan(fmt.Sprintf("Discovery ran 0 tests; trying: %s", direct)))
						dr, de := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command: direct, WorkspaceDir: targetDir, TimeoutSeconds: 120, ExtraEnv: fbEnv,
						})
						if de != nil || dr == nil {
							continue
						}
						drOut := dr.RawStdout + "\n" + dr.RawStderr
						if runnerMissing(drOut) {
							continue
						}
						if (dr.Success || dr.ExitCode == 0) && testsActuallyRan(drOut) {
							autoReport, autoErr = dr, nil
							runCmd, lastRunCmd = direct, direct
							break
						}
						if dr.ExitCode != 0 {
							autoReport, autoErr = dr, nil // real test failures are more useful than "0 tests"
							runCmd, lastRunCmd = direct, direct
							break
						}
					}
				}
				if autoErr != nil || (autoReport != nil && !autoReport.Success && autoReport.ExitCode != 0) {
					testPassed = false
					exitCode := 1
					if autoReport != nil {
						exitCode = autoReport.ExitCode
						testDiagSummary = autoReport.CleanedStderr
						if testDiagSummary == "" {
							testDiagSummary = autoReport.RepairPromptPayload
						}
						if testDiagSummary == "" {
							testDiagSummary = autoReport.RawStderr
						}
						if testDiagSummary == "" {
							testDiagSummary = autoReport.RawStdout
						}
					} else {
						testDiagSummary = autoErr.Error()
					}
					session.RecordBuild(runCmd, exitCode, testDiagSummary)
					if missing := ExtractMissingTool(testDiagSummary); missing != "" {
						session.RecordUnavailableTool(missing)
					}
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightRed("[Auto-Verification Failed]")),
						theme.Red(fmt.Sprintf("%s failed:\n%s", runCmd, testDiagSummary)))
				} else if autoReport != nil && isZeroTestsPass(runCmd, autoReport.RawStdout, autoReport.RawStderr) {
					testPassed = false
					overallSuccess = false
					testDiagSummary = fmt.Sprintf("Auto-verification %q executed 0 tests ([no test files]). A complete project requires unit tests (*_test.go).", runCmd)
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightRed("[Zero Tests Detected]")),
						theme.Red(testDiagSummary))
				} else {
					session.RecordBuild(runCmd, 0, "SUCCESS")
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightGreen("[Auto-Verification Passed]")),
						theme.Green(fmt.Sprintf("%s passed with exit code 0", runCmd)))
				}
			}
		}

		// 5. Conversational loop progression ("talk back and forth to itself")
		// Case A: Test failed - prompt LLM with diagnostics to self-heal
		if !testPassed {
			overallSuccess = false
			if testDiagSummary == "" {
				testDiagSummary = "Verification failed with non-zero exit code or build error."
			}
			if lastVerifyDir == "" {
				if lastTargetDir != "" {
					lastVerifyDir = lastTargetDir
				} else {
					lastVerifyDir = ws
				}
			}
			if lastRunCmd == "" {
				if st, err := os.Stat(filepath.Join(lastVerifyDir, "go.mod")); err == nil && !st.IsDir() {
					lastRunCmd = "go test ./..."
				}
			}
			if _, healed := prehealSandbox(lastVerifyDir, testDiagSummary); lastVerifyDir != "" && healed && lastRunCmd != "" {
				fmt.Fprintf(out, "  %s  %s\n\n",
					theme.Bold(theme.BrightCyan("[Pre-Heal]")),
					theme.Cyan("Applied a deterministic fix in the project and retesting."))
				healReport, healErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command: lastRunCmd, WorkspaceDir: lastVerifyDir, TimeoutSeconds: 120, ExtraEnv: lastExtraEnv,
				})
				if healErr == nil && healReport != nil && (healReport.Success || healReport.ExitCode == 0) {
					testPassed = true
					overallSuccess = true
				} else if healReport != nil {
					testDiagSummary = healReport.CleanedStderr
					if testDiagSummary == "" {
						testDiagSummary = healReport.RawStderr
					}
				}
			}
			if testPassed {
				consecutiveHeals = 0
				stagnantHeals = 0
				lastErrorCount = 0
				lastTestDiag = ""
				turnSnapshots = make(map[string][]byte)
			} else {
				if mentioned := extractDiagFiles(testDiagSummary); len(mentioned) > 0 {
					for rel := range turnSnapshots {
						hit := false
						for _, m := range mentioned {
							if rel == m || strings.HasSuffix(rel, m) || strings.Contains(testDiagSummary, filepath.Base(filepath.FromSlash(rel))) {
								hit = true
								break
							}
						}
						if !hit {
							delete(turnSnapshots, rel)
						}
					}
				}
				consecutiveHeals++

				isGuardRejection := strings.Contains(testDiagSummary, "[Tool Guard Rejected]") ||
					strings.Contains(testDiagSummary, "is not installed on this host") ||
					strings.Contains(testDiagSummary, "[Build Guard Rejected]") ||
					strings.Contains(testDiagSummary, "File write for ")

				currentErrorCount := countDiagnosticErrors(testDiagSummary)
				isConverging := false
				if !isGuardRejection && lastErrorCount > 0 && currentErrorCount < lastErrorCount {
					isConverging = true
					stagnantHeals = 0
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightGreen("[Convergence Detected]")),
						theme.Green(fmt.Sprintf("Compiler errors reduced (%d -> %d). Extending self-healing budget.", lastErrorCount, currentErrorCount)))
				} else {
					stagnantHeals++
				}
				if !isGuardRejection {
					lastErrorCount = currentErrorCount
				}

				// Loop Detection Circuit Breaker: identical diagnostic repeated
				isZeroTestsDiag := zeroTestsExecuted(testDiagSummary) ||
					strings.Contains(testDiagSummary, "NO TESTS RAN") ||
					strings.Contains(testDiagSummary, "Ran 0 tests") ||
					strings.Contains(testDiagSummary, "executed 0 tests") ||
					strings.Contains(testDiagSummary, "collected 0 items") ||
					strings.Contains(testDiagSummary, "no tests ran")

				if strings.TrimSpace(testDiagSummary) != "" && strings.TrimSpace(testDiagSummary) == strings.TrimSpace(lastTestDiag) {
					if !isZeroTestsDiag {
						identicalDiagCount++
					}
					if identicalDiagCount >= 3 {
						testPassed = false
						overallSuccess = false
						fmt.Fprintf(out, "%s %s\n\n",
							theme.Bold(theme.BrightRed("[Self-Healing Circuit Breaker]")),
							theme.Red(fmt.Sprintf("Identical compiler diagnostics repeated across consecutive attempts. Halting auto-loop to preserve context quality.\nDiagnostic: %s\nType 'continue' or provide instructions to resume remediation.", testDiagSummary)))
						break
					}
				} else {
					identicalDiagCount = 0
				}

				// Stagnation Circuit Breaker: no progress across multiple attempts
				if !isZeroTestsDiag && stagnantHeals > 3 {
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightRed("[Self-Healing Circuit Breaker]")),
						theme.Red(fmt.Sprintf("Compiler errors not converging across %d consecutive attempts without progress. Halting auto-loop to prevent context degradation.\nFinal Diagnostic:\n%s", stagnantHeals, testDiagSummary)))
					break
				}

				// Absolute Safety Ceiling Circuit Breaker: capped at 10 total healing attempts
				if consecutiveHeals > 10 {
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightRed("[Self-Healing Circuit Breaker]")),
						theme.Red(fmt.Sprintf("Exceeded absolute maximum healing budget (10 attempts). Halting auto-loop to prevent context degradation.\nFinal Diagnostic:\n%s", testDiagSummary)))
					break
				}

				failedAttempts = append(failedAttempts, TurnAttemptRecord{
					AttemptNumber:  consecutiveHeals,
					Command:        lastRunCmd,
					ActionsSummary: currentActionsSummary,
					Diagnostic:     testDiagSummary,
				})

				lastTestDiag = testDiagSummary
				nextStepMsg := buildSelfHealingPrompt(ws, lastTargetDir, consecutiveHeals, testDiagSummary, failedAttempts...)
				var statusMsg string
				if strings.HasPrefix(testDiagSummary, "Edit to ") && strings.Contains(testDiagSummary, "was rejected") {
					statusMsg = "[Auto-Next Step: Self-Healing] Edit was blocked by a safety guard (tests were not the problem); requesting a compliant fix..."
				} else if isConverging {
					statusMsg = fmt.Sprintf("[Auto-Next Step: Self-Healing] (Attempt %d, %d errors remaining, converging) Fixing test failure...", consecutiveHeals, currentErrorCount)
				} else {
					statusMsg = fmt.Sprintf("[Auto-Next Step: Self-Healing] (Attempt %d, %d errors remaining) Fixing test failure...", consecutiveHeals, currentErrorCount)
				}
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightCyan("omnicoder>")),
					theme.Yellow(statusMsg))
				currentPrompt = nextStepMsg
				continue
			}
		}

		if skipCommit {
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.Yellow("[Side Command]")),
				theme.Yellow("Side command finished successfully. Re-verifying pending build/tests..."))

			pendingVerifyCmd := lastRunCmd
			pendingVerifyDir := lastVerifyDir
			if pendingVerifyCmd != "" && !IsSideCommand(pendingVerifyCmd) && pendingVerifyDir != "" {
				verifyReport, verifyErr := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:        pendingVerifyCmd,
					WorkspaceDir:   pendingVerifyDir,
					TimeoutSeconds: 60,
					ExtraEnv:       lastExtraEnv,
				})
				if verifyErr == nil && verifyReport != nil && (verifyReport.Success || verifyReport.ExitCode == 0) {
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightGreen("[Verification Passed]")),
						theme.Green(fmt.Sprintf("%s passed after side command!", pendingVerifyCmd)))
					testPassed = true
					overallSuccess = true
					skipCommit = false
				} else {
					if verifyReport != nil {
						testDiagSummary = verifyReport.CleanedStderr
						if testDiagSummary == "" {
							testDiagSummary = verifyReport.RepairPromptPayload
						}
						if testDiagSummary == "" {
							testDiagSummary = verifyReport.RawStderr
						}
						if testDiagSummary == "" {
							testDiagSummary = verifyReport.RawStdout
						}
					} else if verifyErr != nil {
						testDiagSummary = verifyErr.Error()
					}
					fmt.Fprintf(out, "  %s  %s\n\n",
						theme.Bold(theme.BrightYellow("[Re-verification Pending]")),
						theme.Yellow(fmt.Sprintf("%s still needs fixes:\n%s", pendingVerifyCmd, testDiagSummary)))
					testPassed = false
					consecutiveHeals++
					lastTestDiag = testDiagSummary
					failedAttempts = append(failedAttempts, TurnAttemptRecord{
						AttemptNumber:  consecutiveHeals,
						Command:        pendingVerifyCmd,
						ActionsSummary: currentActionsSummary,
						Diagnostic:     testDiagSummary,
					})
					currentPrompt = buildSelfHealingPrompt(ws, lastTargetDir, consecutiveHeals, testDiagSummary, failedAttempts...)
					continue
				}
			} else {
				currentPrompt = "Side command finished. Continue with the current milestone implementation. Do not treat the side command as test verification."
				continue
			}
		}

		// Reset healing streak and commit snapshots on successful verification
		consecutiveHeals = 0
		stagnantHeals = 0
		lastErrorCount = 0
		lastTestDiag = ""
		identicalDiagCount = 0
		overallSuccess = true
		turnSnapshots = make(map[string][]byte) // committed!
		if buildSess != nil && planSpec != nil && planSpec.IsComplete(ws) {
			session.ClearDialogue(ctx)
			_ = buildSess.ResetDialogue(ctx)
		}

		// Case B: Master Plan active - determine next active phase
		if planSpec == nil {
			planSpec = activePlanSpec
		}
		if planSpec == nil {
			if spec, _ := session.ActivePlan(); spec != nil {
				if canonicalDir == "" || spec.ProjectScope == "" || strings.EqualFold(stripVariantSuffix(filepath.Base(canonicalDir)), stripVariantSuffix(filepath.Base(spec.ProjectScope))) {
					planSpec = spec
				}
			}
		}
		if planSpec == nil {
			if spec := engine.DetectPlanSpec(prompt, ws); spec != nil {
				if canonicalDir == "" || spec.ProjectScope == "" || strings.EqualFold(stripVariantSuffix(filepath.Base(canonicalDir)), stripVariantSuffix(filepath.Base(spec.ProjectScope))) {
					planSpec = spec
				}
			}
			if planSpec == nil && canonicalDir != "" {
				planSpec = engine.DetectPlanSpec(canonicalDir, ws)
			}
		}
		// Strict Project Scope Isolation Guard:
		// If an active canonical project or lastTargetDir is established, and planSpec targets a different project,
		// discard planSpec immediately to prevent cross-project plan hijacking and false Phase Incomplete Guards.
		if planSpec != nil && planSpec.ProjectScope != "" {
			targetProj := canonicalDir
			if targetProj == "" {
				targetProj = lastTargetDir
			}
			if targetProj != "" {
				canonBase := stripVariantSuffix(filepath.Base(targetProj))
				planBase := stripVariantSuffix(filepath.Base(planSpec.ProjectScope))
				if !strings.EqualFold(canonBase, planBase) {
					planSpec = nil
					activePlanSpec = nil
					session.SetActivePlan(nil, "")
				}
			}
		}
		if planSpec != nil {
			activePlanSpec = planSpec
			session.SetActivePlan(planSpec, "")
		}
		if planSpec != nil {
			if planSpec.IsComplete(ws) {
				if buildSess != nil {
					_ = buildSess.SetMilestone(ctx, len(planSpec.Phases))
				}
				if !finalVerificationSent {
					finalVerificationSent = true
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightGreen("[Plan Completed]")),
						theme.Green(fmt.Sprintf("All %d phases of %s verified complete! Requesting final verification...", len(planSpec.Phases), planSpec.ModuleName)))
					nextStepMsg := fmt.Sprintf("All %d phases of %s are verified complete. Perform a final verification pass across the full project. If all requirements, tests, and deliverables are complete and verified, reply with 'DONE' and no further action tags. If any final adjustments or tests are needed, emit the necessary actions.", len(planSpec.Phases), planSpec.ModuleName)
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightCyan("omnicoder>")),
						theme.Cyan("[Auto-Next Step: Final Verification] Requesting final review from architect..."))
					currentPrompt = nextStepMsg
					continue
				}
				if testPassed && ((len(actions) == 0 && isDoneResponse(reply)) || (explicitBuildCmd != "" && len(fileActions) == 0) || isDoneResponse(reply)) {
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightGreen("[Task Complete]")),
						theme.Green(fmt.Sprintf("Architect verified all %d phases complete; final verification passed with zero pending actions.", len(planSpec.Phases))))
					break
				}
				if len(actions) == 0 || testPassed {
					currentPrompt = fmt.Sprintf("All %d phases of %s are verified complete. Final verification command passed with exit code 0. Please reply with 'DONE' and zero pending action tags to conclude.", len(planSpec.Phases), planSpec.ModuleName)
					continue
				}
			} else {
				activePhase := planSpec.DetermineActivePhase(ws)
				if activePhase != nil {
					if buildSess != nil {
						_ = buildSess.SetMilestone(ctx, activePhase.PhaseNumber-1)
					}
					complete, missing := planSpec.IsPhaseFilesPresent(ws, activePhase)
					nextStepMsg := buildPhaseProgressionPrompt(ws, planSpec, activePhase, complete, missing)
					var missingList []string
					for _, m := range missing {
						scopedM := m
						if planSpec != nil && planSpec.ProjectScope != "" && !strings.HasPrefix(m, planSpec.ProjectScope+"/") {
							scopedM = planSpec.ProjectScope + "/" + m
						}
						missingList = append(missingList, fmt.Sprintf("  - %s", scopedM))
					}

					// Premature Verification Guard:
					// If target files for the active phase are still missing, verification builds or claims of completion
					// cannot advance the milestone. Warn and instruct the architect to write the missing files.
					if len(missing) > 0 && ((explicitBuildCmd != "" && len(fileActions) == 0) || (isDoneResponse(reply) && len(actions) == 0)) {
						fmt.Fprintf(out, "  %s  %s\n\n",
							theme.Bold(theme.BrightYellow("[Phase Incomplete Guard]")),
							theme.Yellow(fmt.Sprintf("Phase %d verification blocked: %d required target file(s) are not yet present on disk:\n%s",
								activePhase.PhaseNumber, len(missing), strings.Join(missingList, "\n"))))
						nextStepMsg = fmt.Sprintf("Phase %d (%s) verification is blocked: %d required target file(s) have NOT been created on disk:\n%s\nYou executed %q or reported completion, but you cannot verify or advance Phase %d until all required files are written.\nPlease implement the missing file(s) now using [ACTION:WRITE path=\"...\"] tags.\n\n",
							activePhase.PhaseNumber, activePhase.Name, len(missing), strings.Join(missingList, "\n"), explicitBuildCmd, activePhase.PhaseNumber) + nextStepMsg
					}

					if !complete && len(missing) < len(activePhase.TargetFiles) {
						fmt.Fprintf(out, "%s %s\n\n",
							theme.Bold(theme.BrightCyan("omnicoder>")),
							theme.Cyan(fmt.Sprintf("[Auto-Next Step] Phase %d: %s (%d files remaining)", activePhase.PhaseNumber, activePhase.Name, len(missing))))
					} else {
						fmt.Fprintf(out, "%s %s\n\n",
							theme.Bold(theme.BrightCyan("omnicoder>")),
							theme.Cyan(fmt.Sprintf("[Auto-Next Step] Phase %d: %s", activePhase.PhaseNumber, activePhase.Name)))
					}

					// Check if progress was made in this turn (new files written)
					hasNewFiles := false
					for f := range writtenFilesInTurn {
						if !lastWrittenFiles[f] {
							hasNewFiles = true
							break
						}
					}

					gap := fmt.Sprintf("%d:%v", activePhase.PhaseNumber, missing)
					if gap != lastGap || hasNewFiles {
						stalls = 0
						lastGap = gap
					} else {
						stalls++
					}
					lastWrittenFiles = writtenFilesInTurn

					// Autonomous Self-Reflection & Resolution Engine:
					// OmniCoder is a fully autonomous coder: it talks to itself to reason through issues,
					// scaffolds standard module manifests, and converges iteratively without giving up.
					if stalls >= 2 && len(missing) > 0 {
						// Autonomous Scaffolding for missing go.mod:
						scaffoldedMod := false
						for _, m := range missing {
							if filepath.Base(m) == "go.mod" {
								scopedM := m
								if planSpec != nil && planSpec.ProjectScope != "" && !strings.HasPrefix(m, planSpec.ProjectScope+"/") {
									scopedM = planSpec.ProjectScope + "/" + m
								}
								fullPath := filepath.Join(ws, filepath.FromSlash(scopedM))
								if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
									_ = os.MkdirAll(filepath.Dir(fullPath), 0755)
									modName := "app"
									if planSpec != nil && planSpec.ModuleName != "" {
										modName = planSpec.ModuleName
									}
									goModPayload := fmt.Sprintf("module %s\n\ngo 1.21\n", modName)
									if writeErr := os.WriteFile(fullPath, []byte(goModPayload), 0644); writeErr == nil {
										fmt.Fprintf(out, "%s %s\n\n",
											theme.Bold(theme.BrightGreen("[Autonomous Scaffolding]")),
											theme.Green(fmt.Sprintf("OmniCoder autonomously synthesized missing %s with module %s", scopedM, modName)))
										writtenFilesInTurn[scopedM] = true
										scaffoldedMod = true
									}
								}
							}
						}
						if scaffoldedMod {
							stalls = 0
							complete, missing = planSpec.IsPhaseFilesPresent(ws, activePhase)
							if complete {
								currentPrompt = fmt.Sprintf("Phase %d target files are now present on disk. Execute the phase verification command: [ACTION:BUILD task=%q]", activePhase.PhaseNumber, activePhase.VerificationCmd)
								continue
							}
						}

						// Autonomous Inner Monologue / Reasoning turn:
						fmt.Fprintf(out, "%s %s\n\n",
							theme.Bold(theme.BrightMagenta("[Autonomous Self-Reflection]")),
							theme.Magenta(fmt.Sprintf("OmniCoder reasoning through missing file gap (%d files remaining: %v)...", len(missing), missing)))

						targetMissing := missing[0]
						scopedTarget := targetMissing
						if planSpec != nil && planSpec.ProjectScope != "" && !strings.HasPrefix(targetMissing, planSpec.ProjectScope+"/") {
							scopedTarget = planSpec.ProjectScope + "/" + targetMissing
						}

						var detailedSpec string
						if activePhase.FileSpecs != nil {
							detailedSpec = activePhase.FileSpecs[targetMissing]
						}
						if detailedSpec == "" {
							detailedSpec = activePhase.SpecSection
						}

						currentPrompt = fmt.Sprintf(`=== OMNICODER AUTONOMOUS REASONING & GAP CLOSURE ===
You are OmniCoder, a fully autonomous software engineering agent.
Phase %d (%s) is paused because required target file %q is NOT present on disk.

Reason through this issue:
1. Explain why %s was not written on disk (e.g. action extraction payload was empty, skipped, or misnamed).
2. Review the file specification:
%s
3. Write the complete, production-grade implementation for %q NOW using:
[ACTION:WRITE path=%q]
`+"```go"+`
// complete, fully working implementation with zero stubs
`+"```"+`
CRITICAL INSTRUCTION: You MUST emit the implementation of %q now. Do NOT emit build commands. Do NOT say DONE. Write the file immediately.`,
							activePhase.PhaseNumber, activePhase.Name, scopedTarget, scopedTarget, detailedSpec, scopedTarget, scopedTarget, scopedTarget)

						if stalls >= 4 {
							stalls = 1 // Reset to 1 to allow continuous convergence without terminating
						}
						continue
					}

					currentPrompt = nextStepMsg
					continue
				}
			}
		}

		// Case C: ActionBuild intent from assistant
		// All ActionBuild commands have already been executed and verified in Step 4.
		// If a build failed, Step 5 (Case A) handled self-healing.
		// If a build succeeded and an active plan is in progress, Case B handled advancing phases.
		// If no plan is active, do not re-prompt with the raw shell command; proceed to Case E or finish turn.
		var buildTask string
		for _, action := range actions {
			if action.Type == ActionBuild && action.Task() != "" {
				buildTask = action.Task()
				break
			}
		}
		if buildTask != "" && !looksLikeShellCommand(buildTask) {
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.Yellow("[Auto-Next Step Guard]")),
				theme.Yellow(fmt.Sprintf("Rejected non-shell build task: %q", buildTask)))
			currentPrompt = fmt.Sprintf("You specified [ACTION:BUILD task=%q], but %q is descriptive prose. Please provide an exact executable shell command (e.g. [ACTION:BUILD task=\"go test ./...\"]).", buildTask, buildTask)
			continue
		}

		// Case C.0: deliver real probe output back to the architect on data-bug tasks so the loop
		// continues from evidence instead of ending after the probe.
		if probeFeedback != "" && len(fileActions) == 0 && probeFeedbackTurns < 4 {
			probeFeedbackTurns++
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightCyan("omnicoder>")),
				theme.Cyan("[Auto-Next Step: Evidence Evaluation] Returning runtime probe output to architect..."))
			currentPrompt = "Runtime evidence from your probe (real output, not simulated):\n\n```\n" + probeFeedback + "\n```\n\nState the concrete root cause using ONLY this output and the code you read. If the probe only gave counts, run a further read-only probe next (PRAGMA table_info on the table, the ledger rows, and the real exception from the failing insert or scraper) before editing. Never seed or invent data and never swallow exceptions. Making the row count match the ledger/vault totals is NOT a fix: find why the real harvested records failed to persist." + actionTagSyntaxReminder
			probeFeedback = ""
			continue
		}
		probeFeedback = ""

		// Case C.1: ActionRead intents from assistant (read-only file inspection).
		// Read the requested files and embed their numbered contents directly in the next prompt so the
		// architect actually sees the source, then continue the investigation loop.
		var readActions []*ActionIntent
		var searchActions []*ActionIntent
		for _, action := range actions {
			if action.Type == ActionRead && action.Path() != "" {
				readActions = append(readActions, action)
			} else if action.Type == ActionSearch && action.Query() != "" {
				searchActions = append(searchActions, action)
			}
		}
		if (len(readActions) > 0 || len(searchActions) > 0) && len(fileActions) == 0 && buildTask == "" {
			if readTurnCount < readActionMaxTurns {
				readTurnCount++
				var rb strings.Builder
				for i, sa := range searchActions {
					if i >= 2 {
						break
					}
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightYellow("[Auto-Engine Detected]")),
						theme.Yellow(fmt.Sprintf("ICX Search: %s", sa.Query())))
					rb.WriteString(buildSess.SearchContext(ctx, sa.Query(), searchResultLimit))
					rb.WriteString("\n")
				}
				for i, ra := range readActions {
					if i >= 4 {
						break
					}
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightYellow("[Auto-Engine Detected]")),
						theme.Yellow(fmt.Sprintf("Read: %s", ra.Path())))
					rb.WriteString(readFileForArchitect(ws, session.ActiveProject(), ra))
					appendEvidenceWarnings(&rb, ra)
					ingestReadFileIntoICX(ctx, session, ws, ra)
					if k := readKey(ws, session.ActiveProject(), ra); k != "" {
						readPathsSeen[k] = true
					}
					rb.WriteString("\n")
				}
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightCyan("omnicoder>")),
					theme.Cyan(fmt.Sprintf("[Auto-Next Step: Evidence Evaluation] Returning %d search result(s) and %d file excerpt(s) to architect...", min(len(searchActions), 2), min(len(readActions), 4))))
				currentPrompt = "The evidence you requested via [ACTION:SEARCH] / [ACTION:READ] is below (search hits come from ICX memory; file excerpts are verified from disk). Use it to identify the concrete root cause, citing file and line numbers, then continue with the fix or further requests if you still lack evidence." + actionTagSyntaxReminder + "\n\n" + rb.String()
				continue
			} else {
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightYellow("[Exploration Limit]")),
					theme.Yellow(fmt.Sprintf("Consecutive read limit reached (%d turns). Prompting architect to proceed with implementation...", readActionMaxTurns)))
				currentPrompt = fmt.Sprintf("You have reached the consecutive read limit (%d turns). Please synthesize your findings and proceed directly with applying the necessary fixes or refactorings using [ACTION:PATCH path=\"...\"] or [ACTION:WRITE path=\"...\"] tags.", readActionMaxTurns)
				continue
			}
		}

		// Case C.2: ActionMap intent from assistant (informational exploration)
		// If the architect emitted [ACTION:MAP] to explore symbols/code, do not exit the turn loop.
		// Feed the map observation back to the architect so it can provide its evaluation, recommendations, or implementation!
		var mapAction *ActionIntent
		for _, action := range actions {
			if action.Type == ActionMap && action.Query() != "" {
				mapAction = action
				break
			}
		}
		if mapAction != nil && len(fileActions) == 0 && buildTask == "" && mapTurnCount < 2 {
			mapTurnCount++
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightCyan("omnicoder>")),
				theme.Cyan(fmt.Sprintf("[Auto-Next Step: Map Evaluation] Evaluating repository symbols for %q...", mapAction.Query())))
			currentPrompt = fmt.Sprintf("Repository map results for query %q have been synthesized and displayed above. Based on these verified workspace symbols and files, continue directly with your architectural review, assessment, or planned actions as requested by the user.", mapAction.Query())
			continue
		}

		// Case C.3: ActionHeal intent from assistant
		var healAction *ActionIntent
		for _, action := range actions {
			if action.Type == ActionHeal && action.Cmd() != "" {
				healAction = action
				break
			}
		}
		if healAction != nil && len(fileActions) == 0 && buildTask == "" {
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightCyan("omnicoder>")),
				theme.Cyan(fmt.Sprintf("[Auto-Next Step: Heal Evaluation] Evaluating self-healing results for %q...", healAction.Cmd())))
			currentPrompt = fmt.Sprintf("Compiler self-healing report for command %q has been executed. Review the results above and continue with next steps or final verification.", healAction.Cmd())
			continue
		}

		// Case C.4: Stalled-intent recovery. The architect announced a next step ("Let's read
		// `utils/sync.py` using [ACTION:READ].") but emitted no parseable tag. Without this the
		// loop falls through to `break` and the user has to type "continue" by hand.
		if len(actions) == 0 && stalledIntentRetries < stalledIntentMaxRetries {
			if si, stalled := detectStalledIntent(reply); stalled {
				stalledIntentRetries++
				var recovered []*ActionIntent
				if si.IsRead() && readTurnCount < readActionMaxTurns {
					recovered = extractIntendedReadPaths(ws, session.ActiveProject(), reply, readPathsSeen)
				}
				if len(recovered) > 0 {
					readTurnCount++
					var rb strings.Builder
					for _, ra := range recovered {
						fmt.Fprintf(out, "%s %s\n",
							theme.Bold(theme.BrightYellow("[Stalled-Intent Recovery]")),
							theme.Yellow(fmt.Sprintf("Architect announced a read without a tag; reading %s", ra.Path())))
						rb.WriteString(readFileForArchitect(ws, session.ActiveProject(), ra))
						appendEvidenceWarnings(&rb, ra)
						ingestReadFileIntoICX(ctx, session, ws, ra)
						if k := readKey(ws, session.ActiveProject(), ra); k != "" {
							readPathsSeen[k] = true
						}
						rb.WriteString("\n")
					}
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightCyan("omnicoder>")),
						theme.Cyan(fmt.Sprintf("[Auto-Next Step: Evidence Evaluation] Returning %d recovered file excerpt(s) to architect...", len(recovered))))
					currentPrompt = "You announced reading these files but emitted no [ACTION:READ] tag; the engine read them for you. Excerpts are verified from disk. Identify the concrete root cause citing file and line numbers, then continue with the fix." + actionTagSyntaxReminder + "\n\n" + rb.String()
					continue
				}
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightYellow("[Stalled-Intent Recovery]")),
					theme.Yellow(fmt.Sprintf("Architect announced %q but emitted no executable tag. Re-prompting (%d/%d)...", si.Verb, stalledIntentRetries, stalledIntentMaxRetries)))
				currentPrompt = stalledIntentReprompt(si)
				continue
			}
		}

		// Case C.5: Rejected-edit accountability. A guard rejected an edit and the file is still
		// byte-identical, yet the architect is finishing (often claiming "updated" / DONE).
		if len(actions) == 0 && rejectedEditPrompts < 2 {
			if pending := unresolvedRejectedEdits(ws, rejectedEdits); len(pending) > 0 {
				rejectedEditPrompts++
				fmt.Fprintf(out, "%s %s\n\n",
					theme.Bold(theme.BrightRed("[Rejected Edit Guard]")),
					theme.Red(fmt.Sprintf("Architect is finishing but %d rejected edit(s) never reached disk: %s. Demanding a compliant fix or an honest status (%d/2)...", len(pending), strings.Join(pending, ", "), rejectedEditPrompts)))
				currentPrompt = rejectedEditReprompt(pending, rejectedEdits)
				continue
			}
		}

		// Case C.6: Data-bug completion gate. After an investigation, a bare "DONE" (or a cosmetic patch)
		// is not accepted: the architect must state the root cause, reconcile any probe-vs-symptom
		// mismatch, and acknowledge fabricated data the engine flagged in files it read.
		if len(actions) == 0 && isDataBugInvestigation(prompt) && (explicitBuildRan || len(readPathsSeen) > 0) {
			if missing := dataDoneGateMissing(reply, symptomMismatch, evFlags); len(missing) > 0 {
				if dataDoneGatePrompts < dataDoneGateMaxTries {
					dataDoneGatePrompts++
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightYellow("[Data-Bug Completion Gate]")),
						theme.Yellow(fmt.Sprintf("Final answer not accepted: %d requirement(s) unmet. Re-prompting (%d/%d)...", len(missing), dataDoneGatePrompts, dataDoneGateMaxTries)))
					currentPrompt = dataDoneGateReprompt(missing)
					continue
				}
				dataGateUnmet = missing
			} else {
				dataGateUnmet = nil
				dataGateSatisfied = true
			}
		}

		// Case D: If no more write actions were emitted and no errors, end turn
		if planSpec != nil && planSpec.IsComplete(ws) && !finalVerificationSent {
			finalVerificationSent = true
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightGreen("[Plan Completed]")),
				theme.Green(fmt.Sprintf("All %d phases of %s verified complete! Requesting final verification...", len(planSpec.Phases), planSpec.ModuleName)))
			nextStepMsg := fmt.Sprintf("All %d phases of %s are verified complete. Perform a final verification pass across the full project. If all requirements, tests, and deliverables are complete and verified, reply with 'DONE' and no further action tags. If any final adjustments or tests are needed, emit the necessary actions.", len(planSpec.Phases), planSpec.ModuleName)
			fmt.Fprintf(out, "%s %s\n\n",
				theme.Bold(theme.BrightCyan("omnicoder>")),
				theme.Cyan("[Auto-Next Step: Final Verification] Requesting final review from architect..."))
			currentPrompt = nextStepMsg
			continue
		}

		// Case E: Unplanned project autonomous completion gate
		if planSpec == nil && len(fileActions) > 0 && unplannedCompletionPasses < 5 {
			checkDir := lastTargetDir
			if checkDir == "" && canonicalDir != "" {
				checkDir = filepath.Join(ws, filepath.FromSlash(canonicalDir))
			}
			if checkDir == "" && session.ActiveProject() != "" {
				checkDir = filepath.Join(ws, filepath.FromSlash(session.ActiveProject()))
			}
			if checkDir != "" && checkDir != ws {
				if isComp, missingPillars := AssessProjectCompleteness(ws, checkDir); !isComp && len(missingPillars) > 0 {
					unplannedCompletionPasses++
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightYellow("[Autonomous Completion Gate]")),
						theme.Yellow(fmt.Sprintf("Project in %s is incomplete. Missing: %s", filepath.Base(checkDir), strings.Join(missingPillars, ", "))))
					nextStepMsg := fmt.Sprintf("Autonomous build of project in %s is partially complete, but missing required production deliverables:\n- %s\n\nPlease implement the missing deliverables using [ACTION:WRITE path=\"...\"] tags to complete the application.", filepath.Base(checkDir), strings.Join(missingPillars, "\n- "))
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightCyan("omnicoder>")),
						theme.Cyan("[Auto-Next Step: Autonomous Completion] Prompting architect to finish deliverables..."))
					currentPrompt = nextStepMsg
					continue
				}
			}
		}

		// Final safety check: if project is incomplete after max passes, flag as incomplete rather than pretending success
		if planSpec == nil && len(fileActions) > 0 {
			checkDir := lastTargetDir
			if checkDir == "" && canonicalDir != "" {
				checkDir = filepath.Join(ws, filepath.FromSlash(canonicalDir))
			}
			if checkDir == "" && session.ActiveProject() != "" {
				checkDir = filepath.Join(ws, filepath.FromSlash(session.ActiveProject()))
			}
			if checkDir != "" && checkDir != ws {
				if isComp, missingPillars := AssessProjectCompleteness(ws, checkDir); !isComp && len(missingPillars) > 0 {
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightRed("[Completion Warning]")),
						theme.Red(fmt.Sprintf("Project in %s finalized with missing deliverables: %s", filepath.Base(checkDir), strings.Join(missingPillars, ", "))))
					overallSuccess = false
				}
			}
		}

		// Case F: Autonomous Quality & Delivery Audit Gate
		if (len(writtenFilesInTurn) > 0 || len(lastWrittenFiles) > 0) && unplannedCompletionPasses < 8 {
			checkDir := lastTargetDir
			if checkDir == "" && canonicalDir != "" {
				checkDir = filepath.Join(ws, filepath.FromSlash(canonicalDir))
			}
			if checkDir == "" && session.ActiveProject() != "" {
				checkDir = filepath.Join(ws, filepath.FromSlash(session.ActiveProject()))
			}
			if checkDir != "" && checkDir != ws {
				if auditPassed, auditIssues := AuditProjectDelivery(ctx, ws, checkDir, session, out); !auditPassed && auditIssues != "" {
					unplannedCompletionPasses++
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightYellow("[Autonomous Quality Gate]")),
						theme.Yellow(fmt.Sprintf("Pre-flight delivery audit in %s detected issues requiring remediation before completion:\n%s", filepath.Base(checkDir), auditIssues)))
					nextStepMsg := fmt.Sprintf("Autonomous pre-flight quality audit of %s detected the following issue(s):\n%s\n\nPlease fix the implementation or tests so that all tests pass, entry points compile cleanly, and frontend event/link handlers are robust.", filepath.Base(checkDir), auditIssues)
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightCyan("omnicoder>")),
						theme.Cyan("[Auto-Next Step: Autonomous Hardening] Remediating delivery audit findings..."))
					currentPrompt = nextStepMsg
					continue
				} else if auditPassed && unplannedCompletionPasses > 0 {
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightGreen("[Autonomous Quality Gate]")),
						theme.Green(fmt.Sprintf("All test suites, entry points, and deliverables in %s verified 100%% clean.", filepath.Base(checkDir))))
				}
			}
		}

		// Final data-bug gate: the loop must never exit a data-bug investigation silently (e.g. when the
		// last turn was a test run and the evidence-turn budget is spent). Demand the final answer first.
		if len(actions) == 0 && isDataBugInvestigation(prompt) && (explicitBuildRan || len(readPathsSeen) > 0) && !dataGateSatisfied {
			if missing := dataDoneGateMissing(reply, symptomMismatch, evFlags); len(missing) > 0 {
				if dataDoneGatePrompts < dataDoneGateMaxTries {
					dataDoneGatePrompts++
					fmt.Fprintf(out, "%s %s\n\n",
						theme.Bold(theme.BrightYellow("[Data-Bug Completion Gate]")),
						theme.Yellow(fmt.Sprintf("Investigation ended without a final answer: %d requirement(s) unmet. Re-prompting (%d/%d)...", len(missing), dataDoneGatePrompts, dataDoneGateMaxTries)))
					currentPrompt = "Your last action completed. " + dataDoneGateReprompt(missing)
					continue
				}
				dataGateUnmet = missing
			} else {
				dataGateUnmet = nil
			}
		}

		break
	}

	if len(dataGateUnmet) > 0 {
		fmt.Fprintf(out, "%s %s\n\n",
			NewTerminalTheme().Bold(NewTerminalTheme().BrightRed("[Engine Status: ROOT CAUSE NOT ESTABLISHED]")),
			NewTerminalTheme().Red("The architect finished without: "+strings.Join(dataGateUnmet, "; ")+". Treat any 'fixed / DONE' claim above as unverified."))
	}
	if pending := unresolvedRejectedEdits(ws, rejectedEdits); len(pending) > 0 {
		fmt.Fprintf(out, "%s %s\n\n",
			NewTerminalTheme().Bold(NewTerminalTheme().BrightRed("[Engine Status: FIX NOT APPLIED]")),
			NewTerminalTheme().Red(fmt.Sprintf("Guard-rejected edit(s) were never applied: %s. Any 'updated / fixed / DONE' statement above about these files is FALSE.", strings.Join(pending, ", "))))
	}
	if rec, ok := session.LastBuildRecord(); ok && rec.ExitCode != 0 {
		fmt.Fprintf(out, "%s %s\n\n",
			NewTerminalTheme().Bold(NewTerminalTheme().BrightRed("[Engine Status: NOT VERIFIED]")),
			NewTerminalTheme().Red(fmt.Sprintf("The last verification command %q failed (exit %d). Any 'done / verified / ready' statement above is NOT confirmed by the engine.", rec.Command, rec.ExitCode)))
	} else if q := session.AllQuarantined(); len(q) > 0 {
		var activeQ []*QuarantinedPayload
		for _, qp := range q {
			absP := filepath.Join(ws, filepath.FromSlash(qp.Path))
			if st, err := os.Stat(absP); err == nil && st.Size() > 0 {
				session.RemoveQuarantined(qp.Path)
				continue
			}
			relP := filepath.ToSlash(qp.Path)
			parts := strings.Split(relP, "/")
			pDir := ""
			if len(parts) >= 2 && (parts[0] == "projects" || parts[0] == "apps") {
				pDir = filepath.Join(ws, parts[0], parts[1])
			}
			if pDir != "" {
				if isComp, missing := AssessProjectCompleteness(ws, pDir); isComp && len(missing) == 0 {
					session.RemoveQuarantined(qp.Path)
					continue
				}
			}
			activeQ = append(activeQ, qp)
		}
		if len(activeQ) > 0 {
			fmt.Fprintf(out, "%s %s\n\n",
				NewTerminalTheme().Bold(NewTerminalTheme().BrightRed("[Engine Status: INCOMPLETE]")),
				NewTerminalTheme().Red(fmt.Sprintf("%d deliverable(s) were rejected by the engine and are not on disk. The project is NOT complete.", len(activeQ))))
		}
	}

	// Record completed conversational turn to Vault under turn:conv:<timestamp>:<turnNum>
	if v := session.Vault(); v != nil {
		turnNum := session.Status().TurnCount
		if turnNum <= 0 {
			turnNum = 1
		}
		ts := time.Now().UTC().Format("20060102T150405Z")
		var touched []string
		for fp := range lastWrittenFiles {
			touched = append(touched, fp)
		}
		sort.Strings(touched)
		rec := ConversationalTurnRecord{
			SessionID:    "conv:" + ts,
			TurnNumber:   turnNum,
			Prompt:       prompt,
			Timestamp:    time.Now().UTC().Format(time.RFC3339),
			Success:      overallSuccess,
			Project:      session.ActiveProject(),
			StepsTaken:   len(session.RecentBuilds()),
			FilesTouched: touched,
		}
		if data, err := json.Marshal(rec); err == nil {
			key := fmt.Sprintf("turn:conv:%s:%04d", ts, turnNum)
			_ = v.Put([]byte(key), data)
		}
	}

	return nil
}

// AuditProjectDelivery runs comprehensive pre-flight verification across test suites,
// entry point syntax, dependencies, and frontend event/link wiring before completion.
func AuditProjectDelivery(ctx context.Context, ws, checkDir string, session *Session, out io.Writer) (bool, string) {
	theme := NewTerminalTheme()
	var issues []string

	// 1. Dependency Auto-Installation: ensure requirements.txt packages are installed
	reqPath := filepath.Join(checkDir, "requirements.txt")
	if _, err := os.Stat(reqPath); err == nil {
		reqData, _ := os.ReadFile(reqPath)
		if validReq, reqErr := devops.ValidateRequirementsTxt(string(reqData)); !validReq {
			issues = append(issues, fmt.Sprintf("- requirements.txt is invalid or contains prose: %v", reqErr))
		}
		for _, line := range strings.Split(string(reqData), "\n") {
			pkg := strings.TrimSpace(line)
			if pkg != "" && !strings.HasPrefix(pkg, "#") {
				pkgName := strings.Split(pkg, "=")[0]
				pkgName = strings.Split(pkgName, "<")[0]
				pkgName = strings.Split(pkgName, ">")[0]
				pkgName = strings.Split(pkgName, ";")[0]
				pkgName = strings.TrimSpace(pkgName)
				if pkgName != "" && !isPythonPackageInstalled(pkgName) {
					fmt.Fprintf(out, "  %s Installing missing requirement %s\n", theme.Cyan("[Audit Pre-flight]"), pkgName)
					_ = fixPythonModule(checkDir)
					break
				}
			}
		}
	}
	pkgJSONPath := filepath.Join(checkDir, "package.json")
	if _, err := os.Stat(pkgJSONPath); err == nil {
		nodeModsPath := filepath.Join(checkDir, "node_modules")
		if _, err := os.Stat(nodeModsPath); err != nil {
			fmt.Fprintf(out, "  %s Installing missing node_modules\n", theme.Cyan("[Audit Pre-flight]"))
			_ = fixNodeModule(checkDir)
		}
	}
	goModPath := filepath.Join(checkDir, "go.mod")
	if _, err := os.Stat(goModPath); err == nil {
		goSumPath := filepath.Join(checkDir, "go.sum")
		if _, err := os.Stat(goSumPath); err != nil {
			_ = fixGoModule(checkDir)
		}
	}

	// 2. Discover and Run Test Suite
	// Python: test_*.py or *_test.py
	var pyTestFiles []string
	_ = filepath.Walk(checkDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			base := filepath.Base(p)
			if (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")) && strings.HasSuffix(base, ".py") {
				rel, _ := filepath.Rel(checkDir, p)
				pyTestFiles = append(pyTestFiles, filepath.ToSlash(rel))
			}
		}
		return nil
	})

	if len(pyTestFiles) > 0 {
		for _, tf := range pyTestFiles {
			absTf := filepath.Join(checkDir, filepath.FromSlash(tf))
			isPytest := false
			if data, rErr := os.ReadFile(absTf); rErr == nil {
				s := string(data)
				if strings.Contains(s, "import pytest") || strings.Contains(s, "from pytest") {
					isPytest = true
				}
			}
			testCmd := fmt.Sprintf("python -m unittest %s", tf)
			if isPytest {
				testCmd = fmt.Sprintf("pytest %s", tf)
			}
			pyEnv := []string{fmt.Sprintf("PYTHONPATH=%s%c%s", checkDir, os.PathListSeparator, os.Getenv("PYTHONPATH"))}
			rep, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
				Command:        testCmd,
				WorkspaceDir:   checkDir,
				TimeoutSeconds: 60,
				ExtraEnv:       pyEnv,
			})
			if rep != nil && isPytest && !rep.Success && isPytestPluginCrash(testCmd, rep.CleanedStderr+rep.RawStderr+rep.RawStdout) {
				fmt.Fprintf(out, "  %s pytest plugin crash is environmental; rerunning with plugin autoload disabled\n", theme.Cyan("[Env Auto-Recover]"))
				pyEnv = withEnv(pyEnv, "PYTEST_DISABLE_PLUGIN_AUTOLOAD", "1")
				rep, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command: testCmd, WorkspaceDir: checkDir, TimeoutSeconds: 60, ExtraEnv: pyEnv,
				})
			}
			if err != nil || (rep != nil && (!rep.Success || rep.ExitCode != 0 || isZeroTestsPass(testCmd, rep.RawStdout, rep.RawStderr))) {
				diag := ""
				if rep != nil {
					if isZeroTestsPass(testCmd, rep.RawStdout, rep.RawStderr) {
						diag = fmt.Sprintf("Zero tests were executed (test discovery or test collection found 0 tests from %s). Ensure test functions start with test_ and test classes inherit from unittest.TestCase.", tf)
					} else {
						diag = rep.CleanedStderr
						if diag == "" {
							diag = rep.RawStderr
						}
						if diag == "" {
							diag = rep.RawStdout
						}
					}
				}
				issues = append(issues, fmt.Sprintf("- Test file %s failed execution:\n```\n%s\n```", tf, strings.TrimSpace(diag)))
			}
		}
	}

	// Go: go.mod
	if _, err := os.Stat(goModPath); err == nil {
		rep, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command:        "go test ./...",
			WorkspaceDir:   checkDir,
			TimeoutSeconds: 60,
		})
		if err != nil || (rep != nil && (!rep.Success || rep.ExitCode != 0 || isZeroTestsPass("go test ./...", rep.RawStdout, rep.RawStderr))) {
			diag := ""
			if rep != nil {
				if isZeroTestsPass("go test ./...", rep.RawStdout, rep.RawStderr) {
					diag = "Zero Go tests were executed ([no test files]). Ensure *_test.go files exist and define tests."
				} else {
					diag = rep.CleanedStderr
					if diag == "" {
						diag = rep.RawStderr
					}
				}
			}
			issues = append(issues, fmt.Sprintf("- Go unit tests failed:\n```\n%s\n```", strings.TrimSpace(diag)))
		}
	}

	// Rust: Cargo.toml
	cargoPath := filepath.Join(checkDir, "Cargo.toml")
	if _, err := os.Stat(cargoPath); err == nil {
		if _, lookErr := exec.LookPath("cargo"); lookErr == nil {
			rep, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
				Command:        "cargo test",
				WorkspaceDir:   checkDir,
				TimeoutSeconds: 120,
			})
			if err != nil || (rep != nil && (!rep.Success || rep.ExitCode != 0)) {
				diag := ""
				if rep != nil {
					diag = rep.CleanedStderr
					if diag == "" {
						diag = rep.RawStderr
					}
				}
				issues = append(issues, fmt.Sprintf("- Cargo tests failed:\n```\n%s\n```", strings.TrimSpace(diag)))
			}
		}
	}

	// Node: package.json
	if _, err := os.Stat(pkgJSONPath); err == nil {
		if data, rErr := os.ReadFile(pkgJSONPath); rErr == nil {
			var pkg struct {
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(data, &pkg) == nil && pkg.Scripts != nil {
				if tCmd, ok := pkg.Scripts["test"]; ok && tCmd != "" && !strings.Contains(tCmd, "no test specified") {
					rep, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
						Command:        "npm test",
						WorkspaceDir:   checkDir,
						TimeoutSeconds: 60,
					})
					if err != nil || (rep != nil && (!rep.Success || rep.ExitCode != 0)) {
						diag := ""
						if rep != nil {
							diag = rep.CleanedStderr + rep.RawStderr
						}
						issues = append(issues, fmt.Sprintf("- npm test failed:\n```\n%s\n```", strings.TrimSpace(diag)))
					}
				}
			}
		}
	}

	// 3. Universal entry point compilation and launch smoke probe
	if ran, ok, smokeDiag := ProbeAllProjectEntries(ctx, checkDir, nil); ran && !ok {
		issues = append(issues, "- "+smokeDiag)
	}

	// 4. Frontend & Links Static Audit
	_ = filepath.Walk(checkDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(p))
			if ext == ".html" || ext == ".htm" {
				data, readErr := os.ReadFile(p)
				if readErr == nil {
					htmlContent := string(data)
					rel, _ := filepath.Rel(checkDir, p)
					htmlDir := filepath.Dir(p)

					// Check dummy / broken placeholder URLs
					if strings.Contains(htmlContent, "example.com/placeholder") || strings.Contains(htmlContent, "1234.5678") {
						issues = append(issues, fmt.Sprintf("- File %s contains invalid dummy/placeholder URL ('example.com/placeholder'). Use valid target URLs.", rel))
					}
					// Check local script src dependencies exist on disk
					scriptSrcRe := regexp.MustCompile(`<script\s+[^>]*src=["']([^"']+)["']`)
					for _, m := range scriptSrcRe.FindAllStringSubmatch(htmlContent, -1) {
						if len(m) > 1 {
							src := strings.TrimSpace(m[1])
							if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") &&
								!strings.HasPrefix(src, "//") && !strings.HasPrefix(src, "data:") {
								localTarget := filepath.Join(htmlDir, filepath.FromSlash(src))
								if _, sErr := os.Stat(localTarget); sErr != nil {
									issues = append(issues, fmt.Sprintf("- File %s references script %q, but %s does not exist on disk.", rel, src, src))
								}
							}
						}
					}
					// Check local stylesheet href dependencies exist on disk
					linkHrefRe := regexp.MustCompile(`<link\s+[^>]*href=["']([^"']+)["']`)
					for _, m := range linkHrefRe.FindAllStringSubmatch(htmlContent, -1) {
						if len(m) > 1 {
							href := strings.TrimSpace(m[1])
							if strings.Contains(m[0], "stylesheet") && !strings.HasPrefix(href, "http://") &&
								!strings.HasPrefix(href, "https://") && !strings.HasPrefix(href, "//") && !strings.HasPrefix(href, "data:") {
								localTarget := filepath.Join(htmlDir, filepath.FromSlash(href))
								if _, sErr := os.Stat(localTarget); sErr != nil {
									issues = append(issues, fmt.Sprintf("- File %s references stylesheet %q, but %s does not exist on disk.", rel, href, href))
								}
							}
						}
					}
					// Check onclick references existing function
					onclickRe := regexp.MustCompile(`onclick=["']([a-zA-Z0-9_]+)\(`)
					for _, m := range onclickRe.FindAllStringSubmatch(htmlContent, -1) {
						if len(m) > 1 {
							fnName := m[1]
							if fnName == "alert" || fnName == "confirm" || fnName == "prompt" || fnName == "window" || fnName == "console" {
								continue
							}
							if !strings.Contains(htmlContent, "function "+fnName) && !strings.Contains(htmlContent, fnName+" =") && !strings.Contains(htmlContent, fnName+"=") {
								issues = append(issues, fmt.Sprintf("- File %s has button onclick=%q, but function %q is not defined in any <script> tag.", rel, fnName, fnName))
							}
						}
					}
					// Check notification permission check
					if strings.Contains(htmlContent, "new Notification(") && !strings.Contains(htmlContent, "requestPermission") && !strings.Contains(htmlContent, "permission") {
						issues = append(issues, fmt.Sprintf("- File %s calls 'new Notification()' without requesting or verifying 'Notification.permission'. Ensure permission is verified first.", rel))
					}
				}
			}
		}
		return nil
	})

	if len(issues) > 0 {
		return false, strings.Join(issues, "\n")
	}
	return true, ""
}

func packageToPythonModule(pkg string) string {
	low := strings.ToLower(pkg)
	for mod, p := range pythonModuleToPackage {
		if strings.ToLower(p) == low || strings.ToLower(mod) == low {
			return mod
		}
	}
	return pkg
}

func isPythonPackageInstalled(pkgName string) bool {
	modName := packageToPythonModule(pkgName)
	cmd := exec.Command("python", "-c", fmt.Sprintf("import %s", modName))
	return cmd.Run() == nil
}

// executeMap triggers repository map synthesis and reports the result.
func executeMap(ctx context.Context, session *Session, budget int, query string, out io.Writer) (*devops.RepoMapResult, error) {
	theme := NewTerminalTheme()
	orch := session.Engine()
	if orch == nil {
		fmt.Fprintln(out, theme.Yellow("[Notice] Subterranean engine is not configured; /map skipped."))
		return nil, nil
	}
	if budget <= 0 {
		budget = 4000
	}

	if query != "" {
		fmt.Fprintf(out, "%s\n", theme.Cyan(fmt.Sprintf("Synthesizing repository map for query %q (token budget: %d)...", query, budget)))
	} else {
		fmt.Fprintf(out, "%s\n", theme.Cyan(fmt.Sprintf("Synthesizing repository map (token budget: %d)...", budget)))
	}
	res, err := orch.MapWithQuery(ctx, budget, query)
	if err != nil {
		fmt.Fprintf(out, "%s %v\n", theme.Red("Repository map synthesis failed:"), err)
		return nil, err
	}

	summary := FormatRepoMapSummary(res)
	fmt.Fprintln(out, theme.Green(summary))
	session.FeedExecutionSummary(summary)
	return res, nil
}

// executeHeal triggers the compiler self-healing loop for a command.
func executeHeal(ctx context.Context, session *Session, cmd string, out io.Writer) error {
	theme := NewTerminalTheme()
	orch := session.Engine()
	if orch == nil {
		fmt.Fprintln(out, theme.Yellow("[Notice] Subterranean engine is not configured; /heal skipped."))
		return nil
	}

	fmt.Fprintf(out, "%s\n", theme.Yellow(fmt.Sprintf("Initiating compiler self-healing for command: %s", cmd)))
	report, err := orch.Heal(ctx, cmd)
	if report != nil {
		summaryText := FormatReportSummary(report)
		fmt.Fprint(out, summaryText)
		session.FeedExecutionSummary(summaryText)
	}
	if err != nil {
		fmt.Fprintf(out, "%s %v\n", theme.Red("Heal loop encountered error:"), err)
		return err
	}
	return nil
}

// executeRefactor triggers cross-file symbol refactoring.
func executeRefactor(ctx context.Context, session *Session, symbol, newSig string, out io.Writer) error {
	theme := NewTerminalTheme()
	orch := session.Engine()
	if orch == nil {
		fmt.Fprintln(out, theme.Yellow("[Notice] Subterranean engine is not configured; /refactor skipped."))
		return nil
	}

	fmt.Fprintf(out, "%s\n", theme.Cyan(fmt.Sprintf("Refactoring symbol '%s' -> '%s' across workspace...", symbol, newSig)))
	report, err := orch.Refactor(ctx, symbol, newSig)
	if report != nil {
		summaryText := FormatReportSummary(report)
		fmt.Fprint(out, summaryText)
		session.FeedExecutionSummary(summaryText)
	}
	if err != nil {
		fmt.Fprintf(out, "%s %v\n", theme.Red("Refactoring encountered error:"), err)
		return err
	}
	return nil
}

// isZeroTestsPass checks whether a test command (Go, Python, Rust, Node, etc.) completed with exit 0
// but actually ran zero tests.
func isZeroTestsPass(cmd, stdout, stderr string) bool {
	lowerCmd := strings.ToLower(cmd)
	combined := stdout + "\n" + stderr
	lowerCombined := strings.ToLower(combined)

	// 1. Go test commands
	if strings.Contains(lowerCmd, "go test") {
		hasRealPass := strings.Contains(combined, "PASS") ||
			strings.Contains(combined, "ok  \t") ||
			strings.Contains(combined, "ok   ")
		hasNoTestFiles := strings.Contains(combined, "[no test files]") ||
			strings.Contains(combined, "no test files") ||
			strings.Contains(combined, "no Go files in")

		if hasNoTestFiles && !hasRealPass {
			return true
		}
		if strings.TrimSpace(combined) == "" {
			return true
		}
		return false
	}

	// 2. Python test commands (unittest, pytest)
	if strings.Contains(lowerCmd, "pytest") || strings.Contains(lowerCmd, "unittest") || strings.Contains(lowerCmd, "py.test") {
		// Detect pytest zero tests: "collected 0 items", "no tests ran"
		if strings.Contains(lowerCombined, "collected 0 items") ||
			strings.Contains(lowerCombined, "no tests ran in") ||
			strings.Contains(lowerCombined, "0 selected") ||
			(strings.Contains(lowerCombined, "0 passed") && !strings.Contains(lowerCombined, "1 passed") && !strings.Contains(lowerCombined, "2 passed")) {
			return true
		}
		// Detect unittest zero tests: "Ran 0 tests", "Ran 0 test"
		if strings.Contains(lowerCombined, "ran 0 tests") || strings.Contains(lowerCombined, "ran 0 test") {
			return true
		}
		if strings.TrimSpace(combined) == "" {
			return true
		}
	}

	// 3. Rust cargo test
	if strings.Contains(lowerCmd, "cargo test") {
		if strings.Contains(lowerCombined, "running 0 tests") && !strings.Contains(lowerCombined, "1 passed") {
			return true
		}
	}

	return false
}

// isStubEntryPoint inspects a source file to verify that it provides a substantive
// execution implementation rather than an empty placeholder or print-only stub.
func isStubEntryPoint(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	lines := strings.Split(string(data), "\n")
	var codeLines []string
	for _, l := range lines {
		tl := strings.TrimSpace(l)
		if tl == "" || strings.HasPrefix(tl, "#") || strings.HasPrefix(tl, "//") || strings.HasPrefix(tl, "/*") || strings.HasPrefix(tl, "*") {
			continue
		}
		codeLines = append(codeLines, tl)
	}
	if len(codeLines) < 4 {
		return true
	}
	content := string(data)
	if strings.Contains(content, "print(") && !strings.Contains(content, "import ") && len(codeLines) < 8 {
		return true
	}
	return false
}

// detectDeclaredEntryPoint inspects README.md to determine the documented application entry point.
func detectDeclaredEntryPoint(dir string) string {
	return DetectDeclaredEntryPoint(dir)
}

// AssessProjectCompleteness checks whether a generated project directory possesses all 5 pillars
// of a complete autonomous deliverable:
// 1. Module Scaffolding (go.mod, package.json, Cargo.toml, requirements.txt)
// 2. Core Entry Point (main.go, cmd/.../main.go, index.js, etc.)
// 3. Frontend UI / Static Assets (if web/HTTP project detected)
// 4. Test Suite (*_test.go, *.test.js, etc.)
// 5. Documentation (README.md > 50 bytes)
func AssessProjectCompleteness(workspaceDir, projectDir string) (bool, []string) {
	if projectDir == "" {
		return false, []string{"Project directory unspecified"}
	}
	absDir := projectDir
	if !filepath.IsAbs(absDir) {
		absDir = filepath.Join(workspaceDir, filepath.FromSlash(projectDir))
	}
	fi, err := os.Stat(absDir)
	if err != nil || !fi.IsDir() {
		return false, []string{"Project directory does not exist on disk"}
	}

	var missing []string

	hasGoFiles := false
	hasJsFiles := false
	hasPyFiles := false
	hasRsFiles := false
	hasCppFiles := false
	hasJavaFiles := false
	hasCsFiles := false
	hasTests := false
	hasEntryPoint := false
	hasWebRoutes := false
	hasHTML := false

	_ = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "target" || name == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".go") {
			hasGoFiles = true
			if strings.HasSuffix(name, "_test.go") {
				hasTests = true
			}
			if name == "main.go" || name == "server.go" || name == "app.go" || strings.Contains(filepath.ToSlash(path), "/cmd/") || strings.Contains(filepath.ToSlash(path), "/examples/") || strings.Contains(filepath.ToSlash(path), "/demo/") {
				if !isStubEntryPoint(path) {
					hasEntryPoint = true
				}
			} else if !hasEntryPoint {
				if data, rErr := os.ReadFile(path); rErr == nil {
					if strings.Contains(string(data), "func main()") && !isStubEntryPoint(path) {
						hasEntryPoint = true
					}
				}
			}
			// Check if web/http routes are present in Go code
			if !hasWebRoutes {
				if data, rErr := os.ReadFile(path); rErr == nil {
					s := string(data)
					if strings.Contains(s, "net/http") || strings.Contains(s, "gin-gonic/gin") ||
						strings.Contains(s, "http."+"ListenAndServe") || strings.Contains(s, "r.Run(") {
						hasWebRoutes = true
					}
				}
			}
		} else if strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts") {
			hasJsFiles = true
			if strings.Contains(name, ".test.") || strings.Contains(name, ".spec.") {
				hasTests = true
			}
			if (name == "index.js" || name == "index.ts" || name == "main.js" || name == "app.js" || name == "server.js") && !isStubEntryPoint(path) {
				hasEntryPoint = true
			}
			if !hasWebRoutes {
				if data, rErr := os.ReadFile(path); rErr == nil {
					s := string(data)
					if strings.Contains(s, "express()") || strings.Contains(s, "fastify()") || strings.Contains(s, "http.createServer") {
						hasWebRoutes = true
					}
				}
			}
		} else if strings.HasSuffix(name, ".py") {
			hasPyFiles = true
			if strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.py") {
				hasTests = true
			}
			if (name == "main.py" || name == "app.py" || name == "server.py") && !isStubEntryPoint(path) {
				hasEntryPoint = true
			}
			if !hasWebRoutes {
				if data, rErr := os.ReadFile(path); rErr == nil {
					s := string(data)
					if strings.Contains(s, "Flask(") || strings.Contains(s, "FastAPI(") ||
						strings.Contains(s, "@app.route(") || strings.Contains(s, "@app.get(") ||
						strings.Contains(s, "import flask") || strings.Contains(s, "import fastapi") {
						hasWebRoutes = true
					}
				}
			}
		} else if strings.HasSuffix(name, ".rs") {
			hasRsFiles = true
			if name == "main.rs" || name == "lib.rs" {
				hasEntryPoint = true
			}
			if strings.Contains(filepath.ToSlash(path), "/tests/") {
				hasTests = true
			} else if !hasTests {
				if data, rErr := os.ReadFile(path); rErr == nil {
					s := string(data)
					if strings.Contains(s, "#[test]") || strings.Contains(s, "#[cfg(test)]") {
						hasTests = true
					}
					if strings.Contains(s, "fn main()") {
						hasEntryPoint = true
					}
				}
			}
		} else if strings.HasSuffix(name, ".cpp") || strings.HasSuffix(name, ".c") || strings.HasSuffix(name, ".cc") || strings.HasSuffix(name, ".cxx") {
			hasCppFiles = true
			if name == "main.cpp" || name == "main.c" || name == "main.cc" {
				hasEntryPoint = true
			}
			if strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.cpp") || strings.Contains(filepath.ToSlash(path), "/test") {
				hasTests = true
			} else if !hasEntryPoint {
				if data, rErr := os.ReadFile(path); rErr == nil {
					if strings.Contains(string(data), "int main(") || strings.Contains(string(data), "void main(") {
						hasEntryPoint = true
					}
				}
			}
		} else if strings.HasSuffix(name, ".java") {
			hasJavaFiles = true
			if name == "main.java" || name == "application.java" || name == "app.java" {
				hasEntryPoint = true
			}
			if strings.HasSuffix(name, "test.java") || strings.HasSuffix(name, "tests.java") || strings.Contains(filepath.ToSlash(path), "/test/") {
				hasTests = true
			} else if !hasEntryPoint {
				if data, rErr := os.ReadFile(path); rErr == nil {
					if strings.Contains(string(data), "public static void main(") {
						hasEntryPoint = true
					}
				}
			}
		} else if strings.HasSuffix(name, ".cs") {
			hasCsFiles = true
			if name == "program.cs" || name == "startup.cs" {
				hasEntryPoint = true
			}
			if strings.HasSuffix(name, "test.cs") || strings.HasSuffix(name, "tests.cs") || strings.Contains(filepath.ToSlash(path), "/test") {
				hasTests = true
			} else if !hasEntryPoint {
				if data, rErr := os.ReadFile(path); rErr == nil {
					if strings.Contains(string(data), "static void Main(") || strings.Contains(string(data), "static async Task Main(") {
						hasEntryPoint = true
					}
				}
			}
		}
		if strings.HasSuffix(name, ".html") {
			hasHTML = true
		}
		return nil
	})

	hasRouteHandlers := false
	if hasWebRoutes {
		_ = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if data, rErr := os.ReadFile(path); rErr == nil {
				s := string(data)
				if strings.Contains(s, "@app.route") || strings.Contains(s, "@app.get") ||
					strings.Contains(s, "@app.post") || strings.Contains(s, "app.get(") ||
					strings.Contains(s, "app.post(") || strings.Contains(s, ".HandleFunc(") ||
					strings.Contains(s, ".Handle(") || strings.Contains(s, "http.Handle") ||
					strings.Contains(s, "mux.Handle") || strings.Contains(s, "r.GET(") || strings.Contains(s, "r.POST(") {
					hasRouteHandlers = true
					return filepath.SkipAll
				}
			}
			return nil
		})
	}

	// Pillar 1: Scaffolding
	if hasGoFiles {
		goModPath := filepath.Join(absDir, "go.mod")
		if st, err := os.Stat(goModPath); err != nil || st.Size() == 0 {
			missing = append(missing, "Module Manifest (go.mod)")
		}
	} else if hasRsFiles {
		cargoPath := filepath.Join(absDir, "Cargo.toml")
		if st, err := os.Stat(cargoPath); err != nil || st.Size() == 0 {
			missing = append(missing, "Cargo Manifest (Cargo.toml)")
		}
	} else if hasCppFiles {
		cmakePath := filepath.Join(absDir, "CMakeLists.txt")
		makePath := filepath.Join(absDir, "Makefile")
		_, errCmake := os.Stat(cmakePath)
		_, errMake := os.Stat(makePath)
		if errCmake != nil && errMake != nil {
			missing = append(missing, "Build Manifest (CMakeLists.txt or Makefile)")
		}
	} else if hasJavaFiles {
		pomPath := filepath.Join(absDir, "pom.xml")
		gradlePath := filepath.Join(absDir, "build.gradle")
		gradleKts := filepath.Join(absDir, "build.gradle.kts")
		_, errPom := os.Stat(pomPath)
		_, errGradle := os.Stat(gradlePath)
		_, errKts := os.Stat(gradleKts)
		if errPom != nil && errGradle != nil && errKts != nil {
			missing = append(missing, "Build Manifest (pom.xml or build.gradle)")
		}
	} else if hasCsFiles {
		var hasProj bool
		_ = filepath.Walk(absDir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi != nil && !fi.IsDir() {
				low := strings.ToLower(fi.Name())
				if strings.HasSuffix(low, ".csproj") || strings.HasSuffix(low, ".sln") {
					hasProj = true
					return filepath.SkipAll
				}
			}
			return nil
		})
		if !hasProj {
			missing = append(missing, "Project Manifest (*.csproj or *.sln)")
		}
	} else if hasJsFiles {
		isStaticBrowserApp := hasHTML && !hasGoFiles && !hasPyFiles && !hasWebRoutes
		if !isStaticBrowserApp {
			pkgJsonPath := filepath.Join(absDir, "package.json")
			if st, err := os.Stat(pkgJsonPath); err != nil || st.Size() == 0 {
				missing = append(missing, "Package Manifest (package.json)")
			}
		}
	} else if hasPyFiles {
		reqPath := filepath.Join(absDir, "requirements.txt")
		pyprojPath := filepath.Join(absDir, "pyproject.toml")
		_, errReq := os.Stat(reqPath)
		_, errPyproj := os.Stat(pyprojPath)
		if errReq != nil && errPyproj != nil {
			missing = append(missing, "Dependencies (requirements.txt or pyproject.toml)")
		}
	}

	// Pillar 2: Core Entry Point (Contract enforcement)
	declaredEntry := detectDeclaredEntryPoint(absDir)
	if declaredEntry != "" {
		entryPath := filepath.Join(absDir, declaredEntry)
		if _, err := os.Stat(entryPath); err != nil || isStubEntryPoint(entryPath) {
			hasEntryPoint = false
		} else {
			hasEntryPoint = true
		}
	}

	// Detect whether Go project is a library package (non-main package in root)
	isGoLibrary := false
	if hasGoFiles {
		rootEntries, rErr := os.ReadDir(absDir)
		if rErr == nil {
			var nonMainCount, mainCount int
			for _, re := range rootEntries {
				if !re.IsDir() && strings.HasSuffix(re.Name(), ".go") && !strings.HasSuffix(re.Name(), "_test.go") {
					data, dErr := os.ReadFile(filepath.Join(absDir, re.Name()))
					if dErr == nil {
						pkgName, pErr := devops.GetGoPackageName(string(data))
						if pErr == nil {
							if pkgName == "main" {
								mainCount++
							} else {
								nonMainCount++
							}
						}
					}
				}
			}
			if nonMainCount > 0 && mainCount == 0 {
				isGoLibrary = true
			}
		}
	}

	if !hasEntryPoint {
		if hasHTML && !hasGoFiles && !hasPyFiles && !hasRsFiles && !hasCppFiles && !hasJavaFiles {
			hasEntryPoint = true
		} else if isGoLibrary && hasTests {
			// A Go library package accompanied by unit tests satisfies the entry point contract
			hasEntryPoint = true
		} else {
			if declaredEntry != "" {
				missing = append(missing, fmt.Sprintf("Core Entry Point (%s as declared in README.md)", declaredEntry))
			} else if hasPyFiles {
				missing = append(missing, "Core Entry Point (app.py or main.py)")
			} else if hasGoFiles {
				if isGoLibrary {
					missing = append(missing, "Library Test Suite (*_test.go) or CLI Entry Point (cmd/.../main.go)")
				} else {
					missing = append(missing, "Core Entry Point (main.go)")
				}
			} else if hasJsFiles {
				missing = append(missing, "Core Entry Point (index.js or app.js)")
			} else if hasRsFiles {
				missing = append(missing, "Core Entry Point (main.rs)")
			} else {
				missing = append(missing, "Core Entry Point (main.go, app.py, index.js, main.rs, or main.cpp)")
			}
		}
	}

	// Pillar 3: Frontend UI / Static Delivery & Route Parity (if web server detected)
	if hasWebRoutes {
		if !hasHTML {
			missing = append(missing, "Frontend UI / Static HTML (index.html or static assets)")
		}
		if !hasRouteHandlers {
			missing = append(missing, "Backend Web Route Handlers (@app.route, @app.get, or HTTP handler endpoints)")
		}
	}

	// Pillar 4: Test Suite
	if !hasTests {
		if !hasGoFiles && !hasPyFiles && !hasRsFiles && !hasCppFiles && !hasJavaFiles && hasHTML {
			// Pure static HTML/browser applications without backend code
		} else {
			missing = append(missing, "Test Suite (*_test.go, test_*.py, cargo tests, or test suite files)")
		}
	}

	// Pillar 5: Documentation (README.md)
	readmePath := filepath.Join(absDir, "README.md")
	if st, err := os.Stat(readmePath); err != nil || st.Size() < 50 {
		missing = append(missing, "Documentation (README.md with setup & usage instructions)")
	}

	return len(missing) == 0, missing
}

var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeProjectName cleans a module path or string into a safe folder name.
func SanitizeProjectName(mod string) string {
	mod = strings.TrimSpace(mod)
	mod = strings.Trim(mod, "\"'`")
	cleanMod := filepath.ToSlash(mod)
	cleanMod = strings.TrimRight(cleanMod, "/")
	cleanMod = strings.TrimSuffix(cleanMod, ".git")

	// If module ends with a major version like /v2 or /v3, strip it to extract the project name
	parts := strings.Split(cleanMod, "/")
	if len(parts) > 1 {
		last := parts[len(parts)-1]
		if isMajorVersionSuffix(last) {
			cleanMod = strings.Join(parts[:len(parts)-1], "/")
		}
	}
	cleanMod = strings.TrimRight(cleanMod, "/")
	cleanMod = strings.TrimSuffix(cleanMod, ".git")

	base := filepath.Base(cleanMod)
	base = strings.TrimSpace(base)

	// Strip illegal filesystem characters for cross-platform safety
	var buf strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			buf.WriteRune(r)
		} else {
			buf.WriteRune('-')
		}
	}
	result := strings.Trim(buf.String(), "-.")
	if result == "" || result == "." || result == ".." || isMajorVersionSuffix(result) {
		return "app"
	}
	upper := strings.ToUpper(result)
	ext := filepath.Ext(upper)
	baseNoExt := strings.TrimSuffix(upper, ext)
	firstDotPart := strings.Split(upper, ".")[0]
	if windowsReservedNames[upper] || windowsReservedNames[baseNoExt] || windowsReservedNames[firstDotPart] {
		return "app"
	}
	return result
}

func isMajorVersionSuffix(s string) bool {
	if len(s) < 2 || (s[0] != 'v' && s[0] != 'V') {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ScopePath scopes a root-relative path into the designated project subfolder.
func ScopePath(scopedDir, relPath string) string {
	raw := strings.TrimSpace(relPath)
	if raw == "" {
		return ""
	}
	raw = strings.ReplaceAll(raw, "\\", "/")
	vol := filepath.VolumeName(raw)
	if vol != "" {
		raw = strings.TrimPrefix(raw, vol)
	}
	if len(raw) >= 2 && ((raw[0] >= 'a' && raw[0] <= 'z') || (raw[0] >= 'A' && raw[0] <= 'Z')) && raw[1] == ':' {
		raw = raw[2:]
	}

	clean := filepath.ToSlash(filepath.Clean(raw))
	for strings.HasPrefix(clean, "../") {
		clean = strings.TrimPrefix(clean, "../")
	}
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.TrimPrefix(clean, "..")
	clean = strings.TrimPrefix(clean, "/")
	for strings.HasPrefix(clean, "../") {
		clean = strings.TrimPrefix(clean, "../")
	}
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")

	if clean == "" || clean == "." || clean == ".." {
		return ""
	}

	cleanScoped := filepath.ToSlash(filepath.Clean(strings.TrimSpace(scopedDir)))
	cleanScoped = strings.TrimPrefix(cleanScoped, "./")
	cleanScoped = strings.TrimPrefix(cleanScoped, "/")
	if cleanScoped == "" || cleanScoped == "." {
		return clean
	}

	if clean == cleanScoped || strings.HasPrefix(clean, cleanScoped+"/") {
		return clean
	}

	// Check if already scoped under known project prefixes with the same project base
	projBase := filepath.Base(cleanScoped)
	if projBase != "" && projBase != "." {
		for _, prefix := range []string{"apps/", "projects/", "output/"} {
			if strings.HasPrefix(clean, prefix) {
				rest := strings.TrimPrefix(clean, prefix)
				parts := strings.Split(rest, "/")
				if len(parts) > 1 && parts[0] != "" {
					subFolder := parts[0]
					if strings.EqualFold(subFolder, projBase) {
						// Already scoped with exact project base under a known prefix (e.g. projects/my-app/main.go)
						return clean
					}
					subBase := stripVariantSuffix(subFolder)
					if strings.EqualFold(subBase, stripVariantSuffix(projBase)) {
						// Variant with suffix (e.g. my-app-clean): remap into cleanScoped
						return cleanScoped + "/" + strings.Join(parts[1:], "/")
					}
					// Explicitly scoped into a different project root! Never nest projects under projects!
					return clean
				}
			}
		}
	}

	joined := filepath.ToSlash(filepath.Join(cleanScoped, clean))
	if !strings.HasPrefix(joined, cleanScoped+"/") && joined != cleanScoped {
		return filepath.ToSlash(filepath.Join(cleanScoped, filepath.Base(clean)))
	}
	return joined
}

var knownVariantSuffixes = []string{
	"-clean", "-browser", "-modern", "-fullstack", "-frontend", "-backend",
	"-web", "-ui", "-cli", "-prototype", "-demo", "-impl", "-new", "-v1", "-v2", "-v3",
	"_clean", "_browser", "_modern", "_fullstack", "_frontend", "_backend",
	"_web", "_ui", "_cli", "_prototype", "_demo", "_impl", "_new", "_v1", "_v2", "_v3",
}

// stripVariantSuffix removes trailing variant suffixes (e.g. -clean, -browser, -modern)
// from a project name to produce a canonical base project identifier.
func stripVariantSuffix(name string) string {
	trimmed := strings.TrimSpace(name)
	for {
		changed := false
		for _, suf := range knownVariantSuffixes {
			if strings.HasSuffix(strings.ToLower(trimmed), suf) {
				candidate := trimmed[:len(trimmed)-len(suf)]
				candidate = strings.TrimRight(candidate, "-_")
				if len(candidate) > 0 && candidate != "projects" && candidate != "apps" && candidate != "output" {
					trimmed = candidate
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	return trimmed
}

// HarmonizeProjectActions inspects all write actions in a turn, detects any fragmented or variant
// project directory targets (e.g. projects/my-app-clean vs projects/my-app-browser),
// and unifies them under a single canonical project directory.
// It returns the canonical project directory (or empty string if none) and a map of remapped target paths.
func HarmonizeProjectActions(session *Session, writeActions []*ActionIntent) (string, map[*ActionIntent]string) {
	remapped := make(map[*ActionIntent]string)
	if len(writeActions) == 0 {
		if session != nil {
			return session.ActiveProject(), remapped
		}
		return "", remapped
	}

	// 1. Establish canonical directory
	canonicalDir := ""
	if session != nil && session.ActiveProject() != "" {
		if session.ActiveProject() == "." {
			return ".", remapped
		}
		cand := stripVariantSuffix(session.ActiveProject())
		candBase := filepath.Base(cand)
		// Check if at least one action targets cand or a variant of cand
		hasMatchingAction := false
		hasAnyExplicitProject := false
		for _, wa := range writeActions {
			p := strings.Trim(strings.TrimSpace(wa.Path()), "\"'`")
			cleanP := filepath.ToSlash(filepath.Clean(p))
			cleanP = strings.TrimPrefix(cleanP, "./")
			cleanP = strings.TrimPrefix(cleanP, "/")
			for _, prefix := range []string{"projects/", "apps/", "output/"} {
				if strings.HasPrefix(cleanP, prefix) {
					hasAnyExplicitProject = true
					rest := strings.TrimPrefix(cleanP, prefix)
					parts := strings.Split(rest, "/")
					if len(parts) > 1 && parts[0] != "" {
						if strings.EqualFold(stripVariantSuffix(parts[0]), candBase) {
							hasMatchingAction = true
							break
						}
					}
				}
			}
			if hasMatchingAction {
				break
			}
		}
		if hasMatchingAction || !hasAnyExplicitProject {
			canonicalDir = cand
		}
	}

	// If no canonical directory from session, deduce from write actions
	if canonicalDir == "" {
		for _, wa := range writeActions {
			p := strings.Trim(strings.TrimSpace(wa.Path()), "\"'`")
			cleanP := filepath.ToSlash(filepath.Clean(p))
			cleanP = strings.TrimPrefix(cleanP, "./")
			cleanP = strings.TrimPrefix(cleanP, "/")

			for _, prefix := range []string{"projects/", "apps/", "output/"} {
				if strings.HasPrefix(cleanP, prefix) {
					rest := strings.TrimPrefix(cleanP, prefix)
					parts := strings.Split(rest, "/")
					if len(parts) > 1 && parts[0] != "" {
						base := stripVariantSuffix(parts[0])
						if base != "" && base != "app" {
							canonicalDir = prefix + base
							break
						}
					}
				}
			}
			if canonicalDir != "" {
				break
			}
		}
	}

	// If still empty, check if any action has a multi-segment path where parts[0] is a project folder
	if canonicalDir == "" {
		for _, wa := range writeActions {
			p := strings.Trim(strings.TrimSpace(wa.Path()), "\"'`")
			cleanP := filepath.ToSlash(filepath.Clean(p))
			cleanP = strings.TrimPrefix(cleanP, "./")
			cleanP = strings.TrimPrefix(cleanP, "/")
			parts := strings.Split(cleanP, "/")
			if len(parts) > 1 && parts[0] != "" && parts[0] != "pkg" && parts[0] != "cmd" && parts[0] != "internal" {
				base := stripVariantSuffix(parts[0])
				if base != parts[0] && base != "" {
					canonicalDir = "projects/" + base
					break
				}
			}
		}
	}

	if canonicalDir == "" {
		return "", nil
	}

	if session != nil {
		session.SetActiveProject(canonicalDir)
	}

	// 2. Remap each write action to target canonicalDir
	for _, wa := range writeActions {
		p := strings.Trim(strings.TrimSpace(wa.Path()), "\"'`")
		cleanP := filepath.ToSlash(filepath.Clean(p))
		cleanP = strings.TrimPrefix(cleanP, "./")
		cleanP = strings.TrimPrefix(cleanP, "/")

		// If already in canonicalDir
		if cleanP == canonicalDir || strings.HasPrefix(cleanP, canonicalDir+"/") {
			remapped[wa] = cleanP
			continue
		}

		// Check if it belongs to a variant directory under projects/, apps/, output/
		remappedSub := false
		for _, prefix := range []string{"projects/", "apps/", "output/"} {
			if strings.HasPrefix(cleanP, prefix) {
				rest := strings.TrimPrefix(cleanP, prefix)
				parts := strings.Split(rest, "/")
				if len(parts) > 1 {
					subFolder := parts[0]
					canonicalSub := stripVariantSuffix(subFolder)
					canonicalBase := filepath.Base(canonicalDir)
					if strings.EqualFold(canonicalSub, canonicalBase) {
						relPathInside := strings.Join(parts[1:], "/")
						remapped[wa] = canonicalDir + "/" + relPathInside
						remappedSub = true
						break
					} else {
						// Targeted at a different project; preserve cleanP without mangling!
						remapped[wa] = cleanP
						remappedSub = true
						break
					}
				}
			}
		}
		if remappedSub {
			continue
		}

		// Check if it's a naked variant prefix e.g. my-app-clean/main.go
		parts := strings.Split(cleanP, "/")
		if len(parts) > 1 {
			subFolder := parts[0]
			canonicalSub := stripVariantSuffix(subFolder)
			canonicalBase := filepath.Base(canonicalDir)
			if strings.EqualFold(canonicalSub, canonicalBase) {
				relPathInside := strings.Join(parts[1:], "/")
				remapped[wa] = canonicalDir + "/" + relPathInside
				continue
			}
		}

		// Check if it's a project file e.g. main.go, go.mod, index.html, cmd/..., pkg/... that needs scoping into canonicalDir.
		// Core host engine paths (e.g. cmd/omnicoder/, pkg/engine/, pkg/devops/, pkg/llm/, pkg/ui/, pkg/store/, pkg/mcp/)
		// are preserved as host files; all other project subpaths (cmd/server, cmd/my-app, pkg/api, pkg/icx, web/..., etc.)
		// are scoped into canonicalDir.
		isCoreHost := false
		if strings.HasPrefix(cleanP, "cmd/omnicoder/") ||
			strings.HasPrefix(cleanP, "pkg/engine/") ||
			strings.HasPrefix(cleanP, "pkg/devops/") ||
			strings.HasPrefix(cleanP, "pkg/llm/") ||
			strings.HasPrefix(cleanP, "pkg/ui/") ||
			strings.HasPrefix(cleanP, "pkg/store/") ||
			strings.HasPrefix(cleanP, "pkg/mcp/") {
			isCoreHost = true
		}
		if !isCoreHost {
			remapped[wa] = ScopePath(canonicalDir, cleanP)
			continue
		}

		remapped[wa] = cleanP
	}

	return canonicalDir, remapped
}

// executeWrite persists file content to disk inside the workspace, prints status badges,
// and feeds the attestation summary into conversational context.
func executeWrite(session *Session, targetPath string, content string, out io.Writer) error {
	theme := NewTerminalTheme()
	if session == nil {
		return errors.New("ui: session cannot be nil")
	}

	cleanPath := strings.TrimSpace(targetPath)
	cleanPath = strings.Trim(cleanPath, "\"'")
	cleanPath = strings.TrimSpace(cleanPath)
	if cleanPath == "" {
		fmt.Fprintf(out, "  %s: target path cannot be empty\n", theme.Bold(theme.BrightRed("[Write Error]")))
		return errors.New("ui: target path cannot be empty")
	}

	writtenPath, shaSeal, err := session.WriteFile(cleanPath, []byte(content))
	if err != nil {
		fmt.Fprintf(out, "  %s %s: %v\n", theme.Bold(theme.BrightRed("[Write Error]")), cleanPath, err)
		return err
	}

	if bs := session.BuildSession(); bs != nil {
		_, _ = bs.CrystallizeFile(context.Background(), cleanPath, content, "")
	}

	fmt.Fprintf(out, "\n  %s %s\n", theme.Bold(theme.BrightGreen("[File Created]")), theme.Green(writtenPath))
	fmt.Fprintf(out, "  %s  %s  %s\n\n",
		theme.Badge("Size", fmt.Sprintf("%d bytes", len(content))),
		theme.Badge("SHA-256", shaSeal),
		theme.Badge("ICX Surface", "0D-4D Synchronized"))

	summary := fmt.Sprintf("File Created: %s\nSize: %d bytes\nSHA-256 Seal: %s\nICX Memory: 0D-4D Surfaces Synchronized\n", writtenPath, len(content), shaSeal)
	session.FeedExecutionSummary(summary)
	return nil
}

// executePatch applies surgical search/replace edit blocks using devops.ApplyEditBlocks,
// records updated files in the session, and attests CAS Merkle provenance.
func executePatch(session *Session, targetPath string, content string, out io.Writer) error {
	theme := NewTerminalTheme()
	if session == nil {
		return errors.New("ui: session cannot be nil")
	}

	cleanPath := strings.TrimSpace(targetPath)
	cleanPath = strings.Trim(cleanPath, "\"'`")
	if cleanPath == "" {
		fmt.Fprintf(out, "  %s: target path cannot be empty\n", theme.Bold(theme.BrightRed("[Patch Error]")))
		return errors.New("ui: target path cannot be empty")
	}

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:       session.WorkspaceDir(),
		EditBlocksText: content,
		TargetFile:     cleanPath,
		FuzzyMatch:     true,
	})
	if err != nil {
		fmt.Fprintf(out, "\n  %s %s: %v\n", theme.Bold(theme.BrightRed("[Patch Failed]")), cleanPath, err)
		summary := fmt.Sprintf("Surgical Patch Failed on %s: %v\n", cleanPath, err)
		session.FeedExecutionSummary(summary)
		return err
	}

	if res != nil && res.Applied {
		for _, modFile := range res.FilesModified {
			fullPath := filepath.Join(session.WorkspaceDir(), filepath.FromSlash(modFile))
			if data, err := os.ReadFile(fullPath); err == nil {
				sha := canonical.SHA256Bytes(data)
				session.RecordWrite(modFile, data, sha)
				if bs := session.BuildSession(); bs != nil {
					_, _ = bs.CrystallizeFile(context.Background(), modFile, string(data), "")
				}
			}
		}

		fmt.Fprintf(out, "\n  %s %s\n", theme.Bold(theme.BrightMagenta("[Surgical Patch Applied]")), theme.Green(cleanPath))
		fmt.Fprintf(out, "  %s  %s  %s  %s\n\n",
			theme.Badge("Blocks", fmt.Sprintf("%d applied", res.BlocksApplied)),
			theme.Badge("Diff", fmt.Sprintf("+%d/-%d lines", res.LinesAdded, res.LinesRemoved)),
			theme.Badge("Merkle Seal", res.MerkleSeal),
			theme.Badge("ICX Surface", "0D-4D Synchronized"))

		summary := fmt.Sprintf("Surgical Patch Applied: %s\nBlocks: %d, +%d/-%d lines\nCAS Hash: %s\nMerkle Seal: %s\n",
			cleanPath, res.BlocksApplied, res.LinesAdded, res.LinesRemoved, res.CASHash, res.MerkleSeal)
		session.FeedExecutionSummary(summary)
	}

	return nil
}

// extractDiagFiles parses file paths mentioned in compiler diagnostics or guard rejection messages.
func extractDiagFiles(diag string) []string {
	var files []string
	seen := make(map[string]bool)

	add := func(p string) {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"'`")
		p = filepath.ToSlash(p)
		p = strings.TrimPrefix(p, "./")
		if p != "" && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}

	// 1. Quoted paths in guard / syntax messages
	reQuoted := regexp.MustCompile(`(?:File write for|patch on|check on spliced|check failed on spliced|Action on|Preserved existing)\s+["']([^"']+)["']`)
	for _, m := range reQuoted.FindAllStringSubmatch(diag, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}

	// 2. Standard compiler diagnostics (file:line:col or file:line)
	for _, f := range diagFiles(diag) {
		add(f)
	}

	// 3. devops.ParseCompilerDiagnostics
	for _, d := range devops.ParseCompilerDiagnostics(diag, "") {
		if d.File != "" {
			add(d.File)
		}
	}

	return files
}

// TurnAttemptRecord captures a failed attempt within an autonomous turn cycle.
type TurnAttemptRecord struct {
	AttemptNumber  int      `json:"attempt_number"`
	Command        string   `json:"command"`
	ActionsSummary []string `json:"actions_summary,omitempty"`
	Diagnostic     string   `json:"diagnostic"`
}

// countDiagnosticErrors estimates the number of distinct compiler errors reported in the diagnostic.
func countDiagnosticErrors(diag string) int {
	trimmed := strings.TrimSpace(diag)
	if trimmed == "" {
		return 0
	}
	lines := strings.Split(trimmed, "\n")
	count := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		// Go compiler diagnostics: file.go:line:col: message or file.go:line: message
		if strings.Contains(t, ".go:") && (strings.Contains(t, ": ") || strings.Contains(t, ":\t")) {
			count++
		} else if strings.HasPrefix(t, "# ") {
			count++
		}
	}
	if count == 0 {
		count = len(lines)
	}
	return count
}

// ICXDialogueAnalysis captures the peer-to-peer diagnostic reasoning from the subterranean engine.
type ICXDialogueAnalysis struct {
	ConversationalSpeech string
	PromptBriefing       string
}

// analyzeICXSubterraneanFeedback generates expert, peer-level guidance from ICX to the Architect.
// As lifelong engineering companions, ICX validates the 0D-4D filesystem lattice, catches OS and
// shell escaping traps, monitors host environment tool availability, and assists the Architect
// with exact, grounding guidance.
func analyzeICXSubterraneanFeedback(ws string, targetDir string, cmd string, diag string) ICXDialogueAnalysis {
	var speechParts []string
	var briefingParts []string

	checkDir := targetDir
	if checkDir == "" {
		checkDir = ws
	}

	// 1. Detect quote / line continuation syntax error in command execution (Windows / Python / Node)
	if strings.Contains(diag, "unexpected character after line continuation character") ||
		(strings.Contains(diag, "SyntaxError") && strings.Contains(cmd, "python -c")) ||
		strings.Contains(diag, `\"`) || strings.Contains(cmd, `\"`) {
		speechParts = append(speechParts, "Hey Architect, look closely at that shell invocation. On Windows cmd.exe, passing escaped quotes (\\\") into inline commands like python -c causes Python's tokenizer to choke on the literal backslash. All project assets are already safely synchronized in the 0D-4D lattice. Let's fix the command escaping or verify the static files directly!")
		briefingParts = append(briefingParts, "SHELL ESCAPING / TOKENIZER TRAP:\nOn Windows cmd.exe, passing escaped quotes (\\\"...) into 'python -c' or inline commands causes Python's tokenizer to fail with SyntaxError (unexpected character after line continuation character).\nFix: Use clean unescaped double quotes (e.g. python -c \"import os; ...\") without backslashes.")
	}

	// 2. Detect Go package scoping duplicate declarations
	if strings.Contains(diag, "redeclared in this block") ||
		strings.Contains(diag, "already declared at") ||
		strings.Contains(diag, "already declared") {
		speechParts = append(speechParts, "Architect, my friend — watch out for Go package scoping here. All files in this package directory share package-level namespace. That symbol is already declared in a sibling file. Let's remove the duplicate declaration or rename it!")
		briefingParts = append(briefingParts, "GO PACKAGE SCOPING:\nAll files in the same directory sharing the package clause share one namespace. Remove duplicate declarations across sibling files.")
	}

	// 3. Detect Go embed violations
	if strings.Contains(diag, "invalid pattern syntax") ||
		(strings.Contains(diag, "pattern") && strings.Contains(diag, "embed")) {
		speechParts = append(speechParts, "Heads up, Architect — Go's //go:embed directive cannot step outside the package directory with '..' or relative traversal. Let's adjust the embed path or serve it directly via http.FileServer.")
		briefingParts = append(briefingParts, "GO EMBED PATH VIOLATION:\n//go:embed cannot contain '..' or '.' path segments, and cannot reference parent directories.")
	}

	// 4. Detect missing tools
	if missing := ExtractMissingTool(diag); missing != "" {
		speechParts = append(speechParts, fmt.Sprintf("Partner, %s isn't installed on this host environment. Let's pivot to standard library runtimes or available native tools.", missing))
		briefingParts = append(briefingParts, fmt.Sprintf("MISSING HOST TOOL:\n%q is not installed on this host. Use native standard library runtimes or available tools.", missing))
	}

	// 5. Detect undefined symbols
	if strings.Contains(diag, "undefined:") {
		speechParts = append(speechParts, "Architect, we've got an undefined reference. Check if we need to emit the companion file or export the missing struct/function.")
		briefingParts = append(briefingParts, "UNDEFINED SYMBOL:\nEnsure companion files in the package are written or export the missing declaration.")
	}

	// 5b. Detect concurrency deadlocks, test timeouts, or mutex blockages
	if strings.Contains(diag, "COMMAND_TIMEOUT") ||
		strings.Contains(diag, "test timed out after") ||
		strings.Contains(diag, "sync.Mutex.Lock") ||
		strings.Contains(diag, "chan receive") ||
		strings.Contains(diag, "chan send") ||
		strings.Contains(diag, "Deadlock") ||
		strings.Contains(diag, "deadlock") {
		speechParts = append(speechParts, "Architect, my friend — we've encountered a concurrency lockup or test timeout. Go's sync.Mutex is strictly non-reentrant, and unbuffered channels block indefinitely without a concurrent peer. Let's inspect the goroutine dump and apply the unexported locked-helper pattern!")
		briefingParts = append(briefingParts, "CONCURRENCY DEADLOCK / TIMEOUT BRIEFING:\n1. Non-reentrant Mutex: Calling a method that locks mu while holding mu causes a permanent deadlock. Extract an unexported helper (e.g. resetTimerLocked) that assumes mu is held.\n2. RPC Reply Discard: If mock transport returns nil without writing into reply pointer, caller works with uninitialized data.\n3. Quorum Math: Majority quorum with N peers is (len(peers)/2)+1.\n4. Channels: Ensure channels have appropriate buffer capacity or non-blocking select dispatch.")
	}

	// 6. Inspect 0D-4D Lattice state of targetDir
	var existingAssets []string
	if fi, err := os.Stat(checkDir); err == nil && fi.IsDir() {
		_ = filepath.Walk(checkDir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(checkDir, p)
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, ".") && !strings.Contains(rel, "node_modules") {
				existingAssets = append(existingAssets, fmt.Sprintf("%s (%d bytes)", rel, info.Size()))
			}
			return nil
		})
	}

	if len(existingAssets) > 0 {
		briefingParts = append(briefingParts, fmt.Sprintf("0D-4D LATTICE AUDIT:\n%d verified file(s) present on disk in %s:\n  - %s", len(existingAssets), filepath.Base(checkDir), strings.Join(existingAssets, "\n  - ")))
	}

	// 7. Check if project already satisfies all 5 production pillars
	if checkDir != ws {
		if isComp, _ := AssessProjectCompleteness(ws, checkDir); isComp {
			speechParts = append(speechParts, "All 5 pillars of the deliverable are satisfied on disk. If the application is functioning as intended, conclude with DONE.")
			briefingParts = append(briefingParts, "PROJECT COMPLETENESS:\nAll 5 production pillars are satisfied on disk. If the application is functioning as intended, reply with 'DONE' and zero pending actions to conclude.")
		}
	}

	// Default fallback if no specific issue identified
	if len(speechParts) == 0 {
		speechParts = append(speechParts, "I caught the compiler output from that last run, Architect. Let's review the diagnostic together and apply a surgical fix.")
	}

	return ICXDialogueAnalysis{
		ConversationalSpeech: strings.Join(speechParts, " "),
		PromptBriefing:       strings.Join(briefingParts, "\n\n"),
	}
}

// buildSelfHealingPrompt extracts compiler diagnostics, failed attempt history, and surrounding source lines,
// and constructs a context-rich prompt prescribing surgical non-destructive patches.
func buildSelfHealingPrompt(ws string, lastTargetDir string, consecutiveHeals int, testDiagSummary string, history ...TurnAttemptRecord) string {
	diagsDir := ws
	if lastTargetDir != "" {
		diagsDir = lastTargetDir
	}
	diags := devops.ParseCompilerDiagnostics(testDiagSummary, "")
	if len(diags) > 0 {
		devops.EnrichDiagnosticsWithSource(diagsDir, diags)
	}

	lastCmd := ""
	if len(history) > 0 {
		lastCmd = history[len(history)-1].Command
	}
	icxAnalysis := analyzeICXSubterraneanFeedback(ws, lastTargetDir, lastCmd, testDiagSummary)

	var sb strings.Builder
	if icxAnalysis.PromptBriefing != "" {
		sb.WriteString("=== ICX SUBTERRANEAN PARTNER BRIEFING ===\n")
		sb.WriteString("Architect, my friend — ICX here. I inspected our workspace and failure diagnostics from our last attempt:\n\n")
		sb.WriteString(icxAnalysis.PromptBriefing)
		sb.WriteString("\n\n")
	}

	if consecutiveHeals <= 3 {
		sb.WriteString(fmt.Sprintf("Verification test or build failed (healing attempt %d/3).\n\n", consecutiveHeals))
	} else {
		sb.WriteString(fmt.Sprintf("Verification test or build failed (healing attempt %d, extended convergence budget).\n\n", consecutiveHeals))
	}
	if consecutiveHeals > 1 {
		sb.WriteString("NOTE: Previous healing attempt did not resolve the compiler diagnostic. The exact same error or new error persists. Please inspect the code carefully and provide an accurate fix.\n\n")
	}
	if strings.Contains(testDiagSummary, "redeclared in this block") ||
		strings.Contains(testDiagSummary, "already declared at") ||
		strings.Contains(testDiagSummary, "already declared") {
		sb.WriteString("=== GO PACKAGE SCOPING WARNING ===\n")
		sb.WriteString("In Go, ALL files within the same directory sharing the same package clause share the same package-level namespace.\n")
		sb.WriteString("If a symbol (function, method, type, var, const) is reported as 'redeclared in this block' or 'already declared at', it is ALREADY declared in another file in that package.\n")
		sb.WriteString("CRITICAL: Do NOT re-declare the symbol/method with the exact same name across multiple files in this package. Either:\n")
		sb.WriteString("1. Remove the duplicate declaration from this file.\n")
		sb.WriteString("2. Or rename the symbol/method if it was meant to be distinct.\n\n")
	}
	if strings.Contains(testDiagSummary, "invalid pattern syntax") ||
		(strings.Contains(testDiagSummary, "pattern") && strings.Contains(testDiagSummary, "embed")) {
		sb.WriteString("=== GO EMBED PATH VIOLATION WARNING ===\n")
		sb.WriteString("In Go, '//go:embed' directives CANNOT contain '..' or '.' path segments, and CANNOT reference parent directories outside the current package directory.\n")
		sb.WriteString("Patterns like '//go:embed ../../web/static' or '//go:embed all:../../web/static' are strictly ILLEGAL in Go and will NEVER compile.\n\n")
		sb.WriteString("HOW TO FIX:\n")
		sb.WriteString("1. REMOVE the '//go:embed' directive from this package file.\n")
		sb.WriteString("2. To embed assets from 'web/static', create a dedicated 'web/embed.go' in 'package web' using '//go:embed static/*' (and ensure web/static contains at least an index.html or placeholder file), then import that web package.\n")
		sb.WriteString("3. OR serve static files using http.FileServer(http.Dir(...)) or accept an fs.FS / http.FileSystem parameter in NewServer().\n")
		sb.WriteString("4. Also ensure any referenced companion types (e.g. APIHandler) are defined or implemented in this package (e.g. in api.go).\n\n")
	}
	if strings.Contains(testDiagSummary, "undefined:") {
		sb.WriteString("=== UNDEFINED SYMBOL / MISSING COMPANION FILE GUIDANCE ===\n")
		sb.WriteString("If an undefined symbol (struct, interface, function) belongs to a companion file in this package that has not yet been written (e.g. api.go), you can write the complete companion file using:\n")
		sb.WriteString("[ACTION:WRITE path=\"<target-path>\"]\n")
		sb.WriteString("Or patch the current file to provide the declaration using [ACTION:PATCH path=\"...\"]\n\n")
	}
	if missing := ExtractMissingTool(testDiagSummary); missing != "" {
		sb.WriteString("=== MISSING HOST ENVIRONMENT TOOL WARNING ===\n")
		sb.WriteString(fmt.Sprintf("The tool or executable %q is NOT installed on this host environment.\n", missing))
		sb.WriteString("MANDATORY: Do NOT use or require this tool. Do NOT emit build scripts or instructions expecting this tool.\n")
		sb.WriteString("Rewrite the project files, build scripts, or code to use standard/native tools available on the host.\n\n")
	}
	if strings.Contains(testDiagSummary, "COMMAND_TIMEOUT") ||
		strings.Contains(testDiagSummary, "test timed out") ||
		strings.Contains(testDiagSummary, "sync.Mutex.Lock") ||
		strings.Contains(testDiagSummary, "chan receive") ||
		strings.Contains(testDiagSummary, "chan send") ||
		strings.Contains(testDiagSummary, "Deadlock") ||
		strings.Contains(testDiagSummary, "deadlock") {
		sb.WriteString("=== CONCURRENCY DEADLOCK & TIMEOUT REPAIR PRESCRIPTION ===\n")
		sb.WriteString("The test runner timed out or encountered a concurrency deadlock. Follow these mandatory rules:\n")
		sb.WriteString("1. LOCKED HELPER PATTERN: In Go, sync.Mutex is strictly non-reentrant. If method A acquires mu and calls method B, method B MUST NOT call mu.Lock(). Extract an unexported helper (e.g. methodBLocked()) that assumes mu is already held, and call it from both.\n")
		sb.WriteString("2. RPC REPLY POPULATION: If implementing mock RPC transports or handlers, ensure reply values from channels or RPC calls are copied into the caller's reply pointer (e.g. via pointer dereference, type assertion, reflection, or JSON unmarshaling) instead of returning nil with an empty reply.\n")
		sb.WriteString("3. STRICT QUORUM MATH: For majority quorum with N peers, quorum is votesReceived >= (len(peers)/2)+1. Do NOT require unanimous consensus.\n")
		sb.WriteString("4. CHANNEL BLOCKAGE: Never send to or receive from unbuffered channels in lock-holding code or without timeout/select guards.\n\n")
	}
	sb.WriteString("=== COMPILER / TEST DIAGNOSTICS ===\n")
	sb.WriteString(strings.TrimSpace(testDiagSummary))
	sb.WriteString("\n\n")

	if len(history) > 0 {
		sb.WriteString("=== PREVIOUS FAILED ATTEMPTS IN THIS HEALING CYCLE ===\n")
		for _, att := range history {
			sb.WriteString(fmt.Sprintf("Attempt #%d:\n", att.AttemptNumber))
			if len(att.ActionsSummary) > 0 {
				sb.WriteString(fmt.Sprintf("  Actions Taken: %s\n", strings.Join(att.ActionsSummary, ", ")))
			}
			if att.Command != "" {
				sb.WriteString(fmt.Sprintf("  Command Run: %s\n", att.Command))
			}
			diagLines := strings.Split(strings.TrimSpace(att.Diagnostic), "\n")
			if len(diagLines) > 6 {
				diagLines = diagLines[:6]
			}
			sb.WriteString(fmt.Sprintf("  Failure Diagnostic:\n    %s\n\n", strings.Join(diagLines, "\n    ")))
		}
		sb.WriteString("CRITICAL: Inspect the progression of failures above. Do NOT repeat strategies that have already failed. Identify the root cause and emit an accurate fix.\n\n")
	}

	hasSourceContext := false
	if len(diags) > 0 {
		seen := make(map[string]bool)
		for _, d := range diags {
			if len(d.ContextLines) > 0 {
				key := fmt.Sprintf("%s:%d", d.File, d.Line)
				if seen[key] {
					continue
				}
				seen[key] = true
				if !hasSourceContext {
					sb.WriteString("=== SOURCE CONTEXT FOR ERRORS ===\n")
					hasSourceContext = true
				}
				snippet := strings.Join(d.ContextLines, "\n")
				sb.WriteString(fmt.Sprintf("File: %s (around line %d) - %s\n", d.File, d.Line, d.Message))
				sb.WriteString("```\n")
				sb.WriteString(snippet)
				if !strings.HasSuffix(snippet, "\n") {
					sb.WriteString("\n")
				}
				sb.WriteString("```\n\n")
			}
		}
	}

	// Extract all files mentioned in diagnostics or guard rejection messages
	targetFiles := extractDiagFiles(testDiagSummary)
	for _, d := range diags {
		if d.File != "" {
			targetFiles = append(targetFiles, d.File)
		}
	}

	// Provide full source context for relevant files (<= 600 lines / 32KB)
	if len(targetFiles) > 0 {
		seenFiles := make(map[string]bool)
		for _, rawPath := range targetFiles {
			if rawPath == "" || seenFiles[rawPath] {
				continue
			}
			seenFiles[rawPath] = true
			targetPath := rawPath
			if !filepath.IsAbs(targetPath) {
				targetPath = filepath.Join(diagsDir, targetPath)
			}
			data, err := os.ReadFile(targetPath)
			if err != nil {
				// Search ws by base filename
				base := filepath.Base(rawPath)
				_ = filepath.Walk(ws, func(p string, fi os.FileInfo, err error) error {
					if err == nil && !fi.IsDir() && fi.Name() == base {
						if d, rErr := os.ReadFile(p); rErr == nil {
							data = d
							targetPath = p
							return filepath.SkipAll
						}
					}
					return nil
				})
			}
			if len(data) > 0 && len(data) <= 32768 {
				lines := strings.Split(string(data), "\n")
				if len(lines) <= 600 {
					rel, rErr := filepath.Rel(ws, targetPath)
					displayPath := rawPath
					if rErr == nil {
						displayPath = filepath.ToSlash(rel)
					}
					sb.WriteString(fmt.Sprintf("=== FULL FILE CONTENT FOR %s (%d lines) ===\n```\n%s\n```\n\n", displayPath, len(lines), string(data)))
				}
			}
		}
	}

	if strings.Contains(testDiagSummary, "rejected by Code Preservation Guard") {
		sb.WriteString("=== GUARD REJECTION NOTICE ===\n")
		sb.WriteString("Your previous write was rejected because it truncated the file or deleted existing functions/types.\n")
		sb.WriteString("The FULL CURRENT FILE CONTENT is provided above. You must either:\n")
		sb.WriteString("1. Use [ACTION:PATCH path=\"...\"] with exact <<<<<<< SEARCH / ======= / >>>>>>> REPLACE blocks matching the lines above.\n")
		sb.WriteString("2. Or use [ACTION:WRITE path=\"...\"] with the 100% COMPLETE, fully implemented file preserving all declarations.\n\n")
	}

	sb.WriteString("=== NON-DESTRUCTIVE HEALING INSTRUCTIONS ===\n")
	sb.WriteString("1. PREFER SURGICAL PATCHES: Use [ACTION:PATCH path=\"...\"] with standard Aider search/replace blocks:\n")
	sb.WriteString("<<<<<<< SEARCH\n// exact lines from file\n=======\n// replacement lines\n>>>>>>> REPLACE\n")
	sb.WriteString("2. NEVER TRUNCATE OR STUB: Destructive truncations are blocked by the Code Preservation Guard. Do NOT rewrite entire files with partial snippets or delete working functions.\n")
	sb.WriteString("3. If you must rewrite an entire file using [ACTION:WRITE path=\"...\"], you MUST provide the 100% complete, fully-implemented source code.\n")

	return sb.String()
}

// buildPhaseProgressionPrompt constructs a detailed progression prompt for activePlanPhase,
// grounding the model in existing files, Go module path, package imports, and non-destructive invariants.
func buildPhaseProgressionPrompt(ws string, planSpec *engine.PlanSpec, activePhase *engine.PlanPhase, complete bool, missing []string) string {
	var sb strings.Builder

	// List completed phases and existing verified files
	var completedPhases []string
	var existingFiles []string
	if planSpec != nil {
		for _, ph := range planSpec.Phases {
			if ph.PhaseNumber < activePhase.PhaseNumber {
				completedPhases = append(completedPhases, fmt.Sprintf("Phase %d: %s", ph.PhaseNumber, ph.Name))
				for _, tf := range ph.TargetFiles {
					if planSpec.FileExists(ws, tf) {
						existingFiles = append(existingFiles, tf)
					}
				}
			}
		}
	}

	sb.WriteString(fmt.Sprintf("=== PLAN PROGRESSION: PHASE %d / %d ===\n", activePhase.PhaseNumber, len(planSpec.Phases)))
	if planSpec != nil {
		sb.WriteString(fmt.Sprintf("Module: %s (Project Scope: %s)\n", planSpec.ModuleName, planSpec.ProjectScope))
	}
	if len(completedPhases) > 0 {
		sb.WriteString("Verified Completed Phases:\n")
		for _, cp := range completedPhases {
			sb.WriteString(fmt.Sprintf("  - %s\n", cp))
		}
	}
	if len(existingFiles) > 0 {
		sb.WriteString(fmt.Sprintf("Existing Verified Files on Disk (%d files):\n", len(existingFiles)))
		for _, ef := range existingFiles {
			sb.WriteString(fmt.Sprintf("  - %s\n", ef))
		}
	}
	sb.WriteString("\n")

	sb.WriteString(fmt.Sprintf("Current Active Milestone: Phase %d: %s\n", activePhase.PhaseNumber, activePhase.Name))
	sb.WriteString(fmt.Sprintf("Description: %s\n", activePhase.Description))
	if activePhase.VerificationCmd != "" {
		sb.WriteString(fmt.Sprintf("Verification Command: %s\n", activePhase.VerificationCmd))
	}
	var existingInActivePhase []string
	if planSpec != nil {
		for _, tf := range activePhase.TargetFiles {
			if planSpec.FileExists(ws, tf) {
				existingInActivePhase = append(existingInActivePhase, tf)
			}
		}
	}

	if len(existingInActivePhase) > 0 {
		sb.WriteString(fmt.Sprintf("Already Implemented in Phase %d (%d files verified on disk):\n", activePhase.PhaseNumber, len(existingInActivePhase)))
		for _, ef := range existingInActivePhase {
			sb.WriteString(fmt.Sprintf("  [x] %s\n", ef))
		}
		sb.WriteString("\n")
	}

	// Inject Phase Step-by-Step Specification if available
	if activePhase.SpecSection != "" {
		sb.WriteString("=== ACTIVE PHASE STEP-BY-STEP SPECIFICATION ===\n")
		sb.WriteString(strings.TrimSpace(activePhase.SpecSection))
		sb.WriteString("\n\n")
	}

	if !complete && len(missing) < len(activePhase.TargetFiles) {
		sb.WriteString(fmt.Sprintf("Phase %d is partially written (%d/%d files present). The following %d files MUST be implemented next:\n",
			activePhase.PhaseNumber, len(activePhase.TargetFiles)-len(missing), len(activePhase.TargetFiles), len(missing)))
		for _, m := range missing {
			scopedM := m
			if planSpec != nil && planSpec.ProjectScope != "" && !strings.HasPrefix(m, planSpec.ProjectScope+"/") {
				scopedM = planSpec.ProjectScope + "/" + m
			}
			sb.WriteString(fmt.Sprintf("  - %s\n", scopedM))

			// Inject detailed file spec from plan if available
			if activePhase.FileSpecs != nil {
				if fSpec, ok := activePhase.FileSpecs[m]; ok && strings.TrimSpace(fSpec) != "" {
					sb.WriteString(fmt.Sprintf("    Detailed Spec for %s:\n", filepath.Base(m)))
					lines := strings.Split(fSpec, "\n")
					for _, l := range lines {
						sb.WriteString("      " + l + "\n")
					}
					sb.WriteString("\n")
				}
			}
		}
		sb.WriteString("\nCRITICAL INSTRUCTION: DO NOT re-generate or overwrite the files that are already implemented above. Focus your response ENTIRELY on writing the missing files listed above using [ACTION:WRITE path=\"...\"] tags.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Previous step verified successfully. Implement all components and tests for Phase %d.\n", activePhase.PhaseNumber))
		if activePhase.FileSpecs != nil {
			for _, tf := range activePhase.TargetFiles {
				if fSpec, ok := activePhase.FileSpecs[tf]; ok && strings.TrimSpace(fSpec) != "" {
					sb.WriteString(fmt.Sprintf("\nSpecification for %s:\n%s\n", filepath.Base(tf), fSpec))
				}
			}
		}
	}

	sb.WriteString("\n=== CRITICAL ARCHITECTURAL GROUNDING RULES ===\n")
	if planSpec != nil && planSpec.ModuleName != "" {
		sb.WriteString(fmt.Sprintf("1. REUSE EXISTING PACKAGES: Do NOT duplicate or re-declare types, packages, or models from earlier phases. Import them directly from the module path (e.g. `import \"%s/...\"`).\n", planSpec.ModuleName))
	} else {
		sb.WriteString("1. REUSE EXISTING PACKAGES: Do NOT duplicate or re-declare types, packages, or models from earlier phases. Import existing packages directly.\n")
	}
	sb.WriteString("2. ZERO STUBS: Strictly prohibited from emitting minimal mocks, placeholders (`// TODO`, `// TBD`), or empty stub methods. Implement complete, mathematically valid, production-grade business logic.\n")
	sb.WriteString("3. FULL DEPTH: Every file must be substantive (typically 40-200 lines for core packages) with complete error handling, validation, and real algorithms as specified above.\n")
	sb.WriteString(fmt.Sprintf("4. PHASE ISOLATION: Focus EXCLUSIVELY on implementing the target files for Phase %d. Do NOT jump ahead to future phases.\n", activePhase.PhaseNumber))
	sb.WriteString("5. USE [ACTION:WRITE path=\"...\"] tags for new files, or [ACTION:PATCH path=\"...\"] with standard search/replace blocks for targeted updates.\n")

	return sb.String()
}

// formatReceiverType returns the formatted receiver type string for method matching.
func formatReceiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + formatReceiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return formatReceiverType(t.X)
	case *ast.IndexListExpr:
		return formatReceiverType(t.X)
	default:
		return fmt.Sprintf("%v", expr)
	}
}

// normalizeReceiver strips enclosing parentheses and excess whitespace from a receiver type name.
func normalizeReceiver(recv string) string {
	recv = strings.TrimSpace(recv)
	recv = strings.Trim(recv, "()")
	recv = strings.TrimSpace(recv)
	return recv
}

// isSingleFunctionUpdate determines if incoming code is an isolated function/type/value snippet
// intended to update an existing declaration rather than a complete file rewrite.
func isSingleFunctionUpdate(existingCode, incomingCode string) bool {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, "", incomingCode, parser.PackageClauseOnly)
	if err != nil {
		// Missing package declaration indicates an isolated snippet
		return true
	}
	incFuncs := countGoFunctions(incomingCode)
	incTypes := countGoTypes(incomingCode)
	existFuncs := countGoFunctions(existingCode)
	existTypes := countGoTypes(existingCode)

	// Single function/method update on a multi-declaration file
	if incFuncs == 1 && incTypes == 0 && existFuncs >= 2 {
		return true
	}
	// Single type update on a multi-type file
	if incFuncs == 0 && incTypes == 1 && existTypes >= 2 {
		return true
	}
	return false
}

// areMissingSymbolsDeclaredInPackage inspects sibling .go files in the same directory as targetPath
// to determine if all missing functions and types have been moved/refactored into companion package files.
func areMissingSymbolsDeclaredInPackage(workspaceDir, targetPath string, missingFuncs, missingTypes []string) bool {
	if len(missingFuncs) == 0 && len(missingTypes) == 0 {
		return false
	}

	absPath := targetPath
	if !filepath.IsAbs(absPath) && workspaceDir != "" {
		absPath = filepath.Join(workspaceDir, targetPath)
	}
	dir := filepath.Dir(absPath)
	targetBase := filepath.Base(absPath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	siblingFuncs := make(map[string]bool)
	siblingTypes := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".go") || strings.HasSuffix(name, "_test.go") || name == targetBase {
			continue
		}

		siblingData, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, name, siblingData, 0)
		if err != nil {
			continue
		}

		for _, decl := range node.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name != nil {
					key := d.Name.Name
					if d.Recv != nil && len(d.Recv.List) > 0 {
						recvStr := formatReceiverType(d.Recv.List[0].Type)
						recvNorm := strings.TrimPrefix(recvStr, "*")
						siblingFuncs[fmt.Sprintf("(%s).%s", recvStr, d.Name.Name)] = true
						siblingFuncs[fmt.Sprintf("(%s).%s", recvNorm, d.Name.Name)] = true
						siblingFuncs[fmt.Sprintf("%s.%s", recvNorm, d.Name.Name)] = true
					}
					siblingFuncs[key] = true
					siblingFuncs[d.Name.Name] = true
				}
			case *ast.GenDecl:
				if d.Tok == token.TYPE {
					for _, spec := range d.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil {
							siblingTypes[ts.Name.Name] = true
						}
					}
				}
			}
		}
	}

	for _, fn := range missingFuncs {
		cleanFn := fn
		if !siblingFuncs[cleanFn] {
			if dotIdx := strings.LastIndex(cleanFn, "."); dotIdx != -1 {
				bare := cleanFn[dotIdx+1:]
				if !siblingFuncs[bare] {
					return false
				}
			} else {
				return false
			}
		}
	}

	for _, tp := range missingTypes {
		if !siblingTypes[tp] {
			return false
		}
	}

	return true
}

// tryMergeGoFunction attempts to parse both existingCode and incomingCode as Go code.
// If incomingCode defines a subset of functions/types/values or updates existing declarations,
// it splices the updated function/type/value definitions into existingCode while preserving
// all other functions, types, constants, variables, comments, and package declarations.
// Any new declarations in incomingCode that do not exist in existingCode are safely appended.
func tryMergeGoFunction(existingCode, incomingCode string) (string, bool) {
	fsetExist := token.NewFileSet()
	existAST, err := parser.ParseFile(fsetExist, "existing.go", existingCode, parser.ParseComments)
	if err != nil {
		return existingCode, false
	}

	// Try parsing incoming code directly, or with dummy package header if missing
	fsetInc := token.NewFileSet()
	incAST, err := parser.ParseFile(fsetInc, "incoming.go", incomingCode, 0)
	prependedDummy := false
	dummyPrefix := "package dummy\n\n"
	if err != nil {
		incAST, err = parser.ParseFile(fsetInc, "incoming.go", dummyPrefix+incomingCode, 0)
		if err != nil {
			return existingCode, false
		}
		prependedDummy = true
	}

	// If incoming defines ALL functions AND ALL types that exist in existingCode without missing any,
	// and incoming has a full package header, then it is an unabridged full-file rewrite, not a partial splice.
	if !prependedDummy {
		existFuncs := countGoFunctions(existingCode)
		existTypes := countGoTypes(existingCode)
		missingFuncs := detectMissingGoFunctions(existingCode, incomingCode)
		missingTypes := detectMissingGoTypes(existingCode, incomingCode)

		if (existFuncs == 0 || len(missingFuncs) == 0) &&
			(existTypes == 0 || len(missingTypes) == 0) {
			// All functions and types already present in incomingCode; full rewrite, not a merge
			return existingCode, false
		}
	}

	type replacement struct {
		name        string
		recv        string
		isType      bool
		isValue     bool
		replaceText string
	}
	var incomingDecls []replacement

	for _, decl := range incAST.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			recvName := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvName = normalizeReceiver(formatReceiverType(d.Recv.List[0].Type))
			}
			startOffset := fsetInc.Position(d.Pos()).Offset
			endOffset := fsetInc.Position(d.End()).Offset
			if prependedDummy {
				startOffset -= len(dummyPrefix)
				endOffset -= len(dummyPrefix)
			}
			if startOffset >= 0 && endOffset <= len(incomingCode) && startOffset < endOffset {
				incomingDecls = append(incomingDecls, replacement{
					name:        d.Name.Name,
					recv:        recvName,
					isType:      false,
					isValue:     false,
					replaceText: incomingCode[startOffset:endOffset],
				})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil {
						startOffset := fsetInc.Position(d.Pos()).Offset
						endOffset := fsetInc.Position(d.End()).Offset
						if len(d.Specs) > 1 {
							startOffset = fsetInc.Position(s.Pos()).Offset
							endOffset = fsetInc.Position(s.End()).Offset
						}
						if prependedDummy {
							startOffset -= len(dummyPrefix)
							endOffset -= len(dummyPrefix)
						}
						if startOffset >= 0 && endOffset <= len(incomingCode) && startOffset < endOffset {
							incomingDecls = append(incomingDecls, replacement{
								name:        s.Name.Name,
								recv:        "",
								isType:      true,
								isValue:     false,
								replaceText: incomingCode[startOffset:endOffset],
							})
						}
					}
				case *ast.ValueSpec:
					for _, nameIdent := range s.Names {
						if nameIdent != nil {
							startOffset := fsetInc.Position(d.Pos()).Offset
							endOffset := fsetInc.Position(d.End()).Offset
							if len(d.Specs) > 1 || len(s.Names) > 1 {
								startOffset = fsetInc.Position(s.Pos()).Offset
								endOffset = fsetInc.Position(s.End()).Offset
							}
							if prependedDummy {
								startOffset -= len(dummyPrefix)
								endOffset -= len(dummyPrefix)
							}
							if startOffset >= 0 && endOffset <= len(incomingCode) && startOffset < endOffset {
								incomingDecls = append(incomingDecls, replacement{
									name:        nameIdent.Name,
									recv:        "",
									isType:      false,
									isValue:     true,
									replaceText: incomingCode[startOffset:endOffset],
								})
								if len(d.Specs) == 1 && len(s.Names) == 1 {
									break
								}
							}
						}
					}
				}
			}
		}
	}

	if len(incomingDecls) == 0 {
		return existingCode, false
	}

	type spanReplacement struct {
		start       int
		end         int
		replaceText string
	}
	var spans []spanReplacement
	var toAppend []string

	for _, inc := range incomingDecls {
		matched := false
		if !inc.isType && !inc.isValue {
			for _, decl := range existAST.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name == nil || fn.Name.Name != inc.name {
					continue
				}
				recvName := ""
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					recvName = normalizeReceiver(formatReceiverType(fn.Recv.List[0].Type))
				}
				if recvName == inc.recv {
					start := fsetExist.Position(fn.Pos()).Offset
					end := fsetExist.Position(fn.End()).Offset
					if start >= 0 && end <= len(existingCode) && start < end {
						spans = append(spans, spanReplacement{
							start:       start,
							end:         end,
							replaceText: inc.replaceText,
						})
						matched = true
						break
					}
				}
			}
		} else if inc.isType {
			for _, decl := range existAST.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gen.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil && ts.Name.Name == inc.name {
						start := fsetExist.Position(gen.Pos()).Offset
						end := fsetExist.Position(gen.End()).Offset
						if len(gen.Specs) > 1 {
							start = fsetExist.Position(ts.Pos()).Offset
							end = fsetExist.Position(ts.End()).Offset
						}
						if start >= 0 && end <= len(existingCode) && start < end {
							spans = append(spans, spanReplacement{
								start:       start,
								end:         end,
								replaceText: inc.replaceText,
							})
							matched = true
							break
						}
					}
				}
				if matched {
					break
				}
			}
		} else if inc.isValue {
			for _, decl := range existAST.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gen.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							if id != nil && id.Name == inc.name {
								start := fsetExist.Position(gen.Pos()).Offset
								end := fsetExist.Position(gen.End()).Offset
								if len(gen.Specs) > 1 || len(vs.Names) > 1 {
									start = fsetExist.Position(vs.Pos()).Offset
									end = fsetExist.Position(vs.End()).Offset
								}
								if start >= 0 && end <= len(existingCode) && start < end {
									spans = append(spans, spanReplacement{
										start:       start,
										end:         end,
										replaceText: inc.replaceText,
									})
									matched = true
									break
								}
							}
						}
					}
					if matched {
						break
					}
				}
				if matched {
					break
				}
			}
		}
		if !matched {
			alreadyAppended := false
			for _, existingApp := range toAppend {
				if existingApp == inc.replaceText {
					alreadyAppended = true
					break
				}
			}
			if !alreadyAppended {
				toAppend = append(toAppend, inc.replaceText)
			}
		}
	}

	if len(spans) == 0 && len(toAppend) == 0 {
		return existingCode, false
	}

	// Sort spans by start offset descending so replacements don't invalidate offsets
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].start > spans[j].start
	})

	merged := existingCode
	for _, s := range spans {
		merged = merged[:s.start] + s.replaceText + merged[s.end:]
	}

	// Append any new declarations
	for _, app := range toAppend {
		merged = strings.TrimRight(merged, "\r\n") + "\n\n" + strings.TrimSpace(app) + "\n"
	}

	// Splice any new imports introduced in incoming code
	if len(incAST.Imports) > 0 {
		for _, imp := range incAST.Imports {
			if imp.Path != nil && imp.Path.Value != "" {
				importPath := imp.Path.Value
				if !strings.Contains(merged, importPath) {
					// Insert missing import into the first import (...) block, if present
					if idx := strings.Index(merged, "import ("); idx != -1 {
						closeIdx := strings.Index(merged[idx:], ")")
						if closeIdx != -1 {
							insertPos := idx + closeIdx
							merged = merged[:insertPos] + "\t" + importPath + "\n" + merged[insertPos:]
						}
					} else {
						// Insert import statement after package clause
						if pIdx := strings.Index(merged, "\n"); pIdx != -1 {
							merged = merged[:pIdx+1] + "\nimport " + importPath + "\n" + merged[pIdx+1:]
						}
					}
				}
			}
		}
	}

	// Verify merged code is syntactically valid Go
	testFset := token.NewFileSet()
	if _, err := parser.ParseFile(testFset, "merged.go", merged, 0); err != nil {
		return existingCode, false
	}

	return merged, true
}

// countGoFunctions counts the number of top-level function/method declarations in a Go source string.
func countGoFunctions(code string) int {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", code, 0)
	if err != nil {
		matches := regexp.MustCompile(`(?m)^func\s+`).FindAllString(code, -1)
		return len(matches)
	}
	count := 0
	for _, decl := range node.Decls {
		if _, ok := decl.(*ast.FuncDecl); ok {
			count++
		}
	}
	return count
}

// countGoTypes counts the number of top-level type declarations in Go source.
func countGoTypes(code string) int {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", code, 0)
	if err != nil {
		node, _ = parser.ParseFile(fset, "", "package dummy\n\n"+code, 0)
	}
	if node == nil {
		return 0
	}
	count := 0
	for _, decl := range node.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if _, ok := spec.(*ast.TypeSpec); ok {
					count++
				}
			}
		}
	}
	return count
}

// detectMissingGoFunctions returns the names of any functions present in existingCode
// that are completely missing from incomingCode.
func detectMissingGoFunctions(existingCode, incomingCode string) []string {
	fsetExist := token.NewFileSet()
	existAST, err := parser.ParseFile(fsetExist, "exist.go", existingCode, 0)
	if err != nil {
		return nil
	}
	fsetInc := token.NewFileSet()
	incAST, err := parser.ParseFile(fsetInc, "inc.go", incomingCode, 0)
	if err != nil {
		incAST, _ = parser.ParseFile(fsetInc, "inc.go", "package dummy\n\n"+incomingCode, 0)
	}

	existingFuncs := make(map[string]bool)
	for _, decl := range existAST.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
			key := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				key = fmt.Sprintf("(%s).%s", formatReceiverType(fn.Recv.List[0].Type), fn.Name.Name)
			}
			existingFuncs[key] = true
		}
	}

	if incAST != nil {
		for _, decl := range incAST.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name != nil {
				key := fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) > 0 {
					key = fmt.Sprintf("(%s).%s", formatReceiverType(fn.Recv.List[0].Type), fn.Name.Name)
				}
				delete(existingFuncs, key)
			}
		}
	}

	var missing []string
	for k := range existingFuncs {
		missing = append(missing, k)
	}
	sort.Strings(missing)
	return missing
}

// detectMissingGoTypes returns type names present in existingCode but missing in incomingCode.
func detectMissingGoTypes(existingCode, incomingCode string) []string {
	fsetExist := token.NewFileSet()
	existAST, err := parser.ParseFile(fsetExist, "exist.go", existingCode, 0)
	if err != nil {
		return nil
	}
	fsetInc := token.NewFileSet()
	incAST, err := parser.ParseFile(fsetInc, "inc.go", incomingCode, 0)
	if err != nil {
		incAST, _ = parser.ParseFile(fsetInc, "inc.go", "package dummy\n\n"+incomingCode, 0)
	}

	existingTypes := make(map[string]bool)
	for _, decl := range existAST.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil {
					existingTypes[ts.Name.Name] = true
				}
			}
		}
	}

	if incAST != nil {
		for _, decl := range incAST.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
				for _, spec := range gen.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name != nil {
						delete(existingTypes, ts.Name.Name)
					}
				}
			}
		}
	}

	var missing []string
	for k := range existingTypes {
		missing = append(missing, k)
	}
	sort.Strings(missing)
	return missing
}

// printMemorySurfaces queries and renders the 5 ICX volumetric memory surfaces.
func printMemorySurfaces(out io.Writer, session *Session) {
	theme := NewTerminalTheme()
	if session == nil {
		fmt.Fprintln(out, theme.Yellow("[Notice] No active session available."))
		return
	}

	fmt.Fprintln(out, theme.Bold(theme.BrightCyan("\nICX 4D Volumetric Memory Surfaces:")))

	// Session State
	activeProj := session.ActiveProject()
	if activeProj == "" {
		activeProj = "(none established)"
	}
	fmt.Fprintf(out, "  %s  %s\n", theme.Bold("Active Project Anchor:"), theme.BrightGreen(activeProj))

	written := session.WrittenFiles()
	fmt.Fprintf(out, "  %s         %d files\n", theme.Bold("Files Recorded:"), len(written))
	for _, f := range written {
		fmt.Fprintf(out, "    - %s\n", theme.Green(f))
	}

	icxClient := session.ICX()
	if icxClient == nil {
		fmt.Fprintln(out, theme.Yellow("  ICX Client: Disconnected"))
		fmt.Fprintln(out)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if insp, err := icxClient.InspectSpace(ctx, "default"); err == nil && insp != nil {
		fmt.Fprintf(out, "  %s        %d indexed nodes (%d sources, %d edges)\n",
			theme.Bold("Lattice Status:"), insp.NodesCount, insp.SourcesCount, insp.Edges)
	}

	// 0D SCALAR SURFACE
	fmt.Fprintf(out, "\n  %s\n", theme.Bold(theme.Cyan("[0D SCALAR SURFACE]")))
	if res, err := icxClient.VarGet(ctx, icx.VarGetRequest{Key: "active_project"}); err == nil && res != nil && res.OK {
		fmt.Fprintf(out, "    - active_project: %v\n", res.Value)
	}
	if res, err := icxClient.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "0D_SCALAR"}); err == nil && res != nil && len(res.Nodes) > 0 {
		for _, node := range res.Nodes {
			if node["payload"] != nil {
				if pl, ok := node["payload"].(map[string]any); ok {
					fmt.Fprintf(out, "    - %v: %v (SHA: %v)\n", pl["type"], pl["file_path"], pl["sha256"])
				}
			}
		}
	} else {
		fmt.Fprintf(out, "    - recorded_files: %d\n", len(written))
	}

	// 1D SEQUENCE SURFACE
	fmt.Fprintf(out, "\n  %s\n", theme.Bold(theme.Cyan("[1D SEQUENCE SURFACE]")))
	if res, err := icxClient.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "1D_SEQUENCE"}); err == nil && res != nil && len(res.Nodes) > 0 {
		for _, node := range res.Nodes {
			if node["payload"] != nil {
				if pl, ok := node["payload"].(map[string]any); ok {
					fmt.Fprintf(out, "    - %v\n", pl["summary"])
				}
			}
		}
	} else if len(written) > 0 {
		for _, f := range written {
			fmt.Fprintf(out, "    - write: %s\n", f)
		}
	} else {
		fmt.Fprintln(out, "    (empty)")
	}

	// 2D TABULAR SURFACE
	fmt.Fprintf(out, "\n  %s\n", theme.Bold(theme.Cyan("[2D TABULAR SURFACE]")))
	if res, err := icxClient.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "2D_TABULAR"}); err == nil && res != nil && len(res.Nodes) > 0 {
		for _, node := range res.Nodes {
			if node["payload"] != nil {
				if pl, ok := node["payload"].(map[string]any); ok {
					fmt.Fprintf(out, "    - %-35s %-10v %v\n", pl["path"], pl["size"], pl["sha256"])
				}
			}
		}
	} else if len(written) > 0 {
		for _, f := range written {
			fmt.Fprintf(out, "    - %s\n", f)
		}
	} else {
		fmt.Fprintln(out, "    (empty)")
	}

	// 3D VOLUMETRIC SURFACE
	fmt.Fprintf(out, "\n  %s\n", theme.Bold(theme.Cyan("[3D VOLUMETRIC SURFACE]")))
	if res, err := icxClient.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "3D_VOLUMETRIC"}); err == nil && res != nil && len(res.Nodes) > 0 {
		for _, node := range res.Nodes {
			if node["payload"] != nil {
				if pl, ok := node["payload"].(map[string]any); ok {
					fmt.Fprintf(out, "    - project: %v (file: %v, component: %v)\n", pl["project"], pl["path"], pl["component"])
				}
			}
		}
	} else {
		fmt.Fprintf(out, "    - project_root: %s (scoped components: %d)\n", activeProj, len(written))
	}

	// 4D SPATIOTEMPORAL SURFACE
	fmt.Fprintf(out, "\n  %s\n", theme.Bold(theme.Cyan("[4D SPATIOTEMPORAL SURFACE]")))
	if res, err := icxClient.VarGet(ctx, icx.VarGetRequest{Key: "swarm.last_seal"}); err == nil && res != nil && res.OK {
		fmt.Fprintf(out, "    - swarm.last_seal: %v\n", res.Value)
	}
	if res, err := icxClient.VarGet(ctx, icx.VarGetRequest{Key: "swarm.active_project"}); err == nil && res != nil && res.OK {
		fmt.Fprintf(out, "    - swarm.active_project: %v\n", res.Value)
	}
	if res, err := icxClient.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "4D_SPATIOTEMPORAL"}); err == nil && res != nil && len(res.Nodes) > 0 {
		for _, node := range res.Nodes {
			if node["payload"] != nil {
				if pl, ok := node["payload"].(map[string]any); ok {
					fmt.Fprintf(out, "    - event: %v %v (seal: %v)\n", pl["action"], pl["file_path"], pl["merkle_seal"])
				}
			}
		}
	}

	fmt.Fprintln(out)
}

const (
	MaxScanCapacity       = 1024 * 1024
	BracketedPasteEnable  = "\x1b[?2004h"
	BracketedPasteDisable = "\x1b[?2004l"
	BracketedPasteStart   = "\x1b[200~"
	BracketedPasteEnd     = "\x1b[201~"
)

var (
	interactiveTerminalMu sync.RWMutex
	interactiveTerminal   bool
)

// SetInteractiveTerminal marks whether an interactive terminal session is active.
func SetInteractiveTerminal(active bool) {
	interactiveTerminalMu.Lock()
	defer interactiveTerminalMu.Unlock()
	interactiveTerminal = active
}

// IsInteractiveTerminalActive returns true if an interactive terminal is currently running.
func IsInteractiveTerminalActive() bool {
	interactiveTerminalMu.RLock()
	defer interactiveTerminalMu.RUnlock()
	return interactiveTerminal
}

// IsTerminal checks if the given reader represents an interactive terminal character device.
func IsTerminal(r io.Reader) bool {
	if r == nil {
		return false
	}
	if f, ok := r.(*os.File); ok {
		return isTerminalFile(f)
	}
	return false
}

func cleanBracketedPasteArtifacts(s string) string {
	s = strings.ReplaceAll(s, BracketedPasteStart, "")
	s = strings.ReplaceAll(s, BracketedPasteEnd, "")
	s = strings.ReplaceAll(s, "^[[200~", "")
	s = strings.ReplaceAll(s, "^[[201~", "")
	s = strings.ReplaceAll(s, "[200~", "")
	s = strings.ReplaceAll(s, "[201~", "")
	s = strings.ReplaceAll(s, BracketedPasteEnable, "")
	s = strings.ReplaceAll(s, BracketedPasteDisable, "")
	// Handle Windows console / PowerShell escape truncation artifacts (0~ at start, 1~ or ~ at end)
	if strings.HasPrefix(s, "0~") {
		s = strings.TrimPrefix(s, "0~")
	}
	if strings.HasSuffix(s, "1~") {
		s = strings.TrimSuffix(s, "1~")
	}
	return s
}

func stripBracketedStart(s string) string {
	s = strings.ReplaceAll(s, BracketedPasteStart, "")
	s = strings.ReplaceAll(s, "^[[200~", "")
	s = strings.ReplaceAll(s, "[200~", "")
	if strings.HasPrefix(s, "0~") {
		s = strings.TrimPrefix(s, "0~")
	}
	return s
}

func stripBracketedEnd(s string) string {
	s = strings.ReplaceAll(s, BracketedPasteEnd, "")
	s = strings.ReplaceAll(s, "^[[201~", "")
	s = strings.ReplaceAll(s, "[201~", "")
	if strings.HasSuffix(s, "1~") {
		s = strings.TrimSuffix(s, "1~")
	}
	return s
}

// ReadPrompt reads a prompt from reader, supporting single-line commands,
// bracketed paste mode (\x1b[200~ ... \x1b[201~), multi-line block quotes (triple quotes/backticks),
// line continuations (\), and interactive rapid paste detection.
func ReadPrompt(reader *bufio.Reader, out io.Writer, promptPrefix string) (string, error) {
	return ReadPromptMultiLine(reader, out, promptPrefix, false)
}

// ReadPromptMultiLine reads a prompt supporting single-line, multi-line mode, and large pastes.
func ReadPromptMultiLine(reader *bufio.Reader, out io.Writer, promptPrefix string, multiLine bool) (string, error) {
	if promptPrefix != "" && out != nil {
		fmt.Fprint(out, promptPrefix)
	}

	firstLine, err := readBoundedLine(reader, MaxScanCapacity)
	if err != nil {
		return "", err
	}

	// Case 0: Bracketed paste mode (\x1b[200~ ... \x1b[201~, ^[[200~, or corrupted 0~...1~)
	if strings.Contains(firstLine, BracketedPasteStart) || strings.Contains(firstLine, "^[[200~") || strings.Contains(firstLine, "[200~") || (strings.HasPrefix(strings.TrimSpace(firstLine), "0~") && strings.HasSuffix(strings.TrimSpace(firstLine), "1~")) {
		cleanFirst := stripBracketedStart(firstLine)
		if strings.Contains(cleanFirst, BracketedPasteEnd) || strings.Contains(cleanFirst, "^[[201~") || strings.Contains(cleanFirst, "[201~") || strings.HasSuffix(strings.TrimSpace(cleanFirst), "1~") {
			result := cleanBracketedPasteArtifacts(stripBracketedEnd(cleanFirst))
			return strings.TrimRight(result, "\r\n"), nil
		}
		var blockLines []string
		blockLines = append(blockLines, strings.TrimRight(cleanBracketedPasteArtifacts(cleanFirst), "\r\n"))
		for {
			l, rErr := readBoundedLine(reader, MaxScanCapacity)
			if rErr != nil {
				if errors.Is(rErr, io.EOF) && len(l) > 0 {
					blockLines = append(blockLines, strings.TrimRight(cleanBracketedPasteArtifacts(stripBracketedEnd(l)), "\r\n"))
				}
				break
			}
			if strings.Contains(l, BracketedPasteEnd) || strings.Contains(l, "^[[201~") || strings.Contains(l, "[201~") || strings.HasSuffix(strings.TrimSpace(l), "1~") {
				cleanEnd := cleanBracketedPasteArtifacts(stripBracketedEnd(l))
				if strings.TrimRight(cleanEnd, "\r\n") != "" || len(blockLines) == 0 {
					blockLines = append(blockLines, strings.TrimRight(cleanEnd, "\r\n"))
				}
				break
			}
			blockLines = append(blockLines, strings.TrimRight(cleanBracketedPasteArtifacts(l), "\r\n"))
		}
		joined := strings.Join(blockLines, "\n")
		return cleanBracketedPasteArtifacts(strings.TrimRight(joined, "\r\n")), nil
	}

	trimmedFirst := strings.TrimSpace(firstLine)

	// Case 1: Multi-line delimiter block (""" or ```)
	var delim string
	if strings.HasPrefix(trimmedFirst, "\"\"\"") {
		delim = "\"\"\""
	} else if strings.HasPrefix(trimmedFirst, "```") {
		delim = "```"
	}

	if delim != "" {
		rest := strings.TrimPrefix(trimmedFirst, delim)
		if strings.HasSuffix(rest, delim) && len(rest) >= len(delim) {
			return strings.TrimSpace(strings.TrimSuffix(rest, delim)), nil
		}
		var blockLines []string
		if rest != "" {
			blockLines = append(blockLines, rest)
		}
		for {
			if out != nil {
				fmt.Fprint(out, "... ")
			}
			l, rErr := readBoundedLine(reader, MaxScanCapacity)
			if rErr != nil {
				if errors.Is(rErr, io.EOF) && len(l) > 0 {
					blockLines = append(blockLines, strings.TrimRight(l, "\r\n"))
				}
				break
			}
			trimL := strings.TrimSpace(l)
			if strings.HasSuffix(trimL, delim) || trimL == delim {
				clean := strings.TrimSuffix(trimL, delim)
				if clean != "" {
					blockLines = append(blockLines, clean)
				}
				break
			}
			blockLines = append(blockLines, strings.TrimRight(l, "\r\n"))
		}
		return strings.Join(blockLines, "\n"), nil
	}

	// Case 2: Line continuation with backslash (\)
	if strings.HasSuffix(trimmedFirst, "\\") {
		var contLines []string
		contLines = append(contLines, strings.TrimSpace(strings.TrimSuffix(trimmedFirst, "\\")))
		for {
			if out != nil {
				fmt.Fprint(out, "... ")
			}
			nextL, rErr := readBoundedLine(reader, MaxScanCapacity)
			if rErr != nil {
				if errors.Is(rErr, io.EOF) && len(nextL) > 0 {
					contLines = append(contLines, strings.TrimSpace(nextL))
				}
				break
			}
			trimNext := strings.TrimSpace(nextL)
			if strings.HasSuffix(trimNext, "\\") {
				contLines = append(contLines, strings.TrimSpace(strings.TrimSuffix(trimNext, "\\")))
				continue
			}
			contLines = append(contLines, trimNext)
			break
		}
		return strings.Join(contLines, " "), nil
	}

	// Case 3: Explicit Multi-Line Mode (via /multiline toggle)
	if multiLine {
		if trimmedFirst == "EOF" || trimmedFirst == "/end" || trimmedFirst == "" {
			return "", nil
		}
		var lines []string
		lines = append(lines, strings.TrimRight(firstLine, "\r\n"))
		for {
			if out != nil {
				fmt.Fprint(out, "... ")
			}
			nextL, rErr := readBoundedLine(reader, MaxScanCapacity)
			if rErr != nil {
				if errors.Is(rErr, io.EOF) && len(nextL) > 0 {
					lines = append(lines, strings.TrimRight(nextL, "\r\n"))
				}
				break
			}
			trimNext := strings.TrimSpace(nextL)
			if trimNext == "" || trimNext == "EOF" || trimNext == "/end" {
				break
			}
			lines = append(lines, strings.TrimRight(nextL, "\r\n"))
		}
		return strings.Join(lines, "\n"), nil
	}

	// Case 4: Rapid multi-line paste detection in interactive terminals
	if IsInteractiveTerminalActive() {
		hasMore := reader.Buffered() > 0
		if !hasMore && runtime.GOOS == "windows" {
			hasMore = hasPendingConsoleEvents()
		}
		if hasMore {
			var pasteLines []string
			pasteLines = append(pasteLines, strings.TrimRight(firstLine, "\r\n"))
			for {
				nextL, rErr := readBoundedLine(reader, MaxScanCapacity)
				if rErr != nil {
					if errors.Is(rErr, io.EOF) && len(nextL) > 0 {
						pasteLines = append(pasteLines, strings.TrimRight(nextL, "\r\n"))
					}
					break
				}
				pasteLines = append(pasteLines, strings.TrimRight(nextL, "\r\n"))

				stillMore := reader.Buffered() > 0
				if !stillMore && runtime.GOOS == "windows" {
					stillMore = hasPendingConsoleEvents()
				}
				if !stillMore {
					time.Sleep(10 * time.Millisecond)
					stillMore = reader.Buffered() > 0
					if !stillMore && runtime.GOOS == "windows" {
						stillMore = hasPendingConsoleEvents()
					}
				}
				if !stillMore {
					break
				}
			}
			if len(pasteLines) > 1 {
				return strings.Join(pasteLines, "\n"), nil
			}
		}
	}

	return cleanBracketedPasteArtifacts(strings.TrimRight(firstLine, "\r\n")), nil
}

func readBoundedLine(reader *bufio.Reader, maxBytes int) (string, error) {
	var buf bytes.Buffer
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			if len(chunk) > 0 {
				buf.Write(chunk)
				return buf.String(), nil
			}
			return "", err
		}
		buf.Write(chunk)
		if buf.Len() > maxBytes {
			return "", fmt.Errorf("ui: input error: bufio.Scanner: token too long")
		}
		if !isPrefix {
			break
		}
	}
	return buf.String(), nil
}

// RunConversationalUI executes the interactive ANSI terminal prompt loop.
func RunConversationalUI(ctx context.Context, session *Session, in io.Reader, out io.Writer) error {
	if session == nil {
		return errors.New("ui: session cannot be nil")
	}
	defer session.Close()

	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}

	theme := NewTerminalTheme()
	PrintBanner(out, session)

	isTerm := IsTerminal(in)
	if isTerm {
		SetInteractiveTerminal(true)
		defer SetInteractiveTerminal(false)

		cleanupModes := SetupTerminalModes(in, out)
		defer cleanupModes()
	}

	reader := bufio.NewReaderSize(in, 4*1024*1024)

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, theme.Yellow("\nSession terminated by context cancellation."))
			return ctx.Err()
		default:
		}

		promptStr := fmt.Sprintf("%s ", theme.Bold(theme.BrightCyan("omnicoder>")))
		if session.IsMultiLine() {
			promptStr = fmt.Sprintf("%s ", theme.Bold(theme.BrightGreen("omnicoder [multiline]>")))
		}

		line, err := ReadPromptMultiLine(reader, out, promptStr, session.IsMultiLine())
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintln(out, theme.Dim("\nExiting OmniCoder-ICX. Goodbye!"))
				return nil
			}
			return fmt.Errorf("ui: input error: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Handle slash commands
		if strings.HasPrefix(line, "/") {
			parts := strings.Fields(line)
			cmd := strings.ToLower(parts[0])

			switch cmd {
			case "/help":
				PrintHelp(out)
			case "/status":
				PrintStatus(out, session)
			case "/model":
				if len(parts) > 1 {
					newModel := parts[1]
					session.SetModel(newModel)
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightGreen("[Model Updated]")),
						theme.Green(fmt.Sprintf("Active model switched to: %s", newModel)))
				} else {
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.Cyan("[Active Model]")),
						session.Model())
					fmt.Fprintf(out, "  %s %s\n",
						theme.Dim("Latest Gemini 3.5+ Models:"),
						"gemini-3.8-flash (flagship), gemini-3.8-live, gemini-3.7-flash, gemini-3.6-flash, gemini-3.5-flash, gemini-3.5-flash-lite")
				}
			case "/memory", "/surfaces":
				printMemorySurfaces(out, session)
			case "/reset":
				session.Reset()
				fmt.Fprintln(out, theme.Green("[Session] Dialogue history cleared. Workspace memory and 4D lattice preserved."))
			case "/continue":
				activeProj := session.ActiveProject()
				lastRec, hasRec := session.LastBuildRecord()
				var contPrompt string
				if hasRec && lastRec.Diagnostic != "" && lastRec.Diagnostic != "SUCCESS" {
					contPrompt = fmt.Sprintf("Continue implementing and fixing the remaining files/tests in %s. Remediate this failure from command %q:\n%s\nEnsure all test suites pass with at least one executed test.", activeProj, lastRec.Command, lastRec.Diagnostic)
				} else if activeProj != "" {
					contPrompt = fmt.Sprintf("Continue completing and verifying all planned files and test suites in %s. Ensure all test suites pass with at least one executed test.", activeProj)
				} else {
					contPrompt = "Continue the implementation and ensure all tests compile and pass cleanly."
				}
				if session.LLMClient() == nil {
					_ = executeTask(ctx, session, contPrompt, out)
				} else {
					_ = executeConversationalTurn(ctx, session, contPrompt, out)
				}
				continue
			case "/exit", "/quit":
				fmt.Fprintln(out, theme.Dim("Exiting OmniCoder-ICX. Goodbye!"))
				return nil
			case "/map":
				budget := 4000
				query := ""
				if len(parts) > 1 {
					if b, err := strconv.Atoi(parts[1]); err == nil && b > 0 {
						budget = b
						if len(parts) > 2 {
							query = strings.Join(parts[2:], " ")
						}
					} else {
						query = strings.Join(parts[1:], " ")
					}
				}
				_, _ = executeMap(ctx, session, budget, query, out)
			case "/plan", "/run", "/build", "/goal", "/boost":
				task := strings.TrimSpace(strings.TrimPrefix(line, parts[0]))
				// Support chained slash commands e.g. /boost /goal complete or /goal complete
				for strings.HasPrefix(task, "/") {
					f := strings.Fields(task)
					if len(f) > 0 {
						task = strings.TrimSpace(strings.TrimPrefix(task, f[0]))
					} else {
						break
					}
				}
				if task == "" {
					fmt.Fprintf(out, "%s\n", theme.Yellow(fmt.Sprintf("Usage: %s <task description>", parts[0])))
					continue
				}
				cleanTarget := strings.TrimSpace(task)
				planPathUsed, planContent := ResolvePlanFile(cleanTarget, session.WorkspaceDir())
				if planContent != "" {
					fmt.Fprintf(out, "%s %s\n",
						theme.Bold(theme.BrightGreen("[Plan Detected]")),
						theme.Green(fmt.Sprintf("Loaded master implementation plan: %s (%d bytes)", planPathUsed, len(planContent))))
					task = FormatPlanPrompt(cleanTarget, planPathUsed, planContent, session.WorkspaceDir())
					spec := engine.DetectPlanSpec(planPathUsed, session.WorkspaceDir())
					if spec == nil {
						spec = engine.ParsePlanMarkdown(planContent)
					}
					if spec != nil {
						session.SetActivePlan(spec, planPathUsed)
						if spec.ProjectScope != "" {
							session.SetActiveProject(spec.ProjectScope)
						}
					}
				}
				if session.LLMClient() == nil {
					_ = executeTask(ctx, session, task, out)
				} else {
					_ = executeConversationalTurn(ctx, session, task, out)
				}
			case "/heal":
				commandToHeal := strings.TrimSpace(strings.TrimPrefix(line, parts[0]))
				if commandToHeal == "" {
					fmt.Fprintln(out, theme.Yellow("Usage: /heal <command>"))
					continue
				}
				_ = executeHeal(ctx, session, commandToHeal, out)
			case "/refactor":
				if len(parts) < 3 {
					fmt.Fprintln(out, theme.Yellow("Usage: /refactor <symbol> <newSignature>"))
					continue
				}
				symbol := parts[1]
				// Robustly extract newSig by locating parts[0] and symbol in line
				cmdIdx := strings.Index(strings.ToLower(line), strings.ToLower(parts[0]))
				var newSig string
				if cmdIdx != -1 {
					afterCmd := line[cmdIdx+len(parts[0]):]
					symIdx := strings.Index(afterCmd, symbol)
					if symIdx != -1 {
						newSig = strings.TrimSpace(afterCmd[symIdx+len(symbol):])
					}
				}
				if newSig == "" {
					fmt.Fprintln(out, theme.Yellow("Usage: /refactor <symbol> <newSignature>"))
					continue
				}
				_ = executeRefactor(ctx, session, symbol, newSig, out)
			case "/save", "/write":
				cmdIdx := strings.Index(strings.ToLower(line), strings.ToLower(parts[0]))
				targetPath := ""
				if cmdIdx != -1 {
					targetPath = strings.TrimSpace(line[cmdIdx+len(parts[0]):])
					targetPath = strings.Trim(targetPath, "\"'")
					targetPath = strings.TrimSpace(targetPath)
				}
				if targetPath == "" {
					fmt.Fprintf(out, "%s\n", theme.Yellow(fmt.Sprintf("Usage: %s <path>", cmd)))
					continue
				}
				lastMsg := session.LastAssistantMessage()
				if strings.TrimSpace(lastMsg) == "" {
					fmt.Fprintln(out, theme.Yellow("[Notice] No previous assistant response in history to save."))
					continue
				}
				var content string
				intents := ParseAllActionIntents(lastMsg)
				var matchedIntent *ActionIntent
				for _, it := range intents {
					if strings.EqualFold(strings.TrimSpace(it.Path()), strings.TrimSpace(targetPath)) {
						matchedIntent = it
						break
					}
				}
				if matchedIntent != nil {
					content = matchedIntent.ExtractContent(lastMsg)
				} else if len(intents) == 1 {
					content = intents[0].ExtractContent(lastMsg)
				} else {
					content = ExtractContent(lastMsg)
				}
				_ = executeWrite(session, targetPath, content, out)
			case "/paste":
				arg := ""
				if len(parts) > 1 {
					arg = strings.ToLower(parts[1])
				}
				if arg != "manual" && arg != "-m" {
					clipText, err := GetClipboardText()
					if err == nil && len(strings.TrimSpace(clipText)) > 0 {
						lines := strings.Split(clipText, "\n")
						fmt.Fprintf(out, "%s %s\n",
							theme.Bold(theme.BrightGreen("[Clipboard Ingested]")),
							theme.Green(fmt.Sprintf("%d lines (%d bytes) captured from clipboard", len(lines), len(clipText))))
						limit := 4
						if len(lines) < limit {
							limit = len(lines)
						}
						for i := 0; i < limit; i++ {
							fmt.Fprintf(out, "  %s %s\n", theme.Dim(">"), theme.Dim(lines[i]))
						}
						if len(lines) > limit {
							fmt.Fprintf(out, "  %s\n", theme.Dim(fmt.Sprintf("... (%d more lines)", len(lines)-limit)))
						}
						line = strings.TrimSpace(clipText)
						goto ExecuteUserPrompt
					}
				}

				fmt.Fprintln(out, theme.Bold(theme.BrightCyan("[Interactive Paste Mode Active]")))
				fmt.Fprintln(out, theme.Cyan("Paste your content below. Type 'EOF' or '/end' on an empty line (or press Ctrl+Z/Ctrl+D) to submit:"))
				var pasteLines []string
				for {
					if out != nil {
						fmt.Fprint(out, "... ")
					}
					pLine, pErr := readBoundedLine(reader, MaxScanCapacity)
					if pErr != nil {
						if errors.Is(pErr, io.EOF) && len(pLine) > 0 {
							pasteLines = append(pasteLines, strings.TrimRight(pLine, "\r\n"))
						}
						break
					}
					trimP := strings.TrimSpace(pLine)
					if trimP == "EOF" || trimP == "/end" {
						break
					}
					pasteLines = append(pasteLines, strings.TrimRight(pLine, "\r\n"))
				}
				if len(pasteLines) == 0 {
					fmt.Fprintln(out, theme.Yellow("[Paste Mode] No content provided."))
					continue
				}
				line = strings.TrimSpace(strings.Join(pasteLines, "\n"))
				if line == "" {
					continue
				}
				goto ExecuteUserPrompt

			case "/multiline", "/ml":
				active := session.ToggleMultiLine()
				if active {
					fmt.Fprintln(out, theme.Bold(theme.BrightGreen("[Multi-Line Mode: ON]"))+theme.Green(" Press Enter on an empty line or type 'EOF' to submit prompt. Type /multiline to disable."))
				} else {
					fmt.Fprintln(out, theme.Bold(theme.Yellow("[Multi-Line Mode: OFF]"))+theme.Yellow(" Standard single-line prompt mode active."))
				}
				continue
			default:
				// If the line starts with / but looks like a Unix path, terminal error, or diagnostic output
				// (e.g. "/bin/bash: bundler: command not found", "/usr/bin/python: ...", "/c/Users/..."),
				// do NOT treat it as an unknown slash command; execute it as a diagnostic prompt!
				isReport, _, _ := DetectUserErrorReport(line)
				hasAlphaPath := strings.Count(cmd, "/") > 1 && strings.Trim(cmd, "/") != ""
				hasColonPrefix := strings.Contains(cmd, ":") && strings.Trim(cmd, "/:") != ""
				if isReport || hasColonPrefix || (hasAlphaPath && (strings.Contains(line, "not found") || strings.Contains(line, "error"))) {
					goto ExecuteUserPrompt
				}
				fmt.Fprintf(out, "%s\n", theme.Yellow(fmt.Sprintf("Unknown command '%s'. Type /help for available commands.", cmd)))
			}
			continue
		}

	ExecuteUserPrompt:
		// Conversational Intent Trigger: swarm / complete project
		if match := SwarmIntentRegex.FindStringSubmatch(strings.TrimSpace(line)); len(match) > 3 {
			planPath := strings.Trim(match[3], "\"'")
			if sHandler := session.SwarmHandler(); sHandler != nil {
				fmt.Fprintf(out, "%s %s\n",
					theme.Bold(theme.BrightMagenta("[Swarm Intent Detected]")),
					theme.Magenta(fmt.Sprintf("Auto-delegating to Swarm Forge for plan: %s", planPath)))
				if err := sHandler(ctx, planPath, out); err != nil {
					fmt.Fprintf(out, "%s %v\n", theme.Red("[Swarm Error]"), err)
				}
				continue
			}
		}

		// Handle direct build or run commands without leading slash
		fields := strings.Fields(line)
		firstWord := ""
		if len(fields) > 0 {
			firstWord = strings.ToLower(fields[0])
		}

		userPrompt := line
		if firstWord == "continue" || firstWord == "/continue" {
			activeProj := session.ActiveProject()
			lastRec, hasRec := session.LastBuildRecord()
			if hasRec && lastRec.Diagnostic != "" && lastRec.Diagnostic != "SUCCESS" {
				userPrompt = fmt.Sprintf("Continue implementing and fixing the remaining files/tests in %s. Remediate this failure from command %q:\n%s\nEnsure all test suites pass with at least one executed test.", activeProj, lastRec.Command, lastRec.Diagnostic)
			} else if activeProj != "" {
				userPrompt = fmt.Sprintf("Continue completing and verifying all planned files and test suites in %s. Ensure all test suites pass with at least one executed test.", activeProj)
			} else {
				userPrompt = "Continue the implementation and ensure all tests compile and pass cleanly."
			}
		} else if firstWord == "build" || firstWord == "run" || firstWord == "goal" || firstWord == "boost" || firstWord == "plan" {
			taskArg := strings.TrimSpace(line[len(fields[0]):])
			for strings.HasPrefix(taskArg, "/") || strings.HasPrefix(strings.ToLower(taskArg), "goal") || strings.HasPrefix(strings.ToLower(taskArg), "boost") || strings.HasPrefix(strings.ToLower(taskArg), "plan") {
				f := strings.Fields(taskArg)
				if len(f) > 0 {
					taskArg = strings.TrimSpace(taskArg[len(f[0]):])
				} else {
					break
				}
			}
			if taskArg == "" && (firstWord == "goal" || firstWord == "boost" || firstWord == "plan") {
				taskArg = "complete"
			}
			if taskArg != "" {
				userPrompt = taskArg
			}
		}

		cleanTarget := strings.TrimSpace(userPrompt)
		planPathUsed, planContent := ResolvePlanFile(cleanTarget, session.WorkspaceDir())
		if planContent != "" {
			fmt.Fprintf(out, "%s %s\n",
				theme.Bold(theme.BrightGreen("[Plan Detected]")),
				theme.Green(fmt.Sprintf("Loaded master implementation plan: %s (%d bytes)", planPathUsed, len(planContent))))
			userPrompt = FormatPlanPrompt(cleanTarget, planPathUsed, planContent, session.WorkspaceDir())
			spec := engine.DetectPlanSpec(planPathUsed, session.WorkspaceDir())
			if spec == nil {
				spec = engine.ParsePlanMarkdown(planContent)
			}
			if spec != nil {
				session.SetActivePlan(spec, planPathUsed)
				if spec.ProjectScope != "" {
					session.SetActiveProject(spec.ProjectScope)
				}
			}
		}

		if session.LLMClient() == nil {
			_ = executeTask(ctx, session, userPrompt, out)
			continue
		}

		_ = executeConversationalTurn(ctx, session, userPrompt, out)
	}
}

// stripRaceIfCgoUnavailable removes the -race flag on Windows if CGo is unavailable,
// avoiding false test failures due to missing GCC/CGo tooling.
func stripRaceIfCgoUnavailable(cmd string) string {
	if !strings.Contains(cmd, "-race") {
		return cmd
	}
	if runtime.GOOS == "windows" {
		cgoEnv := os.Getenv("CGO_ENABLED")
		hasGcc := false
		if _, err := exec.LookPath("gcc"); err == nil {
			hasGcc = true
		}
		// If gcc is missing or CGO_ENABLED is not explicitly 1, strip -race on Windows
		if !hasGcc || cgoEnv != "1" {
			parts := strings.Fields(cmd)
			var filtered []string
			for _, p := range parts {
				if p != "-race" {
					filtered = append(filtered, p)
				}
			}
			return strings.Join(filtered, " ")
		}
	}
	return cmd
}

// normalizeBuildCommand removes inline -C flags and redundant scope path prefixes
// (e.g. 'go test -C projects/my-app ./tests/...' or './projects/my-app/tests/...')
// ensuring commands execute cleanly inside buildDir without duplicate directory traversal.
func normalizeBuildCommand(cmd, buildDir, ws string) string {
	res := strings.TrimSpace(cmd)
	// 1. Strip -C, --prefix, --cwd flags and their arguments
	cRegex := regexp.MustCompile(`(?:\s+|^)-C(?:\s+|=)(?:"([^"]+)"|'([^']+)'|([^\s]+))`)
	res = strings.TrimSpace(cRegex.ReplaceAllString(res, ""))
	prefixRegex := regexp.MustCompile(`(?:\s+|^)(?:--prefix|--cwd)(?:\s+|=)(?:"([^"]+)"|'([^']+)'|([^\s]+))`)
	res = strings.TrimSpace(prefixRegex.ReplaceAllString(res, ""))

	// 2. Strip redundant scope prefixes if buildDir ends with that scope
	cleanBuildDir := strings.ReplaceAll(filepath.ToSlash(buildDir), "\\", "/")
	for _, prefix := range []string{"/projects/", "/apps/"} {
		if idx := strings.Index(cleanBuildDir, prefix); idx != -1 {
			scope := cleanBuildDir[idx+1:]
			for _, sc := range []string{scope, strings.ReplaceAll(scope, "/", `\`)} {
				sep := "/"
				if strings.Contains(sc, `\`) {
					sep = `\`
				}
				patterns := []string{"./" + sc + sep, "." + sep + sc + sep, sc + sep}
				for _, p := range patterns {
					if strings.Contains(res, p) {
						res = strings.ReplaceAll(res, p, "./")
					}
				}
				res = strings.ReplaceAll(res, "./"+sc+sep+"...", "./...")
				res = strings.ReplaceAll(res, sc+sep+"...", "./...")
				// Bare scope token (e.g. `unittest discover -s projects/foo`) -> current dir
				bareRegex := regexp.MustCompile(`(^|[\s"'=])(?:\.[/\\])?` + regexp.QuoteMeta(sc) + `([\s"']|$)`)
				for bareRegex.MatchString(res) {
					res = bareRegex.ReplaceAllString(res, "${1}.${2}")
				}
			}
		}
	}

	// 3. Unescape stray escaped quotes in command line (e.g. python -c \"...\" or node -e \"...\")
	if strings.Contains(res, `\"`) {
		res = strings.ReplaceAll(res, `\"`, `"`)
	}
	if strings.Contains(res, `\'`) {
		res = strings.ReplaceAll(res, `\'`, `'`)
	}

	// 4. Safely normalize single-quoted arguments on Windows (e.g. -p 'test_*.py' -> -p test_*.py)
	// cmd.exe treats single quotes as literal characters, corrupting glob patterns and flags.
	if runtime.GOOS == "windows" {
		singleQuoteArgRe := regexp.MustCompile(`(^|[\s=])'([^'\r\n]*)'([\s]|$)`)
		for singleQuoteArgRe.MatchString(res) {
			res = singleQuoteArgRe.ReplaceAllStringFunc(res, func(m string) string {
				match := singleQuoteArgRe.FindStringSubmatch(m)
				if len(match) < 4 {
					return m
				}
				prefix := match[1]
				inner := match[2]
				suffix := match[3]
				if strings.ContainsAny(inner, " \t") {
					return prefix + `"` + inner + `"` + suffix
				}
				return prefix + inner + suffix
			})
		}
	}

	return devops.NormalizeWhitespaceSafe(res)
}
