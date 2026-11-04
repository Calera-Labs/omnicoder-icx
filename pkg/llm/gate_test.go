// SPDX-License-Identifier: Apache-2.0
package llm

import (
	"fmt"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// 1. GateEditCard Token Budget Stress: Giant Instructions & Snippets
// ----------------------------------------------------------------------------

func TestGateEditCard_BudgetInvariants_ExtremeCombinations(t *testing.T) {
	hugeRibbon := FormatASTRibbonFromStrings("calc.go", []string{
		"Add (function:defines)",
		"Sub (function:defines)",
		"Mul (function:defines)",
		"Div (function:defines)",
		"Mod (function:defines)",
		"Pow (function:defines)",
		"Sqrt (function:defines)",
	}, 150)

	testCases := []struct {
		name        string
		filePath    string
		instruction string
		snippet     string
		ribbon      string
		rawAST      []string
	}{
		{
			name:        "Giant 10K character instruction",
			filePath:    "service.go",
			instruction: strings.Repeat("Refactor this critical microservice to ensure complete deterministic behavior and auditability. ", 150),
			snippet:     "func Handle() { return }",
			ribbon:      hugeRibbon,
		},
		{
			name:        "Giant 50K character snippet",
			filePath:    "huge_file.go",
			instruction: "Fix syntax error on line 42",
			snippet:     strings.Repeat("package main\nfunc foo() {\n\tprintln(\"hello world\")\n}\n", 1500),
			ribbon:      hugeRibbon,
		},
		{
			name:        "Both Giant instruction AND Giant snippet AND Max Ribbon",
			filePath:    "everything_giant.go",
			instruction: strings.Repeat("Massive instructions with complex rules and constraints. ", 200),
			snippet:     strings.Repeat("var x = 1234567890;\n", 2000),
			ribbon:      hugeRibbon,
		},
		{
			name:        "Giant 1000-node ASTNeighborhood slice without pre-computed ribbon",
			filePath:    "deep_graph.go",
			instruction: "Update call graph",
			snippet:     "func Call() {}",
			rawAST: func() []string {
				items := make([]string, 1000)
				for i := 0; i < 1000; i++ {
					items[i] = fmt.Sprintf("Node_%d_WithLongMethodSignature_AndDetails (calls target_%d)", i, i)
				}
				return items
			}(),
		},
		{
			name:        "Pathological unspaced 10K character instruction",
			filePath:    "unspaced.go",
			instruction: strings.Repeat("A", 10000),
			snippet:     strings.Repeat("B", 10000),
			ribbon:      hugeRibbon,
		},
		{
			name:        "CJK, Arabic, and Emoji rich instruction and snippet",
			filePath:    "multilingual.go",
			instruction: "🚀 优化性能 修正并发竞态条件 и устранить утечку памяти في الخادم ⚡",
			snippet:     "// 这是一个测试函数 🚀\nfunc 测试() string {\n\treturn \"مرحبا بك في عالم الكود\"\n}\n",
			ribbon:      "<nav:ribbon: root=\"multilingual.go\" hops=5 | d0:multilingual.go(file):root > d1:测试(function):defines />",
		},
		{
			name:        "Completely Empty GateEditCard",
			filePath:    "",
			instruction: "",
			snippet:     "",
			ribbon:      "",
			rawAST:      nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			card := GateEditCard{
				FilePath:        tc.filePath,
				Instruction:     tc.instruction,
				ContextSnippet:  tc.snippet,
				ASTRibbon:       tc.ribbon,
				ASTNeighborhood: tc.rawAST,
			}

			sysPrompt, userPrompt, err := card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt failed: %v", err)
			}

			totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
			if totalTokens >= MaxCardTokenBudget {
				t.Fatalf("VIOLATION: totalTokens %d >= MaxCardTokenBudget %d in scenario %q\nSys: %s\nUser: %s",
					totalTokens, MaxCardTokenBudget, tc.name, sysPrompt, userPrompt)
			}

			if err := ValidateTokenBudget(sysPrompt, userPrompt, MaxCardTokenBudget); err != nil {
				t.Fatalf("VIOLATION: ValidateTokenBudget failed: %v", err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// 2. FormatASTRibbonFromStrings Stress Tests
// ----------------------------------------------------------------------------

func TestFormatASTRibbonFromStrings_Stress(t *testing.T) {
	// 1. Extreme item count
	manyItems := make([]string, 500)
	for i := 0; i < 500; i++ {
		manyItems[i] = fmt.Sprintf("Symbol_%03d (relation_type_long)", i)
	}

	ribbon := FormatASTRibbonFromStrings("root_module.go", manyItems, 150)
	tokens := EstimateTokens(ribbon)
	if tokens >= 160 {
		t.Fatalf("VIOLATION: FormatASTRibbonFromStrings produced %d tokens >= 160 cap:\n%s", tokens, ribbon)
	}
	if !strings.HasPrefix(ribbon, "<nav:ribbon: root=\"root_module.go\" hops=5 | ") {
		t.Errorf("unexpected prefix: %s", ribbon)
	}
	if !strings.HasSuffix(ribbon, "/>") {
		t.Errorf("must end with '/>': %s", ribbon)
	}
	if !strings.Contains(ribbon, "_more") && !strings.Contains(ribbon, "clamped") {
		t.Errorf("expected truncation indicator, got: %s", ribbon)
	}

	// 2. Giant individual items
	giantItems := []string{
		strings.Repeat("extremely_long_identifier_part_", 30),
		strings.Repeat("another_long_identifier_part_", 30),
	}
	ribbonGiant := FormatASTRibbonFromStrings("calc.go", giantItems, 150)
	tokensGiant := EstimateTokens(ribbonGiant)
	if tokensGiant >= 160 {
		t.Fatalf("VIOLATION: giant items produced %d tokens >= 160 cap:\n%s", tokensGiant, ribbonGiant)
	}
	if !strings.HasSuffix(ribbonGiant, "/>") {
		t.Errorf("must end with '/>': %s", ribbonGiant)
	}

	// 3. Pathological Giant rootName
	giantRoot := strings.Repeat("deep_directory_hierarchy_name/", 20) + "main.go"
	ribbonGiantRoot := FormatASTRibbonFromStrings(giantRoot, []string{"fn1", "fn2"}, 150)
	tokensGiantRoot := EstimateTokens(ribbonGiantRoot)
	t.Logf("Giant root from strings tokens: %d, string: %s", tokensGiantRoot, ribbonGiantRoot)
	if tokensGiantRoot >= 160 {
		t.Fatalf("VIOLATION: giant root name produced %d tokens >= 160 cap:\n%s", tokensGiantRoot, ribbonGiantRoot)
	}

	// 4. Duplicate items de-duplication
	dupItems := []string{"Add", "Add", "Add", "Sub", "Sub", "Mul"}
	ribbonDup := FormatASTRibbonFromStrings("math.go", dupItems, 150)
	if strings.Count(ribbonDup, "Add") != 1 {
		t.Errorf("expected 'Add' to appear exactly once, got: %s", ribbonDup)
	}
	if strings.Count(ribbonDup, "Sub") != 1 {
		t.Errorf("expected 'Sub' to appear exactly once, got: %s", ribbonDup)
	}
}

// ----------------------------------------------------------------------------
// 3. GateEditCard Prompt Integrity & Budget Adherence Under Combined Load
// ----------------------------------------------------------------------------

func TestGateEditCard_InstructionAndRibbon_PromptIntegrity(t *testing.T) {
	// Instruction of ~130 tokens, ASTRibbon of ~140 tokens
	instruction := strings.Repeat("Refactor method to check bounds and emit events. ", 15) // ~135 tokens
	ribbon := "<nav:ribbon: root=\"controller.go\" hops=5 | d0:controller.go(file):root > d1:Handle(function):defines > d1:Process(function):defines > d2:Worker(type):calls > d3:Store(interface):calls />"

	card := GateEditCard{
		FilePath:       "pkg/service/controller.go",
		Instruction:    instruction,
		ContextSnippet: "func Handle() { Process() }",
		ASTRibbon:      ribbon,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt returned error: %v", err)
	}

	totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	t.Logf("Total tokens: %d (sys: %d, user: %d)", totalTokens, EstimateTokens(sysPrompt), EstimateTokens(userPrompt))
	t.Logf("User prompt:\n%s", userPrompt)

	// Invariant 1: Total prompt must be strictly < 300 tokens
	if totalTokens >= MaxCardTokenBudget {
		t.Fatalf("VIOLATION: total tokens %d >= MaxCardTokenBudget %d", totalTokens, MaxCardTokenBudget)
	}

	// Invariant 2: User prompt MUST retain essential instructions and file context
	if !strings.Contains(userPrompt, "File: ") || !strings.Contains(userPrompt, "Instruction: ") {
		t.Fatalf("PROMPT CORRUPTION: userPrompt lost 'File:' or 'Instruction:' header under load:\n%s", userPrompt)
	}
}

func TestGateLocateCard_And_CompactMicroCards(t *testing.T) {
	locCard := GateLocateCard{
		IssueSummary: "QuerySet.union() with values_list() crashes when ordering by annotated field",
		Traceback:    "django/db/models/sql/compiler.py:612 in get_order_by",
		VLNHint:      "[django.db.sql.compiler.order_by] Guard col_cnt bounds in get_combinator_sql",
		Candidates: []GateLocateCandidate{
			{File: "django/db/models/sql/compiler.py", Symbol: "get_order_by", LineRange: [2]int{590, 645}, Score: 42.5},
			{File: "django/db/models/query.py", Symbol: "union", LineRange: [2]int{1120, 1145}, Score: 21.0},
		},
		CompactMode: true,
	}
	sysP, userP, err := locCard.FormatPrompt()
	if err != nil {
		t.Fatalf("GateLocateCard.FormatPrompt failed: %v", err)
	}
	if !strings.HasPrefix(sysP, "[OC:LOCATE]") {
		t.Fatalf("expected [OC:LOCATE] header, got: %s", sysP)
	}
	if !strings.Contains(userP, "VLN:") || !strings.Contains(userP, "compiler.py") {
		t.Fatalf("expected VLN hint and candidate in user prompt, got:\n%s", userP)
	}

	editCard := GateEditCard{
		FilePath:       "django/db/models/sql/compiler.py",
		Instruction:    "Guard combinator order_by index bounds",
		ContextSnippet: "def get_order_by(self):\n    return []",
		VLNHint:        "[django.db.sql.compiler.order_by] Guard col_cnt bounds",
		CompactMode:    true,
	}
	eSys, eUser, err := editCard.FormatPrompt()
	if err != nil {
		t.Fatalf("GateEditCard.FormatPrompt failed: %v", err)
	}
	if !strings.HasPrefix(eSys, "[OC:EDIT]") {
		t.Fatalf("expected [OC:EDIT] header, got: %s", eSys)
	}
	if !strings.Contains(eUser, "VLN:") {
		t.Fatalf("expected VLN hint in edit user prompt, got:\n%s", eUser)
	}

	healCard := GateHealCard{
		FilePath:      "django/db/models/sql/compiler.py",
		ErrorLine:     612,
		ErrorMessage:  "IndexError: tuple index out of range",
		Snippet5Lines: "def get_order_by(self):\n    return []",
		VLNHint:       "[django.db.sql.compiler.order_by] Guard col_cnt bounds",
		CompactMode:   true,
	}
	hSys, hUser, err := healCard.FormatPrompt()
	if err != nil {
		t.Fatalf("GateHealCard.FormatPrompt failed: %v", err)
	}
	if !strings.HasPrefix(hSys, "[OC:HEAL]") {
		t.Fatalf("expected [OC:HEAL] header, got: %s", hSys)
	}
	if !strings.Contains(hUser, "VLN:") {
		t.Fatalf("expected VLN: in heal user prompt, got:\n%s", hUser)
	}
}


