// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectFabricatedData(t *testing.T) {
	bad := `default_partitions = [("Primary Catalog API", 500, "synced_remote")]`
	if ok, _ := detectFabricatedData("projects/app/utils/sync.py", bad); !ok {
		t.Error("expected hardcoded ledger counts to be flagged")
	}
	bad2 := "# Add supplementary expanded corpus records so total matches expected ledger partition size"
	if ok, _ := detectFabricatedData("projects/app/utils/sync.py", bad2); !ok {
		t.Error("expected padding-to-match comment to be flagged")
	}
	if ok, _ := detectFabricatedData("projects/app/tests/test_app.py", bad); ok {
		t.Error("test files must be exempt")
	}
	if ok, _ := detectFabricatedData("projects/app/utils/seed.py", bad2); ok {
		t.Error("seed files must be exempt")
	}
	good := "cursor.execute(\"ALTER TABLE records ADD COLUMN category_tag TEXT\")"
	if ok, w := detectFabricatedData("projects/app/utils/db.py", good); ok {
		t.Errorf("schema migration wrongly flagged: %q", w)
	}
	negated := `"""Bootstraps the sync ledger from existing genuine SQLite records without hardcoded fake counts."""`
	if ok, w := detectFabricatedData("projects/app/utils/sync.py", negated); ok {
		t.Errorf("negated phrase wrongly flagged as fabrication: %q", w)
	}
	negated2 := `# Remove fake item loops and example.org URLs`
	if ok, w := detectFabricatedData("projects/app/utils/seed.py", negated2); ok {
		t.Errorf("negated removal comment wrongly flagged: %q", w)
	}
}

func TestReadFileForArchitect_WorkspaceContainment(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "projects", "app"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "projects", "app", "a.py"), []byte("x=1\ny=2\nz=3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mk := func(p, lines string) *ActionIntent {
		return &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": p, "lines": lines}, Payload: p}
	}
	out := readFileForArchitect(ws, "projects/app", mk("a.py", "2-3"))
	if !strings.Contains(out, "2: y=2") || strings.Contains(out, "1: x=1") {
		t.Errorf("unexpected range output: %s", out)
	}
	if out := readFileForArchitect(ws, "", mk("../../etc/passwd", "")); !strings.Contains(out, "[error]") {
		t.Errorf("path escape must be refused: %s", out)
	}
}

func TestParseReadAction(t *testing.T) {
	in := ParseAllActionIntents(`[ACTION:READ path="a/b.py" lines="1-50"]`)
	if len(in) != 1 || in[0].Type != ActionRead || in[0].Path() != "a/b.py" {
		t.Fatalf("bad parse: %+v", in)
	}
}
