// SPDX-License-Identifier: Apache-2.0

package ui

import "strings"

// patchSectionAfter returns the raw SEARCH/REPLACE body that follows a PATCH tag (bounded by the
// closing [/ACTION:...] tag or limit), or "" if that zone holds no complete edit block.
func patchSectionAfter(replyText string, start, limit int) string {
	if start < 0 || start >= len(replyText) {
		return ""
	}
	if limit > len(replyText) || limit < start {
		limit = len(replyText)
	}
	zone := replyText[start:limit]
	if loc := actionCloseRegex.FindStringIndex(zone); loc != nil {
		zone = zone[:loc[0]]
	}
	si := strings.Index(zone, "<<<<<<< SEARCH")
	ri := strings.LastIndex(zone, ">>>>>>> REPLACE")
	if si < 0 || ri < si {
		return ""
	}
	return zone[si:ri+len(">>>>>>> REPLACE")] + "\n"
}
