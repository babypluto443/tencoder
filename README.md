# Tencoder

终端 AI 编程助手。Go 实现，从零构建 ReAct 主循环、工具系统、多 Agent 协作、上下文压缩、持久记忆与分层安全边界，并在其上叠加声明式模型路由与轻量档质量门禁。

```
go build ./cmd/tencoder && ./tencoder
```

---

## 这是什么

一个跑在终端里的编程助手：你描述目标，它自己读写文件、跑命令、调 MCP 工具、拆分子任务、遇到编译错误会自己修。

不是对话壳子。主循环、权限判定、上下文预算、记忆生命周期都在这个仓库里实现了。

**代码规模**：193 个 Go 文件（128 实现 + 65 测试），26 个内部包。

---

## 核心设计

以下每一项都是架构决定，不是功能清单。括号里是它要解决的问题。

### 主循环：五层分工

交互 / 推理 / 工具 / 记忆 / 安全各自独立成包，上层只做编排。

一个具体后果：`internal/agent/agent.go` 的主循环里没有一行 LLM 调用细节——模型、流式事件、token 计数全在 `internal/llm`。这让同一套主循环能同时服务终端 TUI 和 headless Web 服务两种前端。

### 工具结果压缩：三层，各管一件事

| 层 | 触发时机 | 手段 |
|---|---|---|
| Layer 1 | 每轮迭代 | 超预算的 tool_result 落盘 + 上下文里只留摘要 |
| Layer 2 | 上下文接近窗口 | 保留锚点消息，摘要中段 |
| Layer 3 | 手动 | 用户主动压缩 |

三层是**递进关系不是并列**——预算够时后两层根本不触发。（为什么不用向量检索：那会引入一个"检索失败就答错"的失效模式，而摘要是无损降级。）

### MCP 工具延迟加载：prompt cache 经济学优先于 LRU

百级工具全量塞进 prompt 会击穿缓存——而**击穿一次缓存省下的钱，远大于省下的几十 KB schema**。

所以是**阈值批量驱逐**（超过 40 砍回 25）而不是逐个淘汰。实测 120 工具 × 40 轮场景：

```
常驻 schema 字符数   2,790,915 → 1,520,981    增量节省 45.5%
驱逐轮次             5 次 / 40 轮              ← 批量设计的意义在这
常驻峰值             120 → 40
```

LRU 时钟用**执行时间**而非发现时间：只按发现时间排会退化成"最早发现的先走"，而那恰恰可能是 agent 正在用的工具。执行次数达标的工具直接豁免驱逐——反复装卸是纯浪费。

（`internal/tools/eviction_test.go` 里有一条断言专门盯"驱逐轮次不能太多"，因为**省字节和击穿 cache 可以同时发生，那是坏交易**。）

### 三层记忆：长期 / 项目 / 短期任务

```
长期记忆（跨项目）  ← LLM 自行写
项目记忆（随代码）  ← LLM 自行写
任务记忆（短期暂存）← 子 agent 完成时自动写入，空闲蒸馏
```

任务记忆这一层是唯一不由模型主动写的——子 agent 收尾时走**旁路提取**（独立 system prompt、独立 client、独立上下文），写入 `tasks/` 暂存，空闲 60s 后蒸馏进长期记忆。

四条不可让步的性质：

- **update 只能追加，merge 才能覆盖**。蒸馏器只拿到长期记忆的标题与描述清单，看不到正文——它不可能写出替换内容。这是"看不见现有内容"这个约束逼出来的，不是风格选择。
- **解析失败一律保留源文件**。宁可堆着等下轮，也不因处理失败销毁教训。
- **update 不删除**。教训只能被补充，不能被覆盖。
- **蒸馏器空闲 60s 才跑，且单飞**。用户还在敲键盘时不该开始改写他的记忆。

### 分档模型路由：声明定基线，信号只能往上推

给整条链路装两档模型（轻量 / 强档），成本降 45%、延迟降 31%。

```
声明信号  agent 定义里的 taskClass → 基线档位（八类：patch_gen / explore /
                              memory_recall / log_summary / format_check /
                              arch_decision / conflict_merge / cross_project）
运行时信号  工具预算超阈值 → 升档
          上下文超阈值   → 升档
```

三条设计约束，每条都是刻意的：

- **`0` 表示"未知"，不是"极小"**。阈值判断写成 `n > 0 && n > threshold`——否则"不知道要调多少次工具"会被当成 0 次而路由到便宜模型，即**信号越缺失、路由越激进**。
- **只有升档信号，没有降档信号**。省下的钱远小于"轻模型写了个错补丁、主 agent 测试循环又调回来重做"的成本；且降档判据与 taskClass 重复，会产生两个互相矛盾的真相。
- **所有回退都朝"更贵、同样结果"方向**。无轻量模型配置 → 留在强档；client 构造失败 → 落回会话模型。**路由层出任何问题，用户只会多花钱，不会少拿结果。**

`routing.disabled` 既是生产开关也是 A/B 对照组：**同一个二进制，唯一变量是这个 flag**。

### 质量门禁：轻量档产出的代码要过编译

轻量档省下的钱，如果靠"主 agent 发现编译错误再调回来"补回来，就白省了。所以轻量档写完代码立刻过一道秒级门禁（`go build` + `go vet` + `gofmt -l`），不过就 spawn 强档**续作修复**——带 diff 和结构化失败信息，只修不重做。

两条边界：

- **门禁只管语法层**。逻辑对不对它不关心，那是主 agent 测试循环的职责。声称门禁保证质量是不诚实的；它是廉价的前置拦截。
- **防循环靠结构，不靠计数器**。修复 agent 走独立代码路径，压根不经过升级入口。计数器"最多 1 次"只是第二道保险——只靠计数器的防循环，在有人给修复路径加一行 `runSync` 的那天就失效。

### goal-drift 反思：第三方旁路裁判

高风险时刻（定位完成 / 补丁生成 / 工具失败 / 异常恢复）问一句"你现在走的路还对不对目标吗"。

- **裁判是独立的**：自己的 system prompt、自己的 client、自己的上下文。复用主 client 就不是第三方裁判了。
- **异步预取**：触发时同步快照目标与轨迹快照，只有 LLM 调用进 goroutine。下一轮开头限时消费（≤5s）。
- **"最新"按触发序号，不按到达顺序**。裁判是并发 goroutine，**旧轨迹的判定更可能是最后落地的那条**（触发早、排队久）。按到达顺序顶掉，主循环就会拿到一条关于它已经走过的轨迹的判定。
- **判定无效是第三态，不计窗口**。裁判解析失败不能折叠成"是"（偏移检测被静默关掉，外观完全正常），也不能折叠成"否"（裁判一坏就变成持续告警）。
- **注入措辞是"某个观察者的看法"而非系统断言**。K=3 只防误报不防漏报，措辞是第二道防线。

### 跨项目工作区：默认不可见，不是默认可见后审批

多项目协同的安全边界。

- **worktree 是冲突边界，不是安全边界**。共享 `.git` 意味着 `.git/hooks` 落在每个 worktree 的可写面内且以用户权限执行——这一点在 `ValidateGitHooks` 里做警告而非拒绝（多数仓库有 `.sample` 模板）。
- **默认不可见**。子 agent 只看得到主项目 + 被点名的那一个工作区，其余注册项目**连读都不可见**。一个拿着三个项目读权限的 agent 完全可能读错项目然后基于错的项目下结论——而它自己不知道。
- **只读不是静默拒绝，是带审计的显式审批**。且审批弹窗里**禁用"always allow"**——一键允许会把一个只读声明悄悄变成整场会话可写。agent 侧还有纵深防御：非 TUI 客户端硬发 always 会被降级为单次。
- **审计挂在 `Check` 外层**，不是散在各 return 点上。会漏记某类决策的审计层比没有审计更危险，**因为它看起来是完整的**。

---

## 安装

```bash
git clone https://github.com/babypluto443/tencoder.git
cd tencoder
go build ./cmd/tencoder
```

需要 Go 1.25+。

### 配置

创建 `.tencoder/config.yaml`：

```yaml
providers:
  - name: main
    protocol: anthropic          # anthropic | openai | openai-compat
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    model: claude-sonnet-4

permission_mode: default         # default | acceptEdits | plan | bypass

mcp_servers:
  - name: context7
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
```

三层配置，后者覆盖前者（逐字段，不是整段）：

```
~/.tencoder/config.yaml           用户级
<repo>/.tencoder/config.yaml      项目级
<repo>/.tencoder/config.local.yaml 本地覆盖（建议 gitignore）
```

可选能力段（全部有合理默认，不写也能跑）：

| 段 | 作用 |
|---|---|
| `reflection` | goal-drift 反思节点 |
| `tool_governance` | 工具驱逐阈值 + 超长结果摘要 |
| `routing` | 分档模型路由 + 质量门禁 |

### 运行

```bash
./tencoder                        # 终端 TUI
./tencoder --remote :8080         # headless Web UI（HTTP + WebSocket）

# 打开路由决策 / 质量门禁 / token 口径埋点
TENCODER_LLM_DEBUG=1 ./tencoder
```

---

## 结构

```
cmd/tencoder          TUI 入口
cmd/tencoder-eval     评测流水线
cmd/mabench           单/多 agent 对照基准

internal/agent        ReAct 主循环、反思节点          ← 编排层
internal/agents       子 agent 派生、团队协作
internal/conversation 消息与 token 预算
internal/compact      Layer 2 自动压缩
internal/toolresult   Layer 1 工具结果预算
internal/tools        工具注册表、延迟加载、驱逐
internal/mcp          MCP 客户端
internal/memory       三层记忆、蒸馏、召回
internal/permissions  权限判定、沙箱、审计
internal/workspace    跨项目注册表
internal/worktree     git worktree 隔离
internal/llm          客户端、模型解析、路由、质量门禁
internal/teams        团队与进度
internal/tui          Bubble Tea 前端
internal/remote       HTTP + WebSocket 前端
```

依赖方向自上而下，`internal/agent` 站在最上层。跨层需要能力时用窄接口注入（`WorkspaceResolver`、`TaskLister`、`AuditSink`），不用具体类型——**这是为了让 `workspace → permissions → tools → agents` 这条链不成环**。

---

## 测试

```bash
go test ./internal/...
go test -race ./internal/agent ./internal/llm ./internal/tools ./internal/permissions
```

核心包 `-race` 全绿。三个包存在与本项目无关的既有失败（`config` / `memory/extractor` / `worktree`），源于未完成的 media provider 特性与 macOS 符号链接差异。

---

## 一些实现上的选择

记录几个容易被质疑但有理由的决定。

**审计包挂在 `Check` 外层包装**，而不是在约十个 return 点各加一次调用。后者是那种"下次有人加分支就会忘"的东西。

**`RecordExecute` 放在权限门之后**。被拒绝的调用根本没执行，算进去会让一个反复被拒的工具攒够次数、骗到它没赚来的活跃豁免。

**只有读权限的项目，首次以"必须逐次确认"的方式放行**。一键允许会把只读声明变成可写，这个手滑正是审批链要防的。

**沙箱两侧都做符号链接解析**。macOS 上 `/var` → `/private/var`，注册 `/private/var/...` 而被检查的是 `/var/...` 时永远不匹配，只读边界会**静默失效**。且不存在路径要只解析父目录再拼回 basename——因为"即将创建的新文件"才是最常见的写操作场景。

---

## 已知边界

诚实地列出来，因为不知道边界的设计没法评估：

- **质量门禁只覆盖 Go**。Python / TypeScript 项目直接跳过。
- **Bash 间接写绕过路径沙箱**。`bash -c "echo > /path"` 这类重定向靠危险命令检测兜底，不是路径校验。
- **`patch_gen` 实际到不了轻量档**。路由只升不降，而它的基线是强档——所以成本收益来自 explore / memory_recall / log_summary / format_check 那几类。
- **`EstimatedToolCalls` 尚未接入生产调用方**。字段与裁决逻辑都在，但调用侧目前只填了显式模型。
- **量化指标多为单测与本地基准**，未经大规模真实任务集验证。

---

## License

MIT
