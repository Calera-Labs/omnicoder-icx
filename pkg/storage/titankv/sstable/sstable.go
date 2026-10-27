// SPDX-License-Identifier: Apache-2.0
package sstable

import (
	"bytes"
	"encoding/binary"
	"hash/fnv"
	"io"
	"os"
	"sort"
)

const (
	MagicBytes    = 0x544954414E535354 // "TITANSST"
	IndexInterval = 16                 // sparse index every 16 keys
)

// BloomFilter implementation using FNV-1a double hashing
type BloomFilter struct {
	bits []byte
	m    uint32 // number of bits
	k    uint32 // number of hash functions
}

func NewBloomFilter(n int) *BloomFilter {
	if n < 1 {
		n = 1
	}
	// 10 bits per key for ~1% false positive rate
	m := uint32(n * 10)
	if m < 64 {
		m = 64
	}
	// Round up to a whole number of bytes: DecodeBloomFilter derives m from
	// len(bits)*8, so encoder and decoder must agree on the modulus.
	m = (m + 7) / 8 * 8
	k := uint32(7)
	bytesLen := (m + 7) / 8
	return &BloomFilter{
		bits: make([]byte, bytesLen),
		m:    m,
		k:    k,
	}
}

func (bf *BloomFilter) add(key []byte) {
	h1, h2 := hash1(key), hash2(key)
	for i := uint32(0); i < bf.k; i++ {
		bitIdx := (h1 + i*h2) % bf.m
		bf.bits[bitIdx/8] |= (1 << (bitIdx % 8))
	}
}

func (bf *BloomFilter) MayContain(key []byte) bool {
	if bf == nil || len(bf.bits) == 0 {
		return true
	}
	h1, h2 := hash1(key), hash2(key)
	for i := uint32(0); i < bf.k; i++ {
		bitIdx := (h1 + i*h2) % bf.m
		if (bf.bits[bitIdx/8] & (1 << (bitIdx % 8))) == 0 {
			return false
		}
	}
	return true
}

func hash1(key []byte) uint32 {
	h := fnv.New32a()
	h.Write(key)
	return h.Sum32()
}

func hash2(key []byte) uint32 {
	h := fnv.New32()
	h.Write(key)
	return h.Sum32()
}

func (bf *BloomFilter) Bytes() []byte {
	return bf.bits
}

func DecodeBloomFilter(data []byte) *BloomFilter {
	m := uint32(len(data) * 8)
	return &BloomFilter{
		bits: data,
		m:    m,
		k:    7,
	}
}

// IndexEntry represents a sparse index point
type IndexEntry struct {
	Key         []byte
	Offset      uint64
	BlockLength uint32
}

// SSTableBuilder creates an SSTable file on disk
type SSTableBuilder struct {
	file         *os.File
	path         string
	dataOffset   uint64
	indexEntries []IndexEntry
	bloom        *BloomFilter
	lastKey      []byte
	count        int
}

func NewSSTableBuilder(path string, estimatedKeys int) (*SSTableBuilder, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &SSTableBuilder{
		file:  file,
		path:  path,
		bloom: NewBloomFilter(estimatedKeys),
	}, nil
}

func (b *SSTableBuilder) Add(key []byte, value []byte, isDeleted bool) error {
	b.bloom.add(key)

	if b.count%IndexInterval == 0 {
		b.indexEntries = append(b.indexEntries, IndexEntry{
			Key:         append([]byte(nil), key...),
			Offset:      b.dataOffset,
			BlockLength: 0,
		})
	}
	b.count++
	b.lastKey = append([]byte(nil), key...)

	keyLen := uint16(len(key))
	valLen := uint32(len(value))
	headerLen := 2 + 4 + 1
	recordSize := headerLen + int(keyLen) + int(valLen)

	buf := make([]byte, recordSize)
	binary.BigEndian.PutUint16(buf[0:2], keyLen)
	binary.BigEndian.PutUint32(buf[2:6], valLen)
	if isDeleted {
		buf[6] = 0x01
	} else {
		buf[6] = 0x00
	}
	copy(buf[7:7+keyLen], key)
	copy(buf[7+keyLen:], value)

	if len(b.indexEntries) > 0 {
		lastIdx := &b.indexEntries[len(b.indexEntries)-1]
		if lastIdx.Offset <= b.dataOffset {
			lastIdx.BlockLength = uint32(b.dataOffset - lastIdx.Offset + uint64(recordSize))
		}
	}

	n, err := b.file.Write(buf)
	if err != nil {
		return err
	}
	b.dataOffset += uint64(n)
	return nil
}

func (b *SSTableBuilder) Close() error {
	indexOffset := b.dataOffset
	var indexBuf bytes.Buffer
	binary.Write(&indexBuf, binary.BigEndian, uint32(len(b.indexEntries)))
	for _, ie := range b.indexEntries {
		binary.Write(&indexBuf, binary.BigEndian, uint16(len(ie.Key)))
		indexBuf.Write(ie.Key)
		binary.Write(&indexBuf, binary.BigEndian, ie.Offset)
		binary.Write(&indexBuf, binary.BigEndian, ie.BlockLength)
	}
	indexBytes := indexBuf.Bytes()
	n, err := b.file.Write(indexBytes)
	if err != nil {
		b.file.Close()
		return err
	}
	indexLen := uint64(n)
	indexEndOffset := indexOffset + indexLen

	bloomOffset := indexEndOffset
	bloomBytes := b.bloom.Bytes()
	var bloomHeader [4]byte
	binary.BigEndian.PutUint32(bloomHeader[:], uint32(len(bloomBytes)))

	if _, err := b.file.Write(bloomHeader[:]); err != nil {
		b.file.Close()
		return err
	}
	if _, err := b.file.Write(bloomBytes); err != nil {
		b.file.Close()
		return err
	}
	_ = uint64(4 + len(bloomBytes))

	var footer [32]byte
	binary.BigEndian.PutUint64(footer[0:8], indexOffset)
	binary.BigEndian.PutUint64(footer[8:16], indexLen)
	binary.BigEndian.PutUint64(footer[16:24], bloomOffset)
	binary.BigEndian.PutUint64(footer[24:32], uint64(MagicBytes))

	if _, err := b.file.Write(footer[:]); err != nil {
		b.file.Close()
		return err
	}

	return b.file.Close()
}

type SSTableReader struct {
	file         *os.File
	path         string
	indexEntries []IndexEntry
	bloom        *BloomFilter
	indexOffset  uint64
	indexLen     uint64
	bloomOffset  uint64
	bloomLen     uint64
}

func (r *SSTableReader) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

func OpenSSTableReader(path string) (*SSTableReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	fileSize := fi.Size()
	if fileSize < 32 {
		file.Close()
		return nil, io.ErrUnexpectedEOF
	}

	footerBuf := make([]byte, 32)
	if _, err := file.Seek(fileSize-32, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := io.ReadFull(file, footerBuf); err != nil {
		file.Close()
		return nil, err
	}

	indexOffset := binary.BigEndian.Uint64(footerBuf[0:8])
	indexLen := binary.BigEndian.Uint64(footerBuf[8:16])
	bloomOffset := binary.BigEndian.Uint64(footerBuf[16:24])

	magic := binary.BigEndian.Uint64(footerBuf[24:32])
	if magic != MagicBytes {
		file.Close()
		return nil, os.ErrInvalid
	}

	// Read Bloom Filter header (4 bytes length)
	var bloomHeader [4]byte
	if _, err := file.Seek(int64(bloomOffset), io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := io.ReadFull(file, bloomHeader[:]); err != nil {
		file.Close()
		return nil, err
	}
	bLen := binary.BigEndian.Uint32(bloomHeader[:])

	bloomBuf := make([]byte, bLen)
	if _, err := io.ReadFull(file, bloomBuf); err != nil {
		file.Close()
		return nil, err
	}
	bloom := DecodeBloomFilter(bloomBuf)

	// Read Index Block
	indexData := make([]byte, indexLen)
	if _, err := file.Seek(int64(indexOffset), io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := io.ReadFull(file, indexData); err != nil {
		file.Close()
		return nil, err
	}

	r := bytes.NewReader(indexData)
	var numIndices uint32
	binary.Read(r, binary.BigEndian, &numIndices)

	var indexEntries []IndexEntry
	for i := uint32(0); i < numIndices; i++ {
		var keyLen uint16
		binary.Read(r, binary.BigEndian, &keyLen)
		keyBytes := make([]byte, keyLen)
		io.ReadFull(r, keyBytes)
		var offset uint64
		var blockLen uint32
		binary.Read(r, binary.BigEndian, &offset)
		binary.Read(r, binary.BigEndian, &blockLen)

		indexEntries = append(indexEntries, IndexEntry{
			Key:         keyBytes,
			Offset:      offset,
			BlockLength: blockLen,
		})
	}

	return &SSTableReader{
		file:         file,
		path:         path,
		indexEntries: indexEntries,
		bloom:        bloom,
		indexOffset:  indexOffset,
		indexLen:     indexLen,
		bloomOffset:  bloomOffset,
		bloomLen:     uint64(4 + bLen),
	}, nil
}

func (r *SSTableReader) Get(key []byte) ([]byte, bool, bool, error) {
	if !r.bloom.MayContain(key) {
		return nil, false, false, nil
	}

	idx := sort.Search(len(r.indexEntries), func(i int) bool {
		return bytes.Compare(r.indexEntries[i].Key, key) > 0
	})

	var targetEntry *IndexEntry
	if idx > 0 {
		targetEntry = &r.indexEntries[idx-1]
	} else if len(r.indexEntries) > 0 && bytes.Compare(r.indexEntries[0].Key, key) <= 0 {
		targetEntry = &r.indexEntries[0]
	}

	if targetEntry == nil {
		return nil, false, false, nil
	}

	currOffset := targetEntry.Offset
	for currOffset < r.indexOffset {
		kBytes, vBytes, isDel, size, err := r.readRecordAt(int64(currOffset))
		if err != nil {
			break
		}
		currOffset += uint64(size)

		cmp := bytes.Compare(kBytes, key)
		if cmp == 0 {
			return vBytes, isDel, true, nil
		}
		if cmp > 0 {
			break
		}
	}

	// Key not present in this table (e.g. Bloom filter false positive).
	return nil, false, false, nil
}

// readRecordAt decodes one data record at the given offset using ReadAt, which
// does not touch the shared file offset and is safe for concurrent use.
func (r *SSTableReader) readRecordAt(offset int64) (key, val []byte, isDel bool, size int64, err error) {
	hdr := make([]byte, 7)
	if _, err = r.file.ReadAt(hdr, offset); err != nil {
		return nil, nil, false, 0, err
	}
	keyLen := int64(binary.BigEndian.Uint16(hdr[0:2]))
	valLen := int64(binary.BigEndian.Uint32(hdr[2:6]))
	isDel = hdr[6] == 0x01
	body := make([]byte, keyLen+valLen)
	if len(body) > 0 {
		if _, err = r.file.ReadAt(body, offset+7); err != nil {
			return nil, nil, false, 0, err
		}
	}
	return body[:keyLen], body[keyLen:], isDel, 7 + keyLen + valLen, nil
}

func (r *SSTableReader) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

type SSTIterator struct {
	reader *SSTableReader
	offset int64
	maxLen int64
	curr   struct {
		key       []byte
		value     []byte
		isDeleted bool
		valid     bool
	}
}

func (r *SSTableReader) Iterator() *SSTIterator {
	it := &SSTIterator{
		reader: r,
		offset: 0,
		maxLen: int64(r.indexOffset),
	}
	it.Next()
	return it
}

func (it *SSTIterator) Valid() bool {
	return it.curr.valid
}

func (it *SSTIterator) Next() {
	if it.offset >= it.maxLen {
		it.curr.valid = false
		return
	}

	kBytes, vBytes, isDel, size, err := it.reader.readRecordAt(it.offset)
	if err != nil {
		it.curr.valid = false
		return
	}

	it.offset += size
	it.curr.key = kBytes
	it.curr.value = vBytes
	it.curr.isDeleted = isDel
	it.curr.valid = true
}

func (it *SSTIterator) Key() []byte {
	return it.curr.key
}

func (it *SSTIterator) Value() []byte {
	return it.curr.value
}

func (it *SSTIterator) IsDeleted() bool {
	return it.curr.isDeleted
}
