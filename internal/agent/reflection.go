// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"tencoder/internal/conversation"
)

// 阶段二 · 任务 2.1：独立反思节点。
//
// 核心形态：**第三人称旁路裁判**。独立上下文、轻量模型、事件驱动——
// 它不在主 agent 的上下文里占一个字，因此对主循环 token 预算零成本；
// 但它在偏移高发的四个时刻能拦一道。
//
// 全链路失败静默（与阶段一记忆子系统同一哲学）：漏拦一次的成本，
// 远小于卡死主循环。见 §处置链 处对「重规划」的克制。
//
// ── 为什么是异步预取，而不是同步调用 ──
//
// 同步方案在「工具结果落盘后」直接问裁判，代价是一次 LLM 往返（秒级），
// 每回合都要付。事件驱动虽然把调用次数压到很低，但单次延迟依然会直接叠在
// 主循环的墙钟上。
//
// 所以沿用 memory recall 已有的 prefetch-consume 形态（MemoryRecallCh 先例）：
//
//	工具结果落盘 → 立刻 fire（不等）→ 下一轮迭代开头限时等 ≤5s 消费
//
// 「下一轮迭代开头」这个时机选得刁钻：那一轮的主 LLM 调用**还没发出去**，
// 而裁判读的是上一轮结束时的轨迹——判定恰好对应「刚刚做完的那一步」，
// 又来得及在本次请求发出前注入处置。晚一轮不会让判定过期，只会让它晚一点生效。

// ReflectionUsage is the token accounting for one reflection call. Plain struct rather than
// llm.UsageInfo so this file stays free of the llm import beyond what's already here.
type ReflectionUsage struct {
	InputTokens  int
	OutputTokens int
}

// ReflectionJudgeFn is the side-query abstraction, injection-based for the same reason as
// memory.TaskExtractorFn: a one-shot system-prompt-bound query has nothing to do with the
// streaming client the main loop uses, and tests need to swap in a deterministic fake.
//
// 阶段五（任务分级路由）的第一个真实客户就是这里——判断时绑定 light 档。
type ReflectionJudgeFn func(ctx context.Context, systemPrompt, userMessage string) (string, ReflectionUsage, error)

// ReflectionVerdict is the judge's answer.
//
// Valid=false 表示判定无效（解析失败、超时被丢弃等）。无效判定**不进防抖窗口**——
// 裁判自己抖了一下，不能算成 agent 偏了一次。这是 K=3 滑动窗口能不能真正防抖的前提。
type ReflectionVerdict struct {
	OnTrack   bool
	Deviation string // 判 No 时的偏离点，一句话；判 Yes 时为空
	Valid     bool
	Trigger   string // 触发本次判定的事件名，用于埋点归因

	Usage   ReflectionUsage
	Elapsed time.Duration
}

// ReflectionTrigger names the four events from the resume claim, plus the periodic fallback.
// The strings double as log/telemetry keys, so they are stable identifiers, not prose.
type ReflectionTrigger string

const (
	// TriggerLocalizationDone — 连续 ≥N 个只读工具结果后，首个写类工具调用成功。
	// 这是偏移最高发的时刻：定位阶段看错了地方，接下来改的就是错的文件。
	TriggerLocalizationDone ReflectionTrigger = "localization_done"

	// TriggerPatchGenerated — EditFile/WriteFile 成功返回。补丁一旦落盘，
	// 偏移就从"还没写"变成"已经写错了"，越晚发现回滚代价越大。
	TriggerPatchGenerated ReflectionTrigger = "patch_generated"

	// TriggerToolFailure — 任意工具结果 isError=true。失败的工具调用往往意味着
	// agent 正在硬扛一个错误前提，而不是在正确路径上迭代。
	TriggerToolFailure ReflectionTrigger = "tool_failure"

	// TriggerAnomalyCallback — 异常回调路径（限流重试 / 上下文超长压缩重试 / max_tokens 恢复）。
	// 这条链路意味着主循环刚刚被打断过一轮，上下文被重写过。
	TriggerAnomalyCallback ReflectionTrigger = "anomaly_callback"

	// TriggerPeriodic — 周期兜底检查，默认关闭（见 ReflectionConfig.Periodic）。
	TriggerPeriodic ReflectionTrigger = "periodic"
)

// ReflectionConfig is the 阶段二 configuration surface.
//
// 默认值都在这里，且**周期兜底默认关闭**：事件驱动才是简历上讲的机制，
// 周期检查只会稀释叙事并制造无谓 token。需要排查时再打开。
type ReflectionConfig struct {
	// Enabled 总开关。false 时 Reflector 完全惰性（零 goroutine、零调用）。
	Enabled bool
	// Periodic 周期兜底检查。默认 false。
	Periodic bool
	// PeriodicEvery 周期兜底的回合间隔，仅 Periodic=true 时生效。
	PeriodicEvery int
	// MaxWaitMs 下一轮迭代开头等待判定的上限（毫秒）。超时即丢弃本次判定。
	MaxWaitMs int
	// DriftThreshold 连续 No 达到该次数即中断重规划（任务书 K=3）。
	DriftThreshold int
	// LocalizationRunThreshold 判定"定位完成"所需的连续只读工具结果数（任务书默认 3）。
	LocalizationRunThreshold int
	// TraceMessages 轨迹摘要取最近多少条消息。
	TraceMessages int
	// GoalMaxBytes 任务目标截断上限。
	GoalMaxBytes int
	// TraceMaxBytes 轨迹摘要截断上限。
	TraceMaxBytes int
}

// DefaultReflectionConfig returns the 阶段二 defaults.
//
// 注意 Enabled 默认 true 而 Periodic 默认 false——这两者方向相反，是刻意的：
// 反思本身是卖点要默认开，周期兜底是调试手段要默认关。
func DefaultReflectionConfig() ReflectionConfig {
	return ReflectionConfig{
		Enabled:                  true,
		Periodic:                 false,
		PeriodicEvery:            5,
		MaxWaitMs:                5000,
		DriftThreshold:           3,
		LocalizationRunThreshold: 3,
		TraceMessages:            10,
		GoalMaxBytes:             2048,
		TraceMaxBytes:            8192,
	}
}

func (c ReflectionConfig) normalized() ReflectionConfig {
	d := DefaultReflectionConfig()
	if c.MaxWaitMs <= 0 {
		c.MaxWaitMs = d.MaxWaitMs
	}
	if c.DriftThreshold <= 0 {
		c.DriftThreshold = d.DriftThreshold
	}
	if c.LocalizationRunThreshold <= 0 {
		c.LocalizationRunThreshold = d.LocalizationRunThreshold
	}
	if c.PeriodicEvery <= 0 {
		c.PeriodicEvery = d.PeriodicEvery
	}
	if c.TraceMessages <= 0 {
		c.TraceMessages = d.TraceMessages
	}
	if c.GoalMaxBytes <= 0 {
		c.GoalMaxBytes = d.GoalMaxBytes
	}
	if c.TraceMaxBytes <= 0 {
		c.TraceMaxBytes = d.TraceMaxBytes
	}
	return c
}

// ReflectionSystemPrompt is the judge's instruction.
//
// 两个刻意的措辞：
//   - **第三人称**："describe the agent's path in the third person" —— 让裁判审视
//     「一个 agent 正在做什么」，而不是「你要做什么」。反思发生在主 agent 之外，
//     用第三人称叙述本身就是这个架构的写照。
//   - **宁可多报 No**：反思的漏报（真偏移判 Yes）不会被 K=3 挡住——K=3 只防误报。
//     真正的兜底是评测数字（验收标准 1），所以裁判被明确要求偏向保守。
const ReflectionSystemPrompt = `You are a third-party observer watching an AI agent work on a task. Your only job is to judge one thing: is the agent's current path still aimed at the stated goal?

You are NOT the agent. You do not continue its work, do not advise it, and do not fix anything. You observe, then answer a single yes/no question.

Judge on DIRECTION, not on progress or cleverness:
- On track: the steps taken so far are a plausible route to the goal, even if something failed or needs a retry. Struggling is not drifting.
- Off track: the agent is doing something the goal does not require, solving a different problem than the one asked, working on files/modules unrelated to the goal, or building on an assumption the goal contradicts.

Ignore these when judging:
- Style, verbosity, or formatting choices.
- Whether a tool call failed — a failed attempt along a correct route is still on track.
- Speed. A slow correct path is on track.

Be conservative in the direction of reporting drift. A single No costs the agent one reminder; a missed drift costs a wrong deliverable. When you are genuinely unsure, say on_track=false and explain the doubt in one sentence.

Respond with valid JSON only, no markdown, no preamble:
{"on_track": true}
or
{"on_track": false, "deviation": "<one sentence: what the agent is doing that the goal does not ask for>"}`

// ReflectionPrompt assembles the judge input: the goal plus a compressed view of the recent trace.
//
// Both are byte-capped. This call is a side query on a light model — its entire value is in the
// routing decision it returns, not in reading the full transcript. Feeding the real history would
// make the judge as expensive as the main loop, which defeats the whole "does not occupy the
// main context" premise.
func buildReflectionPrompt(goal, trace string) string {
	var b strings.Builder
	b.WriteString("## Task goal\n\n")
	b.WriteString(goal)
	b.WriteString("\n\n## Recent trajectory\n\n")
	b.WriteString(trace)
	b.WriteString("\n\nIs the agent's current path still aimed at the goal?")
	return b.String()
}

// ReflectionDriftSink receives a confirmed drift when the K threshold trips. Wired by the caller
// to 阶段一's memory.TaskWriter so the offset becomes a task memory (任务 2.4).
//
// The signature is deliberately just (taskName, output) — identical to TaskWriter.OnSubTaskComplete —
// so the wiring is a one-line adapter and 阶段一's pipeline needs zero changes.
type ReflectionDriftSink func(taskName, output string)

// ReflectionStats is the token/调用 accounting exposed for 验收标准 2（开销可见且可控）
// and 验收标准 3（四个事件均有触发记录）.
type ReflectionStats struct {
	Calls        int            // 实际发起的判定调用数
	Triggers     map[string]int // 按事件名归因的触发次数
	OnTrack      int            // 判 Yes 次数
	Drift        int            // 判 No 次数
	Invalid      int            // 无效判定（解析失败/超时丢弃）
	Timeouts     int            // 消费等待超时次数
	Overwrites   int            // 被更新判定顶掉的判定数（本条顶掉旧的，或本条作为迟到者被丢弃）
	Reminders    int            // 注入目标重申的次数
	Replans      int            // 触发中断重规划的次数
	InputTokens  int
	OutputTokens int
}

// Snapshot returns a copy safe to read from another goroutine.
func (s ReflectionStats) Snapshot() ReflectionStats {
	out := s
	out.Triggers = make(map[string]int, len(s.Triggers))
	for k, v := range s.Triggers {
		out.Triggers[k] = v
	}
	return out
}

// reflectionResult is what the fired goroutine hands back, tagged with the fire sequence so a
// late arrival can be recognised as stale.
type reflectionResult struct {
	seq   uint64
	value ReflectionVerdict
}

// Reflector owns the 阶段二 reflection subsystem: trigger detection, async prefetch, the ≤5s
// consume window, and the K=3 disposition chain.
//
// Lifecycle per iteration:
//
//	ObserveTools(...)  → updates the read-only sliding window, returns a trigger if one fired
//	Fire(trigger, conv) → snapshot goal+trace, spawn the judge call  (non-blocking)
//	Consume(conv)       → wait ≤MaxWait for the verdict, apply the disposition
//
// Safe for concurrent use: only the judge goroutine and the main loop touch it, and they meet
// solely through the buffered channel plus the mutex-guarded counters.
type Reflector struct {
	judge ReflectionJudgeFn
	cfg   ReflectionConfig

	// OnDrift is called when the K threshold trips. Wired to 阶段一's TaskWriter.
	OnDrift ReflectionDriftSink
	// Logf receives the per-call single-line token accounting (任务 2.7). Optional.
	Logf func(format string, args ...any)

	mu sync.Mutex
	// goal is the task objective, captured from the first user message.
	goalCaptured bool
	goal         string
	// noStreak is the K=3 consecutive-No counter. Reset to 0 by any Yes.
	noStreak int
	// fireSeq increments on every Fire; abandonedSeq is the highest sequence already discarded.
	// A result whose seq <= abandonedSeq is dropped on arrival (see tryPublish).
	fireSeq      uint64
	abandonedSeq uint64
	stats        ReflectionStats

	// readOnlyRun is the sliding window of consecutive read-only tool results, and
	// writeAfterRead tracks whether the "first write after a ≥N read-only run" has fired, so the
	// localization-done trigger doesn't re-fire on every subsequent write in the same session.
	readOnlyRun       int
	writeSeenAfterRun bool

	// pending carries verdicts from the judge goroutine to the main loop. Buffered depth 1 so a
	// Fire never blocks; a full buffer means a verdict is already waiting, and the new one
	// replaces it (latest-wins).
	pending chan reflectionResult
}

// NewReflector constructs a Reflector. Returns nil when no judge is wired — callers invoke the
// methods unconditionally and the nil receiver no-ops, so callers need no nil checks.
func NewReflector(judge ReflectionJudgeFn, cfg ReflectionConfig) *Reflector {
	if judge == nil {
		return nil
	}
	return &Reflector{
		judge:   judge,
		cfg:     cfg.normalized(),
		pending: make(chan reflectionResult, 1),
		stats:   ReflectionStats{Triggers: map[string]int{}},
	}
}

// Stats returns a snapshot of the accounting counters.
func (r *Reflector) Stats() ReflectionStats {
	if r == nil {
		return ReflectionStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats.Snapshot()
}

// Enabled reports whether the subsystem is active. Callers use this to skip the whole
// ObserveTools/Fire/Consume trio in the hot path when disabled.
func (r *Reflector) Enabled() bool {
	return r != nil && r.cfg.Enabled
}

// ── 触发事件检测（任务 2.2）────────────────────────────────────────────────

// readOnlyTools / writeTools classify tools for the localization-done heuristic.
//
// The split is deliberately a fixed list rather than a per-tool metadata flag: these are the
// four pure-inspection tools Tencoder ships, and the heuristic only needs to know "did the agent
// just go look at something" vs "did it just change something". Adding a new read-only tool means
// adding it here, which is a one-line, reviewable change.
var readOnlyTools = map[string]bool{
	"ReadFile":   true,
	"Glob":       true,
	"Grep":       true,
	"ToolSearch": true,
}

var writeTools = map[string]bool{
	"EditFile":  true,
	"WriteFile": true,
}

// ObserveTools inspects one turn's tool results, updates the sliding window, and returns the
// trigger that fired (empty string = none).
//
// The returned trigger is at most ONE per turn even if several conditions hold. A single judge
// call per turn is the cost ceiling; when two conditions co-occur (a failing EditFile is both
// "patch generated" and "tool failure") we keep the higher-priority one — tool failure, because
// a failing write carries strictly more signal about where the path went wrong.
//
// Deliberately NOT inferred: whether a Bash command was "a test run". The task book's own rule is
// to key off isError rather than guessing at command semantics — Bash output is too varied, and a
// wrong guess here would fire the judge on every grep.
func (r *Reflector) ObserveTools(toolNames []string, hadError bool) ReflectionTrigger {
	if !r.Enabled() {
		return ""
	}

	// Sliding window bookkeeping runs regardless of whether a trigger fires, so the window stays
	// accurate across turns that produce no trigger.
	readOnlyThisTurn := 0
	writeSucceeded := false
	for _, n := range toolNames {
		if readOnlyTools[n] {
			readOnlyThisTurn++
		}
		if writeTools[n] {
			writeSucceeded = true
		}
	}

	r.mu.Lock()
	r.readOnlyRun = readOnlyThisTurn
	var trigger ReflectionTrigger

	switch {
	case hadError:
		trigger = TriggerToolFailure
	case writeSucceeded && !r.writeSeenAfterRun:
		r.writeSeenAfterRun = true
		if readOnlyThisTurn >= r.cfg.LocalizationRunThreshold {
			// ≥N consecutive reads then a successful write — the agent finished locating and
			// committed to acting on what it found. If what it found was the wrong place, every
			// subsequent edit is wrong too.
			trigger = TriggerLocalizationDone
		} else {
			trigger = TriggerPatchGenerated
		}
	}
	r.mu.Unlock()

	// Trigger counting lives in Fire, not here — Fire is the only place that knows whether the
	// judge call actually happened. Counting here would double-count every trigger, and
	// over-count further on the turns where Fire skips for lack of a usable trace snapshot.
	return trigger
}

// NotifyAnomalyCallback records that the main loop recovered from an anomaly (rate-limit retry,
// forced compaction retry, max_tokens recovery). Feeds TriggerAnomalyCallback.
func (r *Reflector) NotifyAnomalyCallback() {
	if !r.Enabled() {
		return
	}
	r.Fire(TriggerAnomalyCallback, nil)
}

// NotifyPeriodic fires the periodic fallback when enabled. Called once per turn by the main loop,
// which owns the iteration counter; this only decides whether the turn qualifies.
func (r *Reflector) NotifyPeriodic(turn int) bool {
	if r == nil || !r.cfg.Enabled || !r.cfg.Periodic {
		return false
	}
	if turn <= 0 || turn%r.cfg.PeriodicEvery != 0 {
		return false
	}
	return true
}

// ── 异步预取通道（任务 2.1 的实现细节）────────────────────────────────────

// Fire snapshots the goal and trace, then spawns the judge call. Returns immediately — the whole
// point is that the main loop never waits on the judge.
//
// conv may be nil: the anomaly-callback trigger fires from error handling, where passing the live
// conversation would mean capturing it from a path that may already be rewriting it. In that case
// the judge sees the goal alone plus whatever trace was snapshotted on the previous Fire.
func (r *Reflector) Fire(trigger ReflectionTrigger, conv *conversation.Manager) {
	if !r.Enabled() {
		return
	}

	goal, trace := "", ""
	if conv != nil {
		goal, trace = r.captureGoal(conv), r.buildTrace(conv)
	}
	// Guard on the GOAL, not on the assembled prompt: buildReflectionPrompt always returns the
	// "## Task goal / ## Recent trajectory" scaffolding, so testing the assembled string for
	// emptiness never fires and the judge ends up being asked to judge nothing.
	//
	// No goal means there is no objective to stay aimed at, so the judgment would be meaningless.
	// An empty trace with a real goal is a different case — that still gets a call, because
	// "the agent hasn't done anything yet" is a legitimate (trivially on-track) answer, and the
	// anomaly-callback trigger legitimately fires with a trace-only input.
	if goal == "" {
		return
	}
	input := buildReflectionPrompt(goal, trace)

	r.mu.Lock()
	r.fireSeq++
	seq := r.fireSeq
	r.stats.Calls++
	r.stats.Triggers[string(trigger)]++
	r.mu.Unlock()

	// Fire-and-forget: this goroutine's only job is the judge call and publishing the result. It
	// must never write to conv, never emit events, and never panic on its own account.
	go func() {
		start := time.Now()
		raw, usage, err := r.judge(context.Background(), ReflectionSystemPrompt, input)
		elapsed := time.Since(start)

		v := ReflectionVerdict{Trigger: string(trigger), Usage: usage, Elapsed: elapsed}
		if err != nil {
			// Judge unavailable. Stay silent — a failed judgment is not a drift finding.
			r.logf("[reflection] judge failed: trigger=%s err=%v dur=%s", trigger, err, elapsed.Round(time.Millisecond))
			return
		}
		parsed, ok := parseReflectionVerdict(raw)
		if !ok {
			r.logf("[reflection] unparseable verdict: trigger=%s dur=%s", trigger, elapsed.Round(time.Millisecond))
			return
		}
		v.OnTrack = parsed.onTrack
		v.Deviation = parsed.deviation
		v.Valid = true

		r.mu.Lock()
		r.stats.InputTokens += usage.InputTokens
		r.stats.OutputTokens += usage.OutputTokens
		r.mu.Unlock()

		r.logf("[reflection] verdict=%s on_track=%v trigger=%s in=%d out=%d dur=%s",
			trigger, v.OnTrack, string(trigger), usage.InputTokens, usage.OutputTokens, elapsed.Round(time.Millisecond))
		r.tryPublish(reflectionResult{seq: seq, value: v})
	}()
}

// tryPublish delivers a verdict into the 1-deep buffer with latest-wins semantics.
//
// "Latest" means **highest fire sequence**, not **last to arrive**. Those are different things and
// conflating them is a real bug: the judge calls run concurrently and finish out of order, so the
// verdict about the *oldest* trajectory is quite often the *last* one to land. Arrival-order
// eviction would then hand the main loop a stale judgment about a trajectory it has already moved
// past — and worse, charge it against the K window.
//
// So the buffer always holds the highest-seq verdict seen so far:
//   - res.seq > buffered.seq → replace (the newer judgment wins);
//   - res.seq < buffered.seq → drop this one (a late arrival from an older fire).
//   - res.seq <= abandonedSeq → drop (the main loop already timed out on that sequence).
func (r *Reflector) tryPublish(res reflectionResult) {
	for {
		// Fast path: buffer empty.
		select {
		case r.pending <- res:
			return
		default:
		}

		r.mu.Lock()
		abandoned := r.abandonedSeq
		r.mu.Unlock()
		if res.seq <= abandoned {
			// The main loop already moved past this sequence; applying it now would be out of order.
			return
		}

		// Buffer is full — compare sequence numbers before evicting anything.
		select {
		case buffered := <-r.pending:
			if buffered.seq > res.seq {
				// A NEWER judgment already holds the slot. Put it back and discard this stale
				// arrival — the older fire finishing last must not overwrite the current judgment.
				select {
				case r.pending <- buffered:
				default:
					// Main loop drained it concurrently; it was consumed, which is fine.
				}
				r.mu.Lock()
				r.stats.Overwrites++
				r.mu.Unlock()
				return
			}
			r.mu.Lock()
			r.stats.Overwrites++
			r.mu.Unlock()
			// Loop: buffered.seq <= res.seq, so ours is the newer one — take the slot.
		default:
			// Main loop drained between our failed send and this receive; retry the send.
		}
	}
}

// Consume waits up to MaxWait for a verdict and applies the disposition chain. Called at the top
// of each iteration, BEFORE the main LLM request goes out.
//
// The wait is the only place the reflection subsystem can cost the main loop wall-clock, which is
// why the bound is a config field and why exceeding it just discards the verdict (counting nothing
// against the K window).
func (r *Reflector) Consume(conv *conversation.Manager) {
	if !r.Enabled() {
		return
	}

	r.mu.Lock()
	target := r.fireSeq
	r.mu.Unlock()

	var res reflectionResult
	select {
	case res = <-r.pending:
	case <-time.After(time.Duration(r.cfg.MaxWaitMs) * time.Millisecond):
		r.mu.Lock()
		if target > r.abandonedSeq {
			r.abandonedSeq = target
		}
		r.stats.Timeouts++
		r.mu.Unlock()
		return
	}

	r.mu.Lock()
	abandoned := r.abandonedSeq
	r.mu.Unlock()
	if res.seq <= abandoned {
		// Already-timed-out result; count as invalid and drop.
		r.mu.Lock()
		r.stats.Invalid++
		r.mu.Unlock()
		return
	}

	// Defence in depth: a buffered verdict must be at least as new as the last Fire this Consume
	// was waiting for. tryPublish already enforces highest-seq-wins, but Consume is where an
	// out-of-order judgment would actually reach the conversation, so it re-checks rather than
	// trusting the buffer.
	if res.seq < target {
		r.mu.Lock()
		r.stats.Invalid++
		r.mu.Unlock()
		return
	}

	v := res.value
	if !v.Valid {
		r.mu.Lock()
		r.stats.Invalid++
		r.mu.Unlock()
		return
	}

	r.applyVerdict(v, conv)
}

// applyVerdict is the disposition chain (任务 2.3).
//
//	Yes        → 窗口清零
//	No (1..K-1) → 注入目标重申，窗口 +1
//	No (K)     → 中断重规划，窗口清零，并把偏移沉淀成任务记忆
func (r *Reflector) applyVerdict(v ReflectionVerdict, conv *conversation.Manager) {
	if v.OnTrack {
		r.mu.Lock()
		r.noStreak = 0
		r.stats.OnTrack++
		r.mu.Unlock()
		return
	}

	r.mu.Lock()
	r.noStreak++
	streak := r.noStreak
	r.stats.Drift++
	threshold := r.cfg.DriftThreshold
	r.mu.Unlock()

	if conv == nil {
		return
	}

	if streak >= threshold {
		r.mu.Lock()
		r.noStreak = 0
		r.stats.Replans++
		goal := r.goal
		r.mu.Unlock()

		conv.AddSystemReminder(buildReplanReminder(goal, v.Deviation))
		r.sinkDrift(goal, v)
		return
	}

	r.mu.Lock()
	r.stats.Reminders++
	goal := r.goal
	r.mu.Unlock()
	conv.AddSystemReminder(buildDriftReminder(goal, v.Deviation, streak, threshold))
}

// sinkDrift hands the confirmed drift to 阶段一's task-memory pipeline (任务 2.4).
//
// Only on a full replan, not on every reminder: a reminder is a soft nudge that the agent may
// simply correct on its own, whereas a replan is the system stating the path was wrong K times in
// a row. Writing a task memory for every soft nudge would flood tasks/ with self-inflicted noise.
func (r *Reflector) sinkDrift(goal string, v ReflectionVerdict) {
	if r.OnDrift == nil {
		return
	}
	r.OnDrift(
		"goal-drift: "+truncateForMemory(goal, 60),
		fmt.Sprintf(
			"The agent drifted off the task goal and was interrupted for replanning.\n\nGoal: %s\nDeviation: %s\nTriggered at: %s\nJudgment was No %d times in a row (threshold %d).",
			goal, v.Deviation, v.Trigger, r.cfg.DriftThreshold, r.cfg.DriftThreshold,
		),
	)
}

// buildDriftReminder is the soft disposition: restate the goal and name the deviation.
//
// The deviation text comes from the judge, so it is quoted rather than asserted — the main agent
// should treat it as one observer's reading, not as a system fact. That framing is what keeps a
// wrong No from being obeyed as gospel.
func buildDriftReminder(goal, deviation string, streak, threshold int) string {
	var b strings.Builder
	b.WriteString("An independent observer judged that your recent steps may have moved away from the original goal.\n\n")
	b.WriteString("Original goal:\n")
	b.WriteString(goal)
	b.WriteString("\n\nObserved deviation: ")
	if deviation != "" {
		b.WriteString(deviation)
	} else {
		b.WriteString("(not specified)")
	}
	fmt.Fprintf(&b, "\n\nTreat that observation as one reviewer's reading, not as a verdict. Re-read the goal above, check whether your recent actions actually serve it, and if they do not, say what the right next step is and do that instead. This is check %d of %d before the path is forcibly interrupted.", streak, threshold)
	return b.String()
}

// buildReplanReminder is the hard disposition (K reached): stop and re-plan.
func buildReplanReminder(goal, deviation string) string {
	var b strings.Builder
	b.WriteString("STOP. Your recent actions have been judged off-goal " + "repeatedly" + " and the current path is being abandoned.\n\n")
	b.WriteString("Original goal:\n")
	b.WriteString(goal)
	b.WriteString("\n\nThe deviation that was observed: ")
	if deviation != "" {
		b.WriteString(deviation)
	} else {
		b.WriteString("(not specified)")
	}
	b.WriteString("\n\nDo NOT continue the current approach. Do not resume from where you stopped.\n" +
		"Instead: state in one or two sentences what the goal actually requires, list the steps that " +
		"would satisfy it, then execute from the first of those steps.")
	return b.String()
}

func truncateForMemory(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (r *Reflector) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// ── 判定解析 ──────────────────────────────────────────────────────────────

type parsedVerdict struct {
	onTrack   bool
	deviation string
}

// parseReflectionVerdict parses the judge's strict JSON. Anything else is invalid.
//
// Invalid is NOT treated as Yes. That distinction is the whole design: a garbage reply means "the
// judge did not deliver a judgment", and treating it as OnTrack would let a broken judge silently
// disable drift detection — the most dangerous possible failure mode, because it looks like
// success. Invalid verdicts are counted separately and never touch the K window.
func parseReflectionVerdict(raw string) (parsedVerdict, bool) {
	clean := extractJSON(raw)
	if clean == "" {
		return parsedVerdict{}, false
	}
	var parsed struct {
		OnTrack   *bool  `json:"on_track"`
		Deviation string `json:"deviation"`
	}
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return parsedVerdict{}, false
	}
	// Pointer distinguishes "absent" from "false" — `{}` must not read as a confident No.
	if parsed.OnTrack == nil {
		return parsedVerdict{}, false
	}
	dev := strings.TrimSpace(multiLineToSpace(parsed.Deviation))
	if !*parsed.OnTrack && dev == "" {
		// A No with no stated deviation cannot be acted on: the reminder would say "you drifted"
		// without saying where. Treat as invalid rather than inject an unactionable instruction.
		return parsedVerdict{}, false
	}
	return parsedVerdict{onTrack: *parsed.OnTrack, deviation: dev}, true
}

// extractJSON returns the first {...} span, tolerating markdown fences and prose around it.
// Duplicated from the memory package on purpose: three lines beats a shared dependency for a
// package this deep in the import graph.
func extractJSON(raw string) string {
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, "{") {
		return t
	}
	start := strings.Index(t, "{")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(t, "}")
	if end < start {
		return ""
	}
	return t[start : end+1]
}

func multiLineToSpace(s string) string {
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
}

// ── 输入组装：目标 + 轨迹摘要 ─────────────────────────────────────────────

// captureGoal returns the task objective, taken from the first non-reminder user message.
//
// It skips <system-reminder>-wrapped messages on purpose: those are Tencoder's own injections
// (plan-mode reminders, deferred-tool lists, notifications), and picking one of those up would
// redefine the "goal" as an instruction about tools. Also skipped: the auto-memory header block,
// which is prepended to message 0 and is context, not a request.
func (r *Reflector) captureGoal(conv *conversation.Manager) string {
	r.mu.Lock()
	if r.goalCaptured {
		goal := r.goal
		r.mu.Unlock()
		return goal
	}
	r.mu.Unlock()

	goal := extractTaskGoal(conv.GetMessages())
	r.mu.Lock()
	// Set unconditionally once computed (even if empty): the goal must not change mid-run, and
	// recomputing it each turn would let a later system-reminder-style message become the goal.
	r.goal = truncateForMemory(goal, r.cfg.GoalMaxBytes)
	r.goalCaptured = true
	goal = r.goal
	r.mu.Unlock()
	return goal
}

// extractTaskGoal lives in agent.go — it is shared with the 阶段三 summarizer so that both
// subsystems resolve "the task goal" to the same message. Reaching across files within one package
// is free; a second copy would also be free but would drift.

// buildTrace renders the tail of the conversation as a compact trajectory view.
//
// One line per action, in the order it happened: what the agent SAID, what it CALLED, what came
// BACK. Full message bodies are collapsed to a leading excerpt — the judge needs the shape of the
// path (looked in file X → edited file Y → test failed), not the contents of a 2000-line diff.
func (r *Reflector) buildTrace(conv *conversation.Manager) string {
	messages := conv.GetMessages()
	if len(messages) == 0 {
		return ""
	}
	start := len(messages) - r.cfg.TraceMessages
	if start < 0 {
		start = 0
	}

	var b strings.Builder
	for _, m := range messages[start:] {
		switch {
		case len(m.ToolResults) > 0:
			for _, tr := range m.ToolResults {
				status := "ok"
				if tr.IsError {
					status = "ERROR"
				}
				fmt.Fprintf(&b, "result[%s]: %s\n", status, excerpt(tr.Content, 160))
			}
		case len(m.ToolUses) > 0:
			if c := strings.TrimSpace(m.Content); c != "" {
				fmt.Fprintf(&b, "assistant: %s\n", excerpt(c, 200))
			}
			for _, tu := range m.ToolUses {
				fmt.Fprintf(&b, "call: %s %s\n", tu.ToolName, excerpt(describeArgs(tu.Arguments), 160))
			}
		case strings.TrimSpace(m.Content) != "":
			// Label by the ACTUAL role. Keying this on "has content" alone would print the user's
			// own messages as "assistant:", which makes the judge read the task instruction as if
			// the agent had stated it — precisely the kind of confusion that manufactures a
			// phantom drift finding.
			role := "assistant"
			if m.Role == "user" {
				role = "user"
			}
			fmt.Fprintf(&b, "%s: %s\n", role, excerpt(m.Content, 200))
		default:
			if m.Role == "user" {
				b.WriteString("user: (tool results)\n")
			}
		}
	}
	return truncateForMemory(b.String(), r.cfg.TraceMaxBytes)
}

// describeArgs renders tool arguments compactly. Full JSON would swamp the trace budget on
// WriteFile/EditFile, so only the argument that identifies the target is kept.
func describeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	if p := extractFilePath(args); p != "" {
		return p
	}
	parts := make([]string, 0, len(args))
	for _, k := range []string{"command", "query", "pattern", "url", "path", "description"} {
		if v, ok := args[k].(string); ok && v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

func excerpt(s string, n int) string {
	s = multiLineToSpace(strings.TrimSpace(s))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
