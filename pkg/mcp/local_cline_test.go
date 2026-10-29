// SPDX-License-Identifier: Apache-2.0

package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/mcp"
)

func TestLocalCline_CommitAliasIsNotAListedTool(t *testing.T) {
	srv := mcp.NewServer(t.TempDir())
	defer srv.Close()

	def, _, ok := srv.GetTool("omnicoder_git_commit")
	if !ok || def.Name != "git_commit" {
		t.Fatalf("omnicoder_git_commit should resolve to git_commit, got %+v ok=%v", def, ok)
	}
	for _, listed := range srv.ListTools() {
		if listed.Name == "omnicoder_git_commit" {
			t.Fatal("commit alias must not be added to tools/list")
		}
	}
}

func TestLocalCline_DoctorRepeatAndCheapDefault(t *testing.T) {
	srv := mcp.NewServer(t.TempDir())
	defer srv.Close()
	mcp.RegisterOmniCoderTools(srv, nil)
	srv.SetClientInfo("Cline", "qwen/qwen3.8-27b")

	ctx := context.Background()
	first, err := srv.Dispatch(ctx, "omnicoder_doctor", map[string]any{
		"include_git":     false,
		"extract_version": false,
	})
	if err != nil {
		t.Fatalf("doctor failed: %v", err)
	}
	doc, ok := first.(*devops.DoctorResult)
	if !ok {
		t.Fatalf("expected *devops.DoctorResult, got %T", first)
	}
	if doc.Gates.Ran {
		t.Fatal("interactive doctor ran build gates by default")
	}
	if doc.LintStatus != "" {
		t.Fatalf("interactive doctor ran lint by default: %q", doc.LintStatus)
	}
	if !strings.Contains(doc.Next, "git_commit") && !strings.Contains(doc.Next, "Do not call omnicoder_doctor again") {
		t.Fatalf("doctor next line missing stop instruction: %q", doc.Next)
	}

	second, err := srv.Dispatch(ctx, "omnicoder_doctor", map[string]any{
		"include_git":     false,
		"extract_version": false,
	})
	if err != nil {
		t.Fatalf("repeat doctor failed: %v", err)
	}
	repeat, ok := second.(map[string]any)
	if !ok {
		t.Fatalf("expected repeat map, got %T", second)
	}
	if repeat["status"] != "UNCHANGED" {
		t.Fatalf("expected UNCHANGED, got %#v", repeat)
	}
	if !strings.Contains(repeat["next"].(string), "git_commit") {
		t.Fatalf("repeat next line: %#v", repeat["next"])
	}
}

func TestLocalCline_ReadAndChunkString(t *testing.T) {
	dir := t.TempDir()
	srv := mcp.NewServer(dir)
	defer srv.Close()
	mcp.RegisterOmniCoderTools(srv, nil)

	big := strings.Repeat("abcdefghij\n", 2000)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	note := "alpha\nbeta\n"
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	readRes, err := srv.Dispatch(ctx, "read_file", map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	rf := readRes.(*mcp.ReadFileResult)
	if !rf.Truncated || len(rf.Content) > 8192 || !strings.Contains(rf.Next, "offset=") {
		t.Fatalf("expected truncated read, truncated=%v len=%d next=%q", rf.Truncated, len(rf.Content), rf.Next)
	}

	_, err = srv.Dispatch(ctx, "omnicoder_patch", map[string]any{
		"path":   "note.txt",
		"chunks": `[{"start":1,"end":1,"content":"ALPHA"}]`,
	})
	if err != nil {
		t.Fatalf("chunks string patch failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "ALPHA") || strings.Contains(string(got), "alpha") {
		t.Fatalf("chunks string did not apply: %q", got)
	}
}

func TestLocalCline_TraceFollowsWorkspaceAndStampsClient(t *testing.T) {
	origin := t.TempDir()
	other := t.TempDir()
	tracePath := filepath.Join(origin, ".omnicoder", "mcp_trace.jsonl")
	srv := mcp.NewServer(origin, mcp.WithTraceFile(tracePath, false))
	srv.SetClientInfo("Cline", "qwen/qwen3.8-27b")
	if err := srv.SetWorkspace(other); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, path := range []string{tracePath, filepath.Join(other, ".omnicoder", "mcp_trace.jsonl")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var ev mcp.TraceEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &ev); err != nil {
			t.Fatalf("parse %s: %v body=%s", path, err, raw)
		}
		if ev.Client != "Cline" || ev.Model != "qwen/qwen3.8-27b" {
			t.Fatalf("%s client=%q model=%q", path, ev.Client, ev.Model)
		}
		if ev.Event != "workspace_update" || ev.Workspace != other {
			t.Fatalf("%s event=%s workspace=%s", path, ev.Event, ev.Workspace)
		}
	}
}
