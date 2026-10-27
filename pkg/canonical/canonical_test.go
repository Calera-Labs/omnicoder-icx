// SPDX-License-Identifier: Apache-2.0

package canonical_test

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

// Tier 1: Key Sorting Stability Tests
func TestKeyOrderStable(t *testing.T) {
	a, err := canonical.Marshal(map[string]any{"b": 1, "a": 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := `{"a":2,"b":1}`
	if string(a) != expected {
		t.Fatalf("expected %s, got %s", expected, string(a))
	}
}

func TestKeyOrderMultipleKeys(t *testing.T) {
	input := map[string]any{
		"zebra":  1,
		"apple":  2,
		"banana": 3,
		"cat":    4,
		"dog":    5,
	}
	a, err := canonical.Marshal(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := `{"apple":2,"banana":3,"cat":4,"dog":5,"zebra":1}`
	if string(a) != expected {
		t.Fatalf("expected %s, got %s", expected, string(a))
	}
}

// RFC 8785 UTF-16 Code Unit Order Invariant:
// Supplementary character U+10000 (represented in UTF-16 as surrogate pair 0xD800 0xDC00)
// must sort BEFORE BMP character U+E000 (code unit 0xE000) because 0xD800 < 0xE000.
// Under standard UTF-8 byte ordering, U+10000 (0xF0 0x90 0x80 0x80) sorts AFTER U+E000 (0xEE 0x80 0x80).
func TestRFC8785UTF16KeyOrder(t *testing.T) {
	k1 := "\U00010000" // Surrogate pair: 0xD800, 0xDC00
	k2 := "\uE000"     // Single code unit: 0xE000

	m := map[string]any{
		k2: 1,
		k1: 2,
	}

	b, err := canonical.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	expected := `{"` + k1 + `":2,"` + k2 + `":1}`
	if string(b) != expected {
		t.Fatalf("RFC 8785 UTF-16 key sorting failed: expected %s, got %s", expected, string(b))
	}
}

func TestKeyOrderDeterministicAcrossIterations(t *testing.T) {
	baseMap := map[string]any{
		"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5",
		"k6": "v6", "k7": "v7", "k8": "v8", "k9": "v9", "k10": "v10",
	}

	firstOut, err := canonical.Marshal(baseMap)
	if err != nil {
		t.Fatalf("initial marshal error: %v", err)
	}

	for i := 0; i < 100; i++ {
		// Re-insert into randomized map
		shuffled := make(map[string]any)
		keys := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"}
		rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		for _, k := range keys {
			shuffled[k] = baseMap[k]
		}
		out, err := canonical.Marshal(shuffled)
		if err != nil {
			t.Fatalf("iteration %d marshal error: %v", i, err)
		}
		if string(out) != string(firstOut) {
			t.Fatalf("non-deterministic output at iteration %d: %s vs %s", i, out, firstOut)
		}
	}
}

// String Escaping Tests (RFC 8785 Section 3.2.2.2)
func TestStringEscaping(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{`hello "world"`, `"hello \"world\""`},
		{"back\\slash", `"back\\slash"`},
		{"\b\f\n\r\t", `"\b\f\n\r\t"`},
		{"\x00\x1f", `"\u0000\u001f"`},
		{"Hello 🚀 World", `"Hello 🚀 World"`}, // Unicode characters >= 0x20 preserved directly
	}

	for _, tc := range cases {
		out, err := canonical.Marshal(tc.input)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tc.input, err)
		}
		if string(out) != tc.expected {
			t.Errorf("input %q got %s, expected %s", tc.input, string(out), tc.expected)
		}
	}
}

// Float Rejection & Zero-Float Invariant Tests
func TestRejectsFloat(t *testing.T) {
	_, err := canonical.Marshal(map[string]any{"x": 1.2})
	if err == nil || !strings.Contains(err.Error(), "IEEE float forbidden in canonical JSON") {
		t.Fatalf("expected IEEE float reject, got %v", err)
	}

	_, err = canonical.Marshal(map[string]any{"nested": []any{1, 2, 3.14}})
	if err == nil || !strings.Contains(err.Error(), "IEEE float forbidden") {
		t.Fatalf("expected IEEE float reject on nested float, got %v", err)
	}

	_, err = canonical.Marshal(map[string]any{"nan": math.NaN()})
	if err == nil || !strings.Contains(err.Error(), "IEEE float forbidden") {
		t.Fatalf("expected IEEE float reject on NaN, got %v", err)
	}

	_, err = canonical.Marshal(map[string]any{"inf": math.Inf(1)})
	if err == nil || !strings.Contains(err.Error(), "IEEE float forbidden") {
		t.Fatalf("expected IEEE float reject on +Inf, got %v", err)
	}
}

func TestIntegerFloatAllowed(t *testing.T) {
	// Exact integer floats are allowed and formatted as integers
	out, err := canonical.Marshal(map[string]any{"int_float": 42.0})
	if err != nil {
		t.Fatalf("integer float should be permitted, got error: %v", err)
	}
	expected := `{"int_float":42}`
	if string(out) != expected {
		t.Fatalf("expected %s, got %s", expected, string(out))
	}
}

func TestForbidFloat(t *testing.T) {
	if err := canonical.ForbidFloat(1.23, "val"); err == nil {
		t.Fatalf("expected float reject")
	}

	payload := map[string]any{
		"config": map[string]any{
			"threshold": 0.95,
		},
	}
	err := canonical.ForbidFloat(payload, "root")
	if err == nil || !strings.Contains(err.Error(), "root.config.threshold") {
		t.Fatalf("expected path root.config.threshold, got %v", err)
	}

	arrayPayload := []any{1, "safe", 2.718}
	err = canonical.ForbidFloat(arrayPayload, "items")
	if err == nil || !strings.Contains(err.Error(), "items[2]") {
		t.Fatalf("expected path items[2], got %v", err)
	}

	// Clean payload should pass
	cleanPayload := map[string]any{
		"name":  "omnicoder",
		"count": 100,
		"tags":  []any{"a", "b"},
		"flag":  true,
		"int_f": 10.0,
	}
	if err := canonical.ForbidFloat(cleanPayload, "clean"); err != nil {
		t.Fatalf("clean payload should pass ForbidFloat, got: %v", err)
	}
}

// SHA-256 Hashing Tests
func TestSHA256Bytes(t *testing.T) {
	// Standard NIST empty byte SHA-256 vector
	emptyHash := canonical.SHA256Bytes(nil)
	expectedEmpty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if emptyHash != expectedEmpty {
		t.Fatalf("expected empty SHA256 %s, got %s", expectedEmpty, emptyHash)
	}

	h := canonical.SHA256Bytes([]byte("hello world"))
	expectedHello := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if h != expectedHello {
		t.Fatalf("expected %s, got %s", expectedHello, h)
	}
}

func TestSHA256Of(t *testing.T) {
	h := canonical.SHA256Of(map[string]any{"b": 1, "a": 2})
	if len(h) != 64 {
		t.Fatalf("expected 64-char hex digest, got %s", h)
	}

	// Deterministic error hash on forbidden float
	errHash := canonical.SHA256Of(map[string]any{"bad": 1.23})
	if len(errHash) != 64 {
		t.Fatalf("expected 64-char error hex digest, got %s", errHash)
	}
}

// Balanced Binary Merkle Tree with Odd-Leaf Duplication Tests
func TestMerkleRootCalculations(t *testing.T) {
	// 0 elements
	emptyRoot := canonical.MerkleRoot(nil)
	expectedEmpty := canonical.SHA256Bytes(nil)
	if emptyRoot != expectedEmpty {
		t.Fatalf("expected empty root %s, got %s", expectedEmpty, emptyRoot)
	}

	// 1 element
	leaf1 := canonical.SHA256Bytes([]byte("leaf1"))
	singleRoot := canonical.MerkleRoot([]string{leaf1})
	if singleRoot != leaf1 {
		t.Fatalf("expected single root %s, got %s", leaf1, singleRoot)
	}

	// 2 elements
	leaf2 := canonical.SHA256Bytes([]byte("leaf2"))
	twoRoot := canonical.MerkleRoot([]string{leaf1, leaf2})
	expectedTwo := canonical.SHA256Bytes([]byte(leaf1 + leaf2))
	if twoRoot != expectedTwo {
		t.Fatalf("expected two root %s, got %s", expectedTwo, twoRoot)
	}

	// 3 elements (odd count with duplicate last leaf)
	leaf3 := canonical.SHA256Bytes([]byte("leaf3"))
	threeRoot := canonical.MerkleRoot([]string{leaf1, leaf2, leaf3})
	layer1Node1 := canonical.SHA256Bytes([]byte(leaf1 + leaf2))
	layer1Node2 := canonical.SHA256Bytes([]byte(leaf3 + leaf3)) // odd duplication
	expectedThree := canonical.SHA256Bytes([]byte(layer1Node1 + layer1Node2))
	if threeRoot != expectedThree {
		t.Fatalf("expected three root %s, got %s", expectedThree, threeRoot)
	}

	// 4 elements
	leaf4 := canonical.SHA256Bytes([]byte("leaf4"))
	fourRoot := canonical.MerkleRoot([]string{leaf1, leaf2, leaf3, leaf4})
	layer1NodeA := canonical.SHA256Bytes([]byte(leaf1 + leaf2))
	layer1NodeB := canonical.SHA256Bytes([]byte(leaf3 + leaf4))
	expectedFour := canonical.SHA256Bytes([]byte(layer1NodeA + layer1NodeB))
	if fourRoot != expectedFour {
		t.Fatalf("expected four root %s, got %s", expectedFour, fourRoot)
	}

	// 5 elements (odd count across multiple levels)
	leaf5 := canonical.SHA256Bytes([]byte("leaf5"))
	fiveRoot := canonical.MerkleRoot([]string{leaf1, leaf2, leaf3, leaf4, leaf5})
	l1_1 := canonical.SHA256Bytes([]byte(leaf1 + leaf2))
	l1_2 := canonical.SHA256Bytes([]byte(leaf3 + leaf4))
	l1_3 := canonical.SHA256Bytes([]byte(leaf5 + leaf5)) // odd duplication at layer 1
	l2_1 := canonical.SHA256Bytes([]byte(l1_1 + l1_2))
	l2_2 := canonical.SHA256Bytes([]byte(l1_3 + l1_3)) // odd duplication at layer 2
	expectedFive := canonical.SHA256Bytes([]byte(l2_1 + l2_2))
	if fiveRoot != expectedFive {
		t.Fatalf("expected five root %s, got %s", expectedFive, fiveRoot)
	}
}

func TestMerkleRootDoesNotMutateInput(t *testing.T) {
	orig := []string{"h1", "h2", "h3"}
	input := make([]string, len(orig))
	copy(input, orig)

	_ = canonical.MerkleRoot(input)
	for i := range orig {
		if input[i] != orig[i] {
			t.Fatalf("MerkleRoot mutated input slice at index %d", i)
		}
	}
}

// Struct and Reflection Serialization Tests
type samplePayload struct {
	ID       string            `json:"id"`
	Count    int               `json:"count"`
	Active   bool              `json:"active"`
	Optional string            `json:"optional,omitempty"`
	Ignored  string            `json:"-"`
	Metadata map[string]string `json:"metadata"`
}

func TestStructSerialization(t *testing.T) {
	p := samplePayload{
		ID:       "probe_01",
		Count:    10,
		Active:   true,
		Optional: "",
		Ignored:  "secret",
		Metadata: map[string]string{"env": "prod", "arch": "amd64"},
	}

	b, err := canonical.Marshal(p)
	if err != nil {
		t.Fatalf("failed to marshal struct: %v", err)
	}

	res := string(b)
	if strings.Contains(res, "secret") {
		t.Fatalf("ignored field serialized: %s", res)
	}
	if strings.Contains(res, "optional") {
		t.Fatalf("omitempty empty field serialized: %s", res)
	}
	expected := `{"active":true,"count":10,"id":"probe_01","metadata":{"arch":"amd64","env":"prod"}}`
	if res != expected {
		t.Fatalf("expected %s, got %s", expected, res)
	}
}

// Concurrency Safety Under -race
func TestConcurrentMarshalAndMerkle(t *testing.T) {
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			m := map[string]any{
				"idx":   idx,
				"name":  "test",
				"items": []any{1, 2, 3},
				"sub": map[string]any{
					"z": "last",
					"a": "first",
				},
			}

			// Concurrent Marshal
			b, err := canonical.Marshal(m)
			if err != nil {
				t.Errorf("goroutine %d marshal failed: %v", idx, err)
				return
			}
			if !strings.HasPrefix(string(b), `{"idx":`) && !strings.Contains(string(b), `"first"`) {
				t.Errorf("goroutine %d unexpected output: %s", idx, string(b))
			}

			// Concurrent SHA256Of
			h := canonical.SHA256Of(m)
			if len(h) != 64 {
				t.Errorf("goroutine %d invalid hash length: %d", idx, len(h))
			}

			// Concurrent MerkleRoot
			leaves := []string{
				h,
				canonical.SHA256Bytes([]byte("aux1")),
				canonical.SHA256Bytes([]byte("aux2")),
			}
			root := canonical.MerkleRoot(leaves)
			if len(root) != 64 {
				t.Errorf("goroutine %d invalid root length: %d", idx, len(root))
			}

			// Concurrent ForbidFloat
			if err := canonical.ForbidFloat(m, "root"); err != nil {
				t.Errorf("goroutine %d forbid float failed: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
}

// TestCustomStringAliasKeyMap verifies that maps with custom string alias
// key types serialize without panicking.
type testCustomStringKey string

func TestCustomStringAliasKeyMap(t *testing.T) {
	aliasMap := map[testCustomStringKey]int{
		"beta":  2,
		"alpha": 1,
	}
	out, err := canonical.Marshal(aliasMap)
	if err != nil {
		t.Fatalf("unexpected error marshaling map[testCustomStringKey]int: %v", err)
	}
	expected := `{"alpha":1,"beta":2}`
	if string(out) != expected {
		t.Errorf("got %s, expected %s", string(out), expected)
	}
}

// TestFloatOverflowRejection verifies that float values exceeding int64 bounds
// (> math.MaxInt64 or < math.MinInt64) are rejected in both Marshal and ForbidFloat.
type testStructWithFloat struct {
	F float64 `json:"f"`
}

func TestFloatOverflowRejection(t *testing.T) {
	// Values exceeding int64 bounds
	overflowValues := []float64{
		1e20,
		-1e20,
		9223372036854775808.0, // 2^63, exactly math.MaxInt64 + 1 in float64
	}

	for _, f := range overflowValues {
		// 1. Marshal struct containing large float must fail
		if out, err := canonical.Marshal(testStructWithFloat{F: f}); err == nil {
			t.Errorf("Marshal(testStructWithFloat{F: %v}) expected error, got %s", f, string(out))
		}
		// 2. Marshal map containing large float must fail
		if out, err := canonical.Marshal(map[string]any{"v": f}); err == nil {
			t.Errorf("Marshal(map[v: %v]) expected error, got %s", f, string(out))
		}
		// 3. ForbidFloat on standalone float must fail
		if err := canonical.ForbidFloat(f, "val"); err == nil {
			t.Errorf("ForbidFloat(%v) expected error, got nil", f)
		}
		// 4. ForbidFloat on struct must fail
		if err := canonical.ForbidFloat(testStructWithFloat{F: f}, "root"); err == nil {
			t.Errorf("ForbidFloat(testStructWithFloat{F: %v}) expected error, got nil", f)
		}
	}

	// Positive control: exact integer float within int64 range must succeed
	out, err := canonical.Marshal(testStructWithFloat{F: 42.0})
	if err != nil || string(out) != `{"f":42}` {
		t.Errorf("expected 42.0 to serialize to {\"f\":42}, got %s, err: %v", string(out), err)
	}
	if err := canonical.ForbidFloat(testStructWithFloat{F: 42.0}, "root"); err != nil {
		t.Errorf("ForbidFloat failed on 42.0: %v", err)
	}

	// Boundary control: math.MinInt64 (-9223372036854775808.0) is exact in float64
	minF := float64(math.MinInt64)
	out, err = canonical.Marshal(testStructWithFloat{F: minF})
	if err != nil || string(out) != `{"f":-9223372036854775808}` {
		t.Errorf("expected math.MinInt64 float to serialize to -9223372036854775808, got %s, err: %v", string(out), err)
	}
	if err := canonical.ForbidFloat(testStructWithFloat{F: minF}, "root"); err != nil {
		t.Errorf("ForbidFloat failed on math.MinInt64: %v", err)
	}
}

// TestJSONNumberScientificNotation verifies that json.Number with scientific
// notation without dot ("1e-5", "1E-5") is rejected in both Marshal and ForbidFloat.
func TestJSONNumberScientificNotation(t *testing.T) {
	// Forbidden non-integer or overflow json.Number cases
	forbidden := []string{
		"1e-5",
		"1E-5",
		"1e-20",
		"3.14",
		"1e20",
		"-1e20",
	}

	for _, s := range forbidden {
		num := json.Number(s)
		// Marshal standalone
		if out, err := canonical.Marshal(num); err == nil {
			t.Errorf("Marshal(json.Number(%q)) expected error, got %s", s, string(out))
		}
		// Marshal in map
		if out, err := canonical.Marshal(map[string]any{"num": num}); err == nil {
			t.Errorf("Marshal(map[num: json.Number(%q)]) expected error, got %s", s, string(out))
		}
		// ForbidFloat standalone
		if err := canonical.ForbidFloat(num, "val"); err == nil {
			t.Errorf("ForbidFloat(json.Number(%q)) expected error, got nil", s)
		}
		// ForbidFloat in map
		if err := canonical.ForbidFloat(map[string]any{"num": num}, "root"); err == nil {
			t.Errorf("ForbidFloat(map[num: json.Number(%q)]) expected error, got nil", s)
		}
	}

	// Permitted integer json.Number cases (must serialize as canonical integers)
	permitted := []struct {
		input    string
		expected string
	}{
		{"42", "42"},
		{"-42", "-42"},
		{"0", "0"},
		{"-0", "0"},
		{"100000", "100000"},
		{"1e5", "100000"},
		{"1.5e2", "150"},
	}

	for _, tc := range permitted {
		num := json.Number(tc.input)
		out, err := canonical.Marshal(num)
		if err != nil {
			t.Errorf("Marshal(json.Number(%q)) unexpected error: %v", tc.input, err)
		}
		if string(out) != tc.expected {
			t.Errorf("Marshal(json.Number(%q)) got %s, expected %s", tc.input, string(out), tc.expected)
		}
		if err := canonical.ForbidFloat(num, "val"); err != nil {
			t.Errorf("ForbidFloat(json.Number(%q)) unexpected error: %v", tc.input, err)
		}
	}
}

