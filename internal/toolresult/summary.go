// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package toolresult

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 阶段三 · 任务 3.2：工具结果摘要中间层。
//
// 现状（pre-阶段三）：超预算的 tool_result 被 spill 到 `.tencoder/tool_results/`，
// 上下文里留 **前 2KB 机械预览**（`buildSpillPreview`）。
// 机械预览的问题是它**不知道你要干什么**——一个 200KB 的测试失败日志，
// 前 2KB 可能正好是启动横幅，而真正的断言失败在最后一行。
//
// 这里的做法是**不新增一层，而是升级 preview 位**：
// 全文照旧落盘（可 ReadFile 回读），但上下文里放的是"知道任务目标的结构化摘要"。
//
// 三条不变式：
//  1. **冻结**：摘要随决策一起进 ContentReplacementState 逐字节回放，prompt cache 前缀稳定。
//     这和机械预览走的是同一条冻结路径，不是两套机制。
//  2. **失败回退**：摘要调用失败 → 原样回退机械 preview，行为不劣于现状（阶段三原则：
//     全链路失败回退只会更好不会更坏）。
//  3. **同步**：异步来不及——摘要必须赶在下一轮 prompt 组装前进上下文。
//     但 spill 本身是罕见事件（50KB 阈值挡着），单次 1~3s 可接受。
//
// 注入式接口，与阶段一 TaskExtractorFn / 阶段二 ReflectionJudgeFn 同款模式：
// toolresult 不在包级依赖 llm。

// SummaryUsage is the token accounting for one summarization call.
type SummaryUsage struct {
	InputTokens  int
	OutputTokens int
}

// SummarizerFn is the side-query call that turns an oversized tool result into a structured
// summary. Same contract as the other injected side-query fns: one-shot, system-prompt-bound,
// returns raw assistant text.
//
// Returning an error means "fall back to the mechanical preview" — never "give up".
type SummarizerFn func(ctx context.Context, systemPrompt, userMessage string) (string, SummaryUsage, error)

// SummaryStats is the per-call accounting for 验收标准 2 (开销可见且可控).
type SummaryStats struct {
	Calls        int
	Succeeded    int
	FellBack     int // summarizer failed → mechanical preview used instead
	InputTokens  int
	OutputTokens int
	TotalDur     time.Duration
	LastDur      time.Duration
}

// Summarizer owns the preview-upgrade path. One instance per conversation thread, threaded
// through Apply like ContentReplacementState.
//
// Nil-safe: a nil Summarizer means "no side query configured", and every method degrades to the
// existing mechanical preview. That is what makes this feature safe to leave unwired.
type Summarizer struct {
	fn      SummarizerFn
	timeout time.Duration
	stats   SummaryStats
}

// NewSummarizer constructs a Summarizer. Returns nil when fn is nil so callers can hold a nil
// *Summarizer and skip every nil check.
func NewSummarizer(fn SummarizerFn, timeout time.Duration) *Summarizer {
	if fn == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Summarizer{fn: fn, timeout: timeout}
}

// Enabled reports whether summarization is active.
func (s *Summarizer) Enabled() bool { return s != nil && s.fn != nil }

// Stats returns a copy of the accounting counters.
func (s *Summarizer) Stats() SummaryStats {
	if s == nil {
		return SummaryStats{}
	}
	return s.stats
}

// maxSummaryInputBytes caps how much of a spilled result is fed to the summarizer.
// Same reasoning as the reflection trace cap: 200KB of raw text would cost more than the entire
// main-agent turn we're trying to protect. The tail is dropped first because in log-shaped output
// the conclusion (the actual error) lives at the end.
const maxSummaryInputBytes = 24 * 1024

// SummarySystemPrompt is the summarizer instruction.
//
// Two constraints that matter more than prose quality:
//   - **numbers and error text verbatim** — a summary that paraphrases "expected 3, got 4" into
//     "a count mismatch" has destroyed the only part the agent can act on;
//   - **no speculation** — the summarizer does not know the task, so any inference it adds is
//     hallucination wearing a useful hat.
const SummarySystemPrompt = `You compress a large tool output into a short, faithful report for an AI agent that is mid-task and will never see the full text again (it can re-read the original from disk if it truly needs to).

Preserve, in this order of priority:
1. Every number, identifier, file path, symbol name, and error message, EXACTLY as written.
2. The conclusion: what failed, what was found, what the output proves.
3. Counts and totals (lines, rows, matches, bytes).
4. Any instruction, warning, or hint aimed at the caller.

Rules:
- Never paraphrase an error message or a value. Copy it verbatim.
- Never infer what the output "probably means". You cannot see the task. If the meaning is
  ambiguous, report the raw facts and say it is ambiguous.
- Do not add advice, next steps, or conclusions that are not literally in the output.
- Drop repetition and boilerplate (repeated headers, progress bars, timestamps per line).
- If the output is itself a summary or trivial, say so in one line rather than padding.

Respond with the summary as plain markdown text. No preamble, no "Here is the summary".`

// buildSummaryInput assembles the summarizer input: the task goal (so relevance can be judged),
// the tool that produced it, and the (tail-capped) content.
func buildSummaryInput(toolName string, taskGoal, content string) string {
	var b strings.Builder
	b.WriteString("## Tool that produced this output\n")
	b.WriteString(toolName)
	b.WriteString("\n\n## Task the agent is working on\n")
	if taskGoal != "" {
		b.WriteString(taskGoal)
	} else {
		b.WriteString("(unknown — no task goal available)")
	}
	b.WriteString("\n\n## Tool output (tail may be truncated)\n")
	b.WriteString(content)
	b.WriteString("\n\nSummarize the output above. Everything the agent still needs from it must survive; everything else may go.")
	return b.String()
}

// tailExcerpt keeps the END of an oversized body.
//
// Deliberate, and worth stating: for log- and diff-shaped output the conclusion is at the end
// (the panic, the failed assertion, the summary line), while the beginning is startup banners and
// config dumps. Head-truncating — which is what buildSpillPreview does — throws away the part
// that matters.
func tailExcerpt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "...[middle truncated — full text is on disk]...\n" + s[len(s)-max:]
}

// summarize tries the side query and returns a replacement preview, or ("", false) when the
// caller should fall back to the mechanical preview.
//
// Every failure path returns false rather than an error: spilling already succeeded, the full
// text is safely on disk, and a mechanical preview is a perfectly good outcome. Making this
// fallible-but-safe is exactly the "回退只会更好不会更坏" principle.
func (s *Summarizer) summarize(ctx context.Context, toolName, taskGoal, content, spillPath string) (string, SummaryUsage, bool) {
	if !s.Enabled() {
		return "", SummaryUsage{}, false
	}

	usage := SummaryUsage{}
	start := time.Now()

	// Respect the caller's cancellation. Apply runs inside the agent loop, so a cancelled turn
	// must not be held open by a summary call.
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	raw, usage, err := s.fn(callCtx, SummarySystemPrompt, buildSummaryInput(toolName, taskGoal, tailExcerpt(content, maxSummaryInputBytes)))
	elapsed := time.Since(start)

	s.stats.Calls++
	s.stats.TotalDur += elapsed
	s.stats.LastDur = elapsed
	if err == nil {
		s.stats.InputTokens += usage.InputTokens
		s.stats.OutputTokens += usage.OutputTokens
	}

	summary := sanitizeSummary(raw)
	if err != nil || summary == "" {
		s.stats.FellBack++
		return "", usage, false
	}

	s.stats.Succeeded++
	return buildSummaryPreview(summary, spillPath, len(content)), usage, true
}

// sanitizeSummary strips the wrappers a chat model reflexively adds, so the agent sees the content
// rather than the model's conversational habits.
func sanitizeSummary(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Markdown fences, possibly with a language tag.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 && !strings.ContainsAny(s[:i], " \t") {
			s = s[i+1:] // drop the language tag line
		} else {
			s = strings.TrimPrefix(s, "\n")
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(s)
	// Strip a leading "Here is/Here's the summary:" preamble if present.
	//
	// The colon is removed with TrimPrefix, NOT with a TrimLeft over ":-— \n": a TrimLeft that
	// includes '-' would eat the leading dash of the first bullet in "- item\n- item", silently
	// mangling a perfectly good markdown list into "item\n- item".
	for _, p := range []string{"here is the summary", "here's the summary", "here is a summary", "summary", "compressed output"} {
		if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
			s = strings.TrimSpace(s[len(p):])
			s = strings.TrimPrefix(s, ":")
			s = strings.TrimSpace(s)
			break
		}
	}
	return strings.TrimSpace(s)
}

// summaryTagPrefix marks a summarized replacement. It must mirror persistedTagPrefix's role: a
// marker that isAlreadyReplaced recognises, so a summary restored from a session log is frozen as
// itself rather than re-summarized on the next turn.
const summaryTagPrefix = "<summarized-output>"

// isSummarizedReplacement reports whether content is already a summary replacement.
func isSummarizedReplacement(s string) bool {
	return strings.HasPrefix(s, summaryTagPrefix)
}

// buildSummaryPreview renders the summary block that replaces the tool result in context.
//
// Structure is fixed on purpose: the agent must be able to tell *without reading the whole thing*
// that there is more on disk and how to get it. Keeping "full text at <path>" adjacent to the
// summary is what makes hallucinated-detail recovery possible — a model that needs to double
// check a number has an explicit route to do so.
func buildSummaryPreview(summary, spillPath string, originalLen int) string {
	var b strings.Builder
	b.WriteString(summaryTagPrefix)
	b.WriteString("\n")
	// The summary BODY goes first — it is the payload, everything else is metadata about it.
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n")
	fmt.Fprintf(&b, "(compressed from %dKB; full output saved to: %s)\n", originalLen/1024, spillPath)
	b.WriteString("Re-read the file above with ReadFile if an exact value matters.\n")
	b.WriteString("</summarized-output>")
	return b.String()
}
