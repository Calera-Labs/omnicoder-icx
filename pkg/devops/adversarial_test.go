// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// ============================================================================
// SECTION 1: PageRank on dense, highly-cyclic graphs & empty/syntax-error files
// ============================================================================

// TestAdversarial_PageRank_CompleteGraphK50 tests PageRank on a 50-node complete graph
// (every file references every other file's symbols).
func TestAdversarial_PageRank_CompleteGraphK50(t *testing.T) {
	tmpDir := t.TempDir()
	const n = 50

	// Create 50 files. Each file defines a unique symbol and references all other 49 symbols.
	for i := 0; i < n; i++ {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("package mesh\n\ntype Node%02d struct{}\n\nfunc Connect%02d() {\n", i, i))
		for j := 0; j < n; j++ {
			if i != j {
				sb.WriteString(fmt.Sprintf("\t_ = Node%02d{}\n", j))
			}
		}
		sb.WriteString("}\n")
		filename := filepath.Join(tmpDir, fmt.Sprintf("node_%02d.go", i))
		if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
			t.Fatalf("failed to write file %d: %v", i, err)
		}
	}

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  2048,
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed on complete graph K50: %v", err)
	}

	if res.FilesScanned != n {
		t.Errorf("expected %d files scanned, got %d", n, res.FilesScanned)
	}

	// Verify all scores are valid numbers (no NaN, no Inf, positive)
	for _, sym := range res.TopSymbols {
		if math.IsNaN(sym.Score) || math.IsInf(sym.Score, 0) || sym.Score <= 0 {
			t.Fatalf("invalid PageRank score for symbol %s: %v", sym.Name, sym.Score)
		}
	}

	// In a fully symmetric complete graph without query/focus bias, all node scores should be equal
	if len(res.TopSymbols) >= 2 {
		firstScore := res.TopSymbols[0].Score
		for _, sym := range res.TopSymbols {
			diff := math.Abs(sym.Score - firstScore)
			if diff > 1e-4 {
				t.Errorf("complete graph symmetry broken: symbol %s score %f vs first %f (diff: %f)",
					sym.Name, sym.Score, firstScore, diff)
			}
		}
	}
}

// TestAdversarial_PageRank_DanglingAndIsolatedNodes tests PageRank with mixed isolated nodes,
// dangling nodes (no outgoing refs), and disconnected cyclic components.
func TestAdversarial_PageRank_DanglingAndIsolatedNodes(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Cycle: a -> b -> c -> a
	_ = os.WriteFile(filepath.Join(tmpDir, "cycle_a.go"), []byte("package p\ntype SymA struct{}\nfunc RefB() { _ = SymB{} }"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "cycle_b.go"), []byte("package p\ntype SymB struct{}\nfunc RefC() { _ = SymC{} }"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "cycle_c.go"), []byte("package p\ntype SymC struct{}\nfunc RefA() { _ = SymA{} }"), 0644)

	// 2. Dangling node: has symbol SymD, references nothing
	_ = os.WriteFile(filepath.Join(tmpDir, "dangling.go"), []byte("package p\ntype SymD struct{}"), 0644)

	// 3. Sink node: references SymD, but has no symbols itself
	_ = os.WriteFile(filepath.Join(tmpDir, "sink.go"), []byte("package p\nfunc UseD() { _ = SymD{} }"), 0644)

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	for _, s := range res.TopSymbols {
		if math.IsNaN(s.Score) || math.IsInf(s.Score, 0) {
			t.Fatalf("NaN/Inf detected in PageRank: symbol %s has score %v", s.Name, s.Score)
		}
	}
}

// TestAdversarial_PageRank_EmptyAndSyntaxErrorFiles tests behavior when the repository
// contains completely empty files, binary garbage, and syntax errors across languages.
func TestAdversarial_PageRank_EmptyAndSyntaxErrorFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Empty files
	_ = os.WriteFile(filepath.Join(tmpDir, "empty.go"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "empty.ts"), []byte("   \n\t  \r\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "empty.py"), []byte("# just a comment\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "empty.rs"), []byte("// comment only\n"), 0644)

	// Binary garbage in .go file
	_ = os.WriteFile(filepath.Join(tmpDir, "corrupt.go"), []byte{0x00, 0xFF, 0xFE, 0x12, 0x34, 0x00, 0x00}, 0644)

	// Severe syntax errors
	_ = os.WriteFile(filepath.Join(tmpDir, "broken.go"), []byte("package broken\nfunc ((((( {{{{ ;;;"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "broken.ts"), []byte("export class {{{{ func (("), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "broken.py"), []byte("def ((((( :::: \n    class 999:"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "broken.rs"), []byte("pub struct {{{{ fn (((("), 0644)

	// One valid file to ensure there is at least one symbol
	_ = os.WriteFile(filepath.Join(tmpDir, "valid.go"), []byte("package ok\ntype ValidService struct{}\nfunc (v *ValidService) Serve() {}"), 0644)

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap crashed on malformed files: %v", err)
	}

	if res.SymbolsFound == 0 {
		t.Errorf("expected at least valid.go symbols to be found, got 0")
	}
}

// ============================================================================
// SECTION 2: Token budget clamping (<1024 tokens regardless of repo size)
// ============================================================================

// TestAdversarial_TokenBudgetClamping_SingleMassiveFile tests that when a single file
// contains hundreds of symbols, the generated outline does NOT exceed the token budget.
func TestAdversarial_TokenBudgetClamping_SingleMassiveFile(t *testing.T) {
	tmpDir := t.TempDir()

	// 500 functions in a single file -> ~25,000 characters (~7,000 tokens)
	var sb strings.Builder
	sb.WriteString("package monolith\n\n")
	for i := 0; i < 500; i++ {
		sb.WriteString(fmt.Sprintf("func MassiveFunctionNumber%04d(arg1 int, arg2 string) (bool, error) { return true, nil }\n", i))
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "monolith.go"), []byte(sb.String()), 0644)

	budgets := []int{100, 250, 500, 1024}
	for _, budget := range budgets {
		t.Run(fmt.Sprintf("Budget_%d", budget), func(t *testing.T) {
			res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
				WorkspaceDir: tmpDir,
				TokenBudget:  budget,
			})
			if err != nil {
				t.Fatalf("SynthesizeRepoMap failed: %v", err)
			}

			// Estimate tokens using the same formula
			estimated := res.TokenCount
			t.Logf("Requested budget: %d, Result TokenCount: %d, RepoMap len: %d chars",
				budget, estimated, len(res.RepoMap))

			if estimated > budget {
				t.Errorf("TOKEN_BUDGET_OVERFLOW: requested budget %d, but outline has %d tokens (len %d chars)\nOutline preview:\n%s",
					budget, estimated, len(res.RepoMap), res.RepoMap[:min(300, len(res.RepoMap))])
			}
		})
	}
}

// TestAdversarial_TokenBudgetClamping_LargeMultiFileRepo tests 100 files with 30 symbols each.
func TestAdversarial_TokenBudgetClamping_LargeMultiFileRepo(t *testing.T) {
	tmpDir := t.TempDir()

	// 100 files, each with 30 functions -> 3,000 symbols total
	for i := 0; i < 100; i++ {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("package pkg%03d\n\n", i))
		for j := 0; j < 30; j++ {
			sb.WriteString(fmt.Sprintf("func Pkg%03d_Method%03d() string { return \"ok\" }\n", i, j))
		}
		_ = os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("service_%03d.go", i)), []byte(sb.String()), 0644)
	}

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	t.Logf("Large multi-file repo: %d files scanned, %d symbols found, TokenCount: %d",
		res.FilesScanned, res.SymbolsFound, res.TokenCount)

	if res.TokenCount > 1024 {
		t.Errorf("TOKEN_BUDGET_OVERFLOW: outline exceeded 1024 tokens: got %d tokens", res.TokenCount)
	}
}

// ============================================================================
// SECTION 3: Compiler diagnostic parser with malformed stderr, Unicode, Windows paths
// ============================================================================

// TestAdversarial_CompilerDiagnostics_WindowsPathsWithSpaces tests parsing errors
// with Windows drive letters and paths containing SPACES.
func TestAdversarial_CompilerDiagnostics_WindowsPathsWithSpaces(t *testing.T) {
	// Standard Windows paths with spaces like "C:\Workspace\Sample Project\main.go"
	rawStderr := `
C:\Workspace\Sample Project\Module 10\service.go:42:15: undefined: Engine
C:\Workspace\Sample Project\Module 10\handler.ts(25,10): error TS2304: Cannot find name 'Config'
C:\Workspace\Sample Project\Module 10\handler.ts:30:5 - error TS2554: Expected 2 arguments, but got 1
--> C:\Workspace\Sample Project\Module 10\kernel.rs:50:8
error[E0382]: borrow of moved value: 'session'
C:\Workspace\Sample Project\Module 10\app.py:10: error: Incompatible types in assignment
C:\Workspace\Sample Project\Module 10\native.c:88:4: error: unknown type name 'size_t'
C:\Workspace\Sample Project\Module 10\driver.cpp(120,5): error C2065: 'dev': undeclared identifier
`
	diags := devops.ParseCompilerDiagnostics(rawStderr, "")
	t.Logf("Parsed %d diagnostics from stderr with spaces in path", len(diags))

	expectedErrors := map[string]string{
		"go":     "undefined: Engine",
		"ts1":    "Cannot find name 'Config'",
		"ts2":    "Expected 2 arguments, but got 1",
		"rust":   "borrow of moved value: 'session'",
		"py":     "Incompatible types in assignment",
		"c":      "unknown type name 'size_t'",
		"msvc":   "'dev': undeclared identifier",
	}

	foundGo := false
	foundTS := false
	foundRust := false
	foundPy := false
	foundC := false

	for _, d := range diags {
		t.Logf("Diagnostic: File=%s, Line=%d, Msg=%s", d.File, d.Line, d.Message)
		if strings.Contains(d.Message, expectedErrors["go"]) {
			foundGo = true
		}
		if strings.Contains(d.Message, expectedErrors["ts1"]) || strings.Contains(d.Message, expectedErrors["ts2"]) {
			foundTS = true
		}
		if strings.Contains(d.Message, expectedErrors["rust"]) {
			foundRust = true
		}
		if strings.Contains(d.Message, expectedErrors["py"]) {
			foundPy = true
		}
		if strings.Contains(d.Message, expectedErrors["c"]) || strings.Contains(d.Message, expectedErrors["msvc"]) {
			foundC = true
		}
	}

	if !foundGo {
		t.Errorf("Go diagnostic parser failed on Windows path with spaces")
	}
	if !foundTS {
		t.Errorf("TS diagnostic parser failed on Windows path with spaces")
	}
	if !foundRust {
		t.Errorf("Rust diagnostic parser failed on Windows path with spaces")
	}
	if !foundPy {
		t.Errorf("Python diagnostic parser failed on Windows path with spaces")
	}
	if !foundC {
		t.Errorf("C/C++ diagnostic parser failed on Windows path with spaces")
	}
}

// TestAdversarial_CompilerDiagnostics_UnicodeAndSpecialChars tests non-ASCII filenames and error text.
func TestAdversarial_CompilerDiagnostics_UnicodeAndSpecialChars(t *testing.T) {
	rawStderr := `
C:\项目\тест\service_café.go:18:2: undefined: CrèmeBrûlée
/workspace/src/компилятор/main.rs:25:5
error[E0425]: cannot find value 'функция' in this scope
C:\dev\rocket_🚀.py:40: error: Argument 1 to "run" has incompatible type "int"
`
	diags := devops.ParseCompilerDiagnostics(rawStderr, "")
	t.Logf("Parsed %d diagnostics from Unicode stderr", len(diags))

	foundGo := false
	for _, d := range diags {
		t.Logf("Unicode diag: File=%s, Line=%d, Msg=%s", d.File, d.Line, d.Message)
		if strings.Contains(d.Message, "CrèmeBrûlée") {
			foundGo = true
		}
	}

	if !foundGo {
		t.Errorf("Go diagnostic parser failed on Unicode path or message")
	}
}

// TestAdversarial_CompilerDiagnostics_MalformedAndAdversarialStderr tests ANSI bombs,
// binary characters, and massive output strings.
func TestAdversarial_CompilerDiagnostics_MalformedAndAdversarialStderr(t *testing.T) {
	// ANSI sequence bomb + null bytes + truncated lines
	adversarialStderr := "\x1b[31;1;4m\x1b[?25h\x00\x01\x02\n" +
		strings.Repeat("A", 10000) + "\n" +
		"package: broken.go:invalid\n" +
		"::::\n" +
		"main.go:99999999999999999999999999999:99999: out of range\n" +
		"\x1b]0;Title\x07\x1b[2J\n" +
		"main.go:12:3: valid error message\n"

	diags := devops.ParseCompilerDiagnostics(adversarialStderr, "")
	if len(diags) == 0 {
		t.Errorf("expected at least 1 diagnostic recovered from adversarial stderr")
	}

	// Test repair prompt payload clamping
	payload := devops.SynthesizeRepairPayload("go build", 1, diags, adversarialStderr)
	tokens := devops.EstimateTokens(payload)
	t.Logf("SynthesizeRepairPayload produced %d tokens (len: %d chars)", tokens, len(payload))
	if tokens > 300 {
		t.Errorf("REPAIR_PAYLOAD_OVERFLOW: expected < 300 tokens, got %d", tokens)
	}
}

// ============================================================================
// SECTION 4: Git atomic patch applier (corrupt diffs, conflicting context, -race)
// ============================================================================

// TestAdversarial_GitPatch_ConflictingContext tests what happens when a patch has
// context lines that do NOT match the target file.
func TestAdversarial_GitPatch_ConflictingContext(t *testing.T) {
	originalText := `line_one
line_two
line_three
line_four
line_five`

	// This diff claims that lines around line_three are "WRONG_CONTEXT_A" and "WRONG_CONTEXT_B",
	// which do not exist in the original text!
	diffHunk := `--- a/file.txt
+++ b/file.txt
@@ -2,3 +2,3 @@
 WRONG_CONTEXT_A
-line_three
+REPLACED_THREE
 WRONG_CONTEXT_B
`

	patched, added, removed, err := devops.ApplyPatchToText(originalText, diffHunk)
	t.Logf("Conflicting context result:\npatched:\n%s\nadded: %d, removed: %d, err: %v",
		patched, added, removed, err)

	// An atomic patch applier MUST reject conflicting context lines or detect mismatch.
	// If it blindly succeeds and replaces text with the wrong context, it's a silent corruption bug!
	if err == nil {
		if strings.Contains(patched, "WRONG_CONTEXT_A") {
			t.Errorf("SILENT_CORRUPTION: ApplyPatchToText blindly injected conflicting context into output without error!\nOutput:\n%s", patched)
		}
	}
}

// TestAdversarial_GitPatch_ConflictingDeletion tests deleting a line that does not match.
func TestAdversarial_GitPatch_ConflictingDeletion(t *testing.T) {
	originalText := `security_check_enabled = true
allow_admin = false`

	// Diff attempts to delete "database_url = local" at line 1, which does not exist!
	diffHunk := `--- a/config.py
+++ b/config.py
@@ -1,2 +1,2 @@
-database_url = local
+database_url = remote
 allow_admin = false
`

	patched, _, _, err := devops.ApplyPatchToText(originalText, diffHunk)
	t.Logf("Conflicting deletion result: patched:\n%s\nerr: %v", patched, err)

	// If err == nil, it blindly deleted security_check_enabled = true!
	if err == nil {
		if !strings.Contains(patched, "security_check_enabled = true") {
			t.Errorf("SILENT_DELETION_BUG: ApplyPatchToText deleted non-matching line 'security_check_enabled'!\nOutput:\n%s", patched)
		}
	}
}

// TestAdversarial_GitPatch_CorruptDiffHeaders tests malformed and corrupt hunk headers.
func TestAdversarial_GitPatch_CorruptDiffHeaders(t *testing.T) {
	corruptDiffs := []struct {
		name string
		diff string
	}{
		{
			name: "MalformedHunkHeader_NoAtAt",
			diff: "--- a/f.txt\n+++ b/f.txt\n@@ -abc,def +1,2 @@\n+test\n",
		},
		{
			name: "HunkHeader_MissingPlus",
			diff: "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 @@\n-old\n+new\n",
		},
		{
			name: "TruncatedHunk_CountMismatch",
			diff: "--- a/f.txt\n+++ b/f.txt\n@@ -1,100 +1,100 @@\n-line\n",
		},
	}

	for _, tc := range corruptDiffs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := devops.ParseUnifiedDiff(tc.diff)
			t.Logf("%s: parse err: %v", tc.name, err)
		})
	}
}

// TestAdversarial_GitPatch_ConcurrentApplication_Race tests concurrent patch application
// under the Go race detector (-race).
func TestAdversarial_GitPatch_ConcurrentApplication_Race(t *testing.T) {
	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "concurrent.txt")
	initialContent := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	if err := os.WriteFile(targetFile, []byte(initialContent), 0644); err != nil {
		t.Fatal(err)
	}

	diffTemplate := `--- a/concurrent.txt
+++ b/concurrent.txt
@@ -1,2 +1,2 @@
-line 1
+line 1 modified by routine %d
 line 2
`

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			diff := fmt.Sprintf(diffTemplate, id)

			// Concurrent in-memory patch testing
			_, _, _, _ = devops.ApplyPatchToText(initialContent, diff)

			// Concurrent full patch testing (DryRun to avoid filesystem write race collisions)
			_, _ = devops.ApplyPatch(devops.GitPatchRequest{
				RepoPath:   tmpDir,
				TargetFile: "concurrent.txt",
				DiffHunk:   diff,
				DryRun:     true,
			})
		}(i)
	}

	wg.Wait()
}

// TestAdversarial_GitPatch_HunkTargetOutOfBounds tests diff targeting line 99999 in a 3-line file.
func TestAdversarial_GitPatch_HunkTargetOutOfBounds(t *testing.T) {
	originalText := "line 1\nline 2\nline 3"
	diffHunk := `--- a/f.txt
+++ b/f.txt
@@ -99999,1 +99999,1 @@
-line 99999
+line 99999 modified
`
	patched, _, _, err := devops.ApplyPatchToText(originalText, diffHunk)
	t.Logf("Out of bounds hunk result: patched:\n%s\nerr: %v", patched, err)
	if err == nil {
		t.Errorf("ApplyPatchToText accepted out-of-bounds line 99999 without error (appended to EOF)")
	}
}

// TestAdversarial_GitPatch_HunksOutOfOrder tests multiple hunks that are inverted in line order.
func TestAdversarial_GitPatch_HunksOutOfOrder(t *testing.T) {
	originalText := "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10"
	diffHunk := `--- a/f.txt
+++ b/f.txt
@@ -8,1 +8,1 @@
-line 8
+line 8 modified
@@ -2,1 +2,1 @@
-line 2
+line 2 modified
`
	patched, _, _, err := devops.ApplyPatchToText(originalText, diffHunk)
	t.Logf("Out of order hunks result: patched:\n%s\nerr: %v", patched, err)
	lines := strings.Split(patched, "\n")
	if len(lines) >= 3 && lines[1] != "line 2 modified" {
		t.Errorf("ApplyPatchToText scrambled lines when hunks were out of order: line 2 was not updated at line 2 position")
	}
}

