// SPDX-License-Identifier: Apache-2.0
package memtable

import (
	"bytes"
	"testing"
)

func TestSkipListBasic(t *testing.T) {
	sl := NewSkipList()

	sl.Put([]byte("b"), []byte("valB"), false)
	sl.Put([]byte("a"), []byte("valA"), false)
	sl.Put([]byte("c"), []byte("valC"), false)

	entry, found := sl.Get([]byte("b"))
	if !found || !bytes.Equal(entry.Value, []byte("valB")) {
		t.Errorf("expected valB, got %+v", entry)
	}

	entries := sl.Entries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if !bytes.Equal(entries[0].Key, []byte("a")) ||
		!bytes.Equal(entries[1].Key, []byte("b")) ||
		!bytes.Equal(entries[2].Key, []byte("c")) {
		t.Errorf("entries not sorted lexicographically: %+v", entries)
	}
}
