// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

const (
	icxRecallHeader     = "=== ICX MEMORY RECALL ==="
	icxRecallMaxChars   = 1600
	icxSnippetMaxChars  = 240
	searchResultLimit   = 6
	promptRecallTopK    = 3
	defaultICXSpaceName = "default"
)

// spaceIDForProject maps a project scope (e.g. "projects/analytics-service") to an isolated
// ICX memory space, so one project's files, diagnostics and plans never bleed into another's.
// Host/root work keeps the shared "default" space.
func spaceIDForProject(project string) string {
	p := strings.Trim(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(project), "\\", "/")), "/ .")
	if p == "" {
		return defaultICXSpaceName
	}
	var sb strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return "omni_" + sb.String()
}

func snippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// stripPathPrefix removes the "path:<rel>\n" header CrystallizeFile prepends, returning (path, body).
func stripPathPrefix(text string) (string, string) {
	if strings.HasPrefix(text, "path:") {
		if i := strings.IndexByte(text, '\n'); i > 0 {
			return strings.TrimPrefix(text[:i], "path:"), text[i+1:]
		}
	}
	return "", text
}

// SearchContext runs semantic fact search (icx_search_facts / /v1/lattice/search) over this build's
// memory space and formats the hits for the architect. It never invents results: no hits says so.
func (b *BuildSession) SearchContext(ctx context.Context, query string, limit int) string {
	query = strings.TrimSpace(query)
	if b == nil || b.Client == nil {
		return fmt.Sprintf("--- SEARCH %q ---\n[unavailable] no ICX memory is attached to this session; use [ACTION:READ] or [ACTION:MAP].\n", query)
	}
	if limit <= 0 {
		limit = searchResultLimit
	}
	resp, err := b.Client.SearchFacts(ctx, icx.SearchRequest{Query: query, SpaceID: b.SpaceID, Limit: limit})
	if err != nil {
		return fmt.Sprintf("--- SEARCH %q ---\n[error] %v\n", query, err)
	}
	if resp == nil || len(resp.Facts) == 0 {
		return fmt.Sprintf("--- SEARCH %q (space %s) ---\nNo matching facts in ICX memory. The code may not be crystallized yet; use [ACTION:READ] on candidate files.\n", query, b.SpaceID)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- SEARCH %q (space %s, %d match(es)) ---\n", query, b.SpaceID, len(resp.Facts))
	for i, f := range resp.Facts {
		path, body := stripPathPrefix(f.Text)
		label := f.SourceID
		if path != "" {
			label = path
		}
		fmt.Fprintf(&sb, "%d. [%s | %s | score %.2f | sha256 %.12s]\n   %s\n", i+1, label, f.Family, f.RelevanceScore, f.SHA256, snippet(body, icxSnippetMaxChars))
	}
	sb.WriteString("Use [ACTION:READ path=\"...\"] on a hit to see exact lines before editing it.\n")
	return sb.String()
}

// PromptRecall builds a compact memory card for the user's prompt from crystallized project files, so
// the architect starts turn one already knowing which files are relevant. Returns "" when memory has
// nothing relevant for this space.
func (b *BuildSession) PromptRecall(ctx context.Context, prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if b == nil || b.Client == nil || prompt == "" {
		return ""
	}
	if len(prompt) > 600 {
		prompt = prompt[:600]
	}
	resp, err := b.Client.RecallScoped(ctx, icx.RecallRequest{
		Query: prompt, SpaceID: b.SpaceID, Family: familyFiles, TopK: promptRecallTopK,
	})
	if err != nil || resp == nil || len(resp.Matches) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(icxRecallHeader + "\n")
	sb.WriteString("Verified files already crystallized in this project's ICX memory space (" + b.SpaceID + ") that may be relevant. Treat as leads, not as current disk truth; confirm with [ACTION:READ].\n")
	seen := map[string]bool{}
	n := 0
	for _, m := range resp.Matches {
		path, body := stripPathPrefix(m.Content)
		if path == "" {
			if p, ok := m.Metadata["path"].(string); ok {
				path = p
			}
		}
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		n++
		fmt.Fprintf(&sb, "- %s: %s\n", path, snippet(body, 160))
		if sb.Len() > icxRecallMaxChars {
			break
		}
	}
	if n == 0 {
		return ""
	}
	sb.WriteString("=== END ICX MEMORY RECALL ===")
	return sb.String()
}
