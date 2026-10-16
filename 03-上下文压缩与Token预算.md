# 03 · 上下文压缩与 Token 预算（两层压缩 + usageAnchor + 冻结决策）

> 对应简历卖点 2「两层上下文压缩与 JSONL 跨会话记忆，保障长时编程任务的上下文连续性」，也是"百级工具降 85% Token"的工程基础之一。本资料给该模块的阅读顺序。

## 0. 模块定位

两层压缩，职责不同：

- **Layer 1（`toolresult`）**：单条 / 单消息工具结果超预算 → 内容落盘、原处替换为带预览的占位（`<persisted-output>`）。**关键是保护 Anthropic Prompt Cache 前缀字节稳定**。
- **Layer 2（`compact`）**：整体会话接近上下文窗口 → 调 LLM 生成摘要，旧前缀被摘要替换，近期尾部原样保留。

## 1. 建议代码阅读顺序

1. **`internal/toolresult/state.go`** —— `ContentReplacementState`（冻结决策的核心数据结构，24–36）。
2. **`internal/toolresult/budget.go`** —— `Apply`（53–214）冻结决策 + 两遍 spill；`buildSpillPreview`（222）、`writeSpill`（274）。
3. **`internal/toolresult/record.go`** —— `Record`（21）+ `AppendRecords` / `LoadRecords`（jsonl 持久化）。
4. **`internal/toolresult/reconstruct.go`** —— `Reconstruct`（18）从记录恢复 state，跨会话 / fork 复用。
5. **`internal/compact/compact.go`** —— `ManageContext` / `ForceCompact` / `autoCompact` / `UsageAnchor` / `ComputeUsedTokens`。
6. **`internal/compact/recovery.go`** —— `RecoveryState`（压缩后把刚读的文件、激活的 Skill 重注入摘要）。

## 2. Layer 1：工具结果预算（budget.go）

三个阈值常量（21–37）：`SingleResultLimit=50000`（单条超此落盘）、`MessageAggregateLimit=200000`（单消息聚合超此也 spill）、`SpillSubdir=.tencoder/tool_results`。

`Apply`（53–214）注释明确是 **Design A：就地修改 conv**（不再克隆 Manager），state 记录决策、逐字节回放保证前缀稳定（47–49）。核心流程：

- 对每个 tool_result：已在 `Replacements` → **回放精确预览**（78–82）；已 `Seen` 但未替换 → **冻结为原始内容**（83–87）；已是替换标记 → 冻结（88–100）。
- **Pass 1**（104–139）：单条 `> SingleResultLimit` → `writeSpill` 落盘 → `buildSpillPreview` 生成占位（含前 2KB 预览），记 decisions + Record。
- **Pass 2**（141–190）：单消息聚合 `> MessageAggregateLimit` → 按内容长度**降序**逐个 spill，直到 total ≤ 限制。
- **冻结**（192–199）：剩余 fresh 标记 Seen 但不替换。
- **就地替换**（201–211）：`conv.ReplaceToolResults`。

`buildSpillPreview`（222–239）注释重点：**格式变更会导致 Prompt Cache 失效，不要轻易改**。
防循环（110–118、168–172）：`isSpillReadback` 检测"读回 spill 目录里的文件"→ 不重复 spill，否则会无限套娃（读回结果约同样大小又触发 spill）。
`writeSpill`（274–294）：`os.OpenFile(..., O_EXCL, ...)` 原子写，避免并发覆盖（呼应 05 资料里的 O_EXCL 风格）。

## 3. 冻结决策为何保护 Prompt Cache（工程精华）

Anthropic Prompt Cache 按"前缀字节完全相同"命中。若每轮都重新格式化预览（哪怕内容一致、格式略变），前缀就变了 → cache 失效 → 重新计费。

解法：`state.Replacements[id]` 在第一次决定替换后存下**预览字符串**，后续轮次**逐字节回放同一字符串**（78–82），前缀稳定 → cache 命中。`SeenIDs` 保证"见过的 id 决策永久冻结"（state.go 16–22 注释）。这就是简历"降 Token"的隐藏功臣之一。

## 4. Layer 2：自动压缩（compact.go）

`ManageContext`（260–311）算 used tokens（`ComputeUsedTokens`），对比阈值（`computeCompactThreshold` 76–87，effectiveWindow = contextWindow − min(maxOutput, 20000)，阈值 = effective − margin）：

- used `< effectiveWindow − autoMargin(13000)` → 不压（282）。
- used `≥ effectiveWindow − manualMargin(3000)` → `ForceCompact` 强制压（289）。
- 中间 → `autoCompact`（300），带熔断：连续失败 3 次（`MaxConsecutiveAutoCompactFailures`）停止重试（296）。

**`usageAnchor`（194–234）真实用量锚点**——简历"降 Token"的关键技巧：

- `UsageAnchor{BaselineTokens, AnchorCount, HasUsage}`（203–207）。
- `ComputeUsedTokens`（223–234）：**有 anchor → baseline + 只增量估算 anchor 之后的消息**；无 anchor（首轮）→ 全量字符估算。
- `BaselineFromUsage`（213–215）：真实 prompt = `input + cache_read + cache_creation + output`（Anthropic 把 cache 单独报，四者之和才是真实占用）。
- 好处：不用每轮对全对话重算 token，用"真实基线 + 增量"更准也更省。

## 5. autoCompact 保留近期尾部（compact.go:492–565）

- `computeKeepStartIndex`（362–400）：从尾部回溯累计 token，保留 `keepRecentTokens(10000)` / `minKeepMessages(5)`，但不超过 `keepMaxTokens(40000)`；**snapping 不拆 tool_use↔tool_result 对**（390–397），否则 API 报 orphaned tool_result。
- `autoCompact`：旧 prefix 进 LLM 摘要，近期 tail 原样保留（520–521）；摘要 prompt 要求 `<analysis>` + `<summary>` 两段（105–172），`formatCompactSummary`（616）只取 `<summary>`。
- 跨会话 boundary（539–545）：`session.SaveCompactBoundary` 存 compact_boundary，resume 可重建压缩态。
- `*conv = *compacted`（562）：整体替换会话。

## 6. RecoveryState：压缩后恢复（recovery.go）

压缩会抹掉工作会话，`RecoveryState` 并发安全（`sync.Mutex`，50–54）地快照：

- `FileReadRecord`：刚读的文件内容（主循环 `agent.go:635–641` ReadFile 时 `RecordFileRead`）。
- `SkillInvocationRecord`：激活的 Skill SOP。

`BuildRecoveryAttachment`（124–188）把这些快照 + 当前工具清单拼进摘要消息，并附 Note"别从摘要瞎猜，重新读源"。→ 压缩后模型仍"记得"刚看的文件。

## 7. 跨会话 JSONL（record.go + reconstruct.go）

- `Record`（21–25）：`kind / tool_use_id / replacement`，jsonl 一行一决策。
- `AppendRecords`（32–55）追加写 `replacement_records.jsonl`；`LoadRecords`（59–87）读回（缺文件 = 首次运行）。
- `Reconstruct`（18–53）：从 messages + records 重建 state——所有可见 id 先 `Seen`（冻结），再叠加 `Replacements`；支持 fork 继承父 state（`inheritedReplacements`，43–51）。

## 8. 面试要点

- **Q：两层压缩分别解决什么？** A：Layer1 单条/单消息工具结果过大；Layer2 整体接近窗口。
- **Q：冻结决策为什么重要？** A：保护 Anthropic Prompt Cache 前缀稳定，避免重复计费。
- **Q：usageAnchor 怎么用？** A：真实用量钉基线 + 只增量估算新增消息。
- **Q：压缩后模型会不会丢刚读的文件？** A：RecoveryState 快照重注入。
- **Q：为何 keep recent tail 不全摘要？** A：对齐 Claude Code，保留近期准确性；且不能拆 tool_use/result 对（否则 API 拒绝）。

## 9. 自检题（先想再看）

1. `SingleResultLimit` / `MessageAggregateLimit` 各多少？（budget.go:28 / 33）
2. 为什么 spill 预览要"逐字节回放"？（state.go 注释 + budget.go:78）
3. usageAnchor 没有真实用量时怎么算 used tokens？（compact.go:224–225）
4. `computeKeepStartIndex` 为何不能把边界落在 tool_result 上？（compact.go:390–397）
5. `Reconstruct` 如何保证 resume 后决策一致？（record.go + reconstruct.go）

<details>
答案：1. 50000 / 200000。2. 前缀字节变了 Prompt Cache 失效，回放同一字符串保命中。3. 退回全量字符估算 `EstimateTokens(messages)`。4. 会留下 orphaned tool_result，API 拒绝对话。5. 从 jsonl 记录重建 Replacements，已见 id 冻结，逐字节回放。
</details>

## 10. ⚠️ 风险提醒

简历里"关键信息保留率 17% → 100%"是摘要策略（SWEbenchLive）的验证口径，但**评测流水线代码不在此仓库**（见总资料第 9 章）。面试前请单独准备该数字怎么测得、失败案例是什么，否则建议改为"自建自动化评测流水线，迭代摘要策略把关键保留率从 17% 提到 100%"。
