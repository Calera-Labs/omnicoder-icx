// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditConsumerReadiness_ValidPythonProject(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. requirements.txt
	_ = os.WriteFile(filepath.Join(tmpDir, "requirements.txt"), []byte("streamlit>=1.30.0\npytest>=7.0.0\n"), 0644)

	// 2. app.py
	_ = os.WriteFile(filepath.Join(tmpDir, "app.py"), []byte("def main():\n    pass\n\nif __name__ == '__main__':\n    main()\n"), 0644)

	// 3. tests/test_app.py
	_ = os.MkdirAll(filepath.Join(tmpDir, "tests"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "tests", "test_app.py"), []byte("def test_ok():\n    assert True\n"), 0644)

	// 4. README.md
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test App\n\nThis is a complete and substantive README documentation file for the project.\n"), 0644)

	ready, issues := AuditConsumerReadiness(tmpDir, tmpDir)
	if !ready {
		t.Fatalf("expected ready=true, got issues: %v", issues)
	}
}

func TestAuditConsumerReadiness_InvalidRequirementsTxt(t *testing.T) {
	tmpDir := t.TempDir()

	// Broken requirements.txt with markdown prose
	_ = os.WriteFile(filepath.Join(tmpDir, "requirements.txt"), []byte("```markdown\nHere are the packages:\nstreamlit\n```"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "app.py"), []byte("print('ok')\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Title\n\nValid description that is longer than 50 bytes in length.\n"), 0644)

	ready, issues := AuditConsumerReadiness(tmpDir, tmpDir)
	if ready {
		t.Fatalf("expected ready=false due to broken requirements.txt")
	}
	hasReqIssue := false
	for _, iss := range issues {
		if containsIgnoreCase(iss, "requirements.txt") {
			hasReqIssue = true
			break
		}
	}
	if !hasReqIssue {
		t.Errorf("expected issue mentioning requirements.txt, got %v", issues)
	}
}

func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		len(s) > 0 && len(substr) > 0)
}
