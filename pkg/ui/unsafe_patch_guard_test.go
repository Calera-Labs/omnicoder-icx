package ui

import "testing"

func TestUnsafePatch_SeederTriggerFlagged(t *testing.T) {
	p := "<<<<<<< SEARCH\nx = 1\n=======\nfrom utils.seed import seed_database\nseed_database()\n>>>>>>> REPLACE"
	if ok, _ := detectUnsafePatch("projects/app/utils/db.py", p); !ok {
		t.Fatal("seeder trigger in db.py should be flagged")
	}
}

func TestUnsafePatch_SwallowedExceptFlagged(t *testing.T) {
	p := "<<<<<<< SEARCH\nx = 1\n=======\ntry:\n    f()\nexcept Exception:\n    pass\n>>>>>>> REPLACE"
	if ok, _ := detectUnsafePatch("projects/app/utils/db.py", p); !ok {
		t.Fatal("swallowed except should be flagged")
	}
}

func TestUnsafePatch_OldSideIgnored(t *testing.T) {
	p := "<<<<<<< SEARCH\ntry:\n    f()\nexcept Exception:\n    pass\n=======\ntry:\n    f()\nexcept Exception as e:\n    raise\n>>>>>>> REPLACE"
	if ok, w := detectUnsafePatch("projects/app/utils/db.py", p); ok {
		t.Fatalf("removing a swallow must not be flagged: %s", w)
	}
}

func TestUnsafePatch_TestFilesExempt(t *testing.T) {
	if ok, _ := detectUnsafePatch("tests/test_db.py", "except Exception:\n    pass\n"); ok {
		t.Fatal("tests exempt")
	}
}

func TestNeedsRuntimeProbe(t *testing.T) {
	if !needsRuntimeProbe("front end only shows 1 record", "projects/app/utils/db.py") {
		t.Fatal("data bug + db.py should need probe")
	}
	if needsRuntimeProbe("front end only shows 1 record", "projects/app/static/style.css") {
		t.Fatal("css should not need probe")
	}
	if needsRuntimeProbe("add dark mode", "projects/app/utils/db.py") {
		t.Fatal("non-data prompt should not need probe")
	}
	if needsRuntimeProbe("fix record count", "projects/app/tests/test_db.py") {
		t.Fatal("tests exempt")
	}
	if needsRuntimeProbe("Refactor projects/catalog-app database layer to use a connection pool", "projects/app/utils/db.py") {
		t.Fatal("refactor prompt should not be blocked by runtime probe")
	}
	if needsRuntimeProbe("rewrite database layer to use redis", "projects/app/utils/db.py") {
		t.Fatal("rewrite prompt should not be blocked by runtime probe")
	}
}
