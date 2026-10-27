// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

func TestAuditProjectDelivery_PolyglotASTAndGrounding(t *testing.T) {
	tmpDir := t.TempDir()
	lat := icx.NewLocalLattice()
	session := NewSessionWithICX(nil, nil, tmpDir, lat)

	// Write Python backend file
	pyFile := "projects/test-app/main.py"
	pyCode := []byte(`import sqlite3
import json

class Item:
    pass

def get_items():
    return json.dumps([])
`)
	_, _, err := session.WriteFile(pyFile, pyCode)
	if err != nil {
		t.Fatalf("failed to write pyFile: %v", err)
	}

	// Write HTML frontend file
	htmlFile := "projects/test-app/static/index.html"
	htmlCode := []byte(`<!DOCTYPE html>
<html>
<head><title>Test</title></head>
<body>
    <button id="btn1" onclick="loadData()">Load</button>
    <script>
        function loadData() {
            fetch('/api/items');
        }
    </script>
</body>
</html>`)
	_, _, err = session.WriteFile(htmlFile, htmlCode)
	if err != nil {
		t.Fatalf("failed to write htmlFile: %v", err)
	}

	// Verify Epistemic Grounding includes 2D/3D symbol & AST manifest
	block := session.buildEpistemicGroundingBlock()
	if !strings.Contains(block, "[ICX 2D/3D SYMBOL & AST MANIFEST]") {
		t.Fatalf("expected 2D/3D symbol manifest in grounding block, got:\n%s", block)
	}
	if !strings.Contains(block, "main.py") || !strings.Contains(block, "get_items") {
		t.Errorf("grounding block missing main.py symbols, got:\n%s", block)
	}

	// Test AuditProjectDelivery on clean project
	var out bytes.Buffer
	projDir := filepath.Join(tmpDir, "projects", "test-app")
	passed, issues := AuditProjectDelivery(context.Background(), tmpDir, projDir, session, &out)
	if !passed || issues != "" {
		t.Fatalf("expected audit to pass on clean project, got issues: %s", issues)
	}

	// Introduce a broken placeholder URL and broken onclick
	brokenHTML := []byte(`<!DOCTYPE html>
<html>
<body>
    <a href="https://example.com/placeholder">Link</a>
    <button onclick="nonExistentFunc()">Click</button>
</body>
</html>`)
	_ = os.WriteFile(filepath.Join(projDir, "static", "index.html"), brokenHTML, 0644)

	passed, issues = AuditProjectDelivery(context.Background(), tmpDir, projDir, session, &out)
	if passed {
		t.Fatalf("expected audit to fail on broken HTML, but passed")
	}
	if !strings.Contains(issues, "example.com/placeholder") {
		t.Errorf("expected issue for placeholder URL example.com/placeholder, got: %s", issues)
	}
	if !strings.Contains(issues, "nonExistentFunc") {
		t.Errorf("expected issue for nonExistentFunc, got: %s", issues)
	}
}
