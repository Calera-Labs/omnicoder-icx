// SPDX-License-Identifier: Apache-2.0
package titankv

import (
	"path/filepath"
	"strconv"
	"strings"
)

// l0Stamp parses the nanosecond stamp from "l0_<stamp>.sst" (0 if unparsable).
func l0Stamp(path string) int64 {
	b := strings.TrimSuffix(filepath.Base(path), ".sst")
	parts := strings.Split(b, "_")
	if len(parts) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(parts[1], 10, 64)
	return v
}

// l1CoveredStamp parses the covered-L0 stamp from "l1_<now>_<covered>.sst"
// (0 for the legacy "l1_<now>.sst" form).
func l1CoveredStamp(path string) int64 {
	b := strings.TrimSuffix(filepath.Base(path), ".sst")
	parts := strings.Split(b, "_")
	if len(parts) < 3 {
		return 0
	}
	v, _ := strconv.ParseInt(parts[2], 10, 64)
	return v
}
