package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

func TestBuildSession_CardIsRibbonAndSlice(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	b := NewBuildSession(lat, t.TempDir())
	if err := b.Open(ctx, "build a fixture", nil); err != nil {
		t.Fatal(err)
	}
	body := "package fixture\nfunc Alpha() int {\n\treturn 1\n}\nfunc Beta() int {\n\treturn 2\n}\n"
	if _, err := b.CrystallizeFile(ctx, "pkg/fixture/alpha.go", body, "project:pkg/fixture/alpha.go"); err != nil {
		t.Fatal(err)
	}
	card, refused := b.Card(ctx, "file_pkg/fixture/alpha.go", "pkg/fixture/alpha.go", 2)
	if refused {
		t.Fatalf("card refused: %s", card)
	}
	if !strings.Contains(card, "<nav:ribbon:") {
		t.Fatalf("expected ribbon, got %s", card)
	}
	if !strings.Contains(card, "slice_sha256=") {
		t.Fatalf("expected quote slice, got %s", card)
	}
	if strings.Contains(card, "PRIOR ASSISTANT TRANSCRIPT") {
		t.Fatal("card replayed a transcript")
	}
	if err := b.ResetDialogue(ctx); err != nil {
		t.Fatal(err)
	}
	again, refused := b.Card(ctx, "file_pkg/fixture/alpha.go", "pkg/fixture/alpha.go", 2)
	if refused || !strings.Contains(again, "slice_sha256=") {
		t.Fatalf("reset dropped the slot: refused=%v card=%s", refused, again)
	}
}

func TestBuildSession_RegisterMiss(t *testing.T) {
	ctx := context.Background()
	b := NewBuildSession(icx.NewLocalLattice(), t.TempDir())
	if err := b.Open(ctx, "goal", nil); err != nil {
		t.Fatal(err)
	}
	card, refused := b.Card(ctx, "func_missing_Nope", "", 0)
	if !refused || !strings.Contains(card, "404 Register Not Found") {
		t.Fatalf("expected refusal, got refused=%v card=%s", refused, card)
	}
}

func TestBuildSession_PurgeSandboxSource(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	b := NewBuildSession(lat, t.TempDir())
	if err := b.Open(ctx, "goal", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", "package fixture\nfunc A() {}\n", "sandbox:turn1"); err != nil {
		t.Fatal(err)
	}
	if err := b.PurgeSource(ctx, "sandbox:turn1"); err != nil {
		t.Fatal(err)
	}
	q, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{Family: familyFiles, Index: 1})
	if err != nil {
		t.Fatal(err)
	}
	if q.OK {
		t.Fatalf("purged source still quoted: %s", q.Text)
	}
}

func TestBuildSession_ScrubBeforeCrystallize(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	b := NewBuildSession(lat, t.TempDir())
	secret := "sk-1234567890abcdef12345"
	if _, err := b.CrystallizeFile(ctx, "pkg/fixture/a.go", "package fixture\n// "+secret+"\nfunc A() {}\n", "project:a"); err != nil {
		t.Fatal(err)
	}
	q, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{Family: familyFiles, Index: 1})
	if err != nil || !q.OK {
		t.Fatalf("quote failed: %+v %v", q, err)
	}
	if strings.Contains(q.Text, secret) {
		t.Fatalf("secret entered the lattice: %s", q.Text)
	}
}

func TestResolveVerifyDir_NestedModule(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module host\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(ws, "projects", "widget")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module widget\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got := ResolveVerifyDir(ws, ws, "go test ./...")
	if got != nested {
		t.Fatalf("host root was selected: %s", got)
	}
}

func TestResolveVerifyDir_NodeProjectAndPrefix(t *testing.T) {
	ws := t.TempDir()
	nested := filepath.Join(ws, "projects", "web-widget")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "package.json"), []byte("{\"name\": \"web-widget\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Hinted path with package.json
	got := ResolveVerifyDir(ws, nested, "npm run build")
	if got != nested {
		t.Fatalf("expected nested node project (%s), got: %s", nested, got)
	}

	// 2. Command with --prefix flag
	gotPrefix := ResolveVerifyDir(ws, ws, "npm --prefix projects/web-widget run build")
	if gotPrefix != nested {
		t.Fatalf("expected --prefix to resolve to %s, got: %s", nested, gotPrefix)
	}

	// 3. Command with --prefix=
	gotPrefixEq := ResolveVerifyDir(ws, ws, "npm --prefix=projects/web-widget install")
	if gotPrefixEq != nested {
		t.Fatalf("expected --prefix= to resolve to %s, got: %s", nested, gotPrefixEq)
	}
}

func TestSandboxVerify_RedeclarePromotesAndTidyDoesNot(t *testing.T) {
	ctx := context.Background()
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "a.go"), []byte("package fixture\n\nfunc Foo() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "b.go"), []byte("package fixture\n\nfunc Foo() int { return 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := SandboxVerify(ctx, proj, "go test ./...", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Promoted {
		t.Fatalf("expected preheal promote, diag=%s", res.Diag)
	}
	left, _ := os.ReadFile(filepath.Join(proj, "a.go"))
	if strings.Contains(string(left), "func Foo") && strings.Contains(string(mustRead(t, filepath.Join(proj, "b.go"))), "func Foo") {
		t.Fatal("duplicate Foo survived promote")
	}

	before := string(mustRead(t, filepath.Join(proj, "go.mod")))
	side, err := SandboxVerify(ctx, proj, "go mod tidy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if side.Promoted || !side.SideOnly {
		t.Fatalf("tidy promoted: %+v", side)
	}
	if string(mustRead(t, filepath.Join(proj, "go.mod"))) != before {
		t.Fatal("tidy changed the promoted tree")
	}
}

func TestSandboxVerify_DoesNotPromoteHostPass(t *testing.T) {
	ctx := context.Background()
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "bad.go"), []byte("package fixture\n\nfunc Bad() int { i := 1; return 2 }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := SandboxVerify(ctx, proj, "go test ./...", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Promoted {
		t.Fatalf("unused ident was not healed: %s", res.Diag)
	}
	body := string(mustRead(t, filepath.Join(proj, "bad.go")))
	if strings.Contains(body, "i :=") {
		t.Fatalf("unused ident remained: %s", body)
	}
}

func TestQuoteSlot_SliceAndRange(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	if _, err := lat.Remember(ctx, icx.RememberRequest{Text: "abcdef", Family: "file_slots", SourceID: "s"}); err != nil {
		t.Fatal(err)
	}
	start, length := 1, 3
	q, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{Family: "file_slots", Index: 1, Start: &start, Length: &length})
	if err != nil || !q.OK || !q.IsSlice || q.Text != "bcd" || q.SliceSHA256 == "" || q.SHA256 == "" {
		t.Fatalf("slice quote: %+v %v", q, err)
	}
	badStart := 10
	miss, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{Family: "file_slots", Index: 1, Start: &badStart, Length: &length})
	if err != nil || miss.OK || miss.Error != "OUT_OF_RANGE" {
		t.Fatalf("expected OUT_OF_RANGE: %+v %v", miss, err)
	}
}

func TestExtract_UnfencedGoWrite(t *testing.T) {
	reply := "[ACTION:WRITE path=\"projects/my-service/pkg/icx/ribbon.go\"]\npackage icx\n\nfunc FormatRibbon() string {\n\treturn \"ok\"\n}\n```\nNow let's run tests.\n"
	intents := ParseAllActionIntents(reply)
	if len(intents) != 1 {
		t.Fatalf("intents: %d", len(intents))
	}
	got := intents[0].ExtractContent(reply)
	if !strings.Contains(got, "package icx") || !strings.Contains(got, "func FormatRibbon") {
		t.Fatalf("extract dropped the Go body: %q", got)
	}
	if strings.Contains(got, "Now let's") {
		t.Fatalf("extract kept trailing prose: %q", got)
	}
}

func TestPreheal_UndefinedUsesUnusedDecl(t *testing.T) {
	dir := t.TempDir()
	src := "package compute\nfunc Transform() {\n\tvalRaw := 1.0\n\t_ = valOut\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "transform.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	diag := "transform.go:3:2: declared and not used: valRaw\ntransform.go:4:6: undefined: valOut\n"
	why, ok := prehealSandbox(dir, diag)
	if !ok {
		t.Fatal("expected preheal")
	}
	if !strings.Contains(why, "undefined") {
		t.Fatalf("why: %s", why)
	}
	out := string(mustRead(t, filepath.Join(dir, "transform.go")))
	if strings.Contains(out, "valOut") || !strings.Contains(out, "valRaw") {
		t.Fatalf("file: %s", out)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPreheal_ModuleDependency(t *testing.T) {
	diag := "pkg\\server\\stream.go:10:2: missing go.sum entry for module providing package github.com/gorilla/websocket (imported by myservice/pkg/server); to add:\n\tgo get myservice/pkg/server\n"
	if !isModuleDependencyDiag(diag) {
		t.Fatalf("expected isModuleDependencyDiag to be true for missing go.sum diagnostic")
	}

	dir := t.TempDir()
	goMod := "module testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	why, ok := prehealSandbox(dir, diag)
	if !ok {
		t.Fatalf("expected prehealSandbox to succeed on module diagnostic")
	}
	if !strings.Contains(why, "go mod tidy") {
		t.Fatalf("expected 'go mod tidy' in preheal summary, got %q", why)
	}
}

func TestNormalizeBuildCommand(t *testing.T) {
	ws := `C:\workspace`
	buildDir := `C:\workspace\projects\my-service`

	cases := []struct {
		input    string
		expected string
	}{
		{
			input:    "go mod tidy -C projects/my-service",
			expected: "go mod tidy",
		},
		{
			input:    "go test -v -race ./projects/my-service/tests/...",
			expected: "go test -v -race ./tests/...",
		},
		{
			input:    "go test -v -race -C projects/my-service ./tests/...",
			expected: "go test -v -race ./tests/...",
		},
		{
			input:    "go test ./projects/my-service/...",
			expected: "go test ./...",
		},
		{
			input:    "go test -C=projects/my-service ./...",
			expected: "go test ./...",
		},
	}

	for _, tc := range cases {
		got := normalizeBuildCommand(tc.input, buildDir, ws)
		if got != tc.expected {
			t.Errorf("normalizeBuildCommand(%q) = %q; expected %q", tc.input, got, tc.expected)
		}
	}
}

func TestPreheal_InvalidEmbedDirective(t *testing.T) {
	dir := t.TempDir()
	serverDir := filepath.Join(dir, "pkg", "server")
	if err := os.MkdirAll(serverDir, 0755); err != nil {
		t.Fatal(err)
	}

	serverGo := `package server

import (
	"embed"
	"net/http"
)

//go:embed ../../web/static
var staticFiles embed.FS
`
	filePath := filepath.Join(serverDir, "server.go")
	if err := os.WriteFile(filePath, []byte(serverGo), 0644); err != nil {
		t.Fatal(err)
	}

	diag := "pkg\\server\\server.go:10:12: pattern ../../web/static: invalid pattern syntax\n"
	why, ok := prehealSandbox(dir, diag)
	if !ok {
		t.Fatalf("expected prehealSandbox to succeed on invalid embed directive")
	}
	if !strings.Contains(why, "fixed invalid go:embed") {
		t.Errorf("unexpected preheal summary: %s", why)
	}

	fixed, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fixed), "//go:embed ../../web/static") {
		t.Errorf("expected illegal //go:embed directive to be removed/commented out, got:\n%s", string(fixed))
	}
}

