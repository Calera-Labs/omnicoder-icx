// Package orchestrator implements the isolated multi-process parallel agent swarm engine
// for OmniCoder-ICX.
//
// Architectural Invariants & Safety Charter:
//  1. Strictly Additive Architecture: Zero modifications to existing single-agent turn
//     execution pathways, CLI flag behaviors, or MCP tool contracts.
//  2. Dual Execution Isolation: Single-worker executions (omnicoder run / omnicoder_run)
//     remain completely untouched. Swarm orchestration is strictly opt-in via
//     'omnicoder swarm <plan.md>' or the 'omnicoder_swarm' MCP tool.
//  3. Windows NTFS Concurrency Safety: Complete immunity to 0x20 file sharing locks and
//     compiler test binary contention through hermetic Git worktree shards (.worktrees/wt_phase_<id>)
//     and NTFS directory junctions.
//  4. Two-Phase Commit (2PC) Reconciliation: Wave-based disjoint file boundary validation
//     prior to Git merges, coupled with integrated verification and automated Stage 6 self-healing.
//  5. ICX 4D Volumetric Swarm Memory: Wave 0 interface crystallization, AST attestation,
//     and dynamic variable exchange backed by pkg/icx CAS store.
//  6. Zero External Dependencies: Pure Go standard library and internal pkg/* modules only.
package orchestrator
