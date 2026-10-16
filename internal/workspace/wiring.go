// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package workspace

import (
	"fmt"
	"strings"

	"tencoder/internal/audit"
	"tencoder/internal/permissions"
)

// 阶段四装配层：把 workspace 注册表、权限沙箱、审计日志三者接起来。
//
// 单独一个文件而不是散在 tui/remote 里，是因为这段接线有**顺序依赖**，
// 而顺序错了会静默失效——比如先建沙箱再注册 workspace，新注册的只读项目就进不了沙箱。
// 集中在这里，两个入口各自一行调用。

// Wiring is the assembled cross-project + audit setup for one session.
type Wiring struct {
	Registry *Registry
	// ReadOnlyRoots are the readonly project roots fed into the sandbox, keyed by project name.
	ReadOnlyRoots map[string]string
	// WritableRoots are the extra writable roots fed into the sandbox.
	WritableRoots []string
	// Problems are load-time validation messages to surface to the user (non-fatal).
	Problems []string
}

// Wire loads the workspace registry for projectRoot and derives the sandbox roots to install.
//
// projectRoot itself is NOT included in the returned writable roots — the caller already builds
// the sandbox with the main project. Only cross-project entries come back here.
func Wire(projectRoot string) *Wiring {
	reg, problems := Load(projectRoot)
	w := &Wiring{
		Registry:      reg,
		ReadOnlyRoots: map[string]string{},
		Problems:      problems,
	}
	for _, e := range reg.List() {
		if e.Level == LevelWritable {
			w.WritableRoots = append(w.WritableRoots, e.Path)
		} else {
			w.ReadOnlyRoots[e.Path] = e.Name
		}
	}
	return w
}

// InstallSandbox adds every registered project to the sandbox: writable ones as writable roots,
// readonly ones as readonly roots.
//
// Idempotent — AddWritableRoot/AddReadonlyRoot both dedupe, so calling this again after a runtime
// `/workspace add` is safe.
func (w *Wiring) InstallSandbox(sb *permissions.PathSandbox) {
	if w == nil || sb == nil {
		return
	}
	for _, root := range w.WritableRoots {
		sb.AddWritableRoot(root)
	}
	for root, name := range w.ReadOnlyRoots {
		sb.AddReadonlyRoot(root, name)
	}
}

// AuditSink adapts a *audit.Writer to permissions.AuditSink.
//
// The adapter exists so internal/permissions never imports internal/audit: permissions describes
// an event in its own vocabulary (AuditEntry) and this is the one place that knows both shapes.
type AuditSink struct {
	W *audit.Writer
}

// Record implements permissions.AuditSink. Non-blocking by delegation — audit.Writer.Record only
// does a channel send.
func (a *AuditSink) Record(e permissions.AuditEntry) {
	if a == nil || a.W == nil {
		return
	}
	a.W.Record(audit.Record{
		Time:      e.Time,
		SessionID: e.SessionID,
		Agent:     e.Agent,
		Tool:      e.Tool,
		Target:    e.Target,
		Action:    e.Action,
		Decision:  e.Decision,
		Outcome:   e.Outcome,
		Workspace: e.Workspace,
		Reason:    e.Reason,
	})
}

// ParseAddArgs parses `/workspace add <path> [level]`.
//
// Returns the level separately rather than defaulting it silently: registering a project is a
// security-relevant act, and defaulting a missing level to "writable" would make a typo like
// "/workspace add /Users/me/important" grant write access where the user may have meant read.
func ParseAddArgs(args string) (path string, level Level, err error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", "", fmt.Errorf("usage: /workspace add <path> [writable|readonly]")
	}
	path = fields[0]
	if len(fields) == 1 {
		return path, LevelReadonly, nil // safe default, see doc comment
	}
	level = Level(strings.ToLower(fields[1]))
	if level != LevelWritable && level != LevelReadonly {
		return "", "", fmt.Errorf("level must be 'writable' or 'readonly', got %q", fields[1])
	}
	return path, level, nil
}
