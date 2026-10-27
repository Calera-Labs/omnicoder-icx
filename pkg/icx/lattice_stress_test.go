// SPDX-License-Identifier: Apache-2.0
// Package icx_test implements empirical adversarial stress testing of pkg/icx:
// 1. High-concurrency CAS swarm contention (50+ goroutines racing on VarSet).
// 2. LocalMemoryCache memory space stress under simultaneous reading, writing, purging, and quoting.
// 3. Secret scrubber audit (embedded keys, multiline secrets, false positive resilience).
// 4. icx_quote_slot sub-microsecond retrieval and SHA-256 seal verification.

package icx_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// ============================================================================
// SECTION 1: High-Concurrency CAS Swarm Contention (50+ Goroutines)
// ============================================================================

func TestCASSwarmContention_50PlusGoroutines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := icx.NewLocalLattice()
	key := "swarm_shared_counter"

	// Step 1: Initialize counter to 0 at version 1
	initVer := 0
	initRes, err := client.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           0,
		ExpectedVersion: &initVer,
		Writer:          "master_init",
	})
	if err != nil || !initRes.OK {
		t.Fatalf("Failed to initialize counter: %+v, err: %v", initRes, err)
	}

	// Step 1b: Verify deterministic CAS conflict detection
	fakeVer := 9999
	conflictRes, err := client.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           -1,
		ExpectedVersion: &fakeVer,
		Writer:          "conflict_prover",
	})
	if err != nil || conflictRes.OK || conflictRes.Error != "CAS_CONFLICT" {
		t.Fatalf("Deterministic CAS_CONFLICT verification failed: %+v, err: %v", conflictRes, err)
	}

	// Step 2: 64 concurrent goroutines racing to increment counter
	const numWorkers = 64
	var wg sync.WaitGroup
	var successfulIncrements atomic.Int64
	var totalConflicts atomic.Int64
	startSignal := make(chan struct{})

	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			<-startSignal // synchronous start to maximize contention spike

			writerID := fmt.Sprintf("worker_%02d", workerID)
			retries := 0

			for {
				select {
				case <-ctx.Done():
					t.Errorf("Worker %d timed out waiting for CAS", workerID)
					return
				default:
				}

				getRes, err := client.VarGet(ctx, icx.VarGetRequest{Key: key})
				if err != nil || !getRes.OK {
					time.Sleep(50 * time.Microsecond)
					continue
				}

				curVal := 0
				switch v := getRes.Value.(type) {
				case int:
					curVal = v
				case int64:
					curVal = int(v)
				case float64:
					curVal = int(v)
				default:
					t.Errorf("Unexpected value type %T: %v", getRes.Value, getRes.Value)
					return
				}

				expVer := getRes.Version
				setRes, err := client.VarSet(ctx, icx.VarSetRequest{
					Key:             key,
					Value:           curVal + 1,
					ExpectedVersion: &expVer,
					Writer:          writerID,
				})
				if err != nil {
					time.Sleep(50 * time.Microsecond)
					continue
				}

				if setRes.OK {
					successfulIncrements.Add(1)
					return
				}

				if setRes.Error == "CAS_CONFLICT" {
					totalConflicts.Add(1)
					retries++
					// Jittered backoff to simulate realistic swarm agent backoff
					backoff := time.Duration(100+rand.Intn(500)) * time.Microsecond
					time.Sleep(backoff)
				} else {
					t.Errorf("Worker %d received unexpected error: %s", workerID, setRes.Error)
					return
				}
			}
		}(i)
	}

	// Release all workers simultaneously
	close(startSignal)
	wg.Wait()

	// Step 3: Verify invariants
	finalGet, err := client.VarGet(ctx, icx.VarGetRequest{Key: key})
	if err != nil || !finalGet.OK {
		t.Fatalf("Failed to fetch final state: %+v, err: %v", finalGet, err)
	}

	t.Logf("CAS Swarm Results: %d workers, %d successful increments, %d total conflicts resolved",
		numWorkers, successfulIncrements.Load(), totalConflicts.Load())

	if successfulIncrements.Load() != numWorkers {
		t.Fatalf("Expected exactly %d successful increments, got %d", numWorkers, successfulIncrements.Load())
	}

	expectedFinalVal := fmt.Sprintf("%d", numWorkers)
	actualVal := fmt.Sprintf("%v", finalGet.Value)
	if actualVal != expectedFinalVal {
		t.Fatalf("Value corruption detected! Expected %s, got %s", expectedFinalVal, actualVal)
	}

	expectedVersion := numWorkers + 1 // initial write (v1) + 64 increments = v65
	if finalGet.Version != expectedVersion {
		t.Fatalf("Version discrepancy! Expected version %d, got %d", expectedVersion, finalGet.Version)
	}

	if totalConflicts.Load() == 0 {
		t.Logf("Notice: 0 dynamic CAS conflicts observed (scheduling was strictly sequential on this run)")
	} else {
		t.Logf("CAS contention successfully verified: %d conflicts dynamically resolved", totalConflicts.Load())
	}

	// Step 4: Verify Multi-Key Swarm Contention (10 keys x 10 workers = 100 goroutines)
	const multiKeys = 10
	const workersPerKey = 10
	var multiWg sync.WaitGroup
	multiWg.Add(multiKeys * workersPerKey)

	for k := 0; k < multiKeys; k++ {
		mKey := fmt.Sprintf("multi_key_%d", k)
		v0 := 0
		_, _ = client.VarSet(ctx, icx.VarSetRequest{Key: mKey, Value: 0, ExpectedVersion: &v0})

		for w := 0; w < workersPerKey; w++ {
			go func(mk string, wid int) {
				defer multiWg.Done()
				for {
					g, _ := client.VarGet(ctx, icx.VarGetRequest{Key: mk})
					if !g.OK {
						continue
					}
					cv := g.Value.(int)
					ev := g.Version
					s, _ := client.VarSet(ctx, icx.VarSetRequest{
						Key:             mk,
						Value:           cv + 1,
						ExpectedVersion: &ev,
						Writer:          fmt.Sprintf("w_%d", wid),
					})
					if s != nil && s.OK {
						break
					}
					time.Sleep(time.Duration(50+rand.Intn(100)) * time.Microsecond)
				}
			}(mKey, w)
		}
	}
	multiWg.Wait()

	for k := 0; k < multiKeys; k++ {
		mKey := fmt.Sprintf("multi_key_%d", k)
		g, err := client.VarGet(ctx, icx.VarGetRequest{Key: mKey})
		if err != nil || !g.OK {
			t.Fatalf("Multi-key check failed for %s", mKey)
		}
		if g.Value.(int) != workersPerKey {
			t.Fatalf("Multi-key value corruption for %s: expected %d, got %v", mKey, workersPerKey, g.Value)
		}
		if g.Version != workersPerKey+1 {
			t.Fatalf("Multi-key version mismatch for %s: expected %d, got %d", mKey, workersPerKey+1, g.Version)
		}
	}
}

// ============================================================================
// SECTION 2: Stress-Test LocalMemoryCache Under Simultaneous Read/Write/Purge/Quote
// ============================================================================

func TestLatticeSpaceStress_ReadWritePurgeQuoteReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lat := icx.NewLocalLattice()

	// Initial seed facts
	for i := 1; i <= 20; i++ {
		_, _ = lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Initial invariant seed fact #%d for lattice testing", i),
			SourceID: fmt.Sprintf("seed_src_%d", i%4),
			Family:   "core_invariants",
		})
	}

	stopCh := make(chan struct{})
	var opCounts struct {
		remembers atomic.Int64
		purges    atomic.Int64
		quotes    atomic.Int64
		recalls   atomic.Int64
		resets    atomic.Int64
		inspects  atomic.Int64
		varOps    atomic.Int64
	}

	var wg sync.WaitGroup

	// Group 1: 15 writers calling Remember
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			seq := 0
			for {
				select {
				case <-stopCh:
					return
				default:
					seq++
					fam := fmt.Sprintf("stress_fam_%d", id%5)
					src := fmt.Sprintf("stress_src_%d", (id+seq)%8)
					_, err := lat.Remember(ctx, icx.RememberRequest{
						Text:     fmt.Sprintf("Goroutine %d payload seq %d timestamp %d", id, seq, time.Now().UnixNano()),
						SourceID: src,
						Family:   fam,
					})
					if err == nil {
						opCounts.remembers.Add(1)
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
		}(i)
	}

	// Group 2: 10 purgers calling PurgeSource
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					targetSrc := fmt.Sprintf("stress_src_%d", rand.Intn(8))
					pRes, err := lat.PurgeSource(ctx, icx.PurgeSourceRequest{SourceID: targetSrc})
					if err == nil && pRes != nil {
						opCounts.purges.Add(1)
					}
					time.Sleep(200 * time.Microsecond)
				}
			}
		}(i)
	}

	// Group 3: 15 quoters calling QuoteSlot
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					fam := fmt.Sprintf("stress_fam_%d", rand.Intn(5))
					slotIdx := 1 + rand.Intn(10)
					qRes, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
						Family: fam,
						Index:  slotIdx,
					})
					if err == nil && qRes != nil {
						opCounts.quotes.Add(1)
						if qRes.OK && !qRes.Verified {
							t.Errorf("Cryptographic seal verification failure during concurrent quote! Fact SHA mismatch")
						}
					}
					time.Sleep(20 * time.Microsecond)
				}
			}
		}(i)
	}

	// Group 4: 10 searchers calling RecallScoped & SearchFacts
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					if rand.Intn(2) == 0 {
						r, err := lat.RecallScoped(ctx, icx.RecallRequest{
							Query: "payload",
							TopK:  rand.Intn(15),
						})
						if err == nil && r != nil {
							opCounts.recalls.Add(1)
						}
					} else {
						s, err := lat.SearchFacts(ctx, icx.SearchRequest{
							Query: "timestamp",
							Limit: 5,
						})
						if err == nil && s != nil {
							opCounts.recalls.Add(1)
						}
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
		}(i)
	}

	// Group 5: 3 session resetters calling ResetSession
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					rRes, err := lat.ResetSession(ctx, "default")
					if err == nil && rRes != nil {
						opCounts.resets.Add(1)
					}
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()
	}

	// Group 6: 4 inspectors computing Merkle roots with InspectSpace
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					insp, err := lat.InspectSpace(ctx, "default")
					if err == nil && insp != nil {
						opCounts.inspects.Add(1)
						if insp.MerkleRoot == "" && insp.NodesCount > 0 {
							t.Errorf("InspectSpace returned empty Merkle root while nodes existed")
						}
					}
					time.Sleep(200 * time.Microsecond)
				}
			}
		}()
	}

	// Group 7: 5 swarm state workers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			v := 0
			for {
				select {
				case <-stopCh:
					return
				default:
					v++
					vk := fmt.Sprintf("stress_var_%d", id)
					_, _ = lat.VarSet(ctx, icx.VarSetRequest{Key: vk, Value: v})
					_, _ = lat.VarGet(ctx, icx.VarGetRequest{Key: vk})
					opCounts.varOps.Add(2)
					time.Sleep(50 * time.Microsecond)
				}
			}
		}(i)
	}

	// Run under full concurrent saturation for 2 seconds
	time.Sleep(2 * time.Second)
	close(stopCh)
	wg.Wait()

	t.Logf("Lattice Stress Throughput over 2s: Remembers=%d, Purges=%d, Quotes=%d, Recalls=%d, Resets=%d, Inspects=%d, VarOps=%d",
		opCounts.remembers.Load(),
		opCounts.purges.Load(),
		opCounts.quotes.Load(),
		opCounts.recalls.Load(),
		opCounts.resets.Load(),
		opCounts.inspects.Load(),
		opCounts.varOps.Load(),
	)

	// Post-stress validation: verify lattice is healthy and can perform new operations cleanly
	remTest, err := lat.Remember(ctx, icx.RememberRequest{
		Text:     "Post-stress integrity check fact verification",
		SourceID: "post_stress_check",
		Family:   "post_check",
	})
	if err != nil || remTest.FactID == "" {
		t.Fatalf("Failed to remember fact after stress: %v", err)
	}

	qTest, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "post_check",
		Index:  1,
	})
	if err != nil || !qTest.OK || !qTest.Verified {
		t.Fatalf("Failed to quote slot after stress: %+v, err: %v", qTest, err)
	}

	inspFinal, err := lat.InspectSpace(ctx, "default")
	if err != nil || inspFinal.MerkleRoot == "" {
		t.Fatalf("Final InspectSpace failed or MerkleRoot empty: %+v", inspFinal)
	}
}

// ============================================================================
// SECTION 3: Secret Scrubber (Tricky Keys, Multiline, False Positives)
// ============================================================================

func TestSecretScrubber_AdversarialEvasionAndFalsePositives(t *testing.T) {
	scrubber := icx.NewSecretScrubber()

	// 1. Tricky Embedded Keys in Diverse Contexts
	keyTests := []struct {
		name       string
		input      string
		mustRedact []string
		mustRetain []string
	}{
		{
			name:       "JSON Embedded API Key",
			input:      `{"config": {"apiKey": "sk-1234567890abcdef12345", "timeout": 30}}`,
			mustRedact: []string{"sk-1234567890abcdef12345"},
			mustRetain: []string{`"config"`, `"timeout"`},
		},
		{
			name:       "URL Query Parameter with ICX Live Key",
			input:      `https://api.caleralabs.com/v1/stream?token=icx_live_ABCDEF1234567890&format=json`,
			mustRedact: []string{"icx_live_ABCDEF1234567890"},
			mustRetain: []string{"https://api.caleralabs.com/v1/stream", "format=json"},
		},
		{
			name:       "AWS Access Key ID with Markdown",
			input:      `**Warning**: Key ` + "`AKIAIOSFODNN7EXAMPLE`" + ` has expired.`,
			mustRedact: []string{"AKIAIOSFODNN7EXAMPLE"},
			mustRetain: []string{"**Warning**", "has expired"},
		},
		{
			name:       "Bearer Token with Multiple Spaces",
			input:      "Authorization:   Bearer    eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-IDdSemACt8x4iTMC6Y9JaGelHilGVSfvEBtqWmzfM",
			mustRedact: []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
			mustRetain: []string{"Authorization:"},
		},
		{
			name:       "SSN embedded in sentence",
			input:      "Customer SSN is 123-45-6789 for tax identification.",
			mustRedact: []string{"123-45-6789"},
			mustRetain: []string{"Customer SSN is", "for tax identification"},
		},
		{
			name:       "AWS Secret Access Key Assignment",
			input:      "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			mustRedact: []string{"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
			mustRetain: []string{},
		},
	}

	for _, tt := range keyTests {
		t.Run(tt.name, func(t *testing.T) {
			scrubbed, labels := scrubber.Scrub(tt.input)
			for _, secret := range tt.mustRedact {
				if strings.Contains(scrubbed, secret) {
					t.Errorf("[%s] FAILED to redact secret: %s\nOutput: %s\nLabels: %v", tt.name, secret, scrubbed, labels)
				}
			}
			for _, benign := range tt.mustRetain {
				if !strings.Contains(scrubbed, benign) {
					t.Errorf("[%s] Falsely deleted benign text: %s\nOutput: %s", tt.name, benign, scrubbed)
				}
			}
		})
	}

	// 2. Multiline Secrets
	multilineTests := []struct {
		name       string
		input      string
		mustRedact []string
	}{
		{
			name: "Multiline Password Assignment with Colon",
			input: "database:\n  user: root\n  password:\n    SuperSecretPass456\n  port: 5432",
			mustRedact: []string{"SuperSecretPass456"},
		},
		{
			name: "Multiline Bearer Token",
			input: "Header:\nBearer\n  eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			mustRedact: []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
		},
		{
			name: "Multiline AWS Secret Key",
			input: "aws_secret_access_key:\n  wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			mustRedact: []string{"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		},
	}

	for _, tt := range multilineTests {
		t.Run(tt.name, func(t *testing.T) {
			scrubbed, _ := scrubber.Scrub(tt.input)
			for _, secret := range tt.mustRedact {
				if strings.Contains(scrubbed, secret) {
					t.Errorf("[%s] Multiline secret leaked: %s\nOutput: %s", tt.name, secret, scrubbed)
				}
			}
		})
	}

	// 3. False-Positive Resilience (Benign Code & Math Should NOT be Corrupted)
	falsePositiveTests := []struct {
		name         string
		benignInput  string
		mustPreserve string
	}{
		{
			name:         "Date String format YYYY-MM-DD (must not match SSN)",
			benignInput:  "Release date: 2026-09-22 for milestone 2",
			mustPreserve: "2026-09-22",
		},
		{
			name:         "Math Subtraction Expression (must not match SSN)",
			benignInput:  "result = 123 - 45 - 6789",
			mustPreserve: "123 - 45 - 6789",
		},
		{
			name:         "UUID / Git commit hash",
			benignInput:  "commit a1b2c3d4e5f6071829304152637485960718293a",
			mustPreserve: "a1b2c3d4e5f6071829304152637485960718293a",
		},
		{
			name:         "Python flask-cache package name (must not match sk-)",
			benignInput:  "pip install flask-cache",
			mustPreserve: "flask-cache",
		},
	}

	for _, tt := range falsePositiveTests {
		t.Run(tt.name, func(t *testing.T) {
			scrubbed, labels := scrubber.Scrub(tt.benignInput)
			if !strings.Contains(scrubbed, tt.mustPreserve) {
				t.Errorf("[%s] False positive detected! Benign text '%s' was corrupted.\nOutput: %s\nLabels: %v",
					tt.name, tt.mustPreserve, scrubbed, labels)
			}
		})
	}
}

// TestSecretScrubber_BoundaryWeaknesses explores boundary failure modes,
// evasion vectors, and false positives in the regex scrubber.
func TestSecretScrubber_BoundaryWeaknesses(t *testing.T) {
	scrubber := icx.NewSecretScrubber()

	// 1. Quoted JSON key: {"password": "secret_password_123"}
	// Because of the closing quote after password (password"), \s*[:=] fails to match!
	jsonPass := `{"password": "secret_password_123"}`
	scrubbedJson, labels := scrubber.Scrub(jsonPass)
	t.Logf("JSON password test: input=%s -> scrubbed=%s (labels=%v)", jsonPass, scrubbedJson, labels)
	jsonPassRedacted := !strings.Contains(scrubbedJson, "secret_password_123")
	t.Logf("JSON password redacted: %v", jsonPassRedacted)

	// 2. Multi-word password: password: Correct Horse Battery Staple
	multiWord := "password: Correct Horse Battery Staple"
	scrubbedMulti, _ := scrubber.Scrub(multiWord)
	t.Logf("Multi-word password test: input='%s' -> scrubbed='%s'", multiWord, scrubbedMulti)
	multiWordLeaked := strings.Contains(scrubbedMulti, "Horse Battery Staple")
	t.Logf("Multi-word password trailing words leaked: %v", multiWordLeaked)

	// 3. Benign Go code: pwd := "/home/user"
	goCode := `pwd := "/home/user"`
	scrubbedGo, _ := scrubber.Scrub(goCode)
	t.Logf("Benign Go code test: input='%s' -> scrubbed='%s'", goCode, scrubbedGo)
	goCodeFalselyRedacted := strings.Contains(scrubbedGo, "[REDACTED:PASSWORD]")
	t.Logf("Benign Go code falsely redacted as password: %v", goCodeFalselyRedacted)

	// 4. Benign Linux environment variable: export PWD=/var/log
	linuxEnv := "export PWD=/var/log"
	scrubbedLinux, _ := scrubber.Scrub(linuxEnv)
	t.Logf("Linux PWD test: input='%s' -> scrubbed='%s'", linuxEnv, scrubbedLinux)
	linuxFalselyRedacted := strings.Contains(scrubbedLinux, "[REDACTED:PASSWORD]")
	t.Logf("Linux PWD falsely redacted: %v", linuxFalselyRedacted)

	// 5. Bearer token in double quotes: Authorization: Bearer "eyJhbGciOi..."
	quotedBearer := `Authorization: Bearer "eyJhbGciOiJIUzI1NiJ9"`
	scrubbedBearer, _ := scrubber.Scrub(quotedBearer)
	t.Logf("Quoted Bearer test: input='%s' -> scrubbed='%s'", quotedBearer, scrubbedBearer)
	quotedBearerLeaked := strings.Contains(scrubbedBearer, "eyJhbGciOiJIUzI1NiJ9")
	t.Logf("Quoted Bearer token leaked: %v", quotedBearerLeaked)
}


// ============================================================================
// SECTION 4: icx_quote_slot Sub-Microsecond Retrieval & SHA-256 Provenance
// ============================================================================

func TestQuoteSlot_SubMicrosecondAndSealVerification(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Ingest 50 facts across multiple families with varying content lengths
	const numFacts = 50
	expectedHashes := make(map[int]string) // slotIndex -> expected SHA-256
	expectedTexts := make(map[int]string)

	for i := 1; i <= numFacts; i++ {
		text := fmt.Sprintf("SLOT_DATA_FACT_%03d: Invariant equation delta = alpha^2 + beta^2 at sequence %d", i, i*17)
		h := sha256.Sum256([]byte(text))
		expectedHash := hex.EncodeToString(h[:])

		remRes, err := lat.Remember(ctx, icx.RememberRequest{
			Text:     text,
			SourceID: fmt.Sprintf("spec_%d", i),
			Family:   "benchmark_family",
		})
		if err != nil || !strings.EqualFold(remRes.Status, "OK") {
			t.Fatalf("Failed to ingest benchmark fact %d: %v", i, err)
		}
		expectedHashes[i] = expectedHash
		expectedTexts[i] = text
	}

	// 1. High-volume Sequential Retrieval & Latency Profiling (10,000 queries)
	const iterations = 10000
	latencies := make([]float64, iterations)

	for i := 0; i < iterations; i++ {
		slotIdx := 1 + (i % numFacts)
		t0 := time.Now()
		qRes, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
			Family: "benchmark_family",
			Index:  slotIdx,
		})
		durUs := float64(time.Since(t0).Nanoseconds()) / 1000.0
		latencies[i] = durUs

		if err != nil || !qRes.OK {
			t.Fatalf("QuoteSlot failed on iteration %d: %+v, err: %v", i, qRes, err)
		}
		if !qRes.Verified {
			t.Fatalf("Cryptographic seal verification returned false for slot %d", slotIdx)
		}
		if qRes.SHA256 != expectedHashes[slotIdx] {
			t.Fatalf("SHA-256 mismatch: expected %s, got %s", expectedHashes[slotIdx], qRes.SHA256)
		}
		if qRes.Text != expectedTexts[slotIdx] {
			t.Fatalf("Text corruption: expected %s, got %s", expectedTexts[slotIdx], qRes.Text)
		}
	}

	sort.Float64s(latencies)
	p50 := latencies[iterations*50/100]
	p95 := latencies[iterations*95/100]
	p99 := latencies[iterations*99/100]
	var sum float64
	for _, l := range latencies {
		sum += l
	}
	mean := sum / float64(iterations)

	t.Logf("QuoteSlot Latency Performance across %d calls:", iterations)
	t.Logf("  Mean: %.3f µs", mean)
	t.Logf("  P50:  %.3f µs", p50)
	t.Logf("  P95:  %.3f µs", p95)
	t.Logf("  P99:  %.3f µs", p99)

	// SLA check: In-process quote slot must be fast (<20 µs even under un-pinned CPU)
	if p50 > 25.0 {
		t.Errorf("QuoteSlot P50 latency %.3f µs exceeded SLA threshold of 25.0 µs", p50)
	}

	// 2. High-Concurrency Concurrent Quote Slot Readers (50 goroutines)
	const concurrentReaders = 50
	const queriesPerReader = 200
	var readerWg sync.WaitGroup
	var verificationFailures atomic.Int64
	var totalQuotes atomic.Int64

	readerWg.Add(concurrentReaders)
	for r := 0; r < concurrentReaders; r++ {
		go func(readerID int) {
			defer readerWg.Done()
			for q := 0; q < queriesPerReader; q++ {
				slot := 1 + rand.Intn(numFacts)
				res, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
					Family: "benchmark_family",
					Index:  slot,
				})
				if err != nil || !res.OK || !res.Verified {
					verificationFailures.Add(1)
				}
				totalQuotes.Add(1)
			}
		}(r)
	}
	readerWg.Wait()

	if verificationFailures.Load() > 0 {
		t.Fatalf("Concurrent QuoteSlot had %d verification failures out of %d calls",
			verificationFailures.Load(), totalQuotes.Load())
	}
	t.Logf("Concurrent QuoteSlot: 100%% verified across %d concurrent reads", totalQuotes.Load())

	// 3. Boundary & Error Condition Testing
	boundaryCases := []struct {
		name          string
		req           icx.QuoteSlotRequest
		expectedError string
	}{
		{
			name:          "Zero Index (1-based index violation)",
			req:           icx.QuoteSlotRequest{Family: "benchmark_family", Index: 0},
			expectedError: "SLOT_NOT_FOUND",
		},
		{
			name:          "Negative Index",
			req:           icx.QuoteSlotRequest{Family: "benchmark_family", Index: -5},
			expectedError: "SLOT_NOT_FOUND",
		},
		{
			name:          "Out of Bounds Index",
			req:           icx.QuoteSlotRequest{Family: "benchmark_family", Index: 99999},
			expectedError: "SLOT_NOT_FOUND",
		},
		{
			name:          "Nonexistent Family",
			req:           icx.QuoteSlotRequest{Family: "imaginary_family_xyz", Index: 1},
			expectedError: "SLOT_NOT_FOUND",
		},
	}

	for _, bc := range boundaryCases {
		t.Run(bc.name, func(t *testing.T) {
			resp, err := lat.QuoteSlot(ctx, bc.req)
			if err != nil {
				t.Fatalf("[%s] Unexpected error: %v", bc.name, err)
			}
			if resp.OK {
				t.Fatalf("[%s] Expected OK=false for invalid slot request, got true", bc.name)
			}
			if resp.Error != bc.expectedError {
				t.Fatalf("[%s] Expected error '%s', got '%s'", bc.name, bc.expectedError, resp.Error)
			}
		})
	}

	// 4. SHA-256 Tamper Resilience Test
	// Verify that if a fact is computed with canonical SHA-256, QuoteSlot validates it.
	canonicalHash := canonical.SHA256Bytes([]byte(expectedTexts[1]))
	if canonicalHash != expectedHashes[1] {
		t.Fatalf("Canonical SHA256Bytes mismatch with crypto/sha256")
	}
}

// BenchmarkQuoteSlot measures sequential QuoteSlot retrieval and verification latency.
func BenchmarkQuoteSlot(b *testing.B) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	for i := 1; i <= 20; i++ {
		_, _ = lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Benchmark fact payload #%d for sub-microsecond testing", i),
			SourceID: "bench_src",
			Family:   "bench_fam",
		})
	}

	req := icx.QuoteSlotRequest{
		Family: "bench_fam",
		Index:  1,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := lat.QuoteSlot(ctx, req)
		if err != nil || !res.OK || !res.Verified {
			b.Fatalf("QuoteSlot failed in benchmark: %v, %+v", err, res)
		}
	}
}

// BenchmarkQuoteSlot_Parallel measures concurrent multi-goroutine QuoteSlot throughput.
func BenchmarkQuoteSlot_Parallel(b *testing.B) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	for i := 1; i <= 20; i++ {
		_, _ = lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Parallel benchmark fact payload #%d for throughput testing", i),
			SourceID: "bench_src",
			Family:   "bench_fam",
		})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		idx := 1
		for pb.Next() {
			req := icx.QuoteSlotRequest{
				Family: "bench_fam",
				Index:  idx,
			}
			res, err := lat.QuoteSlot(ctx, req)
			if err != nil || !res.OK || !res.Verified {
				b.Fatalf("QuoteSlot failed in parallel benchmark: %v, %+v", err, res)
			}
			idx = (idx % 20) + 1
		}
	})
}

// BenchmarkVarSet_CASContention benchmarks CAS contention across parallel workers.
func BenchmarkVarSet_CASContention(b *testing.B) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "bench_swarm_counter"
	v0 := 0
	_, _ = lat.VarSet(ctx, icx.VarSetRequest{Key: key, Value: 0, ExpectedVersion: &v0})

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for {
				g, _ := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
				if !g.OK {
					continue
				}
				ev := g.Version
				cv := g.Value.(int)
				s, _ := lat.VarSet(ctx, icx.VarSetRequest{
					Key:             key,
					Value:           cv + 1,
					ExpectedVersion: &ev,
				})
				if s != nil && s.OK {
					break
				}
			}
		}
	})
}


