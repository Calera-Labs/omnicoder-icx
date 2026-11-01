// SPDX-License-Identifier: Apache-2.0
// local_cline.go keeps interactive local clients (Cline, Cursor, LM Studio)
// inside a small transcript: short tool results, one commit name, and a
// doctor call that does not invite a retry.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// localModelResultCap is the maximum payload, in bytes, returned to an
// interactive model for read, diff, list, and search results.
const localModelResultCap = 8192

// canonicalToolName maps names a local model invents onto the registered tool.
// The alias is not listed in tools/list, so the tool count stays unchanged.
func canonicalToolName(name string) string {
	switch strings.TrimSpace(name) {
	case "omnicoder_git_commit", "omnicoder__git_commit":
		return "git_commit"
	case "omnicoder_git_status", "omnicoder__git_status":
		return "git_status"
	case "omnicoder_git_diff", "omnicoder__git_diff":
		return "git_diff"
	default:
		return name
	}
}

func (s *Server) setClient(p InitializeParams) {
	model := strings.TrimSpace(os.Getenv("OMNICODER_TRACE_MODEL"))
	if p.InitializationOptions != nil {
		for _, k := range []string{"model", "modelId", "model_id", "model_name"} {
			if v, ok := p.InitializationOptions[k].(string); ok && strings.TrimSpace(v) != "" {
				model = strings.TrimSpace(v)
				break
			}
		}
		if model == "" || model == strings.TrimSpace(os.Getenv("OMNICODER_TRACE_MODEL")) {
			if provider, ok := p.InitializationOptions["provider"].(string); ok && strings.TrimSpace(provider) != "" && model != "" && !strings.Contains(model, provider) {
				model = strings.TrimSpace(provider) + "/" + model
			}
		}
	}
	name := strings.TrimSpace(p.ClientInfo.Name)
	if ver := strings.TrimSpace(p.ClientInfo.Version); name != "" && ver != "" {
		name = name + "/" + ver
	}
	s.initMu.Lock()
	s.clientName = name
	s.clientModel = model
	s.initMu.Unlock()
}

// SetClientInfo records the calling agent and model for traces and for the
// interactive doctor profile. Tests and embeddings use this directly.
func (s *Server) SetClientInfo(name, model string) {
	s.setClient(InitializeParams{
		ClientInfo:            ClientInfo{Name: name},
		InitializationOptions: map[string]any{"model": model},
	})
}

func (s *Server) clientIdentity() (name, model string) {
	s.initMu.RLock()
	defer s.initMu.RUnlock()
	return s.clientName, s.clientModel
}

func (s *Server) localInteractiveClient() bool {
	name, _ := s.clientIdentity()
	n := strings.ToLower(name)
	if n == "" {
		return false
	}
	for _, p := range []string{"cline", "cursor", "antigravity", "vscode", "roo", "continue", "claude-dev"} {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

func (s *Server) logTrace(ev TraceEvent) {
	if s.tracer == nil {
		return
	}
	name, model := s.clientIdentity()
	if ev.Client == "" {
		ev.Client = name
	}
	if ev.Model == "" {
		ev.Model = model
	}
	s.tracer.LogEvent(ev)
}

func (s *Server) doctorRepeated(key, fp string) bool {
	s.doctorMu.Lock()
	defer s.doctorMu.Unlock()
	return key != "" && s.lastDoctorKey == key && s.lastDoctorFP == fp
}

func (s *Server) noteDoctor(key, fp string) {
	s.doctorMu.Lock()
	s.lastDoctorKey = key
	s.lastDoctorFP = fp
	s.doctorMu.Unlock()
}

func doctorCallKey(ws string, gates, lint, git, ver bool, arts, bundles []string) string {
	payload := struct {
		WS      string   `json:"ws"`
		Gates   bool     `json:"gates"`
		Lint    bool     `json:"lint"`
		Git     bool     `json:"git"`
		Ver     bool     `json:"ver"`
		Arts    []string `json:"arts"`
		Bundles []string `json:"bundles"`
	}{ws, gates, lint, git, ver, arts, bundles}
	b, err := json.Marshal(payload)
	if err != nil {
		return ws
	}
	return string(b)
}

func treeFingerprint(ctx context.Context, dir string) string {
	head, _, herr := runGit(ctx, dir, "rev-parse", "HEAD")
	por, _, perr := runGit(ctx, dir, "status", "--porcelain=v1")
	if herr != nil && perr != nil {
		if info, err := os.Stat(dir); err == nil {
			return "nogit:" + info.ModTime().UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
		}
		return "nogit"
	}
	return strings.TrimSpace(head) + "\n" + por
}

func doctorNextLine(res *devops.DoctorResult) string {
	if res != nil && !res.Gates.Ran {
		return "doctor done. Do not call omnicoder_doctor again. Next step is omnicoder_heal."
	}
	if res != nil && res.Git.IsRepo && !res.Git.IsClean {
		return "doctor done. Do not call omnicoder_doctor again. Next step is git_commit."
	}
	return "doctor done. Do not call omnicoder_doctor again."
}

// repeatableInteractiveTools are the calls a local model retries after an OK
// result. A second call with the same arguments, while the tree is unchanged,
// is a loop, not progress.
func repeatableInteractiveTool(name string) bool {
	switch name {
	case "list_directory", "read_file", "omnicoder_map", "omnicoder_status":
		return true
	default:
		return false
	}
}

func interactiveMutatingTool(name string) bool {
	switch name {
	case "write_file", "omnicoder_patch":
		return true
	default:
		return false
	}
}

func argString(args map[string]any, key, fallback string) string {
	if args == nil {
		return fallback
	}
	v, ok := args[key].(string)
	if !ok {
		return fallback
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func argBool(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	v, ok := args[key].(bool)
	return ok && v
}

func argNumber(args map[string]any, key string) int {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func interactiveArgKey(tool string, args map[string]any) string {
	pick := map[string]any{"tool": tool}
	switch tool {
	case "list_directory":
		pick["path"] = argString(args, "path", ".")
		pick["recursive"] = argBool(args, "recursive")
		pick["max_depth"] = argNumber(args, "max_depth")
	case "read_file":
		pick["path"] = argString(args, "path", "")
		pick["offset"] = argNumber(args, "offset")
		pick["limit"] = argNumber(args, "limit")
		pick["start_line"] = argNumber(args, "start_line")
		pick["end_line"] = argNumber(args, "end_line")
	default:
		pick["path"] = argString(args, "path", "")
	}
	b, err := json.Marshal(pick)
	if err != nil {
		return tool
	}
	return string(b)
}

func (s *Server) clearInteractiveSeen() {
	s.repeatMu.Lock()
	s.repeatFP = ""
	s.repeatKeys = nil
	s.repeatMu.Unlock()
}

// refuseRepeatedInteractive stops a local model from listing, mapping, or
// reading the same target again when the tree has not changed.
func (s *Server) refuseRepeatedInteractive(ctx context.Context, tool string, args map[string]any) (any, bool) {
	if !s.localInteractiveClient() || !repeatableInteractiveTool(tool) {
		return nil, false
	}
	ws := s.ResolveEffectiveWorkspace(args, "")
	fp := treeFingerprint(ctx, ws)
	key := interactiveArgKey(tool, args)
	s.repeatMu.Lock()
	defer s.repeatMu.Unlock()
	if s.repeatFP != fp || s.repeatKeys == nil {
		return nil, false
	}
	if _, seen := s.repeatKeys[key]; !seen {
		return nil, false
	}
	return map[string]any{
		"status": "UNCHANGED",
		"next":   "unchanged. Do not call " + tool + " again. Next step is omnicoder_heal.",
	}, true
}

func (s *Server) noteInteractiveCall(ctx context.Context, tool string, args map[string]any) {
	if !s.localInteractiveClient() {
		return
	}
	if interactiveMutatingTool(tool) {
		s.clearInteractiveSeen()
		return
	}
	if !repeatableInteractiveTool(tool) {
		return
	}
	ws := s.ResolveEffectiveWorkspace(args, "")
	fp := treeFingerprint(ctx, ws)
	key := interactiveArgKey(tool, args)
	s.repeatMu.Lock()
	defer s.repeatMu.Unlock()
	if s.repeatFP != fp || s.repeatKeys == nil {
		s.repeatFP = fp
		s.repeatKeys = map[string]struct{}{}
	}
	s.repeatKeys[key] = struct{}{}
}

// stampInteractiveResult puts the next action on the first line a local model
// will follow. write_file and a directory listing otherwise look like success
// with no stop.
func (s *Server) stampInteractiveResult(tool string, args map[string]any, result any) any {
	if !s.localInteractiveClient() || result == nil {
		return result
	}
	switch tool {
	case "write_file":
		if r, ok := result.(*WriteFileResult); ok && r != nil && r.Next == "" {
			if r.SyntaxWarning != "" {
				r.Next = "wrote " + r.Path + " (WARNING: " + r.SyntaxWarning + "). Call omnicoder_heal once to fix."
			} else {
				r.Next = "wrote " + r.Path + ". Call omnicoder_heal once. Do not list_directory or read the file back."
			}
		}
	case "list_directory":
		if entries, ok := result.([]DirectoryEntry); ok {
			path := argString(args, "path", ".")
			return map[string]any{
				"next":    "listed " + path + ". Do not list this path again. Next step is omnicoder_heal.",
				"entries": entries,
			}
		}
	case "read_file":
		if r, ok := result.(*ReadFileResult); ok && r != nil && !r.Truncated {
			r.Next = "read " + r.Path + ". Do not read it again. Next step is omnicoder_heal or omnicoder_patch {path, old, new}."
		}
	}
	return result
}

// capOutboundResult shortens tool payloads before they are appended to an
// interactive transcript. Small results keep their original Go type.
func capOutboundResult(name string, result any) any {
	switch name {
	case "read_file":
		if r, ok := result.(*ReadFileResult); ok {
			return capReadResult(r)
		}
	case "git_diff":
		if r, ok := result.(*GitDiffResult); ok {
			return capDiffResult(r)
		}
	case "git_status":
		if r, ok := result.(*GitStatusResult); ok && r != nil && r.Next == "" {
			if r.Clean {
				r.Next = "clean. No commit needed. Do not call omnicoder_doctor."
			} else {
				r.Next = "dirty. Commit with git_commit. Do not call omnicoder_doctor first."
			}
		}
	case "git_commit":
		if r, ok := result.(*GitCommitResult); ok && r != nil && r.Next == "" {
			r.Next = "committed " + r.CommitHash + ". Do not call omnicoder_doctor again."
		}
	case "list_directory":
		if entries, ok := result.([]DirectoryEntry); ok {
			return capDirectoryEntries(entries)
		}
	case "omnicoder_search":
		if m, ok := result.(map[string]any); ok {
			return capSearchResult(m)
		}
	case "omnicoder_patch":
		if m, ok := result.(map[string]any); ok {
			delete(m, "raw_diff")
			delete(m, "backup_path")
			if _, hasNext := m["next"]; !hasNext {
				if applied, _ := m["applied"].(bool); applied {
					nextMsg := "Run your test/build command via omnicoder_run or omnicoder_heal to verify."
					if warn, _ := m["syntax_warning"].(string); warn != "" {
						nextMsg += " (WARNING: " + warn + ")"
					}
					m["next"] = nextMsg
				} else {
					m["next"] = "Read the target file lines around the edit site and retry with exact context."
				}
			}
		}
	case "omnicoder_doctor":
		if r, ok := result.(*devops.DoctorResult); ok && r != nil && r.Next == "" {
			r.Next = doctorNextLine(r)
		}
	}
	return result
}

func capReadResult(r *ReadFileResult) *ReadFileResult {
	if r == nil {
		return nil
	}
	if len(r.Content) <= localModelResultCap {
		if r.Next == "" {
			r.Next = fmt.Sprintf("read %s (%d lines). Edit with omnicoder_patch {path, old, new}.", r.Path, r.LineCount)
		}
		return r
	}
	cut := localModelResultCap
	if i := strings.LastIndex(r.Content[:cut], "\n"); i > 256 {
		cut = i
	}
	r.Content = r.Content[:cut]
	r.LineCount = 1
	if r.Content != "" {
		r.LineCount = strings.Count(r.Content, "\n") + 1
	}
	r.ByteSize = int64(len(r.Content))
	r.Truncated = true
	nextOffset := r.StartLine + r.LineCount
	if nextOffset < 1 {
		nextOffset = r.LineCount + 1
	}
	r.Next = fmt.Sprintf("truncated at %d bytes. Call read_file path=%s offset=%d. Do not read the whole file.", localModelResultCap, r.Path, nextOffset)
	return r
}

func capDiffResult(r *GitDiffResult) *GitDiffResult {
	if r == nil {
		return nil
	}
	r.TotalBytes = len(r.Diff)
	if len(r.Diff) > localModelResultCap {
		cut := localModelResultCap
		if i := strings.LastIndex(r.Diff[:cut], "\n"); i > 256 {
			cut = i
		}
		r.Diff = r.Diff[:cut]
		r.Truncated = true
		r.LineCount = strings.Count(r.Diff, "\n") + 1
		r.Next = fmt.Sprintf("diff truncated at %d of %d bytes. Call git_diff with path set to one file.", localModelResultCap, r.TotalBytes)
		return r
	}
	if r.Empty {
		r.Next = "diff empty. Nothing to commit."
	} else if r.Next == "" {
		r.Next = "diff ready. Commit with git_commit."
	}
	return r
}

func capDirectoryEntries(entries []DirectoryEntry) any {
	raw, err := json.Marshal(entries)
	if err != nil || len(raw) <= localModelResultCap {
		return entries
	}
	kept := entries
	for len(kept) > 1 {
		kept = kept[:len(kept)/2]
		raw, err = json.Marshal(kept)
		if err == nil && len(raw) <= localModelResultCap-256 {
			break
		}
	}
	return map[string]any{
		"next":      fmt.Sprintf("listing truncated to %d of %d entries. Pass a narrower path. Prefer omnicoder_map.", len(kept), len(entries)),
		"truncated": true,
		"total":     len(entries),
		"entries":   kept,
	}
}

func capSearchResult(m map[string]any) map[string]any {
	if m == nil {
		return m
	}
	query, _ := m["query"].(string)
	raw, err := json.Marshal(m)
	if err == nil && len(raw) > localModelResultCap {
		var generic map[string]any
		if json.Unmarshal(raw, &generic) == nil {
			if hits, ok := generic["results"].([]any); ok {
				for len(hits) > 1 {
					hits = hits[:len(hits)/2]
					generic["results"] = hits
					generic["count"] = len(hits)
					generic["total_matches"] = len(hits)
					raw, _ = json.Marshal(generic)
					if len(raw) <= localModelResultCap-128 {
						break
					}
				}
				q, _ := generic["query"].(string)
				generic["truncated"] = true
				generic["next"] = fmt.Sprintf("search %q truncated. Narrow the query. Open one hit with read_file around its line.", q)
				return generic
			}
		}
	}
	if _, exists := m["next"]; !exists {
		count := 0
		switch c := m["count"].(type) {
		case int:
			count = c
		case float64:
			count = int(c)
		}
		m["next"] = fmt.Sprintf("search %q returned %d hits. Open one hit with read_file. Do not read whole files.", query, count)
	}
	return m
}
