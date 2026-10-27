// SPDX-License-Identifier: Apache-2.0
package sstable

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Regression: Get must report found=false for absent keys (below the first
// key, between keys, above the last key), never found=true with empty value.
func TestGetAbsentKeysNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.sst")
	b, _ := NewSSTableBuilder(path, 100)
	for i := 100; i < 200; i += 2 {
		_ = b.Add([]byte(fmt.Sprintf("key%06d", i)), []byte("v"), false)
	}
	_ = b.Close()
	r, err := OpenSSTableReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Probe many absent keys; even if Bloom yields false positives, found must be false.
	for i := 0; i < 400; i++ {
		if i >= 100 && i < 200 && i%2 == 0 {
			continue
		}
		if _, _, found, _ := r.Get([]byte(fmt.Sprintf("key%06d", i))); found {
			t.Fatalf("absent key %d reported found", i)
		}
	}
}
