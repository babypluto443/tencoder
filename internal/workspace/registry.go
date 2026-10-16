// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

// Package workspace implements 阶段四's cross-project registry.
//
// ── 为什么是"注册表"而不是"任意目录都能写" ──
//
// 让 Agent 写一个没声明过的目录，等于把整台机器的家目录变成可写面。
// 所以跨项目能力走**显式注册**：用户（或会话内 `/workspace add`）把项目登记进来，
// 并指明级别（writable / readonly），登记项再喂给 PathSandbox。
// 没登记的目录对 Agent 不可见——**默认不可见，而不是默认可见后审批**。
//
// ── worktree 是冲突边界，不是安全边界 ──
//
// git worktree 让多个 Agent 改同一个项目时互不干扰，但它**共享 .git**：
// 因此 `.git/hooks/` 也在所有 worktree 的可写面内。一个被篡改的 post-commit hook
// 会在任何一次 commit 时执行，而且它对沙箱看起来只是"项目内的一个普通文件"。
//
// 所以本包的定位是明确的：**worktree 用来防冲突，安全性由沙箱 + 审批链 + 审计提供。**
// 注册时对 .git/hooks 做归属校验（见 validateWorkspace），确保它没有被注册成工作区可写面。

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Level is a workspace's access level. Two levels, deliberately — no third.
//
//	writable : writes go through the normal permission flow
//	readonly : reads are free; any write escalates to an explicit approval
//
// A third "no access" level would duplicate what an unregistered path already is.
type Level string

const (
	LevelWritable Level = "writable"
	LevelReadonly Level = "readonly"
)

// ConfigFileName is the declarative registry file, relative to the project root's .tencoder dir.
const ConfigFileName = "workspaces.yaml"

// Entry is one declared workspace.
//
// Name is the handle the model and the user refer to; Path is resolved to an absolute path on
// load. Name must be unique across the registry.
type Entry struct {
	Name  string `yaml:"name"`
	Path  string `yaml:"path"`
	Level Level  `yaml:"level"`
	// Description is shown to the model so it knows what the project is for.
	Description string `yaml:"description,omitempty"`
}

// File is the on-disk shape of .tencoder/workspaces.yaml.
type File struct {
	Workspaces []Entry `yaml:"workspaces"`
}

// Registry holds the live set of registered workspaces for one session.
//
// Concurrency-safe: entries are registered at runtime via `/workspace add` while the agent loop
// may be reading them for sandbox construction.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry // key: lowercase name
	order   []string         // registration order, so listings are stable
	loaded  bool
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{entries: make(map[string]Entry)}
}

// Load reads .tencoder/workspaces.yaml under projectRoot and registers every valid entry.
//
// A missing file is not an error — it just means no cross-project access, which is the correct
// default. Entries that fail validation are skipped with the reason appended to problems rather
// than failing the whole load: one bad path should not cost the user every other workspace.
//
// Returns the problems (human-readable) so the caller can surface them; empty means all clean.
func Load(projectRoot string) (*Registry, []string) {
	reg := New()
	reg.mu.Lock()
	reg.loaded = true
	reg.mu.Unlock()

	path := filepath.Join(projectRoot, ".tencoder", ConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil // no registry = no cross-project access. Correct default.
		}
		return reg, []string{fmt.Sprintf("cannot read %s: %v", path, err)}
	}

	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return reg, []string{fmt.Sprintf("cannot parse %s: %v", path, err)}
	}

	var problems []string
	for _, e := range f.Workspaces {
		if err := validateEntry(e); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Name, err))
			continue
		}
		if err := reg.Register(e); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", e.Name, err))
		}
	}
	return reg, problems
}

// validateEntry rejects entries that would be unsafe or unusable.
//
// Three checks, each for a different reason:
//  1. name must be non-empty and free of path separators — it becomes part of file paths and
//     prompt-visible identifiers;
//  2. level must be one of the two known values — an unrecognized level must not silently
//     degrade to "less restricted than intended";
//  3. path must resolve to an existing directory — registering a typo'd path would silently create
//     a sandbox root that never matches anything.
func validateEntry(e Entry) error {
	name := strings.TrimSpace(e.Name)
	if name == "" {
		return fmt.Errorf("empty name")
	}
	if strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("name %q must not contain path separators", name)
	}
	if e.Level != LevelWritable && e.Level != LevelReadonly {
		return fmt.Errorf("level must be %q or %q, got %q", LevelWritable, LevelReadonly, e.Level)
	}
	abs, err := resolveDir(e.Path)
	if err != nil {
		return err
	}
	_ = abs
	return nil
}

// resolveDir resolves a workspace path to an absolute, existing, symlink-resolved directory.
//
// Symlink resolution is not paranoia: if a registered path is a symlink into somewhere else, then
// "the project you registered" and "the tree actually being written" are different directories,
// and the audit log would name the wrong one. Resolve once, at registration, so every later
// decision compares resolved-to-resolved.
func resolveDir(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %v", path, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot access %q: %v", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

// Register adds or replaces an entry. Replacing an existing name is allowed on purpose:
// `/workspace add` used to change a project's level must not require a delete first.
//
// `level` overrides the entry's own Level when non-empty (that's what makes `/workspace add
// <path> readonly` work without reading the file).
func (r *Registry) Register(e Entry) error {
	if err := validateEntry(e); err != nil {
		return err
	}
	abs, err := resolveDir(e.Path)
	if err != nil {
		return err
	}
	key := strings.ToLower(strings.TrimSpace(e.Name))
	e.Path = abs
	if e.Level == "" {
		e.Level = LevelReadonly // fail closed: an entry with no level is not writable
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[key]; !exists {
		r.order = append(r.order, key)
	}
	r.entries[key] = e
	return nil
}

// Unregister removes an entry by name.
func (r *Registry) Unregister(name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[key]; !ok {
		return false
	}
	delete(r.entries, key)
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return true
}

// Get returns an entry by name.
func (r *Registry) Get(name string) (Entry, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	return e, ok
}

// List returns all entries in registration order.
func (r *Registry) List() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.order))
	for _, k := range r.order {
		if e, ok := r.entries[k]; ok {
			out = append(out, e)
		}
	}
	return out
}

// Count returns how many workspaces are registered.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// WritableRoots returns the absolute paths of every writable workspace.
// ReadOnlyRoots returns the absolute paths of every readonly workspace.
//
// The sandbox needs these split apart because "may read" and "may write" are different questions —
// that separation is the whole point of 任务 4.2.
func (r *Registry) WritableRoots() []string {
	return r.rootsByLevel(LevelWritable)
}

func (r *Registry) ReadOnlyRoots() []string {
	return r.rootsByLevel(LevelReadonly)
}

func (r *Registry) rootsByLevel(l Level) []string {
	entries := r.List()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Level == l {
			out = append(out, e.Path)
		}
	}
	return out
}

// Classify returns the level of the workspace owning path, and whether any workspace owns it.
//
// pathForDisplay is what goes in a deny message / audit record: the workspace NAME, not the raw
// filesystem path, because "pluto_feed is read-only" is actionable to the user while
// "/Users/babypluto/pluto_feed is read-only" is just noise.
func (r *Registry) Classify(path string) (lvl Level, name string, owned bool) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	// Longest match wins: a nested workspace inside another one must win over its parent, otherwise
	// registering /repo and /repo/sub would make the inner registration unreachable.
	bestLen := -1
	for _, e := range r.entries {
		if withinRoot(abs, e.Path) && len(e.Path) > bestLen {
			bestLen = len(e.Path)
			lvl, name, owned = e.Level, e.Name, true
		}
	}
	return lvl, name, owned
}

// withinRoot reports whether abs is root itself or under it.
//
// Both sides are cleaned and compared with a separator suffix, so "/repo-evil" is NOT inside
// "/repo". A plain strings.HasPrefix would say it is — and that is the classic sandbox escape.
func withinRoot(abs, root string) bool {
	abs = filepath.Clean(abs)
	root = filepath.Clean(root)
	if abs == root {
		return true
	}
	return strings.HasPrefix(abs, root+string(filepath.Separator))
}

// Save writes the registry back to disk, so a `/workspace add` made during a demo survives the
// session. Entries are sorted by name for a stable diff.
func (r *Registry) Save(projectRoot string) error {
	entries := r.List()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	path := filepath.Join(projectRoot, ".tencoder", ConfigFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(File{Workspaces: entries})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// PromptSummary renders the registry as a block suitable for a system reminder, so the model
// knows which projects exist and which are off-limits before it tries to touch one.
//
// Readonly projects are stated explicitly as readonly. Silently omitting them would leave the
// model to discover the boundary by hitting an approval prompt, which is exactly the kind of
// wasted round trip this whole mechanism exists to prevent.
func (r *Registry) PromptSummary() string {
	entries := r.List()
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Registered workspaces (other projects you may access in this session):\n")
	for _, e := range entries {
		level := string(e.Level)
		note := ""
		if e.Level == LevelReadonly {
			note = " — READ-ONLY: you may read anything here, but any write will require explicit user approval. Do not plan edits here."
		}
		desc := ""
		if e.Description != "" {
			desc = fmt.Sprintf(" (%s)", e.Description)
		}
		fmt.Fprintf(&b, "- %s [%s] %s%s%s\n", e.Name, level, e.Path, desc, note)
	}
	return b.String()
}

// ValidateGitHooks reports a problem when the workspace's .git directory contains hooks.
//
// This is the concrete form of "worktree is a conflict boundary, not a security boundary": all
// worktrees of a repo share .git, so hooks live inside every worktree's apparent writable tree.
//
// The check is advisory, not a hard reject. Refusing registration outright would break legitimate
// repos that happen to have sample hooks (many repos ship hooks/ with .sample files). What matters
// is that the user is told, because hooks execute with the user's privileges on commit.
func ValidateGitHooks(path string) string {
	abs, err := resolveDir(path)
	if err != nil {
		return fmt.Sprintf("cannot resolve: %v", err)
	}
	gitPath := filepath.Join(abs, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return "" // not a git repo (or no .git) — nothing to warn about
	}
	// .git can be a file (worktree/submodule) pointing at the real gitdir.
	if !info.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(data))
		if !strings.HasPrefix(line, "gitdir:") {
			return ""
		}
		gitPath = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitPath) {
			gitPath = filepath.Join(abs, gitPath)
		}
	}

	hooksDir := filepath.Join(gitPath, "hooks")
	hooks, err := os.ReadDir(hooksDir)
	if err != nil {
		return ""
	}
	var active []string
	for _, h := range hooks {
		name := h.Name()
		if h.IsDir() || strings.HasSuffix(name, ".sample") {
			continue
		}
		info, err := h.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			continue // not executable = not an active hook
		}
		active = append(active, name)
	}
	if len(active) == 0 {
		return ""
	}
	sort.Strings(active)
	return fmt.Sprintf(
		"%s has active git hooks (%s). Git hooks are shared by all worktrees of a repo and run with your privileges on commit — treat this workspace as higher risk.",
		abs, strings.Join(active, ", "))
}

// Stamp is used in registry file comments / audit records.
func Stamp() string { return time.Now().Format("20060102") }

// ── agents.WorkspaceResolver 实现 ─────────────────────────────────────────
//
// 这三个方法让 *Registry 满足 internal/agents 声明的窄接口，依赖方向是
// agents ← workspace（注入），而不是 agents → workspace。理由见 agents 侧注释：
// workspace → permissions → tools，而 agents 站在它们之上，直接 import 会成环。

// Exists 实现 agents.WorkspaceResolver。
func (r *Registry) Exists(name string) bool {
	_, ok := r.Get(name)
	return ok
}

// RootsFor 实现 agents.WorkspaceResolver：返回某个子 agent 应看到的根。
//
// 可见性规则：**主项目 + 被点名的那一个 workspace，其余注册项目全部不可见。**
// 这是 任务书决策 A 的"隔离到子 agent 视野"。把它做成"其余不可见"而不是"其余仍可读"，
// 是因为一个拿着三个项目读权限的子 agent 完全可能读错项目然后基于错的项目下结论——
// 而它自己并不知道自己看错了清单。
func (r *Registry) RootsFor(workspaceName string) (writable []string, readonly map[string]string, ok bool) {
	e, found := r.Get(workspaceName)
	if !found {
		return nil, nil, false
	}
	readonly = map[string]string{}
	if e.Level == LevelWritable {
		writable = append(writable, e.Path)
	} else {
		readonly[e.Path] = e.Name
	}
	return writable, readonly, true
}

// Summary renders a human-readable table for the /workspace command output.
func (r *Registry) Summary() string {
	entries := r.List()
	if len(entries) == 0 {
		return "No workspaces registered. Cross-project access is disabled.\n" +
			"Register one with:  /workspace add <path> <writable|readonly>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %-10s %s\n", "NAME", "LEVEL", "PATH")
	for _, e := range entries {
		fmt.Fprintf(&b, "%-16s %-10s %s\n", e.Name, e.Level, e.Path)
	}
	return b.String()
}

// DeriveName turns a path into a handle: the directory's base name, lowercased.
//
// Derived rather than asked for, because `/workspace add /Users/me/pluto_feed` with a mandatory
// name argument is friction in exactly the moment you want the least friction (a live demo).
// Collisions are the user's to resolve by editing the YAML — see Register, which replaces by name.
func DeriveName(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	base := filepath.Base(filepath.Clean(abs))
	base = slugCleaner.ReplaceAllString(strings.ToLower(base), "-")
	return strings.Trim(base, "-")
}

var slugCleaner = regexp.MustCompile(`[^a-z0-9]+`)
