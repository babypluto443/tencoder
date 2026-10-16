// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package tools

import (
	"sort"
	"time"
)

// 阶段三 · 任务 3.1/3.2：MCP 工具动态装载 / 驱逐。
//
// 现状是**只装不卸**：`MarkDiscovered` 之后 schema 永久留在 `GetAllSchemas` 里。
// 长时间会话里 Agent 会逐个 ToolSearch 出一堆工具，常驻 schema 只增不减，
// 于是「延迟加载省下的 85%」会随着会话变长被一点点吃回去。本文件补上卸载这一半。
//
// ── 第一约束不是 LRU，是 prompt cache 经济学 ──
//
// 一个自然但错误的做法是「每个回合检查一次，超了就去掉最久未用的那个」。
// 工具列表位于 prompt 的**前缀**，每回合变动一次 = 每回合击穿一次 prompt cache，
// 而 cache 命中省下的钱远大于省下的那几个 KB schema。
//
// 所以这里的设计是**阈值批量驱逐**：常驻数超过高水位（比如 40）时，一次砍回低水位
// （比如 25）。百级工具的长会话里，整场会话大概只会发生两三次失效，
// 摊销之后可以忽略。这是「批量」而非「逐个」的全部理由。
//
// ── 为什么 Registry 要加锁 ──
//
// 加驱逐必然要引入手写时钟，而时钟要在**工具真正 Execute 时**打点。
// Tencoder 的工具执行发生在 StreamingExecutor 的 goroutine 里
// （见 internal/agent/streaming_executor.go：每个 ToolCallComplete 一个 go func），
// 与主循环的 MarkDiscovered / GetAllSchemas 并发。
//
// 原来的 `tools` / `discoveredTools` 两个 map 是**裸 map 无锁**，
// 这在只有 MarkDiscovered（主循环单线程）时尚未爆，
// 但一旦引入执行侧打点就会变成真竞态。所以这一版顺手把锁补上——
// 这是让 3.1 能安全落地的前提，不是顺手的美化。

// ToolUsageStats is the per-tool bookkeeping the evictor ranks on.
type ToolUsageStats struct {
	// ExecuteCount is how many times the tool actually ran. Drives the active exemption.
	ExecuteCount int
	// LastExecutedMs is the wall-clock of the most recent Execute (unix ms). This is the PRIMARY
	// LRU clock.
	LastExecutedMs int64
	// DiscoveredMs is when ToolSearch loaded the tool's schema (unix ms). Used as the clock for
	// tools that were discovered but never executed — they are occupying the resident budget
	// without producing anything, so they rank as "least recently used" by definition.
	DiscoveredMs int64
}

// lastUsedMs returns the clock this tool is ranked by.
//
// Execute time wins over discovery time, which is the whole point: a tool discovered at T0 and
// executed at T50 is *hot*, while one discovered at T40 and never executed is cold, even though
// the first was "discovered" much earlier.
func (s ToolUsageStats) lastUsedMs() int64 {
	if s.LastExecutedMs > s.DiscoveredMs {
		return s.LastExecutedMs
	}
	return s.DiscoveredMs
}

// EvictionPolicy is the 阶段三 configuration surface. Zero values fall back to the defaults in
// DefaultEvictionPolicy.
type EvictionPolicy struct {
	// HighWatermark: evict when discovered count exceeds this. The task book default is 40.
	HighWatermark int
	// LowWatermark: evict down to this. Must be < HighWatermark or every round would re-trigger.
	LowWatermark int
	// ActiveExemptExecutions: a tool executed at least this many times in the session is never
	// evicted. Default 2. Rationale: repeatedly loading and evicting a tool you use constantly is
	// pure waste — you pay a ToolSearch round trip every time to re-acquire something you're
	// about to use again.
	ActiveExemptExecutions int
	// Disabled turns eviction off entirely (used by tests that need a stable tool set).
	Disabled bool
}

// DefaultEvictionPolicy returns the task book's numbers: >40 triggers, cut back to 25, tools
// executed ≥2 times are exempt.
func DefaultEvictionPolicy() EvictionPolicy {
	return EvictionPolicy{
		HighWatermark:          40,
		LowWatermark:           25,
		ActiveExemptExecutions: 2,
	}
}

func (p EvictionPolicy) normalized() EvictionPolicy {
	d := DefaultEvictionPolicy()
	if p.HighWatermark <= 0 {
		p.HighWatermark = d.HighWatermark
	}
	if p.LowWatermark <= 0 {
		p.LowWatermark = d.LowWatermark
	}
	if p.ActiveExemptExecutions < 0 {
		p.ActiveExemptExecutions = d.ActiveExemptExecutions
	}
	// A low watermark at or above the high watermark would evict forever without converging.
	if p.LowWatermark >= p.HighWatermark {
		p.LowWatermark = d.LowWatermark
		if p.LowWatermark >= p.HighWatermark {
			p.HighWatermark = p.LowWatermark + 1
		}
	}
	return p
}

// EvictionEvent describes one completed eviction round. Surfaced to the TUI so a sudden tool-schema
// change is explainable rather than mysterious (验收标准 3 asks for evidence, and a silent drop
// would look like a bug).
type EvictionEvent struct {
	Trigger       string // "high_watermark"
	DiscoveredIn  int    // resident count before
	Evicted       int    // how many were returned to the deferred pool
	Remaining     int    // resident count after
	SkippedActive int    // candidates skipped because they hit the active exemption
	Names         []string
}

// SetEvictionPolicy configures the evictor. Nil-safe on Registry.
func (r *Registry) SetEvictionPolicy(p EvictionPolicy) {
	r.mu.Lock()
	r.evictionPolicy = p.normalized()
	r.mu.Unlock()
}

// RecordDiscovered stamps the discovery time for a tool. Called from MarkDiscovered so the
// discovery clock can never drift from the actual discovery.
func (r *Registry) RecordDiscovered(name string) {
	r.mu.Lock()
	st := r.usage[name]
	if st.ExecuteCount == 0 {
		// Only stamp discovery when the tool has never run. Re-discovering an already-used tool
		// must not make it look cold again.
		st.DiscoveredMs = time.Now().UnixMilli()
	}
	r.usage[name] = st
	r.discoveredTools[name] = true
	r.mu.Unlock()
}

// MarkDiscovered loads a deferred tool's schema into the resident set. Thread-safe.
func (r *Registry) MarkDiscovered(name string) {
	r.RecordDiscovered(name)
}

// RecordExecute notes that a tool actually ran. Called from the tool execution path.
//
// This is the write that makes the LRU clock meaningful — without it every tool's clock would be
// its discovery time and "least recently used" would degenerate into "discovered longest ago",
// which would happily evict the tool the agent is actively using right now.
func (r *Registry) RecordExecute(name string) {
	r.mu.Lock()
	st := r.usage[name]
	st.ExecuteCount++
	st.LastExecutedMs = time.Now().UnixMilli()
	r.usage[name] = st
	r.mu.Unlock()
}

// IsDiscovered reports whether a tool's schema is currently resident. Thread-safe.
func (r *Registry) IsDiscovered(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discoveredTools[name]
}

// DiscoveredCount returns how many deferred tool schemas are currently resident.
func (r *Registry) DiscoveredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.discoveredTools)
}

// Usage returns a snapshot of a tool's usage stats. Zero value if unknown.
func (r *Registry) Usage(name string) ToolUsageStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usage[name]
}

// MaybeEvict runs a round if the resident count is over the high watermark. Returns nil when no
// round ran (either disabled, or under the threshold) — so the caller can simply check for nil.
//
// Called once per agent iteration. That call frequency is the point: even though it's checked
// every turn, it only ACTS on the rare occasion the count crosses the watermark, which is what
// keeps prompt-cache invalidation to a handful per session.
func (r *Registry) MaybeEvict() *EvictionEvent {
	r.mu.Lock()
	policy := r.evictionPolicy
	resident := len(r.discoveredTools)
	r.mu.Unlock()

	if policy.Disabled || resident <= policy.HighWatermark {
		return nil
	}
	return r.evictTo(policy)
}

// EvictTo unconditionally trims the resident set down to the low watermark, returning an event
// describing what happened. Exposed for benchmarks and tests that need a deterministic starting
// state; production should use MaybeEvict.
func (r *Registry) EvictTo() *EvictionEvent {
	r.mu.Lock()
	policy := r.evictionPolicy
	r.mu.Unlock()
	return r.evictTo(policy)
}

// evictTo is the single implementation behind both entry points. It snapshots candidates under the
// lock, ranks them, then mutates under a second lock — the ranking reads usage stats that Execute
// goroutines keep updating, so holding the lock across the sort would be both wrong (long hold)
// and unnecessary (a slightly stale ranking is harmless).
func (r *Registry) evictTo(policy EvictionPolicy) *EvictionEvent {
	policy = policy.normalized()

	r.mu.Lock()
	type candidate struct {
		name string
		stat ToolUsageStats
	}
	candidates := make([]candidate, 0, len(r.discoveredTools))
	for name := range r.discoveredTools {
		// Only deferred tools are ever resident, so only they are candidates. A non-deferred tool
		// must never be touched by this path.
		if t, ok := r.tools[name]; !ok || !isDeferred(t) {
			continue
		}
		candidates = append(candidates, candidate{name: name, stat: r.usage[name]})
	}
	before := len(r.discoveredTools)
	r.mu.Unlock()

	// Need to shed this many.
	target := before - policy.LowWatermark
	if target <= 0 {
		return nil
	}

	// Rank: coldest first by the execute-or-discovery clock.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].stat.lastUsedMs() < candidates[j].stat.lastUsedMs()
	})

	var victims []candidate
	skippedActive := 0
	for _, c := range candidates {
		if len(victims) >= target {
			break
		}
		if policy.ActiveExemptExecutions > 0 && c.stat.ExecuteCount >= policy.ActiveExemptExecutions {
			skippedActive++
			continue
		}
		victims = append(victims, c)
	}

	if len(victims) == 0 {
		// Everything resident is actively used. Evicting anyway would defeat the exemption, so
		// report it and let the session run over the watermark.
		return &EvictionEvent{
			Trigger:       "high_watermark",
			DiscoveredIn:  before,
			Evicted:       0,
			Remaining:     before,
			SkippedActive: skippedActive,
		}
	}

	// Re-verify membership before mutating: a ToolSearch could have re-discovered one of these in
	// between the snapshot and here, and evicting a tool that was just loaded would be a visible
	// no-op round trip for the model.
	r.mu.Lock()
	names := make([]string, 0, len(victims))
	for _, v := range victims {
		if r.discoveredTools[v.name] {
			delete(r.discoveredTools, v.name)
			names = append(names, v.name)
		}
	}
	remaining := len(r.discoveredTools)
	r.mu.Unlock()

	return &EvictionEvent{
		Trigger:       "high_watermark",
		DiscoveredIn:  before,
		Evicted:       len(names),
		Remaining:     remaining,
		SkippedActive: skippedActive,
		Names:         names,
	}
}

// StatsSnapshot is a point-in-time view of eviction bookkeeping, for tests and benchmarks.
type StatsSnapshot struct {
	Discovered int
	Deferred   int
	TotalTools int
	Executed   int // tools with ExecuteCount > 0
}

func (r *Registry) StatsSnapshot() StatsSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := StatsSnapshot{
		Discovered: len(r.discoveredTools),
		TotalTools: len(r.tools),
	}
	for _, t := range r.tools {
		if isDeferred(t) {
			s.Deferred++
		}
	}
	for _, u := range r.usage {
		if u.ExecuteCount > 0 {
			s.Executed++
		}
	}
	return s
}
