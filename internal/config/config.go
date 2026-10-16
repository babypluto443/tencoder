// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"tencoder/internal/hooks"
)

var envKeyMap = map[string]string{
	"anthropic":     "ANTHROPIC_API_KEY",
	"openai":        "OPENAI_API_KEY",
	"openai-compat": "OPENAI_API_KEY",
}

var validProtocols = map[string]bool{
	"anthropic":     true,
	"openai":        true,
	"openai-compat": true,
}

type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

type ProviderConfig struct {
	Name            string `yaml:"name"`
	Protocol        string `yaml:"protocol"`
	BaseURL         string `yaml:"base_url"`
	Model           string `yaml:"model"`
	APIKey          string `yaml:"api_key"`
	Thinking        bool   `yaml:"thinking"`
	ContextWindow   int    `yaml:"context_window"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`

	// fetchedContextWindow caches the max_input_tokens auto-pulled from the
	// provider's /v1/models endpoint (layer 2 of GetContextWindow). Populated
	// once at client init via SetFetchedContextWindow; 0 means "not fetched".
	// Not a yaml field — it's a runtime cache, never persisted.
	fetchedContextWindow int
}

// modelContextWindows maps a model-name substring to its context window
// (max input tokens). Matched from most specific to most generic; the first
// substring hit wins. Values are reasonable starting points only — they may
// drift as models are updated/renamed. When a value is wrong, set
// `context_window` in config to override (that takes top priority).
var modelContextWindows = []struct {
	substr string
	window int
}{
	{"1m", 1000000},      // also covers "-1m" suffixes (e.g. claude-...-1m)
	{"gpt-4.1", 1000000}, // GPT-4.1 family ships a 1M window
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"o1", 200000}, // OpenAI reasoning models o1 / o3 / o4
	{"o3", 200000},
	{"o4", 200000},
	{"gpt-3.5", 16385},
	{"claude", 200000},
}

// SetFetchedContextWindow records the context window auto-pulled from the
// provider (layer 2). A non-positive value is ignored so a failed fetch never
// pollutes the cache. Called once per provider at client init.
func (p *ProviderConfig) SetFetchedContextWindow(window int) {
	if window > 0 {
		p.fetchedContextWindow = window
	}
}

// lookupModelContextWindow returns the built-in mapping-table window for the
// given model via substring match (layer 3), or 0 if nothing matches.
func lookupModelContextWindow(model string) int {
	m := strings.ToLower(model)
	for _, e := range modelContextWindows {
		if strings.Contains(m, e.substr) {
			return e.window
		}
	}
	return 0
}

// GetContextWindow resolves the model's context window with four layers of
// fallback, highest priority first:
//
//  1. config-supplied context_window (> 0) — explicit override, always wins.
//  2. value auto-fetched from the provider's /v1/models endpoint, cached via
//     SetFetchedContextWindow (only Anthropic-protocol providers ever set it;
//     a failed/absent fetch leaves it 0 and is skipped).
//  3. built-in model-name → window mapping table (substring match).
//  4. conservative default (claude → 200000, otherwise → 128000).
func (p *ProviderConfig) GetContextWindow() int {
	if p.ContextWindow > 0 {
		return p.ContextWindow
	}
	if p.fetchedContextWindow > 0 {
		return p.fetchedContextWindow
	}
	if w := lookupModelContextWindow(p.Model); w > 0 {
		return w
	}
	if strings.Contains(p.Model, "claude") {
		return 200000
	}
	return 128000
}

func (p *ProviderConfig) GetMaxOutputTokens() int {
	if p.MaxOutputTokens > 0 {
		return p.MaxOutputTokens
	}
	if p.Thinking {
		return 64000
	}
	return 8192
}

func (p *ProviderConfig) ResolveAPIKey() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	envVar := envKeyMap[p.Protocol]
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}

type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	URL       string            `yaml:"url"`
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
	Env       map[string]string `yaml:"env"`
}

// SandboxConfig 控制 OS 级沙箱的配置
type SandboxConfig struct {
	Enabled        bool `yaml:"enabled"`         // 是否启用沙箱
	AutoAllow      bool `yaml:"auto_allow"`      // 沙箱内命令是否自动放行
	NetworkEnabled bool `yaml:"network_enabled"` // 是否允许网络访问
}

// ReflectionConfig 阶段二 · 目标偏移反思节点的配置。
//
// Enabled / Periodic 用 *bool 而不是 bool：Go 的 bool 零值是 false，无法区分
// 「用户显式写了 false」和「用户没写、走默认值」。反思节点默认开启、周期兜底默认关闭，
// 这两个默认值恰好一真一假，没有指针就无法同时表达「有默认」和「可覆盖」。
type ReflectionConfig struct {
	// Enabled 总开关，默认 true。显式写 false 可关闭反思节点。
	Enabled *bool `yaml:"enabled"`
	// Periodic 周期兜底检查，默认 false（事件驱动才是主机制，周期检查只用于排查漏触发）。
	Periodic *bool `yaml:"periodic"`
	// PeriodicEvery 周期兜底的回合间隔，仅 periodic=true 时生效。
	PeriodicEvery int `yaml:"periodic_every"`
	// MaxWaitMs 下一轮迭代开头等待判定的上限毫秒数，默认 5000。
	MaxWaitMs int `yaml:"max_wait_ms"`
	// DriftThreshold 连续 No 达到该次数即中断重规划，默认 3。
	DriftThreshold int `yaml:"drift_threshold"`
	// LocalizationRunThreshold 判定「定位完成」所需的连续只读工具数，默认 3。
	LocalizationRunThreshold int `yaml:"localization_run_threshold"`
}

// IsEnabled returns the effective enabled flag. Default true.
func (r ReflectionConfig) IsEnabled() bool {
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

// IsPeriodic returns the effective periodic-fallback flag. Default false.
func (r ReflectionConfig) IsPeriodic() bool {
	if r.Periodic == nil {
		return false
	}
	return *r.Periodic
}

// ToolGovernanceConfig 阶段三 · MCP 工具治理配置（驱逐 + 工具结果摘要）。
type ToolGovernanceConfig struct {
	// EvictEnabled 是否启用延迟工具的动态驱逐，默认 true。
	//
	// 关闭后行为与阶段二之前一致：schema 只装不卸。这不是「省事开关」，而是给需要
	// 稳定工具集的场景（长会话复现、prompt cache 敏感的任务）留的逃生门。
	EvictEnabled *bool `yaml:"evict_enabled"`
	// EvictHighWatermark 常驻（已装载 schema）工具数超过该值触发一轮驱逐，默认 40。
	EvictHighWatermark int `yaml:"evict_high_watermark"`
	// EvictLowWatermark 一轮驱逐砍回到该值，默认 25。必须小于高水位。
	EvictLowWatermark int `yaml:"evict_low_watermark"`
	// ActiveExemptExecutions 会话内被执行次数达到该值的工具永不驱逐，默认 2。
	ActiveExemptExecutions int `yaml:"active_exempt_executions"`
	// SummaryEnabled 是否用旁路 LLM 摘要替换机械预览，默认 false。
	//
	// 默认关是刻意的：摘要是**同步**调用（必须赶在下一轮 prompt 组装前进上下文），
	// 会给 spill 那一轮加 1~3s。摘要质量未经真实会话验证前，默认走机械预览这条
	// 「绝不会更坏」的路径；确认收益后再打开。
	SummaryEnabled *bool `yaml:"summary_enabled"`
	// SummaryTimeoutMs 单次摘要调用的超时，默认 30000。
	SummaryTimeoutMs int `yaml:"summary_timeout_ms"`
}

// IsEvictEnabled returns the effective eviction flag. Default true.
func (t ToolGovernanceConfig) IsEvictEnabled() bool {
	if t.EvictEnabled == nil {
		return true
	}
	return *t.EvictEnabled
}

// IsSummaryEnabled returns the effective summary flag. Default false.
func (t ToolGovernanceConfig) IsSummaryEnabled() bool {
	if t.SummaryEnabled == nil {
		return false
	}
	return *t.SummaryEnabled
}

// RoutingConfig 是阶段五的路由配置（任务 5.2）。
//
// 薄封装而非直接用 llm.RoutingConfig：配置包在 llm 之下，让它 import llm 会成环。
// 装配层（tui/remote）负责在两者之间搬运字段。
type RoutingConfig struct {
	// Disabled 关闭路由，全部走强档。这是 决策 C 的对照组开关——同一个二进制同时能产出
	// 实验组与对照组，唯一变量就是这个 flag，而不是构建标签或两份代码。
	Disabled *bool `yaml:"disabled"`
	// ToolCallBudget：预计工具调用数超过此值则升档。
	ToolCallBudget int `yaml:"tool_call_budget"`
	// ContextTokensThreshold：起始上下文超过此 token 数则升档。
	ContextTokensThreshold int `yaml:"context_tokens_threshold"`
	// LightModel / StrongModel 是两档的模型 ID 或短别名（haiku/sonnet/opus 等）。
	// 留空表示"沿用会话模型"，此时该档等于不路由。
	LightModel  string `yaml:"light_model"`
	StrongModel string `yaml:"strong_model"`
	// GateEnabled 打开 5.4 的语法门禁 + 升级链。默认关闭：门禁会额外跑 go build/vet/gofmt，
	// 在非 Go 项目上只是纯开销，且未验证前不应对所有任务默认生效。
	GateEnabled *bool `yaml:"gate_enabled"`
	// GateTimeoutMs 是门禁三件套的预算（毫秒）。超时按"环境问题"处理，不触发升级。
	GateTimeoutMs int `yaml:"gate_timeout_ms"`
}

// IsGateEnabled 返回门禁开关，缺省 false。
func (c RoutingConfig) IsGateEnabled() bool { return c.GateEnabled != nil && *c.GateEnabled }

type AppConfig struct {
	Providers             []ProviderConfig     `yaml:"providers"`
	PermissionMode        string               `yaml:"permission_mode"`
	MCPServers            []MCPServerConfig    `yaml:"mcp_servers"`
	Hooks                 []hooks.Hook         `yaml:"hooks"`
	Sandbox               SandboxConfig        `yaml:"sandbox"`
	EnableCoordinatorMode bool                 `yaml:"enable_coordinator_mode"`
	Reflection            ReflectionConfig     `yaml:"reflection"`
	ToolGovernance        ToolGovernanceConfig `yaml:"tool_governance"`
	Routing               RoutingConfig        `yaml:"routing"`
}

func loadSingleFile(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("Failed to parse config %s: %s", path, err)}
	}
	return &cfg, nil
}

func mergeConfig(base, override *AppConfig) *AppConfig {
	if override.Providers != nil {
		base.Providers = override.Providers
	}
	if override.PermissionMode != "" {
		base.PermissionMode = override.PermissionMode
	}
	if len(override.MCPServers) > 0 {
		byName := make(map[string]int)
		for i, s := range base.MCPServers {
			byName[s.Name] = i
		}
		for _, s := range override.MCPServers {
			if idx, ok := byName[s.Name]; ok {
				base.MCPServers[idx] = s
			} else {
				base.MCPServers = append(base.MCPServers, s)
				byName[s.Name] = len(base.MCPServers) - 1
			}
		}
	}
	base.Hooks = append(base.Hooks, override.Hooks...)
	// 沙箱配置：后加载的配置覆盖先前的
	if override.Sandbox.Enabled {
		base.Sandbox = override.Sandbox
	}
	if override.EnableCoordinatorMode {
		base.EnableCoordinatorMode = true
	}
	// 阶段二 reflection：逐字段覆盖。不能用 struct 整体赋值——那会让本地层只写一个
	// max_wait_ms 就把上层显式设置的 enabled: false 冲掉。指针字段为 nil 表示"未设置"。
	if override.Reflection.Enabled != nil {
		base.Reflection.Enabled = override.Reflection.Enabled
	}
	if override.Reflection.Periodic != nil {
		base.Reflection.Periodic = override.Reflection.Periodic
	}
	if override.Reflection.PeriodicEvery > 0 {
		base.Reflection.PeriodicEvery = override.Reflection.PeriodicEvery
	}
	if override.Reflection.MaxWaitMs > 0 {
		base.Reflection.MaxWaitMs = override.Reflection.MaxWaitMs
	}
	if override.Reflection.DriftThreshold > 0 {
		base.Reflection.DriftThreshold = override.Reflection.DriftThreshold
	}
	if override.Reflection.LocalizationRunThreshold > 0 {
		base.Reflection.LocalizationRunThreshold = override.Reflection.LocalizationRunThreshold
	}
	// 阶段三 tool_governance：同样逐字段覆盖，理由同 reflection。
	tg := override.ToolGovernance
	if tg.EvictEnabled != nil {
		base.ToolGovernance.EvictEnabled = tg.EvictEnabled
	}
	if tg.EvictHighWatermark > 0 {
		base.ToolGovernance.EvictHighWatermark = tg.EvictHighWatermark
	}
	if tg.EvictLowWatermark > 0 {
		base.ToolGovernance.EvictLowWatermark = tg.EvictLowWatermark
	}
	if tg.ActiveExemptExecutions > 0 {
		base.ToolGovernance.ActiveExemptExecutions = tg.ActiveExemptExecutions
	}
	if tg.SummaryEnabled != nil {
		base.ToolGovernance.SummaryEnabled = tg.SummaryEnabled
	}
	if tg.SummaryTimeoutMs > 0 {
		base.ToolGovernance.SummaryTimeoutMs = tg.SummaryTimeoutMs
	}
	// 阶段五 routing：逐字段覆盖，理由同 reflection / tool_governance。
	// 指针字段只在"显式写了"时覆盖——否则项目层想把门禁关掉，就得把用户层整段 routing 重写一遍。
	if override.Routing.Disabled != nil {
		base.Routing.Disabled = override.Routing.Disabled
	}
	if override.Routing.ToolCallBudget > 0 {
		base.Routing.ToolCallBudget = override.Routing.ToolCallBudget
	}
	if override.Routing.ContextTokensThreshold > 0 {
		base.Routing.ContextTokensThreshold = override.Routing.ContextTokensThreshold
	}
	if override.Routing.LightModel != "" {
		base.Routing.LightModel = override.Routing.LightModel
	}
	if override.Routing.StrongModel != "" {
		base.Routing.StrongModel = override.Routing.StrongModel
	}
	if override.Routing.GateEnabled != nil {
		base.Routing.GateEnabled = override.Routing.GateEnabled
	}
	if override.Routing.GateTimeoutMs > 0 {
		base.Routing.GateTimeoutMs = override.Routing.GateTimeoutMs
	}
	return base
}

func validateProviders(cfg *AppConfig) error {
	if len(cfg.Providers) == 0 {
		return &ConfigError{Message: "At least one provider must be configured"}
	}
	requiredFields := []string{"name", "protocol", "base_url", "model"}
	for i, p := range cfg.Providers {
		var missing []string
		values := map[string]string{
			"name":     p.Name,
			"protocol": p.Protocol,
			"base_url": p.BaseURL,
			"model":    p.Model,
		}
		for _, f := range requiredFields {
			if values[f] == "" {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			return &ConfigError{
				Message: fmt.Sprintf("Provider #%d: missing fields: %s", i+1, strings.Join(missing, ", ")),
			}
		}
		if !validProtocols[p.Protocol] {
			return &ConfigError{
				Message: fmt.Sprintf("Provider #%d: invalid protocol '%s', must be one of: anthropic, openai, openai-compat", i+1, p.Protocol),
			}
		}
	}
	return nil
}

func LoadConfig(path string) (*AppConfig, error) {
	if path != "" {
		cfg, err := loadSingleFile(path)
		if err != nil {
			return nil, err
		}
		if err := validateProviders(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("Failed to get working directory: %s", err)}
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".tencoder", "config.yaml"),
		filepath.Join(wd, ".tencoder", "config.yaml"),
		filepath.Join(wd, ".tencoder", "config.local.yaml"),
	}

	var merged *AppConfig
	for _, path := range candidates {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		layer, err := loadSingleFile(path)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = layer
		} else {
			merged = mergeConfig(merged, layer)
		}
	}

	if merged == nil {
		return nil, &ConfigError{Message: "No config file found. Expected .tencoder/config.yaml in project or ~/.tencoder/config.yaml"}
	}

	if err := validateProviders(merged); err != nil {
		return nil, err
	}
	return merged, nil
}
