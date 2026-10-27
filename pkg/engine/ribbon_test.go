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

func TestFormatASTRibbon_TokenBoundsAndSyntax(t *testing.T) {
	// 1. Empty steps -> isolated
	emptyRibbon := FormatASTRibbon([]string{"file_calc.go"}, nil, 155)
	expectedEmpty := "<nav:ribbon: root=\"calc.go\" hops=5 | isolated />"
	if emptyRibbon != expectedEmpty {
		t.Errorf("expected %q, got %q", expectedEmpty, emptyRibbon)
	}

	// 2. Normal steps
	steps := []icx.WalkStep{
		{Depth: 0, Name: "calc.go", Type: "file", Relation: ""},
		{Depth: 1, Name: "Add", Type: "function", Relation: "defines"},
		{Depth: 1, Name: "Sub", Type: "function", Relation: "defines"},
		{Depth: 2, Name: "Logger", Type: "type", Relation: "calls"},
	}
	ribbon := FormatASTRibbon([]string{"file_calc.go"}, steps, 155)
	if !strings.HasPrefix(ribbon, "<nav:ribbon: root=\"calc.go\" hops=5 | ") {
		t.Errorf("unexpected ribbon prefix: %s", ribbon)
	}
	if !strings.Contains(ribbon, "d0:calc.go(file):root") {
		t.Errorf("expected d0:calc.go in ribbon, got: %s", ribbon)
	}
	if !strings.Contains(ribbon, "d1:Add(function):defines") {
		t.Errorf("expected d1:Add in ribbon, got: %s", ribbon)
	}
	if !strings.HasSuffix(ribbon, "/>") {
		t.Errorf("ribbon must close with '/>', got: %s", ribbon)
	}

	tokens := llm.EstimateTokens(ribbon)
	if tokens >= 160 {
		t.Errorf("ribbon tokens %d >= 160 budget", tokens)
	}

	// 3. 500 steps stress test -> strictly < 160 tokens
	manySteps := make([]icx.WalkStep, 500)
	for i := 0; i < 500; i++ {
		manySteps[i] = icx.WalkStep{
			Depth:    (i % 5) + 1,
			Name:     fmt.Sprintf("Identifier_%d_VeryLongSymbolNameForStressTesting", i),
			Type:     "function",
			Relation: "calls",
		}
	}
	clampedRibbon := FormatASTRibbon([]string{"file_engine.go"}, manySteps, 155)
	clampedTokens := llm.EstimateTokens(clampedRibbon)
	if clampedTokens >= 160 {
		t.Fatalf("VIOLATION: clamped ribbon tokens %d >= 160 cap", clampedTokens)
	}
	if !strings.Contains(clampedRibbon, "_more") && !strings.Contains(clampedRibbon, "clamped") {
		t.Errorf("expected truncation indicator in clamped ribbon, got: %s", clampedRibbon)
	}
}

func TestExecuteStage2ASTBlastRadiusWalk_E2E(t *testing.T) {
	lat := icx.NewLocalMemoryCache("e2e_space")
	mockGate := llm.NewMockDecisionGate()

	// Ingest AST nodes
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "file_service.go",
		Name:      "service.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "func_Start",
		Name:      "Start",
		Type:      "function",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTEdge("file_service.go", "defines", "func_Start")

	orch, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: t.TempDir(),
		ICXClient:    lat,
		DecisionGate: mockGate,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	pipeline := &TurnPipeline{orch: orch}
	ctx := context.Background()
	state := NewEngineState("test AST ribbon walk", EngineConfig{MaxTurns: 3})
	state.LatticeSpaceID = "e2e_space"

	turn := &TurnState{
		TurnNumber: 1,
		Task:       "test AST ribbon walk",
		FocusFiles: []string{"service.go"},
	}

	err = pipeline.executeStage2ASTBlastRadiusWalk(ctx, state, turn, EngineConfig{MaxTurns: 3})
	if err != nil {
		t.Fatalf("executeStage2ASTBlastRadiusWalk failed: %v", err)
	}

	// Verify turn.ASTRibbon is populated
	if turn.ASTRibbon == "" {
		t.Fatal("turn.ASTRibbon was not populated")
	}
	if !strings.HasPrefix(turn.ASTRibbon, "<nav:ribbon: root=\"service.go\" hops=5 | ") {
		t.Errorf("unexpected turn.ASTRibbon: %s", turn.ASTRibbon)
	}
	if !strings.Contains(turn.ASTRibbon, "d1:Start(function):defines") {
		t.Errorf("expected d1:Start in turn.ASTRibbon, got: %s", turn.ASTRibbon)
	}

	// Verify tokens < 160
	tokens := llm.EstimateTokens(turn.ASTRibbon)
	if tokens >= 160 {
		t.Errorf("turn.ASTRibbon tokens %d >= 160", tokens)
	}

	// Verify backward compatibility: turn.ASTNeighborhood has raw entries
	if len(turn.ASTNeighborhood) == 0 {
		t.Fatal("turn.ASTNeighborhood should retain raw neighborhood strings")
	}
}

func TestStage4_PassesASTRibbonToGateEditCard(t *testing.T) {
	lat := icx.NewLocalMemoryCache("e2e_space")
	mockGate := llm.NewMockDecisionGate()

	var receivedCard llm.GateEditCard
	mockGate.IntentFunc = func(ctx context.Context, card llm.GateIntentCard) (*llm.IntentDecision, error) {
		return &llm.IntentDecision{
			Operation:   llm.OpModify,
			TargetFiles: []string{"main.go"},
			Rationale:   "Add initialization",
		}, nil
	}
	mockGate.EditFunc = func(ctx context.Context, card llm.GateEditCard) (*llm.EditDecision, error) {
		receivedCard = card
		return &llm.EditDecision{
			FilePath:    card.FilePath,
			Explanation: "done",
		}, nil
	}

	orch, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: t.TempDir(),
		ICXClient:    lat,
		DecisionGate: mockGate,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	pipeline := &TurnPipeline{orch: orch}
	ctx := context.Background()
	state := NewEngineState("test gate ribbon propagation", EngineConfig{MaxTurns: 3})
	state.LatticeSpaceID = "e2e_space"

	testRibbon := "<nav:ribbon: root=\"main.go\" hops=5 | d0:main.go(file):root > d1:main(function):defines />"
	turn := &TurnState{
		TurnNumber:      1,
		Task:            "test gate ribbon propagation",
		FocusFiles:      []string{"main.go"},
		TargetFiles:     []string{"main.go"},
		ASTRibbon:       testRibbon,
		ASTNeighborhood: []string{"main.go ( file)", "main (defines function)"},
	}

	err = pipeline.executeStage4MicroDecisionGate(ctx, state, turn, EngineConfig{MaxTurns: 3})
	if err != nil {
		t.Fatalf("executeStage4MicroDecisionGate failed: %v", err)
	}

	if receivedCard.ASTRibbon != testRibbon {
		t.Errorf("GateEditCard did not receive ASTRibbon: expected %q, got %q", testRibbon, receivedCard.ASTRibbon)
	}
	if len(receivedCard.ASTNeighborhood) != 2 {
		t.Errorf("GateEditCard did not receive ASTNeighborhood slice: got %d elements", len(receivedCard.ASTNeighborhood))
	}
}
