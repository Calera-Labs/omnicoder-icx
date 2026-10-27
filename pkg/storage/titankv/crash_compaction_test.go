// SPDX-License-Identifier: Apache-2.0
package titankv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Simulates a crash after a compaction published its merged L1 table but
// before it deleted its input tables (plus a half-written .tmp). Reopening must
// not let the stale inputs shadow the compacted result.
func TestCrashAfterCompactionBeforeInputCleanup(t *testing.T) {
	dir := t.TempDir()

	// Phase 1: no compaction (huge L0 limit); overwrite + delete across tables.
	db, err := Open(Options{Dir: dir, MemTableThreshold: 32 * 1024, MaxL0Files: 1000})
	if err != nil {
		t.Fatal(err)
	}
	const n = 6000
	for i := 0; i < n; i++ {
		_ = db.Put([]byte(fmt.Sprintf("c%06d", i)), []byte("v1"))
	}
	for i := 0; i < n; i += 3 {
		_ = db.Put([]byte(fmt.Sprintf("c%06d", i)), []byte("v2"))
	}
	for i := 1; i < n; i += 3 {
		_ = db.Delete([]byte(fmt.Sprintf("c%06d", i)))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Save stale inputs (the L0 tables) before compaction removes them.
	stale := map[string][]byte{}
	matches, _ := filepath.Glob(filepath.Join(dir, "l0_*.sst"))
	if len(matches) < 3 {
		t.Fatalf("need >=3 L0 tables, got %d", len(matches))
	}
	for _, p := range matches {
		b, _ := os.ReadFile(p)
		stale[p] = b
	}

	// Phase 2: reopen with a low limit and force a compaction.
	db, err = Open(Options{Dir: dir, MemTableThreshold: 32 * 1024, MaxL0Files: 3})
	if err != nil {
		t.Fatal(err)
	}
	db.compactChan <- struct{}{}
	deadline := time.Now().Add(30 * time.Second)
	for {
		l0, l1 := db.LevelCounts()
		if l1 == 1 && l0 == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("compaction did not finish: L0=%d L1=%d", l0, l1)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// "Crash": inputs were never deleted, and a partial table is lying around.
	for p, b := range stale {
		if err := os.WriteFile(p, b, 0644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "l1_999999999999999999_0.sst.tmp"), []byte("garbage"), 0644)

	// Phase 3: reopen must recover to the correct state.
	db, err = Open(Options{Dir: dir, MemTableThreshold: 32 * 1024, MaxL0Files: 3})
	if err != nil {
		t.Fatalf("reopen after simulated crash: %v", err)
	}
	defer db.Close()
	for i := 0; i < n; i++ {
		got, err := db.Get([]byte(fmt.Sprintf("c%06d", i)))
		switch {
		case i%3 == 1:
			if err == nil {
				t.Fatalf("key %d should be deleted, got %q", i, got)
			}
		case i%3 == 0:
			if err != nil || string(got) != "v2" {
				t.Fatalf("key %d want v2 got %q err=%v", i, got, err)
			}
		default:
			if err != nil || string(got) != "v1" {
				t.Fatalf("key %d want v1 got %q err=%v", i, got, err)
			}
		}
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(tmps) != 0 {
		t.Fatalf("stray tmp files remain: %v", tmps)
	}
}
