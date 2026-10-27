// SPDX-License-Identifier: Apache-2.0
package llm

import (
	"fmt"
	"strings"
	"testing"
)

func TestGateEditCard_ASTRibbon_PromptFormatting(t *testing.T) {
	ribbon := "<nav:ribbon: root=\"calc.go\" hops=5 | d0:calc.go(file):root > d1:Add(function):defines > d1:Sub(function):defines />"

	card := GateEditCard{
		FilePath:       "calc.go",
		Instruction:    "Refactor Add and Sub to support overflow checks",
		ContextSnippet: "func Add(a, b int) int { return a + b }\nfunc Sub(a, b int) int { return a - b }\n",
		ASTRibbon:      ribbon,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}

	// 1. Verify AST ribbon line is emitted
	if !strings.Contains(userPrompt, "AST: "+ribbon) {
		t.Errorf("expected user prompt to contain 'AST: <ribbon>', got:\n%s", userPrompt)
	}

	// 2. Verify AST ribbon tokens are strictly < 160
	astTokens := EstimateTokens(ribbon)
	if astTokens >= 160 {
		t.Errorf("AST ribbon must be < 160 tokens, got %d", astTokens)
	}

	// 3. Verify total card prompt is strictly < 300 tokens
	totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens >= MaxCardTokenBudget {
		t.Errorf("Total card tokens %d exceeds max budget %d", totalTokens, MaxCardTokenBudget)
	}

	if err := ValidateTokenBudget(sysPrompt, userPrompt, MaxCardTokenBudget); err != nil {
		t.Errorf("ValidateTokenBudget failed: %v", err)
	}
}

func TestGateEditCard_ASTNeighborhood_AutomaticRibbonConversion(t *testing.T) {
	// Raw string slice fallback: should automatically convert to ribbon
	rawSlice := []string{
		"calc.go ( file)",
		"Add (defines function)",
		"Sub (defines function)",
		"Mul (defines function)",
		"Div (defines function)",
	}

	card := GateEditCard{
		FilePath:        "calc.go",
		Instruction:     "Add integer division zero-check",
		ContextSnippet:  "func Div(a, b int) int { return a / b }",
		ASTNeighborhood: rawSlice,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}

	// Must be converted into <nav:ribbon:...>
	if !strings.Contains(userPrompt, "AST: <nav:ribbon:") {
		t.Errorf("expected prompt to contain '<nav:ribbon:', got:\n%s", userPrompt)
	}

	totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens >= MaxCardTokenBudget {
		t.Errorf("Total card tokens %d exceeds max budget %d", totalTokens, MaxCardTokenBudget)
	}
}

func TestGateEditCard_MassiveASTNeighborhood_Clamping(t *testing.T) {
	// 500 items in AST neighborhood
	manyAST := make([]string, 500)
	for i := 0; i < 500; i++ {
		manyAST[i] = fmt.Sprintf("Node_%d (calls helper_%d)", i, i)
	}

	card := GateEditCard{
		FilePath:        "engine.go",
		Instruction:     "Optimize hot path execution",
		ContextSnippet:  "func Execute() { println(\"running\") }",
		ASTNeighborhood: manyAST,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}

	// Verify total tokens strictly < 300
	totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens >= MaxCardTokenBudget {
		t.Errorf("500 AST nodes caused prompt tokens %d >= %d", totalTokens, MaxCardTokenBudget)
	}

	// Verify the AST line contains [+..._more] or clamped indicator
	if !strings.Contains(userPrompt, "_more") && !strings.Contains(userPrompt, "clamped") {
		t.Errorf("expected ribbon truncation indicator in prompt, got:\n%s", userPrompt)
	}
}

func TestFormatASTRibbonFromStrings_EmptyAndClamped(t *testing.T) {
	// Empty slice
	emptyRes := FormatASTRibbonFromStrings("calc.go", nil, 150)
	expectedEmpty := "<nav:ribbon: root=\"calc.go\" hops=5 | isolated />"
	if emptyRes != expectedEmpty {
		t.Errorf("expected %q, got %q", expectedEmpty, emptyRes)
	}

	// Giant inputs
	var items []string
	for i := 0; i < 200; i++ {
		items = append(items, fmt.Sprintf("very_long_component_name_number_%d_with_extended_attributes_in_ast", i))
	}
	clampedRes := FormatASTRibbonFromStrings("root.go", items, 100)
	tokens := EstimateTokens(clampedRes)
	if tokens >= 100 {
		t.Errorf("expected < 100 tokens, got %d", tokens)
	}
	if !strings.HasPrefix(clampedRes, "<nav:ribbon: root=\"root.go\" hops=5 | ") {
		t.Errorf("unexpected prefix: %s", clampedRes)
	}
	if !strings.HasSuffix(clampedRes, "/>") {
		t.Errorf("must end with '/>', got: %s", clampedRes)
	}
}
