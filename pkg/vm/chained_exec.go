// SPDX-License-Identifier: Apache-2.0
// Package vm provides pure Go client primitives and hermetic sandboxing for Calera LLM-VM.
// chained_exec.go implements a deterministic shell-chaining execution engine that
// supports compound operators (&&, ||, ;), directory navigation (cd), and inline/exported
// environment variables across Windows, macOS, and Linux.

package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ChainOperator identifies a compound shell control operator.
type ChainOperator string

const (
	OpNone ChainOperator = ""
	OpAnd  ChainOperator = "&&"
	OpOr   ChainOperator = "||"
	OpSeq  ChainOperator = ";"
)

// CommandSegment represents an individual command step in a chain.
type CommandSegment struct {
	Raw      string
	Operator ChainOperator
}

// ChainedExecConfig specifies execution parameters for a chained command pipeline.
type ChainedExecConfig struct {
	InitialWorkdir string
	SandboxRoot    string   // Optional: if set, directory changes outside this boundary are blocked
	HostWorkspace  string   // Optional: if set, absolute cd paths matching HostWorkspace are automatically mapped into SandboxRoot
	BaseEnv        []string // Base environment variables (default: os.Environ())
	TimeoutSeconds int      // Total or per-segment timeout limit
	Shell          string   // Optional: "auto", "powershell", "cmd", "sh", "bash"
	Script         string   // Optional: multi-line script content to execute
}

// ParseCommandChain splits a compound command string into individual segments,
// respecting single quotes, double quotes, and backslash escape sequences.
func ParseCommandChain(cmdLine string) []CommandSegment {
	var segments []CommandSegment
	var cur strings.Builder
	runes := []rune(cmdLine)
	n := len(runes)

	inSingle := false
	inDouble := false
	escaped := false
	currentOp := OpNone

	flush := func(nextOp ChainOperator) {
		trimmed := strings.TrimSpace(cur.String())
		if trimmed != "" {
			segments = append(segments, CommandSegment{
				Raw:      trimmed,
				Operator: currentOp,
			})
		}
		cur.Reset()
		currentOp = nextOp
	}

	for i := 0; i < n; i++ {
		r := runes[i]

		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}

		if r == '\\' && !inSingle {
			escaped = true
			cur.WriteRune(r)
			continue
		}

		if r == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteRune(r)
			continue
		}

		if r == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteRune(r)
			continue
		}

		// When not inside quotes, check operators: &&, ||, ;
		if !inSingle && !inDouble {
			if r == '&' && i+1 < n && runes[i+1] == '&' {
				flush(OpAnd)
				i++ // skip second &
				continue
			}
			if r == '|' && i+1 < n && runes[i+1] == '|' {
				flush(OpOr)
				i++ // skip second |
				continue
			}
			if r == ';' {
				flush(OpSeq)
				continue
			}
		}

		cur.WriteRune(r)
	}

	flush(OpNone)
	return segments
}

// isCdCommand checks if a segment is a 'cd' command and extracts its argument.
func isCdCommand(raw string) (targetDir string, isCd bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "cd" {
		return "", true
	}
	if strings.HasPrefix(trimmed, "cd ") || strings.HasPrefix(trimmed, "cd\t") {
		arg := strings.TrimSpace(trimmed[2:])
		if (strings.HasPrefix(arg, "\"") && strings.HasSuffix(arg, "\"")) ||
			(strings.HasPrefix(arg, "'") && strings.HasSuffix(arg, "'")) {
			if len(arg) >= 2 {
				arg = arg[1 : len(arg)-1]
			}
		}
		return strings.TrimSpace(arg), true
	}
	return "", false
}

// resolveCdDirectory handles target directory resolution, respecting sandbox boundaries and host workspace remapping.
func resolveCdDirectory(target, currentDir, sandboxRoot, hostWorkspace, previousDir string) (string, error) {
	cleanTarget := strings.TrimSpace(target)

	var dest string
	if cleanTarget == "" || cleanTarget == "~" || cleanTarget == "%USERPROFILE%" {
		if sandboxRoot != "" {
			dest = sandboxRoot
		} else {
			if h, err := os.UserHomeDir(); err == nil {
				dest = h
			} else {
				dest = currentDir
			}
		}
	} else if cleanTarget == "-" {
		if previousDir != "" {
			dest = previousDir
		} else {
			dest = currentDir
		}
	} else {
		targetNorm := filepath.FromSlash(cleanTarget)
		if filepath.IsAbs(targetNorm) {
			dest = filepath.Clean(targetNorm)
		} else {
			dest = filepath.Clean(filepath.Join(currentDir, targetNorm))
		}
	}

	// Remap HostWorkspace paths to SandboxRoot if executing inside a sandbox
	if sandboxRoot != "" && hostWorkspace != "" {
		cleanHost := filepath.Clean(hostWorkspace)
		cleanDest := filepath.Clean(dest)
		hostLower := strings.ToLower(cleanHost)
		destLower := strings.ToLower(cleanDest)

		if destLower == hostLower {
			dest = filepath.Clean(sandboxRoot)
		} else if strings.HasPrefix(destLower, hostLower+string(filepath.Separator)) {
			rel := cleanDest[len(cleanHost)+1:]
			dest = filepath.Clean(filepath.Join(sandboxRoot, rel))
		}
	}

	// Containment check if sandboxRoot is configured
	if sandboxRoot != "" {
		cleanRoot := filepath.Clean(sandboxRoot)
		cleanDest := filepath.Clean(dest)
		rootLower := strings.ToLower(cleanRoot)
		destLower := strings.ToLower(cleanDest)
		if !strings.HasPrefix(destLower, rootLower) {
			return currentDir, fmt.Errorf("cd: permission denied: path %q escapes sandbox boundary", target)
		}
	}

	fi, err := os.Stat(dest)
	if err != nil {
		return currentDir, fmt.Errorf("cd: no such file or directory: %s", target)
	}
	if !fi.IsDir() {
		return currentDir, fmt.Errorf("cd: not a directory: %s", target)
	}

	return dest, nil
}

// isExportCommand checks for 'export KEY=VAL'.
func isExportCommand(raw string) (key string, val string, isExport bool) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "export ") {
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "export "))
		parts := strings.SplitN(rest, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			if (strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"")) ||
				(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
				if len(v) >= 2 {
					v = v[1 : len(v)-1]
				}
			}
			return k, v, true
		}
	}
	return "", "", false
}

// isSetCommand checks for Windows cmd-style 'set KEY=VAL'.
func isSetCommand(raw string) (key string, val string, isSet bool) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "set ") && strings.Contains(trimmed, "=") {
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "set "))
		parts := strings.SplitN(rest, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			if (strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"")) ||
				(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
				if len(v) >= 2 {
					v = v[1 : len(v)-1]
				}
			}
			return k, v, true
		}
	}
	return "", "", false
}

var inlineEnvRegex = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(".*?"|'.*?'|\S+)\s*(.*)$`)

// extractInlineEnvVars parses leading 'KEY=VAL' declarations preceding a command.
func extractInlineEnvVars(cmdLine string) (map[string]string, string) {
	vars := make(map[string]string)
	cur := strings.TrimSpace(cmdLine)
	for {
		m := inlineEnvRegex.FindStringSubmatch(cur)
		if len(m) < 4 {
			break
		}
		k := m[1]
		v := m[2]
		if (strings.HasPrefix(v, "\"") && strings.HasSuffix(v, "\"")) ||
			(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'")) {
			if len(v) >= 2 {
				v = v[1 : len(v)-1]
			}
		}
		vars[k] = v
		cur = strings.TrimSpace(m[3])
		if cur == "" {
			break
		}
	}
	return vars, cur
}

// buildEnvironment combines base system env with overlay map.
func buildEnvironment(base []string, overlay map[string]string) []string {
	envMap := make(map[string]string)
	source := base
	if len(source) == 0 {
		source = os.Environ()
	}

	for _, entry := range source {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			key := parts[0]
			if runtime.GOOS == "windows" {
				key = strings.ToUpper(key)
			}
			envMap[key] = parts[1]
		}
	}

	for k, v := range overlay {
		key := k
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		envMap[key] = v
	}

	if runtime.GOOS == "windows" {
		if _, ok := envMap["PATHEXT"]; !ok {
			envMap["PATHEXT"] = ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC;.CPL"
		}
	}

	// Non-interactive CI flags to prevent test runners (Jest, Vitest, npm, git)
	// from entering interactive watch mode or prompting for user credentials.
	if _, ok := envMap["CI"]; !ok {
		envMap["CI"] = "true"
	}
	if _, ok := envMap["CONTINUOUS_INTEGRATION"]; !ok {
		envMap["CONTINUOUS_INTEGRATION"] = "true"
	}
	if _, ok := envMap["GIT_TERMINAL_PROMPT"]; !ok {
		envMap["GIT_TERMINAL_PROMPT"] = "0"
	}
	if _, ok := envMap["TERM"]; !ok {
		envMap["TERM"] = "dumb"
	}

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}

// isPowerShellConstruct detects constructs that cmd.exe fails on but PowerShell handles naturally.
func isPowerShellConstruct(cmdStr string) bool {
	trimmed := strings.TrimSpace(cmdStr)
	if strings.HasPrefix(trimmed, "powershell") || strings.HasPrefix(trimmed, "pwsh") {
		return true
	}
	if strings.Contains(cmdStr, "\n") || strings.Contains(cmdStr, "<<") || strings.Contains(cmdStr, "$") {
		return true
	}
	if strings.Contains(cmdStr, "{") || strings.Contains(cmdStr, "}") {
		return true
	}
	if strings.HasPrefix(trimmed, "(") || strings.HasPrefix(trimmed, "@(") {
		return true
	}
	lower := strings.ToLower(trimmed)
	psVerbs := []string{
		"get-", "set-", "new-", "test-", "select-", "start-", "stop-", "remove-",
		"write-", "out-", "invoke-", "format-", "export-", "import-", "where-",
		"foreach-", "measure-", "compare-", "join-", "split-", "convert-",
		"convertto-", "convertfrom-", "clear-", "enable-", "disable-", "update-",
		"find-", "search-", "add-", "copy-", "move-", "rename-", "resolve-",
	}
	for _, v := range psVerbs {
		if strings.HasPrefix(lower, v) || strings.Contains(lower, " "+v) || strings.Contains(lower, "| "+v) || strings.Contains(lower, "|"+v) {
			return true
		}
	}
	if strings.HasPrefix(lower, "cat >") || strings.HasPrefix(lower, "cat <") ||
		lower == "ls" || strings.HasPrefix(lower, "ls ") {
		return true
	}
	return false
}

const windowsPowerShellPreamble = `$ErrorActionPreference = 'Continue'; $env:PATHEXT = '.COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC;.CPL'; if (Get-Command 'docker.exe' -ErrorAction SilentlyContinue) { Set-Alias -Name docker -Value (Get-Command 'docker.exe').Source -Scope Global -Option AllScope -Force }; if (Get-Command 'curl.exe' -ErrorAction SilentlyContinue) { Set-Alias -Name curl -Value (Get-Command 'curl.exe').Source -Scope Global -Option AllScope -Force }; `

// killProcessTree terminates the command process and all its children.
// On Windows, taskkill /T /F /PID ensures child processes (node.exe, npm, etc.) are terminated,
// preventing orphaned processes from holding open stdio pipes and causing client EOF errors.
func killProcessTree(cmd *exec.Cmd) {
	KillProcessTree(cmd)
}

// runCommandWithAsyncDrain executes a command and drains stdout/stderr concurrently via non-blocking
// buffers, ensuring child process handle inheritance does not trigger stdio EOF hangs or blocking reads.
func runCommandWithAsyncDrain(ctx context.Context, cmd *exec.Cmd) (string, string, int, error) {
	ConfigureSubprocessGroup(cmd)
	cmd.Stdin = strings.NewReader("") // Ensure non-interactive execution

	stdoutPipe, stdoutW, err := os.Pipe()
	if err != nil {
		return "", err.Error(), 1, err
	}
	stderrPipe, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutPipe.Close()
		_ = stdoutW.Close()
		return "", err.Error(), 1, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		_ = stdoutPipe.Close()
		_ = stdoutW.Close()
		_ = stderrPipe.Close()
		_ = stderrW.Close()
		return "", err.Error(), 1, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	var stdoutBuf, stderrBuf strings.Builder
	var bufMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			n, rErr := stdoutPipe.Read(buf)
			if n > 0 {
				bufMu.Lock()
				stdoutBuf.Write(buf[:n])
				bufMu.Unlock()
			}
			if rErr != nil {
				break
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			n, rErr := stderrPipe.Read(buf)
			if n > 0 {
				bufMu.Lock()
				stderrBuf.Write(buf[:n])
				bufMu.Unlock()
			}
			if rErr != nil {
				break
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
		killProcessTree(cmd)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	case runErr = <-done:
	}

	// Drain pipes with non-blocking timeout to avoid lingering if child handles leaked
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(500 * time.Millisecond):
		// Unblock if child process leaked handles without exiting
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
		<-drained
	}
	_ = stdoutPipe.Close()
	_ = stderrPipe.Close()

	bufMu.Lock()
	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()
	bufMu.Unlock()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return stdoutStr, stderrStr, exitCode, runErr
}

// executeSingleStep runs a single shell command within workdir, env, and optional shell override.
func executeSingleStep(ctx context.Context, cmdStr, workdir string, env []string, shell string) (string, string, int, error) {
	var cmd *exec.Cmd
	s := strings.ToLower(strings.TrimSpace(shell))

	if s == "powershell" || s == "pwsh" {
		fullCmd := windowsPowerShellPreamble + cmdStr
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", fullCmd)
	} else if s == "cmd" {
		cmd = exec.Command("cmd.exe", "/d", "/c", cmdStr)
	} else if s == "sh" || s == "bash" {
		shBin := "sh"
		if s == "bash" {
			shBin = "bash"
		}
		cmd = exec.Command(shBin, "-c", cmdStr)
	} else {
		// auto-detect
		if runtime.GOOS == "windows" {
			if isPowerShellConstruct(cmdStr) {
				fullCmd := windowsPowerShellPreamble + cmdStr
				cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", fullCmd)
			} else {
				cmd = exec.Command("cmd.exe", "/d", "/c", cmdStr)
			}
		} else {
			cmd = exec.Command("sh", "-c", cmdStr)
		}
	}
	cmd.Dir = workdir
	cmd.Env = env

	return runCommandWithAsyncDrain(ctx, cmd)
}

// executeScript runs an entire multi-line script payload safely.
func executeScript(ctx context.Context, cfg ChainedExecConfig, scriptContent string) (*CommandResult, error) {
	workdir := cfg.InitialWorkdir
	if workdir == "" {
		if cwd, err := os.Getwd(); err == nil {
			workdir = cwd
		} else {
			workdir = "."
		}
	}

	env := buildEnvironment(cfg.BaseEnv, nil)
	s := strings.ToLower(strings.TrimSpace(cfg.Shell))

	var cmd *exec.Cmd
	var scriptPath string

	if runtime.GOOS == "windows" && s != "sh" && s != "bash" {
		tmpDir := workdir
		if fi, err := os.Stat(tmpDir); err != nil || !fi.IsDir() {
			tmpDir = os.TempDir()
		}
		tmpFile, err := os.CreateTemp(tmpDir, "omnicoder_script_*.ps1")
		if err != nil {
			tmpFile, err = os.CreateTemp(os.TempDir(), "omnicoder_script_*.ps1")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp script file: %w", err)
			}
		}
		scriptPath = tmpFile.Name()
		_ = tmpFile.Close()

		psFilePreamble := "#requires -version 5.1\n$ErrorActionPreference = 'Continue'\n$env:PATHEXT = '.COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC;.CPL'\nif (Get-Command 'docker.exe' -ErrorAction SilentlyContinue) {\n    Set-Alias -Name docker -Value (Get-Command 'docker.exe').Source -Scope Global -Option AllScope -Force\n}\nif (Get-Command 'curl.exe' -ErrorAction SilentlyContinue) {\n    Set-Alias -Name curl -Value (Get-Command 'curl.exe').Source -Scope Global -Option AllScope -Force\n}\n"
		fullScriptContent := psFilePreamble + scriptContent

		if err := os.WriteFile(scriptPath, []byte(fullScriptContent), 0700); err != nil {
			_ = os.Remove(scriptPath)
			return nil, fmt.Errorf("failed to write script content: %w", err)
		}
		defer os.Remove(scriptPath)

		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	} else {
		tmpDir := workdir
		if fi, err := os.Stat(tmpDir); err != nil || !fi.IsDir() {
			tmpDir = os.TempDir()
		}
		tmpFile, err := os.CreateTemp(tmpDir, "omnicoder_script_*.sh")
		if err != nil {
			tmpFile, err = os.CreateTemp(os.TempDir(), "omnicoder_script_*.sh")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp script file: %w", err)
			}
		}
		scriptPath = tmpFile.Name()
		_ = tmpFile.Close()

		if err := os.WriteFile(scriptPath, []byte(scriptContent), 0700); err != nil {
			_ = os.Remove(scriptPath)
			return nil, fmt.Errorf("failed to write script content: %w", err)
		}
		defer os.Remove(scriptPath)

		shBin := "sh"
		if s == "bash" {
			shBin = "bash"
		}
		cmd = exec.Command(shBin, scriptPath)
	}

	cmd.Dir = workdir
	cmd.Env = env

	stdoutStr, stderrStr, exitCode, _ := runCommandWithAsyncDrain(ctx, cmd)

	return &CommandResult{
		ExitCode:        exitCode,
		Stdout:          stdoutStr,
		Stderr:          stderrStr,
		ExecutionEngine: "Calera_Chained_Shell_Engine",
	}, nil
}

// ExecuteChainedCommand executes a compound command pipeline supporting &&, ||, ;,
// directory tracking (cd), and environment assignments with deterministic control flow.
func ExecuteChainedCommand(ctx context.Context, cfg ChainedExecConfig, cmdLine string) (*CommandResult, error) {
	if strings.TrimSpace(cfg.Script) != "" {
		return executeScript(ctx, cfg, cfg.Script)
	}

	trimmed := strings.TrimSpace(cmdLine)
	if trimmed == "" {
		return &CommandResult{
			ExitCode:        0,
			Stdout:          "",
			Stderr:          "",
			ExecutionEngine: "Calera_Chained_Shell_Engine",
		}, nil
	}

	currentWorkdir := cfg.InitialWorkdir
	if currentWorkdir == "" {
		if cwd, err := os.Getwd(); err == nil {
			currentWorkdir = cwd
		} else {
			currentWorkdir = "."
		}
	}
	previousWorkdir := currentWorkdir

	segments := ParseCommandChain(trimmed)
	if len(segments) == 0 {
		return &CommandResult{
			ExitCode:        0,
			Stdout:          "",
			Stderr:          "",
			ExecutionEngine: "Calera_Chained_Shell_Engine",
		}, nil
	}

	effectiveShell := cfg.Shell
	if effectiveShell == "" && runtime.GOOS == "windows" {
		if isPowerShellConstruct(trimmed) {
			effectiveShell = "powershell"
		} else {
			for _, seg := range segments {
				if isPowerShellConstruct(seg.Raw) {
					effectiveShell = "powershell"
					break
				}
			}
		}
	}

	persistentEnv := make(map[string]string)
	var accumulatedStdout strings.Builder
	var accumulatedStderr strings.Builder
	lastExitCode := 0
	lastExecuted := false

	for i, seg := range segments {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Evaluate conditional short-circuiting
		if i > 0 && lastExecuted {
			switch seg.Operator {
			case OpAnd:
				if lastExitCode != 0 {
					// Preceding command failed: skip AND branch
					continue
				}
			case OpOr:
				if lastExitCode == 0 {
					// Preceding command succeeded: skip OR branch
					continue
				}
			case OpSeq:
				// Always execute on semicolon
			}
		}

		rawCmd := seg.Raw

		// 1. Handle 'cd' command
		if target, isCd := isCdCommand(rawCmd); isCd {
			newDir, err := resolveCdDirectory(target, currentWorkdir, cfg.SandboxRoot, cfg.HostWorkspace, previousWorkdir)
			if err != nil {
				lastExitCode = 1
				accumulatedStderr.WriteString(err.Error() + "\n")
			} else {
				previousWorkdir = currentWorkdir
				currentWorkdir = newDir
				lastExitCode = 0
			}
			lastExecuted = true
			continue
		}

		// 2. Handle 'export KEY=VAL'
		if k, v, isExport := isExportCommand(rawCmd); isExport {
			persistentEnv[k] = v
			lastExitCode = 0
			lastExecuted = true
			continue
		}

		// 3. Handle 'set KEY=VAL'
		if k, v, isSet := isSetCommand(rawCmd); isSet {
			persistentEnv[k] = v
			lastExitCode = 0
			lastExecuted = true
			continue
		}

		// 4. Handle inline environment variables (e.g. 'FOO=bar cmd args')
		inlineVars, remainingCmd := extractInlineEnvVars(rawCmd)
		if remainingCmd == "" && len(inlineVars) > 0 {
			for k, v := range inlineVars {
				persistentEnv[k] = v
			}
			lastExitCode = 0
			lastExecuted = true
			continue
		}

		// Combine persistent env with any transient inline variables
		combinedEnvMap := make(map[string]string)
		for k, v := range persistentEnv {
			combinedEnvMap[k] = v
		}
		for k, v := range inlineVars {
			combinedEnvMap[k] = v
		}

		env := buildEnvironment(cfg.BaseEnv, combinedEnvMap)

		// Execute the command segment
		cmdToRun := remainingCmd
		if cmdToRun == "" {
			cmdToRun = rawCmd
		}

		stdoutStr, stderrStr, exitCode, _ := executeSingleStep(ctx, cmdToRun, currentWorkdir, env, effectiveShell)
		if stdoutStr != "" {
			accumulatedStdout.WriteString(stdoutStr)
		}
		if stderrStr != "" {
			accumulatedStderr.WriteString(stderrStr)
		}
		lastExitCode = exitCode
		lastExecuted = true
	}

	return &CommandResult{
		ExitCode:        lastExitCode,
		Stdout:          accumulatedStdout.String(),
		Stderr:          accumulatedStderr.String(),
		ExecutionEngine: "Calera_Chained_Shell_Engine",
	}, nil
}
