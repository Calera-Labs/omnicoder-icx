// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// ============================================================================
// 1. TABS VS SPACES & MIXED INDENTATION IN ReplaceFuzzy (TIER 3)
// ============================================================================

func TestAdversarial_ReplaceFuzzy_MixedTabsAndSpaces(t *testing.T) {
	// Scenario A: Original code has tabs, search block has spaces.
	origTabs := "func Process() {\n\tif true {\n\t\tstepOne()\n\t\tstepTwo()\n\t}\n}\n"
	searchSpaces := "    if true {\n        stepOne()\n        stepTwo()\n    }"
	replaceSpaces := "    if true {\n        stepUnified()\n    }"

	res, added, removed, err := devops.ReplaceFuzzy(origTabs, searchSpaces, replaceSpaces, true)
	t.Logf("Scenario A (Tabs in orig, Spaces in search):\nerr: %v\nres:\n%s\nadded=%d, removed=%d",
		err, res, added, removed)

	if err != nil {
		t.Errorf("ReplaceFuzzy failed to match tabbed code with spaced search: %v", err)
	} else {
		if !strings.Contains(res, "stepUnified()") {
			t.Errorf("Replacement not applied cleanly in tabbed code:\n%s", res)
		}
	}

	// Scenario B: Original code has 1 TAB, Search block has 1 TAB but different inner indentation
	origTabInner := "\tfunc Sub() {\n\t\tval := 1\n\t}\n"
	searchTabInner := "func Sub() {\n\tval := 1\n}"
	replaceTabInner := "func Sub() {\n\tval := 42\n}"

	resB, _, _, errB := devops.ReplaceFuzzy(origTabInner, searchTabInner, replaceTabInner, true)
	t.Logf("Scenario B (Tabbed outer, tabbed inner):\nerr: %v\nres:\n%s", errB, resB)
	if errB != nil {
		t.Errorf("Tier 3 failed on tabbed outer vs unindented search: %v", errB)
	}

	// Scenario D: Indentation Preservation on Tabbed Code.
	// When original code is indented with tabs (e.g. 3 tabs) and search/replace is unindented,
	// Tier 3 should re-indent the replacement to the candidate indentation.
	orig3Tabs := "func Nested() {\n\t\t\tval := 1\n\t\t\treturn val\n}\n"
	searchUnindented := "val := 1\nreturn val"
	replaceUnindented := "val := 2\nreturn val"

	resD, _, _, errD := devops.ReplaceFuzzy(orig3Tabs, searchUnindented, replaceUnindented, true)
	t.Logf("Scenario D (Indentation preservation on tabs):\nerr: %v\nres:\n%s", errD, resD)
	if errD == nil {
		if strings.Contains(resD, "\nval := 2") {
			t.Errorf("Expected 3-tab indentation to be preserved:\nResult:\n%s", resD)
		}
	}
}

func TestAdversarial_ReplaceFuzzy_DeeplyNestedWhitespace(t *testing.T) {
	// 40 spaces deep (10 levels of 4-space nesting)
	indent40 := strings.Repeat(" ", 40)
	indent44 := strings.Repeat(" ", 44)
	orig := fmt.Sprintf("%sfor i := 0; i < 10; i++ {\n%sdoWork(i)\n%s}\n", indent40, indent44, indent40)

	// Search block with only 4 spaces
	search := "for i := 0; i < 10; i++ {\n    doWork(i)\n}"
	replace := "for i := 0; i < 10; i++ {\n    doWorkOptimized(i)\n}"

	res, _, _, err := devops.ReplaceFuzzy(orig, search, replace, true)
	if err != nil {
		t.Fatalf("Deeply nested whitespace (40 spaces) match failed: %v", err)
	}

	// Replacement should preserve the 40-space base + 4-space inner nesting
	expectedInner := strings.Repeat(" ", 44) + "doWorkOptimized(i)"
	if !strings.Contains(res, expectedInner) {
		t.Errorf("Deep nesting did not preserve candidate indentation:\n%s", res)
	}
}

// ============================================================================
// 2. WINDOWS CRLF VS UNIX LF COMPREHENSIVE STRESS TEST
// ============================================================================

func TestAdversarial_ReplaceFuzzy_WindowsCRLFCombinations(t *testing.T) {
	crlf := "\r\n"
	lf := "\n"

	baseContentLines := []string{
		"package test",
		"func Calculate(a, b int) int {",
		"    // addition",
		"    sum := a + b",
		"    return sum",
		"}",
	}

	searchLines := []string{
		"    // addition",
		"    sum := a + b",
		"    return sum",
	}

	replaceLines := []string{
		"    // multiplication",
		"    product := a * b",
		"    return product",
	}

	combinations := []struct {
		name       string
		contentSep string
		searchSep  string
		replaceSep string
	}{
		{"All_CRLF", crlf, crlf, crlf},
		{"Content_CRLF_Search_LF", crlf, lf, lf},
		{"Content_LF_Search_CRLF", lf, crlf, lf},
		{"Content_CRLF_Replace_CRLF", crlf, lf, crlf},
		{"Mixed_Line_Endings", "mixed", lf, lf},
	}

	for _, comb := range combinations {
		t.Run(comb.name, func(t *testing.T) {
			var content string
			if comb.contentSep == "mixed" {
				// Alternating CRLF and LF
				content = baseContentLines[0] + crlf +
					baseContentLines[1] + lf +
					baseContentLines[2] + crlf +
					baseContentLines[3] + lf +
					baseContentLines[4] + crlf +
					baseContentLines[5] + lf
			} else {
				content = strings.Join(baseContentLines, comb.contentSep)
			}

			search := strings.Join(searchLines, comb.searchSep)
			replace := strings.Join(replaceLines, comb.replaceSep)

			res, added, removed, err := devops.ReplaceFuzzy(content, search, replace, true)
			if err != nil {
				t.Fatalf("combination %s failed: %v", comb.name, err)
			}

			if !strings.Contains(res, "product := a * b") {
				t.Errorf("combination %s: replacement text missing from result:\n%s", comb.name, res)
			}
			if strings.Contains(res, "sum := a + b") {
				t.Errorf("combination %s: search text was not removed from result:\n%s", comb.name, res)
			}
			if added != 3 || removed != 3 {
				t.Errorf("combination %s: line count mismatch: added=%d, removed=%d", comb.name, added, removed)
			}
		})
	}
}

// ============================================================================
// 3. NON-SIGNIFICANT BLANK LINES (TIER 4) STRESS TEST
// ============================================================================

func TestAdversarial_ReplaceFuzzy_BlankLinePermutations(t *testing.T) {
	// Original content with multiple random blank lines and lines with only spaces/tabs
	orig := `
func ComplexRoutine() {

    initResources()


    // intermediate comment
	
    executeTasks()


    teardown()

}
`
	// Search block with zero blank lines
	searchNoBlanks := `func ComplexRoutine() {
    initResources()
    // intermediate comment
    executeTasks()
    teardown()
}`

	replace := `func ComplexRoutine() {
    runAllUnified()
}`

	res, _, _, err := devops.ReplaceFuzzy(orig, searchNoBlanks, replace, true)
	if err != nil {
		t.Fatalf("Tier 4 non-significant blank lines failed: %v", err)
	}

	if !strings.Contains(res, "runAllUnified()") {
		t.Errorf("Tier 4 replacement failed to apply:\n%s", res)
	}
	if strings.Contains(res, "initResources()") || strings.Contains(res, "executeTasks()") {
		t.Errorf("Tier 4 old code still present:\n%s", res)
	}
}

// ============================================================================
// 4. BOUNDARY CONDITIONS & EDGE CASES (EMPTY STRINGS, MASSIVE STRINGS)
// ============================================================================

func TestAdversarial_ReplaceFuzzy_BoundaryConditions(t *testing.T) {
	// 1. Search text not in content (fuzzy match enabled) -> must fail cleanly with FUZZY_SEARCH_MISMATCH
	_, _, _, err := devops.ReplaceFuzzy("content line", "completely different", "new", true)
	if err == nil || !strings.Contains(err.Error(), "FUZZY_SEARCH_MISMATCH") {
		t.Errorf("expected FUZZY_SEARCH_MISMATCH, got: %v", err)
	}

	// 2. Empty search text -> should not panic
	res, _, _, err := devops.ReplaceFuzzy("existing content", "", "prepended ", true)
	if err != nil {
		t.Errorf("empty search text failed: %v", err)
	}
	if !strings.HasPrefix(res, "prepended ") {
		t.Errorf("expected prepended string, got: %s", res)
	}

	// 3. Empty content
	_, _, _, err = devops.ReplaceFuzzy("", "search", "replace", true)
	if err == nil {
		t.Errorf("expected error when searching in empty content")
	}

	// 4. Massive content (10,000 lines) with search block near the end
	var sb strings.Builder
	for i := 0; i < 10000; i++ {
		sb.WriteString(fmt.Sprintf("// line %d\n", i))
	}
	sb.WriteString("func TargetNearEnd() { return }\n")
	for i := 0; i < 10; i++ {
		sb.WriteString(fmt.Sprintf("// trailing %d\n", i))
	}
	bigContent := sb.String()

	bigSearch := "func TargetNearEnd() { return }"
	bigReplace := "func TargetNearEnd() { return true }"
	resBig, _, _, err := devops.ReplaceFuzzy(bigContent, bigSearch, bigReplace, true)
	if err != nil {
		t.Fatalf("ReplaceFuzzy failed on 10,000 line content: %v", err)
	}
	if !strings.Contains(resBig, "return true") {
		t.Errorf("ReplaceFuzzy did not replace target in 10,000 line content")
	}
}

// ============================================================================
// 6. MASSIVE BATCH & REPEATED PATTERNS
// ============================================================================

func TestAdversarial_ReplaceFuzzy_RepeatedPatterns(t *testing.T) {
	// Content with 50 identical blocks
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString(fmt.Sprintf("// block %d\nfunc Handler() string {\n    return \"default\"\n}\n", i))
	}
	content := sb.String()

	search := "func Handler() string {\n    return \"default\"\n}"
	replace := "func Handler() string {\n    return \"updated\"\n}"

	res, added, removed, err := devops.ReplaceFuzzy(content, search, replace, true)
	if err != nil {
		t.Fatalf("ReplaceFuzzy failed on repeated blocks: %v", err)
	}

	// Should replace the FIRST occurrence only
	if !strings.Contains(res, "// block 0\nfunc Handler() string {\n    return \"updated\"\n}") {
		t.Errorf("expected first occurrence to be replaced")
	}
	// Subsequent occurrences should remain untouched
	if !strings.Contains(res, "// block 1\nfunc Handler() string {\n    return \"default\"\n}") {
		t.Errorf("subsequent occurrences should remain default")
	}
	if added != 3 || removed != 3 {
		t.Errorf("line count mismatch: added=%d, removed=%d", added, removed)
	}
}

// ============================================================================
// 7. CONCURRENCY & DATA RACE STRESS TEST ON ReplaceFuzzy
// ============================================================================

func TestAdversarial_ReplaceFuzzy_ConcurrencyRace(t *testing.T) {
	const goroutines = 50
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for iter := 0; iter < iterations; iter++ {
				content := fmt.Sprintf("func Worker%d() {\n\tstepA()\n\tstepB()\n}\n", gid)
				search := fmt.Sprintf("func Worker%d() {\n    stepA()\n    stepB()\n}", gid)
				replace := fmt.Sprintf("func Worker%d() {\n    stepUnified(%d)\n}", gid, iter)

				_, _, _, _ = devops.ReplaceFuzzy(content, search, replace, true)
			}
		}(g)
	}

	wg.Wait()
}
