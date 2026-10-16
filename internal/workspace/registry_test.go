// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tencoder/internal/audit"
	"tencoder/internal/permissions"
	"tencoder/internal/tools"
)

// 阶段四的验证。两级模型、路径逃逸、只读升级、子 agent 视野收窄。

// mkProject creates a fake project directory and returns its absolute path.
func mkProject(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Return the symlink-resolved path so tests compare resolved-to-resolved, the same way the
	// registry and sandbox do. On macOS t.TempDir() is under /var (a symlink to /private/var),
	// so comparing raw paths here would fail for reasons unrelated to the logic under test.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func writeRegistry(t *testing.T, projectRoot, yaml string) {
	t.Helper()
	dir := filepath.Join(projectRoot, ".tencoder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ── 两级模型 ──────────────────────────────────────────────────────────────

func TestLoadMissingRegistryIsEmptyNotAnError(t *testing.T) {
	root := t.TempDir()
	reg, problems := Load(root)
	if len(problems) != 0 {
		t.Errorf("a missing registry must not produce problems, got %v", problems)
	}
	if reg.Count() != 0 {
		t.Errorf("no registry = no cross-project access, got %d entries", reg.Count())
	}
}

func TestLoadTwoLevels(t *testing.T) {
	root := t.TempDir()
	a := mkProject(t, "alpha")
	b := mkProject(t, "beta")
	writeRegistry(t, root, "workspaces:\n"+
		"  - name: alpha\n    path: "+a+"\n    level: writable\n"+
		"  - name: beta\n    path: "+b+"\n    level: readonly\n    description: reference repo\n")

	reg, problems := Load(root)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if reg.Count() != 2 {
		t.Fatalf("Count = %d, want 2", reg.Count())
	}
	if got := reg.WritableRoots(); len(got) != 1 || got[0] != a {
		t.Errorf("WritableRoots = %v, want [%s]", got, a)
	}
	if got := reg.ReadOnlyRoots(); len(got) != 1 || got[0] != b {
		t.Errorf("ReadOnlyRoots = %v, want [%s]", got, b)
	}
}

func TestLoadSkipsInvalidEntriesButKeepsValidOnes(t *testing.T) {
	root := t.TempDir()
	good := mkProject(t, "good")
	writeRegistry(t, root, "workspaces:\n"+
		"  - name: good\n    path: "+good+"\n    level: writable\n"+
		"  - name: badlevel\n    path: "+good+"\n    level: superuser\n"+ // invalid level
		"  - name: badpath\n    path: /nope/does/not/exist\n    level: readonly\n"+ // missing dir
		"  - name: has/slash\n    path: "+good+"\n    level: readonly\n") // name with separator

	reg, problems := Load(root)
	if reg.Count() != 1 {
		t.Errorf("the one valid entry should survive, got %d", reg.Count())
	}
	if len(problems) != 3 {
		t.Errorf("expected 3 problems for the 3 invalid entries, got %d: %v", len(problems), problems)
	}
}

func TestRegisterRejectsUnknownLevelFailsClosed(t *testing.T) {
	reg := New()
	dir := mkProject(t, "p")
	// An entry with NO level must not become writable.
	if err := reg.Register(Entry{Name: "p", Path: dir}); err == nil {
		t.Error("an entry with no level should be rejected")
	}
	e, ok := reg.Get("p")
	if ok {
		t.Errorf("rejected entry must not be stored, got %+v", e)
	}
}

func TestParseAddArgsDefaultsToReadonly(t *testing.T) {
	// Defaulting a missing level to "writable" would turn a typo into a grant of write access.
	_, lvl, err := ParseAddArgs("/tmp/someproject")
	if err != nil || lvl != LevelReadonly {
		t.Errorf("default level = %q err=%v, want readonly", lvl, err)
	}
	if _, _, err := ParseAddArgs("/tmp/p bogus"); err == nil {
		t.Error("an invalid explicit level must be rejected, not silently defaulted")
	}
}

func TestDeriveName(t *testing.T) {
	if got := DeriveName("/Users/me/pluto_feed"); got != "pluto-feed" {
		t.Errorf("DeriveName = %q, want pluto-feed", got)
	}
}

// ── 路径逃逸（沙箱的核心不变量）──────────────────────────────────────────

func TestWithinRootRejectsSiblingPrefix(t *testing.T) {
	// The classic escape: "/repo-evil" must NOT be considered inside "/repo".
	if withinRoot("/repo-evil/x.go", "/repo") {
		t.Error("sibling directory with a shared prefix must not pass withinRoot")
	}
	if !withinRoot("/repo/sub/x.go", "/repo") {
		t.Error("a genuine subdirectory must pass withinRoot")
	}
	if !withinRoot("/repo", "/repo") {
		t.Error("the root itself must pass withinRoot")
	}
}

func TestClassifyPicksLongestMatch(t *testing.T) {
	reg := New()
	outer := mkProject(t, "outer")
	inner := filepath.Join(outer, "vendor", "inner")
	os.MkdirAll(inner, 0o755)

	reg.Register(Entry{Name: "outer", Path: outer, Level: LevelWritable})
	reg.Register(Entry{Name: "inner", Path: inner, Level: LevelReadonly})

	lvl, name, owned := reg.Classify(filepath.Join(inner, "x.go"))
	if !owned || name != "inner" || lvl != LevelReadonly {
		t.Errorf("nested workspace must win over its parent: got %s/%s owned=%v", lvl, name, owned)
	}

	lvl, name, owned = reg.Classify(filepath.Join(outer, "src", "y.go"))
	if !owned || name != "outer" || lvl != LevelWritable {
		t.Errorf("outer workspace classification wrong: got %s/%s", lvl, name)
	}
}

func TestClassifyUnownedPath(t *testing.T) {
	reg := New()
	reg.Register(Entry{Name: "a", Path: mkProject(t, "a"), Level: LevelWritable})
	if _, _, owned := reg.Classify("/etc/passwd"); owned {
		t.Error("a path outside every workspace must not be classified as owned")
	}
}

// ── 沙箱读写拆分 ──────────────────────────────────────────────────────────

func newSandboxWithReadonly(t *testing.T) (*permissions.PathSandbox, string) {
	t.Helper()
	main := mkProject(t, "main")
	ro := mkProject(t, "readonly_proj")
	sb := permissions.NewPathSandbox(main)
	sb.AddReadonlyRoot(ro, "pluto_feed")
	return sb, ro
}

func TestCheckReadAllowsReadonlyButCheckWriteDoesNot(t *testing.T) {
	sb, ro := newSandboxWithReadonly(t)
	target := filepath.Join(ro, "internal", "handler.go")

	if ok, reason := sb.CheckRead(target); !ok {
		t.Errorf("reading inside a readonly workspace must be allowed, got %q", reason)
	}
	ok, reason := sb.CheckWrite(target)
	if ok {
		t.Error("writing inside a readonly workspace must not be allowed")
	}
	if !strings.Contains(reason, "pluto_feed") {
		t.Errorf("refusal must name the project, got %q", reason)
	}
}

func TestReadonlyViolationNamesWorkspace(t *testing.T) {
	sb, ro := newSandboxWithReadonly(t)
	hit, name := sb.ReadonlyViolation(filepath.Join(ro, "a.go"))
	if !hit || name != "pluto_feed" {
		t.Errorf("ReadonlyViolation = (%v, %q), want (true, pluto_feed)", hit, name)
	}
}

func TestWritableRootInsideReadonlyTreeIsNotAViolation(t *testing.T) {
	// If /repo/vendor were readonly AND /repo were writable, the inner registration must not
	// block writes to /repo itself — otherwise registering a writable project would be silently
	// neutered by a nested readonly entry.
	sb := permissions.NewPathSandbox("/repo")
	sb.AddReadonlyRoot("/repo/vendor", "vendor")
	if hit, _ := sb.ReadonlyViolation("/repo/src/main.go"); hit {
		t.Error("a path inside the writable root must not be a readonly violation")
	}
	if ok, _ := sb.CheckWrite("/repo/src/main.go"); !ok {
		t.Error("write inside the writable root must be allowed")
	}
}

func TestOutsideAllWorkspacesIsRejectedForBoth(t *testing.T) {
	sb, _ := newSandboxWithReadonly(t)
	if ok, _ := sb.CheckRead("/etc/shadow"); ok {
		t.Error("read outside every workspace must be rejected")
	}
	if ok, _ := sb.CheckWrite("/etc/shadow"); ok {
		t.Error("write outside every workspace must be rejected")
	}
}

// ── 只读升级为审批链 ──────────────────────────────────────────────────────

func TestReadonlyWriteEscalatesToAskWithAllowAlwaysDisabled(t *testing.T) {
	main := mkProject(t, "main")
	ro := mkProject(t, "ro")
	sb := permissions.NewPathSandbox(main)
	sb.AddReadonlyRoot(ro, "pluto_feed")
	c := permissions.NewChecker(sb, &permissions.RuleEngine{}, permissions.ModeDefault)

	w := &writeTool{}
	d := c.Check(w, map[string]any{"file_path": filepath.Join(ro, "x.go")})
	if d.Effect != permissions.Ask {
		t.Errorf("readonly write must ASK, got %s (%s)", d.Effect, d.Reason)
	}
	if !d.ReadonlyWrite {
		t.Error("ReadonlyWrite must be set so the TUI can disable 'always allow'")
	}
	if d.Workspace != "pluto_feed" {
		t.Errorf("Workspace = %q, want pluto_feed", d.Workspace)
	}
}

func TestWritableProjectDoesNotEscalate(t *testing.T) {
	// Acceptance requirement: writable projects get NO new confirmation. If this escalated the
	// same way readonly does, 阶段四 would be adding friction everywhere to solve a problem that
	// only exists for readonly projects.
	main := mkProject(t, "main")
	sb := permissions.NewPathSandbox(main)
	c := permissions.NewChecker(sb, &permissions.RuleEngine{}, permissions.ModeAcceptEdits)

	d := c.Check(&writeTool{}, map[string]any{"file_path": filepath.Join(main, "x.go")})
	if d.Effect != permissions.Allow {
		t.Errorf("writable project write must stay allowed, got %s", d.Effect)
	}
	if d.ReadonlyWrite {
		t.Error("a writable-project write must not be marked ReadonlyWrite")
	}
}

func TestReadInsideReadonlyNeedsNoApproval(t *testing.T) {
	main := mkProject(t, "main")
	ro := mkProject(t, "ro")
	sb := permissions.NewPathSandbox(main)
	sb.AddReadonlyRoot(ro, "pluto_feed")
	c := permissions.NewChecker(sb, &permissions.RuleEngine{}, permissions.ModeDefault)

	d := c.Check(&readTool{}, map[string]any{"file_path": filepath.Join(ro, "x.go")})
	if d.Effect != permissions.Allow {
		t.Errorf("reading a readonly project must be allowed outright, got %s (%s)", d.Effect, d.Reason)
	}
}

// ── 子 agent 视野收窄 ────────────────────────────────────────────────────

func TestRootsForScopesToTheNamedWorkspaceOnly(t *testing.T) {
	reg := New()
	a := mkProject(t, "alpha")
	b := mkProject(t, "beta")
	c := mkProject(t, "gamma")
	reg.Register(Entry{Name: "alpha", Path: a, Level: LevelWritable})
	reg.Register(Entry{Name: "beta", Path: b, Level: LevelReadonly})
	reg.Register(Entry{Name: "gamma", Path: c, Level: LevelReadonly})

	writable, readonly, ok := reg.RootsFor("alpha")
	if !ok {
		t.Fatal("alpha should resolve")
	}
	if len(writable) != 1 || writable[0] != a {
		t.Errorf("writable = %v, want [%s]", writable, a)
	}
	// The point of scoping: beta and gamma are NOT visible to an alpha-scoped sub-agent.
	if len(readonly) != 0 {
		t.Errorf("an alpha-scoped sub-agent must not see other projects, got %v", readonly)
	}

	writable, readonly, ok = reg.RootsFor("beta")
	if !ok || len(writable) != 0 {
		t.Errorf("beta is readonly: writable = %v", writable)
	}
	if _, present := readonly[b]; !present {
		t.Errorf("readonly must contain beta, got %v", readonly)
	}

	if _, _, ok := reg.RootsFor("nonexistent"); ok {
		t.Error("an unknown workspace must not resolve")
	}
}

// ── 装配 ─────────────────────────────────────────────────────────────────

func TestWireInstallsRootsIntoSandbox(t *testing.T) {
	root := t.TempDir()
	a := mkProject(t, "alpha")
	b := mkProject(t, "beta")
	writeRegistry(t, root, "workspaces:\n"+
		"  - name: alpha\n    path: "+a+"\n    level: writable\n"+
		"  - name: beta\n    path: "+b+"\n    level: readonly\n")

	w := Wire(root)
	sb := permissions.NewPathSandbox(root)
	w.InstallSandbox(sb)

	if ok, _ := sb.CheckWrite(filepath.Join(a, "x.go")); !ok {
		t.Error("writable workspace must be installed as writable")
	}
	ok, _ := sb.CheckWrite(filepath.Join(b, "x.go"))
	if ok {
		t.Error("readonly workspace must not be installed as writable")
	}
	if hit, name := sb.ReadonlyViolation(filepath.Join(b, "x.go")); !hit || name != "beta" {
		t.Errorf("readonly violation = (%v, %q), want (true, beta)", hit, name)
	}
}

func TestInstallSandboxIsIdempotent(t *testing.T) {
	root := t.TempDir()
	a := mkProject(t, "alpha")
	writeRegistry(t, root, "workspaces:\n  - name: alpha\n    path: "+a+"\n    level: writable\n")

	w := Wire(root)
	sb := permissions.NewPathSandbox(root)
	w.InstallSandbox(sb)
	before := len(sb.GetAllowedRoots())
	w.InstallSandbox(sb) // e.g. after a runtime /workspace add
	if after := len(sb.GetAllowedRoots()); after != before {
		t.Errorf("install must be idempotent: roots %d → %d", before, after)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	root := t.TempDir()
	a := mkProject(t, "alpha")
	reg := New()
	reg.Register(Entry{Name: "alpha", Path: a, Level: LevelWritable, Description: "the main repo"})
	if err := reg.Save(root); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, problems := Load(root)
	if len(problems) != 0 {
		t.Fatalf("reload problems: %v", problems)
	}
	e, ok := reloaded.Get("alpha")
	if !ok {
		t.Fatal("entry lost across save/load")
	}
	if e.Level != LevelWritable || e.Description != "the main repo" {
		t.Errorf("round trip lost data: %+v", e)
	}
}

func TestPromptSummaryMarksReadonlyExplicitly(t *testing.T) {
	reg := New()
	reg.Register(Entry{Name: "alpha", Path: mkProject(t, "alpha"), Level: LevelWritable})
	reg.Register(Entry{Name: "beta", Path: mkProject(t, "beta"), Level: LevelReadonly})

	s := reg.PromptSummary()
	if !strings.Contains(s, "READ-ONLY") {
		t.Errorf("readonly workspaces must be stated as readonly so the model doesn't plan edits:\n%s", s)
	}
	if strings.Contains(s, "[readonly] "+mustPath(reg, "beta")+" [writable]") {
		t.Errorf("level and path got crossed:\n%s", s)
	}
}

func TestGitHookWarningDetectsExecutableHook(t *testing.T) {
	repo := mkProject(t, "repo")
	hooks := filepath.Join(repo, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-commit.sample"), []byte("#!/bin/sh\n"), 0o755)
	if msg := ValidateGitHooks(repo); msg != "" {
		t.Errorf("a .sample hook must not trigger a warning, got %q", msg)
	}
	os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	msg := ValidateGitHooks(repo)
	if !strings.Contains(msg, "pre-commit") {
		t.Errorf("an executable hook must be reported, got %q", msg)
	}
}

func mustPath(reg *Registry, name string) string {
	e, ok := reg.Get(name)
	if !ok {
		return "<missing>"
	}
	return e.Path
}

// ── 测试桩 ───────────────────────────────────────────────────────────────

type writeTool struct{}

func (w *writeTool) Name() string                 { return "WriteFile" }
func (w *writeTool) Description() string          { return "write" }
func (w *writeTool) Category() tools.ToolCategory { return tools.CategoryWrite }
func (w *writeTool) Schema() map[string]any       { return map[string]any{"name": "WriteFile"} }
func (w *writeTool) Execute(context.Context, map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: "ok"}
}

type readTool struct{}

func (r *readTool) Name() string                 { return "ReadFile" }
func (r *readTool) Description() string          { return "read" }
func (r *readTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (r *readTool) Schema() map[string]any       { return map[string]any{"name": "ReadFile"} }
func (r *readTool) Execute(context.Context, map[string]any) tools.ToolResult {
	return tools.ToolResult{Output: "ok"}
}

// ── 审计适配器 ────────────────────────────────────────────────────────────

// TestAuditSinkAdapterForwardsAllFields — the adapter is the one place that knows both
// permissions.AuditEntry and audit.Record, so field loss there would silently gut the trail.
func TestAuditSinkAdapterForwardsAllFields(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	os.MkdirAll(main, 0o755)

	w, err := audit.NewWriter(root)
	if err != nil {
		t.Fatal(err)
	}
	c := permissions.NewChecker(permissions.NewPathSandbox(main), &permissions.RuleEngine{}, permissions.ModeDefault)
	c.Audit = &AuditSink{W: w}
	c.SessionID = "sess-9"
	c.AgentName = "main"

	c.Check(&writeTool{}, map[string]any{"file_path": filepath.Join(main, "x.go")})
	w.Close()

	recs, err := audit.ReadAll(filepath.Join(root, ".tencoder", audit.Subdir))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("adapter did not forward the record, got %d", len(recs))
	}
	r := recs[0]
	if r.SessionID != "sess-9" || r.Agent != "main" || r.Tool != "WriteFile" ||
		r.Target == "" || r.Decision == "" || r.Outcome == "" || r.Time == "" {
		t.Errorf("adapter lost fields: %+v", r)
	}
}
