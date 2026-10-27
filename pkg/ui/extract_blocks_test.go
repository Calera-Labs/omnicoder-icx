package ui

import (
	"strings"
	"testing"
)

func TestExtractionCollision_LeadingBlock(t *testing.T) {
	snippet := `
Here is the environment check:
` + "```bash" + `
go version
` + "```" + `

Now we initiate Phase 1:

[ACTION:WRITE path="projects/my-service/go.mod"]
` + "```go" + `
module myservice

go 1.22

require github.com/gorilla/websocket v1.5.3
` + "```" + `

[ACTION:WRITE path="projects/my-service/main.go"]
` + "```go" + `
package main

import (
	"flag"
	"log"
)

func main() {
	flag.Parse()
	log.Println("Starting...")
}
` + "```" + `

---

### 2. Package ` + "`pkg/canonical`" + `: RFC 8785 Canonical Hashing & Merkle Engine

[ACTION:WRITE path="projects/my-service/pkg/canonical/canonical.go"]
package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// MarshalCanonical serializes a struct to byte-exact RFC 8785 canonical JSON.
func MarshalCanonical(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal error: %w", err)
	}
	return data, nil
}

[ACTION:WRITE path="projects/my-service/pkg/canonical/merkle.go"]
package canonical

import (
	"crypto/sha256"
	"encoding/hex"
)

// ComputeMerkleRoot computes the balanced SHA-256 Merkle root from leaf hashes.
func ComputeMerkleRoot(leaves []string) string {
	return "root"
}

[ACTION:BUILD task="go test -v ./pkg/canonical/..."]
`
	intents := ParseAllActionIntents(snippet)
	if len(intents) != 5 {
		t.Fatalf("expected 5 intents, got %d", len(intents))
	}

	contentMod := intents[0].ExtractContent(snippet)
	contentMain := intents[1].ExtractContent(snippet)
	contentCanon := intents[2].ExtractContent(snippet)
	contentMerkle := intents[3].ExtractContent(snippet)

	t.Logf("Go.mod extracted content:\n%s\n---", contentMod)
	t.Logf("Main.go extracted content:\n%s\n---", contentMain)
	t.Logf("Canonical.go extracted content:\n%s\n---", contentCanon)
	t.Logf("Merkle.go extracted content:\n%s\n---", contentMerkle)

	if !strings.Contains(contentMod, "module myservice") {
		t.Fatalf("go.mod content incorrect: %q", contentMod)
	}
	if !strings.Contains(contentMain, "package main") || !strings.Contains(contentMain, "Starting...") {
		t.Fatalf("main.go content incorrect: %q", contentMain)
	}
	if strings.Contains(contentCanon, "package main") {
		t.Fatalf("BUG: canonical.go contains 'package main' from main.go!")
	}
	if !strings.Contains(contentCanon, "package canonical") || !strings.Contains(contentCanon, "MarshalCanonical") {
		t.Fatalf("canonical.go does not contain MarshalCanonical, got: %q", contentCanon)
	}
	if !strings.Contains(contentMerkle, "ComputeMerkleRoot") {
		t.Fatalf("merkle.go does not contain ComputeMerkleRoot, got: %q", contentMerkle)
	}
}
