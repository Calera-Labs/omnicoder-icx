// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

var (
	// swallowedExceptRegex flags `except ...: pass` (Python) which hides real failures.
	swallowedExceptRegex = regexp.MustCompile(`(?m)except\b[^\n:]*:[ \t]*(#[^\n]*)?\n?[ \t]*pass\b`)
	// seederTriggerRegex flags new code that imports or calls a seeding routine.
	seederTriggerRegex = regexp.MustCompile(`(?i)(from\s+[\w.]*seed[\w.]*\s+import|import\s+[\w.]*seed\w*|\bseed_\w+\s*\()`)
	// dataBugPromptRegex recognises prompts describing data/DB/count discrepancies.
	dataBugPromptRegex = regexp.MustCompile(`(?i)\b(data|database|db|records?|rows?|count|counts|ledger|sqlite|table|only (shows?|displays?)|missing)\b`)
	// refactorOrBuildPromptRegex matches prompts that request refactoring, building, migrating,
	// or implementing features/architectures rather than diagnosing an existing bug.
	refactorOrBuildPromptRegex = regexp.MustCompile(`(?i)(^\s*refactor\b|\b(refactor|rewrite|migrate|redesign)\b)`)
	// dataLayerPathRegex matches files that write/shape persisted data.
	dataLayerPathRegex = regexp.MustCompile(`(?i)(^|/)(db|database|models?|schema|migrations?|seed\w*|sync\w*|scrapers?/[^/]+|ingest\w*|store|repository)\.(py|go|js|ts|sql)$|/(scrapers?|migrations?)/`)
)

// isDataBugInvestigation reports whether the user prompt describes an existing defect, discrepancy,
// or missing data that requires investigation, rather than an active engineering/refactoring task.
func isDataBugInvestigation(prompt string) bool {
	if refactorOrBuildPromptRegex.MatchString(prompt) {
		return false
	}
	return dataBugPromptRegex.MatchString(prompt)
}

// addedText returns only the text being added: the REPLACE side of aider blocks, else the content.
func addedText(content string) string {
	if !(strings.Contains(content, "<<<<<<< SEARCH") && strings.Contains(content, ">>>>>>> REPLACE")) {
		return content
	}
	var b strings.Builder
	rest := content
	for {
		i := strings.Index(rest, "=======")
		if i < 0 {
			break
		}
		after := rest[i+len("======="):]
		j := strings.Index(after, ">>>>>>> REPLACE")
		if j < 0 {
			break
		}
		b.WriteString(after[:j])
		b.WriteString("\n")
		rest = after[j+len(">>>>>>> REPLACE"):]
	}
	return b.String()
}

// editSides returns the concatenated SEARCH (removed/context) and REPLACE (resulting) text of aider
// blocks. For whole-file content (no blocks) search is "" and replace is the content.
func editSides(targetPath, content string) (search, replace string) {
	if !(strings.Contains(content, "<<<<<<< SEARCH") && strings.Contains(content, ">>>>>>> REPLACE")) {
		return "", content
	}
	blocks, err := devops.ParseAiderEditBlocks(content, targetPath)
	if err != nil || len(blocks) == 0 {
		return "", addedText(content)
	}
	var s, r strings.Builder
	for _, b := range blocks {
		s.WriteString(b.SearchText + "\n")
		r.WriteString(b.ReplaceText + "\n")
	}
	return s.String(), r.String()
}

// newMatch returns the first match of re that the edit INTRODUCES: a match occurring more often in the
// replace side than in the search side. Pre-existing matches kept as context, or removed, don't count.
func newMatch(re *regexp.Regexp, search, replace string) string {
	for _, m := range re.FindAllString(replace, -1) {
		if strings.Count(normalizeWS(replace), normalizeWS(m)) > strings.Count(normalizeWS(search), normalizeWS(m)) {
			return m
		}
	}
	return ""
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// detectUnsafePatch flags edits that add swallowed exceptions or trigger a seeder, both of which
// mask the real defect. Test and fixture files are exempt. Only newly introduced code is judged.
func detectUnsafePatch(targetPath, content string) (bool, string) {
	if isTestPath(targetPath) {
		return false, ""
	}
	search, replace := editSides(targetPath, content)
	if newMatch(swallowedExceptRegex, search, replace) != "" {
		return true, "it adds a swallowed exception (`except ...: pass`), which hides the real failure; log or re-raise it."
	}
	if !isFixturePath(targetPath) && newMatch(seederTriggerRegex, search, replace) != "" {
		return true, "it wires a seeder into runtime code, which can mask missing real data with sample data; fix the pipeline that should produce the data."
	}
	return false, ""
}

// needsRuntimeProbe reports whether an edit to targetPath should wait for runtime evidence.
func needsRuntimeProbe(prompt, targetPath string) bool {
	if isTestPath(targetPath) {
		return false
	}
	if !isDataBugInvestigation(prompt) {
		return false
	}
	return dataLayerPathRegex.MatchString(strings.ToLower(filepath.ToSlash(targetPath)))
}
