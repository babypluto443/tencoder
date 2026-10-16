// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package teams

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"tencoder/internal/tools"
)

// 阶段四 · 任务 4.1：跨 Agent 进度接口。
//
// ── 为什么是"查询模型"而不是"事件注入" ──
//
// 一个显然的做法是：每次子 agent 调用工具，就把进度事件塞进 lead 的上下文。
// 这会让 lead 的上下文被进度噪声淹没——几十个子 agent 步就是几十条
// "X is editing foo.go"，而 lead 真正需要的对话内容被挤到 cache 之外。
// 这恰好是"而非全文粘贴"初衷的反面。
//
// 所以这里做成**查询**：lead 平时安静（完成事件走已有的 mailbox 通知），
// 什么时候关心进度、关心到什么粒度，由它自己决定。上下文里只多一条它主动查的结果。
//
// 数据源全部复用现有的 TeammateProgress + TaskManager，**零新状态**——
// 这是这个工具能很快落地的原因，也是它不会引入新的不一致来源的原因。

// GetTeamProgressTool returns a structured snapshot of running teammates.
//
// Zero new state: it reads TeammateProgress (already maintained for the TUI) and TaskManager
// (already tracking background tasks). If it kept its own copy of that data it would be a third
// source of truth that could disagree with the other two.
type GetTeamProgressTool struct {
	TeamMgr *TeamManager
	// TaskMgr, when set, adds background sub-agent tasks to the report — those are spawned via
	// AgentTool rather than Team, so they have no TeammateProgress.
	TaskMgr TaskLister
	// Requester is the calling agent's name, used to label the report.
	Requester string
}

// TaskLister is the slice of agents.TaskManager this tool needs.
//
// Declared as an interface so internal/teams does not import internal/agents — agents already
// imports teams (AgentTool has a TeamMgr field), so a concrete dependency here would be a cycle.
type TaskLister interface {
	// ListProgress returns a snapshot of tracked tasks for reporting.
	ListProgress() []TaskProgress
}

// TaskProgress is one background task's public progress.
type TaskProgress struct {
	ID      string
	Name    string
	Status  string
	Elapsed time.Duration
	Output  string
}

func (t *GetTeamProgressTool) Name() string                 { return "GetTeamProgress" }
func (t *GetTeamProgressTool) Category() tools.ToolCategory { return tools.CategoryRead }

// Read category, deliberately: this tool changes nothing. It must not trigger a write approval
// prompt, and it must not appear in the audit log — a lead polling for progress should be free.
func (t *GetTeamProgressTool) Description() string {
	return `Get a structured progress snapshot of every agent you have running.

Returns for each teammate: name, status (running/idle/completed/failed), what it is doing right now, how many tool calls it has made, how long it has been running, and its token usage.

Call this when you need to decide what to do next — e.g. before starting dependent work on a teammate's output, or when deciding whether to wait or take over. Do NOT call it every turn: teammates report completion on their own, and polling wastes context.`
}

func (t *GetTeamProgressTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *GetTeamProgressTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	if t.TeamMgr == nil {
		return tools.ToolResult{Output: "Error: no team manager configured", IsError: true}
	}

	progress := t.TeamMgr.GetAllTeammateProgress()
	tasks := []TaskProgress{}
	if t.TaskMgr != nil {
		tasks = t.TaskMgr.ListProgress()
	}

	if len(progress) == 0 && len(tasks) == 0 {
		return tools.ToolResult{Output: "No agents are running. You are working alone."}
	}

	var b strings.Builder
	if t.Requester != "" {
		fmt.Fprintf(&b, "Progress snapshot for %s\n", t.Requester)
	}

	if len(progress) > 0 {
		b.WriteString("\nTeammates:\n")
		// Sort for a stable report: repeated calls shouldn't reorder rows and make the lead think
		// something changed when only the display order did.
		sort.Slice(progress, func(i, j int) bool { return progress[i].Name < progress[j].Name })
		for _, p := range progress {
			writeTeammateLine(&b, p)
		}
	}

	if len(tasks) > 0 {
		b.WriteString("\nBackground tasks:\n")
		for _, tp := range tasks {
			line := fmt.Sprintf("- %s [%s]", tp.Name, tp.Status)
			if tp.Elapsed > 0 {
				line += fmt.Sprintf(" elapsed=%s", tp.Elapsed.Round(time.Second))
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\nTeammates report completion on their own. Call this again only when you need to make a decision that depends on their state.")
	return tools.ToolResult{Output: b.String()}
}

func writeTeammateLine(b *strings.Builder, p *TeammateProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Fprintf(b, "- %s [%s]", p.Name, p.Status)
	if p.StartTime > 0 {
		fmt.Fprintf(b, " running=%s", time.Since(time.UnixMilli(p.StartTime)).Round(time.Second))
	}
	if p.ToolUseCount > 0 {
		fmt.Fprintf(b, " tools=%d", p.ToolUseCount)
	}
	if p.TokenCount > 0 {
		fmt.Fprintf(b, " tokens=%d", p.TokenCount)
	}
	if p.LastActivity != nil {
		fmt.Fprintf(b, "\n    now: %s", p.LastActivity.Description)
	}
	if len(p.RecentActivities) > 1 {
		b.WriteString("\n    recent:")
		// Skip the newest — it is already reported as "now".
		for _, a := range p.RecentActivities[:len(p.RecentActivities)-1] {
			fmt.Fprintf(b, "\n      - %s", a.Description)
		}
	}
	b.WriteString("\n")
}
