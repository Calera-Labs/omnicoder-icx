// SPDX-License-Identifier: Apache-2.0
// Package vm provides pure Go client primitives and hermetic sandboxing for Calera LLM-VM
// (@caleralabs/llm-vm-mcp), enabling isolated microVM execution, Fat-Payload protection,
// and instant Copy-on-Write (<10ms) snapshot checkpoints and rollbacks.

package vm

import (
	"context"
	"time"
)

// SandboxConfig specifies resource and isolation parameters for a microVM sandbox.
type SandboxConfig struct {
	MemoryMB       int    `json:"memory_mb,omitempty"`
	VCPUs          int    `json:"vcpus,omitempty"`
	Backend        string `json:"backend,omitempty"` // "auto", "microvm-container", "qemu-microvm", "wsl2", "local-fallback"
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	ICXAPIKey      string `json:"icx_api_key,omitempty"`
	WorkspaceDir   string `json:"workspace_dir,omitempty"`
}

// Sandbox represents an active microVM sandbox handle.
type Sandbox struct {
	ID           string    `json:"id"`
	WorkspaceDir string    `json:"workspace_dir"`
	Backend      string    `json:"backend"`
	CreatedAt    time.Time `json:"created_at"`
	TimeoutSec   int       `json:"timeout_seconds"`
}

// CommandRequest encapsulates parameters for executing commands in the sandbox.
type CommandRequest struct {
	SandboxID      string `json:"sandbox_id"`
	Command        string `json:"command"`
	Script         string `json:"script,omitempty"`
	Shell          string `json:"shell,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Raw            bool   `json:"raw,omitempty"`
}

// SandboxStatus provides explicit telemetry on sandbox isolation, workspace persistence, and rollback triggers.
type SandboxStatus struct {
	IsEphemeral          bool   `json:"is_ephemeral"`
	PersistedToWorkspace bool   `json:"persisted_to_workspace"`
	WorkspaceDir         string `json:"workspace_dir"`
	SandboxDir           string `json:"sandbox_dir,omitempty"`
	RolledBack           bool   `json:"rolled_back"`
	RollbackReason       string `json:"rollback_reason,omitempty"`
}

// CommandResult reports the command execution outcome with Fat-Payload protection.
type CommandResult struct {
	ExitCode        int            `json:"exit_code"`
	Stdout          string         `json:"stdout"`
	Stderr          string         `json:"stderr"`
	Summary         string         `json:"summary,omitempty"` // <80 tokens token-budget summary
	VaultedSlot     string         `json:"vaulted_slot,omitempty"`
	ExecutionEngine string         `json:"execution_engine"`
	IsTruncated     bool           `json:"is_truncated"`
	SandboxStatus   *SandboxStatus `json:"sandbox_status,omitempty"`
}

// Checkpoint represents a point-in-time snapshot of the sandbox file tree.
type Checkpoint struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ContainmentReport reports the results of airgap, rootfs, and UID isolation probes.
type ContainmentReport struct {
	AirgapVerified    bool   `json:"airgap_verified"`
	ReadOnlyRootfs    bool   `json:"read_only_rootfs"`
	NonRootUID        bool   `json:"non_root_uid"`
	MathematicalProof string `json:"mathematical_proof"` // \partial^2 = 0
	Details           string `json:"details"`
}

// Client defines the contract for communicating with Calera LLM-VM MCP.
type Client interface {
	CreateSandbox(ctx context.Context, cfg SandboxConfig) (*Sandbox, error)
	RunCommand(ctx context.Context, req CommandRequest) (*CommandResult, error)
	WriteFile(ctx context.Context, sandboxID, relPath, content string) error
	ReadFile(ctx context.Context, sandboxID, relPath string) (string, error)
	Snapshot(ctx context.Context, sandboxID, label string) (string, error)
	Rollback(ctx context.Context, sandboxID, checkpointID string) error
	DestroySandbox(ctx context.Context, sandboxID string) error
	VerifyContainment(ctx context.Context, sandboxID string) (*ContainmentReport, error)
	SyncICXMemory(ctx context.Context, sandboxID string) error
	ClaimFreeTokens(ctx context.Context, email, agentName string) (string, error)
}
