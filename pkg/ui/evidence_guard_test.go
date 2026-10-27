// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic fixtures exercising bulk-generated seed loops and hardcoded ledger counts.
const seedGeneratorSrc = `def seed_database():
    generators = [
        ("Primary Catalog API", 500, "Catalog / Metadata", "Service Item", "System"),
    ]
    for source_type, count, category, title_prefix, auth in generators:
        for i in range(1, count + 1):
            title = f"{title_prefix} - Item #{i}"
            author = f"{auth} (Entry #{i})"
            url = f"https://example.org/{source_type.lower().replace(' ', '_')}/record_{i}"
            try:
                cursor.execute("""
                    INSERT OR IGNORE INTO records (source_title) VALUES (?)
                """, (title,))
            except Exception:
                pass
`

const ledgerBootstrapSrc = `                default_partitions = [
                    ("Primary Catalog API", 500, "synced_remote"),
                    ("Secondary Index API", 350, "synced_remote"),
                    ("Test DB", 7, "restored_local")
                ]
`

const printOnlyInsertSrc = `def insert_record(a):
    try:
        cursor.execute("INSERT OR IGNORE INTO records VALUES (?)", (a,))
        conn.commit()
    except Exception as e:
        print(f"Database insertion error: {e}")
    finally:
        conn.close()
`

func writeWS(t *testing.T, ws, rel, content string) {
	t.Helper()
	full := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceWarnings_FlagsSeedGeneratorAndLedger(t *testing.T) {
	ws := t.TempDir()
	proj := "projects/catalog-app"
	writeWS(t, ws, proj+"/utils/seed.py", seedGeneratorSrc)
	writeWS(t, ws, proj+"/utils/sync.py", ledgerBootstrapSrc)
	writeWS(t, ws, proj+"/utils/db.py", printOnlyInsertSrc)

	w, fl := evidenceWarningsForRead(ws, proj, &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": "utils/seed.py"}})
	if !fl.FabricatedRows || !strings.Contains(w, "bulk-generated INSERT loop") || !strings.Contains(w, "placeholder record") {
		t.Fatalf("seed.py generator not flagged as fabricated:\n%s", w)
	}
	if !strings.Contains(w, "L6:") {
		t.Fatalf("expected line citation for the range loop, got:\n%s", w)
	}

	w, fl = evidenceWarningsForRead(ws, proj, &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": "utils/sync.py"}})
	if !fl.HardcodedCounts || strings.Count(w, "hardcoded harvest/ledger count") != 2 { // 500 and 350; single-digit 7 is below the threshold
		t.Fatalf("hardcoded ledger counts not flagged:\n%s", w)
	}

	w, fl = evidenceWarningsForRead(ws, proj, &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": "utils/db.py"}})
	if fl.FabricatedRows || fl.HardcodedCounts || !strings.Contains(w, "only printed") {
		t.Fatalf("print-only except should be a non-fabrication warning:\n%s", w)
	}
	if !fl.InflatedCounts || !strings.Contains(w, "INSERT OR IGNORE without checking cursor.rowcount") {
		t.Fatalf("INSERT OR IGNORE with no rowcount check should be flagged as inflating counts:\n%s", w)
	}
}

// Unchecked ingest counter: the counter runs whether or not insert_record stored anything.
const scraperCounterSrc = `def run():
    total_inserted = 0
    for item in results:
        if keyword in text:
            insert_record(
                source_title=title,
                url_or_id=item_id,
                source_type="Primary Catalog API"
            )
            total_inserted += 1
    tracker.record_batch("Catalog API", total_inserted, samples)
    return total_inserted
`

const scraperCheckedSrc = `def run():
    total_inserted = 0
    for item in results:
        if insert_record(source_title=title):
            total_inserted += 1
`

func TestEvidenceWarnings_FlagsUncheckedScraperCounter(t *testing.T) {
	ws := t.TempDir()
	proj := "projects/app"
	writeWS(t, ws, proj+"/scrapers/catalog.py", scraperCounterSrc)
	writeWS(t, ws, proj+"/scrapers/checked.py", scraperCheckedSrc)
	w, fl := evidenceWarningsForRead(ws, proj, &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": "scrapers/catalog.py"}})
	if !fl.InflatedCounts || !strings.Contains(w, "L10:") || !strings.Contains(w, "count ATTEMPTS") {
		t.Fatalf("unchecked counter after insert_record not flagged at L10:\n%s", w)
	}
	if w, fl := evidenceWarningsForRead(ws, proj, &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": "scrapers/checked.py"}}); fl.InflatedCounts {
		t.Fatalf("a counter guarded by the insert result must not be flagged:\n%s", w)
	}
}

func TestEvidenceWarnings_TestFilesExempt(t *testing.T) {
	ws := t.TempDir()
	writeWS(t, ws, "projects/app/tests/test_app.py", seedGeneratorSrc)
	if w, _ := evidenceWarningsForRead(ws, "projects/app", &ActionIntent{Type: ActionRead, Attributes: map[string]string{"path": "tests/test_app.py"}}); w != "" {
		t.Fatalf("test files must not be flagged:\n%s", w)
	}
}

func TestDetectSymptomMismatch(t *testing.T) {
	prompt := "fix the data issue, the sync log shows data but the front end is only displaying 1 source"
	probe := "(1200,)\n[('Secondary Feed', 350), ('Primary Catalog API', 500), ('Test DB', 1)]"
	rep, obs, mm := detectSymptomMismatch(prompt, `python -c "import sqlite3; ..."`, probe)
	if !mm || rep != 1 || obs != 1200 {
		t.Fatalf("expected mismatch 1 vs 1200, got rep=%d obs=%d mm=%v", rep, obs, mm)
	}
	// Test runs never count as symptom probes, and decimals like 0.020s are not counts.
	if _, _, mm := detectSymptomMismatch(prompt, "python projects/app/tests/test_app.py", "Ran 4 tests in 0.020s\n\nOK"); mm {
		t.Fatal("test output must not trigger a mismatch")
	}
	if _, _, mm := detectSymptomMismatch("make the header blue", "python -c x", "(1200,)"); mm {
		t.Fatal("no reported count means no mismatch")
	}
}

func TestDataDoneGate(t *testing.T) {
	all := evidenceFlags{FabricatedRows: true, HardcodedCounts: true, InflatedCounts: true}
	if missing := dataDoneGateMissing("DONE", true, evidenceFlags{FabricatedRows: true}); len(missing) != 3 {
		t.Fatalf("bare DONE should miss all 3 requirements, got %v", missing)
	}
	good := "Root cause: utils/seed.py:127-152 generates 1,200 placeholder rows (example.org URLs), the ledger bootstrap has " +
		"hardcoded counts, and the scrapers count records whose insert failed, so the ledger 500/350/200 is inflated. The user's view of 1 record was captured before the " +
		"seeder ran; the symptom could not be reproduced against the current DB.\nFix applied: No fix applied.\nSymptom status: not reproduced"
	if missing := dataDoneGateMissing(good, true, all); len(missing) != 0 {
		t.Fatalf("complete answer rejected: %v", missing)
	}
	// Incomplete answer: casual "mismatch", seeder named but rows not called fake,
	// scraper counter ignored, and a "resolved and verified" claim for a symptom never reproduced.
	latest := "Root cause: `utils/sync.py` (lines 46-52) initialized `sync_ledger` with hardcoded static counts (`500`, `350`) " +
		"while `app.py` loaded records before `seed_database()` populated SQLite, causing a mismatch where the UI displayed sparse records.\n" +
		"Fix applied: Patched `utils/sync.py` to dynamically query ledger counts.\nSymptom status: resolved and verified"
	missing := dataDoneGateMissing(latest, true, all)
	if len(missing) != 4 {
		t.Fatalf("latest-run answer should miss reconciliation, honest status, fake rows and inflated counter (4), got %d: %v", len(missing), missing)
	}
	if p := dataDoneGateReprompt([]string{"x"}); !strings.Contains(p, "Root cause:") {
		t.Fatal("reprompt must show the required format")
	}
}

func TestDirectPythonTestCmdAndRanCheck(t *testing.T) {
	dir := t.TempDir()
	writeWS(t, dir, "tests/test_app.py", "import unittest\n")
	cmd := directPythonTestCmd(dir)
	if cmd != "python tests/test_app.py" {
		t.Fatalf("direct command must be unquoted with forward slashes, got %q", cmd)
	}
	cmds := zeroTestFallbackCmds(dir)
	if len(cmds) != 2 || cmds[0] != "python -m pytest -q -p no:cacheprovider tests" || cmds[1] != cmd {
		t.Fatalf("fallback order must be pytest then direct, got %q", cmds)
	}
	spaced := t.TempDir()
	writeWS(t, spaced, "my tests/test_x.py", "import unittest\n")
	if c := directPythonTestCmd(spaced); c != "" {
		t.Fatalf("paths with spaces must be skipped (runner splits on whitespace), got %q", c)
	}
	if !runnerMissing("/usr/bin/python3: No module named pytest") || runnerMissing("4 passed") {
		t.Fatal("runnerMissing misclassified output")
	}
	if !testsActuallyRan("....\nRan 4 tests in 0.020s\n\nOK") {
		t.Fatal("expected 4 tests to count as executed")
	}
	if testsActuallyRan("Ran 0 tests in 0.000s\n\nNO TESTS RAN") || testsActuallyRan("") {
		t.Fatal("zero or empty output must not count as executed")
	}
}
