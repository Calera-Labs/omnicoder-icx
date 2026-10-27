package ui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// failingPatchReply reproduces a self-healing turn where a single [ACTION:PATCH]
// is followed by prose, an [ACTION:BUILD], and a trailing DONE.
const failingPatchReply = "Let's use `[ACTION:PATCH]` to update `schema_validator.py` so that v2/v3 formats are correctly classified as Modern Schema.\n\n" +
	"[ACTION:PATCH path=\"projects/schema-validator/schema_validator.py\"]\n" +
	"<<<<<<< SEARCH\n" +
	"        if not fmt_clean:\n" +
	"            text_lower = str(fmt_str).lower()\n" +
	"            if any(k in text_lower for k in [\"xml\", \"csv\", \"legacy\", \"flatfile\", \"v1\", \"raw\", \"txt\"]):\n" +
	"                return \"Legacy Schema\"\n" +
	"=======\n" +
	"        if not fmt_clean:\n" +
	"            text_lower = str(fmt_str).lower()\n" +
	"            if any(k in text_lower for k in [\"protobuf\", \"parquet\", \"arrow\"]):\n" +
	"                return \"Modern Schema\"\n" +
	">>>>>>> REPLACE\n" +
	"[/ACTION:PATCH]\n\n" +
	"Now let's run the test suite again via `[ACTION:BUILD]`.\n\n" +
	"[ACTION:BUILD task=\"python projects/schema-validator/test_validator.py\"]\n\n" +
	"DONE\n"

func TestPatchExtractionKeepsReplaceTerminator(t *testing.T) {
	intents := ParseAllActionIntents(failingPatchReply)
	var patch *ActionIntent
	for _, it := range intents {
		if it.IsPatch() {
			patch = it
			break
		}
	}
	if patch == nil {
		t.Fatalf("expected a PATCH intent, got %v", intents)
	}

	content := patch.ExtractContent(failingPatchReply)
	if !strings.Contains(content, ">>>>>>> REPLACE") {
		t.Fatalf("extracted patch lost its REPLACE terminator:\n%s", content)
	}

	blocks, err := devops.ParseAiderEditBlocks(content, patch.Path())
	if err != nil {
		t.Fatalf("patch failed to parse: %v\ncontent:\n%s", err, content)
	}
	if len(blocks) != 1 || !strings.Contains(blocks[0].ReplaceText, "Modern Schema") {
		t.Fatalf("unexpected blocks: %+v", blocks)
	}
}

func TestPatchAppliesEndToEnd(t *testing.T) {
	ws := t.TempDir()
	rel := "projects/schema-validator/schema_validator.py"
	full := filepath.Join(ws, filepath.FromSlash(rel))
	_ = os.MkdirAll(filepath.Dir(full), 0755)
	original := "def detect_format(fmt_str):\n" +
		"    fmt_clean = ''\n" +
		"    try:\n" +
		"        if not fmt_clean:\n" +
		"            text_lower = str(fmt_str).lower()\n" +
		"            if any(k in text_lower for k in [\"xml\", \"csv\", \"legacy\", \"flatfile\", \"v1\", \"raw\", \"txt\"]):\n" +
		"                return \"Legacy Schema\"\n" +
		"    except Exception:\n" +
		"        return None\n"
	if err := os.WriteFile(full, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	var patch *ActionIntent
	for _, it := range ParseAllActionIntents(failingPatchReply) {
		if it.IsPatch() {
			patch = it
		}
	}
	res, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:       ws,
		EditBlocksText: patch.ExtractContent(failingPatchReply),
		TargetFile:     rel,
		FuzzyMatch:     true,
	})
	if err != nil || res == nil || !res.Applied {
		t.Fatalf("patch did not apply: res=%+v err=%v", res, err)
	}
	data, _ := os.ReadFile(full)
	if !strings.Contains(string(data), "return \"Modern Schema\"") || strings.Contains(string(data), "Legacy Schema") {
		t.Fatalf("file not patched as expected:\n%s", data)
	}
}

func TestSanitizerStillStripsStrayMarkersOnPlainCode(t *testing.T) {
	got := sanitizeExtractedContent("print('hi')\n>>>>>>> REPLACE\nDONE\n")
	if strings.Contains(got, ">>>>") || strings.Contains(got, "DONE") {
		t.Fatalf("stray markers should still be stripped from non-patch payloads, got %q", got)
	}
}

func TestNormalizeBuildCommandBareScope(t *testing.T) {
	ws := filepath.Join("C:", "ws")
	buildDir := filepath.Join(ws, "projects", "schema-validator")
	cases := map[string]string{
		`python -m unittest discover -s projects/schema-validator -p "test_*.py"`: `python -m unittest discover -s . -p "test_*.py"`,
		`python -m unittest projects/schema-validator/test_validator.py`:          `python -m unittest ./test_validator.py`,
		`pytest projects\schema-validator\tests`:                                  `pytest ./tests`,
		`python -m pytest projects/schema-validator-v2/tests`:                     `python -m pytest projects/schema-validator-v2/tests`,
	}
	for in, want := range cases {
		if got := normalizeBuildCommand(in, buildDir, ws); got != want {
			t.Errorf("normalizeBuildCommand(%q)\n got: %q\nwant: %q", in, got, want)
		}
	}

	if runtime.GOOS == "windows" {
		sqIn := `python -m unittest discover -s projects/schema-validator -p 'test_*.py'`
		sqWant := `python -m unittest discover -s . -p test_*.py`
		if got := normalizeBuildCommand(sqIn, buildDir, ws); got != sqWant {
			t.Errorf("normalizeBuildCommand(%q)\n got: %q\nwant: %q", sqIn, got, sqWant)
		}
	}
}

func TestResolveVerifyDirPythonProject(t *testing.T) {
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module host\n"), 0644)
	proj := filepath.Join(ws, "projects", "schema-validator")
	_ = os.MkdirAll(proj, 0755)
	_ = os.WriteFile(filepath.Join(proj, "requirements.txt"), []byte("pandas\n"), 0644)

	// Hinted project dir with only requirements.txt must be kept.
	if got := ResolveVerifyDir(ws, proj, "python test_validator.py"); got != proj {
		t.Errorf("hinted python project dropped: got %q want %q", got, proj)
	}
	// No hint, but the command names the project path explicitly.
	if got := ResolveVerifyDir(ws, ws, "python -m unittest projects/schema-validator/test_validator.py"); got != proj {
		t.Errorf("project referenced by command not resolved: got %q want %q", got, proj)
	}
	// Go commands keep their existing resolution.
	if got := ResolveVerifyDir(ws, ws, "go test ./..."); got != ws {
		t.Errorf("go command should stay at workspace: got %q", got)
	}
}

func TestPythonInitsGuards(t *testing.T) {
	// Go host workspace: no inits anywhere.
	host := t.TempDir()
	_ = os.WriteFile(filepath.Join(host, "go.mod"), []byte("module host\n"), 0644)
	bench := filepath.Join(host, "benchmarks")
	_ = os.MkdirAll(bench, 0755)
	_ = os.WriteFile(filepath.Join(bench, "run.py"), []byte("x = 1\n"), 0644)
	if ensurePythonPackageInits(host) {
		t.Errorf("should not create inits in a Go host workspace")
	}
	if _, err := os.Stat(filepath.Join(bench, "__init__.py")); err == nil {
		t.Errorf("benchmarks/__init__.py must not be created")
	}

	// Python root: hyphenated sub-folder skipped, valid package still healed.
	root := t.TempDir()
	hyph := filepath.Join(root, "my-scripts")
	pkg := filepath.Join(root, "tests")
	_ = os.MkdirAll(hyph, 0755)
	_ = os.MkdirAll(pkg, 0755)
	_ = os.WriteFile(filepath.Join(hyph, "a.py"), []byte("x = 1\n"), 0644)
	_ = os.WriteFile(filepath.Join(pkg, "test_a.py"), []byte("x = 1\n"), 0644)
	ensurePythonPackageInits(root)
	if _, err := os.Stat(filepath.Join(hyph, "__init__.py")); err == nil {
		t.Errorf("non-identifier folder must not receive __init__.py")
	}
	if _, err := os.Stat(filepath.Join(pkg, "__init__.py")); err != nil {
		t.Errorf("tests/__init__.py should still be created")
	}
}
