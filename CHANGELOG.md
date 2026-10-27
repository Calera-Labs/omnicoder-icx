# Changelog

All notable changes to **OmniCoder-ICX** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.0.0] — 2026-10-26

### Added
- **0D–4D Volumetric Repository Lattice (`pkg/icx`)**: Multi-resolution spatial code indexing (0D scalar invariants, 1D `<160`-token navigation ribbons, 2D BM25 symbol indices, 3D AST call/import graphs, and 4D Compare-And-Swap actuation receipts).
- **Sub-300-Token Micro-Decision Cards (`pkg/llm`)**: Bounded `<300`-token structured JSON decision schemas (`GateRouteCard`, `GateEditCard`, `GateHealCard`, `GateSummaryCard`) with automatic prompt window reset after each step.
- **8-Stage Turn Pipeline & Beam-K Tournament Healer (`pkg/engine`, `pkg/devops`)**:
  - AST dependency PageRank repository mapping (`<1,024` tokens).
  - 5-tier fuzzy code splicer (`ReplaceFuzzy`) supporting exact, whitespace-normalized, blank-line-tolerant, relative-indentation-preserving, and ellipsis-anchored patches.
  - Deterministic 0ms Python/Go import pre-healer (`attemptDeterministicPreHeal`) and multi-candidate (`--beam-k=3`) tournament scoring.
  - Intra-file call-graph and class `__init__` helper window extraction (`ExtractIntraFileCallGraphSlices`).
- **Parallel Swarm Forge (`pkg/orchestrator`)**: Multi-worker Git worktree orchestration with Wave 0 interface freeze, disjoint file ownership locks, and Two-Phase Commit (2PC) verification gates.
- **20 Model Context Protocol (MCP) Server Tools (`pkg/mcp`)**: Full JSON-RPC 2.0 MCP server (`omnicoder mcp`) with 19-point workspace path containment fence, automatic credential redaction, and structured JSONL audit tracing.
- **Cross-Platform Pure-Go Release Pipeline (`scripts/build_release.ps1`)**: Zero-CGo standalone binaries (`CGO_ENABLED=0`) for Windows (`amd64`), Linux (`amd64`), and macOS (`arm64`).
