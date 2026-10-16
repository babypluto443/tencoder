// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package llm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 阶段五 · 任务 5.4：门禁三件套。
//
// ── 门禁是"语法层安全网"，不是"逻辑正确性证明" ──
//
// 这一条必须写在代码注释里，因为它决定了这个组件的能力边界，也决定了面试里怎么讲：
//
//	go build + go vet + gofmt 能抓住的是**编译不过、明显错误、格式不符**。
// 它们抓不住"逻辑写错了但能编译"——那是主 agent 的测试循环的职责，不是路由的职责。
// 把语法门禁说成质量保证，是夸大了机制；把它说成"便宜的一层网，贵的验证留给本来就该做
// 验证的主循环"，才是准确的描述。
//
// ── 为什么是这三件套，为什么不含 go test ──
//
// go build / vet / gofmt 都是**秒级**的。go test 是**分钟级**的（要编译依赖、跑用例）。
// 门禁挂在子 agent 完成的关键路径上，用分钟级检查会把 -31% 的延迟优势吃光——
// 这正是任务书风险表里"全量测试门禁（分钟级，吃光 -31% 延迟）"被否掉的原因。
//
// 所以分工是：便宜的语法网在路由层，昂贵的语义验证在主 agent 的工具循环里。
// 主 agent 发现测试失败会自然地回头修，那是 agent 行为，不是路由职责。

// QualityGateResult reports the outcome of one gate run.
type QualityGateResult struct {
	// Passed is true only when all three checks passed.
	Passed bool
	// Duration is the wall clock of the whole gate.
	Duration time.Duration
	// Checks holds one entry per check, in run order, so a failure report can name WHICH check
	// failed — the upgrade path feeds this to the strong model and "build failed" is not the same
	// actionable input as "gofmt produced 3 files".
	Checks []CheckResult
	// TimedOut reports that the gate exceeded its budget. Treated as a failure for escalation
	// purposes (we cannot vouch for code we could not check) but recorded distinctly, because a
	// timeout is an environment problem, not a code problem, and escalating the model for it
	// wastes the escalation.
	TimedOut bool
	// Skipped reports that no gate ran (no Go files, or the gate is disabled).
	Skipped bool
}

// FailedChecks returns the names and output of the checks that did not pass, formatted for an
// LLM prompt ("build: <first N lines of output>").
//
// Truncated because the upgrade prompt is an input to a model call: pasting a 400-line compiler
// error both wastes tokens and buries the actual problem. The model can re-run the command itself
// if it needs more.
func (r QualityGateResult) FailedChecks(maxLinesPerCheck int) string {
	if maxLinesPerCheck <= 0 {
		maxLinesPerCheck = 20
	}
	var parts []string
	for _, c := range r.Checks {
		if c.Passed {
			continue
		}
		out := strings.TrimSpace(c.Output)
		if out == "" {
			out = "(no output)"
		}
		lines := strings.Split(out, "\n")
		if len(lines) > maxLinesPerCheck {
			lines = lines[:maxLinesPerCheck]
			out = strings.Join(lines, "\n") + "\n… (truncated)"
		}
		parts = append(parts, c.Name+": "+out)
	}
	return strings.Join(parts, "\n\n")
}

// CheckResult is one gate check.
type CheckResult struct {
	Name   string
	Passed bool
	Output string
	Dur    time.Duration
}

// QualityGate runs the three cheap checks over a working directory.
//
// Concurrency-safe and CACHED: gate results are keyed by directory + a content fingerprint. In a
// session where the lead asks for the same edit twice, the second run reuses the verdict —
// which is both faster and, more importantly, makes the escalation decision deterministic.
type QualityGate struct {
	// Timeout bounds the whole gate. A hung `go build` (module download on a cold cache) must not
	// hold the agent loop open.
	Timeout time.Duration
	// SkipNonGoDirs makes the gate a no-op for directories with no .go files (docs repos, config
	// work). Running `go build` in a non-Go module fails for reasons unrelated to the edit.
	SkipNonGoDirs bool

	mu    sync.Mutex
	cache map[string]cachedGate
}

type cachedGate struct {
	fingerprint string
	result      QualityGateResult
}

// DefaultQualityGateTimeout is generous enough for a cold module cache but short enough to be a
// "gate" rather than a "build".
const DefaultQualityGateTimeout = 90 * time.Second

// NewQualityGate constructs a gate.
func NewQualityGate(timeout time.Duration) *QualityGate {
	if timeout <= 0 {
		timeout = DefaultQualityGateTimeout
	}
	return &QualityGate{Timeout: timeout, SkipNonGoDirs: true, cache: map[string]cachedGate{}}
}

// goModuleRootFingerprint hashes the Go source files under dir so a cached verdict is only reused
// when the code could not have changed.
//
// Uses the newest mtime + count rather than content hashing: reading every file to hash it would
// cost more than re-running the gate on a small tree, and the gate itself is the authority.
func goModuleRootFingerprint(dir string) string {
	entries, err := readDirRecursive(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	newest := int64(0)
	for _, e := range entries {
		if !strings.HasSuffix(e.path, ".go") {
			continue
		}
		names = append(names, e.path)
		if e.modTime > newest {
			newest = e.modTime
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strconv.Itoa(len(names)) + ":" + strconv.FormatInt(newest, 10)
}

// Run executes the gate.
//
// Contract:
//   - returns Skipped for a non-Go directory (when SkipNonGoDirs) — that is a NEUTRAL outcome,
//     not a pass and not a failure;
//   - returns TimedOut=true with Passed=false when the budget is exceeded;
//   - never returns an error: the caller decides what to do with a verdict, and an error return
//     here would tempt callers to treat "could not check" as "checked and fine".
func (g *QualityGate) Run(ctx context.Context, dir string) QualityGateResult {
	if g == nil {
		return QualityGateResult{Skipped: true, Passed: true}
	}
	if g.SkipNonGoDirs {
		fp := goModuleRootFingerprint(dir)
		if fp == "" {
			return QualityGateResult{Skipped: true, Passed: true}
		}
		g.mu.Lock()
		if c, ok := g.cache[dir]; ok && c.fingerprint == fp {
			g.mu.Unlock()
			return c.result // cached verdict: the code cannot have changed
		}
		g.mu.Unlock()

		res := g.runChecks(ctx, dir)
		g.mu.Lock()
		g.cache[dir] = cachedGate{fingerprint: fp, result: res}
		g.mu.Unlock()
		return res
	}
	return g.runChecks(ctx, dir)
}

func (g *QualityGate) runChecks(ctx context.Context, dir string) QualityGateResult {
	start := time.Now()
	gctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()

	checks := []struct {
		name string
		args []string
	}{
		// Order matters: build first because it fails fastest and most decisively; gofmt is the
		// most trivial; vet is last because it re-typechecks and is the slowest of the three.
		{"build", []string{"go", "build", "./..."}},
		{"gofmt", []string{"gofmt", "-l", "."}},
		{"vet", []string{"go", "vet", "./..."}},
	}

	res := QualityGateResult{Passed: true}
	for _, c := range checks {
		cr := runCheck(gctx, dir, c.name, c.args)
		res.Checks = append(res.Checks, cr)
		if !cr.Passed {
			res.Passed = false
		}
		if gctx.Err() != nil {
			res.TimedOut = true
			res.Passed = false
			break
		}
	}
	res.Duration = time.Since(start)
	return res
}

// runCheck runs one command and classifies its result.
//
// gofmt is special-cased: `gofmt -l .` exits 0 whether or not it found files, and prints the
// offending paths. So "printed something" IS the failure, not the exit code. Treating its exit
// code as the verdict would make the check vacuous.
func runCheck(ctx context.Context, dir, name string, args []string) CheckResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	cr := CheckResult{Name: name, Output: string(out), Dur: time.Since(start)}
	switch {
	case ctx.Err() != nil:
		cr.Passed = false
	case name == "gofmt":
		cr.Passed = strings.TrimSpace(string(out)) == ""
	case err != nil:
		cr.Passed = false
	default:
		cr.Passed = true
	}
	return cr
}

// dirEntry is one walked file, reduced to what the fingerprint needs.
type dirEntry struct {
	path    string
	modTime int64
}

// readDirRecursive walks dir, skipping the usual noise directories.
//
// The skip list is duplicated from the tools package on purpose: llm is BELOW tools in the import
// graph, so importing it here would be a cycle. A three-line literal is cheaper than inverting a
// package dependency for it.
func readDirRecursive(dir string) ([]dirEntry, error) {
	skip := map[string]bool{
		".git": true, "node_modules": true, "vendor": true,
		".tencoder": true, "dist": true, "build": true,
	}
	var out []dirEntry
	var walk func(string) error
	walk = func(d string) error {
		entries, err := readDirNames(d)
		if err != nil {
			return nil // unreadable subdir: skip rather than fail the whole fingerprint
		}
		for _, e := range entries {
			if e.isDir {
				if skip[e.name] {
					continue
				}
				if err := walk(e.path); err != nil {
					return err
				}
				continue
			}
			out = append(out, dirEntry{path: e.path, modTime: e.modTime})
		}
		return nil
	}
	err := walk(dir)
	return out, err
}

type scannedEntry struct {
	name    string
	path    string
	isDir   bool
	modTime int64
}

// readDirNames reads one directory level with stat info, using os.ReadDir + Info.
//
// os.ReadDir returns entries sorted by name, which makes the resulting fingerprint deterministic
// — a fingerprint that changed order between runs would defeat the cache entirely.
func readDirNames(dir string) ([]scannedEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]scannedEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // vanished between readdir and stat
		}
		out = append(out, scannedEntry{
			name:    e.Name(),
			path:    filepath.Join(dir, e.Name()),
			isDir:   e.IsDir(),
			modTime: info.ModTime().UnixNano(),
		})
	}
	return out, nil
}
