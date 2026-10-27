// SPDX-License-Identifier: Apache-2.0
package sstable

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Regression: Bloom filters must have zero false negatives after an
// encode -> file -> decode round trip, for every key count (including those
// where n*10 is not a multiple of 8).
func TestBloomRoundTripNoFalseNegatives(t *testing.T) {
	for n := 1; n <= 300; n += 7 {
		path := filepath.Join(t.TempDir(), "t.sst")
		b, err := NewSSTableBuilder(path, n)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if err := b.Add([]byte(fmt.Sprintf("key%06d", i)), []byte("v"), false); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := OpenSSTableReader(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, _, found, _ := r.Get([]byte(fmt.Sprintf("key%06d", i))); !found {
				t.Fatalf("n=%d: false negative for key %d", n, i)
			}
		}
		r.Close()
	}
}
