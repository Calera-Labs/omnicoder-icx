// SPDX-License-Identifier: Apache-2.0
package titankv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/storage/titankv/memtable"
	"github.com/caleralabs/omnicoder-icx/pkg/storage/titankv/sstable"
	"github.com/caleralabs/omnicoder-icx/pkg/storage/titankv/wal"
)

type Options struct {
	Dir               string
	MemTableThreshold int64 // e.g. 4 * 1024 * 1024 (4MB)
	MaxL0Files        int   // e.g. 4
}

type DB struct {
	mu          sync.RWMutex
	opts        Options
	wal         *wal.WAL
	activeMem   *memtable.SkipList
	immMem      *memtable.SkipList
	immWalPath  string // archived WAL backing immMem; removed after a successful flush
	l0Tables    []*sstable.SSTableReader
	l1Tables    []*sstable.SSTableReader
	closed      bool
	flushChan   chan *memtable.SkipList
	compactChan chan struct{}
	wg          sync.WaitGroup
	sequenceNum uint64
}

func Open(opts Options) (*DB, error) {
	if opts.MemTableThreshold <= 0 {
		opts.MemTableThreshold = 4 * 1024 * 1024
	}
	if opts.MaxL0Files <= 0 {
		opts.MaxL0Files = 4
	}
	if err := os.MkdirAll(opts.Dir, 0755); err != nil {
		return nil, err
	}

	walPath := filepath.Join(opts.Dir, "active.wal")

	// Recover archived (rotated but possibly unflushed) WALs oldest-first,
	// then the active WAL, so newer writes override older ones.
	activeMem := memtable.NewSkipList()
	archived, _ := filepath.Glob(filepath.Join(opts.Dir, "wal_*.wal"))
	sort.Strings(archived)
	var allRecs []wal.Record
	for _, p := range append(append([]string{}, archived...), walPath) {
		recs, _, err := wal.Recover(p)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			activeMem.Put(rec.Key, rec.Value, rec.Type == wal.OpDelete)
		}
		allRecs = append(allRecs, recs...)
	}

	// Fold archived WALs into a single fresh active.wal, then delete them.
	if len(archived) > 0 {
		tmpPath := walPath + ".tmp"
		_ = os.Remove(tmpPath)
		tw, err := wal.Open(tmpPath)
		if err != nil {
			return nil, err
		}
		for _, rec := range allRecs {
			if err := tw.Append(rec.Type, rec.Key, rec.Value); err != nil {
				tw.Close()
				return nil, err
			}
		}
		if err := tw.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(tmpPath, walPath); err != nil {
			return nil, err
		}
		for _, p := range archived {
			_ = os.Remove(p)
		}
	}

	w, err := wal.Open(walPath)
	if err != nil {
		return nil, err
	}

	// Load existing SSTables from directory
	l0Tables, l1Tables, err := loadSSTables(opts.Dir)
	if err != nil {
		w.Close()
		return nil, err
	}

	db := &DB{
		opts:        opts,
		wal:         w,
		activeMem:   activeMem,
		flushChan:   make(chan *memtable.SkipList, 10),
		compactChan: make(chan struct{}, 10),
		l0Tables:    l0Tables,
		l1Tables:    l1Tables,
	}

	db.wg.Add(2)
	go db.flushWorker()
	go db.compactionWorker()

	return db, nil
}

func loadSSTables(dir string) ([]*sstable.SSTableReader, []*sstable.SSTableReader, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}

	var l0Names []string
	var l1Names []string

	for _, e := range entries {
		name := e.Name()
		if (len(name) > 4 && name[4:] == ".sst") || filepath.Ext(name) == ".sst" {
			if len(name) >= 3 && name[0:2] == "l0" {
				l0Names = append(l0Names, filepath.Join(dir, name))
			} else if len(name) >= 3 && name[0:2] == "l1" {
				l1Names = append(l1Names, filepath.Join(dir, name))
			} else {
				l0Names = append(l0Names, filepath.Join(dir, name))
			}
		}
	}

	sort.Strings(l0Names)
	sort.Strings(l1Names)

	// Crash recovery: drop partially written tables from an interrupted flush/compaction.
	if tmps, _ := filepath.Glob(filepath.Join(dir, "*.sst.tmp")); len(tmps) > 0 {
		for _, p := range tmps {
			_ = os.Remove(p)
		}
	}

	// Only the newest L1 is live; older ones were superseded by a compaction
	// that crashed before deleting its inputs. The L1 name records the newest
	// L0 stamp it already contains, so covered L0 tables are stale too.
	var covered int64
	if n := len(l1Names); n > 0 {
		for _, p := range l1Names[:n-1] {
			_ = os.Remove(p)
		}
		l1Names = l1Names[n-1:]
		covered = l1CoveredStamp(l1Names[0])
	}
	if covered > 0 {
		kept := l0Names[:0]
		for _, p := range l0Names {
			if l0Stamp(p) <= covered {
				_ = os.Remove(p)
				continue
			}
			kept = append(kept, p)
		}
		l0Names = kept
	}

	var l0 []*sstable.SSTableReader
	for _, p := range l0Names {
		r, err := sstable.OpenSSTableReader(p)
		if err != nil {
			return nil, nil, err
		}
		l0 = append(l0, r)
	}

	var l1 []*sstable.SSTableReader
	for _, p := range l1Names {
		r, err := sstable.OpenSSTableReader(p)
		if err != nil {
			return nil, nil, err
		}
		l1 = append(l1, r)
	}

	return l0, l1, nil
}

func (db *DB) Put(key, value []byte) error {
	if len(key) == 0 {
		return errors.New("key cannot be empty")
	}
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return errors.New("db is closed")
	}

	if err := db.wal.Append(wal.OpPut, key, value); err != nil {
		db.mu.Unlock()
		return err
	}

	db.activeMem.Put(key, value, false)

	if db.activeMem.Size() >= db.opts.MemTableThreshold && db.immMem == nil {
		db.immMem = db.activeMem
		db.activeMem = memtable.NewSkipList()

		// Rotate WAL
		db.wal.Close()
		oldWalPath := filepath.Join(db.opts.Dir, "active.wal")
		archiveWalPath := filepath.Join(db.opts.Dir, fmt.Sprintf("wal_%d.wal", time.Now().UnixNano()))
		os.Rename(oldWalPath, archiveWalPath)
		db.immWalPath = archiveWalPath

		var err error
		db.wal, err = wal.Open(oldWalPath)
		if err != nil {
			db.mu.Unlock()
			return err
		}

		select {
		case db.flushChan <- db.immMem:
		default:
		}
	}

	db.mu.Unlock()
	return nil
}

func (db *DB) Delete(key []byte) error {
	if len(key) == 0 {
		return errors.New("key cannot be empty")
	}
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return errors.New("db is closed")
	}

	if err := db.wal.Append(wal.OpDelete, key, nil); err != nil {
		db.mu.Unlock()
		return err
	}

	db.activeMem.Put(key, nil, true)

	if db.activeMem.Size() >= db.opts.MemTableThreshold && db.immMem == nil {
		db.immMem = db.activeMem
		db.activeMem = memtable.NewSkipList()

		db.wal.Close()
		oldWalPath := filepath.Join(db.opts.Dir, "active.wal")
		archiveWalPath := filepath.Join(db.opts.Dir, fmt.Sprintf("wal_%d.wal", time.Now().UnixNano()))
		os.Rename(oldWalPath, archiveWalPath)
		db.immWalPath = archiveWalPath

		var err error
		db.wal, err = wal.Open(oldWalPath)
		if err != nil {
			db.mu.Unlock()
			return err
		}

		select {
		case db.flushChan <- db.immMem:
		default:
		}
	}

	db.mu.Unlock()
	return nil
}

func (db *DB) Get(key []byte) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, errors.New("db is closed")
	}

	// 1. Active MemTable
	if entry, found := db.activeMem.Get(key); found {
		if entry.IsDeleted {
			return nil, os.ErrNotExist
		}
		return entry.Value, nil
	}

	// 2. Immutable MemTable
	if db.immMem != nil {
		if entry, found := db.immMem.Get(key); found {
			if entry.IsDeleted {
				return nil, os.ErrNotExist
			}
			return entry.Value, nil
		}
	}

	// 3. Level 0 SSTables (newest to oldest)
	for i := len(db.l0Tables) - 1; i >= 0; i-- {
		val, isDel, found, err := db.l0Tables[i].Get(key)
		if err == nil && found {
			if isDel {
				return nil, os.ErrNotExist
			}
			return val, nil
		}
	}

	// 4. Level 1+ SSTables
	for i := len(db.l1Tables) - 1; i >= 0; i-- {
		val, isDel, found, err := db.l1Tables[i].Get(key)
		if err == nil && found {
			if isDel {
				return nil, os.ErrNotExist
			}
			return val, nil
		}
	}

	return nil, os.ErrNotExist
}

func (db *DB) flushWorker() {
	defer db.wg.Done()
	for mem := range db.flushChan {
		if mem == nil {
			return
		}
		entries := mem.Entries()
		if len(entries) == 0 {
			db.mu.Lock()
			db.immMem = nil
			db.mu.Unlock()
			continue
		}

		sstPath := filepath.Join(db.opts.Dir, fmt.Sprintf("l0_%d.sst", time.Now().UnixNano()))
		builder, err := sstable.NewSSTableBuilder(sstPath+".tmp", len(entries))
		if err != nil {
			continue
		}
		for _, e := range entries {
			builder.Add(e.Key, e.Value, e.IsDeleted)
		}
		if err := builder.Close(); err != nil {
			_ = os.Remove(sstPath + ".tmp")
			continue
		}
		if err := os.Rename(sstPath+".tmp", sstPath); err != nil {
			_ = os.Remove(sstPath + ".tmp")
			continue
		}

		reader, err := sstable.OpenSSTableReader(sstPath)
		if err != nil {
			continue
		}

		db.mu.Lock()
		db.l0Tables = append(db.l0Tables, reader)
		db.immMem = nil
		archivedWal := db.immWalPath
		db.immWalPath = ""
		numL0 := len(db.l0Tables)
		db.mu.Unlock()

		// SSTable is durable and visible; the archived WAL is now redundant.
		if archivedWal != "" {
			_ = os.Remove(archivedWal)
		}

		if numL0 >= db.opts.MaxL0Files {
			select {
			case db.compactChan <- struct{}{}:
			default:
			}
		}
	}
}

func (db *DB) compactionWorker() {
	defer db.wg.Done()
	for range db.compactChan {
		db.mu.Lock()
		if len(db.l0Tables) < db.opts.MaxL0Files {
			db.mu.Unlock()
			continue
		}

		// Snapshot the tables being compacted. Order is oldest -> newest so that
		// newer entries (and tombstones) win in the merge: L1 first, then L0 in
		// flush order. Tables flushed after this snapshot are left untouched.
		n0 := len(db.l0Tables)
		oldL0 := append([]*sstable.SSTableReader(nil), db.l0Tables[:n0]...)
		oldL1 := append([]*sstable.SSTableReader(nil), db.l1Tables...)
		var allReaders []*sstable.SSTableReader
		allReaders = append(allReaders, oldL1...)
		allReaders = append(allReaders, oldL0...)

		db.mu.Unlock()

		// Merge sort entries across all readers
		mergedMap := make(map[string]struct {
			val       []byte
			isDeleted bool
		})

		for _, r := range allReaders {
			it := r.Iterator()
			for it.Valid() {
				k := string(it.Key())
				mergedMap[k] = struct {
					val       []byte
					isDeleted bool
				}{
					val:       it.Value(),
					isDeleted: it.IsDeleted(),
				}
				it.Next()
			}
		}

		var keys []string
		for k := range mergedMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		// Name records the newest L0 stamp merged in, for crash recovery.
		covered := l0Stamp(oldL0[len(oldL0)-1].Path())
		newSSTPath := filepath.Join(db.opts.Dir, fmt.Sprintf("l1_%d_%d.sst", time.Now().UnixNano(), covered))
		builder, err := sstable.NewSSTableBuilder(newSSTPath+".tmp", len(keys))
		if err != nil {
			continue
		}

		for _, k := range keys {
			entry := mergedMap[k]
			if entry.isDeleted {
				continue
			}
			builder.Add([]byte(k), entry.val, entry.isDeleted)
		}
		if err := builder.Close(); err != nil {
			_ = os.Remove(newSSTPath + ".tmp")
			continue
		}
		if err := os.Rename(newSSTPath+".tmp", newSSTPath); err != nil {
			_ = os.Remove(newSSTPath + ".tmp")
			continue
		}

		newReader, err := sstable.OpenSSTableReader(newSSTPath)
		if err != nil {
			continue
		}

		db.mu.Lock()
		// Close and remove only the tables that were compacted.
		for _, r := range oldL0 {
			r.Close()
			if p := r.Path(); p != "" {
				_ = os.Remove(p)
			}
		}
		for _, r := range oldL1 {
			r.Close()
			if p := r.Path(); p != "" {
				_ = os.Remove(p)
			}
		}

		// Keep any L0 tables flushed while compaction was running.
		db.l0Tables = append([]*sstable.SSTableReader(nil), db.l0Tables[n0:]...)
		db.l1Tables = []*sstable.SSTableReader{newReader}
		db.mu.Unlock()
	}
}

func (db *DB) LevelCounts() (int, int) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.l0Tables), len(db.l1Tables)
}

func (db *DB) MemTableSize() int64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.activeMem == nil {
		return 0
	}
	return db.activeMem.Size()
}

func (db *DB) Dir() string {
	return db.opts.Dir
}

func (db *DB) Close() error {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	db.mu.Unlock()

	close(db.flushChan)
	close(db.compactChan)
	db.wg.Wait()

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.wal != nil {
		db.wal.Close()
	}
	for _, r := range db.l0Tables {
		r.Close()
	}
	for _, r := range db.l1Tables {
		r.Close()
	}
	return nil
}

// Iterator support for Scan
type Iterator interface {
	Valid() bool
	Next()
	Key() []byte
	Value() []byte
}

type kvItem struct {
	key       []byte
	value     []byte
	isDeleted bool
	sourceIdx int
}

// Multi-way Merge Scan Iterator
type ScanIterator struct {
	items []kvItem
}

func (db *DB) Scan(startKey, endKey []byte) (Iterator, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.closed {
		return nil, errors.New("db is closed")
	}

	mergedMap := make(map[string]struct {
		val       []byte
		isDeleted bool
	})

	// Collect from l1 tables (oldest)
	for _, r := range db.l1Tables {
		it := r.Iterator()
		for it.Valid() {
			k := string(it.Key())
			mergedMap[k] = struct {
				val       []byte
				isDeleted bool
			}{val: it.Value(), isDeleted: it.IsDeleted()}
			it.Next()
		}
	}

	// Collect from l0 tables
	for _, r := range db.l0Tables {
		it := r.Iterator()
		for it.Valid() {
			k := string(it.Key())
			mergedMap[k] = struct {
				val       []byte
				isDeleted bool
			}{val: it.Value(), isDeleted: it.IsDeleted()}
			it.Next()
		}
	}

	// Collect from immutable memtable
	if db.immMem != nil {
		entries := db.immMem.Entries()
		for _, e := range entries {
			k := string(e.Key)
			mergedMap[k] = struct {
				val       []byte
				isDeleted bool
			}{val: e.Value, isDeleted: e.IsDeleted}
		}
	}

	// Collect from active memtable
	entries := db.activeMem.Entries()
	for _, e := range entries {
		k := string(e.Key)
		mergedMap[k] = struct {
			val       []byte
			isDeleted bool
		}{val: e.Value, isDeleted: e.IsDeleted}
	}

	var items []kvItem
	for k, v := range mergedMap {
		if v.isDeleted {
			continue
		}
		kb := []byte(k)
		if len(startKey) > 0 && bytes.Compare(kb, startKey) < 0 {
			continue
		}
		if len(endKey) > 0 && bytes.Compare(kb, endKey) >= 0 {
			continue
		}
		items = append(items, kvItem{
			key:       kb,
			value:     v.val,
			isDeleted: v.isDeleted,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return bytes.Compare(items[i].key, items[j].key) < 0
	})

	return &ScanIterator{
		items: items,
	}, nil
}

func (si *ScanIterator) Valid() bool {
	return len(si.items) > 0
}

func (si *ScanIterator) Next() {
	if len(si.items) > 0 {
		si.items = si.items[1:]
	}
}

func (si *ScanIterator) Key() []byte {
	if len(si.items) == 0 {
		return nil
	}
	return si.items[0].key
}

func (si *ScanIterator) Value() []byte {
	if len(si.items) == 0 {
		return nil
	}
	return si.items[0].value
}
