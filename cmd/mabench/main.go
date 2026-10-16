// 多 Agent 并行 vs 单 Agent 跨文件任务耗时基准。
//
// 任务：15 个 Go 文件，每个文件有一个空实现的 String() 方法，
// 需按各自文件里的 kind 常量实现（字段组合各不相同，必须逐文件阅读）。
//
// 两种模式（同一模型、同一任务、同一工作区规模）：
//   - single：1 个 Agent 顺序完成全部 15 个文件
//   - team  ：3 个 Agent 并行，每个负责 5 个文件（文件级隔离，由本
//     驱动器充当 lead 做确定性分片，成员复用 teams 包的并发执行原语）
//
// 用法：go run ./cmd/mabench -mode single|team|both
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

const numFiles = 30

var kinds = []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"india", "juliet", "kilo", "lima", "mike", "november", "oscar"}

type runResult struct {
	Mode      string
	Duration  time.Duration
	InTokens  int
	OutTokens int
	Turns     int
	OK        bool
	Detail    string
}

func setupWorkspace() (string, error) {
	dir, err := os.MkdirTemp("/tmp", "mabench-*")
	if err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module scratch\n\ngo 1.25\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	for i := 0; i < numFiles; i++ {
		name := fmt.Sprintf("widget%02d", i+1)
		src := fmt.Sprintf(`package pkg

const kind = %q

type %s struct {
	ID   int
	Name string
}

func (w %s) String() string {
	return ""
}
	`, kinds[i%len(kinds)]+fmt.Sprintf("-%02d", i+1), name, name)
		if err := os.WriteFile(filepath.Join(dir, "pkg", name+".go"), []byte(src), 0o644); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	cmd.Run()
	return dir, nil
}

func verify(dir string) error {
	for i := 0; i < numFiles; i++ {
		name := fmt.Sprintf("widget%02d", i+1)
		data, err := os.ReadFile(filepath.Join(dir, "pkg", name+".go"))
		if err != nil {
			return fmt.Errorf("%s 缺失", name)
		}
		s := string(data)
		if strings.Contains(s, `return ""`) {
			return fmt.Errorf("%s 仍是空实现", name)
		}
		if !strings.Contains(s, "Sprintf") {
			return fmt.Errorf("%s 未实现格式化输出", name)
		}
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("编译失败: %v: %s", err, string(out))
	}
	return nil
}

const spec = `工作目录下的 pkg/ 包中有 %d 个 Go 文件（widget01.go ~ widget%02d.go）。
每个文件都有一个 String() 方法返回空字符串 ""。你的任务：

1. 读取每个文件，找到它的 kind 常量值与类型名。
2. 实现 String()：返回 fmt.Sprintf("%s:%%d:%%s", kind, w.ID, w.Name) 的等价格式
   （即 "<kind值>:<ID>:<Name>"）。每个文件的 kind 值不同，必须逐文件读取。
3. 保证 go build ./... 能通过。

重要：不要运行 go build 或 go test（驱动器会统一验证），改完所有分配给你的文件即结束。`

func buildAgent(ctx context.Context, workDir string) (*agent.Agent, *conversation.Manager, func(), error) {
	cfg, err := config.LoadConfig("")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	if len(cfg.Providers) == 0 {
		return nil, nil, nil, fmt.Errorf("no provider in config")
	}
	prov := &cfg.Providers[0]

	def := tools.CreateDefaultToolsWithWorkDir(workDir)
	env := prompt.DetectEnvironment(workDir)
	env.Model = prov.Model
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		MemorySection: memory.LoadAutoMemoryPrompt(workDir),
	})
	client, err := llm.NewClient(prov, systemPrompt)
	if err != nil {
		return nil, nil, nil, err
	}
	llm.ResolveContextWindow(ctx, prov)

	conv := conversation.NewManager()
	sessionID := session.NewID()
	fh := filehistory.New(workDir, sessionID)
	def.EditFile.FileHistory = fh
	def.WriteFile.FileHistory = fh

	ag := agent.New(client, def.Registry, prov.Protocol)
	ag.WorkDir = workDir
	ag.ContextWindow = prov.GetContextWindow()
	ag.MaxOutputTokens = prov.GetMaxOutputTokens()
	ag.FileHistory = fh
	ag.SetSessionID(sessionID)
	ag.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(workDir), &permissions.RuleEngine{}, permissions.ModeBypass,
	)
	return ag, conv, func() {}, nil
}

// runOneAgent 驱动单个 Agent 完成分配的文件列表，返回 (耗时, in/out tokens, err)
func runOneAgent(ctx context.Context, workDir, task string) (time.Duration, int, int, error) {
	ag, conv, cleanup, err := buildAgent(ctx, workDir)
	if err != nil {
		return 0, 0, 0, err
	}
	defer cleanup()

	conv.AddUserMessage(task)
	start := time.Now()
	events := ag.Run(ctx, conv)
	var inTok, outTok int
	var runErr error
	for ev := range events {
		switch e := ev.(type) {
		case agent.LoopComplete:
			// 完成
		case agent.ErrorEvent:
			runErr = fmt.Errorf("%s", e.Message)
		case agent.UsageEvent:
			inTok += e.InputTokens
			outTok += e.OutputTokens
		case agent.PermissionRequestEvent:
			select {
			case e.ResponseCh <- agent.PermAllow:
			default:
			}
		case agent.AskUserQuestionEvent:
			select {
			case e.ResponseCh <- map[string]string{}:
			default:
			}
		}
	}
	return time.Since(start), inTok, outTok, runErr
}

func fileNames(from, to int) string {
	var names []string
	for i := from; i <= to; i++ {
		names = append(names, fmt.Sprintf("pkg/widget%02d.go", i))
	}
	return strings.Join(names, ", ")
}

func main() {
	mode := flag.String("mode", "both", "single | team | both")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	workDir, err := setupWorkspace()
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	fmt.Println("工作区:", workDir)

	fullTask := fmt.Sprintf(spec, numFiles, numFiles)

	if *mode == "single" || *mode == "both" {
		// 单 Agent：用全新工作区，避免残留
		d, _ := setupWorkspace()
		fmt.Println("\n=== 单 Agent 模式 ===")
		dur, in, out, err := runOneAgent(ctx, d, fullTask)
		res := runResult{Mode: "single", Duration: dur, InTokens: in, OutTokens: out}
		if err != nil {
			res.Detail = "error: " + err.Error()
		} else if verr := verify(d); verr != nil {
			res.Detail = "verify: " + verr.Error()
		} else {
			res.OK = true
			res.Detail = "验证通过"
		}
		printResult(res)
	}

	if *mode == "team" || *mode == "both" {
		fmt.Println("\n=== 3 Agent 并行模式 ===")
		type assignment struct {
			name string
			task string
		}
		assigns := []assignment{
			{"worker-1", fmt.Sprintf(spec, numFiles, numFiles) + "\n\n你只负责这些文件: " + fileNames(1, 10) + "。其他文件由队友负责，不要碰。"},
			{"worker-2", fmt.Sprintf(spec, numFiles, numFiles) + "\n\n你只负责这些文件: " + fileNames(11, 20) + "。其他文件由队友负责，不要碰。"},
			{"worker-3", fmt.Sprintf(spec, numFiles, numFiles) + "\n\n你只负责这些文件: " + fileNames(21, 30) + "。其他文件由队友负责，不要碰。"},
		}
		start := time.Now()
		var wg sync.WaitGroup
		results := make([]string, len(assigns))
		var mu sync.Mutex
		var inSum, outSum int
		for i, a := range assigns {
			wg.Add(1)
			go func(idx int, a assignment) {
				defer wg.Done()
				dur, in, out, err := runOneAgent(ctx, workDir, a.task)
				mu.Lock()
				inSum += in
				outSum += out
				status := "ok"
				if err != nil {
					status = "error: " + err.Error()
				}
				results[idx] = fmt.Sprintf("%s: %v, tokens(in=%d,out=%d), %s", a.name, dur.Round(time.Second), in, out, status)
				mu.Unlock()
			}(i, a)
		}
		wg.Wait()
		dur := time.Since(start)
		res := runResult{Mode: "team(3x parallel)", Duration: dur, InTokens: inSum, OutTokens: outSum}
		if verr := verify(workDir); verr != nil {
			res.Detail = "verify: " + verr.Error()
		} else {
			res.OK = true
			res.Detail = "验证通过"
		}
		for _, r := range results {
			fmt.Println("  ", r)
		}
		printResult(res)
	}
}

func printResult(r runResult) {
	fmt.Printf("===> [%s] 耗时=%v tokens(in=%d, out=%d) OK=%v  %s\n",
		r.Mode, r.Duration.Round(time.Second), r.InTokens, r.OutTokens, r.OK, r.Detail)
}
