// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package llm

import (
	"context"
	"fmt"
	"strings"

	"tencoder/internal/config"
	"tencoder/internal/conversation"
)

// 阶段五 · 任务 5.1/5.2：任务分级多模型动态路由。
//
// ── 为什么是声明式，不是自动分类 ──
//
// 两个被否掉的方案，各自的死法都很干脆：
//
//	LLM 自判档位 —— 为省钱先花钱。省下来的钱扣掉这次分类调用就所剩无几，
//	                 而且分类本身可能判错。
//	关键词规则   —— 不可靠，且面试里难以自圆其说（"你怎么知道这条规则是对的"）。
//
// 声明式的理由是**调用方最清楚自己在干什么活**。写工具结果摘要的那个函数，
// 百分之百知道这是 light 档的活——它不需要再花钱问一次。
// 代价是要在五个装配点各写一个 TaskClass，但那是**声明**，不是**推断**，零运行时开销。

// Tier is a model class. Two tiers only.
type Tier string

const (
	// TierLight is the cheap/fast tier: side queries, format checks, exploration.
	TierLight Tier = "light"
	// TierStrong is the capable tier: patch generation, architecture decisions, conflict merges.
	TierStrong Tier = "strong"
)

// TaskClass is the declared kind of work. The baseline tier comes from this enum alone; the other
// two routing signals only ever RAISE the tier (任务书决策 A: 拿不准就 strong).
type TaskClass string

const (
	// ClassMemoryRecall picks which memories are relevant to a query.
	ClassMemoryRecall TaskClass = "memory_recall"
	// ClassLogSummary compresses an oversized tool result.
	ClassLogSummary TaskClass = "log_summary"
	// ClassFormatCheck runs gofmt-style checks and reports diffs.
	ClassFormatCheck TaskClass = "format_check"
	// ClassExplore reads and searches a codebase without changing it.
	ClassExplore TaskClass = "explore"

	// ClassPatchGen writes or edits code.
	ClassPatchGen TaskClass = "patch_gen"
	// ClassArchDecision decides structure, interfaces, trade-offs.
	ClassArchDecision TaskClass = "arch_decision"
	// ClassConflictMerge reconciles divergent edits.
	ClassConflictMerge TaskClass = "conflict_merge"
	// ClassCrossProject modifies another registered project.
	ClassCrossProject TaskClass = "cross_project"
)

// baselineTier is the single source of truth for "which class starts where".
//
// The split is deliberately conservative: anything that WRITES code, decides structure, merges
// conflicting work, or touches another project starts at strong. A light-tier mistake on a
// read-only exploration costs a retry; the same mistake on a patch costs correctness — and the
// quality gate only catches syntax, not logic (任务书风险表: 明确定位门禁是语法层安全网).
var baselineTier = map[TaskClass]Tier{
	ClassMemoryRecall:  TierLight,
	ClassLogSummary:    TierLight,
	ClassFormatCheck:   TierLight,
	ClassExplore:       TierLight,
	ClassPatchGen:      TierStrong,
	ClassArchDecision:  TierStrong,
	ClassConflictMerge: TierStrong,
	ClassCrossProject:  TierStrong,
}

// BaselineTier returns the declared baseline tier for a class.
//
// An UNKNOWN class resolves to strong. That is the fail-safe direction: a new call site that
// forgets to declare its class should cost money, not correctness.
func BaselineTier(c TaskClass) Tier {
	if t, ok := baselineTier[c]; ok {
		return t
	}
	return TierStrong
}

// KnownClass reports whether c is one of the declared classes (used by tests + telemetry to spot
// undeclared call sites).
func KnownClass(c TaskClass) bool {
	_, ok := baselineTier[c]
	return ok
}

// AllTaskClasses returns every declared class, for docs/telemetry.
func AllTaskClasses() []TaskClass {
	return []TaskClass{
		ClassMemoryRecall, ClassLogSummary, ClassFormatCheck, ClassExplore,
		ClassPatchGen, ClassArchDecision, ClassConflictMerge, ClassCrossProject,
	}
}

// RouteHints carries the two non-declaration signals.
type RouteHints struct {
	// EstimatedToolCalls is the caller's estimate of how many tool invocations this task needs.
	// Zero means "unknown" and does not trigger the budget rule.
	EstimatedToolCalls int
	// ContextTokens is the approximate size of the context this task starts with.
	// Zero means "unknown".
	ContextTokens int
	// ExplicitModel, when non-empty, is a caller/model override that wins over routing entirely
	// (a spec that says `model: opus` means opus, whatever the signals say).
	ExplicitModel string
}

// RoutingConfig is the 阶段五 configuration surface.
type RoutingConfig struct {
	// Disabled forces every call to the strong tier — the A/B control group for the measurement
	// (决策 C: 对照组 = 路由关闭). Keeping it as a switch rather than a build tag means the same
	// binary produces both groups, so the only variable is the flag.
	Disabled bool
	// ToolCallBudget: EstimatedToolCalls above this promotes the tier.
	ToolCallBudget int
	// ContextTokensThreshold: ContextTokens above this promotes the tier.
	//
	// Why context size is a signal at all: a small model given a 100k-token transcript
	// degrades sharply — it is not "slower", it is wrong, because attention over that much
	// context is where the capability gap shows first.
	ContextTokensThreshold int
	// LightModel / StrongModel are the model IDs (or short aliases like "haiku") per tier.
	// Empty means "use the session's own model", which disables routing for that tier.
	LightModel  string
	StrongModel string
}

// DefaultRoutingConfig returns 任务书's defaults.
func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		Disabled:               false,
		ToolCallBudget:         20,
		ContextTokensThreshold: 60000,
		LightModel:             "haiku",
		StrongModel:            "", // same as the session model
	}
}

func (c RoutingConfig) normalized() RoutingConfig {
	d := DefaultRoutingConfig()
	if c.ToolCallBudget <= 0 {
		c.ToolCallBudget = d.ToolCallBudget
	}
	if c.ContextTokensThreshold <= 0 {
		c.ContextTokensThreshold = d.ContextTokensThreshold
	}
	return c
}

// RouteResult is what Resolve returns alongside the client.
type RouteResult struct {
	Client Client
	Tier   Tier
	// Reason is a short human-readable explanation of the decision, e.g.
	// "declared=patch_gen→strong; promoted by tool budget (35>20)".
	// Surfaced in the debug log and the eval report — a routing system whose decisions can't be
	// explained can't be tuned.
	Reason string
	// Model is the concrete model ID chosen.
	Model string
	// Promoted reports whether a non-declaration signal raised the tier above the baseline.
	Promoted bool
	// Suppressed reports that routing was disabled and the strong tier was forced.
	Suppressed bool
}

// Router picks a model tier per task class and builds the client for it.
//
// Concurrency-safe: sub-agent goroutines spawn concurrently and each asks for a client.
// Stateless apart from the injected provider config, which is copied per Resolve (same pattern as
// NewModelResolver) so no caller can mutate the session's own provider.
type Router struct {
	baseCfg config.ProviderConfig
	cfg     RoutingConfig
}

// NewRouter constructs a Router over a base provider config. A nil baseCfg yields a nil Router,
// whose methods no-op to the strong path — callers need no nil checks.
func NewRouter(baseCfg *config.ProviderConfig, cfg RoutingConfig) *Router {
	if baseCfg == nil {
		return nil
	}
	return &Router{baseCfg: *baseCfg, cfg: cfg.normalized()}
}

// Config returns the effective configuration (for diagnostics/tests).
func (r *Router) Config() RoutingConfig {
	if r == nil {
		return RoutingConfig{}
	}
	return r.cfg
}

// Decide is the routing decision as a pure function of (class, hints, config) — separated from
// Resolve so the decision can be tested exhaustively without constructing clients.
func Decide(class TaskClass, hints RouteHints, cfg RoutingConfig) (tier Tier, reason string, promoted, suppressed bool) {
	cfg = cfg.normalized()

	base := BaselineTier(class)
	if cfg.Disabled {
		// Control group: everything runs on the session model. Reason is explicit so a log reader
		// can tell "routing said no" apart from "routing was off".
		return TierStrong, fmt.Sprintf("routing disabled; %s forced to strong", class), false, true
	}

	parts := []string{fmt.Sprintf("declared=%s→%s", class, base)}

	// Signal 1: estimated tool budget.
	overBudget := hints.EstimatedToolCalls > 0 && hints.EstimatedToolCalls > cfg.ToolCallBudget
	if overBudget {
		parts = append(parts, fmt.Sprintf("promoted by tool budget (%d>%d)", hints.EstimatedToolCalls, cfg.ToolCallBudget))
	}

	// Signal 2: context size.
	bigContext := hints.ContextTokens > 0 && hints.ContextTokens > cfg.ContextTokensThreshold
	if bigContext {
		parts = append(parts, fmt.Sprintf("promoted by context size (%d>%d)", hints.ContextTokens, cfg.ContextTokensThreshold))
	}

	promoted = overBudget || bigContext
	// Only one promotion step: a task that trips both signals is not "very strong", it is strong.
	if promoted && base == TierLight {
		return TierStrong, strings.Join(parts, "; "), true, false
	}
	return base, strings.Join(parts, "; "), false, false
}

// Resolve picks a client for a task class.
//
// API shape follows 任务书: Resolve(class, hints) → (client, tier, reason). The extra RouteResult
// fields (model/promoted/suppressed) are added because the eval report needs them and returning a
// 3-tuple plus a 4th value is not expressible in Go.
func (r *Router) Resolve(class TaskClass, hints RouteHints) RouteResult {
	if r == nil {
		return RouteResult{Tier: TierStrong, Reason: "no router wired"}
	}

	tier, reason, promoted, suppressed := Decide(class, hints, r.cfg)

	// An explicit model override wins over routing entirely: a sub-agent definition that says
	// `model: opus` means opus. Routing adjusts the DEFAULT, it does not overrule the user.
	if hints.ExplicitModel != "" && hints.ExplicitModel != "inherit" {
		modelID, resolved := modelAliases[hints.ExplicitModel]
		if !resolved {
			modelID = hints.ExplicitModel
		}
		cfg := r.baseCfg
		cfg.Model = modelID
		client, err := NewClient(&cfg, "")
		if err == nil {
			return RouteResult{
				Client: client, Tier: TierStrong, Model: modelID,
				Reason: fmt.Sprintf("%s; explicit model %q overrides routing", reason, hints.ExplicitModel),
			}
		}
		// An unusable explicit model must not fail the task — fall through to the tier default.
		reason = fmt.Sprintf("%s; explicit model %q unusable (%v), using %s", reason, hints.ExplicitModel, err, tier)
	}

	target := r.baseCfg
	switch tier {
	case TierLight:
		if r.cfg.LightModel == "" {
			// No light model configured → stay on the session model. Silently degrading to strong
			// is the correct failure mode: worse cost, same result.
			return RouteResult{Client: r.sessionClient(), Tier: TierStrong, Model: target.Model,
				Reason: reason + "; no light model configured"}
		}
		modelID, ok := modelAliases[r.cfg.LightModel]
		if !ok {
			modelID = r.cfg.LightModel
		}
		target.Model = modelID
	default:
		if r.cfg.StrongModel != "" {
			modelID, ok := modelAliases[r.cfg.StrongModel]
			if !ok {
				modelID = r.cfg.StrongModel
			}
			target.Model = modelID
		}
	}

	client, err := NewClient(&target, "")
	if err != nil {
		// Falling back to the session client keeps the task runnable. A routing failure must not
		// become an outage.
		return RouteResult{Client: r.sessionClient(), Tier: TierStrong, Model: r.baseCfg.Model,
			Reason:   fmt.Sprintf("%s; client build failed (%v), fell back to session model", reason, err),
			Promoted: promoted, Suppressed: suppressed}
	}
	return RouteResult{Client: client, Tier: tier, Model: target.Model, Reason: reason,
		Promoted: promoted, Suppressed: suppressed}
}

func (r *Router) sessionClient() Client {
	if r == nil {
		return nil
	}
	c, err := NewClient(&r.baseCfg, "")
	if err != nil {
		return nil
	}
	return c
}

// SideQueryClient returns a client bound to a system prompt, routed by class.
//
// This is the entry point for the five side-call sites (决策 A 接入面 1). They all previously did
// `llm.NewClient(provider, sysPrompt)` directly, which hard-coded the session model.
func (r *Router) SideQueryClient(ctx context.Context, class TaskClass, systemPrompt string, conv *conversation.Manager) (Client, Tier, error) {
	if r == nil {
		return nil, TierStrong, nil
	}
	// A side query's "context size" hint is the conversation it is about to send — that is exactly
	// the quantity the context signal is meant to capture.
	hints := RouteHints{}
	if conv != nil {
		hints.ContextTokens = EstimateConversationTokens(conv)
	}
	res := r.Resolve(class, hints)
	if res.Client == nil {
		return nil, res.Tier, fmt.Errorf("router produced no client for %s", class)
	}
	// The side-query clients need their own system prompt; Resolve builds with an empty one
	// because it does not know the prompt. Set it here rather than threading prompts through the
	// router, which would couple routing to call-site details.
	res.Client.SetSystemPrompt(systemPrompt)
	_ = ctx
	return res.Client, res.Tier, nil
}

// EstimateConversationTokens is a cheap character-count estimate.
//
// Four chars per token is the standard rough conversion, and it is deliberately the same divisor
// used for the 阶段三 schema measurements — mixing divisors across stages would make the cost
// comparison meaningless.
func EstimateConversationTokens(conv *conversation.Manager) int {
	if conv == nil {
		return 0
	}
	chars := 0
	for _, m := range conv.GetMessages() {
		chars += len(m.Content)
		for _, tu := range m.ToolUses {
			chars += len(tu.ToolName) + 16
		}
		for _, tr := range m.ToolResults {
			chars += len(tr.Content)
		}
		for _, tb := range m.ThinkingBlocks {
			chars += len(tb.Thinking)
		}
	}
	return chars / 4
}

// RouteMetrics is the 阶段五 telemetry surface (验收标准 3 需要"轻量档占比"这个单列指标).
type RouteMetrics struct {
	Total      int
	ByTier     map[Tier]int
	ByClass    map[TaskClass]int
	Promoted   int // baseline light → strong by a signal
	Suppressed int // routing disabled
	TokenIn    int
	TokenOut   int
}

// LightShare returns the fraction of routed calls that landed on the light tier.
//
// Computed over *all* calls including suppressed ones, because the denominator must be the whole
// workload — otherwise a run with routing mostly disabled would report a flattering 90% light
// share over a handful of routed calls.
func (m RouteMetrics) LightShare() float64 {
	if m.Total == 0 {
		return 0
	}
	return float64(m.ByTier[TierLight]) / float64(m.Total)
}

// Snapshot returns a deep copy safe to hand across goroutines.
func (m RouteMetrics) Snapshot() RouteMetrics {
	out := m
	out.ByTier = make(map[Tier]int, len(m.ByTier))
	for k, v := range m.ByTier {
		out.ByTier[k] = v
	}
	out.ByClass = make(map[TaskClass]int, len(m.ByClass))
	for k, v := range m.ByClass {
		out.ByClass[k] = v
	}
	return out
}

// RecordRoute folds one resolved route into the metrics.
func (m *RouteMetrics) RecordRoute(class TaskClass, res RouteResult, in, out int) {
	m.Total++
	if m.ByTier == nil {
		m.ByTier = map[Tier]int{}
		m.ByClass = map[TaskClass]int{}
	}
	m.ByTier[res.Tier]++
	m.ByClass[class]++
	if res.Promoted {
		m.Promoted++
	}
	if res.Suppressed {
		m.Suppressed++
	}
	m.TokenIn += in
	m.TokenOut += out
}

// ParseTaskClass maps a YAML/frontmatter string to a TaskClass, tolerating case, spaces,
// hyphens and underscores. An unrecognised value returns "" (unclassified).
//
// Tolerance matters because this string is hand-written in agent definition files: rejecting
// "Patch_Gen" for being spelled differently would train users to distrust the field. Safety comes
// from the OTHER direction instead — "" routes to the strong tier.
func ParseTaskClass(s string) TaskClass {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.ReplaceAll(n, "-", "_")
	n = strings.ReplaceAll(n, " ", "_")
	for _, c := range AllTaskClasses() {
		if string(c) == n {
			return c
		}
	}
	return ""
}
