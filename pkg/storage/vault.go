// SPDX-License-Identifier: Apache-2.0
// Package storage provides the high-level persistence and checkpoint vault for OmniCoder-ICX,
// powered by the embedded zero-dependency TitanKV LSM-Tree storage engine.
package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/caleralabs/omnicoder-icx/pkg/storage/titankv"
)

var (
	// ErrNotFound is returned when a requested key or record does not exist.
	ErrNotFound = errors.New("storage: key not found")
	// ErrVaultClosed is returned when an operation is attempted on a closed vault.
	ErrVaultClosed = errors.New("storage: vault is closed")
)

// VaultOption configures a Vault instance.
type VaultOption func(*vaultConfig)

type vaultConfig struct {
	memTableThreshold int64
	maxL0Files        int
}

// WithMemTableThreshold sets the in-memory flush threshold in bytes.
func WithMemTableThreshold(bytes int64) VaultOption {
	return func(c *vaultConfig) {
		if bytes > 0 {
			c.memTableThreshold = bytes
		}
	}
}

// WithMaxL0Files sets the number of L0 files before triggering compaction.
func WithMaxL0Files(count int) VaultOption {
	return func(c *vaultConfig) {
		if count > 0 {
			c.maxL0Files = count
		}
	}
}

// VaultStats captures operational metrics of the embedded storage engine.
type VaultStats struct {
	Dir           string `json:"dir"`
	L0Files       int    `json:"l0_files"`
	L1Files       int    `json:"l1_files"`
	MemTableBytes int64  `json:"memtable_bytes"`
	TurnCount     int    `json:"turn_count"`
	CASCount      int    `json:"cas_count"`
	HealCount     int    `json:"heal_count"`
}

// Vault provides high-level persistence primitives for OmniCoder-ICX.
type Vault struct {
	mu     sync.RWMutex
	db     *titankv.DB
	dir    string
	closed bool
}

// OpenVault initializes or reopens a Vault in the given workspace directory.
// The store is created under filepath.Join(workspaceDir, ".omnicoder", "store").
func OpenVault(workspaceDir string, opts ...VaultOption) (*Vault, error) {
	// If workspace is inside a git repo, ensure .omnicoder/ is in .git/info/exclude
	gitExclude := filepath.Join(workspaceDir, ".git", "info", "exclude")
	if fi, err := os.Stat(gitExclude); err == nil && !fi.IsDir() {
		if content, err := os.ReadFile(gitExclude); err == nil {
			if !bytes.Contains(content, []byte(".omnicoder")) {
				_ = os.WriteFile(gitExclude, append(content, []byte("\n.omnicoder/\n")...), 0644)
			}
		}
	}

	storeDir := filepath.Join(workspaceDir, ".omnicoder", "store")
	return OpenVaultAt(storeDir, opts...)
}

// OpenVaultAt initializes or reopens a Vault directly at the specified storage directory.
func OpenVaultAt(storeDir string, opts ...VaultOption) (*Vault, error) {
	cfg := vaultConfig{
		memTableThreshold: 4 * 1024 * 1024, // 4MB default
		maxL0Files:        4,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	db, err := titankv.Open(titankv.Options{
		Dir:               storeDir,
		MemTableThreshold: cfg.memTableThreshold,
		MaxL0Files:        cfg.maxL0Files,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: failed to open titankv: %w", err)
	}

	return &Vault{
		db:  db,
		dir: storeDir,
	}, nil
}

// Dir returns the directory path of the active vault.
func (v *Vault) Dir() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dir
}

// Close gracefully flushes and closes the underlying TitanKV storage engine.
func (v *Vault) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return nil
	}
	v.closed = true
	return v.db.Close()
}

// ============================================================================
// Turn Checkpoint API (Typed Generics & JSON)
// ============================================================================

// SaveTurnState serializes and persists a turn state under key "turn:<sessionID>:<turnNum:04d>".
func SaveTurnState[T any](v *Vault, sessionID string, turnNum int, state T) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("storage: failed to marshal turn state: %w", err)
	}
	return v.SaveTurn(sessionID, turnNum, data)
}

// GetTurnState deserializes a turn state from key "turn:<sessionID>:<turnNum:04d>".
func GetTurnState[T any](v *Vault, sessionID string, turnNum int, target *T) error {
	data, err := v.GetTurn(sessionID, turnNum)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("storage: failed to unmarshal turn state: %w", err)
	}
	return nil
}

// ListTurnStates deserializes all turn states belonging to a session in sequential order.
func ListTurnStates[T any](v *Vault, sessionID string) ([]T, error) {
	rawList, err := v.ListTurns(sessionID)
	if err != nil {
		return nil, err
	}

	results := make([]T, 0, len(rawList))
	for i, raw := range rawList {
		var item T
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("storage: failed to unmarshal turn at index %d: %w", i, err)
		}
		results = append(results, item)
	}
	return results, nil
}

// SaveTurn persists a raw JSON turn payload under key "turn:<sessionID>:<turnNum:04d>".
func (v *Vault) SaveTurn(sessionID string, turnNum int, payload []byte) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return ErrVaultClosed
	}

	key := fmt.Sprintf("turn:%s:%04d", sessionID, turnNum)
	return v.db.Put([]byte(key), payload)
}

// GetTurn retrieves a raw JSON turn payload by session ID and turn number.
func (v *Vault) GetTurn(sessionID string, turnNum int) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, ErrVaultClosed
	}

	key := fmt.Sprintf("turn:%s:%04d", sessionID, turnNum)
	val, err := v.db.Get([]byte(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return val, nil
}

// ListTurns scans and retrieves all raw turn payloads for a session in strictly ascending order.
func (v *Vault) ListTurns(sessionID string) ([][]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, ErrVaultClosed
	}

	prefix := fmt.Sprintf("turn:%s:", sessionID)
	endPrefix := fmt.Sprintf("turn:%s;\x00", sessionID) // ';' is lexicographically after ':'

	it, err := v.db.Scan([]byte(prefix), []byte(endPrefix))
	if err != nil {
		return nil, err
	}

	var results [][]byte
	for it.Valid() {
		valCopy := append([]byte(nil), it.Value()...)
		results = append(results, valCopy)
		it.Next()
	}
	return results, nil
}

// ============================================================================
// Content-Addressed Storage (CAS) API
// ============================================================================

// PutCAS computes the SHA-256 hash of data and commits it under key "cas:<sha256>".
// Returns the hex-encoded SHA-256 hash.
func (v *Vault) PutCAS(data []byte) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return "", ErrVaultClosed
	}

	sum := sha256.Sum256(data)
	hashStr := hex.EncodeToString(sum[:])
	key := fmt.Sprintf("cas:%s", hashStr)

	if err := v.db.Put([]byte(key), data); err != nil {
		return "", err
	}
	return hashStr, nil
}

// GetCAS retrieves data by its SHA-256 content hash.
func (v *Vault) GetCAS(hash string) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, ErrVaultClosed
	}

	key := fmt.Sprintf("cas:%s", hash)
	val, err := v.db.Get([]byte(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return val, nil
}

// HasCAS checks if a given content hash exists in the vault.
func (v *Vault) HasCAS(hash string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return false
	}

	key := fmt.Sprintf("cas:%s", hash)
	_, err := v.db.Get([]byte(key))
	return err == nil
}

// ============================================================================
// Self-Healing Pattern Cache API
// ============================================================================

// PutHealPattern stores a successful patch solution for a given error signature.
func (v *Vault) PutHealPattern(errorFingerprint string, patch string) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return ErrVaultClosed
	}

	key := fmt.Sprintf("heal:%s", errorFingerprint)
	return v.db.Put([]byte(key), []byte(patch))
}

// GetHealPattern retrieves a cached patch solution for an error signature.
func (v *Vault) GetHealPattern(errorFingerprint string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return "", false
	}

	key := fmt.Sprintf("heal:%s", errorFingerprint)
	val, err := v.db.Get([]byte(key))
	if err != nil {
		return "", false
	}
	return string(val), true
}

// ============================================================================
// Milestone Index API
// ============================================================================

// SaveMilestone persists a milestone record for a session and turn number.
func (v *Vault) SaveMilestone(sessionID string, turnNum int, milestoneData []byte) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return ErrVaultClosed
	}

	key := fmt.Sprintf("milestone:%s:%04d", sessionID, turnNum)
	return v.db.Put([]byte(key), milestoneData)
}

// ListMilestones retrieves all milestones for a session in sequential order.
func (v *Vault) ListMilestones(sessionID string) ([][]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, ErrVaultClosed
	}

	prefix := fmt.Sprintf("milestone:%s:", sessionID)
	endPrefix := fmt.Sprintf("milestone:%s;\x00", sessionID)

	it, err := v.db.Scan([]byte(prefix), []byte(endPrefix))
	if err != nil {
		return nil, err
	}

	var results [][]byte
	for it.Valid() {
		results = append(results, append([]byte(nil), it.Value()...))
		it.Next()
	}
	return results, nil
}

// ============================================================================
// Raw Key-Value Interface
// ============================================================================

// Put stores a raw key-value pair.
func (v *Vault) Put(key, value []byte) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return ErrVaultClosed
	}
	return v.db.Put(key, value)
}

// Get retrieves a raw key-value pair.
func (v *Vault) Get(key []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, ErrVaultClosed
	}
	val, err := v.db.Get(key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return val, nil
}

// Delete removes a key.
func (v *Vault) Delete(key []byte) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return ErrVaultClosed
	}
	return v.db.Delete(key)
}

// Scan creates a multi-way merge range iterator over [startKey, endKey).
func (v *Vault) Scan(startKey, endKey []byte) (titankv.Iterator, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, ErrVaultClosed
	}
	return v.db.Scan(startKey, endKey)
}

// Stats returns operational metrics and counts for the storage vault.
func (v *Vault) Stats() VaultStats {
	v.mu.RLock()
	defer v.mu.RUnlock()

	l0, l1 := v.db.LevelCounts()
	memSize := v.db.MemTableSize()

	stats := VaultStats{
		Dir:           v.dir,
		L0Files:       l0,
		L1Files:       l1,
		MemTableBytes: memSize,
	}

	// Count turns, CAS objects, and heal entries via scan
	it, err := v.db.Scan(nil, nil)
	if err == nil {
		for it.Valid() {
			k := it.Key()
			if bytes.HasPrefix(k, []byte("turn:")) {
				stats.TurnCount++
			} else if bytes.HasPrefix(k, []byte("cas:")) {
				stats.CASCount++
			} else if bytes.HasPrefix(k, []byte("heal:")) {
				stats.HealCount++
			}
			it.Next()
		}
	}

	return stats
}
