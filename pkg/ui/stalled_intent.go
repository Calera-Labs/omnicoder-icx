// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"path/filepath"
	"regexp"
	"strings"
)

// stalledIntentMaxRetries caps how many times per user request the engine will recover a reply
// that announced a next step without emitting an executable action tag.
const stalledIntentMaxRetries = 4

// stalledIntentTailBytes is the size of the reply tail inspected for a closing announcement.
// Only the closing statement matters: "Let's read X." at the end means the model expects a turn.
const stalledIntentTailBytes = 900

var (
	// stalledIntentRegex matches announcements such as "Let's read", "I will now patch",
	// "Next, I'll run" and captures the verb.
	stalledIntentRegex = regexp.MustCompile(`(?i)\b(?:let'?s|let me|i'?ll|i will|i am going to|i'm going to|i need to|we need to|we should|we'll|we will|next,?\s+(?:i|we)(?:'ll| will)?|now,?\s+(?:i|we)(?:'ll| will)?)\s+(?:now\s+|first\s+|next\s+|then\s+|also\s+)?(read|inspect|open|examine|check|look at|review|view|search|query|grep|run|probe|execute|test|patch|edit|update|modify|fix|write|apply|add|change|implement|create|rewrite|replace)\b`)

	// bareActionTagRegex matches an action tag mentioned without any attributes, e.g. "using `[ACTION:READ]`".
	bareActionTagRegex = regexp.MustCompile(`\[ACTION:([A-Z_]+)\]`)

	// backtickPathRegex matches a backtick-quoted relative file path with an extension.
	backtickPathRegex = regexp.MustCompile("`([A-Za-z0-9_./\\\\-]+\\.[A-Za-z0-9]{1,8})`")
)

var stalledReadVerbs = map[string]bool{
	"read": true, "inspect": true, "open": true, "examine": true, "check": true,
	"look at": true, "review": true, "view": true,
}

var stalledBinaryExts = map[string]bool{
	".db": true, ".sqlite": true, ".sqlite3": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".ico": true, ".exe": true, ".dll": true, ".zip": true, ".pdf": true,
	".pyc": true, ".parquet": true, ".woff": true, ".woff2": true,
}

// stalledIntent describes a reply that promised a next step but emitted no executable tag.
type stalledIntent struct {
	Verb     string // normalized verb, e.g. "read", "patch"
	Sentence string // the announcing sentence (trimmed), for logging and re-prompting
	BareTag  string // action type mentioned without attributes, e.g. "READ"
}

// IsRead reports whether the stalled step was a read-only file inspection.
func (s stalledIntent) IsRead() bool {
	return stalledReadVerbs[s.Verb] || s.BareTag == "READ"
}

// detectStalledIntent inspects the closing part of a reply that produced zero parsed actions and
// reports whether it announced a next step the engine would otherwise silently drop.
func detectStalledIntent(reply string) (stalledIntent, bool) {
	tail := reply
	if len(tail) > stalledIntentTailBytes {
		tail = tail[len(tail)-stalledIntentTailBytes:]
	}

	var si stalledIntent
	if m := bareActionTagRegex.FindAllStringSubmatch(tail, -1); len(m) > 0 {
		si.BareTag = m[len(m)-1][1]
	}
	locs := stalledIntentRegex.FindAllStringSubmatchIndex(tail, -1)
	if len(locs) > 0 {
		last := locs[len(locs)-1]
		si.Verb = strings.ToLower(strings.Join(strings.Fields(tail[last[2]:last[3]]), " "))
		si.Sentence = sentenceAround(tail, last[0])
	}
	if si.Verb == "" && si.BareTag == "" {
		return si, false
	}
	if si.Verb == "" {
		si.Verb = strings.ToLower(si.BareTag)
	}
	if si.Sentence == "" {
		si.Sentence = strings.TrimSpace(lastLine(tail))
	}
	return si, true
}

// extractIntendedReadPaths returns synthetic READ intents for backtick-quoted files named in the
// announcing part of the reply that actually exist inside the workspace. Already-read paths are skipped.
func extractIntendedReadPaths(ws, activeProject, reply string, alreadyRead map[string]bool) []*ActionIntent {
	tail := reply
	if len(tail) > stalledIntentTailBytes {
		tail = tail[len(tail)-stalledIntentTailBytes:]
	}
	if locs := stalledIntentRegex.FindAllStringIndex(tail, -1); len(locs) > 0 {
		// Prefer paths named from the first announcement in the tail onward.
		tail = tail[locs[0][0]:]
	}

	var out []*ActionIntent
	seen := make(map[string]bool)
	for _, m := range backtickPathRegex.FindAllStringSubmatch(tail, -1) {
		raw := strings.TrimSpace(m[1])
		if raw == "" || stalledBinaryExts[strings.ToLower(filepath.Ext(raw))] {
			continue
		}
		full, err := resolveReadPath(ws, activeProject, raw)
		if err != nil {
			continue
		}
		key := strings.ToLower(filepath.Clean(full))
		if seen[key] || alreadyRead[key] {
			continue
		}
		seen[key] = true
		out = append(out, &ActionIntent{
			Type:       ActionRead,
			RawTag:     "[ACTION:READ path=\"" + raw + "\"] (recovered)",
			Attributes: map[string]string{"path": raw},
		})
		if len(out) >= 4 {
			break
		}
	}
	return out
}

// readKey normalizes a READ action's path for de-duplication across turns.
func readKey(ws, activeProject string, a *ActionIntent) string {
	full, err := resolveReadPath(ws, activeProject, a.Path())
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.Clean(full))
}

// stalledIntentReprompt builds the corrective prompt when the engine cannot execute the announced
// step itself (e.g. a patch, a probe, or a read of an unresolvable file).
func stalledIntentReprompt(si stalledIntent) string {
	return "ENGINE NOTICE: Your previous reply announced a next step (\"" + si.Sentence + "\") but contained NO executable action tag, so nothing ran and the session would have ended.\n" +
		"Do not describe the step. Emit the literal tag NOW, with attributes, in this exact syntax:\n" +
		"  [ACTION:READ path=\"relative/path.py\" lines=\"1-200\"]\n" +
		"  [ACTION:SEARCH query=\"symbol or phrase\"]\n" +
		"  [ACTION:BUILD task=\"python -c \\\"...read-only probe...\\\"\"]\n" +
		"  [ACTION:PATCH path=\"relative/path.py\"] followed by the SEARCH/REPLACE block\n" +
		"  [ACTION:WRITE path=\"relative/path.py\"] followed by the complete file in a fenced block\n" +
		"Continue autonomously until the user's request is fixed and verified, then reply DONE with no tags."
}

// actionTagSyntaxReminder is appended to evidence-feedback prompts so the model keeps the loop alive.
const actionTagSyntaxReminder = "\n\nTo request more evidence or make a change you MUST emit the literal tag with attributes " +
	"(e.g. [ACTION:READ path=\"utils/db.py\"], [ACTION:BUILD task=\"...\"], [ACTION:PATCH path=\"...\"]). " +
	"Merely writing \"Let's read X\" executes nothing. Keep going until the issue is fixed and verified, then reply DONE."

func sentenceAround(s string, pos int) string {
	start := strings.LastIndexAny(s[:pos], ".!?\n")
	if start < 0 {
		start = 0
	} else {
		start++
	}
	end := strings.IndexAny(s[pos:], ".!?\n")
	if end < 0 {
		end = len(s)
	} else {
		end = pos + end + 1
	}
	sentence := strings.TrimSpace(s[start:end])
	if len(sentence) > 240 {
		sentence = sentence[:240] + "..."
	}
	return sentence
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n\t ")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
