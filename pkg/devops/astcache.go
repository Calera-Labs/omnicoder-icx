// SPDX-License-Identifier: Apache-2.0
// Package devops provides deterministic DevOps primitives for OmniCoder-ICX.
// astcache.go implements an in-memory concurrent cache and non-blocking background
// pre-warming engine for workspace AST symbols and cross-file reference graphs.

package devops

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// FileASTEntry stores cached symbol definitions and references for a single source file.
type FileASTEntry struct {
	ModTime  time.Time
	FileSize int64
	Tags     []SymbolTag
	Refs     map[string]bool
}

// ASTCache provides an in-memory concurrent cache for parsed AST symbols and cross-file references.
type ASTCache struct {
	mu      sync.RWMutex
	entries map[string]FileASTEntry
}

var globalASTCache = &ASTCache{
	entries: make(map[string]FileASTEntry),
}

// GetASTCache returns the global singleton ASTCache.
func GetASTCache() *ASTCache {
	return globalASTCache
}

// Get retrieves cached symbols and references for a file if ModTime and FileSize match.
func (c *ASTCache) Get(relPath string, modTime time.Time, size int64) ([]SymbolTag, map[string]bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, exists := c.entries[relPath]
	if !exists {
		return nil, nil, false
	}
	if !entry.ModTime.Equal(modTime) || entry.FileSize != size {
		return nil, nil, false
	}
	return entry.Tags, entry.Refs, true
}

// Put caches extracted symbols and references for a file.
func (c *ASTCache) Put(relPath string, modTime time.Time, size int64, tags []SymbolTag, refs map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[relPath] = FileASTEntry{
		ModTime:  modTime,
		FileSize: size,
		Tags:     tags,
		Refs:     refs,
	}
}

// Invalidate removes a file entry from the cache.
func (c *ASTCache) Invalidate(relPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clean := filepath.ToSlash(filepath.Clean(relPath))
	delete(c.entries, clean)
	delete(c.entries, relPath)
}

// InvalidateAll clears all cached entries.
func (c *ASTCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]FileASTEntry)
}

// Count returns the number of files currently cached.
func (c *ASTCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// PrewarmASTCache eagerly discovers and indexes code files across the workspace in the background.
// It is non-blocking, cooperatively yields CPU via runtime.Gosched(), and is safe to run concurrently.
func PrewarmASTCache(workspaceDir string) int {
	if workspaceDir == "" || workspaceDir == "." {
		if wd, err := os.Getwd(); err == nil {
			workspaceDir = wd
		}
	}

	count := 0
	_ = filepath.WalkDir(workspaceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if ignoredDirectories[base] || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs":
			// Process supported code file
		default:
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		rel, relErr := filepath.Rel(workspaceDir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		// Check if already in cache and fresh
		if _, _, ok := globalASTCache.Get(rel, info.ModTime(), info.Size()); ok {
			return nil
		}

		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		tags, refs := extractFileSymbolsAndRefs(rel, string(raw), ext)
		globalASTCache.Put(rel, info.ModTime(), info.Size(), tags, refs)
		count++

		// Cooperative yield every 20 files to keep background pre-warming feather-light
		if count%20 == 0 {
			runtime.Gosched()
		}
		return nil
	})

	return count
}
