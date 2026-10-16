# 07 - 多模型接入与 Prompt 组装（协议路由 / 模型别名 / 系统提示拼装 / Prompt Cache 锚点）

> 对应简历卖点：*「多模型支持：通过协议适配层（Anthropic / OpenAI / OpenAI-Compatible）统一接入，子代理可按需切换 sonnet/opus/haiku；系统提示按优先级分段拼装，并锚定 Anthropic Prompt Cache 前缀以降低成本」*
> 本模块讲清三件事：**① 一个 `Client` 接口怎么屏蔽三种协议**；**② "sonnet/opus/haiku" 短名怎么映射到真实模型并切到子代理**；**③ 系统提示怎么按优先级分段拼出来、又在哪几个点锚定 Prompt Cache**。

---

## 一、这个模块在整套架构里的位置

`llm` 包是主循环（见 01）和子代理（见 05）的"模型出口"——`Agent.Run` 每轮都调 `client.Stream(...)` 拿事件。`prompt` 包是"系统提示生成器"，在 `llm.Client` 构造时把一坨 section 拼成 `systemPrompt` 字符串注入。

| 文件 | 职责 |
|------|------|
| `internal/llm/client.go` | `Client` 接口 + `NewClient` 协议路由 + `ResolveContextWindow`（第 2 层上下文窗口解析） |
| `internal/llm/model_resolver.go` | `NewModelResolver`：短名 → 真实模型 ID，给子代理切模型用 |
| `internal/llm/anthropic.go` | Anthropic 协议的流式实现（最重），含 Prompt Cache 锚点、thinking、用量回传、错误分类 |
| `internal/llm/openai.go` / `openai_compat.go` | OpenAI / 兼容协议的实现（结构同 anthropic，路由兜底） |
| `internal/prompt/builder.go` | `Builder` 按优先级拼 section；`BuildSystemPrompt` 注入指令/记忆/Skills |
| `internal/prompt/sections.go` | 各 section 的实际文案与优先级常量 |

---

## 二、推荐代码阅读顺序

1. **`internal/llm/client.go`** —— 先看 `Client` 接口（只有 `Stream` + `SetSystemPrompt` 两个方法，极简）和 `NewClient` 的协议 `switch`。这是"多模型"的门面。
2. **`internal/llm/model_resolver.go`** —— 看 `modelAliases`（haiku/sonnet/opus）和 `NewModelResolver` 返回的闭包。这是子代理 `selectClient` 切模型的底层。
3. **`internal/prompt/builder.go`** —— 看 `Section`/`Builder`/`Build`（按 `Priority` 排序）+ `BuildSystemPrompt`（指令/记忆/Skills 的优先级）。这是"系统提示怎么拼"。
4. **`internal/prompt/sections.go`** —— 看各 section 文案与优先级常量（Identity=0 … Memory=95）。理解"什么内容排最前、什么排最后"。
5. **`internal/llm/anthropic.go`** —— 重头戏：流式 `Stream`、Prompt Cache 三个锚点、thinking（adaptive vs enabled）、`FetchModelContextWindow`、`buildAnthropicMessages`（合并连续 user 消息、保留 thinking block）。
6. （选读）`internal/llm/openai.go` / `openai_compat.go` —— 看协议兜底的对称性。

---

## 三、逐文件讲解

### 3.1 `internal/llm/client.go` —— 统一 Client 接口 + 协议路由

- **`Client` 接口（17-20 行）**：只暴露
  - `Stream(ctx, conv, tools) (<-chan StreamEvent, <-chan error)`——流式输出，事件 channel + 错误 channel（双 channel 是 Go 流式 API 惯用法，错误不混进事件流）。
  - `SetSystemPrompt(prompt)`——运行时改系统提示（压缩后重设、或注入记忆后用）。
  - 额外 `MaxTokensSetter` 接口（22-24）可选：`SetMaxOutputTokens`。
- **`NewClient（26-37 行）**：按 `cfg.Protocol` 分发到三种实现：`anthropic` / `openai` / `openai-compat`，未知协议报错。**这就是"协议适配层"**——上层（`agent.Run`）只认 `Client` 接口，不关心底层是哪家。
- **`ResolveContextWindow（56-77 行）"第 2 层上下文窗口解析"**：
  - 若 `cfg.ContextWindow > 0`（用户显式配）→ 直接用，不拉。
  - 仅 `anthropic` 协议才拉（62 行）；其他协议跳过。
  - 构造 client → 断言成 `contextWindowFetcher` → `FetchModelContextWindow` 拉 `{base_url}/v1/models/{model}` 的 `max_input_tokens`，缓存到 `cfg.SetFetchedContextWindow`。
  - **全程 best-effort，永不返回 error、永不阻塞超 fetch 自身超时、绝不 panic**（注释 50-55）——拉不到就退回内置映射表/默认值。启动期安全调用。

> 考点：**为什么 `Client` 接口这么瘦（只有两个方法）？** 因为主循环只关心"给我流式事件"和"换系统提示"。把协议细节（SSE 解析、thinking、工具 schema 转换）封在各自实现里，上层零改动就能换模型/换厂。这是依赖倒置——上层依赖抽象 `Client`，不依赖 `anthropic.Client`。

### 3.2 `internal/llm/model_resolver.go` —— 短名 → 真实模型

- **`modelAliases（11-16 行）**：`haiku→claude-haiku-4-5-20251001`、`sonnet→claude-sonnet-4-6-20250514`、`opus→claude-opus-4-6-20250514`。把"人话短名"映射到带日期戳的真实模型 ID。
- **`NewModelResolver（18-29 行）**：返回一个 `func(shortName) (Client, error)` 闭包：
  - 短名命中别名就换成真实 ID，否则当真实 ID 直接用（21-23，所以也能直接传完整模型名）。
  - `cfg := baseCfg; cfg.Model = modelID`（25-26）——**复用 base 配置**（API key、base url、thinking 等），只换 model 字段。
  - 返回 `NewClient(&cfg, "")`——走 3.1 的协议路由。
- 这个闭包就是 `AgentTool.ModelResolver`（见 05 的 `selectClient`），子代理 `model: "opus"` 时调它拿到独立 `Client`。

> 考点：**为什么子代理切换模型是"新 Client"而不是共用父 Client？** 因为不同模型可能不同思考配置/预算，且子代理要独立 `SetSystemPrompt`/独立 `MaxTokens`。用 `baseCfg` 克隆只改 `Model` 既隔离又复用凭证，不重复读配置。

### 3.3 `internal/prompt/builder.go` —— 优先级拼装器

- **`Section（17-21）**：`Name`/`Priority`/`Content`。`Priority` 越小越靠前。
- **`Builder.Add` / `Build（50-68）**：`Build` 按 `Priority` 升序排序，去空，用 `\n\n` 拼接。排序保证"系统提示的结构稳定"——这对 **Prompt Cache 前缀稳定性**很关键（结构一变，缓存前缀就变，命中率掉）。
- **`DetectEnvironment（70-91）**：探测 `OS`/`Arch`/`Shell`/`IsGitRepo`/`GitBranch`/`Date`，用 `git rev-parse` 判断是否在仓库内、取当前分支。这些进 `EnvironmentSection`。
- **`BuildSystemPrompt（93-132）"系统提示总装"**：
  - 先 `Add` 一堆固定 section（Identity/System/DoingTasks/…/Environment，优先级 0~70）。
  - `CustomInstructions`（优先级 80）：TENCODER.md 加载来的用户指令，前缀 `# Project Instructions`。
  - `SkillSection`（优先级 90）：已加载 Skills 的提示。
  - `MemorySection`（优先级 95）：持久记忆，**放最后 = 最高模型注意力**（注释 122"放在最后以获得最高模型注意力"）。
  - 返回 `b.Build()`。

> 考点（简历"按优先级分段拼装"支撑）：优先级顺序就是"注意力顺序"——身份/系统规则在开头定调，环境信息在中段，**用户记忆（记忆系统，见 06）放最后 95 拿最高注意力**。且因 `Build` 稳定排序，拼出来的字符串逐字节可复现 → Anthropic 缓存前缀命中（见 3.5 锚点）。

### 3.4 `internal/prompt/sections.go` —— 各段文案与优先级

固定 section 的优先级链（看 `Priority` 字段）：
- `IdentitySection` = 0：你是 Tencoder 终端 AI 编程助手；安全约束（防注入/不猜 URL）。
- `SystemSection` = 10：工具按权限执行、接近上限自动压缩（"上下文实际无上限"）、`<system-reminder>` 含义、prompt 注入要先告知用户。
- `DoingTasksSection` = 20：任务执行准则——先读再改、优先编辑不新建、不过度抽象、默认不写注释、完成前先验证。
- `ExecutingActionsSection` = 30：高风险操作先确认清单（rm -rf / force-push / push / 删表等）。
- `UsingToolsSection` = 40：专用工具优先于 Bash（ReadFile 而非 cat）、独立工具并行调、Agent/Team 用法、延迟加载工具用 ToolSearch。
- `ToneStyleSection` = 50：默认不用 emoji、引用用 `file_path:line_number`、工具调用前用句号不用冒号。
- `OutputEfficiencySection` = 60：面向用户文本要精简、不播报内部权衡、结尾一两句总结、不产出中间文档。
- `EnvironmentSection` = 70：工作目录/平台/Shell/Git/模型/日期。

> 这些 section 直接对应 01 主循环里"模型应该遵守的行为准则"，也是权限/工具/压缩等模块在提示层的一致性来源。

### 3.5 `internal/llm/anthropic.go` —— 流式实现 + Prompt Cache 锚点（重点）

这是"多模型"里最重的一个实现，也是简历"锚定 Prompt Cache 前缀降低成本"的落地处。

- **`supportsAdaptiveThinking（26-38）**：模型名 `claude-opus-4-` / `claude-sonnet-4-` 且版本号 ≥6 走**自适应思考**（4-6+），否则（4-5）走**启用式思考**（带 budget）。这是一个"按模型能力分叉"的细节。
- **`newAnthropicClient（49-70）**：取 API key（缺失返回 `AuthenticationError`），建 SDK client（带 `BaseURL` 适配自建/代理端点），把 `thinking`/`systemPrompt`/`maxOutputTokens`/`contextWindow` 存进结构体。
- **`FetchModelContextWindow（89-108）**：同 3.1 的"第 2 层"。`defer recover()` 防 panic（注释 90-96：启动期必须静默降级），`WithMaxRetries(0)` 失败快速返回（防重试风暴），超时 3s（`anthropicModelFetchTimeout`）。
- **`Stream（110-316）"核心流式"**：
  - `buildAnthropicMessages(conv.GetMessages())`（114）把内部 `conversation.Message` 转成 Anthropic SDK 的 `MessageParam`（见 3.6）。
  - **工具 schema 转换（116-132）**：从 `toolSchemas`（即 02 里 `Registry` 吐的 `map[string]any`）抽 `name`/`description`/`input_schema`，拼 `ToolUnionParam`。
  - **Prompt Cache 三个锚点（139-175，重点）**：
    1. **System 块**（148-151）：在 system 文本上挂 `CacheControl: Ephemeral`——锚定"最稳定前缀"。
    2. **工具列表最后一项**（171-173）：工具 schema 跨轮稳定，缓存整个工具块"几乎免费"。
    3. **最后一条 user 消息的尾块**（`markLastUserTailForCache`，154 → 327-345）：锚定在最终 user 消息最后一块（text 或 tool_result）。
    - 注释 139-144 点明：Anthropic 在每个断点前缓存前缀，下次请求前缀**字节一致**才命中；`toolresult.ContentReplacementState`（见 03）正是保证 tool_result 内容跨断点字节稳定的机制。三者配合 = 系统提示 + 工具 + 历史 tool_result 都尽量命中缓存。
  - **thinking 配置（155-167）**：`supportsAdaptiveThinking` 决定 `OfAdaptive` 还是 `OfEnabled`（`BudgetTokens = maxTokens-1`）。
  - **SSE 读取在独立 goroutine（185-295）**：`readNext` 在子 goroutine 跑，`select` 同时监听 `ctx.Done()` / `idle.C`（5 分钟空闲超时 `anthropicStreamIdleTimeout`，200-219）/ `nextCh`。解决"底层连接静默断开时 `stream.Next()` 永久阻塞"的问题——用 idle timer 主动超时报错。
  - **事件翻译（241-292）**：把 SDK 的 `ContentBlockStart/Delta/Stop` 翻成本仓库的 `StreamEvent`（`ThinkingDelta`/`TextDelta`/`ToolCallStart`/`ToolCallDelta`/`ToolCallComplete`/`ThinkingComplete`）。thinking 块单独累积 `thinkingAccum` + `thinkingSignature`（同步进 conversation 的 `ThinkingBlocks`，Fork 字节重放依赖它）。
  - **用量回传（306-312）**：`UsageInfo{InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens}`——这正是 03 的 `UsageAnchor`/`ComputeUsedTokens` 的输入（Anthropic 的 input+cache_read+cache_creation+output 四项和）。
  - **`StreamEnd`（312）**：带 `StopReason`（空则 `"end_turn"`）——主循环据此判断是否要继续 ReAct（tool_use → 继续；end_turn → 结束）。
- **`classifyAnthropicError（410-436）**：把 SDK error 分类成 `ContextTooLongError`（413/太长的 prompt）/ `AuthenticationError` / `RateLimitError`（带 `Retry-After`）/ `LLMError` / `NetworkError`。上层可据类型决定重试/压缩/报错。

### 3.6 `buildAnthropicMessages（347-408）"消息转换"**

- assistant 消息：依次拼 `ThinkingBlocks`（带 signature）→ text → `ToolUses`（tool_use 块）。
- 仅含 `ToolResults` 的消息 → role=user，每块一个 `tool_result`（带 `tool_use_id` + `isError`）。
- **普通 user 文本合并（391-405）**：连续 user 文本消息合并进同一条 message 的 content（保持 user/assistant 交替，Anthropic 要求严格交替）。`canMerge` 判断前一条是 user 且非 tool_result。
- 空 assistant 块补一个空 text（367-369）——防止"只有 tool_use 的空 assistant"被拒。

> 考点：**为什么要把 thinking block 也序列化进消息？** 因为 Anthropic 要求 assistant 消息里的 thinking 与后续 tool_use **顺序一致**才能命中缓存（见 05 `buildForkedConversation` 字节级重放同样保留 thinking）。这是 Prompt Cache 正确性的硬约束。

---

## 四、面试高频 Q&A

**Q1：怎么同时支持 Anthropic / OpenAI / 兼容协议？**
定义瘦 `Client` 接口（`Stream` + `SetSystemPrompt`）。`NewClient` 按 `cfg.Protocol` switch 到三种实现；上层（`agent.Run`、子代理）只依赖接口，不关心底层厂。加新厂只需实现接口 + 在 switch 加分支。

**Q2：子代理的 sonnet/opus/haiku 怎么映射到真实模型？**
`model_resolver.go` 的 `modelAliases` 把短名映射到带日期戳的真实 ID（如 `opus→claude-opus-4-6-...`）。`NewModelResolver` 返回闭包，克隆 base 配置只改 `Model` 字段，复用 API key/base url，返回新 `Client`。`AgentTool.selectClient` 用它在 spawn 子代理时切模型。

**Q3：系统提示为什么"按优先级分段拼装"？**
`Builder` 按 `Priority` 排序拼接：身份/系统规则在开头（0~10）定调，用户指令 80、Skills 90、记忆 95（最后 = 最高注意力）。稳定排序还保证拼出的字符串逐字节可复现 → Anthropic 缓存前缀命中率高。

**Q4：Prompt Cache 在代码里锚了哪几个点？为什么？**
三处 ephemeral 缓存断点：① system 文本（最稳定前缀）；② 工具列表最后一项（工具 schema 跨轮不变）；③ 最后一条 user 消息的尾块（tool_result）。Anthropic 在每个断点前缓存前缀，下次前缀字节一致才命中。`toolresult.ContentReplacementState`（见 03）保证 tool_result 内容跨断点字节稳定，三者配合把"系统提示+工具+历史 tool_result"都尽量命中缓存，降成本。

**Q5：thinking（扩展思考）怎么处理？不同模型有区别吗？**
`supportsAdaptiveThinking` 按模型名版本分叉：opus-4-6/sonnet-4-6+ 走自适应思考（无预算上限），4-5 走启用式（带 `BudgetTokens=maxTokens-1`）。thinking 文本与 signature 单独累积进 `ThinkingBlocks`，且序列化进消息以保持与 tool_use 顺序一致（缓存命中硬约束）。

**Q6：流式读取为什么要在子 goroutine + 加 idle 超时？**
SDK 的 `stream.Next()` 在底层连接静默断开（无 FIN/RST）时会永久阻塞。这里把"读下一条"放子 goroutine，主循环 `select` 同时监听 `ctx.Done()` 和 5 分钟 idle timer，连接假死时主动超时报错，而非卡死整个 Agent。

---

## 五、自检题（合上代码自己答）

1. `Client` 接口只有哪两个方法？为什么这么瘦？（答：Stream + SetSystemPrompt；依赖倒置，上层只关心流式事件）
2. `model_resolver` 克隆 base 配置时改了哪个字段？（答：`cfg.Model`；其余复用凭证/base url/thinking）
3. 系统提示里谁的 `Priority` 最高、排最后？（答：MemorySection = 95，拿最高注意力）
4. 三个 Prompt Cache 锚点分别锚在哪？（答：system 文本、工具列表末项、最后 user 消息尾块）
5. 为什么 assistant 消息要保留 thinking block 且顺序与 tool_use 一致？（答：Anthropic 缓存前缀字节一致要求）
6. `FetchModelContextWindow` 为什么 `defer recover()` + `WithMaxRetries(0)`？（答：启动期静默降级，防 panic 防重试风暴）

---

## 六、风险提醒（面试诚实边界）

- "多模型"在仓库里是**真实实现**（三协议 + 别名解析 + 切模型），但 `openai.go` / `openai_compat.go` 我本次未逐行展开（只确认路由存在）。面试被追问 OpenAI 协议细节时，说明"结构对称于 anthropic.go，本次重点看了 anthropic 实现"。
- "降低 X% 成本"来自 Prompt Cache 命中，**具体百分比没有离线基准数据**，属设计预期，别说成实测。
- 头部仍是"来源：公众号@小林coding"模板头，如实说明是学习性复现/二次开发。
