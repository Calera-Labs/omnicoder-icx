// SPDX-License-Identifier: Apache-2.0
package devops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToggleChecklist(t *testing.T) {
	tmpDir := t.TempDir()
	docPath := filepath.Join(tmpDir, "spec.md")
	content := `# Project Specification

### Release Checklist
- [ ] Observability middleware and metrics
- [ ] Security headers and rate limiting
- [ ] Dockerfile multi-stage build

### Appendix
End of specification.
`
	if err := os.WriteFile(docPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := ToggleChecklist(tmpDir, "spec.md", []ChecklistToggle{
		{Pattern: "Observability", Checked: true, Note: "verified"},
		{Pattern: "Security headers", Checked: true},
	})
	if err != nil {
		t.Fatalf("ToggleChecklist failed: %v", err)
	}

	if res.MatchedCount != 2 {
		t.Errorf("expected 2 matches, got %d", res.MatchedCount)
	}

	updated, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(updated)
	if !strings.Contains(s, "- [x] Observability middleware and metrics (verified)") {
		t.Errorf("expected Observability to be checked with note, got:\n%s", s)
	}
	if !strings.Contains(s, "- [x] Security headers and rate limiting") {
		t.Errorf("expected Security headers to be checked, got:\n%s", s)
	}
	if !strings.Contains(s, "- [ ] Dockerfile multi-stage build") {
		t.Errorf("expected Dockerfile to remain unchecked, got:\n%s", s)
	}
}

func TestUpdateSection(t *testing.T) {
	tmpDir := t.TempDir()
	docPath := filepath.Join(tmpDir, "paper.md")
	content := `# Project Paper

## Section 1
Original section 1 content.

## Section 2
Old section 2 content line 1.
Old section 2 content line 2.

## Section 3
Original section 3 content.
`
	if err := os.WriteFile(docPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := UpdateSection(tmpDir, "paper.md", "## Section 2", "New section 2 replacement content.")
	if err != nil {
		t.Fatalf("UpdateSection failed: %v", err)
	}
	if res.Status != "UPDATED" {
		t.Errorf("expected status UPDATED, got %s", res.Status)
	}

	updated, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(updated)

	if !strings.Contains(s, "New section 2 replacement content.") {
		t.Errorf("expected new content, got:\n%s", s)
	}
	if strings.Contains(s, "Old section 2 content") {
		t.Errorf("expected old content to be removed, got:\n%s", s)
	}
	if !strings.Contains(s, "## Section 3\nOriginal section 3 content.") {
		t.Errorf("expected Section 3 to be preserved, got:\n%s", s)
	}
}
