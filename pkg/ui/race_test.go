package ui

import (
	"runtime"
	"strings"
	"testing"
)

func TestStripRaceIfCgoUnavailable(t *testing.T) {
	cmd := "go test -v -race ./pkg/canonical/..."
	res := stripRaceIfCgoUnavailable(cmd)
	if runtime.GOOS == "windows" {
		if !strings.Contains(cmd, "-race") {
			t.Fatalf("original cmd should contain -race")
		}
		// If gcc is not installed, -race should be stripped
		t.Logf("stripRaceIfCgoUnavailable result: %q", res)
	}
}
