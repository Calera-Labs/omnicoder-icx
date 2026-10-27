// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// mutatingProbeRegex flags probe commands that write to persisted state. On data-bug tasks a probe
// must observe the real state, not change it: running a seeder or init routine "to check" silently
// rewrites the evidence (e.g. filling a database with generated rows).
var mutatingProbeRegex = regexp.MustCompile(`(?i)(\bseed\w*\s*\(|\bimport\s+[\w.]*seed\w*|from\s+[\w.]*seed\w*\s+import|[/\\.]seed\w*\.py\b|\binit_db\s*\(|\bsync_\w+\s*\(|\binsert_\w+\s*\(|\.commit\s*\(|\bexecutescript\s*\(|\.to_sql\s*\(|\bINSERT\s+(OR\s+\w+\s+)?INTO\b|\bUPDATE\s+\w+\s+SET\b|\bDELETE\s+FROM\b|\bDROP\s+TABLE\b|\bALTER\s+TABLE\b|\bTRUNCATE\b|\bVACUUM\b)`)

// detectMutatingProbe reports whether a BUILD command issued during a data-bug task would mutate
// persisted data. Test runners are exempt (they are expected to use their own fixtures).
func detectMutatingProbe(prompt, cmd string) (bool, string) {
	if !isDataBugInvestigation(prompt) || isTestRunnerCmd(cmd) {
		return false, ""
	}
	if m := mutatingProbeRegex.FindString(cmd); m != "" {
		return true, strings.TrimSpace(m)
	}
	return false, ""
}

// mutatingProbeDiag is fed back to the architect when a probe is rejected.
func mutatingProbeDiag(cmd, why string) string {
	return "Probe " + quoteShort(cmd) + " was NOT run: it calls `" + why + "`, which writes to the database. " +
		"On a data bug, probes must be read-only so they observe the real state instead of rewriting it " +
		"(e.g. calling a seeder or init_db() fills the DB and destroys the evidence). " +
		"Open SQLite read-only and query directly, e.g.:\n" +
		"  python -c \"import sqlite3; c=sqlite3.connect('file:projects/<app>/data/<db>.db?mode=ro', uri=True); " +
		"print(c.execute('PRAGMA table_info(records)').fetchall()); " +
		"print(c.execute('SELECT source_type, COUNT(*) FROM records GROUP BY 1').fetchall())\"\n" +
		"Then read the code that is supposed to persist the missing rows and capture its real exception."
}

func quoteShort(s string) string {
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return "\"" + s + "\""
}

// rejectedEdit records a guard-rejected edit and the file content hash at rejection time.
type rejectedEdit struct {
	Reason string
	Hash   string // sha256 of the file when the edit was rejected ("" if the file did not exist)
}

// fileHash returns the sha256 of a workspace file, or "" if it cannot be read.
func fileHash(ws, relPath string) string {
	data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(relPath)))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// unresolvedRejectedEdits returns the rejected edits whose target file is still byte-identical to
// its state at rejection time, i.e. no compliant fix was applied afterwards. Sorted by path.
func unresolvedRejectedEdits(ws string, rejected map[string]rejectedEdit) []string {
	var paths []string
	for p, r := range rejected {
		if fileHash(ws, p) == r.Hash {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// rejectedEditReprompt tells the architect its claimed fix never reached disk.
func rejectedEditReprompt(paths []string, rejected map[string]rejectedEdit) string {
	var b strings.Builder
	b.WriteString("ENGINE NOTICE: The following edit(s) were REJECTED by engine guards and the files are UNCHANGED on disk:\n")
	for _, p := range paths {
		b.WriteString("  - " + p + ": " + rejected[p].Reason + "\n")
	}
	b.WriteString("Any statement that these files were updated or that the issue is fixed is false. ")
	b.WriteString("Either emit a compliant fix now (no swallowed exceptions, no seeders wired into runtime, no generated/fabricated records), ")
	b.WriteString("or state plainly that the fix was NOT applied and what the real root cause is. Do not reply DONE unless a real fix is on disk and verified.")
	return b.String()
}
