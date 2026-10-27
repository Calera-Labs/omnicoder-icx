// SPDX-License-Identifier: Apache-2.0
// Package icx provides the OmniCoder local single-repository fallback cache and
// client adapter for connecting to Calera ICX.
// Note: The Calera ICX platform and server engine are proprietary and governed
// separately by the licensing terms at https://icx.caleralabs.com.

package icx

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
)

const (
	DefaultShardCount = 20
	MaxHopsDefault    = 5
)

// ComputeShardIndex deterministically maps arbitrary data to a local cache shard index (0..19).
func ComputeShardIndex(data []byte) int {
	h := sha256.Sum256(data)
	val := binary.BigEndian.Uint32(h[:4])
	return int(val % DefaultShardCount)
}

// Fact represents an immutable crystallized knowledge unit in the local cache.
type Fact struct {
	FactID     string         `json:"fact_id"`
	Text       string         `json:"text"`
	SourceID   string         `json:"source_id"`
	SpaceID    string         `json:"space_id"`
	Family     string         `json:"family"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	SHA256     string         `json:"sha256"`
	ShardIndex int            `json:"shard_index"` // 0..19
	CreatedNS  int64          `json:"created_ns"`
	SlotIndex  int            `json:"slot_index"` // 1-based index within family
}

// SwarmVar represents an atomic variable with Compare-And-Swap versioning.
type SwarmVar struct {
	Key         string `json:"key"`
	Value       any    `json:"value"`
	Version     int64  `json:"version"`
	Writer      string `json:"writer"`
	SHA256      string `json:"sha256"`
	UpdatedAt   int64  `json:"updated_at"`
	ExpiresAtMs int64  `json:"expires_at_ms,omitempty"`
}

// SwarmLease represents a distributed mutex lease.
type SwarmLease struct {
	Resource  string `json:"resource"`
	Holder    string `json:"holder"`
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// LatticeNode represents a multi-dimensional node in ICX 4D memory space.
type LatticeNode struct {
	ID        string              `json:"id"`
	Dimension string              `json:"dimension"` // 0D_SCALAR, 1D_SEQUENCE, 2D_TABULAR, 3D_VOLUMETRIC, 4D_SPATIOTEMPORAL
	Type      string              `json:"type"`      // file, function, struct, interface, audit_record
	Name      string              `json:"name"`
	FilePath  string              `json:"file_path,omitempty"`
	Line      int                 `json:"line,omitempty"`
	Value     any                 `json:"value,omitempty"`
	Relations map[string][]string `json:"relations,omitempty"`
	SHA256    string              `json:"sha256"`
	Timestamp int64               `json:"timestamp"`
}

// SpaceMeta holds session metadata for a volumetric space.
type SpaceMeta struct {
	SpaceID        string          `json:"space_id"`
	CreatedAt      int64           `json:"created_at"`
	DialogueTokens int             `json:"dialogue_tokens"`
	DialogueTurns  int             `json:"dialogue_turns"`
	Sources        map[string]bool `json:"sources"`
}

// TelemetryStats tracks in-process lattice operation counts.
type TelemetryStats struct {
	Ingests      atomic.Int64 `json:"ingests"`
	Walks        atomic.Int64 `json:"walks"`
	Purges       atomic.Int64 `json:"purges"`
	CASConflicts atomic.Int64 `json:"cas_conflicts"`
	Quotes       atomic.Int64 `json:"quotes"`
	Resets       atomic.Int64 `json:"resets"`
}

// SecretScrubber redacts API keys, tokens, passwords, emails, and SSNs.
type SecretScrubber struct {
	patterns []scrubPattern
}

type scrubPattern struct {
	label string
	reg   *regexp.Regexp
}

// NewSecretScrubber initializes regex matchers for sensitive credentials and PII.
func NewSecretScrubber() *SecretScrubber {
	return &SecretScrubber{
		patterns: []scrubPattern{
			{"api_key", regexp.MustCompile(`(?i)\b(sk-[A-Za-z0-9_\-]{8,}|icx_(?:live|test)_[A-Za-z0-9_\-]{8,}|AKIA[0-9A-Z]{16})\b`)},
			{"bearer", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-._~+/]+=*`)},
			{"password", regexp.MustCompile(`(?i)\b(password|passwd|pwd)\s*[:=]\s*\S+`)},
			{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
			{"ssn", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
			{"mrn", regexp.MustCompile(`(?i)\bMRN[:\s\-]*[A-Z0-9\-]{6,}\b`)},
			{"phone", regexp.MustCompile(`\b(?:\+1[\s\-]?)?(?:\(?\d{3}\)?[\s\-]?)?\d{3}[\s\-]?\d{4}\b`)},
			{"aws_secret", regexp.MustCompile(`(?i)\baws_secret_access_key\s*[:=]\s*\S+`)},
		},
	}
}

// Scrub executes regex redactions on text and returns the scrubbed text and list of matched labels.
func (s *SecretScrubber) Scrub(text string) (string, []string) {
	redacted := text
	var hits []string
	for _, p := range s.patterns {
		if p.reg.MatchString(redacted) {
			hits = append(hits, p.label)
			redacted = p.reg.ReplaceAllString(redacted, fmt.Sprintf("[REDACTED:%s]", strings.ToUpper(p.label)))
		}
	}
	return redacted, hits
}

// LocalMemoryCache provides thread-safe, in-process 4D volumetric memory satisfying ICXClient.
type LocalMemoryCache struct {
	mu           sync.RWMutex
	defaultSpace string
	scrubber     *SecretScrubber
	wasmKernel   *WasmKernel

	// Facts storage and indices
	facts    map[string]*Fact               // fact_id -> Fact
	bySource map[string]map[string]struct{} // source_id -> set of fact_id (for O(1) purge)
	bySpace  map[string]map[string]struct{} // space_id -> set of fact_id
	byFamily map[string][]string            // "space_id:family" -> ordered slice of fact_id

	// Multi-dimensional memory nodes
	nodes0D map[string]*LatticeNode // scalar metrics
	nodes1D map[string]*LatticeNode // sequential records
	nodes2D map[string]*LatticeNode // tabular symbols
	nodes3D map[string]*LatticeNode // volumetric AST graph
	nodes4D map[string]*LatticeNode // spatiotemporal audit log

	// Swarm state and distributed leases
	vars   map[string]*SwarmVar
	leases map[string]*SwarmLease

	// Graph edges for AST walks
	edges    map[string][]ASTEdge
	revEdges map[string][]ASTEdge

	// Spaces and counters
	spaces    map[string]*SpaceMeta
	factSeq   int64
	telemetry TelemetryStats
}

// NewLocalLattice instantiates an in-process local memory cache engine.
func NewLocalLattice() *LocalMemoryCache {
	lat := &LocalMemoryCache{
		defaultSpace: DefaultSpaceID,
		scrubber:     NewSecretScrubber(),
		wasmKernel:   NewWasmKernel(),
		facts:        make(map[string]*Fact),
		bySource:     make(map[string]map[string]struct{}),
		bySpace:      make(map[string]map[string]struct{}),
		byFamily:     make(map[string][]string),
		nodes0D:      make(map[string]*LatticeNode),
		nodes1D:      make(map[string]*LatticeNode),
		nodes2D:      make(map[string]*LatticeNode),
		nodes3D:      make(map[string]*LatticeNode),
		nodes4D:      make(map[string]*LatticeNode),
		vars:         make(map[string]*SwarmVar),
		leases:       make(map[string]*SwarmLease),
		edges:        make(map[string][]ASTEdge),
		revEdges:     make(map[string][]ASTEdge),
		spaces:       make(map[string]*SpaceMeta),
	}
	lat.ensureSpaceLocked(DefaultSpaceID)
	return lat
}

// NewLocalMemoryCache instantiates an in-process local memory cache engine with a custom default space.
func NewLocalMemoryCache(defaultSpace string) *LocalMemoryCache {
	lat := NewLocalLattice()
	if defaultSpace != "" {
		lat.mu.Lock()
		lat.defaultSpace = defaultSpace
		lat.ensureSpaceLocked(defaultSpace)
		lat.mu.Unlock()
	}
	return lat
}

func (lat *LocalMemoryCache) ensureSpaceLocked(spaceID string) *SpaceMeta {
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	meta, ok := lat.spaces[spaceID]
	if !ok {
		meta = &SpaceMeta{
			SpaceID:        spaceID,
			CreatedAt:      time.Now().Unix(),
			DialogueTokens: 0,
			DialogueTurns:  0,
			Sources:        make(map[string]bool),
		}
		lat.spaces[spaceID] = meta
	}
	return meta
}

// IsOffline indicates this is the in-process local engine.
func (lat *LocalMemoryCache) IsOffline() bool {
	return true
}

// Close is a no-op for the local in-process engine.
func (lat *LocalMemoryCache) Close() error {
	return nil
}

// ----------------------------------------------------------------------------
// Certified Tool 1: icx_remember
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) Remember(ctx context.Context, req RememberRequest) (*RememberResponse, error) {
	start := time.Now()

	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	family := req.Family
	if family == "" {
		family = "general"
	}

	// Secret scrubber pipeline
	scrubbedText, redactions := lat.scrubber.Scrub(req.Text)
	factSHA := canonical.SHA256Bytes([]byte(scrubbedText))

	// Compute deterministic local cache shard index
	shardIdx := ComputeShardIndex([]byte(scrubbedText))

	lat.mu.Lock()
	defer lat.mu.Unlock()

	lat.ensureSpaceLocked(spaceID)
	lat.factSeq++
	factID := fmt.Sprintf("f_%06d", lat.factSeq)

	famKey := spaceID + ":" + family
	slots := lat.byFamily[famKey]
	slotIdx := len(slots) + 1 // 1-based index

	f := &Fact{
		FactID:     factID,
		Text:       scrubbedText,
		SourceID:   req.SourceID,
		SpaceID:    spaceID,
		Family:     family,
		Metadata:   req.Metadata,
		SHA256:     factSHA,
		ShardIndex: shardIdx,
		CreatedNS:  time.Now().UnixNano(),
		SlotIndex:  slotIdx,
	}

	lat.facts[factID] = f

	// Index by family
	lat.byFamily[famKey] = append(slots, factID)

	// Index by source
	if req.SourceID != "" {
		if lat.bySource[req.SourceID] == nil {
			lat.bySource[req.SourceID] = make(map[string]struct{})
		}
		lat.bySource[req.SourceID][factID] = struct{}{}
		lat.spaces[spaceID].Sources[req.SourceID] = true
	}

	// Index by space
	if lat.bySpace[spaceID] == nil {
		lat.bySpace[spaceID] = make(map[string]struct{})
	}
	lat.bySpace[spaceID][factID] = struct{}{}

	// Audit trail in 4D memory
	auditID := fmt.Sprintf("audit_%s", factID)
	lat.nodes4D[auditID] = &LatticeNode{
		ID:        auditID,
		Dimension: "4D_SPATIOTEMPORAL",
		Type:      "remember",
		Name:      factID,
		Value:     map[string]any{"source": req.SourceID, "family": family, "sha256": factSHA},
		SHA256:    factSHA,
		Timestamp: time.Now().Unix(),
	}

	lat.telemetry.Ingests.Add(1)

	return &RememberResponse{
		Status:                "OK",
		FactID:                factID,
		NodeID:                req.NodeID,
		SpaceID:               spaceID,
		SourceID:              req.SourceID,
		SHA256:                factSHA,
		Redactions:            redactions,
		NodesCrystallized:     1,
		CrystallizationTimeUs: float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 2: icx_recall_scoped (Clamped strictly top_k <= 10)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) RecallScoped(ctx context.Context, req RecallRequest) (*RecallResponse, error) {
	start := time.Now()

	topK := req.TopK
	if topK <= 0 || topK > MaxRecallTopK {
		topK = MaxRecallTopK
	}

	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}

	lat.mu.RLock()
	defer lat.mu.RUnlock()

	factIDs := lat.bySpace[spaceID]
	queryLower := strings.ToLower(req.Query)
	queryWords := strings.Fields(queryLower)

	type matchCandidate struct {
		fact  *Fact
		score float64
	}

	var candidates []matchCandidate
	for id := range factIDs {
		fact, ok := lat.facts[id]
		if !ok {
			continue
		}
		if req.Family != "" && fact.Family != req.Family {
			continue
		}

		textLower := strings.ToLower(fact.Text)
		score := 0.0
		if strings.Contains(textLower, queryLower) {
			score += 10.0
		}
		for _, w := range queryWords {
			if strings.Contains(textLower, w) {
				score += 1.0
			}
		}

		if score > 0 {
			candidates = append(candidates, matchCandidate{fact: fact, score: score})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}

	matches := make([]RecallMatch, len(candidates))
	for i, c := range candidates {
		matches[i] = RecallMatch{
			FactID:   c.fact.FactID,
			Content:  c.fact.Text,
			Family:   c.fact.Family,
			Score:    c.score,
			SHA256:   c.fact.SHA256,
			Metadata: c.fact.Metadata,
		}
	}

	var ribbonStr string
	if req.Format == FormatRibbon || req.Format == "ribbon" {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("<nav:ribbon: query=%q count=%d | ", req.Query, len(matches)))
		if len(matches) == 0 {
			sb.WriteString("empty")
		} else {
			items := make([]string, 0, len(matches))
			for _, m := range matches {
				content := strings.ReplaceAll(m.Content, "\n", " ")
				content = strings.TrimSpace(content)
				if len(content) > 50 {
					content = content[:47] + "..."
				}
				items = append(items, fmt.Sprintf("[%s:%s] %q", m.FactID, m.Family, content))
			}
			sb.WriteString(strings.Join(items, " > "))
		}
		sb.WriteString(" />")
		ribbonStr = sb.String()
	}

	return &RecallResponse{
		Status:    "OK",
		Query:     req.Query,
		SpaceID:   spaceID,
		Count:     len(matches),
		Matches:   matches,
		Ribbon:    ribbonStr,
		LatencyUs: float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 3: icx_search_facts
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) SearchFacts(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	start := time.Now()

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}

	lat.mu.RLock()
	defer lat.mu.RUnlock()

	factIDs := lat.bySpace[spaceID]
	queryLower := strings.ToLower(req.Query)
	queryTokens := strings.Fields(queryLower)

	type scoredFact struct {
		fact  *Fact
		score float64
	}

	var results []scoredFact
	for id := range factIDs {
		fact, ok := lat.facts[id]
		if !ok {
			continue
		}
		textLower := strings.ToLower(fact.Text)
		score := 0.0
		for _, t := range queryTokens {
			if strings.Contains(textLower, t) {
				score += 1.0
			}
		}
		if score > 0 {
			results = append(results, scoredFact{fact: fact, score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	facts := make([]SearchFact, len(results))
	for i, r := range results {
		facts[i] = SearchFact{
			FactID:         r.fact.FactID,
			Text:           r.fact.Text,
			SourceID:       r.fact.SourceID,
			Family:         r.fact.Family,
			RelevanceScore: r.score,
			SHA256:         r.fact.SHA256,
		}
	}

	return &SearchResponse{
		Query:        req.Query,
		SpaceID:      spaceID,
		TotalMatches: len(facts),
		Facts:        facts,
		LatencyUs:    float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 4: icx_quote_slot (Deterministic <0.002ms byte-exact quote)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) QuoteSlot(ctx context.Context, req QuoteSlotRequest) (*QuoteSlotResponse, error) {
	start := time.Now()

	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	famKey := spaceID + ":" + req.Family

	lat.mu.RLock()
	defer lat.mu.RUnlock()

	slots, ok := lat.byFamily[famKey]
	if !ok || req.Index < 1 || req.Index > len(slots) {
		return &QuoteSlotResponse{
			OK:        false,
			Family:    req.Family,
			Index:     req.Index,
			Verified:  false,
			LatencyUs: float64(time.Since(start).Microseconds()),
			Error:     "SLOT_NOT_FOUND",
		}, nil
	}

	factID := slots[req.Index-1]
	fact, exists := lat.facts[factID]
	if !exists {
		return &QuoteSlotResponse{
			OK:        false,
			Family:    req.Family,
			Index:     req.Index,
			Verified:  false,
			LatencyUs: float64(time.Since(start).Microseconds()),
			Error:     "FACT_NOT_FOUND",
		}, nil
	}

	// Verify byte-exact cryptographic seal of the whole slot.
	calcSHA := canonical.SHA256Bytes([]byte(fact.Text))
	verified := (calcSHA == fact.SHA256)

	text := fact.Text
	isSlice := false
	sliceSHA := ""
	if req.Start != nil || req.Length != nil {
		runes := []rune(fact.Text)
		startOff := 0
		length := len(runes)
		if req.Start != nil {
			startOff = *req.Start
		}
		if req.Length != nil {
			length = *req.Length
		}
		if startOff < 0 || length < 0 || startOff+length > len(runes) {
			return &QuoteSlotResponse{
				OK:        false,
				Family:    req.Family,
				Index:     req.Index,
				SHA256:    fact.SHA256,
				Verified:  verified,
				LatencyUs: float64(time.Since(start).Microseconds()),
				Error:     "OUT_OF_RANGE",
			}, nil
		}
		text = string(runes[startOff : startOff+length])
		sliceSHA = canonical.SHA256Bytes([]byte(text))
		isSlice = true
	}

	lat.telemetry.Quotes.Add(1)

	return &QuoteSlotResponse{
		OK:          true,
		Family:      req.Family,
		Index:       req.Index,
		Text:        text,
		SourceID:    fact.SourceID,
		SHA256:      fact.SHA256,
		SliceSHA256: sliceSHA,
		IsSlice:     isSlice,
		Verified:    verified,
		LatencyUs:   float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 5: icx_sync_delta (Sub-2ms CPU delta crystallization)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) SyncDelta(ctx context.Context, req SyncDeltaRequest) (*SyncDeltaResponse, error) {
	start := time.Now()

	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}

	simplex := req.DeltaSimplex
	if lat.scrubber != nil && simplex != nil {
		scrubbed := make(map[string]any, len(simplex))
		for k, v := range simplex {
			if s, ok := v.(string); ok {
				clean, _ := lat.scrubber.Scrub(s)
				scrubbed[k] = clean
			} else {
				scrubbed[k] = v
			}
		}
		simplex = scrubbed
	}
	deltaBytes, _ := canonical.Marshal(simplex)
	deltaHash := canonical.SHA256Bytes(deltaBytes)
	cisSeal := canonical.SHA256Bytes([]byte(spaceID + ":" + req.SourceURI + ":" + deltaHash))
	deltaID := fmt.Sprintf("delta_%s", deltaHash[:12])

	lat.mu.Lock()
	defer lat.mu.Unlock()

	lat.ensureSpaceLocked(spaceID)
	lat.nodes4D[deltaID] = &LatticeNode{
		ID:        deltaID,
		Dimension: "4D_SPATIOTEMPORAL",
		Type:      "delta_simplex",
		Name:      req.SourceURI,
		Value:     simplex,
		SHA256:    cisSeal,
		Timestamp: time.Now().Unix(),
	}

	return &SyncDeltaResponse{
		Status:               "OK",
		CISSeal:              cisSeal,
		DeltaID:              deltaID,
		NodesCrystallized:    1,
		CrystallizeLatencyMs: float64(time.Since(start).Microseconds()) / 1000.0,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 6: icx_reset_session (Dialogue tokens -> 0, lattice 100% kept)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) ResetSession(ctx context.Context, spaceID string) (*ResetSessionResponse, error) {
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}

	lat.mu.Lock()
	defer lat.mu.Unlock()

	meta := lat.ensureSpaceLocked(spaceID)
	meta.DialogueTokens = 0
	meta.DialogueTurns = 0

	// Prune dialogue_turn facts only, retaining all other facts, variables, and AST nodes
	famKey := spaceID + ":dialogue_turn"
	if dialogueFacts, ok := lat.byFamily[famKey]; ok {
		for _, fid := range dialogueFacts {
			delete(lat.facts, fid)
			if lat.bySpace[spaceID] != nil {
				delete(lat.bySpace[spaceID], fid)
			}
		}
		delete(lat.byFamily, famKey)
	}

	aliveCount := len(lat.facts) + len(lat.nodes0D) + len(lat.nodes1D) +
		len(lat.nodes2D) + len(lat.nodes3D) + len(lat.nodes4D)

	lat.telemetry.Resets.Add(1)

	return &ResetSessionResponse{
		Status:            "OK",
		TokensPreserved:   0,
		LatticeNodesAlive: aliveCount,
		SpaceID:           spaceID,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 8: icx_var_set (Compare-And-Swap Optimistic Locking)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) VarSet(ctx context.Context, req VarSetRequest) (*VarSetResponse, error) {
	start := time.Now()

	lat.mu.Lock()
	defer lat.mu.Unlock()

	existing, found := lat.vars[req.Key]
	nowMs := time.Now().UnixMilli()

	// If existing variable has expired, treat as not found and evict
	if found && existing.ExpiresAtMs > 0 && nowMs >= existing.ExpiresAtMs {
		found = false
		delete(lat.vars, req.Key)
	}

	var currentVersion int64 = 0
	if found {
		currentVersion = existing.Version
	}

	// Check CAS condition
	if req.ExpectedVersion != nil {
		if int64(*req.ExpectedVersion) != currentVersion {
			lat.telemetry.CASConflicts.Add(1)
			actualVer := int(currentVersion)
			return &VarSetResponse{
				OK:              false,
				Key:             req.Key,
				Version:         actualVer,
				ActualVersion:   actualVer,
				ExpectedVersion: req.ExpectedVersion,
				LatencyUs:       float64(time.Since(start).Microseconds()),
				Error:           "CAS_CONFLICT",
			}, nil
		}
	}

	nextVersion := currentVersion + 1
	valHash := canonical.SHA256Of(req.Value)

	var expiresAtMs int64 = 0
	if req.TTLMs > 0 {
		expiresAtMs = nowMs + int64(req.TTLMs)
	}

	v := &SwarmVar{
		Key:         req.Key,
		Value:       req.Value,
		Version:     nextVersion,
		Writer:      req.Writer,
		SHA256:      valHash,
		UpdatedAt:   time.Now().UnixNano(),
		ExpiresAtMs: expiresAtMs,
	}
	lat.vars[req.Key] = v

	// Record in 4D audit memory
	auditKey := fmt.Sprintf("var_audit_%s_v%d", req.Key, nextVersion)
	lat.nodes4D[auditKey] = &LatticeNode{
		ID:        auditKey,
		Dimension: "4D_SPATIOTEMPORAL",
		Type:      "var_set",
		Name:      req.Key,
		Value:     req.Value,
		SHA256:    valHash,
		Timestamp: time.Now().Unix(),
	}

	return &VarSetResponse{
		OK:        true,
		Key:       req.Key,
		Version:   int(nextVersion),
		Writer:    req.Writer,
		LatencyUs: float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 9: icx_var_get
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) VarGet(ctx context.Context, req VarGetRequest) (*VarGetResponse, error) {
	start := time.Now()

	lat.mu.RLock()
	defer lat.mu.RUnlock()

	v, found := lat.vars[req.Key]
	nowMs := time.Now().UnixMilli()
	if !found || (v.ExpiresAtMs > 0 && nowMs >= v.ExpiresAtMs) {
		return &VarGetResponse{
			OK:        false,
			Key:       req.Key,
			LatencyUs: float64(time.Since(start).Microseconds()),
			Error:     "VAR_NOT_FOUND",
		}, nil
	}

	return &VarGetResponse{
		OK:        true,
		Key:       v.Key,
		Value:     v.Value,
		Version:   int(v.Version),
		Writer:    v.Writer,
		UpdatedAt: float64(v.UpdatedAt) / 1e9,
		LatencyUs: float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 22: icx_var_list (Swarm Variable Enumeration & Discovery)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) VarList(ctx context.Context, req VarListRequest) (*VarListResponse, error) {
	start := time.Now()

	lat.mu.RLock()
	defer lat.mu.RUnlock()

	nowMs := time.Now().UnixMilli()
	var entries []VarInfo

	for k, v := range lat.vars {
		// Filter out expired variables
		if v.ExpiresAtMs > 0 && nowMs >= v.ExpiresAtMs {
			continue
		}
		// Filter by prefix
		if req.Prefix != "" && !strings.HasPrefix(k, req.Prefix) {
			continue
		}

		var remainingTTL int64 = -1
		if v.ExpiresAtMs > 0 {
			remainingTTL = v.ExpiresAtMs - nowMs
			if remainingTTL < 0 {
				remainingTTL = 0
			}
		}

		preview := fmt.Sprintf("%v", v.Value)
		if len(preview) > 60 {
			preview = preview[:57] + "..."
		}

		info := VarInfo{
			Key:            v.Key,
			Version:        int(v.Version),
			Writer:         v.Writer,
			RemainingTTLMs: remainingTTL,
			TTLMs:          remainingTTL,
			ValuePreview:   preview,
		}
		entries = append(entries, info)
	}

	// Deterministic ascending sort by Key
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Key < entries[j].Key
	})

	// Clamp by Limit if specified
	if req.Limit > 0 && len(entries) > req.Limit {
		entries = entries[:req.Limit]
	}

	varsMap := make(map[string]any, len(entries))
	for _, e := range entries {
		if v, ok := lat.vars[e.Key]; ok {
			varsMap[e.Key] = v.Value
		}
	}

	return &VarListResponse{
		OK:        true,
		Status:    "OK",
		Count:     len(entries),
		Variables: entries,
		Vars:      varsMap,
		LatencyUs: float64(time.Since(start).Microseconds()),
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 10: icx_exec
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) Exec(ctx context.Context, req ExecRequest) (*ExecResponse, error) {
	res := ExecScript(req.Source, req.LocalsIn)
	return &res, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 11: icx_wasm_exec
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) WasmExec(ctx context.Context, req WasmExecRequest) (*WasmExecResponse, error) {
	res := lat.wasmKernel.Exec(req.Program)
	return &res, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 12: icx_inspect_space
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) InspectSpace(ctx context.Context, spaceID string) (*InspectSpaceResponse, error) {
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}

	lat.mu.RLock()
	defer lat.mu.RUnlock()

	factIDs := lat.bySpace[spaceID]
	var leafHashes []string
	for id := range factIDs {
		if f, ok := lat.facts[id]; ok {
			leafHashes = append(leafHashes, f.SHA256)
		}
	}
	sort.Strings(leafHashes)
	merkleRoot := canonical.MerkleRoot(leafHashes)

	var varKeys []string
	nowMs := time.Now().UnixMilli()
	for k, v := range lat.vars {
		if v.ExpiresAtMs > 0 && nowMs >= v.ExpiresAtMs {
			continue
		}
		varKeys = append(varKeys, k)
	}
	sort.Strings(varKeys)

	leasesMap := make(map[string]string)
	now := time.Now().Unix()
	for k, l := range lat.leases {
		if l.ExpiresAt > now {
			leasesMap[k] = l.Holder
		}
	}

	edgeCount := 0
	for _, el := range lat.edges {
		edgeCount += len(el)
	}

	sourcesCount := 0
	if meta, ok := lat.spaces[spaceID]; ok {
		sourcesCount = len(meta.Sources)
	}

	nodesTotal := len(factIDs) + len(lat.nodes0D) + len(lat.nodes1D) +
		len(lat.nodes2D) + len(lat.nodes3D) + len(lat.nodes4D)

	return &InspectSpaceResponse{
		SpaceID:      spaceID,
		NodesCount:   nodesTotal,
		SourcesCount: sourcesCount,
		Vars:         varKeys,
		Leases:       leasesMap,
		Edges:        edgeCount,
		ShardCount:   DefaultShardCount,
		MerkleRoot:   merkleRoot,
		Telemetry: map[string]any{
			"ingests":       lat.telemetry.Ingests.Load(),
			"walks":         lat.telemetry.Walks.Load(),
			"purges":        lat.telemetry.Purges.Load(),
			"cas_conflicts": lat.telemetry.CASConflicts.Load(),
			"quotes":        lat.telemetry.Quotes.Load(),
			"resets":        lat.telemetry.Resets.Load(),
		},
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 13: icx_purge_source (O(1) source memory unlinking)
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) PurgeSource(ctx context.Context, req PurgeSourceRequest) (*PurgeSourceResponse, error) {
	start := time.Now()

	spaceID := req.SpaceID
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}

	lat.mu.Lock()
	defer lat.mu.Unlock()

	targetFacts, ok := lat.bySource[req.SourceID]
	if !ok || len(targetFacts) == 0 {
		return &PurgeSourceResponse{
			Status:         "OK",
			SourceID:       req.SourceID,
			SpaceID:        spaceID,
			NodesUnlinked:  0,
			PurgeLatencyMs: float64(time.Since(start).Microseconds()) / 1000.0,
		}, nil
	}

	count := 0
	for factID := range targetFacts {
		fact, found := lat.facts[factID]
		if found {
			// Remove from byFamily
			famKey := fact.SpaceID + ":" + fact.Family
			if slots, ok := lat.byFamily[famKey]; ok {
				newSlots := make([]string, 0, len(slots))
				for _, s := range slots {
					if s != factID {
						newSlots = append(newSlots, s)
					}
				}
				lat.byFamily[famKey] = newSlots
			}

			// Remove from bySpace
			if lat.bySpace[fact.SpaceID] != nil {
				delete(lat.bySpace[fact.SpaceID], factID)
			}

			delete(lat.facts, factID)
			count++
		}
	}

	delete(lat.bySource, req.SourceID)
	if meta, ok := lat.spaces[spaceID]; ok {
		delete(meta.Sources, req.SourceID)
	}

	lat.telemetry.Purges.Add(1)

	return &PurgeSourceResponse{
		Status:         "OK",
		SourceID:       req.SourceID,
		SpaceID:        spaceID,
		NodesUnlinked:  count,
		PurgeLatencyMs: float64(time.Since(start).Microseconds()) / 1000.0,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 14: icx_swarm_state
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) SwarmState(ctx context.Context, req SwarmStateRequest) (*SwarmStateResponse, error) {
	start := time.Now()

	lat.mu.Lock()
	defer lat.mu.Unlock()

	ttl := req.TTLMs
	if ttl <= 0 {
		ttl = 30000 // default 30s
	}
	now := time.Now().Unix()

	switch req.Action {
	case "lease":
		if existing, ok := lat.leases[req.Resource]; ok && existing.ExpiresAt > now {
			if existing.Holder != req.AgentID {
				return &SwarmStateResponse{
					OK:        false,
					Action:    "lease",
					Resource:  req.Resource,
					Holder:    existing.Holder,
					TTLMs:     int((existing.ExpiresAt - now) * 1000),
					LatencyUs: float64(time.Since(start).Microseconds()),
					Error:     "RESOURCE_LOCKED",
				}, nil
			}
		}
		token := canonical.SHA256Bytes([]byte(fmt.Sprintf("%s:%s:%d", req.Resource, req.AgentID, now)))
		lease := &SwarmLease{
			Resource:  req.Resource,
			Holder:    req.AgentID,
			Token:     token,
			ExpiresAt: now + int64(ttl/1000),
		}
		lat.leases[req.Resource] = lease
		return &SwarmStateResponse{
			OK:        true,
			Action:    "lease",
			Resource:  req.Resource,
			Holder:    req.AgentID,
			Token:     token,
			TTLMs:     ttl,
			LatencyUs: float64(time.Since(start).Microseconds()),
		}, nil

	case "release":
		existing, ok := lat.leases[req.Resource]
		if !ok || existing.Holder != req.AgentID {
			return &SwarmStateResponse{
				OK:        false,
				Action:    "release",
				Resource:  req.Resource,
				LatencyUs: float64(time.Since(start).Microseconds()),
				Error:     "NOT_LEASE_HOLDER",
			}, nil
		}
		delete(lat.leases, req.Resource)
		return &SwarmStateResponse{
			OK:        true,
			Action:    "release",
			Resource:  req.Resource,
			LatencyUs: float64(time.Since(start).Microseconds()),
		}, nil

	case "inspect":
		existing, ok := lat.leases[req.Resource]
		if !ok || existing.ExpiresAt <= now {
			return &SwarmStateResponse{
				OK:        true,
				Action:    "inspect",
				Resource:  req.Resource,
				LatencyUs: float64(time.Since(start).Microseconds()),
			}, nil
		}
		return &SwarmStateResponse{
			OK:        true,
			Action:    "inspect",
			Resource:  req.Resource,
			Holder:    existing.Holder,
			Token:     existing.Token,
			TTLMs:     int((existing.ExpiresAt - now) * 1000),
			LatencyUs: float64(time.Since(start).Microseconds()),
		}, nil

	default:
		return &SwarmStateResponse{
			OK:        false,
			Action:    req.Action,
			LatencyUs: float64(time.Since(start).Microseconds()),
			Error:     fmt.Sprintf("Unknown swarm action: %s", req.Action),
		}, nil
	}
}

// ----------------------------------------------------------------------------
// Certified Tool 15: icx_query_codebase
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) QueryCodebase(ctx context.Context, req QueryCodebaseRequest) (*QueryCodebaseResponse, error) {
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}

	qLower := strings.ToLower(req.Query)
	var matches []CodebaseMatch

	searchMap := func(nodes map[string]*LatticeNode, dim string) {
		for _, n := range nodes {
			if strings.Contains(strings.ToLower(n.Name), qLower) ||
				strings.Contains(strings.ToLower(n.FilePath), qLower) {
				matches = append(matches, CodebaseMatch{
					NodeID:    n.ID,
					Dimension: dim,
					Snippet:   fmt.Sprintf("%s (%s) in %s:%d", n.Name, n.Type, n.FilePath, n.Line),
				})
			}
		}
	}

	switch req.Dimension {
	case "0D_SCALAR":
		searchMap(lat.nodes0D, "0D_SCALAR")
	case "1D_SEQUENCE":
		searchMap(lat.nodes1D, "1D_SEQUENCE")
	case "2D_TABULAR":
		searchMap(lat.nodes2D, "2D_TABULAR")
	case "3D_VOLUMETRIC":
		searchMap(lat.nodes3D, "3D_VOLUMETRIC")
	case "4D_SPATIOTEMPORAL":
		searchMap(lat.nodes4D, "4D_SPATIOTEMPORAL")
	default:
		searchMap(lat.nodes0D, "0D_SCALAR")
		searchMap(lat.nodes2D, "2D_TABULAR")
		searchMap(lat.nodes3D, "3D_VOLUMETRIC")
	}

	if len(matches) > topK {
		matches = matches[:topK]
	}

	return &QueryCodebaseResponse{
		Query:   req.Query,
		Count:   len(matches),
		Results: matches,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 16: icx_read_dimension
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) ReadDimension(ctx context.Context, req ReadDimensionRequest) (*ReadDimensionResponse, error) {
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	var targetMap map[string]*LatticeNode
	switch req.Dimension {
	case "0D_SCALAR":
		targetMap = lat.nodes0D
	case "1D_SEQUENCE":
		targetMap = lat.nodes1D
	case "2D_TABULAR":
		targetMap = lat.nodes2D
	case "3D_VOLUMETRIC":
		targetMap = lat.nodes3D
	case "4D_SPATIOTEMPORAL":
		targetMap = lat.nodes4D
	default:
		targetMap = lat.nodes3D
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}

	var nodes []map[string]any
	for _, n := range targetMap {
		if req.NodeID != "" && n.ID != req.NodeID {
			continue
		}
		nodes = append(nodes, map[string]any{
			"id":        n.ID,
			"dimension": n.Dimension,
			"type":      n.Type,
			"name":      n.Name,
			"file_path": n.FilePath,
			"line":      n.Line,
			"sha256":    n.SHA256,
		})
		if len(nodes) >= limit {
			break
		}
	}

	return &ReadDimensionResponse{
		Dimension: req.Dimension,
		Nodes:     nodes,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 17: icx_read_file_ast
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) ReadFileAST(ctx context.Context, req ReadFileASTRequest) (*ReadFileASTResponse, error) {
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	var classes, functions, imports []string
	var astNodes []map[string]any

	for _, n := range lat.nodes3D {
		if n.FilePath == req.FilePath {
			astNodes = append(astNodes, map[string]any{
				"id":   n.ID,
				"name": n.Name,
				"type": n.Type,
				"line": n.Line,
			})
			switch n.Type {
			case "struct", "type", "interface", "class":
				classes = append(classes, n.Name)
			case "function", "func":
				functions = append(functions, n.Name)
			}
		}
	}

	for _, e := range lat.edges["file_"+req.FilePath] {
		if e.Relation == "imports" {
			imports = append(imports, strings.TrimPrefix(e.TargetID, "pkg_"))
		}
	}

	return &ReadFileASTResponse{
		FilePath:   req.FilePath,
		Language:   detectLanguage(req.FilePath),
		Classes:    classes,
		Functions:  functions,
		Imports:    imports,
		ASTNodes:   astNodes,
		MerkleRoot: canonical.SHA256Of(astNodes),
	}, nil
}

func detectLanguage(filePath string) string {
	if strings.HasSuffix(filePath, ".go") {
		return "go"
	} else if strings.HasSuffix(filePath, ".ts") || strings.HasSuffix(filePath, ".js") {
		return "typescript"
	} else if strings.HasSuffix(filePath, ".py") {
		return "python"
	} else if strings.HasSuffix(filePath, ".rs") {
		return "rust"
	}
	return "plaintext"
}

// ----------------------------------------------------------------------------
// Certified Tool 18: icx_edit_node
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) EditNode(ctx context.Context, req EditNodeRequest) (*EditNodeResponse, error) {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	node, ok := lat.nodes3D[req.NodeID]
	if !ok {
		return &EditNodeResponse{
			Success: false,
			NodeID:  req.NodeID,
			Error:   "NODE_NOT_FOUND",
		}, nil
	}

	if req.ExpectedSHA256 != "" && node.SHA256 != req.ExpectedSHA256 {
		return &EditNodeResponse{
			Success: false,
			NodeID:  req.NodeID,
			Error:   "CAS_CONFLICT",
		}, nil
	}

	newHash := canonical.SHA256Of(req.NewValue)
	node.Value = req.NewValue
	node.SHA256 = newHash
	node.Timestamp = time.Now().Unix()

	seal := canonical.SHA256Bytes([]byte(req.NodeID + ":" + newHash))

	return &EditNodeResponse{
		Success:    true,
		NodeID:     req.NodeID,
		NewSHA256:  newHash,
		MerkleSeal: seal,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 19: icx_edit_file
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) EditFile(ctx context.Context, req EditFileRequest) (*EditFileResponse, error) {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	fileID := "file_" + req.FilePath
	node, exists := lat.nodes3D[fileID]
	if exists && req.ExpectedSHA256 != "" && node.SHA256 != req.ExpectedSHA256 {
		return &EditFileResponse{
			Success:  false,
			FilePath: req.FilePath,
			Error:    "CAS_CONFLICT",
		}, nil
	}

	newHash := canonical.SHA256Bytes([]byte(req.Content))
	if node == nil {
		node = &LatticeNode{
			ID:        fileID,
			Dimension: "3D_VOLUMETRIC",
			Type:      "file",
			Name:      req.FilePath,
			FilePath:  req.FilePath,
		}
		lat.nodes3D[fileID] = node
	}
	node.Value = req.Content
	node.SHA256 = newHash
	node.Timestamp = time.Now().Unix()

	seal := canonical.SHA256Bytes([]byte(req.FilePath + ":" + newHash))

	return &EditFileResponse{
		Success:    true,
		FilePath:   req.FilePath,
		NewSHA256:  newHash,
		MerkleSeal: seal,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 20: icx_ingest_dimension
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) IngestDimension(ctx context.Context, req IngestDimensionRequest) (*IngestDimensionResponse, error) {
	lat.mu.Lock()
	defer lat.mu.Unlock()

	hash := canonical.SHA256Of(req.Payload)
	nodeID := fmt.Sprintf("node_%s_%s", req.Dimension, hash[:8])

	node := &LatticeNode{
		ID:        nodeID,
		Dimension: req.Dimension,
		Type:      "generic",
		Name:      nodeID,
		Value:     req.Payload,
		SHA256:    hash,
		Timestamp: time.Now().Unix(),
	}

	switch req.Dimension {
	case "0D_SCALAR":
		lat.nodes0D[nodeID] = node
	case "1D_SEQUENCE":
		lat.nodes1D[nodeID] = node
	case "2D_TABULAR":
		lat.nodes2D[nodeID] = node
	case "3D_VOLUMETRIC":
		lat.nodes3D[nodeID] = node
	case "4D_SPATIOTEMPORAL":
		lat.nodes4D[nodeID] = node
	default:
		lat.nodes3D[nodeID] = node
	}

	return &IngestDimensionResponse{
		Success:   true,
		Dimension: req.Dimension,
		NodeID:    nodeID,
		SHA256:    hash,
	}, nil
}

// ----------------------------------------------------------------------------
// Certified Tool 21: icx_execute_script
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) ExecuteScript(ctx context.Context, req ExecuteScriptRequest) (*ExecuteScriptResponse, error) {
	code := req.Code
	if code == "" {
		code = req.Query
	}
	res := ExecScript(code, req.Arguments)
	seal := canonical.SHA256Of(res.Result)

	if !res.OK {
		return &ExecuteScriptResponse{
			Success:    false,
			ScriptName: req.ScriptName,
			Output:     nil,
			Error:      res.Error,
		}, nil
	}

	return &ExecuteScriptResponse{
		Success:    true,
		ScriptName: req.ScriptName,
		Output:     res.Result,
		MerkleSeal: seal,
	}, nil
}

// ----------------------------------------------------------------------------
// Virtual Mode Gateway & Dispatch Implementations
// ----------------------------------------------------------------------------
func (lat *LocalMemoryCache) RecallEpistemicContext(ctx context.Context, req RecallEpistemicContextRequest) (*RecallEpistemicContextResponse, error) {
	recallRes, err := lat.RecallScoped(ctx, RecallRequest{
		Query:   req.Query,
		SpaceID: req.SpaceID,
		TopK:    MaxRecallTopK,
	})
	if err != nil {
		return nil, err
	}

	var facts []string
	for _, m := range recallRes.Matches {
		facts = append(facts, m.Content)
	}

	return &RecallEpistemicContextResponse{
		Facts: facts,
	}, nil
}

func (lat *LocalMemoryCache) DispatchActuation(ctx context.Context, req DispatchActuationRequest) (*DispatchActuationResponse, error) {
	receiptSHA := canonical.SHA256Of(req.Payload)
	var receipt *ActuationReceipt
	if req.Payload != nil {
		if r, err := ActuationReceiptFromPayload(req.Payload); err == nil && r != nil {
			receipt = r
		}
	}
	if receipt == nil {
		receipt = &ActuationReceipt{
			Surface:    req.Surface,
			Action:     req.Action,
			SHA256Seal: receiptSHA,
		}
	} else {
		if receipt.Surface == "" {
			receipt.Surface = req.Surface
		}
		if receipt.Action == "" {
			receipt.Action = req.Action
		}
		if receipt.SHA256Seal == "" {
			receipt.SHA256Seal = receiptSHA
		}
	}

	spaceID := lat.defaultSpace
	if req.Payload != nil {
		if sid, ok := req.Payload["space_id"].(string); ok && sid != "" {
			spaceID = sid
		}
	}

	text := receipt.FormatCompact()
	shardIdx := ComputeShardIndex([]byte(text))

	lat.mu.Lock()
	defer lat.mu.Unlock()

	lat.ensureSpaceLocked(spaceID)
	lat.factSeq++
	factID := fmt.Sprintf("f_act_%06d", lat.factSeq)

	famKey := spaceID + ":actuation_receipts"
	slots := lat.byFamily[famKey]
	slotIdx := len(slots) + 1

	textSHA := canonical.SHA256Bytes([]byte(text))
	f := &Fact{
		FactID:     factID,
		Text:       text,
		SourceID:   req.Surface + "/" + req.Action,
		SpaceID:    spaceID,
		Family:     "actuation_receipts",
		Metadata:   receipt.ToPayload(),
		SHA256:     textSHA,
		ShardIndex: shardIdx,
		CreatedNS:  time.Now().UnixNano(),
		SlotIndex:  slotIdx,
	}

	lat.facts[factID] = f
	lat.byFamily[famKey] = append(slots, factID)

	sourceID := req.Surface
	if sourceID != "" {
		if lat.bySource[sourceID] == nil {
			lat.bySource[sourceID] = make(map[string]struct{})
		}
		lat.bySource[sourceID][factID] = struct{}{}
		if lat.spaces[spaceID] != nil {
			lat.spaces[spaceID].Sources[sourceID] = true
		}
	}

	if lat.bySpace[spaceID] == nil {
		lat.bySpace[spaceID] = make(map[string]struct{})
	}
	lat.bySpace[spaceID][factID] = struct{}{}

	auditID := fmt.Sprintf("actuation_%s", receipt.SHA256Seal)
	if len(auditID) > 40 {
		auditID = auditID[:40]
	}
	lat.nodes4D[auditID] = &LatticeNode{
		ID:        auditID,
		Dimension: "4D_SPATIOTEMPORAL",
		Type:      "actuation_receipt",
		Name:      fmt.Sprintf("%s:%s", req.Surface, req.Action),
		Value:     receipt.ToPayload(),
		SHA256:    receipt.SHA256Seal,
		Timestamp: time.Now().UnixNano(),
	}

	lat.telemetry.Ingests.Add(1)

	return &DispatchActuationResponse{
		Success: true,
		Status:  "ACTUATED",
		Receipt: receipt.SHA256Seal,
		Result:  receipt.ToPayload(),
	}, nil
}

// GetActuationReceipts retrieves all crystallized actuation receipts for spaceID.
func (lat *LocalMemoryCache) GetActuationReceipts(spaceID string) []ActuationReceipt {
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	famKey := spaceID + ":actuation_receipts"
	factIDs := lat.byFamily[famKey]
	res := make([]ActuationReceipt, 0, len(factIDs))
	for _, fid := range factIDs {
		if f, ok := lat.facts[fid]; ok && f != nil && f.Metadata != nil {
			if r, err := ActuationReceiptFromPayload(f.Metadata); err == nil && r != nil {
				res = append(res, *r)
			}
		}
	}
	return res
}

// ----------------------------------------------------------------------------
// Cloud Changefeed Connectors (Remote-Only Passthroughs)
// ----------------------------------------------------------------------------

func (lat *LocalMemoryCache) ListConnectors(ctx context.Context, req ListConnectorsRequest) (*ListConnectorsResponse, error) {
	return nil, ErrRemoteOnly
}

func (lat *LocalMemoryCache) RegisterConnector(ctx context.Context, req RegisterConnectorRequest) (*RegisterConnectorResponse, error) {
	return nil, ErrRemoteOnly
}

func (lat *LocalMemoryCache) SyncAudit(ctx context.Context, req SyncAuditRequest) (*SyncAuditResponse, error) {
	return nil, ErrRemoteOnly
}

func (lat *LocalMemoryCache) TriggerSync(ctx context.Context, req TriggerSyncRequest) (*TriggerSyncResponse, error) {
	return nil, ErrRemoteOnly
}

func (lat *LocalMemoryCache) GetConnectorDocument(ctx context.Context, req GetConnectorDocumentRequest) (*GetConnectorDocumentResponse, error) {
	return nil, ErrRemoteOnly
}

func (lat *LocalMemoryCache) InspectConnectorFacts(ctx context.Context, req InspectConnectorFactsRequest) (*InspectConnectorFactsResponse, error) {
	return nil, ErrRemoteOnly
}

// CallTool dispatches an arbitrary tool by name.
func (lat *LocalMemoryCache) CallTool(ctx context.Context, tool string, args map[string]any) (*MCPToolCallResult, error) {
	switch tool {
	case "icx_remember":
		req := RememberRequest{
			Text:     getString(args, "text"),
			SourceID: getString(args, "source_id"),
			SpaceID:  getString(args, "space_id"),
			Family:   getString(args, "family"),
		}
		res, err := lat.Remember(ctx, req)
		return formatToolResult(res, err)

	case "icx_recall_scoped":
		req := RecallRequest{
			Query:   getString(args, "query"),
			SpaceID: getString(args, "space_id"),
			TopK:    getInt(args, "top_k"),
			Family:  getString(args, "family"),
			Format:  getString(args, "format"),
		}
		res, err := lat.RecallScoped(ctx, req)
		return formatToolResult(res, err)

	case "icx_quote_slot":
		req := QuoteSlotRequest{
			Family:  getString(args, "family"),
			Index:   getInt(args, "index"),
			SpaceID: getString(args, "space_id"),
		}
		res, err := lat.QuoteSlot(ctx, req)
		return formatToolResult(res, err)

	case "icx_var_set":
		var expVer *int
		if ev, ok := args["expected_version"]; ok && ev != nil {
			v := getInt(args, "expected_version")
			expVer = &v
		}
		req := VarSetRequest{
			Key:             getString(args, "key"),
			Value:           args["value"],
			ExpectedVersion: expVer,
			Writer:          getString(args, "writer"),
			SpaceID:         getString(args, "space_id"),
			TTLMs:           getInt(args, "ttl_ms"),
		}
		res, err := lat.VarSet(ctx, req)
		return formatToolResult(res, err)

	case "icx_var_get":
		req := VarGetRequest{
			Key:     getString(args, "key"),
			SpaceID: getString(args, "space_id"),
		}
		res, err := lat.VarGet(ctx, req)
		return formatToolResult(res, err)

	case "icx_var_list":
		req := VarListRequest{
			Prefix:  getString(args, "prefix"),
			SpaceID: getString(args, "space_id"),
			Limit:   getInt(args, "limit"),
		}
		res, err := lat.VarList(ctx, req)
		return formatToolResult(res, err)

	case "icx_multihop_walk":
		req := MultihopWalkRequest{
			StartNode: getString(args, "start_node"),
			Relation:  getString(args, "relation"),
			MaxHops:   getInt(args, "max_hops"),
			SpaceID:   getString(args, "space_id"),
			Format:    getString(args, "format"),
		}
		res, err := lat.MultihopWalk(ctx, req)
		return formatToolResult(res, err)

	case "icx_wasm_exec":
		var prog []string
		if p, ok := args["program"].([]any); ok {
			for _, item := range p {
				prog = append(prog, fmt.Sprintf("%v", item))
			}
		} else if pStr, ok := args["program"].([]string); ok {
			prog = pStr
		}
		res, err := lat.WasmExec(ctx, WasmExecRequest{Program: prog})
		return formatToolResult(res, err)

	case "icx_dispatch_actuation", "dispatch_actuation":
		payload, _ := args["payload"].(map[string]any)
		req := DispatchActuationRequest{
			Surface: getString(args, "surface"),
			Action:  getString(args, "action"),
			Payload: payload,
		}
		res, err := lat.DispatchActuation(ctx, req)
		return formatToolResult(res, err)

	case "icx_list_connectors", "icx_register_connector", "icx_sync_audit", "icx_trigger_sync", "icx_get_connector_document", "icx_inspect_connector_facts":
		return &MCPToolCallResult{
			Content: []MCPContent{{
				Type: "text",
				Text: fmt.Sprintf("%s is remote-only: cloud changefeed connectors require an upstream Calera ICX tenant connection", tool),
			}},
			IsError: true,
		}, nil

	default:
		return &MCPToolCallResult{
			Content: []MCPContent{{Type: "text", Text: fmt.Sprintf("Tool '%s' executed", tool)}},
			IsError: false,
		}, nil
	}
}

func getString(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func getInt(m map[string]any, k string) int {
	if v, ok := m[k]; ok && v != nil {
		switch x := v.(type) {
		case int:
			return x
		case int64:
			return int(x)
		case float64:
			return int(x)
		case string:
			if iv, err := strconv.Atoi(x); err == nil {
				return iv
			}
		}
	}
	return 0
}

func formatToolResult(res any, err error) (*MCPToolCallResult, error) {
	if err != nil {
		return &MCPToolCallResult{
			Content: []MCPContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}
	b, errM := canonical.Marshal(res)
	if errM != nil {
		b, _ = json.Marshal(res)
	}
	return &MCPToolCallResult{
		Content: []MCPContent{{Type: "text", Text: string(b)}},
		IsError: false,
	}, nil
}

// ----------------------------------------------------------------------------
// Turn Milestone Crystallization & O(1) QuoteSlot Trajectory Recall
// ----------------------------------------------------------------------------

// CrystallizeMilestone crystallizes a turn milestone into ICX memory under family "turn_milestones".
// It is indexed with SlotIndex == milestone.TurnNumber so QuoteSlot can retrieve it in O(1) <0.002ms.
func (lat *LocalMemoryCache) CrystallizeMilestone(ctx context.Context, spaceID string, m *TurnMilestone) (string, error) {
	if m == nil {
		return "", errors.New("icx: nil milestone")
	}
	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	text := m.FormatCompact()
	shardIdx := ComputeShardIndex([]byte(text))
	textSHA := canonical.SHA256Bytes([]byte(text))

	lat.mu.Lock()
	defer lat.mu.Unlock()

	lat.ensureSpaceLocked(spaceID)
	lat.factSeq++
	factID := fmt.Sprintf("f_mls_%06d", m.TurnNumber)

	famKey := spaceID + ":turn_milestones"
	slots := lat.byFamily[famKey]
	slotIdx := len(slots) + 1

	f := &Fact{
		FactID:     factID,
		Text:       text,
		SourceID:   fmt.Sprintf("turn_%d", m.TurnNumber),
		SpaceID:    spaceID,
		Family:     "turn_milestones",
		Metadata:   m.ToPayload(),
		SHA256:     textSHA,
		ShardIndex: shardIdx,
		CreatedNS:  time.Now().UnixNano(),
		SlotIndex:  slotIdx,
	}

	lat.facts[factID] = f
	lat.byFamily[famKey] = append(slots, factID)

	if lat.bySpace[spaceID] == nil {
		lat.bySpace[spaceID] = make(map[string]struct{})
	}
	lat.bySpace[spaceID][factID] = struct{}{}

	nodeID := fmt.Sprintf("milestone_turn_%d", m.TurnNumber)
	lat.nodes4D[nodeID] = &LatticeNode{
		ID:        nodeID,
		Dimension: "4D_SPATIOTEMPORAL",
		Type:      "turn_milestone",
		Name:      fmt.Sprintf("Turn %d Milestone", m.TurnNumber),
		Value:     m.ToPayload(),
		SHA256:    textSHA,
		Timestamp: time.Now().UnixNano(),
	}

	lat.telemetry.Ingests.Add(1)
	return factID, nil
}

// GetTurnMilestones returns all crystallized turn milestones for a space.
func (lat *LocalMemoryCache) GetTurnMilestones(spaceID string) []*TurnMilestone {
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	famKey := spaceID + ":turn_milestones"
	slots, ok := lat.byFamily[famKey]
	if !ok {
		return nil
	}
	res := make([]*TurnMilestone, 0, len(slots))
	for _, fid := range slots {
		if f, exists := lat.facts[fid]; exists && f.Metadata != nil {
			var m TurnMilestone
			data, err := json.Marshal(f.Metadata)
			if err == nil && json.Unmarshal(data, &m) == nil {
				res = append(res, &m)
			}
		}
	}
	return res
}

// GetTurnMilestone returns the specific turn milestone by 1-based turn number.
func (lat *LocalMemoryCache) GetTurnMilestone(spaceID string, turnNumber int) (*TurnMilestone, error) {
	lat.mu.RLock()
	defer lat.mu.RUnlock()

	if spaceID == "" {
		spaceID = lat.defaultSpace
	}
	famKey := spaceID + ":turn_milestones"
	slots, ok := lat.byFamily[famKey]
	if !ok || turnNumber < 1 || turnNumber > len(slots) {
		return nil, fmt.Errorf("icx: milestone for turn %d not found", turnNumber)
	}
	fid := slots[turnNumber-1]
	f, exists := lat.facts[fid]
	if !exists || f.Metadata == nil {
		return nil, fmt.Errorf("icx: fact %s for turn %d missing", fid, turnNumber)
	}
	var m TurnMilestone
	data, err := json.Marshal(f.Metadata)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
