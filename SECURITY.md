# Security Policy

## Supported Versions

The following versions of **OmniCoder-ICX** currently receive security updates:

| Version | Supported          |
| ------- | ------------------ |
| 1.0.x   | :white_check_mark: |
| < 1.0   | :x:                |

## Reporting a Vulnerability

We take the security of **OmniCoder-ICX** seriously. If you discover a security vulnerability, **please do not report it in a public GitHub issue**.

Instead, please report vulnerabilities privately through one of the following channels:

1. **GitHub Private Vulnerability Reporting:** Use the [Report a vulnerability](https://github.com/Calera-Labs/omnicoder-icx/security/advisories/new) button under the repository's **Security** tab.
2. **Email:** Send a detailed report to **Calera Computing, Inc.** at **support@caleralabs.com**.

### What to Include in Your Report

- A clear description of the vulnerability and its potential impact.
- Affected package(s), file path(s), and command or MCP tool surface (e.g., `pkg/mcp`, `pkg/vm`, `pkg/engine`, `cmd/omnicoder`).
- Minimal, reproducible steps or proof-of-concept input demonstrating the issue.
- Operating system, Go version, and configuration flags (`--sandbox`, `--offline`, etc.).

### Response Timeline

- **Acknowledgment:** Within **48 hours** of receipt.
- **Triage & Assessment:** Within **5 business days**.
- **Remediation & Coordinated Disclosure:** We aim to release a patch within **14–30 days** depending on severity and will credit reporters in the release notes unless anonymity is requested.

---

## Security Architecture Overview

OmniCoder-ICX enforces multiple defense-in-depth boundaries when executing autonomous code edits and MCP tool calls:

1. **Workspace Path Containment Fence (`pkg/mcp`):**
   - Rejects absolute paths outside the active workspace root, parent-directory traversal (`..`), UNC network paths (`\\server\share`), and OS/system directories (`C:\Windows`, `/etc`, `/proc`, `/sys`, `~/.ssh`, `~/.aws`).
   - Enforces symlink resolution (`filepath.EvalSymlinks`) so symlinks inside the workspace cannot escape to files outside the workspace boundary.
2. **Copy-on-Write (CoW) Transactional Sandbox (`pkg/vm`, `pkg/devops`):**
   - Stages candidate edits in an isolated workspace overlay and runs syntax, vet, and test gates before atomically committing verified diffs to the working tree.
3. **Credential & Secret Redaction (`pkg/engine`):**
   - Automatically scrubs API keys (`sk-*`, `AIza*`, `ghp_*`), bearer tokens, and environment credentials from persisted execution traces (`trace.jsonl`) and diagnostic receipts.
4. **Offline / Air-Gapped Mode (`--offline`):**
   - Restricts all inference and tool traffic to loopback (`127.0.0.1` / `localhost`) local model daemons (`Ollama`, `LM Studio`, `vLLM`).
