// SPDX-License-Identifier: Apache-2.0
package icx

import (
	"context"
	"strings"
	"testing"
)

func TestRecallScoped_RibbonFormat(t *testing.T) {
	lat := NewLocalMemoryCache("test_space")
	ctx := context.Background()

	// Ingest some facts
	_, err := lat.Remember(ctx, RememberRequest{
		Text:     "func Add(a, b int) int { return a + b }",
		SourceID: "calc.go",
		Family:   "core",
		SpaceID:  "test_space",
	})
	if err != nil {
		t.Fatalf("Remember failed: %v", err)
	}

	_, err = lat.Remember(ctx, RememberRequest{
		Text:     "func Sub(a, b int) int { return a - b }",
		SourceID: "calc.go",
		Family:   "core",
		SpaceID:  "test_space",
	})
	if err != nil {
		t.Fatalf("Remember failed: %v", err)
	}

	// 1. Standard format (no ribbon)
	resNormal, err := lat.RecallScoped(ctx, RecallRequest{
		Query:   "func",
		SpaceID: "test_space",
	})
	if err != nil {
		t.Fatalf("RecallScoped normal failed: %v", err)
	}
	if resNormal.Ribbon != "" {
		t.Errorf("expected empty Ribbon in normal format, got %q", resNormal.Ribbon)
	}
	if len(resNormal.Matches) != 2 {
		t.Errorf("expected 2 matches, got %d", len(resNormal.Matches))
	}

	// 2. Ribbon format (FormatRibbon)
	resRibbon, err := lat.RecallScoped(ctx, RecallRequest{
		Query:   "func",
		SpaceID: "test_space",
		Format:  FormatRibbon,
	})
	if err != nil {
		t.Fatalf("RecallScoped ribbon failed: %v", err)
	}
	if resRibbon.Ribbon == "" {
		t.Fatal("expected non-empty Ribbon, got empty string")
	}
	if !strings.HasPrefix(resRibbon.Ribbon, "<nav:ribbon: query=\"func\" count=2") {
		t.Errorf("unexpected ribbon prefix: %s", resRibbon.Ribbon)
	}
	if !strings.HasSuffix(resRibbon.Ribbon, "/>") {
		t.Errorf("ribbon must end with '/>', got: %s", resRibbon.Ribbon)
	}
	if !strings.Contains(resRibbon.Ribbon, "[") || !strings.Contains(resRibbon.Ribbon, ":core]") {
		t.Errorf("ribbon must contain [id:family] entries, got: %s", resRibbon.Ribbon)
	}

	// 3. Ribbon format with 0 matches
	resEmpty, err := lat.RecallScoped(ctx, RecallRequest{
		Query:   "nonexistent_token_xyz",
		SpaceID: "test_space",
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped empty failed: %v", err)
	}
	expectedEmpty := "<nav:ribbon: query=\"nonexistent_token_xyz\" count=0 | empty />"
	if resEmpty.Ribbon != expectedEmpty {
		t.Errorf("expected %q, got %q", expectedEmpty, resEmpty.Ribbon)
	}
}

func TestMultihopWalk_RibbonFormat(t *testing.T) {
	lat := NewLocalMemoryCache("ast_space")
	ctx := context.Background()

	// Ingest sample AST nodes & edges
	lat.AddASTNode(&LatticeNode{
		ID:        "file_calc.go",
		Name:      "calc.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTNode(&LatticeNode{
		ID:        "func_Add",
		Name:      "Add",
		Type:      "function",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTNode(&LatticeNode{
		ID:        "func_Sub",
		Name:      "Sub",
		Type:      "function",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTEdge("file_calc.go", "defines", "func_Add")
	lat.AddASTEdge("file_calc.go", "defines", "func_Sub")

	// Walk with format: ribbon
	walkRes, err := lat.MultihopWalk(ctx, MultihopWalkRequest{
		StartNode: "file_calc.go",
		MaxHops:   5,
		SpaceID:   "ast_space",
		Format:    FormatRibbon,
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}
	if !walkRes.OK {
		t.Fatalf("walk failed: %s", walkRes.Error)
	}
	if walkRes.Ribbon == "" {
		t.Fatal("expected non-empty Ribbon in MultihopWalkResponse")
	}
	if !strings.HasPrefix(walkRes.Ribbon, "<nav:ribbon: root=\"calc.go\" hops=5 | ") {
		t.Errorf("unexpected ribbon prefix: %s", walkRes.Ribbon)
	}
	if !strings.Contains(walkRes.Ribbon, "d0:calc.go(file):root") {
		t.Errorf("expected d0:calc.go in ribbon, got: %s", walkRes.Ribbon)
	}
	if !strings.Contains(walkRes.Ribbon, "d1:Add(function):defines") {
		t.Errorf("expected d1:Add in ribbon, got: %s", walkRes.Ribbon)
	}
}

func TestCallTool_RecallScoped_Ribbon(t *testing.T) {
	lat := NewLocalMemoryCache("tool_space")
	ctx := context.Background()

	_, err := lat.Remember(ctx, RememberRequest{
		Text:     "security key authentication validator",
		SourceID: "auth.go",
		Family:   "security",
		SpaceID:  "tool_space",
	})
	if err != nil {
		t.Fatalf("Remember failed: %v", err)
	}

	toolRes, err := lat.CallTool(ctx, "icx_recall_scoped", map[string]any{
		"query":    "security",
		"space_id": "tool_space",
		"format":   "ribbon",
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if toolRes.IsError {
		t.Fatalf("CallTool returned error: %s", toolRes.Content[0].Text)
	}
	if len(toolRes.Content) == 0 {
		t.Fatal("expected tool result content")
	}
	text := toolRes.Content[0].Text
	if !strings.Contains(text, "<nav:ribbon:") {
		t.Errorf("expected ribbon in tool output, got: %s", text)
	}
}
