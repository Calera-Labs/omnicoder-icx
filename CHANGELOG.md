# Changelog

All notable changes to **OmniCoder-ICX** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.1.0] — 2026-11-01

### Added
- **Open-Weights & Guided JSON Resilience (`pkg/llm`)**:
  - Automatic non-RFC-8259 JSON backslash sanitization (`\w`, `\s`, `\d`, `\A`, `\Z`, `\'`, invalid `\u`) inside JSON string literals via `escapeControlCharsInJSON`.
  - Truncated JSON edit-block extraction (`parseLooseJSONEditBlocks`, `extractJSONStringAfterKey`) when `finish_reason="length"` cuts off mid-response.
  - Guided JSON mode (`response_format: {"type": "json_object"}`) across all 6 decision gates via `SupportsJSONObjectFormat`, with automatic HTTP `400`/`422` fallback in `Client.Complete`.
  - Dynamic 24,576-token context ceilings and 2,048-token output budgets for 27B–70B+ and large-context models (`IsLargeContextModel`).
- **Best-Candidate Speculative Beam Retention & Elision-Safe Patching (`cmd/omnicoder`, `pkg/devops`)**:
  - `executeHeal` retains and applies the highest-scoring syntax-valid candidate (`bestFallbackBlocks`) across speculative beam turns (`--beam-k > 1`) when multi-step problems require partial progress.
  - `ReplaceFuzzy` automatically strips synthetic prompt context headers (`# --- Additional Relevant`, `=== Swarm Supporting File:`) and splices across `... (docstring elided)` markers.
  - Added `--cmd` / `--command` flags to `omnicoder heal` and auto-inlined root `PROMPT.md` instructions in `--direct` mode while preserving syntactically valid deliverables.
- **MCP Local-Model Guardrails & Telemetry (`pkg/mcp`, `pkg/devops`)**:
  - Immediate syntax and Go same-package `func main()` collision detection (`checkWrittenFileSyntaxAndCollisions`) on `write_file` and `omnicoder_patch`, surfaced via `syntax_warning` and `next` steering hints.
  - Automatic LLM path artifact sanitization (`CleanLLMPathArtifacts`) stripping trailing markdown backticks (`` ` ``), wrapping quotes, and shell prompt sigils (`%`, `$`).
  - Temp-dir isolation (`os.TempDir()`), `srvCmd.Wait()` process handle release, and stale binary sweeping for `omnicoder_verify_service` (`RunServiceVerifier`).
  - Actionable 15-line `Closest context` snippet with 1-based line numbers (`FindClosestBlockContext`) attached to `BLOCK_MATCH_FAILED` / `FUZZY_SEARCH_MISMATCH` errors.
  - Tracked test deletion guard in `git_commit`, `git_status`, and `omnicoder_doctor` (`DeletedTests`, `allow_test_deletion`).
  - Interactive steering on repeated full-file `write_file` overwrites (`OverwroteExisting`, `PreviousLines`) directing local models toward surgical `omnicoder_patch` calls.
  - Umbrella workspace subproject fallback (`resolveUmbrellaGitSubproject`) and `workspace` argument support across `git_status`, `git_diff`, and `git_commit`.
  - Automatic `.clinerules` seeding (`EnsureLocalClineRules`) with `.git/info/exclude` hygiene when switching into a workspace with `.cline/` or an active Cline session.
  - Compact local-model tool schemas, interactive `next` hints, and unredacted `token_budget`, `max_tokens`, and token count fields in `.omnicoder/mcp_trace.jsonl` (`SanitizeArguments`).

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
