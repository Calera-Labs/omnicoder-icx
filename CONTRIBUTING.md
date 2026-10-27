# Contributing to OmniCoder-ICX

Thank you for your interest in contributing to **OmniCoder-ICX** by **Calera Computing, Inc.** ([https://caleralabs.com](https://caleralabs.com))! This document outlines the cultural principles, architectural invariants, and engineering standards enforced across the repository.

---

## 1. Cultural Mandate: The Ezekiel Gate & *"Never Faking It"*

All contributions to OmniCoder-ICX are governed by our [Code of Conduct & Stewardship Charter](CODE_OF_CONDUCT.md):

1. **The Watchman's Obligation (*The Ezekiel Gate — Ezekiel 3:17–18*):**
   - We build deterministic engineering tools that remain under accountable human-in-the-loop oversight—never simulated personas, unaccountable oracles, or surveillance/weaponized systems.
2. **Counter-Sycophancy & *"Never Faking It"*:**
   - Never mask a failing test, compiler error, or edge case with placeholder stubs (`// TODO`), hardcoded test-fitting, or mock doubles pretending to be real execution.
   - If a benchmark or hypothesis fails, report the raw diagnostic truth and engineer the real fix.

---

## 2. Core Architectural Invariants

1. **Pure Go, Zero CGo (`CGO_ENABLED=0`)**:
   - All packages under `cmd/` and `pkg/` must compile with `CGO_ENABLED=0` across Windows, Linux, and macOS using the Go standard library.
   - Do not introduce C/C++ bindings or external shared library dependencies (`.dll`, `.so`, `.dylib`).

2. **Strict `< 300` Token Decision Card Budget (`pkg/llm/cards.go`)**:
   - Micro-decision cards (`GateEditCard`, `GateHealCard`, `GateDiagnoseCard`) must strictly obey the `< 300` token prompt budget.
   - Any changes to prompt card formatting must pass `go test -v ./pkg/llm/...`.

3. **Stdio Framing Isolation in MCP Mode (`pkg/mcp/server.go`)**:
   - When running as an MCP server (`omnicoder mcp`), `os.Stdout` is reserved exclusively for JSON-RPC 2.0 protocol frames.
   - All diagnostic logging and trace telemetry must be written to `os.Stderr` or `.omnicoder/mcp_trace.jsonl`.

---

## 3. Pre-Submission Verification Checklist

Before opening a pull request, run the validation suite:

```powershell
# Windows (PowerShell)
powershell -ExecutionPolicy Bypass -File scripts\validate_omnicoder.ps1
```

Or via standard Go commands:

```bash
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./pkg/storage/... ./pkg/engine/...
```

---

## 4. License & Stewardship

By contributing to OmniCoder-ICX, you agree that your contributions to the standalone OmniCoder codebase will be licensed under the [Apache License, Version 2.0](LICENSE) (see [NOTICE](NOTICE)) and abide by the [Calera Computing, Inc. Code of Conduct & Stewardship Charter](CODE_OF_CONDUCT.md). Note that the Apache 2.0 license applies solely to OmniCoder standalone (without Calera ICX); the **Calera ICX** platform is proprietary to **Calera Computing, Inc.** and is governed separately under the licensing terms and Stewardship Doctrine (*The Ezekiel Gate*) at [https://icx.caleralabs.com](https://icx.caleralabs.com) (contact: **support@caleralabs.com**).
