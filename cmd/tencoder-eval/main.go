// tencoder-eval 是 SWE-bench-Live 自动化评测流水线 CLI。
//
// 子命令：
//
//	fetch     拉取数据集 split 到本地缓存
//	derive    用 gold patch 推导缺失的评测元数据（F2P/P2P/test_cmds）
//	run       对指定实例执行 Agent rollout，产出 predictions.jsonl
//	evaluate  对已有 predictions 在 Docker 中执行测试并判定 resolved
//	pipeline  fetch → run → (derive) → evaluate 一条龙
//
// 示例：
//
//	tencoder-eval fetch --split lite --cache-dir .tencoder/eval
//	tencoder-eval pipeline --split lite --limit 2 --run-id demo --root .tencoder/eval
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tencoder/internal/eval"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	var err error
	switch sub {
	case "fetch":
		err = cmdFetch(args)
	case "derive":
		err = cmdDerive(args)
	case "run":
		err = cmdRun(args)
	case "evaluate":
		err = cmdEvaluate(args)
	case "pipeline":
		err = cmdPipeline(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tencoder-eval — SWE-bench-Live evaluation pipeline

Usage:
  tencoder-eval <command> [flags]

Commands:
  fetch     Fetch a dataset split to local cache
  derive    Derive missing eval metadata (F2P/P2P/test_cmds) via gold patch
  run       Run agent rollouts for selected instances
  evaluate  Evaluate predictions inside Docker containers
  pipeline  fetch -> run -> (derive) -> evaluate in one go
`)
}

type commonFlags struct {
	split     string
	cacheDir  string
	root      string
	runID     string
	instances string
	limit     int
	namespace string
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.split, "split", "lite", "dataset split: lite / verified / full")
	fs.StringVar(&c.cacheDir, "cache-dir", ".tencoder/eval/cache", "dataset cache dir")
	fs.StringVar(&c.root, "root", ".tencoder/eval", "eval output root (runs/, cache/)")
	fs.StringVar(&c.runID, "run-id", "", "run id (default: timestamp)")
	fs.StringVar(&c.instances, "instances", "", "comma-separated instance_ids (empty = all)")
	fs.IntVar(&c.limit, "limit", 0, "max instances (0 = no limit)")
	fs.StringVar(&c.namespace, "namespace", "starryzhang", "DockerHub namespace for instance images")
	return c
}

func (c *commonFlags) instanceIDs() []string {
	if c.instances == "" {
		return nil
	}
	var ids []string
	for _, p := range strings.Split(c.instances, ",") {
		if p = strings.TrimSpace(p); p != "" {
			ids = append(ids, p)
		}
	}
	return ids
}

func (c *commonFlags) resolveRunID() string {
	if c.runID != "" {
		return c.runID
	}
	return eval.RunTimestamp()
}

func cmdDerive(args []string) error {
	fs := flag.NewFlagSet("derive", flag.ExitOnError)
	c := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &eval.DatasetStore{CacheDir: c.cacheDir}
	cachePath, err := store.FetchSplit(c.split)
	if err != nil {
		return err
	}
	instances, err := store.LoadInstances(cachePath, c.instanceIDs(), c.limit)
	if err != nil {
		return err
	}
	workRoot := filepath.Join(c.root, "workspaces")
	evaluator := &eval.Evaluator{Namespace: c.namespace}
	deriver := &eval.Deriver{Evaluator: evaluator}

	derived := 0
	for i := range instances {
		inst := instances[i]
		if len(inst.FAILToPASS) > 0 || len(inst.PASSToPASS) > 0 {
			fmt.Printf("[derive] %s: metadata present, skip\n", inst.InstanceID)
			continue
		}
		fmt.Printf("[derive] %s (%d/%d)...\n", inst.InstanceID, i+1, len(instances))
		baseDir, err := eval.PrepareBaseWorkdir(ctx, &inst, workRoot)
		if err != nil {
			return err
		}
		bLog, gLog, err := deriver.DeriveMetadata(ctx, &inst, baseDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[derive] %s failed: %s\n", inst.InstanceID, err)
			continue
		}
		_ = runStoreSave(filepath.Join(c.root, "runs"), inst.InstanceID, "baseline", bLog)
		_ = runStoreSave(filepath.Join(c.root, "runs"), inst.InstanceID, "gold", gLog)
		if err := store.UpdateInstance(cachePath, inst); err != nil {
			return err
		}
		fmt.Printf("[derive] %s: F2P=%d P2P=%d\n", inst.InstanceID, len(inst.FAILToPASS), len(inst.PASSToPASS))
		derived++
	}
	fmt.Printf("[derive] done — %d instance(s) derived, cached at %s\n", derived, cachePath)
	return nil
}

// runStoreSave 把推导日志落盘到 runs/derive-logs/<instance>-<kind>.log。
func runStoreSave(runsRoot, instanceID, kind, log string) error {
	dir := filepath.Join(runsRoot, "derive-logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, eval.SanitizeID(instanceID)+"-"+kind+".log"), []byte(log), 0o644)
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	c := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	store := &eval.DatasetStore{CacheDir: c.cacheDir}
	path, err := store.FetchSplit(c.split)
	if err != nil {
		return err
	}
	insts, err := store.LoadInstances(path, nil, 0)
	if err != nil {
		return err
	}
	fmt.Printf("dataset cached: %s (%d instances)\n", path, len(insts))
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runPipelinePhase(c, true, false)
}

func cmdEvaluate(args []string) error {
	fs := flag.NewFlagSet("evaluate", flag.ExitOnError)
	c := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runPipelinePhase(c, false, true)
}

func cmdPipeline(args []string) error {
	fs := flag.NewFlagSet("pipeline", flag.ExitOnError)
	c := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runPipelinePhase(c, true, true)
}

// runPipelinePhase 按 doRollout / doEval 组合执行各阶段。
func runPipelinePhase(c *commonFlags, doRollout, doEval bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ids := c.instanceIDs()
	store := &eval.DatasetStore{CacheDir: c.cacheDir}
	cachePath, err := store.FetchSplit(c.split)
	if err != nil {
		return err
	}
	instances, err := store.LoadInstances(cachePath, ids, c.limit)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return fmt.Errorf("no instances selected (ids=%v limit=%d)", ids, c.limit)
	}
	fmt.Printf("[eval] %d instance(s) selected\n", len(instances))

	runStore, err := eval.NewRunStore(c.root, c.resolveRunID())
	if err != nil {
		return err
	}
	fmt.Printf("[eval] run dir: %s\n", runStore.Dir)

	workRoot := filepath.Join(c.root, "workspaces")
	runner := &eval.RolloutRunner{TrajectoryDir: filepath.Join(runStore.Dir, "trajectories")}
	evaluator := &eval.Evaluator{Namespace: c.namespace}

	report := &eval.Report{RunID: c.resolveRunID()}

	for i := range instances {
		inst := instances[i]
		fmt.Printf("[eval] === %s (%d/%d) ===\n", inst.InstanceID, i+1, len(instances))
		result := eval.InstanceResult{InstanceID: inst.InstanceID}

		var patch string
		if doRollout {
			rr, err := runner.Run(ctx, &inst, workRoot)
			if rr != nil {
				result.DurationSec = rr.Duration.Seconds()
				result.Trajectory = rr.Trajectory
				result.InputTokens = rr.InputTokens
				result.OutputTokens = rr.OutputTokens
				patch = rr.ModelPatch
				if rr.Err != "" {
					result.Error = rr.Err
				}
			}
			if err != nil {
				if result.Error == "" {
					result.Error = err.Error()
				}
				fmt.Fprintf(os.Stderr, "[eval] rollout error: %s\n", err)
			}
			if err := runStore.SavePrediction(eval.Prediction{
				InstanceID:      inst.InstanceID,
				ModelPatch:      patch,
				ModelNameOrPath: "tencoder",
			}); err != nil {
				return err
			}
		} else {
			// evaluate-only 模式：从 runStore 读已有 predictions
			preds, lerr := runStore.LoadPredictions()
			if lerr != nil {
				return fmt.Errorf("load predictions: %w", lerr)
			}
			patch = preds[inst.InstanceID].ModelPatch
		}

		if doEval && result.Error == "" {
			// 元数据缺失（防污染设计）时先推导，并回写缓存供后续复用
			if len(inst.FAILToPASS) == 0 && len(inst.PASSToPASS) == 0 {
				deriver := &eval.Deriver{Evaluator: evaluator}
				bLog, gLog, derr := deriver.DeriveMetadata(ctx, &inst, filepath.Join(workRoot, eval.SanitizeID(inst.InstanceID)))
				if derr != nil {
					result.Error = "derive metadata: " + derr.Error()
				} else {
					_ = runStoreSave(filepath.Join(c.root, "runs"), inst.InstanceID, "baseline", bLog)
					_ = runStoreSave(filepath.Join(c.root, "runs"), inst.InstanceID, "gold", gLog)
					if uerr := store.UpdateInstance(cachePath, inst); uerr != nil {
						fmt.Fprintf(os.Stderr, "[eval] update cache: %s\n", uerr)
					}
					fmt.Printf("[eval] derived metadata: F2P=%d P2P=%d\n", len(inst.FAILToPASS), len(inst.PASSToPASS))
				}
			}
		}
		if doEval && result.Error == "" {
			er, err := evaluator.Evaluate(ctx, &inst, filepath.Join(workRoot, eval.SanitizeID(inst.InstanceID)), patch)
			if err != nil {
				result.Error = err.Error()
			} else if er != nil {
				result.Resolved = er.Resolved
				result.EmptyPatch = er.EmptyPatch
				result.ApplyFailed = er.ApplyFailed
				result.Missing = er.Missing
				result.DurationSec = er.Duration.Seconds()
				if _, lerr := runStore.SaveLog(inst.InstanceID, er.Log); lerr != nil {
					fmt.Fprintf(os.Stderr, "[eval] save log: %s\n", lerr)
				}
			}
		}

		report.Results = append(report.Results, result)
		report.Total++
		if result.Resolved {
			report.ResolvedCnt++
		}
		fmt.Printf("[eval] %s resolved=%v (running %d/%d, pass@1=%.2f)\n",
			inst.InstanceID, result.Resolved, report.ResolvedCnt, report.Total,
			float64(report.ResolvedCnt)/float64(report.Total))
	}

	path, err := runStore.SaveReport(report)
	if err != nil {
		return err
	}
	fmt.Printf("[eval] done — pass@1 = %d/%d (%.1f%%)\nreport: %s\n",
		report.ResolvedCnt, report.Total, 100*report.PassAt1, path)
	return nil
}
