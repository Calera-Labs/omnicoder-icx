// SPDX-License-Identifier: Apache-2.0
package wal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWALAppendAndRecovery(t *testing.T) {
	dir, err := os.MkdirTemp("", "wal_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	walPath := filepath.Join(dir, "test.wal")
	w, err := Open(walPath)
	if err != nil {
		t.Fatal(err)
	}

	err = w.Append(OpPut, []byte("key1"), []byte("val1"))
	if err != nil {
		t.Fatal(err)
	}
	err = w.Append(OpDelete, []byte("key2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = w.Append(OpPut, []byte("key3"), []byte("val3-long-value"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	recs, _, err := Recover(walPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d", len(recs))
	}

	if recs[0].Type != OpPut || !bytes.Equal(recs[0].Key, []byte("key1")) || !bytes.Equal(recs[0].Value, []byte("val1")) {
		t.Errorf("record 0 mismatch: %+v", recs[0])
	}
	if recs[1].Type != OpDelete || !bytes.Equal(recs[1].Key, []byte("key2")) {
		t.Errorf("record 1 mismatch: %+v", recs[1])
	}
	if recs[2].Type != OpPut || !bytes.Equal(recs[2].Key, []byte("key3")) || !bytes.Equal(recs[2].Value, []byte("val3-long-value")) {
		t.Errorf("record 2 mismatch: %+v", recs[2])
	}
}

func TestWALTruncationCorruption(t *testing.T) {
	dir, err := os.MkdirTemp("", "wal_corruption_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	walPath := filepath.Join(dir, "test.wal")
	w, err := Open(walPath)
	if err != nil {
		t.Fatal(err)
	}

	w.Append(OpPut, []byte("k1"), []byte("v1"))
	w.Append(OpPut, []byte("k2"), []byte("v2"))
	w.Close()

	// Append garbage / partial torn bytes at the end
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0x01, 0x02, 0x03, 0x04, 0xff, 0xee})
	f.Close()

	recs, _, err := Recover(walPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(recs) != 2 {
		t.Fatalf("expected 2 valid records after recovery truncation, got %d", len(recs))
	}
}
