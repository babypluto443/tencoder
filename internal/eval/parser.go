package eval

import (
	"regexp"
	"strings"
)

// pytest 状态标记
const (
	StatusPassed = "PASSED"
	StatusFailed = "FAILED"
	StatusError  = "ERROR"
)

var (
	// 冗长模式行：tests/test_x.py::test_y[-params] PASSED（前面可能带百分比进度）
	verboseRe = regexp.MustCompile(`^(?:\d+% \|[^|]*\| \d+/\d+ )?([^\s]+::[^\s]+) (PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS)\b`)
	// 短摘要行：PASSED/FAILED/ERROR <nodeid>（pytest -rA 输出，状态在前）
	summaryRe = regexp.MustCompile(`^(PASSED|FAILED|ERROR|XFAIL|XPASS) ([^\s]+)`)
)

// ParsePytestLog 解析 pytest 输出，返回 test nodeid → 状态映射。
// 兼容两种输出：-v 冗长模式行与末尾 short test summary 行。
// 同一 nodeid 出现多次时取最差结果（ERROR > FAILED > PASSED）。
func ParsePytestLog(log string) map[string]string {
	out := map[string]string{}
	rank := func(s string) int {
		switch s {
		case StatusError:
			return 3
		case StatusFailed:
			return 2
		case StatusPassed:
			return 1
		default:
			return 0
		}
	}
	merge := func(id, status string) {
		id = normalizeNodeID(id)
		if id == "" {
			return
		}
		if prev, ok := out[id]; !ok || rank(status) > rank(prev) {
			out[id] = status
		}
	}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := verboseRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			merge(m[1], normalizeStatus(m[2]))
			continue
		}
		if m := summaryRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			id := m[2]
			if i := strings.Index(id, " - "); i >= 0 {
				id = id[:i]
			}
			merge(id, normalizeStatus(m[1]))
		}
	}
	return out
}

// normalizeStatus 归一 pytest 状态到 PASSED/FAILED/ERROR。
// 与官方 parser 口径一致：XFAIL/XPASS 视为 pass，SKIPPED 保持原样（不算 pass）。
func normalizeStatus(s string) string {
	switch s {
	case "XFAIL", "XPASS":
		return StatusPassed
	case "ERROR":
		return StatusError
	case "FAILED":
		return StatusFailed
	case "SKIPPED":
		return "SKIPPED"
	default:
		return StatusPassed // PASSED
	}
}

func normalizeNodeID(id string) string {
	id = strings.TrimSpace(id)
	// 去掉结尾 ::类名 的兼容处理不做——数据集 nodeid 一般带全路径；
	// 这里只做轻微归一：统一路径分隔符。
	return strings.ReplaceAll(id, "\\", "/")
}

// ParseStatuses 按 log_parser 类型分发解析。
func ParseStatuses(log, logParser string) map[string]string {
	switch strings.ToLower(strings.TrimSpace(logParser)) {
	case "", "pytest":
		return ParsePytestLog(log)
	default:
		return ParsePytestLog(log) // 未知类型按 pytest 处理，SWE-bench-Live 目前仅 pytest
	}
}

// IsResolved 判定实例是否解决：FAIL_TO_PASS 全部 PASSED 且 PASS_TO_PASS 全部 PASSED。
// SKIPPED 视为未通过（严格口径，与 SWE-bench 一致）。
func IsResolved(inst *Instance, statuses map[string]string) (bool, []string) {
	var missing []string
	for _, id := range inst.FAILToPASS {
		if statuses[normalizeNodeID(id)] != StatusPassed {
			missing = append(missing, "F2P: "+id)
		}
	}
	for _, id := range inst.PASSToPASS {
		if statuses[normalizeNodeID(id)] != StatusPassed {
			missing = append(missing, "P2P: "+id)
		}
	}
	return len(missing) == 0, missing
}
