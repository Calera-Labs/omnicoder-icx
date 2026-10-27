// SPDX-License-Identifier: Apache-2.0
package titankv

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentReadWriteSpam(t *testing.T) {
	dir, err := os.MkdirTemp("", "titankv_spam_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := Open(Options{
		Dir:               dir,
		MemTableThreshold: 1024 * 64, // 64KB for frequent flushes
		MaxL0Files:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var wg sync.WaitGroup
	workers := 20
	iterations := 300

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				k := []byte(fmt.Sprintf("key-%d-%d", workerID, i))
				v := []byte(fmt.Sprintf("val-%d-%d", workerID, i))

				if err := db.Put(k, v); err != nil {
					t.Errorf("Put error: %v", err)
				}

				if i%2 == 0 {
					_, _ = db.Get(k)
				}

				if i%5 == 0 {
					_ = db.Delete(k)
				}
			}
		}(w)
	}

	wg.Wait()
}

func TestCrashAndWALRecovery(t *testing.T) {
	dir, err := os.MkdirTemp("", "titankv_crash_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := Open(Options{
		Dir:               dir,
		MemTableThreshold: 10 * 1024 * 1024,
		MaxL0Files:        4,
	})
	if err != nil {
		t.Fatal(err)
	}

	numKeys := 5000
	for i := 0; i < numKeys; i++ {
		k := []byte(fmt.Sprintf("key-%05d", i))
		v := []byte(fmt.Sprintf("val-%05d", i))
		if err := db.Put(k, v); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	// Simulate abrupt crash/tear: corrupt/truncate active.wal by last 15 bytes (partial record)
	walPath := filepath.Join(dir, "active.wal")
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 15 {
		err = os.Truncate(walPath, fi.Size()-15)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Restart engine
	db2, err := Open(Options{
		Dir:               dir,
		MemTableThreshold: 10 * 1024 * 1024,
		MaxL0Files:        4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	// Verify almost all keys recovered (minus last partial write)
	recoveredCount := 0
	for i := 0; i < numKeys; i++ {
		k := []byte(fmt.Sprintf("key-%05d", i))
		val, err := db2.Get(k)
		if err == nil {
			expectedVal := []byte(fmt.Sprintf("val-%05d", i))
			if !bytes.Equal(val, expectedVal) {
				t.Fatalf("value mismatch for %s: got %s", k, val)
			}
			recoveredCount++
		}
	}

	if recoveredCount < 4900 {
		t.Fatalf("expected at least 4900 recovered keys, got %d", recoveredCount)
	}
}

func TestBloomFilterNegativeLookups(t *testing.T) {
	dir, err := os.MkdirTemp("", "titankv_bloom_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := Open(Options{
		Dir:               dir,
		MemTableThreshold: 1024 * 32, // flush frequently
		MaxL0Files:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert 2000 keys
	for i := 0; i < 2000; i++ {
		db.Put([]byte(fmt.Sprintf("exist-%d", i)), []byte("val"))
	}

	// Force flush by closing and reopening
	db.Close()

	db2, err := Open(Options{
		Dir:               dir,
		MemTableThreshold: 10 * 1024 * 1024,
		MaxL0Files:        10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	// Query 10,000 non-existent keys and measure false positives / rejections
	nonExistentQueries := 10000
	falsePositives := 0

	for i := 0; i < nonExistentQueries; i++ {
		k := []byte(fmt.Sprintf("nonexist-%d", i))
		_, err := db2.Get(k)
		if err == nil {
			falsePositives++
		}
	}

	fpRate := float64(falsePositives) / float64(nonExistentQueries)
	// Target false positive rate (~1% to 10% for small test bitsets)
	if fpRate > 0.12 {
		t.Fatalf("Bloom filter false positive rate too high: %.2f%%", fpRate*100)
	}
}

func TestScanIteratorTombstones(t *testing.T) {
	dir, err := os.MkdirTemp("", "titankv_scan_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := Open(Options{
		Dir:               dir,
		MemTableThreshold: 1024 * 16, // small to trigger multiple flushed sstables
		MaxL0Files:        4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Interleave Puts, Deletes, Overwrites
	db.Put([]byte("a"), []byte("valA1"))
	db.Put([]byte("b"), []byte("valB1"))
	db.Put([]byte("c"), []byte("valC1"))
	db.Delete([]byte("b"))               // delete b
	db.Put([]byte("a"), []byte("valA2")) // overwrite a
	db.Put([]byte("d"), []byte("valD1"))

	it, err := db.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var results []string
	for it.Valid() {
		results = append(results, fmt.Sprintf("%s=%s", string(it.Key()), string(it.Value())))
		it.Next()
	}

	expected := []string{"a=valA2", "c=valC1", "d=valD1"}
	if len(results) != len(expected) {
		t.Fatalf("expected %d results, got %d: %v", len(expected), len(results), results)
	}

	for i, res := range results {
		if res != expected[i] {
			t.Errorf("at index %d: expected %s, got %s", i, expected[i], res)
		}
	}
}
