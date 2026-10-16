package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Deriver 从 gold patch 推导缺失的评测元数据（FAIL_TO_PASS / PASS_TO_PASS / test_cmds）。
//
// 背景：SWE-bench-Live 公开数据集为防污染清空了这些字段，官方 harness 依赖
// "gold patch 预验证"流程。本模块等价地实现该流程：
//
//	基线运行：仅应用 test_patch          → 日志 S0（哪些测试本来就挂）
//	gold 运行：应用 test_patch + gold patch → 日志 S1（修复后哪些测试过）
//
//	FAIL_TO_PASS = S0 中未通过 且 S1 中通过 的测试
//	PASS_TO_PASS = S0 中通过   且 S1 中通过 的测试
type Deriver struct {
	Evaluator *Evaluator
}

// DeriveMetadata 推导并回填 inst 的元数据。baseDir 是干净基线工作区。
// 返回两次运行的完整日志（供落盘排查）。
func (d *Deriver) DeriveMetadata(ctx context.Context, inst *Instance, baseDir string) (baselineLog, goldLog string, err error) {
	if d.Evaluator == nil {
		d.Evaluator = &Evaluator{}
	}
	ev := d.Evaluator
	cmds := EffectiveTestCmds(inst)
	if len(cmds) == 0 {
		return "", "", fmt.Errorf("no test commands derivable for %s (no test files in test_patch)", inst.InstanceID)
	}

	// 1. 基线：只打 test_patch
	s0, err := ev.RunPatched(ctx, inst, baseDir, nil, cmds)
	if err != nil {
		return "", "", fmt.Errorf("baseline run: %w", err)
	}
	// 2. gold：test_patch + gold patch
	s1, err := ev.RunPatched(ctx, inst, baseDir, []string{inst.Patch}, cmds)
	if err != nil {
		return s0.Log, "", fmt.Errorf("gold run: %w", err)
	}

	st0 := ParseStatuses(s0.Log, inst.LogParser)
	st1 := ParseStatuses(s1.Log, inst.LogParser)
	inst.FAILToPASS, inst.PASSToPASS = DeriveTestSets(st0, st1)
	inst.TestCmds = cmds
	return s0.Log, s1.Log, nil
}

// EffectiveTestCmds 返回实例的测试命令：优先数据集字段，
// 缺失时按 pytest 约定从 test_patch 触及的测试文件推导
// （与 SWE-bench 的 test_directives 思路一致：只跑相关测试文件）。
func EffectiveTestCmds(inst *Instance) []string {
	if len(inst.TestCmds) > 0 {
		return inst.TestCmds
	}
	var files []string
	for f := range TestFilePaths(inst.TestPatch) {
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	return []string{
		"python -m pytest --no-header -rA --tb=no -p no:cacheprovider " + strings.Join(files, " "),
	}
}

// DeriveTestSets 由两次运行的状态映射推导 F2P / P2P（纯函数，便于测试）。
func DeriveTestSets(s0, s1 map[string]string) (f2p, p2p []string) {
	passed := func(st string) bool { return st == StatusPassed }

	// F2P：基线未过、gold 后过
	s0Failed := map[string]bool{}
	for id, st := range s0 {
		if !passed(st) {
			s0Failed[id] = true
		}
	}
	for id, st := range s1 {
		if passed(st) && s0Failed[id] {
			f2p = append(f2p, id)
		}
	}
	sort.Strings(f2p)

	// P2P：两次都过
	for id, st := range s0 {
		if passed(st) && passed(s1[id]) {
			p2p = append(p2p, id)
		}
	}
	sort.Strings(p2p)
	return f2p, p2p
}
