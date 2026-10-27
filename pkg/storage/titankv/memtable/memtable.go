// SPDX-License-Identifier: Apache-2.0
package memtable

import (
	"bytes"
	"math/rand"
	"sync"
	"sync/atomic"
)

const (
	maxHeight = 12
	p         = 0.5
)

type Entry struct {
	Key       []byte
	Value     []byte
	IsDeleted bool
}

type Node struct {
	key       []byte
	value     []byte
	isDeleted bool
	forward   []*Node
}

type SkipList struct {
	mu     sync.RWMutex
	head   *Node
	height int
	size   int64 // approximate memory size in bytes
}

func NewSkipList() *SkipList {
	return &SkipList{
		head:   newNode(nil, nil, false, maxHeight),
		height: 1,
	}
}

func newNode(key, value []byte, isDeleted bool, height int) *Node {
	return &Node{
		key:       key,
		value:     value,
		isDeleted: isDeleted,
		forward:   make([]*Node, height),
	}
}

func randomHeight() int {
	h := 1
	for h < maxHeight && rand.Float64() < p {
		h++
	}
	return h
}

func (s *SkipList) Put(key, value []byte, isDeleted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	update := make([]*Node, maxHeight)
	curr := s.head

	for i := s.height - 1; i >= 0; i-- {
		for curr.forward[i] != nil && bytes.Compare(curr.forward[i].key, key) < 0 {
			curr = curr.forward[i]
		}
		update[i] = curr
	}

	curr = curr.forward[0]

	// Estimate memory overhead: key len + val len + 32 bytes struct overhead
	entrySize := int64(len(key) + len(value) + 32)

	if curr != nil && bytes.Equal(curr.key, key) {
		// Update existing
		oldSize := int64(len(curr.key) + len(curr.value) + 32)
		curr.value = value
		curr.isDeleted = isDeleted
		atomic.AddInt64(&s.size, entrySize-oldSize)
		return
	}

	h := randomHeight()
	if h > s.height {
		for i := s.height; i < h; i++ {
			update[i] = s.head
		}
		s.height = h
	}

	newNode := newNode(key, value, isDeleted, h)
	for i := 0; i < h; i++ {
		newNode.forward[i] = update[i].forward[i]
		update[i].forward[i] = newNode
	}

	atomic.AddInt64(&s.size, entrySize)
}

func (s *SkipList) Get(key []byte) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	curr := s.head
	for i := s.height - 1; i >= 0; i-- {
		for curr.forward[i] != nil && bytes.Compare(curr.forward[i].key, key) < 0 {
			curr = curr.forward[i]
		}
	}
	curr = curr.forward[0]
	if curr != nil && bytes.Equal(curr.key, key) {
		return Entry{Key: curr.key, Value: curr.value, IsDeleted: curr.isDeleted}, true
	}
	return Entry{}, false
}

func (s *SkipList) Size() int64 {
	return atomic.LoadInt64(&s.size)
}

// Iterator for traversal / scanning
type Iterator struct {
	node *Node
}

func (s *SkipList) Iterator() *Iterator {
	s.mu.RLock()
	// To be fully thread-safe for scans over immutable memtables or locked active memtables,
	// we traverse the forward pointers. Note: for active memtables, callers should use Snapshot or locked scan.
	return &Iterator{node: s.head.forward[0]}
}

func (it *Iterator) Valid() bool {
	return it.node != nil
}

func (it *Iterator) Next() {
	if it.node != nil {
		it.node = it.node.forward[0]
	}
}

func (it *Iterator) Key() []byte {
	if it.node == nil {
		return nil
	}
	return it.node.key
}

func (it *Iterator) Value() []byte {
	if it.node == nil {
		return nil
	}
	return it.node.value
}

func (it *Iterator) IsDeleted() bool {
	if it.node == nil {
		return false
	}
	return it.node.isDeleted
}

// Snapshot returns all entries sorted for flushing
func (s *SkipList) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var entries []Entry
	curr := s.head.forward[0]
	for curr != nil {
		entries = append(entries, Entry{
			Key:       curr.key,
			Value:     curr.value,
			IsDeleted: curr.isDeleted,
		})
		curr = curr.forward[0]
	}
	return entries
}
