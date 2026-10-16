// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package toolresult

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tencoder/internal/conversation"
)

// Budget thresholds. Values match the ch08 spec; ContentReplacementState
// freezes decisions, but the actual numerical limits stay the same as the
// pre-state implementation so behavior on a fresh conversation is identical.
const (
	// SingleResultLimit gates the per-tool spill. Aligned with Claude Code's
	// DEFAULT_MAX_RESULT_SIZE_CHARS = 50_000. Must stay strictly above
	// tools.MaxOutputChars (10000) + truncation suffix; otherwise a tool
	// result truncated to MaxOutputChars trips the spill on every iteration
	// — and reading the spilled file back is itself a ~10K result that
	// would re-spill, looping.
	SingleResultLimit = 50000

	// MessageAggregateLimit triggers Pass 2 when a single user message's
	// tool_result content sums above this. Aligned with Claude Code's
	// MAX_TOOL_RESULTS_PER_MESSAGE_CHARS = 200_000.
	MessageAggregateLimit = 200000

	// SpillSubdir lives under the agent's workdir.
	SpillSubdir = ".tencoder/tool_results"
)

const (
	// persistedTagPrefix must exactly match the first token buildSpillPreview writes.
	//
	// It previously read "[Result of " while the writer emitted "<persisted-output>", so
	// isAlreadyReplaced never matched a mechanical preview — a spilled result restored from a
	// session log was therefore not recognised as already-decided. SeenIDs masked the damage in
	// the common path, which is why it went unnoticed; the mismatch only surfaced once 阶段三 added
	// a second replacement format and a test asserted on both.
	persistedTagPrefix = "<persisted-output>"
)

func isAlreadyReplaced(s string) bool {
	return strings.HasPrefix(s, persistedTagPrefix) || isSummarizedReplacement(s)
}

// Apply 是 Design A 实现：直接就地修改 conv 中超出预算的 tool_result 内容，
// 不再生成新的 Manager 副本。state 的 SeenIDs/Replacements 记录本轮决策，
// 后续调用逐字节回放相同预览字符串，保证 Prompt Cache 前缀稳定。
//
// 返回本轮新增的替换记录（调用方追加到会话日志）和错误（仅文件系统
// 灾难性错误；单条 spill 失败会静默冻结为原始内容，不中断整轮）。
//
// 阶段三 3.2 保留了原签名作为薄封装：summarizer / taskGoal 为 nil / "" 时行为与阶段二之前完全一致。
func Apply(
	conv *conversation.Manager,
	workDir string,
	state *ContentReplacementState,
) ([]Record, error) {
	return ApplyWithSummary(conv, workDir, state, nil, "")
}

// ApplyWithSummary is Apply with the 阶段三 summary path enabled.
//
// 摘要是「preview 位的升级」而不是新增一层：spill 落盘路径、冻结机制、Record 语义全部不变，
// 只有决定留在上下文里的那段文本从「机械截断」换成了「结构化摘要」。
func ApplyWithSummary(
	conv *conversation.Manager,
	workDir string,
	state *ContentReplacementState,
	summarizer *Summarizer,
	taskGoal string,
) ([]Record, error) {
	messages := conv.GetMessages()
	if len(messages) == 0 {
		return nil, nil
	}

	spillDir := filepath.Join(workDir, SpillSubdir)
	absSpillDir, _ := filepath.Abs(spillDir)
	toolUseByID := buildToolUseIndex(messages)

	// makeReplacement is the single place a preview is produced: try the LLM summary, fall back
	// to the mechanical preview. Both paths yield a string that is then frozen into
	// state.Replacements — so prompt-cache byte-stability holds either way, and turning
	// summarization on cannot destabilize an in-flight conversation.
	makeReplacement := func(tu conversation.ToolUseBlock, content, path string) string {
		if summarizer.Enabled() {
			toolName := "unknown"
			if tu.ToolName != "" {
				toolName = tu.ToolName
			}
			if preview, _, ok := summarizer.summarize(context.Background(), toolName, taskGoal, content, path); ok {
				return preview
			}
		}
		return buildSpillPreview(content, path)
	}

	var records []Record

	for i, msg := range messages {
		if len(msg.ToolResults) == 0 {
			continue
		}

		decisions := make(map[string]string, len(msg.ToolResults))
		var fresh []conversation.ToolResultBlock

		for _, tr := range msg.ToolResults {
			if rep, ok := state.Replacements[tr.ToolUseID]; ok {
				// 已决策替换 — 回放精确预览。
				decisions[tr.ToolUseID] = rep
				continue
			}
			if _, ok := state.SeenIDs[tr.ToolUseID]; ok {
				// 已见但未替换 — 永久冻结为原始内容。
				decisions[tr.ToolUseID] = tr.Content
				continue
			}
			if isAlreadyReplaced(tr.Content) {
				// 外部预标记内容（如从磁盘恢复时已写入预览）。
				// 冻结为标记本身，后续轮次保持稳定。
				state.SeenIDs[tr.ToolUseID] = struct{}{}
				state.Replacements[tr.ToolUseID] = tr.Content
				decisions[tr.ToolUseID] = tr.Content
				records = append(records, Record{
					Kind:        "tool-result",
					ToolUseID:   tr.ToolUseID,
					Replacement: tr.Content,
				})
				continue
			}
			fresh = append(fresh, tr)
		}

		// Pass 1: persist any single result above SingleResultLimit.
		persistedByP1 := make(map[string]struct{})
		for _, tr := range fresh {
			if len(tr.Content) <= SingleResultLimit {
				continue
			}
			if isSpillReadback(toolUseByID[tr.ToolUseID], absSpillDir) {
				// Reading a previously-spilled file back — don't spill the
				// readback (it would loop: the readback's own result is
				// ~same size as the spilled file).
				state.SeenIDs[tr.ToolUseID] = struct{}{}
				decisions[tr.ToolUseID] = tr.Content
				persistedByP1[tr.ToolUseID] = struct{}{}
				continue
			}
			path, err := writeSpill(spillDir, tr.ToolUseID, tr.Content)
			if err != nil {
				// Spill failed → freeze as raw (decision still made: never
				// revisit). Subsequent turns will treat this id as frozen-
				// not-replaced and send the raw content.
				state.SeenIDs[tr.ToolUseID] = struct{}{}
				decisions[tr.ToolUseID] = tr.Content
				persistedByP1[tr.ToolUseID] = struct{}{}
				continue
			}
			preview := makeReplacement(toolUseByID[tr.ToolUseID], tr.Content, path)
			decisions[tr.ToolUseID] = preview
			state.SeenIDs[tr.ToolUseID] = struct{}{}
			state.Replacements[tr.ToolUseID] = preview
			records = append(records, Record{
				Kind:        "tool-result",
				ToolUseID:   tr.ToolUseID,
				Replacement: preview,
			})
			persistedByP1[tr.ToolUseID] = struct{}{}
		}

		// Pass 2: aggregate-spill the largest remaining fresh candidates
		// until total ≤ MessageAggregateLimit.
		var remaining []conversation.ToolResultBlock
		for _, tr := range fresh {
			if _, done := persistedByP1[tr.ToolUseID]; done {
				continue
			}
			remaining = append(remaining, tr)
		}

		total := 0
		for _, c := range decisions {
			total += len(c)
		}
		for _, tr := range remaining {
			total += len(tr.Content)
		}

		if total > MessageAggregateLimit && len(remaining) > 0 {
			sorted := append([]conversation.ToolResultBlock(nil), remaining...)
			sort.SliceStable(sorted, func(a, b int) bool {
				return len(sorted[a].Content) > len(sorted[b].Content)
			})
			for _, tr := range sorted {
				if total <= MessageAggregateLimit {
					break
				}
				if isSpillReadback(toolUseByID[tr.ToolUseID], absSpillDir) {
					state.SeenIDs[tr.ToolUseID] = struct{}{}
					decisions[tr.ToolUseID] = tr.Content
					continue
				}
				path, err := writeSpill(spillDir, tr.ToolUseID, tr.Content)
				if err != nil {
					state.SeenIDs[tr.ToolUseID] = struct{}{}
					decisions[tr.ToolUseID] = tr.Content
					continue
				}
				preview := makeReplacement(toolUseByID[tr.ToolUseID], tr.Content, path)
				decisions[tr.ToolUseID] = preview
				state.SeenIDs[tr.ToolUseID] = struct{}{}
				state.Replacements[tr.ToolUseID] = preview
				records = append(records, Record{
					Kind:        "tool-result",
					ToolUseID:   tr.ToolUseID,
					Replacement: preview,
				})
				total -= len(tr.Content) - len(preview)
			}
		}

		// Freeze remaining fresh as "seen but not replaced".
		for _, tr := range fresh {
			if _, decided := decisions[tr.ToolUseID]; decided {
				continue
			}
			state.SeenIDs[tr.ToolUseID] = struct{}{}
			decisions[tr.ToolUseID] = tr.Content
		}

		// 就地替换：按原始顺序写回 conv 的消息历史。
		newResults := make([]conversation.ToolResultBlock, len(msg.ToolResults))
		for k, tr := range msg.ToolResults {
			newResults[k] = conversation.ToolResultBlock{
				ToolUseID: tr.ToolUseID,
				Content:   decisions[tr.ToolUseID],
				IsError:   tr.IsError,
			}
		}
		conv.ReplaceToolResults(i, newResults)
	}

	return records, nil
}

// previewSize 是存盘预览的最大字符数，与 Claude Code 保持一致。
const previewSize = 2000

// buildSpillPreview 构造存盘替换文本，包含前 2KB 预览。
// 一旦写入 state.Replacements 就会逐字节回放，格式变更会导致
// Prompt Cache 失效，不要轻易改动。
func buildSpillPreview(content string, path string) string {
	sizeKB := len(content) / 1024
	preview := content
	hasMore := false
	if len(preview) > previewSize {
		preview = preview[:previewSize]
		hasMore = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<persisted-output>\n")
	fmt.Fprintf(&b, "输出太大（%dKB），完整内容已保存到：\n%s\n\n", sizeKB, path)
	fmt.Fprintf(&b, "预览（前 2KB）：\n%s", preview)
	if hasMore {
		b.WriteString("\n...")
	}
	b.WriteString("\n</persisted-output>")
	return b.String()
}

// buildToolUseIndex maps tool_use_id → ToolUseBlock so spill decisions can
// inspect the originating tool name and arguments. Tool uses live on
// assistant messages while results live on the next user message, paired
// only by ID — pre-indexing avoids an O(n²) cross-message scan.
func buildToolUseIndex(messages []conversation.Message) map[string]conversation.ToolUseBlock {
	idx := make(map[string]conversation.ToolUseBlock, len(messages))
	for _, m := range messages {
		for _, tu := range m.ToolUses {
			idx[tu.ToolUseID] = tu
		}
	}
	return idx
}

// isSpillReadback reports whether the originating tool call was a ReadFile
// of a path inside the spill directory. Spilling the readback would loop —
// the readback's own result is ~same size as the spilled file, so it'd trip
// SingleResultLimit again and produce a chain of stub-of-stub files.
func isSpillReadback(tu conversation.ToolUseBlock, absSpillDir string) bool {
	if tu.ToolName != "ReadFile" || absSpillDir == "" {
		return false
	}
	raw, _ := tu.Arguments["file_path"].(string)
	if raw == "" {
		return false
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return false
	}
	return strings.HasPrefix(abs, absSpillDir)
}

func writeSpill(dir, toolUseID, content string) (string, error) {
	if toolUseID == "" {
		return "", fmt.Errorf("empty tool_use_id")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, toolUseID)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return path, nil
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return path, nil
}
