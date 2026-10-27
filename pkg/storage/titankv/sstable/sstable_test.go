// SPDX-License-Identifier: Apache-2.0
package sstable

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSSTableWriteAndRead(t *testing.T) {
	dir, err := os.MkdirTemp("", "sstable_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "test.sst")
	builder, err := NewSSTableBuilder(path, 100)
	if err != nil {
		t.Fatal(err)
	}

	builder.Add([]byte("apple"), []byte("red"), false)
	builder.Add([]byte("banana"), []byte("yellow"), false)
	builder.Add([]byte("cherry"), []byte("darkred"), false)
	builder.Add([]byte("deleted_key"), nil, true)
	if err := builder.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenSSTableReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if reader.Path() != path {
		t.Errorf("expected reader path %s, got %s", path, reader.Path())
	}

	val, isDel, found, err := reader.Get([]byte("banana"))
	if err != nil || !found || isDel || !bytes.Equal(val, []byte("yellow")) {
		t.Errorf("banana lookup failed: val=%s, found=%v, isDel=%v, err=%v", val, found, isDel, err)
	}

	_, isDel, found, err = reader.Get([]byte("deleted_key"))
	if err != nil || !found || !isDel {
		t.Errorf("deleted_key lookup failed: found=%v, isDel=%v, err=%v", found, isDel, err)
	}

	_, _, found, err = reader.Get([]byte("nonexistent"))
	if err != nil || found {
		t.Errorf("nonexistent key should not be found: found=%v", found)
	}
}
