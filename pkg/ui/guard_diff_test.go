// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"
)

// Regression cases for fabrication and unsafe-patch guards on data-pipeline diffs.

const syncRemoveHardcodedPatch = `<<<<<<< SEARCH
            if count == 0:
                default_partitions = [
                    ("Primary Catalog API", 500, "synced_remote"),
                    ("Secondary Index API", 350, "synced_remote"),
                ]
        except Exception as e:
            print(f"Ledger bootstrap note: {e}")
=======
            cursor.execute("SELECT source_type, COUNT(*) FROM records GROUP BY source_type")
            for source_name, rec_count in cursor.fetchall():
                upsert(source_name, rec_count)
        except Exception as e:
            print(f"Ledger bootstrap error: {e}")
            raise
>>>>>>> REPLACE
`

func TestFabricationGuardAllowsRemovingHardcodedCounts(t *testing.T) {
	if bad, why := detectFabricatedData("projects/app/utils/sync.py", syncRemoveHardcodedPatch); bad {
		t.Fatalf("a patch that REMOVES hardcoded ledger tuples must not be rejected (matched %q)", why)
	}
}

func TestFabricationGuardStillBlocksAddingHardcodedCounts(t *testing.T) {
	adding := "<<<<<<< SEARCH\n    rows = []\n=======\n    rows = [(\"Primary Catalog API\", 500, \"synced_remote\")]\n>>>>>>> REPLACE\n"
	if bad, _ := detectFabricatedData("projects/app/utils/sync.py", adding); !bad {
		t.Fatal("a patch that ADDS a hardcoded ledger tuple must be rejected")
	}
	if bad, _ := detectFabricatedData("projects/app/utils/sync.py", `ledger = [("Primary Catalog API", 500, "synced_remote")]`); !bad {
		t.Fatal("whole-file content with a hardcoded ledger tuple must be rejected")
	}
}

const dbKeepsExceptAddsSeedPatch = `<<<<<<< SEARCH
    if "source_type" not in columns:
        try:
            cursor.execute("ALTER TABLE records ADD COLUMN source_type TEXT")
        except Exception:
            pass
    conn.close()
=======
    if "source_type" not in columns:
        try:
            cursor.execute("ALTER TABLE records ADD COLUMN source_type TEXT")
        except Exception:
            pass
    conn.close()
    from utils.seed import seed_database
    seed_database()
>>>>>>> REPLACE
`

func TestUnsafeGuardJudgesOnlyNewCode(t *testing.T) {
	bad, why := detectUnsafePatch("projects/app/utils/db.py", dbKeepsExceptAddsSeedPatch)
	if !bad || !strings.Contains(why, "seeder") {
		t.Fatalf("expected the seeder reason (the except/pass was pre-existing), got bad=%v why=%q", bad, why)
	}
	keepOnly := strings.Replace(dbKeepsExceptAddsSeedPatch, "    from utils.seed import seed_database\n    seed_database()\n", "    print('ok')\n", 1)
	if bad, why := detectUnsafePatch("projects/app/utils/db.py", keepOnly); bad {
		t.Fatalf("pre-existing except/pass kept as context must not be rejected, got %q", why)
	}
	newExcept := "<<<<<<< SEARCH\n    run()\n=======\n    try:\n        run()\n    except Exception:\n        pass\n>>>>>>> REPLACE\n"
	if bad, _ := detectUnsafePatch("projects/app/utils/db.py", newExcept); !bad {
		t.Fatal("a newly added except/pass must be rejected")
	}
}

func TestObservedMaxCountIgnoresPathsHashesTimestamps(t *testing.T) {
	ledger := "Ledger: [(1, 'Test DB', '2026-10-03 16:15:23', 1, 'a56390c7838e3fbe057ba396f3d7464b5ade75681d76ff647f5f51de170e8e19', 'restored_local'), " +
		"(8, 'Secondary Feed', '2026-10-03 16:20:46', 350, '513c74e73db1e4f6685142d3ea5e1ee764be540b35389ac12b5e37535d11a0d6', 'synced_remote')]\nRecords count: 1200"
	if n := observedMaxCount(ledger); n != 1200 {
		t.Fatalf("ledger probe: want 1200, got %d", n)
	}
	path := "Exists: True Abs: C:\\Users\\dev\\Projects\\sample-app\\data\\records.db\nRow count: 1200"
	if n := observedMaxCount(path); n != 1200 {
		t.Fatalf("path probe: want 1200, got %d", n)
	}
	if n := observedMaxCount("[('Primary Catalog API', 500), ('Test DB', 1)]"); n != 500 {
		t.Fatalf("group-by probe: want 500, got %d", n)
	}
}

func TestPatchExtractionIgnoresEarlierProseCodeFence(t *testing.T) {
	reply := "Let's inspect `app.py` lines 12-16:\n```python\ninit_db()\nsync_mgr = SyncManager()\n```\nSo we patch it.\n\n" +
		"[ACTION:PATCH path=\"projects/app/utils/db.py\"]\n<<<<<<< SEARCH\n    conn.close()\n=======\n    conn.commit()\n    conn.close()\n>>>>>>> REPLACE\n[/ACTION:PATCH]\n"
	intents := ParseAllActionIntents(reply)
	if len(intents) != 1 {
		t.Fatalf("want 1 intent, got %d", len(intents))
	}
	got := ExtractContentForIntent(reply, intents[0])
	if !strings.Contains(got, "<<<<<<< SEARCH") || !strings.Contains(got, ">>>>>>> REPLACE") || strings.Contains(got, "SyncManager") {
		t.Fatalf("patch payload must be the SEARCH/REPLACE body, got:\n%s", got)
	}
}
