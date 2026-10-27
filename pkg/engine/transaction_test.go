// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
)

func TestNewTransaction(t *testing.T) {
	tmpDir := t.TempDir()
	tx := engine.NewTransaction(tmpDir)

	if tx == nil {
		t.Fatal("expected non-nil transaction")
	}
	if !strings.HasPrefix(tx.ID, "tx_") {
		t.Errorf("unexpected tx ID format: %s", tx.ID)
	}
	if tx.RepoDir != tmpDir {
		t.Errorf("expected RepoDir=%s, got %s", tmpDir, tx.RepoDir)
	}
	if tx.Status != engine.TxPending {
		t.Errorf("expected status TxPending, got %v", tx.Status)
	}
	if tx.RollbackFn == nil {
		t.Error("expected non-nil RollbackFn")
	}
	if len(tx.Edits) != 0 {
		t.Errorf("expected 0 edits, got %d", len(tx.Edits))
	}
}

func TestStageFileEdit_SingleFile_Commit(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "calculator.go")
	origContent := `package calc

func Add(a, b int) int {
	return a - b // BUG: subtraction instead of addition
}
`
	if err := os.WriteFile(filePath, []byte(origContent), 0644); err != nil {
		t.Fatalf("failed to write original file: %v", err)
	}

	tx := engine.NewTransaction(tmpDir)
	diff := `--- a/calculator.go
+++ b/calculator.go
@@ -3,3 +3,3 @@
 func Add(a, b int) int {
-	return a - b // BUG: subtraction instead of addition
+	return a + b
 }
`
	edit := engine.FileEdit{
		Path: "calculator.go",
		Diff: diff,
	}

	if err := engine.StageFileEdit(tx, edit); err != nil {
		t.Fatalf("StageFileEdit failed: %v", err)
	}

	if tx.Status != engine.TxStaged {
		t.Fatalf("expected status TxStaged, got %v", tx.Status)
	}
	if len(tx.Edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(tx.Edits))
	}

	// Verify file modified on disk
	modifiedBytes, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed reading modified file: %v", err)
	}
	if !strings.Contains(string(modifiedBytes), "return a + b") {
		t.Errorf("expected file to contain 'return a + b', got:\n%s", string(modifiedBytes))
	}

	// Verify hashes computed
	if tx.Edits[0].OriginalHash != canonical.SHA256Bytes([]byte(origContent)) {
		t.Errorf("original hash mismatch: %s", tx.Edits[0].OriginalHash)
	}
	if tx.Edits[0].PatchedHash != canonical.SHA256Bytes(modifiedBytes) {
		t.Errorf("patched hash mismatch: %s", tx.Edits[0].PatchedHash)
	}

	// Commit transaction
	if err := engine.CommitTransaction(tx); err != nil {
		t.Fatalf("CommitTransaction failed: %v", err)
	}
	if tx.Status != engine.TxCommitted {
		t.Fatalf("expected status TxCommitted, got %v", tx.Status)
	}
	if tx.MerkleSeal == "" {
		t.Error("expected non-empty MerkleSeal on commit")
	}

	// Double commit is idempotent
	if err := engine.CommitTransaction(tx); err != nil {
		t.Errorf("second commit should be idempotent, got: %v", err)
	}
}

func TestStageFileEdit_MultiFile_Commit(t *testing.T) {
	tmpDir := t.TempDir()

	file1 := filepath.Join(tmpDir, "models.go")
	orig1 := "package store\n\ntype User struct {\n\tID int\n}\n"
	if err := os.WriteFile(file1, []byte(orig1), 0644); err != nil {
		t.Fatal(err)
	}

	file2 := filepath.Join(tmpDir, "handlers.go")
	orig2 := "package store\n\nfunc HandleUser() string {\n\treturn \"old\"\n}\n"
	if err := os.WriteFile(file2, []byte(orig2), 0644); err != nil {
		t.Fatal(err)
	}

	tx := engine.NewTransaction(tmpDir)

	edit1 := engine.FileEdit{
		Path: "models.go",
		Diff: `--- a/models.go
+++ b/models.go
@@ -3,3 +3,4 @@
 type User struct {
 	ID int
+	Name string
 }
`,
	}
	edit2 := engine.FileEdit{
		Path: "handlers.go",
		Diff: `--- a/handlers.go
+++ b/handlers.go
@@ -3,3 +3,3 @@
 func HandleUser() string {
-	return "old"
+	return "new"
 }
`,
	}
	// Edit 3 creates a brand new file
	edit3 := engine.FileEdit{
		Path: "version.go",
		Diff: "package store\n\nconst Version = \"2.0.0\"\n",
	}

	if err := engine.StageFileEdit(tx, edit1); err != nil {
		t.Fatalf("edit1 failed: %v", err)
	}
	if err := engine.StageFileEdit(tx, edit2); err != nil {
		t.Fatalf("edit2 failed: %v", err)
	}
	if err := engine.StageFileEdit(tx, edit3); err != nil {
		t.Fatalf("edit3 failed: %v", err)
	}

	if len(tx.Edits) != 3 {
		t.Fatalf("expected 3 edits, got %d", len(tx.Edits))
	}

	touched := tx.TouchFiles()
	if len(touched) != 3 {
		t.Errorf("expected 3 touched files, got %d", len(touched))
	}

	if err := engine.CommitTransaction(tx); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if tx.Status != engine.TxCommitted {
		t.Errorf("expected TxCommitted, got %v", tx.Status)
	}
}

func TestRollbackTransaction_MultiFile(t *testing.T) {
	tmpDir := t.TempDir()

	file1 := filepath.Join(tmpDir, "core.go")
	orig1 := "package core\n\nfunc Run() string {\n\treturn \"alpha\"\n}\n"
	if err := os.WriteFile(file1, []byte(orig1), 0644); err != nil {
		t.Fatal(err)
	}

	file2 := filepath.Join(tmpDir, "config.go")
	orig2 := "package core\n\nvar Debug = false\n"
	if err := os.WriteFile(file2, []byte(orig2), 0644); err != nil {
		t.Fatal(err)
	}

	tx := engine.NewTransaction(tmpDir)

	edit1 := engine.FileEdit{
		Path: "core.go",
		Diff: `--- a/core.go
+++ b/core.go
@@ -3,3 +3,3 @@
 func Run() string {
-	return "alpha"
+	return "mutated"
 }
`,
	}
	edit2 := engine.FileEdit{
		Path: "config.go",
		Diff: `--- a/config.go
+++ b/config.go
@@ -3,1 +3,1 @@
-var Debug = false
+var Debug = true
`,
	}
	edit3 := engine.FileEdit{
		Path: "scratch.go",
		Diff: "package core\n// ephemeral scratch file\n",
	}

	if err := engine.StageFileEdit(tx, edit1); err != nil {
		t.Fatal(err)
	}
	if err := engine.StageFileEdit(tx, edit2); err != nil {
		t.Fatal(err)
	}
	if err := engine.StageFileEdit(tx, edit3); err != nil {
		t.Fatal(err)
	}

	// Verify all 3 files exist and are modified
	if b, _ := os.ReadFile(file1); !strings.Contains(string(b), "mutated") {
		t.Fatal("file1 was not staged")
	}
	scratchPath := filepath.Join(tmpDir, "scratch.go")
	if _, err := os.Stat(scratchPath); os.IsNotExist(err) {
		t.Fatal("scratch file was not created")
	}

	// Trigger Rollback
	if err := engine.RollbackTransaction(tx); err != nil {
		t.Fatalf("RollbackTransaction failed: %v", err)
	}
	if tx.Status != engine.TxAborted {
		t.Errorf("expected TxAborted, got %v", tx.Status)
	}

	// Verify original content restored on file1 & file2
	b1, err := os.ReadFile(file1)
	if err != nil || string(b1) != orig1 {
		t.Errorf("file1 not restored: %s", string(b1))
	}
	b2, err := os.ReadFile(file2)
	if err != nil || string(b2) != orig2 {
		t.Errorf("file2 not restored: %s", string(b2))
	}

	// Verify scratch file deleted
	if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
		t.Error("scratch file was not removed during rollback")
	}

	// Verify cannot commit aborted transaction
	if err := engine.CommitTransaction(tx); err == nil {
		t.Error("expected error when committing aborted transaction")
	}
}

func TestStageMultipleEdits_AtomicRollbackOnFailure(t *testing.T) {
	tmpDir := t.TempDir()

	file1 := filepath.Join(tmpDir, "a.go")
	orig1 := "package main\n\nfunc A() int { return 1 }\n"
	if err := os.WriteFile(file1, []byte(orig1), 0644); err != nil {
		t.Fatal(err)
	}

	tx := engine.NewTransaction(tmpDir)

	edits := []engine.FileEdit{
		{
			Path: "a.go",
			Diff: `--- a/a.go
+++ b/a.go
@@ -3,1 +3,1 @@
-func A() int { return 1 }
+func A() int { return 100 }
`,
		},
		{
			Path: "nonexistent.go",
			Diff: `--- a/nonexistent.go
+++ b/nonexistent.go
@@ -10,1 +10,1 @@
-bad context line that cannot match
+new line
`,
		},
	}

	err := engine.StageMultipleEdits(tx, edits)
	if err == nil {
		t.Fatal("expected StageMultipleEdits to fail on bad context edit")
	}

	// Verify atomic guarantee: a.go was reverted!
	b1, _ := os.ReadFile(file1)
	if string(b1) != orig1 {
		t.Errorf("atomic rollback failed: file a.go was not reverted, got:\n%s", string(b1))
	}
	if tx.Status != engine.TxAborted {
		t.Errorf("expected TxAborted, got %v", tx.Status)
	}
}

func TestParseUnifiedDiffBlocks_MultiFile(t *testing.T) {
	multiDiff := `diff --git a/src/flask/blueprints.py b/src/flask/blueprints.py
index 5fb84d86..cd54628d 100644
--- a/src/flask/blueprints.py
+++ b/src/flask/blueprints.py
@@ -181,6 +181,9 @@ class Blueprint(Scaffold):
         root_path: t.Optional[str] = None,
         cli_group: t.Optional[str] = _sentinel,
     ):
+        if "." in name:
+            raise ValueError("Blueprint name cannot contain a dot")
+
         super().__init__(
             import_name=import_name,
             static_folder=static_folder,
diff --git a/src/flask/helpers.py b/src/flask/helpers.py
index 585b4dea..9e4e3b80 100644
--- a/src/flask/helpers.py
+++ b/src/flask/helpers.py
@@ -12,7 +12,10 @@ from threading import RLock
 import werkzeug.utils
 from werkzeug.exceptions import NotFound
 from werkzeug.routing import BuildError
-from werkzeug.urls import url_quote
+try:
+    from werkzeug.urls import url_quote
+except ImportError:
+    from urllib.parse import quote as url_quote
 
 from .globals import _app_ctx_stack
 from .globals import _request_ctx_stack
diff --git a/src/flask/testing.py b/src/flask/testing.py
index fe3b846a..1411ea93 100644
--- a/src/flask/testing.py
+++ b/src/flask/testing.py
@@ -113,7 +113,7 @@ class FlaskClient(Client):
         super().__init__(*args, **kwargs)
         self.environ_base = {
             "REMOTE_ADDR": "127.0.0.1",
-            "HTTP_USER_AGENT": f"werkzeug/{werkzeug.__version__}",
+            "HTTP_USER_AGENT": f"werkzeug/{getattr(werkzeug, '__version__', '3.0')}",
         }
 
     @contextmanager
`

	edits, err := engine.ParseUnifiedDiffBlocks(multiDiff)
	if err != nil {
		t.Fatalf("ParseUnifiedDiffBlocks failed: %v", err)
	}

	if len(edits) != 3 {
		t.Fatalf("expected 3 file edits, got %d", len(edits))
	}

	// Verify Edit 1: blueprints.py
	if edits[0].Path != "src/flask/blueprints.py" {
		t.Errorf("edit 0 path mismatch: %s", edits[0].Path)
	}
	if edits[0].ASTNodeScope != "class Blueprint(Scaffold):" {
		t.Errorf("edit 0 scope mismatch: got %q", edits[0].ASTNodeScope)
	}
	if !strings.Contains(edits[0].Diff, "Blueprint name cannot contain a dot") {
		t.Errorf("edit 0 diff missing added line")
	}

	// Verify Edit 2: helpers.py
	if edits[1].Path != "src/flask/helpers.py" {
		t.Errorf("edit 1 path mismatch: %s", edits[1].Path)
	}
	if edits[1].ASTNodeScope != "from threading import RLock" {
		t.Errorf("edit 1 scope mismatch: got %q", edits[1].ASTNodeScope)
	}

	// Verify Edit 3: testing.py
	if edits[2].Path != "src/flask/testing.py" {
		t.Errorf("edit 2 path mismatch: %s", edits[2].Path)
	}
	if edits[2].ASTNodeScope != "class FlaskClient(Client):" {
		t.Errorf("edit 2 scope mismatch: got %q", edits[2].ASTNodeScope)
	}
}

func TestParseUnifiedDiffBlocks_MarkdownFences(t *testing.T) {
	markdownText := "Here is the multi-file fix proposed:\n\n```diff\ndiff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1,3 +1,3 @@ func Main()\n-old\n+new\n```\n\nAnd for config:\n```diff\ndiff --git a/config.go b/config.go\n--- a/config.go\n+++ b/config.go\n@@ -5,2 +5,3 @@\n+debug = true\n```\n"

	edits, err := engine.ParseUnifiedDiffBlocks(markdownText)
	if err != nil {
		t.Fatalf("ParseUnifiedDiffBlocks with markdown fences failed: %v", err)
	}

	if len(edits) != 2 {
		t.Fatalf("expected 2 file edits from fences, got %d", len(edits))
	}
	if edits[0].Path != "app.go" {
		t.Errorf("edit 0 path: %s", edits[0].Path)
	}
	if edits[0].ASTNodeScope != "func Main()" {
		t.Errorf("edit 0 scope: %s", edits[0].ASTNodeScope)
	}
	if edits[1].Path != "config.go" {
		t.Errorf("edit 1 path: %s", edits[1].Path)
	}
}

func TestApplyMultiFileUnifiedDiff_EndToEnd(t *testing.T) {
	tmpDir := t.TempDir()

	p1 := filepath.Join(tmpDir, "pkg", "service.go")
	p2 := filepath.Join(tmpDir, "pkg", "router.go")
	_ = os.MkdirAll(filepath.Dir(p1), 0755)

	orig1 := "package pkg\n\nfunc Serve() string {\n\treturn \"alpha\"\n}\n"
	orig2 := "package pkg\n\nfunc Route() string {\n\treturn \"get\"\n}\n"
	_ = os.WriteFile(p1, []byte(orig1), 0644)
	_ = os.WriteFile(p2, []byte(orig2), 0644)

	patchText := `diff --git a/pkg/service.go b/pkg/service.go
--- a/pkg/service.go
+++ b/pkg/service.go
@@ -3,3 +3,3 @@
 func Serve() string {
-	return "alpha"
+	return "beta"
 }
diff --git a/pkg/router.go b/pkg/router.go
--- a/pkg/router.go
+++ b/pkg/router.go
@@ -3,3 +3,3 @@
 func Route() string {
-	return "get"
+	return "post"
 }
`

	tx, err := engine.ApplyMultiFileUnifiedDiff(tmpDir, patchText)
	if err != nil {
		t.Fatalf("ApplyMultiFileUnifiedDiff failed: %v", err)
	}

	if tx.Status != engine.TxStaged {
		t.Errorf("expected TxStaged, got %v", tx.Status)
	}
	if len(tx.Edits) != 2 {
		t.Fatalf("expected 2 edits, got %d", len(tx.Edits))
	}

	// Verify files modified on disk
	b1, _ := os.ReadFile(p1)
	if !strings.Contains(string(b1), "return \"beta\"") {
		t.Errorf("p1 not updated: %s", string(b1))
	}
	b2, _ := os.ReadFile(p2)
	if !strings.Contains(string(b2), "return \"post\"") {
		t.Errorf("p2 not updated: %s", string(b2))
	}

	// Commit transaction
	if err := engine.CommitTransaction(tx); err != nil {
		t.Fatalf("CommitTransaction failed: %v", err)
	}
	if tx.Status != engine.TxCommitted {
		t.Errorf("expected TxCommitted, got %v", tx.Status)
	}
}

func TestConvertFileEditsToEditBlocks(t *testing.T) {
	edits := []engine.FileEdit{
		{
			Path: "test.go",
			Diff: `--- a/test.go
+++ b/test.go
@@ -1,3 +1,3 @@
-foo
+bar
`,
		},
	}

	blocks := engine.ConvertFileEditsToEditBlocks(edits)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 EditBlock, got %d", len(blocks))
	}
	if blocks[0].FilePath != "test.go" {
		t.Errorf("expected test.go, got %s", blocks[0].FilePath)
	}
	if blocks[0].SearchText != "foo" || blocks[0].ReplaceText != "bar" {
		t.Errorf("block content mismatch: search=%q replace=%q", blocks[0].SearchText, blocks[0].ReplaceText)
	}
}
