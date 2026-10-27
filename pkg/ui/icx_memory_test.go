// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestICX_MemorySurfacesIngestion verifies that file writes through session.WriteFile
// synchronize state across all 5 volumetric memory surfaces (0D-4D).
func TestICX_MemorySurfacesIngestion(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-icx-surfaces-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	lat := icx.NewLocalLattice()
	session := NewSessionWithICX(nil, nil, tmpDir, lat)

	// Write two files under a project subfolder
	file1 := "projects/sim-app/main.go"
	content1 := []byte("package main\n\nfunc main() {}\n")
	written1, sha1, err1 := session.WriteFile(file1, content1)
	if err1 != nil {
		t.Fatalf("failed to write file1: %v", err1)
	}
	if sha1 == "" || written1 != file1 {
		t.Errorf("unexpected write result: %s, %s", written1, sha1)
	}

	file2 := "projects/sim-app/internal/handler.go"
	content2 := []byte("package internal\n\ntype Handler struct{}\n")
	written2, sha2, err2 := session.WriteFile(file2, content2)
	if err2 != nil {
		t.Fatalf("failed to write file2: %v", err2)
	}
	if sha2 == "" || written2 != file2 {
		t.Errorf("unexpected write result: %s, %s", written2, sha2)
	}

	// Verify ActiveProject anchored
	expectedProj := "projects/sim-app"
	if session.ActiveProject() != expectedProj {
		t.Errorf("expected active project %q, got %q", expectedProj, session.ActiveProject())
	}

	// Verify WrittenFiles recorded
	written := session.WrittenFiles()
	if len(written) != 2 {
		t.Fatalf("expected 2 written files, got %d", len(written))
	}
	if written[0] != file1 || written[1] != file2 {
		t.Errorf("unexpected written files slice: %v", written)
	}

	ctx := context.Background()

	// 0D Surface verification
	res0D, err := lat.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "0D_SCALAR"})
	if err != nil || res0D == nil || len(res0D.Nodes) < 2 {
		t.Errorf("0D scalar surface missing file seals: %v, %v", res0D, err)
	}

	// 1D Surface verification
	res1D, err := lat.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "1D_SEQUENCE"})
	if err != nil || res1D == nil || len(res1D.Nodes) < 2 {
		t.Errorf("1D sequence surface missing entries: %v, %v", res1D, err)
	}

	// 2D Surface verification
	res2D, err := lat.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "2D_TABULAR"})
	if err != nil || res2D == nil || len(res2D.Nodes) < 2 {
		t.Errorf("2D tabular surface missing entries: %v, %v", res2D, err)
	}

	// 3D Surface verification
	res3D, err := lat.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "3D_VOLUMETRIC"})
	if err != nil || res3D == nil || len(res3D.Nodes) < 2 {
		t.Errorf("3D volumetric surface missing entries: %v, %v", res3D, err)
	}

	// 4D Surface verification (swarm state variables)
	resSeal, err := lat.VarGet(ctx, icx.VarGetRequest{Key: "swarm.last_seal"})
	if err != nil || !resSeal.OK || resSeal.Value != sha2 {
		t.Errorf("4D swarm.last_seal mismatch: %v, %v", resSeal, err)
	}

	resProj, err := lat.VarGet(ctx, icx.VarGetRequest{Key: "swarm.active_project"})
	if err != nil || !resProj.OK || resProj.Value != expectedProj {
		t.Errorf("4D swarm.active_project mismatch: %v, %v", resProj, err)
	}

	// Verify epistemic grounding block generates grounded context
	block := session.buildEpistemicGroundingBlock()
	if !strings.Contains(block, "projects/sim-app") {
		t.Errorf("epistemic grounding block missing active project: %s", block)
	}
	if !strings.Contains(block, "0D-4D Synchronized") {
		t.Errorf("epistemic grounding block missing synchronized badge: %s", block)
	}
}

// TestProjectHarmonizer_VariantRemapping verifies that multiple variant project directories
// (e.g. projects/sim-app-clean, projects/sim-app-browser, projects/sim-app-modern)
// emitted in a single turn are coalesced into the single canonical root projects/sim-app.
func TestProjectHarmonizer_VariantRemapping(t *testing.T) {
	session := NewSession(nil, nil, ".")

	rawActions := []*ActionIntent{
		{
			Type: ActionWrite,
			Attributes: map[string]string{
				"path": "projects/sim-app-clean/main.go",
			},
		},
		{
			Type: ActionWrite,
			Attributes: map[string]string{
				"path": "projects/sim-app-browser/static/index.html",
			},
		},
		{
			Type: ActionWrite,
			Attributes: map[string]string{
				"path": "projects/sim-app-modern/style.css",
			},
		},
	}

	canonicalDir, remapped := HarmonizeProjectActions(session, rawActions)
	expectedRoot := "projects/sim-app"
	if canonicalDir != expectedRoot {
		t.Fatalf("expected canonicalDir %q, got %q", expectedRoot, canonicalDir)
	}

	if session.ActiveProject() != expectedRoot {
		t.Errorf("expected session active project %q, got %q", expectedRoot, session.ActiveProject())
	}

	expectedRemappings := map[string]string{
		"projects/sim-app-clean/main.go":            "projects/sim-app/main.go",
		"projects/sim-app-browser/static/index.html": "projects/sim-app/static/index.html",
		"projects/sim-app-modern/style.css":          "projects/sim-app/style.css",
	}

	for _, wa := range rawActions {
		orig := wa.Path()
		expectedTarget := expectedRemappings[orig]
		actualTarget := remapped[wa]
		if actualTarget != expectedTarget {
			t.Errorf("harmonization failed for %s: expected %q, got %q", orig, expectedTarget, actualTarget)
		}
	}
}

// TestProjectHarmonizer_DifferentProjectIsolation verifies that write actions targeting
// a completely different project (e.g. projects/service-b) while the session is active on projects/service-a
// are strictly isolated and NEVER nested under projects/service-a/projects/service-b.
func TestProjectHarmonizer_DifferentProjectIsolation(t *testing.T) {
	// 1. Verify ScopePath does not nest different projects
	scoped := ScopePath("projects/service-a", "projects/service-b/pkg/canonical/canonical.go")
	expected := "projects/service-b/pkg/canonical/canonical.go"
	if scoped != expected {
		t.Fatalf("ScopePath nested different projects! Expected %q, got %q", expected, scoped)
	}

	// 2. Verify HarmonizeProjectActions preserves distinct project paths without mangling
	session := NewSession(nil, nil, ".")
	session.SetActiveProject("projects/service-a")

	foreignActions := []*ActionIntent{
		{
			Type: ActionWrite,
			Attributes: map[string]string{
				"path": "projects/service-b/pkg/canonical/canonical.go",
			},
		},
		{
			Type: ActionWrite,
			Attributes: map[string]string{
				"path": "projects/service-b/go.mod",
			},
		},
	}

	canonicalDir, remapped := HarmonizeProjectActions(session, foreignActions)
	if canonicalDir == "projects/service-a" {
		// When all actions are for service-b, canonicalDir must not remain locked to service-a
		// or if canonicalDir remains service-a, remapped actions must NOT be nested inside it!
	}

	for _, wa := range foreignActions {
		target := remapped[wa]
		if strings.Contains(target, "projects/service-a/projects/service-b") {
			t.Errorf("HarmonizeProjectActions corruptly nested projects! Got %q", target)
		}
		if !strings.HasPrefix(target, "projects/service-b/") {
			t.Errorf("expected target to remain under projects/service-b/, got %q", target)
		}
	}
}

// TestFatPayloadGuard_Compaction verifies that assistant messages exceeding 2,048 bytes
// have large code blocks (>500 bytes) compacted with a SHA-256 seal when sending messages to the LLM.
func TestFatPayloadGuard_Compaction(t *testing.T) {
	longCode := strings.Repeat("console.log(\"processing request batch\");\n", 40) // ~1600 bytes
	rawMessage := "Here is the implementation:\n```javascript\n" + longCode + "\n```\nAnd another block:\n```html\n" + strings.Repeat("<div>Status entry</div>\n", 30) + "\n```\nDone."

	if len(rawMessage) <= 2048 {
		t.Fatalf("test setup error: rawMessage must be > 2048 bytes, got %d", len(rawMessage))
	}

	compacted := compactHistoricalAssistantMessage(rawMessage)
	if len(compacted) >= len(rawMessage) {
		t.Errorf("expected compacted message to be shorter: %d vs %d", len(compacted), len(rawMessage))
	}
	if !strings.Contains(compacted, "[Code preserved:") {
		t.Errorf("compacted message missing '[Code preserved:' placeholder: %s", compacted)
	}
	if !strings.Contains(compacted, "SHA-256:") {
		t.Errorf("compacted message missing SHA-256 seal: %s", compacted)
	}

	// Short message should remain unchanged
	shortMessage := "Here is a tiny snippet:\n```go\nfunc add(a, b int) int { return a + b }\n```\n"
	unchanged := compactHistoricalAssistantMessage(shortMessage)
	if unchanged != shortMessage {
		t.Errorf("short message should not be compacted: %q vs %q", unchanged, shortMessage)
	}
}

// TestHistoricalUserDiagnosticCompaction verifies that bulky compiler and test failure
// logs from past turns are compacted while preserving requirements specifications and short messages.
func TestHistoricalUserDiagnosticCompaction(t *testing.T) {
	// 1. Long diagnostic message (> 1024 bytes, > 30 lines)
	diagHeader := "Verification test execution failed with exit code 1:\n=== COMPILER OUTPUT ===\n"
	diagLines := strings.Repeat("main.go:42:10: undefined: SomeMissingSymbol\n", 40) // ~1700 bytes
	rawDiag := diagHeader + diagLines

	if len(rawDiag) <= 1024 {
		t.Fatalf("test setup error: rawDiag must be > 1024 bytes, got %d", len(rawDiag))
	}

	compacted := compactHistoricalUserDiagnosticMessage(rawDiag)
	if len(compacted) >= len(rawDiag) {
		t.Errorf("expected compacted diagnostic message to be shorter: %d vs %d", len(compacted), len(rawDiag))
	}
	if !strings.Contains(compacted, "historical diagnostic output truncated") {
		t.Errorf("compacted message missing truncation notice: %s", compacted)
	}
	if !strings.Contains(compacted, "SHA-256:") {
		t.Errorf("compacted message missing SHA-256 seal: %s", compacted)
	}
	// Verify first few lines are preserved
	if !strings.Contains(compacted, "=== COMPILER OUTPUT ===") {
		t.Errorf("compacted message should preserve diagnostic header")
	}

	// 2. Long user architectural requirements (NOT a compiler diagnostic) should NOT be truncated
	reqsHeader := "Here are the product specifications for the new web application:\n"
	reqsBody := strings.Repeat("Feature requirement: User must be able to click and see interactive graphs.\n", 30)
	rawReqs := reqsHeader + reqsBody

	if len(rawReqs) <= 1024 {
		t.Fatalf("test setup error: rawReqs must be > 1024 bytes, got %d", len(rawReqs))
	}

	reqsCompacted := compactHistoricalUserDiagnosticMessage(rawReqs)
	if reqsCompacted != rawReqs {
		t.Errorf("user requirements specification should NOT be compacted: expected %d bytes, got %d bytes", len(rawReqs), len(reqsCompacted))
	}

	// 3. Short diagnostic message (<= 1024 bytes) should remain unchanged
	shortDiag := "Verification test execution failed:\n=== COMPILER OUTPUT ===\nmain.go:1:1: syntax error\n"
	shortCompacted := compactHistoricalUserDiagnosticMessage(shortDiag)
	if shortCompacted != shortDiag {
		t.Errorf("short diagnostic message should not be compacted: %q vs %q", shortCompacted, shortDiag)
	}
}

// TestSessionReset_PreservesLattice verifies that Session.Reset() clears conversation history
// while retaining 100% of the 4D lattice memory.
func TestSessionReset_PreservesLattice(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-reset-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	lat := icx.NewLocalLattice()
	session := NewSessionWithICX(nil, nil, tmpDir, lat)

	_, _, err = session.WriteFile("projects/sim-app/main.go", []byte("package main\n"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Add messages to history
	session.mu.Lock()
	session.history = append(session.history, llm.ChatMessage{Role: "user", Content: "build sim-app"})
	session.history = append(session.history, llm.ChatMessage{Role: "assistant", Content: "Done."})
	session.mu.Unlock()

	if len(session.History()) != 2 {
		t.Fatalf("expected 2 history items, got %d", len(session.History()))
	}

	// Perform reset
	session.Reset()

	// History should be empty
	if len(session.History()) != 0 {
		t.Errorf("expected history to be empty after Reset(), got %d", len(session.History()))
	}

	// Lattice memory should still contain our nodes
	ctx := context.Background()
	res, err := lat.ReadDimension(ctx, icx.ReadDimensionRequest{Dimension: "0D_SCALAR"})
	if err != nil || len(res.Nodes) == 0 {
		t.Errorf("lattice nodes should be preserved after Reset(): %v, %v", res, err)
	}
}

// TestMemorySlashCommand verifies that printMemorySurfaces outputs all 5 surface headers.
func TestMemorySlashCommand(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder-memory-cmd-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	lat := icx.NewLocalLattice()
	session := NewSessionWithICX(nil, nil, tmpDir, lat)

	_, _, _ = session.WriteFile("projects/sim-app/main.go", []byte("package main\n"))

	var buf bytes.Buffer
	printMemorySurfaces(&buf, session)

	out := buf.String()
	surfaces := []string{
		"ICX 4D Volumetric Memory Surfaces:",
		"[0D SCALAR SURFACE]",
		"[1D SEQUENCE SURFACE]",
		"[2D TABULAR SURFACE]",
		"[3D VOLUMETRIC SURFACE]",
		"[4D SPATIOTEMPORAL SURFACE]",
	}

	for _, surface := range surfaces {
		if !strings.Contains(out, surface) {
			t.Errorf("printMemorySurfaces output missing surface header %q\nFull output:\n%s", surface, out)
		}
	}
}
