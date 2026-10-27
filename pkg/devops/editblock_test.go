// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestParseAiderEditBlocks(t *testing.T) {
	sampleText := `Here is the patch:

service.go
<<<<<<< SEARCH
func Old() {
    println("old")
}
=======
func New() {
    println("new")
}
>>>>>>> REPLACE

### utils.go
<<<<<<< SEARCH
const Version = "1.0.0"
=======
const Version = "2.0.0"
>>>>>>> REPLACE
`
	blocks, err := devops.ParseAiderEditBlocks(sampleText, "")
	if err != nil {
		t.Fatalf("ParseAiderEditBlocks failed: %v", err)
	}

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	if blocks[0].FilePath != "service.go" || !strings.Contains(blocks[0].ReplaceText, "func New()") {
		t.Errorf("block 0 mismatch: %+v", blocks[0])
	}

	if blocks[1].FilePath != "utils.go" || !strings.Contains(blocks[1].ReplaceText, "2.0.0") {
		t.Errorf("block 1 mismatch: %+v", blocks[1])
	}

	// Malformed: unclosed SEARCH
	malformed1 := `<<<<<<< SEARCH
func Incomplete()
`
	if _, err := devops.ParseAiderEditBlocks(malformed1, "main.go"); err == nil {
		t.Errorf("expected error for unclosed search block")
	}

	// Malformed: missing REPLACE divider
	malformed2 := `<<<<<<< SEARCH
func A()
=======
func B()
`
	if _, err := devops.ParseAiderEditBlocks(malformed2, "main.go"); err == nil {
		t.Errorf("expected error for unclosed replace block")
	}

	// Malformed: missing file header
	malformed3 := `<<<<<<< SEARCH
func A()
=======
func B()
>>>>>>> REPLACE`
	if _, err := devops.ParseAiderEditBlocks(malformed3, ""); err == nil {
		t.Errorf("expected error when no file path is specified or defaulted")
	}
}

func TestReplaceFuzzy_Tier1_Exact(t *testing.T) {
	orig := "func Calculate() int {\n    return 40 + 2\n}\n"
	search := "    return 40 + 2"
	replace := "    return 100"

	res, added, removed, err := devops.ReplaceFuzzy(orig, search, replace, false)
	if err != nil {
		t.Fatalf("Tier 1 exact match failed: %v", err)
	}

	if !strings.Contains(res, "return 100") || strings.Contains(res, "40 + 2") {
		t.Errorf("unexpected replaced content:\n%s", res)
	}
	if added != 1 || removed != 1 {
		t.Errorf("lines count mismatch: added=%d, removed=%d", added, removed)
	}
}

func TestReplaceFuzzy_Tier2_TrailingWhitespace(t *testing.T) {
	// Original content has trailing whitespace and mixed line endings
	orig := "func Run() {   \r\n    step1()  \t\r\n    step2()\r\n}\r\n"
	// Search text has clean trimmed lines
	search := "func Run() {\n    step1()\n    step2()\n}"
	replace := "func Run() {\n    stepAll()\n}"

	res, _, _, err := devops.ReplaceFuzzy(orig, search, replace, true)
	if err != nil {
		t.Fatalf("Tier 2 trailing whitespace match failed: %v", err)
	}

	if !strings.Contains(res, "stepAll()") || strings.Contains(res, "step1()") {
		t.Errorf("Tier 2 mismatch, got:\n%s", res)
	}
}

func TestReplaceFuzzy_Tier3_IndentationRelaxed(t *testing.T) {
	// Original content is nested 8 spaces deep
	orig := `func Handle() {
        if check {
            doFirst()
            doSecond()
        }
}`
	// Search block has only 4 spaces
	search := `    if check {
        doFirst()
        doSecond()
    }`
	replace := `    if check {
        doUnified()
    }`

	res, _, _, err := devops.ReplaceFuzzy(orig, search, replace, true)
	if err != nil {
		t.Fatalf("Tier 3 indentation relaxed match failed: %v", err)
	}

	if !strings.Contains(res, "doUnified()") || strings.Contains(res, "doFirst()") {
		t.Errorf("Tier 3 mismatch, got:\n%s", res)
	}
	// Verify that replaced line preserved the candidate 8-space base + 4-space inner indentation
	if !strings.Contains(res, "            doUnified()") {
		t.Errorf("Tier 3 replacement indentation not preserved properly:\n%s", res)
	}
}

func TestReplaceFuzzy_Tier4_BlankLineRelaxed(t *testing.T) {
	// Original content has extra blank lines
	orig := `func Flow() {

    initStep()

    execStep()

}`
	// Search block has no blank lines
	search := `func Flow() {
    initStep()
    execStep()
}`
	replace := `func Flow() {
    completeFlow()
}`

	res, _, _, err := devops.ReplaceFuzzy(orig, search, replace, true)
	if err != nil {
		t.Fatalf("Tier 4 blank line match failed: %v", err)
	}

	if !strings.Contains(res, "completeFlow()") || strings.Contains(res, "initStep()") {
		t.Errorf("Tier 4 mismatch, got:\n%s", res)
	}
}

func TestReplaceFuzzy_FuzzyDisabled(t *testing.T) {
	orig := "line1  \nline2\n"
	search := "line1\nline2\n"
	replace := "new"

	_, _, _, err := devops.ReplaceFuzzy(orig, search, replace, false)
	if err == nil || !strings.Contains(err.Error(), "EXACT_SEARCH_MISMATCH") {
		t.Fatalf("expected EXACT_SEARCH_MISMATCH when fuzzy is disabled, got %v", err)
	}
}

func TestApplyEditBlocks_MultiFileTransactional(t *testing.T) {
	tmpDir := t.TempDir()

	f1 := filepath.Join(tmpDir, "f1.go")
	f2 := filepath.Join(tmpDir, "f2.go")
	f3 := filepath.Join(tmpDir, "f3.go")

	_ = os.WriteFile(f1, []byte("package f1\nconst V1 = 1\n"), 0644)
	_ = os.WriteFile(f2, []byte("package f2\nconst V2 = 2\n"), 0644)
	_ = os.WriteFile(f3, []byte("package f3\nconst V3 = 3\n"), 0644)

	blocks := []devops.EditBlock{
		{FilePath: "f1.go", SearchText: "const V1 = 1", ReplaceText: "const V1 = 10"},
		{FilePath: "f2.go", SearchText: "const V2 = 2", ReplaceText: "const V2 = 20"},
		{FilePath: "f3.go", SearchText: "const V3 = 3", ReplaceText: "const V3 = 30"},
	}

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:   tmpDir,
		Blocks:     blocks,
		FuzzyMatch: true,
	})
	if err != nil {
		t.Fatalf("ApplyEditBlocks failed: %v", err)
	}

	if res.Status != "APPLIED" || !res.Applied {
		t.Errorf("status mismatch: status=%s applied=%v", res.Status, res.Applied)
	}
	if len(res.FilesModified) != 3 || res.BlocksApplied != 3 {
		t.Errorf("counts mismatch: files=%d blocks=%d", len(res.FilesModified), res.BlocksApplied)
	}
	if res.CASHash == "" || res.MerkleSeal == "" {
		t.Errorf("missing CAS hash or Merkle seal")
	}
	expectedSeal := canonical.SHA256Of(map[string]any{
		"patch_id":       res.PatchID,
		"cas_hash":       res.CASHash,
		"files_modified": res.FilesModified,
		"blocks_applied": res.BlocksApplied,
		"lines_added":    res.LinesAdded,
		"lines_removed":  res.LinesRemoved,
	})
	if res.MerkleSeal != expectedSeal {
		t.Errorf("merkle seal mismatch: got %s, expected %s", res.MerkleSeal, expectedSeal)
	}

	// Verify all 3 files modified on disk
	b1, _ := os.ReadFile(f1)
	b2, _ := os.ReadFile(f2)
	b3, _ := os.ReadFile(f3)

	if !strings.Contains(string(b1), "const V1 = 10") {
		t.Errorf("f1 was not updated on disk: %s", string(b1))
	}
	if !strings.Contains(string(b2), "const V2 = 20") {
		t.Errorf("f2 was not updated on disk: %s", string(b2))
	}
	if !strings.Contains(string(b3), "const V3 = 30") {
		t.Errorf("f3 was not updated on disk: %s", string(b3))
	}
}

func TestApplyEditBlocks_AtomicRollback_SimulatedFailure(t *testing.T) {
	tmpDir := t.TempDir()

	f1 := filepath.Join(tmpDir, "f1.go")
	f2 := filepath.Join(tmpDir, "f2.go")

	content1 := "package f1\nconst V1 = 1\n"
	content2 := "package f2\nconst V2 = 2\n"

	_ = os.WriteFile(f1, []byte(content1), 0644)
	_ = os.WriteFile(f2, []byte(content2), 0644)

	// Block 1 matches, Block 2 fails
	blocks := []devops.EditBlock{
		{FilePath: "f1.go", SearchText: "const V1 = 1", ReplaceText: "const V1 = 100"},
		{FilePath: "f2.go", SearchText: "NON_EXISTENT_STRING", ReplaceText: "const V2 = 200"},
	}

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:   tmpDir,
		Blocks:     blocks,
		FuzzyMatch: true,
	})
	if err == nil {
		t.Fatalf("expected error on failed block")
	}
	if res.Status != "REJECTED" || res.Applied {
		t.Errorf("expected status REJECTED, got %s", res.Status)
	}

	// Ensure f1 on disk was NOT touched
	b1, _ := os.ReadFile(f1)
	if string(b1) != content1 {
		t.Fatalf("f1 was corrupted on disk despite transaction failure: %s", string(b1))
	}
}

func TestApplyEditBlocks_DryRun(t *testing.T) {
	tmpDir := t.TempDir()
	f1 := filepath.Join(tmpDir, "dry.go")
	orig := "package dry\nfunc Hello() {}\n"
	_ = os.WriteFile(f1, []byte(orig), 0644)

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath: tmpDir,
		Blocks: []devops.EditBlock{
			{FilePath: "dry.go", SearchText: "func Hello() {}", ReplaceText: "func Goodbye() {}"},
		},
		DryRun:     true,
		FuzzyMatch: true,
	})
	if err != nil {
		t.Fatalf("dry run error: %v", err)
	}

	if res.Status != "APPLIED" || res.Applied {
		t.Errorf("dry run status mismatch: status=%s applied=%v", res.Status, res.Applied)
	}

	// File on disk must remain untouched
	b, _ := os.ReadFile(f1)
	if string(b) != orig {
		t.Fatalf("dry run mutated file on disk: %s", string(b))
	}
}

func TestApplyEditBlocks_ConcurrencyRace(t *testing.T) {
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			orig := "func Test() {\n    val := 1\n}\n"
			search := "    val := 1"
			replace := "    val := 2"
			_, _, _, err := devops.ReplaceFuzzy(orig, search, replace, true)
			if err != nil {
				t.Errorf("goroutine %d ReplaceFuzzy failed: %v", idx, err)
			}

			blocksText := `main.go
<<<<<<< SEARCH
val := 1
=======
val := 2
>>>>>>> REPLACE`
			_, err = devops.ParseAiderEditBlocks(blocksText, "")
			if err != nil {
				t.Errorf("goroutine %d ParseAiderEditBlocks failed: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
}

func TestApplyEditBlocks_NewEmptyFile_WithSearchBlock(t *testing.T) {
	tmpDir := t.TempDir()

	// Target file does not exist initially
	targetFile := filepath.Join(tmpDir, "newfile.go")

	blocks := []devops.EditBlock{
		{
			FilePath:    "newfile.go",
			SearchText:  "non_existent_search_content_that_would_otherwise_fail",
			ReplaceText: "package newfile\n\nfunc Hello() string { return \"world\" }\n",
		},
	}

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:   tmpDir,
		Blocks:     blocks,
		FuzzyMatch: true,
	})
	if err != nil {
		t.Fatalf("ApplyEditBlocks failed on new file creation: %v", err)
	}

	if !res.Applied || res.Status != "APPLIED" {
		t.Fatalf("expected APPLIED status, got %s", res.Status)
	}

	content, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("target file was not created on disk: %v", err)
	}

	if !strings.Contains(string(content), "func Hello() string") {
		t.Errorf("unexpected file content: %s", string(content))
	}
}

func TestApplyEditBlocks_NewEmptyFile_MultiBlocks_AndAlreadyApplied(t *testing.T) {
	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "server.go")

	blocks := []devops.EditBlock{
		{
			FilePath:    "server.go",
			SearchText:  "",
			ReplaceText: "package server\n\nfunc Start() error {\n    return nil\n}\n",
		},
		{
			FilePath:    "server.go",
			SearchText:  "non_matching_search_target",
			ReplaceText: "func Stop() error {\n    return nil\n}\n",
		},
		{
			FilePath:    "server.go",
			SearchText:  "another_non_matching_search_target",
			ReplaceText: "func Start() error", // already present in file
		},
	}

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:   tmpDir,
		Blocks:     blocks,
		FuzzyMatch: true,
	})
	if err != nil {
		t.Fatalf("expected multi-block on new file to succeed, got: %v", err)
	}
	if !res.Applied || res.Status != "APPLIED" {
		t.Fatalf("expected APPLIED status, got: %s", res.Status)
	}

	content, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("failed to read created file: %v", err)
	}
	strContent := string(content)
	if !strings.Contains(strContent, "func Start()") || !strings.Contains(strContent, "func Stop()") {
		t.Fatalf("expected both functions in created file, got:\n%s", strContent)
	}
}

func TestApplyEditBlocks_PreservesCRLF(t *testing.T) {
	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "crlf_test.txt")
	crlfContent := "Line One\r\nLine Two\r\nLine Three\r\n"
	if err := os.WriteFile(targetFile, []byte(crlfContent), 0644); err != nil {
		t.Fatalf("failed to write crlf file: %v", err)
	}

	blocks := []devops.EditBlock{
		{
			FilePath:    "crlf_test.txt",
			SearchText:  "Line Two",
			ReplaceText: "Line Two Modified",
		},
	}

	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:   tmpDir,
		Blocks:     blocks,
		FuzzyMatch: true,
	})
	if err != nil {
		t.Fatalf("ApplyEditBlocks failed: %v", err)
	}
	if !res.Applied {
		t.Fatalf("expected patch to be applied")
	}

	raw, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("failed to read target file: %v", err)
	}
	updatedStr := string(raw)
	if !strings.Contains(updatedStr, "Line Two Modified") {
		t.Fatalf("expected replacement to be in file")
	}
	if !strings.Contains(updatedStr, "\r\n") {
		t.Fatalf("expected CRLF line endings to be preserved, got LF-only")
	}
}
