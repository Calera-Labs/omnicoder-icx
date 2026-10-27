// SPDX-License-Identifier: Apache-2.0
package main_test

import (
	"bufio"
	"bytes"
	"context"
	"debug/pe"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/ui"
)

var (
	advTestBinOnce sync.Once
	advTestBinPath string
	advTestBinErr  error
)

func findOmnicoderBinary(t *testing.T) string {
	t.Helper()
	// Check current directory (cmd/omnicoder) or workspace root
	candidates := []string{
		"../../omnicoder.exe",
		"../../omnicoder",
		"omnicoder.exe",
		"omnicoder",
		filepath.Join(os.Getenv("OMNICODER_WORKSPACE"), "omnicoder.exe"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		abs, err := filepath.Abs(c)
		if err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs
			}
		}
	}
	advTestBinOnce.Do(func() {
		binName := "omnicoder"
		if runtime.GOOS == "windows" {
			binName = "omnicoder.exe"
		}
		tmpDir, err := os.MkdirTemp("", "omnicoder-adv-bin-*")
		if err != nil {
			advTestBinErr = err
			return
		}
		outPath := filepath.Join(tmpDir, binName)
		cmd := exec.Command("go", "build", "-o", outPath, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if _, err := cmd.CombinedOutput(); err != nil {
			advTestBinErr = err
			return
		}
		advTestBinPath = outPath
	})
	if advTestBinErr == nil && advTestBinPath != "" {
		return advTestBinPath
	}
	t.Fatalf("omnicoder binary not found in candidates: %v", candidates)
	return ""
}

// ============================================================================
// Section 1: Subcommand `omnicoder map` timing stress (< 2.0s on 50+ files / 200+ symbols)
// ============================================================================

func TestAdversarial_MapTiming_50Files200Symbols(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	// Create a synthetic repository with 60 files and 300+ symbols
	tempDir := t.TempDir()
	numFiles := 60
	symbolsPerFile := 5

	for i := 1; i <= numFiles; i++ {
		filePath := filepath.Join(tempDir, fmt.Sprintf("module_%03d.go", i))
		content := fmt.Sprintf(`package stresspkg

import "fmt"

const ConstAlpha%d = %d
const ConstBeta%d = %d

type StructOmega%d struct {
	FieldA int
	FieldB string
}

func (s *StructOmega%d) Compute%d(val int) string {
	return fmt.Sprintf("%%d", s.FieldA + val + ConstAlpha%d)
}

func HelperFunc%d() int {
	return ConstBeta%d * 2
}
`, i, i, i, i*2, i, i, i, i, i, i)

		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Fatalf("failed writing test file %s: %v", filePath, err)
		}
	}

	totalExpectedSymbols := numFiles * symbolsPerFile
	t.Logf("Generated synthetic repo at %s with %d files and >= %d symbols", tempDir, numFiles, totalExpectedSymbols)

	// Run omnicoder map across 5 iterations and assert strictly < 2.0s
	for run := 1; run <= 5; run++ {
		start := time.Now()
		cmd := exec.Command(binPath, "map", "--workspace="+tempDir, "--verbose")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("Run %d: omnicoder map failed: %v\nStderr: %s", run, err, stderr.String())
		}

		if elapsed >= 2000*time.Millisecond {
			t.Fatalf("Run %d: VIOLATION: omnicoder map took %v, which exceeds the strict 2.0s limit", run, elapsed)
		}

		outStr := stdout.String()
		if !strings.Contains(outStr, "StructOmega") {
			t.Fatalf("Run %d: omnicoder map output missing expected symbols. Output:\n%s", run, outStr)
		}

		t.Logf("Run %d PASS: omnicoder map completed in %v (< 2.0s threshold)", run, elapsed)
	}

	// Direct devops.SynthesizeRepoMap timing stress
	startDirect := time.Now()
	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tempDir,
		TokenBudget:  1024,
	})
	elapsedDirect := time.Since(startDirect)
	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed: %v", err)
	}
	if res.FilesScanned < numFiles {
		t.Fatalf("expected >= %d files scanned, got %d", numFiles, res.FilesScanned)
	}
	if res.SymbolsFound < 200 {
		t.Fatalf("expected >= 200 symbols found, got %d", res.SymbolsFound)
	}
	if elapsedDirect >= 500*time.Millisecond {
		t.Fatalf("direct map synthesis took %v (exceeded 500ms)", elapsedDirect)
	}
	t.Logf("Direct RepoMap synthesis: %d files, %d symbols in %v", res.FilesScanned, res.SymbolsFound, elapsedDirect)
}

// ============================================================================
// Section 2: Interactive REPL zero-token reset stress
// ============================================================================

func TestAdversarial_REPL_ZeroTokenResetStress(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, "repl", "--offline")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to open stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to open stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start omnicoder repl: %v", err)
	}

	inputScript := []string{
		"remember Fact-Alpha: Omnicoder RFC8785 canonical JSON provenance",
		"remember Fact-Beta: 4D volumetric lattice preserves memory across resets",
		"remember Fact-Gamma: Sub-2ms delta simplex crystallization",
		"status",
		"reset",
		"search Omnicoder",
		"reset",
		"reset",
		"search lattice",
		"reset",
		"status",
		"exit",
	}

	go func() {
		writer := bufio.NewWriter(stdin)
		for _, line := range inputScript {
			_, _ = writer.WriteString(line + "\n")
			_ = writer.Flush()
			time.Sleep(30 * time.Millisecond)
		}
		_ = stdin.Close()
	}()

	scanner := bufio.NewScanner(stdout)
	var outputLines []string
	resetCount := 0
	foundAlpha := false
	foundBeta := false

	for scanner.Scan() {
		line := scanner.Text()
		outputLines = append(outputLines, line)

		if strings.Contains(line, "Session reset! Tokens preserved: 0") {
			resetCount++
		}
		if strings.Contains(line, "Fact-Alpha: Omnicoder") {
			foundAlpha = true
		}
		if strings.Contains(line, "Fact-Beta: 4D volumetric lattice") {
			foundBeta = true
		}
	}

	_ = cmd.Wait()

	if resetCount < 4 {
		t.Fatalf("expected at least 4 successful resets with 'Tokens preserved: 0', got %d. Output:\n%s",
			resetCount, strings.Join(outputLines, "\n"))
	}
	if !foundAlpha {
		t.Fatalf("Fact-Alpha was lost after reset! Facts must remain queryable after reset")
	}
	if !foundBeta {
		t.Fatalf("Fact-Beta was lost after multiple consecutive resets! Facts must remain queryable")
	}

	t.Logf("REPL Zero-Token Reset PASS: %d consecutive resets verified, facts 100%% queryable", resetCount)

	// In-process deep assertion on LocalMemoryCache AST preservation across resets
	lat := icx.NewLocalLattice()
	testCode := []byte(`package testast
type CacheManager struct { capacity int }
func (c *CacheManager) Evict() bool { return true }
`)
	if err := lat.IngestGoAST("pkg/cache/cache.go", testCode); err != nil {
		t.Fatalf("IngestGoAST failed: %v", err)
	}

	// Add dialogue turn facts (which SHOULD be pruned) and general facts (which MUST NOT be pruned)
	_, _ = lat.Remember(context.Background(), icx.RememberRequest{
		SpaceID: "default",
		Family:  "dialogue_turn",
		Text:    "Turn 1 user dialogue context",
	})
	_, _ = lat.Remember(context.Background(), icx.RememberRequest{
		SpaceID: "default",
		Family:  "persistent_facts",
		Text:    "Persistent invariant claim",
	})

	// Pre-reset state
	infoPre, err := lat.InspectSpace(context.Background(), "default")
	if err != nil {
		t.Fatalf("InspectSpace failed: %v", err)
	}

	// Perform 3 consecutive resets
	for r := 1; r <= 3; r++ {
		resetResp, err := lat.ResetSession(context.Background(), "default")
		if err != nil {
			t.Fatalf("ResetSession %d failed: %v", r, err)
		}
		if resetResp.TokensPreserved != 0 {
			t.Fatalf("ResetSession %d: TokensPreserved must be 0, got %d", r, resetResp.TokensPreserved)
		}
		if resetResp.LatticeNodesAlive == 0 {
			t.Fatalf("ResetSession %d: LatticeNodesAlive must be > 0", r)
		}
	}

	// Verify AST node is still 100% queryable via QueryCodebase
	qRes, err := lat.QueryCodebase(context.Background(), icx.QueryCodebaseRequest{
		Query:     "CacheManager",
		Dimension: "3D_VOLUMETRIC",
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("QueryCodebase failed: %v", err)
	}
	if qRes.Count == 0 {
		t.Fatalf("AST node CacheManager disappeared after session reset! AST must be 100%% preserved")
	}

	// Verify persistent facts remain queryable
	sRes, err := lat.SearchFacts(context.Background(), icx.SearchRequest{
		Query: "Persistent invariant",
	})
	if err != nil || len(sRes.Facts) == 0 {
		t.Fatalf("Persistent fact disappeared after session reset! Facts must remain queryable")
	}

	// Verify dialogue_turn facts WERE pruned
	dRes, err := lat.SearchFacts(context.Background(), icx.SearchRequest{
		Query: "dialogue context",
	})
	if err == nil && len(dRes.Facts) > 0 {
		t.Fatalf("dialogue_turn fact was NOT pruned by ResetSession! Dialogue tokens must be wiped")
	}

	infoPost, err := lat.InspectSpace(context.Background(), "default")
	if err != nil {
		t.Fatalf("InspectSpace post-reset failed: %v", err)
	}
	// Verify that exactly 1 node (the dialogue_turn fact) was pruned, while all AST nodes and persistent facts remained
	if infoPost.NodesCount != infoPre.NodesCount-1 {
		t.Fatalf("expected node count to decrease by exactly 1 (pruned dialogue turn): pre=%d, post=%d",
			infoPre.NodesCount, infoPost.NodesCount)
	}

	t.Logf("Lattice Invariant PASS: AST nodes and persistent facts perfectly preserved across 3 consecutive resets (pre=%d, post=%d)",
		infoPre.NodesCount, infoPost.NodesCount)
}

// ============================================================================
// Section 3: CLI flag permutation and robustness fuzzing
// ============================================================================

func TestAdversarial_FlagPermutations_And_RobustnessFuzzing(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	type testCase struct {
		name       string
		args       []string
		env        map[string]string
		expectExit int
	}

	cases := []testCase{
		// Permutations: flags BEFORE subcommand
		{name: "before: --offline map", args: []string{"--offline", "map"}, expectExit: 0},
		{name: "before: --workspace=. map", args: []string{"--workspace=.", "map"}, expectExit: 0},
		{name: "before: --verbose version", args: []string{"--verbose", "version"}, expectExit: 0},

		// Permutations: flags AFTER subcommand
		{name: "after: map --token-budget=500", args: []string{"map", "--token-budget=500"}, expectExit: 0},
		{name: "after: map --workspace=.", args: []string{"map", "--workspace=."}, expectExit: 0},
		{name: "after: map --offline", args: []string{"map", "--offline"}, expectExit: 0},

		// Permutations: MIXED flags
		{name: "mixed: --workspace=. map --token-budget=300 --verbose", args: []string{"--workspace=.", "map", "--token-budget=300", "--verbose"}, expectExit: 0},
		{name: "mixed: --offline map --token-budget=200", args: []string{"--offline", "map", "--token-budget=200"}, expectExit: 0},

		// Robustness fuzzing: Invalid flags (must exit code 1 without panic)
		{name: "invalid root flag: --bogusflag", args: []string{"--bogusflag"}, expectExit: 1},
		{name: "invalid map flag: map --invalidarg", args: []string{"map", "--invalidarg"}, expectExit: 1},
		{name: "invalid run flag: run --nonexistentflag", args: []string{"run", "--nonexistentflag"}, expectExit: 1},
		{name: "invalid heal flag: heal --badflag", args: []string{"heal", "--badflag"}, expectExit: 1},
		{name: "invalid refactor flag: refactor --unknownflag", args: []string{"refactor", "--unknownflag"}, expectExit: 1},
		{name: "invalid repl flag: repl --unknownflag", args: []string{"repl", "--unknownflag"}, expectExit: 1},

		// Robustness fuzzing: Missing arguments (must exit code 1 without panic)
		{name: "missing arg: run", args: []string{"run"}, expectExit: 1},
		{name: "missing arg: heal", args: []string{"heal"}, expectExit: 1},
		{name: "missing arg: refactor (0 args)", args: []string{"refactor"}, expectExit: 1},
		{name: "missing arg: refactor (1 arg)", args: []string{"refactor", "SymbolOnly"}, expectExit: 1},
		{name: "unknown subcommand: foobar", args: []string{"foobar"}, expectExit: 1},

		// Robustness fuzzing: Non-existent workspace (must exit code 1 or handle gracefully without panic)
		{name: "non-existent workspace map", args: []string{"--workspace=Z:\\DoesNotExist_Omnicoder_99999", "map"}, expectExit: 0},

		// Standard help and version
		{name: "help flag: --help", args: []string{"--help"}, expectExit: 0},
		{name: "help command: help", args: []string{"help"}, expectExit: 0},
		{name: "version command: version", args: []string{"version"}, expectExit: 0},
		{name: "subcommand help: run --help", args: []string{"run", "--help"}, expectExit: 0},
		{name: "subcommand help: run -h", args: []string{"run", "-h"}, expectExit: 0},
		{name: "subcommand help: map --help", args: []string{"map", "--help"}, expectExit: 0},
		{name: "subcommand help: map -h", args: []string{"map", "-h"}, expectExit: 0},
		{name: "subcommand help: heal --help", args: []string{"heal", "--help"}, expectExit: 0},
		{name: "subcommand help: heal -h", args: []string{"heal", "-h"}, expectExit: 0},
		{name: "subcommand help: refactor --help", args: []string{"refactor", "--help"}, expectExit: 0},
		{name: "subcommand help: refactor -h", args: []string{"refactor", "-h"}, expectExit: 0},
		{name: "subcommand help: repl --help", args: []string{"repl", "--help"}, expectExit: 0},
		{name: "subcommand help: repl -h", args: []string{"repl", "-h"}, expectExit: 0},
		{name: "subcommand help: chat --help", args: []string{"chat", "--help"}, expectExit: 0},
		{name: "subcommand help: chat -h", args: []string{"chat", "-h"}, expectExit: 0},

		// Corrupted environment variables (must not crash or panic)
		{
			name:       "corrupt env OMNICODER_MAX_TURNS=invalid_number",
			args:       []string{"version"},
			env:        map[string]string{"OMNICODER_MAX_TURNS": "not_an_integer"},
			expectExit: 0,
		},
		{
			name:       "corrupt env OMNICODER_MAX_TURNS=-100",
			args:       []string{"version"},
			env:        map[string]string{"OMNICODER_MAX_TURNS": "-100"},
			expectExit: 0,
		},
		{
			name:       "corrupt env OMNICODER_OFFLINE=not_a_bool",
			args:       []string{"version"},
			env:        map[string]string{"OMNICODER_OFFLINE": "random_garbage_string"},
			expectExit: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tc.args...)
			cmd.Env = os.Environ()
			for k, v := range tc.env {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
			}

			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			actualExit := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					actualExit = exitErr.ExitCode()
				} else {
					t.Fatalf("unexpected execution error: %v", err)
				}
			}

			outAll := stdout.String() + "\n" + stderr.String()

			// Assert no runtime panic or stack trace
			if strings.Contains(outAll, "panic:") || strings.Contains(outAll, "runtime error:") || strings.Contains(outAll, "goroutine ") {
				t.Fatalf("CRITICAL FAULT: command panicked!\nOutput:\n%s", outAll)
			}

			if actualExit != tc.expectExit {
				t.Fatalf("exit code mismatch: got %d, expected %d\nOutput:\n%s", actualExit, tc.expectExit, outAll)
			}
		})
	}
}

// ============================================================================
// Section 4: Binary integrity & CGO_ENABLED=0 verification
// ============================================================================

func TestAdversarial_BinaryIntegrity_ZeroCGO(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	fi, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("failed to stat omnicoder binary: %v", err)
	}

	t.Logf("omnicoder binary size: %d bytes (%.2f MB)", fi.Size(), float64(fi.Size())/(1024*1024))
	if fi.Size() == 0 {
		t.Fatalf("omnicoder binary is 0 bytes")
	}

	// Verify build settings via `go version -m <binary>`
	cmd := exec.Command("go", "version", "-m", binPath)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m failed: %v", err)
	}
	outStr := string(outBytes)

	if !strings.Contains(outStr, "build\tCGO_ENABLED=0") {
		t.Fatalf("binary was NOT built with CGO_ENABLED=0! Output:\n%s", outStr)
	}
	t.Logf("Confirmed: binary built with CGO_ENABLED=0")

	// If running on Windows, verify PE imported libraries using debug/pe
	if runtime.GOOS == "windows" {
		peFile, err := pe.Open(binPath)
		if err != nil {
			t.Fatalf("pe.Open failed on %s: %v", binPath, err)
		}
		defer peFile.Close()

		libs, err := peFile.ImportedLibraries()
		if err != nil && err != io.EOF {
			t.Fatalf("failed to read PE imported libraries: %v", err)
		}

		t.Logf("PE Imported Libraries: %v", libs)

		// Allowed standard Windows OS subsystem libraries only
		allowedOSLibs := map[string]bool{
			"kernel32.dll": true,
			"ws2_32.dll":   true,
			"shell32.dll":  true,
			"advapi32.dll": true,
			"ntdll.dll":    true,
			"user32.dll":   true,
			"iphlpapi.dll": true,
		}

		for _, lib := range libs {
			lower := strings.ToLower(lib)
			if !allowedOSLibs[lower] {
				t.Fatalf("UNAUTHORIZED EXTERNAL RUNTIME DEPENDENCY: binary imports %q, which is not an authorized Windows OS subsystem library", lib)
			}
		}
		t.Logf("Confirmed: zero external third-party or C runtime DLL dependencies (only standard OS subsystem libraries imported)")
	}
}

// ============================================================================
// Section 5: Terminal Detection & Zero-Argument Launch Adversarial Stress
// ============================================================================

func TestAdversarial_ZeroArgs_PipedStdin_PrintsUsageWithoutHanging(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		t.Fatalf("zero-argument execution failed: %v\nStderr: %s", err, stderr.String())
	}

	combined := stdout.String() + "\n" + stderr.String()
	if !strings.Contains(combined, "OmniCoder-ICX — All-in-One Autonomous Coding Engine") {
		t.Fatalf("expected root usage banner, got:\n%s", combined)
	}
	if strings.Contains(combined, "omnicoder>") {
		t.Fatalf("piped stdin mistakenly entered conversational prompt loop")
	}
	t.Logf("Piped stdin zero-argument launch PASS: correctly printed root usage without hanging")
}

func TestAdversarial_ZeroArgs_NullRedirection_TerminalDetection(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open os.DevNull: %v", err)
	}
	defer devNull.Close()

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Stdin = devNull
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		t.Fatalf("zero-argument execution with devNull failed: %v\nStderr: %s", err, stderr.String())
	}

	combined := stdout.String() + "\n" + stderr.String()
	// An interactive terminal check should distinguish between a real interactive console
	// and a null/bit-bucket device like NUL / /dev/null.
	// If it fails to distinguish, it prints conversational UI banner/prompt and immediate goodbye.
	if strings.Contains(combined, "omnicoder>") || strings.Contains(combined, "OMNICODER-ICX  ::") {
		t.Errorf("TERMINAL DETECTION FLAW: zero-arg launch with os.DevNull entered conversational UI instead of printing usage!\nOutput:\n%s", combined)
	}
	if !strings.Contains(combined, "Usage:") {
		t.Errorf("expected usage output on null redirection, got:\n%s", combined)
	}
}

func TestAdversarial_ChatSubcommand_ExplicitInvocation_WithDevNull(t *testing.T) {
	binPath := findOmnicoderBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open os.DevNull: %v", err)
	}
	defer devNull.Close()

	// Explicit `chat` subcommand MUST enter conversational UI even if stdin is EOF/devNull
	cmd := exec.CommandContext(ctx, binPath, "chat", "--offline", "--workspace="+t.TempDir())
	cmd.Stdin = devNull
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		t.Fatalf("chat subcommand failed: %v\nStderr: %s", err, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "OMNICODER-ICX") {
		t.Fatalf("expected welcome banner in chat subcommand output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "omnicoder>") {
		t.Fatalf("expected prompt in chat subcommand output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "Goodbye") {
		t.Fatalf("expected goodbye on EOF from devNull, got:\n%s", outStr)
	}
	t.Logf("Explicit chat subcommand with devNull PASS: correctly entered chat UI and handled EOF cleanly")
}

func TestAdversarial_TerminalDetection_Oracle(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows console test on non-windows")
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open os.DevNull: %v", err)
	}
	defer devNull.Close()

	fi, err := devNull.Stat()
	if err != nil {
		t.Fatalf("failed to stat devNull: %v", err)
	}
	isCharDev := (fi.Mode() & os.ModeCharDevice) != 0
	t.Logf("os.DevNull Stat().Mode(): %v, ModeCharDevice: %v", fi.Mode(), isCharDev)

	cleanup := ui.SetupTerminalModes(devNull, devNull)
	if cleanup == nil {
		t.Errorf("expected non-nil cleanup function from SetupTerminalModes")
	} else {
		cleanup()
	}

	if !isCharDev {
		t.Errorf("expected NUL to have os.ModeCharDevice on Windows")
	}
}

func TestAdversarial_InteractiveTerminal_PromptLoop_Simulation(t *testing.T) {
	tempWs := t.TempDir()

	inputScript := strings.Join([]string{
		"/help",
		"/status",
		"/map 100",
		"/reset",
		"/exit",
	}, "\n") + "\n"

	input := strings.NewReader(inputScript)
	var output bytes.Buffer

	orch, err := engine.NewOrchestrator(engine.EngineConfig{
		WorkspaceDir: tempWs,
		Offline:      true,
		MaxTurns:     1,
		DecisionGate: llm.NewMockDecisionGate(),
	})
	if err != nil {
		t.Fatalf("failed to create orchestrator: %v", err)
	}
	defer orch.Close()

	session := ui.NewSession(nil, orch, tempWs)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = ui.RunConversationalUI(ctx, session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI returned unexpected error: %v", err)
	}

	outStr := output.String()

	// 1. Verify Banner Rendering
	if !strings.Contains(outStr, "OMNICODER-ICX") {
		t.Errorf("missing banner OMNICODER-ICX in output")
	}
	if !strings.Contains(outStr, "gemini-3.5-flash-lite") {
		t.Errorf("missing default model badge in output")
	}
	if !strings.Contains(outStr, "8-Stage Engine") {
		t.Errorf("missing subterranean badge in output")
	}

	// 2. Verify Prompt Rendering
	promptCount := strings.Count(outStr, "omnicoder>")
	if promptCount < 5 {
		t.Errorf("expected at least 5 prompt renderings ('omnicoder>'), got %d", promptCount)
	}

	// 3. Verify Slash Command Handlers
	if !strings.Contains(outStr, "OmniCoder-ICX Slash Commands:") {
		t.Errorf("missing /help output")
	}
	if !strings.Contains(outStr, "Session Status:") {
		t.Errorf("missing /status output")
	}
	if !strings.Contains(outStr, "Repository Map Synthesized:") {
		t.Errorf("missing /map output")
	}
	if !strings.Contains(outStr, "Dialogue history cleared") {
		t.Errorf("missing /reset confirmation")
	}
	if !strings.Contains(outStr, "Exiting OmniCoder-ICX. Goodbye!") {
		t.Errorf("missing /exit goodbye message")
	}

	t.Logf("Interactive Prompt Loop PASS: banner, badges, %d prompt loops, and all slash commands executed cleanly", promptCount)
}



