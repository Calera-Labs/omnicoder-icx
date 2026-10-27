//go:build validation

// Validation suite for the embedded TitanKV vault. Run with:
//   go test -tags validation -v -count=1 -timeout 20m ./pkg/storage/ -run TestValidation
package storage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func smallVault(t *testing.T, dir string) *Vault {
	t.Helper()
	// Small thresholds force many flushes and L0->L1 compactions.
	v, err := OpenVaultAt(dir, WithMemTableThreshold(64*1024), WithMaxL0Files(3))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return v
}

// V1: large dataset forces flush + compaction; every key must read back intact.
func TestValidation_ScaleIntegrity(t *testing.T) {
	dir := t.TempDir()
	v := smallVault(t, dir)
	const n = 50000
	start := time.Now()
	for i := 0; i < n; i++ {
		k := []byte(fmt.Sprintf("k%08d", i))
		if err := v.Put(k, bytes.Repeat([]byte{byte(i)}, 128)); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote %d keys in %s", n, time.Since(start))
	time.Sleep(2 * time.Second) // let background compaction settle
	s := v.Stats()
	t.Logf("stats: L0=%d L1=%d mem=%d", s.L0Files, s.L1Files, s.MemTableBytes)
	if s.L0Files+s.L1Files == 0 {
		t.Fatal("expected SSTables to be produced")
	}
	for i := 0; i < n; i++ {
		got, err := v.Get([]byte(fmt.Sprintf("k%08d", i)))
		if err != nil || len(got) != 128 || got[0] != byte(i) {
			t.Fatalf("key %d corrupt: err=%v", i, err)
		}
	}
	v.Close()
}

// V2: reopen after clean close; data must persist across SSTables + WAL.
func TestValidation_ReopenPersistence(t *testing.T) {
	dir := t.TempDir()
	v := smallVault(t, dir)
	for i := 0; i < 5000; i++ {
		_ = v.Put([]byte(fmt.Sprintf("p%06d", i)), []byte(fmt.Sprintf("v%d", i)))
	}
	_ = v.Close()
	v2 := smallVault(t, dir)
	defer v2.Close()
	for i := 0; i < 5000; i++ {
		got, err := v2.Get([]byte(fmt.Sprintf("p%06d", i)))
		if err != nil || string(got) != fmt.Sprintf("v%d", i) {
			t.Fatalf("lost key %d: %v", i, err)
		}
	}
}

// V3: deletes/overwrites must survive compaction and reopen (tombstone correctness).
func TestValidation_TombstonesAcrossCompaction(t *testing.T) {
	dir := t.TempDir()
	v := smallVault(t, dir)
	for i := 0; i < 20000; i++ {
		_ = v.Put([]byte(fmt.Sprintf("t%06d", i)), []byte("old"))
	}
	for i := 0; i < 20000; i += 2 {
		_ = v.Delete([]byte(fmt.Sprintf("t%06d", i)))
	}
	for i := 1; i < 20000; i += 4 {
		_ = v.Put([]byte(fmt.Sprintf("t%06d", i)), []byte("new"))
	}
	time.Sleep(2 * time.Second)
	_ = v.Close()
	v = smallVault(t, dir)
	defer v.Close()
	for i := 0; i < 20000; i++ {
		got, err := v.Get([]byte(fmt.Sprintf("t%06d", i)))
		switch {
		case i%2 == 0:
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("key %d should be deleted, got %q err=%v", i, got, err)
			}
		case i%4 == 1:
			if string(got) != "new" {
				t.Fatalf("key %d want new got %q", i, got)
			}
		default:
			if string(got) != "old" {
				t.Fatalf("key %d want old got %q", i, got)
			}
		}
	}
}

// V4: concurrent writers/readers across vault APIs with the race detector.
func TestValidation_ConcurrentMixedWorkload(t *testing.T) {
	v := smallVault(t, t.TempDir())
	defer v.Close()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				data := []byte(fmt.Sprintf("blob-%d-%d", w, i))
				h, err := v.PutCAS(data)
				if err != nil {
					t.Error(err)
					return
				}
				got, err := v.GetCAS(h)
				if err != nil || !bytes.Equal(got, data) {
					t.Errorf("CAS mismatch w=%d i=%d err=%v", w, i, err)
					return
				}
				_ = v.SaveTurn(fmt.Sprintf("s%d", w), i, data)
			}
		}(w)
	}
	wg.Wait()
}

// V5: CAS dedup + heal-pattern round trip.
func TestValidation_CASDedupAndHeal(t *testing.T) {
	v := smallVault(t, t.TempDir())
	defer v.Close()
	buf := make([]byte, 4096)
	_, _ = rand.Read(buf)
	h1, _ := v.PutCAS(buf)
	h2, _ := v.PutCAS(buf)
	if h1 != h2 {
		t.Fatal("identical content must hash identically")
	}
	if got := v.Stats().CASCount; got != 1 {
		t.Fatalf("CAS dedup failed: count=%d", got)
	}
	_ = v.PutHealPattern("undefined: foo", "add import foo")
	if p, ok := v.GetHealPattern("undefined: foo"); !ok || p != "add import foo" {
		t.Fatal("heal pattern round trip failed")
	}
	if _, ok := v.GetHealPattern("never seen"); ok {
		t.Fatal("unexpected heal hit")
	}
}

// V6: scan returns ordered, tombstone-free results across levels.
func TestValidation_ScanOrderingAcrossLevels(t *testing.T) {
	v := smallVault(t, t.TempDir())
	defer v.Close()
	for i := 0; i < 10000; i++ {
		_ = v.Put([]byte(fmt.Sprintf("s%06d", i)), []byte("x"))
	}
	for i := 0; i < 10000; i += 3 {
		_ = v.Delete([]byte(fmt.Sprintf("s%06d", i)))
	}
	it, err := v.Scan([]byte("s000000"), []byte("s999999"))
	if err != nil {
		t.Fatal(err)
	}
	var prev []byte
	count := 0
	for ; it.Valid(); it.Next() {
		k := it.Key()
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("out of order: %s >= %s", prev, k)
		}
		prev = append(prev[:0], k...)
		count++
	}
	want := 10000 - (10000+2)/3
	if count != want {
		t.Fatalf("scan count=%d want=%d", count, want)
	}
}

// V7: use-after-close must error, never panic.
func TestValidation_UseAfterClose(t *testing.T) {
	v := smallVault(t, t.TempDir())
	_ = v.Close()
	if err := v.Put([]byte("a"), []byte("b")); !errors.Is(err, ErrVaultClosed) {
		t.Fatalf("want ErrVaultClosed got %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("double close should be nil: %v", err)
	}
}
