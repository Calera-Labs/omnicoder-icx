// SPDX-License-Identifier: Apache-2.0
package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// TestContracts_ExtractSignatures parses sample Go code containing structs, methods,
// and interfaces; asserts all exported types are extracted with 100% fidelity.
func TestContracts_ExtractSignatures(t *testing.T) {
	ctx := context.Background()
	store := icx.NewCASStore()
	cr := NewContractRegistry(store, "test-space")

	sampleCode := `
package index

import "context"

// IndexEntry represents a B-tree key entry.
type IndexEntry struct {
	Key    string
	Offset int64
	Length int64
}

// BatchRecord tracks batch write history.
type BatchRecord struct {
	ID        string
	Timestamp int64
}

type internalEntry struct {
	offset, length int64
}

// IndexScanner defines the range scanning contract.
type IndexScanner interface {
	ScanRange(ctx context.Context, p IndexEntry) bool
}

type unexportedScanner interface {
	internalCheck()
}

// Compare compares two index entries.
func (g *IndexEntry) Compare(other IndexEntry) int {
	return 0
}

func (g *IndexEntry) privateCalc() {}

// NewIndexEntry constructs an IndexEntry.
func NewIndexEntry(key string, offset, length int64) *IndexEntry {
	return &IndexEntry{Key: key, Offset: offset, Length: length}
}

func internalHelper() {}

const MaxEntries = 50000
const minEntries = 10
`

	contracts, err := cr.ExtractAndPublish(ctx, "pkg/index/types.go", []byte(sampleCode))
	if err != nil {
		t.Fatalf("ExtractAndPublish failed: %v", err)
	}

	if len(contracts) == 0 {
		t.Fatal("expected extracted contracts, got 0")
	}

	bySymbol := make(map[string]ExportedContract)
	for _, c := range contracts {
		bySymbol[c.SymbolName] = c
	}

	// 1. Assert Exported Structs
	gp, ok := bySymbol["IndexEntry"]
	if !ok {
		t.Errorf("missing exported struct IndexEntry")
	} else {
		if gp.Kind != "struct" {
			t.Errorf("expected Kind 'struct', got %q", gp.Kind)
		}
		if gp.PackageName != "index" {
			t.Errorf("expected PackageName 'index', got %q", gp.PackageName)
		}
		if gp.Signature != "type IndexEntry struct" {
			t.Errorf("expected Signature 'type IndexEntry struct', got %q", gp.Signature)
		}
		if gp.MerkleHash == "" {
			t.Errorf("expected non-empty MerkleHash for IndexEntry")
		}
	}

	tr, ok := bySymbol["BatchRecord"]
	if !ok {
		t.Errorf("missing exported struct BatchRecord")
	} else if tr.Kind != "struct" {
		t.Errorf("expected Kind 'struct', got %q", tr.Kind)
	}

	// 2. Assert Exported Interface
	cd, ok := bySymbol["IndexScanner"]
	if !ok {
		t.Errorf("missing exported interface IndexScanner")
	} else {
		if cd.Kind != "interface" {
			t.Errorf("expected Kind 'interface', got %q", cd.Kind)
		}
		if cd.Signature != "type IndexScanner interface" {
			t.Errorf("expected Signature 'type IndexScanner interface', got %q", cd.Signature)
		}
	}

	// 3. Assert Exported Method & Function
	dist, ok := bySymbol["Compare"]
	if !ok {
		t.Errorf("missing exported method Compare")
	} else {
		if dist.Kind != "func" {
			t.Errorf("expected Kind 'func', got %q", dist.Kind)
		}
		if !strings.Contains(dist.Signature, "(g *IndexEntry) Compare(other IndexEntry) int") {
			t.Errorf("expected method receiver in signature, got: %s", dist.Signature)
		}
	}

	newGP, ok := bySymbol["NewIndexEntry"]
	if !ok {
		t.Errorf("missing exported function NewIndexEntry")
	} else {
		if newGP.Kind != "func" {
			t.Errorf("expected Kind 'func', got %q", newGP.Kind)
		}
		if !strings.Contains(newGP.Signature, "func NewIndexEntry(key string, offset, length int64) *IndexEntry") {
			t.Errorf("unexpected func signature: %s", newGP.Signature)
		}
	}

	// 4. Assert Exported Const
	maxAlt, ok := bySymbol["MaxEntries"]
	if !ok {
		t.Errorf("missing exported const MaxEntries")
	} else {
		if maxAlt.Kind != "const" {
			t.Errorf("expected Kind 'const', got %q", maxAlt.Kind)
		}
		if !strings.Contains(maxAlt.Signature, "50000") {
			t.Errorf("unexpected const signature: %s", maxAlt.Signature)
		}
	}

	// 5. Assert Unexported Symbols are 100% Excluded
	unexported := []string{"internalEntry", "unexportedScanner", "privateCalc", "internalHelper", "minEntries"}
	for _, sym := range unexported {
		if _, exists := bySymbol[sym]; exists {
			t.Errorf("unexported symbol %q leaked into exported contracts!", sym)
		}
	}
}

// TestContracts_StorePublishAndGet publishes contracts into icx.CASStore and queries them back;
// validates Merkle hash matches canonical AST hash.
func TestContracts_StorePublishAndGet(t *testing.T) {
	ctx := context.Background()
	store := icx.NewCASStore()
	cr := NewContractRegistry(store, "wave0-space")

	code := `
package icx

// LocalMemoryCache represents 4D memory.
type LocalMemoryCache struct {
	Dimension int
}

// CASStore manages atomic Compare-And-Swap variables.
type CASStore struct {
	SpaceID string
}
`

	published, err := cr.ExtractAndPublish(ctx, "pkg/icx/types.go", []byte(code))
	if err != nil {
		t.Fatalf("ExtractAndPublish failed: %v", err)
	}

	if len(published) != 2 {
		t.Fatalf("expected 2 published contracts, got %d", len(published))
	}

	// Query package contracts back from CASStore
	queried, err := cr.QueryPackageContracts(ctx, "icx")
	if err != nil {
		t.Fatalf("QueryPackageContracts failed: %v", err)
	}

	if len(queried) != 2 {
		t.Fatalf("expected 2 queried contracts, got %d", len(queried))
	}

	for _, c := range queried {
		// Validate Merkle hash matches canonical AST hash
		expectedHash := CanonicalASTHash(c)
		if c.MerkleHash != expectedHash {
			t.Errorf("Merkle hash mismatch for %s: expected %s, got %s", c.SymbolName, expectedHash, c.MerkleHash)
		}

		// Direct lookup via CAS store key
		key := "contract/icx/" + c.SymbolName
		getResp, err := store.VarGet(ctx, icx.VarGetRequest{
			SpaceID: "wave0-space",
			Key:     key,
		})
		if err != nil || !getResp.OK {
			t.Errorf("direct CASStore lookup failed for key %s: %+v, err: %v", key, getResp, err)
		}
	}

	// Validate Merkle root generation over contracts
	merkleRoot := ComputeContractsMerkleRoot(queried)
	if merkleRoot == "" || len(merkleRoot) != 64 {
		t.Errorf("expected 64-character SHA-256 Merkle root, got: %s", merkleRoot)
	}
}

// TestContracts_BuildGroundingBlock asserts that the generated cognitive grounding block
// contains exact package names, type signatures, and warning banners.
func TestContracts_BuildGroundingBlock(t *testing.T) {
	ctx := context.Background()
	store := icx.NewCASStore()
	cr := NewContractRegistry(store, "wave0-space")

	// 1. Publish canonical contracts
	canonicalCode := `
package canonical

type RFC8785CanonicalJSON interface {
	Canonicalize() []byte
}
`
	_, err := cr.ExtractAndPublish(ctx, "pkg/canonical/types.go", []byte(canonicalCode))
	if err != nil {
		t.Fatalf("publish canonical failed: %v", err)
	}

	// 2. Publish icx contracts
	icxCode := `
package icx

type LocalMemoryCache struct {}
type CASStore struct {}
`
	_, err = cr.ExtractAndPublish(ctx, "pkg/icx/types.go", []byte(icxCode))
	if err != nil {
		t.Fatalf("publish icx failed: %v", err)
	}

	// 3. Publish index contracts
	indexCode := `
package index

type IndexEntry struct {}
type BatchRecord struct {}
`
	_, err = cr.ExtractAndPublish(ctx, "pkg/index/types.go", []byte(indexCode))
	if err != nil {
		t.Fatalf("publish index failed: %v", err)
	}

	// 4. Generate grounding block
	block, err := cr.BuildGroundingBlock(ctx, 2, []int{1})
	if err != nil {
		t.Fatalf("BuildGroundingBlock failed: %v", err)
	}

	t.Logf("Generated Grounding Block:\n%s", block)

	// Assertions on exact header
	header := "=== SWARM COGNITIVE GROUNDING: FROZEN CONTRACTS ==="
	if !strings.Contains(block, header) {
		t.Errorf("grounding block missing header %q", header)
	}

	preamble := "The following types are FROZEN in Wave 0 and MUST be imported, NOT redeclared:"
	if !strings.Contains(block, preamble) {
		t.Errorf("grounding block missing preamble %q", preamble)
	}

	// Assert exact package frozen lines
	expectedCanonical := "- package canonical: type RFC8785CanonicalJSON interface"
	if !strings.Contains(block, expectedCanonical) {
		t.Errorf("grounding block missing %q", expectedCanonical)
	}

	expectedICX := "- package icx: type CASStore struct, type LocalMemoryCache struct"
	if !strings.Contains(block, expectedICX) {
		t.Errorf("grounding block missing %q", expectedICX)
	}

	expectedIndex := "- package index: type BatchRecord struct, type IndexEntry struct"
	if !strings.Contains(block, expectedIndex) {
		t.Errorf("grounding block missing %q", expectedIndex)
	}

	// Assert exact warning banners
	banner1 := "CRITICAL INVARIANT: Sibling workers rely on these exact types."
	banner2 := "Do NOT duplicate or alter these struct definitions in your scope!"
	if !strings.Contains(block, banner1) {
		t.Errorf("grounding block missing banner %q", banner1)
	}
	if !strings.Contains(block, banner2) {
		t.Errorf("grounding block missing banner %q", banner2)
	}
}

// TestContracts_DynamicVariableExchange tests 1-hour TTL endpoint publishing
// and sibling worker discovery via CASStore.
func TestContracts_DynamicVariableExchange(t *testing.T) {
	ctx := context.Background()
	store := icx.NewCASStore()
	cr := NewContractRegistry(store, "swarm-run-001")

	// Worker 4 publishes dynamic mock WebSocket endpoint
	endpointKey := "endpoints/websocket/stream"
	endpointVal := "ws://127.0.0.1:8080/v1/stream"

	err := cr.RegisterEndpoint(ctx, endpointKey, endpointVal, time.Hour)
	if err != nil {
		t.Fatalf("RegisterEndpoint failed: %v", err)
	}

	// Worker 5 discovers endpoint
	discovered, err := cr.DiscoverEndpoint(ctx, endpointKey)
	if err != nil {
		t.Fatalf("DiscoverEndpoint failed: %v", err)
	}

	if discovered != endpointVal {
		t.Errorf("expected discovered endpoint %q, got %q", endpointVal, discovered)
	}

	// Query nonexistent endpoint
	_, err = cr.DiscoverEndpoint(ctx, "endpoints/nonexistent")
	if err != icx.ErrEndpointNotFound {
		t.Errorf("expected ErrEndpointNotFound, got %v", err)
	}
}

// TestStore_CAS tests optimistic concurrency locking (CAS), version checking,
// and TTL expiration in CASStore.
func TestStore_CAS(t *testing.T) {
	ctx := context.Background()
	store := icx.NewCASStore()

	space := "cas-test"
	key := "shared_config/cluster_size"

	// Initial write: version 1
	resp1, err := store.Put(ctx, space, key, 3)
	if err != nil || !resp1.OK {
		t.Fatalf("initial Put failed: %+v, err: %v", resp1, err)
	}
	if resp1.Version != 1 {
		t.Errorf("expected version 1, got %d", resp1.Version)
	}

	// Optimistic CAS with matching expected version (1)
	resp2, err := store.PutCAS(ctx, space, key, 5, 1)
	if err != nil || !resp2.OK {
		t.Fatalf("PutCAS with version 1 failed: %+v, err: %v", resp2, err)
	}
	if resp2.Version != 2 {
		t.Errorf("expected version 2, got %d", resp2.Version)
	}

	// Stale CAS attempt with old version (1) must fail with ErrCASConflict
	_, err = store.PutCAS(ctx, space, key, 10, 1)
	if err != icx.ErrCASConflict {
		t.Errorf("expected ErrCASConflict on stale update, got: %v", err)
	}

	// TTL Expiry test: write with 25ms TTL
	tempKey := "transient/token"
	_, err = store.PutWithTTL(ctx, space, tempKey, "abc-xyz", 25)
	if err != nil {
		t.Fatalf("PutWithTTL failed: %v", err)
	}

	// Immediately read
	getResp, err := store.Get(ctx, space, tempKey)
	if err != nil || !getResp.OK {
		t.Fatalf("immediate Get failed: %+v", getResp)
	}

	// Wait 40ms for TTL expiration
	time.Sleep(40 * time.Millisecond)

	expiredResp, err := store.Get(ctx, space, tempKey)
	if err != nil || expiredResp.OK {
		t.Errorf("expected expired variable to not be OK, got: %+v", expiredResp)
	}
}

// TestContracts_ConcurrentStoreAndQuery tests high-concurrency safety across
// multiple worker goroutines publishing and reading contracts simultaneously.
func TestContracts_ConcurrentStoreAndQuery(t *testing.T) {
	ctx := context.Background()
	store := icx.NewCASStore()
	cr := NewContractRegistry(store, "concurrent-space")

	var wg sync.WaitGroup
	workers := 15

	for w := 0; w < workers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			code := `
package workerpkg` + string(rune('A'+workerID)) + `
type ContractNode struct {
	Value int
}
`
			_, err := cr.ExtractAndPublish(ctx, "pkg/node.go", []byte(code))
			if err != nil {
				t.Errorf("worker %d publish failed: %v", workerID, err)
			}
		}()
	}

	wg.Wait()

	allContracts, err := cr.QueryAllContracts(ctx)
	if err != nil {
		t.Fatalf("QueryAllContracts failed: %v", err)
	}

	if len(allContracts) != workers {
		t.Errorf("expected %d contracts published concurrently, got %d", workers, len(allContracts))
	}
}
