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

// Tier 1 — Feature Coverage: Unified Diff Parsing
func TestParseUnifiedDiff_SingleAndMultiHunk(t *testing.T) {
	diffText := `diff --git a/main.go b/main.go
index 83db48f..bf269f4 100644
--- a/main.go
+++ b/main.go
@@ -1,5 +1,6 @@
 package main
 
+import "fmt"
+
 func main() {
-	println("hello")
+	fmt.Println("hello world")
 }
@@ -20,3 +21,4 @@
 func helper() {
+	println("step 2")
 }
`
	diffs, err := devops.ParseUnifiedDiff(diffText)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff failed: %v", err)
	}

	if len(diffs) != 1 {
		t.Fatalf("expected 1 file diff, got %d", len(diffs))
	}

	d := diffs[0]
	if d.OldPath != "main.go" || d.NewPath != "main.go" {
		t.Errorf("paths mismatch: old=%s new=%s", d.OldPath, d.NewPath)
	}
	if len(d.Hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d", len(d.Hunks))
	}
	if d.LinesAdded != 4 || d.LinesRemoved != 1 {
		t.Errorf("lines added/removed mismatch: added=%d, removed=%d", d.LinesAdded, d.LinesRemoved)
	}
}

// Tier 1 — Feature Coverage: In-Memory Line Patch Applier
func TestApplyPatchToText(t *testing.T) {
	orig := `package main

func main() {
	println("hello")
}`

	diffText := `--- a/main.go
+++ b/main.go
@@ -1,4 +1,5 @@
 package main
 
 func main() {
-	println("hello")
+	println("hello world")
+	println("omnicoder")
 }`

	patched, added, removed, err := devops.ApplyPatchToText(orig, diffText)
	if err != nil {
		t.Fatalf("ApplyPatchToText failed: %v", err)
	}

	if added != 2 || removed != 1 {
		t.Errorf("expected added=2 removed=1, got added=%d removed=%d", added, removed)
	}

	expected := `package main

func main() {
	println("hello world")
	println("omnicoder")
}`
	if patched != expected {
		t.Errorf("patched text mismatch:\nGot:\n%s\nExpected:\n%s", patched, expected)
	}
}

// Tier 1 — Feature Coverage: DryRun vs Real Apply & Merkle Seal
func TestApplyPatch_LifecycleAndAttestation(t *testing.T) {
	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "config.go")

	initialCode := `package config

func GetPort() int {
	return 8080
}
`
	if err := os.WriteFile(targetFile, []byte(initialCode), 0644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}

	diffText := `--- a/config.go
+++ b/config.go
@@ -1,5 +1,5 @@
 package config
 
 func GetPort() int {
-	return 8080
+	return 9090
 }
`

	// 1. Dry Run Verification
	reqDry := devops.GitPatchRequest{
		RepoPath:   tmpDir,
		TargetFile: "config.go",
		DiffHunk:   diffText,
		Message:    "update port to 9090",
		DryRun:     true,
	}
	resDry, err := devops.ApplyPatch(reqDry)
	if err != nil {
		t.Fatalf("Dry run failed: %v", err)
	}
	if resDry.Status != "VERIFIED" || resDry.Applied {
		t.Errorf("dry run status mismatch: status=%s applied=%v", resDry.Status, resDry.Applied)
	}

	// Verify file was NOT modified
	diskBytes, _ := os.ReadFile(targetFile)
	if string(diskBytes) != initialCode {
		t.Errorf("dry run mutated file on disk")
	}

	// 2. Real Apply
	reqReal := devops.GitPatchRequest{
		RepoPath:   tmpDir,
		TargetFile: "config.go",
		DiffHunk:   diffText,
		Message:    "update port to 9090",
		Author:     "agent:test",
		DryRun:     false,
	}
	resReal, err := devops.ApplyPatch(reqReal)
	if err != nil {
		t.Fatalf("Real apply failed: %v", err)
	}
	if resReal.Status != "APPLIED" || !resReal.Applied {
		t.Errorf("real apply status mismatch: status=%s applied=%v", resReal.Status, resReal.Applied)
	}

	// 3. Merkle Seal & Hash Attestation
	if resReal.MerkleSeal == "" || resReal.CASHash == "" {
		t.Errorf("missing Merkle seal or CAS hash: seal=%s cas=%s", resReal.MerkleSeal, resReal.CASHash)
	}
	if resReal.OriginalTreeSHA != canonical.SHA256Bytes([]byte(initialCode)) {
		t.Errorf("original tree SHA mismatch: %s", resReal.OriginalTreeSHA)
	}

	// Verify file WAS modified on disk
	updatedBytes, _ := os.ReadFile(targetFile)
	if !strings.Contains(string(updatedBytes), "return 9090") {
		t.Errorf("file on disk does not contain updated port: %s", string(updatedBytes))
	}
}

// Tier 2 — Boundary & Corner Cases: New File Creation & Nested Dirs
func TestApplyPatch_NewFileCreation(t *testing.T) {
	tmpDir := t.TempDir()

	diffText := `--- /dev/null
+++ b/nested/dir/newfile.go
@@ -0,0 +1,3 @@
+package dir
+
+func NewFunc() {}
`
	req := devops.GitPatchRequest{
		RepoPath: tmpDir,
		DiffHunk: diffText,
		DryRun:   false,
	}

	res, err := devops.ApplyPatch(req)
	if err != nil {
		t.Fatalf("ApplyPatch failed to create new file: %v", err)
	}

	if res.Status != "APPLIED" || !res.Applied {
		t.Errorf("status mismatch: %s", res.Status)
	}

	newPath := filepath.Join(tmpDir, "nested", "dir", "newfile.go")
	content, readErr := os.ReadFile(newPath)
	if readErr != nil {
		t.Fatalf("failed to read created file: %v", readErr)
	}

	if !strings.Contains(string(content), "func NewFunc()") {
		t.Errorf("file content incorrect: %s", string(content))
	}
}

// Tier 2 — Boundary & Corner Cases: Empty and Malformed Diffs
func TestParseUnifiedDiff_Errors(t *testing.T) {
	if _, err := devops.ParseUnifiedDiff(""); err == nil {
		t.Errorf("expected error on empty diff, got nil")
	}
	if _, err := devops.ParseUnifiedDiff("   \n\t "); err == nil {
		t.Errorf("expected error on whitespace diff, got nil")
	}
	if _, err := devops.ParseUnifiedDiff("@@ bad @@"); err == nil {
		t.Errorf("expected error on malformed header, got nil")
	}
}

// Tier 2 — Boundary & Corner Cases: Unified Diff Generator
func TestGenerateUnifiedDiff(t *testing.T) {
	oldText := "line1\nline2\n"
	newText := "line1\nline2_modified\n"

	diff := devops.GenerateUnifiedDiff("test.txt", "test.txt", oldText, newText)
	if diff == "" {
		t.Fatalf("expected non-empty diff")
	}

	// Verify the generated diff can be parsed and applied back
	patched, _, _, err := devops.ApplyPatchToText(oldText, diff)
	if err != nil {
		t.Fatalf("failed to apply generated diff: %v", err)
	}
	if patched != newText {
		t.Errorf("roundtrip diff failed:\nGot:\n%s\nExpected:\n%s", patched, newText)
	}
}

// Concurrency race test
func TestGitPatch_ConcurrencyRace(t *testing.T) {
	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			diff := devops.GenerateUnifiedDiff("a.go", "a.go", "a\n", "b\n")
			_, err := devops.ParseUnifiedDiff(diff)
			if err != nil {
				t.Errorf("goroutine %d failed to parse diff: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
}
