# 08 - MCP 工具延迟加载（协议接入 / 工具包装 / 延迟排除 / 按需发现）

> 对应简历卖点：*「MCP 工具延迟加载：百级 MCP 工具默认不进初始工具集，模型需要时经 ToolSearch 按需加载，初始系统提示 token 占用降低约 85%」*
> 本模块讲清两件事：**① 怎么把任意 MCP 服务器接进来、把它的工具包装成本仓库的 `Tool`**；**② "延迟加载"到底是怎么实现的**——注册时全留着，但发往模型的初始 schema 把 deferred 工具剔掉，模型要用时再 `ToolSearch` 召回。

---

## 一、这个模块在整套架构里的位置

MCP（Model Context Protocol）是"接外部工具/数据源"的标准协议。本仓库的 `internal/mcp` 负责**接入与包装**；真正的"延迟"逻辑在 `internal/tools` 的 `Registry`（它认识 `DeferrableTool` 接口）和 `ToolSearch` 工具（按需召回）里。

| 文件 | 职责 |
|------|------|
| `internal/mcp/mcp.go` | `ServerConfig` / `Client`（stdio & http&sse 两种传输）/ `Manager`（连多服务器）/ `MCPToolWrapper`（MCP 工具 → `Tool` 接口，标 `ShouldDefer()=true`） |
| `internal/tools/tool.go` | `Registry.GetAllSchemas` 在组 schema 时**跳过未发现的 deferred 工具**；`SearchDeferred` / `FindDeferredByNames` 供 ToolSearch 用 |
| `internal/tools/tool_search.go` | `ToolSearch` 工具：模型按需查/加载 deferred 工具，并 `MarkDiscovered` |

> 一句话：**MCP 工具"全注册、半暴露"——注册进 Registry 但默认不进模型视野；模型自己发现需要了，调 ToolSearch 把它们拉进下一轮 schema。**

---

## 二、推荐代码阅读顺序

1. **`internal/mcp/mcp.go`** —— 先读 `ServerConfig`（stdio vs url 两种形态）、`Client.Connect`（两种传输怎么建）、`MCPToolWrapper`（关键：`ShouldDefer()=true`、名字 `mcp__server__tool`）。
2. **`internal/tools/tool.go`** —— 看 `DeferrableTool` 接口、`isDeferred`、`GetAllSchemas`（**跳过 deferred 未发现的工具** = 延迟加载的核心）、`SearchDeferred` / `FindDeferredByNames` / `MarkDiscovered`。
3. **`internal/tools/tool_search.go`** —— 看 `ToolSearch` 怎么被模型调用、`select:` 精准加载 vs 关键词搜索、`MarkDiscovered` 让工具进后续 schema。
4. （回看）`internal/agents/tool_filter.go`（`FilterToolsForAgent` Layer 1：MCP 工具 `mcp__*` 永远放行）和 `prompt/sections.go`（`UsingToolsSection` 里"延迟加载工具用 ToolSearch"的提示）——把整条链路串起来。

---

## 三、逐文件讲解

### 3.1 `internal/mcp/mcp.go` —— 接入与包装

**`ServerConfig（26-34）**：一个 MCP 服务器的配置——`Command`+`Args`（stdio 本地进程）、或 `URL`+`Transport`+`Headers`（远程）。`Env` 可注入环境变量。
- `IsStdio`（36-38）：有 `Command` 就是 stdio。
- `transportKind`（42-49）：`sse` → 旧版 SSE 传输；其他（空/`http`/`streamable`）→ 2025-03-26 的 Streamable HTTP。

**`headerRoundTripper` / `newHTTPClient（52-75）**：HTTP 传输时，给每个出站请求注入固定 header（`os.ExpandEnv` 支持 `$VAR` 展开）。有 header 才自建 client，否则用 `http.DefaultClient`。

**`Client.Connect（87-121）"建连"**：
- stdio 分支（93-103）：`exec.Command` 起子进程，`cmd.Env` 合并当前环境 + 配置 `Env`（96-98）。**关键细节 `cmd.Stderr = io.Discard`（102）**：注释解释——若不脱离父 tty 的 stderr，npx/node 子进程会检测到 TTY 发 OSC 颜色查询，终端把响应塞进主控 stdin 污染 TUI 输入。
- http 分支（104-110）：`transportKind()` 选 `SSEClientTransport` 或 `StreamableClientTransport`，挂带 header 的 `httpClient`。
- 最后 `sdkClient.Connect(ctx, transport, nil)` 建 `session`（115）。

**`ListTools` / `CallTool（123-150）`**：
- `ListTools` 调 `session.ListTools` 拿工具定义。
- `CallTool` 调 `session.CallTool`，把返回 content 里的 `*mcp.TextContent` 拼成文本；空则 `(no output)`；`result.IsError` 透传给 `ToolResult.IsError`。

**`Manager` / `ConnectAll（158-227）"连多服务器"**：
- 遍历 `configs`，每个 `NewClient` + `Connect`；连不上记 `errs` 跳过（196-201，单服务器失败不拖垮整体）。
- `InitializeResult().Instructions` 取服务器级指令（205-207）。
- `ListTools` 拿到每个工具定义，包成 `MCPToolWrapper` 加进 `registered`（218-224）。
- 返回 `ConnectResult{Mgr, Tools, Servers, Errors}`（226）——`Tools` 就是一堆 `MCPToolWrapper`，交给上层 `RegisterAllTools` 注册进 `Registry`。

**`MCPToolWrapper（246-281）"MCP 工具 → Tool 接口"**（本模块最关键的小类）：
- **`Name()`（252-254）**：`"mcp__" + SanitizeName(server) + "__" + SanitizeName(tool)`。双下划线分隔，**`mcp__` 前缀**正是 `tool_filter.go` Layer 1 "MCP 工具永远放行"的识别标志（见 05）。`SanitizeName`（256-258）把非字母数字替换成 `_`，保证名字合法。
- **`ShouldDefer()`（261）`return true`** —— 这就是"标记自己是延迟工具"。`Registry` 看到 `true` 就不把它塞进初始 schema。
- `Schema`（263-273）：直接把 MCP 的 `InputSchema` 透传成 `input_schema`。
- `Execute`（275-281）：转发 `client.CallTool(ctx, toolDef.Name, args)`——运行时真正调用 MCP 服务器。

> 考点：**为什么 MCP 工具要 `ShouldDefer()=true` 而不是"不注册"？** 因为"延迟"≠"不接"。服务器启动时已全部连上、工具全在 `Registry` 里（可搜索、可加载），只是**不进发给模型的初始 schema**。等模型需要时 `ToolSearch` 立刻能取——既省初始 token，又不丢能力。

### 3.2 `internal/tools/tool.go` —— "延迟"的真正执行点

- **`DeferrableTool` 接口（41-43）**：`ShouldDefer() bool`。任何 `Tool` 实现它并返回 true，就被当作可延迟工具。
- **`isDeferred（98-103）**：类型断言 `DeferrableTool`，取 `ShouldDefer()`。MCP 工具因此被识别为 deferred。
- **`GetAllSchemas（109-128）"延迟加载核心"**：
  ```go
  for _, t := range r.tools {
      if isDeferred(t) && !r.discoveredTools[t.Name()] {
          continue          // ← 延迟且未被发现 → 不进 schema
      }
      ...
  }
  ```
  **这行 `continue` 就是简历"初始工具集不包含所有 MCP 工具"的实现**：deferred 且 `discoveredTools` 里没有的工具，直接跳过，不发给模型。非 deferred 工具（ReadFile/Bash/Grep…）和已被 `MarkDiscovered` 的 deferred 工具正常进入。
  - 协议适配：OpenAI / openai-compat 时把 schema 重写成 `{"type":"function","name","description","parameters"}` 形状（116-122），Anthropic 直接用 `base`（123-125）。
- **`GetDeferredToolNames` / `GetDeferredTools` / `SearchDeferred` / `FindDeferredByNames（130-201）`**：
  - `SearchDeferred`（150-177）：按 `query` 在 deferred 工具的名字/描述里做**子串匹配**（大小写不敏感），返回命中 schema（同样做协议适配），上限 `maxResults`。
  - `FindDeferredByNames`（179-201）：按名字精确取（供 `ToolSearch` 的 `select:` 语法）。
  - 这两个是 `ToolSearch` 的底层数据源。
- **`MarkDiscovered` / `IsDiscovered（74-80）`**：`discoveredTools` map 记录"已被 ToolSearch 召回"的工具名。`GetAllSchemas` 据此决定要不要跳过——**召回后即永久进后续 schema**（不是一次性）。

### 3.3 `internal/tools/tool_search.go` —— 模型按需召回

- **`ToolSearchTool` 是常驻（非 deferred）工具**（20-30），模型一开始就能用它。
- **`Description`（22-30）**：明确告诉模型"有些工具是 deferred（默认不加载），用我来发现/加载"；两种 query 形式：`select:Name1,Name2`（精准）或关键词搜索。
- **`Execute（56-106）"召回流程"**：
  1. `query` 空报错。
  2. `max_results` 默认 5、上限 20（62-68）。
  3. `select:` 前缀 → `FindDeferredByNames`；否则 → `SearchDeferred`（72-80）。
  4. 无匹配时，列出所有可用 deferred 名提示（82-93）。
  5. **`MarkDiscovered`（95-100）**：对返回的每个 schema 调 `registry.MarkDiscovered(name)`——**这一步让它们在下一轮 `GetAllSchemas` 里不再被 `continue` 跳过**，即"加载进后续请求"。
  6. 返回"Found N tool(s). Their full schemas are now loaded…"（102-105），模型下一轮就能直接调这些工具。

> 考点（简历"降低约 85%"的支撑）：假设接了上百个 MCP 工具，**初始发给模型的 schema 只有内置工具（ReadFile/Bash/Grep/Glob/Edit/Write/ToolSearch…）**，MCP 工具全 deferred 不出现 → 初始 token 大幅下降。模型真正需要某个 MCP 能力时才 `ToolSearch` 召回那几个，按需进入。**"百级降 85%"是"初始只暴露内置、MCP 全延迟"的设计预期**，具体比例随接入工具数变化。

### 3.4 整条链路串起来（跨文件）

1. 启动：`mcp.Manager.ConnectAll` 连服务器 → 每工具包成 `MCPToolWrapper{ShouldDefer:true}` → `registry.Register`（全注册）。
2. 每轮组 schema：`registry.GetAllSchemas(protocol)` 跳过 deferred 未发现的 → **初始视野只有内置工具 + ToolSearch**。
3. 模型发现需要某 MCP 能力（系统提示 `UsingToolsSection` 已提示"用 ToolSearch"）→ 调 `ToolSearch(query)`。
4. `ToolSearch` 经 `SearchDeferred`/`FindDeferredByNames` 取 schema + `MarkDiscovered`。
5. 下一轮 `GetAllSchemas` 把这些工具放出 → 模型直接调 `mcp__server__tool`。
6. 调用时 `MCPToolWrapper.Execute` → `client.CallTool` 真打 MCP 服务器。
7. `FilterToolsForAgent` Layer 1（`mcp__` 前缀）保证 MCP 工具在子代理里也永远可用（见 05）。

---

## 四、面试高频 Q&A

**Q1：MCP 工具怎么接入并变成仓库里的 Tool？**
`mcp.Client.Connect` 按配置建连（stdio 起子进程 / http 或 sse 远程），`ListTools` 取工具定义，每个包成 `MCPToolWrapper`——它实现 `Tool` 接口，`Name()` 拼成 `mcp__<server>__<tool>`、`Execute` 转发 `client.CallTool`。`Manager.ConnectAll` 连多服务器并把全部 wrapper 注册进 `Registry`。

**Q2："延迟加载"到底延迟了什么？**
延迟的是"进模型初始工具集"。MCP 工具 `ShouldDefer()=true`，`Registry.GetAllSchemas` 在组 schema 时把"deferred 且未被发现"的工具 `continue` 掉，不发给模型。工具其实已连好、已在 Registry 里，只是不出现在初始视野。

**Q3：模型怎么把延迟工具"加载"回来？**
调常驻的 `ToolSearch` 工具：`select:Name` 精准取或关键词搜索，`SearchDeferred`/`FindDeferredByNames` 返回 schema 并 `MarkDiscovered(name)`。下一轮 `GetAllSchemas` 看到 discovered 就不跳过了，工具进入后续 schema 供直接调用。

**Q4：为什么延迟加载能降 token？**
初始 schema 只含内置工具（十来个），上百个 MCP 工具全 deferred 不出现 → 初始系统提示/工具定义 token 大幅下降。只有模型真需要的少数 MCP 工具才按需召回。这是"百级降约 85%"的设计来源。

**Q5：stdio 传输为什么要把子进程 stderr 丢到 io.Discard？**
不脱离父 tty 的 stderr 时，npx/node 子进程会检测到 TTY 发 OSC 颜色查询，终端把响应写回主控 stdin，污染 TUI 输入。丢 stderr 规避这个副作用。

**Q6：MCP 工具在子代理里能用吗？**
能。`FilterToolsForAgent` Layer 1（`tool_filter.go`）对 `mcp__` 前缀工具永远放行，不论 sync/async/teammate。

---

## 五、自检题（合上代码自己答）

1. `MCPToolWrapper.Name()` 的格式是什么？前缀有什么作用？（答：`mcp__<server>__<tool>`；`mcp__` 前缀被 `FilterToolsForAgent` Layer 1 识别为"永远放行"）
2. `Registry.GetAllSchemas` 里哪行实现"延迟"？（答：`if isDeferred(t) && !r.discoveredTools[t.Name()] { continue }`）
3. `ToolSearch` 的 `select:` 和关键词搜索分别走哪个底层方法？（答：`FindDeferredByNames` / `SearchDeferred`）
4. `MarkDiscovered` 之后，工具是只进一轮还是永久进后续 schema？（答：永久——`discoveredTools` 记录，后续 `GetAllSchemas` 不再跳过）
5. 为什么 MCP 工具是 `ShouldDefer()=true` 而非"不注册"？（答：延迟≠不接；需可搜索、可即时加载，只是不占初始 token）
6. `headerRoundTripper` 做什么？（答：HTTP 传输给每个出站请求注入固定 header，支持 `$VAR` 环境展开）

---

## 六、风险提醒（面试诚实边界）

- "百级工具降约 85%"是**设计预期/经验估算**，具体比例随接入的 MCP 服务器数量与工具定义长度变化，**仓库内没有离线基准脚本**佐证 85% 这个精确数字。
- 仓库接的是标准 MCP Go SDK（`github.com/modelcontextprotocol/go-sdk/mcp`），`MCPToolWrapper` 是本仓库的适配层；面试说"自研 MCP 协议"是错的对——应说"基于标准 MCP SDK 做了工具包装与延迟加载适配"。
- 头部仍是"来源：公众号@小林coding"模板头，如实说明是学习性复现/二次开发。
