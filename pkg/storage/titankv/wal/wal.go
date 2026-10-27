// SPDX-License-Identifier: Apache-2.0
package wal

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	OpPut    byte = 0x01
	OpDelete byte = 0x02

	HeaderSize = 4 + 4 + 1 + 2 + 4 // CRC32(4) + RecLen(4) + OpType(1) + KeyLen(2) + ValLen(4) = 15 bytes
)

type Record struct {
	Type  byte
	Key   []byte
	Value []byte
}

type WAL struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	size   int64
	closed bool
}

func Open(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &WAL{
		file: file,
		path: path,
		size: fi.Size(),
	}, nil
}

func (w *WAL) Append(opType byte, key, value []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return os.ErrClosed
	}

	keyLen := uint16(len(key))
	valLen := uint32(len(value))
	payloadLen := 1 + 2 + int(keyLen) + 4 + int(valLen)
	frame := make([]byte, 4+4+payloadLen)

	binary.BigEndian.PutUint32(frame[4:8], uint32(payloadLen))
	frame[8] = opType
	binary.BigEndian.PutUint16(frame[9:11], keyLen)
	copy(frame[11:11+int(keyLen)], key)
	binary.BigEndian.PutUint32(frame[11+int(keyLen):11+int(keyLen)+4], valLen)
	copy(frame[11+int(keyLen)+4:11+int(keyLen)+4+int(valLen)], value)

	checksum := crc32.ChecksumIEEE(frame[8:])
	binary.BigEndian.PutUint32(frame[0:4], checksum)

	n, err := w.file.Write(frame)
	if err != nil {
		return err
	}
	w.size += int64(n)
	return w.file.Sync()
}

func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

// Recover reads the WAL file, verifies checksums, truncates trailing corrupt/partial writes, and returns valid records.
func Recover(path string) ([]Record, int64, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	fileSize := fi.Size()

	var records []Record
	var validOffset int64 = 0

	buf := make([]byte, 8) // CRC(4) + RecLen(4)
	offset := int64(0)

	for offset < fileSize {
		if offset+8 > fileSize {
			// Partial header, truncate
			break
		}
		_, err := file.Seek(offset, io.SeekStart)
		if err != nil {
			break
		}
		if _, err := io.ReadFull(file, buf); err != nil {
			break
		}
		expectedCRC := binary.BigEndian.Uint32(buf[0:4])
		payloadLen := binary.BigEndian.Uint32(buf[4:8])

		totalLen := 8 + int64(payloadLen)
		if offset+totalLen > fileSize {
			// Partial payload, truncate
			break
		}

		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(file, payload); err != nil {
			break
		}

		actualCRC := crc32.ChecksumIEEE(payload)
		if actualCRC != expectedCRC {
			// Checksum mismatch, corruption or partial write at tail
			break
		}

		// Parse payload: [Op:1][KeyLen:2][Key][ValLen:4][Val]
		if len(payload) < 3 {
			break
		}
		opType := payload[0]
		keyLen := binary.BigEndian.Uint16(payload[1:3])
		if 3+int(keyLen)+4 > len(payload) {
			break
		}
		key := make([]byte, keyLen)
		copy(key, payload[3:3+keyLen])

		valLenIdx := 3 + int(keyLen)
		if valLenIdx+4 > len(payload) {
			break
		}
		valLen := binary.BigEndian.Uint32(payload[valLenIdx : valLenIdx+4])
		if valLenIdx+4+int(valLen) != len(payload) {
			break
		}
		val := make([]byte, valLen)
		copy(val, payload[valLenIdx+4:valLenIdx+4+int(valLen)])

		records = append(records, Record{
			Type:  opType,
			Key:   key,
			Value: val,
		})
		offset += totalLen
		validOffset = offset
	}

	// Truncate file to last valid offset if corrupted tail exists
	if validOffset < fileSize {
		_ = file.Truncate(validOffset)
	}

	return records, validOffset, nil
}
