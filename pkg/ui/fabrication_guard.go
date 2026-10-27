// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// fabricationTermRegex flags code that invents data ("synthetic records", "mock corpus", ...).
	fabricationTermRegex = regexp.MustCompile(`(?i)\b(synthetic|supplementary|simulated|fabricated|mock|dummy|fake)\b[^\n]{0,60}\b(records?|corpus|partitions?|ledger|counts?|rows?|dataset)\b`)
	// fabricationMatchCountRegex flags code that pads data so totals "match" an expected number.
	fabricationMatchCountRegex = regexp.MustCompile(`(?i)\b(match(es|ing)?|equal(s)?|reach(es|ing)?)\b\s+(the\s+)?(expected|ledger|icx|vault)\b[^\n]{0,40}\b(count|counts|total|size)\b`)
	// hardcodedLedgerRegex flags literal ("Source", 500, "synced...") style hardcoded harvest counts.
	hardcodedLedgerRegex = regexp.MustCompile(`\(\s*["'][^"']+["']\s*,\s*\d{2,}\s*,\s*["'](synced|restored|local)[a-z_]*["']\s*\)`)
)

// isFixturePath reports whether a path is a test/seed/fixture file, where sample data is legitimate.
func isFixturePath(p string) bool {
	l := strings.ToLower(filepath.ToSlash(p))
	base := filepath.Base(l)
	return strings.Contains(l, "/tests/") || strings.HasPrefix(l, "tests/") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.go") ||
		strings.Contains(base, "seed") || strings.Contains(base, "fixture") ||
		strings.Contains(l, "/testdata/") || strings.Contains(l, "/examples/")
}

// generatedRecordLoopRegex flags loops over range() that INSERT rows (bulk-generated records).
var generatedRecordLoopRegex = regexp.MustCompile(`(?is)for\s+\w+\s+in\s+range\([^)]*\)\s*:.{0,800}?INSERT\b`)

// placeholderRecordRegex flags templated fake record titles, authors and URLs.
var placeholderRecordRegex = regexp.MustCompile(`(?i)(item|record|entry|document|article|row)\s*#\s*(\{\w+\}|\d+|i\b)|example\.(org|com)/[^"'\s]*(record|item|doc)_?\{?\w*\}?`)

// isTestPath reports whether a path is an actual test file, where loops and fake inputs are legitimate.
func isTestPath(p string) bool {
	l := strings.ToLower(filepath.ToSlash(p))
	base := filepath.Base(l)
	return strings.Contains(l, "/tests/") || strings.HasPrefix(l, "tests/") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, "_test.py") || strings.Contains(l, "/testdata/")
}

var (
	// negationPrefixRegex detects negative polarity words that precede a term (e.g. "without hardcoded fake counts", "avoid synthetic records").
	negationPrefixRegex = regexp.MustCompile(`(?i)\b(without|no|remove|removing|removed|avoid|avoiding|prevent|preventing|replace|replacing|replaced|delete|deleting|deleted|eliminate|eliminating|eliminated|instead\s+of|free\s+of)\s+([a-z0-9_-]+\s+){0,3}$`)
)

func isNegatedMatch(text string, start int) bool {
	lineStart := strings.LastIndex(text[:start], "\n")
	var linePrefix string
	if lineStart == -1 {
		linePrefix = text[:start]
	} else {
		linePrefix = text[lineStart+1 : start]
	}
	return negationPrefixRegex.MatchString(linePrefix)
}

func newFabricationMatch(re *regexp.Regexp, search, replace string) string {
	indices := re.FindAllStringIndex(replace, -1)
	for _, loc := range indices {
		if isNegatedMatch(replace, loc[0]) {
			continue
		}
		m := replace[loc[0]:loc[1]]
		if strings.Count(normalizeWS(replace), normalizeWS(m)) > strings.Count(normalizeWS(search), normalizeWS(m)) {
			return m
		}
	}
	return ""
}

// detectFabricatedData detects edits that invent records or hardcode harvest counts instead of
// fixing the real data pipeline. Test files are fully exempt. Seed, fixture and example files are
// exempt from the wording checks (small hand-written samples are fine) but never from bulk-generated
// or templated placeholder records.
func detectFabricatedData(targetPath, content string) (bool, string) {
	if isTestPath(targetPath) {
		return false, ""
	}
	// Judge only what the edit introduces: a patch whose SEARCH side contains hardcoded counts and
	// whose REPLACE side removes them is the fix, not a fabrication.
	search, replace := editSides(targetPath, content)
	if !isFixturePath(targetPath) {
		for _, re := range []*regexp.Regexp{fabricationTermRegex, fabricationMatchCountRegex, hardcodedLedgerRegex} {
			if m := newFabricationMatch(re, search, replace); m != "" {
				return true, strings.TrimSpace(m)
			}
		}
	}
	if m := newMatch(placeholderRecordRegex, search, replace); m != "" {
		return true, strings.TrimSpace(m)
	}
	if newMatch(generatedRecordLoopRegex, search, replace) != "" {
		return true, "bulk-generated INSERT loop"
	}
	return false, ""
}

// listTestFiles returns up to max Python/Go/JS test files found under dir, relative to dir.
func listTestFiles(dir string, max int) []string {
	var found []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "__pycache__", "venv", ".venv", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		n := strings.ToLower(d.Name())
		if strings.HasPrefix(n, "test_") && strings.HasSuffix(n, ".py") ||
			strings.HasSuffix(n, "_test.py") || strings.HasSuffix(n, "_test.go") ||
			strings.HasSuffix(n, ".test.js") || strings.HasSuffix(n, ".test.ts") {
			if rel, e := filepath.Rel(dir, p); e == nil {
				found = append(found, filepath.ToSlash(rel))
			}
			if len(found) >= max {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}
