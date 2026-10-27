// SPDX-License-Identifier: Apache-2.0
// Package icx_test implements empirical adversarial stress testing for
// Swarm Variable Discovery (icx_var_list & TTL Support).
//
// Verification areas:
// 1. TTL Expiration Boundaries (1ms, 5ms, 50ms, 1000ms, persistent, TTL extensions, TTL removals)
// 2. Expiration Invariants (Expired keys NEVER returned by VarGet or listed by VarList/InspectSpace)
// 3. CAS Contention on Expired Keys (ExpectedVersion > 0 fails with CAS_CONFLICT; ExpectedVersion == 0 succeeds at v1)
// 4. Pathological Prefix Filtering (Unicode, emoji, symbols, dots, colons, slashes, long keys, empty prefix)
// 5. Strict Ascending Sort Order (Monotonic key ordering under diverse UTF-8 characters and limit clamping)
// 6. CallTool Robustness (Type coercions for ttl_ms, limit, expected_version across float64/int/string)
// 7. High-Concurrency Stress Race (50 workers contending on expiry, CAS recreation, reads, and lists)

package icx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// ============================================================================
// SUITE 1: TTL Expiration Boundaries & Lifecycles
// ============================================================================

func TestTTLExpiry_Boundaries(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. Boundary 1ms TTL
	key1ms := "boundary_1ms"
	res1, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key1ms,
		Value:  "val_1ms",
		Writer: "test",
		TTLMs:  1,
	})
	if err != nil || !res1.OK {
		t.Fatalf("VarSet 1ms failed: %+v, %v", res1, err)
	}
	// Wait sufficient time for 1ms TTL to elapse (OS scheduler granularity on Windows is 1-15ms)
	time.Sleep(25 * time.Millisecond)
	get1, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key1ms})
	if err != nil || get1.OK || get1.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected 1ms TTL key to be expired and return VAR_NOT_FOUND, got: %+v", get1)
	}

	// 2. Boundary 5ms TTL
	key5ms := "boundary_5ms"
	res5, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key5ms,
		Value:  "val_5ms",
		Writer: "test",
		TTLMs:  5,
	})
	if err != nil || !res5.OK {
		t.Fatalf("VarSet 5ms failed: %+v, %v", res5, err)
	}
	time.Sleep(30 * time.Millisecond)
	get5, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key5ms})
	if err != nil || get5.OK || get5.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected 5ms TTL key to be expired, got: %+v", get5)
	}

	// 3. Boundary 50ms TTL
	key50ms := "boundary_50ms"
	res50, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key50ms,
		Value:  "val_50ms",
		Writer: "test",
		TTLMs:  50,
	})
	if err != nil || !res50.OK {
		t.Fatalf("VarSet 50ms failed: %+v, %v", res50, err)
	}
	// Immediately check: must be active
	get50Imm, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key50ms})
	if err != nil || !get50Imm.OK || get50Imm.Value != "val_50ms" {
		t.Fatalf("Immediate VarGet 50ms failed: %+v", get50Imm)
	}
	// Verify VarList reports valid RemainingTTLMs in range (0, 50]
	list50Imm, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "boundary_50ms"})
	if err != nil || !list50Imm.OK || list50Imm.Count != 1 {
		t.Fatalf("Immediate VarList 50ms failed: %+v", list50Imm)
	}
	rem50 := list50Imm.Variables[0].RemainingTTLMs
	if rem50 <= 0 || rem50 > 50 {
		t.Fatalf("Expected RemainingTTLMs in (0, 50], got %d", rem50)
	}
	// Sleep past 50ms
	time.Sleep(75 * time.Millisecond)
	get50After, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key50ms})
	if err != nil || get50After.OK || get50After.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected 50ms key to be expired, got: %+v", get50After)
	}

	// 4. Boundary 1000ms TTL
	key1000ms := "boundary_1000ms"
	res1000, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key1000ms,
		Value:  "val_1000ms",
		Writer: "test",
		TTLMs:  1000,
	})
	if err != nil || !res1000.OK {
		t.Fatalf("VarSet 1000ms failed: %+v, %v", res1000, err)
	}
	list1000, err := lat.VarList(ctx, icx.VarListRequest{Prefix: key1000ms})
	if err != nil || !list1000.OK || list1000.Count != 1 {
		t.Fatalf("VarList 1000ms failed: %+v", list1000)
	}
	rem1000 := list1000.Variables[0].RemainingTTLMs
	if rem1000 < 800 || rem1000 > 1000 {
		t.Fatalf("Expected RemainingTTLMs in [800, 1000], got %d", rem1000)
	}

	// 5. Persistent variable: TTLMs == 0
	keyPersist := "boundary_persist_0"
	resPersist, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    keyPersist,
		Value:  "val_persist",
		Writer: "test",
		TTLMs:  0,
	})
	if err != nil || !resPersist.OK {
		t.Fatalf("VarSet persistent failed: %+v", resPersist)
	}
	listPersist, err := lat.VarList(ctx, icx.VarListRequest{Prefix: keyPersist})
	if err != nil || !listPersist.OK || listPersist.Count != 1 {
		t.Fatalf("VarList persistent failed: %+v", listPersist)
	}
	if listPersist.Variables[0].RemainingTTLMs != -1 {
		t.Fatalf("Expected RemainingTTLMs == -1 for persistent key, got %d", listPersist.Variables[0].RemainingTTLMs)
	}

	// 6. Negative TTL: TTLMs < 0 (treated as non-expiring)
	keyNeg := "boundary_persist_neg"
	resNeg, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    keyNeg,
		Value:  "val_neg",
		Writer: "test",
		TTLMs:  -100,
	})
	if err != nil || !resNeg.OK {
		t.Fatalf("VarSet neg TTL failed: %+v", resNeg)
	}
	listNeg, err := lat.VarList(ctx, icx.VarListRequest{Prefix: keyNeg})
	if err != nil || !listNeg.OK || listNeg.Count != 1 {
		t.Fatalf("VarList neg TTL failed: %+v", listNeg)
	}
	if listNeg.Variables[0].RemainingTTLMs != -1 {
		t.Fatalf("Expected RemainingTTLMs == -1 for negative TTLMs, got %d", listNeg.Variables[0].RemainingTTLMs)
	}

	// 7. TTL Extension on Active Key
	keyExt := "boundary_extend"
	_, _ = lat.VarSet(ctx, icx.VarSetRequest{
		Key:    keyExt,
		Value:  "initial",
		Writer: "test",
		TTLMs:  40,
	})
	// Extend TTL before expiration to 300ms
	time.Sleep(15 * time.Millisecond)
	v1 := 1
	resExt, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             keyExt,
		Value:           "extended",
		ExpectedVersion: &v1,
		Writer:          "test",
		TTLMs:           300,
	})
	if err != nil || !resExt.OK || resExt.Version != 2 {
		t.Fatalf("TTL extension VarSet failed: %+v", resExt)
	}
	// Wait 60ms (original 40ms has elapsed, but extended 300ms has not)
	time.Sleep(60 * time.Millisecond)
	getExt, err := lat.VarGet(ctx, icx.VarGetRequest{Key: keyExt})
	if err != nil || !getExt.OK || getExt.Value != "extended" {
		t.Fatalf("Extended key should still be alive, got: %+v", getExt)
	}

	// 8. TTL Removal: Active Key Converted to Persistent
	keyRem := "boundary_remove_ttl"
	_, _ = lat.VarSet(ctx, icx.VarSetRequest{
		Key:    keyRem,
		Value:  "temp",
		Writer: "test",
		TTLMs:  40,
	})
	time.Sleep(10 * time.Millisecond)
	resRem, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             keyRem,
		Value:           "now_persistent",
		ExpectedVersion: &v1,
		Writer:          "test",
		TTLMs:           0, // remove TTL
	})
	if err != nil || !resRem.OK || resRem.Version != 2 {
		t.Fatalf("TTL removal VarSet failed: %+v", resRem)
	}
	time.Sleep(60 * time.Millisecond) // original 40ms has elapsed
	getRem, err := lat.VarGet(ctx, icx.VarGetRequest{Key: keyRem})
	if err != nil || !getRem.OK || getRem.Value != "now_persistent" {
		t.Fatalf("Persistent-converted key should survive original TTL, got: %+v", getRem)
	}
}

// ============================================================================
// SUITE 2: Expiration Invariants (Expired Keys NEVER Returned)
// ============================================================================

func TestExpiredVars_NeverReturned(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const numExpiring = 40
	const numPersistent = 10

	// Set 40 variables with short TTLs (20-40ms)
	for i := 0; i < numExpiring; i++ {
		key := fmt.Sprintf("exp_var_%03d", i)
		_, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    key,
			Value:  fmt.Sprintf("val_exp_%d", i),
			Writer: "worker_exp",
			TTLMs:  20 + (i % 20),
		})
		if err != nil {
			t.Fatalf("Failed to set expiring var %s: %v", key, err)
		}
	}

	// Set 10 persistent variables
	for i := 0; i < numPersistent; i++ {
		key := fmt.Sprintf("perm_var_%03d", i)
		_, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    key,
			Value:  fmt.Sprintf("val_perm_%d", i),
			Writer: "worker_perm",
			TTLMs:  0,
		})
		if err != nil {
			t.Fatalf("Failed to set perm var %s: %v", key, err)
		}
	}

	// Sleep 70ms so all 40 expiring variables are definitely past TTL
	time.Sleep(70 * time.Millisecond)

	// Invariant 1: VarGet on every expired key MUST return VAR_NOT_FOUND
	for i := 0; i < numExpiring; i++ {
		key := fmt.Sprintf("exp_var_%03d", i)
		getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
		if err != nil {
			t.Fatalf("VarGet returned unexpected error for %s: %v", key, err)
		}
		if getRes.OK {
			t.Fatalf("INVARIANT VIOLATION: Expired key %s returned OK: true!", key)
		}
		if getRes.Error != "VAR_NOT_FOUND" {
			t.Fatalf("INVARIANT VIOLATION: Expired key %s error = %q, expected 'VAR_NOT_FOUND'", key, getRes.Error)
		}
	}

	// Invariant 2: VarList with prefix "exp_" MUST return 0 count
	listExp, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "exp_"})
	if err != nil || !listExp.OK {
		t.Fatalf("VarList exp_ failed: %+v", listExp)
	}
	if listExp.Count != 0 || len(listExp.Variables) != 0 || len(listExp.Vars) != 0 {
		t.Fatalf("INVARIANT VIOLATION: VarList returned %d expired variables!", listExp.Count)
	}

	// Invariant 3: VarList with prefix "" MUST return EXACTLY the 10 persistent variables
	listAll, err := lat.VarList(ctx, icx.VarListRequest{Prefix: ""})
	if err != nil || !listAll.OK {
		t.Fatalf("VarList all failed: %+v", listAll)
	}
	if listAll.Count != numPersistent {
		t.Fatalf("Expected exactly %d persistent variables in VarList, got %d", numPersistent, listAll.Count)
	}
	for _, v := range listAll.Variables {
		if !strings.HasPrefix(v.Key, "perm_var_") {
			t.Fatalf("INVARIANT VIOLATION: Unexpected key %s in VarList", v.Key)
		}
		if v.RemainingTTLMs != -1 {
			t.Fatalf("Expected RemainingTTLMs == -1 for persistent key %s, got %d", v.Key, v.RemainingTTLMs)
		}
	}

	// Invariant 4: InspectSpace MUST omit all expired keys
	spaceRes, err := lat.InspectSpace(ctx, "default")
	if err != nil {
		t.Fatalf("InspectSpace failed: %v", err)
	}
	for _, k := range spaceRes.Vars {
		if strings.HasPrefix(k, "exp_var_") {
			t.Fatalf("INVARIANT VIOLATION: InspectSpace returned expired key %s", k)
		}
	}
	if len(spaceRes.Vars) != numPersistent {
		t.Fatalf("Expected InspectSpace to report %d keys, got %d", numPersistent, len(spaceRes.Vars))
	}
}

// ============================================================================
// SUITE 3: CAS Contention & Recreation on Expired Keys
// ============================================================================

func TestCASContention_ExpiredKeys(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "cas_expired_lifecycle"

	// Step 1: Initialize at version 1
	v0 := 0
	r1, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "value_v1",
		ExpectedVersion: &v0,
		Writer:          "init",
		TTLMs:           30,
	})
	if err != nil || !r1.OK || r1.Version != 1 {
		t.Fatalf("Initial set failed: %+v", r1)
	}

	// Step 2: Progress to version 2
	v1 := 1
	r2, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "value_v2",
		ExpectedVersion: &v1,
		Writer:          "updater",
		TTLMs:           30,
	})
	if err != nil || !r2.OK || r2.Version != 2 {
		t.Fatalf("Version 2 update failed: %+v", r2)
	}

	// Step 3: Progress to version 3
	v2 := 2
	r3, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "value_v3",
		ExpectedVersion: &v2,
		Writer:          "updater",
		TTLMs:           30,
	})
	if err != nil || !r3.OK || r3.Version != 3 {
		t.Fatalf("Version 3 update failed: %+v", r3)
	}

	// Step 4: Let key expire completely
	time.Sleep(50 * time.Millisecond)

	// Step 5: Adversarial CAS updates with stale expected versions
	testVersions := []int{1, 2, 3, 4, 99}
	for _, expVer := range testVersions {
		verCopy := expVer
		resConflict, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:             key,
			Value:           fmt.Sprintf("attempt_v%d", expVer),
			ExpectedVersion: &verCopy,
			Writer:          "stale_caller",
		})
		if err != nil {
			t.Fatalf("VarSet unexpected error: %v", err)
		}
		if resConflict.OK {
			t.Fatalf("Expected CAS conflict for expired key expecting v%d, but got OK: true!", expVer)
		}
		if resConflict.Error != "CAS_CONFLICT" {
			t.Fatalf("Expected error 'CAS_CONFLICT', got %q", resConflict.Error)
		}
		if resConflict.ActualVersion != 0 {
			t.Fatalf("Expected ActualVersion == 0 on expired key, got %d", resConflict.ActualVersion)
		}
	}

	// Step 6: Valid recreation with ExpectedVersion == 0 MUST succeed at Version 1
	resRecreate, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "reborn_value",
		ExpectedVersion: &v0,
		Writer:          "recreator",
		TTLMs:           100,
	})
	if err != nil || !resRecreate.OK {
		t.Fatalf("Recreation with ExpectedVersion=0 failed: %+v", resRecreate)
	}
	if resRecreate.Version != 1 {
		t.Fatalf("Expected new version to be exactly 1 after recreation, got %d", resRecreate.Version)
	}

	// Step 7: Subsequent update with ExpectedVersion == 1 MUST succeed at Version 2
	v1After := 1
	resNext, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "reborn_v2",
		ExpectedVersion: &v1After,
		Writer:          "updater",
	})
	if err != nil || !resNext.OK || resNext.Version != 2 {
		t.Fatalf("Update after recreation failed: %+v", resNext)
	}
}

// ============================================================================
// SUITE 4: Pathological Prefix Filtering & Special Characters
// ============================================================================

func TestPrefixFiltering_Pathological(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Diverse adversarial dataset of keys
	adversarialKeys := []string{
		// Dots, colons, slashes
		"cluster.node.alpha.cpu",
		"cluster.node.alpha.ram",
		"cluster.node.beta.cpu",
		"urn:uuid:6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"urn:uuid:6ba7b811-9dad-11d1-80b4-00c04fd430c8",
		"path/to/resource/alpha",
		"path/to/resource/beta",
		// Unicode / Multi-byte scripts
		"变量/全局/名称",
		"变量/全局/版本",
		"変数/ローカル/値",
		"параметр.системы.порт",
		"параметр.системы.хост",
		// Emoji prefixes
		"🚀/deployment/prod/status",
		"🚀/deployment/staging/status",
		"🔥/alert/high/cpu",
		// Punctuation & symbols
		"!@#$key_symbols",
		"key+with-minus_and.dots",
		// Long key (512 runes)
		strings.Repeat("long_prefix_", 30) + "key1",
		strings.Repeat("long_prefix_", 30) + "key2",
	}

	for _, k := range adversarialKeys {
		res, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    k,
			Value:  "val_" + k,
			Writer: "worker_1",
		})
		if err != nil || !res.OK {
			t.Fatalf("Failed to set key %q: %+v", k, res)
		}
	}

	testCases := []struct {
		name          string
		prefix        string
		expectedCount int
	}{
		{"EmptyPrefix", "", len(adversarialKeys)},
		{"ClusterDotPrefix", "cluster.node.alpha.", 2},
		{"ClusterAllDot", "cluster.", 3},
		{"UrnPrefix", "urn:uuid:", 2},
		{"PathSlashPrefix", "path/to/resource/", 2},
		{"ChinesePrefix", "变量/全局/", 2},
		{"JapanesePrefix", "変数/ローカル/", 1},
		{"CyrillicPrefix", "параметр.системы.", 2},
		{"EmojiRocketPrefix", "🚀/deployment/", 2},
		{"EmojiFirePrefix", "🔥/alert/", 1},
		{"SymbolPrefix", "!@#$", 1},
		{"LongPrefix", strings.Repeat("long_prefix_", 30), 2},
		{"NonMatchingPrefix", "does_not_exist_in_lattice_xyz", 0},
		{"CaseMismatchPrefix", "Cluster.", 0}, // case sensitivity check
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			listRes, err := lat.VarList(ctx, icx.VarListRequest{Prefix: tc.prefix})
			if err != nil || !listRes.OK {
				t.Fatalf("VarList failed for prefix %q: %+v, err: %v", tc.prefix, listRes, err)
			}
			if listRes.Count != tc.expectedCount {
				t.Fatalf("Prefix %q: expected count %d, got %d (variables: %+v)",
					tc.prefix, tc.expectedCount, listRes.Count, listRes.Variables)
			}
			if len(listRes.Variables) != tc.expectedCount {
				t.Fatalf("Prefix %q: len(Variables) %d != Count %d", tc.prefix, len(listRes.Variables), listRes.Count)
			}
			// Verify all returned variables actually start with prefix
			for _, v := range listRes.Variables {
				if !strings.HasPrefix(v.Key, tc.prefix) {
					t.Fatalf("Returned key %q does not match prefix %q", v.Key, tc.prefix)
				}
				// Verify Vars dictionary has matching value
				if listRes.Vars[v.Key] != "val_"+v.Key {
					t.Fatalf("Vars map mismatch for %q: got %v", v.Key, listRes.Vars[v.Key])
				}
			}
		})
	}
}

// ============================================================================
// SUITE 5: Strict Ascending Sort Order Under Adversarial Inputs
// ============================================================================

func TestStrictAscendingSort(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Generate 120 diverse keys in random order
	rawKeys := []string{
		"z_omega", "a_alpha", "M_middle", "m_lower", "1_numeric", "0_zero",
		"9_nine", "___triple_underscore", "-_dash", "b_bravo", "c_charlie",
		"ä_umlaut", "ö_umlaut", "ü_umlaut", "🚀_emoji", "✨_sparkles",
		"alpha/1", "alpha/2", "alpha.1", "alpha.2", "alpha:1", "alpha:2",
	}
	for i := 0; i < 98; i++ {
		rawKeys = append(rawKeys, fmt.Sprintf("rand_key_%04d_%x", rand.Intn(10000), rand.Int63()))
	}

	// Shuffle before inserting
	rand.Shuffle(len(rawKeys), func(i, j int) {
		rawKeys[i], rawKeys[j] = rawKeys[j], rawKeys[i]
	})

	for _, k := range rawKeys {
		_, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    k,
			Value:  "val",
			Writer: "sorter",
		})
		if err != nil {
			t.Fatalf("VarSet failed for %s: %v", k, err)
		}
	}

	// Test unlimited sort
	listRes, err := lat.VarList(ctx, icx.VarListRequest{})
	if err != nil || !listRes.OK {
		t.Fatalf("VarList all failed: %+v", listRes)
	}

	// Verify strict ascending order: entries[i].Key < entries[i+1].Key
	for i := 0; i < len(listRes.Variables)-1; i++ {
		cur := listRes.Variables[i].Key
		next := listRes.Variables[i+1].Key
		if !(cur < next) {
			t.Fatalf("INVARIANT VIOLATION: Not strictly ascending at index %d: %q >= %q", i, cur, next)
		}
	}

	// Test with various limits
	limits := []int{1, 5, 20, 50, 100}
	for _, limit := range limits {
		limRes, err := lat.VarList(ctx, icx.VarListRequest{Limit: limit})
		if err != nil || !limRes.OK {
			t.Fatalf("VarList limit %d failed: %+v", limit, limRes)
		}
		if limRes.Count != limit {
			t.Fatalf("Expected count == %d, got %d", limit, limRes.Count)
		}
		// Must match the first 'limit' entries of the full sorted list
		for i := 0; i < limit; i++ {
			if limRes.Variables[i].Key != listRes.Variables[i].Key {
				t.Fatalf("Limit %d mismatch at index %d: expected %q, got %q",
					limit, i, listRes.Variables[i].Key, limRes.Variables[i].Key)
			}
		}
	}
}

// ============================================================================
// SUITE 6: CallTool Robustness & Type Coercion
// ============================================================================

func TestCallTool_AdversarialTypes(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. icx_var_set with float64 ttl_ms (standard JSON unmarshaling into map[string]any)
	setCall1, err := lat.CallTool(ctx, "icx_var_set", map[string]any{
		"key":    "calltool_float_ttl",
		"value":  "hello_float",
		"ttl_ms": float64(50),
	})
	if err != nil || setCall1.IsError {
		t.Fatalf("CallTool float64 ttl_ms failed: %+v", setCall1)
	}

	// 2. icx_var_set with string ttl_ms ("60")
	setCall2, err := lat.CallTool(ctx, "icx_var_set", map[string]any{
		"key":    "calltool_str_ttl",
		"value":  "hello_str",
		"ttl_ms": "60",
	})
	if err != nil || setCall2.IsError {
		t.Fatalf("CallTool string ttl_ms failed: %+v", setCall2)
	}

	// 3. icx_var_set with string expected_version ("0")
	setCall3, err := lat.CallTool(ctx, "icx_var_set", map[string]any{
		"key":              "calltool_cas_str",
		"value":            "hello_cas",
		"expected_version": "0",
	})
	if err != nil || setCall3.IsError {
		t.Fatalf("CallTool string expected_version failed: %+v", setCall3)
	}

	// 4. icx_var_list with float64 limit
	listCall, err := lat.CallTool(ctx, "icx_var_list", map[string]any{
		"prefix": "calltool_",
		"limit":  float64(2),
	})
	if err != nil || listCall.IsError {
		t.Fatalf("CallTool float64 limit failed: %+v", listCall)
	}
	var listResp icx.VarListResponse
	if err := json.Unmarshal([]byte(listCall.Content[0].Text), &listResp); err != nil {
		t.Fatalf("Failed to parse JSON response: %v, text: %s", err, listCall.Content[0].Text)
	}
	if listResp.Count != 2 {
		t.Fatalf("Expected count=2 from limit, got %d", listResp.Count)
	}

	// 5. Verify TTL expiration via CallTool icx_var_get
	time.Sleep(80 * time.Millisecond)
	getCall, err := lat.CallTool(ctx, "icx_var_get", map[string]any{
		"key": "calltool_float_ttl",
	})
	if err != nil || getCall.IsError {
		t.Fatalf("CallTool icx_var_get failed: %+v", getCall)
	}
	var getResp icx.VarGetResponse
	_ = json.Unmarshal([]byte(getCall.Content[0].Text), &getResp)
	if getResp.OK || getResp.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected VAR_NOT_FOUND via CallTool, got %+v", getResp)
	}
}

// ============================================================================
// SUITE 7: High-Concurrency Stress Race
// ============================================================================

func TestConcurrency_ContentionRace(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const numWorkers = 40
	const numIters = 25

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	var (
		setSuccess atomic.Int64
		casConflict atomic.Int64
		getSuccess atomic.Int64
		getNotFound atomic.Int64
		listSuccess atomic.Int64
	)

	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < numIters; it++ {
				key := fmt.Sprintf("race_key_%d", (workerID+it)%8)
				ttl := 15 + rand.Intn(35) // 15-50ms TTL

				// Concurrently try CAS set expecting version 0 (recreation) or arbitrary version
				expV := (it % 3)
				var expPtr *int
				if expV > 0 {
					expPtr = &expV
				}

				setRes, err := lat.VarSet(ctx, icx.VarSetRequest{
					Key:             key,
					Value:           fmt.Sprintf("worker_%d_iter_%d", workerID, it),
					ExpectedVersion: expPtr,
					Writer:          fmt.Sprintf("worker_%d", workerID),
					TTLMs:           ttl,
				})
				if err == nil {
					if setRes.OK {
						setSuccess.Add(1)
					} else if setRes.Error == "CAS_CONFLICT" {
						casConflict.Add(1)
					}
				}

				// Concurrently get
				getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
				if err == nil {
					if getRes.OK {
						getSuccess.Add(1)
					} else {
						getNotFound.Add(1)
					}
				}

				// Concurrently list
				listRes, err := lat.VarList(ctx, icx.VarListRequest{
					Prefix: "race_key_",
					Limit:  4,
				})
				if err == nil && listRes.OK {
					listSuccess.Add(1)
					// Assert sorting invariant even during high-concurrency race
					for idx := 0; idx < len(listRes.Variables)-1; idx++ {
						if !(listRes.Variables[idx].Key < listRes.Variables[idx+1].Key) {
							t.Errorf("Sort violated during race: %q >= %q",
								listRes.Variables[idx].Key, listRes.Variables[idx+1].Key)
						}
					}
				}

				time.Sleep(time.Duration(1+rand.Intn(2)) * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	totalOps := setSuccess.Load() + casConflict.Load() + getSuccess.Load() + getNotFound.Load() + listSuccess.Load()
	if totalOps == 0 {
		t.Fatalf("No operations completed during race!")
	}
	t.Logf("Race test completed successfully: %d sets, %d CAS conflicts, %d gets, %d not-founds, %d lists (total %d ops)",
		setSuccess.Load(), casConflict.Load(), getSuccess.Load(), getNotFound.Load(), listSuccess.Load(), totalOps)
}
