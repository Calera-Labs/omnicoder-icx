// SPDX-License-Identifier: Apache-2.0

package canonical_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

// utf16OracleLess is the ground-truth reference implementation of RFC 8785 section 3.2.3
// using Go's standard library unicode/utf16.Encode.
func utf16OracleLess(a, b string) bool {
	uA := utf16.Encode([]rune(a))
	uB := utf16.Encode([]rune(b))
	minLen := len(uA)
	if len(uB) < minLen {
		minLen = len(uB)
	}
	for i := 0; i < minLen; i++ {
		if uA[i] != uB[i] {
			return uA[i] < uB[i]
		}
	}
	return len(uA) < len(uB)
}

// ============================================================================
// 1. FUZZ RFC 8785 KEY SORTING WITH UNICODE SURROGATES
// ============================================================================

func TestFuzz_RFC8785_UnicodeSurrogatesAndBMP(t *testing.T) {
	// Key test vectors:
	// 1. Supplementary character U+10000 (0xD800 0xDC00 in UTF-16) vs BMP Private Use U+E000 (0xE000 in UTF-16).
	//    Under UTF-8 bytes: U+10000 is [0xF0, 0x90, 0x80, 0x80] which sorts AFTER U+E000 [0xEE, 0x80, 0x80].
	//    Under UTF-16 code units: 0xD800 < 0xE000, so U+10000 MUST sort BEFORE U+E000.
	// 2. High surrogate equality, differing low surrogate:
	//    U+10000 (0xD800 0xDC00) vs U+10001 (0xD800 0xDC01).
	// 3. Different supplementary planes:
	//    SMP (Plane 1): U+1F680 (🚀, 0xD83D 0xDE80)
	//    SIP (Plane 2): U+20000 (𠀀, 0xD840 0xDC00)
	//    TIP (Plane 3): U+30000 (𰀀, 0xD880 0xDC00)
	// 4. BMP characters near boundary:
	//    U+D7FF (last character before surrogate range)
	//    U+E000 (first character after surrogate range)
	//    U+FFFF (last BMP character)

	keys := []string{
		"\U00010000", // 0xD800 0xDC00
		"\U00010001", // 0xD800 0xDC01
		"\U000103FF", // 0xD800 0xDFFF
		"\U00010400", // 0xD801 0xDC00
		"\uD7FF",     // 0xD7FF
		"\uE000",     // 0xE000
		"\uFFFF",     // 0xFFFF
		"🚀",          // U+1F680 (0xD83D 0xDE80)
		"🛸",          // U+1F6F8 (0xD83D 0xDEF8)
		"𠀀",          // U+20000 (0xD840 0xDC00)
		"a",
		"z",
		"~",
		"\u007F",
		"\u0080",
		"\u07FF",
		"\u0800",
	}

	m := make(map[string]any)
	for i, k := range keys {
		m[k] = i
	}

	out, err := canonical.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal failed on surrogate test map: %v", err)
	}

	// Parse JSON back to verify valid JSON structure
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("Marshal produced invalid JSON: %v, raw: %s", err, string(out))
	}

	// Extract serialized keys in encounter order from output
	serialized := string(out)
	var serializedKeys []string
	dec := json.NewDecoder(strings.NewReader(serialized))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("expected '{', got %v", tok)
	}
	for dec.More() {
		kTok, err := dec.Token()
		if err != nil {
			break
		}
		serializedKeys = append(serializedKeys, kTok.(string))
		// skip value
		var v any
		_ = dec.Decode(&v)
	}

	// Verify that serialized keys are strictly in UTF-16 code unit order according to the oracle
	for i := 0; i < len(serializedKeys)-1; i++ {
		k1 := serializedKeys[i]
		k2 := serializedKeys[i+1]
		if !utf16OracleLess(k1, k2) {
			t.Errorf("KEY_ORDER_VIOLATION: key[%d] %q (runes: %U) should sort before key[%d] %q (runes: %U)",
				i, k1, []rune(k1), i+1, k2, []rune(k2))
		}
	}
}

func TestFuzz_RFC8785_RandomizedFuzzer(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// Rune pools: ASCII, BMP, Surrogates, Symbols
	runePool := []rune{
		'a', 'b', 'c', '1', '2', '_', '-', ':', '/',
		'\u00e9', '\u00fc', '\u4e16', '\u754c', // accented, CJK
		'\uD7FF', '\uE000', '\uFFFF', // boundary BMP
		0x10000, 0x10001, 0x1F600, 0x1F680, 0x20000, 0x10FFFF, // Supplementary planes
	}

	for iter := 0; iter < 100; iter++ {
		numKeys := 10 + rng.Intn(40)
		m := make(map[string]any)
		for k := 0; k < numKeys; k++ {
			keyLen := 1 + rng.Intn(6)
			runes := make([]rune, keyLen)
			for l := 0; l < keyLen; l++ {
				runes[l] = runePool[rng.Intn(len(runePool))]
			}
			m[string(runes)] = k
		}

		out, err := canonical.Marshal(m)
		if err != nil {
			t.Fatalf("iter %d: Marshal failed: %v", iter, err)
		}

		// Decode and verify ordering
		dec := json.NewDecoder(strings.NewReader(string(out)))
		_, _ = dec.Token() // '{'
		var prevKey string
		first := true
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				break
			}
			currKey := kTok.(string)
			var v any
			_ = dec.Decode(&v)

			if !first {
				if !utf16OracleLess(prevKey, currKey) && prevKey != currKey {
					t.Fatalf("iter %d: key ordering violation: %q vs %q", iter, prevKey, currKey)
				}
			}
			prevKey = currKey
			first = false
		}
	}
}

// ============================================================================
// 2. NESTED STRUCTURES & COMPLEX TYPES
// ============================================================================

func TestAdversarial_DeeplyNestedStructures(t *testing.T) {
	// Build a 50-level deep map
	root := make(map[string]any)
	curr := root
	for i := 0; i < 50; i++ {
		next := make(map[string]any)
		curr[fmt.Sprintf("level_%02d", i)] = next
		curr = next
	}
	curr["leaf"] = "deep_value"

	out, err := canonical.Marshal(root)
	if err != nil {
		t.Fatalf("Marshal failed on 50-level nested map: %v", err)
	}

	if !strings.Contains(string(out), `"leaf":"deep_value"`) {
		t.Fatalf("nested payload missing leaf value")
	}

	// Verify ForbidFloat on deep map
	if err := canonical.ForbidFloat(root, "root"); err != nil {
		t.Fatalf("ForbidFloat failed on clean deep map: %v", err)
	}

	// Inject float at level 50
	curr["bad_float"] = 3.14159
	if err := canonical.ForbidFloat(root, "root"); err == nil {
		t.Fatalf("expected ForbidFloat to detect float at depth 50")
	}
}

type customKeyType string

func TestAdversarial_TypedMapsAndCustomKeyTypes(t *testing.T) {
	// Typed string map
	typedMap := map[string]int{
		"beta":  2,
		"alpha": 1,
	}
	out, err := canonical.Marshal(typedMap)
	if err != nil {
		t.Fatalf("Marshal failed on map[string]int: %v", err)
	}
	expected := `{"alpha":1,"beta":2}`
	if string(out) != expected {
		t.Errorf("got %s, expected %s", string(out), expected)
	}

	// Custom string alias key type (e.g. type CustomKey string)
	t.Run("CustomStringAliasKey", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("canonical.Marshal panicked on map with string alias key: %v", r)
			}
		}()
		aliasMap := map[customKeyType]string{
			"y": "second",
			"x": "first",
		}
		out, err = canonical.Marshal(aliasMap)
		if err != nil {
			t.Fatalf("Marshal failed on map[customKeyType]string: %v", err)
		}
		expectedAlias := `{"x":"first","y":"second"}`
		if string(out) != expectedAlias {
			t.Errorf("got %s, expected %s", string(out), expectedAlias)
		}
	})

	// Non-string key map MUST fail
	intKeyMap := map[int]string{
		1: "one",
		2: "two",
	}
	_, err = canonical.Marshal(intKeyMap)
	if err == nil {
		t.Errorf("expected error when marshaling map[int]string, but succeeded")
	}
}

// ============================================================================
// 3. FLOAT EDGE CASES (NaN, Inf, Integer floats, Fractional floats)
// ============================================================================

func TestAdversarial_FloatEdgeCases(t *testing.T) {
	testCases := []struct {
		name        string
		val         any
		shouldError bool
	}{
		{"Float64_NaN", math.NaN(), true},
		{"Float64_PosInf", math.Inf(1), true},
		{"Float64_NegInf", math.Inf(-1), true},
		{"Float64_Fractional_Pi", 3.1415926535, true},
		{"Float64_Fractional_Small", 0.0000001, true},
		{"Float64_Fractional_Negative", -0.5, true},
		{"Float32_NaN", float32(math.NaN()), true},
		{"Float32_PosInf", float32(math.Inf(1)), true},
		{"Float32_Fractional", float32(2.71828), true},
		{"Float64_Zero", 0.0, false},
		{"Float64_NegZero", math.Copysign(0.0, -1.0), false},
		{"Float64_Integer_42", 42.0, false},
		{"Float64_Integer_Negative", -100.0, false},
		{"Float32_Integer", float32(10.0), false},
		{"Float64_LargeInt_OverflowMaxInt64", 1e20, true}, // > math.MaxInt64, cannot be int64
		{"Float64_LargeInt_NegOverflowMinInt64", -1e20, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"v": tc.val}
			out, err := canonical.Marshal(m)
			if tc.shouldError {
				if err == nil {
					t.Errorf("%s: expected zero-float error for %v, got success: %s", tc.name, tc.val, string(out))
				}
			} else {
				if err != nil {
					t.Errorf("%s: unexpected error for integer float %v: %v", tc.name, tc.val, err)
				}
				if tc.name == "Float64_NegZero" {
					if string(out) != `{"v":0}` {
						t.Errorf("Negative zero should serialize to {\"v\":0}, got %s", string(out))
					}
				}
			}
		})
	}
}

type structWithFloat struct {
	F float64 `json:"f"`
}

func TestAdversarial_StructFloatOverflow(t *testing.T) {
	// 1e20 is an integer float, but exceeds math.MaxInt64.
	s := structWithFloat{F: 1e20}
	out, err := canonical.Marshal(s)
	t.Logf("Marshal structWithFloat(1e20): err=%v, out=%s", err, string(out))
	if err == nil {
		if strings.Contains(string(out), "-9223372036854775808") {
			t.Errorf("writeReflectValue formatted 1e20 as -9223372036854775808 due to int64 overflow!\nOutput: %s", string(out))
		}
	}
}

// ============================================================================
// 4. MERKLE ROOT STRESS TESTING (1,000+ LEAVES, ODD COUNTS)
// ============================================================================

func TestAdversarial_MerkleRoot_MassiveArraysAndOddCounts(t *testing.T) {
	// Test specific challenging sizes:
	// - 0, 1, 2, 3, 5, 7, 9 (small odds)
	// - 127, 128, 129
	// - 1000, 1001, 1023, 1024, 1025 (massive arrays)
	// - 2047, 2048, 2049
	sizes := []int{0, 1, 2, 3, 5, 7, 9, 127, 128, 129, 1000, 1001, 1023, 1024, 1025, 2047, 2048, 2049}

	for _, n := range sizes {
		t.Run(fmt.Sprintf("Size_%d", n), func(t *testing.T) {
			leaves := make([]string, n)
			for i := 0; i < n; i++ {
				sum := sha256.Sum256([]byte(fmt.Sprintf("leaf_payload_%d", i)))
				leaves[i] = hex.EncodeToString(sum[:])
			}

			// Keep original copy to verify immutability
			orig := make([]string, n)
			copy(orig, leaves)

			root := canonical.MerkleRoot(leaves)

			// 1. Root length must be exactly 64 hex characters
			if len(root) != 64 {
				t.Fatalf("size %d: invalid root length %d: %s", n, len(root), root)
			}

			// 2. Input immutability
			for i := 0; i < n; i++ {
				if leaves[i] != orig[i] {
					t.Fatalf("size %d: input slice mutated at index %d", n, i)
				}
			}

			// 3. Determinism: calling again with identical input produces identical root
			root2 := canonical.MerkleRoot(leaves)
			if root != root2 {
				t.Fatalf("size %d: non-deterministic root: %s vs %s", n, root, root2)
			}

			// 4. Avalanche / Collision resistance: flipping 1 bit in 1 leaf changes root
			if n > 0 {
				mutatedLeaves := make([]string, n)
				copy(mutatedLeaves, leaves)
				// flip first character of first leaf
				if mutatedLeaves[0][0] == 'a' {
					mutatedLeaves[0] = "b" + mutatedLeaves[0][1:]
				} else {
					mutatedLeaves[0] = "a" + mutatedLeaves[0][1:]
				}
				mutatedRoot := canonical.MerkleRoot(mutatedLeaves)
				if mutatedRoot == root {
					t.Fatalf("size %d: collision! root did not change after leaf mutation", n)
				}
			}
		})
	}
}

// ============================================================================
// 5. CONCURRENCY, ZERO DATA RACES, AND ZERO PANICS
// ============================================================================

func TestAdversarial_Concurrency_ZeroRacesAndPanics(t *testing.T) {
	const workers = 50
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func(workerID int) {
			defer wg.Done()
			for iter := 0; iter < iterations; iter++ {
				// 1. Concurrent Marshal
				payload := map[string]any{
					"worker": workerID,
					"iter":   iter,
					"tags":   []string{"alpha", "beta", "gamma"},
					"data": map[string]any{
						"🚀": "rocket",
						"a":  "first",
						"z":  "last",
					},
				}
				b, err := canonical.Marshal(payload)
				if err != nil {
					t.Errorf("worker %d iter %d: marshal failed: %v", workerID, iter, err)
					return
				}
				if len(b) == 0 {
					t.Errorf("worker %d iter %d: empty marshal output", workerID, iter)
				}

				// 2. Concurrent SHA256Of
				hash := canonical.SHA256Of(payload)
				if len(hash) != 64 {
					t.Errorf("worker %d iter %d: invalid hash: %s", workerID, iter, hash)
				}

				// 3. Concurrent MerkleRoot with 50 leaves (odd duplication)
				leaves := make([]string, 51)
				for i := 0; i < 51; i++ {
					leaves[i] = hash
				}
				root := canonical.MerkleRoot(leaves)
				if len(root) != 64 {
					t.Errorf("worker %d iter %d: invalid merkle root: %s", workerID, iter, root)
				}
			}
		}(w)
	}

	wg.Wait()
}
