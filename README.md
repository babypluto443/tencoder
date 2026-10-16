<div align="center">

# Tencoder

**终端里的 AI 编程助手**

读写文件 · 执行命令 · 拆解子任务 · 自主修复编译错误

[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey)]()
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](#license)

</div>

---

Tencoder 是一个跑在终端里的编程助手：你描述目标，它自己规划步骤、调用工具、派生子 agent 并行工作，遇到编译错误会自己修。支持 Anthropic / OpenAI 及任意 OpenAI 兼容 API，支持 MCP 工具扩展。

```text
你: 给 user service 加一个分页接口，写完跑一遍测试

Tencoder: 定位到 internal/user/…
  ├─ 子 agent 1 (worktree 隔离) → 写 handler / service / repository
  ├─ 子 agent 2                → 补单测
  └─ 质量门禁 go build + vet + gofmt 通过
     回归测试 32/32 通过
```

## 功能

- **ReAct 主循环** — 规划、执行、观察、修正，单任务最多 200 轮工具调用
- **子 agent 派生** — 同步 / 后台 / 团队协作三种模式，支持 git worktree 隔离，各自可指定模型与权限
- **三层上下文压缩** — 工具结果预算 → 自动压缩 → 手动压缩，长会话不爆窗口
- **三层记忆** — 跨项目长期记忆 / 项目记忆 / 任务记忆（子 agent 自动写入，空闲自动蒸馏）
- **MCP 工具延迟加载** — 百级工具场景下常驻 schema 增量节省 45.5%，驱逐后可随时重新发现
- **分档模型路由** — 廉价任务走轻量模型（如 Haiku），复杂任务自动升档；轻量档代码产出过编译门禁，不过则强档续作修复
- **目标偏移检测** — 独立旁路裁判在四个高风险时刻检查执行轨迹是否偏离目标
- **权限与沙箱** — 四种权限模式、路径沙箱、只读项目审批、OS 级沙箱、JSONL 审计日志
- **跨项目工作区** — 一个会话内协作多个仓库，子 agent 视野默认收窄

## 快速开始

```bash
git clone https://github.com/babypluto443/tencoder.git
cd tencoder
go build ./cmd/tencoder
```

需要 Go 1.25+。

### 1. 配置

创建 `.tencoder/config.yaml`：

```yaml
providers:
  - name: main
    protocol: anthropic        # anthropic | openai | openai-compat
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    model: claude-sonnet-4

permission_mode: default
```

`api_key` 支持环境变量展开，建议不要明文写入。

### 2. 运行

```bash
./tencoder                      # 终端 TUI
./tencoder --remote :8080       # headless Web UI（HTTP + WebSocket）
TENCODER_LLM_DEBUG=1 ./tencoder # 带路由 / 门禁 / token 埋点日志
```

`--remote` 不带参数时默认监听 `:18888`。

### 3. 开始对话

直接输入任务即可。斜杠命令：

| 命令 | 说明 |
|---|---|
| `/plan` | 切换到计划模式（只读，先出方案再动手） |
| `/review` | 审查当前代码改动 |
| `/rewind` | 回退对话与文件到之前的检查点 |
| `/resume` | 恢复历史会话 |
| `/session` | 会话管理 |
| `/compact` | 手动压缩上下文 |
| `/memory` | 管理自动记忆 |
| `/skills` | 查看可用 skills（`reload` 热加载） |
| `/mcp` | MCP 服务器状态 |
| `/permission` | 权限管理 |
| `/sandbox` | 配置 OS 级沙箱 |
| `/workspace` | 管理跨项目工作区 |
| `/status` · `/clear` · `/help` | 状态 / 清空 / 帮助 |

## 配置

配置分三层，后者逐字段覆盖前者：

```
~/.tencoder/config.yaml             用户级
<repo>/.tencoder/config.yaml        项目级
<repo>/.tencoder/config.local.yaml  本地覆盖（建议 gitignore）
```

### 权限模式

| 模式 | 读 | 写 | 命令 |
|---|---|---|---|
| `default` | 允许 | 询问 | 询问 |
| `acceptEdits` | 允许 | 允许 | 询问 |
| `plan` | 只读 | — | — |
| `bypassPermissions` | 允许 | 允许 | 允许 |

### 扩展能力

**子 agent 定义** — 放在 `.tencoder/agents/*.md`（项目）或 `~/.tencoder/agents/*.md`（用户级），YAML frontmatter 声明工具范围、模型、任务类别：

```markdown
---
name: refactor-helper
description: 代码重构辅助，定位引用并安全改名
tools: [ReadFile, Grep, EditFile]
model: inherit
taskClass: patch_gen
---
你负责安全地执行重构：先定位全部引用，再逐一修改，最后验证编译。
```

**Skills** — 放在 `.tencoder/skills/`，主 agent 按需加载。

**MCP 服务器** — 支持 stdio 与 Streamable HTTP：

```yaml
mcp_servers:
  - name: context7
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
  - name: remote-mcp
    url: https://example.com/mcp
    transport: http
```

### 可选配置段

全部有默认值，不配置也能正常使用。

<details>
<summary><b>routing</b> — 分档模型路由与质量门禁</summary>

```yaml
routing:
  disabled: false              # true = 全部走强档（A/B 对照组开关）
  tool_call_budget: 20         # 预计工具调用超过此数 → 升强档
  context_tokens_threshold: 60000
  light_model: haiku           # 轻量档模型；留空 = 沿用会话模型
  strong_model: ""
  gate_enabled: false          # 轻量档代码产出过编译门禁
  gate_timeout_ms: 30000
```

路由三原则：任务类别声明决定基线档位；运行时信号只升档不降档；`0` 表示未知而非极小，不会触发任何升档判断。路由自身故障时一律回退到会话模型——多花钱，不少拿结果。

</details>

<details>
<summary><b>tool_governance</b> — 工具驱逐与结果摘要</summary>

```yaml
tool_governance:
  evict_enabled: true
  evict_high_watermark: 40     # 常驻 schema 超过此数 → 批量驱逐回低水位
  evict_low_watermark: 25
  active_exempt_executions: 2  # 执行次数达标的工具免于驱逐
  summary_enabled: false       # 超长工具结果旁路 LLM 摘要（默认关闭）
  summary_timeout_ms: 20000
```

驱逐是批量而非逐个：工具 schema 位于 prompt 前缀，逐个淘汰会反复击穿 prompt cache。

</details>

<details>
<summary><b>reflection</b> — 目标偏移检测</summary>

```yaml
reflection:
  enabled: true
  periodic: false              # 周期性兜底检查（默认关闭，仅事件触发）
  max_wait_ms: 5000            # 主循环等待判定结果的上限
  drift_threshold: 3           # 连续 N 次判定偏离 → 提示重规划
```

</details>

## 架构

```
cmd/
  tencoder        TUI 入口        internal/tui
  tencoder-eval   评测流水线       internal/eval
  mabench         单/多 agent 基准

internal/
  agent           ReAct 主循环、目标偏移反思
  agents          子 agent 派生、团队、质量门禁接线
  conversation    消息与 token 预算
  compact         Layer 2 自动压缩
  toolresult      Layer 1 工具结果预算与摘要
  tools           工具注册表、延迟加载、驱逐
  mcp             MCP 客户端
  memory          三层记忆、蒸馏、召回
  permissions     权限判定、路径沙箱、审计
  workspace       跨项目注册表
  worktree        git worktree 隔离
  llm             客户端、模型解析、分档路由、质量门禁
  teams           团队与进度
  tui / remote    两种前端
```

依赖方向自上而下。跨层能力通过窄接口注入（`WorkspaceResolver`、`AuditSink` 等），避免 `workspace → permissions → tools → agents` 成环。

## 开发

```bash
go build ./...      # 构建
go test ./...       # 全量测试
go test -race ./internal/...   # 竞态检测
```

核心包 `-race` 全绿。`config`、`memory/extractor`、`worktree` 三个包存在与本仓库无关的既有失败。

## 设计取舍

几条有意为之、容易被质疑的决定：

- **工具驱逐按 prompt cache 经济学设计**，不按 LRU。逐个驱逐省字节但击穿缓存，是坏交易。
- **审计挂在权限判定外层包装**，不在十几个 return 点各记一次。漏记的审计比没有审计更危险。
- **路由失败的回退方向只有一个**：更贵、同样结果。路由层永远不能把故障变成"任务不执行"。
- **只读工作区的写入必须逐次确认**，且审批界面移除"不再询问"选项——一键允许会把只读声明变成整场会话可写。

## 已知边界

- 质量门禁只覆盖 Go 项目，其他语言直接跳过
- `bash -c "echo > /path"` 类间接写不经过路径沙箱，由危险命令检测兜底
- `patch_gen` 类任务基线即强档，路由只升不降，因此实际轻量档占比来自 explore / memory_recall 等类
- 量化指标来自单测与本地基准，未经大规模真实任务集验证

## License

[MIT](LICENSE)
