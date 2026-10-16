// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 阶段一任务记忆链路的单测。覆盖：白名单解析 → 落盘 → 召回可见 → 蒸馏四动作 → 单飞保护。
// 每个测试都用 t.TempDir() 作为 projectRoot，绝不碰仓库里真实的 .tencoder/memory/。

func writeTaskFile(t *testing.T, projectRoot, name, body string) string {
	t.Helper()
	dir := GetTaskMemPath(projectRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	path := filepath.Join(dir, name)
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\ntype: task\n---\n\n%s", name, name, body)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func countFilesIn(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

// --- 1.2 提取与落盘 ---------------------------------------------------------

func TestParseTaskExtractionRejectsWorthSavingFalse(t *testing.T) {
	if _, ok := parseTaskExtraction(`{"worth_saving": false}`); ok {
		t.Error("worth_saving=false must not produce a record")
	}
}

func TestParseTaskExtractionRequiresPitfallAndRecovery(t *testing.T) {
	// A task memory without a recovery path is worse than none: recall would surface a warning the
	// reader cannot act on. So it must be rejected rather than half-written.
	cases := map[string]string{
		"no pitfall":     `{"worth_saving":true,"name":"a","description":"d","recovery":"r"}`,
		"no recovery":    `{"worth_saving":true,"name":"a","description":"d","pitfall":"p"}`,
		"no name":        `{"worth_saving":true,"description":"d","pitfall":"p","recovery":"r"}`,
		"no description": `{"worth_saving":true,"name":"a","pitfall":"p","recovery":"r"}`,
		"not json":       "I could not find anything worth saving.",
	}
	for name, raw := range cases {
		if _, ok := parseTaskExtraction(raw); ok {
			t.Errorf("%s: expected rejection, got a record", name)
		}
	}
}

func TestParseTaskExtractionSanitizesName(t *testing.T) {
	rec, ok := parseTaskExtraction(`{"worth_saving":true,"name":"Hard Coded Fixture / Path!","description":"add memory export — fixture hardcodes an absolute path","pitfall":"p","recovery":"r"}`)
	if !ok {
		t.Fatal("expected a valid record")
	}
	if strings.ContainsAny(rec.Name, "/!") || strings.ToLower(rec.Name) != rec.Name {
		t.Errorf("name not sanitized: %q", rec.Name)
	}
	if rec.Description != "add memory export — fixture hardcodes an absolute path" {
		t.Errorf("description mangled: %q", rec.Description)
	}
}

func TestParseTaskExtractionCollapsesNewlineInDescription(t *testing.T) {
	// A newline in frontmatter silently truncates the field for the scanner, so it must be
	// flattened rather than written raw.
	rec, ok := parseTaskExtraction(`{"worth_saving":true,"name":"a","description":"line one\nline two","pitfall":"p","recovery":"r"}`)
	if !ok {
		t.Fatal("expected a valid record")
	}
	if strings.ContainsAny(rec.Description, "\n\r\t") {
		t.Errorf("description still contains control chars: %q", rec.Description)
	}
}

func TestTaskWriterWritesFileOnRealExtraction(t *testing.T) {
	root := t.TempDir()
	var logs []string
	var mu sync.Mutex
	w := NewTaskWriter(TaskWriterDeps{
		ProjectRoot: root,
		Extractor: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return `{"worth_saving":true,"name":"hardcoded-fixture-path","description":"add memory export — fixture hardcodes an absolute path","pitfall":"the fixture hardcoded /abs/path","recovery":"patch the fixture before running the package test","result":"export landed","pivots":["swapped the fixture path"]}`,
				TaskExtractionUsage{InputTokens: 1200, OutputTokens: 90}, nil
		},
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	})
	if w == nil {
		t.Fatal("NewTaskWriter returned nil with a wired extractor")
	}
	w.OnSubTaskComplete(context.Background(), "general-purpose: add export", "some sub-agent output")

	taskDir := GetTaskMemPath(root)
	if got := countFilesIn(t, taskDir); got != 1 {
		t.Fatalf("expected 1 task memory, got %d", got)
	}
	data, err := os.ReadFile(filepath.Join(taskDir, "hardcoded-fixture-path.md"))
	if err != nil {
		t.Fatalf("read task memory: %v", err)
	}
	body := string(data)
	for _, want := range []string{"type: task", "## 踩坑与原因", "## 恢复路径", "## 结果", "转折点"} {
		if !strings.Contains(body, want) {
			t.Errorf("task memory missing %q:\n%s", want, body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "in=1200") || !strings.Contains(logs[0], "out=90") {
		t.Errorf("expected one token-accounting line, got %v", logs)
	}
}

func TestTaskWriterStaysSilentOnExtractorError(t *testing.T) {
	root := t.TempDir()
	w := NewTaskWriter(TaskWriterDeps{
		ProjectRoot: root,
		Extractor: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return "", TaskExtractionUsage{}, fmt.Errorf("side query unavailable")
		},
	})
	w.OnSubTaskComplete(context.Background(), "t", "output")
	if got := countFilesIn(t, GetTaskMemPath(root)); got != 0 {
		t.Errorf("failed extraction must not write anything, got %d files", got)
	}
}

func TestTaskWriterNilReceiverIsSafe(t *testing.T) {
	var w *TaskWriter
	w.OnSubTaskComplete(context.Background(), "t", "output") // must not panic
}

// --- 1.2 落盘后能被 1.4 召回看到 -------------------------------------------

func TestTaskMemoriesSurfaceInRecallScan(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, root, "hardcoded-fixture-path.md", "body")

	// The recall path scans the project memory ROOT (not tasks/ directly), so the only thing to
	// verify is that the recursive scan picks the staging file up and tags its type.
	headers, err := ScanMemoryFiles(context.Background(), GetAutoMemPath(root), "project")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var found *MemoryHeader
	for i := range headers {
		if headers[i].Type == TypeTask {
			found = &headers[i]
		}
	}
	if found == nil {
		t.Fatalf("task memory not surfaced by scan; got %+v", headers)
	}
	if found.Filename != TaskMemDirName+"/hardcoded-fixture-path.md" {
		t.Errorf("expected dir-relative filename, got %q", found.Filename)
	}
}

// --- 1.3 蒸馏 ---------------------------------------------------------------

func TestConsolidatorCreateThenDeletesSource(t *testing.T) {
	root := t.TempDir()
	src := writeTaskFile(t, root, "hardcoded-fixture-path.md", "the fixture hardcoded /abs/path")

	calls := 0
	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(_ context.Context, _, _ string) (string, TaskExtractionUsage, error) {
			calls++
			return `{"actions":[{"task_file":"tasks/hardcoded-fixture-path.md","action":"create","target_file":"fixture-paths.md","content":"---\nname: fixture-paths\ndescription: fixtures\ntype: project\n---\n\nDo not hardcode absolute paths in fixtures.\n"}]}`,
				TaskExtractionUsage{InputTokens: 3000, OutputTokens: 200}, nil
		},
	})
	if c == nil {
		t.Fatal("NewConsolidator returned nil with a wired fn")
	}
	if n := c.Run(context.Background()); n != 1 {
		t.Fatalf("consumed = %d, want 1", n)
	}
	if calls != 1 {
		t.Errorf("distiller called %d times, want 1", calls)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source task memory should be deleted after a successful create")
	}
	target := filepath.Join(GetAutoMemPath(root), "fixture-paths.md")
	if _, err := os.Stat(target); err != nil {
		t.Errorf("long-term memory not created: %v", err)
	}
	if got := countFilesIn(t, GetTaskMemPath(root)); got != 0 {
		t.Errorf("staging dir should be drained, got %d files", got)
	}
}

func TestConsolidatorSkipDropsSourceWithoutWriting(t *testing.T) {
	root := t.TempDir()
	src := writeTaskFile(t, root, "situation-specific.md", "just a fix recipe")

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return `{"actions":[{"task_file":"tasks/situation-specific.md","action":"skip"}]}`, TaskExtractionUsage{}, nil
		},
	})
	if n := c.Run(context.Background()); n != 1 {
		t.Fatalf("skip must consume the source, consumed = %d", n)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("skipped source should be deleted")
	}
	// skip is the whitelist enforcer — nothing durable may have been written.
	if got := countFilesIn(t, GetAutoMemPath(root)); got != 0 {
		t.Errorf("skip wrote %d long-term file(s), want 0", got)
	}
}

func TestConsolidatorUpdateAppendsInsteadOfOverwriting(t *testing.T) {
	root := t.TempDir()
	src := writeTaskFile(t, root, "more.md", "new pitfall")
	existing := filepath.Join(GetAutoMemPath(root), "known-issues.md")
	original := "---\nname: known-issues\ntype: project\n---\n\nPRE-EXISTING CONTENT\n"
	if err := os.MkdirAll(GetAutoMemPath(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return `{"actions":[{"task_file":"tasks/more.md","action":"update","target_file":"known-issues.md","content":"ADDED LINE"}]}`, TaskExtractionUsage{}, nil
		},
	})
	if n := c.Run(context.Background()); n != 1 {
		t.Fatalf("consumed = %d, want 1", n)
	}
	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "PRE-EXISTING CONTENT") {
		t.Error("update must not destroy content the distiller never saw")
	}
	if !strings.Contains(string(data), "ADDED LINE") {
		t.Error("update did not add the new content")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be deleted after a successful update")
	}
}

func TestConsolidatorUpdateOnMissingTargetKeepsSource(t *testing.T) {
	root := t.TempDir()
	src := writeTaskFile(t, root, "x.md", "body")

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			// Target does not exist — silently converting to create would hide the inconsistency.
			return `{"actions":[{"task_file":"tasks/x.md","action":"update","target_file":"ghost.md","content":"c"}]}`, TaskExtractionUsage{}, nil
		},
	})
	if n := c.Run(context.Background()); n != 0 {
		t.Errorf("consumed = %d, want 0 for a failed action", n)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("failed action must keep the source for the next round")
	}
}

func TestConsolidatorIgnoresHallucinatedTaskFile(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, root, "real.md", "body")

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return `{"actions":[{"task_file":"../../etc/passwd","action":"skip"},{"task_file":"tasks/real.md","action":"skip"}]}`, TaskExtractionUsage{}, nil
		},
	})
	if n := c.Run(context.Background()); n != 1 {
		t.Errorf("consumed = %d, want 1 (only the real file)", n)
	}
}

func TestConsolidatorKeepsSourcesOnUnparseableOutput(t *testing.T) {
	root := t.TempDir()
	src := writeTaskFile(t, root, "keepme.md", "the only record of a real pitfall")

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return "I'm not sure what to do with these.", TaskExtractionUsage{}, nil
		},
	})
	if n := c.Run(context.Background()); n != 0 {
		t.Errorf("consumed = %d, want 0", n)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("a parse failure must never delete the only record of a pitfall")
	}
}

func TestConsolidatorMergeCollapsesDuplicates(t *testing.T) {
	root := t.TempDir()
	src := writeTaskFile(t, root, "dup.md", "body")
	memDir := GetAutoMemPath(root)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.md", "b.md"} {
		if err := os.WriteFile(filepath.Join(memDir, f), []byte("---\ntype: project\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			return `{"actions":[{"task_file":"tasks/dup.md","action":"merge","target_file":"a.md","delete_files":["b.md","not-offered.md"],"content":"MERGED BODY"}]}`, TaskExtractionUsage{}, nil
		},
	})
	if n := c.Run(context.Background()); n != 1 {
		t.Fatalf("consumed = %d, want 1", n)
	}
	survivor, _ := os.ReadFile(filepath.Join(memDir, "a.md"))
	if !strings.Contains(string(survivor), "MERGED BODY") {
		t.Error("merge did not write the merged body")
	}
	if _, err := os.Stat(filepath.Join(memDir, "b.md")); !os.IsNotExist(err) {
		t.Error("listed duplicate should be deleted")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be deleted")
	}
}

func TestConsolidatorSingleFlightSkipsConcurrentRound(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, root, "a.md", "body")

	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var calls int32
	var mu sync.Mutex

	c := NewConsolidator(ConsolidatorDeps{
		ProjectRoot: root,
		ConsolidatorFn: func(context.Context, string, string) (string, TaskExtractionUsage, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			entered <- struct{}{}
			<-release
			return `{"actions":[{"task_file":"tasks/a.md","action":"skip"}]}`, TaskExtractionUsage{}, nil
		},
	})

	go c.Run(context.Background())
	<-entered // first round is now in flight and blocked

	if n := c.Run(context.Background()); n != 0 {
		t.Errorf("second concurrent round should be skipped, got consumed=%d", n)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("distiller ran %d times, want exactly 1 (single-flight)", calls)
	}
}

func TestConsolidatorNilReceiverIsSafe(t *testing.T) {
	var c *Consolidator
	if n := c.Run(context.Background()); n != 0 {
		t.Error("nil consolidator should be a no-op")
	}
	c.NotifyActivity()
	c.Stop()
}

// --- paths / 类型 ----------------------------------------------------------

func TestGetTaskMemPathAndSandboxExemption(t *testing.T) {
	root := t.TempDir()
	dir := GetTaskMemPath(root)
	if !strings.HasSuffix(dir, TaskMemDirName+string(os.PathSeparator)) {
		t.Errorf("expected trailing separator, got %q", dir)
	}
	if !strings.HasPrefix(dir, GetAutoMemPath(root)) {
		t.Error("task dir must live under the memory root")
	}
	// No separate sandbox exemption needed: IsAutoMemPath is a prefix test on the memory root.
	inside := filepath.Join(dir, "x.md")
	if !IsAutoMemPath(inside, root) {
		t.Error("task memory path must be sandbox-allowed via the memory root prefix")
	}
	if !IsTaskMemPath(inside, root) {
		t.Error("IsTaskMemPath should recognise a file inside tasks/")
	}
	if IsTaskMemPath(filepath.Join(GetAutoMemPath(root), "longterm.md"), root) {
		t.Error("a long-term memory is not a task memory")
	}
}

func TestParseMemoryTypeAcceptsTaskButModelTaxonomyDoesNot(t *testing.T) {
	if got, ok := ParseMemoryType("task"); !ok || got != TypeTask {
		t.Error("frontmatter type: task must parse")
	}
	for _, m := range MemoryTypes {
		if m == TypeTask {
			t.Error("TypeTask must stay out of the model-writable taxonomy")
		}
	}
}
