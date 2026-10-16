// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 阶段三 · 任务 3.1/3.2 的度量与验证。
//
// 口径（任务书 §2.D/E）：
//   - 百级工具场景扩到 120 个 realistic 工具；
//   - token 口径 = GetAllSchemas 序列化后的**字符数**（可复现，token 按 ÷4 估算并标注）；
//   - 同义混淆场景验证「驱逐后能重发现」且「选择准确率不因驱逐下降」。

// newHundredToolRegistry builds a registry with the 6 builtin tools, ToolSearch, and n deferred
// realistic MCP tools — the shape a real MCP-heavy session has.
func newHundredToolRegistry(n int, policy EvictionPolicy) (*Registry, []*realisticMCPTool) {
	reg := NewRegistry()
	for _, tool := range CreateDefaultTools().Registry.ListTools() {
		reg.Register(tool)
	}
	reg.Register(&ToolSearchTool{Registry: reg, Protocol: "anthropic"})
	mcp := makeRealisticTools(n)
	for _, mt := range mcp {
		reg.Register(mt)
	}
	reg.SetEvictionPolicy(policy)
	return reg, mcp
}

// residentSchemaBytes returns the serialized size of the tools parameter as currently resident.
// This is the 阶段三 headline metric: "常驻 schema 字符数".
func residentSchemaBytes(reg *Registry) int {
	data, _ := json.Marshal(reg.GetAllSchemas("anthropic"))
	return len(data)
}

// reminderBytes mirrors exactly what agent.go injects each turn: the deferred-name list.
func reminderBytes(reg *Registry) int {
	names := reg.GetDeferredToolNames()
	if len(names) == 0 {
		return 0
	}
	return len("The following deferred tools are available via ToolSearch. Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" +
		strings.Join(names, "\n"))
}

// TestHundredToolSessionEvictionIncrementalSaving measures the 阶段三 increment: a long session
// where the agent gradually loads many tools, with and without the evictor.
//
// This is the number behind 验收标准 1 — "85% 基础上的增量". The comparison that matters is NOT
// vs full loading (that is the existing 85%); it is **no-eviction vs eviction in the same
// scenario**, because that isolates exactly what 阶段三 adds.
func TestHundredToolSessionEvictionIncrementalSaving(t *testing.T) {
	const (
		numTools     = 120
		numTurns     = 40
		evictHi      = 40
		evictLo      = 25
		toolsPerTurn = 3 // agent ToolSearches a few new tools each turn
	)

	policy := EvictionPolicy{HighWatermark: evictHi, LowWatermark: evictLo, ActiveExemptExecutions: 2}

	// Two registries over the SAME session script: one with eviction, one without.
	regEvict, mcp := newHundredToolRegistry(numTools, policy)
	regNoEvict, _ := newHundredToolRegistry(numTools, EvictionPolicy{Disabled: true})

	// Pre-eviction baseline: before anything is discovered both are identical (builtins only).
	baseBytes := residentSchemaBytes(regEvict)

	var totalEvict, totalNoEvict int
	var evictionRounds int
	var maxResidentEvict, maxResidentNoEvict int
	evictedTotal := 0

	t.Logf("=== 百级工具会话：%d 个 deferred 工具 × %d 轮，每轮 ToolSearch %d 个 ===\n", numTools, numTurns, toolsPerTurn)
	t.Logf("起始常驻 schema: %d bytes (仅内置工具)", baseBytes)
	t.Logf("")
	t.Logf("%-6s %14s %14s %10s %10s %8s", "Turn", "evict(bytes)", "noevict(bytes)", "resident_E", "resident_N", "evicted")

	nextTool := 0
	for turn := 1; turn <= numTurns; turn++ {
		// The agent discovers a few new tools this turn (ToolSearch select:...).
		for i := 0; i < toolsPerTurn && nextTool < len(mcp); i++ {
			name := mcp[nextTool].Name()
			regEvict.MarkDiscovered(name)
			regNoEvict.MarkDiscovered(name)
			// Use roughly a third of what gets loaded — a realistic ratio, and it keeps the
			// active-exemption path exercised rather than degenerate.
			if nextTool%3 == 0 {
				regEvict.RecordExecute(name)
			}
			nextTool++
		}

		evictedThisTurn := 0
		if ev := regEvict.MaybeEvict(); ev != nil && ev.Evicted > 0 {
			evictionRounds++
			evictedTotal += ev.Evicted
			evictedThisTurn = ev.Evicted
		}

		eBytes := residentSchemaBytes(regEvict) + reminderBytes(regEvict)
		nBytes := residentSchemaBytes(regNoEvict) + reminderBytes(regNoEvict)
		totalEvict += eBytes
		totalNoEvict += nBytes

		re := regEvict.DiscoveredCount()
		rn := regNoEvict.DiscoveredCount()
		if re > maxResidentEvict {
			maxResidentEvict = re
		}
		if rn > maxResidentNoEvict {
			maxResidentNoEvict = rn
		}

		if turn%5 == 0 || evictedThisTurn > 0 {
			t.Logf("%-6d %14d %14d %10d %10d %8d", turn, eBytes, nBytes, re, rn, evictedThisTurn)
		}
	}

	saving := 1 - float64(totalEvict)/float64(totalNoEvict)
	t.Logf("")
	t.Logf("=== 汇总 ===")
	t.Logf("无驱逐累计:        %d bytes (~%d tokens)", totalNoEvict, totalNoEvict/4)
	t.Logf("有驱逐累计:        %d bytes (~%d tokens)", totalEvict, totalEvict/4)
	t.Logf("阶段三增量节省:    %.1f%%  (~%d tokens, 口径：字符数 ÷4 估算)", saving*100, (totalNoEvict-totalEvict)/4)
	t.Logf("驱逐轮次:          %d 次 / %d 轮  ← prompt cache 失效次数", evictionRounds, numTurns)
	t.Logf("驱逐工具总数:      %d", evictedTotal)
	t.Logf("常驻峰值:          驱逐 %d vs 不驱逐 %d (高水位 %d)", maxResidentEvict, maxResidentNoEvict, evictHi)

	// The increment must be real, not noise.
	if saving < 0.10 {
		t.Errorf("阶段三增量节省应 >= 10%%，实际 %.1f%%", saving*100)
	}
	// The whole point of batch eviction is a LOW round count. If we evicted every turn we'd be
	// trading schema bytes for prompt-cache invalidations, which is a losing trade.
	if evictionRounds > numTurns/4 {
		t.Errorf("驱逐轮次 %d 过多（> %d）；批量驱逐的意义就是摊销 cache 失效",
			evictionRounds, numTurns/4)
	}
}

// TestEvictionIsLossless — the core safety property: eviction returns tools to the deferred pool,
// they reappear in GetDeferredToolNames, and ToolSearch can load them again.
func TestEvictionIsLossless(t *testing.T) {
	reg, mcp := newHundredToolRegistry(60, EvictionPolicy{HighWatermark: 10, LowWatermark: 5, ActiveExemptExecutions: 2})

	loaded := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		reg.MarkDiscovered(mcp[i].Name())
		loaded = append(loaded, mcp[i].Name())
	}
	if reg.DiscoveredCount() != 30 {
		t.Fatalf("setup: discovered = %d, want 30", reg.DiscoveredCount())
	}

	ev := reg.MaybeEvict()
	if ev == nil || ev.Evicted == 0 {
		t.Fatal("30 resident tools against a high watermark of 10 must trigger eviction")
	}
	if reg.DiscoveredCount() != 5 {
		t.Errorf("after eviction resident = %d, want low watermark 5", reg.DiscoveredCount())
	}

	// Every evicted tool must be back in the deferred pool...
	deferred := map[string]bool{}
	for _, n := range reg.GetDeferredToolNames() {
		deferred[n] = true
	}
	for _, n := range ev.Names {
		if !deferred[n] {
			t.Errorf("evicted tool %q is not back in the deferred pool", n)
		}
	}

	// ...and actually reloadable through ToolSearch.
	search := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	reloaded := ev.Names[0]
	res := search.Execute(context.Background(), map[string]any{"query": "select:" + reloaded})
	if res.IsError {
		t.Fatalf("ToolSearch failed to reload an evicted tool: %s", res.Output)
	}
	if !reg.IsDiscovered(reloaded) {
		t.Errorf("tool %q was not re-marked discovered after ToolSearch reload", reloaded)
	}
}

// TestEvictionRespectsActiveExemption — a tool used constantly must survive even when it is the
// coldest by clock.
func TestEvictionRespectsActiveExemption(t *testing.T) {
	reg, mcp := newHundredToolRegistry(30, EvictionPolicy{HighWatermark: 5, LowWatermark: 2, ActiveExemptExecutions: 2})

	// "hot" is discovered first (oldest clock) but executed 3 times.
	reg.MarkDiscovered(mcp[0].Name())
	for i := 0; i < 3; i++ {
		reg.RecordExecute(mcp[0].Name())
	}
	// The rest are discovered later and never used.
	for i := 1; i < 20; i++ {
		reg.MarkDiscovered(mcp[i].Name())
	}

	ev := reg.MaybeEvict()
	if ev == nil {
		t.Fatal("expected an eviction round")
	}
	for _, n := range ev.Names {
		if n == mcp[0].Name() {
			t.Error("an actively-used tool (3 executions) was evicted; the exemption failed")
		}
	}
	if !reg.IsDiscovered(mcp[0].Name()) {
		t.Error("the active tool must still be resident")
	}
	if ev.SkippedActive == 0 {
		t.Error("SkippedActive should record that candidates were skipped by the exemption")
	}
}

// TestEvictionPrefersNeverExecutedOverStale — a tool discovered at T0 but never run is colder
// than one discovered at T40 and run at T50, despite the older discovery time.
func TestEvictionPrefersNeverExecutedOverStale(t *testing.T) {
	reg := NewRegistry()
	// Three tools so a single round evicts two and ordering is observable. (LowWatermark must be
	// strictly below HighWatermark — normalized() rejects equality, since trimming back to exactly
	// the threshold would re-trigger every turn.)
	for _, n := range []string{"old-never-used", "middle-stale", "new-and-used"} {
		reg.Register(&namedStubTool{name: n})
		reg.MarkDiscovered(n)
	}
	reg.SetEvictionPolicy(EvictionPolicy{HighWatermark: 2, LowWatermark: 1, ActiveExemptExecutions: 5})

	// Force a wide clock gap by writing the usage records directly: "old-never-used" has an ancient
	// discovery clock and no execution at all; "new-and-used" was discovered late AND executed after
	// that. A clock that ranked by discovery-time alone would evict the wrong one.
	reg.usage["old-never-used"] = ToolUsageStats{DiscoveredMs: 1000}
	reg.usage["middle-stale"] = ToolUsageStats{DiscoveredMs: 5000}
	reg.usage["new-and-used"] = ToolUsageStats{DiscoveredMs: 9000, LastExecutedMs: 9500, ExecuteCount: 1}

	ev := reg.MaybeEvict()
	if ev == nil {
		t.Fatal("expected eviction")
	}
	if ev.Evicted != 2 {
		t.Fatalf("evicted %d, want 2 (3 resident → low watermark 1)", ev.Evicted)
	}
	if ev.Names[0] != "old-never-used" || ev.Names[1] != "middle-stale" {
		t.Errorf("eviction order wrong: got %v, want [old-never-used middle-stale] (coldest first)", ev.Names)
	}
	if !reg.IsDiscovered("new-and-used") {
		t.Error("the recently-executed tool must survive")
	}
}

// TestEvictionDisabledLeavesResidentSetUntouched — the escape hatch must be a true no-op.
func TestEvictionDisabledLeavesResidentSetUntouched(t *testing.T) {
	reg, mcp := newHundredToolRegistry(50, EvictionPolicy{Disabled: true})
	for i := 0; i < 45; i++ {
		reg.MarkDiscovered(mcp[i].Name())
	}
	if ev := reg.MaybeEvict(); ev != nil {
		t.Errorf("eviction disabled must never act, got %+v", ev)
	}
	if reg.DiscoveredCount() != 45 {
		t.Errorf("resident = %d, want 45 unchanged", reg.DiscoveredCount())
	}
}

// TestEvictionNeverTargetsNonDeferredTools — builtin tools are not evictable, ever.
func TestEvictionNeverTargetsNonDeferredTools(t *testing.T) {
	reg := NewRegistry()
	for _, tool := range CreateDefaultTools().Registry.ListTools() {
		reg.Register(tool)
	}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("mcp__x__t%03d", i)
		reg.Register(&realisticMCPTool{name: name, desc: "d"})
		reg.MarkDiscovered(name) // without this nothing is resident and nothing is evictable
	}
	reg.SetEvictionPolicy(EvictionPolicy{HighWatermark: 2, LowWatermark: 1})

	ev := reg.MaybeEvict()
	if ev == nil {
		t.Fatal("expected eviction of deferred tools")
	}
	for _, n := range ev.Names {
		// NB: variable must not be named `t` — that shadows the *testing.T.
		if tool := reg.Get(n); tool != nil && !isDeferred(tool) {
			t.Errorf("evicted non-deferred builtin tool %q", n)
		}
	}
	// Builtins must all still be present.
	for _, builtin := range []string{"ReadFile", "WriteFile", "EditFile", "Bash", "Glob", "Grep"} {
		if reg.Get(builtin) == nil {
			t.Errorf("builtin %q disappeared", builtin)
		}
	}
}

// ── 验收标准 3：同义工具混淆（任务 T7）────────────────────────────────────

// synonymFamily is one group of near-identical tools — the scenario where a model picks the
// wrong one and eviction could plausibly make it worse (the right tool was evicted, the
// near-identical decoy is still resident).
type synonymFamily struct {
	names   []string
	correct string // the one a well-behaved selector should pick for this intent
	keyword string // what distinguishes it from its siblings
}

var synonymFamilies = []synonymFamily{
	{
		names:   []string{"mcp__db__query_rows", "mcp__db__query_rows_stream", "mcp__db__query_rows_paged"},
		correct: "mcp__db__query_rows",
		keyword: "query rows",
	},
	{
		names:   []string{"mcp__fs__read_file", "mcp__fs__read_file_async", "mcp__fs__read_file_cached"},
		correct: "mcp__fs__read_file",
		keyword: "read file",
	},
	{
		names:   []string{"mcp__git__commit", "mcp__git__commit_amend", "mcp__git__commit_signoff"},
		correct: "mcp__git__commit",
		keyword: "commit",
	},
	{
		names:   []string{"mcp__http__get", "mcp__http__get_cached", "mcp__http__get_retry"},
		correct: "mcp__http__get",
		keyword: "get",
	},
	{
		names:   []string{"mcp__log__search", "mcp__log__search_recent", "mcp__log__search_structured"},
		correct: "mcp__log__search",
		keyword: "search",
	},
}

// newSynonymRegistry registers all 5 families (15 tools) as deferred.
func newSynonymRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(&ToolSearchTool{Registry: reg, Protocol: "anthropic"})
	for _, fam := range synonymFamilies {
		for _, n := range fam.names {
			reg.Register(&synonymMCPTool{name: n, base: fam.keyword})
		}
	}
	return reg
}

// TestSynonymToolsRediscoveryAfterEviction is the core of 验收标准 3.
//
// The failure mode this guards against: eviction removes a tool, ToolSearch can no longer find it,
// and the model silently picks a near-identical decoy that is still resident — a correctness
// regression that a byte-count benchmark would never catch.
func TestSynonymToolsRediscoveryAfterEviction(t *testing.T) {
	reg := newSynonymRegistry()
	search := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}

	// Load everything, then evict down to 4 so most tools are back in the pool.
	for _, fam := range synonymFamilies {
		for _, n := range fam.names {
			reg.MarkDiscovered(n)
		}
	}
	reg.SetEvictionPolicy(EvictionPolicy{HighWatermark: 6, LowWatermark: 4, ActiveExemptExecutions: 2})
	ev := reg.MaybeEvict()
	if ev == nil || ev.Evicted == 0 {
		t.Fatal("expected an eviction round")
	}
	t.Logf("驱逐了 %d 个工具，剩余常驻 %d", ev.Evicted, reg.DiscoveredCount())

	// For every evicted tool, a keyword search must be able to find it again — including finding
	// the CORRECT member of its family rather than only a decoy.
	for _, fam := range synonymFamilies {
		for _, name := range fam.names {
			if reg.IsDiscovered(name) {
				continue // still resident, nothing to rediscover
			}
			res := search.Execute(context.Background(), map[string]any{"query": "select:" + name})
			if res.IsError {
				t.Errorf("ToolSearch failed to reload %q: %s", name, res.Output)
				continue
			}
			if !reg.IsDiscovered(name) {
				t.Errorf("evicted tool %q was not reloaded by an exact-name search", name)
			}
		}
	}

	// Keyword (not exact-name) search must still surface each family's members, so the model can
	// find the right one by intent rather than by remembering a name.
	for _, fam := range synonymFamilies {
		res := search.Execute(context.Background(), map[string]any{"query": fam.keyword, "max_results": 10})
		if res.IsError {
			t.Errorf("keyword search for %q failed: %s", fam.keyword, res.Output)
			continue
		}
		if !strings.Contains(res.Output, fam.correct) {
			t.Errorf("keyword search %q did not surface the correct tool %q", fam.keyword, fam.correct)
		}
	}
}

// TestSynonymSelectionUnaffectedByEviction compares "which tools are findable" before and after
// eviction — the mechanical proxy for 选择准确率 that doesn't need a live model.
func TestSynonymSelectionUnaffectedByEviction(t *testing.T) {
	// Baseline: everything resident, no eviction.
	baseline := newSynonymRegistry()
	for _, fam := range synonymFamilies {
		for _, n := range fam.names {
			baseline.MarkDiscovered(n)
		}
	}
	baselineResident := map[string]bool{}
	for _, s := range baseline.GetAllSchemas("anthropic") {
		if n, ok := s["name"].(string); ok {
			baselineResident[n] = true
		}
	}

	// Evicted state.
	evicted := newSynonymRegistry()
	for _, fam := range synonymFamilies {
		for _, n := range fam.names {
			evicted.MarkDiscovered(n)
		}
	}
	evicted.SetEvictionPolicy(EvictionPolicy{HighWatermark: 8, LowWatermark: 4, ActiveExemptExecutions: 2})
	evicted.MaybeEvict()

	// The key property: eviction must not make any family UNREACHABLE. Every tool that was
	// reachable before must still be discoverable (resident or via ToolSearch) after.
	search := &ToolSearchTool{Registry: evicted, Protocol: "anthropic"}
	for _, fam := range synonymFamilies {
		for _, n := range fam.names {
			if !baselineResident[n] {
				continue
			}
			if evicted.IsDiscovered(n) {
				continue
			}
			// Not resident → must be findable again.
			res := search.Execute(context.Background(), map[string]any{"query": "select:" + n})
			if res.IsError || !evicted.IsDiscovered(n) {
				t.Errorf("tool %q became unreachable after eviction — 选择准确率会下降", n)
			}
		}
	}

	// And the correct member of each family must remain reachable in every case.
	for _, fam := range synonymFamilies {
		reachable := evicted.IsDiscovered(fam.correct)
		if !reachable {
			search.Execute(context.Background(), map[string]any{"query": "select:" + fam.correct})
			reachable = evicted.IsDiscovered(fam.correct)
		}
		if !reachable {
			t.Errorf("correct tool %q is unreachable after eviction", fam.correct)
		}
	}
}

// ── 测试桩 ───────────────────────────────────────────────────────────────

// namedStubTool is a minimal deferred tool used for clock-ordering tests.
type namedStubTool struct{ name string }

func (t *namedStubTool) Name() string           { return t.name }
func (t *namedStubTool) Description() string    { return "stub " + t.name }
func (t *namedStubTool) Category() ToolCategory { return CategoryCommand }
func (t *namedStubTool) ShouldDefer() bool      { return true }
func (t *namedStubTool) Execute(context.Context, map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}
func (t *namedStubTool) Schema() map[string]any {
	return map[string]any{
		"name":         t.name,
		"description":  t.Description(),
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// synonymMCPTool is a deferred tool whose description intentionally overlaps its siblings, which is
// what makes the confusion scenario meaningful.
type synonymMCPTool struct {
	name string
	base string
}

func (t *synonymMCPTool) Name() string { return t.name }
func (t *synonymMCPTool) Description() string {
	return t.base + " operation (variant of " + t.base + ")"
}
func (t *synonymMCPTool) Category() ToolCategory { return CategoryCommand }
func (t *synonymMCPTool) ShouldDefer() bool      { return true }
func (t *synonymMCPTool) Execute(context.Context, map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}
func (t *synonymMCPTool) Schema() map[string]any {
	return map[string]any{
		"name":         t.name,
		"description":  t.Description(),
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
