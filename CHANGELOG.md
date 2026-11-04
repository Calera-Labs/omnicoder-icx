# Changelog

All notable changes to **OmniCoder-ICX** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.2.0] — 2026-11-03

### Added
- **Config 3 `v4` Coupling Laws & Official Zero-Leakage `SWE-300` / `SWE-900` Upgrades (`pkg/devops/localizer.go`, `cmd/omnicoder/main.go`, `pkg/vln`, `training/colab_a100_swe300_parallel.py`)**:
  - **Law 1 Stack-Trace Exception-Helper Dampening:** Dampens deepest traceback frames (`s_tb_deep`) when domain symbol usage is absent (`s_usage == 0`), boosting intermediate caller frames where issue domain symbols co-occur.
  - **Stage-1 Code-Structured Signal Extraction & 1-Hop Import-Graph Propagation (`extract_issue_signals`, `LocalizeFaults`):** Extracts code-only title tokens, backticked/quoted `.py` filenames, diagnostic codes (`models.E028`), hyphenated CLI flags, `UPPER_CASE` settings, CamelCase/alphanumeric identifiers (`Line3D`), class prefixes of dotted expressions, snake_case directory+stem synergy, batched multi-symbol co-occurrence bonuses, strict non-source path filtering (`doc/`, `testing/`, `examples/`, `benchmarks/`, `tutorials/`, `galleries/`), and 1-hop relative import-graph resolution (`from .<submod> import <sym>`).
  - **Law 2 Dual-Symbol 16-Line Focal Windowing (`extract_py_focal_windows`):** Expands `[OC:HEAL]` focal windows to 16 lines (`580` chars), centers traceback pins near `ws + 5`, prefers innermost `FunctionDef`/`AsyncFunctionDef` over outer `ClassDef` shells, and routes secondary window `w2` (`K=2`) to the #2 ranked non-overlapping function in the primary file (`2.18x` increase in gold-hunk window overlap: `49/300` $\to$ `107/300`).
  - **Law 3 Unbiased Speculative `K=4` Tournament & Law 4 $\Delta\text{indent}$ AST Splicing:** Removes static beam prior penalties between `K=0..3` and adds automatic indentation delta (`\Delta\text{indent}`) shifting + unique `(first_line, last_line)` boundary anchor matching (`99.3%` real-source anchor match, `96.7%` AST-verified patch rate).
  - **Empirical `SWE-300` (`princeton-nlp/SWE-bench_Lite`, `0%` overlap) Results:** Top-1 File Localization jumped from `35.3%` (`v1`) $\to$ `40.3%` (`v2`) $\to$ `45.0%` (`v3`) $\to$ **`50.3%` (`151/300`, `v4`)**; Stage-1 Top-6 Recall reached **`75.0%` (`225/300`)**; Valid Python AST Reproducers reached **`100.0%` (`300/300`)**; and End-to-End `GoldFile` AST-Verified Patches reached **`42.7%` (`128/300`)** in **`8.46 minutes`** (`2.86x` faster than `v1`).

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
