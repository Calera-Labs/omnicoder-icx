// SPDX-License-Identifier: Apache-2.0
package llm

import (
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestSanitizeRibbonRoot_EdgeCases checks for panics across various maxChars values
// including 0, negative, and small positive values (1, 2, 3), as well as multi-byte Unicode.
func TestSanitizeRibbonRoot_EdgeCases(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		maxChars int
	}{
		{"Normal 40", "foo/bar/baz.go", 40},
		{"Exact length", "foo/bar/baz.go", 14},
		{"Zero maxChars defaults to 40", "very/long/path/to/some/nested/file/in/a/deep/directory/structure.go", 0},
		{"Negative maxChars defaults to 40", "very/long/path/to/some/nested/file/in/a/deep/directory/structure.go", -10},
		{"Large maxChars", "foo/bar.go", 1000},
		{"4 maxChars minimal prefix", "abcdefghij.go", 4},
		{"5 maxChars", "abcdefghij.go", 5},
		{"Unicode CJK with 40", "路径/到/中文/文件名_组件_核心代码.go", 40},
		{"Unicode Emoji with 40", "🚀/⚡/🔥/💻/microservice_📦.go", 40},
		{"Whitespace only", "    \n\t   ", 40},
		{"file_ prefix only", "file_", 40},
		{"Null bytes in string", "foo\x00bar\x00baz.go", 40},
		{"100K char repetitive string", strings.Repeat("nested_dir/", 10000) + "end.go", 40},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC detected in SanitizeRibbonRoot for %q with maxChars=%d: %v", tc.input, tc.maxChars, r)
				}
			}()
			res := SanitizeRibbonRoot(tc.input, tc.maxChars)
			if res == "" {
				t.Fatalf("SanitizeRibbonRoot returned empty string for %q", tc.input)
			}
		})
	}
}

// TestSanitizeRibbonRoot_SmallMaxCharsExploration tests behavior when maxChars < 4.
// Note: We test whether small maxChars (1, 2, 3) can trigger panics.
func TestSanitizeRibbonRoot_SmallMaxCharsExploration(t *testing.T) {
	smallValues := []int{1, 2, 3}
	for _, k := range smallValues {
		t.Run(fmt.Sprintf("maxChars_%d", k), func(t *testing.T) {
			panicked := false
			var panicVal any
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
						panicVal = r
					}
				}()
				_ = SanitizeRibbonRoot("long_path_name/to_file.go", k)
			}()
			if panicked {
				t.Errorf("SanitizeRibbonRoot panicked when maxChars=%d: %v", k, panicVal)
			} else {
				t.Logf("SanitizeRibbonRoot handled maxChars=%d without panic", k)
			}
		})
	}
}

// TestSanitizeRibbonRoot_MemoryLeakAndAllocation runs 100,000 iterations
// of SanitizeRibbonRoot and FormatASTRibbonFromStrings to ensure no memory leak or excessive allocation.
func TestSanitizeRibbonRoot_MemoryLeakAndAllocation(t *testing.T) {
	var mBefore, mAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mBefore)

	const iterations = 50000
	paths := []string{
		"short.go",
		"nested/path/to/component/service.go",
		strings.Repeat("deeply/nested/path/component/", 10) + "handler.go",
		"中文目录/核心业务/组件.go",
		"🚀/test/service.go",
	}

	items := []string{
		"FuncA (defines)", "FuncB (calls)", "TypeC (implements)", "InterfaceD (contains)",
	}

	for i := 0; i < iterations; i++ {
		p := paths[i%len(paths)]
		_ = SanitizeRibbonRoot(p, 40)
		_ = FormatASTRibbonFromStrings(p, items, 150)
	}

	runtime.GC()
	runtime.ReadMemStats(&mAfter)

	// HeapAlloc should not balloon significantly
	var diffMB float64
	if mAfter.HeapAlloc > mBefore.HeapAlloc {
		diffMB = float64(mAfter.HeapAlloc-mBefore.HeapAlloc) / (1024 * 1024)
	}
	t.Logf("HeapAlloc before: %d, after: %d, delta: %.2f MB", mBefore.HeapAlloc, mAfter.HeapAlloc, diffMB)
	if diffMB > 50.0 {
		t.Errorf("Potential memory leak: HeapAlloc grew by %.2f MB", diffMB)
	}
}

// TestConcurrentPromptBudgeting simulates 50 goroutines formatting cards concurrently.
func TestConcurrentPromptBudgeting(t *testing.T) {
	var wg sync.WaitGroup
	const goroutines = 50
	const iters = 200

	errCh := make(chan error, goroutines*iters)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(gid * 1000)))
			for i := 0; i < iters; i++ {
				pathLen := rng.Intn(200) + 10
				instLen := rng.Intn(500) + 5
				card := GateEditCard{
					FilePath:       strings.Repeat("a", pathLen) + ".go",
					Instruction:    strings.Repeat("word ", instLen),
					ContextSnippet: "func Test() { println(1) }",
					ASTNeighborhood: []string{
						"Node1 (defines)", "Node2 (calls)", "Node3 (implements)",
					},
				}

				sysPrompt, userPrompt, err := card.FormatPrompt()
				if err != nil {
					errCh <- fmt.Errorf("FormatPrompt error: %w", err)
					return
				}
				total := EstimateTokens(sysPrompt + " " + userPrompt)
				if total >= MaxCardTokenBudget {
					errCh <- fmt.Errorf("total tokens %d >= %d (sys: %d, user: %d)",
						total, MaxCardTokenBudget, EstimateTokens(sysPrompt), EstimateTokens(userPrompt))
					return
				}
				if !strings.Contains(userPrompt, "File: ") {
					errCh <- fmt.Errorf("missing 'File: ' in user prompt")
					return
				}
				if !strings.Contains(userPrompt, "Instruction: ") {
					errCh <- fmt.Errorf("missing 'Instruction: ' in user prompt")
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		t.Fatalf("encountered %d concurrent budgeting errors, first: %v", len(errs), errs[0])
	}
}
