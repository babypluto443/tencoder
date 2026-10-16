// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 阶段一 · 任务 1.2：子任务完成后同步短期任务记忆。
//
// 设计锚点（见 01-阶段一-开发任务.md 决策 C/D）：
//   - 长期记忆存"世界的状态"，任务记忆存"过程中得到的教训"；
//   - task 记忆是暂存态——子 agent 收尾写入 → 空闲时蒸馏进长期记忆（task_consolidate.go）→ 源文件删除。
// 因此这里只做"写入暂存态"，不做任何合并/去重判断，那属于蒸馏环节。
//
// 提取走旁路轻量模型，且接口是注入式的（抄 find_relevant_memories.go 里 SelectorFn 的做法），
// 这样 memory 包不必在包级依赖 llm —— TUI 侧已有现成的 side-query client 构造代码可以复用。

// TaskExtractionUsage is the token accounting for one extraction call. Kept as a plain struct
// (not llm.UsageInfo) so the memory package stays free of a package-level llm dependency.
type TaskExtractionUsage struct {
	InputTokens  int
	OutputTokens int
}

// TaskExtractorFn is the side-query abstraction for the extraction LLM call. Same contract as
// SelectorFn: one-shot call, raw assistant text back, errors mean "extraction failed → write
// nothing". The usage return is what makes 验收标准 3 (每任务新增 token 开销) measurable.
type TaskExtractorFn func(ctx context.Context, systemPrompt, userMessage string) (string, TaskExtractionUsage, error)

// TaskExtractionSystemPrompt is the system prompt for task-memory extraction. It embeds
// WhatNotToSaveTaskSection (the four-item whitelist) verbatim so the whitelist has exactly one
// definition — the rules the design doc was written about and the rules the model actually sees.
const TaskExtractionSystemPrompt = `You extract task memories from a finished sub-agent run.

A task memory captures what was learned DURING the sub-task — the pitfalls hit and how they were recovered from — so a later session that revisits the same work does not pay for the same failure twice. It is NOT a log of what the sub-agent did.

` + WhatNotToSaveTaskSection + `

You will be given the sub-task's name/prompt and its final output. Judge whether the four-item whitelist has anything in it. Most runs have nothing worth keeping — an uneventful run is a normal outcome and you must return worth_saving=false rather than inventing content.

If and only if there is a real pitfall (something failed, something surprised the sub-agent, or the user stated a constraint that the code cannot show), return worth_saving=true and fill in:
- name: 2-4 words, lowercase, hyphen-separated. Names the PITFALL, not the task ("hardcoded-fixture-path", not "add-export").
- description: ONE line, must START with the task keyword(s) so keyword overlap works as the recall signal. State the pitfall in the same line: "add memory export — test fixture hardcodes an absolute path; patch it before running the package test".
- pitfall: what went wrong, WITH the cause (not just the symptom).
- recovery: the action sequence that unstuck it, phrased so it can be followed cold ("run the package test before the full suite", "check X before trying Y").
- result: one line on what the task ended up delivering, plus its impact on later work (hidden coupling like "changing A requires updating B"), or just the outcome if there is none.
- pivots: OPTIONAL, 1-3 items, only the moments that changed the outcome. Omit entirely if the work went straight through. Never a step-by-step transcript.

Respond with valid JSON only, no markdown, no commentary:
{"worth_saving": false}
or
{"worth_saving": true, "name": "...", "description": "...", "pitfall": "...", "recovery": "...", "result": "...", "pivots": ["..."]}`

// TaskWriterDeps holds the collaborators a TaskWriter needs. Constructed once at TUI startup.
type TaskWriterDeps struct {
	ProjectRoot string // absolute project root; task memories live under <root>/.tencoder/memory/tasks/
	Extractor   TaskExtractorFn
	// Logf is optional; used for the single-line token accounting log (阶段一 T6).
	Logf func(format string, args ...any)
}

// TaskWriter writes task memories when sub-agents finish. One instance per session; safe for
// concurrent use (it holds no mutable state of its own).
type TaskWriter struct {
	deps TaskWriterDeps
}

// NewTaskWriter constructs a TaskWriter. Returns nil when no extractor is wired — callers can
// then invoke OnSubTaskComplete unconditionally, and the nil receiver no-ops.
func NewTaskWriter(deps TaskWriterDeps) *TaskWriter {
	if deps.Extractor == nil || deps.ProjectRoot == "" {
		return nil
	}
	return &TaskWriter{deps: deps}
}

// OnSubTaskComplete is the completion hook. It performs the extraction inline (blocking on the
// side-query LLM) but is itself only ever reached from a fire-and-forget goroutine — see the
// TaskManager hook that calls it, which launches the goroutine so SetCompleted returns immediately.
//
// Failure policy, consistent with the rest of the memory subsystem ("失败即不召回" philosophy):
// every error path here is silent. A missing task memory costs a future session one rediscovery;
// a panicking or blocking write path would cost the user their conversation.
func (w *TaskWriter) OnSubTaskComplete(ctx context.Context, taskName, output string) {
	if w == nil {
		return
	}
	// A failed sub-agent's output is an error string, not a narrative — extracting from it
	// produces noise. The pitfall worth remembering is usually recorded by the sub-agent that
	// recovered from it, which does write a normal output.
	if strings.TrimSpace(output) == "" {
		return
	}

	dir := GetTaskMemPath(w.deps.ProjectRoot)
	if dir == "" {
		return
	}

	start := time.Now()
	raw, usage, err := w.deps.Extractor(ctx, TaskExtractionSystemPrompt,
		buildTaskExtractionMessage(taskName, output))
	elapsed := time.Since(start)
	if err != nil {
		w.logf("[task-memory] extract failed: err=%v dur=%s", err, elapsed.Round(time.Millisecond))
		return
	}

	rec, ok := parseTaskExtraction(raw)
	if !ok {
		// Unparseable or worth_saving=false. Not worth a log line — "nothing to save" is the
		// expected majority outcome and logging it every time would drown the token accounting.
		return
	}

	path, err := writeTaskMemoryFile(dir, rec)
	if err != nil {
		w.logf("[task-memory] write failed: name=%s err=%v", rec.Name, err)
		return
	}
	w.logf("[task-memory] saved: file=%s in=%d out=%d dur=%s",
		filepath.Base(path), usage.InputTokens, usage.OutputTokens, elapsed.Round(time.Millisecond))
}

// TaskRecord is one extracted task memory: the whitelist fields plus the rendered body.
// Name/Description land in frontmatter; the rest is body text.
type TaskRecord struct {
	Name        string
	Description string
	Pitfall     string
	Recovery    string
	Result      string
	Pivots      []string
}

// Body renders the record as markdown. The four-part structure from the design doc (goal →
// turning points → pitfalls + recovery → result) shows up here as: the goal is NOT repeated
// (it lives in the frontmatter description), pivots are the only action-sequence content kept,
// and pitfalls + recovery are the mandatory body.
func (r TaskRecord) Body() string {
	var b strings.Builder
	if len(r.Pivots) > 0 {
		b.WriteString("## 转折点\n\n")
		for _, p := range r.Pivots {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## 踩坑与原因\n\n%s\n\n", r.Pitfall)
	fmt.Fprintf(&b, "## 恢复路径\n\n%s\n\n", r.Recovery)
	fmt.Fprintf(&b, "## 结果\n\n%s\n", r.Result)
	return b.String()
}

// taskExtractionResponse mirrors the JSON contract in TaskExtractionSystemPrompt.
type taskExtractionResponse struct {
	// WorthSaving gates everything: false means "nothing durable here", which is the expected
	// answer for most sub-task runs.
	WorthSaving bool     `json:"worth_saving"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Pitfall     string   `json:"pitfall"`
	Recovery    string   `json:"recovery"`
	Result      string   `json:"result"`
	Pivots      []string `json:"pivots"`
}

var (
	slugCleaner = regexp.MustCompile(`[^a-z0-9]+`)
	// Control chars and newlines must not reach frontmatter — a description containing a newline
	// would silently truncate the field and produce a header the scanner can't read.
	descCleaner = regexp.MustCompile(`[\r\n\t]+`)
)

// parseTaskExtraction turns the model's raw reply into a TaskRecord. Returns ok=false for
// unparseable output and for worth_saving=false, which are the same thing to the caller: write
// nothing. A record missing the mandatory pitfall/recovery fields is rejected rather than
// half-written — a task memory without a recovery path is worse than no task memory, because
// recall would surface a warning the reader cannot act on.
func parseTaskExtraction(raw string) (TaskRecord, bool) {
	clean := extractJSONObject(raw)
	if clean == "" {
		return TaskRecord{}, false
	}
	var parsed taskExtractionResponse
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return TaskRecord{}, false
	}
	if !parsed.WorthSaving {
		return TaskRecord{}, false
	}

	desc := strings.TrimSpace(descCleaner.ReplaceAllString(parsed.Description, " "))
	pitfall := strings.TrimSpace(parsed.Pitfall)
	recovery := strings.TrimSpace(parsed.Recovery)
	name := slugCleaner.ReplaceAllString(strings.ToLower(strings.TrimSpace(parsed.Name)), "-")
	name = strings.Trim(name, "-")

	if desc == "" || pitfall == "" || recovery == "" || name == "" {
		return TaskRecord{}, false
	}

	var pivots []string
	for _, p := range parsed.Pivots {
		if p = strings.TrimSpace(p); p != "" {
			pivots = append(pivots, p)
		}
	}
	if len(pivots) > 3 {
		pivots = pivots[:3]
	}

	return TaskRecord{
		Name:        name,
		Description: truncateRunes(desc, 300),
		Pitfall:     pitfall,
		Recovery:    recovery,
		Result:      strings.TrimSpace(parsed.Result),
		Pivots:      pivots,
	}, true
}

// writeTaskMemoryFile writes one task memory into dir, creating the directory first.
// Filenames are content-derived (the pitfall name) rather than task-ID-derived, so the filename
// alone is informative when grepping the staging dir by hand. Collisions get a numeric suffix.
func writeTaskMemoryFile(dir string, rec TaskRecord) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("empty task memory dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := uniqueTaskMemoryPath(dir, rec.Name)
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\ntype: %s\n---\n\n%s",
		rec.Name, rec.Description, TypeTask, rec.Body())
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func uniqueTaskMemoryPath(dir, name string) string {
	candidate := filepath.Join(dir, name+".md")
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}
	for i := 2; i < 100; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d.md", name, i))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	// Pathological collision count; fall back to something guaranteed distinct.
	return filepath.Join(dir, fmt.Sprintf("%s-%d.md", name, time.Now().UnixNano()))
}

// MaxTaskExtractionOutputBytes caps how much of a sub-agent's final output is fed to the
// extractor. Sub-agent output is unbounded (200 turns of streaming text), and the pitfall — if
// there is one — is virtually always stated in the concluding stretch where the agent reports
// what it found. 24KB of tail keeps the call's input tokens bounded and predictable.
const MaxTaskExtractionOutputBytes = 24 * 1024

// buildTaskExtractionMessage assembles the extractor input: the sub-task's name plus the tail of
// its output. The name matters because it carries the task keyword the description must lead with.
func buildTaskExtractionMessage(taskName, output string) string {
	tail := output
	if len(tail) > MaxTaskExtractionOutputBytes {
		tail = tail[len(tail)-MaxTaskExtractionOutputBytes:]
	}
	return fmt.Sprintf("Sub-task name: %s\n\nSub-agent final output:\n%s", taskName, tail)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (w *TaskWriter) logf(format string, args ...any) {
	if w.deps.Logf != nil {
		w.deps.Logf(format, args...)
	}
}
