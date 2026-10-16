// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"tencoder/internal/compact"
	"tencoder/internal/conversation"
	"tencoder/internal/filehistory"
	"tencoder/internal/hooks"
	"tencoder/internal/llm"
	"tencoder/internal/permissions"
	"tencoder/internal/planfile"
	"tencoder/internal/prompt"
	"tencoder/internal/toolresult"
	"tencoder/internal/tools"
)

const (
	maxTokensCeiling          = 64000
	maxOutputTokensRecoveries = 3
)

type Agent struct {
	Client        llm.Client
	Registry      *tools.Registry
	Protocol      string
	WorkDir       string
	MaxIterations int
	ContextWindow int
	// MaxOutputTokens is the model's max output budget; used by Layer 2 to
	// compute the effective window for the compaction threshold. Zero falls
	// back to the summaryOutputReserve default inside compact.
	MaxOutputTokens int
	Checker         *permissions.Checker
	Hooks           *hooks.Engine
	// SessionID identifies the on-disk session log this agent appends to. When
	// set, Layer 2 compaction writes a compact_boundary record into that session
	// so a later resume can rebuild the compacted state instead of replaying the
	// full pre-compaction transcript. Empty disables boundary persistence (tests,
	// one-shot callers).
	SessionID      string
	NotificationFn func() []string
	// ToolNameFilter, when non-nil, drops any tool whose Name returns false from the schemas sent to
	// the LLM. The filter is consulted at the top of every iteration so callers can flip Coordinator
	// Mode on or off (e.g., when a team is created/torn down) without restarting the agent.
	Instructions  string
	MemoryContent string
	// MemoryRecallCh 非阻塞 memory recall：prefetch 与主 LLM 调用并行，
	// 工具执行后从 channel 读取并注入（与 Claude Code 一致）
	MemoryRecallCh <-chan string
	ToolNameFilter func(name string) bool
	// OnLoopComplete, when non-nil, is invoked fire-and-forget after the agent reaches LoopComplete
	// (final assistant message, no tool calls remaining). Used by ch09 background memory extraction.
	// Replaces the original stopHooks dispatcher; failures are silent and must not block the main
	// loop. The callback receives the live conversation — do not mutate it from another goroutine.
	OnLoopComplete func(conv *conversation.Manager)
	FileHistory    *filehistory.History
	// Reflector is the 阶段二 goal-drift reflection subsystem: a third-party side judge that
	// checks "is this path still aimed at the goal?" at the four high-risk moments
	// (localization done / patch generated / tool failure / anomaly recovery).
	//
	// Nil is the normal disabled state — every call site goes through nil-safe methods, so an
	// agent without a Reflector behaves exactly as before 阶段二. Kept as an opaque field (rather
	// than the reflection config) because the main loop only needs Fire/Consume/ObserveTools.
	Reflector *Reflector
	// Summarizer is the 阶段三 tool-result summary middleware. When non-nil, oversized tool
	// results get an LLM-written structured summary in place of the mechanical head-truncated
	// preview; the full text still lands on disk and remains readable. Nil = mechanical preview
	// (the pre-阶段三 behaviour), which is the default.
	Summarizer *toolresult.Summarizer
	// taskGoal is the user's original objective, captured once at Run() start. Set once and read
	// many: the goal must not drift mid-run even if the user rewords the request later.
	taskGoal        string
	compactTracking compact.AutoCompactTrackingState
	// ReplacementState carries the per-conversation-thread tool-result
	// decision log across iterations. Lazily created on first Run; forks
	// inherit a Clone() so they share the parent's frozen decisions but do
	// not write back into the parent's maps. Exposed so external code (e.g.
	// AgentTool fork-spawn) can pre-populate from a parent state.
	ReplacementState *toolresult.ContentReplacementState
	// RecoveryState holds the snapshots needed to rebuild working context
	// after Layer 2 collapses the conversation into a summary: most-recent
	// file reads and skill invocations. The struct is concurrency-safe so
	// the streaming executor can write to it from multiple goroutines.
	RecoveryState *compact.RecoveryState
	eventCh       chan AgentEvent
	// activeSkills tracks which Skill SOPs have been activated in this session (name → body).
	// Used by /skills to show what's active and by RecoveryState to survive compaction.
	// The body is injected once into the conversation as a message — NOT re-injected every turn.
	activeSkills map[string]string
}

// ActivateSkill records a skill activation. The body is kept for /skills listing and compaction
// recovery, but is NOT re-injected every turn — it lives in the conversation as a regular message.
func (a *Agent) ActivateSkill(name, body string) {
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]string)
	}
	a.activeSkills[name] = body
}

// ClearActiveSkills drops every pinned SOP. Called by /clear so a fresh conversation doesn't carry
// over SOPs from a prior task. Safe to call when no skills were ever activated.
func (a *Agent) ClearActiveSkills() {
	a.activeSkills = nil
}

// GetActiveSkills returns a copy of the currently-pinned SOPs (name → body). Used by tests and by
// /skills to surface what's active.
func (a *Agent) GetActiveSkills() map[string]string {
	out := make(map[string]string, len(a.activeSkills))
	for k, v := range a.activeSkills {
		out[k] = v
	}
	return out
}

// SetToolFilter installs a tool visibility filter for the current conversation. The filter is
// consulted at the top of every iteration so callers can flip Coordinator mode on or off without
// restarting the agent. Passing nil clears any previous filter.
func (a *Agent) SetToolFilter(allow func(name string) bool) {
	a.ToolNameFilter = allow
}

// ToolRegistry returns the live tool registry. Named ToolRegistry (not just Registry, even though
// that would match the field name) to avoid the method/field collision Go disallows. Matches the
// skills.SkillHost contract.
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.Registry
}

func New(client llm.Client, registry *tools.Registry, protocol string) *Agent {
	wd, _ := os.Getwd()
	return &Agent{
		Client:           client,
		Registry:         registry,
		Protocol:         protocol,
		WorkDir:          wd,
		MaxIterations:    0,
		ContextWindow:    200000,
		ReplacementState: toolresult.New(),
		RecoveryState:    compact.NewRecoveryState(),
	}
}

// SetSessionID wires the on-disk session log id onto the agent so Layer 2
// compaction can persist a compact_boundary record into the same session the TUI
// is appending plain messages to. Called from the TUI right after the agent is
// constructed (and again after a resume switches sessions).
func (a *Agent) SetSessionID(id string) { a.SessionID = id }

// currentToolSchemas builds the schema list the next API call will use,
// honouring any active ToolNameFilter (e.g. Teams coordinator mode).
// Shared between the recovery attachment (which lists what's still
// available after compact) and the actual Stream call so both views
// stay consistent.
// TaskGoal returns the user's original objective, or "" if none could be located.
// Used by the 阶段三 summarizer to know which parts of an oversized tool result matter.
//
// The extraction rules are the same ones the 阶段二 reflection node uses — skip
// <system-reminder> wrappers and the auto-memory header, take the first real user message —
// because "the task goal" must mean the same thing to both subsystems. Implemented here rather
// than exported from reflection.go so the agent package owns the definition and toolresult never
// needs to know where it came from.
func (a *Agent) TaskGoal() string {
	return a.taskGoal
}

// extractTaskGoal finds the task objective in a conversation. Shared with the reflection node's
// goal capture so both subsystems agree on what "the goal" is.
func extractTaskGoal(messages []conversation.Message) string {
	for _, m := range messages {
		if m.Role != "user" || len(m.ToolResults) > 0 || m.Content == "" {
			continue
		}
		c := m.Content
		if strings.HasPrefix(c, "<system-reminder>") {
			continue
		}
		if strings.HasPrefix(c, "# autoMemory") || strings.HasPrefix(c, "# tencoderMd") {
			continue
		}
		return c
	}
	return ""
}

func (a *Agent) currentToolSchemas() []map[string]any {
	schemas := a.Registry.GetAllSchemas(a.Protocol)
	if a.ToolNameFilter == nil {
		return schemas
	}
	allow := func(name string) bool {
		if t := a.Registry.Get(name); t != nil && tools.IsSystemTool(t) {
			return true
		}
		return a.ToolNameFilter(name)
	}
	return filterSchemasByName(schemas, allow)
}

func (a *Agent) Run(ctx context.Context, conv *conversation.Manager) <-chan AgentEvent {
	ch := make(chan AgentEvent, 32)

	go func() {
		defer close(ch)
		defer a.emitHook(hooks.EventSessionEnd, "", nil)

		a.emitHook(hooks.EventSessionStart, "", nil)

		conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)

		// 阶段三: capture the task goal once, right after the auto-memory header is prepended
		// (so the header itself can't be mistaken for the goal). The 阶段三 summarizer uses this
		// to judge which parts of an oversized tool result are worth keeping.
		a.taskGoal = extractTaskGoal(conv.GetMessages())

		var totalInput, totalOutput int
		consecutiveUnknown := 0
		maxTokensEscalated := false
		outputRecoveries := 0
		// usageAnchor pins the last real API usage to the conversation length at
		// the moment it was reported, so the compaction check can use
		// baseline + incremental estimate instead of estimating every message.
		// Zero-value (HasUsage false) on the first iteration → full estimate fallback.
		var usageAnchor compact.UsageAnchor

		for iteration := 1; ; iteration++ {
			if a.MaxIterations > 0 && iteration > a.MaxIterations {
				ch <- ErrorEvent{Message: fmt.Sprintf("Agent reached maximum iterations (%d)", a.MaxIterations)}
				return
			}

			if ctx.Err() != nil {
				return
			}

			// 阶段二: consume the previous turn's reflection verdict BEFORE this turn's request
			// goes out. Placement is load-bearing in two ways: the injection still reaches the
			// LLM (system reminders added after the request is sent are invisible), and the judge
			// got the full idle window (one whole turn) to answer. Waits at most MaxWait; on
			// timeout the verdict is discarded and the K window is not charged for it.
			a.Reflector.Consume(conv)

			// 阶段三 3.1: eviction check, placed BEFORE currentToolSchemas so the schemas sent
			// this turn are already the trimmed set.
			//
			// Checked every iteration but ACTS only when the resident count crosses the high
			// watermark — that asymmetry is the entire design. Tool schemas sit at the prompt
			// PREFIX, so changing them every turn would invalidate the prompt cache every turn,
			// and cache hits are worth far more than the few KB of schemas being trimmed. A
			// hundred-tool session triggers this two or three times in total, which amortizes to
			// nothing.
			if ev := a.Registry.MaybeEvict(); ev != nil && ev.Evicted > 0 {
				ch <- ToolEvictionEvent{
					Evicted:       ev.Evicted,
					Remaining:     ev.Remaining,
					SkippedActive: ev.SkippedActive,
					Names:         ev.Names,
				}
				// Tell the model explicitly. Without this it would see tools vanish from the list
				// and might conclude they don't exist; with it, the deferred-name reminder
				// injected further below re-lists them and the model knows to ToolSearch again.
				conv.AddSystemReminder(fmt.Sprintf(
					"Tool context trimmed: %d deferred tool schema(s) were unloaded from the tools list to keep the context small (still %d resident). They are NOT gone — reload them with ToolSearch if you need them. Unloaded: %s",
					ev.Evicted, ev.Remaining, strings.Join(ev.Names, ", ")))
			}

			// Compute the tool schema list once per iteration so the recovery
			// attachment (when compact fires) and the actual Stream call below
			// agree on what's wired up. Skill filters can only change between
			// iterations, never within one.
			toolSchemas := a.currentToolSchemas()

			// Plan mode: inject structured workflow reminder.
			if a.Checker != nil && a.Checker.Mode == permissions.ModePlan {
				planPath := planfile.GetOrCreatePlanPath(a.WorkDir)
				a.Checker.PlanFilePath = planPath
				planExists := planfile.PlanExists(a.WorkDir)
				reminder := prompt.BuildPlanModeReminder(planPath, planExists, iteration)
				conv.AddSystemReminder(reminder)
			}

			if a.NotificationFn != nil {
				for _, note := range a.NotificationFn() {
					conv.AddSystemReminder(note)
				}
			}

			a.emitHook(hooks.EventTurnStart, "", nil)

			// Inject deferred tool names into system-reminder so the model knows what's available via
			// ToolSearch.
			if deferredNames := a.Registry.GetDeferredToolNames(); len(deferredNames) > 0 {
				reminder := "The following deferred tools are available via ToolSearch. Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" + strings.Join(deferredNames, "\n")
				conv.AddSystemReminder(reminder)
			}

			a.emitHook(hooks.EventPreSend, "", nil)

			// Layer 1: apply tool-result budget（就地修改 conv）
			// system reminders 已注入，直接在 conv 上替换超限内容
			// 阶段三 3.2：Summarizer 非 nil 时，超预算结果的「preview 位」由机械截断升级为
			// 旁路 LLM 结构化摘要；Summarizer 为 nil（默认）时行为与阶段二之前完全一致。
			newRecords, _ := toolresult.ApplyWithSummary(conv, a.WorkDir, a.ReplacementState, a.Summarizer, a.TaskGoal())
			if len(newRecords) > 0 {
				_ = toolresult.AppendRecords(a.WorkDir, newRecords)
			}

			// Layer 2: auto-compact
			// conv 已经过 Layer 1 裁剪，直接用其消息估算 token
			if msg, err := compact.ManageContext(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.MaxOutputTokens, &a.compactTracking, a.RecoveryState, toolSchemas, usageAnchor, nil); err == nil && msg != "" {
				ch <- CompactEvent{Message: msg}
				usageAnchor = compact.UsageAnchor{}
				conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)
				// 压缩后 conv 已变，重新应用 Layer 1 budget
				toolresult.ApplyWithSummary(conv, a.WorkDir, a.ReplacementState, a.Summarizer, a.TaskGoal()) //nolint:errcheck
			}

			events, errs := a.Client.Stream(ctx, conv, toolSchemas)

			var text string
			var toolCalls []llm.ToolCallComplete
			var thinkingBlocks []conversation.ThinkingBlock
			var stopReason string
			var usage llm.UsageInfo

			executor := NewStreamingExecutor(a.Registry, a.Checker, ch)

			for ev := range events {
				switch e := ev.(type) {
				case llm.ThinkingDelta:
					ch <- ThinkingText{Text: e.Text}
				case llm.ThinkingComplete:
					thinkingBlocks = append(thinkingBlocks, conversation.ThinkingBlock{
						Thinking:  e.Thinking,
						Signature: e.Signature,
					})
				case llm.TextDelta:
					text += e.Text
					ch <- StreamText{Text: e.Text}
				case llm.ToolCallStart:
					ch <- ToolUseEvent{ToolID: e.ToolID, ToolName: e.ToolName}
				case llm.ToolCallDelta:
					// ignore
				case llm.ToolCallComplete:
					toolCalls = append(toolCalls, e)
					ch <- ToolUseEvent{
						ToolID:   e.ToolID,
						ToolName: e.ToolName,
						Args:     e.Arguments,
					}
					// Start executing immediately while LLM continues streaming.
					executor.Submit(ctx, a, e)
				case llm.StreamEnd:
					stopReason = e.StopReason
					usage = e.Usage
				}
			}
			a.emitHook(hooks.EventPostReceive, text, nil)

			// Handle stream errors.
			select {
			case err := <-errs:
				if err != nil {
					if retry, compacted := a.handleStreamError(ctx, ch, conv, err); retry {
						if compacted {
							// ForceCompact rewrote the conversation; the prior
							// anchor's AnchorCount no longer maps to it. Drop it
							// so the next check falls back to a full estimate.
							usageAnchor = compact.UsageAnchor{}
							conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)
						}
						continue // retry the turn
					}
					ch <- ErrorEvent{Message: err.Error()}
					return
				}
			default:
			}

			totalInput += usage.InputTokens
			totalOutput += usage.OutputTokens
			ch <- UsageEvent{InputTokens: totalInput, OutputTokens: totalOutput}

			// Real-usage baseline for this turn: input + cache_read +
			// cache_creation + output. Covers every message that existed when the
			// prompt was sent plus the assistant message about to be appended.
			// anchorBaseline is only adopted as the anchor once that assistant
			// message lands (see anchorAfterAssistant), so anchorCount lines up
			// with the messages the baseline accounts for. Zero usage (compat
			// endpoints that don't report it) leaves the prior anchor in place.
			anchorBaseline := compact.BaselineFromUsage(usage)
			anchorAfterAssistant := func() {
				if anchorBaseline > 0 {
					usageAnchor = compact.UsageAnchor{
						BaselineTokens: anchorBaseline,
						AnchorCount:    conv.Len(),
						HasUsage:       true,
					}
				}
			}

			// Handle max_tokens stop reason.
			if stopReason == "max_tokens" {
				if !maxTokensEscalated {
					// First hit: escalate silently.
					if setter, ok := a.Client.(llm.MaxTokensSetter); ok {
						setter.SetMaxOutputTokens(maxTokensCeiling)
						maxTokensEscalated = true
					}
					if text != "" {
						conv.AddAssistantFull(text, thinkingBlocks, nil)
						anchorAfterAssistant()
						conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Do not apologize or repeat previous content. Pick up mid-thought if needed.")
					}
					ch <- RetryEvent{Reason: "max_tokens escalation", Wait: 0}
					a.Reflector.NotifyAnomalyCallback()
					continue
				} else if outputRecoveries < maxOutputTokensRecoveries {
					// Multi-turn recovery.
					outputRecoveries++
					conv.AddAssistantFull(text, thinkingBlocks, nil)
					anchorAfterAssistant()
					conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Break remaining work into smaller pieces.")
					ch <- RetryEvent{Reason: fmt.Sprintf("max_tokens recovery %d/%d", outputRecoveries, maxOutputTokensRecoveries), Wait: 0}
					a.Reflector.NotifyAnomalyCallback()
					continue
				}
				// Exhausted: fall through to normal completion.
			} else {
				// Reset recovery counter on successful turn.
				outputRecoveries = 0
			}

			if len(toolCalls) == 0 {
				conv.AddAssistantFull(text, thinkingBlocks, nil)
				if a.FileHistory != nil {
					summary := text
					if len(summary) > 60 {
						summary = summary[:60] + "..."
					}
					a.FileHistory.MakeSnapshot(conv.Len(), summary)
				}
				ch <- LoopComplete{TotalTurns: iteration}
				if a.OnLoopComplete != nil {
					go a.OnLoopComplete(conv)
				}
				return
			}

			var toolUses []conversation.ToolUseBlock
			for _, tc := range toolCalls {
				toolUses = append(toolUses, conversation.ToolUseBlock{
					ToolUseID: tc.ToolID,
					ToolName:  tc.ToolName,
					Arguments: tc.Arguments,
				})
			}
			conv.AddAssistantFull(text, thinkingBlocks, toolUses)
			// Anchor real usage to the conversation now that the assistant message
			// is in place; subsequent tool results + next user message are
			// estimated incrementally on top of this baseline.
			anchorAfterAssistant()

			// Collect results from streaming executor (tools already started during LLM streaming).
			results := executor.CollectResults()

			var toolResults []conversation.ToolResultBlock
			for _, r := range results {
				if r.isUnknown {
					consecutiveUnknown++
				} else {
					consecutiveUnknown = 0
				}

				ch <- ToolResultEvent{
					ToolID:   r.toolID,
					ToolName: r.toolName,
					Output:   r.output,
					IsError:  r.isError,
					Elapsed:  r.elapsed,
				}

				truncated := r.output
				if len(truncated) > tools.MaxOutputChars {
					truncated = truncated[:tools.MaxOutputChars] + "\n… (output truncated)"
				}
				toolResults = append(toolResults, conversation.ToolResultBlock{
					ToolUseID: r.toolID,
					Content:   truncated,
					IsError:   r.isError,
				})
			}

			if consecutiveUnknown >= 3 {
				ch <- ErrorEvent{Message: "Too many consecutive unknown tool calls"}
				return
			}

			exitPlanCalled := false
			for _, tc := range toolCalls {
				if tc.ToolName == "ExitPlanMode" {
					exitPlanCalled = true
					break
				}
			}
			conv.AddToolResultsMessage(toolResults)

			// 阶段二: decide whether this turn's tool results are one of the four reflection triggers,
			// and if so fire the judge immediately. Firing here (rather than at the top of the next
			// iteration) gives the judge the whole upcoming turn — main LLM call, tool execution,
			// bookkeeping — as its head start over the ≤5s consume window.
			if a.Reflector.Enabled() {
				var toolNames []string
				hadError := false
				for _, r := range results {
					toolNames = append(toolNames, r.toolName)
					if r.isError {
						hadError = true
					}
				}
				if trigger := a.Reflector.ObserveTools(toolNames, hadError); trigger != "" {
					a.Reflector.Fire(trigger, conv)
				} else if a.Reflector.NotifyPeriodic(iteration) {
					// 周期兜底：默认关闭（ReflectionConfig.Periodic），开启后每 N 回合补一次检查，
					// 用于排查「事件触发漏掉了某类偏移」。
					a.Reflector.Fire(TriggerPeriodic, conv)
				}
			}

			// 非阻塞 memory recall：工具执行完后检查 prefetch 是否就绪
			if a.MemoryRecallCh != nil {
				select {
				case recall := <-a.MemoryRecallCh:
					if recall != "" {
						conv.AddSystemReminder(recall)
					}
					a.MemoryRecallCh = nil // 只消费一次
				default:
					// prefetch 还没好，下轮再检查
				}
			}

			if exitPlanCalled {
				ch <- TurnComplete{Turn: iteration}
				ch <- LoopComplete{TotalTurns: iteration}
				return
			}
			ch <- TurnComplete{Turn: iteration}
			a.emitHook(hooks.EventTurnEnd, "", nil)
		}
	}()

	return ch
}

// emitHook fires a hook event when an Engine is configured. Failures are non-fatal and surface via
// the hook notification queue (drained into the next turn's system reminders).
func (a *Agent) emitHook(event hooks.EventName, message string, args map[string]any) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.RunHooks(hooks.HookContext{
		EventName: event,
		ToolArgs:  args,
		Message:   message,
	})
}

// filterSchemasByName keeps only the tool schemas whose "name" passes the allow predicate. Used by
// Coordinator Mode to restrict a Lead agent to coordination-only tools while teammates do the
// actual work.
func filterSchemasByName(schemas []map[string]any, allow func(name string) bool) []map[string]any {
	out := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		if allow(name) {
			out = append(out, s)
		}
	}
	return out
}

// handleStreamError returns (retry, compacted): retry signals the caller to
// re-run the turn; compacted signals that a ForceCompact rewrote the
// conversation, so the caller must drop its usage anchor (its AnchorCount no
// longer maps to the new transcript).
func (a *Agent) handleStreamError(ctx context.Context, ch chan AgentEvent, conv *conversation.Manager, err error) (retry, compacted bool) {
	var ctxErr *llm.ContextTooLongError
	if errors.As(err, &ctxErr) {
		// conv 已由 Layer 1 就地裁剪，直接传 nil 让 ForceCompact 使用 conv 自身消息
		msg, compactErr := compact.ForceCompact(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.RecoveryState, a.currentToolSchemas(), nil)
		if compactErr == nil && msg != "" {
			ch <- CompactEvent{Message: "Auto-compacted due to context length: " + msg}
			// 阶段二: the transcript was just rewritten under the agent. That is precisely the
			// moment worth a reflection check — whatever the agent was reasoning toward may no
			// longer be in the context it thinks it has. Fired with nil conv on purpose: passing
			// the live conversation here would capture it from the error path that is actively
			// rewriting it.
			a.Reflector.NotifyAnomalyCallback()
			return true, true // retry, and the anchor is now stale
		}
		return false, false
	}

	var rlErr *llm.RateLimitError
	if errors.As(err, &rlErr) {
		wait := parseRetryAfter(rlErr.RetryAfter)
		ch <- RetryEvent{Reason: "rate limited", Wait: wait}
		select {
		case <-time.After(wait):
			// 阶段二: interrupted mid-turn and about to retry the same request.
			a.Reflector.NotifyAnomalyCallback()
			return true, false // retry without compaction
		case <-ctx.Done():
			return false, false
		}
	}

	return false, false
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 5 * time.Second
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 5 * time.Second
}

type toolExecResult struct {
	toolID    string
	toolName  string
	output    string
	isError   bool
	elapsed   time.Duration
	isUnknown bool
}

// extractFilePath pulls a representative path from common tool argument keys so hooks can do path-
// glob matching (`file_path =* "**/*.go"`).
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "pattern", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (a *Agent) executeSingleTool(ctx context.Context, eventCh chan AgentEvent, tc llm.ToolCallComplete) toolExecResult {
	tool := a.Registry.Get(tc.ToolName)
	start := time.Now()

	if tool == nil {
		return toolExecResult{
			toolID:    tc.ToolID,
			toolName:  tc.ToolName,
			output:    fmt.Sprintf("Error: unknown tool '%s'", tc.ToolName),
			isError:   true,
			elapsed:   time.Since(start),
			isUnknown: true,
		}
	}

	if a.Checker != nil {
		decision := a.Checker.Check(tool, tc.Arguments)
		if decision.Effect == permissions.Deny {
			return toolExecResult{
				toolID:   tc.ToolID,
				toolName: tc.ToolName,
				output:   fmt.Sprintf("Permission denied: %s", decision.Reason),
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
		if decision.Effect == permissions.Ask {
			respCh := make(chan PermissionResponse, 1)
			// 使用 DescribeToolAction 生成人类可读的操作描述
			desc := permissions.DescribeToolAction(tc.ToolName, tc.Arguments)
			// 阶段四 4.2: forward the decision so the UI can drop "always allow" on a
			// read-only-workspace write, and stamp the target so the user's answer can be audited.
			d := decision
			eventCh <- PermissionRequestEvent{
				ToolName:   tc.ToolName,
				Desc:       desc,
				ResponseCh: respCh,
				Decision:   &d,
				Target:     permissions.ExtractContent(tc.ToolName, tc.Arguments),
			}
			resp := <-respCh

			// Audit the human's answer. The Check wrapper already recorded that the PROMPT
			// appeared; without this line "asked" and "denied" would look identical in the trail.
			a.Checker.RecordApproval(
				tc.ToolName,
				permissions.ExtractContent(tc.ToolName, tc.Arguments),
				decision.Workspace,
				resp != PermDeny,
				decision.ReadonlyWrite,
			)

			if resp == PermDeny {
				return toolExecResult{
					toolID:   tc.ToolID,
					toolName: tc.ToolName,
					output:   "Permission denied by user",
					isError:  true,
					elapsed:  time.Since(start),
				}
			}
			if resp == PermAllowAlways {
				if decision.ReadonlyWrite {
					// Defence in depth: the TUI removes this option, but a non-TUI client
					// (remote UI, script) could still send it. Honouring it would permanently
					// convert a read-only workspace into a writable one.
					resp = PermAllow
				}
			}
			if resp == PermAllowAlways {
				content := permissions.ExtractContent(tc.ToolName, tc.Arguments)
				pattern := content + "*"
				if len(content) > 60 {
					pattern = content[:60] + "*"
				}
				// Layer 4b: 写入会话级放行集合（内存），同时持久化到规则引擎
				a.Checker.AddSessionAllow(tc.ToolName, pattern)
				a.Checker.RuleEngine.AppendLocalRule(permissions.Rule{
					ToolName: tc.ToolName,
					Pattern:  pattern,
					Effect:   permissions.RuleAllow,
				})
			}
		}
	}

	if a.Hooks != nil {
		hctx := hooks.HookContext{
			EventName: hooks.EventPreToolUse,
			ToolName:  tc.ToolName,
			ToolArgs:  tc.Arguments,
			FilePath:  extractFilePath(tc.Arguments),
		}
		if rejected, msg := a.Hooks.RunPreToolHooks(hctx); rejected {
			return toolExecResult{
				toolID:   tc.ToolID,
				toolName: tc.ToolName,
				output:   "Blocked by hook: " + msg,
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
	}

	// 阶段三 3.2: stamp the LRU clock at the moment the tool actually runs. This is the write
	// that makes eviction meaningful — without it every tool's clock is its discovery time and
	// "least recently used" degenerates into "discovered longest ago", which would happily evict
	// the tool the agent is using right now.
	//
	// Placed here, after the permission gate: a denied call never ran, so counting it would let a
	// repeatedly-denied tool accumulate executions and earn the active exemption it never earned.
	a.Registry.RecordExecute(tc.ToolName)

	result := tool.Execute(ctx, tc.Arguments)

	// Snapshot what ReadFile just handed to the model so the compact
	// recovery block can replay it after a Layer 2 summary wipes the
	// transcript. Re-reading from disk is one extra open per ReadFile —
	// cheaper than parsing line numbers back out of the tool output.
	if !result.IsError && tc.ToolName == "ReadFile" {
		if p, _ := tc.Arguments["file_path"].(string); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				a.RecoveryState.RecordFileRead(p, string(data))
			}
		}
	}

	if a.Hooks != nil {
		a.Hooks.RunHooks(hooks.HookContext{
			EventName: hooks.EventPostToolUse,
			ToolName:  tc.ToolName,
			ToolArgs:  tc.Arguments,
			FilePath:  extractFilePath(tc.Arguments),
			Message:   result.Output,
		})
	}

	return toolExecResult{
		toolID:   tc.ToolID,
		toolName: tc.ToolName,
		output:   result.Output,
		isError:  result.IsError,
		elapsed:  time.Since(start),
	}
}

func formatToolArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}
