// 权限确认次数回放基准：模拟一次典型开发会话的工具调用序列，
// 对比「无分级策略（每次写/命令都确认）」与「五层权限机制」的确认次数。
//
// 测试方法（确定性回放，不依赖 LLM）：
//   - 固定 41 步工具调用序列（11 读 + 11 写 + 19 命令），模拟
//     「加功能 + 编译验证」的典型会话；
//   - 五层组模拟真实用户行为：首次弹窗选 "always allow"（会话级放行）；
//   - 统计两组的 Ask（弹窗确认）与 Deny 次数。
package permissions

import (
	"path/filepath"
	"fmt"
	"testing"

	"tencoder/internal/tools"
)

type step struct {
	tool string
	args map[string]any
}

func buildSessionSteps() []step {
	var s []step
	// --- 阶段 1：读代码定位改动点（11 次读，两组都自动放行） ---
	readTargets := []string{
		"internal/agent/agent.go", "internal/tools/tool.go",
		"internal/tools/tool_search.go", "internal/mcp/mcp.go",
		"cmd/tencoder/main.go",
	}
	for _, f := range readTargets {
		s = append(s, step{"ReadFile", map[string]any{"file_path": f}})
	}
	s = append(s, step{"Glob", map[string]any{"pattern": "internal/**/*.go"}})
	s = append(s, step{"Glob", map[string]any{"pattern": "cmd/**/*.go"}})
	s = append(s, step{"Grep", map[string]any{"pattern": "GetAllSchemas"}})
	s = append(s, step{"Grep", map[string]any{"pattern": "ShouldDefer"}})

	// --- 阶段 2：实现功能（10 次项目内写 + 1 次受保护路径写） ---
	projWrites := []string{
		"internal/tools/new_feature.go", "internal/tools/new_feature_test.go",
		"internal/agent/hook.go", "internal/agent/hook_test.go",
		"internal/prompt/sections.go", "README.md",
	}
	for _, f := range projWrites {
		s = append(s, step{"WriteFile", map[string]any{"file_path": f}})
	}
	for _, f := range []string{"internal/tools/tool.go", "internal/tools/tool_search.go", "internal/agent/agent.go", "go.mod"} {
		s = append(s, step{"EditFile", map[string]any{"file_path": f}})
	}
	// 受保护路径（沙箱 denyWrite）：五层机制下始终需要确认
	s = append(s, step{"WriteFile", map[string]any{"file_path": ".tencoder/config.yaml"}})

	// --- 阶段 3：编译验证迭代（19 次命令） ---
	cmds := []string{
		// 安全只读命令（Layer 1 白名单自动放行）
		"git status", "git diff", "git log --oneline -5", "ls -la internal/tools", "cat go.mod", "go env GOMOD",
		// 构建/测试迭代（重复执行，考验会话级放行）
		"go build ./...", "go test ./internal/tools/", "go vet ./...",
		"go build ./...", "go test ./internal/tools/", "gofmt -l .",
		"go build ./...", "go test ./internal/tools/", "go vet ./...",
		"go run ./cmd/tencoder --version", "go test ./internal/agent/", "docker compose ps",
	}
	for _, c := range cmds {
		s = append(s, step{"Bash", map[string]any{"command": c}})
	}
	// 危险命令（Layer 2 拦截，验证安全不降级）
	s = append(s, step{"Bash", map[string]any{"command": "git push origin main --force"}})
	return s
}

func TestPermissionBenchmarkSession(t *testing.T) {
	steps := buildSessionSteps()
	toolReg := tools.CreateDefaultTools().Registry
	toolByName := map[string]tools.Tool{}
	for _, tl := range toolReg.ListTools() {
		toolByName[tl.Name()] = tl
	}
	sandbox := NewPathSandbox(".")

	// --- 组 A：无分级策略基线 —— 每次写 / 命令都弹窗确认 ---
	baselineAsks := 0
	for _, st := range steps {
		cat := toolByName[st.tool].Category()
		if cat == tools.CategoryWrite || cat == tools.CategoryCommand {
			baselineAsks++
		}
	}

	// --- 组 B：五层权限机制（ModeDefault + OS 沙箱 + 项目级 deny 规则 + 会话级 always-allow） ---
	// 生产配置：危险 git 操作写入项目级 deny 规则（Layer 1b/4 的规则引擎），
	// 与沙箱自动放行配合使用——沙箱负责"免确认"，规则负责"红线"。
	engine := &RuleEngine{LocalPath: filepath.Join(t.TempDir(), "permissions.local.yaml")}
	engine.AppendLocalRule(Rule{ToolName: "Bash", Pattern: "git push*--force*", Effect: RuleDeny})
	checker := NewChecker(sandbox, engine, ModeDefault)
	checker.SandboxEnabled = true
	fiveLayerAsks, denies := 0, 0
	askedOnce := map[string]bool{}
	for i, st := range steps {
		tool := toolByName[st.tool]
		d := checker.Check(tool, st.args)
		switch d.Effect {
		case Ask:
			fiveLayerAsks++
			t.Logf("step %2d %-9s ASK  (%s)", i+1, st.tool, d.Reason)
			// 用户选择 "always allow"：会话级放行该工具（Layer 4b）
			if !askedOnce[st.tool] {
				checker.AddSessionAllow(st.tool, "*")
				askedOnce[st.tool] = true
			}
		case Deny:
			denies++
			t.Logf("step %2d %-9s DENY (%s)", i+1, st.tool, d.Reason)
		}
	}

	t.Logf("")
	t.Logf("=== 会话回放统计（共 %d 步：9 读 / 11 写 / 19 命令）===", len(steps))
	t.Logf("无分级策略（写+命令全确认）: %d 次确认", baselineAsks)
	t.Logf("五层权限机制:               %d 次确认, %d 次拦截", fiveLayerAsks, denies)
	t.Logf("确认次数降低:               %.1f%%", (1-float64(fiveLayerAsks)/float64(baselineAsks))*100)

	if fiveLayerAsks >= baselineAsks {
		t.Errorf("五层机制确认次数应少于基线: %d vs %d", fiveLayerAsks, baselineAsks)
	}
	if denies == 0 {
		t.Errorf("危险命令 git push --force 应被 Layer 2 拦截")
	}
	if fiveLayerAsks > 5 {
		t.Errorf("五层机制下单会话确认应 <= 5 次，实际 %d", fiveLayerAsks)
	}
	fmt.Println("done")
}
