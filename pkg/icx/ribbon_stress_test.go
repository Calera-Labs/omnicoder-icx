// SPDX-License-Identifier: Apache-2.0
// Package icx_test implements empirical adversarial stress testing for
// 1D Geodesic Ribbon Mode (<nav:ribbon>).
//
// Test Dimensions:
// 1. RecallScoped Ribbon Cardinality: 0 matches, 1 match, 100 matches, top_k clamping.
// 2. Special Characters, Unicode, Quotes, Newlines, XML/Tags in Query and Facts.
// 3. MultihopWalk Ribbon Formatting: cycles, self-loops, diamonds, isolated nodes, non-existent nodes.
// 4. CallTool Dispatch: icx_recall_scoped & icx_multihop_walk with format: "ribbon", malformed inputs.
// 5. Concurrency & High Contention: multi-threaded read/write/walk/calltool under go test -race.

package icx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// ============================================================================
// 1. RecallScoped Ribbon Cardinality Stress (0, 1, 100 matches, TopK clamping)
// ============================================================================

func TestRecallScoped_Ribbon_CardinalityStress(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1.1 Zero matches
	resEmpty, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   "non_existent_unique_token_9999",
		SpaceID: "cardinality_space",
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped failed on empty: %v", err)
	}
	if resEmpty.Count != 0 || len(resEmpty.Matches) != 0 {
		t.Errorf("expected 0 matches, got count=%d, len=%d", resEmpty.Count, len(resEmpty.Matches))
	}
	expectedEmpty := `<nav:ribbon: query="non_existent_unique_token_9999" count=0 | empty />`
	if resEmpty.Ribbon != expectedEmpty {
		t.Errorf("expected ribbon %q, got %q", expectedEmpty, resEmpty.Ribbon)
	}

	// 1.2 Zero matches with empty query
	resBlankQuery, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   "",
		SpaceID: "cardinality_space",
		Format:  icx.FormatRibbon,
	})
	if err != nil {
		t.Fatalf("RecallScoped failed on blank query: %v", err)
	}
	expectedBlank := `<nav:ribbon: query="" count=0 | empty />`
	if resBlankQuery.Ribbon != expectedBlank {
		t.Errorf("expected ribbon %q, got %q", expectedBlank, resBlankQuery.Ribbon)
	}

	// 1.3 Space Isolation: Fact exists in space_A, queried in space_B
	_, err = lat.Remember(ctx, icx.RememberRequest{
		Text:     "isolated space alpha fact token_iso_123",
		SourceID: "space_a.go",
		Family:   "core",
		SpaceID:  "space_A",
	})
	if err != nil {
		t.Fatalf("Remember failed: %v", err)
	}
	resSpaceB, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   "token_iso_123",
		SpaceID: "space_B",
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped space_B failed: %v", err)
	}
	if resSpaceB.Count != 0 {
		t.Errorf("expected 0 matches across isolated spaces, got count=%d", resSpaceB.Count)
	}
	if !strings.Contains(resSpaceB.Ribbon, "count=0 | empty />") {
		t.Errorf("expected empty ribbon for space isolation, got %q", resSpaceB.Ribbon)
	}

	// 1.4 Single match
	_, err = lat.Remember(ctx, icx.RememberRequest{
		Text:     "func CalculateTotalRevenue(orders []Order) decimal.Decimal",
		SourceID: "revenue.go",
		Family:   "finance",
		SpaceID:  "cardinality_space",
	})
	if err != nil {
		t.Fatalf("Remember single failed: %v", err)
	}

	resSingle, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   "CalculateTotalRevenue",
		SpaceID: "cardinality_space",
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped single failed: %v", err)
	}
	if resSingle.Count != 1 || len(resSingle.Matches) != 1 {
		t.Fatalf("expected exactly 1 match, got count=%d, matches=%d", resSingle.Count, len(resSingle.Matches))
	}
	if !strings.HasPrefix(resSingle.Ribbon, `<nav:ribbon: query="CalculateTotalRevenue" count=1 | `) {
		t.Errorf("unexpected single match ribbon prefix: %s", resSingle.Ribbon)
	}
	if !strings.HasSuffix(resSingle.Ribbon, `/>`) {
		t.Errorf("ribbon must close with '/>': %s", resSingle.Ribbon)
	}
	if !strings.Contains(resSingle.Ribbon, ":finance]") {
		t.Errorf("ribbon should contain family ':finance]', got: %s", resSingle.Ribbon)
	}

	// 1.5 100 Matches and TopK Clamping Invariant
	// Populate 100 facts sharing a common keyword
	commonKeyword := "swarm_telemetry_metric_shard"
	for i := 0; i < 100; i++ {
		_, err := lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Fact %03d: %s value=%d status=OK", i, commonKeyword, i*10),
			SourceID: fmt.Sprintf("metric_%d.go", i),
			Family:   "metrics",
			SpaceID:  "cardinality_space",
		})
		if err != nil {
			t.Fatalf("Remember shard %d failed: %v", i, err)
		}
	}

	// Test case A: TopK = 0 -> must default to MaxRecallTopK (10)
	resTopK0, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   commonKeyword,
		SpaceID: "cardinality_space",
		TopK:    0,
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped TopK=0 failed: %v", err)
	}
	if resTopK0.Count != 10 || len(resTopK0.Matches) != 10 {
		t.Errorf("TopK=0 should default to 10, got count=%d, matches=%d", resTopK0.Count, len(resTopK0.Matches))
	}
	if !strings.Contains(resTopK0.Ribbon, "count=10 | ") {
		t.Errorf("ribbon should indicate count=10, got: %s", resTopK0.Ribbon)
	}
	delimiterCount0 := strings.Count(resTopK0.Ribbon, " > ")
	if delimiterCount0 != 9 {
		t.Errorf("expected 9 ' > ' delimiters for 10 items, got %d", delimiterCount0)
	}

	// Test case B: TopK = 5 -> exactly 5 matches
	resTopK5, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   commonKeyword,
		SpaceID: "cardinality_space",
		TopK:    5,
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped TopK=5 failed: %v", err)
	}
	if resTopK5.Count != 5 || len(resTopK5.Matches) != 5 {
		t.Errorf("TopK=5 should return 5, got count=%d, matches=%d", resTopK5.Count, len(resTopK5.Matches))
	}
	if !strings.Contains(resTopK5.Ribbon, "count=5 | ") {
		t.Errorf("ribbon should indicate count=5, got: %s", resTopK5.Ribbon)
	}
	if strings.Count(resTopK5.Ribbon, " > ") != 4 {
		t.Errorf("expected 4 ' > ' delimiters for 5 items, got %d", strings.Count(resTopK5.Ribbon, " > "))
	}

	// Test case C: TopK = 100 -> must be strictly clamped to MaxRecallTopK (10)
	resTopK100, err := lat.RecallScoped(ctx, icx.RecallRequest{
		Query:   commonKeyword,
		SpaceID: "cardinality_space",
		TopK:    100,
		Format:  "ribbon",
	})
	if err != nil {
		t.Fatalf("RecallScoped TopK=100 failed: %v", err)
	}
	if resTopK100.Count != 10 || len(resTopK100.Matches) != 10 {
		t.Errorf("TopK=100 must be clamped to 10, got count=%d, matches=%d", resTopK100.Count, len(resTopK100.Matches))
	}
	if !strings.Contains(resTopK100.Ribbon, "count=10 | ") {
		t.Errorf("ribbon should indicate count=10 when clamped, got: %s", resTopK100.Ribbon)
	}
}

// ============================================================================
// 2. Special Characters, Unicode, Quotes, Newlines & XML Injection
// ============================================================================

func TestRecallScoped_Ribbon_SpecialCharsAndUnicode(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	spaceID := "adversarial_chars_space"

	testCases := []struct {
		name        string
		text        string
		query       string
		family      string
		expectMatch bool
	}{
		{
			name:        "Quotes and escaped backslashes",
			text:        `const JSONExample = "{\"status\": \"active\", \"msg\": \"hello \\\"world\\\"\"}"`,
			query:       `JSONExample`,
			family:      `quotes`,
			expectMatch: true,
		},
		{
			name:        "Windows file paths with backslashes",
			text:        `Path config: C:\Workspace\OmniCoder Workspace\omnicoder.exe`,
			query:       `OmniCoder Workspace`,
			family:      `paths`,
			expectMatch: true,
		},
		{
			name:        "Multiline text with carriage returns and tabs",
			text:        "Line 1: init\r\n\tLine 2: processing payload\r\n\tLine 3: finished\n\n\n",
			query:       "processing payload",
			family:      "multiline",
			expectMatch: true,
		},
		{
			name:        "Chinese CJK Unicode characters",
			text:        "func 计算高精度收益率(本金 decimal.Decimal, 年利率 decimal.Decimal) decimal.Decimal",
			query:       "计算高精度收益率",
			family:      "unicode_cjk",
			expectMatch: true,
		},
		{
			name:        "Unicode emojis and symbols",
			text:        "Telemetry status: 🚀 Rocket launch successful with ⚡ power and 🔥 flame 💎",
			query:       "Rocket launch",
			family:      "emoji",
			expectMatch: true,
		},
		{
			name:        "Long CJK string exceeding 50 bytes (testing truncation & UTF-8 integrity)",
			text:        "这是一个非常非常长的中文注释信息用来测试在五十五字节截断点时的行为是否会导致非法字节序列或者破坏整体格式",
			query:       "中文注释信息",
			family:      "cjk_long",
			expectMatch: true,
		},
		{
			name:        "XML and tag injection attempt",
			text:        `Exploit attempt: <nav:ribbon: evil="true" /> </nav:ribbon> [id:fake] "fake content" > [evil]`,
			query:       `Exploit attempt`,
			family:      `sec_injection`,
			expectMatch: true,
		},
		{
			name:        "Query with quotes, slashes, and regex metacharacters",
			text:        `Regex pattern matching: ^[a-zA-Z0-9_.-]+@[a-zA-Z0-9-]+\.[a-zA-Z0-9-.]+$`,
			query:       `^[a-zA-Z0-9_.-]+`,
			family:      `regex`,
			expectMatch: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lat.Remember(ctx, icx.RememberRequest{
				Text:     tc.text,
				SourceID: "adversarial_test.go",
				Family:   tc.family,
				SpaceID:  spaceID,
			})
			if err != nil {
				t.Fatalf("Remember failed for %s: %v", tc.name, err)
			}

			res, err := lat.RecallScoped(ctx, icx.RecallRequest{
				Query:   tc.query,
				SpaceID: spaceID,
				Family:  tc.family,
				Format:  icx.FormatRibbon,
			})
			if err != nil {
				t.Fatalf("RecallScoped failed for %s: %v", tc.name, err)
			}

			if tc.expectMatch {
				if res.Count == 0 {
					t.Fatalf("[%s] expected at least 1 match for query %q, got 0", tc.name, tc.query)
				}
			}

			// Validate Ribbon Structure Invariants
			ribbon := res.Ribbon
			if !strings.HasPrefix(ribbon, "<nav:ribbon:") {
				t.Errorf("[%s] ribbon must start with '<nav:ribbon:', got: %s", tc.name, ribbon)
			}
			if !strings.HasSuffix(ribbon, "/>") {
				t.Errorf("[%s] ribbon must end with '/>', got: %s", tc.name, ribbon)
			}

			// Invariant: Ribbon must not contain raw unescaped newlines
			if strings.Contains(ribbon, "\n") || strings.Contains(ribbon, "\r") {
				t.Errorf("[%s] ribbon must not contain raw newline characters, got: %q", tc.name, ribbon)
			}

			// Invariant: JSON serialization must succeed cleanly without errors or corruption
			marshaled, err := json.Marshal(res)
			if err != nil {
				t.Fatalf("[%s] json.Marshal failed on RecallResponse: %v", tc.name, err)
			}

			var unmarshaled icx.RecallResponse
			if err := json.Unmarshal(marshaled, &unmarshaled); err != nil {
				t.Fatalf("[%s] json.Unmarshal failed: %v", tc.name, err)
			}
			if unmarshaled.Ribbon != ribbon {
				t.Errorf("[%s] ribbon mismatch after round-trip JSON: expected %q, got %q", tc.name, ribbon, unmarshaled.Ribbon)
			}
		})
	}
}

// ============================================================================
// 3. MultihopWalk Ribbon Formatting Under Adversarial Graphs
// ============================================================================

func TestMultihopWalk_Ribbon_AdversarialGraph(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	spaceID := "walk_ribbon_space"

	// 3.1 Non-existent start node -> clean error response, empty ribbon, no panic
	resNotFound, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "non_existent_node_xyz",
		MaxHops:   5,
		SpaceID:   spaceID,
		Format:    "ribbon",
	})
	if err != nil {
		t.Fatalf("MultihopWalk should not return error for missing node: %v", err)
	}
	if resNotFound.OK {
		t.Errorf("expected OK=false for non-existent node, got true")
	}
	if resNotFound.Error != "NODE_NOT_FOUND" {
		t.Errorf("expected NODE_NOT_FOUND, got %q", resNotFound.Error)
	}
	if resNotFound.Ribbon != "" {
		t.Errorf("expected empty ribbon on error, got %q", resNotFound.Ribbon)
	}

	// 3.2 Isolated single node (0 edges)
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "file_isolated.go",
		Name:      "isolated.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})
	resIsolated, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "file_isolated.go",
		MaxHops:   5,
		SpaceID:   spaceID,
		Format:    icx.FormatRibbon,
	})
	if err != nil {
		t.Fatalf("MultihopWalk isolated failed: %v", err)
	}
	if !resIsolated.OK {
		t.Fatalf("MultihopWalk isolated should be OK, got err: %s", resIsolated.Error)
	}
	if !strings.HasPrefix(resIsolated.Ribbon, `<nav:ribbon: root="isolated.go" hops=5 | `) {
		t.Errorf("unexpected isolated ribbon prefix: %s", resIsolated.Ribbon)
	}
	if !strings.Contains(resIsolated.Ribbon, "d0:isolated.go(file):root") {
		t.Errorf("expected root step in isolated walk ribbon, got: %s", resIsolated.Ribbon)
	}

	// 3.3 Graph with Self-loops, Cyclic recursion, and Reconvergent Diamonds
	// Nodes:
	// root -> node_A -> node_B -> node_A (cycle)
	// root -> node_C -> node_D (diamond branch 1)
	// root -> node_E -> node_D (diamond branch 2)
	// node_D -> node_D (self-loop)
	lat.AddASTNode(&icx.LatticeNode{ID: "root", Name: "RootService", Type: "struct"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_A", Name: "ServiceA", Type: "interface"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_B", Name: "ServiceB", Type: "function"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_C", Name: "BranchC", Type: "type"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_D", Name: "SinkD", Type: "function"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_E", Name: "BranchE", Type: "type"})

	lat.AddASTEdge("root", "calls", "node_A")
	lat.AddASTEdge("node_A", "calls", "node_B")
	lat.AddASTEdge("node_B", "calls", "node_A") // cycle A-B-A
	lat.AddASTEdge("root", "imports", "node_C")
	lat.AddASTEdge("node_C", "calls", "node_D")
	lat.AddASTEdge("root", "imports", "node_E")
	lat.AddASTEdge("node_E", "calls", "node_D") // diamond reconvergence to SinkD
	lat.AddASTEdge("node_D", "calls", "node_D") // self loop

	resComplex, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "root",
		MaxHops:   5,
		SpaceID:   spaceID,
		Format:    "ribbon",
	})
	if err != nil {
		t.Fatalf("MultihopWalk complex graph failed: %v", err)
	}
	if !resComplex.OK {
		t.Fatalf("expected OK=true on complex graph, got %s", resComplex.Error)
	}
	if resComplex.Ribbon == "" {
		t.Fatal("expected non-empty ribbon in complex walk response")
	}

	// Invariants on ribbon output
	if !strings.HasPrefix(resComplex.Ribbon, `<nav:ribbon: root="root" hops=5 | `) {
		t.Errorf("unexpected complex ribbon root: %s", resComplex.Ribbon)
	}
	if !strings.HasSuffix(resComplex.Ribbon, `/>`) {
		t.Errorf("complex ribbon must end with '/>', got: %s", resComplex.Ribbon)
	}
	if !strings.Contains(resComplex.Ribbon, "d0:RootService(struct):root") {
		t.Errorf("missing root entry in complex ribbon: %s", resComplex.Ribbon)
	}
	if !strings.Contains(resComplex.Ribbon, "ServiceA") || !strings.Contains(resComplex.Ribbon, "SinkD") {
		t.Errorf("missing visited nodes in complex ribbon: %s", resComplex.Ribbon)
	}

	// Verify cycle detection operated correctly without hanging
	if resComplex.CyclesDetected == 0 {
		t.Errorf("expected cycles to be detected on A-B-A cycle and self-loop, got 0")
	}

	// 3.4 Node Names with Special Characters (Generics, Methods, Paths)
	specialName := "pkg/ui/cli.go::(*Session[T, K]).WriteFile"
	lat.AddASTNode(&icx.LatticeNode{
		ID:   "special_node",
		Name: specialName,
		Type: "method",
	})
	lat.AddASTEdge("root", "calls", "special_node")

	resSpecial, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "special_node",
		MaxHops:   2,
		Format:    "ribbon",
	})
	if err != nil {
		t.Fatalf("MultihopWalk special failed: %v", err)
	}
	if !strings.Contains(resSpecial.Ribbon, specialName) {
		t.Errorf("ribbon should preserve special characters in node name, got: %s", resSpecial.Ribbon)
	}
}

// ============================================================================
// 4. CallTool Dispatch with format: "ribbon"
// ============================================================================

func TestCallTool_Ribbon_Integration(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	spaceID := "tool_dispatch_space"

	// Populate lattice facts
	_, err := lat.Remember(ctx, icx.RememberRequest{
		Text:     "kernel security audit authorization token validator",
		SourceID: "security.go",
		Family:   "auth",
		SpaceID:  spaceID,
	})
	if err != nil {
		t.Fatalf("Remember failed: %v", err)
	}

	// 4.1 icx_recall_scoped with format: "ribbon"
	toolRes, err := lat.CallTool(ctx, "icx_recall_scoped", map[string]any{
		"query":    "security audit",
		"space_id": spaceID,
		"top_k":    5,
		"format":   "ribbon",
	})
	if err != nil {
		t.Fatalf("CallTool icx_recall_scoped failed: %v", err)
	}
	if toolRes.IsError {
		t.Fatalf("CallTool returned IsError=true: %s", toolRes.Content[0].Text)
	}
	if len(toolRes.Content) == 0 {
		t.Fatal("CallTool returned 0 content items")
	}

	// Parse JSON payload returned by CallTool
	var recallRes icx.RecallResponse
	if err := json.Unmarshal([]byte(toolRes.Content[0].Text), &recallRes); err != nil {
		t.Fatalf("failed to unmarshal CallTool JSON result: %v", err)
	}
	if recallRes.Count != 1 {
		t.Errorf("expected count=1, got %d", recallRes.Count)
	}
	if !strings.HasPrefix(recallRes.Ribbon, `<nav:ribbon: query="security audit" count=1 | `) {
		t.Errorf("unexpected ribbon in CallTool response: %s", recallRes.Ribbon)
	}

	// 4.2 icx_recall_scoped with format: "ribbon" and ZERO matches
	toolResEmpty, err := lat.CallTool(ctx, "icx_recall_scoped", map[string]any{
		"query":    "non_existent_metric_9876",
		"space_id": spaceID,
		"format":   "ribbon",
	})
	if err != nil {
		t.Fatalf("CallTool empty failed: %v", err)
	}
	if toolResEmpty.IsError {
		t.Fatalf("CallTool empty returned IsError: %s", toolResEmpty.Content[0].Text)
	}
	var recallResEmpty icx.RecallResponse
	if err := json.Unmarshal([]byte(toolResEmpty.Content[0].Text), &recallResEmpty); err != nil {
		t.Fatalf("failed to unmarshal empty CallTool JSON: %v", err)
	}
	if recallResEmpty.Ribbon != `<nav:ribbon: query="non_existent_metric_9876" count=0 | empty />` {
		t.Errorf("unexpected empty ribbon from CallTool: %s", recallResEmpty.Ribbon)
	}

	// 4.3 icx_multihop_walk with format: "ribbon"
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "node_dispatch_entry",
		Name:      "Entrypoint",
		Type:      "function",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "node_dispatch_worker",
		Name:      "Worker",
		Type:      "struct",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTEdge("node_dispatch_entry", "calls", "node_dispatch_worker")

	walkToolRes, err := lat.CallTool(ctx, "icx_multihop_walk", map[string]any{
		"start_node": "node_dispatch_entry",
		"max_hops":   3,
		"space_id":   spaceID,
		"format":     "ribbon",
	})
	if err != nil {
		t.Fatalf("CallTool icx_multihop_walk failed: %v", err)
	}
	if walkToolRes.IsError {
		t.Fatalf("CallTool walk returned IsError: %s", walkToolRes.Content[0].Text)
	}

	var walkRes icx.MultihopWalkResponse
	if err := json.Unmarshal([]byte(walkToolRes.Content[0].Text), &walkRes); err != nil {
		t.Fatalf("failed to unmarshal walk CallTool result: %v", err)
	}
	if !walkRes.OK {
		t.Fatalf("walk failed: %s", walkRes.Error)
	}
	if !strings.HasPrefix(walkRes.Ribbon, `<nav:ribbon: root="node_dispatch_entry" hops=5 | `) {
		t.Errorf("unexpected walk ribbon: %s", walkRes.Ribbon)
	}
	if !strings.Contains(walkRes.Ribbon, "d0:Entrypoint(function):root") {
		t.Errorf("missing entrypoint step in walk ribbon: %s", walkRes.Ribbon)
	}

	// 4.4 Defensive robustness: malformed arguments to CallTool
	malformedCalls := []struct {
		tool string
		args map[string]any
	}{
		{
			tool: "icx_recall_scoped",
			args: map[string]any{"top_k": "invalid_string", "format": 12345},
		},
		{
			tool: "icx_recall_scoped",
			args: map[string]any{"top_k": 3.14159, "query": nil},
		},
		{
			tool: "icx_multihop_walk",
			args: map[string]any{"max_hops": "invalid", "start_node": nil},
		},
		{
			tool: "icx_multihop_walk",
			args: map[string]any{"max_hops": 4.5, "format": true},
		},
	}

	for i, mc := range malformedCalls {
		t.Run(fmt.Sprintf("Malformed_%d_%s", i, mc.tool), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("CallTool panicked on malformed input: %v", r)
				}
			}()
			res, err := lat.CallTool(ctx, mc.tool, mc.args)
			if err != nil {
				t.Fatalf("CallTool returned go error: %v", err)
			}
			if len(res.Content) == 0 {
				t.Errorf("expected at least 1 content item from CallTool")
			}
		})
	}
}

// ============================================================================
// 5. Concurrency Safety Under High Contention (go test -race)
// ============================================================================

func TestRibbon_HighContentionRace(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	spaceID := "concurrency_race_space"

	// Seed base facts and AST nodes
	for i := 0; i < 20; i++ {
		_, _ = lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Base seed fact %d for concurrency testing", i),
			SourceID: fmt.Sprintf("seed_%d.go", i),
			Family:   "seed",
			SpaceID:  spaceID,
		})
		lat.AddASTNode(&icx.LatticeNode{
			ID:        fmt.Sprintf("seed_node_%d", i),
			Name:      fmt.Sprintf("SeedFunc_%d", i),
			Type:      "function",
			Dimension: "3D_VOLUMETRIC",
		})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 200)

	iterationsPerWorker := 40

	// 10 Goroutines: Remembering new facts concurrently
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for i := 0; i < iterationsPerWorker; i++ {
				_, err := lat.Remember(ctx, icx.RememberRequest{
					Text:     fmt.Sprintf("Worker %d Fact %d random=%d", workerID, i, rng.Intn(1000)),
					SourceID: fmt.Sprintf("worker_%d.go", workerID),
					Family:   "concurrent_family",
					SpaceID:  spaceID,
				})
				if err != nil {
					errCh <- fmt.Errorf("Remember failed: %w", err)
					return
				}
			}
		}(w)
	}

	// 10 Goroutines: RecallScoped with ribbon format concurrently
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				res, err := lat.RecallScoped(ctx, icx.RecallRequest{
					Query:   "concurrent",
					SpaceID: spaceID,
					TopK:    5,
					Format:  "ribbon",
				})
				if err != nil {
					errCh <- fmt.Errorf("RecallScoped failed: %w", err)
					return
				}
				if !strings.HasPrefix(res.Ribbon, "<nav:ribbon:") || !strings.HasSuffix(res.Ribbon, "/>") {
					errCh <- fmt.Errorf("malformed ribbon in concurrent recall: %s", res.Ribbon)
					return
				}
			}
		}(w)
	}

	// 10 Goroutines: Mutating AST graph and walking with format: ribbon concurrently
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				nodeID := fmt.Sprintf("dyn_node_%d_%d", workerID, i)
				lat.AddASTNode(&icx.LatticeNode{
					ID:        nodeID,
					Name:      nodeID,
					Type:      "type",
					Dimension: "3D_VOLUMETRIC",
				})
				targetID := fmt.Sprintf("seed_node_%d", i%20)
				lat.AddASTEdge(nodeID, "calls", targetID)

				walkRes, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
					StartNode: nodeID,
					MaxHops:   3,
					SpaceID:   spaceID,
					Format:    icx.FormatRibbon,
				})
				if err != nil {
					errCh <- fmt.Errorf("MultihopWalk failed: %w", err)
					return
				}
				if !walkRes.OK {
					errCh <- fmt.Errorf("MultihopWalk OK=false: %s", walkRes.Error)
					return
				}
				if !strings.HasPrefix(walkRes.Ribbon, "<nav:ribbon:") || !strings.HasSuffix(walkRes.Ribbon, "/>") {
					errCh <- fmt.Errorf("malformed walk ribbon under concurrency: %s", walkRes.Ribbon)
					return
				}
			}
		}(w)
	}

	// 10 Goroutines: CallTool dispatch concurrently
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				tool := "icx_recall_scoped"
				args := map[string]any{
					"query":    "seed",
					"space_id": spaceID,
					"format":   "ribbon",
				}
				if i%2 == 1 {
					tool = "icx_multihop_walk"
					args = map[string]any{
						"start_node": fmt.Sprintf("seed_node_%d", i%20),
						"format":     "ribbon",
					}
				}
				res, err := lat.CallTool(ctx, tool, args)
				if err != nil {
					errCh <- fmt.Errorf("CallTool %s failed: %w", tool, err)
					return
				}
				if res.IsError {
					errCh <- fmt.Errorf("CallTool %s returned error: %s", tool, res.Content[0].Text)
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	var errList []error
	for err := range errCh {
		errList = append(errList, err)
	}
	if len(errList) > 0 {
		t.Fatalf("encountered %d concurrency errors, first: %v", len(errList), errList[0])
	}
}
