// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// 1. Evidence warnings: annotate files the architect reads when they fabricate data or hide errors.
// ---------------------------------------------------------------------------------------------

// printedOnlyExceptRegex flags handlers that only print the error, so callers cannot tell a write failed.
var printedOnlyExceptRegex = regexp.MustCompile(`(?m)except\b[^\n:]*:[ \t]*\n[ \t]*print\([^\n]*\)[ \t]*\n(?:[ \t]*finally\b|[ \t]*\S)`)

type evidenceFinding struct {
	Line       int
	Message    string
	Fabricated bool
	Kind       string // "rows", "counts", "inflate" or ""
}

// evidenceFlags records which kinds of misleading data the architect has been shown.
type evidenceFlags struct {
	FabricatedRows  bool // generated/placeholder records
	HardcodedCounts bool // literal ledger/harvest numbers
	InflatedCounts  bool // counters that count attempts, not rows actually stored
}

func (f evidenceFlags) any() bool { return f.FabricatedRows || f.HardcodedCounts || f.InflatedCounts }

func (f evidenceFlags) merge(o evidenceFlags) evidenceFlags {
	return evidenceFlags{f.FabricatedRows || o.FabricatedRows, f.HardcodedCounts || o.HardcodedCounts, f.InflatedCounts || o.InflatedCounts}
}

func lineOf(content string, idx int) int {
	return strings.Count(content[:idx], "\n") + 1
}

var (
	// writeCallStmtRegex matches a bare statement calling a write routine (return value discarded).
	writeCallStmtRegex = regexp.MustCompile(`^(?:\w+\.)*(insert\w*|save\w*|upsert\w*|store_\w*|add_record\w*|write_record\w*|persist\w*)\s*\(`)
	counterIncRegex    = regexp.MustCompile(`^\w+\s*\+=\s*1\s*(#.*)?$`)
)

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " \t")) }

// uncheckedCounterLines returns 1-based lines of `n += 1` counters that follow a write call whose
// result is discarded at the same block level: the counter counts attempts, not stored rows.
func uncheckedCounterLines(content string) []int {
	lines := strings.Split(content, "\n")
	var out []int
	for i, l := range lines {
		if !writeCallStmtRegex.MatchString(strings.TrimSpace(l)) {
			continue
		}
		ind := indentOf(l)
		for j := i + 1; j < len(lines) && j <= i+15; j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" {
				continue
			}
			if indentOf(lines[j]) < ind {
				break
			}
			if indentOf(lines[j]) == ind && counterIncRegex.MatchString(t) {
				out = append(out, j+1)
				break
			}
		}
	}
	return out
}

// scanEvidenceFindings inspects full file content for fabricated-data and error-hiding patterns.
func scanEvidenceFindings(relPath, content string) []evidenceFinding {
	if isTestPath(relPath) {
		return nil
	}
	var out []evidenceFinding
	add := func(re *regexp.Regexp, msg string, kind string, max int) {
		for i, loc := range re.FindAllStringIndex(content, -1) {
			if i >= max {
				break
			}
			out = append(out, evidenceFinding{Line: lineOf(content, loc[0]), Message: msg, Fabricated: kind == "rows" || kind == "counts", Kind: kind})
		}
	}
	add(generatedRecordLoopRegex, "bulk-generated INSERT loop: rows written here are programmatically invented, not harvested", "rows", 2)
	add(placeholderRecordRegex, "templated placeholder record (e.g. \"Item #{i}\", example.org URL): not real source data", "rows", 2)
	add(hardcodedLedgerRegex, "hardcoded harvest/ledger count: this number is a literal, not a measurement of stored rows", "counts", 6)
	if !isFixturePath(relPath) {
		add(fabricationTermRegex, "code describes its own data as synthetic/mock/simulated", "rows", 2)
	}
	for i, ln := range uncheckedCounterLines(content) {
		if i >= 2 {
			break
		}
		out = append(out, evidenceFinding{Line: ln, Kind: "inflate",
			Message: "counter incremented whether or not the preceding write stored anything (its result is discarded): logged harvest counts count ATTEMPTS and can exceed rows actually stored"})
	}
	if loc := insertOrIgnoreRegex.FindStringIndex(content); loc != nil && !strings.Contains(content, "rowcount") && !strings.Contains(strings.ToLower(content), "changes()") {
		out = append(out, evidenceFinding{Line: lineOf(content, loc[0]), Kind: "inflate",
			Message: "INSERT OR IGNORE without checking cursor.rowcount: duplicate/rejected rows are skipped silently and the caller cannot tell nothing was stored"})
	}
	add(swallowedExceptRegex, "swallowed exception (`except: pass`): failures here are invisible", "", 3)
	add(printedOnlyExceptRegex, "exception is only printed: callers cannot tell the operation failed (counts may include failed writes)", "", 3)
	return out
}

var insertOrIgnoreRegex = regexp.MustCompile(`(?i)INSERT\s+OR\s+IGNORE`)

// evidenceWarningsForRead returns an engine warning block for a file the architect read, and whether
// any finding indicates fabricated data. Findings cover the WHOLE file, not just the excerpt shown.
func evidenceWarningsForRead(ws, activeProject string, a *ActionIntent) (string, evidenceFlags) {
	var flags evidenceFlags
	full, err := resolveReadPath(ws, activeProject, a.Path())
	if err != nil {
		return "", flags
	}
	info, err := os.Stat(full)
	if err != nil || info.Size() > readIngestMaxBytes {
		return "", flags
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", flags
	}
	rel, _ := filepath.Rel(ws, full)
	rel = filepath.ToSlash(rel)
	findings := scanEvidenceFindings(rel, strings.ReplaceAll(string(data), "\r\n", "\n"))
	if len(findings) == 0 {
		return "", flags
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[ENGINE WARNING] %s (whole-file scan):\n", rel)
	for _, f := range findings {
		fmt.Fprintf(&b, "  - L%d: %s\n", f.Line, f.Message)
		switch f.Kind {
		case "rows":
			flags.FabricatedRows = true
		case "counts":
			flags.HardcodedCounts = true
		case "inflate":
			flags.InflatedCounts = true
		}
	}
	if flags.FabricatedRows || flags.HardcodedCounts {
		b.WriteString("  Rows or counts produced by this code are NOT real data. Any row count that includes them is inflated; say so in your root cause and never treat them as harvested records.\n")
	}
	if flags.InflatedCounts {
		b.WriteString("  Counts logged by this code are NOT proof that rows were stored. Compare them against real rows (sample a few rows per source and check their URLs/IDs) before trusting any total.\n")
	}
	return b.String(), flags
}

// ---------------------------------------------------------------------------------------------
// 2. Symptom mismatch: the probe contradicts the count the user reported.
// ---------------------------------------------------------------------------------------------

var (
	reportedCountRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bonly\s+(?:show(?:s|ing)?|display(?:s|ing)?|returns?|returning|has|have|lists?|listing|loads?|loading)\s+(\d{1,6})\b`),
		regexp.MustCompile(`(?i)\bonly\s+(\d{1,6})\s+(?:records?|rows?|sources?|items?|results?|entries)\b`),
	}
	reportedOneWordRegex = regexp.MustCompile(`(?i)\bonly\s+(?:show(?:s|ing)?|display(?:s|ing)?|returns?|returning|has|have|lists?|listing|loads?|loading)\s+(one|a single)\b`)
	// probeIntegerRegex matches standalone integers (not parts of decimals like 0.020 or identifiers like f66).
	probeIntegerRegex = regexp.MustCompile(`(?:^|[^\w.])(\d{1,7})(?:[^\w.]|$)`)
)

// reportedSymptomCount extracts the count the user says they see ("only displaying 1 source").
func reportedSymptomCount(prompt string) (int, bool) {
	for _, re := range reportedCountRegexes {
		if m := re.FindStringSubmatch(prompt); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
	}
	if reportedOneWordRegex.MatchString(prompt) {
		return 1, true
	}
	return 0, false
}

// observedMaxCount returns the largest standalone integer in probe output, ignoring digits that are
// part of file paths, hex hashes/checksums, timestamps or alphanumeric identifiers.
func observedMaxCount(output string) int {
	var kept []string
	for _, tok := range strings.Fields(output) {
		if strings.ContainsAny(tok, `\/`) {
			continue // path segment (e.g. "C:\...\workspace")
		}
		kept = append(kept, tok)
	}
	clean := strings.Join(kept, " ")
	clean = probeTimestampRegex.ReplaceAllString(clean, " ")
	clean = probeHexRegex.ReplaceAllString(clean, " ")
	maxN := 0
	for _, m := range probeIntegerRegex.FindAllStringSubmatch(clean, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > maxN {
			maxN = n
		}
	}
	return maxN
}

var (
	probeTimestampRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?)?`)
	probeHexRegex       = regexp.MustCompile(`\b[0-9a-fA-F]{16,}\b`)
)

// isTestInvocation reports whether a command runs tests (direct test files included).
func isTestInvocation(cmd string) bool {
	l := strings.ToLower(filepath.ToSlash(cmd))
	return isTestRunnerCmd(cmd) || strings.Contains(l, "/tests/") || strings.Contains(l, "test_") || strings.Contains(l, "_test.")
}

// detectSymptomMismatch reports when a read-only probe shows far more data than the user reported
// seeing, meaning the bug has NOT been reproduced by that probe.
func detectSymptomMismatch(prompt, cmd, output string) (reported, observed int, mismatch bool) {
	if isTestInvocation(cmd) {
		return 0, 0, false
	}
	reported, ok := reportedSymptomCount(prompt)
	if !ok {
		return 0, 0, false
	}
	observed = observedMaxCount(output)
	if observed >= 10 && observed >= reported*10 {
		return reported, observed, true
	}
	return reported, observed, false
}

func symptomMismatchNote(reported, observed int) string {
	return fmt.Sprintf("\n\nENGINE NOTICE: SYMPTOM NOT REPRODUCED. The user reports seeing %d, but this probe shows %d. "+
		"Until you explain that gap with evidence you have not found the bug. Check, with read-only probes: whether the UI is reading a "+
		"different/stale database or cache, whether rows were written AFTER the user's observation (seeders, init routines), and whether "+
		"the extra rows are real or generated placeholders (inspect sample rows and URLs). If you cannot reproduce it, say so plainly. "+
		"Do NOT ship an unrelated or cosmetic patch as the fix.", reported, observed)
}

// ---------------------------------------------------------------------------------------------
// 3. Data-bug completion gate.
// ---------------------------------------------------------------------------------------------

var (
	rootCauseRegex = regexp.MustCompile(`(?i)\broot[\s-]*cause\b`)
	// notReproducedRegex needs an actual reconciliation, not a casual "mismatch" in passing.
	notReproducedRegex     = regexp.MustCompile(`(?i)(could\s*n[o']?t|cannot|can't|unable to|did\s*n[o']?t|not)\s+(be\s+)?reproduc|\bstale\b|captured before|observed before|screenshot (was )?(taken|captured)`)
	fabricatedRowsAckRegex = regexp.MustCompile(`(?i)placeholder|example\.(org|com)|generated|synthetic|\bfake\b|fabricated`)
	hardcodedAckRegex      = regexp.MustCompile(`(?i)hard[\s-]?coded|literal|static count`)
	inflatedAckRegex       = regexp.MustCompile(`(?i)regardless|return value|result is (discarded|ignored)|INSERT OR IGNORE|rowcount|\+=\s*1|increment|count(s|ed|ing)? (attempts|calls)|(insert|write)s?\s+(that\s+)?fail|never (actually )?(stored|persisted|saved)|not (actually )?(stored|persisted|saved)`)
	symptomStatusRegex     = regexp.MustCompile(`(?im)^\W*symptom status\W*:?\s*(.*)$`)
	claimsResolvedRegex    = regexp.MustCompile(`(?i)\b(resolved|verified|fixed)\b`)
	dataDoneGateMaxTries   = 2
)

// dataDoneGateMissing lists what a final data-bug reply still has to establish before DONE is accepted.
func dataDoneGateMissing(reply string, symptomMismatch bool, flags evidenceFlags) []string {
	var missing []string
	if !rootCauseRegex.MatchString(reply) {
		missing = append(missing, "an explicit \"Root cause:\" line backed by the probe output and file/line citations")
	}
	if symptomMismatch && !notReproducedRegex.MatchString(reply) {
		missing = append(missing, "an explanation of why your probe disagrees with what the user sees (or a plain statement that the symptom could not be reproduced)")
	}
	if symptomMismatch {
		if m := symptomStatusRegex.FindStringSubmatch(reply); m != nil && claimsResolvedRegex.MatchString(m[1]) && !notReproducedRegex.MatchString(m[1]) {
			missing = append(missing, "an honest Symptom status: you never reproduced what the user sees, so it cannot be \"resolved and verified\"; say \"not reproduced\" unless a probe after your fix shows the user's view changing")
		}
	}
	if flags.FabricatedRows && !fabricatedRowsAckRegex.MatchString(reply) {
		missing = append(missing, "the generated/placeholder ROWS the engine flagged (e.g. a seeder writing example.org records): how many of the counted rows are fake")
	}
	if flags.HardcodedCounts && !hardcodedAckRegex.MatchString(reply) {
		missing = append(missing, "the hardcoded ledger/harvest counts the engine flagged, and whether the displayed numbers come from them")
	}
	if flags.InflatedCounts && !inflatedAckRegex.MatchString(reply) {
		missing = append(missing, "the counter the engine flagged that is incremented whether or not the write stored anything (so logged harvest counts are attempts, not stored rows)")
	}
	return missing
}

func dataDoneGateReprompt(missing []string) string {
	var b strings.Builder
	b.WriteString("ENGINE NOTICE: This is a data bug and your final answer is not accepted yet. It is missing:\n")
	for _, m := range missing {
		b.WriteString("  - " + m + "\n")
	}
	b.WriteString("Reply in this form (and emit more read-only probes or a real fix first if you lack evidence):\n")
	b.WriteString("  Root cause: <one or two sentences, cite file:line and probe output>\n")
	b.WriteString("  Evidence: <the probe lines that prove it>\n")
	b.WriteString("  Fix applied: <files changed, or \"No fix applied\" and why>\n")
	b.WriteString("  Symptom status: <resolved and verified / not reproduced / still present>\n")
	b.WriteString("A cosmetic or unrelated change is not a fix.")
	return b.String()
}

// ---------------------------------------------------------------------------------------------
// 4. Zero-test fallback: run test files directly when discovery collects nothing.
// ---------------------------------------------------------------------------------------------

var testsActuallyRanRegex = regexp.MustCompile(`(?i)\bRan\s+[1-9]\d*\s+tests?\b|\b[1-9]\d*\s+passed\b`)

// directPythonTestCmd builds a command that runs each Python test file directly, or "" if none exist.
// Paths are emitted unquoted with forward slashes: the runner splits single commands on whitespace and
// execs them without a shell, so literal quotes would become part of the path (Errno 22 on Windows).
// Relative paths containing spaces cannot survive that split and are skipped.
func directPythonTestCmd(dir string) string {
	var parts []string
	for _, f := range listTestFiles(dir, 5) {
		f = filepath.ToSlash(f)
		if !strings.HasSuffix(strings.ToLower(f), ".py") || strings.ContainsAny(f, " \t\"") {
			continue
		}
		parts = append(parts, "python "+f)
	}
	return strings.Join(parts, " && ")
}

// pytestFallbackCmd runs pytest over the directories holding Python test files (pytest-style test
// functions are invisible to unittest discovery and do nothing when the file is run directly).
func pytestFallbackCmd(dir string) string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range listTestFiles(dir, 20) {
		f = filepath.ToSlash(f)
		if !strings.HasSuffix(strings.ToLower(f), ".py") || strings.ContainsAny(f, " \t\"") {
			continue
		}
		d := pathpkg.Dir(f)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return ""
	}
	return "python -m pytest -q -p no:cacheprovider " + strings.Join(dirs, " ")
}

// zeroTestFallbackCmds lists, in order, the commands to try when test discovery collected nothing.
func zeroTestFallbackCmds(dir string) []string {
	var cmds []string
	for _, c := range []string{pytestFallbackCmd(dir), directPythonTestCmd(dir)} {
		if c != "" {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// runnerMissing reports output showing the fallback runner itself is unavailable (not a test failure).
func runnerMissing(output string) bool {
	l := strings.ToLower(output)
	return strings.Contains(l, "no module named pytest") || strings.Contains(l, "no module named 'pytest'")
}

// testsActuallyRan reports whether runner output proves at least one test executed.
func testsActuallyRan(output string) bool {
	return testsActuallyRanRegex.MatchString(output)
}
