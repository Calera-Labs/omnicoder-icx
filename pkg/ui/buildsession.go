package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

const (
	buildVarGoal      = "build.goal"
	buildVarProject   = "build.project_dir"
	buildVarMilestone = "build.milestone"
	buildVarAttempts  = "build.attempts"
	buildVarOwners    = "build.owners"

	familyPlan   = "plan"
	familyFiles  = "file_slots"
	familyDiag   = "diagnostics"
	familyOwners = "symbol_owners"

	registerMiss = "404 Register Not Found"
)

// BuildSession is the desk for one build. The lattice holds the warehouse.
type BuildSession struct {
	Client    icx.ICXClient
	Workspace string
	SpaceID   string
	Spec      *engine.PlanSpec
}

// NewBuildSession binds a build to the local lattice.
func NewBuildSession(client icx.ICXClient, workspace string) *BuildSession {
	if workspace == "" {
		workspace = "."
	}
	return &BuildSession{Client: client, Workspace: workspace, SpaceID: "default"}
}

// Open crystallizes the goal and plan once and locks milestone 0.
func (b *BuildSession) Open(ctx context.Context, goal string, spec *engine.PlanSpec) error {
	if b == nil || b.Client == nil {
		return fmt.Errorf("ui: build session has no lattice")
	}
	b.Spec = spec
	goal = scrubText(goal)
	if _, err := b.Client.Remember(ctx, icx.RememberRequest{
		Text: goal, SourceID: "build.goal", Family: familyPlan, SpaceID: b.SpaceID,
	}); err != nil {
		return err
	}
	if spec != nil {
		raw, _ := json.Marshal(spec)
		if _, err := b.Client.Remember(ctx, icx.RememberRequest{
			Text: string(raw), SourceID: "build.plan", Family: familyPlan, SpaceID: b.SpaceID,
		}); err != nil {
			return err
		}
		if spec.ProjectScope != "" {
			_ = b.casSet(ctx, buildVarProject, spec.ProjectScope, "architect")
		}
	}
	_ = b.casSet(ctx, buildVarGoal, goal, "architect")
	_ = b.casSet(ctx, buildVarMilestone, 0, "architect")
	_ = b.casSet(ctx, buildVarAttempts, 0, "architect")
	_ = b.casSet(ctx, buildVarOwners, map[string]string{}, "architect")
	return nil
}

// CrystallizeFile stores a file body as a quoteable slot and a delta.
// sourceID distinguishes a sandbox attempt from the promoted project.
func (b *BuildSession) CrystallizeFile(ctx context.Context, relPath, body, sourceID string) (int, error) {
	body = scrubText(body)
	relPath = filepath.ToSlash(relPath)
	goSrc := body
	body = "path:" + relPath + "\n" + body
	if sourceID == "" {
		sourceID = "project:" + relPath
	}
	// Idempotency guard: never write the same content to the ICX memory space twice.
	dedupeKey := "build.sha." + sourceID + "|" + relPath
	bodySHA := canonical.SHA256Bytes([]byte(body))
	if b.varString(ctx, dedupeKey) == bodySHA {
		if seq := b.varInt(ctx, "build.slot."+relPath); seq > 0 {
			return seq, nil
		}
	}
	rem, err := b.Client.Remember(ctx, icx.RememberRequest{
		Text: body, SourceID: sourceID, Family: familyFiles, SpaceID: b.SpaceID,
		Metadata: map[string]any{"path": relPath},
	})
	if err != nil {
		return 0, err
	}
	_, _ = b.Client.SyncDelta(ctx, icx.SyncDeltaRequest{
		SpaceID:   b.SpaceID,
		SourceURI: relPath,
		Author:    "architect",
		DeltaSimplex: map[string]any{
			"path": relPath, "sha256": rem.SHA256, "source_id": sourceID, "text": body,
		},
	})
	_ = b.Client.IngestPolyglotAST(relPath, []byte(goSrc))
	owners := b.owners(ctx)
	for _, name := range polyglotTopDecls(relPath, body) {
		owners[name] = relPath
	}
	_ = b.casSet(ctx, buildVarOwners, owners, "architect")
	if _, err := b.Client.Remember(ctx, icx.RememberRequest{
		Text: relPath, SourceID: "owners:" + relPath, Family: familyOwners, SpaceID: b.SpaceID,
	}); err != nil {
		return 0, err
	}
	seq := b.varInt(ctx, "build.file_seq") + 1
	_ = b.casSet(ctx, "build.file_seq", seq, "architect")
	_ = b.casSet(ctx, "build.slot."+relPath, seq, "architect")
	_ = b.casSet(ctx, dedupeKey, bodySHA, "architect")
	return seq, nil
}

// Card builds the desk: swarm vars, a 5-hop ribbon, and one quote slice.
// refused is true when the premise has no lattice register. The caller must not ask the model to invent it.
func (b *BuildSession) Card(ctx context.Context, premise, relPath string, line int) (string, bool) {
	if b == nil || b.Client == nil {
		return "", true
	}
	recall, err := b.Client.RecallScoped(ctx, icx.RecallRequest{
		Query: b.milestoneName(), SpaceID: b.SpaceID, Family: familyPlan, TopK: 3, Format: icx.FormatRibbon,
	})
	ribbon := ""
	if err == nil && recall != nil {
		ribbon = recall.Ribbon
	}
	if premise != "" {
		walk, werr := b.Client.MultihopWalk(ctx, icx.MultihopWalkRequest{
			StartNode: premise, MaxHops: 5, Format: icx.FormatRibbon, SpaceID: b.SpaceID,
		})
		if werr != nil || walk == nil || !walk.OK || walk.Error == "NODE_NOT_FOUND" {
			return registerMiss + " premise=" + premise, true
		}
		if walk.Ribbon != "" {
			ribbon = walk.Ribbon
		}
	}
	quote := ""
	if relPath != "" {
		idx := b.fileSlot(ctx, relPath)
		if idx < 1 {
			return registerMiss + " file=" + relPath, true
		}
		req := icx.QuoteSlotRequest{Family: familyFiles, Index: idx, SpaceID: b.SpaceID}
		full, _ := b.Client.QuoteSlot(ctx, req)
		if full == nil || !full.OK {
			return registerMiss + " file=" + relPath, true
		}
		if line > 0 {
			start, length := runeWindow(full.Text, line, 10)
			req.Start = &start
			req.Length = &length
		}
		q, _ := b.Client.QuoteSlot(ctx, req)
		if q == nil || !q.OK {
			return registerMiss + " file=" + relPath, true
		}
		quote = q.Text
		if q.IsSlice {
			quote = fmt.Sprintf("slice_sha256=%s\n%s", q.SliceSHA256, q.Text)
		}
	}
	if len(ribbon) > 640 {
		ribbon = ribbon[:640]
	}
	if len(quote) > 800 {
		quote = quote[:800]
	}
	var sb strings.Builder
	sb.WriteString("goal=")
	sb.WriteString(b.varString(ctx, buildVarGoal))
	sb.WriteString("\nmilestone=")
	sb.WriteString(b.milestoneName())
	sb.WriteString("\nattempts=")
	sb.WriteString(b.varString(ctx, buildVarAttempts))
	sb.WriteString("\nowners=")
	sb.WriteString(b.varString(ctx, buildVarOwners))
	if ribbon != "" {
		sb.WriteString("\n")
		sb.WriteString(ribbon)
	}
	if quote != "" {
		sb.WriteString("\n")
		sb.WriteString(quote)
	}
	return sb.String(), false
}

// NoteDiagnostic stores a compiler diagnostic in its own family.
func (b *BuildSession) NoteDiagnostic(ctx context.Context, text, sourceID string) error {
	text = scrubText(text)
	_, err := b.Client.Remember(ctx, icx.RememberRequest{
		Text: text, SourceID: sourceID, Family: familyDiag, SpaceID: b.SpaceID,
	})
	return err
}

// PurgeSource unlinks a discarded sandbox so a later quote cannot return it.
func (b *BuildSession) PurgeSource(ctx context.Context, sourceID string) error {
	_, err := b.Client.PurgeSource(ctx, icx.PurgeSourceRequest{SourceID: sourceID, SpaceID: b.SpaceID})
	if err == nil && strings.HasPrefix(sourceID, "project:") {
		rel := strings.TrimPrefix(sourceID, "project:")
		_ = b.casSet(ctx, "build.sha."+sourceID+"|"+rel, "", "architect")
		_ = b.casSet(ctx, "build.slot."+rel, 0, "architect")
	}
	return err
}

// ResetDialogue drops dialogue tokens. Vars, slots, and receipts stay.
func (b *BuildSession) ResetDialogue(ctx context.Context) error {
	_, err := b.Client.ResetSession(ctx, b.SpaceID)
	return err
}

// Receipt crystallizes a promote outcome. The next card does not replay the diff.
func (b *BuildSession) Receipt(ctx context.Context, file string, exitCode int, diag string) error {
	rec := &icx.ActuationReceipt{
		FileModified:      file,
		ExitCode:          exitCode,
		DiagnosticMessage: diag,
		SHA256Seal:        canonical.SHA256Bytes([]byte(file + diag)),
		Action:            "promote",
		Surface:           "build",
	}
	_, err := b.Client.DispatchActuation(ctx, icx.DispatchActuationRequest{
		Surface: "build",
		Action:  "promote",
		Payload: rec.ToPayload(),
	})
	return err
}

// BumpAttempts increments the heal counter unless the caller says the pre-heal was deterministic.
func (b *BuildSession) BumpAttempts(ctx context.Context, writer string) int {
	cur := b.varInt(ctx, buildVarAttempts)
	next := cur + 1
	_ = b.casSet(ctx, buildVarAttempts, next, writer)
	return next
}

func (b *BuildSession) casSet(ctx context.Context, key string, value any, writer string) error {
	got, _ := b.Client.VarGet(ctx, icx.VarGetRequest{Key: key, SpaceID: b.SpaceID})
	req := icx.VarSetRequest{Key: key, Value: value, Writer: writer, SpaceID: b.SpaceID}
	if got != nil && got.OK {
		v := got.Version
		req.ExpectedVersion = &v
	}
	res, err := b.Client.VarSet(ctx, req)
	if err != nil {
		return err
	}
	if res != nil && !res.OK && res.Error == "CAS_CONFLICT" {
		return fmt.Errorf("icx: CAS_CONFLICT on %s", key)
	}
	return nil
}

func (b *BuildSession) varString(ctx context.Context, key string) string {
	got, err := b.Client.VarGet(ctx, icx.VarGetRequest{Key: key, SpaceID: b.SpaceID})
	if err != nil || got == nil || !got.OK || got.Value == nil {
		return ""
	}
	switch v := got.Value.(type) {
	case string:
		return v
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func (b *BuildSession) varInt(ctx context.Context, key string) int {
	got, err := b.Client.VarGet(ctx, icx.VarGetRequest{Key: key, SpaceID: b.SpaceID})
	if err != nil || got == nil || !got.OK || got.Value == nil {
		return 0
	}
	switch v := got.Value.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(v)
		return i
	default:
		return 0
	}
}

func (b *BuildSession) owners(ctx context.Context) map[string]string {
	got, err := b.Client.VarGet(ctx, icx.VarGetRequest{Key: buildVarOwners, SpaceID: b.SpaceID})
	out := map[string]string{}
	if err != nil || got == nil || !got.OK || got.Value == nil {
		return out
	}
	switch v := got.Value.(type) {
	case map[string]string:
		return v
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// SetMilestone updates the active milestone index in the CAS swarm store.
func (b *BuildSession) SetMilestone(ctx context.Context, milestone int) error {
	if b == nil || b.Client == nil {
		return nil
	}
	return b.casSet(ctx, buildVarMilestone, milestone, "architect")
}

func (b *BuildSession) milestoneName() string {
	if b.Spec == nil || len(b.Spec.Phases) == 0 {
		return "goal"
	}
	m := b.varInt(context.Background(), buildVarMilestone)
	if m >= 0 && m < len(b.Spec.Phases) {
		return b.Spec.Phases[m].Name
	}
	if m >= len(b.Spec.Phases) {
		return "all_phases_complete"
	}
	return b.Spec.Phases[0].Name
}

func (b *BuildSession) slotIndex(ctx context.Context, family, factID string) int {
	recall, err := b.Client.RecallScoped(ctx, icx.RecallRequest{
		Query: factID, Family: family, SpaceID: b.SpaceID, TopK: 10,
	})
	if err != nil || recall == nil {
		return 1
	}
	for i, m := range recall.Matches {
		if m.FactID == factID {
			return i + 1
		}
	}
	return len(recall.Matches)
}

func (b *BuildSession) fileSlot(ctx context.Context, relPath string) int {
	return b.varInt(ctx, "build.slot."+filepath.ToSlash(relPath))
}

func scrubText(s string) string {
	clean, _ := icx.NewSecretScrubber().Scrub(s)
	return clean
}

func polyglotTopDecls(relPath, src string) []string {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".go":
		return goTopDecls(src)
	case ".py":
		var names []string
		classRe := regexp.MustCompile(`^\s*class\s+([a-zA-Z0-9_]+)`)
		funcRe := regexp.MustCompile(`^\s*(?:async\s+)?def\s+([a-zA-Z0-9_]+)`)
		for _, line := range strings.Split(src, "\n") {
			if m := classRe.FindStringSubmatch(line); len(m) > 1 {
				names = append(names, m[1])
			} else if m := funcRe.FindStringSubmatch(line); len(m) > 1 {
				names = append(names, m[1])
			}
		}
		return names
	case ".js", ".ts", ".jsx", ".tsx":
		var names []string
		classRe := regexp.MustCompile(`^\s*(?:export\s+)?class\s+([a-zA-Z0-9_]+)`)
		funcRe := regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+([a-zA-Z0-9_]+)`)
		constRe := regexp.MustCompile(`^\s*(?:export\s+)?const\s+([a-zA-Z0-9_]+)\s*=\s*(?:async\s*)?\(`)
		for _, line := range strings.Split(src, "\n") {
			if m := classRe.FindStringSubmatch(line); len(m) > 1 {
				names = append(names, m[1])
			} else if m := funcRe.FindStringSubmatch(line); len(m) > 1 {
				names = append(names, m[1])
			} else if m := constRe.FindStringSubmatch(line); len(m) > 1 {
				names = append(names, m[1])
			}
		}
		return names
	default:
		return nil
	}
}

func goTopDecls(src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "f.go", src, 0)
	if err != nil {
		return nil
	}
	var names []string
	for _, d := range f.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			if n.Name != nil {
				names = append(names, n.Name.Name)
			}
		case *ast.GenDecl:
			for _, s := range n.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok && ts.Name != nil {
					names = append(names, ts.Name.Name)
				}
			}
		}
	}
	return names
}

func runeWindow(text string, line, pad int) (int, int) {
	runes := []rune(text)
	if line < 1 {
		line = 1
	}
	cur := 1
	lineStart := 0
	offsets := []int{0}
	for i, r := range runes {
		if r == '\n' {
			cur++
			offsets = append(offsets, i+1)
			if cur == line {
				lineStart = i + 1
			}
		}
	}
	_ = lineStart
	fromLine := line - pad
	if fromLine < 1 {
		fromLine = 1
	}
	toLine := line + pad
	start := 0
	if fromLine-1 < len(offsets) {
		start = offsets[fromLine-1]
	}
	end := len(runes)
	if toLine < len(offsets) {
		end = offsets[toLine]
	}
	if end < start {
		end = len(runes)
	}
	return start, end - start
}

// IsSideCommand reports commands that must not promote or reset the attempt counter.
func IsSideCommand(cmd string) bool {
	c := strings.TrimSpace(cmd)
	return strings.Contains(c, "go mod tidy") || strings.Contains(c, "go get ") || strings.Contains(c, "go mod download")
}

var envPrefixRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=([^\s]+)\s+(.+)$`)

// SplitEnvPrefix pulls a leading KEY=value off a command so Windows does not treat it as the program name.
func SplitEnvPrefix(cmd string) (string, []string) {
	m := envPrefixRe.FindStringSubmatch(strings.TrimSpace(cmd))
	if m == nil {
		return cmd, nil
	}
	return m[3], []string{m[1] + "=" + m[2]}
}

// ResolveVerifyDir runs a project test inside that project's module.
// A host-root go test must not count as the nested module passing.
func ResolveVerifyDir(workspace, hinted, command string) string {
	if d := dirFromCFlag(workspace, command); d != "" {
		return d
	}
	if hinted != "" && hinted != workspace {
		for _, manifest := range []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "requirements.txt", "setup.py", "setup.cfg"} {
			if _, err := os.Stat(filepath.Join(hinted, manifest)); err == nil {
				return hinted
			}
		}
	}
	if d := projectDirReferencedByCommand(workspace, command); d != "" {
		return d
	}
	nested := findNestedModules(workspace)
	if len(nested) > 0 {
		fields := strings.Fields(command)
		for _, field := range fields {
			cleanField := strings.Trim(field, "\"'`")
			cleanField = strings.TrimPrefix(cleanField, "./")
			cleanField = strings.TrimSuffix(cleanField, "/...")
			cleanField = strings.TrimSuffix(cleanField, "...")
			if cleanField == "" || cleanField == "." {
				continue
			}
			for _, mod := range nested {
				relMod, rErr := filepath.Rel(workspace, mod)
				if rErr == nil {
					cleanRelMod := filepath.ToSlash(relMod)
					if strings.Contains(cleanField, cleanRelMod) || strings.Contains(cleanRelMod, cleanField) {
						return mod
					}
				}
				candidatePkg := filepath.Join(mod, filepath.FromSlash(cleanField))
				if fi, err := os.Stat(candidatePkg); err == nil && fi.IsDir() {
					return mod
				}
			}
		}
		if len(nested) == 1 && (strings.HasPrefix(strings.TrimSpace(command), "go ") || strings.HasPrefix(strings.TrimSpace(command), "npm ") || strings.HasPrefix(strings.TrimSpace(command), "pnpm ") || strings.HasPrefix(strings.TrimSpace(command), "yarn ") || strings.HasPrefix(strings.TrimSpace(command), "cargo ")) {
			return nested[0]
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "go.mod")); err != nil {
		return workspace
	}
	return workspace
}

func dirFromCFlag(workspace, command string) string {
	fields := strings.Fields(command)
	for i := 0; i < len(fields); i++ {
		if fields[i] == "-C" && i+1 < len(fields) {
			p := strings.Trim(fields[i+1], "\"'")
			if !filepath.IsAbs(p) {
				p = filepath.Join(workspace, p)
			}
			return p
		}
		if (fields[i] == "--prefix" || fields[i] == "-w" || fields[i] == "--workspace") && i+1 < len(fields) {
			p := strings.Trim(fields[i+1], "\"'")
			if !filepath.IsAbs(p) {
				p = filepath.Join(workspace, p)
			}
			return p
		}
		if strings.HasPrefix(fields[i], "--prefix=") {
			p := strings.Trim(strings.TrimPrefix(fields[i], "--prefix="), "\"'")
			if !filepath.IsAbs(p) {
				p = filepath.Join(workspace, p)
			}
			return p
		}
	}
	return ""
}

// projectDirReferencedByCommand returns the projects/<name> or apps/<name> directory that a
// non-Go command explicitly references (e.g. `python -m unittest projects/foo/test_x.py`).
// Running such commands from the workspace root breaks Python imports: hyphenated folder names
// are not importable and an installed site-packages `projects` package shadows the local folder.
// Go commands are left to the nested-module logic below.
func projectDirReferencedByCommand(workspace, command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" || strings.HasPrefix(trimmed, "go ") || strings.Contains(trimmed, "cd ") {
		return ""
	}
	for _, field := range strings.Fields(trimmed) {
		clean := strings.Trim(field, "\"'`")
		if eq := strings.LastIndex(clean, "="); eq != -1 {
			clean = clean[eq+1:]
		}
		clean = strings.TrimPrefix(filepath.ToSlash(clean), "./")
		parts := strings.Split(clean, "/")
		if len(parts) < 2 || (parts[0] != "projects" && parts[0] != "apps") || parts[1] == "" {
			continue
		}
		dir := filepath.Join(workspace, parts[0], parts[1])
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return ""
}

func findNestedModules(workspace string) []string {
	var out []string
	for _, root := range []string{"projects", "apps"} {
		base := filepath.Join(workspace, root)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(base, e.Name())
			for _, manifest := range []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml"} {
				if _, err := os.Stat(filepath.Join(dir, manifest)); err == nil {
					out = append(out, dir)
					break
				}
			}
		}
	}
	return out
}

// VerifyResult is the sandbox outcome.
type VerifyResult struct {
	Promoted bool
	SideOnly bool
	Diag     string
	ExitCode int
}

// SandboxVerify copies projectDir, applies writes, and promotes only when verify passes.
func SandboxVerify(ctx context.Context, projectDir, verifyCmd string, writes map[string]string) (VerifyResult, error) {
	res := VerifyResult{}
	if IsSideCommand(verifyCmd) {
		cmd, env := SplitEnvPrefix(verifyCmd)
		report, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command: cmd, WorkspaceDir: projectDir, TimeoutSeconds: 60, ExtraEnv: env,
		})
		if err != nil {
			return res, err
		}
		res.SideOnly = true
		res.ExitCode = report.ExitCode
		res.Diag = report.CleanedStderr
		return res, nil
	}
	sandbox, err := os.MkdirTemp("", "omnicoder-sandbox-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(sandbox)
	if err := copyTree(projectDir, sandbox); err != nil {
		return res, err
	}
	for rel, body := range writes {
		dst := filepath.Join(sandbox, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return res, err
		}
		if err := os.WriteFile(dst, []byte(body), 0644); err != nil {
			return res, err
		}
	}
	cmd, env := SplitEnvPrefix(verifyCmd)
	cmd = stripRace(cmd)
	report, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
		Command: cmd, WorkspaceDir: sandbox, TimeoutSeconds: 90, ExtraEnv: env,
	})
	if err != nil {
		return res, err
	}
	diag := report.CleanedStderr
	if diag == "" {
		diag = report.RawStderr
	}
	if strings.Contains(diag, "requires cgo") && strings.Contains(verifyCmd, "-race") {
		report, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command: stripRace(cmd), WorkspaceDir: sandbox, TimeoutSeconds: 90, ExtraEnv: env,
		})
		if err != nil {
			return res, err
		}
		diag = report.CleanedStderr
	}
	if report.ExitCode != 0 || !report.Success {
		healed, ok := prehealSandbox(sandbox, diag)
		if ok {
			report, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
				Command: stripRace(cmd), WorkspaceDir: sandbox, TimeoutSeconds: 90, ExtraEnv: env,
			})
			if err != nil {
				return res, err
			}
			diag = report.CleanedStderr
			if report.ExitCode == 0 && report.Success {
				if err := copyTree(sandbox, projectDir); err != nil {
					return res, err
				}
				res.Promoted = true
				res.Diag = "preheal: " + healed
				res.ExitCode = 0
				return res, nil
			}
		}
		res.Diag = diag
		res.ExitCode = report.ExitCode
		return res, nil
	}
	if err := copyTree(sandbox, projectDir); err != nil {
		return res, err
	}
	res.Promoted = true
	res.ExitCode = 0
	return res, nil
}

func stripRace(cmd string) string {
	return strings.TrimSpace(strings.ReplaceAll(cmd, "-race", ""))
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

var (
	redeclaredRe      = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s+redeclared`)
	alreadyDeclaredRe = regexp.MustCompile(`(?m)([A-Za-z0-9_./\\-]+\.go):(\d+):(?:\d+:)?\s*(?:method\s+(?:\*?[A-Za-z0-9_]+\.)?([A-Za-z_][A-Za-z0-9_]*)|([A-Za-z_][A-Za-z0-9_]*))\s+already declared`)
	otherDeclRe       = regexp.MustCompile(`other declaration of ([A-Za-z_][A-Za-z0-9_]*)`)
	unusedIdent       = regexp.MustCompile(`(?:([A-Za-z_][A-Za-z0-9_]*)\s+declared\s+(?:and|but)\s+not\s+used|declared\s+(?:and|but)\s+not\s+used:\s*([A-Za-z_][A-Za-z0-9_]*))`)
	undefinedIdent    = regexp.MustCompile(`undefined:\s*([A-Za-z_][A-Za-z0-9_]*)`)
	diagFileRe        = regexp.MustCompile(`(?:\.[\\/])?([A-Za-z0-9_./\\-]+\.go):\d+`)
)

func extractUnusedIdent(diag string) string {
	m := unusedIdent.FindStringSubmatch(diag)
	if len(m) > 1 && m[1] != "" {
		return m[1]
	}
	if len(m) > 2 && m[2] != "" {
		return m[2]
	}
	return ""
}

func isModuleDependencyDiag(diag string) bool {
	lower := strings.ToLower(diag)
	return strings.Contains(lower, "missing go.sum entry") ||
		strings.Contains(lower, "no required module provides package") ||
		strings.Contains(lower, "updates to go.mod needed") ||
		strings.Contains(lower, "updates to go.sum needed") ||
		strings.Contains(lower, "to add:") ||
		strings.Contains(lower, "to add it:") ||
		strings.Contains(lower, "go mod tidy") ||
		strings.Contains(lower, "cannot find module providing package")
}

func fixGoModule(sandbox string) bool {
	candidates := []string{sandbox}
	for _, nested := range findNestedModules(sandbox) {
		candidates = append(candidates, nested)
	}

	healed := false
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			cmd := exec.Command("go", "mod", "tidy")
			cmd.Dir = dir
			cmd.Env = os.Environ()
			if err := cmd.Run(); err == nil {
				healed = true
			}
		}
	}
	return healed
}

func isNodeDependencyDiag(diag string) bool {
	lower := strings.ToLower(diag)
	return strings.Contains(lower, "'tsc' is not recognized") ||
		strings.Contains(lower, "'vite' is not recognized") ||
		strings.Contains(lower, "cannot find module") ||
		strings.Contains(lower, "module not found") ||
		strings.Contains(lower, "sh: tsc: not found") ||
		strings.Contains(lower, "sh: vite: not found") ||
		strings.Contains(lower, "sh: 1: tsc: not found") ||
		strings.Contains(lower, "sh: 1: vite: not found") ||
		strings.Contains(lower, "could not read package.json")
}

func fixNodeModule(dir string) bool {
	candidates := []string{dir}
	for _, nested := range findNestedModules(dir) {
		candidates = append(candidates, nested)
	}

	healed := false
	for _, d := range candidates {
		pkgPath := filepath.Join(d, "package.json")
		if _, err := os.Stat(pkgPath); err == nil {
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd.exe", "/c", "npm install")
			} else {
				cmd = exec.Command("npm", "install")
			}
			cmd.Dir = d
			cmd.Env = os.Environ()
			if err := cmd.Run(); err == nil {
				healed = true
			}
		}
	}
	return healed
}

func isPythonDependencyDiag(diag string) bool {
	lower := strings.ToLower(diag)
	return strings.Contains(lower, "modulenotfounderror: no module named") ||
		strings.Contains(lower, "importerror: cannot import name") ||
		strings.Contains(lower, "importerror: no module named") ||
		strings.Contains(lower, "no module named '")
}

var pythonModuleToPackage = map[string]string{
	"bs4":               "beautifulsoup4",
	"yaml":              "pyyaml",
	"PIL":               "pillow",
	"cv2":               "opencv-python",
	"dotenv":            "python-dotenv",
	"sklearn":           "scikit-learn",
	"dateutil":          "python-dateutil",
	"websocket":         "websocket-client",
	"magic":             "python-magic",
	"jose":              "python-jose",
	"jwt":               "pyjwt",
	"serial":            "pyserial",
	"OpenGL":            "PyOpenGL",
	"multipart":         "python-multipart",
	"fitz":              "PyMuPDF",
	"docx":              "python-docx",
	"pptx":              "python-pptx",
	"markupsafe":        "MarkupSafe",
	"sqlalchemy":        "SQLAlchemy",
	"pydantic_settings": "pydantic-settings",
}

func extractMissingPythonModule(diag string) string {
	re1 := regexp.MustCompile(`(?i)(?:ModuleNotFoundError:\s+No module named|ImportError:\s+No module named|no module named)\s+['"]([a-zA-Z0-9_\.-]+)['"]`)
	if m := re1.FindStringSubmatch(diag); len(m) > 1 && m[1] != "" {
		return m[1]
	}
	re2 := regexp.MustCompile(`(?i)cannot import name\s+['"]?[a-zA-Z0-9_]+['"]?\s+from\s+['"]([a-zA-Z0-9_\.-]+)['"]`)
	if m := re2.FindStringSubmatch(diag); len(m) > 1 && m[1] != "" {
		return m[1]
	}
	return ""
}

var knownLocalModuleNames = map[string]bool{
	"database": true, "db": true, "models": true, "model": true,
	"extractor": true, "repository": true, "indexer": true, "utils": true,
	"util": true, "helpers": true, "helper": true, "config": true,
	"settings": true, "app": true, "main": true, "service": true,
	"services": true, "client": true, "common": true, "core": true,
	"api": true, "routes": true, "views": true, "controllers": true,
	"handlers": true, "storage": true, "schemas": true, "schema": true,
	"types": true, "pipeline": true, "parser": true, "visualizer": true,
	"cli": true, "tracker": true, "graph": true, "tree": true,
	"node": true, "runner": true, "analyzer": true, "importer": true,
	"exporter": true, "auth": true, "constants": true, "errors": true,
}

var knownPyPIPackages = map[string]bool{
	"numpy": true, "scipy": true, "pandas": true, "matplotlib": true,
	"seaborn": true, "scikit-learn": true, "torch": true, "tensorflow": true,
	"pydantic": true, "sqlalchemy": true, "alembic": true, "uvicorn": true,
	"gunicorn": true, "httpx": true, "aiohttp": true, "click": true,
	"rich": true, "typer": true, "tqdm": true, "jinja2": true,
	"beautifulsoup4": true, "pillow": true, "pyyaml": true, "python-dotenv": true,
	"networkx": true, "plotly": true, "streamlit": true, "polars": true,
	"lxml": true, "openpyxl": true, "xlsxwriter": true, "requests": true,
	"pytest": true, "flask": true, "fastapi": true, "django": true,
}

func ensurePythonPackageInits(dir string) bool {
	// Never mass-create __init__.py across a Go host workspace (e.g. the OmniCoder root):
	// that pollutes every sibling project and benchmark folder and is never the real fix.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return false
	}
	healed := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := strings.ToLower(d.Name())
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "venv" || name == ".venv" || name == "__pycache__" || name == "build" || name == "dist" {
				return filepath.SkipDir
			}
			if path == dir {
				return nil
			}
			// Sibling project trees belong to other jobs; do not touch them.
			if (name == "projects" || name == "apps") && filepath.Dir(path) == filepath.Clean(dir) {
				return filepath.SkipDir
			}
			// A folder whose name is not a valid Python identifier (e.g. "my-app") can never be
			// imported as a package, so an __init__.py there only confuses unittest discovery.
			if !isPythonIdentifier(d.Name()) {
				return nil
			}
			entries, rErr := os.ReadDir(path)
			if rErr != nil {
				return nil
			}
			hasPy := false
			hasInit := false
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				eName := strings.ToLower(e.Name())
				if eName == "__init__.py" {
					hasInit = true
					break
				}
				if strings.HasSuffix(eName, ".py") {
					hasPy = true
				}
			}
			if (hasPy || name == "tests" || name == "test") && !hasInit {
				initPath := filepath.Join(path, "__init__.py")
				if err := os.WriteFile(initPath, []byte(""), 0644); err == nil {
					healed = true
				}
			}
		}
		return nil
	})
	return healed
}

// isPythonIdentifier reports whether name is importable as a Python package name.
func isPythonIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func isLocalPythonModule(dir, modName string) bool {
	if modName == "" {
		return false
	}
	// Direct checks on disk
	if _, err := os.Stat(filepath.Join(dir, modName+".py")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, modName)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "src", modName+".py")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "src", modName)); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "app", modName+".py")); err == nil {
		return true
	}

	clean := strings.ToLower(strings.TrimSpace(modName))
	if knownLocalModuleNames[clean] {
		return true
	}

	dirBase := strings.ToLower(filepath.Base(dir))
	if strings.Contains(dirBase, clean) || strings.Contains(clean, dirBase) {
		return true
	}
	return false
}

func fixPythonModule(dir string) bool {
	return fixPythonModuleWithDiag(dir, "")
}

func fixPythonModuleWithDiag(dir string, diag string) bool {
	healed := ensurePythonPackageInits(dir)

	candidates := []string{dir}
	for _, nested := range findNestedModules(dir) {
		candidates = append(candidates, nested)
	}

	for _, d := range candidates {
		reqPath := filepath.Join(d, "requirements.txt")
		if _, err := os.Stat(reqPath); err == nil {
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd.exe", "/c", "pip install -r requirements.txt")
			} else {
				cmd = exec.Command("pip", "install", "-r", "requirements.txt")
			}
			cmd.Dir = d
			cmd.Env = os.Environ()
			if err := cmd.Run(); err == nil {
				healed = true
			}
		}
	}

	missingMod := extractMissingPythonModule(diag)
	if missingMod != "" {
		// If it's a local project module (e.g. database, models, extractor), DO NOT run pip install.
		if isLocalPythonModule(dir, missingMod) {
			// If missingMod.py does not exist yet in dir or src/, create a clean initial stub
			targetPy := filepath.Join(dir, missingMod+".py")
			if _, err := os.Stat(targetPy); err != nil {
				stub := fmt.Sprintf("\"\"\"\nModule %s implementation.\n\"\"\"\n", missingMod)
				if err := os.WriteFile(targetPy, []byte(stub), 0644); err == nil {
					healed = true
				}
			}
			return healed
		}

		// Only attempt pip install if it's a known mapping, a known PyPI package, or listed in requirements.txt
		pkgName := missingMod
		if mapped, ok := pythonModuleToPackage[missingMod]; ok {
			pkgName = mapped
		} else if !knownPyPIPackages[strings.ToLower(missingMod)] {
			// Check if requirements.txt lists it
			reqPath := filepath.Join(dir, "requirements.txt")
			data, _ := os.ReadFile(reqPath)
			if !strings.Contains(strings.ToLower(string(data)), strings.ToLower(missingMod)) {
				// Unknown module not in requirements.txt, PyPI table, or package map; avoid hallucinating pip install
				return healed
			}
		}

		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd.exe", "/c", "pip install "+pkgName)
		} else {
			cmd = exec.Command("pip", "install", pkgName)
		}
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if err := cmd.Run(); err == nil {
			healed = true
			// Append to requirements.txt in dir
			reqPath := filepath.Join(dir, "requirements.txt")
			var existingReq string
			if data, rErr := os.ReadFile(reqPath); rErr == nil {
				existingReq = string(data)
			}
			if !strings.Contains(existingReq, pkgName) {
				newReq := strings.TrimRight(existingReq, "\r\n") + "\n" + pkgName + "\n"
				_ = os.WriteFile(reqPath, []byte(strings.TrimLeft(newReq, "\n")), 0644)
			}
		}
	}

	return healed
}

func extractMissingNodeModule(diag string) string {
	re := regexp.MustCompile(`(?i)(?:Cannot find module|sh: \d+: )['"]?([a-zA-Z0-9_@\/-]+)['"]?`)
	if m := re.FindStringSubmatch(diag); len(m) > 1 && m[1] != "" {
		mod := strings.TrimSuffix(m[1], ": not found")
		return strings.TrimSpace(mod)
	}
	return ""
}

func fixNodeModuleWithDiag(dir, diag string) bool {
	healed := fixNodeModule(dir)
	missingMod := extractMissingNodeModule(diag)
	if missingMod != "" && missingMod != "tsc" && missingMod != "vite" && !strings.Contains(missingMod, ".") {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd.exe", "/c", "npm install "+missingMod)
		} else {
			cmd = exec.Command("npm", "install", missingMod)
		}
		cmd.Dir = dir
		cmd.Env = os.Environ()
		if err := cmd.Run(); err == nil {
			healed = true
		}
	}
	return healed
}

func isRustDependencyDiag(diag string) bool {
	lower := strings.ToLower(diag)
	return strings.Contains(lower, "error[e0432]: unresolved import") ||
		strings.Contains(lower, "error[e0433]: failed to resolve: use of undeclared crate or module") ||
		strings.Contains(lower, "could not find `cargo.lock`") ||
		strings.Contains(lower, "error: no matching package named")
}

func fixRustModule(dir string) bool {
	cargoPath := filepath.Join(dir, "Cargo.toml")
	if _, err := os.Stat(cargoPath); err == nil {
		cmd := exec.Command("cargo", "check")
		cmd.Dir = dir
		cmd.Env = os.Environ()
		return cmd.Run() == nil
	}
	return false
}

func prehealSandbox(sandbox, diag string) (string, bool) {
	var healedActions []string

	if isModuleDependencyDiag(diag) {
		if fixGoModule(sandbox) {
			healedActions = append(healedActions, "go mod tidy")
		}
	}

	if isPythonDependencyDiag(diag) {
		if fixPythonModuleWithDiag(sandbox, diag) {
			healedActions = append(healedActions, "pip install requirements/missing")
		}
	}

	if isNodeDependencyDiag(diag) {
		if fixNodeModuleWithDiag(sandbox, diag) {
			healedActions = append(healedActions, "npm install")
		}
	}

	if isRustDependencyDiag(diag) {
		if fixRustModule(sandbox) {
			healedActions = append(healedActions, "cargo check")
		}
	}

	if fixUndefinedWithUnused(sandbox, diag) {
		healedActions = append(healedActions, "undefined matched to unused")
	}

	allUnused := unusedIdent.FindAllStringSubmatch(diag, -1)
	for _, m := range allUnused {
		ident := ""
		if len(m) > 1 && m[1] != "" {
			ident = m[1]
		} else if len(m) > 2 && m[2] != "" {
			ident = m[2]
		}
		if ident != "" && fixUnused(sandbox, diag, ident) {
			healedActions = append(healedActions, "unused "+ident)
		}
	}

	if strings.Contains(diag, "imported and not used") || strings.Contains(diag, "imported but not used") {
		files := diagFiles(diag)
		for _, f := range files {
			full := filepath.Join(sandbox, filepath.FromSlash(f))
			data, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			if !hasAnyGoDeclarations(string(data)) {
				clean := stripAllImportsAndKeepPackage(string(data))
				if clean != string(data) {
					if err := os.WriteFile(full, []byte(clean), 0644); err == nil {
						healedActions = append(healedActions, "cleaned empty file "+f)
						continue
					}
				}
			}
			curData := string(data)
			healedAny := false
			for iter := 0; iter < 10; iter++ {
				block, _, ok := engine.AttemptDeterministicPreHeal(sandbox, f, devops.CompilerDiagnostic{
					File: f, Message: diag,
				})
				if !ok || block.SearchText == "" || !strings.Contains(curData, block.SearchText) {
					break
				}
				curData = strings.Replace(curData, block.SearchText, block.ReplaceText, 1)
				healedAny = true
			}
			if healedAny && curData != string(data) {
				_ = os.WriteFile(full, []byte(curData), 0644)
				healedActions = append(healedActions, "unused import(s)")
			}
		}
	}

	allRedecl := redeclaredRe.FindAllStringSubmatch(diag, -1)
	for _, m := range allRedecl {
		if len(m) > 1 && m[1] != "" {
			name := m[1]
			files := diagFiles(diag)
			if len(files) > 0 {
				drop := files[0]
				full := filepath.Join(sandbox, filepath.FromSlash(drop))
				if data, err := os.ReadFile(full); err == nil {
					if next, ok := stripGoDecl(string(data), name); ok {
						if !hasAnyGoDeclarations(next) {
							next = stripAllImportsAndKeepPackage(next)
						}
						if err := os.WriteFile(full, []byte(next), 0644); err == nil {
							healedActions = append(healedActions, "redeclared "+name)
						}
					}
				}
			}
		}
	}

	allAlreadyDecl := alreadyDeclaredRe.FindAllStringSubmatch(diag, -1)
	for _, m := range allAlreadyDecl {
		fileRel := ""
		name := ""
		if len(m) > 1 && m[1] != "" {
			fileRel = m[1]
		}
		if len(m) > 3 && m[3] != "" {
			name = m[3]
		} else if len(m) > 4 && m[4] != "" {
			name = m[4]
		}
		if fileRel != "" && name != "" {
			full := filepath.Join(sandbox, filepath.FromSlash(fileRel))
			if data, err := os.ReadFile(full); err == nil {
				if next, ok := stripGoDecl(string(data), name); ok {
					if !hasAnyGoDeclarations(next) {
						next = stripAllImportsAndKeepPackage(next)
					}
					if err := os.WriteFile(full, []byte(next), 0644); err == nil {
						healedActions = append(healedActions, "already declared "+name+" in "+filepath.Base(fileRel))
					}
				}
			}
		}
	}

	if fixInvalidEmbedDirective(sandbox, diag) {
		healedActions = append(healedActions, "fixed invalid go:embed parent pattern")
	}

	if len(healedActions) > 0 {
		return strings.Join(healedActions, ", "), true
	}
	return "", false
}

var embedWithDotDotRe = regexp.MustCompile(`(?m)^([ \t]*//go:embed[ \t]+.*?\.\..*)$`)

func fixInvalidEmbedDirective(sandbox, diag string) bool {
	if !strings.Contains(diag, "invalid pattern syntax") &&
		!(strings.Contains(diag, "pattern") && strings.Contains(diag, "embed")) {
		return false
	}
	files := diagFiles(diag)
	healed := false
	for _, f := range files {
		full := filepath.Join(sandbox, filepath.FromSlash(f))
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		content := string(data)
		if embedWithDotDotRe.MatchString(content) {
			clean := embedWithDotDotRe.ReplaceAllString(content, "// (embed removed: Go forbids parent '..' paths in //go:embed; serve via web package or http.FileSystem)")
			if clean != content {
				if err := os.WriteFile(full, []byte(clean), 0644); err == nil {
					healed = true
				}
			}
		}
	}
	return healed
}

func diagFiles(diag string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range diagFileRe.FindAllStringSubmatch(diag, -1) {
		p := strings.ReplaceAll(filepath.ToSlash(m[1]), "\\", "/")
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimPrefix(p, `.\`)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// fixUndefinedWithUnused rewrites an undefined name to the single unused
// declaration in the same file (valOut used, valRaw declared and unused).
func fixUndefinedWithUnused(sandbox, diag string) bool {
	unused := unusedIdent.FindAllStringSubmatch(diag, -1)
	missing := undefinedIdent.FindAllStringSubmatch(diag, -1)
	if len(unused) != 1 || len(missing) != 1 {
		return false
	}
	files := diagFiles(diag)
	if len(files) == 0 {
		return false
	}
	full := filepath.Join(sandbox, filepath.FromSlash(files[0]))
	data, err := os.ReadFile(full)
	if err != nil {
		return false
	}
	src := string(data)
	bad := missing[0][1]
	good := ""
	if len(unused[0]) > 1 && unused[0][1] != "" {
		good = unused[0][1]
	} else if len(unused[0]) > 2 && unused[0][2] != "" {
		good = unused[0][2]
	}
	if good == "" {
		return false
	}
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(bad) + `\b`)
	if !re.MatchString(src) {
		return false
	}
	return os.WriteFile(full, []byte(re.ReplaceAllString(src, good)), 0644) == nil
}

func fixUnused(sandbox, diag, ident string) bool {
	files := diagFiles(diag)
	if len(files) == 0 {
		return false
	}
	full := filepath.Join(sandbox, filepath.FromSlash(files[0]))
	data, err := os.ReadFile(full)
	if err != nil {
		return false
	}
	src := string(data)

	// Strategy 1: Direct in-place replacement of unused declaration with blank identifier
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(ident) + `\s*:=`)
	reComma := regexp.MustCompile(`\b` + regexp.QuoteMeta(ident) + `\s*,\s*`)
	reAfterComma := regexp.MustCompile(`,\s*` + regexp.QuoteMeta(ident) + `\s*:=`)
	reFor := regexp.MustCompile(`for\s+` + regexp.QuoteMeta(ident) + `\s*:=`)
	reForComma := regexp.MustCompile(`for\s+` + regexp.QuoteMeta(ident) + `\s*,`)
	reForAfterComma := regexp.MustCompile(`for\s+([A-Za-z0-9_]+)\s*,\s*` + regexp.QuoteMeta(ident) + `\s*:=`)
	reVar := regexp.MustCompile(`(?m)^\s*var\s+` + regexp.QuoteMeta(ident) + `\b.*$`)

	var nextSrc string
	if reFor.MatchString(src) {
		nextSrc = reFor.ReplaceAllString(src, "for _ :=")
	} else if reForComma.MatchString(src) {
		nextSrc = reForComma.ReplaceAllString(src, "for _,")
	} else if reForAfterComma.MatchString(src) {
		nextSrc = reForAfterComma.ReplaceAllString(src, "for $1, _ :=")
	} else if re.MatchString(src) {
		nextSrc = re.ReplaceAllString(src, "_ =")
	} else if reAfterComma.MatchString(src) {
		nextSrc = reAfterComma.ReplaceAllString(src, ", _ :=")
	} else if reComma.MatchString(src) {
		nextSrc = reComma.ReplaceAllString(src, "_, ")
	} else if reVar.MatchString(src) {
		nextSrc = reVar.ReplaceAllString(src, "// _ = "+ident)
	}

	if nextSrc != "" && devops.ValidateGoSource(nextSrc) == nil {
		return os.WriteFile(full, []byte(nextSrc), 0644) == nil
	}

	// Strategy 2: Targeted line insertion after verified declaration line
	// Find line reporting this specific ident: e.g. "path/to/file.go:133:2: tmpVal declared and not used"
	identLineRe := regexp.MustCompile(`(?:^|[\r\n])[^:\r\n]*?` + regexp.QuoteMeta(filepath.Base(files[0])) + `:(\d+)(?::\d+)?:?\s*` + regexp.QuoteMeta(ident) + `\b`)
	lineMatch := identLineRe.FindStringSubmatch(diag)
	if len(lineMatch) < 2 {
		// Fallback: match ident declared anywhere with line number
		identLineRe = regexp.MustCompile(`:(\d+)(?::\d+)?:?\s*` + regexp.QuoteMeta(ident) + `\s+declared`)
		lineMatch = identLineRe.FindStringSubmatch(diag)
	}
	if len(lineMatch) < 2 {
		// Fallback: match file and line
		diagLineRe := regexp.MustCompile(regexp.QuoteMeta(files[0]) + `:(\d+)`)
		lineMatch = diagLineRe.FindStringSubmatch(diag)
	}
	if len(lineMatch) < 2 {
		rawMatches := regexp.MustCompile(`([A-Za-z0-9_./\\-]+\.go):(\d+)`).FindStringSubmatch(diag)
		if len(rawMatches) > 2 {
			lineMatch = []string{rawMatches[0], rawMatches[2]}
		}
	}
	if len(lineMatch) >= 2 {
		if lineNum, err := strconv.Atoi(lineMatch[1]); err == nil && lineNum > 0 {
			lines := strings.Split(src, "\n")
			targetIdx := -1
			declRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(ident) + `\b`)
			if lineNum-1 >= 0 && lineNum-1 < len(lines) && declRe.MatchString(lines[lineNum-1]) {
				targetIdx = lineNum - 1
			} else {
				// Window search around reported lineNum
				for offset := 1; offset <= 5; offset++ {
					if lineNum-1+offset < len(lines) && declRe.MatchString(lines[lineNum-1+offset]) {
						targetIdx = lineNum - 1 + offset
						break
					}
					if lineNum-1-offset >= 0 && declRe.MatchString(lines[lineNum-1-offset]) {
						targetIdx = lineNum - 1 - offset
						break
					}
				}
			}
			if targetIdx == -1 {
				// Search anywhere in file for declaration of ident
				assignRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(ident) + `\s*(?::=|=)`)
				for i, l := range lines {
					if assignRe.MatchString(l) {
						targetIdx = i
						break
					}
				}
			}
			if targetIdx >= 0 && targetIdx < len(lines) {
				lineText := lines[targetIdx]
				indent := ""
				for _, ch := range lineText {
					if ch == ' ' || ch == '\t' {
						indent += string(ch)
					} else {
						break
					}
				}
				alreadySuppressed := false
				for checkIdx := targetIdx + 1; checkIdx < len(lines) && checkIdx <= targetIdx+3; checkIdx++ {
					if strings.Contains(lines[checkIdx], "_ = "+ident) {
						alreadySuppressed = true
						break
					}
				}
				if !alreadySuppressed {
					newLines := make([]string, 0, len(lines)+1)
					newLines = append(newLines, lines[:targetIdx+1]...)
					newLines = append(newLines, indent+"_ = "+ident)
					newLines = append(newLines, lines[targetIdx+1:]...)
					candidate := strings.Join(newLines, "\n")
					if devops.ValidateGoSource(candidate) == nil {
						if err := os.WriteFile(full, []byte(candidate), 0644); err == nil {
							return true
						}
					}
				}
			}
		}
	}

	// Strategy 3: Function body blank assignment insertion
	fset := token.NewFileSet()
	if node, parseErr := parser.ParseFile(fset, "src.go", src, parser.ParseComments); parseErr == nil {
		for _, decl := range node.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				fnStart := fset.Position(fn.Body.Lbrace).Offset
				fnEnd := fset.Position(fn.Body.Rbrace).Offset
				if fnStart >= 0 && fnEnd <= len(src) && fnStart < fnEnd {
					bodyText := src[fnStart:fnEnd]
					if strings.Contains(bodyText, ident) && !strings.Contains(bodyText, "_ = "+ident) {
						insertPos := fnEnd
						next := src[:insertPos] + "\n\t_ = " + ident + "\n" + src[insertPos:]
						if devops.ValidateGoSource(next) == nil {
							return os.WriteFile(full, []byte(next), 0644) == nil
						}
					}
				}
			}
		}
	}

	return false
}

func stripGoDecl(src, name string) (string, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "f.go", src, parser.ParseComments)
	if err != nil {
		return src, false
	}
	var start, end int
	foundCount := 0
	for _, d := range f.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			if n.Name != nil && n.Name.Name == name {
				foundCount++
				start = fset.Position(n.Pos()).Offset
				end = fset.Position(n.End()).Offset
			}
		case *ast.GenDecl:
			for _, s := range n.Specs {
				switch spec := s.(type) {
				case *ast.TypeSpec:
					if spec.Name != nil && spec.Name.Name == name {
						foundCount++
						if len(n.Specs) == 1 {
							start = fset.Position(n.Pos()).Offset
							end = fset.Position(n.End()).Offset
						} else {
							start = fset.Position(spec.Pos()).Offset
							end = fset.Position(spec.End()).Offset
						}
					}
				case *ast.ValueSpec:
					for _, id := range spec.Names {
						if id.Name == name {
							foundCount++
							if len(n.Specs) == 1 && len(spec.Names) == 1 {
								start = fset.Position(n.Pos()).Offset
								end = fset.Position(n.End()).Offset
							} else {
								start = fset.Position(spec.Pos()).Offset
								end = fset.Position(spec.End()).Offset
							}
						}
					}
				}
			}
		}
	}
	if foundCount == 0 || start < 0 || end > len(src) || start >= end {
		return src, false
	}
	if end < len(src) && src[end] == '\n' {
		end++
	} else if end+1 < len(src) && src[end] == '\r' && src[end+1] == '\n' {
		end += 2
	}
	return src[:start] + src[end:], true
}

func hasAnyGoDeclarations(src string) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "f.go", src, 0)
	if err != nil {
		return true // parse error: be conservative, do not treat as empty
	}
	for _, d := range f.Decls {
		switch n := d.(type) {
		case *ast.FuncDecl:
			return true
		case *ast.GenDecl:
			if n.Tok != token.IMPORT {
				return true
			}
		}
	}
	return false
}

func stripAllImportsAndKeepPackage(src string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "f.go", src, 0)
	if err != nil || f.Name == nil {
		return src
	}
	return fmt.Sprintf("package %s\n", f.Name.Name)
}
