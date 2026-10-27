// SPDX-License-Identifier: Apache-2.0

package ui

import "testing"

func TestDetectFabricatedData_SeedFileGeneratedRecords(t *testing.T) {
	loop := "for i in range(1, count + 1):\n" +
		"    title = f\"{title_prefix} - Item #{i}\"\n" +
		"    cursor.execute(\"INSERT OR IGNORE INTO records (source_title) VALUES (?)\", (title,))\n"
	if ok, _ := detectFabricatedData("projects/app/utils/seed.py", loop); !ok {
		t.Error("bulk-generated records in a seed file must be flagged")
	}
	url := `url = f"https://example.org/{source_type}/record_{i}"`
	if ok, _ := detectFabricatedData("projects/app/utils/seed.py", url); !ok {
		t.Error("templated example.org record URLs must be flagged")
	}
	if ok, _ := detectFabricatedData("projects/app/tests/test_app.py", loop); ok {
		t.Error("real test files must stay exempt")
	}
	handWritten := `{"title": "Sample Document", "url": "https://docs.python.org/3/library/sqlite3.html"}`
	if ok, w := detectFabricatedData("projects/app/utils/seed.py", handWritten); ok {
		t.Errorf("small hand-written seed samples must pass, flagged %q", w)
	}
	scraper := "for rec in contexts:\n    insert_record(rec['title'])"
	if ok, w := detectFabricatedData("projects/app/scrapers/catalog.py", scraper); ok {
		t.Errorf("real scraper loops must pass, flagged %q", w)
	}
}
