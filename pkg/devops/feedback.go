// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// feedback.go implements multi-language compiler & test runner diagnostics parsing,
// ANSI stripping, 5-line source context extraction, and token-minimized repair payloads (<300 tokens).

package devops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// CompilerDiagnostic represents a structured compiler, linter, or test error.
type CompilerDiagnostic struct {
	File          string   `json:"file"`
	Line          int      `json:"line"`
	Column        int      `json:"column,omitempty"`
	Severity      string   `json:"severity"` // "ERROR", "WARNING"
	ErrorCode     string   `json:"error_code,omitempty"`
	Message       string   `json:"message"`
	OffendingLine string   `json:"offending_line,omitempty"`
	ContextLines  []string `json:"context_lines,omitempty"` // 5 lines: [errLine-2, errLine+2]
}

// CompilerFeedbackRequest encapsulates execution and diagnostic parsing parameters.
type CompilerFeedbackRequest struct {
	Command                   string    `json:"command"`
	WorkspaceDir              string    `json:"workspace_dir"`
	LanguageHint              string    `json:"language_hint,omitempty"`
	TimeoutSeconds            int       `json:"timeout_seconds,omitempty"`
	DeterministicMockStderr   string    `json:"deterministic_mock_stderr,omitempty"`
	DeterministicMockStdout   string    `json:"deterministic_mock_stdout,omitempty"`
	DeterministicMockExitCode int       `json:"deterministic_mock_exit_code,omitempty"`
	ExtraEnv                  []string  `json:"extra_env,omitempty"`
	ExecutionBackend          string    `json:"execution_backend,omitempty"` // "Host_Subprocess", "Calera_MicroVM", "Deterministic_Mock"
	SandboxID                 string    `json:"sandbox_id,omitempty"`
	VMClient                  vm.Client `json:"-"`
}

// CompilerFeedbackReport reports the execution outcome with extracted diagnostics and repair prompt.
type CompilerFeedbackReport struct {
	Success             bool                 `json:"success"`
	ExitCode            int                  `json:"exit_code"`
	Command             string               `json:"command,omitempty"`
	RawStdout           string               `json:"raw_stdout,omitempty"`
	RawStderr           string               `json:"raw_stderr,omitempty"`
	CleanedStderr       string               `json:"cleaned_stderr,omitempty"`
	Diagnostics         []CompilerDiagnostic `json:"diagnostics,omitempty"`
	ErrorCount          int                  `json:"error_count"`
	RepairPromptPayload string               `json:"repair_prompt_payload,omitempty"` // <300 tokens
	ExecutionEngine     string               `json:"execution_engine,omitempty"`
}

var (
	// ANSI escape stripper: handles CSI (\x1b[...letter) and OSC (\x1b]...bell/slash)
	ansiStripRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\].*?(\x07|\x1b\\)`)

	// Go error regex: path/to/file.go:12:34: message or path/to/file.go:12: message (supports Windows drive letters, spaces, Unicode)
	goErrorRegex = regexp.MustCompile(`^(?:#\s+[^\r\n]+\s+)?((?:[a-zA-Z]:)?[^:\r\n]+?\.go):(\d+)(?::(\d+))?:\s*(.*)$`)

	// Go panic regex: panic: message
	goPanicRegex = regexp.MustCompile(`^panic:\s*(.*)$`)

	// Go stack trace regex: \tpath/to/file.go:12 or path/to/file.go:12 +0x25
	goStackTraceRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^:\r\n]+?\.go):(\d+)(?:\s+\+0x[0-9a-fA-F]+)?$`)

	// Rust / Cargo error regex:
	rustCargoErrorRegex    = regexp.MustCompile(`^(error|warning)(?:\[([A-Z0-9]+)\])?:\s*(.*)$`)
	rustCargoLocationRegex = regexp.MustCompile(`^\s*(?:-->\s*)?((?:[a-zA-Z]:)?[^:\r\n]+?\.rs):(\d+)(?::(\d+))?`)

	// TypeScript / tsc error regex:
	tsParenErrorRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^(\r\n]+?\.tsx?)\((\d+),(\d+)\):\s*(error|warning)\s*(TS\d+)?:\s*(.*)$`)
	tsColonErrorRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^:\r\n]+?\.tsx?):(\d+):(\d+)\s*-\s*(error|warning)\s*(TS\d+)?:\s*(.*)$`)

	// Jest / Vitest test failure regexes:
	jestFailFileRegex   = regexp.MustCompile(`^(?:FAIL|✕)\s+((?:[a-zA-Z]:)?[^:\r\n]+?\.(?:test|spec)\.[tj]sx?|[^\s:\r\n]+?\.[tj]sx?)`)
	jestStackTraceRegex = regexp.MustCompile(`^\s*at\s+(?:.*?\s+\()?((?:[a-zA-Z]:)?[^:\r\n()]+?\.[tj]sx?):(\d+):(\d+)\)?$`)
	jestTestNameRegex   = regexp.MustCompile(`^\s*(?:●|✕)\s+(.*?)\s*(?:›\s*(.*))?$`)

	// Python error regex:
	pyMypyErrorRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^:\r\n]+?\.py):(\d+)(?::(\d+))?:\s*(error|warning):\s*(.*)$`)
	pyTracebackRegex = regexp.MustCompile(`^\s*File\s+"([^"]+\.py)",\s*line\s+(\d+)(?:,\s*in\s+([a-zA-Z0-9_<>]+))?`)
	pyTestErrorRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^:\r\n]+?\.py):(\d+):\s*([A-Za-z0-9_]+Error|AssertionError|Exception|Failed)(?::\s*(.*))?$`)
	pyTestTraceRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^:\r\n]+?\.py):(\d+):(?:\s*in\s+(.*))?$`)
	pyTestSummaryFailedRegex = regexp.MustCompile(`^FAILED\s+([^\s:]+\.py)(?:::([^\s-]+))?\s+-\s+(.+)$`)

	// C / C++ error regex:
	cCppErrorRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^:\r\n]+?\.(?:c|cpp|cc|cxx|h|hpp)):(\d+):(\d+):\s*(error|warning):\s*(.*)$`)
	msvcErrorRegex = regexp.MustCompile(`^((?:[a-zA-Z]:)?[^(\r\n]+?\.(?:c|cpp|cc|cxx|h|hpp))\((\d+)(?:,(\d+))?\):\s*(error|warning)\s*(C\d+)?:\s*(.*)$`)

	// Go goroutine dump regexes:
	goGoroutineHeaderRegex = regexp.MustCompile(`^goroutine\s+(\d+)\s+\[([^\]]+)\]:`)
	goRunningTestRegex     = regexp.MustCompile(`^\s*(Test[A-Za-z0-9_]+)\s*\([0-9.]+s\)`)
)

// StripANSI removes terminal escape sequences from output text.
func StripANSI(input string) string {
	return ansiStripRegex.ReplaceAllString(input, "")
}

// EstimateTokens provides a deterministic token estimation for text (<300 tokens budget validation).
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	t := int(float64(len(text)) / 3.5)
	if t < 1 {
		return 1
	}
	return t
}

// InjectGoTestTimeout injects an internal -timeout flag into a 'go test' command
// if one is not already present, ensuring Go's test runner panics and dumps goroutines
// before the outer subprocess context deadline kills the process.
func InjectGoTestTimeout(cmdStr string, timeoutSeconds int) string {
	if !strings.Contains(cmdStr, "go test") {
		return cmdStr
	}
	if strings.Contains(cmdStr, "-timeout") || strings.Contains(cmdStr, "--timeout") {
		return cmdStr
	}
	intTimeout := timeoutSeconds - 5
	if intTimeout < 5 {
		intTimeout = 5
	}
	if intTimeout > 45 {
		intTimeout = 45
	}
	flag := fmt.Sprintf("-timeout=%ds", intTimeout)
	reGoTest := regexp.MustCompile(`(^|\s|&&|;)(go\s+test)($|\s)`)
	return reGoTest.ReplaceAllString(cmdStr, fmt.Sprintf("${1}${2} %s${3}", flag))
}

// resolveTargetDir resolves a target directory path relative to current directory, parent, or ancestors.
func resolveTargetDir(currentDir, targetDir string) string {
	cleanTarget := strings.Trim(targetDir, `"'` + "`")
	cleanTarget = strings.TrimSpace(cleanTarget)
	if cleanTarget == "" || cleanTarget == "." {
		return currentDir
	}
	if filepath.IsAbs(cleanTarget) {
		if fi, err := os.Stat(cleanTarget); err == nil && fi.IsDir() {
			return cleanTarget
		}
	}
	candidate := filepath.Join(currentDir, filepath.FromSlash(cleanTarget))
	if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
		return candidate
	}
	// Check if already inside target directory (e.g. currentDir ends with targetDir or base matches)
	if strings.EqualFold(filepath.Base(currentDir), filepath.Base(cleanTarget)) {
		return currentDir
	}
	// Check parent directory
	candidateParent := filepath.Join(filepath.Dir(currentDir), filepath.FromSlash(cleanTarget))
	if fi, err := os.Stat(candidateParent); err == nil && fi.IsDir() {
		return candidateParent
	}
	// Check grandparent directory
	candidateGP := filepath.Join(filepath.Dir(filepath.Dir(currentDir)), filepath.FromSlash(cleanTarget))
	if fi, err := os.Stat(candidateGP); err == nil && fi.IsDir() {
		return candidateGP
	}
	return currentDir
}

// killProcessTree terminates the command process and all its children.
// On Windows, taskkill /T /F /PID ensures child processes (node.exe, npm, python, etc.) are terminated,
// preventing orphaned processes from holding open stdio pipes and causing client EOF errors or hangs.
func killProcessTree(cmd *exec.Cmd) {
	vm.KillProcessTree(cmd)
}

// RunCompilerFeedbackLoop executes target build/test command, parses errors, and generates repair payload.
func RunCompilerFeedbackLoop(ctx context.Context, req CompilerFeedbackRequest) (*CompilerFeedbackReport, error) {
	if req.WorkspaceDir == "" {
		req.WorkspaceDir = "."
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 30
	}
	if strings.Contains(req.Command, "go test") {
		req.Command = InjectGoTestTimeout(req.Command, req.TimeoutSeconds)
	}

	var stdoutStr, stderrStr string
	var exitCode int
	engine := "Host_Subprocess"

	if req.DeterministicMockStderr != "" || req.DeterministicMockExitCode != 0 || req.DeterministicMockStdout != "" {
		stderrStr = req.DeterministicMockStderr
		stdoutStr = req.DeterministicMockStdout
		exitCode = req.DeterministicMockExitCode
		engine = "Deterministic_Mock"
	} else if (req.ExecutionBackend == "Calera_MicroVM" || req.ExecutionBackend == "microvm") && req.VMClient != nil {
		sbID := req.SandboxID
		if sbID == "" {
			sb, err := req.VMClient.CreateSandbox(ctx, vm.SandboxConfig{
				WorkspaceDir:   req.WorkspaceDir,
				TimeoutSeconds: req.TimeoutSeconds,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create microVM sandbox: %w", err)
			}
			sbID = sb.ID
			defer func() {
				_ = req.VMClient.DestroySandbox(context.Background(), sbID)
			}()
		}

		res, err := req.VMClient.RunCommand(ctx, vm.CommandRequest{
			SandboxID:      sbID,
			Command:        req.Command,
			Workdir:        req.WorkspaceDir,
			TimeoutSeconds: req.TimeoutSeconds,
		})
		if err != nil {
			return nil, fmt.Errorf("microVM execution failed: %w", err)
		}
		stdoutStr = res.Stdout
		stderrStr = res.Stderr
		exitCode = res.ExitCode
		engine = res.ExecutionEngine
		if res.Summary != "" && !strings.Contains(stderrStr, res.Summary) {
			if stderrStr != "" {
				stderrStr = stderrStr + "\n" + res.Summary
			} else {
				stderrStr = res.Summary
			}
		}
	} else {
		tCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()

		currentDir := req.WorkspaceDir
		if abs, err := filepath.Abs(currentDir); err == nil {
			currentDir = abs
		}
		currentCmd := strings.TrimSpace(req.Command)

		// Regex to extract leading cd <path> && or cd <path> ;
		cdRegex := regexp.MustCompile(`(?i)^\s*cd\s+(?:"([^"]+)"|'([^']+)'|([^\s&;]+))\s*(?:&&|;)\s*(.*)$`)
		for {
			m := cdRegex.FindStringSubmatch(currentCmd)
			if len(m) == 0 {
				break
			}
			tDir := m[1]
			if tDir == "" {
				tDir = m[2]
			}
			if tDir == "" {
				tDir = m[3]
			}
			currentCmd = strings.TrimSpace(m[4])
			currentDir = resolveTargetDir(currentDir, tDir)
		}

		// Extract and resolve -C <path> flag if present (e.g. 'go mod tidy -C projects/my-app' or 'go test -C <dir>')
		cRegex := regexp.MustCompile(`(?:\s+|^)-C(?:\s+|=)(?:"([^"]+)"|'([^']+)'|([^\s]+))`)
		if m := cRegex.FindStringSubmatch(currentCmd); len(m) > 0 {
			tDir := m[1]
			if tDir == "" {
				tDir = m[2]
			}
			if tDir == "" {
				tDir = m[3]
			}
			currentDir = resolveTargetDir(currentDir, tDir)
			currentCmd = strings.TrimSpace(cRegex.ReplaceAllString(currentCmd, ""))
		}

		// Extract and resolve npm/yarn/pnpm --prefix <path> or --cwd <path> flag if present
		prefixRegex := regexp.MustCompile(`(?:\s+|^)(?:--prefix|--cwd)(?:\s+|=)(?:"([^"]+)"|'([^']+)'|([^\s]+))`)
		if m := prefixRegex.FindStringSubmatch(currentCmd); len(m) > 0 {
			tDir := m[1]
			if tDir == "" {
				tDir = m[2]
			}
			if tDir == "" {
				tDir = m[3]
			}
			currentDir = resolveTargetDir(currentDir, tDir)
			currentCmd = strings.TrimSpace(prefixRegex.ReplaceAllString(currentCmd, ""))
		}

		// Normalize redundant scope path prefixes if currentDir already points to or ends with that scope
		// (e.g. currentDir is .../projects/my-app and command has ./projects/my-app/tests/... or projects/my-app/...)
		cleanCurDir := filepath.ToSlash(currentDir)
		for _, prefix := range []string{"/projects/", "/apps/"} {
			if idx := strings.Index(cleanCurDir, prefix); idx != -1 {
				scope := cleanCurDir[idx+1:] // e.g. "projects/my-app"
				patterns := []string{"./" + scope + "/", scope + "/"}
				for _, p := range patterns {
					if strings.Contains(currentCmd, p) {
						currentCmd = strings.ReplaceAll(currentCmd, p, "./")
					}
				}
				currentCmd = strings.ReplaceAll(currentCmd, "./"+scope+"/...", "./...")
				currentCmd = strings.ReplaceAll(currentCmd, scope+"/...", "./...")
			}
		}

		// Normalize whitespace safely preserving spaces inside quotes
		currentCmd = NormalizeWhitespaceSafe(currentCmd)
		if strings.Contains(currentCmd, `\"`) {
			currentCmd = strings.ReplaceAll(currentCmd, `\"`, `"`)
		}
		if strings.Contains(currentCmd, `\'`) {
			currentCmd = strings.ReplaceAll(currentCmd, `\'`, `'`)
		}

		if currentCmd == "" {
			// No command to execute after cd, considered success
			return &CompilerFeedbackReport{
				Success:         true,
				ExitCode:        0,
				Command:         req.Command,
				ExecutionEngine: "Directory_Change_Only",
			}, nil
		}

		// Safe Python -c execution: extract inline script to temporary file to avoid shell quoting and '&' command splitting hazards
		var tempScriptPath string
		var pyPrefix string
		normLower := strings.ToLower(strings.TrimSpace(currentCmd))
		if strings.HasPrefix(normLower, "python -c ") {
			pyPrefix = "python"
		} else if strings.HasPrefix(normLower, "python3 -c ") {
			pyPrefix = "python3"
		} else if strings.HasPrefix(normLower, "py -c ") {
			pyPrefix = "py"
		}

		if pyPrefix != "" {
			idx := strings.Index(currentCmd, "-c ")
			if idx != -1 {
				rawCode := strings.TrimSpace(currentCmd[idx+3:])
				if (strings.HasPrefix(rawCode, "\"") && strings.HasSuffix(rawCode, "\"")) ||
					(strings.HasPrefix(rawCode, "'") && strings.HasSuffix(rawCode, "'")) {
					if len(rawCode) >= 2 {
						rawCode = rawCode[1 : len(rawCode)-1]
					}
				}
				tmpDir := filepath.Join(os.TempDir(), "omnicoder_scripts")
				_ = os.MkdirAll(tmpDir, 0755)
				tmpScript := filepath.Join(tmpDir, fmt.Sprintf("verify_%d.py", time.Now().UnixNano()))
				if err := os.WriteFile(tmpScript, []byte(rawCode), 0644); err == nil {
					tempScriptPath = tmpScript
					defer os.Remove(tmpScript)
				}
			}
		}

		if tempScriptPath == "" && (strings.Contains(currentCmd, "&&") || strings.Contains(currentCmd, "||") || strings.Contains(currentCmd, ";")) {
			chainedRes, cErr := vm.ExecuteChainedCommand(tCtx, vm.ChainedExecConfig{
				InitialWorkdir: currentDir,
				BaseEnv:        append(os.Environ(), req.ExtraEnv...),
				TimeoutSeconds: req.TimeoutSeconds,
			}, currentCmd)
			if cErr != nil && errors.Is(cErr, context.DeadlineExceeded) {
				exitCode = 124
				if stderrStr != "" {
					stderrStr += "\n"
				}
				stderrStr += fmt.Sprintf("COMMAND_TIMEOUT: execution exceeded timeout threshold (%ds). Suspected infinite loop, channel deadlock, or mutex blockage.", req.TimeoutSeconds)
			} else if cErr != nil {
				return nil, fmt.Errorf("chained command execution failed: %w", cErr)
			} else {
				stdoutStr = chainedRes.Stdout
				stderrStr = chainedRes.Stderr
				exitCode = chainedRes.ExitCode
				engine = "Host_Chained_Engine"
			}
		} else {
			var cmd *exec.Cmd
			if tempScriptPath != "" {
				cmd = exec.CommandContext(tCtx, pyPrefix, tempScriptPath)
				cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
			} else if strings.HasPrefix(currentCmd, "powershell") || strings.HasPrefix(currentCmd, "pwsh") {
				cmd = exec.CommandContext(tCtx, "powershell", "-NoProfile", "-Command", currentCmd)
			} else if runtime.GOOS == "windows" {
				useDirect := false
				var parts []string
				if !strings.ContainsAny(currentCmd, "&|;><^()") {
					parts = strings.Fields(currentCmd)
					if len(parts) > 0 {
						bin := strings.ToLower(parts[0])
						cmdBuiltins := map[string]bool{
							"echo": true, "dir": true, "type": true, "copy": true,
							"del": true, "mkdir": true, "md": true, "rmdir": true,
							"rd": true, "cls": true, "set": true, "cd": true, "rem": true,
						}
						if !cmdBuiltins[bin] {
							if _, lookErr := exec.LookPath(parts[0]); lookErr == nil {
								useDirect = true
							}
						}
					}
				}
				if useDirect {
					cleanParts := make([]string, len(parts))
					for i, p := range parts {
						if len(p) >= 2 && strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") {
							cleanParts[i] = p[1 : len(p)-1]
						} else {
							cleanParts[i] = p
						}
					}
					cmd = exec.CommandContext(tCtx, cleanParts[0], cleanParts[1:]...)
				} else {
					cmdRun := currentCmd
					singleQuoteArgRe := regexp.MustCompile(`(^|[\s=])'([^'\r\n]*)'([\s]|$)`)
					for singleQuoteArgRe.MatchString(cmdRun) {
						cmdRun = singleQuoteArgRe.ReplaceAllStringFunc(cmdRun, func(m string) string {
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
					cmd = exec.CommandContext(tCtx, "cmd.exe", "/c", cmdRun)
				}
			} else if strings.ContainsAny(currentCmd, "&|;><*?$()[]'\"`") || strings.HasPrefix(currentCmd, "cd ") {
				shell := "sh"
				if _, statErr := os.Stat("/bin/bash"); statErr == nil {
					shell = "/bin/bash"
				}
				cmd = exec.CommandContext(tCtx, shell, "-c", currentCmd)
			} else {
				parts := strings.Fields(currentCmd)
				if len(parts) == 0 {
					return nil, fmt.Errorf("EMPTY_COMMAND: no command provided")
				}
				cleanParts := make([]string, len(parts))
				for i, p := range parts {
					if len(p) >= 2 && ((strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'")) || (strings.HasPrefix(p, "\"") && strings.HasSuffix(p, "\""))) {
						cleanParts[i] = p[1 : len(p)-1]
					} else {
						cleanParts[i] = p
					}
				}
				cmd = exec.CommandContext(tCtx, cleanParts[0], cleanParts[1:]...)
			}

			cmd.Dir = currentDir
			cmd.Env = os.Environ()
			cmd.Env = append(cmd.Env, "PYTEST_DISABLE_PLUGIN_AUTOLOAD=1", "PYTHONUNBUFFERED=1")
			if len(req.ExtraEnv) > 0 {
				cmd.Env = append(cmd.Env, req.ExtraEnv...)
			}
			vm.ConfigureSubprocessGroup(cmd)
			var stdoutBuf, stderrBuf strings.Builder
			cmd.Stdout = &stdoutBuf
			cmd.Stderr = &stderrBuf

			if err := cmd.Start(); err != nil {
				return nil, fmt.Errorf("failed to start command: %w", err)
			}

			done := make(chan error, 1)
			go func() {
				done <- cmd.Wait()
			}()

			var runErr error
			select {
			case <-tCtx.Done():
				runErr = tCtx.Err()
				killProcessTree(cmd)
				select {
				case <-done:
				case <-time.After(1 * time.Second):
				}
			case runErr = <-done:
			}
			stdoutStr = stdoutBuf.String()
			stderrStr = stderrBuf.String()

			if tCtx.Err() == context.DeadlineExceeded {
				exitCode = 124
				if stderrStr != "" {
					stderrStr += "\n"
				}
				stderrStr += fmt.Sprintf("COMMAND_TIMEOUT: execution exceeded timeout threshold (%ds). Suspected infinite loop, channel deadlock, or mutex blockage.", req.TimeoutSeconds)
			} else if runErr != nil {
				if exitErr, ok := runErr.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = 1
					if stderrStr == "" {
						stderrStr = runErr.Error()
					} else {
						stderrStr += "\n" + runErr.Error()
					}
				}
			}
		}
	}

	cleanedStderr := StripANSI(stderrStr)
	cleanedStdout := StripANSI(stdoutStr)

	diagnostics := ParseCompilerDiagnostics(cleanedStderr, req.LanguageHint)
	if len(diagnostics) == 0 && exitCode != 0 {
		diagnostics = ParseCompilerDiagnostics(cleanedStdout, req.LanguageHint)
	}

	if len(diagnostics) == 0 && exitCode != 0 {
		// Fallback diagnostic: capture output summary so LLM repair loop is never starved of context
		summary := strings.TrimSpace(cleanedStdout)
		if summary == "" {
			summary = strings.TrimSpace(cleanedStderr)
		}
		if len(summary) > 2000 {
			lines := strings.Split(summary, "\n")
			if len(lines) > 30 {
				summary = strings.Join(lines[len(lines)-30:], "\n")
			}
		}
		diagnostics = append(diagnostics, CompilerDiagnostic{
			File:     "test",
			Line:     1,
			Severity: "ERROR",
			Message:  fmt.Sprintf("Command failed with exit code %d:\n%s", exitCode, summary),
		})
	}

	enrichDiagnosticsWithSource(req.WorkspaceDir, diagnostics)

	success := (exitCode == 0 && len(diagnostics) == 0)

	repairPayload := ""
	if !success {
		repairPayload = SynthesizeRepairPayload(req.Command, exitCode, diagnostics, cleanedStderr)
	}

	return &CompilerFeedbackReport{
		Success:             success,
		ExitCode:            exitCode,
		Command:             req.Command,
		RawStdout:           stdoutStr,
		RawStderr:           stderrStr,
		CleanedStderr:       cleanedStderr,
		Diagnostics:         diagnostics,
		ErrorCount:          len(diagnostics),
		RepairPromptPayload: repairPayload,
		ExecutionEngine:     engine,
	}, nil
}

// ParseCompilerDiagnostics extracts structured file, line, and error messages from compiler output.
func ParseCompilerDiagnostics(rawText string, langHint string) []CompilerDiagnostic {
	scanner := bufio.NewScanner(strings.NewReader(rawText))
	var diagnostics []CompilerDiagnostic

	var pendingRustSev string
	var pendingRustCode string
	var pendingRustMsg string
	var pendingRustFile string
	var pendingRustLine int
	var pendingRustCol int
	var recentGoPanic string
	var currentTimeoutTest string
	var currentGoroutineID string
	var currentGoroutineState string
	var recentJestFile string
	var recentJestTest string
	seenGoroutineDiags := make(map[string]bool)

	flushPendingRust := func() {
		if pendingRustFile != "" {
			sev := pendingRustSev
			if sev == "" {
				sev = "ERROR"
			}
			msg := pendingRustMsg
			if msg == "" {
				msg = "Rust compilation error"
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:      filepath.ToSlash(pendingRustFile),
				Line:      pendingRustLine,
				Column:    pendingRustCol,
				Severity:  sev,
				ErrorCode: pendingRustCode,
				Message:   msg,
			})
			pendingRustFile = ""
			pendingRustLine = 0
			pendingRustCol = 0
			pendingRustSev = ""
			pendingRustCode = ""
			pendingRustMsg = ""
		}
	}

	var recentELine string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// 1. Go Error
		if matches := goErrorRegex.FindStringSubmatch(line); len(matches) > 4 {
			errMsg := strings.TrimSpace(matches[4])
			if strings.Contains(errMsg, "too many errors") {
				continue
			}
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			colNum := 0
			if matches[3] != "" {
				colNum, _ = strconv.Atoi(matches[3])
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:     filepath.ToSlash(matches[1]),
				Line:     lineNum,
				Column:   colNum,
				Severity: "ERROR",
				Message:  errMsg,
			})
			continue
		}

		// 1b. Go Running Test in Timeout Dump (e.g. "\tTestInitialElectionSingleLeader (10.00s)")
		if m := goRunningTestRegex.FindStringSubmatch(line); len(m) > 1 {
			currentTimeoutTest = m[1]
			continue
		}

		// 1c. Go Panic Header
		if matches := goPanicRegex.FindStringSubmatch(line); len(matches) > 1 {
			flushPendingRust()
			cleanPanic := strings.TrimSuffix(strings.TrimSpace(matches[1]), " [recovered]")
			recentGoPanic = cleanPanic
			continue
		}

		// 1d. Go Goroutine Header (e.g. "goroutine 20 [sync.Mutex.Lock]:")
		if matches := goGoroutineHeaderRegex.FindStringSubmatch(line); len(matches) > 2 {
			flushPendingRust()
			currentGoroutineID = matches[1]
			currentGoroutineState = strings.TrimSpace(matches[2])
			continue
		}

		// 1e. Go Stack Trace Frame (following panic or blocked goroutine)
		isBlockedGoroutine := currentGoroutineState != "" && (strings.Contains(currentGoroutineState, "Lock") ||
			strings.Contains(currentGoroutineState, "chan") ||
			strings.Contains(currentGoroutineState, "select") ||
			strings.Contains(currentGoroutineState, "semacquire"))

		if recentGoPanic != "" || isBlockedGoroutine {
			if matches := goStackTraceRegex.FindStringSubmatch(line); len(matches) > 2 {
				frameFile := matches[1]
				// Skip Go runtime, sync internals, and testing framework internals
				if !strings.Contains(frameFile, "/runtime/") && !strings.Contains(frameFile, "\\runtime\\") &&
					!strings.Contains(frameFile, "/testing/") && !strings.Contains(frameFile, "\\testing\\") &&
					!strings.Contains(frameFile, "/sync/") && !strings.Contains(frameFile, "\\sync\\") &&
					!strings.Contains(frameFile, "_testmain.go") {
					lineNum, _ := strconv.Atoi(matches[2])

					var msg string
					if isBlockedGoroutine || strings.Contains(recentGoPanic, "test timed out") {
						prefix := "Deadlock / Concurrency timeout"
						if currentTimeoutTest != "" {
							prefix = fmt.Sprintf("Deadlock / Concurrency timeout in %s", currentTimeoutTest)
						}
						state := currentGoroutineState
						if state == "" {
							state = "blocked"
						}
						msg = fmt.Sprintf("%s: goroutine %s blocked on [%s]", prefix, currentGoroutineID, state)
					} else {
						msg = "panic: " + recentGoPanic
						if strings.HasPrefix(recentGoPanic, "panic: ") {
							msg = recentGoPanic
						}
					}

					key := fmt.Sprintf("%s:%d:%s", filepath.ToSlash(frameFile), lineNum, msg)
					if !seenGoroutineDiags[key] {
						seenGoroutineDiags[key] = true
						diagnostics = append(diagnostics, CompilerDiagnostic{
							File:     filepath.ToSlash(frameFile),
							Line:     lineNum,
							Severity: "ERROR",
							Message:  msg,
						})
					}

					if !isBlockedGoroutine {
						recentGoPanic = ""
					}
					continue
				}
			}
		}

		// 2. TypeScript Error (Parenthetical: file(line,col))
		if matches := tsParenErrorRegex.FindStringSubmatch(line); len(matches) > 6 {
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			colNum, _ := strconv.Atoi(matches[3])
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:      filepath.ToSlash(matches[1]),
				Line:      lineNum,
				Column:    colNum,
				Severity:  strings.ToUpper(matches[4]),
				ErrorCode: matches[5],
				Message:   strings.TrimSpace(matches[6]),
			})
			continue
		}

		// 2b. TypeScript Error (Colon: file:line:col - error TS...)
		if matches := tsColonErrorRegex.FindStringSubmatch(line); len(matches) > 6 {
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			colNum, _ := strconv.Atoi(matches[3])
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:      filepath.ToSlash(matches[1]),
				Line:      lineNum,
				Column:    colNum,
				Severity:  strings.ToUpper(matches[4]),
				ErrorCode: matches[5],
				Message:   strings.TrimSpace(matches[6]),
			})
			continue
		}

		// 2c. Jest / Vitest FAIL file header (e.g. FAIL src/components/Button.test.tsx)
		if matches := jestFailFileRegex.FindStringSubmatch(line); len(matches) > 1 {
			flushPendingRust()
			recentJestFile = filepath.ToSlash(matches[1])
			continue
		}

		// 2d. Jest / Vitest test failure heading (e.g. ● Button component › renders correctly)
		if matches := jestTestNameRegex.FindStringSubmatch(line); len(matches) > 1 {
			testName := strings.TrimSpace(matches[1])
			if len(matches) > 2 && matches[2] != "" {
				testName += " › " + strings.TrimSpace(matches[2])
			}
			recentJestTest = testName
			continue
		}

		// 2e. Jest / Vitest stack trace frame (e.g. at Object.<anonymous> (src/components/Button.test.tsx:42:15))
		if matches := jestStackTraceRegex.FindStringSubmatch(line); len(matches) > 3 {
			flushPendingRust()
			file := filepath.ToSlash(matches[1])
			lineNum, _ := strconv.Atoi(matches[2])
			colNum, _ := strconv.Atoi(matches[3])
			msg := "Test failure"
			if recentJestTest != "" {
				msg = fmt.Sprintf("Test failure in %s", recentJestTest)
			}
			alreadyReported := false
			for _, d := range diagnostics {
				if d.File == file && d.Line == lineNum {
					alreadyReported = true
					break
				}
			}
			if !alreadyReported {
				diagnostics = append(diagnostics, CompilerDiagnostic{
					File:     file,
					Line:     lineNum,
					Column:   colNum,
					Severity: "ERROR",
					Message:  msg,
				})
			}
			continue
		}

		// 3. Rust Error Header + Location (supports both Header->Location and Location->Header)
		if matches := rustCargoErrorRegex.FindStringSubmatch(line); len(matches) > 3 {
			pendingRustSev = strings.ToUpper(matches[1])
			pendingRustCode = matches[2]
			pendingRustMsg = strings.TrimSpace(matches[3])
			if pendingRustFile != "" {
				diagnostics = append(diagnostics, CompilerDiagnostic{
					File:      filepath.ToSlash(pendingRustFile),
					Line:      pendingRustLine,
					Column:    pendingRustCol,
					Severity:  pendingRustSev,
					ErrorCode: pendingRustCode,
					Message:   pendingRustMsg,
				})
				pendingRustFile = ""
				pendingRustLine = 0
				pendingRustCol = 0
				pendingRustSev = ""
				pendingRustCode = ""
				pendingRustMsg = ""
			}
			continue
		}
		if matches := rustCargoLocationRegex.FindStringSubmatch(line); len(matches) > 2 {
			lineNum, _ := strconv.Atoi(matches[2])
			colNum := 0
			if len(matches) > 3 && matches[3] != "" {
				colNum, _ = strconv.Atoi(matches[3])
			}
			if pendingRustMsg != "" {
				sev := pendingRustSev
				if sev == "" {
					sev = "ERROR"
				}
				diagnostics = append(diagnostics, CompilerDiagnostic{
					File:      filepath.ToSlash(matches[1]),
					Line:      lineNum,
					Column:    colNum,
					Severity:  sev,
					ErrorCode: pendingRustCode,
					Message:   pendingRustMsg,
				})
				pendingRustSev = ""
				pendingRustCode = ""
				pendingRustMsg = ""
			} else {
				if pendingRustFile != "" {
					flushPendingRust()
				}
				pendingRustFile = matches[1]
				pendingRustLine = lineNum
				pendingRustCol = colNum
			}
			continue
		}

		// 4. Python / Mypy Error
		if matches := pyMypyErrorRegex.FindStringSubmatch(line); len(matches) > 5 {
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			colNum := 0
			if matches[3] != "" {
				colNum, _ = strconv.Atoi(matches[3])
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:     filepath.ToSlash(matches[1]),
				Line:     lineNum,
				Column:   colNum,
				Severity: strings.ToUpper(matches[4]),
				Message:  strings.TrimSpace(matches[5]),
			})
			continue
		}

		// 4b. Python Traceback frame
		if matches := pyTracebackRegex.FindStringSubmatch(line); len(matches) > 2 {
			flushPendingRust()
			recentELine = ""
			lineNum, _ := strconv.Atoi(matches[2])
			inFunc := matches[3]
			if inFunc == "" {
				inFunc = "unknown"
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:     filepath.ToSlash(matches[1]),
				Line:     lineNum,
				Severity: "ERROR",
				Message:  fmt.Sprintf("Traceback frame in %s", inFunc),
			})
			continue
		}

		// 4c. Pytest Assertion / Error line (e.g. tests/test_cli.py:495: AssertionError)
		if matches := pyTestErrorRegex.FindStringSubmatch(line); len(matches) > 2 {
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			errCode := ""
			errMsg := ""
			if len(matches) > 3 && matches[3] != "" {
				errCode = matches[3]
			}
			if len(matches) > 4 && matches[4] != "" {
				errMsg = strings.TrimSpace(matches[4])
			}
			if (errMsg == "" || errMsg == errCode) && recentELine != "" {
				errMsg = recentELine
			}
			if errMsg == "" && errCode != "" {
				errMsg = errCode
			}
			fileSlash := filepath.ToSlash(matches[1])
			if len(diagnostics) > 0 {
				lastIdx := len(diagnostics) - 1
				if diagnostics[lastIdx].File == fileSlash && diagnostics[lastIdx].Line == lineNum {
					if diagnostics[lastIdx].ErrorCode == "" && errCode != "" {
						diagnostics[lastIdx].ErrorCode = errCode
					}
					if diagnostics[lastIdx].Message == "" || strings.HasPrefix(diagnostics[lastIdx].Message, "Pytest failure") {
						diagnostics[lastIdx].Message = errMsg
					}
					continue
				}
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:      fileSlash,
				Line:      lineNum,
				Severity:  "ERROR",
				ErrorCode: errCode,
				Message:   errMsg,
			})
			continue
		}

		// 4d. Pytest Traceback frame: path/to/file.py:123: in function_name or path/to/file.py:123:
		if matches := pyTestTraceRegex.FindStringSubmatch(line); len(matches) > 2 {
			flushPendingRust()
			recentELine = ""
			lineNum, _ := strconv.Atoi(matches[2])
			inFunc := ""
			if len(matches) > 3 {
				inFunc = strings.TrimSpace(matches[3])
			}
			msg := "Pytest failure"
			if inFunc != "" {
				msg = fmt.Sprintf("Pytest failure in %s", inFunc)
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:     filepath.ToSlash(matches[1]),
				Line:     lineNum,
				Severity: "ERROR",
				Message:  msg,
			})
			continue
		}

		// 4e. Python / Pytest Exception Line: E ... or [pkg.module.]NameError: ...
		if isEx, exText := matchPythonException(line); isEx && exText != "" {
			if strings.HasPrefix(exText, "? ") || exText == "?" {
				continue
			}
			isPytestELine := strings.HasPrefix(line, "E ") || strings.HasPrefix(line, "E\t")
			if len(diagnostics) > 0 {
				lastIdx := len(diagnostics) - 1
				curMsg := diagnostics[lastIdx].Message
				if curMsg == "" || strings.HasPrefix(curMsg, "Pytest failure") || strings.HasPrefix(curMsg, "Traceback frame") || curMsg == diagnostics[lastIdx].ErrorCode {
					diagnostics[lastIdx].Message = exText
					recentELine = exText
				} else if (curMsg == "assert 1 == 0" || strings.Contains(curMsg, "exit_code")) && (strings.Contains(exText, "Error") || strings.Contains(exText, "Exception") || strings.Contains(exText, "where ") || strings.Contains(exText, "DoesNotExist")) {
					// Enrich generic exit_code assertion failure with underlying exception details
					diagnostics[lastIdx].Message = fmt.Sprintf("%s: %s", curMsg, exText)
					recentELine = diagnostics[lastIdx].Message
				} else if isPytestELine && len(curMsg) < 360 && !strings.Contains(curMsg, exText) {
					diagnostics[lastIdx].Message = fmt.Sprintf("%s | %s", curMsg, exText)
					recentELine = diagnostics[lastIdx].Message
				} else if recentELine == "" {
					recentELine = exText
				}
			} else if recentELine == "" {
				recentELine = exText
			}
			continue
		}

		// 4f. Pytest Summary Failed Line: FAILED path/to/test.py::test_name - ErrorMessage
		if matches := pyTestSummaryFailedRegex.FindStringSubmatch(line); len(matches) > 3 {
			failFile := filepath.ToSlash(matches[1])
			failMsg := strings.TrimSpace(matches[3])
			updated := false
			for i := range diagnostics {
				if diagnostics[i].File == failFile || filepath.Base(diagnostics[i].File) == filepath.Base(failFile) {
					if strings.Contains(diagnostics[i].Message, "Error") || strings.Contains(diagnostics[i].Message, "where ") {
						// Preserve richer diagnostic message with exception details
					} else if !strings.HasSuffix(failMsg, "...") || len(diagnostics[i].Message) <= len(failMsg) {
						diagnostics[i].Message = failMsg
					}
					updated = true
					break
				}
			}
			if !updated {
				diagnostics = append(diagnostics, CompilerDiagnostic{
					File:     failFile,
					Line:     1,
					Severity: "ERROR",
					Message:  failMsg,
				})
			}
			continue
		}

		// 5. C/C++ GCC/Clang Error
		if matches := cCppErrorRegex.FindStringSubmatch(line); len(matches) > 5 {
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			colNum := 0
			if matches[3] != "" {
				colNum, _ = strconv.Atoi(matches[3])
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:     filepath.ToSlash(matches[1]),
				Line:     lineNum,
				Column:   colNum,
				Severity: strings.ToUpper(matches[4]),
				Message:  strings.TrimSpace(matches[5]),
			})
			continue
		}

		// 5b. C/C++ MSVC Error
		if matches := msvcErrorRegex.FindStringSubmatch(line); len(matches) > 6 {
			flushPendingRust()
			lineNum, _ := strconv.Atoi(matches[2])
			colNum := 0
			if matches[3] != "" {
				colNum, _ = strconv.Atoi(matches[3])
			}
			diagnostics = append(diagnostics, CompilerDiagnostic{
				File:      filepath.ToSlash(matches[1]),
				Line:      lineNum,
				Column:    colNum,
				Severity:  strings.ToUpper(matches[4]),
				ErrorCode: matches[5],
				Message:   strings.TrimSpace(matches[6]),
			})
			continue
		}
	}

	flushPendingRust()

	if len(diagnostics) == 0 && recentJestFile != "" {
		msg := "Jest/Vitest test suite failure"
		if recentJestTest != "" {
			msg = fmt.Sprintf("Test suite failure in %s", recentJestTest)
		}
		diagnostics = append(diagnostics, CompilerDiagnostic{
			File:     recentJestFile,
			Line:     1,
			Severity: "ERROR",
			Message:  msg,
		})
	}

	if recentGoPanic != "" && len(diagnostics) == 0 {
		diagnostics = append(diagnostics, CompilerDiagnostic{
			File:     "test:1",
			Line:     1,
			Severity: "ERROR",
			Message:  "panic: " + recentGoPanic,
		})
	}

	return diagnostics
}

// EnrichDiagnosticsWithSource extracts 5-line source context [errLine-2, errLine+2] with `> %4d: %s`.
func EnrichDiagnosticsWithSource(workspaceDir string, diags []CompilerDiagnostic) {
	enrichDiagnosticsWithSource(workspaceDir, diags)
}

// enrichDiagnosticsWithSource extracts 5-line source context [errLine-2, errLine+2] with `> %4d: %s`.
func enrichDiagnosticsWithSource(workspaceDir string, diags []CompilerDiagnostic) {
	for i := range diags {
		targetFile := diags[i].File
		if !filepath.IsAbs(targetFile) {
			targetFile = filepath.Join(workspaceDir, targetFile)
		}
		targetFile = filepath.Clean(targetFile)

		raw, err := os.ReadFile(targetFile)
		if err != nil {
			baseName := filepath.Base(targetFile)
			_ = filepath.Walk(workspaceDir, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr == nil && !info.IsDir() && info.Name() == baseName {
					if foundData, rErr := os.ReadFile(path); rErr == nil {
						raw = foundData
						targetFile = path
						return filepath.SkipAll
					}
				}
				return nil
			})
		}
		if len(raw) == 0 {
			continue
		}

		rawStr := strings.TrimRight(string(raw), "\r\n")
		if rawStr == "" {
			continue
		}
		rawLines := strings.Split(rawStr, "\n")
		lines := make([]string, len(rawLines))
		for idx, l := range rawLines {
			lines[idx] = strings.TrimRight(l, "\r")
		}

		errLine := diags[i].Line
		if errLine > 0 && errLine <= len(lines) {
			diags[i].OffendingLine = strings.TrimSpace(lines[errLine-1])

			// Exact 5-line window: [errLine-2, errLine+2]
			start := errLine - 2
			if start < 1 {
				start = 1
			}
			end := errLine + 2
			if end > len(lines) {
				end = len(lines)
			}

			var ctxSnippet []string
			for idx := start; idx <= end; idx++ {
				marker := "  "
				if idx == errLine {
					marker = "> "
				}
				ctxSnippet = append(ctxSnippet, fmt.Sprintf("%s%4d: %s", marker, idx, lines[idx-1]))
			}
			diags[i].ContextLines = ctxSnippet
		}
	}
}

// SynthesizeRepairPayload formats diagnostics into a high-signal prompt payload strictly bounded to <300 tokens.
func SynthesizeRepairPayload(cmd string, exitCode int, diags []CompilerDiagnostic, rawStderr string) string {
	var sb strings.Builder

	sb.WriteString("## Compiler Failure Diagnostics\n\n")
	sb.WriteString(fmt.Sprintf("**Command**: `%s` (Exit: %d)\n\n", cmd, exitCode))

	if len(diags) > 0 {
		// Display top 1-2 primary diagnostics to avoid token overflow
		displayCount := len(diags)
		if displayCount > 2 {
			displayCount = 2
		}

		for i := 0; i < displayCount; i++ {
			d := diags[i]
			codeStr := ""
			if d.ErrorCode != "" {
				codeStr = fmt.Sprintf(" [%s]", d.ErrorCode)
			}
			sb.WriteString(fmt.Sprintf("### Error %d: `%s:%d`%s\n", i+1, d.File, d.Line, codeStr))
			sb.WriteString(fmt.Sprintf("> **%s**: %s\n\n", d.Severity, d.Message))

			if len(d.ContextLines) > 0 {
				sb.WriteString("```\n")
				sb.WriteString(strings.Join(d.ContextLines, "\n"))
				sb.WriteString("\n```\n\n")
			}
		}

		if len(diags) > displayCount {
			sb.WriteString(fmt.Sprintf("... and %d more diagnostic(s) omitted.\n\n", len(diags)-displayCount))
		}
	} else if strings.TrimSpace(rawStderr) != "" {
		sb.WriteString("### Raw Error Output\n```\n")
		lines := strings.Split(rawStderr, "\n")
		if len(lines) > 10 {
			lines = lines[len(lines)-10:]
		}
		sb.WriteString(strings.Join(lines, "\n"))
		sb.WriteString("\n```\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Command failed with exit code %d (no output on stdout/stderr).\n\n", exitCode))
	}

	sb.WriteString("### Prescribed Action\n")
	sb.WriteString("Emit search/replace edit blocks (`<<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE`) or [ACTION:WRITE path=\"...\"] to fix the reported error.\n")

	payload := sb.String()

	// Strict token budget enforcement: < 300 tokens (approx 1050 chars)
	if len(payload) > 1024 {
		payload = payload[:1000] + "\n...\n[Truncated to fit <300 token budget]\n"
	}

	return payload
}

func countActualErrors(rep *CompilerFeedbackReport) int {
	if rep == nil || len(rep.Diagnostics) == 0 {
		return 0
	}
	actual := 0
	for _, d := range rep.Diagnostics {
		if d.Message == "" || strings.HasPrefix(d.Message, "Traceback frame") || strings.HasPrefix(d.Message, "Pytest failure") || strings.HasPrefix(d.Message, "Emitted warnings") {
			continue
		}
		actual++
	}
	if actual == 0 {
		return 1
	}
	return actual
}

func extractPrimaryException(rep *CompilerFeedbackReport) string {
	if rep == nil || len(rep.Diagnostics) == 0 {
		return ""
	}
	for i := len(rep.Diagnostics) - 1; i >= 0; i-- {
		msg := rep.Diagnostics[i].Message
		if !strings.HasPrefix(msg, "Traceback frame") && !strings.HasPrefix(msg, "Pytest failure") && !strings.HasPrefix(msg, "Emitted warnings") {
			return msg
		}
	}
	return rep.Diagnostics[len(rep.Diagnostics)-1].Message
}

// FormatPrimaryFailureSummary returns a concise summary of the actual exception, file:line, and E-line for reflexion prompts.
func FormatPrimaryFailureSummary(rep *CompilerFeedbackReport) string {
	if rep == nil || len(rep.Diagnostics) == 0 {
		return "test failed"
	}
	var rawELines []string
	for _, rawLine := range strings.Split(rep.RawStdout+"\n"+rep.CleanedStderr, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "E   ") || strings.HasPrefix(trimmed, "E ") || strings.HasPrefix(trimmed, "AssertionError:") || strings.HasPrefix(trimmed, "FAILED ") {
			if len(trimmed) > 180 {
				trimmed = trimmed[:180] + "..."
			}
			rawELines = append(rawELines, trimmed)
		}
	}
	if len(rawELines) > 3 {
		rawELines = rawELines[len(rawELines)-3:]
	}
	for i := len(rep.Diagnostics) - 1; i >= 0; i-- {
		d := rep.Diagnostics[i]
		if d.Message != "" && !strings.HasPrefix(d.Message, "Traceback frame") && !strings.HasPrefix(d.Message, "Pytest failure") && !strings.HasPrefix(d.Message, "Emitted warnings") {
			var eLines []string
			for j := i - 1; j >= 0; j-- {
				caller := rep.Diagnostics[j]
				if caller.File == "" || filepath.Base(caller.File) == filepath.Base(d.File) {
					continue
				}
				cSlash := strings.ToLower(filepath.ToSlash(caller.File))
				if strings.Contains(cSlash, "site-packages") || strings.Contains(cSlash, "dist-packages") ||
					strings.Contains(cSlash, "/lib/python") || strings.HasSuffix(cSlash, "/pytest.py") ||
					strings.Contains(cSlash, "_unittest_backport.py") || strings.Contains(cSlash, "test/testcases.py") ||
					strings.HasSuffix(cSlash, "db/utils.py") || strings.HasSuffix(cSlash, "sqlite3/base.py") {
					continue
				}
				callerCtx := ""
				for _, ccl := range caller.ContextLines {
					t := strings.TrimSpace(ccl)
					if strings.HasPrefix(t, ">") {
						callerCtx = " " + t
						break
					}
				}
				eLines = append(eLines, fmt.Sprintf("caller %s:%d%s", filepath.Base(caller.File), caller.Line, callerCtx))
				break
			}
			for _, cl := range d.ContextLines {
				trimmed := strings.TrimSpace(cl)
				if strings.HasPrefix(trimmed, "E ") || strings.HasPrefix(trimmed, "> ") || strings.Contains(trimmed, "Error:") || strings.Contains(trimmed, "assert ") {
					eLines = append(eLines, trimmed)
					if len(eLines) >= 2 {
						break
					}
				}
			}
			for _, reLine := range rawELines {
				if len(eLines) < 3 && !strings.Contains(d.Message, reLine) {
					eLines = append(eLines, reLine)
				}
			}
			if len(eLines) > 0 {
				return fmt.Sprintf("%s:%d: %s (%s)", filepath.Base(d.File), d.Line, d.Message, strings.Join(eLines, " | "))
			}
			return fmt.Sprintf("%s:%d: %s", filepath.Base(d.File), d.Line, d.Message)
		}
	}
	last := rep.Diagnostics[len(rep.Diagnostics)-1]
	if len(rawELines) > 0 {
		return fmt.Sprintf("%s:%d: %s (%s)", filepath.Base(last.File), last.Line, last.Message, strings.Join(rawELines, " | "))
	}
	return fmt.Sprintf("%s:%d: %s", filepath.Base(last.File), last.Line, last.Message)
}

func extractTestFrameLocation(rep *CompilerFeedbackReport) (string, int) {
	if rep == nil {
		return "", 0
	}
	for _, d := range rep.Diagnostics {
		base := strings.ToLower(filepath.Base(d.File))
		fSlash := strings.ToLower(filepath.ToSlash(d.File))
		if strings.HasPrefix(base, "test_") || strings.Contains(base, "_test.") || strings.HasPrefix(fSlash, "tests/") || strings.Contains(fSlash, "/tests/") {
			if d.Line > 0 {
				return base, d.Line
			}
		}
	}
	return "", 0
}

// ScoreHealCandidate evaluates a candidate trial execution report against an initial baseline report.
// It returns an integer score and a boolean indicating whether the candidate introduced syntax/indentation errors.
// Higher scores indicate superior progression along the error gradient.
func ScoreHealCandidate(initialReport, trialReport *CompilerFeedbackReport, candBlocksCount int) (int, bool) {
	if trialReport == nil || trialReport.ExitCode == 124 {
		return -1000, false
	}
	if len(trialReport.Diagnostics) == 1 && trialReport.Diagnostics[0].File == "test" &&
		initialReport != nil && len(initialReport.Diagnostics) > 0 && initialReport.Diagnostics[0].File != "test" {
		return -500, false
	}

	hasSyntaxError := false
	for _, d := range trialReport.Diagnostics {
		if strings.Contains(d.Message, "SyntaxError") || strings.Contains(d.Message, "IndentationError") {
			hasSyntaxError = true
			break
		}
	}
	if hasSyntaxError {
		return -1000, true
	}

	if trialReport.Success && trialReport.ExitCode == 0 {
		return 1000, false
	}

	score := 0
	initialActual := countActualErrors(initialReport)
	trialActual := countActualErrors(trialReport)

	diagReduction := initialActual - trialActual
	if diagReduction > 0 {
		score += diagReduction * 100
		if initialReport != nil && len(initialReport.Diagnostics) > 0 && len(trialReport.Diagnostics) > 0 &&
			initialReport.Diagnostics[0].Message != trialReport.Diagnostics[0].Message {
			score += 30
		}
	} else if diagReduction < 0 {
		score += diagReduction * 50
	}

	// Error-gradient progression tracking using the innermost exception (not outer Traceback frame)
	initialErr := extractPrimaryException(initialReport)
	currErr := extractPrimaryException(trialReport)

	isFatalCrash := strings.Contains(initialErr, "ImportError") || strings.Contains(initialErr, "ModuleNotFoundError") ||
		strings.Contains(initialErr, "AttributeError") || strings.Contains(initialErr, "TypeError") ||
		strings.Contains(initialErr, "NameError") || strings.Contains(initialErr, "UnboundLocalError") ||
		strings.Contains(initialErr, "KeyError") || strings.Contains(initialErr, "IndexError") ||
		strings.Contains(initialErr, "ValueError") || strings.Contains(initialErr, "RecursionError") ||
		strings.Contains(initialErr, "RuntimeError") || strings.Contains(initialErr, "ZeroDivisionError") ||
		strings.Contains(initialErr, "NotImplementedError") || strings.Contains(initialErr, "OverflowError")
	currIsFatalCrash := strings.Contains(currErr, "ImportError") || strings.Contains(currErr, "ModuleNotFoundError") ||
		strings.Contains(currErr, "AttributeError") || strings.Contains(currErr, "TypeError") ||
		strings.Contains(currErr, "NameError") || strings.Contains(currErr, "UnboundLocalError") ||
		strings.Contains(currErr, "KeyError") || strings.Contains(currErr, "IndexError") ||
		strings.Contains(currErr, "ValueError") || strings.Contains(currErr, "RecursionError") ||
		strings.Contains(currErr, "RuntimeError") || strings.Contains(currErr, "ZeroDivisionError") ||
		strings.Contains(currErr, "NotImplementedError") || strings.Contains(currErr, "OverflowError")

	// Test-line progression: if execution within the same test function advanced to a later line number,
	// earlier statements/assertions in the test function succeeded!
	initTestFile, initTestLine := extractTestFrameLocation(initialReport)
	trialTestFile, trialTestLine := extractTestFrameLocation(trialReport)
	if initTestFile != "" && initTestFile == trialTestFile && initTestLine > 0 && trialTestLine > 0 {
		if trialTestLine > initTestLine && trialTestLine-initTestLine <= 25 && !currIsFatalCrash {
			score += 90
		} else if trialTestLine < initTestLine {
			score -= 90
		}
	}

	if currErr != "" && initialErr != "" && currErr != initialErr {
		isSigError := strings.Contains(initialErr, "unexpected keyword argument") ||
			strings.Contains(initialErr, "missing 1 required") ||
			strings.Contains(initialErr, "positional argument")
		currIsSigError := strings.Contains(currErr, "unexpected keyword argument") ||
			strings.Contains(currErr, "missing 1 required") ||
			strings.Contains(currErr, "positional argument")

		// Crash-to-assertion transition (e.g. fatal crash -> reached test assert)
		initIsImportOrNameErr := strings.Contains(initialErr, "ImportError") || strings.Contains(initialErr, "ModuleNotFoundError") ||
			strings.Contains(initialErr, "NameError") || strings.Contains(initialErr, "UnboundLocalError")
		currIsImportOrNameErr := strings.Contains(currErr, "ImportError") || strings.Contains(currErr, "ModuleNotFoundError") ||
			strings.Contains(currErr, "NameError") || strings.Contains(currErr, "UnboundLocalError")
		if !initIsImportOrNameErr && currIsImportOrNameErr {
			return -1000, true
		}
		if (!strings.Contains(initialErr, "RecursionError") && strings.Contains(currErr, "RecursionError")) ||
			(!strings.Contains(initialErr, "AttributeError") && strings.Contains(currErr, "AttributeError")) {
			return -1000, true
		}

		isAssertion := strings.Contains(currErr, "AssertionError") || strings.Contains(currErr, "assert ") || strings.Contains(currErr, "FAILED")

		if isFatalCrash && isAssertion {
			score += 50
		} else if isSigError && !currIsSigError && !currIsImportOrNameErr {
			// Resolved constructor/method signature error and advanced into method execution
			score += 65
		} else if !isFatalCrash && currIsFatalCrash {
			return -1000, true // Regression: turned assertion failure into runtime crash
		}
	}

	if initialReport != nil && trialReport.ExitCode < initialReport.ExitCode {
		score += 25
	}
	if score > 0 && candBlocksCount > 0 && candBlocksCount <= 2 {
		score += 10
	}

	return score, false
}

// matchPythonException tests if line is a Python exception declaration (built-in, pytest E line, or custom domain exception).
func matchPythonException(line string) (bool, string) {
	if strings.HasPrefix(line, "E   ") || strings.HasPrefix(line, "E ") || strings.HasPrefix(line, "E\t") {
		return true, strings.TrimSpace(strings.TrimLeft(line[1:], " \t"))
	}
	trimmed := strings.TrimSpace(line)
	colonIdx := strings.Index(trimmed, ":")
	if colonIdx <= 0 {
		return false, ""
	}
	typePart := strings.TrimSpace(trimmed[:colonIdx])
	// Class name / dotted package must not contain spaces or symbols
	if strings.ContainsAny(typePart, " \t\r\n\"'()[]{}<>=;,") {
		return false, ""
	}
	parts := strings.Split(typePart, ".")
	className := parts[len(parts)-1]
	if len(className) < 3 {
		return false, ""
	}
	// Common Python exception naming suffixes and keywords
	if strings.HasSuffix(className, "Error") ||
		strings.HasSuffix(className, "Exception") ||
		strings.HasSuffix(className, "Warning") ||
		strings.HasSuffix(className, "DoesNotExist") ||
		strings.HasSuffix(className, "MultipleObjectsReturned") ||
		strings.HasSuffix(className, "Fault") ||
		className == "StopIteration" ||
		className == "SystemExit" ||
		className == "KeyboardInterrupt" ||
		className == "GeneratorExit" {
		return true, trimmed
	}
	return false, ""
}
