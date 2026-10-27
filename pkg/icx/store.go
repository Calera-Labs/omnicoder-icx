// SPDX-License-Identifier: Apache-2.0
// Package icx implements the ICX 4D Volumetric Swarm Variable Store & CAS substrate.
// Provides Compare-And-Swap optimistic locking, dynamic endpoint registration with TTL,
// variable discovery, and cryptographic Merkle provenance across parallel swarm workers.

package icx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

var (
	// ErrEndpointNotFound is returned when a requested swarm endpoint does not exist or has expired.
	ErrEndpointNotFound = errors.New("swarm endpoint not found or expired")
	// ErrCASConflict is returned when an optimistic concurrency update fails due to a version mismatch.
	ErrCASConflict = errors.New("optimistic concurrency conflict (CAS_CONFLICT)")
)

// CASStore coordinates Compare-And-Swap variable storage, contract registration, and content-addressed storage
// over the ICX 4D volumetric lattice memory substrate.
type CASStore struct {
	lattice *LocalMemoryCache
	mu      sync.RWMutex
}

// NewCASStore instantiates a new CASStore. If no lattice is provided, an in-process local LocalMemoryCache is initialized.
func NewCASStore(lattice ...*LocalMemoryCache) *CASStore {
	var lat *LocalMemoryCache
	if len(lattice) > 0 && lattice[0] != nil {
		lat = lattice[0]
	} else {
		lat = NewLocalLattice()
	}
	return &CASStore{lattice: lat}
}

// Lattice returns the underlying in-process LocalMemoryCache engine.
func (s *CASStore) Lattice() *LocalMemoryCache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lattice
}

// VarSet performs an atomic Compare-And-Swap update on a swarm variable.
func (s *CASStore) VarSet(ctx context.Context, req VarSetRequest) (*VarSetResponse, error) {
	if s.lattice == nil {
		return nil, fmt.Errorf("cas store lattice is nil")
	}
	return s.lattice.VarSet(ctx, req)
}

// VarGet retrieves a swarm variable by key, enforcing TTL expiration.
func (s *CASStore) VarGet(ctx context.Context, req VarGetRequest) (*VarGetResponse, error) {
	if s.lattice == nil {
		return nil, fmt.Errorf("cas store lattice is nil")
	}
	return s.lattice.VarGet(ctx, req)
}

// VarList enumerates unexpired swarm variables matching a key prefix.
func (s *CASStore) VarList(ctx context.Context, req VarListRequest) (*VarListResponse, error) {
	if s.lattice == nil {
		return nil, fmt.Errorf("cas store lattice is nil")
	}
	return s.lattice.VarList(ctx, req)
}

// Put writes a key-value pair unconditionally into the CAS store.
func (s *CASStore) Put(ctx context.Context, spaceID, key string, value any) (*VarSetResponse, error) {
	return s.VarSet(ctx, VarSetRequest{
		SpaceID: spaceID,
		Key:     key,
		Value:   value,
	})
}

// PutWithTTL writes a key-value pair with an expiration TTL in milliseconds.
func (s *CASStore) PutWithTTL(ctx context.Context, spaceID, key string, value any, ttlMs int) (*VarSetResponse, error) {
	return s.VarSet(ctx, VarSetRequest{
		SpaceID: spaceID,
		Key:     key,
		Value:   value,
		TTLMs:   ttlMs,
	})
}

// PutCAS executes an optimistic Compare-And-Swap update, asserting that the existing version equals expectedVersion.
func (s *CASStore) PutCAS(ctx context.Context, spaceID, key string, value any, expectedVersion int) (*VarSetResponse, error) {
	exp := expectedVersion
	resp, err := s.VarSet(ctx, VarSetRequest{
		SpaceID:         spaceID,
		Key:             key,
		Value:           value,
		ExpectedVersion: &exp,
	})
	if err != nil {
		return nil, err
	}
	if !resp.OK && resp.Error == "CAS_CONFLICT" {
		return resp, ErrCASConflict
	}
	return resp, nil
}

// Get retrieves a variable value by key.
func (s *CASStore) Get(ctx context.Context, spaceID, key string) (*VarGetResponse, error) {
	return s.VarGet(ctx, VarGetRequest{
		SpaceID: spaceID,
		Key:     key,
	})
}

// List enumerates variables matching prefix in the specified memory space.
func (s *CASStore) List(ctx context.Context, spaceID, prefix string) (*VarListResponse, error) {
	return s.VarList(ctx, VarListRequest{
		SpaceID: spaceID,
		Prefix:  prefix,
	})
}

// Delete removes a key by overwriting it with an immediate expiration (1ms TTL).
func (s *CASStore) Delete(ctx context.Context, spaceID, key string) error {
	_, err := s.PutWithTTL(ctx, spaceID, key, nil, 1)
	return err
}

// RegisterEndpoint publishes a dynamically allocated endpoint with a 1-hour default TTL (or specified custom duration).
func (s *CASStore) RegisterEndpoint(ctx context.Context, spaceID, key, endpoint string, ttl ...time.Duration) (*VarSetResponse, error) {
	ttlMs := 3600 * 1000 // 1 hour default (3,600,000 ms)
	if len(ttl) > 0 && ttl[0] > 0 {
		ttlMs = int(ttl[0].Milliseconds())
	}
	return s.PutWithTTL(ctx, spaceID, key, endpoint, ttlMs)
}

// DiscoverEndpoint looks up a dynamically registered endpoint by key.
func (s *CASStore) DiscoverEndpoint(ctx context.Context, spaceID, key string) (string, error) {
	resp, err := s.Get(ctx, spaceID, key)
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.OK {
		return "", ErrEndpointNotFound
	}
	epStr, ok := resp.Value.(string)
	if !ok {
		return fmt.Sprintf("%v", resp.Value), nil
	}
	return epStr, nil
}

// ComputeMerkleRoot calculates a deterministic balanced binary Merkle tree root for a slice of leaf hashes.
func (s *CASStore) ComputeMerkleRoot(leafHashes []string) string {
	return canonical.MerkleRoot(leafHashes)
}
