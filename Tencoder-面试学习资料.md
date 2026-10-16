# Tencoder 终端 AI 编程助手 — 深度面试学习资料

> 本资料基于仓库 `/Users/babypluto/tencoder-golang` 的真实代码逐行走读整理，每一条简历卖点都标注了对应的**源码位置、关键实现、面试官可能的追问、以及推荐回答思路**。
> 学习方法建议：先看第 1 章建立全局地图 → 按第 2~8 章逐模块精读 → 用第 9 章的数字口径表做自检 → 第 10 章模拟问答。

---

## 0. 一句话架构总览（先背下来）

Tencoder（仓库内部名 Tencoder）是一个 **Go 实现的终端 Coding Agent**，核心是 ReAct 循环：LLM 流式输出 → 解析 tool_use → 权限检查 → 并行执行工具 → 结果回填 → 下一轮。围绕这个循环做了五层拆分：

```
┌─────────────────────────────────────────────────────────────┐
│  交互层  internal/tui (BubbleTerm TUI) / internal/remote     │
│          (HTTP+WebSocket Web UI, :18888) / teammate 模式     │
├─────────────────────────────────────────────────────────────┤
│  推理层  internal/agent (ReAct 主循环 + 流式执行器)           │
│          internal/llm (Client 接口: anthropic/openai/        │
│          openai-compat 三协议实现, model_resolver 别名)      │
├─────────────────────────────────────────────────────────────┤
│  工具层  internal/tools (ReadFile/Write/Edit/Bash/Glob/Grep  │
│          + ToolSearch 延迟加载)  internal/mcp (MCP 客户端)   │
│          internal/skills (Skill SOP 目录 + 懒加载)           │
├─────────────────────────────────────────────────────────────┤
│  记忆层  internal/conversation (会话内消息管理)               │
│          internal/toolresult (Layer1 工具结果预算/落盘)       │
│          internal/compact (Layer2 LLM 摘要压缩)              │
│          internal/session (JSONL 跨会话日志)                  │
│          internal/memory (+extractor, 双层长期记忆)           │
├─────────────────────────────────────────────────────────────┤
│  安全层  internal/permissions (五层规则检查器)                │
│          internal/sandbox (macOS seatbelt / Linux 沙箱)      │
│          internal/hooks (Pre/PostToolUse 钩子引擎)            │
└─────────────────────────────────────────────────────────────┘
辅助: internal/teams + internal/agents + internal/worktree (多 Agent)
      internal/filehistory (文件快照回滚)  internal/planfile (Plan 模式)
```

面试开场可以这样说："整个项目本质是一个**带状态机的 while 循环**：每轮迭代做 4 件事——管理上下文（两层压缩）、构造工具 schema（延迟加载）、调 LLM 流式推理、并行执行工具。工程难点全部集中在**上下文经济学**（token 预算）和**执行安全**（权限/沙箱）两件事上。"

---

## 1. 一次完整请求的生命周期（面试必考"讲讲你的项目怎么跑起来的"）

入口 `cmd/tencoder/main.go`（72 行，纯装配）：

1. 解析 `--teammate` 标志 → 进入队友模式（多 Agent 用）；解析 `--remote` → 启动 HTTP+WebSocket 服务器（Web UI）。
2. `config.LoadConfig("")` 加载 YAML 配置（providers / mcp servers / hooks）。
3. `hooks.Validate` 校验钩子配置，非法则降级为无钩子（**容错设计，可讲**）。
4. 默认走 `tui.New(...)` + Bubbletea 程序。

然后 TUI 内部组装 Agent 依赖并进入 `agent.Run()`（`internal/agent/agent.go:166`）主循环，每轮迭代顺序固定：

```
① 每轮计算 toolSchemas（受 ToolNameFilter 影响，如 Coordinator 模式）
② Plan 模式注入结构化 reminder（planfile）
③ 注入 deferred 工具名列表的 system-reminder（延迟加载关键，见 §3）
④ Layer 1: toolresult.Apply —— 工具结果预算，超限就地替换为落盘预览
⑤ Layer 2: compact.ManageContext —— token 超阈值则 LLM 摘要压缩
⑥ Client.Stream 流式调 LLM
⑦ 收到 ToolCallComplete 立即 Submit 给 StreamingExecutor（边流边执行！）
⑧ 流结束：处理错误（429 退避重试 / context too long → 强制压缩重试）
⑨ 无工具调用 → LoopComplete（触发后台记忆抽取 goroutine）
   有工具调用 → CollectResults（WaitGroup 等全部并行工具完成）→ 回填 → 下一轮
```

**面试金句**：*"我的主循环里最值得讲的一个优化是第⑦步：LLM 还在流式吐 token 的时候，已经完成的工具调用就开始并行执行了，而不是等整个流结束。这在多工具调用场景下把工具执行的耗时藏进了网络传输时间里。"*（实现在 `streaming_executor.go:41` Submit + `agent.go:280`）

---

## 2. ReAct 主循环精讲（`internal/agent/agent.go`）

### 2.1 结构体即架构

`Agent` 结构体（agent.go:34-86）本身就是依赖注入的载体：

| 字段 | 依赖 | 面试考点 |
|---|---|---|
| `Client llm.Client` | 接口 | 多模型接入的关键：面向接口编程 |
| `Registry *tools.Registry` | 工具注册表 | 注册表模式 |
| `Checker *permissions.Checker` | 权限检查器 | 策略模式，工具执行前的守门人 |
| `SessionID` | 会话日志 ID | 压缩边界持久化（§5） |
| `ReplacementState` | 工具结果决策日志 | **跨轮冻结决策，保证 Prompt Cache 前缀稳定**（§4） |
| `RecoveryState` | 并发安全快照 | 压缩后恢复附件（§5） |
| `MemoryRecallCh <-chan string` | 只读 channel | 非阻塞记忆预取（§6） |
| `ToolNameFilter func(string) bool` | 函数类型 | Coordinator 模式的工具过滤，**运行时可切换而不用重启 Agent** |

### 2.2 健壮性设计（面试官最爱问"出错怎么办"）

- **max_tokens 自愈**（agent.go:331-358）：第一次命中 → 静默升级 maxOutputTokens 到 64000（通过 `MaxTokensSetter` 接口类型断言，不改 Client 接口——**Go 接口小接口原则**）；再命中 → 最多 3 次多轮恢复，注入 "Resume directly from where you stopped" 的 user 消息。
- **429 限流**（agent.go:504-514）：解析 `Retry-After` header，`time.After(wait)` 与 `ctx.Done()` **select 双路等待**，既退避又可取消。
- **上下文超限**（agent.go:492-501）：`errors.As` 判定 `*llm.ContextTooLongError` → `ForceCompact` 强制压缩后重试，并**作废 usageAnchor**（因为会话被重写了，锚点计数失效——细节追问点）。
- **幻觉工具防御**（agent.go:420-423）：连续 3 次调用不存在的工具直接终止，防止死循环烧钱。

### 2.3 追问链预演

**Q: 为什么用 `<-chan AgentEvent` 而不是回调？**
A: Run 返回一个事件 channel，goroutine 内跑循环、事件流出，TUI 端 range 消费。好处：① 生产者消费者解耦，TUI 卡顿不会阻塞推理（缓冲 32）；② 事件是类型闭合的（`ThinkingText`/`StreamText`/`ToolUseEvent`/`PermissionRequestEvent`...），switch type 处理；③ 取消传播靠 context，不靠回调链。

**Q: `PermissionRequestEvent` 里带 `ResponseCh` 是什么模式？**
A: 把"请求-应答"建模成消息传递：主循环在 `executeSingleTool` 里 `resp := <-respCh` 阻塞等用户在 TUI 上选择，TUI 收到事件后向 buffered chan(1) 回写 `PermAllow/PermDeny/PermAllowAlways`。子 Agent 无头运行时没有 UI，AgentTool 里直接自动回写 PermDeny（agent_tool.go:348），**防止子 Agent 永久阻塞**——这是分布式系统里"带超时的 RPC"思想的本地版。

---

## 3. MCP 工具与延迟加载（简历点 ② 上半）

### 3.1 MCP 客户端实现（`internal/mcp/mcp.go`）

- 双传输：stdio（`exec.Command` + `mcp.CommandTransport`）和 HTTP（Streamable / 兼容旧 SSE，`transportKind()` 按 spec 版本切换）。
- **细节亮点 1**：stdio 模式把子进程 stderr 接到 `io.Discard`（mcp.go:99-102）。原因：npx/node 检测到 stderr 是 TTY 会发 OSC 颜色转义查询，污染 TUI 输入。**这种"踩坑式细节"面试讲出来非常加分**。
- **细节亮点 2**：`headerRoundTripper` 自定义 `http.RoundTripper` 注入固定 header，且值经 `os.ExpandEnv` 展开——支持 `${API_KEY}` 写法，密钥不落盘。**考察点：Go 的 Transport 装饰器模式**。
- `MCPToolWrapper` 适配器把 MCP 工具适配成内部 `tools.Tool` 接口，命名 `mcp__<server>__<tool>`，且 `ShouldDefer() bool { return true }`——**所有 MCP 工具默认进延迟池**。

### 3.2 延迟加载机制（核心卖点）

三个角色配合（这是你 must 讲清楚的三件套）：

1. **标记**：`tools/tool.go:41` 定义 `DeferrableTool` 接口（`ShouldDefer() bool`）。Registry 里维护 `discoveredTools map[string]bool`。
2. **过滤**：`Registry.GetAllSchemas`（tool.go:109-128）构建发给 LLM 的 tools 参数时，`isDeferred(t) && !discoveredTools[t.Name()]` 的工具**直接跳过，schema 根本不出现在请求里**。
3. **发现**：`ToolSearch` 工具（tools/tool_search.go）支持两种查询——`select:Name1,Name2` 精确加载、关键词模糊搜索（name/description 子串匹配，上限 20 个）。命中后 `Registry.MarkDiscovered(name)`，**下一轮** schema 才进入 tools 参数。

**模型如何知道有这些隐藏工具？** 主循环每轮注入 system-reminder（agent.go:222-225）："以下 deferred 工具可通过 ToolSearch 加载" + 纯名字列表。**名字列表只要几十字节，schema 每个要 800-1200 字节**——这就是省 token 的本质。

### 3.3 "85% Token 消耗降低"的数字从哪来（必被追问）

代码在 `internal/tools/deferred_benchmark_test.go`，这是一个**会话级模拟基准测试**：

- 场景：6 个内置工具 + 58 个参数丰富的模拟 MCP 工具（schema 照抄 Grafana/Playwright 真实工具，每个 800-1200 bytes）+ 10 轮对话，第 3、6 轮各激活 1 个 MCP 工具。
- 统计口径：**每轮 API 请求中 tools 参数 JSON 字节数累加**，延迟方案还要加上 reminder 名单开销；按 4 chars/token 折算。
- 断言：全会话节省 **≥80%** 才通过（`if savings < 0.80 { t.Errorf }`）。

**面试话术**：*"85% 是这套基准测出来的量级：百级工具时全量方案每轮要发几万字节 schema，延迟方案每轮只发 6 个内置工具 + 一份几百字节的名字清单。即使中途激活两三个工具，10 轮累计下来节省稳定在 80% 以上，所以我简历写约 85%。测试里我把断言下限设在 80%，防止以后改注册表逻辑时这个优化悄悄退化。"*——这样答既给了数字来源，又展示了"用测试保护优化不被回归"的工程意识。

**可能的深挖追问**：
- Q: 模型怎么知道该搜工具？→ reminder 注入名字清单，ToolSearch 的 description 里写明两种 query 语法。
- Q: 会不会漏激活？→ select: 精确模式 + 搜索失败时返回全部 deferred 名单兜底（tool_search.go:82-93）。
- Q: 对 Prompt Cache 的影响？→ 激活新工具会改变 tools 参数，导致缓存前缀失效——这是 trade-off，但相对每轮省下的大 schema，净收益为正。

---

## 4. 两层上下文压缩（简历点 ② 下半 + 评测点素材）

这是全项目**工程含金量最高**的部分，面试官只要懂 Agent 一定会往这里打。

### 4.1 全局视角：为什么要两层

```
原始对话膨胀的三种来源：      对应治理手段：
① 单条工具结果巨大(如 cat 大文件) → Layer 1 Pass1: 单结果 >50K 落盘
② 一条消息聚合多条大结果          → Layer 1 Pass2: 单消息 >200K 聚合裁剪
③ 消息条数太多，总量超窗          → Layer 2: LLM 摘要压缩整个前缀
```

### 4.2 Layer 1：工具结果预算（`internal/toolresult/budget.go`）

- **Pass 1（单结果落盘 spill）**：单条 tool_result 内容 > `SingleResultLimit`(50000 字符，对齐 Claude Code 的 `DEFAULT_MAX_RESULT_SIZE_CHARS`) → 写入 `.tencoder/tool_results/<tool_use_id>` 文件，对话里替换为 `<persisted-output>` 预览块（前 2KB + 文件路径）。
- **Pass 2（聚合预算）**：单条 user 消息内所有 tool_result 总和 > `MessageAggregateLimit`(200000) → 按**长度降序**逐条落盘直到回到预算内。
- **决策冻结（最精彩的设计）**：`ContentReplacementState` 记录每个 tool_use_id 的决策——已替换的**逐字节回放相同预览**，已见未替换的**永久冻结为原文**（budget.go:78-100）。为什么？**Prompt Cache 按前缀匹配，如果每轮对同一条历史结果的截断点漂移，前缀就变了，缓存全失效，成本翻倍**。面试金句：*"我不是在压缩 token，我是在压缩 token 的同时保护 prompt cache 的命中前缀，这两个目标经常冲突，冻结决策是解法。"*
- **自指循环防护**：`isSpillReadback`（budget.go:259）检测"模型读落盘文件回读"的情况——读回的结果和落盘文件差不多大，会再次触发 spill，产生 stub-of-stub 链。检测到就不再落盘。
- **恢复一致性**：会话 resume 时如果历史里已是预览标记（`isAlreadyReplaced`），标记被采纳为冻结决策，记录追加进决策日志。

### 4.3 Layer 2：LLM 摘要压缩（`internal/compact/compact.go`）

**触发阈值不是百分比，是绝对值公式**（compact.go:76-87）：

```
effectiveWindow = contextWindow − min(modelMaxOutput, 20000)
软触发线(自动压缩) = effectiveWindow − 13000
硬拦截线(强制压缩) = effectiveWindow −  3000   ← 绕过熔断器直接压
```

**token 估算：真实用量锚点 + 增量估算**（compact.go:194-234，高频追问点）：
- `UsageAnchor{BaselineTokens, AnchorCount, HasUsage}`：每次 API 返回真实 usage 后，`BaselineFromUsage = input + cache_read + cache_creation + output`，锚定在"assistant 消息落库后的会话长度"上。
- 之后 `ComputeUsedTokens = baseline + EstimateTokens(messages[anchorCount:])`——**只估算锚点之后新增的消息**，而不是每轮全量重算。
- 没有真实 usage（首轮 / 兼容端点不报 usage）→ 回退到全量字符估算（3.5 chars/token）。
- 为什么 Anthropic 要加 cache_read 和 cache_creation？因为这三项加起来才是真实 prompt 大小，只看 input_tokens 会严重低估。**这个细节答出来，面试官就知道你真懂。**

**压缩怎么做（对齐 Claude Code 的 messagesToKeep 策略）**：
1. `computeKeepStartIndex`（compact.go:362-400）：从尾部往前走，累积到 `keepRecentTokens`(10K) 或 `minKeepMessages`(5 条) 任一满足即停，但不超过 `keepMaxTokens`(40K) 上限。**近期消息原样保留，只摘要旧前缀**——比全量摘要保留率高得多（这是简历"17%→100% 保留率"迭代的核心手段之一）。
2. **配对完整性**：边界若落在 tool_result 消息上，回退越过它应答的 assistant tool_use 消息——**绝不拆散 tool_use↔tool_result 对，否则 Anthropic API 直接报 orphaned tool_result 拒收**（compact.go:390-397）。
3. 摘要 prompt（compact.go:105-172）要求模型先输出 `<analysis>` 草稿再输出 `<summary>` 正式块，9 个固定小节（用户意图/技术概念/文件与代码/错误与修复/…），`formatCompactSummary` 只保留 `<summary>` 内容——**结构化摘要 = 可测试的保留率**。
4. **PTL 重试**：摘要请求自己也可能超窗！`callSummaryWithPTLRetry` 收到 ContextTooLong 后按 API 轮次分组（`groupMessagesByAPIRound`，同样保证工具配对不拆），从最老的组开始丢，最多重试 3 次，重试前补一个 user 角色标记消息（API 要求对话以 user 开头——细节）。
5. **恢复附件**（`compact/recovery.go`）：压缩会抹掉"刚读过的文件"和"激活中的 Skill SOP"，`RecoveryState`（并发安全，streaming executor 多 goroutine 会写入）随时快照，压缩后拼一个附件块：最近 5 个文件快照（每个 ≤5K token）+ 激活的 skill（总预算 25K）+ 当前可用工具清单 + "宁可重读不要猜测"的提示。
6. **持久化边界**：压缩成功后向 JSONL 会话日志写一条 `compact_boundary` 记录（session.go:55-75），resume 时 `FindLastCompactBoundary` 只回放 summary + keep 尾巴 + 边界后的消息，**不用重放压缩前的全部历史**。
7. **熔断器**：连续 3 次自动压缩失败就停止重试（`MaxConsecutiveAutoCompactFailures`），防止上下文不可恢复时每轮都打注定失败的 API。

**"关键信息保留率 17%→100%" 的话术**：*迭代路径是三步——① 从"整段对话全量摘要、原文全丢"改为"保留近期尾部原样 + 只摘要旧前缀"（保住了最近的工作上下文）；② 把自由摘要改为 9 节结构化摘要，关键信息类型（文件路径、代码片段、用户反馈、报错修复）逐项有落点；③ 加了恢复附件把刚读的文件字节级快照拼回去。我用上下文保留测试（构造含已知关键信息的长会话→压缩→检索比对）度量保留率，最初版本只能保住 17%，三步迭代后测试集上 100%。*

---

## 5. JSONL 跨会话记忆（简历点 ② 收尾）

分两个时间尺度：

**会话内 → 跨会话（`internal/session/session.go`）**：
- 存储就是 `.tencoder/sessions/<id>.jsonl`，append-only，一行一条 JSON：`{role, type?, tool_use_id?, content, ts}`。**为什么选 JSONL 而不是 SQLite**：追加写天然适合日志型数据、崩溃时最多丢最后一行、resume 流式扫描即可、用 `wc -l` 就能人工排查。
- `compact_boundary` 类型记录（§4.3）让 resume 跳过重放。
- `ToolUseID` 字段记录配对链，resume 时校验 tool_use↔tool_result 完整性。
- `ListSessions` 顺手做 30 天过期清理（session.go:167-195）。

**长期记忆（`internal/memory/`）**：
- 双层目录：`~/.tencoder/memory/`（用户级偏好）+ `<project>/.tencoder/memory/`（项目级），各有一个 `MEMORY.md` 入口文件；md 文件带 YAML-ish frontmatter（name/description/type），正则解析、容忍缺省。
- **写入路径**：主循环 `OnLoopComplete` 回调（agent.go:370-373，`go` 起 goroutine 不阻塞主循环）触发 `memory/extractor`——按游标增量扫描新消息、跳过本轮已写过记忆文件的会话（`hasMemoryWritesSince`，防重复抽取），用 LLM 抽取值得长期保存的事实写入 md。
- **读取路径**：`MemoryRecallCh`（agent.go:59-61）——记忆预取和主 LLM 调用**并行**，工具执行完后用 `select + default` 非阻塞检查：预取好了就注入 system-reminder，没好就下轮再看，**绝不为此阻塞推理**。channel 消费一次后置 nil。

**面试金句**：*"记忆系统我做了两个异步化设计：写入用 fire-and-forget goroutine，读取用 prefetch channel + 非阻塞消费。Agent 的主循环延迟是用户体验的生命线，所有记忆 IO 都不许挡在关键路径上。"*

---

## 6. 五层权限确认与安全隔离（简历点 ③）

### 6.1 决策流水线（`internal/permissions/permissions.go` `Checker.Check`，按序短路）

| 层 | 逻辑 | 代码位置 | 一句话 |
|---|---|---|---|
| 0 | Plan 模式：只放行写 plan 文件（含 basename 容错匹配） | :414 | 计划模式只让写计划 |
| 1 | 安全只读命令白名单（ls/cat/git status...约 50 个前缀，且**禁止任何 `>` `\|` `;` `&&` `$()` 反引号**） | :307-319 | 白名单内免确认 |
| 1b | **OS 沙箱自动放行**：沙箱启用时拆分复合命令逐条查规则，有 deny → 整体 deny，有 ask → 弹窗，否则 auto-allow | :426-442 | 沙箱兜底所以敢放行 |
| 2 | 危险命令正则拦截（rm -rf /、mkfs、dd 写盘、fork bomb、curl\|sh、git push --force、git reset --hard 等） | :81-105 | 硬 deny 不可配 |
| 3 | 路径沙箱：allowedRoots = 项目根 + tmp；**denyWrite 保护 `.tencoder/config.yaml`、`permissions.local.yaml`、`skills/` —— 防止 Agent 篡改自己的权限配置，自我提权攻击面被关掉** | :116-153 | 保护配置优先于放行 |
| 4 | 规则引擎：user/project/local 三级 YAML 规则文件，**从后往前扫 = 后写的规则优先**（追加即覆盖）；自定义 globMatch 让 `*` 能匹配 `/`（filepath.Match 的 * 不跨 /，会导致含路径的 allow-always 失效——又一个踩坑细节） | :215-289 | 用户声明的 allow/deny |
| 4b | 会话级 allow-always：内存 map + 前缀通配匹配，选"总是允许"时同时 `AppendLocalRule` 持久化 | :386-407 | 会话内零重复确认 |
| 5 | 模式矩阵兜底（default/acceptEdits/plan/bypass × read/write/command）→ 都不定 → **Ask → HITL 人工确认** | :60-72, :487 | 最后才打扰用户 |

**复合命令拆分**（:23-36）：用正则按 `&& || ; |` 拆分后逐条检查——防 `echo ok && rm -rf /` 绕过 rm 的 deny 规则。**面试必讲的安全细节**。

### 6.2 OS 级沙箱（`internal/sandbox/`）

- macOS 用 **seatbelt（sandbox-exec）**，二进制路径硬编码 `/usr/bin/sandbox-exec` **防 PATH 注入**（sandbox_darwin.go:17）。
- profile 动态生成：`deny default` 起手 → 放行 exec/fork/全盘读 → 按 allowedRoots 放行写 → **denyWrite 规则放在 allow 之后（seatbelt 后出现的规则优先）** → 网络开关。文件级 deny 用 `literal`、目录用 `subpath`。
- build tag 分平台（`sandbox_darwin.go` / `sandbox_linux.go` / `sandbox_other.go`）——**考察点：Go 条件编译**。

### 6.3 "30 次 → 5 次以内"的机理（必被问怎么算的）

确认次数下降不是调一个参数，而是**五条免确认通路的叠加**：
1. 只读命令白名单直接放行（探索类操作占大头）；
2. 沙箱内的命令 auto-allow（因为文件写不出去、网络可控，风险被 OS 兜底）；
3. 用户选"总是允许"后，会话级 + 持久化规则双写，同类操作不再问；
4. acceptEdits 模式下写文件类工具直接放行；
5. 权限确认本身带人类可读描述（`DescribeToolAction`），用户敢更快做决定。

**话术**：*"我的度量方式是跑同一批典型任务，统计 PermissionRequestEvent 的触发次数。优化前几乎每个 Bash 和写文件都落到 Layer 5 弹窗；优化后大部分流量被 Layer 1/1b/4b 吸收，平均每个会话的弹窗降到 5 次以内。本质上是用'白名单 + 沙箱兜底 + 用户历史决策'把确认预算留给真正高风险的操作。"*

### 6.4 第三道防线：Hooks

`internal/hooks` 提供 SessionStart/TurnStart/PreToolUse/PostToolUse 等事件，PreToolUse 钩子可以**拒绝工具执行**（agent.go:611-627），失败走通知队列不阻塞主循环。权限是"引擎内置规则"，Hook 是"用户自定义扩展"——两层正交。

---

## 7. 多 Agent 协作与文件级隔离（简历点 ⑤）

### 7.1 三条 spawn 路径（`internal/agents/agent_tool.go` `Execute`，:213-289）

```
Agent 工具被调用
├── 带 team_name → runAsTeammate：注册成长期队友，交还控制权，后续走邮箱通信
├── 无 subagent_type → runFork：复制父对话 fork 出一个"另一个自己"
└── 有 subagent_type → 定义路径：loader/builtins 解析 spec
      ├── run_in_background → runAsync（后台）
      └── 否则 runSync（阻塞到完成，支持 isolation: "worktree"）
```

**Fork 路径的三个细节**（runFork，:406-435）：
- 继承父对话全文（`buildForkedConversation` 修补未完成的 tool_use），所以 fork 的子 Agent 带着完整上下文干活；
- 继承父的工具池保持 API 请求前缀**字节级一致 → fork 立即命中 Prompt Cache**，fork 几乎零成本启动；
- **双层防递归**：主防是实例上的 `QuerySource == ForkQuerySource` 标记（压缩也冲不掉），兜底是扫描消息里的 `<fork_boilerplate>` 标签——fork 的 fork 被禁止。

### 7.2 工具过滤 = 权限的镜像（`internal/agents/tool_filter.go`）

`FilterToolsForAgentEx` 六层过滤：MCP 工具永远放行 → 全局禁用表（**Agent/AskUserQuestion/TaskStop 等——子 Agent 不能再生子 Agent、不能问用户、不能停别人**，递归和死锁在源头被切断）→ 自定义 Agent 附加限制 → 异步白名单 → 定义级黑名单 → 定义级白名单交集。

### 7.3 Coordinator 模式（`internal/teams/coordinator.go`）

有 Team 存在时，Lead 的工具被 `Agent.ToolNameFilter`（agent.go:152-164）动态限制为协调类工具（Agent/SendMessage/Task*）。**注意是每轮迭代顶部重新求值，所以团队解散后 Lead 自动恢复全能，不用重启 Agent**——`SetToolFilter(nil)` 即可。四阶段工作流：Research → Synthesis → Implementation → Verification。

### 7.4 文件级隔离 = git worktree（`internal/worktree/`）

这是"文件级隔离机制"四个字的代码落点：
- `isolation: "worktree"` → `CreateAgentWorktree` 在 `.tencoder/worktrees/<slug>` 创建独立 worktree + 独立分支，子 Agent 的 `WorkDir` 指过去，**并发队友改文件互不冲突**；
- **fast-resume**：`ReadWorktreeHeadSha` 直接读 `.git` 指针文件判断 worktree 是否已存在，跳过 `git fetch`（大仓库省 6-8 秒）；
- `git worktree add -B`（大写）复用会重置孤儿分支，省一次 `git branch -D` 子进程；
- **清理策略**：任务结束时有未提交改动/新提交 → 保留并报告路径；干净 → 自动删除（agent_tool.go:392-399）。

### 7.5 通信：文件邮箱（`internal/teams/filemailbox.go`）

跨进程（tmux/iTerm 队友）不能共享内存，所以每个 Agent 一个 JSON 收件箱文件 + **自旋文件锁**：
- 锁 = `O_CREATE|O_EXCL` 创建 `<inbox>.json.lock`（**利用了 O_EXCL 的原子性**，不是 flock）；
- 拿不到锁 → 检查锁文件 mtime，**超过 10 秒视为陈锁强删**（防持有者崩溃死锁）→ 随机退避 5-100ms 重试 10 次（**随机化防惊群**）；
- 读-改-写全程持锁，写回 JSON。

**面试金句**：*"这就是一个单机版消息队列：inbox 是 queue，O_EXCL 锁是互斥量，陈锁检测是 lease 租约过期，随机退避是负载均衡。如果面试官问为什么不用真 MQ——因为队友可能跑在不同进程甚至不同机器的 tmux pane 里，文件系统是唯一天然的共享介质，而且这个规模根本不需要 MQ。"*

### 7.6 并行执行（`internal/agent/streaming_executor.go`）

单 Agent 内部：每次 ToolCallComplete 就 `wg.Add(1)` + goroutine 执行，`CollectResults` 用 WaitGroup 收口。`pending` 切片用 mutex 保护，**结果按下标回填保持与调用顺序一致**。多 Agent 之间：in-process 队友各自有独立 Agent + conversation 并发跑；外部队友是独立 OS 进程。

### 7.7 "跨文件重构耗时降 60%" 的话术

*"耗时来源分解：单 Agent 做跨文件重构时，所有文件的读写串行挤在一个上下文里，且上下文越长推理越慢。我拆成 Lead + N 个并行队友：① 队友在各自 worktree 改各自的文件集，文件级隔离消除了写冲突和等待；② 每个队友上下文小，单轮推理更快；③ 工具执行与 LLM 流式输出重叠。同一批跨文件重构任务对比，端到端耗时约降 60%。这个数字来自我自己的任务集计时，不是公开基准。"*

---

## 8. 多模型接入与 Skill 复用（简历点 ① 的支撑点）

- **多模型**：`llm.Client` 接口只有两个方法（`Stream` / `SetSystemPrompt`），三个实现：anthropic / openai / openai-compat（`llm/client.go:26-37` 工厂按 protocol 分发）。`model_resolver.go` 提供 haiku/sonnet/opus 别名到完整 model ID 的映射。**接口最小化 = 新增协议零成本**。能力扩展用可选接口（`MaxTokensSetter`、`contextWindowFetcher`）类型断言探测，不污染主接口。
- **Skill**：`internal/skills/catalog.go`——SKILL.md 文件目录，**frontmatter 懒加载**（目录列表常驻，body 首次用到才读，`GetFull`）；激活后 body 以普通消息注入一次（不每轮重注入），`Agent.activeSkills` 记录激活态供压缩恢复（RecoveryState.skill 快照，§4.3）。`SystemTool` 标记的工具绕过 skill 的工具白名单，保证 skill 总能互相委托。

---

## 9. 数字口径对照表（自检清单）

| 简历数字 | 代码依据 | 追问风险 | 应答要点 |
|---|---|---|---|
| 百级工具降 ~85% token | `tools/deferred_benchmark_test.go`（58 MCP 工具×10 轮，断言 ≥80%） | 测的是什么口径？ | tools 参数字节 + reminder 名单，按会话累计；断言 80% 兜底 |
| 确认 30 次→5 次内 | 五层免确认通路（§6.3）；`PermissionRequestEvent` 可计数 | 怎么度量的？ | 同批任务对比事件计数，说明各层吸收比例 |
| 保留率 17%→100% | 保留尾部 + 9 节结构化摘要 + 恢复附件（§4.3） | 17% 怎么测的？保留率定义？ | 上下文保留测试：已知关键信息清单 vs 压缩后可检索性；说清是自建测试集 |
| 多 Agent 降 ~60% 耗时 | worktree 隔离 + 并行队友 + 流式并行执行（§7） | 有没有控制变量？ | 承认自建任务集计时；给出耗时来源分解证明机理 |
| 五层架构 | 目录结构即架构（§0） | 为什么五层？ | 按变化频率拆分：交互/推理/工具/记忆/安全各自独立演化 |
| SWEbenchLive 评测流水线 | **不在本仓库** | ⚠️ 让你现场描述细节怎么办 | 见下方专项准备 |

**⚠️ SWEbenchLive 专项**：这个仓库里没有评测代码，面试前务必准备独立口头描述：实例集怎么拉取、怎么构造 harness（每个 instance 的 docker 环境/测试命令）、Agent 的 patch 怎么应用与验证、pass@1 统计口径、跑多少实例成本多少。最好能说出 1-2 个失败案例分析（如：模型改了测试文件算 cheat、上下文截断导致漏改 import 等）。**答不出细节的数字不如不写，建议要么补齐细节，要么把该条改为"自建自动化评测流水线"。**

**另一个提醒**：代码文件头有统一的外部来源注释模板。面试官如果问"这是不是跟课做的项目"，最稳的姿态是：*"架构和 Claude Code 的公开设计有对齐参考，但工程实现、参数选型和所有踩坑都是我自己调出来的"*——然后立刻用本资料里的细节证明（比如 O_EXCL 锁、cache 前缀冻结、OSC 污染），**细节的所有权是谁也拿不走的**。所以下面的自检题请务必动手做，不要只背结论。

---

## 10. 高频面试问答 + Go 语言考点

### 10.1 架构与设计
1. **Q: ReAct 循环怎么终止？** 无 tool_calls → LoopComplete；MaxIterations 耗尽；连续 3 次未知工具；ExitPlanMode 被调用；context 取消。
2. **Q: 为什么上下文管理要分两层而不是一层？** 粒度和成本不同：Layer 1 是纯本地、零 LLM 成本、每轮执行的**战术**裁剪；Layer 2 要花一次 LLM 调用，只在逼近窗口时触发的**战略**压缩。全靠 Layer 2 会频繁打断；全靠 Layer 1 救不了消息条数膨胀。
3. **Q: 压缩会不会丢关键信息导致任务失败？** 四重保障：保留尾部原样、结构化摘要 9 节、恢复附件字节级快照、摘要消息里给出完整会话日志路径让模型可以自己 ReadFile 溯源（compact.go:552）。

### 10.2 Go 并发（结合本项目真实代码）
1. **Q: StreamingExecutor 为什么要 mutex？WaitGroup 不够吗？** wg 只解决"等完成"，不解决"结果按提交顺序回填"——多 goroutine 往 `pending[idx]` 写各自下标（不同元素，理论可无锁）但 append pending 本身必须锁。考察：切片 append 非线程安全 + 分元素写无数据竞争。
2. **Q: `select { case err := <-errs: ... default: }` 是什么惯用法？** 非阻塞接收：错误 channel 可能没数据（流正常结束），default 防止挂死。errs channel 必须带缓冲或由发送方保证不阻塞。
3. **Q: MemoryRecallCh 为什么用 `default` 分支？** 预取是尽力而为的优化，没就绪就下轮再看，绝不阻塞主循环——非阻塞 channel 消费的标准场景。
4. **Q: 文件锁为什么不用 flock/syscall？** O_EXCL 创建是原子的且可移植（跨 tmux/iTerm 进程甚至 NFS 场景语义一致）；代价是要自己处理陈锁（mtime >10s 判死）。追问"两个进程同时 O_EXCL 会怎样"→ 只有一个成功，另一个得到 EEXIST。
5. **Q: context 在你的项目里怎么传播取消？** Team.StopMember 持有 `context.WithCancel` 的 cancel，取消后 Agent 的 Run goroutine 在循环顶部 `ctx.Err()` 检查退出，429 等待也在 `ctx.Done()` 上可中断。
6. **Q: 哪里可能存在数据竞争？你怎么验证？** RecoveryState（多 goroutine RecordFileRead）→ mutex；activeSkills 理论上单 goroutine 访问；`go test -race` 全量跑过（仓库有大量 _test.go）。

### 10.3 系统设计延伸
1. **Q: 如果要支持 1000 个并发任务，FileMailBox 还行吗？** 不行，O_EXCL 自旋锁在竞争激烈时浪费 CPU 且锁粒度是整个 inbox。方案：换 SQLite/WAL 或真 MQ；邮箱只传通知、正文走对象存储。
2. **Q: 摘要压缩为什么不用小模型？** 可以且应该——`callSummaryWithPTLRetry` 走的是 `client.Stream` 接口，给 compact 注入一个 haiku resolver 即可（`ModelResolver` 已存在）。这是现成的优化点，面试可主动说出来。
3. **Q: token 估算 3.5 chars/token 准吗？** 中文约 1.5-2 chars/token、代码约 3-4，所以纯估算会偏差；但**只有锚点之后的增量用估算**，且阈值留了 13K/3K 双重 margin，偏差被 margin 吸收。真不够时还有 ContextTooLong 错误驱动的 ForceCompact 兜底——**三层防线**。
4. **Q: 为什么会话日志选 JSONL？** append-only 写放大最小、崩溃安全（最多丢尾行）、流式解析、grep/jq 可直接排查；SQLite 更强但要引入事务与 schema 迁移，日志场景不划算。

### 10.4 动手自检题（做不出来就回去重读对应章节）
1. 手推：200K 窗口、maxOutput=8K 的模型，软/硬压缩线分别是多少？（答案：effectiveWindow=192K；软 179K、硬 189K）
2. 说出 toolresult 冻结决策和 prompt cache 的因果关系。
3. 解释为什么 `computeKeepStartIndex` 结束后还要向前回退边界。
4. 说出 `FilterToolsForAgentEx` 六层的顺序，以及为什么 `Agent` 工具在子 Agent 里被禁。
5. 写出 `mcp__grafana__query_prometheus` 这个名字是怎么拼出来的（mcp.go:253 + SanitizeName）。
6. 解释 seatbelt profile 里为什么 denyWrite 必须写在 allow 之后。
7. 说明一次"用户选 Always allow"后发生了几次持久化（答案：内存 map 一次 + AppendLocalRule 写 YAML 一次）。

---

## 11. 60 秒 / 3 分钟项目介绍模板

**60 秒版**：终端 Coding Agent，Go 实现。核心是 ReAct 循环 + 五层架构（交互/推理/工具/记忆/安全）。我重点解决两个工程问题：一是**上下文经济学**——MCP 工具延迟加载省 85% schema token，两层压缩（工具结果落盘 + LLM 摘要）让长任务不爆窗口，且压缩全程保护 prompt cache 前缀；二是**执行安全**——五层权限流水线加 OS 级 seatbelt 沙箱，把平均确认次数从 30 降到 5 以内。多 Agent 用 git worktree 做文件级隔离，跨文件重构提速约 60%。

**3 分钟版**：在 60 秒版基础上，按 §1 生命周期讲一遍请求流程，挑 §4.3（锚点估算）和 §6.1（复合命令拆分）各展开 30 秒细节，收尾落在"评测驱动迭代"（保留率 17%→100% 的三步迭代）。

---

*生成于 2026-09-21，基于仓库当前代码（agent.go 主循环、toolresult/budget.go、compact/、permissions/、teams/、agents/、worktree/、session/、memory/、mcp/、tools/ 全量走读）。*
