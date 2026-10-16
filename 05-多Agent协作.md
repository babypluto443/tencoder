# 05 - 多 Agent 协作（子代理 / Fork / Team / Worktree 隔离）

> 对应简历卖点：*「多 Agent 协作：基于 git worktree 的文件级隔离，让多个 Agent 并行编辑不同模块互不冲突，跨文件重构耗时降低 60%」*
> 本模块把"多 Agent"拆成**三种运行形态**（同步子代理 / 后台 Fork / 长驻 Team 队友）+ **一套 IPC 信箱（FileMailBox）** + **一级文件系统隔离（git worktree）**，讲清它们各自解决什么问题、代码怎么跑、以及为什么能"互不影响"。

---

## 一、这个模块在整套架构里的位置

`internal/agent`（主循环，见 01）只解决"单个 Agent 怎么跑 ReAct 循环"。当你要让一个任务被切成几块并行做、或让一个长驻队友在后台持续干活时，就需要"多 Agent 协作"层：

| 层次 | 包 | 职责 |
|------|----|------|
| 协作调度 | `internal/agents`（agent_tool.go / subagent.go / tool_filter.go） | 决定"这次 spawn 是同步子代理 / 后台 Fork / Team 队友"，并据此组装子 Agent 的工具集、权限、模型、隔离目录 |
| 团队管理 | `internal/teams`（teams.go / filemailbox.go / spawn.go / inprocess.go / tmux.go / iterm.go） | Team 的注册、启动、停止、队友间消息投递（邮箱） |
| 文件系统隔离 | `internal/worktree`（create.go / setup.go / changes.go / cleanup.go / notice.go） | 给并行队友每人一个独立 git worktree，编辑互不踩踏 |

**一句话记忆**：`AgentTool.Execute` 是三种形态的"分叉点"，`FileMailBox` 是队友间唯一的通信渠道，`git worktree` 是并行队友物理隔离的手段。

---

## 二、推荐代码阅读顺序

按"先懂通信介质 → 再懂调度分叉 → 再懂隔离"的顺序读，比直接读 agent_tool.go 容易懂：

1. **`internal/teams/filemailbox.go`** —— 先读这个。它是所有 Team 通信的底座：用**文件 + 文件锁**实现了一个极简消息队列。读懂 `withLock` 的 `O_EXCL` 原子锁 + 陈旧锁回收 + 随机退避，你就懂了"为什么多进程/多 goroutine 写同一个信箱不会乱"。
2. **`internal/teams/teams.go`** —— 看 `Team` / `Member` / `TeamManager` 的数据结构，以及 `AddMember` / `StartMember` / `StopMember` / `SendMessage`。这是 Team 的"门面"。
3. **`internal/agents/tool_filter.go`** —— 看 `FilterToolsForAgent` 的 6 层工具过滤。子 Agent 不是"继承全部工具"，而是被层层裁剪，这是安全与隔离的关键。
4. **`internal/agents/subagent.go`** —— 看 `SubAgentSpec`、`BuiltinSpecs`（general-purpose / plan / explore 三种内置角色）、`TaskManager`、`SpawnSubAgent`。这是"同步子代理 / 后台子代理"的执行体。
5. **`internal/agents/agent_tool.go`** —— 重头戏。`AgentTool.Execute` 是三种形态的**分发中心**，然后逐个看 `runSync` / `runFork` / `runAsync` / `runAsTeammate`。
6. **`internal/worktree/create.go`** —— 看 `getOrCreateWorktree` 的"快速恢复"与 `git worktree add -B`，理解隔离目录怎么建、怎么复用。
7. （选读）`internal/teams/spawn.go` / `inprocess.go` / `tmux.go` / `iterm.go` —— 三种后端（同进程 / tmux / iTerm）如何真正把队友跑起来。

> 读 agent_tool.go 时务必对照 01 主循环的 `Agent.Run`：子 Agent 本质就是"再 new 一个 `agent.Agent`，丢一个独立 `conversation.Manager`，然后 `Run(ctx, conv)`"——主循环被**复用**了，没有为子 Agent 重写一套循环。

---

## 三、逐文件讲解

### 3.1 `internal/teams/filemailbox.go` —— 文件级消息队列（IPC 底座）

**核心结构体**：`FileMailBox` 只持有一个 `baseDir`。每个 Agent 一个 inbox 文件 `<agentID>.json`，一把锁文件 `<agentID>.json.lock`（见 `inboxPath` / `lockPath`，34-40 行）。

**消息结构** `FileMailMessage`（20-27 行）：`From` / `Text` / `Timestamp` / `Read` / `Color` / `Summary`。注意它是**整 JSON 数组**读写，不是一个文件一条消息——简单但要求写操作原子（见下文锁）。

**关键方法 `withLock`（76-116 行）—— 这是全模块最值得背的细节**：

```go
for attempt := 0; attempt < 10; attempt++ {
    lockFd, err = os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
    if err == nil { break }
    if !os.IsExist(err) { return err }           // 非"已存在"错误，直接返回
    // 锁存在：检查是否陈旧（>10s）
    if info, statErr := os.Stat(lockFile); statErr == nil {
        if time.Since(info.ModTime()) > 10*time.Second {
            os.Remove(lockFile)                  // 回收死锁
        }
    }
    sleepMs := 5 + rand.Intn(96)                 // 5~100ms 随机退避
    time.Sleep(time.Duration(sleepMs) * time.Millisecond)
}
```

三个考点：
- **`O_EXCL` 原子抢锁**：`O_CREATE|O_EXCL` 在同一路径已存在文件时会失败返回 `EEXIST`，而不是覆盖。这把"抢锁"变成了一个**原子的、无竞态**的系统调用，不需要 `flock()`/Redis。
- **陈旧锁回收**：如果持锁进程崩溃没释放，锁文件会一直挂着。这里用 `ModTime() > 10s` 判断"这把锁估计是死锁"，直接 `os.Remove` 抢过来。10s 是经验值（正常一次 inbox 读写远小于 1s）。
- **随机退避**：`5 + rand.Intn(96)` 即 5~100ms。多 goroutine 同时抢同一把锁时，随机退避避免"同时重试同时失败"的惊群效应。

拿到锁后：`readInbox` → `fn` 应用变更 → `writeInbox`（106-115 行）。`defer os.Remove(lockFile)` 在 103 行保证释放。

**对外接口**：`Send`（42，追加一条未读消息）、`ReadUnread`（52，过滤未读）、`MarkAllRead`（66，批量标记已读）。都是"调 `withLock` + 一个纯函数式 mutation"的模式。

> 面试话术：*"队友通信我用的是文件级信箱而非内存 channel，因为 Team 队友可能是跨进程（tmux/iTerm 后端）跑的，内存 channel 跨不了进程；文件信箱天然跨进程、可持久、崩溃可恢复。`O_EXCL` 抢锁比语言层锁更适合多进程场景。"*

### 3.2 `internal/teams/teams.go` —— Team 数据结构与门面

- **`TeamMode`（22-27 行）**：`ModeInProcess`（同进程 goroutine）/ `ModeTmux`（每个队友一个 tmux pane）/ `ModeITerm`（iTerm）。后端选择决定队友是"线程内"还是"真·独立终端"。
- **`Member`（37-47 行）**：`AgentRef`（子 Agent）、`Conv`（独立会话）、`Active`、`Cancel`（context 取消函数）、`PaneID`（tmux/iTerm 的 pane 句柄，同进程为空）、`Progress`（进度）。
- **`Team（49-55 行）**：`Members map` + `MailBox`（就是 3.1 的 `FileMailBox`）+ `mu` 互斥锁。
- **`NewTeam`（57-65 行）**：信箱目录固定为 `<teamsBaseDir>/<teamName>/inboxes`。`teamsBaseDir` 默认是 `.tencoder/teams`（29-35 行，可用 `TENCODER_TEAMS_DIR` 覆盖）。
- **`AddMember`（67-80 行）**：`t.mu.Lock()` 保护 map 写入；`agent.New(client, registry, protocol)` new 一个子 Agent；`conversation.NewManager()` 给它**独立会话**（子 Agent 看不到主会话历史——这是"上下文隔离"的关键）。
- **`StartMember`（82-98 行）**：建 `context.WithCancel`、把 task 作为 user message 塞进 `Conv`、`member.AgentRef.Run(...)` 返回事件 channel。注意这里是**阻塞** `Run` 直到子 Agent 结束（同步路径）。
- **`StopMember`（100-122 行）**：先按 `Mode` 拆掉真实 OS pane（tmux/iTerm 后端才需要），再 `Cancel()` 取消 goroutine，最后 `Active=false`。
- **`SendMessage`（136-142 行）**：直接 `MailBox.Send(to, ...)`，把消息投到收件人信箱。注意它**不阻塞**，投递即返回——队友下次拉取 `ReadUnread` 时才会看到。
- **`TeamManager`（144-217 行）**：全局 Team 注册表，`CreateTeam` / `GetTeam` / `DeleteTeam` / `CloseAll`，全是 `mu` 保护的 map 操作。`CreateTeamWith`（165）给 tmux/iTerm 子进程用——它们本地也建一个 Team，但指向**同一个信箱目录**，所以能跨进程互相 `SendMessage`。

### 3.3 `internal/agents/tool_filter.go` —— 子 Agent 工具集的 6 层裁剪

子 Agent **不**继承主 Agent 的全部工具。`FilterToolsForAgentEx`（96-154 行）按 6 层顺序过滤（注释在 85-95 行写得很清楚）：

1. **Layer 1 MCP 工具（`mcp__*`）永远放行**（113-116）——MCP 工具是接入外部能力，子 Agent 也能用。
2. **Layer 2 全局黑名单 `AllAgentDisallowedTools`（19-27 行）**——`Agent` 本身、`TaskOutput`、`AskUserQuestion`、`Workflow` 等被全局禁掉，防递归 spawn / 防跳出子代理语义。
3. **Layer 3 自定义 Agent 额外限制**（仅 `isCustom` 时，24 行）——从 `.tencoder/agents/` 加载的 agent 再加一层。
4. **Layer 4 后台白名单 `AsyncAgentAllowedTools`（44-61 行）**——后台 Agent 只能用一个"读+写文件+搜索+执行"的白名单；若是同进程 Team 队友（`isInProcessTeammate`），再加 `InProcessTeammateAllowedTools`（66-75，如 `TaskCreate`/`SendMessage`/`Cron*`）让它能协调任务、发消息。
5. **Layer 5 定义级黑名单**（142 `disallowed[name]`）——agent 定义里写死不让用的。
6. **Layer 6 定义级白名单**（147 `hasWhitelist`）——agent 定义里写"只允许这些"；注意 `*` 表示"不限制这层"（103 行判断）。

`hasWhitelist` 的判定很妙：`len(allowedTools)==1 && allowedTools[0]=="*"` 时视为"无白名单"（103 行），这样 `tools: ["*"]` 的 agent 不被 Layer 6 卡死。

> 考点：为什么 `Agent` 工具在全局黑名单里（23 行）？因为**后台子代理不能再 spawn 子代理**（防无限递归）；但同进程 Team 队友例外——Layer 4 里 `name=="Agent"` 在 `isInProcessTeammate` 时被重新放行（131-136），且只允许同步子代理（嵌套 spawn 在调用点校验）。

### 3.4 `internal/agents/subagent.go` —— 子 Agent 规格与执行体

- **`SubAgentSpec`（189-227 行）**：子 Agent 的"运行配置"——`Tools` / `DisallowedTools` / `SystemPromptOverride` / `MaxTurns` / `Model` / `PermissionMode` / `Isolation`（worktree）/ `Memory` 作用域等。它是"定义层"和"执行层"之间的桥。
- **`BuiltinSpecs（264-287 行）**——三个内置角色：
  - `general-purpose`：全工具、200 轮上限。
  - `plan`：**只读**（`DisallowedTools: EditFile/WriteFile`）+ 专属系统提示（229-262，强制"不得改文件，只出方案"）+ 15 轮。
  - `explore`：**只读** + `Model: haiku`（便宜快）+ 不限制轮数（注释 281-284 解释了之前 30 轮太小会误杀）。
- **`TaskManager（43-184 行）**——后台任务的注册表：`CreateTask` → `SetRunning` → 跑完 `SetCompleted`/`SetFailed`。`DrainNotifications`（134）把完成通知攒起来，主循环下一轮读走（这就是"后台任务完成 → task-notification 提醒"的实现）。`CancelTask`（174）通过 `Cancel()` 取消。
- **`SpawnSubAgent（289-346 行）**——后台子代理的真正启动点：
  - `FilterToolsForAgent(registry, spec.Tools, spec.DisallowedTools, true)`（304）——`isAsync=true` 走白名单。
  - `deriveSubAgentChecker(parentChecker, spec.PermissionMode)`（307）——权限检查器：Sandbox/RuleEngine 复用父的，只换 `Mode`（见 agent_tool.go 的 `deriveSubAgentChecker` 注释 504-518）。
  - `go func(){ ... subAgent.Run(subCtx, conv) ... }()`（314-343）——**开 goroutine 跑子 Agent**，主线程不等它；子 Agent 是 headless，遇到 `PermissionRequestEvent` 直接 `PermDeny` 自动拒绝（332-334），不会卡在权限 UI 上。
  - 跑完 `taskMgr.SetCompleted(taskID, output)`，主循环后续通过通知拿到结果。

### 3.5 `internal/agents/agent_tool.go` —— 三种形态的分发中心

这是本模块最重的文件。`AgentTool` 是一个**工具**（`Name()=="Agent"`，97 行），主 Agent 通过调用它来 spawn 子 Agent。它的 `Execute`（213-289 行）是**分发中心**：

```
Execute:
  必填校验 description + prompt（216）
  cwd 与 isolation=worktree 互斥（230-235）
  mode 合法性校验（237-242）
  ── 分叉点 ──
  if team_name != "" && TeamMgr != nil  → runAsTeammate   （247-249）长驻队友
  if subagent_type == ""                → runFork         （252-254）后台 Fork（复制主会话上下文）
  else（有 subagent_type）               → 解析 spec
        if run_in_background || spec.Background → runAsync（285-287）后台子代理
        else                             → runSync        （288）同步子代理（阻塞等结果）
```

**`runSync`（291-404 行）**——同步子代理：
- `FilterToolsForAgent(... false)`（292）走定义级过滤（非 async）。
- `agent.New(...)` 复用主循环；`deriveSubAgentChecker` 设权限（296）。
- `isolation == "worktree"`（305-320）：调 `worktree.CreateAgentWorktree` 建隔离目录，把 `subAgent.WorkDir` 指过去，并在 prompt 前注入"你在 worktree 里"的提示（`BuildWorktreeNotice`）。
- `ch := subAgent.Run(ctx, conv)`（338）**同步**消费事件：把子 Agent 的 `StreamText` 攒成 output、`ToolResultEvent` 发进度、`PermissionRequestEvent` 自动拒绝、`ErrorEvent` 转成错误返回（340-373）。
- 结束若 worktree 有改动则保留并提示路径，否则自动 `RemoveAgentWorktree` 清理（392-399）。

**`runFork`（406-485 行）**——后台 Fork，**复制主会话全部上下文**：
- **嵌套 Fork 防护**两层（411-429）：① `QuerySource == ForkQuerySource` 直接拒（压缩也拦得住）；② 遍历消息找 `ForkBoilerplateTag`（history 被 summary 掉时的兜底）。
- `buildForkedConversation`（549-586）：**字节级重放**父消息——保留 thinking block 和 tool_use，**未完成的 tool_use 补占位 `"(tool execution interrupted by fork)"`**（559-567）。目的：**让子 Agent 的 API 请求前缀和父的字节级一致**，命中 Anthropic Prompt Cache（注释 553-555）。
- `cloneRegistryForFork`（525-537）：复制父工具集，但把 `*AgentTool` 浅拷贝并把 `QuerySource` 设成 `ForkQuerySource`，这样子 Agent 若再 fork 会被上面第①层拦下（保持"Fork 不可再 Fork"）。
- 继承父 `ParentReplacementState.Clone()`（448-450）——tool result 压缩决策也一并克隆，**同样是为了 prompt cache 前缀字节稳定**。
- `go func(){ subAgent.Run(...) }()`（461-477）：**永远后台跑**，结果进 `TaskManager`，主线程立刻返回"已启动（task xxx）"。

> 考点：Fork 和"有 subagent_type 的子代理"最大区别——Fork **带着父的完整历史**（所以能续上上下文、命中 cache），子代理是**干净的新会话**（看不到父历史）。Fork 适合"把当前进行到一半的任务分出去并行推进"；子代理适合"给一个独立子任务，它不该被当前上下文干扰"。

**`runAsTeammate`（607-703 行）**——长驻 Team 队友：
- 查 `Team`、`memberName` 唯一性（611-627）。
- 解析 spec、过滤工具、`isolation=="worktree"` 给队友独立 worktree（642-665）。
- `teams.SpawnTeammate(...)`（667）按 `Team.Mode` 真正拉起队友（同进程/tmux/iTerm）。
- **主线程立即返回**（697-702），不阻塞——后续通过 `SendMessage` + 信箱 idle 通知协调。
- 同进程队友若返回 event channel，起一个 goroutine `drainTeammateEvents` 排空，避免生产者阻塞（686-688、708-731）。

`emitProgress`（492-502）值得记：**非阻塞**发送进度（用 `select` + `default` 丢弃），因为曾因 `ProgressCh` 缓冲满导致子 Agent 事件循环卡死、ESC/ctx 取消失效——进度是"尽力而为的 UI 反馈，不是负载态"。

### 3.6 `internal/worktree/create.go` —— 文件系统隔离（git worktree）

- **`WorktreesDir`（18-20）**：所有 worktree 放 `<repoRoot>/.tencoder/worktrees`（已在 .gitignore）。
- **`getOrCreateWorktree`（52-124 行）**——建 or 复用：
  - **快速恢复路径**（56-68）：`ReadWorktreeHeadSha` 直接读 `.git` 指针文件（无 subprocess、无向上 walk）。在 1600 万对象的仓库里，这能省掉每次 resume 都要跑的 `git fetch` commit-graph 扫描（~6-8s）。已存在就直接 `Existed=true` 返回，跳过 `git worktree add`。
  - **创建路径**（70-115）：用纯文件系统读解析默认分支（`GetDefaultBranch`），尽量跳过 `git fetch`（74-77 注释）；`origin/<default>` 本地已知就直接用，否则才 `git fetch`（95-100，离线再退回 `HEAD`）。
  - **`git worktree add -B worktree-<flat> <path> <baseBranch>`**（112）：`-B`（大写）而非 `-b`，会**重置**被删 worktree 留下的孤儿分支，省一次 `git branch -D`（注释 110-111）。
- 关联：`agent.go` 里 `runSync`/`runAsTeammate` 调的是 `worktree.CreateAgentWorktree`（create.go 之上的封装，见 `agent.go`），它内部走 `getOrCreateWorktree`；`HasWorktreeChanges` / `RemoveAgentWorktree` 在 `changes.go` / `cleanup.go`，决定子 Agent 结束后 worktree 保留还是清理。

> 考点（简历"降低 60%"的支撑）：多个 Team 队友**并行**改同一仓库时，每人一个独立 git worktree = 独立工作目录 + 独立分支，文件写入互不覆盖、互不触发对方正在编辑文件的锁/冲突；主 Agent 和队友也在不同目录，所以"跨文件重构"可以真的并行推进，而不是串行排队。worktree 的"快速恢复"又让重复 spawn 同一队友省掉每次 6-8s 的 fetch。

---

## 四、面试高频 Q&A

**Q1：子代理和 Fork 有什么区别？什么时候用哪个？**
子代理（`subagent_type=*`）拿一个**全新会话**，看不到父历史，适合"独立子任务"；Fork（不带 `subagent_type`）**复制父的完整上下文**（字节级重放），后台跑，适合"把进行到一半的任务分出去并行推进"。Fork 还刻意保持 tool_use 顺序和 replacement state 字节一致以命中 Prompt Cache，且**禁止嵌套 Fork**（防失控）。

**Q2：多 Agent 之间怎么通信？为什么不用 channel？**
Team 队友之间用 `FileMailBox`：每个 Agent 一个 `<id>.json` inbox，消息是 JSON 数组，投递即返回（`Send`）、拉取时过滤未读（`ReadUnread`）、处理后 `MarkAllRead`。不用内存 channel 是因为队友可能是**跨进程**（tmux/iTerm 后端）跑的，channel 跨不了进程；文件信箱跨进程、可持久、崩溃可恢复。

**Q3：并发写同一个信箱文件怎么保证不丢消息、不串消息？**
`withLock` 用 `os.OpenFile(..., O_CREATE|O_EXCL|O_WRONLY, 0644)` 原子抢锁——同一路径已存在会失败，天然无竞态。抢不到时：检查锁是否陈旧（>10s 直接删掉回收），然后 `5~100ms` 随机退避重试（最多 10 次）。拿到锁才 read→mutation→write，结束 `defer Remove` 释放。

**Q4：子代理的权限和工具是怎么限制的？**
权限：`deriveSubAgentChecker` 复用父的 Sandbox/RuleEngine，只覆盖 `Mode`（如 plan 模式）。工具：`FilterToolsForAgent` 6 层裁剪——MCP 全放、全局黑名单（禁递归 spawn）、自定义额外限制、后台白名单、定义黑名单、定义白名单（`*` 跳过）。后台子代理只能用一个"读+写文件+搜索+执行"白名单，且不能再 spawn 子代理。

**Q5：git worktree 隔离解决了什么？**
让并行队友各自在独立目录+独立分支编辑，文件写入互不覆盖；主 Agent 与队友也分离。结合"快速恢复"（读 `.git` 指针跳过 fetch，省 6-8s），使"跨文件重构"可真正并行，这也是简历"耗时降 60%"的底层支撑。

**Q6：子代理/队友是阻塞还是非阻塞？**
`runSync` 阻塞（主 Agent 等它出结果）；`runFork` 和 `runAsync`（后台子代理）和 `runAsTeammate`（队友）都**非阻塞**，结果通过 `TaskManager` 通知或信箱协调回流。headless 子代理遇到权限请求一律自动拒绝，不会卡 UI。

---

## 五、自检题（合上代码自己答）

1. `withLock` 用 `O_EXCL` 抢锁，和用 `flock()`/语言层 `sync.Mutex` 比，最大的优势是什么场景？（答：跨进程）
2. 为什么 `AllAgentDisallowedTools` 要禁掉 `Agent` 工具，但 `InProcessTeammateAllowedTools` 又把它放回来？（答：防无限递归；队友需协调，且仅允许同步子代理）
3. `buildForkedConversation` 为什么要"字节级重放"并补 tool_use 占位？（答：Prompt Cache 前缀字节一致才能命中）
4. 一句代码说明"Fork 不可再 Fork"是怎么实现的。（答：`cloneRegistryForFork` 把子 AgentTool 的 `QuerySource` 设 `ForkQuerySource`，`runFork` 开头检查它直接拒）
5. `git worktree add -B` 的大写 B 解决了什么具体问题？（答：重置被删 worktree 留下的孤儿分支，省一次 `git branch -D`）
6. 为什么 `emitProgress` 必须用非阻塞 `select/default` 发送？（答：曾因缓冲满卡死子 Agent 事件循环，导致取消失效）

---

## 六、风险提醒（面试诚实边界）

- **"降低 60%"是简历定性表述**，仓库里能支撑的是 worktree 隔离 + 快速恢复（省 fetch）的工程实现；**具体 60% 数字没有评测脚本佐证**，面试被追问要说"这是设计目标/经验估算，未跑基准对比"。
- 三种后端中 `tmux.go` / `iterm.go` 是真实实现，但**跨进程队友的"共享同一信箱目录"**依赖 `TENCODER_TEAMS_DIR` 或默认 `.tencoder/teams` 路径一致——这部分在面试里可讲设计，但别夸大"已大规模验证"。
- 简历头部注释仍是"来源：公众号@小林coding"的模板头（见各文件第 1-4 行），面试时如实说明代码是**学习性复现/二次开发**，避免被误认为完全原创。
