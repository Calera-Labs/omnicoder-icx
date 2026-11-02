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
	if !strings.Contains(repeat["next"].(string), "omnicoder_heal") {
		t.Fatalf("repeat next line: %#v", repeat["next"])
	}
}

func TestLocalCline_RepeatListRefusesAndWriteSaysHeal(t *testing.T) {
	dir := t.TempDir()
	srv := mcp.NewServer(dir)
	defer srv.Close()
	mcp.RegisterOmniCoderTools(srv, nil)
	srv.SetClientInfo("Cline", "meta/muse-glimmer")

	ctx := context.Background()
	first, err := srv.Dispatch(ctx, "list_directory", map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	listed, ok := first.(map[string]any)
	if !ok {
		t.Fatalf("expected listed map, got %T", first)
	}
	if !strings.Contains(listed["next"].(string), "omnicoder_heal") {
		t.Fatalf("first list next: %#v", listed["next"])
	}

	second, err := srv.Dispatch(ctx, "list_directory", map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("repeat list failed: %v", err)
	}
	repeat, ok := second.(map[string]any)
	if !ok || repeat["status"] != "UNCHANGED" {
		t.Fatalf("expected UNCHANGED, got %#v", second)
	}
	if !strings.Contains(repeat["next"].(string), "omnicoder_heal") {
		t.Fatalf("repeat list next: %#v", repeat["next"])
	}

	wrote, err := srv.Dispatch(ctx, "write_file", map[string]any{
		"path":    "main.go",
		"content": "package main\nfunc main() {}\n",
	})
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	wf, ok := wrote.(*mcp.WriteFileResult)
	if !ok {
		t.Fatalf("expected WriteFileResult, got %T", wrote)
	}
	if !strings.Contains(wf.Next, "omnicoder_heal") || !strings.Contains(wf.Next, "main.go") {
		t.Fatalf("write next: %q", wf.Next)
	}

	after, err := srv.Dispatch(ctx, "list_directory", map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("list after write failed: %v", err)
	}
	if _, ok := after.(map[string]any); !ok {
		t.Fatalf("list after write should succeed once, got %T", after)
	}
	if m, ok := after.(map[string]any); ok && m["status"] == "UNCHANGED" {
		t.Fatal("write should allow one new listing")
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

func TestWriteFile_DuplicateMainWarning_And_TraceTokenBudget(t *testing.T) {
	dir := t.TempDir()
	srv := mcp.NewServer(dir)
	defer srv.Close()
	mcp.RegisterOmniCoderTools(srv, nil)
	srv.SetClientInfo("Cline", "google/gemma-4-31B-it")

	ctx := context.Background()
	_, err := srv.Dispatch(ctx, "write_file", map[string]any{
		"path":    "main.go",
		"content": "package main\nfunc main() {}\n",
	})
	if err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	second, err := srv.Dispatch(ctx, "write_file", map[string]any{
		"path":    "second.go",
		"content": "package main\nfunc main() {}\n",
	})
	if err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	wf, ok := second.(*mcp.WriteFileResult)
	if !ok {
		t.Fatalf("expected *mcp.WriteFileResult, got %T", second)
	}
	if !strings.Contains(wf.SyntaxWarning, "duplicate func main()") {
		t.Fatalf("expected duplicate func main() warning, got: %q", wf.SyntaxWarning)
	}
	if !strings.Contains(wf.Next, "WARNING:") {
		t.Fatalf("expected WARNING in Next line, got: %q", wf.Next)
	}

	sanitized := mcp.SanitizeArguments(map[string]any{
		"token_budget": 1200,
		"api_key":      "secret123",
	})
	if sanitized["token_budget"] == "[REDACTED]" {
		t.Fatalf("token_budget must not be redacted: %#v", sanitized)
	}
	if sanitized["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key must be redacted: %#v", sanitized)
	}
}

func TestResolveEffectiveWorkspace_RelativeSubprojectAndDoctorWorkdir(t *testing.T) {
	umbrella := t.TempDir()
	subProj := filepath.Join(umbrella, "P-08")
	if err := os.MkdirAll(subProj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subProj, "go.mod"), []byte("module example.com/p08\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subProj, "version.go"), []byte("package p08\n\nconst Version = \"2.4.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := mcp.NewServer(umbrella)
	defer srv.Close()
	mcp.RegisterOmniCoderTools(srv, nil)
	srv.SetClientInfo("Cline", "qwen/qwen3.8-27b")

	// 1. Relative subproject via workdir alias on omnicoder_doctor
	res, err := srv.Dispatch(context.Background(), "omnicoder_doctor", map[string]any{
		"workdir":         "P-08",
		"include_git":     false,
		"extract_version": true,
	})
	if err != nil {
		t.Fatalf("omnicoder_doctor with relative workdir failed: %v", err)
	}
	doc, ok := res.(*devops.DoctorResult)
	if !ok {
		t.Fatalf("expected *devops.DoctorResult, got %T", res)
	}
	if doc.Workspace != subProj {
		t.Fatalf("expected doctor workspace %s, got %s", subProj, doc.Workspace)
	}
	if doc.Version != "2.4.0" {
		t.Fatalf("expected version 2.4.0, got %q", doc.Version)
	}
}
