// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ----------------------------------------------------------------------------
// 1. Extreme Scale: 500+ Nodes Dense Star Graph
// ----------------------------------------------------------------------------

func TestASTGraph_500Nodes_DenseStar(t *testing.T) {
	lat := icx.NewLocalMemoryCache("star_space")
	ctx := context.Background()

	lat.AddASTNode(&icx.LatticeNode{
		ID:        "file_hub.go",
		Name:      "hub.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})

	const numChildren = 500
	for i := 0; i < numChildren; i++ {
		fnID := fmt.Sprintf("func_child_%d", i)
		fnName := fmt.Sprintf("ChildWorkerFunction_%d_WithLongDescriptiveSymbolName", i)
		lat.AddASTNode(&icx.LatticeNode{
			ID:        fnID,
			Name:      fnName,
			Type:      "function",
			Dimension: "3D_VOLUMETRIC",
		})
		lat.AddASTEdge("file_hub.go", "defines", fnID)
	}

	walkRes, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "file_hub.go",
		MaxHops:   5,
		SpaceID:   "star_space",
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}
	if !walkRes.OK {
		t.Fatalf("walk not OK: %s", walkRes.Error)
	}
	if len(walkRes.Steps) <= numChildren {
		t.Logf("Walk reached %d steps", len(walkRes.Steps))
	}

	// Test FormatASTRibbon directly
	ribbon := FormatASTRibbon([]string{"file_hub.go"}, walkRes.Steps, 155)
	tokens := llm.EstimateTokens(ribbon)
	if tokens >= 160 {
		t.Fatalf("VIOLATION: 500-node star graph ribbon tokens %d >= 160 cap:\n%s", tokens, ribbon)
	}
	if !strings.HasPrefix(ribbon, "<nav:ribbon: root=\"hub.go\" hops=5 | ") {
		t.Errorf("unexpected ribbon prefix: %s", ribbon)
	}
	if !strings.HasSuffix(ribbon, "/>") {
		t.Errorf("ribbon does not close with '/>': %s", ribbon)
	}
	if !strings.Contains(ribbon, "_more") && !strings.Contains(ribbon, "clamped") {
		t.Errorf("expected truncation indicator in 500-node ribbon, got: %s", ribbon)
	}

	// Test full executeStage2ASTBlastRadiusWalk pipeline
	orch, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: t.TempDir(),
		ICXClient:    lat,
		DecisionGate: llm.NewMockDecisionGate(),
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}
	pipeline := &TurnPipeline{orch: orch}
	state := NewEngineState("500-node star test", EngineConfig{MaxTurns: 3})
	state.LatticeSpaceID = "star_space"
	turn := &TurnState{
		TurnNumber: 1,
		Task:       "test 500 node star",
		FocusFiles: []string{"hub.go"},
	}

	if err := pipeline.executeStage2ASTBlastRadiusWalk(ctx, state, turn, EngineConfig{MaxTurns: 3}); err != nil {
		t.Fatalf("executeStage2ASTBlastRadiusWalk failed: %v", err)
	}

	stageTokens := llm.EstimateTokens(turn.ASTRibbon)
	if stageTokens >= 160 {
		t.Fatalf("VIOLATION: executeStage2ASTBlastRadiusWalk produced %d tokens >= 160 cap:\n%s", stageTokens, turn.ASTRibbon)
	}
}

// ----------------------------------------------------------------------------
// 2. Deep Linear Chain Graph (100 Hops)
// ----------------------------------------------------------------------------

func TestASTGraph_DeepChain(t *testing.T) {
	lat := icx.NewLocalMemoryCache("chain_space")
	ctx := context.Background()

	const chainLength = 100
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "file_start.go",
		Name:      "start.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})

	prevID := "file_start.go"
	for i := 1; i <= chainLength; i++ {
		nodeID := fmt.Sprintf("chain_node_%d", i)
		lat.AddASTNode(&icx.LatticeNode{
			ID:        nodeID,
			Name:      fmt.Sprintf("Step_%d", i),
			Type:      "step",
			Dimension: "3D_VOLUMETRIC",
		})
		lat.AddASTEdge(prevID, "calls", nodeID)
		prevID = nodeID
	}

	// Walk with 5 hops (default blast radius)
	walkRes, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "file_start.go",
		MaxHops:   5,
		SpaceID:   "chain_space",
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}

	ribbon := FormatASTRibbon([]string{"file_start.go"}, walkRes.Steps, 155)
	tokens := llm.EstimateTokens(ribbon)
	if tokens >= 160 {
		t.Fatalf("VIOLATION: deep chain ribbon tokens %d >= 160 cap", tokens)
	}
	if !strings.HasSuffix(ribbon, "/>") {
		t.Errorf("ribbon does not close with '/>': %s", ribbon)
	}
}

// ----------------------------------------------------------------------------
// 3. Cyclic Graphs & Self Loops
// ----------------------------------------------------------------------------

func TestASTGraph_CyclicAndSelfLoops(t *testing.T) {
	lat := icx.NewLocalMemoryCache("cycle_space")
	ctx := context.Background()

	// Triangle cycle A -> B -> C -> A, plus self-loop B -> B
	lat.AddASTNode(&icx.LatticeNode{ID: "node_A", Name: "NodeA", Type: "module", Dimension: "3D_VOLUMETRIC"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_B", Name: "NodeB", Type: "module", Dimension: "3D_VOLUMETRIC"})
	lat.AddASTNode(&icx.LatticeNode{ID: "node_C", Name: "NodeC", Type: "module", Dimension: "3D_VOLUMETRIC"})

	lat.AddASTEdge("node_A", "imports", "node_B")
	lat.AddASTEdge("node_B", "calls", "node_C")
	lat.AddASTEdge("node_C", "calls", "node_A")
	lat.AddASTEdge("node_B", "calls", "node_B") // self-loop

	walkRes, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "node_A",
		MaxHops:   5,
		SpaceID:   "cycle_space",
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}
	if walkRes.CyclesDetected < 1 {
		t.Errorf("expected at least 1 cycle detected, got %d", walkRes.CyclesDetected)
	}

	ribbon := FormatASTRibbon([]string{"node_A"}, walkRes.Steps, 155)
	tokens := llm.EstimateTokens(ribbon)
	if tokens >= 160 {
		t.Fatalf("VIOLATION: cyclic ribbon tokens %d >= 160 cap", tokens)
	}
	if !strings.HasPrefix(ribbon, "<nav:ribbon: root=\"node_A\" hops=5 | ") {
		t.Errorf("unexpected prefix: %s", ribbon)
	}
	if !strings.HasSuffix(ribbon, "/>") {
		t.Errorf("ribbon does not close with '/>': %s", ribbon)
	}
}

// ----------------------------------------------------------------------------
// 4. Empty, Nil, and Isolated Graphs
// ----------------------------------------------------------------------------

func TestASTGraph_EmptyAndIsolated(t *testing.T) {
	testCases := []struct {
		name       string
		candidates []string
		steps      []icx.WalkStep
		expected   string
	}{
		{
			name:       "nil candidates and nil steps",
			candidates: nil,
			steps:      nil,
			expected:   "<nav:ribbon: root=\"root\" hops=5 | isolated />",
		},
		{
			name:       "empty candidates and empty steps",
			candidates: []string{},
			steps:      []icx.WalkStep{},
			expected:   "<nav:ribbon: root=\"root\" hops=5 | isolated />",
		},
		{
			name:       "candidate with empty file_ prefix",
			candidates: []string{"file_"},
			steps:      nil,
			expected:   "<nav:ribbon: root=\"root\" hops=5 | isolated />",
		},
		{
			name:       "candidate file_main.go with nil steps",
			candidates: []string{"file_main.go"},
			steps:      nil,
			expected:   "<nav:ribbon: root=\"main.go\" hops=5 | isolated />",
		},
		{
			name:       "candidate file_main.go with 0 steps",
			candidates: []string{"file_main.go"},
			steps:      []icx.WalkStep{},
			expected:   "<nav:ribbon: root=\"main.go\" hops=5 | isolated />",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ribbon := FormatASTRibbon(tc.candidates, tc.steps, 155)
			if ribbon != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, ribbon)
			}
			tokens := llm.EstimateTokens(ribbon)
			if tokens >= 160 {
				t.Errorf("tokens %d >= 160", tokens)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 5. Adversarial Injections & Pathological Node Names
// ----------------------------------------------------------------------------

func TestASTGraph_AdversarialNodeNames(t *testing.T) {
	adversarialSteps := []icx.WalkStep{
		{Depth: 0, Name: "main.go", Type: "file", Relation: ""},
		{Depth: 1, Name: "<nav:ribbon: injection/>", Type: "function", Relation: "defines"},
		{Depth: 1, Name: "quotes\"and'specials`", Type: "type", Relation: "calls"},
		{Depth: 2, Name: "newlines\n\r\tspaces", Type: "var", Relation: "uses"},
		{Depth: 2, Name: "Unicode_🚀_⚡_漢字_مرحبا", Type: "const", Relation: "implements"},
		{Depth: 3, Name: strings.Repeat("very_long_identifier_", 20), Type: "package", Relation: "imports"},
	}

	ribbon := FormatASTRibbon([]string{"file_main.go"}, adversarialSteps, 155)
	tokens := llm.EstimateTokens(ribbon)
	if tokens >= 160 {
		t.Fatalf("VIOLATION: adversarial steps ribbon tokens %d >= 160 cap", tokens)
	}
	if !strings.HasPrefix(ribbon, "<nav:ribbon: root=\"main.go\" hops=5 | ") {
		t.Errorf("unexpected prefix: %s", ribbon)
	}
	if !strings.HasSuffix(ribbon, "/>") {
		t.Errorf("ribbon does not close with '/>': %s", ribbon)
	}
}

// ----------------------------------------------------------------------------
// 6. Stress Test: Pathological Giant Root / File Path
// ----------------------------------------------------------------------------

func TestASTRibbon_GiantRootName_BudgetAdherence(t *testing.T) {
	// What happens if candidate node has a 600-char path?
	giantPath := "file_" + strings.Repeat("nested_subfolder_dir/", 30) + "adversarial_component.go"
	steps := []icx.WalkStep{
		{Depth: 0, Name: "adversarial_component.go", Type: "file", Relation: "root"},
		{Depth: 1, Name: "Handler", Type: "function", Relation: "defines"},
	}

	ribbon := FormatASTRibbon([]string{giantPath}, steps, 155)
	tokens := llm.EstimateTokens(ribbon)
	t.Logf("Giant root ribbon: %s (tokens: %d)", ribbon, tokens)

	// Invariant: ribbon MUST strictly stay under 160 tokens
	if tokens >= 160 {
		t.Fatalf("VIOLATION: giant root caused ribbon tokens %d >= 160 cap:\n%s", tokens, ribbon)
	}
}
