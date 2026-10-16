package eval

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EvalResult 是单个实例的评测结论。
type EvalResult struct {
	InstanceID  string
	Resolved    bool
	Missing     []string // 未通过的 F2P/P2P 测试项
	EmptyPatch  bool
	ApplyFailed string // 补丁应用失败原因（空 = 成功）
	Log         string // 测试日志（截断存储）
	Duration    time.Duration
	Container   string
	Err         string
}

// RunOutput 一次容器化测试运行的产出。
type RunOutput struct {
	Log         string
	ApplyFailed string
	Container   string
}

// maxLogBytes 报告中保留的日志上限（完整日志落在 run 目录的 log 文件里）。
const maxLogBytes = 256 * 1024

// Evaluator 在 Docker 容器内执行补丁验证与测试。
// 流程对齐 SWE-bench harness：干净基线代码拷入 /testbed → 应用补丁
// （model/test/gold 按场景组合）→ 执行测试命令 → 解析日志 → 判定 resolved。
type Evaluator struct {
	Docker    *Docker
	Namespace string // DockerHub 命名空间，默认 starryzhang
	// WorkdirInContainer 容器内仓库路径，官方约定 /testbed
	WorkdirInContainer string
}

func (e *Evaluator) testbed() string {
	if e.WorkdirInContainer != "" {
		return e.WorkdirInContainer
	}
	return "/testbed"
}

func (e *Evaluator) docker() *Docker {
	if e.Docker == nil {
		e.Docker = &Docker{}
	}
	return e.Docker
}

// Evaluate 评测 model_patch 是否解决实例。
// baseDir 必须是已重置到 base_commit 的干净工作区。
func (e *Evaluator) Evaluate(ctx context.Context, inst *Instance, baseDir, modelPatch string) (*EvalResult, error) {
	res := &EvalResult{InstanceID: inst.InstanceID}
	if strings.TrimSpace(modelPatch) == "" {
		res.EmptyPatch = true
		return res, nil
	}

	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	out, err := e.RunPatched(ctx, inst, baseDir, []string{modelPatch}, nil)
	res.Container = out.Container
	if err != nil {
		res.Err = err.Error()
		return res, nil
	}
	if out.ApplyFailed != "" {
		res.ApplyFailed = out.ApplyFailed
		return res, nil
	}
	res.Log = out.Log

	statuses := ParseStatuses(out.Log, inst.LogParser)
	resolved, missing := IsResolved(inst, statuses)
	res.Resolved = resolved
	res.Missing = missing
	return res, nil
}

// RunPatched 执行一次容器化运行：拷入基线代码 → 应用 test_patch 与
// extraPatches（顺序执行，model_patch/gold_patch 由调用方决定）→ 跑测试命令。
// testCmds 为空时用 EffectiveTestCmds 推导。
func (e *Evaluator) RunPatched(ctx context.Context, inst *Instance, baseDir string, extraPatches []string, testCmds []string) (*RunOutput, error) {
	out := &RunOutput{}
	d := e.docker()
	image := ResolveImage(inst, e.Namespace)
	name := "mew-eval-" + SanitizeID(inst.InstanceID) + "-" + fmt.Sprint(time.Now().UnixNano()%1e6)
	out.Container = name

	if len(testCmds) == 0 {
		testCmds = EffectiveTestCmds(inst)
	}
	if len(testCmds) == 0 {
		return out, fmt.Errorf("no test commands available for %s", inst.InstanceID)
	}

	// 1. 启动容器（本地无镜像时自动 pull 一次再重试）
	if _, err := d.StartContainer(ctx, image, name); err != nil {
		if perr := d.Pull(ctx, image); perr != nil {
			return out, fmt.Errorf("pull %s: %w", image, perr)
		}
		if _, err := d.StartContainer(ctx, image, name); err != nil {
			return out, fmt.Errorf("start container from %s: %w", image, err)
		}
	}
	defer d.RemoveContainer(context.WithoutCancel(ctx), name)

	// 2. 拷入干净基线代码
	if err := d.CopyToDir(ctx, name, baseDir, e.testbed()); err != nil {
		return out, fmt.Errorf("copy testbed: %w", err)
	}

	// 3. 应用补丁：先 model/gold 补丁（stdin 管入），最后 test_patch（harness 专属）
	for i, patch := range extraPatches {
		if strings.TrimSpace(patch) == "" {
			continue
		}
		if err := e.applyPatch(ctx, name, patch); err != nil {
			out.ApplyFailed = fmt.Sprintf("patch #%d: %s", i, err.Error())
			return out, nil
		}
	}
	if err := e.applyPatch(ctx, name, inst.TestPatch); err != nil {
		out.ApplyFailed = "test_patch: " + err.Error()
		return out, nil
	}

	// 4. 执行测试命令，聚合输出
	var logBuf strings.Builder
	for _, cmd := range testCmds {
		o, err := d.Exec(ctx, name, e.testbed(), cmd, nil)
		logBuf.WriteString("$ " + cmd + "\n" + o + "\n")
		if err != nil {
			logBuf.WriteString("[exit-error] " + err.Error() + "\n")
		}
	}
	fullLog := logBuf.String()
	out.Log = fullLog
	return out, nil
}

// applyPatch 通过 stdin 把 unified diff 管入容器内的 git apply；
// 失败时兜底 patch -p1（容忍目标路径前缀差异）。
func (e *Evaluator) applyPatch(ctx context.Context, container, patch string) error {
	if _, err := e.docker().Exec(ctx, container, e.testbed(), "git apply --whitespace=nowarn -", strings.NewReader(patch)); err != nil {
		if _, err2 := e.docker().Exec(ctx, container, e.testbed(), "patch -p1 --fuzz=5", strings.NewReader(patch)); err2 != nil {
			return fmt.Errorf("git apply: %v; patch -p1: %v", firstNonEmpty(err.Error(), "nil"), firstNonEmpty(err2.Error(), "nil"))
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
