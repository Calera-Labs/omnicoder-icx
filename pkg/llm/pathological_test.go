// SPDX-License-Identifier: Apache-2.0
package llm

import (
	"fmt"
	"strings"
	"testing"
)

func TestPathologicalPaths_SanitizeRibbonRoot(t *testing.T) {
	testPaths := []struct {
		name string
		path string
	}{
		{"Empty string", ""},
		{"Whitespace only", "   \t\n  "},
		{"file_ prefix only", "file_"},
		{"file_ with whitespace", "file_   "},
		{"Dot only", "."},
		{"Slash only", "/"},
		{"Backslash only", "\\"},
		{"Double dot only", ".."},
		{"Multiple dots", "......."},
		{"Multiple slashes", "////"},
		{"Trailing slash", "foo/bar/"},
		{"Trailing dot slash", "foo/bar/."},
		{"Trailing dot dot slash", "foo/bar/.."},
		{"Relative path traversal", "../../../../../../../../etc/passwd"},
		{"Windows drive path", "C:\\Workspace\\Project\\deep\\nested\\file.go"},
		{"1000+ char repetitive path", strings.Repeat("sub_dir_name/", 100) + "main.go"},
		{"1000+ char single token without slash", strings.Repeat("LongSymbolWithoutAnySlash", 50)},
		{"5000+ char massive path", strings.Repeat("a/", 2500) + "end.go"},
		{"Unicode Cyrillic path", "директория/поддиректория/файл_компонента.go"},
		{"Unicode CJK path", "项目/核心模块/分布式存储/事务管理器.go"},
		{"Unicode Arabic RTL path", "مسار/مجلد/ملف_المشروع.go"},
		{"Unicode Emoji rich path", "🚀/⚡/🔥/💻/microservice_📦.go"},
		{"Unicode multi-byte boundary cut", strings.Repeat("a", 35) + "日本語.go"},
		{"Unicode 4-byte emoji boundary cut", strings.Repeat("b", 35) + "🚀🚀🚀.go"},
		{"Mixed dots and slashes", "./././..//./../..././foo.go"},
		{"Spaces inside path segments", "My Workspace/Sample Project/src/app.go"},
		{"Special characters in path", "foo-bar_baz.v1+build.2@service#header$value%20.go"},
		{"Quotes and brackets in path", "dir[\"sub\"]/'quote'/`backtick`/file.go"},
		{"Newlines and tabs in path", "dir\nwith\nnewlines/\tfile\t.go"},
	}

	for _, tc := range testPaths {
		t.Run(tc.name, func(t *testing.T) {
			// Test SanitizeRibbonRoot directly
			sanitized := SanitizeRibbonRoot(tc.path, 40)
			if sanitized == "" {
				t.Errorf("SanitizeRibbonRoot returned empty string for %q", tc.path)
			}
			if len(sanitized) > 60 { // allow slight buffer for prefix .../ but should stay bounded
				t.Errorf("SanitizeRibbonRoot returned length %d > 60 for %q: %s", len(sanitized), tc.path, sanitized)
			}

			// Test FormatASTRibbonFromStrings with this root
			items := []string{"NodeA (function:defines)", "NodeB (type:calls)", "NodeC (interface:implements)"}
			ribbon := FormatASTRibbonFromStrings(tc.path, items, 150)
			tokens := EstimateTokens(ribbon)
			if tokens >= 160 {
				t.Errorf("VIOLATION: FormatASTRibbonFromStrings produced %d tokens >= 160 cap for %q:\n%s", tokens, tc.path, ribbon)
			}
			if !strings.HasSuffix(ribbon, "/>") {
				t.Errorf("ribbon does not close with '/>' for %q: %s", tc.path, ribbon)
			}

			// Test FormatASTRibbonFromStrings with empty items
			ribbonEmpty := FormatASTRibbonFromStrings(tc.path, nil, 150)
			tokensEmpty := EstimateTokens(ribbonEmpty)
			if tokensEmpty >= 160 {
				t.Errorf("VIOLATION: empty items ribbon produced %d tokens >= 160 cap for %q:\n%s", tokensEmpty, tc.path, ribbonEmpty)
			}

			// Test GateEditCard with this pathological path under heavy instruction load
			card := GateEditCard{
				FilePath:       tc.path,
				Instruction:    strings.Repeat("Refactor this method to strictly obey token budgeting. ", 20), // ~150 tokens
				ContextSnippet: "func Process() error { return nil }",
				ASTRibbon:      ribbon,
			}

			sysPrompt, userPrompt, err := card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt failed for %q: %v", tc.path, err)
			}

			totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
			if totalTokens >= MaxCardTokenBudget {
				t.Errorf("VIOLATION: GateEditCard totalTokens %d >= 300 for path %q\nSys: %s\nUser: %s",
					totalTokens, tc.path, sysPrompt, userPrompt)
			}

			// Invariant: File: and Instruction: headers MUST NEVER be stripped
			if !strings.Contains(userPrompt, "File: ") {
				t.Errorf("PROMPT CORRUPTION: userPrompt lost 'File: ' header for %q:\n%s", tc.path, userPrompt)
			}
			if !strings.Contains(userPrompt, "Instruction: ") {
				t.Errorf("PROMPT CORRUPTION: userPrompt lost 'Instruction: ' header for %q:\n%s", tc.path, userPrompt)
			}
		})
	}
}

func TestGateEditCard_VaryingInstructionLengths_HeaderPreservation(t *testing.T) {
	// Sweep instruction length across wide range to catch any off-by-one or edge-case clamping failure
	lengths := []int{0, 1, 5, 20, 50, 100, 200, 300, 500, 1000, 2000, 5000, 10000, 25000}

	for _, length := range lengths {
		name := fmt.Sprintf("Instruction length %d", length)
		t.Run(name, func(t *testing.T) {
			instruction := strings.Repeat("X", length)
			if length > 20 {
				instruction = strings.Repeat("word ", length/5)
			}

			card := GateEditCard{
				FilePath:       "pkg/controller/handler.go",
				Instruction:    instruction,
				ContextSnippet: strings.Repeat("func LongCodeSnippet() {\n\tprintln(\"test\")\n}\n", 50),
				ASTRibbon:      "<nav:ribbon: root=\"handler.go\" hops=5 | d0:handler.go(file):root > d1:Serve(function):defines />",
			}

			sysPrompt, userPrompt, err := card.FormatPrompt()
			if err != nil {
				t.Fatalf("FormatPrompt failed: %v", err)
			}

			totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
			if totalTokens >= MaxCardTokenBudget {
				t.Fatalf("VIOLATION: totalTokens %d >= 300 at instruction length %d\nUser: %s", totalTokens, length, userPrompt)
			}

			if !strings.Contains(userPrompt, "File: ") {
				t.Fatalf("PROMPT CORRUPTION: lost 'File: ' header at length %d:\n%s", length, userPrompt)
			}
			if !strings.Contains(userPrompt, "Instruction: ") {
				t.Fatalf("PROMPT CORRUPTION: lost 'Instruction: ' header at length %d:\n%s", length, userPrompt)
			}
		})
	}
}

func TestGateEditCard_ExtremeASTNeighborhood_PrecedenceAndBudget(t *testing.T) {
	// What happens if ASTNeighborhood has 1500 items, and no pre-computed ribbon?
	items := make([]string, 1500)
	for i := 0; i < 1500; i++ {
		items[i] = fmt.Sprintf("Symbol_%04d_WithExtendedSignatureAndContext_%d (defines)", i, i)
	}

	card := GateEditCard{
		FilePath:        "path/to/extreme/component.go",
		Instruction:     "Refactor and optimize AST processing pipeline",
		ContextSnippet:  "func Run() {}",
		ASTNeighborhood: items,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}

	totalTokens := EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens >= MaxCardTokenBudget {
		t.Fatalf("VIOLATION: total tokens %d >= 300 with 1500 ASTNeighborhood items", totalTokens)
	}

	if !strings.Contains(userPrompt, "File: ") || !strings.Contains(userPrompt, "Instruction: ") {
		t.Fatalf("PROMPT CORRUPTION: lost header:\n%s", userPrompt)
	}
	if !strings.Contains(userPrompt, "AST: ") {
		t.Fatalf("AST header missing when 1500 neighborhood items were provided:\n%s", userPrompt)
	}
}
