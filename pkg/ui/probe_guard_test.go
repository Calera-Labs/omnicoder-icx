// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"testing"
)

const dataBugPrompt = "fix the database query issue where the front end is only displaying 1 record"

func TestDetectMutatingProbe_RejectsSeederAndInitProbes(t *testing.T) {
	// Reject probes that invoke database seeders or destructive schema initializers during data-bug diagnosis.
	for _, cmd := range []string{
		`python -c "import sys; sys.path.insert(0, 'projects/catalog-app'); from utils.seed import seed_database; seed_database(); from utils.db import fetch_all_records; df = fetch_all_records(); print(len(df))"`,
		`python -c "import sys; sys.path.insert(0, 'projects/catalog-app'); from utils.db import init_db, fetch_all_records; init_db(); df = fetch_all_records(); print(len(df))"`,
		`python -c "import sqlite3; c=sqlite3.connect('x.db'); c.execute('DELETE FROM records'); c.commit()"`,
		`python projects/app/utils/seed.py`,
	} {
		if ok, _ := detectMutatingProbe(dataBugPrompt, cmd); !ok {
			t.Errorf("expected mutating probe to be rejected: %s", cmd)
		}
	}
}

func TestDetectMutatingProbe_AllowsReadOnlyProbes(t *testing.T) {
	for _, cmd := range []string{
		`python -c "import sqlite3; c=sqlite3.connect('file:projects/app/data/records.db?mode=ro', uri=True); print(c.execute('PRAGMA table_info(records)').fetchall()); print(c.execute('SELECT source_type, COUNT(*) FROM records GROUP BY 1').fetchall())"`,
		`python -c "import sys; sys.path.insert(0, 'projects/app'); from utils.db import fetch_all_records; print(len(fetch_all_records()))"`,
		`python projects/catalog-app/tests/test_app.py`,
	} {
		if ok, why := detectMutatingProbe(dataBugPrompt, cmd); ok {
			t.Errorf("read-only probe wrongly rejected (%s): %s", why, cmd)
		}
	}
}

func TestDetectMutatingProbe_OnlyOnDataBugs(t *testing.T) {
	if ok, _ := detectMutatingProbe("make the header blue", `python -c "from utils.db import init_db; init_db()"`); ok {
		t.Fatal("guard must only apply to data-bug prompts")
	}
}

func TestUnresolvedRejectedEdits_TracksFileChanges(t *testing.T) {
	ws := t.TempDir()
	rel := "projects/app/utils/db.py"
	full := filepath.Join(ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("count < 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rejected := map[string]rejectedEdit{rel: {Reason: "swallowed exception", Hash: fileHash(ws, rel)}}
	if got := unresolvedRejectedEdits(ws, rejected); len(got) != 1 || got[0] != rel {
		t.Fatalf("expected %s to be unresolved, got %v", rel, got)
	}
	if p := rejectedEditReprompt([]string{rel}, rejected); p == "" {
		t.Fatal("expected a corrective prompt")
	}

	// A later compliant edit changes the file and resolves the rejection.
	if err := os.WriteFile(full, []byte("count < 10  # fixed properly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := unresolvedRejectedEdits(ws, rejected); len(got) != 0 {
		t.Fatalf("expected no unresolved edits after file changed, got %v", got)
	}
}
