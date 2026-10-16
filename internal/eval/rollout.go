package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tencoder/internal/agent"
	"tencoder/internal/config"
	"tencoder/internal/conversation"
	"tencoder/internal/filehistory"
	"tencoder/internal/llm"
	"tencoder/internal/memory"
	"tencoder/internal/permissions"
	"tencoder/internal/prompt"
	"tencoder/internal/session"
	"tencoder/internal/tools"
)

// RolloutResult 是一次 rollout 的产出。
type RolloutResult struct {
	InstanceID   string
	ModelPatch   string // 剔除测试文件后的 unified diff
	RawPatch     string // git diff 原始输出
	TotalTurns   int
	InputTokens  int
	OutputTokens int
	Trajectory   string // 轨迹 JSONL 文件路径
	Duration     time.Duration
	Err          string
}

// RolloutRunner 以无头模式驱动 Tencoder Agent 完成单个实例。
type RolloutRunner struct {
	// Provider 使用的模型配置；nil 时取 config.LoadConfig 的第一个 provider
	Provider *config.ProviderConfig
	// ModelNameOrPath 写入 predictions 的模型标识
	ModelNameOrPath string
	// TrajectoryDir 轨迹输出目录（SWE-bench 提交要求附带轨迹）；空则不落盘
	TrajectoryDir string
	// Logw 过程日志输出（可传 os.Stderr）
	Logw interface{ Write([]byte) (int, error) }
}

// swebenchWorkflowInstruction 是 SWE-bench 协议允许的通用工作流指令
// （不含任何实例特定信息，与 SWE-agent / OpenHands 的合规 prompt 对齐）。
const swebenchWorkflowInstruction = `You are a software engineer resolving a real GitHub issue in the repository checked out at your working directory.

General workflow:
1. Explore the repository to understand the codebase structure and locate the relevant code.
2. Reproduce the bug or reason carefully about the issue description.
3. Implement the minimal, correct fix. Do NOT modify existing test files.
4. You may run tests or scripts to verify your change.
5. When you are confident the issue is resolved, stop making further changes.

Your final git diff will be extracted automatically; you do not need to commit.`

// Run 执行一次 rollout：准备基线工作区 → 无头跑 Agent → 提取补丁 → 清理工作区。
// workRoot 是各实例工作区的父目录（每个实例一个子目录，按 instance_id 命名）。
func (r *RolloutRunner) Run(ctx context.Context, inst *Instance, workRoot string) (*RolloutResult, error) {
	start := time.Now()
	res := &RolloutResult{InstanceID: inst.InstanceID}

	workDir, err := PrepareBaseWorkdir(ctx, inst, workRoot)
	if err != nil {
		return res, fmt.Errorf("prepare workdir: %w", err)
	}

	// Agent 及其工具均以 workDir 为根：agent.New 内部取 os.Getwd()，
	// 因此先 chdir 再组装，结束后恢复（eval 进程生命周期内可接受）。
	prevWd, _ := os.Getwd()
	if err := os.Chdir(workDir); err != nil {
		return res, err
	}
	defer func() {
		_ = os.Chdir(prevWd)
		// 恢复干净基线，供评测阶段整体拷入容器
		resetToBase(ctx, workDir, inst.BaseCommit)
	}()

	ag, conv, cleanup, err := r.buildAgent(ctx, workDir, inst)
	if err != nil {
		return res, err
	}
	defer cleanup()

	if err := prepareTrajectory(r.TrajectoryDir, inst); err != nil {
		return res, err
	}

	conv.AddUserMessage(inst.ProblemStatement + "\n\n" + swebenchWorkflowInstruction)

	events := ag.Run(ctx, conv)
	r.consumeEvents(events, res)
	res.Duration = time.Since(start)

	// 提取补丁：git add -A 使新增文件也进入 diff，随后立即还原暂存区
	patch, perr := gitDiffAll(ctx, workDir)
	if perr != nil {
		return res, fmt.Errorf("extract patch: %w", perr)
	}
	res.RawPatch = patch
	res.ModelPatch = StripTestFiles(patch, TestFilePaths(inst.TestPatch))
	return res, nil
}

// buildAgent 组装无头 Agent，依赖注入路径与 internal/remote 的 initAgent 一致
// （同一套 DI，仅去掉 TUI/MCP/Teams 等交互组件）。
func (r *RolloutRunner) buildAgent(ctx context.Context, workDir string, inst *Instance) (*agent.Agent, *conversation.Manager, func(), error) {
	prov := r.Provider
	if prov == nil {
		cfg, err := config.LoadConfig("")
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load config: %w", err)
		}
		if len(cfg.Providers) == 0 {
			return nil, nil, nil, fmt.Errorf("no provider in config")
		}
		prov = &cfg.Providers[0]
	}

	wd := workDir
	def := tools.CreateDefaultToolsWithWorkDir(wd)

	env := prompt.DetectEnvironment(wd)
	env.Model = prov.Model
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		MemorySection: memory.LoadAutoMemoryPrompt(wd),
	})

	client, err := llm.NewClient(prov, systemPrompt)
	if err != nil {
		return nil, nil, nil, err
	}
	llm.ResolveContextWindow(ctx, prov)

	conv := conversation.NewManager()
	sessionID := session.NewID()
	fh := filehistory.New(wd, sessionID)
	def.EditFile.FileHistory = fh
	def.WriteFile.FileHistory = fh

	ag := agent.New(client, def.Registry, prov.Protocol)
	ag.WorkDir = wd
	ag.ContextWindow = prov.GetContextWindow()
	ag.MaxOutputTokens = prov.GetMaxOutputTokens()
	ag.FileHistory = fh
	ag.SetSessionID(sessionID)

	// 评测场景：bypass 权限 + 无交互。沙箱保留项目根写权限，避免误写工作区外。
	ag.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(wd),
		nil,
		permissions.ModeBypass,
	)

	return ag, conv, func() {}, nil
}

// consumeEvents 消费 Agent 事件流直到结束：自动应答权限/提问事件（无头），
// 统计 token 与轮次，可选写轨迹 JSONL。
func (r *RolloutRunner) consumeEvents(events <-chan agent.AgentEvent, res *RolloutResult) {
	var trajFile *os.File
	if r.TrajectoryDir != "" && res.InstanceID != "" {
		path := filepath.Join(r.TrajectoryDir, SanitizeID(res.InstanceID)+".jsonl")
		if f, err := os.Create(path); err == nil {
			defer f.Close()
			trajFile = f
			res.Trajectory = path
		}
	}
	writeTraj := func(typ string, data any) {
		if trajFile == nil {
			return
		}
		trajFile.Write(marshalJSONLine(map[string]any{"type": typ, "data": data, "ts": time.Now().Unix()}))
		trajFile.Write([]byte("\n"))
	}

	for ev := range events {
		switch e := ev.(type) {
		case agent.LoopComplete:
			res.TotalTurns = e.TotalTurns
			writeTraj("loop_complete", map[string]any{"total_turns": e.TotalTurns})
		case agent.ErrorEvent:
			res.Err = e.Message
			writeTraj("error", e.Message)
		case agent.UsageEvent:
			res.InputTokens += e.InputTokens
			res.OutputTokens += e.OutputTokens
			writeTraj("usage", map[string]int{"input": e.InputTokens, "output": e.OutputTokens})
		case agent.PermissionRequestEvent:
			// 无头评测：全部放行（checker 已是 bypass，这里是兜底）
			select {
			case e.ResponseCh <- agent.PermAllow:
			default:
			}
			writeTraj("permission_auto_allow", e.ToolName)
		case agent.AskUserQuestionEvent:
			// 无头评测：自动作答（协议禁止实例特定的用户交互）
			select {
			case e.ResponseCh <- map[string]string{}:
			default:
			}
			writeTraj("ask_user_auto_answer", nil)
		case agent.StreamText:
			writeTraj("stream_text", e.Text)
		case agent.ToolUseEvent:
			writeTraj("tool_use", map[string]any{"name": e.ToolName, "args": e.Args})
		case agent.ToolResultEvent:
			writeTraj("tool_result", map[string]any{"name": e.ToolName, "is_error": e.IsError, "output": e.Output})
		case agent.CompactEvent:
			writeTraj("compact", e.Message)
		case agent.RetryEvent:
			writeTraj("retry", e.Reason)
		}
	}
}

// PrepareBaseWorkdir 准备基线工作区：clone 仓库（blob 过滤加速）并 checkout base_commit。
// 已存在的工作区会被强制重置到 base_commit 并清理未跟踪文件，保证幂等。
func PrepareBaseWorkdir(ctx context.Context, inst *Instance, workRoot string) (string, error) {
	dir := filepath.Join(workRoot, SanitizeID(inst.InstanceID))
	repoURL := "https://github.com/" + inst.Repo + ".git"
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(workRoot, 0o755); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, "git", "clone", "--filter=blob:none", repoURL, dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone %s: %v: %s", repoURL, err, tail(string(out), 400))
		}
	}
	if err := checkoutCommit(ctx, dir, inst.BaseCommit); err != nil {
		return "", err
	}
	return dir, nil
}

func checkoutCommit(ctx context.Context, dir, commit string) error {
	for _, args := range [][]string{
		{"fetch", "origin", commit},
		{"checkout", "-f", commit},
		{"clean", "-fd"},
	} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			// fetch 允许失败（commit 已在本地）
			if args[0] == "fetch" {
				continue
			}
			return fmt.Errorf("git %v in %s: %v: %s", args, dir, err, tail(string(out), 400))
		}
	}
	return nil
}

// resetToBase 把工作区重置回基线（rollout 结束后调用，供评测整体拷贝）。
func resetToBase(ctx context.Context, dir, commit string) {
	_ = checkoutCommit(ctx, dir, commit)
}

// gitDiffAll 返回包含未跟踪新文件的完整 unified diff。
func gitDiffAll(ctx context.Context, dir string) (string, error) {
	_ = exec.CommandContext(ctx, "git", "-C", dir, "add", "-A").Run()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "--no-pager", "diff", "--cached", "HEAD", "--text")
	out, err := cmd.Output()
	// 还原暂存区，避免污染后续评测拷贝
	_ = exec.CommandContext(ctx, "git", "-C", dir, "reset", "-q").Run()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// prepareTrajectory 确保轨迹目录存在。
func prepareTrajectory(dir string, inst *Instance) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
