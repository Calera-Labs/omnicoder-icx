// SPDX-License-Identifier: Apache-2.0
package devops_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestASTCache_BasicOperations(t *testing.T) {
	cache := devops.GetASTCache()
	cache.InvalidateAll()

	if cache.Count() != 0 {
		t.Fatalf("expected empty cache, got %d", cache.Count())
	}

	tags := []devops.SymbolTag{
		{File: "main.go", Kind: "func", Name: "Hello", Signature: "func Hello()"},
	}
	refs := map[string]bool{"fmt": true, "Println": true}
	now := time.Now()

	cache.Put("main.go", now, 128, tags, refs)

	if cache.Count() != 1 {
		t.Fatalf("expected 1 cached entry, got %d", cache.Count())
	}

	// Hit with matching modTime and size
	cachedTags, cachedRefs, ok := cache.Get("main.go", now, 128)
	if !ok || len(cachedTags) != 1 || cachedTags[0].Name != "Hello" || !cachedRefs["Println"] {
		t.Fatalf("cache hit failed or data mismatch: ok=%v, tags=%+v, refs=%+v", ok, cachedTags, cachedRefs)
	}

	// Miss on modified size
	_, _, ok = cache.Get("main.go", now, 256)
	if ok {
		t.Fatalf("expected cache miss on size change")
	}

	// Miss on modified time
	_, _, ok = cache.Get("main.go", now.Add(time.Second), 128)
	if ok {
		t.Fatalf("expected cache miss on modTime change")
	}

	// Invalidate
	cache.Invalidate("main.go")
	if cache.Count() != 0 {
		t.Fatalf("expected 0 cached entries after invalidate, got %d", cache.Count())
	}
}

func TestASTCache_PrewarmAndRepoMapAcceleration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "omnicoder_prewarm_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create sample Go and TS files
	goFile := filepath.Join(tmpDir, "calculator.go")
	_ = os.WriteFile(goFile, []byte("package calc\n\ntype Calculator struct{}\nfunc (c *Calculator) Add(a, b int) int { return a + b }\n"), 0644)

	tsFile := filepath.Join(tmpDir, "types.ts")
	_ = os.WriteFile(tsFile, []byte("export interface User {\n  id: string;\n  name: string;\n}\nexport function getUser(): User { return { id: '1', name: 'Alice' }; }\n"), 0644)

	cache := devops.GetASTCache()
	cache.InvalidateAll()

	// 1. Run PrewarmASTCache
	cachedCount := devops.PrewarmASTCache(tmpDir)
	if cachedCount < 2 {
		t.Fatalf("expected at least 2 files prewarmed, got %d (cache count: %d)", cachedCount, cache.Count())
	}

	// 2. SynthesizeRepoMap should hit cache directly
	start := time.Now()
	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  512,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed: %v", err)
	}
	if res.SymbolsFound < 2 {
		t.Fatalf("expected at least 2 symbols found, got %d", res.SymbolsFound)
	}

	t.Logf("Cached SynthesizeRepoMap completed in %v (< 20ms target)", elapsed)
}
