// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"os"
	"path/filepath"
	"testing"
)

// Reply captured from a session where the loop ended instead of reading the file.
const stalledReadReply = "Let's inspect `utils/sync.py` and `utils/db.py` to see how `sync_records()` synchronizes remote records into SQLite, and why only 1 record is present in the database.\n\nLet's read `utils/sync.py` using `[ACTION:READ]`."

func TestDetectStalledIntent_AnnouncedReadWithoutTag(t *testing.T) {
	si, ok := detectStalledIntent(stalledReadReply)
	if !ok {
		t.Fatal("expected stalled intent to be detected")
	}
	if !si.IsRead() {
		t.Fatalf("expected read intent, got verb=%q bare=%q", si.Verb, si.BareTag)
	}
	if si.BareTag != "READ" {
		t.Fatalf("expected bare tag READ, got %q", si.BareTag)
	}
}

func TestDetectStalledIntent_IgnoresConclusiveReply(t *testing.T) {
	reply := "The root cause was the missing category_tag column; the migration now adds it and the probe shows 1200 rows. DONE"
	if _, ok := detectStalledIntent(reply); ok {
		t.Fatal("conclusive reply must not be treated as stalled")
	}
}

func TestDetectStalledIntent_AnnouncedPatch(t *testing.T) {
	reply := "The insert fails because the column is missing.\n\nI will now patch `utils/db.py` to add the migration."
	si, ok := detectStalledIntent(reply)
	if !ok || si.IsRead() {
		t.Fatalf("expected non-read stalled intent, got ok=%v verb=%q", ok, si.Verb)
	}
	if p := stalledIntentReprompt(si); p == "" {
		t.Fatal("expected a corrective prompt")
	}
}

func TestExtractIntendedReadPaths_ResolvesProjectFiles(t *testing.T) {
	ws := t.TempDir()
	proj := filepath.Join("projects", "catalog-app")
	for _, rel := range []string{"utils/sync.py", "utils/db.py"} {
		full := filepath.Join(ws, proj, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("print('x')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	got := extractIntendedReadPaths(ws, filepath.ToSlash(proj), stalledReadReply, seen)
	if len(got) != 2 {
		t.Fatalf("expected 2 recovered reads, got %d", len(got))
	}
	if got[0].Path() != "utils/sync.py" || got[1].Path() != "utils/db.py" {
		t.Fatalf("unexpected paths: %q, %q", got[0].Path(), got[1].Path())
	}

	// Already-read files must not be re-read (prevents loops).
	seen[readKey(ws, filepath.ToSlash(proj), got[0])] = true
	seen[readKey(ws, filepath.ToSlash(proj), got[1])] = true
	if again := extractIntendedReadPaths(ws, filepath.ToSlash(proj), stalledReadReply, seen); len(again) != 0 {
		t.Fatalf("expected no re-reads, got %d", len(again))
	}
}
